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
			name:   "fable premium tier",
			model:  "claude-fable-5",
			input:  1_000_000,
			output: 1_000_000,
			want:   10 + 50, // $10 input + $50 output per 1M
		},
		{
			name:   "gpt-5.6-terra mid tier",
			model:  "gpt-5.6-terra",
			input:  1_000_000,
			output: 1_000_000,
			want:   2.5 + 15, // $2.50 input + $15 output per 1M
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
// recording $0. Two sources feed this list: the default allow-list in
// backend/cmd/fishhawkd/serve.go and backend/internal/server/modelpolicy.go
// (claudecode=claude-opus-4-8,claude-sonnet-4-6; codex=gpt-5.5), AND the
// models pinned in .fishhawk/workflows.yaml, which overrides the defaults
// with claude-fable-5 (planner/executor) and gpt-5.6-terra (codex
// reviewer). claude-sonnet-5 is included so a future family-prefix change
// can't silently drop it (it prices via the claude-sonnet prefix). The
// pricing module is standalone (no dependency on backend/server or the
// spec), so this literal list is the manual mirror, and this comment is
// the guard against it going stale when a maintainer changes a dispatched
// model.
func TestCost_PricesLiveModelIDs(t *testing.T) {
	live := []string{
		"claude-opus-4-8",
		"claude-fable-5",
		"claude-sonnet-4-6",
		"claude-sonnet-5",
		"gpt-5.5",
		"gpt-5.6-terra",
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

// TestCacheRates_Multipliers is the values/drift guard for the cache-aware
// rates (#1343): a wrong constant fails CI here rather than silently
// mispricing cached tokens. The Anthropic families price cache READ at 0.1x
// the family input rate and cache WRITE at 1.25x the family input rate (the
// 5-minute-TTL baseline), verified against the Anthropic prompt-caching
// pricing. gpt-5.5 prices cache READ at OpenAI's published cached-input
// discount ($0.50/1M = 0.1x its $5/1M input rate) and cache WRITE at the
// input rate (no separate write premium).
func TestCacheRates_Multipliers(t *testing.T) {
	tests := []struct {
		family             string
		wantReadMultiplier float64 // cacheReadPerToken / inputPerToken
		wantWritePerToken  float64 // absolute, $/token
		wantReadPerToken   float64 // absolute, $/token
	}{
		// Anthropic: read = 0.1x input, write = 1.25x input.
		{family: "claude-opus", wantReadMultiplier: 0.1, wantReadPerToken: 0.5 / 1_000_000, wantWritePerToken: 6.25 / 1_000_000},
		{family: "claude-fable", wantReadMultiplier: 0.1, wantReadPerToken: 1.0 / 1_000_000, wantWritePerToken: 12.5 / 1_000_000},
		{family: "claude-sonnet", wantReadMultiplier: 0.1, wantReadPerToken: 0.3 / 1_000_000, wantWritePerToken: 3.75 / 1_000_000},
		{family: "claude-haiku", wantReadMultiplier: 0.1, wantReadPerToken: 0.1 / 1_000_000, wantWritePerToken: 1.25 / 1_000_000},
		// gpt-5.5: read = $0.50/1M (0.1x input), write = input rate ($5/1M).
		{family: "gpt-5.5", wantReadMultiplier: 0.1, wantReadPerToken: 0.5 / 1_000_000, wantWritePerToken: 5.0 / 1_000_000},
		// gpt-5.6-terra: read = $0.25/1M (0.1x input), write = 1.25x input ($3.125/1M).
		{family: "gpt-5.6-terra", wantReadMultiplier: 0.1, wantReadPerToken: 0.25 / 1_000_000, wantWritePerToken: 3.125 / 1_000_000},
	}
	for _, tc := range tests {
		t.Run(tc.family, func(t *testing.T) {
			r, ok := familyRates[tc.family]
			if !ok {
				t.Fatalf("family %q absent from familyRates", tc.family)
			}
			approx(t, r.cacheReadPerToken, tc.wantReadPerToken)
			approx(t, r.cacheWritePerToken, tc.wantWritePerToken)
			// Cross-check the read multiplier against the family input rate so a
			// future input-rate change must move the cache-read rate in lockstep.
			approx(t, r.cacheReadPerToken, tc.wantReadMultiplier*r.inputPerToken)
		})
	}
	// Anthropic write is 1.25x input; pin the multiplier directly too.
	// gpt-5.6-terra also carries the 1.25x write premium (unlike gpt-5.5).
	for _, family := range []string{"claude-opus", "claude-fable", "claude-sonnet", "claude-haiku", "gpt-5.6-terra"} {
		r := familyRates[family]
		approx(t, r.cacheWritePerToken, 1.25*r.inputPerToken)
	}
}

// TestCostWithCache_ReducesToCost pins the back-compat invariant (#1343):
// CostWithCache(model, in, 0, 0, out) must equal Cost(model, in, out) exactly
// for every live model id, so a non-cache-aware caller routed through the new
// entry point is unaffected.
func TestCostWithCache_ReducesToCost(t *testing.T) {
	for _, model := range []string{"claude-opus-4-8", "claude-fable-5", "claude-sonnet-4-6", "claude-haiku-4-5", "gpt-5.5", "gpt-5.6-terra"} {
		t.Run(model, func(t *testing.T) {
			wantUSD, wantOK := Cost(model, 1_234_567, 89_012)
			gotUSD, gotOK := CostWithCache(model, 1_234_567, 0, 0, 89_012)
			if gotOK != wantOK {
				t.Fatalf("CostWithCache ok = %v, want %v (must match Cost)", gotOK, wantOK)
			}
			approx(t, gotUSD, wantUSD)
		})
	}
}

// TestCostWithCache_PricesAllFour asserts the cache-aware total sums fresh
// input, cache read, cache write, and output each at their own rate.
func TestCostWithCache_PricesAllFour(t *testing.T) {
	// opus: input 5, output 25, cacheRead 0.5, cacheWrite 6.25 per 1M.
	got, ok := CostWithCache("claude-opus-4-8", 1_000_000, 1_000_000, 1_000_000, 1_000_000)
	if !ok {
		t.Fatal("ok = false for known model")
	}
	approx(t, got, 5+0.5+6.25+25)
}

// TestCostWithCache_NegativeClamped pins the per-arg defensive clamp: each of
// the four counts is clamped to 0 independently, so a malformed usage block in
// any single field can never produce a negative ledger entry.
func TestCostWithCache_NegativeClamped(t *testing.T) {
	cases := []struct {
		name                                 string
		fresh, cacheRead, cacheWrite, output int
	}{
		{"negative fresh", -1_000_000, 1_000_000, 1_000_000, 1_000_000},
		{"negative cacheRead", 1_000_000, -1_000_000, 1_000_000, 1_000_000},
		{"negative cacheWrite", 1_000_000, 1_000_000, -1_000_000, 1_000_000},
		{"negative output", 1_000_000, 1_000_000, 1_000_000, -1_000_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CostWithCache("claude-opus-4-8", tc.fresh, tc.cacheRead, tc.cacheWrite, tc.output)
			if !ok {
				t.Fatal("ok = false for known model")
			}
			// The one negative arg contributes 0; the other three (each 1M) price
			// at their own rate. Equivalent to CostWithCache with the negative arg
			// replaced by 0.
			want, _ := CostWithCache("claude-opus-4-8", max0(tc.fresh), max0(tc.cacheRead), max0(tc.cacheWrite), max0(tc.output))
			approx(t, got, want)
			if got < 0 {
				t.Errorf("usd = %v, want >= 0 (negative count must clamp)", got)
			}
		})
	}
	// All-negative degrades to exactly 0.
	got, ok := CostWithCache("claude-opus-4-8", -1, -1, -1, -1)
	if !ok {
		t.Fatal("ok = false for known model")
	}
	if got != 0 {
		t.Errorf("usd = %v, want 0 (all counts negative → clamped to 0)", got)
	}
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// TestCostWithCache_UnknownModel pins the fail-open contract: an unknown or
// empty model id returns (0, false), matching Cost.
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
