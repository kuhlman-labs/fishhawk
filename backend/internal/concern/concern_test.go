package concern

import (
	"errors"
	"testing"
)

// TestTransition_FullMatrix pins every edge of the lifecycle machine.
// Anything not explicitly allowed must fail with InvalidTransitionError.
func TestTransition_FullMatrix(t *testing.T) {
	states := []State{
		StateRaised, StateAddressedPending, StateAddressed,
		StateReopened, StateWaived, StateSuperseded, StateDeferred,
	}
	allowed := map[State][]State{
		StateRaised:           {StateAddressedPending, StateWaived, StateSuperseded, StateDeferred},
		StateAddressedPending: {StateAddressed, StateReopened, StateWaived, StateSuperseded, StateDeferred},
		StateAddressed:        {StateReopened},
		StateReopened:         {StateAddressedPending, StateWaived, StateSuperseded, StateDeferred},
		StateWaived:           {},
		StateSuperseded:       {},
		StateDeferred:         {},
	}
	for _, from := range states {
		want := map[State]bool{}
		for _, to := range allowed[from] {
			want[to] = true
		}
		for _, to := range states {
			err := Transition(from, to)
			if want[to] && err != nil {
				t.Errorf("Transition(%s, %s) = %v, want allowed", from, to, err)
			}
			if !want[to] {
				if err == nil {
					t.Errorf("Transition(%s, %s) = nil, want InvalidTransitionError", from, to)
					continue
				}
				var inv InvalidTransitionError
				if !errors.As(err, &inv) {
					t.Errorf("Transition(%s, %s) error type = %T, want InvalidTransitionError", from, to, err)
				} else if inv.From != from || inv.To != to {
					t.Errorf("InvalidTransitionError = %s -> %s, want %s -> %s", inv.From, inv.To, from, to)
				}
			}
		}
	}
}

// TestTransition_UnknownState ensures a state outside the enum (the
// tolerant TEXT column admits anything) has no outgoing edges.
func TestTransition_UnknownState(t *testing.T) {
	if err := Transition(State("bogus"), StateAddressed); err == nil {
		t.Fatal("Transition from unknown state succeeded, want error")
	}
}

// TestTransition_ReopenWinsOverConfirm pins the precedence rule in BOTH
// arrival orders (#964): heterogeneous reviewers emitting conflicting
// resolutions for the same concern must resolve to reopened
// deterministically, order-independently.
func TestTransition_ReopenWinsOverConfirm(t *testing.T) {
	// Confirm landed first, then a reopen: the reopen APPLIES.
	state := StateAddressedPending
	if err := Transition(state, StateAddressed); err != nil {
		t.Fatalf("confirm from addressed_pending: %v", err)
	}
	state = StateAddressed
	if err := Transition(state, StateReopened); err != nil {
		t.Fatalf("reopen after confirm must apply (reopen wins): %v", err)
	}
	state = StateReopened

	// Reopen landed first, then a late confirm: the confirm is REJECTED
	// with a loggable error — never a silent downgrade.
	err := Transition(state, StateAddressed)
	if err == nil {
		t.Fatal("confirm after reopen succeeded, want InvalidTransitionError (reopen wins)")
	}
	var inv InvalidTransitionError
	if !errors.As(err, &inv) {
		t.Fatalf("error type = %T, want InvalidTransitionError", err)
	}
	// The concern stays reopened.
	if !state.IsOpen() {
		t.Fatal("reopened concern must remain open after the rejected late confirm")
	}
}

func TestStateIsOpen(t *testing.T) {
	open := []State{StateRaised, StateAddressedPending, StateReopened}
	closed := []State{StateAddressed, StateWaived, StateSuperseded, StateDeferred, State("bogus")}
	for _, s := range open {
		if !s.IsOpen() {
			t.Errorf("%s.IsOpen() = false, want true", s)
		}
	}
	for _, s := range closed {
		if s.IsOpen() {
			t.Errorf("%s.IsOpen() = true, want false", s)
		}
	}
}
