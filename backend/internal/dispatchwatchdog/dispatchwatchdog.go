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
// `now - DispatchedAt`, and FailStages anything past the deadline.
//
// The deadline is measured from the DEDICATED dispatch timestamp
// stages.dispatched_at (migration 0072, #2744), NOT from the generic
// stages.updated_at the ~15s progress heartbeat (0070) bumps. Reading
// updated_at let a wedged-but-heartbeating stage forge the watchdog's
// clock; dispatched_at is stamped only on the transition INTO
// 'dispatched', so a heartbeat cannot advance it. The heartbeat is kept
// as the LIVENESS signal: the recorded reason and audit payload carry
// mode=never_checked_in (no heartbeat ever arrived — a truly infra-stuck
// stage) vs mode=wedged_after_checkin (heartbeats arrived, then the
// deadline elapsed anyway), so the two operator situations stop
// collapsing into one category-C reason.
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

// mode distinguishes the two operator situations the watchdog previously
// collapsed into one category-C reason (#2744). It rides both the recorded
// failure reason and the dispatch_watchdog_elapsed audit payload's `mode` key.
type mode string

const (
	// modeNeverCheckedIn: no runner heartbeat ever arrived for this dispatch
	// attempt before the deadline — a truly infra-stuck stage.
	modeNeverCheckedIn mode = "never_checked_in"
	// modeWedgedAfterCheckin: heartbeats arrived (attempt-relative) and the
	// dispatch-relative deadline elapsed anyway — checked in, then went quiet.
	modeWedgedAfterCheckin mode = "wedged_after_checkin"
)

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

	// Liveness supplies, per dispatched stage, its dedicated dispatch
	// timestamp, its updated_at, and its last heartbeat (#2744). Optional:
	// when nil, Run() type-asserts Repo for the capability. When NEITHER
	// carries it, Run() FAILS CLOSED (returns an error, the ticker does not
	// start) rather than falling back to the heartbeat-defeatable
	// Stage.UpdatedAt signal.
	Liveness run.DispatchLivenessLister

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

	// Timeout is the per-stage deadline measured from DispatchedAt
	// (the dedicated dispatch clock; UpdatedAt only for a legacy row
	// whose dispatched_at is nil). A zero value means "never time
	// out" — the ticker still runs
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
	// Resolve the dispatch-liveness capability. FAIL CLOSED when neither an
	// explicit Liveness nor a capable Repo carries it: the only fallback would
	// be Stage.UpdatedAt, the exact heartbeat-defeatable signal #2744 removes,
	// so the watchdog refuses to run rather than run defeatable.
	if t.liveness() == nil {
		return errors.New("dispatchwatchdog: ticker requires a run.DispatchLivenessLister (Repo does not carry the dispatch-liveness capability); refusing to start on the heartbeat-defeatable updated_at signal")
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

	lister := t.liveness()
	if lister == nil {
		// Run() refuses to start without the capability, so a live ticker
		// always has one. Guard anyway for a Tick() called directly in a test.
		logger.LogAttrs(ctx, slog.LevelWarn, "dispatchwatchdog: no dispatch-liveness capability; skipping tick")
		return
	}

	now := time.Now().UTC()
	if t.Now != nil {
		now = t.Now().UTC()
	}

	rows, err := lister.ListDispatchedStageLiveness(ctx)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "dispatchwatchdog: list dispatched liveness failed",
			slog.String("error", err.Error()))
		return
	}

	for _, l := range rows {
		t.handleStage(ctx, logger, now, l)
	}
}

// liveness resolves the dispatch-liveness capability: an explicit Liveness wins,
// else Repo is type-asserted for it. Returns nil when neither carries it.
func (t *Ticker) liveness() run.DispatchLivenessLister {
	if t.Liveness != nil {
		return t.Liveness
	}
	if l, ok := t.Repo.(run.DispatchLivenessLister); ok {
		return l
	}
	return nil
}

