package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// acceptanceRetirementDropReasonRunCancelled is the reason the THIRD
// acceptance_scenario_retirement_dropped writer (#3389, closing the E72.4
// cancel-path residual) stamps on the row it mints when a run carrying
// approved-but-unpersisted retire_scenario entries is cancelled BEFORE its
// acceptance stage spawned a runner. The runner-reported reasons (#3328) and
// approval_chain_unreadable (#3396) each cover a window AFTER the prompt
// fetch; this one covers the window before it, where no runner exists to
// report anything.
const acceptanceRetirementDropReasonRunCancelled = "run_cancelled_before_acceptance"

// acceptanceRetirementCancelUnreadableEvent is the WARN log event the cancel
// hook emits when the approval-chain read that would name the retirements
// fails — the floor of the surface (mirrors acceptanceRetirementsUnservedEvent).
const acceptanceRetirementCancelUnreadableEvent = "acceptance_retirements_cancel_chain_unreadable"

// The cancel_source vocabulary names which run-cancel sink fired. Every sink
// reachable AFTER plan approval is hooked; applies_to.go's
// abandonUnauditedOverrideRun is NOT (it fires at run creation, before any
// approval can carry a retirement, and its audit store is the thing that just
// failed).
const (
	// cancelSourceOperator is POST /v0/runs/{id}/cancel (the REST verb behind
	// fishhawk_cancel_run).
	cancelSourceOperator = "operator_cancel"
	// cancelSourceRunBudget is the per-run budget tripwire (trace.go
	// checkRunBudget).
	cancelSourceRunBudget = "run_budget_exceeded"
	// cancelSourceStageBudget is the BLOCKING per-stage budget tripwire
	// (trace.go checkStageBudget); an advisory breach cancels nothing.
	cancelSourceStageBudget = "stage_budget_exceeded"
	// cancelSourceStageCancelled is orchestrator.completeRun resolving the run
	// to cancelled because a stage was cancelled — the PR-closed-without-merge
	// path — delivered through the RunCancelledObserver seam.
	cancelSourceStageCancelled = "stage_cancelled"
)

// OnRunCancelled satisfies orchestrator.RunCancelledObserver: server.New
// wires the Server as the orchestrator's back-reference (exactly like
// ConsolidatedReview) so completeRun's cancelled resolution reaches the same
// helper the REST and budget sinks call.
func (s *Server) OnRunCancelled(ctx context.Context, runID uuid.UUID, source string) {
	s.recordAcceptanceRetirementsDroppedOnCancel(ctx, runID, source)
}

