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
//	pending                → failed
//	awaiting_host_dispatch → dispatched → running → failed   (parked local spawn abandoned, #1912)
//	dispatched             → running → failed   (e.g. agent never reported)
//	running                → failed             (e.g. policy violation post-trace)
//	awaiting_approval      → failed             (e.g. SLA elapsed, gate rejected)
//
// Park refusal: FailStage REFUSES an awaiting_children stage up-front,
// returning ErrStageParked without attempting any transition. A fan-in
// park is owned by its child slices; failing it (awaiting_children → failed
// is a legal base-table edge for the resolvers) would destroy the park.
// This refusal is required even with the compare-and-swap path below,
// because a CAS anchored at from=awaiting_children would LEGALLY perform
// exactly that destructive move.
//
// Concurrency contract (decided in #1907, restoring pre-#1906 semantics):
// when the repo provides the StageCASTransitioner capability (production
// postgresRepo), FailStage drives each step through TransitionStageFrom
// anchored to the state observed at that step. A concurrent flip landing
// between the load and a step is handled by class:
//
//   - Benign concurrent ADVANCE to a still-live, legally-failable state
//     (e.g. dispatched → running, or running → awaiting_approval): the CAS
//     refuses with StageStateChangedError, and failStageCAS RE-ANCHORS its
//     walk at the observed Actual state and retries (bounded). This ABSORBS
//     the advance and lands failed, exactly as the pre-CAS TransitionStage
//     walk did — so the 18 FailStage call sites (SLA, approvals, dispatch
//     watchdog, webhook dispatcher, trace, plan, pullrequest,
//     scope_completeness, lineage, deploy_trigger, reap) need NO per-site
//     CAS handling: their error branches fire only for the two typed
//     refusals below, retry exhaustion, and genuine repo errors — the same
//     benign-or-real classes their pre-CAS handling was written for.
//   - Flip to a TERMINAL state (another writer already settled the stage):
//     a typed StageStateChangedError, returned unchanged and never retried.
//   - Flip to the awaiting_children PARK (#1903 fan-in): a typed
//     StageStateChangedError (or the up-front ErrStageParked when the park
//     is visible at load), returned unchanged and never retried — the park
//     is owned by its children and must never be collapsed. Because every
//     retry attempt is itself an atomic row-locked CAS and a park observed
//     at ANY attempt refuses without retrying, the park-protection
//     invariant holds by construction across re-anchoring.
//   - Retry EXHAUSTION under pathological livelock (a state flip between
//     every attempt): the last StageStateChangedError is returned. For the
//     reap endpoint this is the documented 500-and-retry contract — the
//     reporter may re-POST and the ~1h dispatch watchdog is the eventual
//     backstop.
//
// In-memory fakes without the capability fall back to the plain
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

	// Non-CAS fallback: a parked awaiting_host_dispatch stage must first step
	// to dispatched (#1912), and dispatched stages must then walk through
	// Running; the state machine forbids skipping either edge (see
	// transition.go). Single-step otherwise.
	if stage.State == StageStateAwaitingHostDispatch {
		if _, err := repo.TransitionStage(ctx, stageID, StageStateDispatched, nil); err != nil {
			return nil, fmt.Errorf("FailStage: awaiting_host_dispatch → dispatched: %w", err)
		}
	}
	if stage.State == StageStateAwaitingHostDispatch || stage.State == StageStateDispatched {
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

// failStageCASMaxAttempts bounds the re-anchor loop below. Each attempt
// absorbs one benign concurrent advance; four is far beyond any realistic
// interleaving (a genuine livelock requires a state flip between every
// attempt) yet caps the pathological case so the loop terminates.
const failStageCASMaxAttempts = 4

// failStageCAS drives the FailStage walk through the compare-and-swap
// capability inside a bounded RE-ANCHOR loop (see FailStage's concurrency
// contract). Each attempt walks the canonical path from the anchored
// from-state: the awaiting_host_dispatch → dispatched step (#1912) anchors to
// awaiting_host_dispatch, the dispatched → running step anchors to dispatched,
// then the final → failed step anchors to the running state that step produced
// (or, when the walk is skipped, to the state anchored at loop entry).
//
// When a CAS step refuses with StageStateChangedError, reanchorTarget
// classifies the row-locked Actual state:
//
//   - Actual still live and legally failable (non-terminal, not the
//     awaiting_children park): RE-ANCHOR from = Actual and loop, absorbing
//     the benign concurrent advance. The loop top re-checks the dispatched
//     case, so a re-anchor at dispatched still walks through running.
//   - Actual terminal OR awaiting_children: the error propagates UNCHANGED
//     and is never retried — the two genuinely-benign refusals the caller
//     classifies as a no-op. Never retrying the park is what makes the
//     #1903 park-protection invariant hold by construction here.
//
// Exhausting the attempt bound returns the last StageStateChangedError (the
// documented retry-me contract). A non-StageStateChangedError returns
// immediately as a genuine repo error, exactly as before.
func failStageCAS(
	ctx context.Context,
	cas StageCASTransitioner,
	stageID uuid.UUID,
	from StageState,
	cat FailureCategory,
	reason string,
) (*Stage, error) {
	cat2 := cat
	reason2 := reason
	var lastErr error
	for attempt := 0; attempt < failStageCASMaxAttempts; attempt++ {
		if from == StageStateAwaitingHostDispatch {
			dispatched, err := cas.TransitionStageFrom(ctx, stageID, StageStateAwaitingHostDispatch, StageStateDispatched, nil)
			if err != nil {
				if next, ok := reanchorTarget(err); ok {
					from, lastErr = next, err
					continue
				}
				return nil, fmt.Errorf("FailStage: awaiting_host_dispatch → dispatched: %w", err)
			}
			from = dispatched.State
		}
		if from == StageStateDispatched {
			running, err := cas.TransitionStageFrom(ctx, stageID, StageStateDispatched, StageStateRunning, nil)
			if err != nil {
				if next, ok := reanchorTarget(err); ok {
					from, lastErr = next, err
					continue
				}
				return nil, fmt.Errorf("FailStage: dispatched → running: %w", err)
			}
			from = running.State
		}

		out, err := cas.TransitionStageFrom(ctx, stageID, from, StageStateFailed, &StageCompletion{
			FailureCategory: &cat2,
			FailureReason:   &reason2,
		})
		if err != nil {
			if next, ok := reanchorTarget(err); ok {
				from, lastErr = next, err
				continue
			}
			return nil, fmt.Errorf("FailStage: %s → failed: %w", from, err)
		}
		return out, nil
	}
	// Retry exhaustion under pathological livelock: surface the last typed
	// refusal so the reap endpoint's re-load yields the documented 500.
	return nil, lastErr
}

// reanchorTarget classifies a CAS step error for failStageCAS's re-anchor
// loop. It returns (Actual, true) — re-anchor and retry — only when err is a
// StageStateChangedError whose row-locked Actual state is still live and
// legally failable. A non-CAS error, or a genuinely-benign refusal (Actual
// terminal, or the awaiting_children park that must never be collapsed),
// returns ("", false) so the caller propagates the error unchanged.
func reanchorTarget(err error) (StageState, bool) {
	var sce StageStateChangedError
	if !errors.As(err, &sce) {
		return "", false
	}
	if sce.Actual.IsTerminal() || sce.Actual == StageStateAwaitingChildren {
		return "", false
	}
	return sce.Actual, true
}
