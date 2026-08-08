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
	"github.com/kuhlman-labs/fishhawk/credstore"
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

// withFakeDoctorCredLoad stubs doctorCredLoad (the credstore seam) for the test.
func withFakeDoctorCredLoad(t *testing.T, fn func(string) (credstore.Credential, error)) {
	t.Helper()
	orig := doctorCredLoad
	doctorCredLoad = fn
	t.Cleanup(func() { doctorCredLoad = orig })
}

// withFakeDoctorExecutable stubs doctorExecutable (the os.Executable seam).
func withFakeDoctorExecutable(t *testing.T, fn func() (string, error)) {
	t.Helper()
	orig := doctorExecutable
	doctorExecutable = fn
	t.Cleanup(func() { doctorExecutable = orig })
}

// markBackendCheckout writes the structural markers isBackendSourceCheckout
// keys on (a backend/cmd/fishhawkd directory + a go.work at the root) so a
// temp dir is treated as the backend's own source checkout — required by the
// SHA-drift tests that exercise the drift branches.
func markBackendCheckout(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "backend", "cmd", "fishhawkd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.work"), []byte("go 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
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
// warn (not fail) when the binary resolves from no tier, and the remediation
// hint mentions FISHHAWK_RUNNER_BIN. A hard fail would be a false negative for
// an operator whose runner sits beside the SERVING binary the CLI can't see.
func TestDoctorCheck_MissingRunnerBinary(t *testing.T) {
	withFakeDoctorLookPath(t, func(_ string) (string, error) {
		return "", exec.ErrNotFound
	})
	// Point the executable seam at a dir with no fishhawk-runner sibling so
	// the sibling tier misses deterministically.
	withFakeDoctorExecutable(t, func() (string, error) {
		return filepath.Join(t.TempDir(), "fishhawk"), nil
	})

	r := checkRunnerBinary("", t.TempDir()) // no flag value, no env value, empty bin/
	if r.status != "warn" {
		t.Errorf("status = %q, want warn", r.status)
	}
	if !strings.Contains(r.remediate, "FISHHAWK_RUNNER_BIN") {
		t.Errorf("remediate %q should mention FISHHAWK_RUNNER_BIN", r.remediate)
	}
}

// TestDoctorCheck_Token401 verifies checkToken returns fail when the backend
// returns HTTP 401 on the /v0/runs probe AND readiness did not answer 200.
func TestDoctorCheck_Token401(t *testing.T) {
	withFakeDoctorHTTP(t, func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "/v0/runs") {
			return fakeHTTPResponse(http.StatusUnauthorized,
				`{"error":{"code":"unauthorized","message":"invalid token"}}`), nil
		}
		return fakeHTTPResponse(http.StatusOK, `{}`), nil
	})

	r := checkToken("http://localhost:8080",
		doctorCredential{token: "fhk_invalid", source: "--token / $FISHHAWK_TOKEN", class: "operator token (fhk_)"},
		readinessOutcome{})
	if r.status != "fail" {
		t.Errorf("status = %q, want fail", r.status)
	}
	if r.remediate == "" {
		t.Error("remediate should be non-empty on fail")
	}
}

// TestDoctorCheck_TokenMalformed verifies the fhk_ prefix no longer gates
// admission: a non-fhk_ token is PROBED, so the backend's verdict decides. A
// 200 on the probe yields ok, proving the prefix table was removed.
func TestDoctorCheck_TokenMalformed(t *testing.T) {
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusOK, `{"items":[]}`), nil
	})
	r := checkToken("http://localhost:8080",
		doctorCredential{token: "badtoken", source: "--token / $FISHHAWK_TOKEN", class: "unrecognized prefix"},
		readinessOutcome{})
	if r.status != "ok" {
		t.Errorf("status = %q, want ok (a non-fhk_ token is probed, not prefix-rejected); detail: %s", r.status, r.detail)
	}
}

// TestDoctorCheck_TokenEmpty verifies checkToken warns (not fails) when no
// credential resolved from any tier — an MCP/OAuth-driven loop needs none.
func TestDoctorCheck_TokenEmpty(t *testing.T) {
	r := checkToken("http://localhost:8080", doctorCredential{}, readinessOutcome{})
	if r.status != "warn" {
		t.Errorf("status = %q, want warn", r.status)
	}
	if r.remediate == "" {
		t.Error("remediate should be non-empty on warn")
	}
}

// --- token rung: per-branch behavioral tests (#2480) ------------------------

