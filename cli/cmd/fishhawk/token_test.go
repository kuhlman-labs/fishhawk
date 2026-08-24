package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/credstore"
)

// tokenTestServer is a single httptest server that plays BOTH the
// GitHub device-flow host and the Fishhawk backend, routed by path.
// It is configurable per test so each failure mode can be driven.
type tokenTestServer struct {
	srv *httptest.Server

	// deviceCode is the response to POST /login/device/code.
	deviceCode deviceCodeResponse

	// deviceCodeStatus overrides the 200 status for POST
	// /login/device/code (e.g. a non-2xx device_flow_disabled OAuth
	// error); deviceCodeBody is the raw body written in that case.
	deviceCodeStatus int
	deviceCodeBody   any

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

	// ghDeviceCodeCalls counts POSTs to GitHub's device-code endpoint,
	// so a test can assert the GitHub leg was NOT driven.
	ghDeviceCodeCalls int

	// --- GitLab device flow (E66.4 / #2392) -----------------------
	//
	// The same server also plays the GitLab instance, on GitLab's own
	// paths. glDeviceCode is the /oauth/authorize_device response;
	// glDeviceStatus/glDeviceBody override it with a raw body.
	glDeviceCode   deviceCodeResponse
	glDeviceStatus int
	glDeviceBody   any

	// glPollStates is the /oauth/token poll sequence. The handler picks
	// the status from the state: GitLab (Doorkeeper) serves RFC 8628
	// §3.5 poll states as OAuth errors on HTTP 400, not on 200 the way
	// GitHub does, so a state carrying an error is answered 400.
	glPollStates []accessTokenResponse
	glPollIdx    int

	// glDeviceForms / glPollForms record the form bodies the CLI POSTed
	// to each GitLab endpoint, so the wire contract is assertable.
	glDeviceForms []url.Values
	glPollForms   []url.Values
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
		glDeviceCode: deviceCodeResponse{
			DeviceCode:      "gl-devcode-123",
			UserCode:        "GLAB-5678",
			VerificationURI: "https://gitlab.example.com/oauth/device",
			ExpiresIn:       900,
			Interval:        5,
		},
		glPollStates: []accessTokenResponse{{AccessToken: "glpat_useraccess"}},
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
		ts.mu.Lock()
		ts.ghDeviceCodeCalls++
		ts.mu.Unlock()
		if ts.deviceCodeStatus != 0 {
			writeJSON(w, ts.deviceCodeStatus, ts.deviceCodeBody)
			return
		}
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
	// GitLab's device-flow endpoints. Both take form bodies; the poll
	// endpoint answers a poll STATE on HTTP 400 (Doorkeeper) and the
	// authorized token on 200.
	mux.HandleFunc("/oauth/authorize_device", func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		ts.glDeviceForms = append(ts.glDeviceForms, readForm(r))
		ts.mu.Unlock()
		if ts.glDeviceStatus != 0 {
			writeJSON(w, ts.glDeviceStatus, ts.glDeviceBody)
			return
		}
		writeJSON(w, http.StatusOK, ts.glDeviceCode)
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		defer ts.mu.Unlock()
		ts.glPollForms = append(ts.glPollForms, readForm(r))
		st := ts.glPollStates[ts.glPollIdx]
		if ts.glPollIdx < len(ts.glPollStates)-1 {
			ts.glPollIdx++
		}
		status := http.StatusOK
		if st.Error != "" {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, st)
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

// readForm parses an application/x-www-form-urlencoded request body.
func readForm(r *http.Request) url.Values {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	v, _ := url.ParseQuery(string(raw))
	return v
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

// A non-2xx device-code response carrying device_flow_disabled surfaces
// both GitHub's error text AND the checkbox hint (#1752, approval
// condition (2)'s sibling non-2xx mode).
func TestTokenLogin_DeviceCodeDisabledNon2xx(t *testing.T) {
	ts := newTokenTestServer(t)
	ts.deviceCodeStatus = http.StatusBadRequest
	ts.deviceCodeBody = map[string]string{
		"error":             "device_flow_disabled",
		"error_description": "Device Flow must be explicitly enabled for the app.",
	}
	setupTokenTest(t, ts)

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "login", "--backend-url", ts.srv.URL}, &stdout, &stderr)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitFailure, stderr.String())
	}
	out := stderr.String()
	if !strings.Contains(out, "device_flow_disabled") {
		t.Errorf("stderr should surface GitHub's error text: %s", out)
	}
	if !strings.Contains(out, "Enable Device Flow") {
		t.Errorf("stderr should append the checkbox hint: %s", out)
	}
}

