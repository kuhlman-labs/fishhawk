// Package dispatchwatchdog runs the background ticker that fails
// stages stuck in 'dispatched' state past a configurable timeout.
//
// Stages enter 'dispatched' when the backend fires a GitHub
// workflow_dispatch but before the runner action checks in via
// trace upload. A stage that stays dispatched indefinitely
// indicates infrastructure failure (action timeout, GitHub-side
// dispatch failure, network partition) — MVP_SPEC §6 category C.
// Without a watchdog the stage sits forever and the run hangs;
// this ticker walks the dispatched-state set, computes
// `now - UpdatedAt`, and FailStages anything past the deadline.
//
// Mirrors the sla.Ticker pattern (E3.5 / approval SLA timeouts);
// the two could be folded into one package later, but keeping
// them separate makes the per-category configuration knobs
// independent and makes it easy to add a third watchdog
// (running-too-long, etc.) without coupling.
package dispatchwatchdog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryDispatchWatchdogElapsed is the audit-log category for
// the chained entry the watchdog writes when a stage transitions
// to failed-C. Stable so log scrapers and the compliance export
// can index on it.
const CategoryDispatchWatchdogElapsed = "dispatch_watchdog_elapsed"

// (Ticker.Advance is a plain `func(ctx, runID) error` rather than
// a named type so any caller can satisfy the dependency without a
// typed conversion. Production wires
// `orchestrator.Orchestrator.Advance` here via a small closure that
// drops the Outcome return value.)

// Ticker scans for stages stuck in dispatched state past Timeout
// and transitions them to failed with category C. Run() blocks
// until ctx is done; for production wiring start it on its own
// goroutine via the server config.
type Ticker struct {
	// Repo persists stages and applies the failed-C transition.
	// Required.
	Repo run.Repository

	// Audit appends the dispatch_watchdog_elapsed entry per
	// timeout. Required.
	Audit audit.Repository

	// Advance walks the run's state machine after the stage
	// transitions to failed-C. Without this the run sits in
	// pending forever once its dispatched stage times out.
	// Optional; nil skips the advancement and logs the gap.
	Advance func(ctx context.Context, runID uuid.UUID) error

	// Logger receives structured warnings about transient transition
	// errors. nil → slog.Default().
	Logger *slog.Logger

	// Interval is the tick period. Defaults to 60s when zero. The
	// ticker fires immediately on Run() start; the interval gates
	// subsequent ticks.
	Interval time.Duration

	// Timeout is the per-stage deadline measured from UpdatedAt.
	// A zero value means "never time out" — the ticker still runs
	// but never transitions anything; useful for the gradual-rollout
	// stage where the watchdog is enabled before a deadline is
	// chosen.
	Timeout time.Duration

	// Now is the clock used for deadline computation. Tests inject
	// a fake clock; production leaves it nil for time.Now.
	Now func() time.Time
}

// Run drives the ticker until ctx is cancelled. Each tick lists
// dispatched stages and transitions any that have elapsed.
// Errors at the per-stage level are logged but don't abort the
// loop — a stuck row shouldn't prevent timeouts on others.
func (t *Ticker) Run(ctx context.Context) error {
	if t.Repo == nil {
		return errors.New("dispatchwatchdog: ticker requires Repo")
	}
	if t.Audit == nil {
		return errors.New("dispatchwatchdog: ticker requires Audit")
	}
	interval := t.Interval
	if interval <= 0 {
		interval = 60 * time.Second
	}

	t.Tick(ctx)

	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			t.Tick(ctx)
		}
	}
}

// Tick performs one pass over dispatched stages. Exposed for
// tests so the deterministic-clock scenarios can drive the
// ticker step-by-step without spinning real timers.
func (t *Ticker) Tick(ctx context.Context) {
	logger := t.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if t.Timeout <= 0 {
		return
	}

	now := time.Now().UTC()
	if t.Now != nil {
		now = t.Now().UTC()
	}

	stages, err := t.Repo.ListStagesDispatched(ctx)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "dispatchwatchdog: list dispatched failed",
			slog.String("error", err.Error()))
		return
	}

	for _, s := range stages {
		t.handleStage(ctx, logger, now, s)
	}
}

// handleStage runs the deadline check on a single row and
// emits the transition + audit entry if elapsed. Per-row errors
// are logged but don't propagate — one bad row shouldn't black-
// hole the rest of the scan.
func (t *Ticker) handleStage(ctx context.Context, logger *slog.Logger, now time.Time, s *run.Stage) {
	deadline := s.UpdatedAt.Add(t.Timeout)
	if now.Before(deadline) {
		return
	}
	elapsed := now.Sub(s.UpdatedAt)
	reason := fmt.Sprintf("dispatch_watchdog: %s elapsed (deadline %s)",
		elapsed.Round(time.Second), t.Timeout)

	if _, err := run.FailStage(ctx, t.Repo, s.ID, run.FailureC, reason); err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "dispatchwatchdog: transition failed",
			slog.String("stage_id", s.ID.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	payload, _ := json.Marshal(map[string]any{
		"stage_id":         s.ID.String(),
		"timeout_seconds":  int64(t.Timeout.Seconds()),
		"elapsed_seconds":  int64(elapsed.Seconds()),
		"updated_at":       s.UpdatedAt.UTC().Format(time.RFC3339Nano),
		"transitioned_at":  now.UTC().Format(time.RFC3339Nano),
		"failure_category": string(run.FailureC),
	})
	stageID := s.ID
	systemKind := audit.ActorSystem
	if _, err := t.Audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     s.RunID,
		StageID:   &stageID,
		Timestamp: now.UTC(),
		Category:  CategoryDispatchWatchdogElapsed,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		// State is already failed; surface the audit gap loudly so
		// operators notice the chain integrity hole. Do NOT roll
		// back the transition — re-running the watchdog won't see
		// the stage again (it's no longer dispatched), so there's
		// nothing to recover.
		logger.LogAttrs(ctx, slog.LevelError, "dispatchwatchdog: audit append failed (state changed without entry)",
			slog.String("stage_id", stageID.String()),
			slog.String("run_id", s.RunID.String()),
			slog.String("error", err.Error()),
		)
	}

	// Walk the run's state machine. Without this, runs whose only
	// dispatched stage is now category-C-failed sit in pending
	// forever; the watchdog is the only path that produces this
	// failure, so the orchestrator never gets called otherwise.
	if t.Advance != nil {
		if err := t.Advance(ctx, s.RunID); err != nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "dispatchwatchdog: orchestrator advance failed",
				slog.String("run_id", s.RunID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("error", err.Error()),
			)
		}
	}
}
