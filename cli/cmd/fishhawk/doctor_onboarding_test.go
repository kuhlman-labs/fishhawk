package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allGreenReadinessJSON is the readiness payload for a fully-onboarded repo:
// App installed, spec fetched + valid, scopes adequate, no reviewer gaps.
// Shared with the runDoctor end-to-end tests in doctor_test.go.
const allGreenReadinessJSON = `{
  "repo": "kuhlman-labs/fishhawk",
  "app": {"installed": true, "installation_id": 42},
  "spec": {"source": "fetched", "valid": true},
  "reviewers": [{"provider": "anthropic", "model": "claude-opus-4-8", "available": true}],
  "scopes": {"adequate": true, "required": ["read:runs"], "missing": []}
}`

// findCheck returns the checkResult with the given label, or fails the test.
func findCheck(t *testing.T, results []checkResult, label string) checkResult {
	t.Helper()
	for _, r := range results {
		if r.label == label {
			return r
		}
	}
	t.Fatalf("no checkResult with label %q in %+v", label, results)
	return checkResult{}
}

// TestCheckOnboardingReadiness_AppNotInstalled asserts the app rung fails with
// the readiness reason and an install-URL remediation.
func TestCheckOnboardingReadiness_AppNotInstalled(t *testing.T) {
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusOK, `{
			"app": {"installed": false, "reason": "GitHub App is not installed on the target repository"},
			"spec": {"source": "unavailable", "note": "app not installed"},
			"reviewers": [],
			"scopes": {"adequate": true}
		}`), nil
	})
	results := checkOnboardingReadiness("http://localhost:8080", "fhk_t", "owner/name")
	app := findCheck(t, results, "app installed")
	if app.status != "fail" {
		t.Errorf("app status = %q, want fail", app.status)
	}
	if !strings.Contains(app.detail, "not installed on the target repository") {
		t.Errorf("app detail = %q, want the readiness reason", app.detail)
	}
	if !strings.Contains(app.remediate, "installations/new") {
		t.Errorf("app remediate = %q, want an install URL", app.remediate)
	}
}

// TestCheckOnboardingReadiness_ReviewerUnavailable asserts each reviewer rung
// fails carrying the adapter missing_hint verbatim.
func TestCheckOnboardingReadiness_ReviewerUnavailable(t *testing.T) {
	const hint = "set FISHHAWKD_ANTHROPIC_API_KEY to enable the anthropic reviewer"
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusOK, `{
			"app": {"installed": true},
			"spec": {"source": "fetched", "valid": true},
			"reviewers": [{"provider": "anthropic", "available": false, "missing_hint": "`+hint+`"}],
			"scopes": {"adequate": true}
		}`), nil
	})
	results := checkOnboardingReadiness("http://localhost:8080", "fhk_t", "owner/name")
	rv := findCheck(t, results, "reviewer available: anthropic")
	if rv.status != "fail" {
		t.Errorf("reviewer status = %q, want fail", rv.status)
	}
	if rv.remediate != hint {
		t.Errorf("reviewer remediate = %q, want the missing_hint verbatim %q", rv.remediate, hint)
	}
}

// TestCheckOnboardingReadiness_ScopeMissing asserts the scope rung fails
// listing the missing scopes and a reissue remediation.
func TestCheckOnboardingReadiness_ScopeMissing(t *testing.T) {
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusOK, `{
			"app": {"installed": true},
			"spec": {"source": "fetched", "valid": true},
			"reviewers": [],
			"scopes": {"adequate": false, "required": ["read:runs","write:runs"], "missing": ["write:runs"]}
		}`), nil
	})
	results := checkOnboardingReadiness("http://localhost:8080", "fhk_t", "owner/name")
	sc := findCheck(t, results, "token scope adequate")
	if sc.status != "fail" {
		t.Errorf("scope status = %q, want fail", sc.status)
	}
	if !strings.Contains(sc.detail, "write:runs") {
		t.Errorf("scope detail = %q, want the missing scope listed", sc.detail)
	}
	if !strings.Contains(sc.remediate, "token issue") || !strings.Contains(sc.remediate, "write:runs") {
		t.Errorf("scope remediate = %q, want a reissue hint naming the missing scope", sc.remediate)
	}
}

