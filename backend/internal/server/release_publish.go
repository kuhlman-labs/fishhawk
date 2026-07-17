package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
)

// CategoryReleasePublished is the audit category the publish endpoint appends
// to the release run's chain after it sets the GitHub Release body + asset
// (E33.3 / #1588). Free-form audit category (no CHECK constraint, no central
// enum), so it needs no migration. It is an INTERNAL audit kind with no
// issue-comment surface — see docs/issue-comment-surfaces.md.
const CategoryReleasePublished = "release_published"

// releaseNotesAssetName is the FIXED asset name the publish integration uses on
// every invocation (binding approval condition). A fixed name is what lets the
// idempotent path replace the asset in place (delete-by-name then upload) so the
// Release body and the attached asset can never diverge.
const releaseNotesAssetName = "release-notes.md"

// releaseNotesAssetContentType is the media type the markdown asset is uploaded
// with.
const releaseNotesAssetContentType = "text/markdown"

// releasePublisher is the narrow GitHub surface the publish handler needs: the
// installation lookup + the three Release write operations + asset delete. The
// production *githubclient.Client satisfies it directly; tests inject a fake so
// the path runs offline. Mirrors the releaseNotesResolverOverride seam.
type releasePublisher interface {
	GetRepoInstallation(ctx context.Context, repo githubclient.RepoRef) (int64, error)
	GetReleaseByTagScoped(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, tag string) (*githubclient.Release, error)
	UpdateReleaseBodyScoped(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, releaseID int64, body string) error
	DeleteReleaseAssetScoped(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, assetID int64) error
	UploadReleaseAssetScoped(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, releaseID int64, name, contentType string, data []byte) error
}

// releasePublishRequest is the JSON body of POST /v0/releases/publish. repo +
// tag identify the published GitHub Release; artifact_id names the persisted
// release_notes artifact whose markdown becomes the Release body + asset;
// run_id + stage_id key the release_published audit entry on the release run's
// chain (run_id is required — AppendChained needs a non-nil RunID; stage_id is
// optional).
type releasePublishRequest struct {
	Repo       string `json:"repo"`
	Tag        string `json:"tag"`
	RunID      string `json:"run_id"`
	StageID    string `json:"stage_id"`
	ArtifactID string `json:"artifact_id"`
}

// releasePublishResponse is the 200 body: the release URL + tag + source
// artifact + content hash, plus published/idempotent flags distinguishing a
// real publish from a no-op re-invoke.
type releasePublishResponse struct {
	ReleaseURL  string `json:"release_url"`
	Tag         string `json:"tag"`
	ArtifactID  string `json:"artifact_id"`
	ContentHash string `json:"content_hash"`
	Published   bool   `json:"published"`
	Idempotent  bool   `json:"idempotent"`
}

