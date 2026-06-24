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

// cacheEfficiencyResponse is the GET /v0/runs/{run_id}/cache-efficiency
// body: the per-run prompt-cache efficiency metric derived from the run's
// cost_recorded audit ledger (ADR-044 slice 3 / #1352). DISPLAY-ONLY — it
// never gates a run; it surfaces how much of the run's input was served
// from cache and the net dollar effect so the operator can see and
// maximize cache-hit usage.
//
// When the run has no cost_recorded entries the endpoint returns 200 with
// an empty object instead of this shape, so callers branch on the presence
// of a field rather than on the status code — mirroring the /budget
// precedent.
type cacheEfficiencyResponse struct {
	FreshInputTokens    int                          `json:"fresh_input_tokens"`
	CacheReadTokens     int                          `json:"cache_read_tokens"`
	CacheWriteTokens    int                          `json:"cache_write_tokens"`
	OutputTokens        int                          `json:"output_tokens"`
	CacheReadRatio      float64                      `json:"cache_read_ratio"`
	ReuseFactor         float64                      `json:"reuse_factor"`
	GrossReadSavingsUSD float64                      `json:"gross_read_savings_usd"`
	WritePenaltyUSD     float64                      `json:"write_penalty_usd"`
	NetSavingsUSD       float64                      `json:"net_savings_usd"`
	Stages              []cacheEfficiencyStageResult `json:"stages,omitempty"`
}

// cacheEfficiencyStageResult is the per-source breakdown row.
type cacheEfficiencyStageResult struct {
	Source              string  `json:"source"`
	FreshInputTokens    int     `json:"fresh_input_tokens"`
	CacheReadTokens     int     `json:"cache_read_tokens"`
	CacheWriteTokens    int     `json:"cache_write_tokens"`
	OutputTokens        int     `json:"output_tokens"`
	CacheReadRatio      float64 `json:"cache_read_ratio"`
	ReuseFactor         float64 `json:"reuse_factor"`
	GrossReadSavingsUSD float64 `json:"gross_read_savings_usd"`
	WritePenaltyUSD     float64 `json:"write_penalty_usd"`
	NetSavingsUSD       float64 `json:"net_savings_usd"`
}

// runCacheEfficiency reads the run's cost_recorded ledger (the same
// AuditRepo.ListForRunByCategory call sumRunTokens uses), decodes each
// payload's model + token split + optional source, and folds them into the
// per-run cache-efficiency metric via cost.AggregateCacheEfficiency.
//
// Returns (nil, nil) — "nothing to report" — when the AuditRepo is
// unconfigured or the run has no cost_recorded entries, so the handler
// renders an empty object and callers branch on presence like /budget. A
// list failure is returned as an error for the handler to map; an
// individual unparsable payload is skipped (best-effort), and a run whose
// every entry is unparsable collapses to (nil, nil).
func (s *Server) runCacheEfficiency(ctx context.Context, runID uuid.UUID) (*cacheEfficiencyResponse, error) {
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

	costEntries := make([]cost.CacheEfficiencyEntry, 0, len(entries))
	for _, e := range entries {
		// input_tokens is the FRESH (cache-exclusive) count since #1349;
		// source is absent on the runner stage-agent path (recordCost) and
		// present on reviewer entries (recordReviewerCost) — an absent source
		// buckets as the `agent` stage inside AggregateCacheEfficiency.
		var p struct {
			Model            string `json:"model"`
			InputTokens      int    `json:"input_tokens"`
			OutputTokens     int    `json:"output_tokens"`
			CacheReadTokens  int    `json:"cache_read_input_tokens"`
			CacheWriteTokens int    `json:"cache_write_input_tokens"`
			Source           string `json:"source"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		costEntries = append(costEntries, cost.CacheEfficiencyEntry{
			Model:      p.Model,
			Source:     p.Source,
			FreshInput: p.InputTokens,
			CacheRead:  p.CacheReadTokens,
			CacheWrite: p.CacheWriteTokens,
			Output:     p.OutputTokens,
		})
	}
	if len(costEntries) == 0 {
		return nil, nil
	}

	agg := cost.AggregateCacheEfficiency(costEntries)
	resp := &cacheEfficiencyResponse{
		FreshInputTokens:    agg.FreshInputTokens,
		CacheReadTokens:     agg.CacheReadTokens,
		CacheWriteTokens:    agg.CacheWriteTokens,
		OutputTokens:        agg.OutputTokens,
		CacheReadRatio:      agg.CacheReadRatio,
		ReuseFactor:         agg.ReuseFactor,
		GrossReadSavingsUSD: agg.GrossReadSavingsUSD,
		WritePenaltyUSD:     agg.WritePenaltyUSD,
		NetSavingsUSD:       agg.NetSavingsUSD,
	}
	for _, st := range agg.Stages {
		resp.Stages = append(resp.Stages, cacheEfficiencyStageResult{
			Source:              st.Source,
			FreshInputTokens:    st.FreshInputTokens,
			CacheReadTokens:     st.CacheReadTokens,
			CacheWriteTokens:    st.CacheWriteTokens,
			OutputTokens:        st.OutputTokens,
			CacheReadRatio:      st.CacheReadRatio,
			ReuseFactor:         st.ReuseFactor,
			GrossReadSavingsUSD: st.GrossReadSavingsUSD,
			WritePenaltyUSD:     st.WritePenaltyUSD,
			NetSavingsUSD:       st.NetSavingsUSD,
		})
	}
	return resp, nil
}

// handleGetRunCacheEfficiency implements GET
// /v0/runs/{run_id}/cache-efficiency. Returns the per-run cache-efficiency
// metric derived from the run's cost_recorded ledger, or 200 with an empty
// object when the run has no cost data. 503 when the run repository is
// unconfigured, 400 on a bad UUID, 404 when the run doesn't resolve, 500 on
// an audit-list failure. Mirrors handleGetRunBudget.
func (s *Server) handleGetRunCacheEfficiency(w http.ResponseWriter, r *http.Request) {
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

	// Existence check for 404 — matches handleGetRunBudget, whose
	// runBudgetStatus surfaces run.ErrNotFound via its GetRun. Cache
	// efficiency reads the audit ledger, not the run row, so the run-lookup
	// here is what distinguishes "missing run" (404) from "no cost data"
	// (200 + {}).
	if _, err := s.cfg.RunRepo.GetRun(r.Context(), runID); err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run failed", map[string]any{"error": err.Error()})
		return
	}

	resp, err := s.runCacheEfficiency(r.Context(), runID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run cache efficiency failed", map[string]any{"error": err.Error()})
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