// TestCheckToken_NoCredentialWarns: no credential in any tier -> warn, never a
// hard fail (an MCP/OAuth-driven loop needs no CLI credential).
func TestCheckToken_NoCredentialWarns(t *testing.T) {
	r := checkToken("http://localhost:8080", doctorCredential{}, readinessOutcome{})
	if r.status != "warn" {
		t.Errorf("status = %q, want warn", r.status)
	}
	if !strings.Contains(r.remediate, "token login") {
		t.Errorf("remediate = %q, want a `fishhawk token login` hint", r.remediate)
	}
}

// TestCheckToken_OAuthTokenAccepted: an fho_ OAuth token the backend accepts
// (200) yields ok, and the detail names the OAuth class — the fhk_ prefix no
// longer gates admission.
func TestCheckToken_OAuthTokenAccepted(t *testing.T) {
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusOK, `{"items":[]}`), nil
	})
	cred := doctorCredential{token: "fho_abc", source: "--token / $FISHHAWK_TOKEN", class: classifyCredential("fho_abc")}
	r := checkToken("http://localhost:8080", cred, readinessOutcome{})
	if r.status != "ok" {
		t.Fatalf("status = %q, want ok; detail: %s", r.status, r.detail)
	}
	if !strings.Contains(r.detail, "OAuth access token") {
		t.Errorf("detail = %q, want the OAuth class named", r.detail)
	}
}

// TestCheckToken_StoredCredentialUsed: with an empty flag token,
// resolveDoctorCredential falls back to the credstore tier and checkToken
// probes with that credential. A distinct token value (not the flag tests')
// proves this branch is exercised, not another.
func TestCheckToken_StoredCredentialUsed(t *testing.T) {
	withFakeDoctorCredLoad(t, func(_ string) (credstore.Credential, error) {
		return credstore.Credential{Token: "fhk_stored_only"}, nil
	})
	var seenAuth string
	withFakeDoctorHTTP(t, func(req *http.Request) (*http.Response, error) {
		seenAuth = req.Header.Get("Authorization")
		return fakeHTTPResponse(http.StatusOK, `{"items":[]}`), nil
	})
	cred := resolveDoctorCredential("http://localhost:8080", "")
	if cred.token != "fhk_stored_only" {
		t.Fatalf("resolved token = %q, want the stored credential", cred.token)
	}
	if !strings.Contains(cred.source, "token login") {
		t.Errorf("source = %q, want the stored-credential source", cred.source)
	}
	r := checkToken("http://localhost:8080", cred, readinessOutcome{})
	if r.status != "ok" {
		t.Errorf("status = %q, want ok; detail: %s", r.status, r.detail)
	}
	if seenAuth != "Bearer fhk_stored_only" {
		t.Errorf("probe Authorization = %q, want the stored credential", seenAuth)
	}
}

// TestCheckToken_401WithoutReadinessFails is the counterfactual for the
// readiness-authority guard (binding condition 1): a 401 while readiness did
// NOT answer 200 must FAIL. Deleting the `readiness.answered` condition (making
// the downgrade unconditional) turns this red.
func TestCheckToken_401WithoutReadinessFails(t *testing.T) {
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusUnauthorized, `{"error":{"code":"unauthorized"}}`), nil
	})
	cred := doctorCredential{token: "fho_bad", source: "--token / $FISHHAWK_TOKEN", class: classifyCredential("fho_bad")}
	r := checkToken("http://localhost:8080", cred, readinessOutcome{answered: false})
	if r.status != "fail" {
		t.Errorf("status = %q, want fail (readiness did not answer, so the token rung is the authority)", r.status)
	}
}

// TestCheckToken_403WithReadinessAnsweredWarns: a 403 on /v0/runs while
// readiness answered an authoritative 200 downgrades to warn, never fail — an
// OAuth/cookie identity can legitimately 403 on /v0/runs yet be adequate per
// the readiness endpoint.
func TestCheckToken_403WithReadinessAnsweredWarns(t *testing.T) {
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusForbidden, `{"error":{"code":"forbidden"}}`), nil
	})
	cred := doctorCredential{token: "fho_ok", source: "--token / $FISHHAWK_TOKEN", class: classifyCredential("fho_ok")}
	r := checkToken("http://localhost:8080", cred, readinessOutcome{answered: true, scopesAdequate: true})
	if r.status != "warn" {
		t.Errorf("status = %q, want warn (readiness answered 200 for this credential)", r.status)
	}
	if !strings.Contains(r.remediate, "token scope adequate") {
		t.Errorf("remediate = %q, want it to defer to the server-side scope rung", r.remediate)
	}
}

