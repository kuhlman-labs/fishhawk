package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/cost"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// costSummaryResponse is the GET /v0/runs/{run_id}/cost body: the per-run
// estimated cost derived from the run's cost_recorded audit ledger, with a
// per-stage (agent / plan_review / implement_review) breakdown and — when the
// run resolved to a merged PR — a cost-per-merged-PR rollup summed across
// every run sharing that PR URL. DISPLAY-ONLY — it never gates a run; it
// surfaces the cost to land work so the operator can see per-stage spend and
// the total cost of a merged PR.
//
// When the run has no cost_recorded entries the endpoint returns 200 with an
// empty object instead of this shape, so callers branch on the presence of a
// field rather than on the status code — mirroring the /cache-efficiency
// precedent.
type costSummaryResponse struct {
	TotalCostUSD float64           `json:"total_cost_usd"`
	Stages       []costStageResult `json:"stages,omitempty"`
	MergedPR     *mergedPRCost     `json:"merged_pr,omitempty"`
}

// costStageResult is the per-source cost breakdown row.
type costStageResult struct {
	Source  string  `json:"source"`
	CostUSD float64 `json:"cost_usd"`
}

// mergedPRCost is the cost-per-merged-PR rollup: the lineage sum of
// CostUSDTotal across every run sharing the PR URL (the original run plus any
// fixup/follow-up runs), present only when the run resolved to a merged PR (a
// pr_merged audit row exists AND the run carries a PullRequestURL).
type mergedPRCost struct {
	PullRequestURL     string  `json:"pull_request_url"`
	CostPerMergedPRUSD float64 `json:"cost_per_merged_pr_usd"`
	RunCount           int     `json:"run_count"`
}

// runCostListLimit bounds the per-PR lineage ListRuns query. Runs-per-PR is
// small (the original plus any fixup/follow-up runs), so a generous fixed cap
// covers the lineage without pagination. ListRunsFilter.Limit must be > 0.
const runCostListLimit = 1000

// runCostSummary reads the run's cost_recorded ledger (the same
// AuditRepo.ListForRunByCategory call runCacheEfficiency uses), folds each
// entry's USD into the per-run total and per-stage breakdown via
// cost.AggregateRunCost, and — when the run resolved to a merged PR — appends
// the cost-per-merged-PR rollup.
//
// Returns (nil, nil) — "nothing to report" — when the AuditRepo is
// unconfigured or the run has no cost_recorded entries, so the handler renders
// an empty object and callers branch on presence like /cache-efficiency. A
// list failure is returned as an error for the handler to map; an individual
// unparsable payload is skipped (best-effort).
func (s *Server) runCostSummary(ctx context.Context, runID uuid.UUID, runRow *run.Run) (*costSummaryResponse, error) {
	if s.cfg.AuditRepo == nil {
		return nil, nil
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "cost_recorded")
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}

	costEntries := make([]cost.RunCostEntry, 0, len(entries))
	for _, e := range entries {
		// usd is the per-entry estimated dollar figure; source is absent on the
		// runner stage-agent path (recordCost) and present as
		// plan_review/implement_review on reviewer entries (recordReviewerCost)
		// — an absent source buckets as the `agent` stage in AggregateRunCost.
		var p struct {
			USD    float64 `json:"usd"`
			Source string  `json:"source"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		costEntries = append(costEntries, cost.RunCostEntry{Source: p.Source, USD: p.USD})
	}
	if len(costEntries) == 0 {
		return nil, nil
	}

	agg := cost.AggregateRunCost(costEntries)
	// total_cost_usd is the run record's authoritative rolled total
	// (CostUSDTotal), which the per-stage sum reconstructs from the ledger.
	resp := &costSummaryResponse{TotalCostUSD: runRow.CostUSDTotal}
	for _, st := range agg.Stages {
		resp.Stages = append(resp.Stages, costStageResult{Source: st.Source, CostUSD: st.CostUSD})
	}

	// Cost-per-merged-PR rollup: a run is a "merged-PR run" iff it has >=1
	// pr_merged audit entry AND a non-nil PullRequestURL. Merge state is read
	// from the existing audit ledger — no GitHub API call — keeping the
	// surface display-only and offline.
	mergedPR, err := s.mergedPRCostFor(ctx, runID, runRow)
	if err != nil {
		return nil, err
	}
	resp.MergedPR = mergedPR
	return resp, nil
}

// mergedPRCostFor returns the cost-per-merged-PR rollup, or nil when the run
// did not resolve to a merged PR. A non-nil result sums CostUSDTotal across
// every run sharing the PR URL via ListRunsFilter.PullRequestURL — the same
// equality filter the threaded-runs view uses — because fixup/follow-up runs
// on the same PR each carry their own cost and the decision-useful "cost to
// land this PR" is their total.
func (s *Server) mergedPRCostFor(ctx context.Context, runID uuid.UUID, runRow *run.Run) (*mergedPRCost, error) {
	if runRow.PullRequestURL == nil || *runRow.PullRequestURL == "" {
		return nil, nil
	}
	merged, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryPRMerged)
	if err != nil {
		return nil, err
	}
	if len(merged) == 0 {
		return nil, nil
	}

	url := *runRow.PullRequestURL
	runs, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
		PullRequestURL: &url,
		Limit:          runCostListLimit,
	})
	if err != nil {
		return nil, err
	}
	var total float64
	for _, rn := range runs {
		total += rn.CostUSDTotal
	}
	return &mergedPRCost{
		PullRequestURL:     url,
		CostPerMergedPRUSD: total,
		RunCount:           len(runs),
	}, nil
}

// handleGetRunCost implements GET /v0/runs/{run_id}/cost. Returns the per-run
// cost breakdown derived from the run's cost_recorded ledger plus the
// cost-per-merged-PR rollup, or 200 with an empty object when the run has no
// cost data. 503 when the run repository is unconfigured, 400 on a bad UUID,
// 404 when the run doesn't resolve, 500 on an audit-list or list-runs failure.
// Mirrors handleGetRunCacheEfficiency.
func (s *Server) handleGetRunCost(w http.ResponseWriter, r *http.Request) {
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

	// Existence check for 404, and the run row is reused for CostUSDTotal +
	// PullRequestURL by runCostSummary so the merged-PR rollup needs no second
	// GetRun.
	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run failed", map[string]any{"error": err.Error()})
		return
	}

	resp, err := s.runCostSummary(r.Context(), runID, runRow)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run cost failed", map[string]any{"error": err.Error()})
		return
	}

	if resp == nil {
		// No cost data — 200 with an empty object so the MCP client treats
		// "no data" uniformly without status-code branching.
		s.writeJSON(w, r, http.StatusOK, struct{}{})
		return
	}

	s.writeJSON(w, r, http.StatusOK, resp)
}
