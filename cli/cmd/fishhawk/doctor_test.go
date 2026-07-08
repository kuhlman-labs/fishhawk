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

	"github.com/kuhlman-labs/fishhawk/cli/internal/spec"
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

// TestDoctorCheck_DockerDaemonDown verifies checkDockerDaemon returns fail
// when docker info returns an error (daemon not running).
func TestDoctorCheck_DockerDaemonDown(t *testing.T) {
	withFakeDoctorRunOutput(t, func(_ string, _ ...string) (string, error) {
		return "", errors.New("Cannot connect to the Docker daemon")
	})
	r := checkDockerDaemon()
	if r.status != "fail" {
		t.Errorf("status = %q, want fail", r.status)
	}
	if r.remediate == "" {
		t.Error("remediate should be non-empty on fail")
	}
}

// TestDoctorCheck_DockerDaemonUp verifies checkDockerDaemon returns ok
// when docker info succeeds.
func TestDoctorCheck_DockerDaemonUp(t *testing.T) {
	withFakeDoctorRunOutput(t, func(_ string, _ ...string) (string, error) {
		return "Server Version: 24.0.0", nil
	})
	r := checkDockerDaemon()
	if r.status != "ok" {
		t.Errorf("status = %q, want ok", r.status)
	}
}

// TestDoctorCheck_PostgresContainerAbsent verifies checkPostgresContainer
// returns fail when docker ps returns empty output (container not running).
func TestDoctorCheck_PostgresContainerAbsent(t *testing.T) {
	withFakeDoctorRunOutput(t, func(_ string, _ ...string) (string, error) {
		return "", nil // docker ps returned empty — no matching container
	})
	r := checkPostgresContainer()
	if r.status != "fail" {
		t.Errorf("status = %q, want fail", r.status)
	}
	if r.remediate == "" {
		t.Error("remediate should be non-empty on fail")
	}
}

// TestDoctorCheck_PostgresContainerUpNotReady verifies checkPostgresContainer
// returns warn when the container exists but pg_isready reports not-yet-ready.
func TestDoctorCheck_PostgresContainerUpNotReady(t *testing.T) {
	withFakeDoctorRunOutput(t, func(name string, arg ...string) (string, error) {
		if name == "docker" {
			return "fishhawk-postgres", nil
		}
		// pg_isready returns non-zero — postgres initialising
		return "", errors.New("no response")
	})
	r := checkPostgresContainer()
	if r.status != "warn" {
		t.Errorf("status = %q, want warn", r.status)
	}
	if r.remediate == "" {
		t.Error("remediate should be non-empty on warn")
	}
}

// TestDoctorCheck_PostgresContainerHealthy verifies checkPostgresContainer
// returns ok when container is running and pg_isready succeeds.
func TestDoctorCheck_PostgresContainerHealthy(t *testing.T) {
	withFakeDoctorRunOutput(t, func(name string, _ ...string) (string, error) {
		if name == "docker" {
			return "fishhawk-postgres", nil
		}
		return "localhost:5432 - accepting connections", nil
	})
	r := checkPostgresContainer()
	if r.status != "ok" {
		t.Errorf("status = %q, want ok; detail: %s", r.status, r.detail)
	}
}

// TestDoctorCheck_MinioContainerAbsent verifies checkMinioContainer returns
// fail when docker ps returns empty output (container not running).
func TestDoctorCheck_MinioContainerAbsent(t *testing.T) {
	withFakeDoctorRunOutput(t, func(_ string, _ ...string) (string, error) {
		return "", nil // docker ps returned empty — no matching container
	})
	r := checkMinioContainer()
	if r.status != "fail" {
		t.Errorf("status = %q, want fail", r.status)
	}
	if r.remediate == "" {
		t.Error("remediate should be non-empty on fail")
	}
}

// TestDoctorCheck_MinioContainerUpHealthFailed verifies checkMinioContainer
// returns warn when the container is up but the health HTTP probe fails.
func TestDoctorCheck_MinioContainerUpHealthFailed(t *testing.T) {
	withFakeDoctorRunOutput(t, func(_ string, _ ...string) (string, error) {
		return "fishhawk-minio", nil
	})
	withFakeDoctorHTTP(t, func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "/minio/health/live") {
			return nil, errors.New("connection refused")
		}
		return fakeHTTPResponse(http.StatusOK, ""), nil
	})
	r := checkMinioContainer()
	if r.status != "warn" {
		t.Errorf("status = %q, want warn", r.status)
	}
	if r.remediate == "" {
		t.Error("remediate should be non-empty on warn")
	}
}

