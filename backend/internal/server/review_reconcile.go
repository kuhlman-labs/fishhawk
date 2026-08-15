package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// reconcileOrphanedReviewsPageSize bounds the run listing page the startup
// reconcile walks per state, mirroring orchestrator.reconcileStuckRunsPageSize.
const reconcileOrphanedReviewsPageSize = 100

// reconcileOrphanedReviewStates is the set of NON-TERMINAL run states the boot
// sweep pages. It is the #2712 root-cause fix: the sweep used to filter on
// run.StateRunning ALONE, which made the plan-review half of #1781 dead code.
// A run is CREATED pending (runs.go handleCreateRun) and, on the GATED plan
// path, advancePlanStageTerminal (plan.go) parks the plan stage at
// awaiting_approval WITHOUT calling Orchestrator.Advance — the first
// pending->running transition happens at plan APPROVAL (approvals.go). So
// EVERY plan review is dispatched while its run is still 'pending', and a
// daemon restart mid-plan-review left a strand no boot sweep could ever heal.
//
// ListRuns takes a single equality string, so the filter cannot be expressed
// as "not terminal"; the set is enumerated here and pinned against
// run.State.IsTerminal() by TestReconcileOrphanedReviewStates_CoversEveryNonTerminalState
// so a future non-terminal state cannot be silently omitted.
var reconcileOrphanedReviewStates = []run.State{run.StatePending, run.StateRunning}

// reconcileSkip* are the recorded reasons a stage's reconcile was a no-op.
// The boot sweep ignores them; the on-demand endpoint (#2712) reports them so
// an operator sees WHY nothing was healed instead of a silent success.
const (
	reconcileSkipNoStartedEntry = "no_review_started_entry"
	reconcileSkipNoConfigured   = "no_configured_agents"
	reconcileSkipNoStageID      = "started_entry_has_no_stage_id"
	reconcileSkipInFlight       = "review_dispatched_by_this_process"
	reconcileSkipAlreadySettled = "round_already_settled"
)

// reconciledStage is the per-stage outcome of one reconcile pass. It carries
// the counts the on-demand endpoint reports and the skip reason when the pass
// healed nothing.
type reconciledStage struct {
	Stage            string `json:"stage"`
	ConfiguredAgents int    `json:"configured_agents"`
	LandedBefore     int    `json:"landed_before"`
	Synthesized      int    `json:"synthesized"`
	Skipped          bool   `json:"skipped"`
	SkipReason       string `json:"skip_reason,omitempty"`
}

// orphanedReviewStageKind describes one review-bearing stage's audit
// categories: the *_review_started dispatch marker, the *_review_failed
// terminal we synthesize, and the full terminal set (reviewed / skipped /
// failed) counted against ConfiguredAgents.
type orphanedReviewStageKind struct {
	// label is the stage name the on-demand endpoint reports ("plan" /
	// "implement"), matching the fishhawk_await_review stage vocabulary.
	label     string
	started   string
	failed    string
	terminals []string
	// isImplement gates the audit-complete republish: only the implement
	// stage feeds the fishhawk_audit_complete review-presence gate (#947).
	isImplement bool
}

// orphanedReviewStages is the two review stages ReconcileOrphanedReviews
// heals, in a stable order.
var orphanedReviewStages = []orphanedReviewStageKind{
	{
		label:     "plan",
		started:   "plan_review_started",
		failed:    "plan_review_failed",
		terminals: []string{"plan_reviewed", "plan_review_skipped", "plan_review_failed"},
	},
	{
		label:       "implement",
		started:     "implement_review_started",
		failed:      "implement_review_failed",
		terminals:   []string{"implement_reviewed", "implement_review_skipped", "implement_review_failed"},
		isImplement: true,
	},
}

// orphanedReviewRestartReason is the ReviewFailedPayload.Reason stamped on a
// synthesized terminal entry. It names the restart so an operator reading the
// audit trail sees why the reviewer produced no real verdict.
const orphanedReviewRestartReason = "reviewer orphaned by daemon restart; no terminal review entry from the prior process"

