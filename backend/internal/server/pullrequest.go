package server

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// maxPullRequestBundleBytes caps the request body. PR artifacts are
// small structured JSON (a handful of fields, no embedded diff), so
// 32 KB is well above any realistic payload and well below trace's
// 64 MiB cap.
const maxPullRequestBundleBytes = 32 * 1024

// pullRequestBody is the wire shape the runner POSTs. Required
// fields are validated structurally below — there's no JSON Schema
// for v0; v1+ can graduate this to `pull_request_v1.schema.json`.
type pullRequestBody struct {
	PRNumber          int    `json:"pr_number"`
	PRURL             string `json:"pr_url"`
	Branch            string `json:"branch"`
	HeadSHA           string `json:"head_sha"`
	BaseSHA           string `json:"base_sha"`
	Title             string `json:"title"`
	Body              string `json:"body,omitempty"`
	FilesChangedCount int    `json:"files_changed_count"`
}

// validate returns a human-readable error if any required field is
// missing. PR upload is irreversible (real PR exists on GitHub by
// the time this fires), so a 400 here means the runner shipped the
// wrong shape — the operator's audit log will need to be reconciled
// by hand.
func (p *pullRequestBody) validate() error {
	switch {
	case p.PRNumber <= 0:
		return errors.New("pr_number must be a positive integer")
	case p.PRURL == "" || !strings.HasPrefix(p.PRURL, "http"):
		return errors.New("pr_url must be a non-empty http(s) URL")
	case p.Branch == "":
		return errors.New("branch is required")
	case p.HeadSHA == "":
		return errors.New("head_sha is required")
	case p.BaseSHA == "":
		return errors.New("base_sha is required")
	case p.Title == "":
		return errors.New("title is required")
	}
	return nil
}