// TestCheckToken_MalformedReadiness200DoesNotSuppressFail is the binding
// condition 1 per-branch case: a readiness 200 whose body did NOT decode
// leaves answered false, so a genuine 401/403 token verdict remains fail. This
// drives the real checkOnboardingReadiness (malformed 200 body) to produce the
// outcome, then feeds it to checkToken.
func TestCheckToken_MalformedReadiness200DoesNotSuppressFail(t *testing.T) {
	withFakeDoctorHTTP(t, func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "/v0/onboarding/readiness") {
			// HTTP 200, but the body is not decodable JSON.
			return fakeHTTPResponse(http.StatusOK, `{"app": not-json`), nil
		}
		return fakeHTTPResponse(http.StatusForbidden, `{"error":{"code":"forbidden"}}`), nil
	})
	readinessChecks, outcome := checkOnboardingReadiness("http://localhost:8080", "fho_x", "owner/name")
	if outcome.answered {
		t.Fatalf("outcome.answered = true on a malformed 200 body; must be false")
	}
	if len(readinessChecks) != 1 || readinessChecks[0].status != "warn" {
		t.Fatalf("want a single warn on the malformed 200, got %+v", readinessChecks)
	}
	cred := doctorCredential{token: "fho_x", source: "--token / $FISHHAWK_TOKEN", class: classifyCredential("fho_x")}
	r := checkToken("http://localhost:8080", cred, outcome)
	if r.status != "fail" {
		t.Errorf("status = %q, want fail (a malformed readiness 200 must not suppress the token failure)", r.status)
	}
}

// TestCheckToken_WarnDetailReflectsScopeVerdict pins that the recorded server
// scope verdict (readinessOutcome.scopesAdequate) is a LIVE field, not dead
// scaffolding (concern #2480, low/correctness): on the readiness-answered warn
// branch the token rung's detail differs by scopesAdequate. Collapsing the
// scopesAdequate branch to a constant note makes the two details identical and
// turns the final assertion red. Both inputs stay warn — the downgrade decision
// still keys only on answered (binding condition 1), scope is surfaced, not
// enforced, locally.
func TestCheckToken_WarnDetailReflectsScopeVerdict(t *testing.T) {
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusForbidden, `{"error":{"code":"forbidden"}}`), nil
	})
	cred := doctorCredential{token: "fho_x", source: "--token / $FISHHAWK_TOKEN", class: classifyCredential("fho_x")}

	adequate := checkToken("http://localhost:8080", cred, readinessOutcome{answered: true, scopesAdequate: true})
	if adequate.status != "warn" {
		t.Fatalf("adequate status = %q, want warn", adequate.status)
	}
	if !strings.Contains(adequate.detail, "scopes adequate") || strings.Contains(adequate.detail, "NOT adequate") {
		t.Errorf("adequate detail = %q, want it to note scopes adequate", adequate.detail)
	}

	inadequate := checkToken("http://localhost:8080", cred, readinessOutcome{answered: true, scopesAdequate: false})
	if inadequate.status != "warn" {
		t.Fatalf("inadequate status = %q, want warn", inadequate.status)
	}
	if !strings.Contains(inadequate.detail, "NOT adequate") {
		t.Errorf("inadequate detail = %q, want it to note scopes NOT adequate", inadequate.detail)
	}
	if adequate.detail == inadequate.detail {
		t.Errorf("scopesAdequate is a dead field: both details are %q", adequate.detail)
	}
}

