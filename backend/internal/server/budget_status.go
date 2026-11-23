package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/budget"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// budgetStatusResponse is the GET /v0/runs/{run_id}/budget body: the
// current calendar-period status of the run's workflow periodic budget
// (#693 / ADR-030). DISPLAY-ONLY — it never gates or blocks a run; it
// surfaces the same evaluation chain checkBudgetAlerts already runs so
// the MCP loop can show spend-vs-limit every stage instead of relying
// on the easily-missed advisory issue comment.
//
// When the run's workflow declares no budget the endpoint returns 200
// with an empty object instead of this shape, so callers branch on the
// presence of `period` rather than on the status code. The budgets are read
// from the host dispatcher's source (runBudgetStatus), which for a GitHub run
// is a forge read per call, except for a run-bound caller, which is served the
// cached admission spec with no forge read.
type budgetStatusResponse struct {
	Period      string   `json:"period"`
	PeriodStart string   `json:"period_start"`
	LimitUSD    float64  `json:"limit_usd"`
	SpentUSD    float64  `json:"spent_usd"`
	Fraction    float64  `json:"fraction"`
	WarnAt      *float64 `json:"warn_at,omitempty"`
	Tier        string   `json:"tier"`
	AckRequired bool     `json:"ack_required,omitempty"`
	Enforcement string   `json:"enforcement"`
}

// runBudgetStatus evaluates the current-period status of the run's GOVERNING
// periodic budget, reusing the same chain checkBudgetAlerts runs
// (budget.PeriodRange -> SumWorkflowCostInRange -> budget.Evaluate).
//
// The budgets come from the same source as the host dispatcher's
// include=dispatch_budget verdict (ADR-030 Amendment A1, ruling C1):
// currentWorkflowBudgets, the repository's current default-branch spec for a
// GitHub run on a server with a GitHub client, else the cached admission spec.
// A failed live read WARN-logs and falls back to the cached admission spec:
// this display is never a gate, and in that state the dispatcher is refusing
// budget_check_failed, so there is no verdict to disagree with.
//
// liveRead=false skips the live read and evaluates the cached admission spec
// only. handleGetRunBudget passes false for a run-bound mcp:run: caller: the
// live read is an uncached forge call on the App installation token, and an
// in-runner agent looping this route must not be able to spend the
// installation's rate limit (that would fail every live read of the
// installation, and the dispatcher's own verdicts with them).
//
// The governing budget is the first enforcement-blocking budget that is Over
// at its effective limit (exactly the budget webhook.CheckBlockingBudget
// blocks on), else Budgets[0] as before, so budget.tier and the dispatcher's
// verdict agree.
//
// Returns (nil, nil) — "no budget to report" — when the RunRepo is
// unconfigured or doesn't implement runCostSummer, the source yields no
// budgets (no cached spec, a parse failure, an absent workflow, none
// declared), or the budget's period is unrecognized. A run-lookup failure
// (including run.ErrNotFound) and a cost-sum failure are returned as errors
// for the handler to map.
func (s *Server) runBudgetStatus(ctx context.Context, runID uuid.UUID, liveRead bool) (*budgetStatusResponse, error) {
	if s.cfg.RunRepo == nil {
		return nil, nil
	}
	summer, ok := s.cfg.RunRepo.(runCostSummer)
	if !ok {
		return nil, nil
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	var budgets []spec.PeriodicBudget
	if liveRead {
		budgets, _, err = s.currentWorkflowBudgets(ctx, runRow)
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"run budget: live spec read failed; reporting the cached admission spec's budget",
				slog.String("run_id", runRow.ID.String()),
				slog.String("error", err.Error()))
			budgets, _, _ = admissionSpecBudgets(runRow)
		}
	} else {
		budgets, _, _ = admissionSpecBudgets(runRow)
	}
	if len(budgets) == 0 {
		return nil, nil
	}

	loc := s.cfg.BudgetLocation
	if loc == nil {
		loc = time.UTC
	}
	now := time.Now()

	b, err := s.governingBudget(ctx, summer, runRow, budgets, now, loc)
	if err != nil {
		return nil, err
	}

	// Apply the operator's calibration override (#1371) on a copy of the
	// budget so spend is evaluated against — and limit_usd/fraction/tier
	// report — the effective limit, matching the alert path
	// (checkBudgetAlerts) which uses the same effectiveBudgetLimit helper.
	b.LimitUSD = s.effectiveBudgetLimit(b)

	d, ok, err := evaluateWorkflowBudget(ctx, summer, runRow.Repo, runRow.WorkflowID, b, now, loc)
	if err != nil {
		return nil, err
	}
	if !ok {
		// Unrecognized period — the schema enum makes this unreachable.
		return nil, nil
	}

	// Single-source escalating ladder (#1371): ok|warn|over|ack_required|page,
	// driven by the configured ack/page multiples (with budget.Tier's
	// defensive fallback to 2x/3x for a zero-value Config). ack_required
	// surfaces as the ack_required boolean so callers need not re-derive it.
	tier := budget.Tier(d, s.cfg.BudgetAckMultiple, s.cfg.BudgetPageMultiple)

	// An empty enforcement value defaults to advisory — the spec's
	// documented zero-value. Normalize it so the wire never carries ""
	// (concern 1): a default-advisory budget surfaces enforcement:"advisory".
	enforcement := string(b.Enforcement)
	if enforcement == "" {
		enforcement = string(spec.EnforcementAdvisory)
	}

	return &budgetStatusResponse{
		Period:      b.Period,
		PeriodStart: d.PeriodStart.Format(time.RFC3339),
		LimitUSD:    b.LimitUSD,
		SpentUSD:    d.Spent,
		Fraction:    d.Fraction,
		WarnAt:      b.WarnAt,
		Tier:        tier,
		AckRequired: budget.AckRequired(tier),
		Enforcement: enforcement,
	}, nil
}

