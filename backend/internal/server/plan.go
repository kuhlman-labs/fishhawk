package server

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// maxPlanBundleBytes caps the request body for plan upload. Plans
// are document-shaped JSON (steps, files, scope, etc.); even verbose
// ones rarely top a few KB. 256KB leaves an order-of-magnitude
// headroom and rejects pathological payloads early.
const maxPlanBundleBytes = 256 * 1024

// handleShipPlan implements POST /v0/runs/{run_id}/plan.
//
// Auth: same Ed25519 signing-key flow as /v0/runs/{run_id}/trace.
// The runner produced the signature over sha256(body) using the
// per-run private key issued by /v0/runs/{run_id}/signing-key.
//
// On success: validates the body against the standard_v1 schema,
// dedups against any existing plan with the same content_hash for
// the same stage (idempotent re-upload), creates an artifacts row,
// and appends a `plan_generated` audit entry. Returns 201 with the
// artifact's id + content_hash.
//
// Failure modes are explicit so the runner can map them to the
// right category:
//   - schema invalid     → 400 (category B / constraint)
//   - signature failures → 401 (matches trace upload)
//   - oversized          → 413
//   - storage failures   → 500 (category C — runner retries)
func (s *Server) handleShipPlan(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SigningRepo == nil || s.cfg.ArtifactRepo == nil ||
		s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "plan_upload_unconfigured",
			"plan upload requires signing, artifact, audit, and run repositories", nil)
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

	// Confirm the stage exists and belongs to the run before doing
	// signature math. A mismatched stage_id is a 404, not a forged
	// signature.
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

	body, err := io.ReadAll(io.LimitReader(r.Body, maxPlanBundleBytes+1))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"could not read request body", map[string]any{"error": err.Error()})
		return
	}
	if len(body) > maxPlanBundleBytes {
		s.writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large",
			"plan body exceeds size cap",
			map[string]any{"limit_bytes": maxPlanBundleBytes})
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

	// Validate payload against standard_v1. Schema errors are 400 so
	// the runner maps them to category-B (constraint failure) rather
	// than retrying — the agent's output is bad and re-shipping the
	// same bytes won't help.
	if err := plan.Validate(body); err != nil {
		// Transition the plan stage to failed-B so the run reflects
		// the bad output rather than getting stuck in `running` (#527).
		// The trace handler defers plan-stage transitions to this
		// handler; we are the only place that knows plan validation
		// failed. Best-effort: a TransitionStage error logs but
		// doesn't change the upload response — the operator's signal
		// is the 400, not the audit row.
		cat := run.FailureB
		reason := "plan_invalid: " + err.Error()
		if _, terr := s.cfg.RunRepo.TransitionStage(r.Context(), stageID,
			run.StageStateFailed, &run.StageCompletion{
				FailureCategory: &cat,
				FailureReason:   &reason,
			}); terr != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"plan upload: transition to failed-B after validation error failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("error", terr.Error()))
		}
		s.writeError(w, r, http.StatusBadRequest, "plan_invalid",
			"plan does not validate against standard_v1",
			map[string]any{"error": err.Error()})
		return
	}

	contentHash := sha256Hex(body)

	// Idempotency: if a plan with this content hash already exists
	// for this stage, return it without inserting a duplicate row.
	// Re-uploads from a retried runner job are common enough that the
	// expected-success case shouldn't fail with a unique-constraint
	// 500.
	if existing, err := s.cfg.ArtifactRepo.GetByHash(r.Context(), stageID, contentHash); err == nil {
		// Plan-ready comment-back retry hook (#245). The notify
		// path may have run from the trace handler before this
		// plan artifact existed, in which case it skipped silently;
		// fire here too so the comment lands. Audit-log dedup keeps
		// the second call a no-op when the first succeeded.
		s.notifyPlanReadyIfReady(r, runID, stage)
		s.writeJSON(w, r, http.StatusOK, planResponse{
			ID:            existing.ID,
			StageID:       existing.StageID,
			ContentHash:   existing.ContentHash,
			SchemaVersion: deref(existing.SchemaVersion),
			Idempotent:    true,
		})
		return
	} else if !errors.Is(err, artifact.ErrNotFound) {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"check existing plan failed", map[string]any{"error": err.Error()})
		return
	}

	schemaVersion := "standard_v1"
	created, err := s.cfg.ArtifactRepo.Create(r.Context(), artifact.CreateParams{
		StageID:       stageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &schemaVersion,
		Content:       json.RawMessage(body),
		ContentHash:   contentHash,
	})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"create plan artifact failed", map[string]any{"error": err.Error()})
		return
	}

	// Audit. The chained-append holds a row-lock on runs so two
	// concurrent uploads can't fork the chain. Failure here leaves
	// us with an artifact row and no audit entry, which is the
	// auditability inverse of what we want — surface 500 so the
	// runner retries (idempotent at the artifact layer via GetByHash).
	auditPayload, _ := json.Marshal(map[string]any{
		"run_id":         runID.String(),
		"stage_id":       stageID.String(),
		"artifact_id":    created.ID.String(),
		"content_hash":   contentHash,
		"schema_version": schemaVersion,
		"size_bytes":     len(body),
	})
	systemKind := audit.ActorKind("system")
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  "plan_generated",
		ActorKind: &systemKind,
		Payload:   auditPayload,
	}); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"append audit entry failed", map[string]any{"error": err.Error()})
		return
	}

	// Plan-ready comment-back hook (#245). Fires here so the
	// notifier sees the just-landed artifact even when the runner's
	// trace-then-plan upload order beats the trace-handler's
	// notifyPlanReady to running. Best-effort + dedup'd via the
	// audit log; safe to call alongside the trace-handler hook.
	s.notifyPlanReadyIfReady(r, runID, stage)

	s.writeJSON(w, r, http.StatusCreated, planResponse{
		ID:            created.ID,
		StageID:       created.StageID,
		ContentHash:   created.ContentHash,
		SchemaVersion: deref(created.SchemaVersion),
		Idempotent:    false,
	})
}

// notifyPlanReadyIfReady wraps the trace handler's notifyPlanReady
// with a "stage is terminal" guard for the plan-upload path (#245).
// The trace handler is reached only after a stage transitions
// terminally, so the trace-side hook can fire unconditionally; the
// plan-upload handler can be reached BEFORE the trace upload (a
// future runner reordering) and shouldn't comment until the stage
// is actually settled.
func (s *Server) notifyPlanReadyIfReady(r *http.Request, runID uuid.UUID, stage *run.Stage) {
	if stage.Type != run.StageTypePlan {
		return
	}
	if !stage.State.IsTerminal() && stage.State != run.StageStateAwaitingApproval {
		return
	}
	s.notifyPlanReady(r.Context(), runID, stage)
}

// planResponse is the JSON returned to the runner on plan upload.
// The runner doesn't need the full content back (it just sent it),
// so the response is the minimal acknowledgement.
type planResponse struct {
	ID            uuid.UUID `json:"id"`
	StageID       uuid.UUID `json:"stage_id"`
	ContentHash   string    `json:"content_hash"`
	SchemaVersion string    `json:"schema_version"`
	// Idempotent is true when the upload matched an existing plan
	// for this stage and no new row was inserted.
	Idempotent bool `json:"idempotent"`
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
