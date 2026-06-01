package server

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// calibrationResult is the response body for GET /v0/calibration.
type calibrationResult struct {
	WorkflowID             string                      `json:"workflow_id,omitempty"`
	StageType              string                      `json:"stage_type"`
	Samples                int                         `json:"samples"`
	PredictedP50Minutes    float64                     `json:"predicted_p50_minutes"`
	ActualP50Minutes       float64                     `json:"actual_p50_minutes"`
	ActualP95Minutes       float64                     `json:"actual_p95_minutes"`
	CalibrationRatio       float64                     `json:"calibration_ratio"`
	ConfidenceBandAccuracy map[string]confidenceBucket `json:"confidence_band_accuracy"`
}

// confidenceBucket counts samples and within_1.5x hits for one
// confidence level (low / medium / high).
type confidenceBucket struct {
	Samples    int `json:"samples"`
	Within1p5x int `json:"within_1.5x"`
}

// runtimeObservedPayload is the subset of the runtime_observed audit
// payload that the calibration handler reads. Fields match the keys
// written by emitRuntimeObserved in trace.go.
type runtimeObservedPayload struct {
	StageType        string  `json:"stage_type"`
	PredictedMinutes float64 `json:"predicted_minutes"`
	Confidence       string  `json:"confidence"`
	ActualMinutes    float64 `json:"actual_minutes"`
	Outcome          string  `json:"outcome"`
}

// handleGetCalibration implements GET /v0/calibration.
//
// Loads all runtime_observed audit entries, applies optional filters
// (workflow_id, stage_type, since), and returns aggregate statistics
// so operators and agents can self-correct future plan estimates.
//
// Query params:
//   - workflow_id  (optional) — filter to entries whose run.workflow_id matches
//   - stage_type   (optional, default "implement")
//   - since        (optional, RFC 3339) — exclude entries before this timestamp
//
// Computation:
//   - predicted_p50 / actual_p50 / actual_p95: sort and pick index
//   - calibration_ratio: actual_p50 / predicted_p50 (NaN-safe; 0 when predicted is 0)
//   - confidence_band_accuracy: per-level sample count + count where
//     actual is within 1.5× of predicted in either direction
func (s *Server) handleGetCalibration(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "calibration_unconfigured",
			"calibration endpoint requires the audit repository to be configured", nil)
		return
	}

	// Parse optional query params.
	workflowID := r.URL.Query().Get("workflow_id")
	stageType := r.URL.Query().Get("stage_type")
	if stageType == "" {
		stageType = "implement"
	}
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

	// Load all runtime_observed entries across both chains.
	category := "runtime_observed"
	entries, err := s.cfg.AuditRepo.ListAll(r.Context(), audit.ListAllParams{
		Category: &category,
	})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list audit entries failed", map[string]any{"error": err.Error()})
		return
	}

	// Decode payloads and apply filters.
	samples := s.filterRuntimeObservedSamples(r.Context(), entries, stageType, workflowID, since)

	result := computeCalibration(workflowID, stageType, samples)
	s.writeJSON(w, r, http.StatusOK, result)
}

// filterRuntimeObservedSamples decodes runtime_observed audit entries and
// keeps those whose stage_type matches stageType and — when workflowID is
// non-empty — whose run resolves to that workflow. The since cutoff drops
// entries older than the timestamp (zero time disables it). Best-effort:
// entries that fail to decode, lack a run id, or whose run can't be
// resolved are skipped. Shared by handleGetCalibration and
// implementCalibrationP95 so the two stay in lockstep.
func (s *Server) filterRuntimeObservedSamples(ctx context.Context, entries []*audit.Entry, stageType, workflowID string, since time.Time) []runtimeObservedPayload {
	var samples []runtimeObservedPayload
	for _, e := range entries {
		if !since.IsZero() && e.Timestamp.Before(since) {
			continue
		}
		var p runtimeObservedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		if p.StageType != stageType {
			continue
		}
		if workflowID != "" {
			// Resolve the run to match workflow_id. Best-effort: skip when
			// RunRepo is nil or the run lookup fails — N+1 is acceptable for
			// v0 data volumes.
			if s.cfg.RunRepo == nil || e.RunID == nil {
				continue
			}
			runRow, err := s.cfg.RunRepo.GetRun(ctx, *e.RunID)
			if err != nil || runRow.WorkflowID != workflowID {
				continue
			}
		}
		samples = append(samples, p)
	}
	return samples
}

// implementCalibrationP95 loads runtime_observed audit entries for the
// workflow, filters them to implement-stage samples, and returns the
// observed p95 actual-runtime in minutes. Returns ok=false when the
// AuditRepo is unconfigured, the ListAll scan fails, or there are zero
// samples — letting callers fall back rather than fail. Reuses the same
// load + filter + computeCalibration path as handleGetCalibration so the
// p95 it returns is identical to what the calibration endpoint reports.
func (s *Server) implementCalibrationP95(ctx context.Context, workflowID string) (p95Minutes float64, ok bool) {
	if s.cfg.AuditRepo == nil {
		return 0, false
	}
	category := "runtime_observed"
	entries, err := s.cfg.AuditRepo.ListAll(ctx, audit.ListAllParams{Category: &category})
	if err != nil {
		return 0, false
	}
	samples := s.filterRuntimeObservedSamples(ctx, entries, "implement", workflowID, time.Time{})
	if len(samples) == 0 {
		return 0, false
	}
	return computeCalibration(workflowID, "implement", samples).ActualP95Minutes, true
}

// computeCalibration derives statistics from the filtered sample slice.
func computeCalibration(workflowID, stageType string, samples []runtimeObservedPayload) calibrationResult {
	res := calibrationResult{
		WorkflowID:             workflowID,
		StageType:              stageType,
		Samples:                len(samples),
		ConfidenceBandAccuracy: map[string]confidenceBucket{},
	}
	if len(samples) == 0 {
		return res
	}

	// Collect actual and predicted values for percentile computation.
	actuals := make([]float64, 0, len(samples))
	predicteds := make([]float64, 0, len(samples))
	for _, s := range samples {
		actuals = append(actuals, s.ActualMinutes)
		predicteds = append(predicteds, s.PredictedMinutes)
	}
	sort.Float64s(actuals)
	sort.Float64s(predicteds)

	res.PredictedP50Minutes = percentile(predicteds, 50)
	res.ActualP50Minutes = percentile(actuals, 50)
	res.ActualP95Minutes = percentile(actuals, 95)

	if res.PredictedP50Minutes > 0 {
		res.CalibrationRatio = res.ActualP50Minutes / res.PredictedP50Minutes
	}

	// Confidence-band accuracy: per-level within_1.5x counts.
	buckets := map[string]confidenceBucket{}
	for _, s := range samples {
		b := buckets[s.Confidence]
		b.Samples++
		if s.PredictedMinutes > 0 {
			ratio := s.ActualMinutes / s.PredictedMinutes
			if ratio >= (1.0/1.5) && ratio <= 1.5 {
				b.Within1p5x++
			}
		}
		buckets[s.Confidence] = b
	}
	res.ConfidenceBandAccuracy = buckets
	return res
}

// percentile returns the p-th percentile of a sorted float64 slice.
// Uses nearest-rank method; returns 0 on an empty slice.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(float64(len(sorted))*p/100.0)) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
