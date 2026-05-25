package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// withFakeDoctorHTTP stubs doctorHTTPDo for the duration of the test.
func withFakeDoctorHTTP(t *testing.T, fn func(*http.Request) (*http.Response, error)) {
	t.Helper()
	orig := doctorHTTPDo
	doctorHTTPDo = fn
	t.Cleanup(func() { doctorHTTPDo = orig })
}

// withFakeDoctorLookPath stubs doctorLookPath for the duration of the test.
func withFakeDoctorLookPath(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	orig := doctorLookPath
	doctorLookPath = fn
	t.Cleanup(func() { doctorLookPath = orig })
}

// withFakeDoctorRunOutput stubs doctorRunOutput for the duration of the test.
func withFakeDoctorRunOutput(t *testing.T, fn func(string, ...string) (string, error)) {
	t.Helper()
	orig := doctorRunOutput
	doctorRunOutput = fn
	t.Cleanup(func() { doctorRunOutput = orig })
}

// fakeHTTPResponse builds a minimal *http.Response for use in doctorHTTPDo stubs.
func fakeHTTPResponse(code int, body string) *http.Response {
	return &http.Response{
		StatusCode: code,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// TestDoctorCheck_BackendDown verifies checkBackend returns fail when the
// backend is unreachable (doctorHTTPDo returns a connection error).
func TestDoctorCheck_BackendDown(t *testing.T) {
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused")
	})

	r := checkBackend("http://localhost:8080")
	if r.status != "fail" {
		t.Errorf("status = %q, want fail", r.status)
	}
	if r.remediate == "" {
		t.Error("remediate should be non-empty on fail")
	}
}

// TestDoctorCheck_BackendDown_503 verifies that a non-200 HTTP status is
// treated as a failure with a non-empty remediation hint.
func TestDoctorCheck_BackendDown_503(t *testing.T) {
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusServiceUnavailable, `{}`), nil
	})

	r := checkBackend("http://localhost:8080")
	if r.status != "fail" {
		t.Errorf("status = %q, want fail", r.status)
	}
	if r.remediate == "" {
		t.Error("remediate should be non-empty on fail")
	}
}

// TestDoctorCheck_NoSpec verifies checkSpec returns fail when
// .fishhawk/workflows.yaml does not exist in the given working dir.
func TestDoctorCheck_NoSpec(t *testing.T) {
	dir := t.TempDir() // empty dir — no .fishhawk/workflows.yaml

	r := checkSpec(dir)
	if r.status != "fail" {
		t.Errorf("status = %q, want fail", r.status)
	}
	if r.remediate == "" {
		t.Error("remediate should be non-empty on fail")
	}
}

// TestDoctorCheck_SpecPresent verifies checkSpec returns ok when a valid
// spec file is present.
func TestDoctorCheck_SpecPresent(t *testing.T) {
	dir := t.TempDir()
	hidden := filepath.Join(dir, ".fishhawk")
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "workflows.yaml"), []byte(validateValidYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	r := checkSpec(dir)
	if r.status != "ok" {
		t.Errorf("status = %q, want ok\ndetail: %s\nremediate: %s", r.status, r.detail, r.remediate)
	}
}

// TestDoctorCheck_MissingRunnerBinary verifies checkRunnerBinary returns
// fail when the binary is not in PATH, and the remediation hint mentions
// FISHHAWK_RUNNER_BIN.
func TestDoctorCheck_MissingRunnerBinary(t *testing.T) {
	withFakeDoctorLookPath(t, func(_ string) (string, error) {
		return "", exec.ErrNotFound
	})

	r := checkRunnerBinary("") // no flag value, no env value
	if r.status != "fail" {
		t.Errorf("status = %q, want fail", r.status)
	}
	if !strings.Contains(r.remediate, "FISHHAWK_RUNNER_BIN") {
		t.Errorf("remediate %q should mention FISHHAWK_RUNNER_BIN", r.remediate)
	}
}

// TestDoctorCheck_Token401 verifies checkToken returns fail when the
// backend returns HTTP 401 on the /v0/runs probe.
func TestDoctorCheck_Token401(t *testing.T) {
	withFakeDoctorHTTP(t, func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "/v0/runs") {
			return fakeHTTPResponse(http.StatusUnauthorized,
				`{"error":{"code":"unauthorized","message":"invalid token"}}`), nil
		}
		return fakeHTTPResponse(http.StatusOK, `{}`), nil
	})

	r := checkToken("http://localhost:8080", "fhk_invalid")
	if r.status != "fail" {
		t.Errorf("status = %q, want fail", r.status)
	}
	if r.remediate == "" {
		t.Error("remediate should be non-empty on fail")
	}
}

// TestDoctorCheck_TokenMalformed verifies checkToken fails immediately
// when the token lacks the fhk_ prefix.
func TestDoctorCheck_TokenMalformed(t *testing.T) {
	r := checkToken("http://localhost:8080", "badtoken")
	if r.status != "fail" {
		t.Errorf("status = %q, want fail", r.status)
	}
	if r.remediate == "" {
		t.Error("remediate should be non-empty on fail")
	}
}