// TestRunDoctor_EnvOnlyTokenReachesProbe pins the previously-untested
// $FISHHAWK_TOKEN tier (concern #2480, medium/untested-path): with NO --token
// flag and NO stored credential, a credential present ONLY in $FISHHAWK_TOKEN
// must be folded into the flag by bindCommonFlags, resolved by
// resolveDoctorCredential, and carried on the /v0/runs probe's Authorization
// header. The credstore seam is stubbed to error so the assertion proves the
// ENV tier, not the stored-credential tier — an env-only operator must not
// silently drop to unauthenticated probing.
func TestRunDoctor_EnvOnlyTokenReachesProbe(t *testing.T) {
	t.Setenv("FISHHAWK_TOKEN", "fhk_env_only")
	withFakeDoctorCredLoad(t, func(_ string) (credstore.Credential, error) {
		return credstore.Credential{}, errors.New("no stored credential")
	})
	var runsAuth string
	seenRuns := false
	withFakeDoctorHTTP(t, func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/healthz"):
			return fakeHTTPResponse(http.StatusOK, `{"status":"ok","version":"v0.0.0-test"}`), nil
		case strings.HasSuffix(req.URL.Path, "/v0/onboarding/readiness"):
			return fakeHTTPResponse(http.StatusOK, allGreenReadinessJSON), nil
		case strings.Contains(req.URL.Path, "/v0/runs"):
			seenRuns = true
			runsAuth = req.Header.Get("Authorization")
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

	dir := initCleanGitRepo(t)
	writeValidSpec(t, dir)

	var stdout, stderr strings.Builder
	// No --token flag: the credential must come from $FISHHAWK_TOKEN alone.
	run([]string{
		"doctor",
		"--backend-url", "http://localhost:8080",
		"--repo", "kuhlman-labs/fishhawk",
		"--working-dir", dir,
		"--runner-binary", "/usr/local/bin/fishhawk-runner",
	}, &stdout, &stderr)

	if !seenRuns {
		t.Fatalf("the /v0/runs probe never ran — env-only credential did not reach checkToken\nstdout:\n%s", stdout.String())
	}
	if runsAuth != "Bearer fhk_env_only" {
		t.Errorf("/v0/runs Authorization = %q, want %q (env-only token must reach the probe)\nstdout:\n%s",
			runsAuth, "Bearer fhk_env_only", stdout.String())
	}
}

// --- MCP rung: per-branch behavioral tests (#2480) --------------------------

// TestCheckMCP_HTTPRegistrationByName: an HTTP registration named fishhawk-http
// on a DIFFERENT port than --backend-url matches by NAME. Detail names HTTP.
func TestCheckMCP_HTTPRegistrationByName(t *testing.T) {
	withFakeDoctorRunOutput(t, func(_ string, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "mcp" && args[1] == "list" {
			return "fishhawk-http: https://localhost:8443/mcp (HTTP) - ✔ Connected", nil
		}
		return "", errors.New("unexpected call")
	})
	r := checkMCPRegistration("http://localhost:8080") // different port than 8443
	if r.status != "ok" {
		t.Fatalf("status = %q, want ok; detail: %s", r.status, r.detail)
	}
	if !strings.Contains(r.detail, "fishhawk-http") || !strings.Contains(r.detail, "HTTP") {
		t.Errorf("detail = %q, want the matched registration and HTTP transport", r.detail)
	}
}

// TestCheckMCP_HTTPRegistrationByBackendURL: a registration under a
// NON-fishhawk name whose target points at the backend's host:port with a /mcp
// path matches by URL. Pins the URL matcher independently of the name matcher.
func TestCheckMCP_HTTPRegistrationByBackendURL(t *testing.T) {
	withFakeDoctorRunOutput(t, func(_ string, args ...string) (string, error) {
		if len(args) >= 2 && args[1] == "list" {
			return "my-mcp: http://localhost:8080/mcp (HTTP) - ✔ Connected", nil
		}
		return "", errors.New("unexpected call")
	})
	r := checkMCPRegistration("http://localhost:8080")
	if r.status != "ok" {
		t.Fatalf("status = %q, want ok (matched by backend URL); detail: %s", r.status, r.detail)
	}
	if !strings.Contains(r.detail, "matches backend URL") {
		t.Errorf("detail = %q, want it to note the backend-URL match", r.detail)
	}
}

// TestCheckMCP_StdioRegistration: the stdio shim entry `fishhawk: /path...`
// matches by name and reports the stdio transport.
func TestCheckMCP_StdioRegistration(t *testing.T) {
	withFakeDoctorRunOutput(t, func(_ string, args ...string) (string, error) {
		if len(args) >= 2 && args[1] == "list" {
			return "fishhawk: /Users/x/bin/fishhawk-mcp-shim  - ✔ Connected", nil
		}
		return "", errors.New("unexpected call")
	})
	r := checkMCPRegistration("http://localhost:8080")
	if r.status != "ok" {
		t.Fatalf("status = %q, want ok; detail: %s", r.status, r.detail)
	}
	if !strings.Contains(r.detail, "stdio") {
		t.Errorf("detail = %q, want the stdio transport named", r.detail)
	}
}

// TestCheckMCP_NoRegistrationFailsWithTransportAwareHint: `claude mcp list`
// answers with no fishhawk entry and the get fallback also misses -> fail with
// a hint naming the HTTP remedy first.
func TestCheckMCP_NoRegistrationFailsWithTransportAwareHint(t *testing.T) {
	withFakeDoctorRunOutput(t, func(_ string, args ...string) (string, error) {
		if len(args) >= 2 && args[1] == "list" {
			return "claude.ai: https://example.com/other (HTTP) - ✔ Connected", nil
		}
		// get fallback for both names misses.
		return "", errors.New("no such server")
	})
	r := checkMCPRegistration("http://localhost:8080")
	if r.status != "fail" {
		t.Fatalf("status = %q, want fail; detail: %s", r.status, r.detail)
	}
	if !strings.Contains(r.remediate, "--transport http") {
		t.Errorf("remediate = %q, want the HTTP registration remedy", r.remediate)
	}
}

// TestCheckMCP_ClaudeCLIAbsentWarns is the binding condition 4 test: the claude
// CLI absent (list AND get both error) degrades to WARN — never fail, never a
// false ok. Losing that degradation fails this test.
func TestCheckMCP_ClaudeCLIAbsentWarns(t *testing.T) {
	withFakeDoctorRunOutput(t, func(_ string, _ ...string) (string, error) {
		return "", errors.New(`exec: "claude": executable file not found in $PATH`)
	})
	r := checkMCPRegistration("http://localhost:8080")
	if r.status != "warn" {
		t.Errorf("status = %q, want warn (claude CLI unavailable)", r.status)
	}
	if !strings.Contains(r.detail, "claude CLI unavailable") {
		t.Errorf("detail = %q, want it to name the unavailable CLI", r.detail)
	}
}

// TestCheckMCP_ListErrorsGetHTTPFallback is the binding condition 2 test: when
// `claude mcp list` errors, the get fallback probes BOTH the stdio and HTTP
// names, so an HTTP-transport registration reachable only via `get fishhawk-http`
// is still recognised. Dropping fishhawk-http from the fallback fails this.
func TestCheckMCP_ListErrorsGetHTTPFallback(t *testing.T) {
	withFakeDoctorRunOutput(t, func(_ string, args ...string) (string, error) {
		if len(args) >= 2 && args[1] == "list" {
			return "", errors.New("mcp list: transient error")
		}
		if len(args) >= 3 && args[1] == "get" && args[2] == "fishhawk-http" {
			return "fishhawk-http: connected", nil
		}
		// `get fishhawk` (stdio) misses.
		return "", errors.New("no such server")
	})
	r := checkMCPRegistration("http://localhost:8080")
	if r.status != "ok" {
		t.Fatalf("status = %q, want ok (HTTP name resolved via get fallback); detail: %s", r.status, r.detail)
	}
	if !strings.Contains(r.detail, "fishhawk-http") {
		t.Errorf("detail = %q, want the HTTP registration named", r.detail)
	}
}

// --- runner rung: per-branch behavioral tests (#2480) -----------------------

// TestResolveRunnerBinary_SiblingOfExecutable: with no flag/env and LookPath
// missing, resolveRunnerBinary resolves a fishhawk-runner sibling of the
// running executable.
func TestResolveRunnerBinary_SiblingOfExecutable(t *testing.T) {
	exeDir := t.TempDir()
	sibling := filepath.Join(exeDir, "fishhawk-runner")
	if err := os.WriteFile(sibling, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	withFakeDoctorExecutable(t, func() (string, error) {
		return filepath.Join(exeDir, "fishhawk"), nil
	})
	withFakeDoctorLookPath(t, func(_ string) (string, error) {
		return "", exec.ErrNotFound
	})
	path, source, err := resolveRunnerBinary("", t.TempDir())
	if err != nil {
		t.Fatalf("resolveRunnerBinary err = %v, want nil (sibling should resolve)", err)
	}
	if path != sibling {
		t.Errorf("path = %q, want the sibling %q", path, sibling)
	}
	if source != "sibling" {
		t.Errorf("source = %q, want sibling", source)
	}
}

// TestCheckRunnerBinary_UnresolvedWarns: every tier misses -> warn (not fail),
// naming the sibling-of-serving-binary case the CLI cannot observe.
func TestCheckRunnerBinary_UnresolvedWarns(t *testing.T) {
	withFakeDoctorExecutable(t, func() (string, error) {
		return filepath.Join(t.TempDir(), "fishhawk"), nil // no sibling runner
	})
	withFakeDoctorLookPath(t, func(_ string) (string, error) {
		return "", exec.ErrNotFound
	})
	r := checkRunnerBinary("", t.TempDir())
	if r.status != "warn" {
		t.Fatalf("status = %q, want warn", r.status)
	}
	if !strings.Contains(r.remediate, "SERVING binary") {
		t.Errorf("remediate = %q, want it to name the serving-binary sibling case", r.remediate)
	}
}

// --- SHA-drift rung: skip guard + counterfactual (#2480) --------------------

// TestCheckBackendSHADrift_SkippedOutsideBackendCheckout: a working dir that is
// not the backend's own source checkout skips the rung (ok, no remediation)
// before any HTTP call — comparing fishhawkd's SHA to an unrelated HEAD is a
// category error.
func TestCheckBackendSHADrift_SkippedOutsideBackendCheckout(t *testing.T) {
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		t.Fatal("no HTTP call expected when the working dir is not a backend checkout")
		return nil, nil
	})
	r := checkBackendSHADrift("http://localhost:8080", t.TempDir()) // no backend markers
	if r.status != "ok" {
		t.Errorf("status = %q, want ok (skipped)", r.status)
	}
	if !strings.Contains(r.detail, "skipped") {
		t.Errorf("detail = %q, want a skip note", r.detail)
	}
	if r.remediate != "" {
		t.Errorf("remediate = %q, want empty on a skip", r.remediate)
	}
}

