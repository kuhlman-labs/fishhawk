package run

import "fmt"

// runTransitions enumerates allowed Run state transitions. Any
// (from, to) not present here is rejected. Same-state transitions
// (idempotent re-apply) are handled in ValidRunTransition, not here.
var runTransitions = map[State]map[State]struct{}{
	StatePending: {
		StateRunning:   {},
		StateCancelled: {},
		StateFailed:    {}, // setup-time failure (e.g., spec invalid before dispatch)
	},
	StateRunning: {
		StateSucceeded: {},
		StateFailed:    {},
		StateCancelled: {},
	},
}

// ValidRunTransition reports whether transitioning from→to is
// permitted. Same-state transitions are treated as valid no-ops so
// callers can be idempotent.
func ValidRunTransition(from, to State) bool {
	if from == to {
		return true
	}
	if from.IsTerminal() {
		return false
	}
	_, ok := runTransitions[from][to]
	return ok
}

// runRetryTransitions enumerates the explicit run-level reopen
// overrides off a terminal state — moves out of a terminal run
// state that the regular ValidRunTransition refuses.
//
// failed → running is the re-drive override (#698): a decomposition
// child run resolved to failed, but its implement-stage failure was
// in a retryable category (A/C, or D-timeout). An operator re-drives
// the child via POST /v0/runs/{run_id}/redrive, which un-terminals
// the run so orchestrator.Advance (a no-op on terminal runs) can
// re-dispatch the reset implement stage. This mirrors the
// stageRetryTransitions pattern exactly: a separate table consulted
// only by RetryRun, so it does not loosen ValidRunTransition for
// ordinary callers.
var runRetryTransitions = map[State]map[State]struct{}{
	StateFailed: {
		StateRunning: {},
	},
}

// ValidRunRetryTransition reports whether `from` is allowed to retry
// (reopen) into `to`. The retry path is intentionally narrow —
// callers that want a regular transition should keep using
// ValidRunTransition + TransitionRun.
func ValidRunRetryTransition(from, to State) bool {
	_, ok := runRetryTransitions[from][to]
	return ok
}

// stageTransitions enumerates allowed Stage state transitions.
//
// Pending → Dispatched: backend has emitted workflow_dispatch.
// Dispatched → Running: runner checked in and started executing.
// Dispatched → Failed: runner never started (category C).
// Running → AwaitingApproval: gate evaluation produced a blocking gate.
// Running → AwaitingInput: the planner emitted a clarification_request
//
//	and the plan stage parked for operator direction (#1057).
//
// Running → Succeeded: gate auto-passed (e.g., implicit no-gate stage).
// Running → Failed: any failure category.
// AwaitingApproval → Succeeded: approver said yes.
// AwaitingApproval → Failed: approver rejected, or D-category timeout.
// AwaitingInput → Pending: operator answered; the orchestrator re-opens
//
//	the parked plan stage to resume in the SAME run (pending-resume).
//
// AwaitingInput → Succeeded: the park resolved without re-dispatch.
// AwaitingInput → Failed: the park was abandoned, or its SLA timed out
//
//	(a D-category judgment, not an agent failure).
//
// Cancelled is reachable from any non-terminal state via manual halt.
var stageTransitions = map[StageState]map[StageState]struct{}{
	StageStatePending: {
		StageStateDispatched:       {},
		StageStateCancelled:        {},
		StageStateFailed:           {},
		StageStateAwaitingChildren: {},
	},
	StageStateDispatched: {
		StageStateRunning:   {},
		StageStateFailed:    {},
		StageStateCancelled: {},
	},
	StageStateRunning: {
		StageStateAwaitingApproval: {},
		StageStateAwaitingInput:    {},
		StageStateSucceeded:        {},
		StageStateFailed:           {},
		StageStateCancelled:        {},
	},
	StageStateAwaitingApproval: {
		StageStateSucceeded: {},
		StageStateFailed:    {},
		StageStateCancelled: {},
	},
	StageStateAwaitingChildren: {
		StageStateSucceeded: {},
		StageStateFailed:    {},
		StageStateCancelled: {},
	},
	StageStateAwaitingInput: {
		StageStatePending:   {}, // operator answered → resume in place
		StageStateSucceeded: {},
		StageStateFailed:    {},
		StageStateCancelled: {},
	},
}

