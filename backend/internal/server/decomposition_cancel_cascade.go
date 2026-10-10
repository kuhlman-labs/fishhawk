package server

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/childcancel"
)

// cascadeCancelToDecomposedChildren cancels every non-terminal decomposition
// child of a run a cancel sink just cancelled (#4186). Every post-admission
// run-cancel sink calls it immediately after recordAcceptanceRetirementsDroppedOnCancel:
// handleCancelRun (operator_cancel, including the idempotent already-cancelled
// re-entry, so re-POSTing cancel converges a child a transient error left
// behind), checkRunBudget, checkStageBudget's blocking arm, and OnRunCancelled
// (orchestrator.completeRun's stage_cancelled resolution, which covers the
// PR-closed-without-merge path).
//
// A child whose implement stage is dispatched/running is CANCELLED, not
// refused: its run flips to cancelled exactly as POST /v0/runs/{child}/cancel
// does, the stage row and runner are untouched, and its audit row carries
// live_stage:true. A non-decomposed run lists zero children and writes
// nothing.
//
// Best-effort: a parent read or child-list failure WARN-logs and returns; a
// per-child transition or append failure WARN-logs that child. Nothing here
// unwinds or changes the caller's response.
func (s *Server) cascadeCancelToDecomposedChildren(ctx context.Context, parentRunID uuid.UUID, cancelSource string) {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return
	}
	parent, err := s.cfg.RunRepo.GetRun(ctx, parentRunID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"run cancel: decomposition child cascade: get parent run failed; children NOT cancelled",
			slog.String("run_id", parentRunID.String()),
			slog.String("cancel_source", cancelSource),
			slog.String("error", err.Error()))
		return
	}
	results, err := childcancel.CascadeFromParent(ctx, s.cfg.RunRepo, s.cfg.AuditRepo, parent, cancelSource)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"run cancel: decomposition child cascade: list children failed; children NOT cancelled",
			slog.String("run_id", parentRunID.String()),
			slog.String("cancel_source", cancelSource),
			slog.String("error", err.Error()))
		return
	}
	if len(results) == 0 {
		return
	}
	var cancelled, skipped, failed, live int
	for _, res := range results {
		switch res.Outcome {
		case childcancel.OutcomeCancelled:
			cancelled++
			if res.LiveStage {
				live++
			}
		case childcancel.OutcomeSkippedTerminal:
			skipped++
		case childcancel.OutcomeFailed:
			failed++
		}
		if res.Err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"run cancel: decomposition child cascade: child "+string(res.Outcome)+" with error",
				slog.String("run_id", parentRunID.String()),
				slog.String("child_run_id", res.ChildRunID.String()),
				slog.String("cancel_source", cancelSource),
				slog.String("outcome", string(res.Outcome)),
				slog.String("error", res.Err.Error()))
		}
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
		"run cancel: decomposition child cascade",
		slog.String("run_id", parentRunID.String()),
		slog.String("cancel_source", cancelSource),
		slog.Int("children", len(results)),
		slog.Int("cancelled", cancelled),
		slog.Int("skipped_terminal", skipped),
		slog.Int("failed", failed),
		slog.Int("live_stage", live))
}
