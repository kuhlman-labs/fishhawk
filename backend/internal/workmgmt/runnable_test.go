package workmgmt

import "testing"

// TestParseRunnableLabel covers the `runnable:` declaration parse (#3649): only
// an explicit `runnable:no` declares not-runnable; every other input — absent,
// `yes`, an unrecognized or empty suffix, a prefix-colliding namespace — degrades
// to runnable (false), and the first runnable label wins.
func TestParseRunnableLabel(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"nil labels", nil, false},
		{"no runnable label", []string{"area:server", "autonomy:low"}, false},
		{"declared not runnable", []string{"runnable:no"}, true},
		{"declared runnable", []string{"runnable:yes"}, false},
		{"unrecognized value degrades to runnable", []string{"runnable:maybe"}, false},
		{"empty value degrades to runnable", []string{"runnable:"}, false},
		{"case-sensitive value degrades to runnable", []string{"runnable:NO"}, false},
		{"prefix collision does not match", []string{"runnables:no"}, false},
		{"embedded namespace does not match", []string{"not-runnable:no"}, false},
		{"alongside other labels", []string{"type:chore", "runnable:no", "autonomy:high"}, true},
		{"first runnable label wins (no first)", []string{"runnable:no", "runnable:yes"}, true},
		{"first runnable label wins (yes first)", []string{"runnable:yes", "runnable:no"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseRunnableLabel(tc.labels); got != tc.want {
				t.Errorf("ParseRunnableLabel(%v) = %v, want %v", tc.labels, got, tc.want)
			}
		})
	}
}