// handleShipPullRequest implements POST /v0/runs/{run_id}/pull-request.
//
// Mirrors the plan-upload handler in shape: signed body, schema-ish
// validation, idempotent on (stage_id, head_sha) — re-running a
// runner job that already pushed the same commits returns the
// existing artifact. Inserts an `artifacts` row with kind=pull_request
// and appends a `pull_request_opened` audit entry.
func (s *Server) handleShipPullRequest(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SigningRepo == nil || s.cfg.ArtifactRepo == nil ||
		s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "pull_request_upload_unconfigured",
			"pull-request upload requires signing, artifact, audit, and run repositories", nil)
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	stageID, err := uuid.Parse(r.URL.Query().Get("stage_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage_id query parameter must be a valid UUID",
			map[string]any{"field": "stage_id", "got": r.URL.Query().Get("stage_id")})
		return
	}

	stage, err := s.cfg.RunRepo.GetStage(r.Context(), stageID)
	if err != nil {
		s.writeError(w, r, http.StatusNotFound, "stage_not_found",
			"stage does not exist",
			map[string]any{"stage_id": stageID.String()})
		return
	}
	if stage.RunID != runID {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage does not belong to the supplied run",
			map[string]any{"stage_id": stageID.String(), "run_id": runID.String()})
		return
	}

	sigHeader := r.Header.Get("X-Fishhawk-Signature")
	if sigHeader == "" {
		s.writeError(w, r, http.StatusUnauthorized, "signature_missing",
			"X-Fishhawk-Signature header is required", nil)
		return
	}
	signature, err := hex.DecodeString(sigHeader)
	if err != nil {
		s.writeError(w, r, http.StatusUnauthorized, "signature_invalid",
			"X-Fishhawk-Signature is not valid hex",
			map[string]any{"error": err.Error()})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxPullRequestBundleBytes+1))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"could not read request body", map[string]any{"error": err.Error()})
		return
	}
	if len(body) > maxPullRequestBundleBytes {
		s.writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large",
			"pull-request body exceeds size cap",
			map[string]any{"limit_bytes": maxPullRequestBundleBytes})
		return
	}

	message := signing.ComputeMessage(body)
	if err := s.cfg.SigningRepo.Verify(r.Context(), runID, message, signature); err != nil {
		switch {
		case errors.Is(err, signing.ErrNotFound):
			s.writeError(w, r, http.StatusNotFound, "signing_key_not_found",
				"no signing key issued for this run", map[string]any{"run_id": runID.String()})
		case errors.Is(err, signing.ErrExpired):
			s.writeError(w, r, http.StatusUnauthorized, "signing_key_expired",
				"signing key TTL has passed", map[string]any{"run_id": runID.String()})
		case errors.Is(err, signing.ErrSignatureInvalid):
			s.writeError(w, r, http.StatusUnauthorized, "signature_invalid",
				"signature does not match the run's stored public key", nil)
		default:
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"signature verification failed", map[string]any{"error": err.Error()})
		}
		return
	}

	var pr pullRequestBody
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&pr); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "pull_request_invalid",
			"pull-request body could not be decoded",
			map[string]any{"error": err.Error()})
		return
	}
	if err := pr.validate(); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "pull_request_invalid",
			"pull-request body missing required fields",
			map[string]any{"error": err.Error()})
		return
	}

	contentHash := sha256Hex(body)

	// Idempotency: dedup on (stage_id, content_hash). The runner
	// computes content_hash over the canonical bytes it shipped, so
	// re-running an identical job returns the same artifact rather
	// than creating a duplicate.
	if existing, err := s.cfg.ArtifactRepo.GetByHash(r.Context(), stageID, contentHash); err == nil {
		s.writeJSON(w, r, http.StatusOK, pullRequestResponse{
			ID:          existing.ID,
			StageID:     existing.StageID,
			ContentHash: existing.ContentHash,
			PRNumber:    pr.PRNumber,
			PRURL:       pr.PRURL,
			HeadSHA:     pr.HeadSHA,
			Idempotent:  true,
		})
		return
	} else if !errors.Is(err, artifact.ErrNotFound) {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"check existing pull-request failed", map[string]any{"error": err.Error()})
		return
	}

	created, err := s.cfg.ArtifactRepo.Create(r.Context(), artifact.CreateParams{
		StageID:     stageID,
		Kind:        artifact.KindPullRequest,
		Content:     json.RawMessage(body),
		ContentHash: contentHash,
		// SchemaVersion intentionally nil for v0 — graduate to
		// pull_request_v1 in v0.x once the field shape settles.
	})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"create pull-request artifact failed", map[string]any{"error": err.Error()})
		return
	}

	auditPayload, _ := json.Marshal(map[string]any{
		"run_id":              runID.String(),
		"stage_id":            stageID.String(),
		"artifact_id":         created.ID.String(),
		"content_hash":        contentHash,
		"pr_number":           pr.PRNumber,
		"pr_url":              pr.PRURL,
		"branch":              pr.Branch,
		"head_sha":            pr.HeadSHA,
		"base_sha":            pr.BaseSHA,
		"files_changed_count": pr.FilesChangedCount,
		"size_bytes":          len(body),
	})
	systemKind := audit.ActorKind("system")
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  "pull_request_opened",
		ActorKind: &systemKind,
		Payload:   auditPayload,
	}); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"append audit entry failed", map[string]any{"error": err.Error()})
		return
	}

	// Backfill the run's pull_request_url so the threaded-runs view
	// (#216) can group every run on this PR with a single equality
	// query. Best-effort: a write failure logs but doesn't unwind
	// the upload — the PR artifact + audit row are already in
	// place, and a cron-style backfill could reconcile later.
	if _, err := s.cfg.RunRepo.SetRunPullRequestURL(r.Context(), runID, pr.PRURL); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"backfill pull_request_url failed",
			slog.String("run_id", runID.String()),
			slog.String("pr_url", pr.PRURL),
			slog.String("error", err.Error()),
		)
	}

	s.writeJSON(w, r, http.StatusCreated, pullRequestResponse{
		ID:          created.ID,
		StageID:     created.StageID,
		ContentHash: created.ContentHash,
		PRNumber:    pr.PRNumber,
		PRURL:       pr.PRURL,
		HeadSHA:     pr.HeadSHA,
		Idempotent:  false,
	})
}

// pullRequestResponse echoes the persisted artifact's identity back
// to the runner. PRNumber and HeadSHA are surfaced explicitly even
// though they're in the artifact body — they're the most operator-
// useful fields for log correlation, and including them avoids a
// second round-trip to read the artifact back.
type pullRequestResponse struct {
	ID          uuid.UUID `json:"id"`
	StageID     uuid.UUID `json:"stage_id"`
	ContentHash string    `json:"content_hash"`
	PRNumber    int       `json:"pr_number"`
	PRURL       string    `json:"pr_url"`
	HeadSHA     string    `json:"head_sha"`
	Idempotent  bool      `json:"idempotent"`
}
