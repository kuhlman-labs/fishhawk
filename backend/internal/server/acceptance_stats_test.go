package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// seedTriageDecided appends an acceptance_triage_decided entry with the given
// class/disposition to the calibrationAuditFake (reused for its ListAll).
func seedTriageDecided(t *testing.T, f *calibrationAuditFake, runID uuid.UUID, class, disposition string, ts time.Time) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"class":       class,
		"disposition": disposition,
	})
	if err != nil {
		t.Fatal(err)
	}
	rid := runID
	f.entries = append(f.entries, &audit.Entry{
		ID:        uuid.New(),
		RunID:     &rid,
		Timestamp: ts,
		Category:  CategoryAcceptanceTriageDecided,
		Payload:   payload,
	})
}

func getTriageStats(t *testing.T, s *Server, query string) (*httptest.ResponseRecorder, acceptanceTriageStatsResult) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v0/acceptance-triage/stats"+query, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	var res acceptanceTriageStatsResult
	if w.Code == http.StatusOK {
		if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return w, res
}

// TestGetAcceptanceTriageStats_MixedClasses is the issue-AC#2 done-means:
// mixed-class entries aggregate into correct class_counts,
// disposition_counts, and plan_review_miss_rate.
func TestGetAcceptanceTriageStats_MixedClasses(t *testing.T) {
	f := &calibrationAuditFake{}
	now := time.Now().UTC()
	runID := uuid.New()
	seedTriageDecided(t, f, runID, "1", "fixup_dispatched", now)
	seedTriageDecided(t, f, runID, "1", "fixup_dispatched", now)
	seedTriageDecided(t, f, runID, "2", "retry_dispatched", now)
	seedTriageDecided(t, f, runID, "3", "paged", now)
	// A non-triage entry must NOT count (the ListAll category filter).
	seedRuntimeObserved(t, f, runID, 10, 12, "low", "succeeded", now)

	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: f})
	w, res := getTriageStats(t, s, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if res.Samples != 4 {
		t.Errorf("samples = %d, want 4", res.Samples)
	}
	if res.ClassCounts["1"] != 2 || res.ClassCounts["2"] != 1 || res.ClassCounts["3"] != 1 {
		t.Errorf("class_counts = %v", res.ClassCounts)
	}
	if res.DispositionCounts["fixup_dispatched"] != 2 || res.DispositionCounts["paged"] != 1 {
		t.Errorf("disposition_counts = %v", res.DispositionCounts)
	}
	if res.PlanReviewMisses != 1 {
		t.Errorf("plan_review_misses = %d, want 1", res.PlanReviewMisses)
	}
	if want := 0.25; res.PlanReviewMissRate != want {
		t.Errorf("plan_review_miss_rate = %v, want %v", res.PlanReviewMissRate, want)
	}
}

// TestGetAcceptanceTriageStats_ZeroEntries: zero samples yields rate 0
// (never NaN) and empty count maps.
func TestGetAcceptanceTriageStats_ZeroEntries(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &calibrationAuditFake{}})
	w, res := getTriageStats(t, s, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if res.Samples != 0 || res.PlanReviewMisses != 0 {
		t.Errorf("zero-entry samples/misses = %d/%d, want 0/0", res.Samples, res.PlanReviewMisses)
	}
	if res.PlanReviewMissRate != 0 {
		t.Errorf("rate = %v, want 0 (never NaN)", res.PlanReviewMissRate)
	}
}

