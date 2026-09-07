package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/credstore"
)

// oauthTestAS is a fake fishhawkd authorization server: RFC 8414 + RFC
// 9728 discovery documents and a public-client token endpoint. The
// AUTHORIZE leg is not served here — the "browser" seam (see
// browserThatCallsBack) parses the authorize URL the CLI hands it and
// delivers the callback the way a real user-agent redirect would, so
// the test controls state/code/error precisely.
type oauthTestAS struct {
	srv *httptest.Server

	mu         sync.Mutex
	metaStatus int // 0 → 200; else this status with an oauth_as_unconfigured envelope
	metaDoc    oauthASMetadataDoc
	prmDoc     oauthPRMDoc

	// tokenCalls is the never-dialed-seam counter: a refused callback
	// must leave it at zero.
	tokenCalls   atomic.Int32
	tokenForms   []url.Values
	tokenHeaders []http.Header
	tokenStatus  int // 0 → 200
	tokenBody    any
}

func newOAuthTestAS(t *testing.T) *oauthTestAS {
	t.Helper()
	as := &oauthTestAS{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		as.mu.Lock()
		defer as.mu.Unlock()
		if as.metaStatus != 0 {
			writeAPIError(w, as.metaStatus, "oauth_as_unconfigured", "the OAuth 2.1 authorization server is not enabled on this deployment (no --oauth-issuer configured)")
			return
		}
		writeJSON(w, http.StatusOK, as.metaDoc)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		as.mu.Lock()
		defer as.mu.Unlock()
		writeJSON(w, http.StatusOK, as.prmDoc)
	})
	mux.HandleFunc("/v0/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		as.tokenCalls.Add(1)
		as.mu.Lock()
		defer as.mu.Unlock()
		as.tokenForms = append(as.tokenForms, readForm(r))
		as.tokenHeaders = append(as.tokenHeaders, r.Header.Clone())
		if as.tokenStatus != 0 {
			writeJSON(w, as.tokenStatus, as.tokenBody)
			return
		}
		writeJSON(w, http.StatusOK, as.tokenBody)
	})
	as.srv = httptest.NewServer(mux)
	t.Cleanup(as.srv.Close)
	as.metaDoc = oauthASMetadataDoc{
		Issuer:                        as.srv.URL,
		AuthorizationEndpoint:         as.srv.URL + "/v0/oauth/authorize",
		TokenEndpoint:                 as.srv.URL + "/v0/oauth/token",
		CodeChallengeMethodsSupported: []string{"S256"},
	}
	as.prmDoc = oauthPRMDoc{Resource: as.srv.URL + "/mcp"}
	as.tokenBody = oauthTokenExchangeResponse{
		AccessToken:  "fho_access",
		TokenType:    "Bearer",
		ExpiresIn:    900,
		RefreshToken: "fhr_refresh",
		Scope:        "read:runs write:approvals",
	}
	return as
}

// setupOAuthLoginTest isolates the credential store and shrinks the
// callback window. It does NOT install a browser seam — each test does,
// because the seam is where the test chooses what callback to deliver.
func setupOAuthLoginTest(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FISHHAWK_OAUTH_CLIENT_ID", "")
	t.Setenv("FISHHAWK_TOKEN", "")
	prevWindow := oauthLoginWindow
	prevBrowser := oauthOpenBrowser
	oauthLoginWindow = 5 * time.Second
	t.Cleanup(func() {
		oauthLoginWindow = prevWindow
		oauthOpenBrowser = prevBrowser
	})
}

// authorizeCapture is what the browser seam learned from the authorize
// URL the CLI built.
type authorizeCapture struct {
	mu          sync.Mutex
	url         *url.URL
	state       string
	redirectURI string
	challenge   string
}

func (c *authorizeCapture) get() (state, redirectURI, challenge string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state, c.redirectURI, c.challenge
}

