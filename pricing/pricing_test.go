package pricing

import (
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
			name:   "gpt-5.6-sol flagship tier",
			model:  "gpt-5.6-sol",
			input:  1_000_000,
			output: 1_000_000,
			want:   5 + 30, // $5 input + $30 output per 1M
		},
		{
			name:   "gpt-5.6-terra mid tier",
			model:  "gpt-5.6-terra",
			input:  1_000_000,
			output: 1_000_000,
			want:   2.5 + 15, // $2.50 input + $15 output per 1M
		},
		{
			name:   "gpt-5.6-luna cost-optimized tier",
			model:  "gpt-5.6-luna",
			input:  1_000_000,
			output: 1_000_000,
			want:   1 + 6, // $1 input + $6 output per 1M
		},
		{
			name:   "gpt-6-astra exact family id",
			model:  "gpt-6-astra",
			input:  1_000_000,
			output: 1_000_000,
			want:   10 + 50, // $10 input + $50 output per 1M
		},
		{
			name:   "gpt-6-astra dated point release inherits family rate",
			model:  "gpt-6-astra-20260901",
			input:  2_000_000,
			output: 1_000_000,
			want:   2*10 + 1*50,
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
// with claude-opus-5 (plan executor), claude-fable-5 (both claudecode
// reviewers) and gpt-6-astra (both codex reviewers, since #3234).
// gpt-5.6-terra is retained as future-swap insurance — it was the codex
// reviewer before #3234 and stays priced so a swap back can't silently
// record $0. claude-sonnet-5 is included so a future family-prefix change
// can't silently drop it (it prices via the claude-sonnet prefix). The
// pricing module is standalone (no dependency on backend/server or the
// spec), so this literal list is the manual mirror, and this comment is
// the guard against it going stale when a maintainer changes a dispatched
// model. TestLiveModelIDs_CoverWorkflowPinnedModels below closes the
// workflow half of that gap mechanically.
func TestCost_PricesLiveModelIDs(t *testing.T) {
	live := []string{
		"claude-opus-4-8",
		"claude-fable-5",
		"claude-sonnet-4-6",
		"claude-sonnet-5",
		"gpt-5.5",
		"gpt-5.6-terra",
		"gpt-6-astra",
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
	for _, model := range []string{"claude-opus-4-8", "gpt-6-astra"} {
		got, ok := Cost(model, -100, -100)
		if !ok {
			t.Fatalf("Cost(%q) ok = false for known model", model)
		}
		if got != 0 {
			t.Errorf("Cost(%q) usd = %v, want 0 (negative tokens clamped)", model, got)
		}
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
		// gpt-5.6 tiers: read = 0.1x input, write = 1.25x input (the 5.6 premium).
		{family: "gpt-5.6-sol", wantReadMultiplier: 0.1, wantReadPerToken: 0.5 / 1_000_000, wantWritePerToken: 6.25 / 1_000_000},
		{family: "gpt-5.6-terra", wantReadMultiplier: 0.1, wantReadPerToken: 0.25 / 1_000_000, wantWritePerToken: 3.125 / 1_000_000},
		{family: "gpt-5.6-luna", wantReadMultiplier: 0.1, wantReadPerToken: 0.1 / 1_000_000, wantWritePerToken: 1.25 / 1_000_000},
		// gpt-6-astra: read = $1/1M (0.1x its $10/1M input), write = $12.50/1M (1.25x).
		{family: "gpt-6-astra", wantReadMultiplier: 0.1, wantReadPerToken: 1.0 / 1_000_000, wantWritePerToken: 12.5 / 1_000_000},
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
	// The whole gpt-5.6 family and gpt-6-astra carry the same 1.25x write
	// premium (unlike gpt-5.5).
	for _, family := range []string{"claude-opus", "claude-fable", "claude-sonnet", "claude-haiku", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-astra"} {
		r := familyRates[family]
		approx(t, r.cacheWritePerToken, 1.25*r.inputPerToken)
	}
}

// TestCostWithCache_ReducesToCost pins the back-compat invariant (#1343):
// CostWithCache(model, in, 0, 0, out) must equal Cost(model, in, out) exactly
// for every live model id, so a non-cache-aware caller routed through the new
// entry point is unaffected.
func TestCostWithCache_ReducesToCost(t *testing.T) {
	for _, model := range []string{"claude-opus-4-8", "claude-fable-5", "claude-sonnet-4-6", "claude-haiku-4-5", "gpt-5.5", "gpt-5.6-terra", "gpt-6-astra"} {
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

	// astra: input 10, output 50, cacheRead 1, cacheWrite 12.5 per 1M.
	got, ok = CostWithCache("gpt-6-astra", 1_000_000, 1_000_000, 1_000_000, 1_000_000)
	if !ok {
		t.Fatal("ok = false for known model gpt-6-astra")
	}
	approx(t, got, 10+1+12.5+50) // = 73.5
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

// workflowPinnedModels reads the workflow spec at path as TEXT and
// scrapes every `model: <id>` declaration. Deliberately not a YAML
// parse and not a spec-package import: the pricing module is standalone
// and must not gain a dependency on backend/internal/spec or a yaml
// library. The scrape can under-report on an exotic YAML form (quoted,
// folded, anchored or alias-referenced values) — it tightens a guard
// and can never wrongly fail. A comment line never matches because the
// pattern anchors `model:` at the start of the (indented) line.
func workflowPinnedModels(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var models []string
	re := regexp.MustCompile(`^\s*model:\s*(\S+)`)
	for _, line := range strings.Split(string(raw), "\n") {
		if m := re.FindStringSubmatch(line); m != nil {
			models = append(models, m[1])
		}
	}
	return models, nil
}

// TestLiveModelIDs_CoverWorkflowPinnedModels is the mechanical half of
// the live-model completeness guard: every model id pinned in the
// repo's own .fishhawk/workflows.yaml must price (ok==true), so a
// future workflow model pin the table does not cover fails in-loop
// instead of silently recording $0 (the #3235 gap: #3234 pinned
// gpt-6-astra with no matching familyRates entry). `scripts/test
// verify` runs the pricing module from inside the monorepo (it iterates
// go.work module DiskPaths with each module as the working directory),
// so the relative path resolves and this guard EXECUTES on the
// committed-tree gate rather than silently skipping there. The skip
// exists for a vendored/standalone checkout of the module, where the
// file is absent.
func TestLiveModelIDs_CoverWorkflowPinnedModels(t *testing.T) {
	const path = "../.fishhawk/workflows.yaml"
	models, err := workflowPinnedModels(path)
	if os.IsNotExist(err) {
		t.Skipf("workflow spec %s absent — pricing module tested outside the monorepo; workflow-pin coverage not checked", path)
	}
	if err != nil {
		t.Fatalf("workflowPinnedModels(%q): %v", path, err)
	}
	if len(models) == 0 {
		t.Fatalf("workflowPinnedModels(%q) scraped no model pins — the scrape regex or the spec layout changed", path)
	}
	for _, model := range models {
		if _, ok := Cost(model, 1, 1); !ok {
			t.Errorf("Cost(%q) ok = false, want true — workflow-pinned model id is unpriced", model)
		}
	}
}

// TestWorkflowPinnedModels_AbsentFile pins the branch the guard's
// t.Skip rides on, without depending on the repo layout: a nonexistent
// path must surface as an os.IsNotExist error, not a scrape of nothing.
func TestWorkflowPinnedModels_AbsentFile(t *testing.T) {
	_, err := workflowPinnedModels(filepath.Join(t.TempDir(), "nope", "workflows.yaml"))
	if !os.IsNotExist(err) {
		t.Errorf("err = %v, want os.IsNotExist", err)
	}
}

// TestWorkflowPinnedModels_ScrapesPins proves the scrape itself (not
// just the happy path over the real spec): a pinned id is extracted, so
// the guard above would fail on an unpriced pin, and a commented-out
// pin is not.
func TestWorkflowPinnedModels_ScrapesPins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflows.yaml")
	spec := "stages:\n  - executor:\n      model: gpt-9-nonexistent\n  # model: commented-out-model\n"
	if err := os.WriteFile(path, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	models, err := workflowPinnedModels(path)
	if err != nil {
		t.Fatalf("workflowPinnedModels: %v", err)
	}
	if len(models) != 1 || models[0] != "gpt-9-nonexistent" {
		t.Errorf("models = %v, want [gpt-9-nonexistent]", models)
	}
	if _, ok := Cost("gpt-9-nonexistent", 1, 1); ok {
		t.Error("fixture model unexpectedly prices — pick a different sentinel id")
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
