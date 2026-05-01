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
		StageStateDispatched: {},
		StageStateCancelled:  {},
		StageStateFailed:     {},
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
