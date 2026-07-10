package issuecomment

import (
	"fmt"
	"math"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/cost"
	"github.com/kuhlman-labs/fishhawk/backend/internal/latency"
)

// EconomicsInput bundles the three already-computed per-run rollups the
// economics block renders (#1702). Every figure is DERIVED from the run's
// existing audit data — the cost_recorded ledger (Cost, Cache) and the
// audit-chain timestamps (Latency) — so the block adds no new writes and no
// new gate. The caller (the notifier's anchor rebuild and the merge-time
// PR-body stamp) folds these before calling RenderEconomicsBlock:
//
//   - Cost:    cost.AggregateRunCost over the cost_recorded entries.
//   - Cache:   cost.AggregateCacheEfficiency over the same entries.
//   - Latency: latency.AggregateGateLatency over the whole chain.
type EconomicsInput struct {
	Cost    cost.RunCostSummary
	Cache   cost.CacheEfficiency
	Latency latency.Rollup
}

// RenderEconomicsBlock renders the compact per-change economics markdown
// block: total cost with the per-stage breakdown, end-to-end wall clock, the
// wait-on-human total with its per-gate breakdown, and cache net savings.
// Pure and deterministic — no IO, no clock — so both the living-anchor
// rebuild and the PR-body stamp render byte-identical output from the same
// rollups.
//
// Returns "" when there is genuinely nothing to report (no cost, no gate
// intervals, no wall clock, no cache activity) so the caller can drop the
// section rather than render an empty heading — mirroring the /cost,
// /cache-efficiency, and /latency read surfaces' "nothing to report" contract.
func RenderEconomicsBlock(in EconomicsInput) string {
	if economicsEmpty(in) {
		return ""
	}

	var b strings.Builder
	b.WriteString("**Economics**\n\n")

	fmt.Fprintf(&b, "- **Total cost**: %s\n", formatUSD(in.Cost.TotalUSD))
	// Stages arrive pre-sorted by source from cost.AggregateRunCost, so the
	// per-stage breakdown is already deterministic.
	for _, st := range in.Cost.Stages {
		fmt.Fprintf(&b, "  - `%s`: %s\n", st.Source, formatUSD(st.CostUSD))
	}

	if in.Latency.WallClockSeconds > 0 {
		fmt.Fprintf(&b, "- **Wall clock**: %s\n", formatDuration(in.Latency.WallClockSeconds))
	}

	if len(in.Latency.Gates) > 0 {
		fmt.Fprintf(&b, "- **Wait on human**: %s\n", formatDuration(in.Latency.TotalWaitOnHumanSeconds))
		for _, g := range in.Latency.Gates {
			fmt.Fprintf(&b, "  - %s: %s\n", economicsGateLabel(g.Gate), formatDuration(g.WaitSeconds))
		}
	}

	if hasCacheActivity(in.Cache) {
		// The savings figure is meaningful only against a denominator: it is
		// what the prompt cache saved versus replaying every cached prefix at
		// full uncached input price. Naming that baseline stops the line from
		// being misread as an absolute discount off the total (#1788).
		fmt.Fprintf(&b, "- **Cache net savings**: %s (vs uncached replay)\n", formatUSD(in.Cache.NetSavingsUSD))
	}

	return strings.TrimRight(b.String(), "\n")
}

// economicsEmpty reports whether the input carries no genuine economics
// signal — no cost, no per-stage rows, no gate intervals, and no cache
// activity. Wall clock alone does NOT keep the block: every run past its first
// audit entry has some wall clock, so triggering on it would render a
// content-free "$0.00 + wall clock" block for every early-stage run. Wall
// clock is shown only as a detail INSIDE a block already earned by cost, a
// gate, or cache. Guards RenderEconomicsBlock against emitting a bare heading.
func economicsEmpty(in EconomicsInput) bool {
	return in.Cost.TotalUSD == 0 &&
		len(in.Cost.Stages) == 0 &&
		len(in.Latency.Gates) == 0 &&
		!hasCacheActivity(in.Cache)
}

// hasCacheActivity reports whether the cache rollup carries any prompt-cache
// read/write tokens or a non-zero net dollar effect — the signal that the
// cache-net-savings line is worth showing.
func hasCacheActivity(c cost.CacheEfficiency) bool {
	return c.CacheReadTokens > 0 || c.CacheWriteTokens > 0 || c.NetSavingsUSD != 0
}

// economicsGateLabel maps a latency gate name to a compact human label for
// the wait-on-human breakdown. Unknown gate names pass through verbatim so a
// future gate still renders (never silently dropped).
func economicsGateLabel(gate string) string {
	switch gate {
	case latency.GatePlanApproval:
		return "plan approval"
	case latency.GateImplementReviewToDispatch:
		return "implement review → dispatch"
	case latency.GateChecksGreenToMerge:
		return "checks green → merge"
	default:
		return gate
	}
}

// formatUSD renders a dollar figure for the economics block. It shows two
// decimals for the common case, but four decimals for a sub-cent, non-zero
// magnitude so a genuinely cheap run's cost is never hidden as "$0.00".
func formatUSD(v float64) string {
	if v != 0 && math.Abs(v) < 0.01 {
		return fmt.Sprintf("$%.4f", v)
	}
	return fmt.Sprintf("$%.2f", v)
}

// formatDuration renders a wait/wall-clock interval (in seconds) as a compact
// human string: "2h 14m" at hour scale, "5m 30s" (or "5m" when whole) at
// minute scale, and "42s" below a minute. A negative input clamps to 0 so a
// caller that hasn't itself clamped never renders a negative duration.
func formatDuration(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}
	total := int(seconds + 0.5) // round to nearest whole second
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		if s > 0 {
			return fmt.Sprintf("%dm %ds", m, s)
		}
		return fmt.Sprintf("%dm", m)
	default:
		return fmt.Sprintf("%ds", s)
	}
}
