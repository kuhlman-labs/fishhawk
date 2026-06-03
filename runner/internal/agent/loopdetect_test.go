package agent

import "testing"

// feed observes every signature in order and returns whether the detector
// tripped at any point plus the streak length at the end.
func feed(d *LoopDetector, sigs []string) (tripped bool) {
	for _, s := range sigs {
		if d.Observe(s) {
			tripped = true
		}
	}
	return tripped
}

func TestLoopDetector_TripsOnIdenticalRun(t *testing.T) {
	d := NewLoopDetector(3)
	// Two identical calls: below threshold, no trip.
	if d.Observe("Read foo") {
		t.Fatal("tripped on first call")
	}
	if d.Observe("Read foo") {
		t.Fatal("tripped on second identical call (threshold 3)")
	}
	// Third identical call hits the threshold.
	if !d.Observe("Read foo") {
		t.Fatal("did not trip on third identical call (threshold 3)")
	}
	if !d.Tripped() {
		t.Error("Tripped() = false after trip")
	}
	if d.Streak() != 3 {
		t.Errorf("Streak() = %d, want 3 at trip", d.Streak())
	}
	if d.Signature() != "Read foo" {
		t.Errorf("Signature() = %q, want %q", d.Signature(), "Read foo")
	}
}

func TestLoopDetector_StaysTripped(t *testing.T) {
	d := NewLoopDetector(2)
	d.Observe("x")
	if !d.Observe("x") {
		t.Fatal("did not trip at threshold 2")
	}
	// A different signature after tripping must not un-trip it.
	if !d.Observe("y") {
		t.Error("un-tripped on a later differing signature")
	}
}

func TestLoopDetector_NoTripCases(t *testing.T) {
	cases := []struct {
		name      string
		threshold int
		sigs      []string
	}{
		{
			// Genuinely varied work — the common case — never trips.
			name:      "varied_tool_calls",
			threshold: 3,
			sigs: []string{
				"Read a.go", "Edit a.go", "Bash go test", "Read b.go",
				"Edit b.go", "Bash go test", "Read c.go",
			},
		},
		{
			// Legit repeat below threshold: re-reading the same file twice,
			// retrying a flaky command twice — fine.
			name:      "legit_repeats_below_threshold",
			threshold: 4,
			sigs: []string{
				"Read x", "Read x", "Bash flaky", "Bash flaky", "Bash flaky",
			},
		},
		{
			// An interleaved differing call resets the streak, so a long
			// run that never reaches threshold-in-a-row does not trip.
			name:      "interleaved_resets_streak",
			threshold: 3,
			sigs: []string{
				"A", "A", "B", "A", "A", "C", "A", "A",
			},
		},
		{
			// Same call only twice in a row but many times overall —
			// distinct files between — must not accumulate.
			name:      "distinct_args_dont_accumulate",
			threshold: 3,
			sigs: []string{
				"Read f1", "Read f2", "Read f3", "Read f4", "Read f5",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewLoopDetector(tc.threshold)
			if feed(d, tc.sigs) {
				t.Errorf("detector tripped on a no-trip trace; streak=%d sig=%q",
					d.Streak(), d.Signature())
			}
		})
	}
}

func TestLoopDetector_EmptySignatureIgnored(t *testing.T) {
	d := NewLoopDetector(3)
	// Empty signatures (non-tool events) must not break an otherwise
	// unbroken run of identical tool calls.
	seq := []string{"loop", "", "loop", "", "loop"}
	if !feed(d, seq) {
		t.Fatal("empty signatures broke the identical run; should be ignored")
	}
	if d.Streak() != 3 {
		t.Errorf("Streak() = %d, want 3 (empties ignored)", d.Streak())
	}
}

func TestLoopDetector_DefaultThreshold(t *testing.T) {
	// threshold <= 0 falls back to DefaultLoopThreshold.
	d := NewLoopDetector(0)
	sigs := make([]string, DefaultLoopThreshold-1)
	for i := range sigs {
		sigs[i] = "same"
	}
	if feed(d, sigs) {
		t.Fatalf("tripped after %d identical calls, default threshold is %d",
			DefaultLoopThreshold-1, DefaultLoopThreshold)
	}
	if !d.Observe("same") {
		t.Errorf("did not trip at the default threshold of %d", DefaultLoopThreshold)
	}
}

func TestLoopDetector_ZeroValueUsable(t *testing.T) {
	var d LoopDetector
	sigs := make([]string, DefaultLoopThreshold)
	for i := range sigs {
		sigs[i] = "z"
	}
	if !feed(&d, sigs) {
		t.Errorf("zero-value detector did not trip at the default threshold")
	}
}
