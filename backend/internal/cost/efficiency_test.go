package cost

import (
	"math"
	"testing"
)

const eff_eps = 1e-9

func approxEqual(got, want float64) bool {
	return math.Abs(got-want) <= eff_eps
}

// findStage returns the per-source rollup with the given source, or fails.
func findStage(t *testing.T, stages []StageCacheEfficiency, source string) StageCacheEfficiency {
	t.Helper()
	for _, st := range stages {
		if st.Source == source {
			return st
		}
	}
	t.Fatalf("no stage rollup for source %q; got %+v", source, stages)
	return StageCacheEfficiency{}
}

// TestAggregateCacheEfficiency_MultiModelMultiStage is the done-means
// behavioral assertion: synthetic opus + gpt-5.5 entries across the
// plan_review / implement_review / agent (no-source) sources fold into the
// hand-computed per-run ratios, net savings, and per-stage breakdown.
//
// Pricing (per 1M tokens, see pricing/pricing.go):
//
//	opus    input 5  cacheRead 0.5  cacheWrite 6.25
//	gpt-5.5 input 5  cacheRead 0.5  cacheWrite 5
//
// Per 1M cacheRead: gross = 5 − 0.5 = 4.5 (both families).
// Per 1M cacheWrite: opus penalty = 6.25 − 5 = 1.25; gpt penalty = 5 − 5 = 0.
func TestAggregateCacheEfficiency_MultiModelMultiStage(t *testing.T) {
	const M = 1_000_000
	entries := []CacheEfficiencyEntry{
		// opus, plan_review: read 3M, write 1M.
		{Model: "claude-opus-4-8", Source: "plan_review", FreshInput: 1 * M, CacheRead: 3 * M, CacheWrite: 1 * M},
		// gpt-5.5, implement_review: read 1M, write 1M (zero write penalty).
		{Model: "gpt-5.5-2026", Source: "implement_review", FreshInput: 1 * M, CacheRead: 1 * M, CacheWrite: 1 * M},
		// opus, NO source (runner stage-agent path): read 0, write 0.
		{Model: "claude-opus-4-8", Source: "", FreshInput: 2 * M, CacheRead: 0, CacheWrite: 0},
	}

	got := AggregateCacheEfficiency(entries)

	// Per-run token totals.
	if got.FreshInputTokens != 4*M || got.CacheReadTokens != 4*M ||
		got.CacheWriteTokens != 2*M || got.OutputTokens != 0 {
		t.Fatalf("run token totals = %+v, want fresh=4M read=4M write=2M out=0", got)
	}
	// cache_read_ratio = 4M/(4M+4M) = 0.5; reuse = 4M/max(2M,1) = 2.0.
	if !approxEqual(got.CacheReadRatio, 0.5) {
		t.Errorf("CacheReadRatio = %v, want 0.5", got.CacheReadRatio)
	}
	if !approxEqual(got.ReuseFactor, 2.0) {
		t.Errorf("ReuseFactor = %v, want 2.0", got.ReuseFactor)
	}
	// gross = 13.5 (opus) + 4.5 (gpt) + 0 = 18.0; penalty = 1.25 + 0 + 0 = 1.25.
	if !approxEqual(got.GrossReadSavingsUSD, 18.0) {
		t.Errorf("GrossReadSavingsUSD = %v, want 18.0", got.GrossReadSavingsUSD)
	}
	if !approxEqual(got.WritePenaltyUSD, 1.25) {
		t.Errorf("WritePenaltyUSD = %v, want 1.25", got.WritePenaltyUSD)
	}
	if !approxEqual(got.NetSavingsUSD, 16.75) {
		t.Errorf("NetSavingsUSD = %v, want 16.75", got.NetSavingsUSD)
	}

	// Stages sorted by source: agent, implement_review, plan_review.
	if len(got.Stages) != 3 {
		t.Fatalf("want 3 stage rollups, got %d: %+v", len(got.Stages), got.Stages)
	}
	wantOrder := []string{"agent", "implement_review", "plan_review"}
	for i, src := range wantOrder {
		if got.Stages[i].Source != src {
			t.Errorf("Stages[%d].Source = %q, want %q (sorted)", i, got.Stages[i].Source, src)
		}
	}

	plan := findStage(t, got.Stages, "plan_review")
	if !approxEqual(plan.CacheReadRatio, 0.75) || !approxEqual(plan.ReuseFactor, 3.0) ||
		!approxEqual(plan.NetSavingsUSD, 12.25) {
		t.Errorf("plan_review stage = %+v, want ratio 0.75 reuse 3.0 net 12.25", plan)
	}
	impl := findStage(t, got.Stages, "implement_review")
	if !approxEqual(impl.CacheReadRatio, 0.5) || !approxEqual(impl.ReuseFactor, 1.0) ||
		!approxEqual(impl.NetSavingsUSD, 4.5) || !approxEqual(impl.WritePenaltyUSD, 0) {
		t.Errorf("implement_review stage = %+v, want ratio 0.5 reuse 1.0 net 4.5 penalty 0", impl)
	}
}

