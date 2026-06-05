package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryStageFixupTriggered is the audit-log category for the entry
// the fix-up handler writes when an implement stage is re-opened to
// route advisory implement-review concerns back to the agent (E22.X /
// #762). The payload carries the operator-selected concern indices and
// the resolved concern objects so the prompt renderer can deliver them
// as binding instructions, plus the bounded-pass receipt fields. This
// entry IS the durable record of the fix-up bound: the handler counts
// prior entries of this category for the stage to enforce the
// configured max-passes ceiling — there is no dedicated column.
const CategoryStageFixupTriggered = "stage_fixup_triggered"

// defaultMaxFixupPasses bounds the number of fix-up passes a single
// implement stage may consume. Default 1 — a fix-up is a bounded,
// operator-gated single pass, never an unbounded auto-loop. Making
// this spec-configurable is deferred (it needs a workflow-spec field;
// out of scope for the trigger surface).
const defaultMaxFixupPasses = 1

// fixupRequest is the JSON body of POST /v0/stages/{stage_id}/fixup.
// Concerns selects which recorded implement-review concerns (by their
// index in the stage's resolved concern set) to route back to the
// agent; it must be non-empty. Reason is an optional operator note
// recorded on the audit entry.
type fixupRequest struct {
	Concerns []int  `json:"concerns"`
	Reason   string `json:"reason"`
}