// A 200 device-code response carrying the error in the body (no
// device_code) hits the same hint, and — per the binding approval
// condition — must NOT print the "To authorize, open ..." device-code
// prompt, since the flow never got a real device code to prompt with.
func TestTokenLogin_DeviceCodeDisabled200Body(t *testing.T) {
	ts := newTokenTestServer(t)
	ts.deviceCode = deviceCodeResponse{
		Error:            "device_flow_disabled",
		ErrorDescription: "Device Flow must be explicitly enabled for the app.",
	}
	setupTokenTest(t, ts)

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "login", "--backend-url", ts.srv.URL}, &stdout, &stderr)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitFailure, stderr.String())
	}
	out := stderr.String()
	if !strings.Contains(out, "Enable Device Flow") {
		t.Errorf("stderr should append the checkbox hint: %s", out)
	}
	if strings.Contains(out, "To authorize, open") {
		t.Errorf("no device-code prompt should print when the device-code request itself failed: %s", out)
	}
	if _, err := credstore.Load(ts.srv.URL); err == nil {
		t.Error("credential must NOT be stored when the device-code request fails")
	}
}

// A different OAuth error on the device-code request surfaces verbatim
// and must NOT carry the device_flow_disabled hint (guards against a
// misleading hint on an unrelated failure).
func TestTokenLogin_DeviceCodeOtherOAuthError(t *testing.T) {
	ts := newTokenTestServer(t)
	ts.deviceCodeStatus = http.StatusBadRequest
	ts.deviceCodeBody = map[string]string{
		"error":             "incorrect_client_credentials",
		"error_description": "The client_id is invalid.",
	}
	setupTokenTest(t, ts)

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "login", "--backend-url", ts.srv.URL}, &stdout, &stderr)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitFailure, stderr.String())
	}
	out := stderr.String()
	if !strings.Contains(out, "incorrect_client_credentials") {
		t.Errorf("stderr should surface the raw OAuth error: %s", out)
	}
	if strings.Contains(out, "Enable Device Flow") {
		t.Errorf("stderr must NOT carry the device-flow hint for an unrelated error: %s", out)
	}
}

// An unrecognized provider is rejected before any network call, and the
// message names the supported set (E66.4 / #2392).
func TestTokenLogin_UnsupportedProvider(t *testing.T) {
	ts := newTokenTestServer(t)
	setupTokenTest(t, ts)
	// Make discovery fail loudly: a provider rejected before the network
	// call cannot have reached it.
	ts.discoveryStatus = http.StatusServiceUnavailable

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "login", "--backend-url", ts.srv.URL, "--provider", "bitbucket"}, &stdout, &stderr)
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitUsage, stderr.String())
	}
	out := stderr.String()
	if !strings.Contains(out, "bitbucket") {
		t.Errorf("stderr should name the unsupported provider: %s", out)
	}
	// The rejection names what IS supported, so the operator can fix it
	// without reading the source.
	for _, want := range []string{"github", "gitlab"} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr should name supported provider %q: %s", want, out)
		}
	}
	if len(ts.mintRequests) != 0 {
		t.Errorf("mint must not be reached for an unsupported provider, got %d", len(ts.mintRequests))
	}
}

// --- GitLab device flow (E66.4 / #2392) ---------------------------

// gitlabDiscovery points the discovery response at a dual-forge backend
// whose gitlab entry names THIS test server as the instance base URL, so
// the CLI's GitLab endpoints resolve to a reachable in-test server (the
// counterfactual-attainability rule's trap (c)).
func gitlabDiscovery(ts *tokenTestServer) tokenLoginDiscovery {
	return tokenLoginDiscovery{
		Provider: "github",
		ClientID: "Iv1.testclient",
		Providers: []tokenLoginProviderEntry{
			{Provider: "github", ClientID: "Iv1.testclient"},
			{Provider: "gitlab", ClientID: "glcid.device", BaseURL: ts.srv.URL},
		},
	}
}

