package repodash

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

// now is a Wednesday; its ISO week starts Monday 2026-09-28.
var now = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) // Wednesday

func win(weeks int) Window { return NewWindow(now, weeks) }

func ev(cat string, at time.Time) Event { return Event{Category: cat, Timestamp: at} }

func approve(at time.Time, stageType string) Event {
	return Event{Category: CategoryApprovalSubmitted, Timestamp: at, StageType: stageType, Decision: "approve"}
}

func reject(at time.Time) Event {
	return Event{Category: CategoryApprovalSubmitted, Timestamp: at, StageType: "plan", Decision: "reject"}
}

func verdict(v string) Event { return Event{Category: CategoryAcceptanceOutcome, Verdict: v} }

func TestRepoDash_WindowAndWeekStart(t *testing.T) {
	if got := WeekStart(now); !got.Equal(time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("WeekStart = %v, want Monday 2026-09-28", got)
	}
	// Sunday belongs to the week that started the previous Monday.
	if got := WeekStart(time.Date(2026, 9, 27, 23, 0, 0, 0, time.UTC)); !got.Equal(time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("WeekStart(Sunday) = %v", got)
	}
	w := win(3)
	if !w.Start.Equal(time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)) || !w.End.Equal(time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("window = [%v, %v)", w.Start, w.End)
	}
	if !w.ScanBoundary().Equal(w.Start.Add(-LookBack)) {
		t.Fatalf("ScanBoundary = %v", w.ScanBoundary())
	}
}

// TestRepoDash_EmptyWindowZeroNotNaN is the counterfactual vehicle for every
// zero-denominator guard: zero merged runs, zero approval samples, zero
// acceptance samples, zero runs. Each rate must be EXACTLY 0 and the folds
// must JSON-encode (NaN would fail json.Marshal).
func TestRepoDash_EmptyWindowZeroNotNaN(t *testing.T) {
	w := win(4)
	th := FoldThroughput(w, nil)
	h := FoldHealth(w, nil)
	e := FoldEconomics(w, nil)
	for name, v := range map[string]float64{
		"median_cycle_time":      th.MedianCycleTimeSeconds,
		"plan_first_shot":        h.PlanFirstShotApprovalRate,
		"fixup_rate":             h.FixupRate,
		"acceptance_pass_rate":   h.AcceptancePassRate,
		"cost_per_merged_change": e.CostPerMergedChangeUSD,
		"total_cost":             e.TotalCostUSD,
		"week0_cache_read_ratio": e.Weeks[0].CacheReadRatio,
	} {
		if v != 0 || math.IsNaN(v) {
			t.Errorf("%s = %v, want exactly 0", name, v)
		}
	}
	if len(th.Weeks) != 4 || th.MergedChanges != 0 || th.WaitOnHuman != nil {
		t.Errorf("throughput = %+v, want 4 zero buckets and no wait block", th)
	}
	if _, err := json.Marshal([]any{th, h, e}); err != nil {
		t.Fatalf("marshal empty folds: %v", err)
	}
}

func TestRepoDash_Ratio(t *testing.T) {
	if Ratio(0, 0) != 0 || Ratio(1, 4) != 0.25 {
		t.Fatalf("Ratio broken: %v %v", Ratio(0, 0), Ratio(1, 4))
	}
}

func TestRepoDash_Median(t *testing.T) {
	cases := []struct {
		in   []float64
		want float64
	}{
		{nil, 0},
		{[]float64{5}, 5},
		{[]float64{9, 1, 5}, 5},     // odd
		{[]float64{10, 2, 4, 8}, 6}, // even: (4+8)/2
	}
	for _, c := range cases {
		if got := Median(c.in); got != c.want {
			t.Errorf("Median(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestRepoDash_ThroughputBucketsWithGapWeek(t *testing.T) {
	w := win(3) // weeks start 09-14, 09-21, 09-28
	mk := func(id, pr string, created, merged time.Time) Run {
		return Run{ID: id, PullRequestURL: pr, CreatedAt: created, Events: []Event{ev(CategoryPRMerged, merged)}}
	}
	runs := []Run{
		mk("r1", "pr/1", time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)),
		mk("r2", "pr/2", time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)),
		// Same PR merged via a second run later: counts once, earliest week.
		mk("r3", "pr/2", time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)),
		// Merged outside the window.
		mk("r4", "pr/4", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)),
	}
	th := FoldThroughput(w, runs)
	got := []int{th.Weeks[0].MergedChanges, th.Weeks[1].MergedChanges, th.Weeks[2].MergedChanges}
	if got[0] != 1 || got[1] != 0 || got[2] != 1 {
		t.Fatalf("weekly = %v, want [1 0 1] (gap week is a zero bucket)", got)
	}
	if th.MergedChanges != 2 || th.CycleTimeSamples != 3 {
		t.Fatalf("merged=%d samples=%d, want 2 / 3", th.MergedChanges, th.CycleTimeSamples)
	}
	// cycles: 1d, 1d, 2d -> median 1d
	if th.MedianCycleTimeSeconds != 86400 {
		t.Fatalf("median = %v, want 86400", th.MedianCycleTimeSeconds)
	}
}