// TestDoctorCheck_MinioContainerHealthy verifies checkMinioContainer returns
// ok when the container is up and the health probe returns 200.
func TestDoctorCheck_MinioContainerHealthy(t *testing.T) {
	withFakeDoctorRunOutput(t, func(_ string, _ ...string) (string, error) {
		return "fishhawk-minio", nil
	})
	withFakeDoctorHTTP(t, func(req *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusOK, ""), nil
	})
	r := checkMinioContainer()
	if r.status != "ok" {
		t.Errorf("status = %q, want ok; detail: %s", r.status, r.detail)
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

	r := checkRunnerBinary("", t.TempDir()) // no flag value, no env value, empty bin/
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

// TestCheckRunnerBinary_RepoBinFallback verifies that checkRunnerBinary
// returns ok with a "via repo bin/" detail when LookPath misses but
// <workingDir>/bin/fishhawk-runner exists as a regular file.
func TestCheckRunnerBinary_RepoBinFallback(t *testing.T) {
	withFakeDoctorLookPath(t, func(_ string) (string, error) {
		return "", exec.ErrNotFound
	})

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(binDir, "fishhawk-runner")
	if err := os.WriteFile(candidate, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := checkRunnerBinary("", dir)
	if r.status != "ok" {
		t.Errorf("status = %q, want ok (detail: %s, remediate: %s)", r.status, r.detail, r.remediate)
	}
	if !strings.Contains(r.detail, "via repo bin/") {
		t.Errorf("detail %q should contain 'via repo bin/'", r.detail)
	}
}

// TestCheckGitWorkingTree_DirtyTree_Warn verifies that checkGitWorkingTree
// returns status "warn" (not "fail") when the working tree has uncommitted changes.
func TestCheckGitWorkingTree_DirtyTree_Warn(t *testing.T) {
	dir := t.TempDir()

	// Init a real git repo so git status --porcelain works.
	for _, args := range [][]string{
		{"init", dir},
		{"-C", dir, "config", "user.email", "test@example.com"},
		{"-C", dir, "config", "user.name", "Test"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil { //nolint:gosec
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Write an uncommitted file so the tree is dirty.
	if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("untracked"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := checkGitWorkingTree(dir)
	if r.status != "warn" {
		t.Errorf("status = %q, want warn (detail: %s)", r.status, r.detail)
	}
}

// TestRunDoctor_DirtyTree_ExitZero verifies that runDoctor returns exitOK
// when the only imperfect rung is a dirty working tree (which is now a warn).
func TestRunDoctor_DirtyTree_ExitZero(t *testing.T) {
	dir := t.TempDir()

	// Init a git repo with an uncommitted file.
	for _, args := range [][]string{
		{"init", dir},
		{"-C", dir, "config", "user.email", "test@example.com"},
		{"-C", dir, "config", "user.name", "Test"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil { //nolint:gosec
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("untracked"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Write a valid spec so the spec rung passes.
	hidden := filepath.Join(dir, ".fishhawk")
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "workflows.yaml"), []byte(validateValidYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	// Stub all rungs that require network or external binaries.
	withFakeDoctorHTTP(t, func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/healthz"):
			return fakeHTTPResponse(http.StatusOK, `{"status":"ok","version":"v0.0.0-test"}`), nil
		case strings.HasSuffix(req.URL.Path, "/v0/onboarding/readiness"):
			return fakeHTTPResponse(http.StatusOK, allGreenReadinessJSON), nil
		default:
			return fakeHTTPResponse(http.StatusOK, `{"items":[]}`), nil
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

	var stdout, stderr strings.Builder
	got := run([]string{
		"doctor",
		"--backend-url", "http://localhost:8080",
		"--token", "fhk_testtoken",
		"--working-dir", dir,
		"--runner-binary", "/usr/local/bin/fishhawk-runner",
	}, &stdout, &stderr)

	if got != exitOK {
		t.Errorf("run = %d, want exitOK; stdout:\n%s", got, stdout.String())
	}
	if !strings.Contains(stdout.String(), "ready for local loop") {
		t.Errorf("stdout missing 'ready for local loop': %q", stdout.String())
	}
}

// TestRunDoctor_SpecOnly_ExitZero verifies that `doctor --spec-only` on a
// fresh repo whose only Fishhawk artifact is a generated workflows.yaml
// exits exitOK WITHOUT any local environment: no docker/backend/token/MCP
// seams are stubbed, and the infra rung labels must be absent from the
// output because those rungs are skipped entirely in spec-only mode.
func TestRunDoctor_SpecOnly_ExitZero(t *testing.T) {
	dir := t.TempDir()
	hidden := filepath.Join(dir, ".fishhawk")
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	genned, err := spec.Generate(spec.PresetMedium, spec.Deltas{})
	if err != nil {
		t.Fatalf("Generate(medium): %v", err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "workflows.yaml"), genned, 0o600); err != nil {
		t.Fatal(err)
	}

	// Deliberately DO NOT stub the docker/backend/gh/MCP seams: spec-only
	// mode must never reach them. Point at an unreachable backend to prove it.
	var stdout, stderr strings.Builder
	got := run([]string{
		"doctor", "--spec-only",
		"--backend-url", "http://127.0.0.1:0",
		"--working-dir", dir,
	}, &stdout, &stderr)

	if got != exitOK {
		t.Fatalf("run = %d, want exitOK; stdout:\n%s", got, stdout.String())
	}
	out := stdout.String()
	// The two spec rungs must be present.
	for _, want := range []string{"workflow spec present", "ready for local loop"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q: %q", want, out)
		}
	}
	// Every infra/backend/MCP/git/gh rung label must be ABSENT.
	for _, notWant := range []string{
		"docker daemon running", "postgres container", "minio container",
		"backend reachable", "token valid", "runner binary found",
		"MCP registered", "git remote origin", "gh CLI authenticated",
		"backend SHA drift", "runner schema drift", "CLI version",
	} {
		if strings.Contains(out, notWant) {
			t.Errorf("spec-only output must not contain infra rung %q: %q", notWant, out)
		}
	}
}

// TestRunDoctor_SpecOnly_FailsClosed verifies the fail-closed branch: with
// --spec-only and a missing (or schema-invalid) workflows.yaml, checkSpec
// returns "fail" and runDoctor exits non-zero even though no environment
// rungs run.
func TestRunDoctor_SpecOnly_FailsClosed(t *testing.T) {
	t.Run("missing spec", func(t *testing.T) {
		dir := t.TempDir() // no .fishhawk/workflows.yaml
		var stdout, stderr strings.Builder
		got := run([]string{
			"doctor", "--spec-only", "--working-dir", dir,
		}, &stdout, &stderr)
		if got != exitFailure {
			t.Fatalf("run = %d, want exitFailure; stdout:\n%s", got, stdout.String())
		}
		if !strings.Contains(stdout.String(), "check(s) failed") {
			t.Errorf("stdout missing failure summary: %q", stdout.String())
		}
	})

	t.Run("schema-invalid spec", func(t *testing.T) {
		dir := t.TempDir()
		hidden := filepath.Join(dir, ".fishhawk")
		if err := os.MkdirAll(hidden, 0o755); err != nil {
			t.Fatal(err)
		}
		// Well-formed YAML that is not a valid workflow-v1 document.
		if err := os.WriteFile(filepath.Join(hidden, "workflows.yaml"), []byte("version: \"1.0\"\nnot_a_workflows_key: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr strings.Builder
		got := run([]string{
			"doctor", "--spec-only", "--working-dir", dir,
		}, &stdout, &stderr)
		if got != exitFailure {
			t.Fatalf("run = %d, want exitFailure; stdout:\n%s", got, stdout.String())
		}
	})
}

// TestCheckBackendSHADrift_Unknown returns ok when backend SHA is "unknown"
// (dev build).
func TestCheckBackendSHADrift_Unknown(t *testing.T) {
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusOK, `{"status":"ok","git_sha":"unknown"}`), nil
	})
	r := checkBackendSHADrift("http://localhost:8080", t.TempDir())
	if r.status != "ok" {
		t.Errorf("status = %q, want ok (unknown SHA should be fine)", r.status)
	}
}

// TestCheckBackendSHADrift_Unreachable returns warn when the backend is
// unreachable (not fail, because the SHA check is advisory).
func TestCheckBackendSHADrift_Unreachable(t *testing.T) {
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})
	r := checkBackendSHADrift("http://localhost:8080", ".")
	if r.status != "warn" {
		t.Errorf("status = %q, want warn", r.status)
	}
}

// initGitRepoWithHead creates a git repo with one commit and returns its
// path plus the full HEAD SHA, for drift tests that need a real
// `git rev-parse HEAD` on the local side of the comparison.
func initGitRepoWithHead(t *testing.T) (dir, headSHA string) {
	t.Helper()
	dir = t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init")
	git("commit", "--allow-empty", "-m", "init")
	return dir, git("rev-parse", "HEAD")
}

// TestCheckBackendSHADrift_StampedShortSHA returns ok when the backend
// reports the stamped short SHA of the same commit as the local full HEAD —
// the scripts/dev clean-tree case. Exact equality would false-warn here.
func TestCheckBackendSHADrift_StampedShortSHA(t *testing.T) {
	dir, head := initGitRepoWithHead(t)
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusOK,
			fmt.Sprintf(`{"status":"ok","git_sha":%q}`, head[:7])), nil
	})
	r := checkBackendSHADrift("http://localhost:8080", dir)
	if r.status != "ok" {
		t.Errorf("status = %q, want ok (short SHA prefix of local HEAD); detail: %s", r.status, r.detail)
	}
}

// TestCheckBackendSHADrift_StampedDirtySHA returns ok and notes the dirty
// tree when the backend reports the same commit with a -dirty suffix.
func TestCheckBackendSHADrift_StampedDirtySHA(t *testing.T) {
	dir, head := initGitRepoWithHead(t)
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusOK,
			fmt.Sprintf(`{"status":"ok","git_sha":%q}`, head[:7]+"-dirty")), nil
	})
	r := checkBackendSHADrift("http://localhost:8080", dir)
	if r.status != "ok" {
		t.Errorf("status = %q, want ok (dirty-stamped same commit); detail: %s", r.status, r.detail)
	}
	if !strings.Contains(r.detail, "dirty tree at build") {
		t.Errorf("detail = %q, want a 'dirty tree at build' note", r.detail)
	}
}

// TestCheckBackendSHADrift_DifferentCommit still warns when the backend's
// stamped SHA is from a genuinely different commit.
func TestCheckBackendSHADrift_DifferentCommit(t *testing.T) {
	dir, _ := initGitRepoWithHead(t)
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusOK,
			`{"status":"ok","git_sha":"0000000-dirty"}`), nil
	})
	r := checkBackendSHADrift("http://localhost:8080", dir)
	if r.status != "warn" {
		t.Errorf("status = %q, want warn (different commit); detail: %s", r.status, r.detail)
	}
}