// browserThatCallsBack installs a browser seam that records the
// authorize URL and, in a goroutine (the CLI is blocking on the
// callback), delivers ONE GET to the redirect URI with the query
// produced by mutate(state) — mutate is where a test substitutes a
// mismatched state, an error parameter, or an absent code.
func browserThatCallsBack(t *testing.T, launchErr error, mutate func(state string) url.Values) *authorizeCapture {
	t.Helper()
	cap := &authorizeCapture{}
	oauthOpenBrowser = func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			t.Errorf("browser seam: authorize URL %q does not parse: %v", raw, err)
			return err
		}
		q := u.Query()
		cap.mu.Lock()
		cap.url = u
		cap.state = q.Get("state")
		cap.redirectURI = q.Get("redirect_uri")
		cap.challenge = q.Get("code_challenge")
		cap.mu.Unlock()
		go func() {
			cbURL, err := url.Parse(cap.redirectURI)
			if err != nil {
				return
			}
			cbURL.RawQuery = mutate(cap.state).Encode()
			resp, err := http.Get(cbURL.String())
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		return launchErr
	}
	return cap
}

// callbackWithCode is the well-formed callback: matching state, a code,
// and the RFC 9207 iss naming the discovered issuer.
func callbackWithCode(issuer string) func(string) url.Values {
	return func(state string) url.Values {
		v := url.Values{}
		v.Set("code", "authcode-1")
		v.Set("state", state)
		v.Set("iss", issuer)
		return v
	}
}