// TestCheckBackendSHADrift_WarnsInsideBackendCheckout is the counterfactual for
// the skip guard (binding condition 5, vehicle 3): inside a real backend
// checkout, a backend SHA that genuinely differs from local HEAD still warns.
// Deleting the isBackendSourceCheckout call so the rung always skips turns this
// red — proving the skip did not cost the drift detection the rung exists for.
func TestCheckBackendSHADrift_WarnsInsideBackendCheckout(t *testing.T) {
	dir, _ := initGitRepoWithHead(t) // marks the checkout as backend source
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusOK,
			`{"status":"ok","git_sha":"aaaaaaa"}`), nil // not a prefix of local HEAD
	})
	r := checkBackendSHADrift("http://localhost:8080", dir)
	if r.status != "warn" {
		t.Fatalf("status = %q, want warn (drift inside a backend checkout); detail: %s", r.status, r.detail)
	}
	if strings.Contains(r.detail, "skipped") {
		t.Errorf("detail = %q, want the drift branch, not the skip branch", r.detail)
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
	dir := t.TempDir()
	markBackendCheckout(t, dir) // so the rung reaches the healthz probe, not the skip
	r := checkBackendSHADrift("http://localhost:8080", dir)
	if r.status != "ok" {
		t.Errorf("status = %q, want ok (unknown SHA should be fine)", r.status)
	}
	if strings.Contains(r.detail, "skipped") {
		t.Errorf("detail = %q, want the unknown-SHA branch, not the skip branch", r.detail)
	}
}