// ValidStageTransition reports whether transitioning from→to is
// permitted. Idempotent same-state re-application is allowed.
func ValidStageTransition(from, to StageState) bool {
	if from == to {
		return true
	}
	if from.IsTerminal() {
		return false
	}
	_, ok := stageTransitions[from][to]
	return ok
}

// stageRetryTransitions enumerates the explicit retry overrides
// off the normal state machine — moves out of a terminal state
// that the regular ValidStageTransition refuses.
//
// Two retry paths live here:
//
//   - failed → awaiting_approval is the D-timeout retry: the SLA
//     elapsed but no plan needs to be regenerated, just re-open
//     the gate. The updated_at trigger restarts the SLA clock.
//   - failed → pending is the A/C retry (E8.6 #173): the agent
//     crashed (A) or the runner never reported in (C); we want
//     a fresh dispatch. The handler hands off to the orchestrator
//     after the transition; the orchestrator walks pending →
//     dispatched and fires workflow_dispatch.
//
// B and D-rejected are deliberately not retriable — the spec or
// the approver said no, the answer doesn't change without a fresh
// run.
var stageRetryTransitions = map[StageState]map[StageState]struct{}{
	StageStateFailed: {
		StageStateAwaitingApproval: {},
		StageStatePending:          {},
	},
}

// ValidStageRetryTransition reports whether `from` is allowed to
// retry into `to`. The retry path is intentionally narrow —
// callers that want a regular transition should keep using
// ValidStageTransition + TransitionStage.
func ValidStageRetryTransition(from, to StageState) bool {
	_, ok := stageRetryTransitions[from][to]
	return ok
}

// stageFixupTransitions enumerates the explicit fix-up override off
// the normal state machine — the implement-review fix-up re-open
// (E22.X / #762).
//
// Two fix-up edges live here, both selecting the implement stage's
// re-open target (pending) so the orchestrator walks pending →
// dispatched and re-dispatches the implement stage with the selected
// concerns delivered as binding instructions:
//
//   - awaiting_approval → pending is the commit-yourself flow: the
//     implement stage parked at its OWN review gate (awaiting_approval),
//     an advisory implement reviewer returned approve_with_concerns, and
//     an operator routed concerns back for a bounded fix-up pass.
//   - succeeded → pending is the push_and_open_pr re-open (#780): with
//     push_and_open_pr=true the implement stage SUCCEEDS (it commits and
//     opens the PR) and the human gate is a SEPARATE review stage parked
//     at awaiting_approval. The PR is open, not merged, so a fix-up
//     commit onto the same PR branch is still meaningful. This edge is
//     admitted only when run.FixupStage has confirmed the run's review
//     stage is still at its gate (see fixup.go); the same re-park of the
//     review stage (awaiting_approval → pending) reuses the first edge.
//
// This is deliberately a SEPARATE table from stageRetryTransitions:
// a fix-up is a distinct semantic from a retry (no failure to clear,
// no self_retry_count bump, re-opened from a healthy gate rather than
// a terminal failure), so widening stageRetryTransitions would conflate
// the two. The repo's TransitionStage consults this table in addition
// to ValidStageTransition so the fix-up edge is admissible there
// without loosening the normal machine for ordinary callers.
var stageFixupTransitions = map[StageState]map[StageState]struct{}{
	StageStateAwaitingApproval: {
		StageStatePending: {},
	},
	StageStateSucceeded: {
		StageStatePending: {},
	},
}

// ValidStageFixupTransition reports whether `from` is allowed to
// re-open into `to` via the fix-up path. The fix-up path is
// intentionally narrow — callers that want a regular transition
// should keep using ValidStageTransition + TransitionStage.
func ValidStageFixupTransition(from, to StageState) bool {
	_, ok := stageFixupTransitions[from][to]
	return ok
}