// TestGetAcceptanceTriageStats_MalformedPayloadCountsInDenominator: an entry
// whose payload fails to decode is counted under class "" so the denominator
// never silently shrinks.
func TestGetAcceptanceTriageStats_MalformedPayloadCountsInDenominator(t *testing.T) {
	f := &calibrationAuditFake{}
	now := time.Now().UTC()
	runID := uuid.New()
	seedTriageDecided(t, f, runID, "3", "paged", now)
	rid := runID
	f.entries = append(f.entries, &audit.Entry{
		ID: uuid.New(), RunID: &rid, Timestamp: now,
		Category: CategoryAcceptanceTriageDecided,
		Payload:  []byte(`{"class":`), // malformed
	})

	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: f})
	w, res := getTriageStats(t, s, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if res.Samples != 2 {
		t.Errorf("samples = %d, want 2 (malformed entry stays in the denominator)", res.Samples)
	}
	if res.ClassCounts[""] != 1 {
		t.Errorf(`class_counts[""] = %d, want 1`, res.ClassCounts[""])
	}
	if want := 0.5; res.PlanReviewMissRate != want {
		t.Errorf("rate = %v, want %v", res.PlanReviewMissRate, want)
	}
}

// TestGetAcceptanceTriageStats_SinceFilter: entries before the cutoff are
// excluded; an invalid since is a 400.
func TestGetAcceptanceTriageStats_SinceFilter(t *testing.T) {
	f := &calibrationAuditFake{}
	now := time.Now().UTC()
	runID := uuid.New()
	seedTriageDecided(t, f, runID, "3", "paged", now.Add(-48*time.Hour))
	seedTriageDecided(t, f, runID, "1", "fixup_dispatched", now)

	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: f})
	w, res := getTriageStats(t, s, "?since="+now.Add(-time.Hour).Format(time.RFC3339))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if res.Samples != 1 || res.ClassCounts["3"] != 0 {
		t.Errorf("since filter did not exclude the old entry: samples=%d class_counts=%v", res.Samples, res.ClassCounts)
	}
}

// TestGetAcceptanceTriageStats_InvalidSince400 pins the since parse-failure
// branch.
func TestGetAcceptanceTriageStats_InvalidSince400(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &calibrationAuditFake{}})
	w, _ := getTriageStats(t, s, "?since=yesterday")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestGetAcceptanceTriageStats_Unconfigured503 pins the nil-AuditRepo guard.
func TestGetAcceptanceTriageStats_Unconfigured503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	w, _ := getTriageStats(t, s, "")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// TestGetAcceptanceTriageStats_ListAllError500 pins the audit-scan failure
// branch.
func TestGetAcceptanceTriageStats_ListAllError500(t *testing.T) {
	f := &calibrationAuditFake{listAllErr: errors.New("scan boom")}
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: f})
	w, _ := getTriageStats(t, s, "")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// TestGetAcceptanceTriageStats_WorkflowFilter: the workflow_id filter
// resolves each entry's run via RunRepo and keeps only matches; entries
// whose run can't be resolved are skipped (best-effort N+1, the
// filterRuntimeObservedSamples posture).
func TestGetAcceptanceTriageStats_WorkflowFilter(t *testing.T) {
	f := &calibrationAuditFake{}
	now := time.Now().UTC()
	matchRun, otherRun, ghostRun := uuid.New(), uuid.New(), uuid.New()
	seedTriageDecided(t, f, matchRun, "3", "paged", now)
	seedTriageDecided(t, f, otherRun, "1", "fixup_dispatched", now)
	seedTriageDecided(t, f, ghostRun, "1", "fixup_dispatched", now) // unresolvable run

	rr := newPromptRunRepo()
	rr.getRuns[matchRun] = &run.Run{ID: matchRun, WorkflowID: "feature_change"}
	rr.getRuns[otherRun] = &run.Run{ID: otherRun, WorkflowID: "docs_change"}

	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: f, RunRepo: rr})
	w, res := getTriageStats(t, s, "?workflow_id=feature_change")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if res.Samples != 1 || res.ClassCounts["3"] != 1 {
		t.Errorf("workflow filter wrong: samples=%d class_counts=%v", res.Samples, res.ClassCounts)
	}
	if res.WorkflowID != "feature_change" {
		t.Errorf("workflow_id echo = %q", res.WorkflowID)
	}
	if res.PlanReviewMissRate != 1.0 {
		t.Errorf("rate = %v, want 1.0", res.PlanReviewMissRate)
	}
}