// handleReleasePublish implements POST /v0/releases/publish (E33.3 / #1588,
// ADR-051 option B, publish half): given a published GitHub Release (by tag)
// and a persisted release_notes artifact, it sets the Release body to the
// persisted notes markdown and attaches the notes as a fixed-name markdown
// asset via the App installation token, then records a release_published audit
// entry on the release run's chain.
//
// Idempotency keys on CONTENT HASH for BOTH surfaces (binding approval
// condition): a re-invoke is a full no-op only when the desired notes hash
// equals the last recorded release_published hash AND the live Release body
// already hashes to the desired notes. Otherwise it PATCHes the body AND
// replaces the asset (delete-by-name then upload) so body and asset can never
// diverge.
//
// Auth mirrors handleReleaseNotesPersist exactly: anonymous → 401; an
// authenticated bearer token additionally requires write:runs → 403; a cookie
// session (empty TokenID) is not scope-gated. A new endpoint, so no existing
// token is tightened (auth-change impact inventory empty). No App permission
// change: the Releases REST endpoints are covered by the App's existing
// contents:write grant (ADR-051 accepted-condition 3, confirmed on #1588).
func (s *Server) handleReleasePublish(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	if id.TokenID != "" && !hasScope(id, "write:runs") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:runs",
			map[string]any{"required_scope": "write:runs"})
		return
	}
	if !s.releasePublishConfigured(w, r) {
		return
	}

	var req releasePublishRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body must be valid JSON",
			map[string]any{"error": err.Error()})
		return
	}
	repo := strings.TrimSpace(req.Repo)
	tag := strings.TrimSpace(req.Tag)
	runIDRaw := strings.TrimSpace(req.RunID)
	stageIDRaw := strings.TrimSpace(req.StageID)
	artifactIDRaw := strings.TrimSpace(req.ArtifactID)
	if missing := firstMissingPublishParam(repo, tag, runIDRaw, artifactIDRaw); missing != "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"repo, tag, run_id, and artifact_id are required",
			map[string]any{"field": missing})
		return
	}
	runID, err := uuid.Parse(runIDRaw)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": runIDRaw})
		return
	}
	artifactID, err := uuid.Parse(artifactIDRaw)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"artifact_id must be a valid UUID",
			map[string]any{"field": "artifact_id", "got": artifactIDRaw})
		return
	}
	var stageID *uuid.UUID
	if stageIDRaw != "" {
		parsed, err := uuid.Parse(stageIDRaw)
		if err != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"stage_id must be a valid UUID when provided",
				map[string]any{"field": "stage_id", "got": stageIDRaw})
			return
		}
		stageID = &parsed
	}

	// Load the persisted release_notes artifact and decode its markdown.
	art, err := s.cfg.ArtifactRepo.Get(r.Context(), artifactID)
	if err != nil {
		if errors.Is(err, artifact.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "artifact_not_found",
				"no artifact with that id",
				map[string]any{"artifact_id": artifactID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "release_publish_failed",
			"load release_notes artifact failed",
			map[string]any{"error": err.Error(), "artifact_id": artifactID.String()})
		return
	}
	if art.Kind != artifact.KindReleaseNotes {
		s.writeError(w, r, http.StatusConflict, "artifact_wrong_kind",
			"artifact is not a release_notes artifact",
			map[string]any{"artifact_id": artifactID.String(), "kind": string(art.Kind)})
		return
	}
	var content releaseNotesContent
	if err := json.Unmarshal(art.Content, &content); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "release_publish_failed",
			"decode release_notes artifact content failed",
			map[string]any{"error": err.Error(), "artifact_id": artifactID.String()})
		return
	}
	markdown := content.Markdown
	desiredHash := sha256Hex([]byte(markdown))

	// Resolve the GitHub publisher + the App installation for the repo.
	pub := s.releasePublisher()
	owner, name, ok := splitRepoFullName(repo)
	if !ok {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"repo must be owner/name",
			map[string]any{"field": "repo", "got": repo})
		return
	}
	repoRef := githubclient.RepoRef{Owner: owner, Name: name}
	instID, err := pub.GetRepoInstallation(r.Context(), repoRef)
	if err != nil {
		if errors.Is(err, githubclient.ErrNotInstalled) {
			s.writeError(w, r, http.StatusServiceUnavailable, "github_app_not_installed",
				"the GitHub App is not installed on the target repo",
				map[string]any{"repo": repo})
			return
		}
		s.writeError(w, r, http.StatusBadGateway, "installation_resolution_failed",
			"could not resolve the GitHub App installation for the target repo",
			map[string]any{"error": err.Error()})
		return
	}

	scope := forge.FromGitHubInstallationID(instID)
	rel, err := pub.GetReleaseByTagScoped(r.Context(), scope, repoRef, tag)
	if err != nil {
		if errors.Is(err, githubclient.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "release_not_found",
				"no published GitHub Release for that tag",
				map[string]any{"repo": repo, "tag": tag})
			return
		}
		s.writeError(w, r, http.StatusBadGateway, "release_lookup_failed",
			"could not look up the GitHub Release",
			map[string]any{"error": err.Error()})
		return
	}

	// Content-hash idempotency for BOTH surfaces (binding approval condition):
	// a full no-op only when the last recorded release_published hash AND the
	// live Release body both already equal the desired notes hash. If EITHER
	// differs we rewrite both surfaces, so they can never diverge.
	recordedHash := s.lastReleasePublishedHash(r.Context(), runID)
	bodyHash := sha256Hex([]byte(rel.Body))
	if recordedHash == desiredHash && bodyHash == desiredHash {
		s.writeJSON(w, r, http.StatusOK, releasePublishResponse{
			ReleaseURL:  rel.HTMLURL,
			Tag:         tag,
			ArtifactID:  artifactID.String(),
			ContentHash: desiredHash,
			Published:   false,
			Idempotent:  true,
		})
		return
	}

	// Rewrite the Release body.
	if err := pub.UpdateReleaseBodyScoped(r.Context(), scope, repoRef, rel.ID, markdown); err != nil {
		s.writeError(w, r, http.StatusBadGateway, "release_publish_failed",
			"update GitHub Release body failed",
			map[string]any{"error": err.Error()})
		return
	}
	// Replace the fixed-name asset: delete any existing copy by name, then
	// upload the current markdown — so the asset content matches the body.
	for _, a := range rel.Assets {
		if a.Name != releaseNotesAssetName {
			continue
		}
		if err := pub.DeleteReleaseAssetScoped(r.Context(), scope, repoRef, a.ID); err != nil {
			s.writeError(w, r, http.StatusBadGateway, "release_publish_failed",
				"delete stale release asset failed",
				map[string]any{"error": err.Error()})
			return
		}
	}
	if err := pub.UploadReleaseAssetScoped(r.Context(), scope, repoRef, rel.ID,
		releaseNotesAssetName, releaseNotesAssetContentType, []byte(markdown)); err != nil {
		s.writeError(w, r, http.StatusBadGateway, "release_publish_failed",
			"upload release-notes asset failed",
			map[string]any{"error": err.Error()})
		return
	}

	// Record the release_published audit entry on the run's chain. Durable
	// before response: an append failure is a 500, not a silent success.
	systemKind := audit.ActorSystem
	payload, _ := json.Marshal(map[string]any{
		"tag":          tag,
		"release_url":  rel.HTMLURL,
		"artifact_id":  artifactID.String(),
		"content_hash": desiredHash,
	})
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:     runID,
		StageID:   stageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryReleasePublished,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "release_publish_audit_failed",
			"record release_published audit entry failed",
			map[string]any{"error": err.Error(), "run_id": runID.String()})
		return
	}

	s.writeJSON(w, r, http.StatusOK, releasePublishResponse{
		ReleaseURL:  rel.HTMLURL,
		Tag:         tag,
		ArtifactID:  artifactID.String(),
		ContentHash: desiredHash,
		Published:   true,
		Idempotent:  false,
	})
}

