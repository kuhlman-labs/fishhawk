package campaign

import "testing"

// TestCampaignTransitions_AllowedAndForbidden table-tests the campaign state
// machine. Each named edge in campaignTransitions gets a true assertion;
// every terminal state rejects all outgoing edges; same-state is an
// idempotent no-op true.
func TestCampaignTransitions_AllowedAndForbidden(t *testing.T) {
	cases := []struct {
		from, to State
		want     bool
	}{
		// pending → running | cancelled | failed
		{StatePending, StateRunning, true},
		{StatePending, StateCancelled, true},
		{StatePending, StateFailed, true},
		{StatePending, StateSucceeded, false}, // can't skip running
		// running → succeeded | failed | cancelled | paused
		{StateRunning, StateSucceeded, true},
		{StateRunning, StateFailed, true},
		{StateRunning, StateCancelled, true},
		{StateRunning, StatePaused, true},
		{StateRunning, StatePending, false}, // no going back
		// paused → running | cancelled (the resume / halt-while-paused edges)
		{StatePaused, StateRunning, true},
		{StatePaused, StateCancelled, true},
		{StatePaused, StateSucceeded, false}, // can't succeed straight from paused
		{StatePaused, StateFailed, false},    // can't fail straight from paused
		{StatePaused, StatePending, false},   // no going back to pending
		// paused is non-terminal: pending can't reach it directly (only running can)
		{StatePending, StatePaused, false},
		// same-state idempotent no-ops
		{StatePending, StatePending, true},
		{StateRunning, StateRunning, true},
		{StatePaused, StatePaused, true},
		{StateSucceeded, StateSucceeded, true},
		{StateFailed, StateFailed, true},
		{StateCancelled, StateCancelled, true},
		// terminals reject every (different) outgoing edge
		{StateSucceeded, StateRunning, false},
		{StateSucceeded, StateFailed, false},
		{StateSucceeded, StateCancelled, false},
		{StateFailed, StateRunning, false},
		{StateFailed, StateSucceeded, false},
		{StateFailed, StateCancelled, false},
		{StateCancelled, StateRunning, false},
		{StateCancelled, StateSucceeded, false},
		{StateCancelled, StateFailed, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+"→"+string(tc.to), func(t *testing.T) {
			if got := ValidCampaignTransition(tc.from, tc.to); got != tc.want {
				t.Errorf("ValidCampaignTransition(%q, %q) = %v, want %v", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

// TestCampaignItemTransitions_AllowedAndForbidden table-tests the campaign
// item state machine — every named edge, the blocked↔pending/running cycle,
// each terminal state rejecting all outgoing edges, and same-state no-ops.
func TestCampaignItemTransitions_AllowedAndForbidden(t *testing.T) {
	cases := []struct {
		from, to ItemState
		want     bool
	}{
		// pending → blocked | running | cancelled | failed
		{ItemStatePending, ItemStateBlocked, true},
		{ItemStatePending, ItemStateRunning, true},
		{ItemStatePending, ItemStateCancelled, true},
		{ItemStatePending, ItemStateFailed, true},
		{ItemStatePending, ItemStateSucceeded, false}, // can't succeed without running
		// blocked → pending | running | cancelled
		{ItemStateBlocked, ItemStatePending, true},
		{ItemStateBlocked, ItemStateRunning, true},
		{ItemStateBlocked, ItemStateCancelled, true},
		{ItemStateBlocked, ItemStateFailed, false},    // a blocked item fails via pending/running, not directly
		{ItemStateBlocked, ItemStateSucceeded, false}, // can't succeed while blocked
		// running → succeeded | failed | cancelled | paused
		{ItemStateRunning, ItemStateSucceeded, true},
		{ItemStateRunning, ItemStateFailed, true},
		{ItemStateRunning, ItemStateCancelled, true},
		{ItemStateRunning, ItemStatePaused, true},
		{ItemStateRunning, ItemStatePending, false}, // no going back to pending
		{ItemStateRunning, ItemStateBlocked, false}, // running can't re-block
		// paused → running | cancelled (resume / halt-while-paused)
		{ItemStatePaused, ItemStateRunning, true},
		{ItemStatePaused, ItemStateCancelled, true},
		{ItemStatePaused, ItemStateSucceeded, false}, // can't succeed straight from paused
		{ItemStatePaused, ItemStateFailed, false},    // can't fail straight from paused
		{ItemStatePaused, ItemStateBlocked, false},   // can't re-block from paused
		// only running can reach paused (not pending/blocked directly)
		{ItemStatePending, ItemStatePaused, false},
		{ItemStateBlocked, ItemStatePaused, false},
		// same-state idempotent no-ops
		{ItemStatePending, ItemStatePending, true},
		{ItemStateBlocked, ItemStateBlocked, true},
		{ItemStateRunning, ItemStateRunning, true},
		{ItemStatePaused, ItemStatePaused, true},
		{ItemStateSucceeded, ItemStateSucceeded, true},
		{ItemStateFailed, ItemStateFailed, true},
		{ItemStateCancelled, ItemStateCancelled, true},
		// terminals reject every (different) outgoing edge
		{ItemStateSucceeded, ItemStateRunning, false},
		{ItemStateSucceeded, ItemStateFailed, false},
		{ItemStateSucceeded, ItemStateCancelled, false},
		{ItemStateFailed, ItemStateRunning, false},
		{ItemStateFailed, ItemStateSucceeded, false},
		{ItemStateFailed, ItemStatePending, false},
		{ItemStateCancelled, ItemStateRunning, false},
		{ItemStateCancelled, ItemStateSucceeded, false},
		{ItemStateCancelled, ItemStateBlocked, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+"→"+string(tc.to), func(t *testing.T) {
			if got := ValidCampaignItemTransition(tc.from, tc.to); got != tc.want {
				t.Errorf("ValidCampaignItemTransition(%q, %q) = %v, want %v", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

// TestInvalidTransitionError_Error confirms the error string carries the
// kind and the refused edge so a 409 surface can render it.
func TestInvalidTransitionError_Error(t *testing.T) {
	err := InvalidTransitionError{Kind: "campaign", From: "succeeded", To: "running"}
	want := "invalid campaign transition: succeeded → running"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	itemErr := InvalidTransitionError{Kind: "campaign_item", From: "failed", To: "running"}
	wantItem := "invalid campaign_item transition: failed → running"
	if got := itemErr.Error(); got != wantItem {
		t.Errorf("Error() = %q, want %q", got, wantItem)
	}
}
