package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
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
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	if id.TokenID != "" && !hasScope(id, "write:stages") && !hasScope(id, "write:retries") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:stages or write:retries",
			map[string]any{"required_scope": "write:stages or write:retries"})
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

	// Pre-fetch the stage to get the RunID for subject-binding guard.
	stage, err := s.cfg.RunRepo.GetStage(r.Context(), stageID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "stage_not_found",
				"no stage with that id", nil)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get stage failed", map[string]any{"error": err.Error()})
		return
	}

	// Subject-binding guard: MCP tokens may only retry stages within
	// their own run. Subject format is "mcp:run:<uuid>" (set by
	// bearerAuth middleware at middleware.go).
	if strings.HasPrefix(id.Subject, "mcp:run:") {
		runIDStr := strings.TrimPrefix(id.Subject, "mcp:run:")
		subjectRunID, parseErr := uuid.Parse(runIDStr)
		if parseErr != nil {
			s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
				"mcp token subject is malformed", nil)
			return
		}
		if subjectRunID != stage.RunID {
			s.writeError(w, r, http.StatusForbidden, "cross_run_retry",
				"mcp token may only retry stages within its own run",
				map[string]any{
					"token_run_id": subjectRunID.String(),
					"stage_run_id": stage.RunID.String(),
				})
			return
		}
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

	// Best-effort fetch run for budget info in the audit receipt.
	// A failure here is logged and the audit receipt omits the budget
	// fields rather than failing the request.
	var runRow *run.Run
	if fetched, fetchErr := s.cfg.RunRepo.GetRun(r.Context(), dec.Stage.RunID); fetchErr == nil {
		runRow = fetched
	} else {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"get run for retry receipt failed",
			slog.String("run_id", dec.Stage.RunID.String()),
			slog.String("error", fetchErr.Error()))
	}

	// Audit first so the retry intent is recorded even if the
	// orchestrator handoff below fails. Same posture as the
	// approvals handler (E7.4 / approvals.go).
	s.writeRetryAudit(r, dec, runRow)

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
// prior failure category + reason, the retry receipt fields, and
// the actor that triggered the retry. Best-effort — the transition
// is already committed, so a failure here logs but doesn't unwind.
func (s *Server) writeRetryAudit(r *http.Request, dec *run.RetryDecision, runRow *run.Run) {
	id := IdentityFrom(r.Context())
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := audit.ActorUser

	ordinal := dec.Stage.SelfRetryCount

	fields := map[string]any{
		"stage_id":            dec.Stage.ID.String(),
		"prior_category":      string(dec.PriorCategory),
		"prior_reason":        dec.PriorReason,
		"prior_failure_class": dec.PriorCategory.Description(),
		"retry_ordinal":       ordinal,
		"admissibility_reason": fmt.Sprintf("category %s (%s); retry %d; via %s",
			string(dec.PriorCategory),
			dec.PriorCategory.Description(),
			ordinal,
			scopeUsed(id)),
	}
	if runRow != nil {
		remaining := runRow.MaxRetriesSnapshot - ordinal
		if remaining < 0 {
			remaining = 0
		}
		fields["remaining_budget"] = remaining
	}

	payload, _ := json.Marshal(fields)

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

// scopeUsed returns the scope string that authorized the retry for
// inclusion in the admissibility_reason receipt field.
func scopeUsed(id Identity) string {
	if hasScope(id, "write:retries") {
		return "write:retries"
	}
	if hasScope(id, "write:stages") {
		return "write:stages"
	}
	return "session"
}
