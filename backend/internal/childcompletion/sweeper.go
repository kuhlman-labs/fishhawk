// Package childcompletion runs the periodic sweeper that resolves
// parent runs parked in awaiting_children. It scans for stages in
// state awaiting_children (#455 / ADR-025 D4), groups them by parent
// run, fetches each parent's decomposed children, and transitions
// the parent stage to succeeded when every child reached a terminal
// state successfully, or to failed (category C) when any child
// terminated unsuccessfully.
//
// Cadence is operator-tuned (--child-completion-interval, default
// 60s). 60s is the upper bound on parent latency after the last
// child terminates — a separate direct-callback hook from a child's
// terminal transition is a worthwhile follow-up that would drop the
// happy-path latency to milliseconds.
package childcompletion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Advancer is the slice of orchestrator.Orchestrator the sweeper
// uses after transitioning a parent stage. Extracting the interface
// keeps the sweeper test-friendly without dragging the orchestrator
// package into the import graph.
type Advancer interface {
	Advance(ctx context.Context, runID uuid.UUID) error
}

// Sweeper periodically resolves awaiting_children parent stages.
// All dependencies are required; a nil Repository, Audit, or
// Advancer is a configuration error rather than a graceful skip
// — the sweeper exists exclusively to advance these stages and
// has nothing useful to do without any of them.
type Sweeper struct {
	Runs     run.Repository
	Audit    audit.Repository
	Advance  Advancer
	Logger   *slog.Logger
	Interval time.Duration
}

// Run blocks until ctx is cancelled, ticking every Interval to
// resolve parent stages whose children have all reached terminal
// states. Returns ctx.Err() when the parent context cancels;
// other errors from individual ticks are logged and the loop
// continues — a one-off DB hiccup shouldn't take the sweeper down.
func (s *Sweeper) Run(ctx context.Context) error {
	if s.Runs == nil || s.Audit == nil || s.Advance == nil {
		return errors.New("childcompletion: Runs, Audit, and Advance must all be set")
	}
	interval := s.Interval
	if interval == 0 {
		interval = 60 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	// Fire once at startup so an instance that just restarted picks
	// up any parents whose children completed during the gap.
	s.tickSafe(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			s.tickSafe(ctx)
		}
	}
}

// tickSafe wraps Tick so a single tick's error doesn't unwind the
// long-running Run loop.
func (s *Sweeper) tickSafe(ctx context.Context) {
	if err := s.Tick(ctx); err != nil {
		s.logger().LogAttrs(ctx, slog.LevelWarn, "childcompletion: tick failed",
			slog.String("error", err.Error()))
	}
}

// Tick performs one sweep of awaiting_children parent stages.
// Exported so tests can drive a single iteration without spinning
// up a goroutine.
func (s *Sweeper) Tick(ctx context.Context) error {
	stages, err := s.Runs.ListStagesAwaitingChildren(ctx)
	if err != nil {
		return fmt.Errorf("list awaiting_children stages: %w", err)
	}
	for _, stage := range stages {
		if err := s.resolveParent(ctx, stage); err != nil {
			s.logger().LogAttrs(ctx, slog.LevelWarn, "childcompletion: resolve parent failed",
				slog.String("parent_stage_id", stage.ID.String()),
				slog.String("parent_run_id", stage.RunID.String()),
				slog.String("error", err.Error()))
		}
	}
	return nil
}