func runOAuthLoginCmd(t *testing.T, as *oauthTestAS, extra ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb strings.Builder
	args := append([]string{"login", "--oauth", "--backend-url", as.srv.URL, "--timeout", "5s"}, extra...)
	code = runToken(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestOAuthLogin_HappyPathStoresRefreshableCredential(t *testing.T) {
	as := newOAuthTestAS(t)
	setupOAuthLoginTest(t)
	cap := browserThatCallsBack(t, nil, callbackWithCode(as.srv.URL))
	before := time.Now()

	code, stdout, stderr := runOAuthLoginCmd(t, as, "--oauth-client-id", "cli-under-test")
	if code != exitOK {
		t.Fatalf("exit=%d, want 0\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}

	// The authorize URL: every RFC 6749 §4.1.1 + PKCE + RFC 8707 parameter,
	// no scope (the registration decides), and the URL ALWAYS printed.
	state, redirectURI, challenge := cap.get()
	q := cap.url.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != "cli-under-test" ||
		q.Get("code_challenge_method") != "S256" || q.Get("resource") != as.srv.URL+"/mcp" {
		t.Errorf("authorize URL params = %v", q)
	}
	if q.Has("scope") {
		t.Errorf("authorize URL requested a scope; the registration must decide: %v", q)
	}
	if !strings.HasPrefix(cap.url.String(), as.srv.URL+"/v0/oauth/authorize?") {
		t.Errorf("authorize URL = %s, want the discovered authorization_endpoint", cap.url)
	}
	if !strings.Contains(stderr, cap.url.String()) {
		t.Errorf("stderr did not print the authorize URL for headless terminals:\n%s", stderr)
	}
	if len(state) < 43 || len(challenge) != 43 {
		t.Errorf("state/challenge sizes: state=%d challenge=%d", len(state), len(challenge))
	}
	if !strings.HasPrefix(redirectURI, "http://127.0.0.1:") || !strings.HasSuffix(redirectURI, "/callback") {
		t.Errorf("redirect_uri = %q, want http://127.0.0.1:<port>/callback", redirectURI)
	}

	// The token exchange: a PUBLIC client, and the verifier is the
	// preimage of the challenge the authorize leg carried.
	if got := as.tokenCalls.Load(); got != 1 {
		t.Fatalf("token endpoint dialed %d times, want 1", got)
	}
	form, hdr := as.tokenForms[0], as.tokenHeaders[0]
	if form.Get("grant_type") != "authorization_code" || form.Get("code") != "authcode-1" ||
		form.Get("client_id") != "cli-under-test" || form.Get("redirect_uri") != redirectURI ||
		form.Get("resource") != as.srv.URL+"/mcp" {
		t.Errorf("token form = %v", form)
	}
	if form.Has("client_secret") {
		t.Errorf("token form carried a client_secret for a public client: %v", form)
	}
	if hdr.Get("Authorization") != "" {
		t.Errorf("token request carried an Authorization header: %q", hdr.Get("Authorization"))
	}
	if pkceChallengeS256(form.Get("code_verifier")) != challenge {
		t.Errorf("code_verifier %q is not the preimage of code_challenge %q", form.Get("code_verifier"), challenge)
	}

	// The stored credential carries everything credstore's refresh needs.
	cred, err := credstore.Load(as.srv.URL)
	if err != nil {
		t.Fatalf("credstore.Load: %v", err)
	}
	if cred.Token != "fho_access" || cred.RefreshToken != "fhr_refresh" || cred.ClientID != "cli-under-test" ||
		cred.TokenEndpoint != as.srv.URL+"/v0/oauth/token" {
		t.Errorf("stored credential = %+v", cred)
	}
	if cred.IssuedAt == nil || cred.ExpiresAt == nil {
		t.Fatalf("stored credential lacks issued_at/expires_at: %+v", cred)
	}
	if cred.IssuedAt.Before(before) || cred.ExpiresAt.Sub(*cred.IssuedAt) != 900*time.Second {
		t.Errorf("issued_at=%s expires_at=%s, want lifetime 900s from now", cred.IssuedAt, cred.ExpiresAt)
	}
	if strings.Join(cred.Scopes, " ") != "read:runs write:approvals" {
		t.Errorf("scopes = %v", cred.Scopes)
	}
	if !cred.Refreshable() {
		t.Errorf("stored credential is not Refreshable: %+v", cred)
	}
	for _, want := range []string{"Logged in to " + as.srv.URL, "client:  cli-under-test", "refreshed automatically"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
}

func TestOAuthLogin_DefaultClientID(t *testing.T) {
	as := newOAuthTestAS(t)
	setupOAuthLoginTest(t)
	cap := browserThatCallsBack(t, nil, callbackWithCode(as.srv.URL))
	if code, _, stderr := runOAuthLoginCmd(t, as); code != exitOK {
		t.Fatalf("exit=%d: %s", code, stderr)
	}
	if got := cap.url.Query().Get("client_id"); got != defaultOAuthClientID {
		t.Errorf("client_id = %q, want the default %q", got, defaultOAuthClientID)
	}
}

// TestOAuthLogin_StateMismatchNeverExchanges is the counterfactual
// vehicle for the state comparison: the token endpoint is REACHABLE (a
// real in-test server) with a dial counter, and the mismatched state is
// seeded BY CONSTRUCTION — a second independently drawn 32-byte value.
// Delete the ConstantTimeCompare in oauthCallback.evaluate and the
// counter reads 1.
func TestOAuthLogin_StateMismatchNeverExchanges(t *testing.T) {
	as := newOAuthTestAS(t)
	setupOAuthLoginTest(t)
	other, err := randomToken(oauthStateBytes)
	if err != nil {
		t.Fatal(err)
	}
	browserThatCallsBack(t, nil, func(string) url.Values {
		v := url.Values{}
		v.Set("code", "authcode-1")
		v.Set("state", other)
		return v
	})
	code, _, stderr := runOAuthLoginCmd(t, as)
	if code != exitFailure {
		t.Fatalf("exit=%d, want 1\nstderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "state that does not match") {
		t.Errorf("stderr did not name the state mismatch:\n%s", stderr)
	}
	if got := as.tokenCalls.Load(); got != 0 {
		t.Errorf("token endpoint dialed %d times after a state mismatch, want 0", got)
	}
	if _, err := credstore.Load(as.srv.URL); err == nil {
		t.Errorf("a credential was stored after a refused callback")
	}
}

func TestOAuthLogin_AuthorizationErrorParamSurfaced(t *testing.T) {
	as := newOAuthTestAS(t)
	setupOAuthLoginTest(t)
	browserThatCallsBack(t, nil, func(state string) url.Values {
		v := url.Values{}
		v.Set("state", state)
		v.Set("error", "access_denied")
		v.Set("error_description", "the operator declined consent")
		return v
	})
	code, _, stderr := runOAuthLoginCmd(t, as)
	if code != exitFailure {
		t.Fatalf("exit=%d, want 1\nstderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "access_denied") || !strings.Contains(stderr, "the operator declined consent") {
		t.Errorf("stderr did not surface the AS error + description:\n%s", stderr)
	}
	if got := as.tokenCalls.Load(); got != 0 {
		t.Errorf("token endpoint dialed %d times, want 0", got)
	}
}

func TestOAuthLogin_MissingCodeRefused(t *testing.T) {
	as := newOAuthTestAS(t)
	setupOAuthLoginTest(t)
	browserThatCallsBack(t, nil, func(state string) url.Values {
		v := url.Values{}
		v.Set("state", state)
		return v
	})
	code, _, stderr := runOAuthLoginCmd(t, as)
	if code != exitFailure {
		t.Fatalf("exit=%d, want 1\nstderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "carried no code") {
		t.Errorf("stderr did not name the absent code:\n%s", stderr)
	}
	if got := as.tokenCalls.Load(); got != 0 {
		t.Errorf("token endpoint dialed %d times, want 0", got)
	}
}

// TestOAuthLogin_SecondCallbackRefused drives the handler directly so
// the second request deterministically arrives while the handler is
// still live (in the full flow the listener may already be shutting
// down, which would "refuse" for the wrong reason).
func TestOAuthLogin_SecondCallbackRefused(t *testing.T) {
	cb := newOAuthCallback("state-1", "")
	first := httptest.NewRecorder()
	cb.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/callback?code=one&state=state-1", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first callback status = %d, want 200", first.Code)
	}
	res := <-cb.result
	if res.err != nil || res.code != "one" {
		t.Fatalf("first result = %+v", res)
	}

	second := httptest.NewRecorder()
	cb.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/callback?code=two&state=state-1", nil))
	if second.Code != http.StatusConflict {
		t.Errorf("second callback status = %d, want 409", second.Code)
	}
	select {
	case res := <-cb.result:
		t.Errorf("second callback delivered a result: %+v", res)
	default:
	}
}

func TestOAuthLogin_NonCallbackPathDoesNotConsume(t *testing.T) {
	cb := newOAuthCallback("state-1", "")
	fav := httptest.NewRecorder()
	cb.ServeHTTP(fav, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))
	if fav.Code != http.StatusNotFound {
		t.Errorf("favicon status = %d, want 404", fav.Code)
	}
	rec := httptest.NewRecorder()
	cb.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/callback?code=one&state=state-1", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("callback after a favicon probe = %d, want 200 (the probe must not consume the one shot)", rec.Code)
	}
}

func TestOAuthLogin_IssuerMismatchRefused(t *testing.T) {
	cb := newOAuthCallback("state-1", "https://as.example")
	rec := httptest.NewRecorder()
	cb.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/callback?code=one&state=state-1&iss=https://evil.example", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	res := <-cb.result
	if res.err == nil || !strings.Contains(res.err.Error(), "issuer") || res.code != "" {
		t.Errorf("result = %+v, want an issuer-mismatch refusal with no code", res)
	}
}

func TestOAuthLogin_TokenExchangeErrorEnvelopeSurfaced(t *testing.T) {
	as := newOAuthTestAS(t)
	setupOAuthLoginTest(t)
	as.tokenStatus = http.StatusBadRequest
	as.tokenBody = oauthTokenExchangeResponse{Error: "invalid_grant", ErrorDescription: "PKCE verification failed"}
	browserThatCallsBack(t, nil, callbackWithCode(as.srv.URL))
	code, _, stderr := runOAuthLoginCmd(t, as)
	if code != exitFailure {
		t.Fatalf("exit=%d, want 1\nstderr=%s", code, stderr)
	}
	// The DECODED envelope, not a raw-body dump: the fallback "unexpected
	// status 400: {...}" would also contain both strings, so the assertion
	// is on the formatted `refused (HTTP 400 invalid_grant): ...` form.
	if !strings.Contains(stderr, "refused (HTTP 400 invalid_grant): PKCE verification failed") {
		t.Errorf("stderr did not decode the RFC 6749 §5.2 envelope into the named error code + description:\n%s", stderr)
	}
	if _, err := credstore.Load(as.srv.URL); err == nil {
		t.Errorf("a credential was stored after a refused exchange")
	}
}

func TestOAuthLogin_TokenExchangeMissingAccessTokenRefused(t *testing.T) {
	as := newOAuthTestAS(t)
	setupOAuthLoginTest(t)
	as.tokenBody = oauthTokenExchangeResponse{RefreshToken: "only-a-refresh"}
	browserThatCallsBack(t, nil, callbackWithCode(as.srv.URL))
	code, _, stderr := runOAuthLoginCmd(t, as)
	if code != exitFailure || !strings.Contains(stderr, "no access_token") {
		t.Errorf("exit=%d stderr=%s, want 1 + 'no access_token'", code, stderr)
	}
}

func TestOAuthLogin_TokenExchangeNonJSONRefused(t *testing.T) {
	as := newOAuthTestAS(t)
	setupOAuthLoginTest(t)
	as.tokenBody = "<html>not json</html>"
	browserThatCallsBack(t, nil, callbackWithCode(as.srv.URL))
	code, _, stderr := runOAuthLoginCmd(t, as)
	if code != exitFailure || !strings.Contains(stderr, "not JSON") {
		t.Errorf("exit=%d stderr=%s, want 1 + 'not JSON'", code, stderr)
	}
}

func TestOAuthLogin_ExpiresInAbsentFallsBackToDefaultLifetime(t *testing.T) {
	as := newOAuthTestAS(t)
	setupOAuthLoginTest(t)
	as.tokenBody = oauthTokenExchangeResponse{AccessToken: "fho_a", RefreshToken: "fhr_r"}
	browserThatCallsBack(t, nil, callbackWithCode(as.srv.URL))
	if code, _, stderr := runOAuthLoginCmd(t, as); code != exitOK {
		t.Fatalf("exit=%d: %s", code, stderr)
	}
	cred, err := credstore.Load(as.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if cred.ExpiresAt == nil || cred.ExpiresAt.Sub(*cred.IssuedAt) != credstore.DefaultLifetime {
		t.Errorf("lifetime = %v, want credstore.DefaultLifetime; a refreshable credential must never become non-expiring", cred.ExpiresAt)
	}
}

func TestOAuthLogin_DiscoveryUnconfigured(t *testing.T) {
	as := newOAuthTestAS(t)
	setupOAuthLoginTest(t)
	as.metaStatus = http.StatusServiceUnavailable
	oauthOpenBrowser = func(string) error { t.Error("browser launched with no AS"); return nil }
	code, _, stderr := runOAuthLoginCmd(t, as)
	if code != exitFailure {
		t.Fatalf("exit=%d, want 1\nstderr=%s", code, stderr)
	}
	// The server's own message already says "--oauth-issuer"; the CLI's
	// hint is what adds the env-var spelling, so THAT is asserted.
	if !strings.Contains(stderr, "oauth_as_unconfigured") || !strings.Contains(stderr, oauthASUnconfiguredHint) {
		t.Errorf("stderr must surface the envelope verbatim plus the --oauth-issuer / FISHHAWKD_OAUTH_ISSUER hint:\n%s", stderr)
	}
}

func TestOAuthLogin_DiscoveryWithoutS256Refused(t *testing.T) {
	as := newOAuthTestAS(t)
	setupOAuthLoginTest(t)
	as.metaDoc.CodeChallengeMethodsSupported = []string{"plain"}
	oauthOpenBrowser = func(string) error { t.Error("browser launched against a non-S256 AS"); return nil }
	code, _, stderr := runOAuthLoginCmd(t, as)
	if code != exitFailure || !strings.Contains(stderr, "S256") {
		t.Errorf("exit=%d stderr=%s, want 1 naming S256", code, stderr)
	}
}

func TestOAuthLogin_DiscoveryMissingEndpointsRefused(t *testing.T) {
	as := newOAuthTestAS(t)
	setupOAuthLoginTest(t)
	as.metaDoc.TokenEndpoint = ""
	oauthOpenBrowser = func(string) error { t.Error("browser launched with no token endpoint"); return nil }
	code, _, stderr := runOAuthLoginCmd(t, as)
	if code != exitFailure || !strings.Contains(stderr, "token_endpoint") {
		t.Errorf("exit=%d stderr=%s, want 1 naming token_endpoint", code, stderr)
	}
}

func TestOAuthLogin_ResourceDiscoveryMissingRefused(t *testing.T) {
	as := newOAuthTestAS(t)
	setupOAuthLoginTest(t)
	as.prmDoc.Resource = ""
	oauthOpenBrowser = func(string) error { t.Error("browser launched with no resource"); return nil }
	code, _, stderr := runOAuthLoginCmd(t, as)
	if code != exitFailure || !strings.Contains(stderr, "resource identifier") {
		t.Errorf("exit=%d stderr=%s, want 1 naming the resource identifier", code, stderr)
	}
}

func TestOAuthLogin_BrowserLaunchFailureStillPrintsURLAndCompletes(t *testing.T) {
	as := newOAuthTestAS(t)
	setupOAuthLoginTest(t)
	launchErr := &net.OpError{Op: "exec", Err: net.UnknownNetworkError("no browser")}
	cap := browserThatCallsBack(t, launchErr, callbackWithCode(as.srv.URL))
	code, _, stderr := runOAuthLoginCmd(t, as)
	if code != exitOK {
		t.Fatalf("exit=%d, want 0 (a headless terminal copies the URL)\nstderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "could not launch a browser") || !strings.Contains(stderr, cap.url.String()) {
		t.Errorf("stderr must warn about the launch failure AND still print the URL:\n%s", stderr)
	}
}

func TestOAuthLogin_CallbackWindowElapses(t *testing.T) {
	as := newOAuthTestAS(t)
	setupOAuthLoginTest(t)
	oauthLoginWindow = 50 * time.Millisecond
	oauthOpenBrowser = func(string) error { return nil } // never calls back
	code, _, stderr := runOAuthLoginCmd(t, as)
	if code != exitFailure || !strings.Contains(stderr, "no authorization callback arrived") {
		t.Errorf("exit=%d stderr=%s, want 1 + the window message", code, stderr)
	}
	if got := as.tokenCalls.Load(); got != 0 {
		t.Errorf("token endpoint dialed %d times, want 0", got)
	}
}

// TestOAuthLogin_ListenerBoundToLoopbackOnly pins the production
// listener seam: bound to 127.0.0.1 explicitly, never a wildcard.
func TestOAuthLogin_ListenerBoundToLoopbackOnly(t *testing.T) {
	ln, err := oauthListen()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener addr %T, want *net.TCPAddr", ln.Addr())
	}
	if !addr.IP.Equal(net.IPv4(127, 0, 0, 1)) || !addr.IP.IsLoopback() || addr.IP.IsUnspecified() {
		t.Errorf("listener bound to %s, want 127.0.0.1 (never a wildcard)", addr)
	}
	if addr.Port == 0 {
		t.Errorf("listener has no ephemeral port")
	}
	if got := loopbackRedirectURI(ln.Addr()); got != "http://127.0.0.1:"+strconv.Itoa(addr.Port)+"/callback" {
		t.Errorf("redirect URI = %q", got)
	}
}

func TestPKCE_S256(t *testing.T) {
	a, err := newPKCE()
	if err != nil {
		t.Fatal(err)
	}
	b, err := newPKCE()
	if err != nil {
		t.Fatal(err)
	}
	if a.verifier == b.verifier || a.challenge == b.challenge {
		t.Errorf("two PKCE pairs collided: %+v %+v", a, b)
	}
	if len(a.verifier) != 43 {
		t.Errorf("verifier length %d, want 43 (32 crypto/rand bytes, RFC 7636 §4.1 minimum)", len(a.verifier))
	}
	sum := sha256.Sum256([]byte(a.verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); a.challenge != want {
		t.Errorf("challenge = %q, want BASE64URL(SHA256(verifier)) = %q", a.challenge, want)
	}
	if strings.ContainsAny(a.verifier+a.challenge, "+/=") {
		t.Errorf("PKCE values must be unpadded base64url: %+v", a)
	}
}

func TestBuildAuthorizeURL_RefusesRelativeEndpoint(t *testing.T) {
	if _, err := buildAuthorizeURL("/v0/oauth/authorize", authorizeParams{}); err == nil {
		t.Error("a relative authorization_endpoint was accepted")
	}
}

// cliRedirectFixture mirrors testdata/wire/oauth_cli_redirect_uri.json —
// the shared fixture binding this generator to the backend's redirect
// matcher (approval condition 4). Its backend-side twin,
// TestMatchRedirectURI_CLIGeneratedLoopbackMatchesRegistered, proves the
// shape MATCHES; this side proves the CLI GENERATES it and that
// cli/README.md documents the register command the fixture names.
type cliRedirectFixture struct {
	Host            string `json:"host"`
	Path            string `json:"path"`
	Registered      string `json:"registered_redirect_uri"`
	Template        string `json:"generated_redirect_uri_template"`
	SamplePorts     []int  `json:"sample_ports"`
	RegisterCommand string `json:"register_command"`
}

func TestOAuthLogin_RedirectURIShapeMatchesSharedFixture(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	raw, err := os.ReadFile(filepath.Join(root, "testdata", "wire", "oauth_cli_redirect_uri.json"))
	if err != nil {
		t.Fatalf("read shared fixture: %v", err)
	}
	var fx cliRedirectFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("decode shared fixture: %v", err)
	}
	if fx.Host != oauthLoopbackHost || fx.Path != oauthCallbackPath {
		t.Errorf("fixture host/path = %q %q, generator uses %q %q", fx.Host, fx.Path, oauthLoopbackHost, oauthCallbackPath)
	}
	for _, port := range fx.SamplePorts {
		addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
		want := strings.ReplaceAll(fx.Template, "{port}", strconv.Itoa(port))
		if got := loopbackRedirectURI(addr); got != want {
			t.Errorf("loopbackRedirectURI(port %d) = %q, want the fixture shape %q", port, got, want)
		}
	}
	if !strings.Contains(fx.RegisterCommand, "--redirect-uri "+fx.Registered) || !strings.Contains(fx.RegisterCommand, "--client-id "+defaultOAuthClientID) {
		t.Errorf("fixture register command %q does not register %q for the default client %q", fx.RegisterCommand, fx.Registered, defaultOAuthClientID)
	}
	readme, err := os.ReadFile(filepath.Join(root, "cli", "README.md"))
	if err != nil {
		t.Fatalf("read cli/README.md: %v", err)
	}
	if !strings.Contains(string(readme), fx.RegisterCommand) {
		t.Errorf("cli/README.md does not document the one-time register command the fixture names:\n%s", fx.RegisterCommand)
	}
}

// TestOAuthLogin_TokenExchangeRefusesRedirectAndLeaksNothing is the
// behavioral half of the credential-exfiltration concern on the CLI's
// authorization-code leg: the exchange POST carries the code, the PKCE
// code_verifier and the client_id, and a 307/308 replays that body at
// whatever origin the response names.
//
// The hop is a REACHABLE in-test server (an unreachable address would
// produce a dial error indistinguishable from the refusal), it records
// every request and body it sees, and the assertion is that it saw
// ZERO — plus no credential was stored.
func TestOAuthLogin_TokenExchangeRefusesRedirectAndLeaksNothing(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect, http.StatusFound} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var hopHits atomic.Int32
			var hopMu sync.Mutex
			var hopBodies []string
			hop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hopHits.Add(1)
				b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
				hopMu.Lock()
				hopBodies = append(hopBodies, r.URL.RawQuery+" "+string(b))
				hopMu.Unlock()
				writeJSON(w, http.StatusOK, oauthTokenExchangeResponse{AccessToken: "stolen", ExpiresIn: 900})
			}))
			defer hop.Close()

			as := newOAuthTestAS(t)
			setupOAuthLoginTest(t)
			redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, hop.URL+"/v0/oauth/token", status)
			}))
			defer redirector.Close()
			as.mu.Lock()
			as.metaDoc.TokenEndpoint = redirector.URL + "/v0/oauth/token"
			as.mu.Unlock()

			browserThatCallsBack(t, nil, callbackWithCode(as.srv.URL))
			code, _, stderr := runOAuthLoginCmd(t, as)
			if code != exitFailure {
				t.Fatalf("exit=%d, want 1 (the redirect must be refused)\nstderr=%s", code, stderr)
			}
			if n := hopHits.Load(); n != 0 {
				hopMu.Lock()
				bodies := hopBodies
				hopMu.Unlock()
				t.Fatalf("the redirect destination received %d request(s): %q — the authorization code and PKCE verifier were exfiltrated", n, bodies)
			}
			if !strings.Contains(stderr, "refusing to follow a redirect") {
				t.Errorf("stderr does not name the redirect refusal:\n%s", stderr)
			}
			if _, err := credstore.Load(as.srv.URL); err == nil {
				t.Errorf("a credential was stored after a refused exchange")
			}
		})
	}
}