// ReconcileOrphanedReviews is the one-shot startup recovery for the review
// twin of #727 (#1781): when fishhawkd restarts while an in-process plan or
// implement review is in flight, the detached reviewing goroutine dies with
// the process, so no terminal audit entry (*_reviewed / *_review_skipped /
// *_review_failed) ever lands. review_status is computed on demand from the
// audit trail (fishhawk-mcp reviewStatusFor: pending while landed terminal
// entries < ConfiguredAgents), so it stays 'pending' forever and await_review
// reports 'genuinely still running' indefinitely, wedging the gate.
//
// This pages every NON-TERMINAL run (reconcileOrphanedReviewStates: pending +
// running — see there for why 'pending' is load-bearing, #2712) and, per
// review-bearing stage, reads the
// LATEST *_review_started anchor (which carries ConfiguredAgents + Authority +
// StageID). For a review whose latest started entry predates the current
// process boot marker (s.processStart) with fewer landed terminals than
// ConfiguredAgents, it emits the missing count of terminal *_review_failed
// entries via the existing emitReviewFailed helper. That drives landed ==
// ConfiguredAgents, so reviewStatusFor returns a terminal 'failed' status,
// await_review resolves, and the operator can re-trigger. Mirrors the #1747
// twin (detached runner dying pre-report → terminal failed) and reuses the
// existing terminal writers.
//
// Attempt correlation (the binding condition): a stage can accumulate several
// review rounds (a fixup re-triggers the review, appending a fresh
// *_review_started). The CURRENT attempt is the latest *_review_started for
// the stage/category; landed terminals are counted ONLY with audit sequence
// strictly greater than that started entry's sequence, and that SAME entry's
// timestamp drives the dispatch-predates-boot comparison — never the earliest
// started nor a run-wide terminal count, which would mix a prior round's
// landed verdicts into the current round's tally.
//
// Best-effort PER RUN: a per-run error is logged and skipped so a single
// unresolvable run never wedges the boot sweep. Only a systemic ListRuns
// paging failure aborts (and is returned). Returns the count of runs whose
// reviews were terminated.
func (s *Server) ReconcileOrphanedReviews(ctx context.Context) (int, error) {
	if s.cfg.RunRepo == nil {
		return 0, fmt.Errorf("server: reconcile orphaned reviews: RunRepo is nil")
	}
	if s.cfg.AuditRepo == nil {
		return 0, fmt.Errorf("server: reconcile orphaned reviews: AuditRepo is nil")
	}

	terminated := 0
	failed := 0
	// healed de-duplicates the run count across the state filters. A run
	// cannot appear under two state filters in one pass, so this is defensive
	// against a future state addition rather than load-bearing today.
	healed := make(map[uuid.UUID]struct{})
	for _, state := range reconcileOrphanedReviewStates {
		offset := 0
		for {
			runs, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
				State:  string(state),
				Limit:  reconcileOrphanedReviewsPageSize,
				Offset: offset,
			})
			if err != nil {
				// A paging failure is systemic (not specific to one run), so
				// abort the sweep — best-effort applies per-run, not to the
				// listing itself. Mirrors orchestrator.ReconcileStuckRuns.
				return terminated, fmt.Errorf("server: reconcile orphaned reviews: list %s runs: %w", state, err)
			}
			if len(runs) == 0 {
				break
			}
			for _, r := range runs {
				stages, err := s.reconcileRunOrphanedReviews(ctx, r.ID)
				if err != nil {
					failed++
					s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "review reconcile: skipped run on error",
						slog.String("run_id", r.ID.String()),
						slog.String("error", err.Error()),
					)
					continue
				}
				if _, seen := healed[r.ID]; !seen && reconcileSynthesizedAny(stages) {
					healed[r.ID] = struct{}{}
					terminated++
					s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "review reconcile: terminated orphaned review(s)",
						slog.String("run_id", r.ID.String()),
						slog.String("run_state", string(state)),
					)
				}
			}
			if len(runs) < reconcileOrphanedReviewsPageSize {
				break
			}
			offset += len(runs)
		}
	}

	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "review reconcile: orphaned-review reconciliation complete",
		slog.Int("terminated", terminated),
		slog.Int("failed", failed),
	)
	return terminated, nil
}

// reconcileSynthesizedAny reports whether any stage in the pass emitted a
// synthesized terminal entry — the run-level "healed" predicate the boot
// sweep counts and the on-demand endpoint reports as `terminated`.
func reconcileSynthesizedAny(stages []reconciledStage) bool {
	for _, st := range stages {
		if st.Synthesized > 0 {
			return true
		}
	}
	return false
}

