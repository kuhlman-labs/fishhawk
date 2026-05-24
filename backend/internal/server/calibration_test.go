package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// calibrationAuditFake is an audit.Repository for calibration tests.
// ListAll and AppendChained are the only methods exercised here.
type calibrationAuditFake struct {
	entries []*audit.Entry
}

func (f *calibrationAuditFake) Append(_ context.Context, _ audit.AppendParams) (*audit.Entry, error) {
	return nil, nil
}
func (f *calibrationAuditFake) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	rid := p.RunID
	e := &audit.Entry{
		ID:        uuid.New(),
		RunID:     &rid,
		Timestamp: p.Timestamp,
		Category:  p.Category,
		Payload:   p.Payload,
	}
	f.entries = append(f.entries, e)
	return e, nil
}
func (f *calibrationAuditFake) AppendGlobalChained(_ context.Context, _ audit.GlobalChainAppendParams) (*audit.Entry, error) {
	return nil, nil
}
func (f *calibrationAuditFake) Get(_ context.Context, _ uuid.UUID) (*audit.Entry, error) {
	return nil, nil
}
func (f *calibrationAuditFake) ListForRun(_ context.Context, _ uuid.UUID) ([]*audit.Entry, error) {
	return nil, nil
}
func (f *calibrationAuditFake) ListGlobal(_ context.Context) ([]*audit.Entry, error) {
	return nil, nil
}
func (f *calibrationAuditFake) LastForRun(_ context.Context, _ uuid.UUID) (*audit.Entry, error) {
	return nil, nil
}
func (f *calibrationAuditFake) ListForRunByCategory(_ context.Context, _ uuid.UUID, _ string) ([]*audit.Entry, error) {
	return nil, nil
}
func (f *calibrationAuditFake) ChainsByParent(_ context.Context, _ uuid.UUID, _ bool) ([]*audit.Entry, error) {
	return nil, nil
}
func (f *calibrationAuditFake) ListAll(_ context.Context, p audit.ListAllParams) ([]*audit.Entry, error) {
	var out []*audit.Entry
	for _, e := range f.entries {
		if p.Category != nil && e.Category != *p.Category {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// seedRuntimeObserved appends a runtime_observed audit entry with the
// given payload fields to the fake's entries slice.
func seedRuntimeObserved(t *testing.T, f *calibrationAuditFake, runID uuid.UUID, predicted, actual float64, confidence, outcome string, ts time.Time) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"stage_type":        "implement",
		"predicted_minutes": predicted,
		"confidence":        confidence,
		"actual_seconds":    actual * 60,
		"actual_minutes":    actual,
		"delta_minutes":     actual - predicted,
		"outcome":           outcome,
	})
	if err != nil {
		t.Fatal(err)
	}
	rid := runID
	f.entries = append(f.entries, &audit.Entry{
		ID:        uuid.New(),
		RunID:     &rid,
		Timestamp: ts,
		Category:  "runtime_observed",
		Payload:   payload,
	})
}

// TestGetCalibration_UnconfiguredReturns503 confirms the endpoint
// 503s when AuditRepo is not wired.
func TestGetCalibration_UnconfiguredReturns503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	req := httptest.NewRequest(http.MethodGet, "/v0/calibration", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// TestGetCalibration_EmptySamples confirms 200 with zero samples.
func TestGetCalibration_EmptySamples(t *testing.T) {
	f := &calibrationAuditFake{}
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: f})
	req := httptest.NewRequest(http.MethodGet, "/v0/calibration", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var res calibrationResult
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Samples != 0 {
		t.Errorf("samples = %d, want 0", res.Samples)
	}
}