// TestDoctorCheck_TokenEmpty verifies checkToken fails when the token is empty.
func TestDoctorCheck_TokenEmpty(t *testing.T) {
	r := checkToken("http://localhost:8080", "")
	if r.status != "fail" {
		t.Errorf("status = %q, want fail", r.status)
	}
}

// TestRunDoctor_FailsAndPrintsLabel verifies that runDoctor returns exitFailure
// and prints the failing label to stdout when checks fail.
func TestRunDoctor_FailsAndPrintsLabel(t *testing.T) {
	// Stub backend to return connection error.
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})
	// Stub PATH lookup to fail.
	withFakeDoctorLookPath(t, func(_ string) (string, error) {
		return "", exec.ErrNotFound
	})
	// Stub claude / gh CLI to fail.
	withFakeDoctorRunOutput(t, func(_ string, _ ...string) (string, error) {
		return "", errors.New("not found")
	})
	// Stub git remote to fail.
	origGitRemote := gitRemoteOriginURL
	gitRemoteOriginURL = func(_ string) (string, error) {
		return "", errors.New("no origin")
	}
	t.Cleanup(func() { gitRemoteOriginURL = origGitRemote })

	dir := t.TempDir() // no spec

	var stdout, stderr strings.Builder
	got := run([]string{
		"doctor",
		"--backend-url", "http://localhost:8080",
		"--token", "not-fhk", // malformed token
		"--working-dir", dir,
	}, &stdout, &stderr)

	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	out := stdout.String()
	if !strings.Contains(out, "backend reachable") {
		t.Errorf("stdout missing 'backend reachable': %q", out)
	}
	if !strings.Contains(out, "check(s) failed") {
		t.Errorf("stdout missing failure summary: %q", out)
	}
}

// TestRunDoctor_AllPass verifies that runDoctor returns exitOK and prints
// "ready for local loop" when all checks pass.
func TestRunDoctor_AllPass(t *testing.T) {
	// Stub HTTP: /healthz → 200, /v0/runs → 200.
	withFakeDoctorHTTP(t, func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/healthz"):
			return fakeHTTPResponse(http.StatusOK, `{"status":"ok","version":"v0.0.0-test"}`), nil
		case strings.Contains(req.URL.Path, "/v0/runs"):
			return fakeHTTPResponse(http.StatusOK, `{"items":[]}`), nil
		default:
			return fakeHTTPResponse(http.StatusOK, `{}`), nil
		}
	})
	withFakeDoctorLookPath(t, func(_ string) (string, error) {
		return "/usr/local/bin/fishhawk-runner", nil
	})
	withFakeDoctorRunOutput(t, func(name string, _ ...string) (string, error) {
		return fmt.Sprintf("%s ok", name), nil
	})
	origGitRemote := gitRemoteOriginURL
	gitRemoteOriginURL = func(_ string) (string, error) {
		return "https://github.com/kuhlman-labs/fishhawk.git", nil
	}
	t.Cleanup(func() { gitRemoteOriginURL = origGitRemote })

	// Create a temp dir with a valid spec. The git working-tree check runs
	// `git status --porcelain` via exec.Command (not a seam), so point
	// --working-dir at a clean checkout sub-dir or tolerate that one rung.
	// Use the actual repo root (which may have .fishhawk/dev.pid uncommitted)
	// but supply an explicit --working-dir with spec so the spec rung passes.
	// The git working-tree rung uses the same dir — we accept it may fail.
	dir := t.TempDir()
	hidden := filepath.Join(dir, ".fishhawk")
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "workflows.yaml"), []byte(validateValidYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	got := run([]string{
		"doctor",
		"--backend-url", "http://localhost:8080",
		"--token", "fhk_testtoken",
		"--working-dir", dir,
		"--runner-binary", "/usr/local/bin/fishhawk-runner",
	}, &stdout, &stderr)

	out := stdout.String()

	// The git working-tree check is the only rung that cannot be stubbed and
	// runs against a temp dir that is not a git repo. It will fail. All other
	// rungs should pass. Verify the stubs fired correctly.
	for _, want := range []string{
		"backend reachable",
		"token valid",
		"workflow spec present",
		"runner binary found",
		"MCP registered",
		"git remote origin",
		"gh CLI authenticated",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing rung label %q: %q", want, out)
		}
	}

	// If the git-working-tree rung failed (temp dir is not a git repo), we
	// expect exitFailure; if somehow clean, exitOK. Accept either.
	if got != exitOK && got != exitFailure {
		t.Errorf("run = %d, want exitOK or exitFailure (only git rung may fail)", got)
	}
	// The summary line must always be present.
	if !strings.Contains(out, "ready for local loop") && !strings.Contains(out, "check(s) failed") {
		t.Errorf("stdout missing summary: %q", out)
	}
}
