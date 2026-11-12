package stagecheck_test

import (
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

func TestDeriveState(t *testing.T) {
	cases := []struct {
		name       string
		status     string
		conclusion *string
		want       stagecheck.State
	}{
		{"queued is pending", "queued", nil, stagecheck.StatePending},
		{"in_progress is pending", "in_progress", nil, stagecheck.StatePending},
		{"completed without conclusion is pending (defensive)", "completed", nil, stagecheck.StatePending},
		{"success", "completed", ptr("success"), stagecheck.StatePass},
		{"neutral counts as pass", "completed", ptr("neutral"), stagecheck.StatePass},
		{"skipped counts as pass", "completed", ptr("skipped"), stagecheck.StatePass},
		{"failure", "completed", ptr("failure"), stagecheck.StateFail},
		{"timed_out is fail", "completed", ptr("timed_out"), stagecheck.StateFail},
		{"cancelled is fail", "completed", ptr("cancelled"), stagecheck.StateFail},
		{"action_required is fail", "completed", ptr("action_required"), stagecheck.StateFail},
		{"stale is fail", "completed", ptr("stale"), stagecheck.StateFail},
		{"startup_failure is fail", "completed", ptr("startup_failure"), stagecheck.StateFail},
		{"unknown conclusion is pending", "completed", ptr("something_new"), stagecheck.StatePending},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stagecheck.DeriveState(c.status, c.conclusion); got != c.want {
				t.Errorf("DeriveState(%q, %v) = %q, want %q", c.status, c.conclusion, got, c.want)
			}
		})
	}
}

// TestDeriveState_SkippedNotAllowedIsFail pins the ONE non-GitHub
// conclusion (E45.55 / #3490): the GitLab pipeline ingester records a
// `skipped` pipeline as `skipped_not_allowed` when the run's snapshot
// does not carry allow_merge_on_skipped_pipeline, and DeriveState must
// read that as FAIL — not borrow the GitHub skipped-is-pass rule, and
// not fall into the unknown-conclusion pending bucket (which would let a
// pending row hide a merge GitLab itself would refuse). The sibling
// `skipped` cell stays pass, so the two conclusions are distinguishable
// by the reader, not only by the writer.
func TestDeriveState_SkippedNotAllowedIsFail(t *testing.T) {
	if got := stagecheck.DeriveState("completed", ptr("skipped_not_allowed")); got != stagecheck.StateFail {
		t.Errorf("DeriveState(completed, skipped_not_allowed) = %q, want %q", got, stagecheck.StateFail)
	}
	if got := stagecheck.DeriveState("completed", ptr("skipped")); got != stagecheck.StatePass {
		t.Errorf("DeriveState(completed, skipped) = %q, want %q (the allowed arm is unchanged)", got, stagecheck.StatePass)
	}
	// Not completed → pending regardless of conclusion; the fail
	// verdict is reserved for a terminal pipeline.
	if got := stagecheck.DeriveState("in_progress", ptr("skipped_not_allowed")); got != stagecheck.StatePending {
		t.Errorf("DeriveState(in_progress, skipped_not_allowed) = %q, want %q", got, stagecheck.StatePending)
	}
	// skipped_not_allowed is NOT a supersession: RedVerdict must be red so
	// the drive ci_failed park (#3414) trips on it.
	c := &stagecheck.Check{State: stagecheck.StateFail, Conclusion: ptr("skipped_not_allowed")}
	if !c.RedVerdict() {
		t.Errorf("RedVerdict() = false for skipped_not_allowed, want true (it is a verdict about the merge, not a supersession)")
	}
}

func TestConclusionSuperseded(t *testing.T) {
	cases := []struct {
		name       string
		conclusion *string
		want       bool
	}{
		{"cancelled is superseded", ptr("cancelled"), true},
		{"stale is superseded", ptr("stale"), true},
		{"nil is not superseded", nil, false},
		{"failure is not superseded", ptr("failure"), false},
		{"timed_out is not superseded", ptr("timed_out"), false},
		{"action_required is not superseded", ptr("action_required"), false},
		{"startup_failure is not superseded", ptr("startup_failure"), false},
		{"success is not superseded", ptr("success"), false},
		{"unknown is not superseded", ptr("something_new"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stagecheck.ConclusionSuperseded(c.conclusion); got != c.want {
				t.Errorf("ConclusionSuperseded(%v) = %v, want %v", c.conclusion, got, c.want)
			}
		})
	}
}

