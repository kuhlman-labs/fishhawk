package timescale

import (
	"testing"
	"time"
)

// TestFactor_ExplicitOverrideWinsOverCI asserts an explicit
// FISHHAWK_TEST_TIME_SCALE wins even when CI is also set — the operator knob
// takes precedence over the CI default.
func TestFactor_ExplicitOverrideWinsOverCI(t *testing.T) {
	t.Setenv(ciEnv, "true")
	t.Setenv(scaleEnv, "7")
	if got := Factor(); got != 7 {
		t.Errorf("Factor() = %d, want 7 (explicit override wins over the CI default)", got)
	}
}

// TestFactor_CIDefaultWhenNoOverride asserts a non-empty CI env var with no
// explicit override yields the CI default 5.
func TestFactor_CIDefaultWhenNoOverride(t *testing.T) {
	t.Setenv(scaleEnv, "")
	t.Setenv(ciEnv, "true")
	if got := Factor(); got != 5 {
		t.Errorf("Factor() = %d, want 5 (CI set, no override)", got)
	}
}

// TestFactor_LocalDefaultWhenNeitherSet asserts that with neither the override
// nor CI set the factor is 1 (unchanged local behavior). CI is cleared via
// t.Setenv so a real CI run of this test still exercises the local path.
func TestFactor_LocalDefaultWhenNeitherSet(t *testing.T) {
	t.Setenv(scaleEnv, "")
	t.Setenv(ciEnv, "")
	if got := Factor(); got != 1 {
		t.Errorf("Factor() = %d, want 1 (neither override nor CI set)", got)
	}
}

// TestFactor_InvalidNonIntegerPanics asserts a set-but-non-integer override
// panics (fail closed) rather than silently degrading to 1.
func TestFactor_InvalidNonIntegerPanics(t *testing.T) {
	t.Setenv(scaleEnv, "fast")
	defer func() {
		if r := recover(); r == nil {
			t.Error("Factor() did not panic on a non-integer FISHHAWK_TEST_TIME_SCALE")
		}
	}()
	_ = Factor()
}

// TestFactor_NonPositivePanics asserts a zero or negative override panics — a
// non-positive scale would yield a zero or negative (wrapped) duration.
func TestFactor_NonPositivePanics(t *testing.T) {
	for _, v := range []string{"0", "-3"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(scaleEnv, v)
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("Factor() did not panic on FISHHAWK_TEST_TIME_SCALE=%q", v)
				}
			}()
			_ = Factor()
		})
	}
}

// TestFactor_AboveMaxPanics is the binding-condition pin: a value above maxFactor
// panics rather than being accepted, so D can never wrap to a misleading
// negative or short duration on a huge input. Also covers the boundary: maxFactor
// itself is accepted, maxFactor+1 panics.
func TestFactor_AboveMaxPanics(t *testing.T) {
	t.Run("maxFactor accepted", func(t *testing.T) {
		t.Setenv(scaleEnv, "1000")
		if got := Factor(); got != maxFactor {
			t.Errorf("Factor() = %d, want %d (maxFactor is accepted)", got, maxFactor)
		}
	})
	t.Run("above maxFactor panics", func(t *testing.T) {
		t.Setenv(scaleEnv, "1001")
		defer func() {
			if r := recover(); r == nil {
				t.Error("Factor() did not panic on FISHHAWK_TEST_TIME_SCALE above maxFactor")
			}
		}()
		_ = Factor()
	})
	t.Run("huge value panics and never wraps", func(t *testing.T) {
		// A value that, if multiplied into a 30s base, would overflow int64 and
		// produce a negative/short duration. It must panic instead.
		t.Setenv(scaleEnv, "999999999999")
		defer func() {
			if r := recover(); r == nil {
				t.Error("Factor() did not panic on a huge FISHHAWK_TEST_TIME_SCALE")
			}
		}()
		_ = Factor()
	})
}

// TestD_MultipliesBaseByFactor asserts D scales the base duration by the factor.
func TestD_MultipliesBaseByFactor(t *testing.T) {
	t.Setenv(ciEnv, "")
	t.Setenv(scaleEnv, "5")
	if got := D(200 * time.Millisecond); got != time.Second {
		t.Errorf("D(200ms) = %v, want 1s (200ms * 5)", got)
	}
	t.Setenv(scaleEnv, "1")
	if got := D(30 * time.Second); got != 30*time.Second {
		t.Errorf("D(30s) at factor 1 = %v, want 30s", got)
	}
}
