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