func TestCheckRedVerdict(t *testing.T) {
	cases := []struct {
		name       string
		state      stagecheck.State
		conclusion *string
		want       bool
	}{
		{"fail + failure is red", stagecheck.StateFail, ptr("failure"), true},
		{"fail + cancelled is not red (superseded)", stagecheck.StateFail, ptr("cancelled"), false},
		{"fail + stale is not red (superseded)", stagecheck.StateFail, ptr("stale"), false},
		{"fail + nil conclusion stays red", stagecheck.StateFail, nil, true},
		{"pending + cancelled is not red", stagecheck.StatePending, ptr("cancelled"), false},
		{"pass + success is not red", stagecheck.StatePass, ptr("success"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			check := &stagecheck.Check{State: c.state, Conclusion: c.conclusion}
			if got := check.RedVerdict(); got != c.want {
				t.Errorf("RedVerdict(state=%q, conclusion=%v) = %v, want %v", c.state, c.conclusion, got, c.want)
			}
		})
	}
}

// TestCheck_CancelledAtHead pins the head-aware cancellation predicate the
// drive ci_cancelled park keys on (#4706). It calls the predicate DIRECTLY:
// the observer's headKnown gate never passes an empty head, so the
// empty_check_head_and_empty_current_head row is the ONLY place the
// `currentHead != ""` arm is isolatable — it is the one row whose check head
// equals the (empty) current head, so dropping the arm turns it true. The
// cancelled@abc / "" row fails the equality on its own and cannot carry that
// credit.
func TestCheck_CancelledAtHead(t *testing.T) {
	cases := []struct {
		name        string
		state       stagecheck.State
		conclusion  *string
		checkHead   string
		currentHead string
		want        bool
	}{
		{"cancelled_at_current_head", stagecheck.StateFail, ptr("cancelled"), "abc", "abc", true},
		{"stale_at_current_head", stagecheck.StateFail, ptr("stale"), "abc", "abc", true},
		{"cancelled_at_older_head", stagecheck.StateFail, ptr("cancelled"), "abc", "def", false},
		{"failure_at_current_head", stagecheck.StateFail, ptr("failure"), "abc", "abc", false},
		{"pass_success", stagecheck.StatePass, ptr("success"), "abc", "abc", false},
		{"fail_nil_conclusion", stagecheck.StateFail, nil, "abc", "abc", false},
		{"pending_cancelled", stagecheck.StatePending, ptr("cancelled"), "abc", "abc", false},
		{"cancelled_with_unknown_current_head", stagecheck.StateFail, ptr("cancelled"), "abc", "", false},
		{"empty_check_head_and_empty_current_head", stagecheck.StateFail, ptr("cancelled"), "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			check := &stagecheck.Check{State: c.state, Conclusion: c.conclusion, HeadSHA: c.checkHead}
			if got := check.CancelledAtHead(c.currentHead); got != c.want {
				t.Errorf("CancelledAtHead(%q) = %v, want %v (state=%q conclusion=%v head=%q)",
					c.currentHead, got, c.want, c.state, c.conclusion, c.checkHead)
			}
		})
	}
}

// TestSupersededByHead pins the PROVEN-supersession predicate the webhook
// CI-failure retry refusal keys on (#4706): both heads known and different.
// Each false row isolates one conjunct — the ""→def row is the only thing
// between an empty check head and a refusal, the abc→"" row the same for an
// empty current head, and the failure abc→def row (two different non-empty
// heads) leaves the conclusion restriction as the only thing in the path.
func TestSupersededByHead(t *testing.T) {
	cases := []struct {
		name        string
		conclusion  *string
		checkHead   string
		currentHead string
		want        bool
	}{
		{"cancelled_older_head", ptr("cancelled"), "abc", "def", true},
		{"stale_older_head", ptr("stale"), "abc", "def", true},
		{"cancelled_same_head", ptr("cancelled"), "abc", "abc", false},
		{"failure_older_head", ptr("failure"), "abc", "def", false},
		{"nil_conclusion", nil, "abc", "def", false},
		{"cancelled_unknown_check_head", ptr("cancelled"), "", "def", false},
		{"cancelled_unknown_current_head", ptr("cancelled"), "abc", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stagecheck.SupersededByHead(c.conclusion, c.checkHead, c.currentHead); got != c.want {
				t.Errorf("SupersededByHead(%v, %q, %q) = %v, want %v",
					c.conclusion, c.checkHead, c.currentHead, got, c.want)
			}
		})
	}
}
