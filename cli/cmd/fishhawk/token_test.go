package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/cli/internal/credstore"
)

// tokenTestServer is a single httptest server that plays BOTH the
// GitHub device-flow host and the Fishhawk backend, routed by path.
// It is configurable per test so each failure mode can be driven.
type tokenTestServer struct {
	srv *httptest.Server

	// deviceCode is the response to POST /login/device/code.
	deviceCode deviceCodeResponse

	// pollStates is the sequence of access-token poll responses; each
	// poll pops the next one, and the last is repeated if exhausted.
	pollStates []accessTokenResponse
	pollIdx    int
	mu         sync.Mutex

	// discovery is the GET /v0/tokens/login response; discoveryStatus
	// overrides the 200 when non-zero (e.g. 503 unconfigured).
	discovery       tokenLoginDiscovery
	discoveryStatus int

	// mint is the POST /v0/tokens/login response; mintStatus + mintErr
	// drive the failure modes.
	mint       tokenLoginResponse
	mintStatus int
	mintErr    string // when set, mint answers an error envelope

	// mintRequests records what the CLI POSTed to the mint endpoint.
	mintRequests []tokenLoginRequest
}

func newTokenTestServer(t *testing.T) *tokenTestServer {
	t.Helper()
	ts := &tokenTestServer{
		deviceCode: deviceCodeResponse{
			DeviceCode:      "devcode-123",
			UserCode:        "WXYZ-1234",
			VerificationURI: "https://github.com/login/device",
			ExpiresIn:       900,
			Interval:        5,
		},
		pollStates: []accessTokenResponse{{AccessToken: "gho_useraccess"}},
		discovery:  tokenLoginDiscovery{Provider: "github", ClientID: "Iv1.testclient"},
		mint: tokenLoginResponse{
			Token:      "fhk_minted",
			Subject:    "github:octocat",
			Scopes:     []string{"read:runs", "write:approvals"},
			AuthMethod: "oauth",
			Provider:   "github",
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/login/device/code", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, ts.deviceCode)
	})
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		defer ts.mu.Unlock()
		st := ts.pollStates[ts.pollIdx]
		if ts.pollIdx < len(ts.pollStates)-1 {
			ts.pollIdx++
		}
		writeJSON(w, http.StatusOK, st)
	})
	mux.HandleFunc("/v0/tokens/login", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if ts.discoveryStatus != 0 {
				writeAPIError(w, ts.discoveryStatus, "tokens_unconfigured", "OAuth login is not configured on this backend")
				return
			}
			writeJSON(w, http.StatusOK, ts.discovery)
		case http.MethodPost:
			var req tokenLoginRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			ts.mu.Lock()
			ts.mintRequests = append(ts.mintRequests, req)
			ts.mu.Unlock()
			if ts.mintStatus != 0 {
				writeAPIError(w, ts.mintStatus, ts.mintErr, "mint rejected")
				return
			}
			writeJSON(w, http.StatusOK, ts.mint)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	ts.srv = httptest.NewServer(mux)
	t.Cleanup(ts.srv.Close)
	return ts
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeAPIError(w http.ResponseWriter, status int, code, msg string) {
	var env apiErrorEnvelope
	env.Error.Code = code
	env.Error.Message = msg
	writeJSON(w, status, env)
}

// setupTokenTest points the device flow at the test server, shrinks
// the poll interval so the loop runs in microseconds, and isolates
// the credential store under a temp XDG dir.
func setupTokenTest(t *testing.T, ts *tokenTestServer) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FISHHAWK_OAUTH_CLIENT_ID", "") // ensure discovery is exercised unless a test overrides
	t.Setenv("FISHHAWK_TOKEN", "")

	prevBase := githubDeviceBaseURL
	prevInterval := deviceFlowInterval
	githubDeviceBaseURL = ts.srv.URL
	deviceFlowInterval = time.Microsecond
	t.Cleanup(func() {
		githubDeviceBaseURL = prevBase
		deviceFlowInterval = prevInterval
	})
}

