// Package repodash folds one repository's runs and their decoded audit facts
// into the repo-dashboard rollups served by GET
// /v0/repos/{owner}/{name}/{throughput,health,economics} (E40.3 / #1714).
//
// It is PURE: no database, no HTTP, no clock. The server gathers the runs in
// the window (see the paging contract in README.md), decodes each run's audit
// rows into Event / CostEntry values, and hands them here. Every ratio is
// guarded on a zero denominator and returns 0 — encoding/json refuses to
// marshal NaN or Inf, so an unguarded division would turn an empty window into
// a 500 rather than a visibly-zero number.
package repodash

import (
	"sort"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/cost"
)

// Window bounds for the `weeks` query parameter.
const (
	DefaultWeeks = 12
	MinWeeks     = 1
	MaxWeeks     = 52
)

// LookBack is how far before the window start the server keeps scanning runs,
// so a run CREATED before the window but MERGED inside it is still counted
// (approval condition 1). A run created earlier than windowStart-LookBack and
// merged inside the window is a documented residual.
const LookBack = 14 * 24 * time.Hour

// MaxRunsScanned is the hard ceiling on runs the server scans for one rollup.
// When the ceiling stops the scan before the look-back boundary is reached,
// every rollup response carries truncated:true.
const MaxRunsScanned = 1000

// Audit categories the folds key on.
const (
	CategoryPRMerged            = "pr_merged"
	CategoryPostMergeObserved   = "post_merge_observed"
	CategoryApprovalSubmitted   = "approval_submitted"
	CategoryPlanRevised         = "plan_revised"
	CategoryPlanReviewFailed    = "plan_review_failed"
	CategoryStageFixupTriggered = "stage_fixup_triggered"
	CategoryAcceptanceOutcome   = "acceptance_outcome_recorded"
)

// Acceptance verdicts as recorded on acceptance_outcome_recorded.
const (
	VerdictPassed       = "passed"
	VerdictFailed       = "failed"
	VerdictNotValidated = "not_validated"
	VerdictUndecidable  = "undecidable"
)

// Window is the half-open reporting interval [Start, End) split into Weeks
// ISO-week buckets (Monday 00:00 UTC starts).
type Window struct {
	Start time.Time
	End   time.Time
	Weeks int
}

// NewWindow returns the window of `weeks` ISO weeks ending with (and
// including) the week containing now.
func NewWindow(now time.Time, weeks int) Window {
	cur := WeekStart(now)
	start := cur.AddDate(0, 0, -7*(weeks-1))
	return Window{Start: start, End: cur.AddDate(0, 0, 7), Weeks: weeks}
}

// ScanBoundary is the created_at below which the server stops scanning.
func (w Window) ScanBoundary() time.Time { return w.Start.Add(-LookBack) }

// Contains reports whether t falls in [Start, End).
func (w Window) Contains(t time.Time) bool {
	return !t.Before(w.Start) && t.Before(w.End)
}

// bucket returns the week index of t, or -1 outside the window.
func (w Window) bucket(t time.Time) int {
	if !w.Contains(t) {
		return -1
	}
	return int(t.Sub(w.Start) / (7 * 24 * time.Hour))
}

// weekStartAt returns the start of bucket i.
func (w Window) weekStartAt(i int) time.Time { return w.Start.AddDate(0, 0, 7*i) }

// WeekStart returns the Monday 00:00 UTC that starts t's ISO week.
func WeekStart(t time.Time) time.Time {
	t = t.UTC()
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	offset := (int(day.Weekday()) + 6) % 7 // Monday = 0
	return day.AddDate(0, 0, -offset)
}

// Event is one decoded audit fact. Only the fields a fold reads are set.
type Event struct {
	Category  string
	Timestamp time.Time
	// StageType is the type of the stage the row names ("plan", ...), when
	// the server could resolve it; approval_submitted keys on it.
	StageType string
	// Decision is approval_submitted's decision ("approve" / "reject").
	Decision string
	// Verdict is acceptance_outcome_recorded's verdict.
	Verdict string
}