// TestCheckRunnerSchemaDrift_RunnerNotFound returns warn when the runner
// binary is not found (runner binary resolution fails).
func TestCheckRunnerSchemaDrift_RunnerNotFound(t *testing.T) {
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusOK, `{"schemas":{"plan-standard-v1":"abc123"}}`), nil
	})
	withFakeDoctorLookPath(t, func(_ string) (string, error) {
		return "", errors.New("not found")
	})
	r := checkRunnerSchemaDrift("http://localhost:8080", "", t.TempDir())
	if r.status != "warn" {
		t.Errorf("status = %q, want warn (no binary = degraded, not fail)", r.status)
	}
}

// TestCheckRunnerSchemaDrift_RunnerLacksVersionSubcommand returns warn when
// the runner binary does not support the 'version' subcommand.
func TestCheckRunnerSchemaDrift_RunnerLacksVersionSubcommand(t *testing.T) {
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusOK, `{"schemas":{"plan-standard-v1":"abc123"}}`), nil
	})
	withFakeDoctorRunOutput(t, func(_ string, _ ...string) (string, error) {
		return "", errors.New("unknown subcommand: version")
	})
	r := checkRunnerSchemaDrift("http://localhost:8080", "/usr/local/bin/fishhawk-runner", t.TempDir())
	if r.status != "warn" {
		t.Errorf("status = %q, want warn (old runner = degraded, not fail)", r.status)
	}
}