// TestCheckOnboardingReadiness_SpecInvalid asserts the committed-spec rung
// fails with a `fishhawk validate` remediation when source==fetched && !valid.
func TestCheckOnboardingReadiness_SpecInvalid(t *testing.T) {
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusOK, `{
			"app": {"installed": true},
			"spec": {"source": "fetched", "valid": false, "error": "stage[0]: missing type"},
			"reviewers": [],
			"scopes": {"adequate": true}
		}`), nil
	})
	results := checkOnboardingReadiness("http://localhost:8080", "fhk_t", "owner/name")
	sp := findCheck(t, results, "workflow spec (committed) valid")
	if sp.status != "fail" {
		t.Errorf("spec status = %q, want fail", sp.status)
	}
	if !strings.Contains(sp.remediate, "fishhawk validate") {
		t.Errorf("spec remediate = %q, want a `fishhawk validate` hint", sp.remediate)
	}
	if !strings.Contains(sp.remediate, "missing type") {
		t.Errorf("spec remediate = %q, want the server error carried through", sp.remediate)
	}
}

// TestCheckOnboardingReadiness_SpecUnavailable asserts an unavailable spec
// source degrades to warn (not fail).
func TestCheckOnboardingReadiness_SpecUnavailable(t *testing.T) {
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusOK, `{
			"app": {"installed": true},
			"spec": {"source": "unavailable", "note": "no workflow spec found on the default branch"},
			"reviewers": [],
			"scopes": {"adequate": true}
		}`), nil
	})
	results := checkOnboardingReadiness("http://localhost:8080", "fhk_t", "owner/name")
	sp := findCheck(t, results, "workflow spec (committed) valid")
	if sp.status != "warn" {
		t.Errorf("spec status = %q, want warn", sp.status)
	}
}

// TestCheckOnboardingReadiness_TransportError asserts a request error degrades
// to a single WARN rather than crashing the doctor.
func TestCheckOnboardingReadiness_TransportError(t *testing.T) {
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})
	results := checkOnboardingReadiness("http://localhost:8080", "fhk_t", "owner/name")
	if len(results) != 1 || results[0].status != "warn" {
		t.Fatalf("want a single warn on transport error, got %+v", results)
	}
}

// TestCheckOnboardingReadiness_Non200 asserts a 401/403/5xx response degrades
// to a single WARN naming the status.
func TestCheckOnboardingReadiness_Non200(t *testing.T) {
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusUnauthorized, `{"error":{"code":"unauthorized"}}`), nil
	})
	results := checkOnboardingReadiness("http://localhost:8080", "fhk_t", "owner/name")
	if len(results) != 1 || results[0].status != "warn" {
		t.Fatalf("want a single warn on non-200, got %+v", results)
	}
	if !strings.Contains(results[0].detail, "401") {
		t.Errorf("detail = %q, want the HTTP status named", results[0].detail)
	}
}

// TestCheckOnboardingReadiness_RepoUnresolved asserts an empty repo degrades
// to a single WARN prompting for --repo, without any HTTP call.
func TestCheckOnboardingReadiness_RepoUnresolved(t *testing.T) {
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		t.Fatal("no HTTP call expected when repo is empty")
		return nil, nil
	})
	results := checkOnboardingReadiness("http://localhost:8080", "fhk_t", "")
	if len(results) != 1 || results[0].status != "warn" {
		t.Fatalf("want a single warn when repo unresolved, got %+v", results)
	}
	if !strings.Contains(results[0].remediate, "--repo") {
		t.Errorf("remediate = %q, want a --repo hint", results[0].remediate)
	}
}

