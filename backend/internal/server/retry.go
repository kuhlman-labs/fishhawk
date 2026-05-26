package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryStageRetried is the audit-log category for the chained
// entry the retry handler writes when a stage re-opens. The
// payload carries the prior failure category and reason so the
// audit trail records what was retried (without forcing a reader
// to walk back to the prior `stage_failed`-shaped entries).
const CategoryStageRetried = "stage_retried"

// handleRetryStage implements POST /v0/stages/{stage_id}/retry.
//
// Per-category retry semantics (E8.3 #146 + E8.6 #173):
//
//	A (agent failure)            → 200; failed → pending →
//	                               (orchestrator) → dispatched.
//	                               workflow_dispatch fires for the
//	                               same workflow_id + workflow_sha.
//	B (constraint/policy)        → 422; the workflow or spec
//	                               needs to change first.
//	C (infrastructure)           → 200; same flow as A — fresh
//	                               runner instance with a fresh
//	                               signing key.
//	D, sla_timeout sub-reason    → 200; failed → awaiting_approval,
//	                               failure metadata cleared,
//	                               updated_at trigger restarts the
//	                               SLA clock for the next ticker
//	                               pass.
//	D, gate-rejected sub-reason  → 422; the approver said no, a
//	                               fresh run is the right next
//	                               step.
//
// The high-level decision tree lives in run.RetryStage; this
// handler is the HTTP shim around it. For A/C the handler also
// invokes the orchestrator after the state transition (and after
// the audit write) to fire the actual workflow_dispatch.
// Orchestrator failures are logged but don't fail the request:
// the audit row is in place, the stage is in pending, an operator
// can re-fire Advance manually if needed.
func (s *Server) handleRetryStage(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriteScope(w, r, "write:stages") {
		return
	}
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "retry_unconfigured",
			"retry endpoint requires run + audit repositories", nil)
		return
	}

	stageID, err := uuid.Parse(r.PathValue("stage_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage_id must be a valid UUID",
			map[string]any{"field": "stage_id", "got": r.PathValue("stage_id")})
		return
	}

	dec, err := run.RetryStage(r.Context(), s.cfg.RunRepo, stageID)
	if err != nil {
		switch {
		case errors.Is(err, run.ErrNotFound):
			s.writeError(w, r, http.StatusNotFound, "stage_not_found",
				"no stage with that id", nil)
			return
		case errors.Is(err, run.ErrRetryNotImplemented):
			// No path returns this as of E8.6, but keep the mapping
			// so callers that switch on it stay sane.
			s.writeError(w, r, http.StatusNotImplemented, "retry_not_implemented",
				err.Error(), nil)
			return
		case errors.Is(err, run.ErrRetryNotApplicable):
			s.writeError(w, r, http.StatusUnprocessableEntity, "retry_not_applicable",
				err.Error(), nil)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"retry failed", map[string]any{"error": err.Error()})
		return
	}

	// Audit first so the retry intent is recorded even if the
	// orchestrator handoff below fails. Same posture as the
	// approvals handler (E7.4 / approvals.go).
	s.writeRetryAudit(r, dec)

	// A/C retries land the stage in pending; hand off to the
	// orchestrator to walk pending → dispatched and fire
	// workflow_dispatch. D-timeout retries land at
	// awaiting_approval and don't need the orchestrator (no
	// dispatch to fire — the gate just re-opens).
	if dec.Stage.State == run.StageStatePending && s.cfg.Orchestrator != nil {
		if _, err := s.cfg.Orchestrator.Advance(r.Context(), dec.Stage.RunID); err != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelError,
				"orchestrator advance failed for retry",
				slog.String("run_id", dec.Stage.RunID.String()),
				slog.String("stage_id", dec.Stage.ID.String()),
				slog.String("error", err.Error()))
			// Don't fail the request: the audit row recorded the
			// retry intent and the stage is in pending. Operator
			// can re-fire Advance manually. Re-fetch the stage so
			// the response reflects whatever state the orchestrator
			// did manage to reach before failing.
		}
		// Re-fetch the stage post-orchestrator so the response
		// reflects dispatched / awaiting_approval, not pending.
		if updated, err := s.cfg.RunRepo.GetStage(r.Context(), dec.Stage.ID); err == nil {
			dec.Stage = updated
		}
	}

	// Sticky status comment (E20.4 / #330). A retry flips a failed
	// stage back to pending / dispatched / awaiting_approval; the
	// status comment should reflect the new shape.
	s.notifyStatusUpdate(r.Context(), dec.Stage.RunID, "stage_retry")

	s.writeJSON(w, r, http.StatusOK, toStageResponse(dec.Stage))
}

// writeRetryAudit appends a stage_retried entry capturing the
// prior failure category + reason, plus the actor that triggered
// the retry. Best-effort — the transition is already committed,
// so a failure here logs but doesn't unwind.
func (s *Server) writeRetryAudit(r *http.Request, dec *run.RetryDecision) {
	id := IdentityFrom(r.Context())
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := audit.ActorUser

	payload, _ := json.Marshal(map[string]any{
		"stage_id":       dec.Stage.ID.String(),
		"prior_category": string(dec.PriorCategory),
		"prior_reason":   dec.PriorReason,
	})

	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        dec.Stage.RunID,
		StageID:      &dec.Stage.ID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryStageRetried,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.Error("audit append failed for retry",
			"run_id", dec.Stage.RunID,
			"stage_id", dec.Stage.ID,
			"error", err.Error(),
		)
	}
}