// TestRepoDash_CycleTimeUsesEarliestPRMerged pins approval condition 2: the
// duration ends at the EARLIEST pr_merged, never post_merge_observed or the
// newest marker.
func TestRepoDash_CycleTimeUsesEarliestPRMerged(t *testing.T) {
	w := win(2)
	created := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	t1 := created.Add(2 * time.Hour)
	t2 := created.Add(10 * time.Hour)
	r := Run{ID: "r", CreatedAt: created, Events: []Event{
		ev(CategoryPRMerged, t1),
		ev(CategoryPostMergeObserved, t2),
		ev(CategoryPRMerged, t2), // a later duplicate must not win
	}}
	th := FoldThroughput(w, []Run{r})
	if th.MedianCycleTimeSeconds != 2*3600 {
		t.Fatalf("cycle = %v, want %v (created -> earliest pr_merged)", th.MedianCycleTimeSeconds, 2*3600)
	}
}

func TestRepoDash_MergedWithoutPRMergedIsExcluded(t *testing.T) {
	w := win(2)
	created := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	runs := []Run{
		{ID: "a", CreatedAt: created, Events: []Event{ev(CategoryPostMergeObserved, created.Add(time.Hour))}},
		{ID: "b", CreatedAt: created, Events: []Event{ev(CategoryPRMerged, created.Add(3*time.Hour))}},
	}
	th := FoldThroughput(w, runs)
	if th.CycleTimeExcluded != 1 || th.CycleTimeSamples != 1 || th.MedianCycleTimeSeconds != 3*3600 {
		t.Fatalf("excluded=%d samples=%d median=%v, want 1 / 1 / 10800", th.CycleTimeExcluded, th.CycleTimeSamples, th.MedianCycleTimeSeconds)
	}
}

func TestRepoDash_WaitOnHumanPresence(t *testing.T) {
	w := win(2)
	created := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	if got := FoldThroughput(w, []Run{{ID: "x", CreatedAt: created}}).WaitOnHuman; got != nil {
		t.Fatalf("wait_on_human = %+v, want nil when no run resolved", got)
	}
	runs := []Run{
		{ID: "a", CreatedAt: created, Wait: &RunWait{TotalSeconds: 100, Gates: []GateWait{{Gate: "plan_approval", WaitSeconds: 100}}}},
		{ID: "b", CreatedAt: created, Wait: &RunWait{TotalSeconds: 300, Gates: []GateWait{{Gate: "plan_approval", WaitSeconds: 200}, {Gate: "merge", WaitSeconds: 100}}}},
	}
	got := FoldThroughput(w, runs).WaitOnHuman
	if got == nil || got.Runs != 2 || got.MedianTotalWaitSeconds != 200 || got.TotalWaitOnHumanSeconds != 400 {
		t.Fatalf("wait = %+v", got)
	}
	if len(got.Gates) != 2 || got.Gates[0].Gate != "merge" || got.Gates[1].MedianWaitSeconds != 150 {
		t.Fatalf("gates = %+v", got.Gates)
	}
}

