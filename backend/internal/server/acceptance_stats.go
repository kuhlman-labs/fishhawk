package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// acceptanceTriageStatsResult is the response body for
// GET /v0/acceptance-triage/stats.
type acceptanceTriageStatsResult struct {
	WorkflowID        string         `json:"workflow_id,omitempty"`
	Samples           int            `json:"samples"`
	ClassCounts       map[string]int `json:"class_counts"`
	DispositionCounts map[string]int `json:"disposition_counts"`
	// PlanReviewMisses is the count of class-3 triage decisions — the
	// plan-review-miss events (E31.11 / ADR-049 decision #4).
	PlanReviewMisses int `json:"plan_review_misses"`
	// PlanReviewMissRate is class-3 decisions / samples; 0 when samples
	// is 0 (never NaN).
	PlanReviewMissRate float64 `json:"plan_review_miss_rate"`
}

// acceptanceTriageStatsPayload is the subset of the
// acceptance_triage_decided payload the stats handler reads.
type acceptanceTriageStatsPayload struct {
	Class       string `json:"class"`
	Disposition string `json:"disposition"`
}

// handleGetAcceptanceTriageStats implements GET /v0/acceptance-triage/stats
// (E31.11 / #1539), modeled on handleGetCalibration: it aggregates
// acceptance_triage_decided audit entries by class and disposition and
// reports plan_review_miss_rate — the ADR-049 decision #4 feedback metric
// (class-3 decisions / all triage decisions).
//
// Query params:
//   - since       (optional, RFC 3339) — exclude entries before this timestamp
//   - workflow_id (optional) — filter to entries whose run.workflow_id matches
//     (best-effort N+1 RunRepo resolution, the filterRuntimeObservedSamples
//     pattern; entries whose run can't be resolved are skipped)
//
// Rate semantics are per triage DECISION, not per run: a run re-triaged
// after a re-run counts each decision. An entry whose payload fails to
// decode a class field is counted under class "" so the denominator never
// silently shrinks. No new auth scope — a read surface with the same
// posture as GET /v0/calibration.
func (s *Server) handleGetAcceptanceTriageStats(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "acceptance_stats_unconfigured",
			"acceptance triage stats require the audit repository to be configured", nil)
		return
	}

	workflowID := r.URL.Query().Get("workflow_id")
	var since time.Time
	if sinceStr := r.URL.Query().Get("since"); sinceStr != "" {
		var err error
		since, err = time.Parse(time.RFC3339, sinceStr)
		if err != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"since must be an RFC 3339 timestamp",
				map[string]any{"field": "since", "got": sinceStr})
			return
		}
	}

	category := CategoryAcceptanceTriageDecided
	entries, err := s.cfg.AuditRepo.ListAll(r.Context(), audit.ListAllParams{
		Category: &category,
	})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list audit entries failed", map[string]any{"error": err.Error()})
		return
	}

	result := acceptanceTriageStatsResult{
		WorkflowID:        workflowID,
		ClassCounts:       map[string]int{},
		DispositionCounts: map[string]int{},
	}
	for _, e := range entries {
		if !since.IsZero() && e.Timestamp.Before(since) {
			continue
		}
		if workflowID != "" {
			// Resolve the run to match workflow_id. Best-effort: skip when
			// RunRepo is nil or the run lookup fails — N+1 is acceptable
			// for v0 data volumes (same posture as
			// filterRuntimeObservedSamples).
			if s.cfg.RunRepo == nil || e.RunID == nil {
				continue
			}
			runRow, rerr := s.cfg.RunRepo.GetRun(r.Context(), *e.RunID)
			if rerr != nil || runRow.WorkflowID != workflowID {
				continue
			}
		}
		// A payload that fails to decode counts under class "" — the
		// denominator never silently shrinks on a malformed entry.
		var p acceptanceTriageStatsPayload
		_ = json.Unmarshal(e.Payload, &p)
		result.Samples++
		result.ClassCounts[p.Class]++
		result.DispositionCounts[p.Disposition]++
		if p.Class == acceptanceClass3 {
			result.PlanReviewMisses++
		}
	}
	if result.Samples > 0 {
		result.PlanReviewMissRate = float64(result.PlanReviewMisses) / float64(result.Samples)
	}
	s.writeJSON(w, r, http.StatusOK, result)
}
