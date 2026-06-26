// Package budget evaluates a workflow's spend against a periodic
// (calendar weekly or monthly) cost ceiling. It is the decision core
// behind ADR-030's periodic per-workflow budgets: advisory budgets warn
// when period spend crosses a configured fraction and 100%; blocking
// budgets refuse a NEW run at admission once the period is exhausted.
//
// Like the spendalert package, the detector is deliberately a pure
// function with NO repository dependency. The caller supplies the
// already-summed period spend (the backend sums runs.cost_usd_total over
// the period's date range) and Evaluate returns a fully-populated
// Decision; the package never queries. Keeping it free of storage makes
// the calendar/timezone arithmetic and the threshold logic trivially
// testable, and means the trace/admission wiring only has to shuttle a
// dollar figure in and a Decision out.
//
// Period boundaries are computed timezone-aware in a supplied
// *time.Location via time.Date(...): weekly resets at the start of the
// ISO week (Monday 00:00 local), monthly at the first of the month
// (00:00 local). The standard library handles DST transitions, so a
// boundary that straddles a clock change still lands on local midnight.
package budget

import "time"

// Decision is the outcome of an Evaluate call. It is fully populated
// regardless of whether any threshold was crossed so the caller can log
// or surface the figures either way; Over and WarnCrossed are the gates
// the caller keys emission and admission off.
type Decision struct {
	// PeriodStart is the inclusive start of the current calendar period
	// in the supplied location. The caller uses it (with PeriodRange) to
	// bound the [from, to) cost sum that produced Spent.
	PeriodStart time.Time
	// Spent is the period spend the caller passed in (echoed for logging).
	Spent float64
	// Limit is the configured ceiling in US dollars (echoed for logging).
	Limit float64
	// Fraction is Spent/Limit, or 0 when Limit is non-positive. Surfaced so
	// an alert payload can report how close to the ceiling the workflow is.
	Fraction float64
	// Over is true when Spent has reached or exceeded Limit (exact 100%
	// counts as over). It is the admission gate for a blocking budget and
	// the 100% emit gate for an advisory one.
	Over bool
	// WarnCrossed is true when a warn_at fraction was configured and
	// Fraction has reached or exceeded it. It is the warn-threshold emit
	// gate for an advisory budget. Note that once Over is true WarnCrossed
	// is also true (warn_at <= 1), so the caller de-dups the two thresholds
	// separately rather than treating them as mutually exclusive.
	WarnCrossed bool
}

// Period values, mirroring the workflow-v0 schema's periodic_budget enum.
const (
	PeriodWeekly  = "weekly"
	PeriodMonthly = "monthly"
)

// Escalating tier strings for a periodic-budget status (#1371). They form
// a single, ordered ladder — ok < warn < over < ack_required < page —
// used as the one source of truth by the display path
// (server.runBudgetStatus / GET /v0/runs/{id}/budget), the alert path
// (server.checkBudgetAlerts / budget_alert), the MCP BudgetStatus block,
// and the plan-approval gate (server.checkPeriodicBudgetTier). The first
// three are byte-identical supersets of the prior {ok, warn, over} set;
// ack_required and page are the new escalation rungs past the limit,
// driven by configured multiples of it.
const (
	TierOK          = "ok"
	TierWarn        = "warn"
	TierOver        = "over"
	TierAckRequired = "ack_required"
	TierPage        = "page"
)

// Default ack/page multiples of the limit (#1371). The ack rung trips at
// 2x the limit and the page rung at 3x. They are the fallback whenever a
// supplied multiple is unusable (non-positive or inverted), so a
// zero-value Config — one built without serve.go's flag wiring, e.g. in
// tests or by an embedder — never collapses every fraction into 'page'.
const (
	DefaultAckMultiple  = 2.0
	DefaultPageMultiple = 3.0
)

