package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// --- fishhawk_report_product_issue (#1006) ---

// productReportFakeBackend serves the two endpoints the tool touches:
// POST /v0/runs/{run_id}/product-reports (the egress) and
// GET  /v0/runs/{run_id}/diagnostics    (the transparency preview).
// lastBody captures the decoded report request so tests assert the
// consent threading. diagStatus drives the preview path (default 200);
// reportStatus + reportErrBody drive the egress error-path tests.
type productReportFakeBackend struct {
	mu           sync.Mutex
	lastBody     productReportBody
	reportCalls  int
	diagCalls    int
	reportStatus int
	reportErr    string
	diagStatus   int
}

func newProductReportFakeBackend(t *testing.T) (*productReportFakeBackend, *httptest.Server) {
	fb := &productReportFakeBackend{reportStatus: http.StatusCreated, diagStatus: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/product-reports", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body productReportBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.reportCalls++
		fb.lastBody = body
		status, errBody := fb.reportStatus, fb.reportErr
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		action := "created"
		if body.Kind == "feature" {
			action = "created"
		}
		_ = json.NewEncoder(w).Encode(ProductReport{
			Fingerprint: "abc123",
			Action:      action,
			Number:      4242,
			URL:         "https://github.com/kuhlman-labs/fishhawk/issues/4242",
			Destination: "kuhlman-labs/fishhawk",
		})
	})
	mux.HandleFunc("GET /v0/runs/{run_id}/diagnostics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fb.mu.Lock()
		fb.diagCalls++
		status := fb.diagStatus
		fb.mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"boom"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(DiagnosticBundle{
			RunID:      r.PathValue("run_id"),
			WorkflowID: "feature_change",
			RunState:   "failed",
			Versions:   DiagnosticVersions{Fishhawkd: DiagnosticComponent{Version: "dev"}},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fb, srv
}

const sampleRunUUID = "11111111-1111-1111-1111-111111111111"

func TestReportProductIssue_HappyPath_DefaultsToFactsOnly(t *testing.T) {
	fb, srv := newProductReportFakeBackend(t)
	r := newResolver(srv, nil)

	_, out, err := r.reportProductIssue(context.Background(), nil, ReportProductIssueInput{
		RunID: sampleRunUUID,
		Kind:  "bug",
	})
	if err != nil {
		t.Fatalf("reportProductIssue: %v", err)
	}
	if out.Report.Number != 4242 || out.Report.Action != "created" {
		t.Errorf("report = %+v, want number=4242 action=created", out.Report)
	}
	if out.FreeTextIncluded {
		t.Error("FreeTextIncluded = true, want false on the default path")
	}
	// Consent contract: no free text on the wire by default.
	if fb.lastBody.IncludeFreeText || fb.lastBody.Description != "" {
		t.Errorf("default request carried free text: %+v", fb.lastBody)
	}
	// Transparency preview attached.
	if out.Diagnostics == nil || out.Diagnostics.WorkflowID != "feature_change" {
		t.Errorf("diagnostics preview = %+v, want the bundle attached", out.Diagnostics)
	}
	if fb.reportCalls != 1 || fb.diagCalls != 1 {
		t.Errorf("calls report=%d diag=%d, want 1/1", fb.reportCalls, fb.diagCalls)
	}
}

func TestReportProductIssue_RunIDFromEnv(t *testing.T) {
	fb, srv := newProductReportFakeBackend(t)
	r := newResolver(srv, map[string]string{"FISHHAWK_RUN_ID": sampleRunUUID})

	_, _, err := r.reportProductIssue(context.Background(), nil, ReportProductIssueInput{Kind: "feature"})
	if err != nil {
		t.Fatalf("reportProductIssue: %v", err)
	}
	if fb.reportCalls != 1 {
		t.Errorf("report called %d times, want 1 (env run-id fallback)", fb.reportCalls)
	}
	if fb.lastBody.Kind != "feature" {
		t.Errorf("kind = %q, want feature", fb.lastBody.Kind)
	}
}

func TestReportProductIssue_FreeTextCrossesOnlyWithConsent(t *testing.T) {
	fb, srv := newProductReportFakeBackend(t)
	r := newResolver(srv, nil)

	// Consent set: the description crosses (server redacts it).
	_, out, err := r.reportProductIssue(context.Background(), nil, ReportProductIssueInput{
		RunID:           sampleRunUUID,
		Description:     "the planner mis-ordered my stages",
		IncludeFreeText: true,
	})
	if err != nil {
		t.Fatalf("reportProductIssue: %v", err)
	}
	if !out.FreeTextIncluded {
		t.Error("FreeTextIncluded = false, want true when consented")
	}
	if !fb.lastBody.IncludeFreeText || fb.lastBody.Description != "the planner mis-ordered my stages" {
		t.Errorf("consented request did not carry the description: %+v", fb.lastBody)
	}
}

func TestReportProductIssue_DescriptionWithoutConsentIsDropped(t *testing.T) {
	fb, srv := newProductReportFakeBackend(t)
	r := newResolver(srv, nil)

	_, out, err := r.reportProductIssue(context.Background(), nil, ReportProductIssueInput{
		RunID:       sampleRunUUID,
		Description: "secret notes that must not leave without consent",
		// IncludeFreeText omitted (false).
	})
	if err != nil {
		t.Fatalf("reportProductIssue: %v", err)
	}
	if out.FreeTextIncluded {
		t.Error("FreeTextIncluded = true, want false without the consent flag")
	}
	// The un-consented description must never reach the wire.
	if fb.lastBody.Description != "" || fb.lastBody.IncludeFreeText {
		t.Errorf("un-consented description leaked onto the wire: %+v", fb.lastBody)
	}
}

func TestReportProductIssue_InvalidRunID_FailsLocally(t *testing.T) {
	fb, srv := newProductReportFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.reportProductIssue(context.Background(), nil, ReportProductIssueInput{RunID: "not-a-uuid"})
	if err == nil || !strings.Contains(err.Error(), "not a valid UUID") {
		t.Fatalf("err = %v, want UUID error", err)
	}
	if fb.reportCalls != 0 {
		t.Errorf("backend called %d times, want 0", fb.reportCalls)
	}
}

func TestReportProductIssue_MissingRunIDNoEnv_FailsLocally(t *testing.T) {
	fb, srv := newProductReportFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.reportProductIssue(context.Background(), nil, ReportProductIssueInput{})
	if err == nil || !strings.Contains(err.Error(), "run_id is required") {
		t.Fatalf("err = %v, want run_id-required error", err)
	}
	if fb.reportCalls != 0 {
		t.Errorf("backend called %d times, want 0", fb.reportCalls)
	}
}

func TestReportProductIssue_KillSwitch_PropagatesError(t *testing.T) {
	fb, srv := newProductReportFakeBackend(t)
	fb.reportStatus = http.StatusForbidden
	fb.reportErr = `{"error":{"code":"product_feedback_disabled","message":"product feedback is disabled for this repository"}}`
	r := newResolver(srv, nil)

	_, _, err := r.reportProductIssue(context.Background(), nil, ReportProductIssueInput{RunID: sampleRunUUID})
	if err == nil || !strings.Contains(err.Error(), "product_feedback_disabled") {
		t.Fatalf("err = %v, want product_feedback_disabled", err)
	}
}

// TestReportProductIssue_PreviewFailure_StillSucceeds asserts a failed
// diagnostics preview fetch does not turn a successful egress into a tool
// error — the report already filed.
func TestReportProductIssue_PreviewFailure_StillSucceeds(t *testing.T) {
	fb, srv := newProductReportFakeBackend(t)
	fb.diagStatus = http.StatusInternalServerError
	r := newResolver(srv, nil)

	_, out, err := r.reportProductIssue(context.Background(), nil, ReportProductIssueInput{RunID: sampleRunUUID})
	if err != nil {
		t.Fatalf("reportProductIssue: %v", err)
	}
	if out.Report.Number != 4242 {
		t.Errorf("report = %+v, want the filed report", out.Report)
	}
	if out.Diagnostics != nil {
		t.Errorf("diagnostics = %+v, want nil on preview failure", out.Diagnostics)
	}
}
