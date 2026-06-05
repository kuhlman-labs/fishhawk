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
// Running → Succeeded: gate auto-passed (e.g., implicit no-gate stage).
// Running → Failed: any failure category.
// AwaitingApproval → Succeeded: approver said yes.
// AwaitingApproval → Failed: approver rejected, or D-category timeout.
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
// awaiting_approval → pending is the single fix-up edge: an advisory
// implement reviewer returned approve_with_concerns, the implement
// stage parked at the review gate (awaiting_approval), and an operator
// routed one or more selected concerns back to the agent for a bounded
// fix-up pass. Re-opening to pending lets the orchestrator walk pending
// → dispatched and re-dispatch the implement stage with the concerns
// delivered as binding instructions.
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
}

// ValidStageFixupTransition reports whether `from` is allowed to
// re-open into `to` via the fix-up path. The fix-up path is
// intentionally narrow — callers that want a regular transition
// should keep using ValidStageTransition + TransitionStage.
func ValidStageFixupTransition(from, to StageState) bool {
	_, ok := stageFixupTransitions[from][to]
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