func TestRepoDash_HealthFolds(t *testing.T) {
	w := win(2)
	c := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	runs := []Run{
		// first-shot approve; two fixups counted once; accepted.
		{ID: "1", CreatedAt: c, Events: []Event{
			approve(c, "plan"), ev(CategoryStageFixupTriggered, c), ev(CategoryStageFixupTriggered, c), verdict(VerdictPassed),
		}, FailureCategories: []string{"A"}},
		// revised before approve: not first-shot; not_validated.
		{ID: "2", CreatedAt: c, Events: []Event{
			ev(CategoryPlanRevised, c), approve(c, "plan"), verdict(VerdictNotValidated),
		}, FailureCategories: []string{"B", "C"}},
		// plan review failed before approve: not first-shot; failed then passed -> latest wins.
		{ID: "3", CreatedAt: c, Events: []Event{
			ev(CategoryPlanReviewFailed, c), approve(c, "plan"), verdict(VerdictFailed), verdict(VerdictPassed),
		}, FailureCategories: []string{"D", "Z"}},
		// rejected then approved: not first-shot; failed.
		{ID: "4", CreatedAt: c, Events: []Event{reject(c), approve(c, "plan"), verdict(VerdictFailed)}},
		// only an implement-stage approve: no plan sample.
		{ID: "5", CreatedAt: c, Events: []Event{approve(c, "implement")}},
		// created outside the window: ignored entirely.
		{ID: "6", CreatedAt: c.AddDate(0, -3, 0), Events: []Event{approve(c, "plan"), ev(CategoryStageFixupTriggered, c)}},
	}
	h := FoldHealth(w, runs)
	if h.RunsConsidered != 5 {
		t.Fatalf("runs considered = %d, want 5", h.RunsConsidered)
	}
	if h.PlanApprovalSamples != 4 || h.PlanFirstShotApprovals != 1 || h.PlanFirstShotApprovalRate != 0.25 {
		t.Fatalf("plan = %d/%d rate %v", h.PlanFirstShotApprovals, h.PlanApprovalSamples, h.PlanFirstShotApprovalRate)
	}
	if h.FixupRuns != 1 || h.FixupRate != 0.2 {
		t.Fatalf("fixup = %d rate %v, want 1 / 0.2 (two rows count once)", h.FixupRuns, h.FixupRate)
	}
	if h.AcceptanceSamples != 4 || h.AcceptancePassed != 2 || h.AcceptanceNotValidated != 1 || h.AcceptanceFailed != 1 || h.AcceptancePassRate != 0.5 {
		t.Fatalf("acceptance = %+v", h)
	}
	if h.FailureCategories != (FailureMix{A: 1, B: 1, C: 1, D: 1}) {
		t.Fatalf("failure mix = %+v", h.FailureCategories)
	}
}

func TestRepoDash_EconomicsFolds(t *testing.T) {
	w := win(2) // weeks 09-21, 09-28
	c := time.Date(2026, 9, 28, 1, 0, 0, 0, time.UTC)
	runs := []Run{
		{ID: "1", PullRequestURL: "pr/1", CreatedAt: c, Events: []Event{ev(CategoryPRMerged, c)}, Cost: []CostEntry{
			{Timestamp: c, Model: "claude-opus-5-5", USD: 3, FreshInput: 100, CacheRead: 300, CacheWrite: 100},
			{Timestamp: time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC), USD: 1, Source: "plan_review"},
			{Timestamp: c.AddDate(0, -2, 0), USD: 99}, // outside window
		}},
		{ID: "2", PullRequestURL: "pr/2", CreatedAt: c, Events: []Event{ev(CategoryPRMerged, c)}, Cost: []CostEntry{{Timestamp: c, USD: 2}}},
	}
	e := FoldEconomics(w, runs)
	if e.CostEntries != 3 || e.TotalCostUSD != 6 || e.MergedChanges != 2 || e.CostPerMergedChangeUSD != 3 {
		t.Fatalf("economics = %+v", e)
	}
	if e.Weeks[0].CostUSD != 1 || e.Weeks[1].CostUSD != 5 {
		t.Fatalf("weekly cost = %v / %v, want 1 / 5", e.Weeks[0].CostUSD, e.Weeks[1].CostUSD)
	}
	if e.Weeks[1].CacheReadRatio != 0.75 || e.Weeks[1].ReuseFactor != 3 {
		t.Fatalf("week1 cache = %+v, want ratio 0.75 reuse 3", e.Weeks[1])
	}
	// No merged change: cost per change guards to 0.
	if got := FoldEconomics(w, []Run{{ID: "x", CreatedAt: c, Cost: []CostEntry{{Timestamp: c, USD: 4}}}}); got.CostPerMergedChangeUSD != 0 || got.TotalCostUSD != 4 {
		t.Fatalf("unmerged economics = %+v", got)
	}
}
