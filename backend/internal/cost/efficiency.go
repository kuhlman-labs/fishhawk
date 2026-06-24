package cost

import (
	"sort"

	"github.com/kuhlman-labs/fishhawk/pricing"
)

// StageAgentSource is the synthetic per-stage bucket for a cost_recorded
// entry that carries NO `source` key — the runner stage-agent path
// (trace.go::recordCost emits no source). Reviewer entries
// (recordReviewerCost) carry an explicit source in {plan_review,
// implement_review}; everything else folds here so the per-stage
// breakdown always names a concrete bucket (ADR-044 slice 3 / #1352).
const StageAgentSource = "agent"

// CacheEfficiencyEntry is one cost_recorded ledger row's cache-relevant
// token split plus the model and source it was attributed to. FreshInput
// is the cache-EXCLUSIVE input (the ledger's `input_tokens` since #1349);
// CacheRead / CacheWrite are the prompt-cache read/write split. Source is
// the audit payload's `source` ("" for the runner stage-agent path, which
// AggregateCacheEfficiency buckets as StageAgentSource).
type CacheEfficiencyEntry struct {
	Model      string
	Source     string
	FreshInput int
	CacheRead  int
	CacheWrite int
	Output     int
}

// StageCacheEfficiency is the per-source rollup: the same token totals,
// ratios, and dollar figures as the per-run CacheEfficiency, summed over
// only the entries that carried this Source.
type StageCacheEfficiency struct {
	Source              string  `json:"source"`
	FreshInputTokens    int     `json:"fresh_input_tokens"`
	CacheReadTokens     int     `json:"cache_read_tokens"`
	CacheWriteTokens    int     `json:"cache_write_tokens"`
	OutputTokens        int     `json:"output_tokens"`
	CacheReadRatio      float64 `json:"cache_read_ratio"`
	ReuseFactor         float64 `json:"reuse_factor"`
	GrossReadSavingsUSD float64 `json:"gross_read_savings_usd"`
	WritePenaltyUSD     float64 `json:"write_penalty_usd"`
	NetSavingsUSD       float64 `json:"net_savings_usd"`
}

// CacheEfficiency is the per-run cache-efficiency metric derived from a
// run's cost_recorded entries (ADR-044 slice 3 / #1352). It folds every
// entry's token split into per-run totals and derives:
//
//   - CacheReadRatio: cache_read / (cache_read + fresh_input), the share of
//     input that was served from cache; 0 when the denominator is 0.
//   - ReuseFactor: cache_read / max(cache_write, 1), how many times each
//     cache-creation token was re-read.
//   - GrossReadSavingsUSD: the dollars saved by serving CacheRead tokens at
//     the family cache-read discount instead of the fresh-input rate.
//   - WritePenaltyUSD: the extra dollars paid to WRITE CacheWrite tokens at
//     the family cache-write premium over the fresh-input rate.
//   - NetSavingsUSD: GrossReadSavingsUSD − WritePenaltyUSD.
//
// Stages carries the same rollup per source, sorted by source for
// deterministic output. DISPLAY-ONLY — derived from existing audit data,
// never a gate.
type CacheEfficiency struct {
	FreshInputTokens    int                    `json:"fresh_input_tokens"`
	CacheReadTokens     int                    `json:"cache_read_tokens"`
	CacheWriteTokens    int                    `json:"cache_write_tokens"`
	OutputTokens        int                    `json:"output_tokens"`
	CacheReadRatio      float64                `json:"cache_read_ratio"`
	ReuseFactor         float64                `json:"reuse_factor"`
	GrossReadSavingsUSD float64                `json:"gross_read_savings_usd"`
	WritePenaltyUSD     float64                `json:"write_penalty_usd"`
	NetSavingsUSD       float64                `json:"net_savings_usd"`
	Stages              []StageCacheEfficiency `json:"stages,omitempty"`
}

