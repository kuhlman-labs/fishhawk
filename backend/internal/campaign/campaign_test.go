package campaign

import "testing"

// TestState_IsTerminal table-tests the campaign terminal classifier:
// succeeded/failed/cancelled are terminal; pending/running/paused/awaiting_human
// are not.
//
// The awaiting_human row (E72.33 / #3660) is LOAD-BEARING, not decorative: every
// edge out of that state in campaignTransitions is reachable only because
// ValidCampaignTransition's IsTerminal short-circuit does not refuse it, and the
// server's #2681 terminal post-filter would rewrite its truthful attend_human_led
// action into `closed` the moment it became terminal. Making the state terminal
// reddens this row first.
func TestState_IsTerminal(t *testing.T) {
	cases := []struct {
		state State
		want  bool
	}{
		{StatePending, false},
		{StateRunning, false},
		{StatePaused, false},        // paused is a non-terminal overlay; a human resumes it
		{StateAwaitingHuman, false}, // derived, non-terminal: a relabel returns it to pending/running
		{StateSucceeded, true},
		{StateFailed, true},
		{StateCancelled, true},
		{State("bogus"), false}, // unknown is non-terminal (fail-open to "more work possible")
	}
	for _, tc := range cases {
		t.Run(string(tc.state), func(t *testing.T) {
			if got := tc.state.IsTerminal(); got != tc.want {
				t.Errorf("State(%q).IsTerminal() = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}

// TestItemState_IsTerminal table-tests the item terminal classifier:
// succeeded/failed/cancelled are terminal; pending/blocked/running are not.
func TestItemState_IsTerminal(t *testing.T) {
	cases := []struct {
		state ItemState
		want  bool
	}{
		{ItemStatePending, false},
		{ItemStateBlocked, false},
		{ItemStateRunning, false},
		{ItemStatePaused, false}, // paused is a non-terminal overlay
		{ItemStateSucceeded, true},
		{ItemStateFailed, true},
		{ItemStateCancelled, true},
		{ItemState("bogus"), false},
	}
	for _, tc := range cases {
		t.Run(string(tc.state), func(t *testing.T) {
			if got := tc.state.IsTerminal(); got != tc.want {
				t.Errorf("ItemState(%q).IsTerminal() = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}
