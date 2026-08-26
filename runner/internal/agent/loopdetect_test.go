package agent

import "testing"

// feed observes every signature in order with the given class and returns
// whether the detector tripped at any point.
func feed(d *LoopDetector, sigs []string, waitPoll bool) (tripped bool) {
	for _, s := range sigs {
		if d.Observe(s, waitPoll) {
			tripped = true
		}
	}
	return tripped
}

// repeat builds a slice of n copies of sig.
func repeat(sig string, n int) []string {
	sigs := make([]string, n)
	for i := range sigs {
		sigs[i] = sig
	}
	return sigs
}

func TestLoopDetector_TripsOnIdenticalRun(t *testing.T) {
	d := NewLoopDetector(3, 0)
	// Two identical calls: below threshold, no trip.
	if d.Observe("Read foo", false) {
		t.Fatal("tripped on first call")
	}
	if d.Observe("Read foo", false) {
		t.Fatal("tripped on second identical call (threshold 3)")
	}
	// Third identical call hits the threshold.
	if !d.Observe("Read foo", false) {
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
	d := NewLoopDetector(2, 0)
	d.Observe("x", false)
	if !d.Observe("x", false) {
		t.Fatal("did not trip at threshold 2")
	}
	// A different signature after tripping must not un-trip it.
	if !d.Observe("y", false) {
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
			d := NewLoopDetector(tc.threshold, 0)
			if feed(d, tc.sigs, false) {
				t.Errorf("detector tripped on a no-trip trace; streak=%d sig=%q",
					d.Streak(), d.Signature())
			}
		})
	}
}

func TestLoopDetector_EmptySignatureIgnored(t *testing.T) {
	d := NewLoopDetector(3, 0)
	// Empty signatures (non-tool events) must not break an otherwise
	// unbroken run of identical tool calls.
	seq := []string{"loop", "", "loop", "", "loop"}
	if !feed(d, seq, false) {
		t.Fatal("empty signatures broke the identical run; should be ignored")
	}
	if d.Streak() != 3 {
		t.Errorf("Streak() = %d, want 3 (empties ignored)", d.Streak())
	}
}

func TestLoopDetector_DefaultThreshold(t *testing.T) {
	// threshold <= 0 falls back to DefaultLoopThreshold.
	d := NewLoopDetector(0, 0)
	sigs := repeat("same", DefaultLoopThreshold-1)
	if feed(d, sigs, false) {
		t.Fatalf("tripped after %d identical calls, default threshold is %d",
			DefaultLoopThreshold-1, DefaultLoopThreshold)
	}
	if !d.Observe("same", false) {
		t.Errorf("did not trip at the default threshold of %d", DefaultLoopThreshold)
	}
}

func TestLoopDetector_ZeroValueUsable(t *testing.T) {
	var d LoopDetector
	sigs := repeat("z", DefaultLoopThreshold)
	if !feed(&d, sigs, false) {
		t.Errorf("zero-value detector did not trip at the default threshold")
	}
}

// TestLoopDetector_WaitPollTier is the #2758 two-tier contract: a
// wait-classed streak survives the BASE threshold and trips only at the
// wait threshold, while a non-wait streak still trips at the base
// threshold with a high wait threshold configured. The second half is the
// discriminating counterfactual for a class mix-up — if the detector used
// the wait threshold for every class it would go red.
func TestLoopDetector_WaitPollTier(t *testing.T) {
	t.Run("wait_survives_base_threshold_trips_at_wait_threshold", func(t *testing.T) {
		d := NewLoopDetector(3, 10)
		if feed(d, repeat("Bash tail", 9), true) {
			t.Fatalf("wait-classed streak tripped below the wait threshold at streak=%d", d.Streak())
		}
		if !d.Observe("Bash tail", true) {
			t.Fatal("wait-classed streak did not trip at the wait threshold (10)")
		}
		if d.Streak() != 10 {
			t.Errorf("Streak() = %d, want 10 at the wait-tier trip", d.Streak())
		}
	})
	t.Run("non_wait_trips_at_base_threshold", func(t *testing.T) {
		// A high wait threshold must not raise the bar for a NON-wait call.
		d := NewLoopDetector(3, 1000)
		if !feed(d, repeat("Bash go test ./...", 3), false) {
			t.Fatal("non-wait streak did not trip at the base threshold (3)")
		}
		if d.Streak() != 3 {
			t.Errorf("Streak() = %d, want 3 at the base-tier trip", d.Streak())
		}
	})
}

