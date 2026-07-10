package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/releaseevidence"
	"github.com/kuhlman-labs/fishhawk/backend/internal/releasenotes"
)

// pgSQLStateForeignKeyViolation is Postgres SQLSTATE 23503, raised when an
// insert violates a foreign-key constraint — here, a caller-supplied stage_id
// that references no stages row.
const pgSQLStateForeignKeyViolation = "23503"

// releaseNotesPersistRequest is the JSON body of POST /v0/releases/notes. repo,
// from, and to are the same coordinates the preview endpoint reads from query
// params; stage_id keys the persisted release_notes artifact. A caller-supplied
// stage_id is required because artifact.Create is stage-scoped and no
// first-class release stage type exists yet (E33.2 risk note): the release-notes
// artifact attaches to a caller-chosen stage exactly as every other artifact is
// keyed.
type releaseNotesPersistRequest struct {
	Repo    string `json:"repo"`
	From    string `json:"from"`
	To      string `json:"to"`
	StageID string `json:"stage_id"`
}

// releaseNotesContent is the JSON content object stored in the release_notes
// artifact: the coordinates plus the rendered markdown, so a later reader can
// re-display the notes without re-assembling.
type releaseNotesContent struct {
	Repo     string `json:"repo"`
	From     string `json:"from"`
	To       string `json:"to"`
	Markdown string `json:"markdown"`
}

// releaseNotesPersistResponse is the 201 body: the persisted artifact's id +
// coordinates + rendered markdown.
type releaseNotesPersistResponse struct {
	ArtifactID  string `json:"artifact_id"`
	StageID     string `json:"stage_id"`
	Repo        string `json:"repo"`
	From        string `json:"from"`
	To          string `json:"to"`
	ContentHash string `json:"content_hash"`
	Markdown    string `json:"markdown"`
}

// releaseNotesError carries a status+code+message back from the shared
// assemble/resolve helpers to either handler, mirroring the small typed-error
// carrier the work-item filing path uses.
type releaseNotesError struct {
	status  int
	code    string
	msg     string
	details map[string]any
}

// handleReleaseNotesPreview implements GET
// /v0/releases/notes/preview?repo=&from=&to= (E33.2 / #1587, ADR-051 option B):
// it assembles the merged-run evidence in the ref range and renders it to
// release-notes markdown WITHOUT persisting anything.
//
// Auth (binding approval condition 3): authenticated read-only — anonymous →
// 401, no scope gate beyond that, matching the sibling read endpoints. A new
// endpoint, so no existing token is tightened (impact inventory empty).
func (s *Server) handleReleaseNotesPreview(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	if !s.releaseNotesConfigured(w, r) {
		return
	}

	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	from := strings.TrimSpace(r.URL.Query().Get("from"))
	to := strings.TrimSpace(r.URL.Query().Get("to"))
	if missing := firstMissingReleaseParam(repo, from, to); missing != "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"repo, from, and to query parameters are required",
			map[string]any{"field": missing})
		return
	}

	ev, herr := s.assembleReleaseEvidence(r.Context(), repo, from, to)
	if herr != nil {
		s.writeError(w, r, herr.status, herr.code, herr.msg, herr.details)
		return
	}

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(releasenotes.Render(ev)))
}

// handleReleaseNotesPersist implements POST /v0/releases/notes (E33.2 / #1587):
// it assembles + renders exactly as the preview endpoint, then persists the
// rendered notes as a `release_notes` artifact keyed to the caller-supplied
// stage_id.
//
// Auth (binding approval condition 3): a persisting write — anonymous → 401,
// and an authenticated token additionally requires the write:runs scope via the
// exact `id.TokenID != "" && !hasScope(id, "write:runs")` gate the sibling
// write handlers (consolidate.go, reap_failure.go) use. A cookie session with
// an empty TokenID is not scope-gated (matching those siblings). A new
// endpoint, so no existing token is tightened (impact inventory empty).
func (s *Server) handleReleaseNotesPersist(w http.ResponseWriter, r *http.Request) {
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
	if !s.releaseNotesConfigured(w, r) {
		return
	}

	var req releaseNotesPersistRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body must be valid JSON",
			map[string]any{"error": err.Error()})
		return
	}
	repo := strings.TrimSpace(req.Repo)
	from := strings.TrimSpace(req.From)
	to := strings.TrimSpace(req.To)
	stageIDRaw := strings.TrimSpace(req.StageID)
	if missing := firstMissingReleaseParam(repo, from, to); missing != "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"repo, from, and to are required",
			map[string]any{"field": missing})
		return
	}
	if stageIDRaw == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage_id is required to key the persisted release_notes artifact",
			map[string]any{"field": "stage_id"})
		return
	}
	stageID, err := uuid.Parse(stageIDRaw)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage_id must be a valid UUID",
			map[string]any{"field": "stage_id", "got": stageIDRaw})
		return
	}

	ev, herr := s.assembleReleaseEvidence(r.Context(), repo, from, to)
	if herr != nil {
		s.writeError(w, r, herr.status, herr.code, herr.msg, herr.details)
		return
	}

	markdown := releasenotes.Render(ev)
	content, err := json.Marshal(releaseNotesContent{Repo: repo, From: from, To: to, Markdown: markdown})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"encode release_notes content failed", map[string]any{"error": err.Error()})
		return
	}
	sum := sha256.Sum256(content)
	created, err := s.cfg.ArtifactRepo.Create(r.Context(), artifact.CreateParams{
		StageID:     stageID,
		Kind:        artifact.KindReleaseNotes,
		Content:     content,
		ContentHash: hex.EncodeToString(sum[:]),
	})
	if err != nil {
		// A caller-supplied stage_id that is a valid UUID but has no
		// stages row fails the artifacts.stage_id FK (SQLSTATE 23503).
		// That is a client mistake, not a server fault — surface it as a
		// 404 rather than mislabeling it 500.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgSQLStateForeignKeyViolation {
			s.writeError(w, r, http.StatusNotFound, "stage_not_found",
				"no stage with that id to key the release_notes artifact",
				map[string]any{"stage_id": stageID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "release_notes_persist_failed",
			"persist release_notes artifact failed",
			map[string]any{"error": err.Error(), "stage_id": stageID.String()})
		return
	}

	s.writeJSON(w, r, http.StatusCreated, releaseNotesPersistResponse{
		ArtifactID:  created.ID.String(),
		StageID:     stageID.String(),
		Repo:        repo,
		From:        from,
		To:          to,
		ContentHash: created.ContentHash,
		Markdown:    markdown,
	})
}