func TestTokenLogin_HappyPath(t *testing.T) {
	ts := newTokenTestServer(t)
	setupTokenTest(t, ts)

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "login", "--backend-url", ts.srv.URL}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitOK, stderr.String())
	}

	// The device prompt (user_code + verification_uri) is printed.
	if !strings.Contains(stderr.String(), "WXYZ-1234") {
		t.Errorf("stderr missing user_code: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "https://github.com/login/device") {
		t.Errorf("stderr missing verification_uri: %s", stderr.String())
	}

	// The result block carries subject / scope / expiry.
	out := stdout.String()
	for _, want := range []string{"github:octocat", "read:runs", "write:approvals", "none (token does not expire)"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q: %s", want, out)
		}
	}

	// The mint request carried the device-flow access token + provider.
	if len(ts.mintRequests) != 1 {
		t.Fatalf("want 1 mint request, got %d", len(ts.mintRequests))
	}
	if ts.mintRequests[0].AccessToken != "gho_useraccess" || ts.mintRequests[0].Provider != "github" {
		t.Errorf("mint request wrong: %+v", ts.mintRequests[0])
	}

	// The minted token is stored, keyed by backend URL.
	cred, err := credstore.Load(ts.srv.URL)
	if err != nil {
		t.Fatalf("stored credential not found: %v", err)
	}
	if cred.Token != "fhk_minted" || cred.Subject != "github:octocat" || cred.Provider != "github" {
		t.Errorf("stored credential wrong: %+v", cred)
	}
}

// --client-id (or FISHHAWK_OAUTH_CLIENT_ID) skips discovery entirely.
func TestTokenLogin_ClientIDOverrideSkipsDiscovery(t *testing.T) {
	ts := newTokenTestServer(t)
	setupTokenTest(t, ts)
	// Make discovery fail: if the CLI hits it, the login would error.
	ts.discoveryStatus = http.StatusServiceUnavailable

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "login", "--backend-url", ts.srv.URL, "--client-id", "Iv1.override"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitOK, stderr.String())
	}
	if _, err := credstore.Load(ts.srv.URL); err != nil {
		t.Fatalf("credential not stored: %v", err)
	}
}

// authorization_pending then success exercises the poll loop.
func TestTokenLogin_PollsThroughPending(t *testing.T) {
	ts := newTokenTestServer(t)
	setupTokenTest(t, ts)
	ts.pollStates = []accessTokenResponse{
		{Error: "authorization_pending"},
		{Error: "slow_down", Interval: 0},
		{AccessToken: "gho_late"},
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "login", "--backend-url", ts.srv.URL}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitOK, stderr.String())
	}
	if len(ts.mintRequests) != 1 || ts.mintRequests[0].AccessToken != "gho_late" {
		t.Fatalf("expected mint with gho_late, got %+v", ts.mintRequests)
	}
}

// access_denied aborts login and stores nothing.
func TestTokenLogin_AccessDenied(t *testing.T) {
	ts := newTokenTestServer(t)
	setupTokenTest(t, ts)
	ts.pollStates = []accessTokenResponse{{Error: "access_denied"}}

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "login", "--backend-url", ts.srv.URL}, &stdout, &stderr)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr.String(), "denied") {
		t.Errorf("stderr missing denial reason: %s", stderr.String())
	}
	if _, err := credstore.Load(ts.srv.URL); err == nil {
		t.Error("credential must NOT be stored on a denied login")
	}
	if len(ts.mintRequests) != 0 {
		t.Errorf("mint must not be called on a denied login, got %d", len(ts.mintRequests))
	}
}

// Discovery returning 503 tokens_unconfigured fails the login with a
// legible error and no browser prompt.
func TestTokenLogin_DiscoveryUnconfigured(t *testing.T) {
	ts := newTokenTestServer(t)
	setupTokenTest(t, ts)
	ts.discoveryStatus = http.StatusServiceUnavailable

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "login", "--backend-url", ts.srv.URL}, &stdout, &stderr)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitFailure, stderr.String())
	}
	if !strings.Contains(stderr.String(), "tokens_unconfigured") {
		t.Errorf("stderr should surface tokens_unconfigured: %s", stderr.String())
	}
	if _, err := credstore.Load(ts.srv.URL); err == nil {
		t.Error("credential must NOT be stored when discovery is unconfigured")
	}
}