// recordAcceptanceRetirementsDroppedOnCancel is the ONE server-side helper
// every post-approval run-cancel sink calls immediately after its successful
// cancel transition (#3389). When the run carries approved retire_scenario
// entries and its acceptance stage never spawned a runner, it appends ONE
// acceptance_scenario_retirement_dropped row for the acceptance stage
// {run_id, stage_id, retired: [full entries], scenario_ids, reason
// run_cancelled_before_acceptance, cancel_source, acceptance_stage_state}
// (actor system) and refreshes the status comment so the #3392 renderer
// surfaces every dropped scenario id on the issue anchor NOW, not on the next
// rebuild. Best-effort throughout: nothing here unwinds the caller.
//
// SPAWN GATE — why `running` and terminal skip: the signed prompt fetch flips
// the acceptance stage dispatched→running (markStageRunningOnPromptFetch,
// prompt.go) in the same handler that serves the retirements
// (fillAcceptanceReplayFields), and the runner arms its deferred drop
// reporter the moment FetchPrompt returns (#3328). So `running` or later
// proves the wire delivered the retirements to a runner that owns the drop
// report from there; a row here would double-count it. pending /
// awaiting_host_dispatch / dispatched record: `dispatched` is a spawn attempt
// whose fetch may not have landed (the flip is best-effort), and a second row
// with a DIFFERENT reason on the dispatched race is the fail-safe direction.
//
// Terminal-FAILED runs are deliberately NOT hooked: `failed` is not absorbing
// (runRetryTransitions failed→running via revive/redrive), so a row at
// failure would be a false report once the run is revived and its acceptance
// stage persists the retirement. `cancelled` IS absorbing (no retry edge
// leaves it), so a row recorded here can never be contradicted later.
func (s *Server) recordAcceptanceRetirementsDroppedOnCancel(ctx context.Context, runID uuid.UUID, cancelSource string) {
	if s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil {
		return
	}
	retired, readErr := s.approvedScenarioRetirements(ctx, runID)
	if readErr != nil {
		// The #3396 shape: the failed read is the only source of the entries
		// it would name, so fall through to an empty-retired row carrying the
		// error rather than dropping the record silently.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"run cancel: acceptance_retirements_cancel_chain_unreadable: approval chain unreadable; recording an empty-retired drop row",
			slog.String("event", acceptanceRetirementCancelUnreadableEvent),
			slog.String("run_id", runID.String()),
			slog.String("cancel_source", cancelSource),
			slog.String("error", readErr.Error()))
	} else if len(retired) == 0 {
		// The common case: one audit read, nothing to report.
		return
	}
	if retired == nil {
		retired = []retiredScenarioEntry{}
	}

	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"run cancel: acceptance retirement drop: list stages failed; drop row NOT recorded",
			slog.String("run_id", runID.String()),
			slog.String("cancel_source", cancelSource),
			slog.String("error", err.Error()))
		return
	}
	var acceptance, planStage *run.Stage
	for _, st := range stages {
		switch st.Type {
		case run.StageTypeAcceptance:
			if acceptance == nil {
				acceptance = st
			}
		case run.StageTypePlan:
			if planStage == nil {
				planStage = st
			}
		}
	}
	target := acceptance
	acceptanceAbsent := false
	if target == nil {
		// Never drop the record for lack of a stage: the plan stage is the
		// one that recorded the approval, so stamp the row there.
		if planStage == nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"run cancel: acceptance retirement drop: run has neither an acceptance nor a plan stage; drop row NOT recorded",
				slog.String("run_id", runID.String()),
				slog.String("cancel_source", cancelSource))
			return
		}
		target = planStage
		acceptanceAbsent = true
	} else if acceptance.State == run.StageStateRunning || acceptance.State.IsTerminal() {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelDebug,
			"run cancel: acceptance retirement drop skipped; the acceptance stage spawned, so the runner's deferred reporter owns the drop",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", acceptance.ID.String()),
			slog.String("acceptance_stage_state", string(acceptance.State)),
			slog.String("cancel_source", cancelSource))
		return
	}
	stageID := target.ID

	ids := make([]string, 0, len(retired))
	for _, e := range retired {
		ids = append(ids, e.ID)
	}
	fields := map[string]any{
		"run_id":        runID.String(),
		"stage_id":      stageID.String(),
		"retired":       retired,
		"scenario_ids":  ids,
		"reason":        acceptanceRetirementDropReasonRunCancelled,
		"cancel_source": cancelSource,
	}
	if acceptanceAbsent {
		fields["acceptance_stage_absent"] = true
	} else {
		fields["acceptance_stage_state"] = string(acceptance.State)
	}
	if readErr != nil {
		fields["error"] = readErr.Error()
	}
	payload, _ := json.Marshal(fields)
	actorKind := audit.ActorSystem
	// Idempotent per (stage_id, reason) — NOT cancel_source, so two sinks
	// racing on one run collapse to one row (#3439). Atomic on the Postgres
	// repo via appendRetirementDropOnce's DedupedChainAppender path.
	appended, err := s.appendRetirementDropOnce(ctx, runID, stageID, acceptanceRetirementDropReasonRunCancelled,
		"run cancel: acceptance retirement drop", audit.ChainAppendParams{
			RunID:     runID,
			StageID:   &stageID,
			Timestamp: time.Now().UTC(),
			Category:  CategoryAcceptanceScenarioRetirementDropped,
			ActorKind: &actorKind,
			Payload:   payload,
		})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"run cancel: acceptance retirement drop: append audit entry failed",
			slog.String("run_id", runID.String()), slog.String("stage_id", stageID.String()),
			slog.String("cancel_source", cancelSource), slog.String("error", err.Error()))
		return
	}
	if !appended {
		return
	}
	s.notifyStatusUpdate(ctx, runID, CategoryAcceptanceScenarioRetirementDropped)
}
