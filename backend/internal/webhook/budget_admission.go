package webhook

import (
	"context"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/budget"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// CostSummer is the optional capability the blocking-budget admission
// gate uses to total a workflow's spend over a calendar period (#688 /
// ADR-030). The Postgres run repository implements it; like the trace
// handler's runCostSummer it is deliberately NOT part of run.Repository
// so the many test fakes that don't sum cost need no stub. Both
// admission seams (the HTTP handler and the webhook dispatcher) assert
// for it at runtime and admit the run when the wired repository doesn't
// satisfy it — capability-absent is fail-open, consistent with the rest
// of the budget wiring's best-effort posture.
type CostSummer interface {
	SumWorkflowCostInRange(ctx context.Context, repo, workflowID string, from, to time.Time) (float64, error)
}

// CheckBlockingBudget evaluates a workflow's blocking periodic budgets
// against the current calendar period's spend and reports whether a NEW
// run must be refused at admission (ADR-030). It is the shared decision
// core behind both admission seams — the HTTP handler (handleCreateRun)
// and the webhook dispatcher, which creates runs directly and bypasses
// the handler. It lives in the webhook package (the lower of the two:
// server already imports webhook) so both seams share one implementation
// with no import cycle.
//
// For each budget whose Enforcement is blocking it sums the workflow's
// spend over the budget's current calendar period (timezone-aware in
// loc) and evaluates it via budget.Evaluate; the FIRST blocking budget
// that is Over wins and is returned with blocked=true. Advisory budgets
// (and the empty-enforcement default, which is advisory) are skipped —
// they surface via the warn path, never gate. An unrecognized period is
// skipped (the schema enum makes it unreachable, but don't bucket into
// the wrong window).
//
// On a sum error the gate is fail-open: blocked=false with err set, so
// the caller can log and proceed rather than refuse a run on a query
// flap. A nil loc is treated as UTC.
func CheckBlockingBudget(ctx context.Context, summer CostSummer, repo, workflowID string, budgets []spec.PeriodicBudget, now time.Time, loc *time.Location) (blocked bool, offending spec.PeriodicBudget, d budget.Decision, err error) {
	if loc == nil {
		loc = time.UTC
	}
	for _, b := range budgets {
		// Only blocking budgets gate admission. An empty enforcement
		// value defaults to advisory (the spec's documented zero
		// value), so the single included case is blocking.
		if b.Enforcement != spec.EnforcementBlocking {
			continue
		}
		start, end := budget.PeriodRange(b.Period, now, loc)
		if start.IsZero() {
			// Unrecognized period — the schema enum makes this
			// unreachable, but don't bucket into the wrong window.
			continue
		}
		spent, serr := summer.SumWorkflowCostInRange(ctx, repo, workflowID, start, end)
		if serr != nil {
			// Fail-open: never refuse a run on a sum-query flap. The
			// caller logs and admits.
			return false, spec.PeriodicBudget{}, budget.Decision{}, serr
		}
		dec := budget.Evaluate(spent, b.LimitUSD, b.WarnAt, b.Period, now, loc)
		if dec.Over {
			return true, b, dec, nil
		}
	}
	return false, spec.PeriodicBudget{}, budget.Decision{}, nil
}