// A mint rejection (e.g. the verified subject lacks operator
// permission → 403) aborts and stores nothing.
func TestTokenLogin_MintRejected(t *testing.T) {
	ts := newTokenTestServer(t)
	setupTokenTest(t, ts)
	ts.mintStatus = http.StatusForbidden
	ts.mintErr = "insufficient_permission"

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "login", "--backend-url", ts.srv.URL}, &stdout, &stderr)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr.String(), "insufficient_permission") {
		t.Errorf("stderr should surface the mint error code: %s", stderr.String())
	}
	if _, err := credstore.Load(ts.srv.URL); err == nil {
		t.Error("credential must NOT be stored when mint is rejected")
	}
}

// Discovery answering 200 but with an empty client_id is a distinct
// guard from the 503 path: the login cannot proceed without a client_id.
func TestTokenLogin_DiscoveryEmptyClientID(t *testing.T) {
	ts := newTokenTestServer(t)
	setupTokenTest(t, ts)
	ts.discovery = tokenLoginDiscovery{Provider: "github", ClientID: ""}

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "login", "--backend-url", ts.srv.URL}, &stdout, &stderr)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitFailure, stderr.String())
	}
	if !strings.Contains(stderr.String(), "client_id") {
		t.Errorf("stderr should explain the missing client_id: %s", stderr.String())
	}
	if len(ts.mintRequests) != 0 {
		t.Errorf("mint must not be reached without a client_id, got %d", len(ts.mintRequests))
	}
}

// An expired device code aborts the login.
func TestTokenLogin_DeviceCodeExpired(t *testing.T) {
	ts := newTokenTestServer(t)
	setupTokenTest(t, ts)
	ts.pollStates = []accessTokenResponse{{Error: "expired_token"}}

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "login", "--backend-url", ts.srv.URL}, &stdout, &stderr)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if _, err := credstore.Load(ts.srv.URL); err == nil {
		t.Error("credential must NOT be stored on an expired device code")
	}
}

// An unrecognized device-flow error is surfaced, not silently retried.
func TestTokenLogin_DeviceFlowUnknownError(t *testing.T) {
	ts := newTokenTestServer(t)
	setupTokenTest(t, ts)
	ts.pollStates = []accessTokenResponse{{Error: "unmapped_error"}}

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "login", "--backend-url", ts.srv.URL}, &stdout, &stderr)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr.String(), "unmapped_error") {
		t.Errorf("stderr should surface the raw device-flow error: %s", stderr.String())
	}
}

// A 200 mint response carrying no token is rejected (never stored as
// an empty credential).
func TestTokenLogin_MintEmptyToken(t *testing.T) {
	ts := newTokenTestServer(t)
	setupTokenTest(t, ts)
	ts.mint = tokenLoginResponse{Subject: "github:octocat", Provider: "github"} // Token == ""

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "login", "--backend-url", ts.srv.URL}, &stdout, &stderr)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr.String(), "empty token") {
		t.Errorf("stderr should flag the empty token: %s", stderr.String())
	}
	if _, err := credstore.Load(ts.srv.URL); err == nil {
		t.Error("an empty-token mint must NOT be stored")
	}
}

// A non-github provider is rejected before any network call.
func TestTokenLogin_UnsupportedProvider(t *testing.T) {
	ts := newTokenTestServer(t)
	setupTokenTest(t, ts)

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "login", "--backend-url", ts.srv.URL, "--provider", "gitlab"}, &stdout, &stderr)
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), "gitlab") {
		t.Errorf("stderr should name the unsupported provider: %s", stderr.String())
	}
}

// `token login --help` describes the OAuth device flow (not just a bare
// flag list) and lists the per-command flags, and exits 0. This pins the
// blocking `fishhawk token login --help` acceptance path.
func TestTokenLogin_HelpDescribesDeviceFlow(t *testing.T) {
	for _, arg := range []string{"--help", "-h"} {
		var stdout, stderr bytes.Buffer
		code := run([]string{"token", "login", arg}, &stdout, &stderr)
		if code != exitOK {
			t.Fatalf("%s: exit = %d, want %d; stderr=%s", arg, code, exitOK, stderr.String())
		}
		out := stderr.String()
		for _, want := range []string{"device flow", "authorize", "--provider", "--client-id"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: help missing %q:\n%s", arg, want, out)
			}
		}
	}
}