// `--provider gitlab` selects the gitlab discovery entry, drives the RFC
// 8628 device grant against the GitLab endpoints (form-encoded, no
// client_secret), and mints with provider=gitlab.
func TestTokenLogin_GitLabHappyPath(t *testing.T) {
	ts := newTokenTestServer(t)
	setupTokenTest(t, ts)
	ts.discovery = gitlabDiscovery(ts)
	ts.mint = tokenLoginResponse{
		Token:      "fhk_gl",
		Subject:    "gitlab:alice",
		Scopes:     []string{"read:runs"},
		AuthMethod: "oauth",
		Provider:   "gitlab",
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "login", "--backend-url", ts.srv.URL, "--provider", "gitlab"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitOK, stderr.String())
	}

	// The GitLab device-code prompt printed.
	if !strings.Contains(stderr.String(), "GLAB-5678") {
		t.Errorf("stderr missing gitlab user_code: %s", stderr.String())
	}

	// The GitLab endpoints were driven — NOT GitHub's.
	if len(ts.glDeviceForms) != 1 {
		t.Fatalf("want 1 /oauth/authorize_device request, got %d", len(ts.glDeviceForms))
	}
	dev := ts.glDeviceForms[0]
	if got := dev.Get("client_id"); got != "glcid.device" {
		t.Errorf("device-code client_id = %q, want the gitlab DEVICE client_id", got)
	}
	if got := dev.Get("scope"); got != "read_api" {
		t.Errorf("device-code scope = %q, want read_api", got)
	}
	// The device application is NON-Confidential: no secret on the wire.
	if got := dev.Get("client_secret"); got != "" {
		t.Errorf("device-code request must not carry a client_secret, got %q", got)
	}
	if len(ts.glPollForms) != 1 {
		t.Fatalf("want 1 /oauth/token request, got %d", len(ts.glPollForms))
	}
	poll := ts.glPollForms[0]
	if got := poll.Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:device_code" {
		t.Errorf("poll grant_type = %q, want the RFC 8628 device grant", got)
	}
	if got := poll.Get("device_code"); got != "gl-devcode-123" {
		t.Errorf("poll device_code = %q, want the issued device code", got)
	}
	if got := poll.Get("client_secret"); got != "" {
		t.Errorf("poll request must not carry a client_secret, got %q", got)
	}
	// GitHub's endpoints were never touched.
	if ts.ghDeviceCodeCalls != 0 {
		t.Errorf("a gitlab login must not touch GitHub's device endpoint, got %d calls", ts.ghDeviceCodeCalls)
	}
	if len(ts.mintRequests) != 1 {
		t.Fatalf("want 1 mint request, got %d", len(ts.mintRequests))
	}
	if ts.mintRequests[0].Provider != "gitlab" {
		t.Errorf("mint provider = %q, want gitlab", ts.mintRequests[0].Provider)
	}
	if ts.mintRequests[0].AccessToken != "glpat_useraccess" {
		t.Errorf("mint access_token = %q, want the GitLab device-flow token", ts.mintRequests[0].AccessToken)
	}

	cred, err := credstore.Load(ts.srv.URL)
	if err != nil {
		t.Fatalf("stored credential not found: %v", err)
	}
	if cred.Subject != "gitlab:alice" || cred.Provider != "gitlab" {
		t.Errorf("stored credential wrong: %+v", cred)
	}
}

// GitLab serves the RFC 8628 poll states as OAuth error responses on
// HTTP 400 (Doorkeeper), not on 200 the way GitHub does. The CLI must
// treat a decodable 400 as a poll STATE and keep polling, not as a
// transport failure.
func TestTokenLogin_GitLabPollsThroughPendingOn400(t *testing.T) {
	ts := newTokenTestServer(t)
	setupTokenTest(t, ts)
	ts.discovery = gitlabDiscovery(ts)
	ts.glPollStates = []accessTokenResponse{
		{Error: "authorization_pending"},
		{Error: "slow_down"},
		{AccessToken: "glpat_late"},
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "login", "--backend-url", ts.srv.URL, "--provider", "gitlab"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitOK, stderr.String())
	}
	if len(ts.mintRequests) != 1 || ts.mintRequests[0].AccessToken != "glpat_late" {
		t.Fatalf("expected mint with glpat_late, got %+v", ts.mintRequests)
	}
}