// CostEntry is one decoded cost_recorded row.
type CostEntry struct {
	Timestamp  time.Time
	Model      string
	Source     string
	USD        float64
	FreshInput int
	CacheRead  int
	CacheWrite int
	Output     int
}

// GateWait is one resolved human-gate interval of a run (#1702).
type GateWait struct {
	Gate        string
	WaitSeconds float64
}

// RunWait is a run's resolved gate-latency rollup.
type RunWait struct {
	TotalSeconds float64
	Gates        []GateWait
}

// Run is one run the server gathered, with its decoded facts.
type Run struct {
	ID                string
	CreatedAt         time.Time
	PullRequestURL    string
	Events            []Event
	FailureCategories []string
	Cost              []CostEntry
	// Wait is nil when no gate interval resolved for the run.
	Wait *RunWait
}

// MergedAt is the run's merge time: the EARLIEST pr_merged timestamp
// (approval condition 2). Never post_merge_observed, never the newest.
func (r Run) MergedAt() (time.Time, bool) {
	var at time.Time
	found := false
	for _, e := range r.Events {
		if e.Category != CategoryPRMerged {
			continue
		}
		if !found || e.Timestamp.Before(at) {
			at, found = e.Timestamp, true
		}
	}
	return at, found
}

// observedMergeAt is the earliest post_merge_observed timestamp — used ONLY to
// place a merged run that lacks a pr_merged row into the window so it can be
// counted as excluded.
func (r Run) observedMergeAt() (time.Time, bool) {
	var at time.Time
	found := false
	for _, e := range r.Events {
		if e.Category != CategoryPostMergeObserved {
			continue
		}
		if !found || e.Timestamp.Before(at) {
			at, found = e.Timestamp, true
		}
	}
	return at, found
}

// changeKey identifies a merged change: the PR URL when known, else the run.
func (r Run) changeKey() string {
	if r.PullRequestURL != "" {
		return r.PullRequestURL
	}
	return "run:" + r.ID
}

// WeekCount is one throughput bucket.
type WeekCount struct {
	WeekStart     time.Time `json:"week_start"`
	MergedChanges int       `json:"merged_changes"`
}

// GateWaitRollup is the per-gate wait fold across the window's runs.
type GateWaitRollup struct {
	Gate              string  `json:"gate"`
	Samples           int     `json:"samples"`
	MedianWaitSeconds float64 `json:"median_wait_seconds"`
}

// WaitOnHuman is the optional throughput sub-block (#1702).
type WaitOnHuman struct {
	Runs                    int              `json:"runs"`
	MedianTotalWaitSeconds  float64          `json:"median_total_wait_seconds"`
	TotalWaitOnHumanSeconds float64          `json:"total_wait_on_human_seconds"`
	Gates                   []GateWaitRollup `json:"gates"`
}

// Throughput is the throughput rollup.
type Throughput struct {
	Weeks                  []WeekCount
	MergedChanges          int
	MedianCycleTimeSeconds float64
	CycleTimeSamples       int
	CycleTimeExcluded      int
	// WaitOnHuman is nil when no run created in the window resolved a
	// gate-latency rollup — the response then omits the key entirely.
	WaitOnHuman *WaitOnHuman
}

// FoldThroughput buckets merged changes by MERGE time and computes the median
// cycle time (created_at -> earliest pr_merged) over runs merged in the
// window. A run merged in the window per post_merge_observed but carrying no
// pr_merged row is excluded from the median and counted in CycleTimeExcluded.
// A change (PR URL) merged by several runs counts once, in the week of its
// earliest merge.
func FoldThroughput(w Window, runs []Run) Throughput {
	out := Throughput{Weeks: make([]WeekCount, w.Weeks)}
	for i := range out.Weeks {
		out.Weeks[i].WeekStart = w.weekStartAt(i)
	}
	firstMerge := map[string]time.Time{}
	var cycles []float64
	for _, r := range runs {
		at, ok := r.MergedAt()
		if !ok {
			if obs, seen := r.observedMergeAt(); seen && w.Contains(obs) {
				out.CycleTimeExcluded++
			}
			continue
		}
		if !w.Contains(at) {
			continue
		}
		cycles = append(cycles, at.Sub(r.CreatedAt).Seconds())
		key := r.changeKey()
		if prev, seen := firstMerge[key]; !seen || at.Before(prev) {
			firstMerge[key] = at
		}
	}
	for _, at := range firstMerge {
		out.Weeks[w.bucket(at)].MergedChanges++
		out.MergedChanges++
	}
	out.CycleTimeSamples = len(cycles)
	out.MedianCycleTimeSeconds = Median(cycles)
	out.WaitOnHuman = foldWait(w, runs)
	return out
}

