package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/latency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// latencySummaryResponse is the GET /v0/runs/{run_id}/latency body: the
// per-run gate-latency (wait-on-human) rollup derived at read time from the
// run's audit-chain timestamps (#1702). It reports the time parked at each
// human gate (plan approval, implement-review → next dispatch, checks-green →
// merge), the total wait on human decisions, and the run's end-to-end wall
// clock. DISPLAY-ONLY — it never gates a run; it surfaces where a change spent
// its wall-clock time so the operator can see human-gate latency.
//
// When no gate interval resolves (a run that hasn't reached its first gate, or
// a run with no audit repository) the endpoint returns 200 with an empty
// object instead of this shape, so callers branch on the presence of a field
// rather than on the status code — mirroring the /cost and /cache-efficiency
// precedents.
type latencySummaryResponse struct {
	Gates                   []latencyGateResult `json:"gates,omitempty"`
	TotalWaitOnHumanSeconds float64             `json:"total_wait_on_human_seconds"`
	WallClockSeconds        float64             `json:"wall_clock_seconds"`
}

// latencyGateResult is one measured human gate: the interval between its
// opening and closing audit markers, with the wait in seconds.
type latencyGateResult struct {
	Gate        string    `json:"gate"`
	OpenedAt    time.Time `json:"opened_at"`
	ClosedAt    time.Time `json:"closed_at"`
	WaitSeconds float64   `json:"wait_seconds"`
}

// runLatencySummary reads the WHOLE audit chain for the run (ListForRun, not
// ListForRunByCategory — the rollup pairs multiple categories), maps each
// entry to a latency.GateEvent in chain order, and folds them via
// latency.AggregateGateLatency. runStart is the run row's CreatedAt; runEnd is
// the newest terminal audit timestamp (pr_merged / post_merge_observed), or the
// last entry's timestamp when the run hasn't merged.
//
// The `ci_green` boundary has no bare audit category: it is synthesized from
// the `run_auto_advanced` entry whose payload rule is
// `checks_green_awaiting_merge` (drive.RuleChecksGreenAwaitingMerge).
//
// Returns (nil, nil) — "nothing to report" — when the AuditRepo is
// unconfigured or no gate interval resolves, so the handler renders an empty
// object and callers branch on presence like /cost. A list failure is returned
// as an error for the handler to map.
func (s *Server) runLatencySummary(ctx context.Context, runID uuid.UUID, runRow *run.Run) (*latencySummaryResponse, error) {
	if s.cfg.AuditRepo == nil {
		return nil, nil
	}
	entries, err := s.cfg.AuditRepo.ListForRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}

	events := make([]latency.GateEvent, 0, len(entries))
	runEnd := runRow.CreatedAt
	for _, e := range entries {
		if e.Timestamp.After(runEnd) {
			runEnd = e.Timestamp
		}
		if cat, ok := gateEventCategory(e.Category, e.Payload); ok {
			events = append(events, latency.GateEvent{Category: cat, Timestamp: e.Timestamp})
		}
	}
	// Prefer a terminal-marker timestamp for runEnd when present; otherwise the
	// max timestamp above already stands in for "as far as the run has gotten".
	if term, ok := latestTerminalTimestamp(entries); ok {
		runEnd = term
	}

	roll := latency.AggregateGateLatency(events, runRow.CreatedAt, runEnd)
	if len(roll.Gates) == 0 {
		return nil, nil
	}

	resp := &latencySummaryResponse{
		TotalWaitOnHumanSeconds: roll.TotalWaitOnHumanSeconds,
		WallClockSeconds:        roll.WallClockSeconds,
	}
	for _, g := range roll.Gates {
		resp.Gates = append(resp.Gates, latencyGateResult{
			Gate:        g.Gate,
			OpenedAt:    g.OpenedAt,
			ClosedAt:    g.ClosedAt,
			WaitSeconds: g.WaitSeconds,
		})
	}
	return resp, nil
}

// gateEventCategory maps an audit entry to the latency category the aggregator
// keys on, or reports ok=false to skip the entry. Most categories map to
// themselves; the synthetic `ci_green` boundary is derived from the
// `run_auto_advanced` entry whose payload rule is checks_green_awaiting_merge.
func gateEventCategory(category string, payload json.RawMessage) (string, bool) {
	switch category {
	case latency.CategoryPlanGenerated,
		latency.CategoryApprovalSubmitted,
		latency.CategoryImplementReviewed,
		latency.CategoryAcceptanceDispatched,
		latency.CategoryPRMerged:
		return category, true
	case drive.Category: // run_auto_advanced
		var adv struct {
			Rule string `json:"rule"`
		}
		if err := json.Unmarshal(payload, &adv); err != nil {
			return "", false
		}
		if adv.Rule == string(drive.RuleChecksGreenAwaitingMerge) {
			return latency.CategoryCIGreen, true
		}
		return "", false
	default:
		return "", false
	}
}

// latestTerminalTimestamp returns the newest pr_merged / post_merge_observed
// timestamp in the chain, or ok=false when the run has no terminal marker.
// Entries arrive ascending by sequence, so the last matching entry is the
// newest.
func latestTerminalTimestamp(entries []*audit.Entry) (time.Time, bool) {
	var latest time.Time
	found := false
	for _, e := range entries {
		if e.Category == CategoryPRMerged || e.Category == CategoryPostMergeObserved {
			if !found || e.Timestamp.After(latest) {
				latest = e.Timestamp
				found = true
			}
		}
	}
	return latest, found
}

// handleGetRunLatency implements GET /v0/runs/{run_id}/latency. Returns the
// per-run gate-latency rollup derived from the run's audit-chain timestamps,
// or 200 with an empty object when no gate interval resolves. 503 when the run
// repository is unconfigured, 400 on a bad UUID, 404 when the run doesn't
// resolve, 500 on an audit-list failure. Mirrors handleGetRunCost.
func (s *Server) handleGetRunLatency(w http.ResponseWriter, r *http.Request) {
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

	// Existence check for 404, and the run row is reused for CreatedAt (runStart)
	// by runLatencySummary so no second GetRun is needed.
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

	resp, err := s.runLatencySummary(r.Context(), runID, runRow)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run latency failed", map[string]any{"error": err.Error()})
		return
	}

	if resp == nil {
		// No gate data — 200 with an empty object so the MCP client treats
		// "no data" uniformly without status-code branching.
		s.writeJSON(w, r, http.StatusOK, struct{}{})
		return
	}

	s.writeJSON(w, r, http.StatusOK, resp)
}
