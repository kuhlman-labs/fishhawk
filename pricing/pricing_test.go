package pricing

import (
	"math"
	"testing"
)

func approx(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("usd = %v, want %v", got, want)
	}
}

func TestCost_KnownTiers(t *testing.T) {
	tests := []struct {
		name          string
		model         string
		input, output int
		want          float64
	}{
		{
			name:   "opus exact family id",
			model:  "claude-opus-4-8",
			input:  1_000_000,
			output: 1_000_000,
			want:   15 + 75, // $15 input + $75 output per 1M
		},
		{
			name:   "sonnet dated point release inherits family rate",
			model:  "claude-sonnet-4-6-20260201",
			input:  2_000_000,
			output: 1_000_000,
			want:   2*3 + 1*15,
		},
		{
			name:   "haiku fractional",
			model:  "claude-haiku-4-5",
			input:  500_000,
			output: 250_000,
			want:   0.5*0.80 + 0.25*4,
		},
		{
			name:   "zero usage is zero cost",
			model:  "claude-opus-4-8",
			input:  0,
			output: 0,
			want:   0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Cost(tc.model, tc.input, tc.output)
			if !ok {
				t.Fatalf("ok = false for known model %q", tc.model)
			}
			approx(t, got, tc.want)
		})
	}
}

func TestCost_UnknownModel(t *testing.T) {
	for _, model := range []string{"", "gpt-4o", "llama-3", "claude"} {
		got, ok := Cost(model, 1000, 1000)
		if ok {
			t.Errorf("Cost(%q) ok = true, want false", model)
		}
		if got != 0 {
			t.Errorf("Cost(%q) usd = %v, want 0 for unknown model", model, got)
		}
	}
}

func TestCost_NegativeTokensClamped(t *testing.T) {
	got, ok := Cost("claude-opus-4-8", -100, -100)
	if !ok {
		t.Fatal("ok = false for known model")
	}
	if got != 0 {
		t.Errorf("usd = %v, want 0 (negative tokens clamped)", got)
	}
}

func TestLookup_LongestPrefixWins(t *testing.T) {
	// Sonnet and Opus share the "claude-" stem; ensure the family
	// prefix, not a shorter accidental match, is what's selected.
	r, ok := lookup("claude-sonnet-4-6")
	if !ok {
		t.Fatal("sonnet not found")
	}
	if r != familyRates["claude-sonnet"] {
		t.Errorf("resolved rate = %+v, want sonnet rate", r)
	}
}