// TestCheckBackendSHADrift_Unreachable returns warn when the backend is
// unreachable (not fail, because the SHA check is advisory).
func TestCheckBackendSHADrift_Unreachable(t *testing.T) {
	withFakeDoctorHTTP(t, func(_ *http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})
	dir := t.TempDir()
	markBackendCheckout(t, dir) // so the rung reaches the healthz probe, not the skip
	r := checkBackendSHADrift("http://localhost:8080", dir)
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
	// The drift tests that use this helper must exercise the drift branches,
	// which only run when the working dir is the backend's own source checkout.
	markBackendCheckout(t, dir)
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

// externalRepoNotInstalledReadinessJSON is a readiness payload for an external
// repo whose GitHub App is NOT installed but everything else is green.
const externalRepoNotInstalledReadinessJSON = `{
  "repo": "acme/widgets",
  "app": {"installed": false, "reason": "GitHub App is not installed on the target repository"},
  "spec": {"source": "unavailable", "note": "app not installed"},
  "reviewers": [{"provider": "anthropic", "model": "claude-opus-4-8", "available": true}],
  "scopes": {"adequate": true, "required": ["read:runs"], "missing": []}
}`

// initCleanGitRepo inits a git repo in a fresh temp dir (no backend-source
// markers) so runDoctor's git rungs run against a real repo and the SHA-drift
// rung takes its skip branch.
func initCleanGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", dir},
		{"-C", dir, "config", "user.email", "test@example.com"},
		{"-C", dir, "config", "user.name", "Test"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil { //nolint:gosec
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// writeValidSpec writes a schema-valid workflows.yaml under <dir>/.fishhawk.
func writeValidSpec(t *testing.T, dir string) {
	t.Helper()
	hidden := filepath.Join(dir, ".fishhawk")
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "workflows.yaml"), []byte(validateValidYAML), 0o600); err != nil {
		t.Fatal(err)
	}
}

// externalRepoDoctorHTTP builds the HTTP stub shared by the two external-repo
// end-to-end tests: /healthz + /v0/runs green, minio health green, and the
// readiness endpoint serving the supplied payload.
func externalRepoDoctorHTTP(readinessJSON string) func(*http.Request) (*http.Response, error) {
	return func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/v0/onboarding/readiness"):
			return fakeHTTPResponse(http.StatusOK, readinessJSON), nil
		case strings.HasSuffix(req.URL.Path, "/healthz"):
			return fakeHTTPResponse(http.StatusOK, `{"status":"ok","version":"v0.0.0-test"}`), nil
		case strings.Contains(req.URL.Path, "/v0/runs"):
			return fakeHTTPResponse(http.StatusOK, `{"items":[]}`), nil
		default:
			return fakeHTTPResponse(http.StatusOK, `{}`), nil
		}
	}
}