func TestTokenList_Empty(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "list"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d", code, exitOK)
	}
	if !strings.Contains(stdout.String(), "no stored credentials") {
		t.Errorf("stdout should note the empty store: %s", stdout.String())
	}
}

func TestTokenList_Populated(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := credstore.Store("http://localhost:8080", credstore.Credential{
		Token:    "fhk_a",
		Subject:  "github:alice",
		Scopes:   []string{"read:runs"},
		Provider: "github",
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "list"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d", code, exitOK)
	}
	out := stdout.String()
	for _, want := range []string{"http://localhost:8080", "github:alice", "read:runs"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q: %s", want, out)
		}
	}
	// The bare token secret must never be printed.
	if strings.Contains(out, "fhk_a") {
		t.Errorf("token list leaked the bearer secret: %s", out)
	}
}

func TestToken_NoSubcommand(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"token"}, io.Discard, &stderr); code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), "login|list") {
		t.Errorf("usage should list subcommands: %s", stderr.String())
	}
}

func TestToken_UnknownSubcommand(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"token", "nope"}, io.Discard, &stderr); code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
}

// newClient token resolution: an explicit --token/FISHHAWK_TOKEN wins
// over a stored credential; when empty, the stored credential is used.
func TestNewClient_TokenPrecedence(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const backend = "http://localhost:8080"
	if err := credstore.Store(backend, credstore.Credential{Token: "fhk_stored"}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	timeout := 5 * time.Second

	// Explicit token wins.
	explicit := "fhk_explicit"
	url := backend
	c := newClient(commonFlags{backendURL: &url, token: &explicit, timeout: &timeout})
	if c.Token != "fhk_explicit" {
		t.Errorf("explicit token should win, got %q", c.Token)
	}

	// Empty token falls back to the stored credential.
	empty := ""
	c = newClient(commonFlags{backendURL: &url, token: &empty, timeout: &timeout})
	if c.Token != "fhk_stored" {
		t.Errorf("should fall back to stored token, got %q", c.Token)
	}

	// No stored credential and empty token → empty (dev backend).
	otherURL := "http://no-cred:9999"
	c = newClient(commonFlags{backendURL: &otherURL, token: &empty, timeout: &timeout})
	if c.Token != "" {
		t.Errorf("want empty token when nothing stored, got %q", c.Token)
	}
}

// TestCheckAPIStatus_SurfacesDetailsError exercises the E39.10 / #1753
// change: a failed mint's response body carries the underlying cause under
// details.error (the backend's map[string]any{"error": ...}), and
// checkAPIStatus appends it to the returned error so the operator sees WHY
// the mint 500'd, not just "permission check failed".
func TestCheckAPIStatus_SurfacesDetailsError(t *testing.T) {
	body := `{"error":{"code":"internal_error","message":"permission check failed"},` +
		`"details":{"error":"identity: do request: 401 Unauthorized"}}`
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	err := checkAPIStatus(resp)
	if err == nil {
		t.Fatal("checkAPIStatus returned nil for a 500 response")
	}
	got := err.Error()
	if !strings.Contains(got, "permission check failed") {
		t.Errorf("error should carry the backend message: %q", got)
	}
	if !strings.Contains(got, "401 Unauthorized") {
		t.Errorf("error should surface details.error cause: %q", got)
	}
}

// TestCheckAPIStatus_NoDetails keeps the details-absent branch honest: an
// error envelope without a details.error still yields the code+message form
// with no dangling separator.
func TestCheckAPIStatus_NoDetails(t *testing.T) {
	body := `{"error":{"code":"insufficient_permission","message":"nope"}}`
	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	err := checkAPIStatus(resp)
	if err == nil {
		t.Fatal("checkAPIStatus returned nil for a 403 response")
	}
	got := err.Error()
	if want := "HTTP 403 (insufficient_permission): nope"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
}