// foldWait folds the per-run gate-latency rollups of runs CREATED in the
// window. Returns nil when none resolved — the presence check the SPA's
// sub-panel keys on.
func foldWait(w Window, runs []Run) *WaitOnHuman {
	var totals []float64
	byGate := map[string][]float64{}
	var grand float64
	for _, r := range runs {
		if r.Wait == nil || !w.Contains(r.CreatedAt) {
			continue
		}
		totals = append(totals, r.Wait.TotalSeconds)
		grand += r.Wait.TotalSeconds
		for _, g := range r.Wait.Gates {
			byGate[g.Gate] = append(byGate[g.Gate], g.WaitSeconds)
		}
	}
	if len(totals) == 0 {
		return nil
	}
	out := &WaitOnHuman{
		Runs:                    len(totals),
		MedianTotalWaitSeconds:  Median(totals),
		TotalWaitOnHumanSeconds: grand,
		Gates:                   []GateWaitRollup{},
	}
	for gate, waits := range byGate {
		out.Gates = append(out.Gates, GateWaitRollup{Gate: gate, Samples: len(waits), MedianWaitSeconds: Median(waits)})
	}
	sort.Slice(out.Gates, func(i, j int) bool { return out.Gates[i].Gate < out.Gates[j].Gate })
	return out
}

// FailureMix counts failed stages per MVP_SPEC §6 category.
type FailureMix struct {
	A int `json:"A"`
	B int `json:"B"`
	C int `json:"C"`
	D int `json:"D"`
}

// Health is the health rollup.
type Health struct {
	RunsConsidered            int
	PlanApprovalSamples       int
	PlanFirstShotApprovals    int
	PlanFirstShotApprovalRate float64
	FixupRuns                 int
	FixupRate                 float64
	AcceptanceSamples         int
	AcceptancePassed          int
	AcceptanceNotValidated    int
	AcceptanceFailed          int
	AcceptanceUndecidable     int
	AcceptancePassRate        float64
	FailureCategories         FailureMix
}

// FoldHealth folds runs CREATED in the window:
//
//   - plan first-shot approval: of runs with a plan-stage approve, those whose
//     first plan approve was preceded by no plan-stage reject, plan_revised or
//     plan_review_failed row;
//   - fixup rate: runs with >=1 stage_fixup_triggered row (once per run) over
//     runs considered;
//   - acceptance pass rate: runs whose LATEST acceptance verdict is passed over
//     runs with any acceptance verdict;
//   - failure mix: failed stages per category A/B/C/D (unknown values skipped).
func FoldHealth(w Window, runs []Run) Health {
	var h Health
	for _, r := range runs {
		if !w.Contains(r.CreatedAt) {
			continue
		}
		h.RunsConsidered++
		if approved, firstShot := planApproval(r.Events); approved {
			h.PlanApprovalSamples++
			if firstShot {
				h.PlanFirstShotApprovals++
			}
		}
		for _, e := range r.Events {
			if e.Category == CategoryStageFixupTriggered {
				h.FixupRuns++
				break
			}
		}
		if v, ok := latestVerdict(r.Events); ok {
			h.AcceptanceSamples++
			switch v {
			case VerdictPassed:
				h.AcceptancePassed++
			case VerdictNotValidated:
				h.AcceptanceNotValidated++
			case VerdictUndecidable:
				h.AcceptanceUndecidable++
			default:
				h.AcceptanceFailed++
			}
		}
		for _, c := range r.FailureCategories {
			switch c {
			case "A":
				h.FailureCategories.A++
			case "B":
				h.FailureCategories.B++
			case "C":
				h.FailureCategories.C++
			case "D":
				h.FailureCategories.D++
			}
		}
	}
	h.PlanFirstShotApprovalRate = Ratio(h.PlanFirstShotApprovals, h.PlanApprovalSamples)
	h.FixupRate = Ratio(h.FixupRuns, h.RunsConsidered)
	h.AcceptancePassRate = Ratio(h.AcceptancePassed, h.AcceptanceSamples)
	return h
}

