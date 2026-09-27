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
// (done), one running, one failed whose dep is unsatisfied (stays Failed, not
// diverted to Restartable), one pending whose only dep succeeded (eligible),
// and one pending whose dep is still running (blocked).
func TestNextEligible_PartiallyComplete(t *testing.T) {
	run := uuid.New()
	items := []*campaign.Item{
		item("issue:1", campaign.ItemStateSucceeded, nil),          // done
		item("issue:2", campaign.ItemStateRunning, &run),           // running
		item("issue:3", campaign.ItemStateFailed, nil, "issue:2"),  // failed, dep running → stays Failed (not restartable)
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
// cancelled item must NEVER be reported as Eligible (auto-dispatch), even with
// no run and no deps (which would otherwise fall through the default branch). A
// no-dep, non-human-led cancelled item IS diverted to Restartable (#1729) — the
// operator restart path — but that is still disjoint from Eligible.
func TestNextEligible_CancelledIsTerminal(t *testing.T) {
	items := []*campaign.Item{
		item("issue:1", campaign.ItemStateCancelled, nil), // terminal, no run, no deps
	}
	got := campaign.NextEligible(items)
	if len(got.Eligible) != 0 {
		t.Errorf("eligible = %v, want none (cancelled is never auto-dispatched)", got.Eligible)
	}
	// deps-satisfied (no deps) + non-low autonomy → Restartable, not Cancelled.
	if !reflect.DeepEqual(got.Restartable, []string{"issue:1"}) {
		t.Errorf("restartable = %v, want [issue:1]", got.Restartable)
	}
	if len(got.Cancelled) != 0 {
		t.Errorf("cancelled = %v, want none (diverted to Restartable)", got.Cancelled)
	}
}

// TestNextEligible_RestartablePartition is the E32.9 (#1729) done-means for the
// Restartable partition: a deps-satisfied, non-autonomy:low cancelled item is
// Restartable; a deps-UNsatisfied cancelled item stays Cancelled; an autonomy:low
// cancelled item stays Cancelled (human-led work is never auto-surfaced for
// restart); and Restartable / Cancelled are disjoint (each cancelled item is in
// exactly one). A cancelled item is NEVER Eligible.
func TestNextEligible_RestartablePartition(t *testing.T) {
	items := []*campaign.Item{
		item("issue:1", campaign.ItemStateSucceeded, nil), // done — satisfies deps below
		// cancelled, deps satisfied, autonomy unset → Restartable.
		{IssueRef: "issue:2", State: campaign.ItemStateCancelled, DependsOn: []string{"issue:1"}},
		// cancelled, deps UNsatisfied → stays Cancelled (never restart against an open edge).
		{IssueRef: "issue:3", State: campaign.ItemStateCancelled, DependsOn: []string{"issue:999"}},
		// cancelled, deps satisfied, autonomy:low → stays Cancelled (human-led).
		{IssueRef: "issue:4", State: campaign.ItemStateCancelled, DependsOn: []string{"issue:1"}, Autonomy: "low"},
	}
	got := campaign.NextEligible(items)
	want := campaign.Eligibility{
		Done:        []string{"issue:1"},
		Restartable: []string{"issue:2"},
		Cancelled:   []string{"issue:3", "issue:4"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NextEligible =\n  %+v\nwant\n  %+v", got, want)
	}
	if len(got.Eligible) != 0 {
		t.Errorf("eligible = %v, want none (a cancelled item is never Eligible)", got.Eligible)
	}
}

// TestNextEligible_FailedRestartablePartition is the E32.37 (#1838) done-means
// for admitting FAILED items to the Restartable partition (mirroring #1729's
// cancelled arm): a deps-satisfied, non-autonomy:low failed item is Restartable;
// a deps-UNsatisfied failed item stays Failed; an autonomy:low failed item stays
// Failed (human-led work is never auto-surfaced for restart); Restartable /
// Failed are disjoint (each failed item is in exactly one). A failed item is
// NEVER Eligible.
func TestNextEligible_FailedRestartablePartition(t *testing.T) {
	items := []*campaign.Item{
		item("issue:1", campaign.ItemStateSucceeded, nil), // done — satisfies deps below
		// failed, deps satisfied, autonomy unset → Restartable.
		{IssueRef: "issue:2", State: campaign.ItemStateFailed, DependsOn: []string{"issue:1"}},
		// failed, deps UNsatisfied → stays Failed (never restart against an open edge).
		{IssueRef: "issue:3", State: campaign.ItemStateFailed, DependsOn: []string{"issue:999"}},
		// failed, deps satisfied, autonomy:low → stays Failed (human-led).
		{IssueRef: "issue:4", State: campaign.ItemStateFailed, DependsOn: []string{"issue:1"}, Autonomy: "low"},
	}
	got := campaign.NextEligible(items)
	want := campaign.Eligibility{
		Done:        []string{"issue:1"},
		Restartable: []string{"issue:2"},
		Failed:      []string{"issue:3", "issue:4"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NextEligible =\n  %+v\nwant\n  %+v", got, want)
	}
	if len(got.Eligible) != 0 {
		t.Errorf("eligible = %v, want none (a failed item is never Eligible)", got.Eligible)
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

// TestNextEligible_HumanLedDiverted is the E32.4 (#1551) done-means: over a
// mixed-autonomy DAG, a deps-satisfied autonomy:low item is diverted to
// HumanLed (never Eligible) while an autonomous sibling lands in Eligible, and
// a deps-UNsatisfied autonomy:low item stays Blocked (not HumanLed).
func TestNextEligible_HumanLedDiverted(t *testing.T) {
	items := []*campaign.Item{
		item("issue:1", campaign.ItemStateSucceeded, nil), // done — satisfies deps below
		// deps satisfied, autonomy:low → human-led (diverted out of Eligible).
		{IssueRef: "issue:2", State: campaign.ItemStatePending, DependsOn: []string{"issue:1"}, Autonomy: "low"},
		// deps satisfied, autonomy:high → eligible (autonomous sibling).
		{IssueRef: "issue:3", State: campaign.ItemStatePending, DependsOn: []string{"issue:1"}, Autonomy: "high"},
		// deps satisfied, autonomy unset → eligible (unknown defaults to non-human-led).
		{IssueRef: "issue:4", State: campaign.ItemStatePending, DependsOn: []string{"issue:1"}},
		// deps UNsatisfied, autonomy:low → stays Blocked, NOT HumanLed.
		{IssueRef: "issue:5", State: campaign.ItemStatePending, DependsOn: []string{"issue:999"}, Autonomy: "low"},
	}
	got := campaign.NextEligible(items)
	want := campaign.Eligibility{
		Eligible: []string{"issue:3", "issue:4"},
		HumanLed: []string{"issue:2"},
		Blocked:  []string{"issue:5"},
		Done:     []string{"issue:1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NextEligible =\n  %+v\nwant\n  %+v", got, want)
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
			// #1838 QUARANTINE: a failed item ALONGSIDE still-actionable work
			// (here a pending sibling) keeps the campaign RUNNING — the failed item
			// is quarantined so its eligible siblings can still be driven, instead
			// of the old unconditional anyFailed->Failed that drove the whole
			// campaign terminal. This is the core fix.
			name: "failed alongside pending is running (quarantine)",
			items: []*campaign.Item{
				item("issue:1", campaign.ItemStateFailed, nil),
				item("issue:2", campaign.ItemStatePending, nil),
			},
			want: campaign.StateRunning,
		},
		{
			// Anti-over-fix guard: when EVERY item is terminal and at least one
			// failed (here succeeded + failed, no actionable item remains), the
			// campaign is genuinely terminal-failed and still derives Failed.
			name: "all terminal with one failed is failed",
			items: []*campaign.Item{
				item("issue:1", campaign.ItemStateSucceeded, nil),
				item("issue:2", campaign.ItemStateFailed, nil),
			},
			want: campaign.StateFailed,
		},
		{
			// Anti-over-fix guard: a single failed item (the whole campaign) is
			// all-terminal, so it still derives Failed — the quarantine only spares
			// a failure that has actionable siblings.
			name: "single failed item is failed",
			items: []*campaign.Item{
				item("issue:1", campaign.ItemStateFailed, nil),
			},
			want: campaign.StateFailed,
		},
		{
			// A failed item with a still-RUNNING sibling is not all-terminal, so
			// the campaign stays running (progress remains in flight).
			name: "failed alongside running is running",
			items: []*campaign.Item{
				item("issue:1", campaign.ItemStateFailed, nil),
				item("issue:2", campaign.ItemStateRunning, nil),
			},
			want: campaign.StateRunning,
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
			// A paused item ALSO excludes the awaiting_human arm (E72.33 / #3660):
			// the partition's Paused bucket is non-empty, so the arm cannot fire —
			// and with no human-led item here it could not anyway.
			name: "all paused is pending, never paused",
			items: []*campaign.Item{
				item("issue:1", campaign.ItemStatePaused, nil),
			},
			want: campaign.StatePending,
		},
		{
			// PRECEDENCE PRESERVATION (E72.33 / #3660): the new awaiting_human arm
			// sits strictly BELOW the allSucceeded arm, so an all-succeeded campaign
			// that HAPPENED to contain human-led items still derives succeeded. (A
			// succeeded item is routed to Done, never HumanLed, so the arm's
			// predicate is false here too — this row pins both facts at once.)
			name: "all succeeded with a human-led item is still succeeded",
			items: []*campaign.Item{
				humanLedItem("issue:1", campaign.ItemStateSucceeded),
				item("issue:2", campaign.ItemStateSucceeded, nil),
			},
			want: campaign.StateSucceeded,
		},
		{
			// PRECEDENCE PRESERVATION: a genuinely terminal-failed campaign still
			// derives failed even with a human-led item among its items. When EVERY
			// item state is terminal the HumanLed bucket is necessarily EMPTY —
			// NextEligible only routes not-yet-run pending/blocked items there — so
			// the awaiting_human arm cannot steal this case.
			name: "all terminal with one failed and a human-led item is still failed",
			items: []*campaign.Item{
				humanLedItem("issue:1", campaign.ItemStateSucceeded),
				humanLedItem("issue:2", campaign.ItemStateFailed),
			},
			want: campaign.StateFailed,
		},
		{
			// THE DONE-MEANS at the pure-engine layer (E72.33 / #3660): a succeeded
			// agent-drivable item plus an OPEN human-led one derives awaiting_human.
			// COUNTERFACTUAL VEHICLE c1: with the arm deleted, anySucceeded is true
			// and allSucceeded false, so DeriveState returns `running`.
			name: "only human-led work remains derives awaiting_human",
			items: []*campaign.Item{
				item("issue:1", campaign.ItemStateSucceeded, nil),
				humanLedItem("issue:2", campaign.ItemStatePending),
			},
			want: campaign.StateAwaitingHuman,
		},
		{
			// The never-started all-human-led campaign: the INTENDED behavior change
			// (it previously derived pending, DeriveState's default branch).
			name: "never-started all-human-led campaign derives awaiting_human",
			items: []*campaign.Item{
				humanLedItem("issue:1", campaign.ItemStatePending),
				humanLedItem("issue:2", campaign.ItemStatePending),
			},
			want: campaign.StateAwaitingHuman,
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

// TestNextEligible_RunlessSucceededUnblocksDependents is the #1558 pure-engine
// pin for run-less out-of-band settlement: a human-led item settled succeeded
// WITHOUT a run (RunID nil) — its issue closed-as-completed and settled by the
// reconcile-on-read pass — must be reported in Done, and its dependent whose
// only unmet dependency was that item must move from Blocked to Eligible.
// Proves the settlement unblocks descendants at the engine level, independent
// of the GitHub-facing reconcile.
func TestNextEligible_RunlessSucceededUnblocksDependents(t *testing.T) {
	items := []*campaign.Item{
		// A human-led item settled succeeded with NO run linkage.
		item("issue:1", campaign.ItemStateSucceeded, nil),
		// Its dependent — blocked until issue:1 succeeded, now eligible.
		item("issue:2", campaign.ItemStatePending, nil, "issue:1"),
	}
	got := campaign.NextEligible(items)
	if !reflect.DeepEqual(got.Done, []string{"issue:1"}) {
		t.Errorf("done = %v, want [issue:1] (run-less succeeded still counts done)", got.Done)
	}
	if !reflect.DeepEqual(got.Eligible, []string{"issue:2"}) {
		t.Errorf("eligible = %v, want [issue:2] (dependent unblocked by run-less predecessor)", got.Eligible)
	}
	if len(got.Blocked) != 0 {
		t.Errorf("blocked = %v, want none", got.Blocked)
	}
}

// TestDeriveState_RunlessMixedHumanLedDAG walks a mixed human-led DAG through
// derivation (#1558): while some items remain unsettled the campaign reads
// running (partial progress from a run-less succeeded item), and once every
// item — including the run-less succeeded ones — is succeeded it reduces to
// StateSucceeded. Together with the pending→succeeded transition edge this is
// what lets an all-human-led campaign terminate without a single dispatch.
func TestDeriveState_RunlessMixedHumanLedDAG(t *testing.T) {
	// Partial: issue:1 settled run-less succeeded, issue:2 still pending.
	partial := []*campaign.Item{
		item("issue:1", campaign.ItemStateSucceeded, nil),
		item("issue:2", campaign.ItemStatePending, nil, "issue:1"),
	}
	if got := campaign.DeriveState(partial); got != campaign.StateRunning {
		t.Errorf("DeriveState(partial) = %q, want running (run-less succeeded is progress)", got)
	}
	// All settled run-less succeeded — the campaign reduces to succeeded.
	allDone := []*campaign.Item{
		item("issue:1", campaign.ItemStateSucceeded, nil),
		item("issue:2", campaign.ItemStateSucceeded, nil, "issue:1"),
	}
	if got := campaign.DeriveState(allDone); got != campaign.StateSucceeded {
		t.Errorf("DeriveState(allDone) = %q, want succeeded (all run-less settled)", got)
	}
}

// TestNextEligible_IssueClosedCancelledIsNotRestartable is the E72.24 (#3563)
// done-means for the resolved_by suppression: an item cancelled BECAUSE its
// issue was closed not_planned/duplicate carries ResolvedByIssueClosed and stays
// in Cancelled — never Restartable — however satisfied its deps are and whatever
// its autonomy tier. The abandoned issue has no forward path, so surfacing
// start_run for it is exactly what must not happen.
//
// COUNTERFACTUAL VEHICLE: deleting the `it.ResolvedBy != ResolvedByIssueClosed`
// conjunct in engine.go NextEligible makes every row below land in Restartable
// and this test go RED.
func TestNextEligible_IssueClosedCancelledIsNotRestartable(t *testing.T) {
	items := []*campaign.Item{
		item("issue:1", campaign.ItemStateSucceeded, nil), // done — satisfies deps below
		// cancelled by the issue-closed settle, deps satisfied, autonomy unset:
		// WITHOUT the marker this is the exact shape #1729 diverts to Restartable.
		{IssueRef: "issue:2", State: campaign.ItemStateCancelled, DependsOn: []string{"issue:1"}, ResolvedBy: campaign.ResolvedByIssueClosed},
		// no deps at all — the other shape that would otherwise be Restartable.
		{IssueRef: "issue:3", State: campaign.ItemStateCancelled, ResolvedBy: campaign.ResolvedByIssueClosed},
	}
	got := campaign.NextEligible(items)
	want := campaign.Eligibility{
		Done:      []string{"issue:1"},
		Cancelled: []string{"issue:2", "issue:3"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NextEligible =\n  %+v\nwant\n  %+v", got, want)
	}
	if len(got.Restartable) != 0 {
		t.Errorf("restartable = %v, want none (an issue-closed cancellation is abandoned, not restartable)", got.Restartable)
	}
	if len(got.Eligible) != 0 {
		t.Errorf("eligible = %v, want none", got.Eligible)
	}
}

// TestNextEligible_OperatorCancelledStaysRestartable is the ANTI-OVER-REACH pin
// paired with TestNextEligible_IssueClosedCancelledIsNotRestartable: the #3563
// suppression must fire ONLY on an issue-closed cancellation. An OPERATOR
// cancellation carries ResolvedBy == "" — seeded here BY CONSTRUCTION (a fresh
// item that never went through a settle, the zero value), never by calling the
// control — and stays Restartable exactly as #1729 shipped it.
//
// It must stay GREEN under the same deletion that reddens its sibling, which is
// what makes that RED discrimination rather than a blanket change.
func TestNextEligible_OperatorCancelledStaysRestartable(t *testing.T) {
	items := []*campaign.Item{
		item("issue:1", campaign.ItemStateSucceeded, nil), // done — satisfies deps below
		// Operator-cancelled: the zero-value ResolvedBy, by construction.
		{IssueRef: "issue:2", State: campaign.ItemStateCancelled, DependsOn: []string{"issue:1"}},
	}
	if got := items[1].ResolvedBy; got != campaign.ResolvedByUnset {
		t.Fatalf("fixture ResolvedBy = %q, want %q (the operator-cancelled default)", got, campaign.ResolvedByUnset)
	}
	got := campaign.NextEligible(items)
	want := campaign.Eligibility{
		Done:        []string{"issue:1"},
		Restartable: []string{"issue:2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NextEligible =\n  %+v\nwant\n  %+v", got, want)
	}
}

// TestNextEligible_IssueClosedMarkerDoesNotSuppressFailed pins the #3563
// suppression's BOUNDARY on the other axis: the marker is consulted by the
// CANCELLED arm only. The failed arm is untouched — a failed item is never
// stamped by the settle pass (class B settles to succeeded, class A only from
// pending/blocked) — so even a hypothetically-marked failed item keeps #1838's
// Restartable diversion. This is the pin that keeps a future edit from widening
// the conjunct across both arms.
func TestNextEligible_IssueClosedMarkerDoesNotSuppressFailed(t *testing.T) {
	items := []*campaign.Item{
		item("issue:1", campaign.ItemStateSucceeded, nil),
		{IssueRef: "issue:2", State: campaign.ItemStateFailed, DependsOn: []string{"issue:1"}, ResolvedBy: campaign.ResolvedByIssueClosed},
	}
	got := campaign.NextEligible(items)
	want := campaign.Eligibility{
		Done:        []string{"issue:1"},
		Restartable: []string{"issue:2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NextEligible =\n  %+v\nwant\n  %+v", got, want)
	}
}

// humanLedItem builds a deps-satisfied autonomy:low ("human-led") item — the
// tier NextEligible diverts into the HumanLed partition.
func humanLedItem(ref string, state campaign.ItemState, deps ...string) *campaign.Item {
	return &campaign.Item{IssueRef: ref, State: state, Autonomy: "low", DependsOn: deps}
}

// TestDeriveState_AwaitingHumanWhenOnlyHumanLedRemains is the pure-engine
// done-means for E72.33 (#3660) and counterfactual c1's named vehicle: a campaign
// whose agent-drivable item succeeded and whose only remaining OPEN item is
// human-led derives awaiting_human, not running.
//
// The fixture is deliberately [succeeded medium, pending autonomy:low] so that
// with the DeriveState arm DELETED the fall-through is anySucceeded -> `running`
// and this equality assertion fails on the BEHAVIORAL line, never on setup.
func TestDeriveState_AwaitingHumanWhenOnlyHumanLedRemains(t *testing.T) {
	items := []*campaign.Item{
		item("issue:1", campaign.ItemStateSucceeded, nil),
		humanLedItem("issue:2", campaign.ItemStatePending),
	}
	if got := campaign.DeriveState(items); got != campaign.StateAwaitingHuman {
		t.Errorf("DeriveState = %q, want awaiting_human (only human-led work remains)", got)
	}
	// The partition the arm is computed from, pinned so a later NextEligible
	// change that re-routed the item would fail here rather than silently.
	e := campaign.NextEligible(items)
	if !reflect.DeepEqual(e.HumanLed, []string{"issue:2"}) {
		t.Errorf("HumanLed = %v, want [issue:2]", e.HumanLed)
	}
	if len(e.Eligible)+len(e.Restartable)+len(e.Running)+len(e.Paused) != 0 {
		t.Errorf("partition should have no eligible/restartable/running/paused item: %+v", e)
	}
}

// TestDeriveState_AwaitingHumanExclusions is the PER-FAILURE-MODE table for the
// four exclusion guards in the awaiting_human arm (m1-m4) plus the deliberately
// NOT-excluded blocked case (m5). Each fixture ISOLATES one guard: the named
// bucket is the ONLY non-empty exclusion, so deleting that one conjunct makes the
// arm fire and the row go RED (the masking-guard hazard). Each `want` is the
// EXACT pre-change verdict, so an over-fire is caught rather than encoded.
func TestDeriveState_AwaitingHumanExclusions(t *testing.T) {
	cases := []struct {
		name string
		// guard names the conjunct this fixture isolates.
		guard string
		items []*campaign.Item
		want  campaign.State
	}{
		{
			// m1 / c2: a RUNNING item alongside an open human-led item. No eligible,
			// restartable or paused item, so len(e.Running)==0 is the only conjunct
			// in the path. Pre-change verdict: anyRunning -> running.
			name:  "m1 running item keeps the campaign running",
			guard: "len(e.Running) == 0",
			items: []*campaign.Item{
				item("issue:1", campaign.ItemStateRunning, nil),
				humanLedItem("issue:2", campaign.ItemStatePending),
			},
			want: campaign.StateRunning,
		},
		{
			// m2 / c3: a PAUSED item alongside an open human-led item. A paused item
			// contributes to NONE of anyRunning/anySucceeded/anyFailed, so the
			// pre-change verdict is PENDING, not running — asserting running here
			// would encode a wrong baseline.
			name:  "m2 paused item derives pending, not awaiting_human",
			guard: "len(e.Paused) == 0",
			items: []*campaign.Item{
				item("issue:1", campaign.ItemStatePaused, nil),
				humanLedItem("issue:2", campaign.ItemStatePending),
			},
			want: campaign.StatePending,
		},
		{
			// m3: an ELIGIBLE sibling (pending, non-low, deps-satisfied) alongside an
			// open human-led item, with a succeeded item supplying the progress that
			// makes the pre-change verdict running.
			name:  "m3 eligible sibling keeps the campaign running",
			guard: "len(e.Eligible) == 0",
			items: []*campaign.Item{
				item("issue:1", campaign.ItemStateSucceeded, nil),
				{IssueRef: "issue:2", State: campaign.ItemStatePending, Autonomy: "medium"},
				humanLedItem("issue:3", campaign.ItemStatePending),
			},
			want: campaign.StateRunning,
		},
		{
			// m4 / c4: the NEGATIVE CONTROL the acceptance criterion is worded to
			// agree with. A deps-SATISFIED failed autonomy:medium item is
			// RESTARTABLE — operator-driveable agent work — so the campaign must stay
			// running and must NOT derive awaiting_human. (A deps-UNsatisfied failed
			// item would land in Failed and leave the c4 deletion unobservable; this
			// fixture gives it no deps at all.)
			name:  "m4 restartable failed sibling keeps the campaign running",
			guard: "len(e.Restartable) == 0",
			items: []*campaign.Item{
				{IssueRef: "issue:1", State: campaign.ItemStateFailed, Autonomy: "medium"},
				humanLedItem("issue:2", campaign.ItemStatePending),
			},
			want: campaign.StateRunning,
		},
		{
			// m5: BLOCKED is deliberately NOT excluded. With no eligible/restartable/
			// running/paused item, a blocked item is waiting (transitively) on the
			// human-led work, so the campaign IS parked on a human. Pinned so a later
			// tightening is a visible decision rather than an accident.
			name:  "m5 blocked dependent still derives awaiting_human",
			guard: "blocked is NOT an exclusion",
			items: []*campaign.Item{
				humanLedItem("issue:1", campaign.ItemStatePending),
				// depends on the human-led item, which has not succeeded -> Blocked.
				{IssueRef: "issue:2", State: campaign.ItemStateBlocked, Autonomy: "medium", DependsOn: []string{"issue:1"}},
			},
			want: campaign.StateAwaitingHuman,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := campaign.DeriveState(tc.items); got != tc.want {
				t.Errorf("DeriveState = %q, want %q (guard under test: %s)", got, tc.want, tc.guard)
			}
		})
	}
}

// TestDeriveState_AwaitingHumanReDerivesPendingOnRelabel is REVISION FIX 1's
// engine-layer half (r1): the relabel-WITHOUT-start corner on a NEVER-STARTED
// campaign.
//
// Per binding condition 1 this test stays within what the campaign package can
// reach — DeriveState, the NextEligible partition and ValidCampaignTransition.
// baseCampaignNextAction is unexported in the SERVER package and is deliberately
// NOT exported to satisfy a test; the next_action == start_run half is asserted in
// the server-package integration test (r2).
//
// The sequence: two autonomy:low pending items, no deps, no RunID -> awaiting_human.
// Flip ONE item's tier to medium (the relabel) -> the partition gains an Eligible
// item, the arm stops firing, and DeriveState falls through to its DEFAULT branch
// returning StatePending (no item has succeeded, run or failed, so the progress arm
// cannot return Running). ValidCampaignTransition must admit that edge or the
// persisted row sticks.
func TestDeriveState_AwaitingHumanReDerivesPendingOnRelabel(t *testing.T) {
	items := []*campaign.Item{
		humanLedItem("issue:1", campaign.ItemStatePending),
		humanLedItem("issue:2", campaign.ItemStatePending),
	}
	if got := campaign.DeriveState(items); got != campaign.StateAwaitingHuman {
		t.Fatalf("DeriveState(all autonomy:low, never started) = %q, want awaiting_human", got)
	}

	// THE RELABEL, in place: autonomy:low -> autonomy:medium on issue:1.
	items[0].Autonomy = "medium"

	e := campaign.NextEligible(items)
	if !reflect.DeepEqual(e.Eligible, []string{"issue:1"}) {
		t.Errorf("post-relabel Eligible = %v, want [issue:1] (the arm must stop firing)", e.Eligible)
	}
	if !reflect.DeepEqual(e.HumanLed, []string{"issue:2"}) {
		t.Errorf("post-relabel HumanLed = %v, want [issue:2]", e.HumanLed)
	}
	if got := campaign.DeriveState(items); got != campaign.StatePending {
		t.Errorf("post-relabel DeriveState = %q, want pending (NOT awaiting_human — the partition has an Eligible item)", got)
	}
	// The persisted row can FOLLOW that derivation only if the edge is admitted;
	// deriveCampaignAfterChange silently drops a derivation the table refuses.
	if !campaign.ValidCampaignTransition(campaign.StateAwaitingHuman, campaign.StatePending) {
		t.Error("ValidCampaignTransition(awaiting_human, pending) = false, want true (REVISION FIX 1)")
	}
	// ONE state change per relabel in each direction: re-deriving the same items
	// is stable, so no flap is possible without another label edit.
	if got := campaign.DeriveState(items); got != campaign.StatePending {
		t.Errorf("re-derive after relabel = %q, want a stable pending", got)
	}
	items[0].Autonomy = "low"
	if got := campaign.DeriveState(items); got != campaign.StateAwaitingHuman {
		t.Errorf("derive after reverting the label = %q, want awaiting_human (one change per relabel)", got)
	}
}
