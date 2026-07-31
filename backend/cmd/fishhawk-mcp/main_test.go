package main

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/credstore"
)

// noCred is a loadCred seam that must never be called — used by the
// rung-1 tests to prove an explicit env token short-circuits the
// store lookup entirely.
func noCred(t *testing.T) func(string) (credstore.Credential, error) {
	t.Helper()
	return func(string) (credstore.Credential, error) {
		t.Fatalf("loadCred must not be called when FISHHAWK_API_TOKEN is set")
		return credstore.Credential{}, nil
	}
}

// credFunc returns a loadCred seam that hands back a fixed credential
// (and error) regardless of the URL, so a table test can drive each
// ladder branch without touching the filesystem.
func credFunc(cred credstore.Credential, err error) func(string) (credstore.Credential, error) {
	return func(string) (credstore.Credential, error) {
		return cred, err
	}
}

func TestParseFlags_DefaultsToStdio(t *testing.T) {
	tf, err := parseFlags([]string{"fishhawk-mcp"}, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if tf.transport != transportStdio {
		t.Errorf("transport = %q, want %q", tf.transport, transportStdio)
	}
	if tf.addr != defaultHTTPAddr {
		t.Errorf("addr = %q, want %q", tf.addr, defaultHTTPAddr)
	}
}

func TestParseFlags_HTTPTransport(t *testing.T) {
	tf, err := parseFlags([]string{"fishhawk-mcp", "--transport", "http", "--addr", "127.0.0.1:9000"}, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if tf.transport != transportHTTP {
		t.Errorf("transport = %q, want %q", tf.transport, transportHTTP)
	}
	if tf.addr != "127.0.0.1:9000" {
		t.Errorf("addr = %q, want 127.0.0.1:9000", tf.addr)
	}
}

func TestParseFlags_RejectsUnknownTransport(t *testing.T) {
	_, err := parseFlags([]string{"fishhawk-mcp", "--transport", "grpc"}, io.Discard)
	if err == nil {
		t.Fatal("expected an error for an unknown --transport value")
	}
	if !strings.Contains(err.Error(), "grpc") {
		t.Errorf("error should name the offending value; got %q", err.Error())
	}
}

func TestLoadConfig_HappyPath(t *testing.T) {
	env := map[string]string{
		"FISHHAWK_BACKEND_URL": "https://app.fishhawk.example.com",
		"FISHHAWK_API_TOKEN":   "tok_abc123",
	}
	cfg, err := loadConfig(envFunc(env), noCred(t))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.backendURL != "https://app.fishhawk.example.com" {
		t.Errorf("backendURL = %q", cfg.backendURL)
	}
	if cfg.apiToken != "tok_abc123" {
		t.Errorf("apiToken = %q", cfg.apiToken)
	}
}

func TestLoadConfig_BackendURLDefaultsToLocalhost(t *testing.T) {
	// Mirror the CLI's default so an operator who's only set the
	// token can flip between cli and mcp without re-configuring.
	env := map[string]string{
		"FISHHAWK_API_TOKEN": "tok",
	}
	cfg, err := loadConfig(envFunc(env), noCred(t))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.backendURL != "http://localhost:8080" {
		t.Errorf("backendURL = %q, want http://localhost:8080", cfg.backendURL)
	}
}

func TestLoadConfig_BackendURLTrailingSlashStripped(t *testing.T) {
	// URL concatenation at request time appends `/v0/…` — keeping
	// the trailing slash would produce `//v0/…` which works on
	// servers but reads ugly in logs and reverse proxies sometimes
	// reject it.
	env := map[string]string{
		"FISHHAWK_BACKEND_URL": "https://app.fishhawk.example.com/",
		"FISHHAWK_API_TOKEN":   "tok",
	}
	cfg, err := loadConfig(envFunc(env), noCred(t))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.backendURL != "https://app.fishhawk.example.com" {
		t.Errorf("backendURL = %q (trailing slash not stripped)", cfg.backendURL)
	}
}

// M1: an explicit env token wins outright and the store is never
// consulted — noCred(t) t.Fatals if loadCred is called. This is what
// keeps the in-runner precedence (runner stamps FISHHAWK_API_TOKEN)
// intact so a stored operator credential can never shadow it.
func TestLoadConfig_EnvTokenWins_StoreUntouched(t *testing.T) {
	env := map[string]string{
		"FISHHAWK_BACKEND_URL": "http://localhost:8080",
		"FISHHAWK_API_TOKEN":   "tok_env",
	}
	cfg, err := loadConfig(envFunc(env), noCred(t))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.apiToken != "tok_env" {
		t.Errorf("apiToken = %q, want tok_env", cfg.apiToken)
	}
}

// M2: env empty → the credential stored for the backend URL is used.
func TestLoadConfig_StoreHitResolvesToken(t *testing.T) {
	env := map[string]string{"FISHHAWK_BACKEND_URL": "http://localhost:8080"}
	cfg, err := loadConfig(envFunc(env), credFunc(credstore.Credential{Token: "fhk_stored"}, nil))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.apiToken != "fhk_stored" {
		t.Errorf("apiToken = %q, want fhk_stored", cfg.apiToken)
	}
}

// The store is keyed by the trailing-slash-trimmed URL, and loadConfig
// trims FISHHAWK_BACKEND_URL before the lookup, so a slashed env value
// still resolves the credential stored under the unslashed key. This
// asserts the URL loadCred is HANDED is already normalized.
func TestLoadConfig_StoreLookupUsesNormalizedURL(t *testing.T) {
	env := map[string]string{"FISHHAWK_BACKEND_URL": "http://localhost:8080/"}
	var gotURL string
	loadCred := func(u string) (credstore.Credential, error) {
		gotURL = u
		return credstore.Credential{Token: "fhk_norm"}, nil
	}
	cfg, err := loadConfig(envFunc(env), loadCred)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if gotURL != "http://localhost:8080" {
		t.Errorf("loadCred called with %q, want the unslashed key http://localhost:8080", gotURL)
	}
	if cfg.apiToken != "fhk_norm" {
		t.Errorf("apiToken = %q, want fhk_norm", cfg.apiToken)
	}
}

// assertRemediation is common to all four fail-closed messages: each
// must name the login command and the looked-up backend URL so the
// operator has an actionable startup error, not a bare 401 later.
func assertRemediation(t *testing.T, err error, url string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a fail-closed error, got nil")
	}
	if !strings.Contains(err.Error(), "fishhawk token login") {
		t.Errorf("error should name `fishhawk token login`; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), url) {
		t.Errorf("error should name the backend URL %q; got %q", url, err.Error())
	}
}

const ladderURL = "http://localhost:8080"

func ladderEnv() map[string]string {
	return map[string]string{"FISHHAWK_BACKEND_URL": ladderURL}
}

// M3: no credential stored → a fail-closed error carrying a substring
// UNIQUE to the not-found branch.
func TestLoadConfig_NotFound(t *testing.T) {
	_, err := loadConfig(envFunc(ladderEnv()), credFunc(credstore.Credential{}, credstore.ErrNotFound))
	assertRemediation(t, err, ladderURL)
	if !strings.Contains(err.Error(), "no Fishhawk credential stored") {
		t.Errorf("not-found error missing its unique substring; got %q", err.Error())
	}
}

// M4: a credential exists but its token is empty → a fail-closed error
// with a substring UNIQUE to the empty-token branch, never a config
// carrying an empty bearer.
func TestLoadConfig_EmptyStoredToken(t *testing.T) {
	cfg, err := loadConfig(envFunc(ladderEnv()), credFunc(credstore.Credential{Token: ""}, nil))
	assertRemediation(t, err, ladderURL)
	if !strings.Contains(err.Error(), "empty token") {
		t.Errorf("empty-token error missing its unique substring; got %q", err.Error())
	}
	if cfg.apiToken != "" {
		t.Errorf("empty-token branch must not resolve a token; got %q", cfg.apiToken)
	}
}

// CONDITION 1: the not-found and empty-stored-token messages must be
// provably DISTINCT — a suite that would still pass if the empty-token
// branch reused the not-found wording does not satisfy the condition.
func TestLoadConfig_NotFoundAndEmptyTokenErrorsDiffer(t *testing.T) {
	_, notFound := loadConfig(envFunc(ladderEnv()), credFunc(credstore.Credential{}, credstore.ErrNotFound))
	_, empty := loadConfig(envFunc(ladderEnv()), credFunc(credstore.Credential{Token: ""}, nil))
	if notFound == nil || empty == nil {
		t.Fatalf("both branches must error: notFound=%v empty=%v", notFound, empty)
	}
	if notFound.Error() == empty.Error() {
		t.Fatalf("not-found and empty-token errors must differ, both were %q", notFound.Error())
	}
}

// M5: an expired credential → a fail-closed error with a substring
// UNIQUE to the expiry branch, naming the expiry.
func TestLoadConfig_ExpiredCredential(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	_, err := loadConfig(envFunc(ladderEnv()), credFunc(credstore.Credential{Token: "fhk_old", ExpiresAt: &past}, nil))
	assertRemediation(t, err, ladderURL)
	if !strings.Contains(err.Error(), "expired at") {
		t.Errorf("expired error missing its unique substring; got %q", err.Error())
	}
}

// M5-control: a NIL ExpiresAt means non-expiring (v0 tokens do not
// expire) and MUST be accepted — reading nil as expired would refuse
// every field credential.
func TestLoadConfig_NilExpiryAccepted(t *testing.T) {
	cfg, err := loadConfig(envFunc(ladderEnv()), credFunc(credstore.Credential{Token: "fhk_live", ExpiresAt: nil}, nil))
	if err != nil {
		t.Fatalf("nil expiry must be accepted as non-expiring, got %v", err)
	}
	if cfg.apiToken != "fhk_live" {
		t.Errorf("apiToken = %q, want fhk_live", cfg.apiToken)
	}
}

// A future expiry is also accepted — only a past, non-nil expiry fails.
func TestLoadConfig_FutureExpiryAccepted(t *testing.T) {
	future := time.Now().Add(time.Hour)
	cfg, err := loadConfig(envFunc(ladderEnv()), credFunc(credstore.Credential{Token: "fhk_future", ExpiresAt: &future}, nil))
	if err != nil {
		t.Fatalf("future expiry must be accepted, got %v", err)
	}
	if cfg.apiToken != "fhk_future" {
		t.Errorf("apiToken = %q, want fhk_future", cfg.apiToken)
	}
}

// M6: a corrupt/unreadable store (a non-ErrNotFound error) → the error
// is surfaced wrapped, with a substring UNIQUE to the corrupt branch
// and distinct from not-found, so a corrupt store is loud rather than
// silently re-read as "no credential".
func TestLoadConfig_CorruptStore(t *testing.T) {
	parseErr := errors.New("credstore: parse /home/x/.config/fishhawk/credentials: unexpected end of JSON input")
	_, err := loadConfig(envFunc(ladderEnv()), credFunc(credstore.Credential{}, parseErr))
	assertRemediation(t, err, ladderURL)
	if !strings.Contains(err.Error(), "cannot read the Fishhawk credential store") {
		t.Errorf("corrupt-store error missing its unique substring; got %q", err.Error())
	}
	// The underlying error must be wrapped so `errors.Is`/`%w` chains
	// carry the file path the operator needs.
	if !errors.Is(err, parseErr) {
		t.Errorf("corrupt-store error must wrap the underlying parse error; got %q", err.Error())
	}
	if strings.Contains(err.Error(), "no Fishhawk credential stored") {
		t.Errorf("corrupt-store wording must be distinct from not-found; got %q", err.Error())
	}
}

// TestLoadConfig_StoredCredentialReachesAPIClient crosses the module
// seam with the REAL credstore package (not a stub): it writes a
// credential with credstore.Store, drives loadConfig(getenv,
// credstore.Load) → newAPIClient, and asserts the constructed client
// carries the stored token as its bearer — proving credstore module →
// mcp config → apiClient holds across the module boundary that
// per-layer units would each pass while leaving the seam untested.
func TestLoadConfig_StoredCredentialReachesAPIClient(t *testing.T) {
	// Point the store at a throwaway dir; credstore honors
	// XDG_CONFIG_HOME. t.Setenv restores the prior value after the test.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const url = "http://localhost:8080"
	if err := credstore.Store(url, credstore.Credential{Token: "fhk_e2e"}); err != nil {
		t.Fatalf("credstore.Store: %v", err)
	}

	// getenv sees no FISHHAWK_API_TOKEN, so the ladder falls to the
	// store rung and resolves the credential we just wrote.
	getenv := func(k string) string {
		if k == "FISHHAWK_BACKEND_URL" {
			return url
		}
		return ""
	}
	cfg, err := loadConfig(getenv, credstore.Load)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.apiToken != "fhk_e2e" {
		t.Fatalf("cfg.apiToken = %q, want fhk_e2e", cfg.apiToken)
	}

	client := newAPIClient(cfg)
	if client.token != "fhk_e2e" {
		t.Errorf("apiClient bearer = %q, want the stored token fhk_e2e", client.token)
	}
}

func TestBuildServer_HandshakeReady(t *testing.T) {
	// E19.2 ships handshake-only: the server should construct
	// cleanly with an empty tool registry. The protocol-level
	// handshake itself is tested by the SDK; this test just locks
	// in that buildServer doesn't panic and returns a non-nil
	// server we can hand to mcp.StdioTransport later.
	srv := buildServer(config{
		backendURL: "http://localhost:8080",
		apiToken:   "tok",
	})
	if srv == nil {
		t.Fatal("buildServer returned nil")
	}
}

func TestHandshakeVersion(t *testing.T) {
	// Unstamped builds (GitSHA "unknown") advertise the bare base so the
	// handshake string is unchanged from pre-stamping behavior; stamped
	// builds append "+<sha>" including any -dirty suffix.
	for _, tc := range []struct {
		sha  string
		want string
	}{
		{"unknown", serverVersion},
		{"", serverVersion},
		{"abc1234", serverVersion + "+abc1234"},
		{"abc1234-dirty", serverVersion + "+abc1234-dirty"},
	} {
		if got := handshakeVersion(tc.sha); got != tc.want {
			t.Errorf("handshakeVersion(%q) = %q, want %q", tc.sha, got, tc.want)
		}
	}
}

// envFunc returns a func(string) string backed by a literal map. We
// don't use os.Setenv in tests because env state would leak across
// parallel runs; loadConfig takes the getter as a parameter for
// exactly this reason.
func envFunc(env map[string]string) func(string) string {
	return func(k string) string {
		return env[k]
	}
}
