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
		// running → awaiting_approval | succeeded | failed | cancelled
		{StageStateRunning, StageStateAwaitingApproval, true},
		{StageStateRunning, StageStateSucceeded, true},
		{StageStateRunning, StageStateFailed, true},
		{StageStateRunning, StageStateCancelled, true},
		{StageStateRunning, StageStatePending, false},
		// awaiting_approval → succeeded | failed | cancelled
		{StageStateAwaitingApproval, StageStateSucceeded, true},
		{StageStateAwaitingApproval, StageStateFailed, true},
		{StageStateAwaitingApproval, StageStateCancelled, true},
		{StageStateAwaitingApproval, StageStateRunning, false}, // no rewinding
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
		StageStatePending:          false,
		StageStateDispatched:       false,
		StageStateRunning:          false,
		StageStateAwaitingApproval: false,
		StageStateSucceeded:        true,
		StageStateFailed:           true,
		StageStateCancelled:        true,
	}
	for s, want := range cases {
		if got := s.IsTerminal(); got != want {
			t.Errorf("StageState(%q).IsTerminal() = %v, want %v", s, got, want)
		}
	}
}
