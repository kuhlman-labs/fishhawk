package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// reportBackend is a self-contained backend stub for the report-issue
// verb: it serves only POST /v0/runs/{run_id}/product-reports. lastBody
// captures the decoded request so tests assert the consent threading;
// status + errBody drive the error-path tests; resp overrides the echoed
// outcome.
type reportBackend struct {
	mu       sync.Mutex
	lastBody productReportRequest
	calls    int
	status   int
	errBody  string
	resp     *productReport
}

func newReportBackend(t *testing.T, runID string) (*reportBackend, *httptest.Server) {
	t.Helper()
	rb := &reportBackend{status: http.StatusCreated}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v0/runs/"+runID+"/product-reports" {
			http.Error(w, `{"error":{"code":"run_not_found","message":"no run"}}`, http.StatusNotFound)
			return
		}
		var body productReportRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		rb.mu.Lock()
		rb.calls++
		rb.lastBody = body
		status, errBody, resp := rb.status, rb.errBody, rb.resp
		rb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = io.WriteString(w, errBody)
			return
		}
		if resp == nil {
			resp = &productReport{
				Fingerprint: "abc123",
				Action:      "created",
				Number:      4242,
				URL:         "https://github.com/kuhlman-labs/fishhawk/issues/4242",
				Destination: "kuhlman-labs/fishhawk",
			}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return rb, srv
}

func TestRunReportIssue_TextOutput_DefaultsToFactsOnly(t *testing.T) {
	id := uuid.New()
	rb, srv := newReportBackend(t, id.String())
	t.Setenv("FISHHAWK_BACKEND_URL", srv.URL)
	t.Setenv("FISHHAWK_TOKEN", "")

	var stdout strings.Builder
	got := run([]string{"report-issue", id.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	out := stdout.String()
	for _, want := range []string{"filed product report #4242", "created", "abc123", "kuhlman-labs/fishhawk"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q:\n%s", want, out)
		}
	}
	// Default path: kind=bug, no free text on the wire.
	if rb.lastBody.Kind != "bug" {
		t.Errorf("kind = %q, want bug", rb.lastBody.Kind)
	}
	if rb.lastBody.IncludeFreeText || rb.lastBody.Description != "" {
		t.Errorf("default request carried free text: %+v", rb.lastBody)
	}
}

func TestRunReportIssue_JSONOutput(t *testing.T) {
	id := uuid.New()
	_, srv := newReportBackend(t, id.String())
	t.Setenv("FISHHAWK_BACKEND_URL", srv.URL)
	t.Setenv("FISHHAWK_TOKEN", "")

	var stdout strings.Builder
	got := run([]string{"report-issue", "--output", "json", id.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	var pr productReport
	if err := json.Unmarshal([]byte(stdout.String()), &pr); err != nil {
		t.Fatalf("decode round-trip: %v", err)
	}
	if pr.Number != 4242 || pr.Action != "created" {
		t.Errorf("decoded = %+v, want number=4242 action=created", pr)
	}
}

func TestRunReportIssue_OccurrenceRendering(t *testing.T) {
	id := uuid.New()
	rb, srv := newReportBackend(t, id.String())
	rb.resp = &productReport{Fingerprint: "fp", Action: "occurrence", Number: 7,
		URL: "https://github.com/kuhlman-labs/fishhawk/issues/7", Destination: "kuhlman-labs/fishhawk"}
	t.Setenv("FISHHAWK_BACKEND_URL", srv.URL)
	t.Setenv("FISHHAWK_TOKEN", "")

	var stdout strings.Builder
	got := run([]string{"report-issue", id.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	if !strings.Contains(stdout.String(), "occurrence appended to existing report #7") {
		t.Errorf("occurrence not rendered:\n%s", stdout.String())
	}
}

func TestRunReportIssue_FreeTextCrossesOnlyWithConsent(t *testing.T) {
	id := uuid.New()
	rb, srv := newReportBackend(t, id.String())
	t.Setenv("FISHHAWK_BACKEND_URL", srv.URL)
	t.Setenv("FISHHAWK_TOKEN", "")

	got := run([]string{"report-issue",
		"--kind", "feature",
		"--description", "I was trying to file a follow-up",
		"--include-free-text",
		id.String()}, io.Discard, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	if !rb.lastBody.IncludeFreeText || rb.lastBody.Description != "I was trying to file a follow-up" {
		t.Errorf("consented request did not carry the description: %+v", rb.lastBody)
	}
	if rb.lastBody.Kind != "feature" {
		t.Errorf("kind = %q, want feature", rb.lastBody.Kind)
	}
}

func TestRunReportIssue_DescriptionWithoutConsent_Warns_AndDrops(t *testing.T) {
	id := uuid.New()
	rb, srv := newReportBackend(t, id.String())
	t.Setenv("FISHHAWK_BACKEND_URL", srv.URL)
	t.Setenv("FISHHAWK_TOKEN", "")

	var stderr strings.Builder
	got := run([]string{"report-issue",
		"--description", "sensitive notes",
		id.String()}, io.Discard, &stderr)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	if !strings.Contains(stderr.String(), "--description ignored without --include-free-text") {
		t.Errorf("stderr missing consent warning: %s", stderr.String())
	}
	// The un-consented description must never reach the wire.
	if rb.lastBody.Description != "" || rb.lastBody.IncludeFreeText {
		t.Errorf("un-consented description leaked: %+v", rb.lastBody)
	}
}

func TestRunReportIssue_BadUUID(t *testing.T) {
	got := run([]string{"report-issue", "not-a-uuid"}, io.Discard, io.Discard)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
}

func TestRunReportIssue_MissingRunID(t *testing.T) {
	var stderr strings.Builder
	got := run([]string{"report-issue"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "run-id> required") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunReportIssue_InvalidKind(t *testing.T) {
	got := run([]string{"report-issue", "--kind", "nope", uuid.New().String()}, io.Discard, io.Discard)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
}

func TestRunReportIssue_InvalidOutput(t *testing.T) {
	got := run([]string{"report-issue", "--output", "xml", uuid.New().String()}, io.Discard, io.Discard)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
}

func TestRunReportIssue_KillSwitch_PropagatesError(t *testing.T) {
	id := uuid.New()
	rb, srv := newReportBackend(t, id.String())
	rb.status = http.StatusForbidden
	rb.errBody = `{"error":{"code":"product_feedback_disabled","message":"product feedback is disabled for this repository"}}`
	t.Setenv("FISHHAWK_BACKEND_URL", srv.URL)
	t.Setenv("FISHHAWK_TOKEN", "")

	var stderr strings.Builder
	got := run([]string{"report-issue", id.String()}, io.Discard, &stderr)
	if got != exitFailure {
		t.Errorf("status = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "product_feedback_disabled") {
		t.Errorf("stderr missing api code: %s", stderr.String())
	}
}