// TestCheckOnboardingReadiness_AllGreen asserts a fully-onboarded repo yields
// only ok rungs — and that the mirror struct decodes every field.
func TestCheckOnboardingReadiness_AllGreen(t *testing.T) {
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusOK, allGreenReadinessJSON), nil
	})
	results := checkOnboardingReadiness("http://localhost:8080", "fhk_t", "kuhlman-labs/fishhawk")
	for _, r := range results {
		if r.status != "ok" {
			t.Errorf("rung %q status = %q, want ok (detail: %s)", r.label, r.status, r.detail)
		}
	}
	// The reviewer rung proves the mirror struct decoded provider/model/available.
	rv := findCheck(t, results, "reviewer available: anthropic")
	if rv.detail != "claude-opus-4-8" {
		t.Errorf("reviewer detail = %q, want the model", rv.detail)
	}
	// The app rung proves installation_id decoded.
	app := findCheck(t, results, "app installed")
	if !strings.Contains(app.detail, "42") {
		t.Errorf("app detail = %q, want the installation id", app.detail)
	}
}

// writeExecPathSpec writes a workflows.yaml with the given body under
// <dir>/.fishhawk and returns dir.
func writeExecPathSpec(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	hidden := filepath.Join(dir, ".fishhawk")
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "workflows.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestCheckExecutionPath_WithExecutor asserts ok when every stage declares an
// executor.
func TestCheckExecutionPath_WithExecutor(t *testing.T) {
	dir := writeExecPathSpec(t, `
version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
      - id: implement
        type: implement
        executor:
          human: true
`)
	r := checkExecutionPath(dir)
	if r.status != "ok" {
		t.Errorf("status = %q, want ok (detail: %s, remediate: %s)", r.status, r.detail, r.remediate)
	}
}

// TestCheckExecutionPath_WithoutExecutor asserts fail when a stage declares no
// executor at all.
func TestCheckExecutionPath_WithoutExecutor(t *testing.T) {
	dir := writeExecPathSpec(t, `
version: "0.3"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
`)
	r := checkExecutionPath(dir)
	if r.status != "fail" {
		t.Errorf("status = %q, want fail (detail: %s)", r.status, r.detail)
	}
	if !strings.Contains(r.remediate, "implement") {
		t.Errorf("remediate = %q, want the unconfigured stage named", r.remediate)
	}
}

// TestCheckExecutionPath_MixedStages is the E29.5 binding-condition case: a
// workflow where some stages declare an executor and at least one does not
// must FAIL, and the remediation must name the unconfigured stage(s). Without
// this, doctor would pass and the run would wedge on the first unconfigured
// stage.
func TestCheckExecutionPath_MixedStages(t *testing.T) {
	dir := writeExecPathSpec(t, `
version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
      - id: implement
        type: implement
      - id: review
        type: review
`)
	r := checkExecutionPath(dir)
	if r.status != "fail" {
		t.Fatalf("status = %q, want fail for a mixed workflow (detail: %s)", r.status, r.detail)
	}
	// Both unconfigured stages must be named; the configured one must not
	// be listed as missing.
	if !strings.Contains(r.remediate, "implement") || !strings.Contains(r.remediate, "review") {
		t.Errorf("remediate = %q, want both unconfigured stages (implement, review) named", r.remediate)
	}
	if strings.Contains(r.remediate, "missing on: plan") ||
		strings.Contains(r.remediate, "plan,") || strings.Contains(r.remediate, ", plan") {
		t.Errorf("remediate = %q, must not list the configured stage 'plan' as missing", r.remediate)
	}
}

// TestCheckExecutionPath_NoSpec asserts warn when no spec is found (checkSpec
// is the authority on a hard-missing spec).
func TestCheckExecutionPath_NoSpec(t *testing.T) {
	r := checkExecutionPath(t.TempDir())
	if r.status != "warn" {
		t.Errorf("status = %q, want warn when no spec is found", r.status)
	}
}
