package run

import (
	"errors"
	"testing"
)

// TestRunTransitions_AllowedAndForbidden table-tests the run state
// machine. Each row is a (from, to) pair with the expected verdict.
func TestRunTransitions_AllowedAndForbidden(t *testing.T) {
	cases := []struct {
		from, to State
		want     bool
	}{
		// pending: only running, cancelled, failed
		{StatePending, StatePending, true},
		{StatePending, StateRunning, true},
		{StatePending, StateCancelled, true},
		{StatePending, StateFailed, true},
		{StatePending, StateSucceeded, false},
		// running: terminal-only
		{StateRunning, StateSucceeded, true},
		{StateRunning, StateFailed, true},
		{StateRunning, StateCancelled, true},
		{StateRunning, StatePending, false},
		// terminals never transition (except idempotent same-state)
		{StateSucceeded, StateSucceeded, true},
		{StateSucceeded, StateRunning, false},
		{StateSucceeded, StateFailed, false},
		{StateFailed, StateRunning, false},
		{StateFailed, StateSucceeded, false},
		{StateCancelled, StateRunning, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+"→"+string(tc.to), func(t *testing.T) {
			if got := ValidRunTransition(tc.from, tc.to); got != tc.want {
				t.Errorf("ValidRunTransition(%q, %q) = %v, want %v", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

// TestRunRetryTransitions table-tests the narrow run-level reopen
// override (#698). Only failed → running is permitted; the table must
// not leak into ordinary transitions.
func TestRunRetryTransitions(t *testing.T) {
	cases := []struct {
		from, to State
		want     bool
	}{
		{StateFailed, StateRunning, true}, // the re-drive reopen
		// Everything else is refused — including the same pairs the
		// normal table allows, since RetryRun is a separate path.
		{StateFailed, StatePending, false},
		{StateFailed, StateSucceeded, false},
		{StateFailed, StateFailed, false}, // not in the retry table
		{StateRunning, StateRunning, false},
		{StateRunning, StateFailed, false},
		{StatePending, StateRunning, false},
		{StateCancelled, StateRunning, false},
		{StateSucceeded, StateRunning, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+"→"+string(tc.to), func(t *testing.T) {
			if got := ValidRunRetryTransition(tc.from, tc.to); got != tc.want {
				t.Errorf("ValidRunRetryTransition(%q, %q) = %v, want %v", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

func TestStageTransitions_AllowedAndForbidden(t *testing.T) {
	cases := []struct {
		from, to StageState
		want     bool
	}{
		// pending → dispatched | cancelled | failed
		{StageStatePending, StageStateDispatched, true},
		{StageStatePending, StageStateCancelled, true},
		{StageStatePending, StageStateFailed, true},
		{StageStatePending, StageStateRunning, false}, // skips dispatched
		// dispatched → running | failed | cancelled
		{StageStateDispatched, StageStateRunning, true},
		{StageStateDispatched, StageStateFailed, true},
		{StageStateDispatched, StageStateCancelled, true},
		{StageStateDispatched, StageStateAwaitingApproval, false},
		// running → awaiting_approval | awaiting_input | awaiting_scope_decision | succeeded | failed | cancelled
		{StageStateRunning, StageStateAwaitingApproval, true},
		{StageStateRunning, StageStateAwaitingInput, true},
		{StageStateRunning, StageStateAwaitingScopeDecision, true},
		{StageStateRunning, StageStateSucceeded, true},
		{StageStateRunning, StageStateFailed, true},
		{StageStateRunning, StageStateCancelled, true},
		{StageStateRunning, StageStatePending, false},
		// awaiting_approval → succeeded | failed | cancelled
		{StageStateAwaitingApproval, StageStateSucceeded, true},
		{StageStateAwaitingApproval, StageStateFailed, true},
		{StageStateAwaitingApproval, StageStateCancelled, true},
		{StageStateAwaitingApproval, StageStateRunning, false}, // no rewinding
		// awaiting_input → pending (resume) | succeeded | failed | cancelled (#1057)
		{StageStateAwaitingInput, StageStatePending, true}, // operator answered → resume in place
		{StageStateAwaitingInput, StageStateSucceeded, true},
		{StageStateAwaitingInput, StageStateFailed, true},
		{StageStateAwaitingInput, StageStateCancelled, true},
		{StageStateAwaitingInput, StageStateDispatched, false}, // resume routes through pending
		// awaiting_scope_decision → running (exempt resume) | failed (category-B) | cancelled (#1231)
		{StageStateAwaitingScopeDecision, StageStateRunning, true},    // operator exempted → resume to open the PR from the held commit
		{StageStateAwaitingScopeDecision, StageStateFailed, true},     // operator failed it → category-B
		{StageStateAwaitingScopeDecision, StageStateCancelled, true},  // manual halt
		{StageStateAwaitingScopeDecision, StageStatePending, false},   // never rewinds to a fresh dispatch
		{StageStateAwaitingScopeDecision, StageStateSucceeded, false}, // success only via the runner's PR-open, not the decision
		{StageStateAwaitingScopeDecision, StageStateDispatched, false},
		// terminal idempotency + lockdown
		{StageStateSucceeded, StageStateSucceeded, true},
		{StageStateSucceeded, StageStateRunning, false},
		{StageStateFailed, StageStateRunning, false},
		{StageStateCancelled, StageStateRunning, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+"→"+string(tc.to), func(t *testing.T) {
			if got := ValidStageTransition(tc.from, tc.to); got != tc.want {
				t.Errorf("ValidStageTransition(%q, %q) = %v, want %v", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

// TestStageFixupRecoveryTransitions table-tests the narrow fix-up
// recovery override (#788). Exactly three edges are valid; everything
// else — including unrelated edges through the recovery validator — is
// refused, because the recovery path un-fails a stage to a healthy prior
// state and must stay a separate, confined semantic.
func TestStageFixupRecoveryTransitions(t *testing.T) {
	cases := []struct {
		from, to StageState
		want     bool
	}{
		// The three recovery edges.
		{StageStateFailed, StageStateSucceeded, true},         // push_and_open_pr restore
		{StageStateFailed, StageStateAwaitingApproval, true},  // commit-yourself restore
		{StageStatePending, StageStateAwaitingApproval, true}, // review re-park restore
		// Unrelated edges the recovery validator must refuse.
		{StageStateSucceeded, StageStateFailed, false},
		{StageStateFailed, StageStatePending, false},           // that is the A/C RETRY edge, not recovery
		{StageStateAwaitingApproval, StageStatePending, false}, // that is the fix-up edge, not recovery
		{StageStateRunning, StageStateSucceeded, false},
		{StageStateFailed, StageStateFailed, false},
		{StageStateCancelled, StageStateAwaitingApproval, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+"→"+string(tc.to), func(t *testing.T) {
			if got := ValidStageFixupRecoveryTransition(tc.from, tc.to); got != tc.want {
				t.Errorf("ValidStageFixupRecoveryTransition(%q, %q) = %v, want %v", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

// TestFailedToSucceededLeaksOnlyThroughRecovery is the critical
// safety invariant (#788 CONDITION 3): the `failed → succeeded` edge —
// the one that would FAKE SUCCESS if it ever escaped the recovery verb —
// must be admitted ONLY by the recovery validator, never by the ordinary
// transition machine or the retry/fix-up override tables.
func TestFailedToSucceededLeaksOnlyThroughRecovery(t *testing.T) {
	if ValidStageTransition(StageStateFailed, StageStateSucceeded) {
		t.Error("ValidStageTransition admits failed → succeeded; it must not (would fake success)")
	}
	if ValidStageRetryTransition(StageStateFailed, StageStateSucceeded) {
		t.Error("ValidStageRetryTransition admits failed → succeeded; the retry path must not")
	}
	if ValidStageFixupTransition(StageStateFailed, StageStateSucceeded) {
		t.Error("ValidStageFixupTransition admits failed → succeeded; the fix-up path must not")
	}
	if !ValidStageFixupRecoveryTransition(StageStateFailed, StageStateSucceeded) {
		t.Error("ValidStageFixupRecoveryTransition must admit failed → succeeded (the recovery edge)")
	}
}

// TestStageReviseTransitions pins the dedicated plan-revise edge (#1099):
// the validator admits ONLY awaiting_approval → pending, and the base
// stageTransitions machine must NOT admit that edge (so the revise re-open
// is reachable only through the dedicated table).
func TestStageReviseTransitions(t *testing.T) {
	cases := []struct {
		from, to StageState
		want     bool
	}{
		// The single revise edge.
		{StageStateAwaitingApproval, StageStatePending, true},
		// Unrelated edges the revise validator must refuse.
		{StageStateSucceeded, StageStatePending, false}, // that is a fix-up edge, not revise
		{StageStateFailed, StageStatePending, false},
		{StageStateRunning, StageStatePending, false},
		{StageStatePending, StageStateAwaitingApproval, false},
		{StageStateAwaitingApproval, StageStateSucceeded, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+"→"+string(tc.to), func(t *testing.T) {
			if got := ValidStageReviseTransition(tc.from, tc.to); got != tc.want {
				t.Errorf("ValidStageReviseTransition(%q, %q) = %v, want %v", tc.from, tc.to, got, tc.want)
			}
		})
	}

	// The base machine must NOT carry the revise edge — it is reachable
	// only through the dedicated revise table (the #1099 invariant).
	if ValidStageTransition(StageStateAwaitingApproval, StageStatePending) {
		t.Error("ValidStageTransition admits awaiting_approval → pending; the base machine must not (revise edge belongs to the dedicated table)")
	}
}

func TestInvalidTransitionError_FormatsHumanReadable(t *testing.T) {
	err := InvalidTransitionError{Kind: "run", From: "pending", To: "succeeded"}
	want := "invalid run transition: pending → succeeded"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestInvalidTransitionError_IsErrorsTarget(t *testing.T) {
	err := error(InvalidTransitionError{Kind: "stage", From: "running", To: "pending"})
	var target InvalidTransitionError
	if !errors.As(err, &target) {
		t.Error("expected errors.As to extract InvalidTransitionError")
	}
	if target.Kind != "stage" {
		t.Errorf("kind = %q, want stage", target.Kind)
	}
}

func TestStateIsTerminal(t *testing.T) {
	cases := map[State]bool{
		StatePending:   false,
		StateRunning:   false,
		StateSucceeded: true,
		StateFailed:    true,
		StateCancelled: true,
	}
	for s, want := range cases {
		if got := s.IsTerminal(); got != want {
			t.Errorf("State(%q).IsTerminal() = %v, want %v", s, got, want)
		}
	}
}

func TestStageStateIsTerminal(t *testing.T) {
	cases := map[StageState]bool{
		StageStatePending:               false,
		StageStateDispatched:            false,
		StageStateRunning:               false,
		StageStateAwaitingApproval:      false,
		StageStateAwaitingInput:         false,
		StageStateAwaitingScopeDecision: false,
		StageStateSucceeded:             true,
		StageStateFailed:                true,
		StageStateCancelled:             true,
	}
	for s, want := range cases {
		if got := s.IsTerminal(); got != want {
			t.Errorf("StageState(%q).IsTerminal() = %v, want %v", s, got, want)
		}
	}
}
