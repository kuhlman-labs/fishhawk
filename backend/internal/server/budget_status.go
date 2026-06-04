package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// budgetTierOK is the no-crossing tier for a periodic-budget status. It
// joins budgetTierWarn / budgetTierOver (declared in trace.go for the
// alert path) to form the {ok, warn, over} display tier set surfaced by
// GET /v0/runs/{run_id}/budget.
const budgetTierOK = "ok"

// budgetStatusResponse is the GET /v0/runs/{run_id}/budget body: the
// current calendar-period status of the run's workflow periodic budget
// (#693 / ADR-030). DISPLAY-ONLY — it never gates or blocks a run; it
// surfaces the same evaluation chain checkBudgetAlerts already runs so
// the MCP loop can show spend-vs-limit every stage instead of relying
// on the easily-missed advisory issue comment.
//
// When the run's workflow declares no budget the endpoint returns 200
// with an empty object instead of this shape, so callers branch on the
// presence of `period` rather than on the status code.
type budgetStatusResponse struct {
	Period      string   `json:"period"`
	PeriodStart string   `json:"period_start"`
	LimitUSD    float64  `json:"limit_usd"`
	SpentUSD    float64  `json:"spent_usd"`
	Fraction    float64  `json:"fraction"`
	WarnAt      *float64 `json:"warn_at,omitempty"`
	Tier        string   `json:"tier"`
	Enforcement string   `json:"enforcement"`
}

// runBudgetStatus evaluates the current-period status of the FIRST
// periodic budget on the run's cached workflow spec, reusing the same
// chain checkBudgetAlerts runs (spec parse -> budget.PeriodRange ->
// SumWorkflowCostInRange -> budget.Evaluate).
//
// Returns (nil, nil) — "no budget to report" — when the RunRepo is
// unconfigured or doesn't implement runCostSummer, the run has no cached
// WorkflowSpec, the spec doesn't parse, the workflow is absent from the
// spec, the workflow declares no budgets, or the budget's period is
// unrecognized. A run-lookup failure (including run.ErrNotFound) and a
// cost-sum failure are returned as errors for the handler to map.
func (s *Server) runBudgetStatus(ctx context.Context, runID uuid.UUID) (*budgetStatusResponse, error) {
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
	if len(runRow.WorkflowSpec) == 0 {
		return nil, nil
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		return nil, nil
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok || len(wf.Budgets) == 0 {
		return nil, nil
	}

	// The display shape carries a single block; the first budget is the
	// one the dogfood workflows declare. A workflow with multiple
	// budgets surfaces only the first here (documented on the endpoint).
	b := wf.Budgets[0]

	loc := s.cfg.BudgetLocation
	if loc == nil {
		loc = time.UTC
	}
	now := time.Now()

	d, ok, err := evaluateWorkflowBudget(ctx, summer, runRow.Repo, runRow.WorkflowID, b, now, loc)
	if err != nil {
		return nil, err
	}
	if !ok {
		// Unrecognized period — the schema enum makes this unreachable.
		return nil, nil
	}

	tier := budgetTierOK
	switch {
	case d.Over:
		tier = budgetTierOver
	case d.WarnCrossed:
		tier = budgetTierWarn
	}

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
		Enforcement: enforcement,
	}, nil
}

// handleGetRunBudget implements GET /v0/runs/{run_id}/budget. Returns
// the current-period periodic-budget status for the run's workflow, or
// 200 with an empty object when no budget is configured. 400 on a bad
// UUID, 404 when the run doesn't resolve, 500 on a cost-sum failure.
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

	status, err := s.runBudgetStatus(r.Context(), runID)
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
