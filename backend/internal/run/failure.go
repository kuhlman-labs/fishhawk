package run

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrStageParked signals that FailStage refused to fail a stage that is in
// a live decomposition fan-in park (awaiting_children). That state is owned
// by the parent's child slices and resolved only by the fan-in resolvers
// (childcompletion sweeper, orchestrator resolveParent, consolidate
// handler); no ordinary failure reporter may collapse it. Callers classify
// this with errors.Is to treat the refusal as a benign no-op — see the reap
// backstop, which returns {transitioned:false} on it.
var ErrStageParked = errors.New("stage is parked awaiting children")

// FailStage transitions a stage to the Failed terminal state with
// the supplied category + reason. It walks the canonical state
// path from whichever non-terminal state the stage is currently
// in, so call sites never need to think about whether they're
// failing from running, awaiting_approval, dispatched, or pending:
//
//	pending           → failed
//	dispatched        → running → failed   (e.g. agent never reported)
//	running           → failed             (e.g. policy violation post-trace)
//	awaiting_approval → failed             (e.g. SLA elapsed, gate rejected)
//
// Park refusal: FailStage REFUSES an awaiting_children stage up-front,
// returning ErrStageParked without attempting any transition. A fan-in
// park is owned by its child slices; failing it (awaiting_children → failed
// is a legal base-table edge for the resolvers) would destroy the park.
// This refusal is required even with the compare-and-swap path below,
// because a CAS anchored at from=awaiting_children would LEGALLY perform
// exactly that destructive move.
//
// Concurrency: when the repo provides the StageCASTransitioner capability
// (production postgresRepo), FailStage drives each step through
// TransitionStageFrom anchored to the state observed at that step, so a
// concurrent park (or any other flip) landing AFTER the load surfaces as a
// typed StageStateChangedError instead of succeeding against a stale
// premise. In-memory fakes without the capability fall back to the plain
// TransitionStage walk, which retains a (fake-only) post-load window — no
// production repo takes that path.
//
// FailStage does NOT append an audit entry. Cause-specific entries
// (policy_evaluated, approval_sla_elapsed, approval_submitted) live
// at the call site and carry the structured payload that explains
// *why* the failure happened. Keeping the audit emission with the
// caller means the per-run hash chain stays in the caller's
// transaction-shaped control flow.
//
// Returns an error if cat isn't one of the four canonical
// categories — a typo here would silently corrupt the stage row,
// so reject early.
func FailStage(
	ctx context.Context,
	repo Repository,
	stageID uuid.UUID,
	cat FailureCategory,
	reason string,
) (*Stage, error) {
	if !cat.Valid() {
		return nil, fmt.Errorf("FailStage: invalid category %q", cat)
	}

	stage, err := repo.GetStage(ctx, stageID)
	if err != nil {
		return nil, fmt.Errorf("FailStage: get stage: %w", err)
	}

	// Up-front park refusal (see doc comment): never collapse a live fan-in
	// park. This fires for CAS and non-CAS repos alike, before any
	// transition is attempted.
	if stage.State == StageStateAwaitingChildren {
		return nil, fmt.Errorf("FailStage: %w: stage %s", ErrStageParked, stageID)
	}

	// Prefer the compare-and-swap capability when the repo provides it so a
	// state flip after the load above is refused atomically rather than
	// applied destructively. Fall back to the plain TransitionStage walk for
	// in-memory fakes that don't implement it.
	if cas, ok := repo.(StageCASTransitioner); ok {
		return failStageCAS(ctx, cas, stageID, stage.State, cat, reason)
	}

	// Non-CAS fallback: dispatched stages must walk through Running first;
	// the state machine forbids dispatched → failed without it (see
	// transition.go). Single-step otherwise.
	if stage.State == StageStateDispatched {
		if _, err := repo.TransitionStage(ctx, stageID, StageStateRunning, nil); err != nil {
			return nil, fmt.Errorf("FailStage: dispatched → running: %w", err)
		}
	}

	cat2 := cat
	reason2 := reason
	out, err := repo.TransitionStage(ctx, stageID, StageStateFailed, &StageCompletion{
		FailureCategory: &cat2,
		FailureReason:   &reason2,
	})
	if err != nil {
		return nil, fmt.Errorf("FailStage: %s → failed: %w", stage.State, err)
	}
	return out, nil
}

// failStageCAS drives the FailStage walk through the compare-and-swap
// capability, re-anchoring the expected from-state at each step: the
// dispatched → running step anchors to dispatched, then the final → failed
// step anchors to the running state that step produced (or, when the walk
// is skipped, to the state observed at load). Any flip landing between the
// load and a step surfaces as StageStateChangedError from
// TransitionStageFrom, which the reap backstop classifies as a benign
// no-op.
func failStageCAS(
	ctx context.Context,
	cas StageCASTransitioner,
	stageID uuid.UUID,
	from StageState,
	cat FailureCategory,
	reason string,
) (*Stage, error) {
	if from == StageStateDispatched {
		running, err := cas.TransitionStageFrom(ctx, stageID, StageStateDispatched, StageStateRunning, nil)
		if err != nil {
			return nil, fmt.Errorf("FailStage: dispatched → running: %w", err)
		}
		from = running.State
	}

	cat2 := cat
	reason2 := reason
	out, err := cas.TransitionStageFrom(ctx, stageID, from, StageStateFailed, &StageCompletion{
		FailureCategory: &cat2,
		FailureReason:   &reason2,
	})
	if err != nil {
		return nil, fmt.Errorf("FailStage: %s → failed: %w", from, err)
	}
	return out, nil
}