// handleStage runs the deadline check on a single row and
// emits the transition + audit entry if elapsed. Per-row errors
// are logged but don't propagate — one bad row shouldn't black-
// hole the rest of the scan.
func (t *Ticker) handleStage(ctx context.Context, logger *slog.Logger, now time.Time, l run.DispatchedStageLiveness) {
	// Deadline base is the DEDICATED dispatch clock. A nil DispatchedAt is a
	// legacy row that escaped the 0072 backfill; degrade to UpdatedAt — a
	// conservative fallback that reproduces exactly the pre-#2744 behaviour for
	// that one row rather than skipping it or instantly failing it.
	base := l.UpdatedAt
	if l.DispatchedAt != nil {
		base = *l.DispatchedAt
	} else {
		logger.LogAttrs(ctx, slog.LevelWarn, "dispatchwatchdog: dispatched_at nil; falling back to updated_at",
			slog.String("stage_id", l.StageID.String()),
			slog.String("run_id", l.RunID.String()),
		)
	}

	deadline := base.Add(t.Timeout)
	if now.Before(deadline) {
		return
	}
	elapsed := now.Sub(base)

	// Classify the two operator situations (#2744) from the liveness signal:
	// a heartbeat that arrived (attempt-relative, per run.ListDispatchedStageLiveness)
	// means the stage checked in and then went quiet; none ever arrived means a
	// truly infra-stuck stage that never reported.
	mode := modeNeverCheckedIn
	var lastHeartbeat string
	if l.LastHeartbeatAt != nil {
		mode = modeWedgedAfterCheckin
		lastHeartbeat = l.LastHeartbeatAt.UTC().Format(time.RFC3339Nano)
	}

	// The reason carries the mode token DERIVED from the mode constant (not a
	// separately-worded phrase), so the recorded failure_reason and the audit
	// payload's `mode` key cannot silently decouple: a wording-only edit that
	// changed the classification would move this token too (#2744 fix-up).
	var reason string
	if mode == modeWedgedAfterCheckin {
		reason = fmt.Sprintf("dispatch_watchdog[mode=%s]: %s since dispatch (deadline %s; last heartbeat %s ago)",
			mode, elapsed.Round(time.Second), t.Timeout, now.Sub(*l.LastHeartbeatAt).Round(time.Second))
	} else {
		reason = fmt.Sprintf("dispatch_watchdog[mode=%s]: %s since dispatch (deadline %s; no runner check-in)",
			mode, elapsed.Round(time.Second), t.Timeout)
	}

	if _, err := run.FailStage(ctx, t.Repo, l.StageID, run.FailureC, reason); err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "dispatchwatchdog: transition failed",
			slog.String("stage_id", l.StageID.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	var dispatchedAt any
	if l.DispatchedAt != nil {
		dispatchedAt = l.DispatchedAt.UTC().Format(time.RFC3339Nano)
	}
	var lastHeartbeatAt any
	if lastHeartbeat != "" {
		lastHeartbeatAt = lastHeartbeat
	}
	payload, _ := json.Marshal(map[string]any{
		"stage_id":          l.StageID.String(),
		"timeout_seconds":   int64(t.Timeout.Seconds()),
		"elapsed_seconds":   int64(elapsed.Seconds()),
		"updated_at":        l.UpdatedAt.UTC().Format(time.RFC3339Nano),
		"transitioned_at":   now.UTC().Format(time.RFC3339Nano),
		"failure_category":  string(run.FailureC),
		"mode":              string(mode),
		"dispatched_at":     dispatchedAt,
		"last_heartbeat_at": lastHeartbeatAt,
	})
	stageID := l.StageID
	systemKind := audit.ActorSystem
	if _, err := t.Audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     l.RunID,
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
			slog.String("run_id", l.RunID.String()),
			slog.String("error", err.Error()),
		)
	}

	// Walk the run's state machine. Without this, runs whose only
	// dispatched stage is now category-C-failed sit in pending
	// forever; the watchdog is the only path that produces this
	// failure, so the orchestrator never gets called otherwise.
	if t.Advance != nil {
		if err := t.Advance(ctx, l.RunID); err != nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "dispatchwatchdog: orchestrator advance failed",
				slog.String("run_id", l.RunID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("error", err.Error()),
			)
		}
	}
}
