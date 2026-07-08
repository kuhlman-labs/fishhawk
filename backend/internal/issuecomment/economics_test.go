package issuecomment

import (
	"math"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/cost"
	"github.com/kuhlman-labs/fishhawk/backend/internal/latency"
)

// fullEconomics is the reference input for the rendered-content assertions:
// a run with per-stage cost, all three gate waits, wall clock, and cache
// savings. 2700s=45m, 1800s=30m, 900s=15m, total 5400s=1h30m, wall 8040s=2h14m.
func fullEconomics() EconomicsInput {
	return EconomicsInput{
		Cost: cost.RunCostSummary{
			TotalUSD: 0.42,
			Stages: []cost.RunCostStage{
				{Source: "agent", CostUSD: 0.30},
				{Source: "implement_review", CostUSD: 0.08},
				{Source: "plan_review", CostUSD: 0.04},
			},
		},
		Cache: cost.CacheEfficiency{
			CacheReadTokens:  1000,
			CacheWriteTokens: 200,
			NetSavingsUSD:    0.12,
		},
		Latency: latency.Rollup{
			Gates: []latency.GateWait{
				{Gate: latency.GatePlanApproval, WaitSeconds: 2700},
				{Gate: latency.GateImplementReviewToDispatch, WaitSeconds: 1800},
				{Gate: latency.GateChecksGreenToMerge, WaitSeconds: 900},
			},
			TotalWaitOnHumanSeconds: 5400,
			WallClockSeconds:        8040,
		},
	}
}

func TestRenderEconomicsBlock_FullContent(t *testing.T) {
	got := RenderEconomicsBlock(fullEconomics())
	for _, want := range []string{
		"**Economics**",
		"- **Total cost**: $0.42",
		"  - `agent`: $0.30",
		"  - `implement_review`: $0.08",
		"  - `plan_review`: $0.04",
		"- **Wall clock**: 2h 14m",
		"- **Wait on human**: 1h 30m",
		"  - plan approval: 45m",
		"  - implement review → dispatch: 30m",
		"  - checks green → merge: 15m",
		"- **Cache net savings**: $0.12",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered block missing %q:\n%s", want, got)
		}
	}
}

// TestRenderEconomicsBlock_Empty is the defensive branch: an all-zero input
// (no cost, no gates, no wall clock, no cache) renders as "" so the caller
// drops the section rather than showing a bare heading.
func TestRenderEconomicsBlock_Empty(t *testing.T) {
	if got := RenderEconomicsBlock(EconomicsInput{}); got != "" {
		t.Errorf("all-zero input should render empty; got %q", got)
	}
}

// TestRenderEconomicsBlock_CostOnly asserts the optional sections
// (wall clock, wait-on-human, cache) are each omitted when their rollup is
// empty — the block renders only what it has.
func TestRenderEconomicsBlock_CostOnly(t *testing.T) {
	got := RenderEconomicsBlock(EconomicsInput{
		Cost: cost.RunCostSummary{TotalUSD: 1.50, Stages: []cost.RunCostStage{{Source: "agent", CostUSD: 1.50}}},
	})
	if !strings.Contains(got, "- **Total cost**: $1.50") {
		t.Errorf("cost-only block missing total:\n%s", got)
	}
	for _, absent := range []string{"Wall clock", "Wait on human", "Cache net savings"} {
		if strings.Contains(got, absent) {
			t.Errorf("cost-only block should omit %q:\n%s", absent, got)
		}
	}
}

// TestRenderEconomicsBlock_PartialGates asserts the renderer surfaces exactly
// the gates the aggregator resolved — a run whose only measured gate is plan
// approval shows just that row.
func TestRenderEconomicsBlock_PartialGates(t *testing.T) {
	got := RenderEconomicsBlock(EconomicsInput{
		Latency: latency.Rollup{
			Gates:                   []latency.GateWait{{Gate: latency.GatePlanApproval, WaitSeconds: 60}},
			TotalWaitOnHumanSeconds: 60,
			WallClockSeconds:        120,
		},
	})
	if !strings.Contains(got, "plan approval: 1m") {
		t.Errorf("expected the plan-approval gate:\n%s", got)
	}
	if strings.Contains(got, "checks green") || strings.Contains(got, "implement review") {
		t.Errorf("unmeasured gates must not render:\n%s", got)
	}
}