// resolveParent inspects one awaiting_children stage's children and
// transitions the stage when every child has reached a terminal run
// state. No-op when any child is still in flight.
func (s *Sweeper) resolveParent(ctx context.Context, parentStage *run.Stage) error {
	parentRunID := parentStage.RunID
	children, err := s.Runs.ListRuns(ctx, run.ListRunsFilter{
		DecomposedFrom: &parentRunID,
		Limit:          100,
	})
	if err != nil {
		return fmt.Errorf("list children: %w", err)
	}
	if len(children) == 0 {
		// Fanout transition recorded the stage as awaiting_children
		// before children were committed, or the children rows were
		// deleted out-of-band. Either way we have nothing to wait
		// on. Leaving the stage parked is the safer default — an
		// operator can manually advance once they've inspected the
		// state.
		return nil
	}

	allTerminal := true
	var failedChildren []*run.Run
	failedChildIDs := make([]string, 0)
	for _, child := range children {
		if !child.State.IsTerminal() {
			allTerminal = false
			break
		}
		if child.State != run.StateSucceeded {
			failedChildren = append(failedChildren, child)
			failedChildIDs = append(failedChildIDs, child.ID.String())
		}
	}
	if !allTerminal {
		return nil
	}

	// #698 / #1081: when children failed but EVERY failed child's
	// implement failure is recoverable in decomposition (A/C/D-timeout,
	// or category B via the in-place recover path), leave the parent
	// parked in awaiting_children rather than resolving it to failed-C,
	// so an operator can re-drive the recoverable child without racing
	// this timer (the event-driven orchestrator path parks identically).
	// We deliberately do NOT log per parked parent on every tick: an
	// indefinitely-parked parent would otherwise emit an INFO line
	// every ~60s. Discoverability comes from the one-time
	// parent_awaiting_redrive audit emitted by the orchestrator path;
	// here we drop to debug so steady-state sweeps stay quiet.
	if len(failedChildren) > 0 && s.failedChildrenAllRecoverable(ctx, failedChildren) {
		s.logger().LogAttrs(ctx, slog.LevelDebug, "childcompletion: parent parked awaiting re-drive",
			slog.String("parent_run_id", parentRunID.String()),
			slog.String("parent_stage_id", parentStage.ID.String()),
			slog.Int("failed_child_count", len(failedChildren)),
		)
		return nil
	}

	anyFailed := len(failedChildren) > 0
	var (
		target     run.StageState
		completion *run.StageCompletion
	)
	if anyFailed {
		target = run.StageStateFailed
		cat := run.FailureC
		sort.Strings(failedChildIDs)
		reason := fmt.Sprintf("decomposed child runs failed: %v", failedChildIDs)
		completion = &run.StageCompletion{
			FailureCategory: &cat,
			FailureReason:   &reason,
		}
	} else {
		target = run.StageStateSucceeded
	}

	if _, err := s.Runs.TransitionStage(ctx, parentStage.ID, target, completion); err != nil {
		return fmt.Errorf("transition parent stage to %s: %w", target, err)
	}

	s.emitChildrenSettled(ctx, parentRunID, parentStage.ID, children, target)

	// Advance the parent to dispatch its next stage (review) or to
	// complete the run. The orchestrator's Advance is idempotent —
	// re-firing on a parent we've already advanced is a no-op.
	if err := s.Advance.Advance(ctx, parentRunID); err != nil {
		return fmt.Errorf("advance parent after children settled: %w", err)
	}
	s.logger().LogAttrs(ctx, slog.LevelInfo, "childcompletion: parent resolved",
		slog.String("parent_run_id", parentRunID.String()),
		slog.String("parent_stage_id", parentStage.ID.String()),
		slog.String("target_state", string(target)),
		slog.Int("child_count", len(children)),
	)
	return nil
}

// failedChildrenAllRecoverable reports whether every failed child run's
// implement-stage failure is recoverable in decomposition (A/C/D-timeout,
// or category B via the in-place recover path). Used by resolveParent to
// decide whether to park the parent awaiting re-drive. A failed child
// whose stages can't be listed, or whose implement stage carries no
// failure category, is treated as NOT recoverable — parking is only safe
// when every failure is positively confirmed recoverable, so an
// unclassifiable child resolves the parent to failed-C rather than
// parking it indefinitely.
func (s *Sweeper) failedChildrenAllRecoverable(ctx context.Context, failed []*run.Run) bool {
	for _, c := range failed {
		stages, err := s.Runs.ListStagesForRun(ctx, c.ID)
		if err != nil {
			s.logger().LogAttrs(ctx, slog.LevelWarn, "childcompletion: list child stages for recoverability check failed",
				slog.String("child_run_id", c.ID.String()),
				slog.String("error", err.Error()),
			)
			return false
		}
		if !run.ImplementFailureRecoverable(stages) {
			return false
		}
	}
	return true
}

// emitChildrenSettled writes a children_settled audit entry naming
// the resolved children and the parent stage's terminal target.
// Best-effort.
func (s *Sweeper) emitChildrenSettled(ctx context.Context, parentRunID, parentStageID uuid.UUID, children []*run.Run, target run.StageState) {
	ids := make([]string, 0, len(children))
	for _, c := range children {
		ids = append(ids, c.ID.String())
	}
	payload, err := json.Marshal(map[string]any{
		"child_run_ids":     ids,
		"parent_stage_id":   parentStageID.String(),
		"resolved_to_state": string(target),
	})
	if err != nil {
		s.logger().LogAttrs(ctx, slog.LevelWarn, "childcompletion: marshal children_settled payload",
			slog.String("error", err.Error()))
		return
	}
	systemKind := audit.ActorSystem
	if _, err := s.Audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     parentRunID,
		StageID:   &parentStageID,
		Timestamp: time.Now().UTC(),
		Category:  "children_settled",
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.logger().LogAttrs(ctx, slog.LevelWarn, "childcompletion: append children_settled",
			slog.String("error", err.Error()))
	}
}

func (s *Sweeper) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}