// Tier maps a Decision to its escalating ladder rung (#1371), the single
// source of truth shared by the display, alert, MCP, and plan-gate paths.
// The ladder is, from a Decision d and the configured multiples:
//
//	d.Fraction >= pageMultiple  -> page
//	d.Fraction >= ackMultiple   -> ack_required
//	d.Over                      -> over
//	d.WarnCrossed               -> warn
//	otherwise                   -> ok
//
// The page and ack bands are checked first and highest-first so a single
// fraction maps to exactly the top rung it has reached. The warn band needs
// no separate input — d.WarnCrossed already encodes the warn_at threshold
// Evaluate applied.
//
// Defensive fallback: a non-positive ackMultiple or pageMultiple, or an
// inverted pair (pageMultiple <= ackMultiple), is replaced wholesale by
// DefaultAckMultiple / DefaultPageMultiple. This guarantees a zero-value
// Config (ackMultiple == pageMultiple == 0) does not classify every
// positive fraction as 'page' (0 >= 0), which would saturate the gate.
func Tier(d Decision, ackMultiple, pageMultiple float64) string {
	if ackMultiple <= 0 || pageMultiple <= 0 || pageMultiple <= ackMultiple {
		ackMultiple = DefaultAckMultiple
		pageMultiple = DefaultPageMultiple
	}
	switch {
	case d.Fraction >= pageMultiple:
		return TierPage
	case d.Fraction >= ackMultiple:
		return TierAckRequired
	case d.Over:
		return TierOver
	case d.WarnCrossed:
		return TierWarn
	default:
		return TierOK
	}
}

// AckRequired reports whether a tier has reached the acknowledgment
// escalation rung — ack_required or page (#1371). The plan-approval gate
// keys its --ack-budget requirement off this, and the budget-status
// response surfaces it as a boolean so a caller need not re-derive the
// ladder ordering.
func AckRequired(tier string) bool {
	return tier == TierAckRequired || tier == TierPage
}

// PeriodRange returns the half-open [start, end) calendar period that
// contains now, evaluated in loc. Weekly periods run Monday 00:00 local
// to the following Monday; monthly periods run the first of the month
// 00:00 local to the first of the next month. The caller uses this to
// bound the cost sum (created_at in [start, end)).
//
// A nil loc is treated as UTC. An unrecognized period yields a zero
// range — the schema enum makes that unreachable in practice, but the
// caller can detect it via start.IsZero() rather than silently bucketing
// into the wrong period.
func PeriodRange(period string, now time.Time, loc *time.Location) (start, end time.Time) {
	if loc == nil {
		loc = time.UTC
	}
	n := now.In(loc)
	switch period {
	case PeriodWeekly:
		// Weekday() is Sunday=0..Saturday=6; shift so Monday=0..Sunday=6
		// to find how many days back the current ISO week began.
		offset := (int(n.Weekday()) + 6) % 7
		start = time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -offset)
		end = start.AddDate(0, 0, 7)
		return start, end
	case PeriodMonthly:
		start = time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, loc)
		end = start.AddDate(0, 1, 0)
		return start, end
	default:
		return time.Time{}, time.Time{}
	}
}

// Evaluate compares already-summed period spend against the limit and
// reports whether the warn_at fraction and the 100% ceiling have been
// crossed. The caller is responsible for summing spent over the period
// returned by PeriodRange before calling; Evaluate never queries.
//
// warnAt is an optional fraction in (0, 1]; a nil warnAt disables the
// warn threshold (WarnCrossed stays false). A non-positive limit is
// treated as no usable ceiling — Fraction stays 0 and nothing is crossed
// — so a misconfigured budget never blocks every run. A nil loc is UTC.
func Evaluate(spent, limit float64, warnAt *float64, period string, now time.Time, loc *time.Location) Decision {
	start, _ := PeriodRange(period, now, loc)

	d := Decision{
		PeriodStart: start,
		Spent:       spent,
		Limit:       limit,
	}

	if limit <= 0 {
		return d
	}

	d.Fraction = spent / limit
	d.Over = spent >= limit
	if warnAt != nil && *warnAt > 0 {
		d.WarnCrossed = d.Fraction >= *warnAt
	}
	return d
}