// findCostStage returns the per-source cost rollup with the given source, or
// fails.
func findCostStage(t *testing.T, stages []RunCostStage, source string) RunCostStage {
	t.Helper()
	for _, st := range stages {
		if st.Source == source {
			return st
		}
	}
	t.Fatalf("no cost stage rollup for source %q; got %+v", source, stages)
	return RunCostStage{}
}

// TestAggregateRunCost_MixedSources: agent (no-source) + plan_review +
// implement_review entries fold into the correct total and per-stage sums
// with deterministic source ordering.
func TestAggregateRunCost_MixedSources(t *testing.T) {
	got := AggregateRunCost([]RunCostEntry{
		{Source: "plan_review", USD: 1.50},
		{Source: "implement_review", USD: 2.25},
		{Source: "", USD: 4.00}, // runner stage-agent path, no source
		{Source: "implement_review", USD: 0.75},
	})

	if !approxEqual(got.TotalUSD, 8.50) {
		t.Errorf("TotalUSD = %v, want 8.50", got.TotalUSD)
	}
	// Stages sorted by source: agent, implement_review, plan_review.
	if len(got.Stages) != 3 {
		t.Fatalf("want 3 stage rollups, got %d: %+v", len(got.Stages), got.Stages)
	}
	wantOrder := []string{"agent", "implement_review", "plan_review"}
	for i, src := range wantOrder {
		if got.Stages[i].Source != src {
			t.Errorf("Stages[%d].Source = %q, want %q (sorted)", i, got.Stages[i].Source, src)
		}
	}
	if st := findCostStage(t, got.Stages, "agent"); !approxEqual(st.CostUSD, 4.00) {
		t.Errorf("agent stage = %v, want 4.00", st.CostUSD)
	}
	if st := findCostStage(t, got.Stages, "implement_review"); !approxEqual(st.CostUSD, 3.00) {
		t.Errorf("implement_review stage = %v, want 3.00 (0.75+2.25)", st.CostUSD)
	}
	if st := findCostStage(t, got.Stages, "plan_review"); !approxEqual(st.CostUSD, 1.50) {
		t.Errorf("plan_review stage = %v, want 1.50", st.CostUSD)
	}
}

// TestAggregateRunCost_EmptySourceBucketsAsAgent: an entry with an empty
// Source is bucketed under the `agent` stage.
func TestAggregateRunCost_EmptySourceBucketsAsAgent(t *testing.T) {
	got := AggregateRunCost([]RunCostEntry{{Source: "", USD: 1.23}})
	if len(got.Stages) != 1 {
		t.Fatalf("want 1 stage, got %d: %+v", len(got.Stages), got.Stages)
	}
	if got.Stages[0].Source != StageAgentSource {
		t.Errorf("Stages[0].Source = %q, want %q", got.Stages[0].Source, StageAgentSource)
	}
	if !approxEqual(got.Stages[0].CostUSD, 1.23) {
		t.Errorf("agent stage = %v, want 1.23", got.Stages[0].CostUSD)
	}
}

// TestAggregateRunCost_Empty: an empty slice yields the zero summary with no
// stages.
func TestAggregateRunCost_Empty(t *testing.T) {
	got := AggregateRunCost(nil)
	if got.TotalUSD != 0 {
		t.Errorf("TotalUSD = %v, want 0", got.TotalUSD)
	}
	if got.Stages != nil {
		t.Errorf("Stages = %+v, want nil for empty input", got.Stages)
	}
}

