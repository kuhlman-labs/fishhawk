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

// reconcileOrphanedReviewsPageSize bounds the running-run listing page the
// startup reconcile walks, mirroring orchestrator.reconcileStuckRunsPageSize.
const reconcileOrphanedReviewsPageSize = 100

// orphanedReviewStageKind describes one review-bearing stage's audit
// categories: the *_review_started dispatch marker, the *_review_failed
// terminal we synthesize, and the full terminal set (reviewed / skipped /
// failed) counted against ConfiguredAgents.
type orphanedReviewStageKind struct {
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
		started:   "plan_review_started",
		failed:    "plan_review_failed",
		terminals: []string{"plan_reviewed", "plan_review_skipped", "plan_review_failed"},
	},
	{
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
// This pages every running run and, per review-bearing stage, reads the
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
	offset := 0
	for {
		runs, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
			State:  string(run.StateRunning),
			Limit:  reconcileOrphanedReviewsPageSize,
			Offset: offset,
		})
		if err != nil {
			// A paging failure is systemic (not specific to one run), so
			// abort the sweep — best-effort applies per-run, not to the
			// listing itself. Mirrors orchestrator.ReconcileStuckRuns.
			return terminated, fmt.Errorf("server: reconcile orphaned reviews: list runs: %w", err)
		}
		if len(runs) == 0 {
			break
		}
		for _, r := range runs {
			did, err := s.reconcileRunOrphanedReviews(ctx, r.ID)
			if err != nil {
				failed++
				s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "review reconcile: skipped run on error",
					slog.String("run_id", r.ID.String()),
					slog.String("error", err.Error()),
				)
				continue
			}
			if did {
				terminated++
				s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "review reconcile: terminated orphaned review(s)",
					slog.String("run_id", r.ID.String()),
				)
			}
		}
		if len(runs) < reconcileOrphanedReviewsPageSize {
			break
		}
		offset += len(runs)
	}

	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "review reconcile: orphaned-review reconciliation complete",
		slog.Int("terminated", terminated),
		slog.Int("failed", failed),
	)
	return terminated, nil
}

// reconcileRunOrphanedReviews handles both review stages for one run. Returns
// whether it emitted any synthesized terminal entry for the run. When the
// implement stage was healed it also republishes fishhawk_audit_complete so
// the #947 review-pending presence gate reflects the now-terminal state.
func (s *Server) reconcileRunOrphanedReviews(ctx context.Context, runID uuid.UUID) (bool, error) {
	emittedAny := false
	emittedImplement := false
	for _, stage := range orphanedReviewStages {
		emitted, err := s.reconcileStageOrphanedReviews(ctx, runID, stage)
		if err != nil {
			return emittedAny, err
		}
		if emitted {
			emittedAny = true
			if stage.isImplement {
				emittedImplement = true
			}
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
	return emittedAny, nil
}

// reconcileStageOrphanedReviews synthesizes the missing terminal
// *_review_failed entries for one stage's CURRENT (latest-started) review
// round when that round was orphaned by a prior-process restart. Returns
// whether it emitted anything.
func (s *Server) reconcileStageOrphanedReviews(ctx context.Context, runID uuid.UUID, stage orphanedReviewStageKind) (bool, error) {
	started, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, stage.started)
	if err != nil {
		return false, fmt.Errorf("list %s for run %s: %w", stage.started, runID, err)
	}
	if len(started) == 0 {
		// No review was ever dispatched for this stage — nothing to heal.
		return false, nil
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
		return false, fmt.Errorf("decode %s payload for run %s: %w", stage.started, runID, err)
	}
	if payload.ConfiguredAgents <= 0 {
		// No reviewer was actually configured on this round — never pending.
		return false, nil
	}
	if latest.StageID == nil {
		// A started entry always carries its stage id; defend anyway rather
		// than emit a terminal entry with no stage anchor.
		return false, nil
	}

	// Boot-marker gate: a review whose latest started entry is NOT before the
	// current process boot is still legitimately in-flight in THIS process —
	// never fail it. At startup processStart == now, so every prior-process
	// dispatch predates it; the comparison is load-bearing only if the pass is
	// ever invoked mid-process-life.
	if !latest.Timestamp.Before(s.processStart) {
		return false, nil
	}

	// Count landed terminals for THIS round only: audit sequence strictly
	// greater than the latest started entry's sequence. A prior round's landed
	// verdicts carry a lower sequence and are excluded — the attempt-mixing
	// fix (binding condition 1).
	landed := 0
	for _, cat := range stage.terminals {
		entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, cat)
		if err != nil {
			return false, fmt.Errorf("list %s for run %s: %w", cat, runID, err)
		}
		for _, e := range entries {
			if e.Sequence > latest.Sequence {
				landed++
			}
		}
	}
	if landed >= payload.ConfiguredAgents {
		// Already settled for this round — idempotent no-op on a second pass.
		return false, nil
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
	return true, nil
}
