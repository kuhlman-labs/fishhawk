package precedent

import (
	"sort"

	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
)

// Tuning replay (E75.5 / #3733 slice 3). DefaultDivergenceConfig's numbers are
// placeholders; Replay is how a maintainer replaces them from EVIDENCE: it
// re-runs the divergence rule over a repository's recorded decision history
// for a grid of candidate (N, X%, window) values and reports, per candidate,
// how often the rule WOULD have fired and why it stayed quiet the rest of the
// time.
//
// It is PURE and CLOCK-FREE like the rest of the package: each historical
// decision's own DecidedAt is the `now` the rule runs under, so a replay is
// byte-reproducible from its input rows. Long-form contract:
// backend/internal/precedent/README.md and the `fishhawkd precedent-tuning`
// section of backend/cmd/fishhawkd/README.md.

// TuningRankLimit and TuningCandidateWindow mirror the live hook's query
// (backend/internal/server: precedentDefaultLimit and precedentCandidateWindow,
// which this pure package cannot import). Per comparison class, the live path
// reads the NEWEST TuningCandidateWindow index rows matching the hard filter
// and ranks the top TuningRankLimit of them; the replay reconstructs exactly
// that set so its fire counts describe what production would have done rather
// than an idealised unbounded precedent set.
const (
	TuningRankLimit       = 20
	TuningCandidateWindow = 500
)

// TuningBreakdown counts, per reason, the examined decisions on which a
// candidate did NOT fire — the "why is it quiet" half of the report.
type TuningBreakdown struct {
	BelowMinDecisions       int `json:"below_min_decisions"`
	OutsideWindow           int `json:"outside_window"`
	DoctrineVersionMismatch int `json:"doctrine_version_mismatch"`
	BelowMinAgreement       int `json:"below_min_agreement"`
	AgreedWithPrecedent     int `json:"agreed_with_precedent"`
}

// TuningResult is one candidate configuration's replay outcome. Config is the
// candidate AS EVALUATED (Enabled forced true — a candidate is judged as though
// it were switched on). FireRate is WouldHaveFired / DecisionsExamined, and 0
// when nothing was examined.
type TuningResult struct {
	Config            DivergenceConfig `json:"config"`
	DecisionsExamined int              `json:"decisions_examined"`
	WouldHaveFired    int              `json:"would_have_fired"`
	FireRate          float64          `json:"fire_rate"`
	NotFired          TuningBreakdown  `json:"not_fired"`
}

// Replay evaluates every candidate in grid against every historical decision in
// rows, returning one TuningResult per candidate in grid order.
//
// For each decision the precedent set is reconstructed AS OF that decision:
// the rows STRICTLY EARLIER in (DecidedAt, SourceSequence) order, of the same
// repository and of each of the decision's ComparisonClasses, narrowed to the
// newest TuningCandidateWindow by source sequence and ranked through the SAME
// Rank the live path uses (limit TuningRankLimit) — so the replay cannot
// diverge from production scoring. The decision's own doctrine version and
// DecidedAt are the version and `now` Decide runs under.
//
// A decision is EXAMINED by a candidate when the candidate's allow-list admits
// it (ClassAllowed: the closed set, the candidate's narrowing, and a reject for
// plan_approval); anything else is not counted at all, so a candidate's fire
// rate is over the decisions it could have fired on. The ranked set does not
// depend on the candidate, so it is built once per decision and shared.
func Replay(rows []decisionindex.Row, grid []DivergenceConfig) []TuningResult {
	results := make([]TuningResult, len(grid))
	for i, cfg := range grid {
		cfg.Enabled = true
		results[i].Config = cfg
	}
	if len(grid) == 0 {
		return results
	}

	sorted := append([]decisionindex.Row(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].DecidedAt.Equal(sorted[j].DecidedAt) {
			return sorted[i].DecidedAt.Before(sorted[j].DecidedAt)
		}
		return sorted[i].SourceSequence < sorted[j].SourceSequence
	})

	for i, d := range sorted {
		examining := false
		for _, r := range results {
			if r.Config.ClassAllowed(d.DecisionClass, d.Outcome) {
				examining = true
				break
			}
		}
		if !examining {
			continue
		}
		items := replayPrecedentSet(d, sorted[:i])
		now := d.DecidedAt.UTC()
		for k := range results {
			cfg := results[k].Config
			if !cfg.ClassAllowed(d.DecisionClass, d.Outcome) {
				continue
			}
			results[k].DecisionsExamined++
			v := Decide(cfg, d.DecisionClass, d.Outcome, d.DoctrineVersion, now, items)
			tally(&results[k], v)
		}
	}

	for k := range results {
		if results[k].DecisionsExamined > 0 {
			results[k].FireRate = float64(results[k].WouldHaveFired) / float64(results[k].DecisionsExamined)
		}
	}
	return results
}

// replayPrecedentSet is the live hook's per-comparison-class query, replayed
// over the rows that precede d.
func replayPrecedentSet(d decisionindex.Row, earlier []decisionindex.Row) []Item {
	var items []Item
	for _, class := range ComparisonClasses(d.DecisionClass) {
		c := Context{
			Repo:            d.Repo,
			DecisionClass:   class,
			StageKind:       d.StageKind,
			TouchedPaths:    d.TouchedPaths,
			ConcernCategory: d.ConcernCategory,
			Severity:        d.Severity,
			EscalationKeys:  d.EscalationKeys,
		}
		// Newest-first by source sequence, capped at the candidate window —
		// the store's `ORDER BY source_sequence DESC LIMIT` under the hard
		// filter. Rank re-asserts the hard filter, but applying it here keeps
		// the window cap counting only rows the store would have returned.
		var cand []decisionindex.Row
		for j := len(earlier) - 1; j >= 0; j-- {
			if hardFilterAdmits(c, earlier[j]) {
				cand = append(cand, earlier[j])
			}
		}
		sort.SliceStable(cand, func(a, b int) bool { return cand[a].SourceSequence > cand[b].SourceSequence })
		if len(cand) > TuningCandidateWindow {
			cand = cand[:TuningCandidateWindow]
		}
		ranked, _ := Rank(c, cand, TuningRankLimit)
		items = append(items, ranked...)
	}
	return items
}

func tally(r *TuningResult, v Verdict) {
	switch v.Kind {
	case VerdictDiverged:
		r.WouldHaveFired++
	case VerdictAgreed:
		r.NotFired.AgreedWithPrecedent++
	case VerdictNoClearPrecedent:
		switch v.Reason {
		case ReasonBelowMinDecisions:
			r.NotFired.BelowMinDecisions++
		case ReasonOutsideWindow:
			r.NotFired.OutsideWindow++
		case ReasonDoctrineVersionMismatch:
			r.NotFired.DoctrineVersionMismatch++
		case ReasonBelowMinAgreement:
			r.NotFired.BelowMinAgreement++
		}
	}
}
