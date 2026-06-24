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
			want:   5 + 25, // $5 input + $25 output per 1M
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
			want:   0.5*1 + 0.25*5,
		},
		{
			name:   "gpt-5.5 exact family id",
			model:  "gpt-5.5",
			input:  1_000_000,
			output: 1_000_000,
			want:   5 + 30, // $5 input + $30 output per 1M
		},
		{
			name:   "gpt-5.5 dated point release inherits family rate",
			model:  "gpt-5.5-20260601",
			input:  2_000_000,
			output: 1_000_000,
			want:   2*5 + 1*30,
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

// TestCost_PricesLiveModelIDs is the drift guard: every model id the
// product actually dispatches today must price (ok==true), so a live
// model the table never covers fails CI here instead of silently
// recording $0. Keep this list in sync with the default allow-list in
// backend/cmd/fishhawkd/serve.go and backend/internal/server/modelpolicy.go
// (claudecode=claude-opus-4-8,claude-sonnet-4-6; codex=gpt-5.5) — those
// are the source of truth for which model ids are in use. The pricing
// module is standalone (no dependency on backend/server), so this
// literal list is the manual mirror, and this comment is the guard
// against it going stale when a maintainer adds a new default model.
func TestCost_PricesLiveModelIDs(t *testing.T) {
	live := []string{
		"claude-opus-4-8",
		"claude-sonnet-4-6",
		"gpt-5.5",
	}
	for _, model := range live {
		if _, ok := Cost(model, 1, 1); !ok {
			t.Errorf("Cost(%q) ok = false, want true — live model id is unpriced", model)
		}
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

// TestCacheRates_DriftGuard pins the verified cache-pricing multipliers so a
// wrong constant fails CI instead of silently mispricing cached tokens
// (#1343). The values were reconciled against the live vendor pages on AsOf:
//   - Anthropic (claude-*): cache READ = 0.1x base input, cache WRITE = 1.25x
//     base input at the 5-minute-TTL baseline.
//   - OpenAI (gpt-5.5): cache READ = $0.50/1M = 0.1x base input (the published
//     cached-input discount); cache WRITE = the base input rate (no OpenAI
//     write premium).
func TestCacheRates_DriftGuard(t *testing.T) {
	cases := []struct {
		family             string
		inputPerMillion    float64
		cacheReadMultiple  float64
		cacheWriteMultiple float64
	}{
		{"claude-opus", 5, 0.1, 1.25},
		{"claude-sonnet", 3, 0.1, 1.25},
		{"claude-haiku", 1, 0.1, 1.25},
		// gpt-5.5: read is 0.1x input ($0.50/1M); write is 1.0x input (no premium).
		{"gpt-5.5", 5, 0.1, 1.0},
	}
	for _, c := range cases {
		t.Run(c.family, func(t *testing.T) {
			r, ok := familyRates[c.family]
			if !ok {
				t.Fatalf("family %q absent from familyRates", c.family)
			}
			wantInput := c.inputPerMillion / 1_000_000
			approx(t, r.inputPerToken, wantInput)
			approx(t, r.cacheReadPerToken, wantInput*c.cacheReadMultiple)
			approx(t, r.cacheWritePerToken, wantInput*c.cacheWriteMultiple)
		})
	}
}

// TestCostWithCache_PricesEachBucket asserts CostWithCache prices fresh input +
// output exactly as Cost and ADDS cache reads at the read rate and cache writes
// at the write rate (#1343). The expected USD is computed from the published
// per-million figures: opus input 5, output 25, cacheRead 0.5, cacheWrite 6.25.
func TestCostWithCache_PricesEachBucket(t *testing.T) {
	const (
		fresh      = 1_000_000
		cacheRead  = 2_000_000
		cacheWrite = 500_000
		output     = 1_000_000
	)
	got, ok := CostWithCache("claude-opus-4-8", fresh, cacheRead, cacheWrite, output)
	if !ok {
		t.Fatal("ok = false for a known model")
	}
	// 1M*$5 + 1M*$25 + 2M*$0.50 + 0.5M*$6.25 = 5 + 25 + 1 + 3.125 = 34.125
	want := 5.0 + 25.0 + 2*0.5 + 0.5*6.25
	approx(t, got, want)
}

// TestCostWithCache_ReducesToCost is the back-compat pin (#1343 binding
// condition 2): with both cache buckets zero, CostWithCache returns EXACTLY
// what Cost returns for the same fresh input + output, so non-cache-aware
// callers are unaffected.
func TestCostWithCache_ReducesToCost(t *testing.T) {
	for _, model := range []string{"claude-opus-4-8", "claude-sonnet-4-6", "claude-haiku-4-5", "gpt-5.5"} {
		t.Run(model, func(t *testing.T) {
			withCache, ok1 := CostWithCache(model, 1234, 0, 0, 5678)
			plain, ok2 := Cost(model, 1234, 5678)
			if !ok1 || !ok2 {
				t.Fatalf("ok = %v / %v, want both true for known model %q", ok1, ok2, model)
			}
			if withCache != plain {
				t.Errorf("CostWithCache(%q, in, 0, 0, out) = %v, want %v (== Cost)", model, withCache, plain)
			}
		})
	}
}

// TestCostWithCache_UnknownModel asserts an unknown/empty model id returns
// (0, false) rather than guessing, mirroring Cost.
func TestCostWithCache_UnknownModel(t *testing.T) {
	for _, model := range []string{"", "gpt-4o", "llama-3", "claude"} {
		got, ok := CostWithCache(model, 1000, 1000, 1000, 1000)
		if ok {
			t.Errorf("CostWithCache(%q) ok = true, want false", model)
		}
		if got != 0 {
			t.Errorf("CostWithCache(%q) usd = %v, want 0 for unknown model", model, got)
		}
	}
}

// TestCostWithCache_NegativeClamp asserts EACH count is independently clamped
// to zero (#1343): a negative fresh, cacheRead, cacheWrite, or output must not
// drag the priced total negative. One assertion per clamped arg, holding the
// others at a known-positive value so only the clamped term is exercised.
func TestCostWithCache_NegativeClamp(t *testing.T) {
	const model = "claude-opus-4-8"
	// Baselines for the three non-clamped args in each case, priced from the
	// opus per-million rates (input 5, output 25, cacheRead 0.5, cacheWrite 6.25).
	tests := []struct {
		name                                 string
		fresh, cacheRead, cacheWrite, output int
		want                                 float64
	}{
		{"negative fresh clamps", -1_000_000, 0, 0, 1_000_000, 25},      // only output prices
		{"negative cacheRead clamps", 0, -1_000_000, 0, 1_000_000, 25},  // only output prices
		{"negative cacheWrite clamps", 0, 0, -1_000_000, 1_000_000, 25}, // only output prices
		{"negative output clamps", 1_000_000, 0, 0, -1_000_000, 5},      // only fresh input prices
		{"all negative is zero", -5, -5, -5, -5, 0},                     // every term clamped
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := CostWithCache(model, tt.fresh, tt.cacheRead, tt.cacheWrite, tt.output)
			if !ok {
				t.Fatal("ok = false for a known model")
			}
			approx(t, got, tt.want)
		})
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
