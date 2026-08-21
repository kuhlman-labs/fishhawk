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

// --- wedge context (#1737) ---

// diagWedgeBody is diagBody plus the wedge_context block.
func diagWedgeBody(runID string) string {
	return `{
		"run_id": "` + runID + `",
		"workflow_id": "feature_change",
		"workflow_spec_hash": "spec123",
		"runner_kind": "github_actions",
		"run_state": "failed",
		"stages": [{"sequence": 0, "type": "review", "state": "running"}],
		"audit_sequence_range": {"min": 10, "max": 22},
		"versions": {"fishhawkd": {"version": "v0.4.1", "git_sha": "abc1234"}, "min_runner_version": "v0.3.0"},
		"wedge_context": {
			"blocking_checks": ["CI Pass", "CodeQL"],
			"campaign_item_state": "failed",
			"blocked_dependents": 3,
			"integrate_wave_error": "slice_integration_conflict"
		}
	}`
}

func newDiagBackendBody(t *testing.T, runID, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/runs/"+runID+"/diagnostics" {
			http.Error(w, `{"error":{"code":"run_not_found","message":"no run"}}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRunDiagnose_WedgeContextRendered asserts every wedge fact survives
// the REST decode into the rendered human output.
func TestRunDiagnose_WedgeContextRendered(t *testing.T) {
	id := uuid.New()
	srv := newDiagBackendBody(t, id.String(), diagWedgeBody(id.String()))
	t.Setenv("FISHHAWK_BACKEND_URL", srv.URL)
	t.Setenv("FISHHAWK_TOKEN", "")

	var stdout strings.Builder
	if got := run([]string{"diagnose", id.String()}, &stdout, io.Discard); got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	out := stdout.String()
	for _, want := range []string{
		"wedge context:", "CI Pass, CodeQL", "campaign item:     failed",
		"blocked dependents: 3", "slice_integration_conflict",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q:\n%s", want, out)
		}
	}
}

// TestRunDiagnose_NoWedgeContext_OutputUnchanged is the anti-noise
// counterfactual vehicle: a bundle with no wedge_context renders no
// wedge section at all.
//
// Counterfactual: delete the nil guard in printWedgeContext and this
// goes RED — the header prints on every run.
func TestRunDiagnose_NoWedgeContext_OutputUnchanged(t *testing.T) {
	id := uuid.New()
	srv := newDiagBackend(t, id.String())
	t.Setenv("FISHHAWK_BACKEND_URL", srv.URL)
	t.Setenv("FISHHAWK_TOKEN", "")

	var stdout strings.Builder
	if got := run([]string{"diagnose", id.String()}, &stdout, io.Discard); got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	if strings.Contains(stdout.String(), "wedge") {
		t.Errorf("non-wedged run rendered a wedge section:\n%s", stdout.String())
	}
}

// TestRunDiagnose_PartialWedgeContext covers the per-field omissions:
// a wedge block carrying only the fan-in marker renders that line and
// no empty check/campaign lines.
func TestRunDiagnose_PartialWedgeContext(t *testing.T) {
	id := uuid.New()
	body := `{
		"run_id": "` + id.String() + `",
		"workflow_id": "feature_change",
		"run_state": "failed",
		"stages": [],
		"versions": {"fishhawkd": {"version": "v0.4.1", "git_sha": "abc1234"}, "min_runner_version": "v0.3.0"},
		"wedge_context": {"integrate_wave_error": "slice_integration_conflict"}
	}`
	srv := newDiagBackendBody(t, id.String(), body)
	t.Setenv("FISHHAWK_BACKEND_URL", srv.URL)
	t.Setenv("FISHHAWK_TOKEN", "")

	var stdout strings.Builder
	if got := run([]string{"diagnose", id.String()}, &stdout, io.Discard); got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	out := stdout.String()
	if !strings.Contains(out, "fan-in:") || !strings.Contains(out, "slice_integration_conflict") {
		t.Errorf("missing fan-in line:\n%s", out)
	}
	for _, unwanted := range []string{"blocking checks:", "campaign item:", "blocked dependents:"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("rendered an empty %q line:\n%s", unwanted, out)
		}
	}
}

// TestRunDiagnose_WedgeContextJSONOutput pins that --output json carries
// the block through verbatim (the mirror struct decodes it).
func TestRunDiagnose_WedgeContextJSONOutput(t *testing.T) {
	id := uuid.New()
	srv := newDiagBackendBody(t, id.String(), diagWedgeBody(id.String()))
	t.Setenv("FISHHAWK_BACKEND_URL", srv.URL)
	t.Setenv("FISHHAWK_TOKEN", "")

	var stdout strings.Builder
	if got := run([]string{"diagnose", id.String(), "--output", "json"}, &stdout, io.Discard); got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	var decoded diagnosticBundle
	if err := json.Unmarshal([]byte(stdout.String()), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.WedgeContext == nil {
		t.Fatal("wedge_context dropped by the CLI mirror struct")
	}
	if decoded.WedgeContext.BlockedDependents != 3 ||
		decoded.WedgeContext.IntegrateWaveError != "slice_integration_conflict" ||
		decoded.WedgeContext.CampaignItemState != "failed" ||
		strings.Join(decoded.WedgeContext.BlockingChecks, ",") != "CI Pass,CodeQL" {
		t.Errorf("wedge_context = %+v", decoded.WedgeContext)
	}
}
