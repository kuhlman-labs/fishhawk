package campaign_test

import (
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
)

// item is a small constructor for an *Item in the engine tests.
func item(ref string, state campaign.ItemState, runID *uuid.UUID, deps ...string) *campaign.Item {
	return &campaign.Item{IssueRef: ref, State: state, RunID: runID, DependsOn: deps}
}

// TestNextEligible_PartiallyComplete asserts the eligible/blocked/running/
// done/failed partition over a partially-complete campaign: one succeeded
// (done), one running, one failed, one pending whose only dep succeeded
// (eligible), and one pending whose dep is still running (blocked).
func TestNextEligible_PartiallyComplete(t *testing.T) {
	run := uuid.New()
	items := []*campaign.Item{
		item("issue:1", campaign.ItemStateSucceeded, nil),          // done
		item("issue:2", campaign.ItemStateRunning, &run),           // running
		item("issue:3", campaign.ItemStateFailed, nil),             // failed
		item("issue:4", campaign.ItemStatePending, nil, "issue:1"), // dep done → eligible
		item("issue:5", campaign.ItemStatePending, nil, "issue:2"), // dep running → blocked
	}

	got := campaign.NextEligible(items)
	want := campaign.Eligibility{
		Eligible: []string{"issue:4"},
		Blocked:  []string{"issue:5"},
		Running:  []string{"issue:2"},
		Done:     []string{"issue:1"},
		Failed:   []string{"issue:3"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NextEligible =\n  %+v\nwant\n  %+v", got, want)
	}
}

// TestNextEligible_AbsentDepBlocks covers the defensive branch: a dependency
// ref that is not present in the campaign is treated as not-satisfied, so the
// item stays blocked rather than being dispatched against an unresolved edge.
func TestNextEligible_AbsentDepBlocks(t *testing.T) {
	items := []*campaign.Item{
		item("issue:1", campaign.ItemStatePending, nil, "issue:999"), // dep absent
	}
	got := campaign.NextEligible(items)
	if len(got.Eligible) != 0 {
		t.Errorf("eligible = %v, want none (absent dep is not satisfied)", got.Eligible)
	}
	if !reflect.DeepEqual(got.Blocked, []string{"issue:1"}) {
		t.Errorf("blocked = %v, want [issue:1]", got.Blocked)
	}
}

// TestNextEligible_RunningByRunLinkage covers the run-linkage branch: an item
// with a RunID set but a non-terminal (pending) state still counts as running.
func TestNextEligible_RunningByRunLinkage(t *testing.T) {
	run := uuid.New()
	items := []*campaign.Item{
		item("issue:1", campaign.ItemStatePending, &run), // run assigned, not terminal
	}
	got := campaign.NextEligible(items)
	if !reflect.DeepEqual(got.Running, []string{"issue:1"}) {
		t.Errorf("running = %v, want [issue:1] (RunID set, non-terminal)", got.Running)
	}
}

// TestNextEligible_CancelledIsTerminal covers the terminal cancelled branch: a
// cancelled item with no run and no deps must NOT be reported as eligible (it
// would otherwise fall through the default branch and could be re-dispatched).
func TestNextEligible_CancelledIsTerminal(t *testing.T) {
	items := []*campaign.Item{
		item("issue:1", campaign.ItemStateCancelled, nil), // terminal, no run, no deps
	}
	got := campaign.NextEligible(items)
	if len(got.Eligible) != 0 {
		t.Errorf("eligible = %v, want none (cancelled is terminal)", got.Eligible)
	}
	if !reflect.DeepEqual(got.Cancelled, []string{"issue:1"}) {
		t.Errorf("cancelled = %v, want [issue:1]", got.Cancelled)
	}
}

// TestNextEligible_PausedBucketed covers the E25.7 classification: a paused
// item carries a RunID and a non-terminal state, so without an explicit case
// it would fall into the Running catch-all. Assert it lands in Paused — NOT
// Running and NOT Eligible — so a paused item is never counted as in-flight or
// re-dispatched until a resume flips it back to running.
func TestNextEligible_PausedBucketed(t *testing.T) {
	run := uuid.New()
	items := []*campaign.Item{
		item("issue:1", campaign.ItemStatePaused, &run), // paused, run still linked
	}
	got := campaign.NextEligible(items)
	if !reflect.DeepEqual(got.Paused, []string{"issue:1"}) {
		t.Errorf("paused = %v, want [issue:1]", got.Paused)
	}
	if len(got.Running) != 0 {
		t.Errorf("running = %v, want none (paused must not be counted Running)", got.Running)
	}
	if len(got.Eligible) != 0 {
		t.Errorf("eligible = %v, want none (paused must not be re-dispatched)", got.Eligible)
	}
}

// TestDeriveState exercises one assertion per derived branch: pending,
// running, succeeded, failed. StateCancelled/StatePaused are operator overlays
// and are intentionally never derived.
func TestDeriveState(t *testing.T) {
	tests := []struct {
		name  string
		items []*campaign.Item
		want  campaign.State
	}{
		{
			name:  "no items is pending",
			items: nil,
			want:  campaign.StatePending,
		},
		{
			name: "all pending is pending",
			items: []*campaign.Item{
				item("issue:1", campaign.ItemStatePending, nil),
				item("issue:2", campaign.ItemStateBlocked, nil),
			},
			want: campaign.StatePending,
		},
		{
			name: "partial progress is running",
			items: []*campaign.Item{
				item("issue:1", campaign.ItemStateSucceeded, nil),
				item("issue:2", campaign.ItemStatePending, nil),
			},
			want: campaign.StateRunning,
		},
		{
			name: "any running is running",
			items: []*campaign.Item{
				item("issue:1", campaign.ItemStateRunning, nil),
				item("issue:2", campaign.ItemStatePending, nil),
			},
			want: campaign.StateRunning,
		},
		{
			name: "all succeeded is succeeded",
			items: []*campaign.Item{
				item("issue:1", campaign.ItemStateSucceeded, nil),
				item("issue:2", campaign.ItemStateSucceeded, nil),
			},
			want: campaign.StateSucceeded,
		},
		{
			name: "any failed is failed",
			items: []*campaign.Item{
				item("issue:1", campaign.ItemStateSucceeded, nil),
				item("issue:2", campaign.ItemStateFailed, nil),
			},
			want: campaign.StateFailed,
		},
		{
			// A paused item is never derived to StatePaused; with a succeeded
			// sibling the campaign reads running (partial progress), proving
			// derivation overlays nothing and treats paused as non-succeeding.
			name: "paused with progress is running, never paused",
			items: []*campaign.Item{
				item("issue:1", campaign.ItemStateSucceeded, nil),
				item("issue:2", campaign.ItemStatePaused, nil),
			},
			want: campaign.StateRunning,
		},
		{
			// A paused item is non-succeeding: it must NOT flip the campaign to
			// succeeded even when every other item succeeded.
			name: "paused blocks all-succeeded",
			items: []*campaign.Item{
				item("issue:1", campaign.ItemStateSucceeded, nil),
				item("issue:2", campaign.ItemStatePaused, nil),
			},
			want: campaign.StateRunning,
		},
		{
			// All paused (no other progress) derives pending, never StatePaused.
			name: "all paused is pending, never paused",
			items: []*campaign.Item{
				item("issue:1", campaign.ItemStatePaused, nil),
			},
			want: campaign.StatePending,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := campaign.DeriveState(tt.items); got != tt.want {
				t.Errorf("DeriveState = %q, want %q", got, tt.want)
			}
		})
	}
}