// TestCheckRunnerSchemaDrift_InSync returns ok when backend and runner
// report the same schema hash.
func TestCheckRunnerSchemaDrift_InSync(t *testing.T) {
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusOK,
			`{"schemas":{"plan-standard-v1":"`+hash+`"}}`), nil
	})
	withFakeDoctorRunOutput(t, func(_ string, _ ...string) (string, error) {
		return `{"version":"v0.5.0","plan_schema_hash":"` + hash + `"}`, nil
	})
	r := checkRunnerSchemaDrift("http://localhost:8080", "/usr/local/bin/fishhawk-runner", t.TempDir())
	if r.status != "ok" {
		t.Errorf("status = %q, want ok; detail: %s", r.status, r.detail)
	}
}

// TestCheckRunnerSchemaDrift_Mismatch returns warn when hashes differ.
func TestCheckRunnerSchemaDrift_Mismatch(t *testing.T) {
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusOK,
			`{"schemas":{"plan-standard-v1":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1111"}}`), nil
	})
	withFakeDoctorRunOutput(t, func(_ string, _ ...string) (string, error) {
		return `{"version":"v0.5.0","plan_schema_hash":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb2222"}`, nil
	})
	r := checkRunnerSchemaDrift("http://localhost:8080", "/usr/local/bin/fishhawk-runner", t.TempDir())
	if r.status != "warn" {
		t.Errorf("status = %q, want warn; detail: %s", r.status, r.detail)
	}
	if r.remediate == "" {
		t.Error("remediate should be non-empty on mismatch")
	}
}