// reconcileRunOrphanedReviews handles both review stages for one run. Returns
// the per-stage outcomes (counts + skip reasons) so the same helper serves the
// boot sweep (which only needs "did anything land") and the on-demand endpoint
// (which reports WHY nothing did). When the implement stage was healed it also
// republishes fishhawk_audit_complete so the #947 review-pending presence gate
// reflects the now-terminal state.
func (s *Server) reconcileRunOrphanedReviews(ctx context.Context, runID uuid.UUID) ([]reconciledStage, error) {
	out := make([]reconciledStage, 0, len(orphanedReviewStages))
	emittedImplement := false
	for _, stage := range orphanedReviewStages {
		res, err := s.reconcileStageOrphanedReviews(ctx, runID, stage)
		if err != nil {
			return out, err
		}
		out = append(out, res)
		if res.Synthesized > 0 && stage.isImplement {
			emittedImplement = true
		}
	}
	if emittedImplement {
		// Re-derive and republish fishhawk_audit_complete so the review-
		// pending presence gate flips off now that the implement review's
		// terminal entries have landed (mirrors runImplementReviewInvocations
		// at trace.go:3163). Best-effort inside the same reconcile pass. The
		// test seam lets a minimal fake observe the call (production wires nil).
		if s.reconcileRecomputeAuditComplete != nil {
			s.reconcileRecomputeAuditComplete(ctx, runID)
		} else {
			s.recomputeAndPublishAuditComplete(ctx, runID)
		}
	}
	return out, nil
}

// reconcileStageOrphanedReviews synthesizes the missing terminal
// *_review_failed entries for one stage's CURRENT (latest-started) review
// round when that round was orphaned by a prior-process restart. Returns the
// per-stage outcome: the round's counts, and — when it healed nothing — the
// gate that refused it, so the on-demand endpoint can report a reason instead
// of a silent no-op. Every gate is unchanged from the #1781 boot sweep; only
// the return type carries more.
func (s *Server) reconcileStageOrphanedReviews(ctx context.Context, runID uuid.UUID, stage orphanedReviewStageKind) (reconciledStage, error) {
	out := reconciledStage{Stage: stage.label}
	skip := func(reason string) reconciledStage {
		out.Skipped = true
		out.SkipReason = reason
		return out
	}

	started, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, stage.started)
	if err != nil {
		return out, fmt.Errorf("list %s for run %s: %w", stage.started, runID, err)
	}
	if len(started) == 0 {
		// No review was ever dispatched for this stage — nothing to heal.
		return skip(reconcileSkipNoStartedEntry), nil
	}

	// The CURRENT attempt is the latest *_review_started (highest audit
	// sequence). A fixup re-triggers the review, appending a fresh started
	// entry; we correlate strictly to this round.
	latest := started[0]
	for _, e := range started[1:] {
		if e.Sequence > latest.Sequence {
			latest = e
		}
	}

	var payload planreview.ReviewStartedPayload
	if err := json.Unmarshal(latest.Payload, &payload); err != nil {
		return out, fmt.Errorf("decode %s payload for run %s: %w", stage.started, runID, err)
	}
	out.ConfiguredAgents = payload.ConfiguredAgents
	if payload.ConfiguredAgents <= 0 {
		// No reviewer was actually configured on this round — never pending.
		return skip(reconcileSkipNoConfigured), nil
	}
	if latest.StageID == nil {
		// A started entry always carries its stage id; defend anyway rather
		// than emit a terminal entry with no stage anchor.
		return skip(reconcileSkipNoStageID), nil
	}

	// Boot-marker gate: a review whose latest started entry is NOT before the
	// current process boot is still legitimately in-flight in THIS process —
	// never fail it. At startup processStart == now, so every prior-process
	// dispatch predates it; the comparison is load-bearing only if the pass is
	// ever invoked mid-process-life.
	if !latest.Timestamp.Before(s.processStart) {
		return skip(reconcileSkipInFlight), nil
	}

	// Count landed terminals for THIS round only: audit sequence strictly
	// greater than the latest started entry's sequence. A prior round's landed
	// verdicts carry a lower sequence and are excluded — the attempt-mixing
	// fix (binding condition 1).
	landed := 0
	for _, cat := range stage.terminals {
		entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, cat)
		if err != nil {
			return out, fmt.Errorf("list %s for run %s: %w", cat, runID, err)
		}
		for _, e := range entries {
			if e.Sequence > latest.Sequence {
				landed++
			}
		}
	}
	out.LandedBefore = landed
	if landed >= payload.ConfiguredAgents {
		// Already settled for this round — idempotent no-op on a second pass.
		return skip(reconcileSkipAlreadySettled), nil
	}

	// Emit exactly (ConfiguredAgents - landed) terminal *_review_failed
	// entries so landed reaches ConfiguredAgents and reviewStatusFor flips
	// from pending to a terminal failed status. The synthesized entries carry
	// a placeholder model ("") — a documented fidelity limitation (binding
	// condition 2): the dead goroutine's model/authority-per-reviewer state is
	// gone, but reviewStatusFor / await_review treat *_review_failed as a
	// terminal failed entry regardless of model.
	missing := payload.ConfiguredAgents - landed
	for i := 0; i < missing; i++ {
		s.emitReviewFailed(ctx, runID, *latest.StageID, stage.failed, payload.Authority, "", orphanedReviewRestartReason, false)
	}
	out.Synthesized = missing
	return out, nil
}