// handleFixupStage implements POST /v0/stages/{stage_id}/fixup.
//
// The implement-review fix-up (E22.X / #762) routes one or more
// advisory implement-review concerns (ADR-027 approve_with_concerns)
// back to the implement agent for a bounded, operator-gated fix-up
// pass, instead of the operator hand-editing the PR branch. It re-opens
// the implement stage parked at the review gate (awaiting_approval →
// pending) and hands off to the orchestrator, which re-dispatches the
// implement stage. The selected concerns are delivered to the agent as
// binding instructions by the prompt renderer (reading them back from
// the audit entry this handler writes).
//
// Distinct from POST /v0/stages/{stage_id}/retry: retry re-opens a
// FAILED stage and regenerates a fresh diff; fix-up re-opens a HEALTHY
// gate, commits onto the SAME PR branch, and is bounded
// (defaultMaxFixupPasses) by counting prior stage_fixup_triggered
// audit entries.
func (s *Server) handleFixupStage(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	if id.TokenID != "" && !hasScope(id, "write:stages") && !hasScope(id, "write:fixups") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:stages or write:fixups",
			map[string]any{"required_scope": "write:stages or write:fixups"})
		return
	}

	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "fixup_unconfigured",
			"fixup endpoint requires run + audit repositories", nil)
		return
	}

	stageID, err := uuid.Parse(r.PathValue("stage_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage_id must be a valid UUID",
			map[string]any{"field": "stage_id", "got": r.PathValue("stage_id")})
		return
	}

	var reqBody fixupRequest
	if r.Body != nil {
		if decErr := json.NewDecoder(r.Body).Decode(&reqBody); decErr != nil && !errors.Is(decErr, io.EOF) {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"request body must be valid JSON {concerns, reason}",
				map[string]any{"error": decErr.Error()})
			return
		}
	}
	if len(reqBody.Concerns) == 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"concerns must select at least one recorded implement-review concern",
			map[string]any{"field": "concerns"})
		return
	}

	// Pre-fetch the stage to get the RunID for the subject-binding guard
	// and the audit lookups below.
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

	// Subject-binding guard: an MCP run-bound token may only fix up
	// stages within its own run. Subject format is "mcp:run:<uuid>"
	// (set by bearerAuth middleware). Mirrors the retry handler.
	if strings.HasPrefix(id.Subject, "mcp:run:") {
		runIDStr := strings.TrimPrefix(id.Subject, "mcp:run:")
		subjectRunID, parseErr := uuid.Parse(runIDStr)
		if parseErr != nil {
			s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
				"mcp token subject is malformed", nil)
			return
		}
		if subjectRunID != stage.RunID {
			s.writeError(w, r, http.StatusForbidden, "cross_run_fixup",
				"mcp token may only fix up stages within its own run",
				map[string]any{
					"token_run_id": subjectRunID.String(),
					"stage_run_id": stage.RunID.String(),
				})
			return
		}
	}

	// Resolve the stage's recorded implement-review concerns from the
	// implement_reviewed audit entries (approve_with_concerns verdicts),
	// in append order. This is the set the operator's indices address.
	concerns, err := s.resolveImplementConcerns(r.Context(), stage.RunID, stageID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"resolve implement-review concerns failed", map[string]any{"error": err.Error()})
		return
	}
	if len(concerns) == 0 {
		s.writeError(w, r, http.StatusUnprocessableEntity, "fixup_not_applicable",
			"stage has no recorded approve_with_concerns implement-review concerns to route back to the agent",
			nil)
		return
	}

	// Validate the selected indices against the resolved set and collect
	// the selected concern objects (deduped, in selection order).
	selected, err := selectConcerns(concerns, reqBody.Concerns)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			err.Error(),
			map[string]any{"field": "concerns", "available": len(concerns)})
		return
	}

	// Count prior fix-up passes for this stage to enforce the bound.
	priorPasses, err := s.countFixupPasses(r.Context(), stage.RunID, stageID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"count prior fix-up passes failed", map[string]any{"error": err.Error()})
		return
	}

	dec, err := run.FixupStage(r.Context(), s.cfg.RunRepo, stageID, run.FixupOptions{
		PriorPassCount: priorPasses,
		MaxPasses:      defaultMaxFixupPasses,
	})
	if err != nil {
		switch {
		case errors.Is(err, run.ErrNotFound):
			s.writeError(w, r, http.StatusNotFound, "stage_not_found",
				"no stage with that id", nil)
			return
		case errors.Is(err, run.ErrFixupBudgetExhausted):
			s.writeError(w, r, http.StatusUnprocessableEntity, "fixup_budget_exhausted",
				err.Error(), map[string]any{"max_passes": defaultMaxFixupPasses, "used": priorPasses})
			return
		case errors.Is(err, run.ErrFixupNotApplicable):
			s.writeError(w, r, http.StatusUnprocessableEntity, "fixup_not_applicable",
				err.Error(), nil)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"fixup failed", map[string]any{"error": err.Error()})
		return
	}

	// Audit first so the fix-up intent (and the selected concerns the
	// prompt renderer reads back) is recorded even if the orchestrator
	// handoff below fails. Same posture as the retry handler.
	s.writeFixupAudit(r, dec, selected, reqBody.Concerns, reqBody.Reason, priorPasses)

	// The fix-up re-open lands the stage in pending; hand off to the
	// orchestrator to walk pending → dispatched and fire
	// workflow_dispatch. Orchestrator failures are logged but don't fail
	// the request: the audit row recorded the intent and the stage is in
	// pending, so an operator can re-fire Advance manually.
	if dec.Stage.State == run.StageStatePending && s.cfg.Orchestrator != nil {
		if _, err := s.cfg.Orchestrator.Advance(r.Context(), dec.Stage.RunID); err != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelError,
				"orchestrator advance failed for fixup",
				slog.String("run_id", dec.Stage.RunID.String()),
				slog.String("stage_id", dec.Stage.ID.String()),
				slog.String("error", err.Error()))
		}
		if updated, err := s.cfg.RunRepo.GetStage(r.Context(), dec.Stage.ID); err == nil {
			dec.Stage = updated
		}
	}

	// Sticky status comment (E20.4 / #330): a fix-up flips the stage back
	// to pending / dispatched; the status comment should reflect that.
	s.notifyStatusUpdate(r.Context(), dec.Stage.RunID, "stage_fixup")

	s.writeJSON(w, r, http.StatusOK, toStageResponse(dec.Stage))
}