// The terminal GitLab poll states abort the login and store nothing.
func TestTokenLogin_GitLabTerminalPollStates(t *testing.T) {
	for _, tc := range []struct {
		name      string
		state     accessTokenResponse
		wantInErr string
	}{
		{"access_denied", accessTokenResponse{Error: "access_denied"}, "denied"},
		{"expired_token", accessTokenResponse{Error: "expired_token"}, "timed out"},
		{"unknown", accessTokenResponse{Error: "invalid_grant"}, "invalid_grant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTokenTestServer(t)
			setupTokenTest(t, ts)
			ts.discovery = gitlabDiscovery(ts)
			ts.glPollStates = []accessTokenResponse{tc.state}

			var stdout, stderr bytes.Buffer
			code := run([]string{"token", "login", "--backend-url", ts.srv.URL, "--provider", "gitlab"}, &stdout, &stderr)
			if code != exitFailure {
				t.Fatalf("exit = %d, want %d; stderr=%s", code, exitFailure, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.wantInErr) {
				t.Errorf("stderr should carry %q: %s", tc.wantInErr, stderr.String())
			}
			if _, err := credstore.Load(ts.srv.URL); err == nil {
				t.Error("credential must NOT be stored on a terminal poll state")
			}
			if len(ts.mintRequests) != 0 {
				t.Errorf("mint must not be reached, got %d", len(ts.mintRequests))
			}
		})
	}
}

// A GitLab device-code request that fails (non-2xx with an OAuth error
// body) surfaces the error and never prompts.
func TestTokenLogin_GitLabDeviceCodeRejected(t *testing.T) {
	ts := newTokenTestServer(t)
	setupTokenTest(t, ts)
	ts.discovery = gitlabDiscovery(ts)
	ts.glDeviceStatus = http.StatusUnauthorized
	ts.glDeviceBody = map[string]string{
		"error":             "invalid_client",
		"error_description": "Client authentication failed.",
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "login", "--backend-url", ts.srv.URL, "--provider", "gitlab"}, &stdout, &stderr)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitFailure, stderr.String())
	}
	if !strings.Contains(stderr.String(), "invalid_client") {
		t.Errorf("stderr should surface the OAuth error: %s", stderr.String())
	}
	if strings.Contains(stderr.String(), "To authorize, open") {
		t.Errorf("no device prompt should print when the device-code request failed: %s", stderr.String())
	}
}

// A 200 device-code response carrying no device_code is refused rather
// than prompting with an empty verification URI.
func TestTokenLogin_GitLabDeviceCodeMissing(t *testing.T) {
	ts := newTokenTestServer(t)
	setupTokenTest(t, ts)
	ts.discovery = gitlabDiscovery(ts)
	ts.glDeviceCode = deviceCodeResponse{Error: "unauthorized_client", ErrorDescription: "device flow not enabled"}

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "login", "--backend-url", ts.srv.URL, "--provider", "gitlab"}, &stdout, &stderr)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitFailure, stderr.String())
	}
	if !strings.Contains(stderr.String(), "device flow not enabled") {
		t.Errorf("stderr should surface the error_description: %s", stderr.String())
	}
	if strings.Contains(stderr.String(), "To authorize, open") {
		t.Errorf("no device prompt should print without a device_code: %s", stderr.String())
	}
}

// A backend advertising gitlab WITHOUT a base_url is a misconfiguration
// the CLI refuses loudly rather than driving a bare "/oauth/..." path.
func TestTokenLogin_GitLabMissingBaseURL(t *testing.T) {
	ts := newTokenTestServer(t)
	setupTokenTest(t, ts)
	disc := gitlabDiscovery(ts)
	disc.Providers[1].BaseURL = ""
	ts.discovery = disc

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "login", "--backend-url", ts.srv.URL, "--provider", "gitlab"}, &stdout, &stderr)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitFailure, stderr.String())
	}
	if !strings.Contains(stderr.String(), "FISHHAWKD_GITLAB_BASE_URL") {
		t.Errorf("stderr should name the missing backend config: %s", stderr.String())
	}
	if len(ts.glDeviceForms) != 0 {
		t.Errorf("no device request should be attempted without a base URL, got %d", len(ts.glDeviceForms))
	}
}

// An entry advertised with an empty client_id hits the no-client_id
// guard rather than driving the flow with "".
func TestTokenLogin_GitLabEmptyClientID(t *testing.T) {
	ts := newTokenTestServer(t)
	setupTokenTest(t, ts)
	disc := gitlabDiscovery(ts)
	disc.Providers[1].ClientID = ""
	ts.discovery = disc

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "login", "--backend-url", ts.srv.URL, "--provider", "gitlab"}, &stdout, &stderr)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitFailure, stderr.String())
	}
	if !strings.Contains(stderr.String(), "client_id") {
		t.Errorf("stderr should explain the missing client_id: %s", stderr.String())
	}
	if len(ts.glDeviceForms) != 0 {
		t.Errorf("no device request should be attempted without a client_id, got %d", len(ts.glDeviceForms))
	}
}

