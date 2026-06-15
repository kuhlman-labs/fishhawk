package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// diagBody is the JSON the fake backend serves for the diagnostics
// endpoint. Mirrors the product-facts wire shape.
func diagBody(runID string) string {
	return `{
		"run_id": "` + runID + `",
		"workflow_id": "feature_change",
		"workflow_spec_hash": "spec123",
		"runner_kind": "local",
		"run_state": "failed",
		"stages": [
			{"sequence": 0, "type": "plan", "state": "succeeded"},
			{"sequence": 1, "type": "implement", "state": "failed"}
		],
		"failing_stage": {"sequence": 1, "type": "implement", "failure_category": "B", "failure_surface": "policy_evaluated"},
		"audit_sequence_range": {"min": 10, "max": 22},
		"versions": {"fishhawkd": {"version": "v0.4.1", "git_sha": "abc1234"}, "min_runner_version": "v0.3.0"}
	}`
}

func newDiagBackend(t *testing.T, runID string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/runs/"+runID+"/diagnostics" {
			http.Error(w, `{"error":{"code":"run_not_found","message":"no run"}}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, diagBody(runID))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunDiagnose_TextOutput(t *testing.T) {
	id := uuid.New()
	srv := newDiagBackend(t, id.String())
	t.Setenv("FISHHAWK_BACKEND_URL", srv.URL)
	t.Setenv("FISHHAWK_TOKEN", "")

	var stdout strings.Builder
	got := run([]string{"diagnose", id.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	out := stdout.String()
	for _, want := range []string{id.String(), "feature_change", "spec123", "local", "failed",
		"v0.4.1", "abc1234", "implement", "category B", "policy_evaluated", "10..22"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q:\n%s", want, out)
		}
	}
}

func TestRunDiagnose_JSONOutput(t *testing.T) {
	id := uuid.New()
	srv := newDiagBackend(t, id.String())
	t.Setenv("FISHHAWK_BACKEND_URL", srv.URL)
	t.Setenv("FISHHAWK_TOKEN", "")

	var stdout strings.Builder
	got := run([]string{"diagnose", "--output", "json", id.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	var b diagnosticBundle
	if err := json.Unmarshal([]byte(stdout.String()), &b); err != nil {
		t.Fatalf("decode round-trip: %v", err)
	}
	if b.RunID != id.String() || b.FailingStage == nil || b.FailingStage.FailureCategory != "B" {
		t.Errorf("decoded bundle wrong: %+v", b)
	}
	if b.AuditSequenceRange == nil || b.AuditSequenceRange.Max != 22 {
		t.Errorf("audit range = %+v", b.AuditSequenceRange)
	}
}

func TestRunDiagnose_BadUUID(t *testing.T) {
	got := run([]string{"diagnose", "not-a-uuid"}, io.Discard, io.Discard)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
}

func TestRunDiagnose_MissingRunID(t *testing.T) {
	var stderr strings.Builder
	got := run([]string{"diagnose"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "run-id> required") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunDiagnose_InvalidOutput(t *testing.T) {
	got := run([]string{"diagnose", "--output", "xml", uuid.New().String()}, io.Discard, io.Discard)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
}

func TestRunDiagnose_NotFound(t *testing.T) {
	id := uuid.New()
	// Backend that 404s every path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"run_not_found","message":"no run with that id"}}`)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("FISHHAWK_BACKEND_URL", srv.URL)
	t.Setenv("FISHHAWK_TOKEN", "")

	var stderr strings.Builder
	got := run([]string{"diagnose", id.String()}, io.Discard, &stderr)
	if got != exitFailure {
		t.Errorf("status = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "run_not_found") {
		t.Errorf("stderr = %q", stderr.String())
	}
}