// stageReviseTransitions enumerates the explicit plan-gate REVISE
// override off the normal state machine — the plan-revise re-open
// (E22.X / #1099).
//
// One revise edge lives here: awaiting_approval → pending for a plan
// stage parked at its approval gate. A `revise` verdict (the third
// plan-gate option alongside approve/reject) re-plans IN PLACE: it
// re-opens the parked plan stage so the orchestrator walks pending →
// dispatched and re-dispatches the plan stage with the operator's
// binding design constraint injected and the prior plan carried as the
// revision base, then the run re-enters the normal review → approve
// gate.
//
// This is deliberately a SEPARATE table from stageFixupTransitions and
// stageRetryTransitions: a revise is a distinct semantic from a fix-up
// (it re-opens a PLAN stage, not an implement stage, and never touches a
// review stage or an implement diff) and from a retry (no failure to
// clear, re-opened from a healthy gate). The repo's TransitionStage
// consults this table in addition to ValidStageTransition so the revise
// edge is admissible there without loosening the normal machine for
// ordinary callers. The domain gate in run.RevisePlanStage (plan-stage
// type + awaiting_approval state + budget) is the real guard.
var stageReviseTransitions = map[StageState]map[StageState]struct{}{
	StageStateAwaitingApproval: {
		StageStatePending: {},
	},
}

// ValidStageReviseTransition reports whether `from` is allowed to
// re-open into `to` via the plan-revise path. The revise path is
// intentionally narrow and SEPARATE from every other table — callers
// that want a regular transition should keep using ValidStageTransition
// + TransitionStage, and only run.RevisePlanStage reaches this edge.
func ValidStageReviseTransition(from, to StageState) bool {
	_, ok := stageReviseTransitions[from][to]
	return ok
}

// stageFixupRecoveryTransitions enumerates the explicit fix-up
// RECOVERY override off the normal state machine — the edges used to
// restore a run to its pre-fix-up review gate when a fix-up
// re-dispatch FAILS (E22.X / #788).
//
// A fix-up re-opens an implement stage from a HEALTHY gate (the PR is
// open and mergeable); if the re-dispatched implement run then fails,
// the implement stage lands terminal `failed` and the review gate is
// gone — even though the original work is intact. A fix-up is a
// best-effort optional pass, so its failure must NOT destroy that
// work. Recovery un-fails the implement stage back to its captured
// prior state and re-parks the review stage that the fix-up re-parked:
//
//   - implement failed → succeeded restores the push_and_open_pr flow
//     (#780): the implement stage had SUCCEEDED (PR opened) before the
//     fix-up re-opened it. Restoring it to succeeded re-stamps ended_at
//     and clears the stale failure metadata (TransitionStage's
//     UpdateStageState sets failure_category/failure_reason directly,
//     not COALESCE).
//   - implement failed → awaiting_approval restores the commit-yourself
//     flow: the implement stage was its OWN gate at awaiting_approval
//     before the re-open.
//   - review pending → awaiting_approval restores the re-parked review
//     gate: the fix-up re-parked the review stage awaiting_approval →
//     pending (#780); recovery puts it back at its gate.
//
// This is deliberately a SEPARATE table from stageRetryTransitions and
// stageFixupTransitions. Admitting `failed → succeeded` is the critical
// safety hazard: if it leaked into the ordinary retry/transition path
// it would FAKE SUCCESS for any failed stage. Keeping it reachable only
// via ValidStageFixupRecoveryTransition (consulted by TransitionStage,
// guarded at the domain layer by RestoreFixupStage) confines that edge
// to the recovery verb.
var stageFixupRecoveryTransitions = map[StageState]map[StageState]struct{}{
	StageStateFailed: {
		StageStateSucceeded:        {},
		StageStateAwaitingApproval: {},
	},
	StageStatePending: {
		StageStateAwaitingApproval: {},
	},
}

// ValidStageFixupRecoveryTransition reports whether `from` is allowed
// to recover into `to` via the fix-up recovery path. The recovery path
// is intentionally narrow and SEPARATE from every other table — callers
// that want a regular transition should keep using ValidStageTransition
// + TransitionStage, and only run.RestoreFixupStage reaches this edge.
func ValidStageFixupRecoveryTransition(from, to StageState) bool {
	_, ok := stageFixupRecoveryTransitions[from][to]
	return ok
}

// InvalidTransitionError describes a refused state transition.
// Callers can errors.Is/As against it to surface a 409 Conflict at
// the HTTP layer.
type InvalidTransitionError struct {
	Kind string // "run" or "stage"
	From string
	To   string
}

func (e InvalidTransitionError) Error() string {
	return fmt.Sprintf("invalid %s transition: %s → %s", e.Kind, e.From, e.To)
}