// A dual-forge backend still serves `--provider github` from the github
// ENTRY (not the legacy top-level mirror of it), driving GitHub's
// endpoints unchanged.
func TestTokenLogin_GitHubSelectedFromProvidersArray(t *testing.T) {
	ts := newTokenTestServer(t)
	setupTokenTest(t, ts)
	disc := gitlabDiscovery(ts)
	// Legacy top-level fields deliberately name gitlab, so a CLI that
	// read them instead of the array would pick the wrong forge.
	disc.Provider = "gitlab"
	disc.ClientID = "glcid.device"
	ts.discovery = disc

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "login", "--backend-url", ts.srv.URL}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitOK, stderr.String())
	}
	if len(ts.glDeviceForms) != 0 {
		t.Errorf("a github login must not touch GitLab's endpoints, got %d", len(ts.glDeviceForms))
	}
	if ts.ghDeviceCodeCalls != 1 {
		t.Errorf("github device-code endpoint calls = %d, want 1", ts.ghDeviceCodeCalls)
	}
	if len(ts.mintRequests) != 1 || ts.mintRequests[0].Provider != "github" {
		t.Fatalf("mint should carry provider=github, got %+v", ts.mintRequests)
	}
}

// Requesting a provider the backend does NOT advertise is refused with
// the configured set named — never silently satisfied from another
// forge's entry, which would drive the wrong instance.
func TestTokenLogin_ProviderNotAdvertised(t *testing.T) {
	ts := newTokenTestServer(t)
	setupTokenTest(t, ts)
	ts.discovery = tokenLoginDiscovery{
		Provider:  "github",
		ClientID:  "Iv1.testclient",
		Providers: []tokenLoginProviderEntry{{Provider: "github", ClientID: "Iv1.testclient"}},
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "login", "--backend-url", ts.srv.URL, "--provider", "gitlab"}, &stdout, &stderr)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitFailure, stderr.String())
	}
	out := stderr.String()
	if !strings.Contains(out, "gitlab") || !strings.Contains(out, "configured: github") {
		t.Errorf("stderr should name the requested and configured providers: %s", out)
	}
	if len(ts.glDeviceForms) != 0 || ts.ghDeviceCodeCalls != 0 {
		t.Error("no device flow should be driven for an unadvertised provider")
	}
	if len(ts.mintRequests) != 0 {
		t.Errorf("mint must not be reached, got %d", len(ts.mintRequests))
	}
}

// A backend PREDATING the providers array (legacy top-level fields only)
// still drives a github login — the forward-compat fallback.
func TestTokenLogin_LegacyDiscoveryFallback(t *testing.T) {
	ts := newTokenTestServer(t)
	setupTokenTest(t, ts)
	// No Providers array at all — exactly the pre-#2392 wire shape.
	ts.discovery = tokenLoginDiscovery{Provider: "github", ClientID: "Iv1.legacy"}

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "login", "--backend-url", ts.srv.URL}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitOK, stderr.String())
	}
	if len(ts.mintRequests) != 1 || ts.mintRequests[0].Provider != "github" {
		t.Fatalf("mint should carry provider=github, got %+v", ts.mintRequests)
	}
}

// A legacy backend cannot serve gitlab: the fallback is scoped to the
// provider the top-level fields actually name, so a gitlab request is
// refused rather than driven with a GitHub client_id.
func TestTokenLogin_LegacyDiscoveryRefusesGitLab(t *testing.T) {
	ts := newTokenTestServer(t)
	setupTokenTest(t, ts)
	ts.discovery = tokenLoginDiscovery{Provider: "github", ClientID: "Iv1.legacy"}

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "login", "--backend-url", ts.srv.URL, "--provider", "gitlab"}, &stdout, &stderr)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitFailure, stderr.String())
	}
	if !strings.Contains(stderr.String(), "configured: github") {
		t.Errorf("stderr should name the legacy configured provider: %s", stderr.String())
	}
	if len(ts.glDeviceForms) != 0 {
		t.Errorf("no GitLab device request should be attempted, got %d", len(ts.glDeviceForms))
	}
	if len(ts.mintRequests) != 0 {
		t.Errorf("mint must not be reached, got %d", len(ts.mintRequests))
	}
}