// TestGetCalibration_P50P95 seeds 10 runtime_observed entries across
// two confidence levels and asserts correct sample count, p50, p95,
// calibration_ratio, and within_1.5x counts.
func TestGetCalibration_P50P95(t *testing.T) {
	f := &calibrationAuditFake{}
	now := time.Now().UTC()
	runID := uuid.New()

	// 6 low-confidence entries: actuals 10, 12, 14, 16, 18, 20 min; predicted 15
	for _, actual := range []float64{10, 12, 14, 16, 18, 20} {
		seedRuntimeObserved(t, f, runID, 15, actual, "low", "succeeded", now)
	}
	// 4 high-confidence entries: actuals 8, 10, 12, 14 min; predicted 10
	for _, actual := range []float64{8, 10, 12, 14} {
		seedRuntimeObserved(t, f, runID, 10, actual, "high", "succeeded", now)
	}

	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: f})
	req := httptest.NewRequest(http.MethodGet, "/v0/calibration", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var res calibrationResult
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if res.Samples != 10 {
		t.Errorf("samples = %d, want 10", res.Samples)
	}

	// actuals sorted: 8, 10, 10, 12, 12, 14, 14, 16, 18, 20
	// p50: ceil(10*50/100)-1 = ceil(5)-1 = idx 4 = 12
	if res.ActualP50Minutes != 12.0 {
		t.Errorf("actual_p50 = %v, want 12.0", res.ActualP50Minutes)
	}
	// p95: ceil(10*95/100)-1 = ceil(9.5)-1 = idx 9 = 20
	if res.ActualP95Minutes != 20.0 {
		t.Errorf("actual_p95 = %v, want 20.0", res.ActualP95Minutes)
	}

	// predicted sorted: 10,10,10,10,15,15,15,15,15,15 → p50 idx 4 = 15
	if res.PredictedP50Minutes != 15.0 {
		t.Errorf("predicted_p50 = %v, want 15.0", res.PredictedP50Minutes)
	}

	// calibration_ratio = 12 / 15 ≈ 0.8
	wantRatio := 12.0 / 15.0
	if res.CalibrationRatio < wantRatio-0.001 || res.CalibrationRatio > wantRatio+0.001 {
		t.Errorf("calibration_ratio = %v, want ~%v", res.CalibrationRatio, wantRatio)
	}

	// low: all 6 actuals in [10, 22.5] → 6 within_1.5x
	lowB := res.ConfidenceBandAccuracy["low"]
	if lowB.Samples != 6 {
		t.Errorf("low.samples = %d, want 6", lowB.Samples)
	}
	if lowB.Within1p5x != 6 {
		t.Errorf("low.within_1.5x = %d, want 6", lowB.Within1p5x)
	}

	// high: all 4 actuals in [6.67, 15] → 4 within_1.5x
	highB := res.ConfidenceBandAccuracy["high"]
	if highB.Samples != 4 {
		t.Errorf("high.samples = %d, want 4", highB.Samples)
	}
	if highB.Within1p5x != 4 {
		t.Errorf("high.within_1.5x = %d, want 4", highB.Within1p5x)
	}
}

// TestGetCalibration_SinceFilter confirms entries before 'since' are excluded.
func TestGetCalibration_SinceFilter(t *testing.T) {
	f := &calibrationAuditFake{}
	runID := uuid.New()
	now := time.Now().UTC()
	old := now.Add(-2 * time.Hour)

	seedRuntimeObserved(t, f, runID, 10, 12, "medium", "succeeded", old) // before since → excluded
	seedRuntimeObserved(t, f, runID, 10, 12, "medium", "succeeded", now) // after since → included
	seedRuntimeObserved(t, f, runID, 10, 12, "medium", "succeeded", now) // after since → included

	since := now.Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: f})
	req := httptest.NewRequest(http.MethodGet, "/v0/calibration?since="+since, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var res calibrationResult
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Samples != 2 {
		t.Errorf("samples = %d, want 2 (since filter excluded old entry)", res.Samples)
	}
}

// TestGetCalibration_BadSince confirms a 400 on an unparseable since.
func TestGetCalibration_BadSince(t *testing.T) {
	f := &calibrationAuditFake{}
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: f})
	req := httptest.NewRequest(http.MethodGet, "/v0/calibration?since=not-a-date", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestGetCalibration_WorkflowIDFilter_NilRunRepo confirms that when
// workflow_id is set and RunRepo is nil, entries are skipped.
func TestGetCalibration_WorkflowIDFilter_NilRunRepo(t *testing.T) {
	f := &calibrationAuditFake{}
	runID := uuid.New()
	now := time.Now().UTC()
	seedRuntimeObserved(t, f, runID, 10, 12, "medium", "succeeded", now)

	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: f})
	req := httptest.NewRequest(http.MethodGet, "/v0/calibration?workflow_id=feature_change", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var res calibrationResult
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// RunRepo is nil → every entry is skipped
	if res.Samples != 0 {
		t.Errorf("samples = %d, want 0 (RunRepo nil skips all workflow_id entries)", res.Samples)
	}
}