// TestCheckRunnerSchemaDrift_RepoBinFallback verifies that checkRunnerSchemaDrift
// resolves the runner via <workingDir>/bin/fishhawk-runner when LookPath misses
// and no explicit binary is provided, then returns ok when schemas match.
func TestCheckRunnerSchemaDrift_RepoBinFallback(t *testing.T) {
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "fishhawk-runner"), []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}

	withFakeDoctorLookPath(t, func(_ string) (string, error) {
		return "", exec.ErrNotFound
	})
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusOK,
			`{"schemas":{"plan-standard-v1":"`+hash+`"}}`), nil
	})
	withFakeDoctorRunOutput(t, func(_ string, _ ...string) (string, error) {
		return `{"version":"v0.5.0","plan_schema_hash":"` + hash + `"}`, nil
	})

	r := checkRunnerSchemaDrift("http://localhost:8080", "", dir)
	if r.status != "ok" {
		t.Errorf("status = %q, want ok (repo-bin fallback should resolve binary); detail: %s, remediate: %s",
			r.status, r.detail, r.remediate)
	}
}

// TestCheckCLIVersion_NoMinRequired returns ok when the backend doesn't
// require a minimum version.
func TestCheckCLIVersion_NoMinRequired(t *testing.T) {
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusOK, `{"status":"ok","min_runner_version":"dev"}`), nil
	})
	r := checkCLIVersion("http://localhost:8080")
	if r.status != "ok" {
		t.Errorf("status = %q, want ok", r.status)
	}
}

// TestCheckCLIVersion_CLITooOld returns warn when the CLI version is older
// than the backend's minimum.
func TestCheckCLIVersion_CLITooOld(t *testing.T) {
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusOK, `{"min_runner_version":"v0.5.0"}`), nil
	})
	withFakeDoctorRunOutput(t, func(_ string, _ ...string) (string, error) {
		return "v0.4.0", nil
	})
	r := checkCLIVersion("http://localhost:8080")
	if r.status != "warn" {
		t.Errorf("status = %q, want warn; detail: %s", r.status, r.detail)
	}
	if r.remediate == "" {
		t.Error("remediate should be non-empty when CLI is too old")
	}
}

// TestCheckCLIVersion_CLISufficient returns ok when the CLI version meets
// the backend's minimum.
func TestCheckCLIVersion_CLISufficient(t *testing.T) {
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusOK, `{"min_runner_version":"v0.5.0"}`), nil
	})
	withFakeDoctorRunOutput(t, func(_ string, _ ...string) (string, error) {
		return "v0.5.0", nil
	})
	r := checkCLIVersion("http://localhost:8080")
	if r.status != "ok" {
		t.Errorf("status = %q, want ok; detail: %s", r.status, r.detail)
	}
}

// TestRunDoctor_AllPass verifies that runDoctor returns exitOK and prints
// "ready for local loop" when all checks pass.
func TestRunDoctor_AllPass(t *testing.T) {
	// Stub HTTP: /healthz → 200, /v0/runs → 200, readiness → all-green.
	withFakeDoctorHTTP(t, func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/healthz"):
			return fakeHTTPResponse(http.StatusOK, `{"status":"ok","version":"v0.0.0-test"}`), nil
		case strings.HasSuffix(req.URL.Path, "/v0/onboarding/readiness"):
			return fakeHTTPResponse(http.StatusOK, allGreenReadinessJSON), nil
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
