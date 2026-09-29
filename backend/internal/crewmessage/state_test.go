package crewmessage

import (
	"errors"
	"testing"
)

// TestStateMachine_PurePredicates table-tests ValidDisposition, StateFor and
// State.Terminal exhaustively over the closed sets plus the empty value and
// out-of-set values (including near-misses a coercing implementation would
// accept: a different case, a state name that is not a disposition).
func TestStateMachine_PurePredicates(t *testing.T) {
	dispositions := []struct {
		d     Disposition
		valid bool
		state State
	}{
		{DispositionAccepted, true, StateAccepted},
		{DispositionRejected, true, StateRejected},
		{DispositionExpired, true, StateExpired},
		{"", false, ""},
		{"maybe", false, ""},
		{"open", false, ""},
		{"Accepted", false, ""},
		{"accepted ", false, ""},
	}
	for _, tc := range dispositions {
		if got := ValidDisposition(tc.d); got != tc.valid {
			t.Errorf("ValidDisposition(%q) = %v, want %v", tc.d, got, tc.valid)
		}
		if got := StateFor(tc.d); got != tc.state {
			t.Errorf("StateFor(%q) = %q, want %q", tc.d, got, tc.state)
		}
		// Every valid disposition lands on a terminal state; nothing lands on
		// open.
		if tc.valid && !StateFor(tc.d).Terminal() {
			t.Errorf("StateFor(%q) = %q is not terminal", tc.d, StateFor(tc.d))
		}
	}

	states := []struct {
		s        State
		terminal bool
	}{
		{StateOpen, false},
		{StateAccepted, true},
		{StateRejected, true},
		{StateExpired, true},
		{"", false},
		{"closed", false},
		{"ACCEPTED", false},
	}
	for _, tc := range states {
		if got := tc.s.Terminal(); got != tc.terminal {
			t.Errorf("State(%q).Terminal() = %v, want %v", tc.s, got, tc.terminal)
		}
	}
}

// TestStateMachine_SentinelsAreDistinct pins that no sentinel wraps or aliases
// another, so errors.Is on one can never match a different refusal.
func TestStateMachine_SentinelsAreDistinct(t *testing.T) {
	all := []error{ErrInvalidDisposition, ErrAlreadyDisposed, ErrMessageNotFound, ErrRoundBoundExhausted, ErrNilEntry}
	for i, a := range all {
		for j, b := range all {
			if i != j && errors.Is(a, b) {
				t.Errorf("errors.Is(%v, %v) = true, want distinct sentinels", a, b)
			}
		}
	}
}