// TestAggregateCacheEfficiency_DivByZeroGuard: an entry with no cache read
// and no fresh input must yield cache_read_ratio 0, never NaN.
func TestAggregateCacheEfficiency_DivByZeroGuard(t *testing.T) {
	got := AggregateCacheEfficiency([]CacheEfficiencyEntry{
		{Model: "claude-opus-4-8", Source: "agent", FreshInput: 0, CacheRead: 0, CacheWrite: 0, Output: 1000},
	})
	if math.IsNaN(got.CacheReadRatio) {
		t.Fatalf("CacheReadRatio is NaN; want 0 for zero denominator")
	}
	if got.CacheReadRatio != 0 {
		t.Errorf("CacheReadRatio = %v, want 0 when cache_read+fresh == 0", got.CacheReadRatio)
	}
}

// TestAggregateCacheEfficiency_ReuseFactorZeroWrite: cache_write == 0 must
// divide by max(write, 1), not 0 — so reuse_factor equals cache_read.
func TestAggregateCacheEfficiency_ReuseFactorZeroWrite(t *testing.T) {
	const M = 1_000_000
	got := AggregateCacheEfficiency([]CacheEfficiencyEntry{
		{Model: "claude-opus-4-8", Source: "agent", FreshInput: 1 * M, CacheRead: 5 * M, CacheWrite: 0},
	})
	if math.IsInf(got.ReuseFactor, 0) {
		t.Fatalf("ReuseFactor is Inf; want cache_read/max(write,1)")
	}
	if !approxEqual(got.ReuseFactor, 5*M) {
		t.Errorf("ReuseFactor = %v, want %v (cache_read / max(0,1))", got.ReuseFactor, float64(5*M))
	}
}

// TestAggregateCacheEfficiency_UnknownModel: an unknown model contributes 0
// savings while its tokens still count toward the totals and ratios.
func TestAggregateCacheEfficiency_UnknownModel(t *testing.T) {
	const M = 1_000_000
	got := AggregateCacheEfficiency([]CacheEfficiencyEntry{
		{Model: "some-unrecognized-model", Source: "agent", FreshInput: 1 * M, CacheRead: 1 * M, CacheWrite: 1 * M},
	})
	if got.GrossReadSavingsUSD != 0 || got.WritePenaltyUSD != 0 || got.NetSavingsUSD != 0 {
		t.Errorf("unknown-model savings = gross %v penalty %v net %v, want all 0",
			got.GrossReadSavingsUSD, got.WritePenaltyUSD, got.NetSavingsUSD)
	}
	// Tokens still aggregate, so the ratios are real.
	if got.CacheReadTokens != 1*M || got.FreshInputTokens != 1*M {
		t.Errorf("unknown-model tokens dropped: %+v", got)
	}
	if !approxEqual(got.CacheReadRatio, 0.5) {
		t.Errorf("CacheReadRatio = %v, want 0.5 (tokens count even for unknown model)", got.CacheReadRatio)
	}
}

// TestAggregateCacheEfficiency_EmptySourceBucketsAsAgent: an entry with an
// empty Source is bucketed under the `agent` stage.
func TestAggregateCacheEfficiency_EmptySourceBucketsAsAgent(t *testing.T) {
	got := AggregateCacheEfficiency([]CacheEfficiencyEntry{
		{Model: "claude-opus-4-8", Source: "", FreshInput: 1000, CacheRead: 100, CacheWrite: 10},
	})
	if len(got.Stages) != 1 {
		t.Fatalf("want 1 stage, got %d: %+v", len(got.Stages), got.Stages)
	}
	if got.Stages[0].Source != StageAgentSource {
		t.Errorf("Stages[0].Source = %q, want %q", got.Stages[0].Source, StageAgentSource)
	}
}

// TestAggregateCacheEfficiency_Empty: an empty slice yields the all-zero
// value with no stages.
func TestAggregateCacheEfficiency_Empty(t *testing.T) {
	got := AggregateCacheEfficiency(nil)
	if got.FreshInputTokens != 0 || got.CacheReadTokens != 0 || got.CacheWriteTokens != 0 ||
		got.OutputTokens != 0 || got.CacheReadRatio != 0 || got.ReuseFactor != 0 ||
		got.GrossReadSavingsUSD != 0 || got.WritePenaltyUSD != 0 || got.NetSavingsUSD != 0 {
		t.Errorf("empty aggregation = %+v, want all-zero", got)
	}
	if got.Stages != nil {
		t.Errorf("Stages = %+v, want nil for empty input", got.Stages)
	}
}