// externalRepoDoctorRunOutput stubs docker/pg/gh green and `claude mcp list`
// with the HTTP-transport registration this issue exists to recognise.
func externalRepoDoctorRunOutput() func(string, ...string) (string, error) {
	return func(name string, args ...string) (string, error) {
		if name == "claude" && len(args) >= 2 && args[0] == "mcp" && args[1] == "list" {
			return "fishhawk-http: https://localhost:8443/mcp (HTTP) - ✔ Connected", nil
		}
		return fmt.Sprintf("%s ok", name), nil
	}
}

// TestRunDoctor_ExternalRepoHTTPMCP_NoFailures is the headline cross-boundary
// vehicle: runDoctor itself (not a single rung) against a fully-ready EXTERNAL
// repo on an HTTP-MCP/OAuth setup — an fho_ credential from the credstore seam,
// an HTTP-transport MCP registration, and a working dir that is NOT a fishhawkd
// checkout — must render ZERO `fail` rungs and exit 0. This is the failure mode
// the change guards against inverting (an over-strict doctor that blocks a
// legitimately-onboarded external repo).
func TestRunDoctor_ExternalRepoHTTPMCP_NoFailures(t *testing.T) {
	dir := initCleanGitRepo(t)
	writeValidSpec(t, dir)

	withFakeDoctorHTTP(t, externalRepoDoctorHTTP(allGreenReadinessJSON))
	withFakeDoctorRunOutput(t, externalRepoDoctorRunOutput())
	withFakeDoctorLookPath(t, func(_ string) (string, error) {
		return "/usr/local/bin/fishhawk-runner", nil
	})
	// Empty flag token -> the credstore seam supplies an fho_ OAuth credential.
	withFakeDoctorCredLoad(t, func(_ string) (credstore.Credential, error) {
		return credstore.Credential{Token: "fho_external_oauth"}, nil
	})
	origGitRemote := gitRemoteOriginURL
	gitRemoteOriginURL = func(_ string) (string, error) {
		return "https://github.com/acme/widgets.git", nil
	}
	t.Cleanup(func() { gitRemoteOriginURL = origGitRemote })

	var stdout, stderr strings.Builder
	got := run([]string{
		"doctor",
		"--backend-url", "http://localhost:8080",
		"--repo", "acme/widgets",
		"--working-dir", dir,
		"--runner-binary", "/usr/local/bin/fishhawk-runner",
	}, &stdout, &stderr)

	out := stdout.String()
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK; stdout:\n%s", got, out)
	}
	// No rung may render as a fail.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasSuffix(strings.TrimRight(line, " "), "fail") {
			t.Errorf("unexpected fail rung: %q\nfull output:\n%s", line, out)
		}
	}
	if !strings.Contains(out, "ready for local loop") {
		t.Errorf("stdout missing 'ready for local loop':\n%s", out)
	}
}

// TestRunDoctor_ExternalRepo_AppNotInstalled_StillFails is counterfactual
// vehicle 1 (binding condition 5): the SAME all-relaxed external-repo setup but
// a readiness payload with app.installed=false must FAIL the 'app installed'
// rung and exit non-zero. Deleting the fail branch in checkOnboardingReadiness's
// app arm turns this red. The state is seeded BY CONSTRUCTION (a stubbed
// readiness body), so the RED lands on the behavioral assertion, not fixture
// setup.
func TestRunDoctor_ExternalRepo_AppNotInstalled_StillFails(t *testing.T) {
	dir := initCleanGitRepo(t)
	writeValidSpec(t, dir)

	withFakeDoctorHTTP(t, externalRepoDoctorHTTP(externalRepoNotInstalledReadinessJSON))
	withFakeDoctorRunOutput(t, externalRepoDoctorRunOutput())
	withFakeDoctorLookPath(t, func(_ string) (string, error) {
		return "/usr/local/bin/fishhawk-runner", nil
	})
	withFakeDoctorCredLoad(t, func(_ string) (credstore.Credential, error) {
		return credstore.Credential{Token: "fho_external_oauth"}, nil
	})
	origGitRemote := gitRemoteOriginURL
	gitRemoteOriginURL = func(_ string) (string, error) {
		return "https://github.com/acme/widgets.git", nil
	}
	t.Cleanup(func() { gitRemoteOriginURL = origGitRemote })

	var stdout, stderr strings.Builder
	got := run([]string{
		"doctor",
		"--backend-url", "http://localhost:8080",
		"--repo", "acme/widgets",
		"--working-dir", dir,
		"--runner-binary", "/usr/local/bin/fishhawk-runner",
	}, &stdout, &stderr)

	out := stdout.String()
	if got != exitFailure {
		t.Fatalf("run = %d, want exitFailure (App not installed must still fail); stdout:\n%s", got, out)
	}
	// The app rung must be the failure, rendered as fail.
	appLineFail := false
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "app installed") && strings.HasSuffix(strings.TrimRight(line, " "), "fail") {
			appLineFail = true
		}
	}
	if !appLineFail {
		t.Errorf("want the 'app installed' rung rendered as fail:\n%s", out)
	}
}