// AggregateCacheEfficiency folds a run's cost_recorded entries into the
// per-run CacheEfficiency and a per-source breakdown. It is pure (no
// repository, no error): an empty slice yields the all-zero value.
//
// The dollar figures accumulate PER ENTRY so each entry is priced against
// its own model — an unknown / unpriced model returns 0 from both pricing
// calls and contributes 0 savings while its tokens still count toward the
// token totals and ratios (the same fail-open contract recordCost uses).
func AggregateCacheEfficiency(entries []CacheEfficiencyEntry) CacheEfficiency {
	var run CacheEfficiency
	// Preserve first-seen insertion would be non-deterministic across the
	// map; we sort Stages by source at the end instead, so the map is only
	// for accumulation.
	stages := map[string]*StageCacheEfficiency{}

	for _, e := range entries {
		src := e.Source
		if src == "" {
			src = StageAgentSource
		}
		st, ok := stages[src]
		if !ok {
			st = &StageCacheEfficiency{Source: src}
			stages[src] = st
		}

		run.FreshInputTokens += e.FreshInput
		run.CacheReadTokens += e.CacheRead
		run.CacheWriteTokens += e.CacheWrite
		run.OutputTokens += e.Output
		st.FreshInputTokens += e.FreshInput
		st.CacheReadTokens += e.CacheRead
		st.CacheWriteTokens += e.CacheWrite
		st.OutputTokens += e.Output

		gross, penalty := entrySavings(e.Model, e.CacheRead, e.CacheWrite)
		run.GrossReadSavingsUSD += gross
		run.WritePenaltyUSD += penalty
		st.GrossReadSavingsUSD += gross
		st.WritePenaltyUSD += penalty
	}

	run.NetSavingsUSD = run.GrossReadSavingsUSD - run.WritePenaltyUSD
	run.CacheReadRatio = cacheReadRatio(run.CacheReadTokens, run.FreshInputTokens)
	run.ReuseFactor = reuseFactor(run.CacheReadTokens, run.CacheWriteTokens)

	for _, st := range stages {
		st.NetSavingsUSD = st.GrossReadSavingsUSD - st.WritePenaltyUSD
		st.CacheReadRatio = cacheReadRatio(st.CacheReadTokens, st.FreshInputTokens)
		st.ReuseFactor = reuseFactor(st.CacheReadTokens, st.CacheWriteTokens)
		run.Stages = append(run.Stages, *st)
	}
	sort.Slice(run.Stages, func(i, j int) bool {
		return run.Stages[i].Source < run.Stages[j].Source
	})
	return run
}

// entrySavings prices one entry's cache read/write against its model per
// ADR-044, reusing slice 1's pricing API with NO new pricing surface:
//
//	gross = Cost(model, cacheRead, 0) − CostWithCache(model, 0, cacheRead, 0, 0)
//	penalty = CostWithCache(model, 0, 0, cacheWrite, 0) − Cost(model, cacheWrite, 0)
//
// Because Cost prices its input at the fresh-input rate and CostWithCache
// prices cacheRead at the cache-read rate, gross is exactly
// cacheRead × (fresh_input_rate − cache_read_rate). An unknown model id
// returns 0 from both functions, so its contribution is 0.
func entrySavings(model string, cacheRead, cacheWrite int) (gross, penalty float64) {
	readFlat, _ := pricing.Cost(model, cacheRead, 0)
	readCached, _ := pricing.CostWithCache(model, 0, cacheRead, 0, 0)
	gross = readFlat - readCached

	writeFlat, _ := pricing.Cost(model, cacheWrite, 0)
	writeCached, _ := pricing.CostWithCache(model, 0, 0, cacheWrite, 0)
	penalty = writeCached - writeFlat
	return gross, penalty
}

// cacheReadRatio is cacheRead / (cacheRead + fresh), guarded to 0 when the
// denominator is 0 so an entry-less or zero-input bucket yields 0.0 rather
// than NaN.
func cacheReadRatio(cacheRead, fresh int) float64 {
	denom := cacheRead + fresh
	if denom == 0 {
		return 0
	}
	return float64(cacheRead) / float64(denom)
}

// reuseFactor is cacheRead / max(cacheWrite, 1): how many times each
// cache-creation token was re-read. The max(write, 1) floor avoids a
// divide-by-zero when nothing was written to cache (read-only or empty),
// in which case the factor degrades to cacheRead itself rather than ∞.
func reuseFactor(cacheRead, cacheWrite int) float64 {
	denom := cacheWrite
	if denom < 1 {
		denom = 1
	}
	return float64(cacheRead) / float64(denom)
}