// releaseNotesConfigured writes a 503 and returns false when a repository the
// assembler crosses (run / audit / concern / artifact) is nil — mirroring the
// sibling handlers' nil-dependency fail-closed posture. The check runs AFTER the
// auth ladder (401/403) so an anonymous caller on an unconfigured server gets
// 401 — the binding auth posture for these endpoints — rather than leaking
// configuration state as a 503 before authentication.
func (s *Server) releaseNotesConfigured(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil || s.cfg.ConcernRepo == nil || s.cfg.ArtifactRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "release_notes_unconfigured",
			"release-notes endpoint requires run, audit, concern, and artifact repositories", nil)
		return false
	}
	return true
}

// assembleReleaseEvidence resolves the merged-PR resolver, wires the assembler
// over the four configured repos, and assembles the release evidence for the
// ref range. It is the shared body of both the preview and persist handlers.
func (s *Server) assembleReleaseEvidence(ctx context.Context, repo, from, to string) (*releaseevidence.ReleaseEvidence, *releaseNotesError) {
	resolver, rerr := s.resolveMergedPRResolver(ctx, repo)
	if rerr != nil {
		return nil, rerr
	}
	asm := &releaseevidence.Assembler{
		Runs:      s.cfg.RunRepo,
		Audit:     s.cfg.AuditRepo,
		Concerns:  s.cfg.ConcernRepo,
		Artifacts: s.cfg.ArtifactRepo,
		PRs:       resolver,
	}
	ev, err := asm.Assemble(ctx, repo, from, to)
	if err != nil {
		return nil, &releaseNotesError{
			status:  http.StatusBadGateway,
			code:    "release_notes_assembly_failed",
			msg:     "assemble release evidence failed",
			details: map[string]any{"error": err.Error()},
		}
	}
	return ev, nil
}

// resolveMergedPRResolver returns the test override when set, else builds the
// production releaseevidence.GitHubResolver from cfg.GitHub + the App
// installation resolved for the target repo. Fails closed: nil GitHub → 503,
// App-not-installed → 503, any other installation-resolution error → 502.
func (s *Server) resolveMergedPRResolver(ctx context.Context, repo string) (releaseevidence.MergedPRResolver, *releaseNotesError) {
	if s.releaseNotesResolverOverride != nil {
		return s.releaseNotesResolverOverride, nil
	}
	if s.cfg.GitHub == nil {
		return nil, &releaseNotesError{
			status: http.StatusServiceUnavailable,
			code:   "release_notes_unconfigured",
			msg:    "release-notes endpoint requires a configured GitHub client to resolve merged PRs",
		}
	}
	owner, name, ok := splitRepoFullName(repo)
	if !ok {
		return nil, &releaseNotesError{
			status:  http.StatusBadRequest,
			code:    "validation_failed",
			msg:     "repo must be owner/name",
			details: map[string]any{"field": "repo", "got": repo},
		}
	}
	instID, err := s.cfg.GitHub.GetRepoInstallation(ctx, githubclient.RepoRef{Owner: owner, Name: name})
	if err != nil {
		if errors.Is(err, githubclient.ErrNotInstalled) {
			return nil, &releaseNotesError{
				status:  http.StatusServiceUnavailable,
				code:    "github_app_not_installed",
				msg:     "the GitHub App is not installed on the target repo",
				details: map[string]any{"repo": repo},
			}
		}
		return nil, &releaseNotesError{
			status:  http.StatusBadGateway,
			code:    "installation_resolution_failed",
			msg:     "could not resolve the GitHub App installation for the target repo",
			details: map[string]any{"error": err.Error()},
		}
	}
	return &releaseevidence.GitHubResolver{Client: s.cfg.GitHub, InstallationID: instID}, nil
}

// firstMissingReleaseParam returns the name of the first empty required
// coordinate, or "" when all three are present.
func firstMissingReleaseParam(repo, from, to string) string {
	switch {
	case repo == "":
		return "repo"
	case from == "":
		return "from"
	case to == "":
		return "to"
	default:
		return ""
	}
}