// TestLoopDetector_ClassFlipResetsStreak pins the total-state-machine rule:
// an identical signature observed with a DIFFERENT class resets the streak
// to one and adopts the new class, rather than accumulating across classes
// under whichever threshold happens to be current.
func TestLoopDetector_ClassFlipResetsStreak(t *testing.T) {
	d := NewLoopDetector(3, 100)
	d.Observe("same", true)
	d.Observe("same", true)
	// Same signature, flipped class: streak restarts at 1, so the base
	// threshold of 3 is not reached by this call.
	if d.Observe("same", false) {
		t.Fatal("tripped on the class-flip call; the flip must reset the streak to 1")
	}
	if d.Streak() != 1 {
		t.Errorf("Streak() = %d, want 1 after a class flip", d.Streak())
	}
	if d.WaitPoll() {
		t.Error("WaitPoll() = true after flipping to the non-wait class")
	}
	// Two more non-wait observations reach the base threshold.
	d.Observe("same", false)
	if !d.Observe("same", false) {
		t.Error("did not trip at the base threshold after adopting the non-wait class")
	}
}

// TestLoopDetector_IndependentThresholdFallbacks proves each constructor
// argument falls back to its OWN default when <= 0 — a shared fallback
// would make one of the two tiers wrong.
func TestLoopDetector_IndependentThresholdFallbacks(t *testing.T) {
	t.Run("wait_threshold_defaults_when_base_is_set", func(t *testing.T) {
		d := NewLoopDetector(3, 0)
		if feed(d, repeat("w", DefaultWaitPollThreshold-1), true) {
			t.Fatalf("wait streak tripped before DefaultWaitPollThreshold (%d)", DefaultWaitPollThreshold)
		}
		if !d.Observe("w", true) {
			t.Errorf("did not trip at DefaultWaitPollThreshold (%d)", DefaultWaitPollThreshold)
		}
	})
	t.Run("base_threshold_defaults_when_wait_is_set", func(t *testing.T) {
		d := NewLoopDetector(0, 1000)
		if feed(d, repeat("n", DefaultLoopThreshold-1), false) {
			t.Fatalf("non-wait streak tripped before DefaultLoopThreshold (%d)", DefaultLoopThreshold)
		}
		if !d.Observe("n", false) {
			t.Errorf("did not trip at DefaultLoopThreshold (%d)", DefaultLoopThreshold)
		}
	})
	t.Run("zero_value_wait_streak_uses_wait_default", func(t *testing.T) {
		var d LoopDetector
		if feed(&d, repeat("w", DefaultLoopThreshold), true) {
			t.Fatal("zero-value detector tripped a wait streak at the BASE default")
		}
		if !feed(&d, repeat("w", DefaultWaitPollThreshold-DefaultLoopThreshold), true) {
			t.Errorf("zero-value detector did not trip a wait streak at DefaultWaitPollThreshold (%d)",
				DefaultWaitPollThreshold)
		}
	})
}

// TestLoopDetector_ReportsTrippingTier pins the two accessors the audit
// payload reads: WaitPoll() is the class of the tripping signature and
// EffectiveThreshold() is the threshold that was reached.
func TestLoopDetector_ReportsTrippingTier(t *testing.T) {
	t.Run("wait", func(t *testing.T) {
		d := NewLoopDetector(3, 5)
		if !feed(d, repeat("Bash tail", 5), true) {
			t.Fatal("wait streak did not trip at 5")
		}
		if !d.WaitPoll() {
			t.Error("WaitPoll() = false at a wait-tier trip")
		}
		if got := d.EffectiveThreshold(); got != 5 {
			t.Errorf("EffectiveThreshold() = %d, want 5", got)
		}
	})
	t.Run("non_wait", func(t *testing.T) {
		d := NewLoopDetector(3, 5)
		if !feed(d, repeat("Bash go test", 3), false) {
			t.Fatal("non-wait streak did not trip at 3")
		}
		if d.WaitPoll() {
			t.Error("WaitPoll() = true at a base-tier trip")
		}
		if got := d.EffectiveThreshold(); got != 3 {
			t.Errorf("EffectiveThreshold() = %d, want 3", got)
		}
	})
}

// TestLoopDetector_WaitPollEmptySignatureIgnored confirms the empty-signature
// no-op holds in the wait tier too: a non-tool event between wait polls must
// not break the streak.
func TestLoopDetector_WaitPollEmptySignatureIgnored(t *testing.T) {
	d := NewLoopDetector(100, 3)
	if !feed(d, []string{"poll", "", "poll", "", "poll"}, true) {
		t.Fatal("empty signatures broke a wait-poll streak; should be ignored")
	}
	if d.Streak() != 3 {
		t.Errorf("Streak() = %d, want 3 (empties ignored)", d.Streak())
	}
}

// TestLoopDetector_WaitPollStaysTripped confirms the stays-tripped
// invariant holds for a wait-tier trip.
func TestLoopDetector_WaitPollStaysTripped(t *testing.T) {
	d := NewLoopDetector(100, 2)
	d.Observe("p", true)
	if !d.Observe("p", true) {
		t.Fatal("wait streak did not trip at the wait threshold 2")
	}
	if !d.Observe("other", false) {
		t.Error("un-tripped on a later differing non-wait signature")
	}
}
