package workmgmt

import "testing"

// TestParseAutonomyLabel covers the tier extraction: no label, each valid tier,
// an out-of-set tier -> "", a non-autonomy label, and the first-wins ordering.
// It is the single-source-of-truth table for the parser the assembly path (via
// the github provider's delegate) and the reconcile-on-read refresh both use
// (#2355).
func TestParseAutonomyLabel(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   string
	}{
		{"nil labels", nil, ""},
		{"no autonomy label", []string{"area:server", "phase:1"}, ""},
		{"low", []string{"autonomy:low"}, "low"},
		{"medium", []string{"autonomy:medium"}, "medium"},
		{"high", []string{"autonomy:high"}, "high"},
		{"out-of-set tier normalizes to empty", []string{"autonomy:critical"}, ""},
		{"empty tier normalizes to empty", []string{"autonomy:"}, ""},
		{"non-autonomy label alongside", []string{"type:feature", "autonomy:high"}, "high"},
		{"first autonomy label wins", []string{"autonomy:low", "autonomy:high"}, "low"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseAutonomyLabel(tc.labels); got != tc.want {
				t.Errorf("ParseAutonomyLabel(%v) = %q, want %q", tc.labels, got, tc.want)
			}
		})
	}
}