// resolveProviderEntry's branches in isolation, including the
// empty-legacy-provider default that only a hand-built response reaches.
func TestResolveProviderEntry(t *testing.T) {
	arrayed := tokenLoginDiscovery{
		Provider: "github",
		ClientID: "Iv1.top",
		Providers: []tokenLoginProviderEntry{
			{Provider: "github", ClientID: "Iv1.gh"},
			{Provider: "gitlab", ClientID: "gl.dev", BaseURL: "https://gitlab.example.com"},
		},
	}
	for _, tc := range []struct {
		name     string
		disc     tokenLoginDiscovery
		provider string
		want     tokenLoginProviderEntry
		wantErr  string
	}{
		{"array github", arrayed, "github", tokenLoginProviderEntry{Provider: "github", ClientID: "Iv1.gh"}, ""},
		{"array gitlab", arrayed, "gitlab", arrayed.Providers[1], ""},
		{"array miss", arrayed, "bitbucket", tokenLoginProviderEntry{}, "configured: github, gitlab"},
		{
			"legacy github",
			tokenLoginDiscovery{Provider: "github", ClientID: "Iv1.legacy"},
			"github",
			tokenLoginProviderEntry{Provider: "github", ClientID: "Iv1.legacy"},
			"",
		},
		{
			// A legacy body with no provider field at all: the only
			// forge a pre-#2392 backend ever served was github.
			"legacy empty provider defaults to github",
			tokenLoginDiscovery{ClientID: "Iv1.legacy"},
			"github",
			tokenLoginProviderEntry{Provider: "github", ClientID: "Iv1.legacy"},
			"",
		},
		{
			"legacy refuses other provider",
			tokenLoginDiscovery{Provider: "github", ClientID: "Iv1.legacy"},
			"gitlab",
			tokenLoginProviderEntry{},
			"configured: github",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveProviderEntry(tc.disc, tc.provider)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got entry %+v", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("entry = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// --client-id overrides the ADVERTISED device client_id for gitlab, but
// discovery is still consulted for the instance base_url — a flag cannot
// supply one, and driving the wrong instance is worse than one extra GET.
func TestTokenLogin_GitLabClientIDOverrideStillUsesDiscoveredBaseURL(t *testing.T) {
	ts := newTokenTestServer(t)
	setupTokenTest(t, ts)
	ts.discovery = gitlabDiscovery(ts)

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "login", "--backend-url", ts.srv.URL,
		"--provider", "gitlab", "--client-id", "gl.override"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitOK, stderr.String())
	}
	if len(ts.glDeviceForms) != 1 {
		t.Fatalf("want 1 device-code request against the discovered base URL, got %d", len(ts.glDeviceForms))
	}
	if got := ts.glDeviceForms[0].Get("client_id"); got != "gl.override" {
		t.Errorf("device-code client_id = %q, want the overridden id", got)
	}
}

// deviceFlowDriverFor refuses a provider it has no endpoints for. This
// is defence in depth behind the flag validation: the two lists could
// drift, and driving an unknown forge is not a failure the flow should
// discover mid-poll.
func TestDeviceFlowDriverFor_UnsupportedProvider(t *testing.T) {
	_, err := deviceFlowDriverFor(tokenLoginProviderEntry{Provider: "bitbucket", ClientID: "x"})
	if err == nil {
		t.Fatal("want an error for an unsupported provider")
	}
	if !strings.Contains(err.Error(), "bitbucket") {
		t.Errorf("error should name the provider: %v", err)
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

// TestTokenList_RendersStoredSubject is the CLI render pin (binding
// approval condition): it seeds the credential store from a RAW JSON fixture
// that references the literal "subject" key — the same key the backend
// mint-response test asserts — then runs `token list` and requires the
// subject to render, not the `-` placeholder. Writing the fixture as raw JSON
// (rather than a credstore.Credential struct) means a rename of the CLI
// decode tag would leave the subject empty and fail this test, keeping the
// key aligned across the mint-response -> credstore -> list-output round-trip.
func TestTokenList_RendersStoredSubject(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)

	dir := filepath.Join(xdg, "fishhawk")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Raw fixture: the store file is a map of backend URL -> credential,
	// and the credential carries the literal "subject" JSON key.
	fixture := `{"http://localhost:8080":{"token":"fhk_a","subject":"github:carol","provider":"github"}}`
	if err := os.WriteFile(filepath.Join(dir, "credentials"), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "list"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitOK, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "github:carol") {
		t.Errorf("token list did not render the stored subject: %s", out)
	}
	// The subject must render, not the empty-value dash placeholder.
	if strings.Contains(out, "subject: -") {
		t.Errorf("token list rendered a dash instead of the subject: %s", out)
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