// resolveImplementConcerns gathers the stage's recorded implement-
// review concerns from implement_reviewed audit entries with an
// approve_with_concerns verdict, in append order, flattened across
// every reviewer. This is the ordered set the operator's selected
// indices address.
func (s *Server) resolveImplementConcerns(ctx context.Context, runID, stageID uuid.UUID) ([]planreview.Concern, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "implement_reviewed")
	if err != nil {
		return nil, fmt.Errorf("list implement_reviewed audit entries: %w", err)
	}
	var concerns []planreview.Concern
	for _, e := range entries {
		if e.StageID == nil || *e.StageID != stageID {
			continue
		}
		var p planreview.ImplementReviewedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			// Tolerate a malformed payload rather than failing the whole
			// resolve — skip the entry; the audit log is append-only and a
			// corrupt blob shouldn't wedge the fix-up surface.
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"fixup: skipping malformed implement_reviewed payload",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("error", err.Error()))
			continue
		}
		if p.Verdict != planreview.VerdictApproveWithConcerns {
			continue
		}
		concerns = append(concerns, p.Concerns...)
	}
	return concerns, nil
}

// countFixupPasses returns the number of prior stage_fixup_triggered
// audit entries recorded for the stage — the durable fix-up-pass
// counter the bound is enforced against.
func (s *Server) countFixupPasses(ctx context.Context, runID, stageID uuid.UUID) (int, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryStageFixupTriggered)
	if err != nil {
		return 0, fmt.Errorf("list %s audit entries: %w", CategoryStageFixupTriggered, err)
	}
	n := 0
	for _, e := range entries {
		if e.StageID != nil && *e.StageID == stageID {
			n++
		}
	}
	return n, nil
}

// selectConcerns validates the operator-selected indices against the
// resolved concern set and returns the selected concern objects in
// selection order, de-duplicated. An out-of-range or duplicate index
// is rejected so the prompt renderer never sees a phantom concern.
func selectConcerns(all []planreview.Concern, indices []int) ([]planreview.Concern, error) {
	seen := map[int]struct{}{}
	out := make([]planreview.Concern, 0, len(indices))
	for _, i := range indices {
		if i < 0 || i >= len(all) {
			return nil, fmt.Errorf("concern index %d out of range [0,%d)", i, len(all))
		}
		if _, dup := seen[i]; dup {
			return nil, fmt.Errorf("concern index %d selected more than once", i)
		}
		seen[i] = struct{}{}
		out = append(out, all[i])
	}
	return out, nil
}

// writeFixupAudit appends a stage_fixup_triggered entry capturing the
// selected concern indices, the resolved concern objects (so the prompt
// renderer delivers them as binding instructions), the operator reason,
// and the bounded-pass receipt fields. Best-effort: the transition is
// already committed, so a failure here logs but doesn't unwind.
func (s *Server) writeFixupAudit(r *http.Request, dec *run.FixupDecision, selected []planreview.Concern, indices []int, reason string, priorPasses int) {
	id := IdentityFrom(r.Context())
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := audit.ActorUser

	passOrdinal := priorPasses + 1
	fields := map[string]any{
		"stage_id":         dec.Stage.ID.String(),
		"prior_state":      string(dec.PriorState),
		"selected_indices": indices,
		"concerns":         selected,
		"reason":           reason,
		"pass_ordinal":     passOrdinal,
		"max_passes":       defaultMaxFixupPasses,
		"remaining_budget": dec.RemainingBudget,
		"admissibility_reason": fmt.Sprintf("fix-up pass %d of %d; %d concern(s) routed back; via %s",
			passOrdinal, defaultMaxFixupPasses, len(selected), fixupScopeUsed(id)),
	}

	payload, _ := json.Marshal(fields)

	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        dec.Stage.RunID,
		StageID:      &dec.Stage.ID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryStageFixupTriggered,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.Error("audit append failed for fixup",
			"run_id", dec.Stage.RunID,
			"stage_id", dec.Stage.ID,
			"error", err.Error(),
		)
	}
}

// fixupScopeUsed returns the scope string that authorized the fix-up
// for the admissibility_reason receipt field.
func fixupScopeUsed(id Identity) string {
	if hasScope(id, "write:fixups") {
		return "write:fixups"
	}
	if hasScope(id, "write:stages") {
		return "write:stages"
	}
	return "session"
}