// releasePublisher returns the test override when set, else the production
// GitHub client (which satisfies the interface). Returns nil only when neither
// is configured — releasePublishConfigured guards against that before the
// handler dereferences the result.
func (s *Server) releasePublisher() releasePublisher {
	if s.releasePublisherOverride != nil {
		return s.releasePublisherOverride
	}
	if s.cfg.GitHub == nil {
		return nil
	}
	return s.cfg.GitHub
}

// releasePublishConfigured writes a 503 and returns false when a dependency the
// publish handler needs (artifact repo, audit repo, or a GitHub publisher) is
// nil — the nil-dependency fail-closed posture the sibling handlers share. Runs
// AFTER the auth ladder so an anonymous caller on an unconfigured server still
// gets 401 rather than leaking configuration state.
func (s *Server) releasePublishConfigured(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.ArtifactRepo == nil || s.cfg.AuditRepo == nil || s.releasePublisher() == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "release_publish_unconfigured",
			"release-publish endpoint requires artifact + audit repositories and a configured GitHub client", nil)
		return false
	}
	return true
}

// lastReleasePublishedHash returns the content_hash of the most recent
// release_published audit entry on the run's chain, or "" when none exists (or
// the lookup fails — a best-effort read that, when it returns "", forces a
// rewrite rather than a wrongful no-op). It is the "recorded hash" half of the
// content-hash idempotency check.
func (s *Server) lastReleasePublishedHash(ctx context.Context, runID uuid.UUID) string {
	entries, err := s.cfg.AuditRepo.ListForRun(ctx, runID)
	if err != nil {
		return ""
	}
	var last string
	for _, e := range entries {
		if e.Category != CategoryReleasePublished {
			continue
		}
		var p struct {
			ContentHash string `json:"content_hash"`
		}
		if json.Unmarshal(e.Payload, &p) == nil && p.ContentHash != "" {
			last = p.ContentHash
		}
	}
	return last
}

// firstMissingPublishParam returns the name of the first empty required field,
// or "" when all are present.
func firstMissingPublishParam(repo, tag, runID, artifactID string) string {
	switch {
	case repo == "":
		return "repo"
	case tag == "":
		return "tag"
	case runID == "":
		return "run_id"
	case artifactID == "":
		return "artifact_id"
	default:
		return ""
	}
}