// TestRenderEconomicsBlock_SubCentCost is the sub-cent formatting branch: a
// genuinely cheap run must not be hidden as "$0.00".
func TestRenderEconomicsBlock_SubCentCost(t *testing.T) {
	got := RenderEconomicsBlock(EconomicsInput{
		Cost: cost.RunCostSummary{TotalUSD: 0.0042, Stages: []cost.RunCostStage{{Source: "agent", CostUSD: 0.0042}}},
	})
	if !strings.Contains(got, "$0.0042") {
		t.Errorf("sub-cent cost should render four decimals:\n%s", got)
	}
	if strings.Contains(got, "$0.00\n") {
		t.Errorf("sub-cent cost must not collapse to $0.00:\n%s", got)
	}
}

// TestRenderEconomicsBlock_Reconciliation asserts the rendered total is the
// authoritative run total (run.CostUSDTotal), and that the derived per-stage
// sum from the same cost_recorded ledger reconciles with it. This mirrors the
// /cost surface, which reports run.CostUSDTotal as the total and the
// AggregateRunCost stages as the breakdown.
func TestRenderEconomicsBlock_Reconciliation(t *testing.T) {
	entries := []cost.RunCostEntry{
		{Source: "agent", USD: 0.25},
		{Source: "plan_review", USD: 0.10},
		{Source: "implement_review", USD: 0.05},
	}
	summary := cost.AggregateRunCost(entries) // TotalUSD = ledger sum = 0.40
	const authoritativeTotal = 0.40
	// The caller sets the authoritative rolled total (run.CostUSDTotal) while
	// keeping the per-stage breakdown from the ledger fold.
	if math.Abs(summary.TotalUSD-authoritativeTotal) > 1e-9 {
		t.Fatalf("ledger sum %v does not reconcile with run total %v", summary.TotalUSD, authoritativeTotal)
	}
	summary.TotalUSD = authoritativeTotal

	got := RenderEconomicsBlock(EconomicsInput{Cost: summary})
	if !strings.Contains(got, "- **Total cost**: $0.40") {
		t.Errorf("rendered total should equal run.CostUSDTotal ($0.40):\n%s", got)
	}
	// Per-stage sum reconciles with the total.
	var stageSum float64
	for _, st := range summary.Stages {
		stageSum += st.CostUSD
	}
	if math.Abs(stageSum-authoritativeTotal) > 1e-9 {
		t.Errorf("per-stage sum %v does not reconcile with total %v", stageSum, authoritativeTotal)
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		sec  float64
		want string
	}{
		{0, "0s"},
		{-5, "0s"}, // negative clamps to 0
		{42, "42s"},
		{60, "1m"},
		{90, "1m 30s"},
		{2700, "45m"},
		{3600, "1h 0m"},
		{8040, "2h 14m"},
	}
	for _, c := range cases {
		if got := formatDuration(c.sec); got != c.want {
			t.Errorf("formatDuration(%v) = %q, want %q", c.sec, got, c.want)
		}
	}
}

func TestFormatUSD(t *testing.T) {
	cases := []struct {
		v    float64
		want string
	}{
		{0, "$0.00"},
		{0.42, "$0.42"},
		{1.5, "$1.50"},
		{0.0042, "$0.0042"},
		{12.345, "$12.35"}, // >= 0.01 rounds to two decimals
	}
	for _, c := range cases {
		if got := formatUSD(c.v); got != c.want {
			t.Errorf("formatUSD(%v) = %q, want %q", c.v, got, c.want)
		}
	}
}

// TestEconomicsGateLabel covers the mapped gate names and the passthrough for
// an unknown gate (never silently dropped).
func TestEconomicsGateLabel(t *testing.T) {
	cases := map[string]string{
		latency.GatePlanApproval:              "plan approval",
		latency.GateImplementReviewToDispatch: "implement review → dispatch",
		latency.GateChecksGreenToMerge:        "checks green → merge",
		"some_future_gate":                    "some_future_gate",
	}
	for gate, want := range cases {
		if got := economicsGateLabel(gate); got != want {
			t.Errorf("economicsGateLabel(%q) = %q, want %q", gate, got, want)
		}
	}
}
