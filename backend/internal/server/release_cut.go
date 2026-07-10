package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// CategoryReleaseCut is the audit category the cut endpoint appends to the
// release run's chain when the operator ratifies the release version (E33.5 /
// #1590, ADR-051). Like release_published it is a free-form audit category (no
// CHECK constraint, no central enum), so it needs no migration, and it is an
// INTERNAL, system-actor audit kind with no issue-comment surface: nothing in
// issuecomment posts it and it has no Notifier method (its
// docs/issue-comment-surfaces.md entry is a deferred follow-up — see the PR
// notes).
const CategoryReleaseCut = "release_cut"

// releaseCutRequest is the JSON body of POST /v0/releases/cut. repo + version
// carry the operator's ratified version decision; artifact_id names the
// persisted release_notes artifact the decision is cut against (validated to be
// a release_notes kind); run_id + stage_id key the release_cut audit entry on
// the release run's chain (run_id is required — AppendChained needs a non-nil
// RunID; stage_id is optional). bump_level is the OPTIONAL advisory semver level
// the operator settled on (patch/minor/major), recorded verbatim for audit — it
// is free-form and never validated, mirroring the advisory classifier hint.
//
// The endpoint records the DECISION only: it appends the audit entry and does
// NOT push a git tag or write to GitHub. Tagging the release stays a human git
// action per the delegating posture (binding approval condition).
type releaseCutRequest struct {
	Repo       string `json:"repo"`
	RunID      string `json:"run_id"`
	StageID    string `json:"stage_id"`
	ArtifactID string `json:"artifact_id"`
	Version    string `json:"version"`
	BumpLevel  string `json:"bump_level"`
}

// releaseCutResponse is the 201 body: the ratified version + the source
// artifact + its content hash + the recorded advisory bump level, plus a
// recorded flag affirming the audit entry landed.
type releaseCutResponse struct {
	Version     string `json:"version"`
	ArtifactID  string `json:"artifact_id"`
	ContentHash string `json:"content_hash"`
	BumpLevel   string `json:"bump_level,omitempty"`
	Recorded    bool   `json:"recorded"`
}

// handleReleaseCut implements POST /v0/releases/cut (E33.5 / #1590, ADR-051):
// it records the operator's ratified release-version decision as a release_cut
// audit entry on the release run's chain, after validating that artifact_id
// names a real release_notes artifact. It performs NO git tag push and NO
// GitHub write — the tag push stays the operator's human git action per the
// delegating posture (binding approval condition).
//
// Auth mirrors handleReleaseNotesPersist / handleReleasePublish exactly:
// anonymous → 401; an authenticated bearer token additionally requires
// write:runs → 403; a cookie session (empty TokenID) is not scope-gated. A new
// endpoint, so no existing token is tightened (auth-change impact inventory
// empty).
func (s *Server) handleReleaseCut(w http.ResponseWriter, r *http.Request) {
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
	if !s.releaseCutConfigured(w, r) {
		return
	}

	var req releaseCutRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body must be valid JSON",
			map[string]any{"error": err.Error()})
		return
	}
	repo := strings.TrimSpace(req.Repo)
	runIDRaw := strings.TrimSpace(req.RunID)
	stageIDRaw := strings.TrimSpace(req.StageID)
	artifactIDRaw := strings.TrimSpace(req.ArtifactID)
	version := strings.TrimSpace(req.Version)
	bumpLevel := strings.TrimSpace(req.BumpLevel)
	if missing := firstMissingCutParam(repo, runIDRaw, artifactIDRaw, version); missing != "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"repo, run_id, artifact_id, and version are required",
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

	// Load the persisted artifact and confirm it is a release_notes kind — the
	// version decision must be cut against real, rendered notes.
	art, err := s.cfg.ArtifactRepo.Get(r.Context(), artifactID)
	if err != nil {
		if errors.Is(err, artifact.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "artifact_not_found",
				"no artifact with that id",
				map[string]any{"artifact_id": artifactID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "release_cut_failed",
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

	// Record the release_cut audit entry on the run's chain. Durable before
	// response: an append failure is a 500, not a silent success. content_hash
	// is the persisted artifact's own hash — it pins WHICH notes were cut.
	systemKind := audit.ActorSystem
	payload, _ := json.Marshal(map[string]any{
		"repo":         repo,
		"version":      version,
		"artifact_id":  artifactID.String(),
		"bump_level":   bumpLevel,
		"content_hash": art.ContentHash,
	})
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:     runID,
		StageID:   stageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryReleaseCut,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "release_cut_audit_failed",
			"record release_cut audit entry failed",
			map[string]any{"error": err.Error(), "run_id": runID.String()})
		return
	}

	s.writeJSON(w, r, http.StatusCreated, releaseCutResponse{
		Version:     version,
		ArtifactID:  artifactID.String(),
		ContentHash: art.ContentHash,
		BumpLevel:   bumpLevel,
		Recorded:    true,
	})
}

// releaseCutConfigured writes a 503 and returns false when a dependency the cut
// handler needs (artifact repo or audit repo) is nil — the nil-dependency
// fail-closed posture the sibling handlers share. It needs NO GitHub client
// because cut records the decision only (no tag push, no GitHub write). Runs
// AFTER the auth ladder so an anonymous caller on an unconfigured server still
// gets 401 rather than leaking configuration state.
func (s *Server) releaseCutConfigured(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.ArtifactRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "release_cut_unconfigured",
			"release-cut endpoint requires artifact + audit repositories", nil)
		return false
	}
	return true
}

// firstMissingCutParam returns the name of the first empty required field, or
// "" when all are present.
func firstMissingCutParam(repo, runID, artifactID, version string) string {
	switch {
	case repo == "":
		return "repo"
	case runID == "":
		return "run_id"
	case artifactID == "":
		return "artifact_id"
	case version == "":
		return "version"
	default:
		return ""
	}
}