// TestGetCalibration_StageTypeDefaultImplement confirms that entries
// whose stage_type is not "implement" are excluded by the default filter.
func TestGetCalibration_StageTypeDefaultImplement(t *testing.T) {
	f := &calibrationAuditFake{}
	runID := uuid.New()
	now := time.Now().UTC()

	// An implement entry (matches default).
	seedRuntimeObserved(t, f, runID, 10, 12, "medium", "succeeded", now)

	// A plan entry (stage_type: plan) — should be excluded.
	planPayload, _ := json.Marshal(map[string]any{
		"stage_type":        "plan",
		"predicted_minutes": 10.0,
		"confidence":        "medium",
		"actual_seconds":    720.0,
		"actual_minutes":    12.0,
		"delta_minutes":     2.0,
		"outcome":           "succeeded",
	})
	planRunID := runID
	f.entries = append(f.entries, &audit.Entry{
		ID:        uuid.New(),
		RunID:     &planRunID,
		Timestamp: now,
		Category:  "runtime_observed",
		Payload:   planPayload,
	})

	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: f})
	req := httptest.NewRequest(http.MethodGet, "/v0/calibration", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var res calibrationResult
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Samples != 1 {
		t.Errorf("samples = %d, want 1 (plan entry excluded by stage_type=implement)", res.Samples)
	}
	if res.StageType != "implement" {
		t.Errorf("stage_type = %q, want implement", res.StageType)
	}
}

// TestGetCalibration_WorkflowIDFilter_WithRunRepo confirms the
// workflow_id filter matches on run.workflow_id when RunRepo is wired.
func TestGetCalibration_WorkflowIDFilter_WithRunRepo(t *testing.T) {
	f := &calibrationAuditFake{}
	rr := newFakeRepo()
	now := time.Now().UTC()

	runA := uuid.New()
	runB := uuid.New()
	rr.runs[runA] = &run.Run{ID: runA, Repo: "x/y", WorkflowID: "feature_change", State: run.StatePending}
	rr.runs[runB] = &run.Run{ID: runB, Repo: "x/y", WorkflowID: "other_workflow", State: run.StatePending}

	seedRuntimeObserved(t, f, runA, 10, 12, "medium", "succeeded", now) // feature_change → included
	seedRuntimeObserved(t, f, runB, 10, 15, "medium", "succeeded", now) // other_workflow → excluded

	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: f, RunRepo: rr})
	req := httptest.NewRequest(http.MethodGet, "/v0/calibration?workflow_id=feature_change", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var res calibrationResult
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Samples != 1 {
		t.Errorf("samples = %d, want 1 (only feature_change)", res.Samples)
	}
	if res.WorkflowID != "feature_change" {
		t.Errorf("workflow_id = %q, want feature_change", res.WorkflowID)
	}
}

// TestComputeCalibration_Percentiles verifies the percentile function
// with a small deterministic dataset.
func TestComputeCalibration_Percentiles(t *testing.T) {
	// 5 entries, actuals 1, 3, 5, 7, 9 (sorted), predicted 5 each.
	// p50: ceil(5*50/100)=ceil(2.5)=3, idx 2 = 5
	// p95: ceil(5*95/100)=ceil(4.75)=5, idx 4 = 9
	samples := []runtimeObservedPayload{
		{StageType: "implement", PredictedMinutes: 5, ActualMinutes: 1, Confidence: "medium"},
		{StageType: "implement", PredictedMinutes: 5, ActualMinutes: 3, Confidence: "medium"},
		{StageType: "implement", PredictedMinutes: 5, ActualMinutes: 5, Confidence: "medium"},
		{StageType: "implement", PredictedMinutes: 5, ActualMinutes: 7, Confidence: "medium"},
		{StageType: "implement", PredictedMinutes: 5, ActualMinutes: 9, Confidence: "medium"},
	}
	res := computeCalibration("", "implement", samples)
	if res.Samples != 5 {
		t.Errorf("samples = %d, want 5", res.Samples)
	}
	if res.ActualP50Minutes != 5.0 {
		t.Errorf("p50 = %v, want 5.0", res.ActualP50Minutes)
	}
	if res.ActualP95Minutes != 9.0 {
		t.Errorf("p95 = %v, want 9.0", res.ActualP95Minutes)
	}
	if res.CalibrationRatio < 1.0-0.001 || res.CalibrationRatio > 1.0+0.001 {
		t.Errorf("calibration_ratio = %v, want 1.0", res.CalibrationRatio)
	}
}

// TestGetCalibration_CalibrationRatioZeroWhenNoPredicted confirms the
// calibration_ratio is 0 (not NaN/Inf) when predicted_minutes is 0.
func TestGetCalibration_CalibrationRatioZeroWhenNoPredicted(t *testing.T) {
	samples := []runtimeObservedPayload{
		{StageType: "implement", PredictedMinutes: 0, ActualMinutes: 10, Confidence: "medium"},
	}
	res := computeCalibration("", "implement", samples)
	if res.CalibrationRatio != 0 {
		t.Errorf("calibration_ratio = %v, want 0 when predicted is 0", res.CalibrationRatio)
	}
}
