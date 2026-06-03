package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryChildRedriven is the audit-log category for the chained
// entry the re-drive handler writes when a failed decomposition child
// run is re-opened. The payload carries the prior implement-stage
// failure category and reason so the audit trail records what was
// re-driven without forcing a reader to walk back to the prior
// stage-failed entries.
const CategoryChildRedriven = "child_redriven"

// handleRedriveChild implements POST /v0/runs/{run_id}/redrive.
//
// Re-drive is the operator recovery action for a decomposition parent
// parked in awaiting_children because every failed child was in a
// retryable category (A/C, or D-timeout) — see #698 and
// run.RetryableFailure. It re-opens the failed child run and its failed
// implement stage and re-dispatches it; on the child's next terminal
// transition the parked parent reconciles through the unchanged
// parent-resolution logic.
//
// Authorization (per the #698 plan's binding condition): re-drive is an
// OPERATOR action. It requires the operator retry scope (write:stages
// or write:retries, the same shape as POST /v0/stages/{stage_id}/retry)
// AND rejects any MCP subject-bound (agent) token outright. An agent
// must not re-drive any run — neither its own nor a sibling/child/
// parent — because re-opening a terminal run is not an agent-permitted
// action. We do not bind-match the agent token to the run; we reject
// all agent-subject tokens for this op.
func (s *Server) handleRedriveChild(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	// Reject agent (MCP subject-bound) tokens outright. Re-opening a
	// terminal run is an operator-only recovery action; an agent token
	// must never re-drive any run.
	if strings.HasPrefix(id.Subject, "mcp:run:") {
		s.writeError(w, r, http.StatusForbidden, "agent_token_forbidden",
			"re-drive is an operator action; agent (mcp) tokens may not re-drive any run", nil)
		return
	}
	if id.TokenID != "" && !hasScope(id, "write:stages") && !hasScope(id, "write:retries") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:stages or write:retries",
			map[string]any{"required_scope": "write:stages or write:retries"})
		return
	}

	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "redrive_unconfigured",
			"redrive endpoint requires run + audit repositories", nil)
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	dec, err := run.RedriveChild(r.Context(), s.cfg.RunRepo, runID)
	if err != nil {
		switch {
		case errors.Is(err, run.ErrNotFound):
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return
		case errors.Is(err, run.ErrRedriveNotApplicable):
			s.writeError(w, r, http.StatusUnprocessableEntity, "redrive_not_applicable",
				err.Error(), nil)
			return
		}
		var inv run.InvalidTransitionError
		if errors.As(err, &inv) {
			s.writeError(w, r, http.StatusConflict, "invalid_state_transition",
				err.Error(),
				map[string]any{"run_id": runID.String(), "from": inv.From, "to": inv.To})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"redrive failed", map[string]any{"error": err.Error()})
		return
	}

	// Audit first so the re-drive intent is recorded even if the
	// orchestrator handoff below fails. Same posture as the retry
	// handler (retry.go).
	s.writeRedriveAudit(r, runID, dec)

	// The re-opened implement stage is in pending; hand off to the
	// orchestrator to walk pending → dispatched and fire
	// workflow_dispatch. Un-terminal-ing the run (RedriveChild reopened
	// it failed → running) is what lets Advance act — it no-ops on
	// terminal runs. A failure here is logged but does not fail the
	// request: the audit row recorded the intent and the run is in
	// running; an operator can re-fire Advance manually.
	if s.cfg.Orchestrator != nil {
		if _, err := s.cfg.Orchestrator.Advance(r.Context(), runID); err != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelError,
				"orchestrator advance failed for redrive",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", dec.Stage.ID.String()),
				slog.String("error", err.Error()))
		}
		// Re-fetch the stage post-orchestrator so the response reflects
		// dispatched, not pending.
		if updated, err := s.cfg.RunRepo.GetStage(r.Context(), dec.Stage.ID); err == nil {
			dec.Stage = updated
		}
	}

	// Sticky status comment (E20.4 / #330): the child flipped from
	// failed back to running, so the status comment should re-render.
	s.notifyStatusUpdate(r.Context(), runID, "child_redrive")

	s.writeJSON(w, r, http.StatusOK, toRunResponse(dec.Run))
}

// writeRedriveAudit appends a child_redriven entry capturing the prior
// implement-stage failure category + reason and the actor that
// triggered the re-drive. Best-effort — the transitions are already
// committed, so a failure here logs but doesn't unwind.
func (s *Server) writeRedriveAudit(r *http.Request, runID uuid.UUID, dec *run.RedriveDecision) {
	id := IdentityFrom(r.Context())
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := audit.ActorUser

	payload, _ := json.Marshal(map[string]any{
		"run_id":              runID.String(),
		"stage_id":            dec.Stage.ID.String(),
		"prior_category":      string(dec.PriorCategory),
		"prior_reason":        dec.PriorReason,
		"prior_failure_class": dec.PriorCategory.Description(),
		"via":                 scopeUsed(id),
	})

	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &dec.Stage.ID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryChildRedriven,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.Error("audit append failed for redrive",
			"run_id", runID,
			"stage_id", dec.Stage.ID,
			"error", err.Error(),
		)
	}
}
