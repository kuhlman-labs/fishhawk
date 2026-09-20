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