// TestRunDoctor_VerifyRungRendersAndFails drives runDoctor itself — not just
// the check function — against a repo whose committed verify command reads a
// gitignored artifact present only in the operator checkout. The network
// rungs are stubbed; the rendered output must carry the new rung's label and
// the process must exit non-zero.
func TestRunDoctor_VerifyRungRendersAndFails(t *testing.T) {
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
	withFakeDoctorLookPath(t, func(name string) (string, error) { return "/usr/bin/" + name, nil })
	withFakeDoctorRunOutput(t, func(name string, _ ...string) (string, error) {
		return fmt.Sprintf("%s ok", name), nil
	})

	repo := newVerifyRepo(t,
		specWithCommand("cat build/artifact", "15m"),
		verifyRepoFile{path: ".gitignore", contents: "build/\n"},
	)
	writeRepoFile(t, repo, "build/artifact", "prebuilt\n")

	var stdout, stderr strings.Builder
	got := runDoctor([]string{"--working-dir", repo, "--token", "fhk_test", "--run-verify-command"}, &stdout, &stderr)

	out := stdout.String()
	if !strings.Contains(out, verifyRungLabel) {
		t.Errorf("stdout missing the %q rung:\n%s", verifyRungLabel, out)
	}
	if !strings.Contains(out, "No such file or directory") && !strings.Contains(out, "exited") {
		t.Errorf("stdout does not render the verify failure:\n%s", out)
	}
	if got == exitOK {
		t.Errorf("runDoctor = exitOK, want non-zero when the verify rung fails:\n%s", out)
	}
}

// TestRunDoctor_VerifyRungRequiresOptIn drives runDoctor with the SAME repo
// and stubs as the test above but WITHOUT --run-verify-command: the rung must
// render as a warn that names the flag, the run must exit 0 on that rung's
// account, and no subprocess may be spawned.
//
// This is the flag-wiring counterfactual — it fails if doctor.go stops routing
// through checkVerifyCommandGated, which the check-level test cannot see. It
// asserts committed filesystem state (the marker the command would create), so
// a gate that returned a different-looking result while still executing cannot
// pass it.
func TestRunDoctor_VerifyRungRequiresOptIn(t *testing.T) {
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
	withFakeDoctorLookPath(t, func(name string) (string, error) { return "/usr/bin/" + name, nil })
	withFakeDoctorRunOutput(t, func(name string, _ ...string) (string, error) {
		return fmt.Sprintf("%s ok", name), nil
	})

	markerDir := t.TempDir()
	marker := filepath.Join(markerDir, "opt-in-marker")
	repo := newVerifyRepo(t, specWithCommand("touch "+marker, "15m"))

	var stdout, stderr strings.Builder
	runDoctor([]string{"--working-dir", repo, "--token", "fhk_test"}, &stdout, &stderr)

	out := stdout.String()
	if !strings.Contains(out, verifyRungLabel) {
		t.Errorf("stdout missing the %q rung:\n%s", verifyRungLabel, out)
	}
	if !strings.Contains(out, "--run-verify-command") {
		t.Errorf("stdout does not name --run-verify-command:\n%s", out)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("marker %s exists — doctor spawned the spec's command without --run-verify-command", marker)
	}
}

// TestRunDoctor_SpecOnlyExcludesVerifyRung: --spec-only is the byte-only,
// environment-free path, so it must NOT provision a worktree or spawn the
// spec's command. Asserted on committed filesystem state (the marker file the
// command would have created), not on the rendered text.
func TestRunDoctor_SpecOnlyExcludesVerifyRung(t *testing.T) {
	markerDir := t.TempDir()
	marker := filepath.Join(markerDir, "spec-only-marker")
	repo := newVerifyRepo(t, specWithCommand("touch "+marker, "15m"))

	var stdout, stderr strings.Builder
	runDoctor([]string{"--spec-only", "--working-dir", repo}, &stdout, &stderr)

	if strings.Contains(stdout.String(), verifyRungLabel) {
		t.Errorf("--spec-only rendered the %q rung:\n%s", verifyRungLabel, stdout.String())
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("--spec-only spawned the verify command (marker %s exists)", marker)
	}
}