// planApproval walks events in chain order: approved reports a plan-stage
// approve exists; firstShot reports nothing disqualifying preceded the first.
func planApproval(events []Event) (approved, firstShot bool) {
	disqualified := false
	for _, e := range events {
		switch e.Category {
		case CategoryPlanRevised, CategoryPlanReviewFailed:
			disqualified = true
		case CategoryApprovalSubmitted:
			if e.StageType != "plan" {
				continue
			}
			if e.Decision == "approve" {
				return true, !disqualified
			}
			disqualified = true
		}
	}
	return false, false
}

// latestVerdict returns the newest acceptance verdict (chain order).
func latestVerdict(events []Event) (string, bool) {
	v, ok := "", false
	for _, e := range events {
		if e.Category == CategoryAcceptanceOutcome && e.Verdict != "" {
			v, ok = e.Verdict, true
		}
	}
	return v, ok
}

// WeekEconomics is one economics bucket.
type WeekEconomics struct {
	WeekStart      time.Time `json:"week_start"`
	CostUSD        float64   `json:"cost_usd"`
	CacheReadRatio float64   `json:"cache_read_ratio"`
	ReuseFactor    float64   `json:"reuse_factor"`
	NetSavingsUSD  float64   `json:"net_savings_usd"`
}

// Economics is the economics rollup.
type Economics struct {
	CostEntries            int
	TotalCostUSD           float64
	MergedChanges          int
	CostPerMergedChangeUSD float64
	Weeks                  []WeekEconomics
}

// FoldEconomics folds every cost_recorded entry timestamped in the window:
// total cost via cost.AggregateRunCost, cost per merged change as that total
// over the DISTINCT changes merged in the window (the throughput definition),
// and per-week cache efficiency via cost.AggregateCacheEfficiency.
func FoldEconomics(w Window, runs []Run) Economics {
	out := Economics{Weeks: make([]WeekEconomics, w.Weeks)}
	perWeekCost := make([][]cost.RunCostEntry, w.Weeks)
	perWeekCache := make([][]cost.CacheEfficiencyEntry, w.Weeks)
	var all []cost.RunCostEntry
	for _, r := range runs {
		for _, c := range r.Cost {
			i := w.bucket(c.Timestamp)
			if i < 0 {
				continue
			}
			out.CostEntries++
			rc := cost.RunCostEntry{Source: c.Source, USD: c.USD}
			all = append(all, rc)
			perWeekCost[i] = append(perWeekCost[i], rc)
			perWeekCache[i] = append(perWeekCache[i], cost.CacheEfficiencyEntry{
				Model: c.Model, Source: c.Source, FreshInput: c.FreshInput,
				CacheRead: c.CacheRead, CacheWrite: c.CacheWrite, Output: c.Output,
			})
		}
	}
	out.TotalCostUSD = cost.AggregateRunCost(all).TotalUSD
	out.MergedChanges = FoldThroughput(w, runs).MergedChanges
	if out.MergedChanges > 0 {
		out.CostPerMergedChangeUSD = out.TotalCostUSD / float64(out.MergedChanges)
	}
	for i := range out.Weeks {
		eff := cost.AggregateCacheEfficiency(perWeekCache[i])
		out.Weeks[i] = WeekEconomics{
			WeekStart:      w.weekStartAt(i),
			CostUSD:        cost.AggregateRunCost(perWeekCost[i]).TotalUSD,
			CacheReadRatio: eff.CacheReadRatio,
			ReuseFactor:    eff.ReuseFactor,
			NetSavingsUSD:  eff.NetSavingsUSD,
		}
	}
	return out
}

// Ratio returns num/den, or 0 when den is 0 (never NaN or Inf).
func Ratio(num, den int) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}

// Median returns the median of xs (mean of the middle two for an even
// count), or 0 for an empty slice. xs is not modified.
func Median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}