// governingBudget selects the budget the display reports: the first
// enforcement-blocking budget that is Over at its effective limit (the one
// webhook.CheckBlockingBudget blocks on, so the tier agrees with the host
// dispatcher's verdict), else budgets[0]. budgets is non-empty. A cost-sum
// failure is returned.
func (s *Server) governingBudget(ctx context.Context, summer runCostSummer, runRow *run.Run, budgets []spec.PeriodicBudget, now time.Time, loc *time.Location) (spec.PeriodicBudget, error) {
	for _, b := range budgets {
		if b.Enforcement != spec.EnforcementBlocking {
			continue
		}
		eb := b
		eb.LimitUSD = s.effectiveBudgetLimit(b)
		d, ok, err := evaluateWorkflowBudget(ctx, summer, runRow.Repo, runRow.WorkflowID, eb, now, loc)
		if err != nil {
			return spec.PeriodicBudget{}, err
		}
		if ok && d.Over {
			return b, nil
		}
	}
	return budgets[0], nil
}

// handleGetRunBudget implements GET /v0/runs/{run_id}/budget. Returns
// the current-period periodic-budget status for the run's workflow, or
// 200 with an empty object when no budget is configured. 400 on a bad
// UUID, 404 when the run doesn't resolve, 500 on a cost-sum failure. A
// run-bound mcp:run: caller is served the cached admission spec's budget and
// never triggers a forge read (runBudgetStatus liveRead=false).
func (s *Server) handleGetRunBudget(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "run_repo_unconfigured",
			"runs endpoint requires a configured run repository", nil)
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	liveRead := !isRunBoundSubject(IdentityFrom(r.Context()).Subject)
	status, err := s.runBudgetStatus(r.Context(), runID, liveRead)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run budget failed", map[string]any{"error": err.Error()})
		return
	}

	if status == nil {
		// No budget configured — 200 with an empty object so the MCP
		// client treats "no budget" uniformly without status-code branching.
		s.writeJSON(w, r, http.StatusOK, struct{}{})
		return
	}

	s.writeJSON(w, r, http.StatusOK, status)
}
