package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

// noRefresh is a refreshCred seam that must never be called — used by
// every test whose credential is outside its refresh skew (or not
// refreshable at all) to prove the ladder never dials a token endpoint
// it has no business dialing.
func noRefresh(t *testing.T) func(string) (credstore.Credential, error) {
	t.Helper()
	return func(string) (credstore.Credential, error) {
		t.Fatalf("refreshCred must not be called for a credential outside its refresh skew")
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
	cfg, err := loadConfig(envFunc(env), noCred(t), noRefresh(t))
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
	cfg, err := loadConfig(envFunc(env), noCred(t), noRefresh(t))
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
	cfg, err := loadConfig(envFunc(env), noCred(t), noRefresh(t))
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
	cfg, err := loadConfig(envFunc(env), noCred(t), noRefresh(t))
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
	cfg, err := loadConfig(envFunc(env), credFunc(credstore.Credential{Token: "fhk_stored"}, nil), noRefresh(t))
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
	cfg, err := loadConfig(envFunc(env), loadCred, noRefresh(t))
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
	_, err := loadConfig(envFunc(ladderEnv()), credFunc(credstore.Credential{}, credstore.ErrNotFound), noRefresh(t))
	assertRemediation(t, err, ladderURL)
	if !strings.Contains(err.Error(), "no Fishhawk credential stored") {
		t.Errorf("not-found error missing its unique substring; got %q", err.Error())
	}
}

// M4: a credential exists but its token is empty → a fail-closed error
// with a substring UNIQUE to the empty-token branch, never a config
// carrying an empty bearer.
func TestLoadConfig_EmptyStoredToken(t *testing.T) {
	cfg, err := loadConfig(envFunc(ladderEnv()), credFunc(credstore.Credential{Token: ""}, nil), noRefresh(t))
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
	_, notFound := loadConfig(envFunc(ladderEnv()), credFunc(credstore.Credential{}, credstore.ErrNotFound), noRefresh(t))
	_, empty := loadConfig(envFunc(ladderEnv()), credFunc(credstore.Credential{Token: ""}, nil), noRefresh(t))
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
	_, err := loadConfig(envFunc(ladderEnv()), credFunc(credstore.Credential{Token: "fhk_old", ExpiresAt: &past}, nil), noRefresh(t))
	assertRemediation(t, err, ladderURL)
	if !strings.Contains(err.Error(), "expired at") {
		t.Errorf("expired error missing its unique substring; got %q", err.Error())
	}
}

// M5-control: a NIL ExpiresAt means non-expiring (v0 tokens do not
// expire) and MUST be accepted — reading nil as expired would refuse
// every field credential.
func TestLoadConfig_NilExpiryAccepted(t *testing.T) {
	cfg, err := loadConfig(envFunc(ladderEnv()), credFunc(credstore.Credential{Token: "fhk_live", ExpiresAt: nil}, nil), noRefresh(t))
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
	cfg, err := loadConfig(envFunc(ladderEnv()), credFunc(credstore.Credential{Token: "fhk_future", ExpiresAt: &future}, nil), noRefresh(t))
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
	_, err := loadConfig(envFunc(ladderEnv()), credFunc(credstore.Credential{}, parseErr), noRefresh(t))
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
// credential with credstore.Store and drives loadConfig(getenv,
// credstore.Load), asserting the resolved config carries the stored
// token — proving credstore module → mcp config holds across the module
// boundary. The onward config → apiClient bearer half moved to
// mcpserver (server_test.go's NewServer/Config.internal() assertion)
// with newAPIClient; loadConfig itself stays a CLI concern here.
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
	cfg, err := loadConfig(getenv, credstore.Load, noRefresh(t))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.apiToken != "fhk_e2e" {
		t.Fatalf("cfg.apiToken = %q, want fhk_e2e", cfg.apiToken)
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

// TestMcpServerConfig pins the fishhawk-mcp production wiring (#2479): the Config
// is TRANSPORT-aware, HTTPTransport:true for the `--transport http` branch (this
// binary's process cwd is then a long-lived daemon's, as wrong as fishhawkd's)
// and false for the stdio default (the client-spawned process, whose cwd IS the
// caller's project). Without this both production callers could ship the
// permissive zero value while every mcpserver test passes.
func TestMcpServerConfig(t *testing.T) {
	cfg := config{backendURL: "http://127.0.0.1:9090", apiToken: "fhk_y"}

	httpCfg := mcpServerConfig(cfg, true)
	if !httpCfg.HTTPTransport {
		t.Error("http branch: HTTPTransport = false, want true")
	}
	if httpCfg.BackendURL != "http://127.0.0.1:9090" || httpCfg.APIToken != "fhk_y" {
		t.Errorf("http branch: passthrough wrong: %+v", httpCfg)
	}

	stdioCfg := mcpServerConfig(cfg, false)
	if stdioCfg.HTTPTransport {
		t.Error("stdio branch: HTTPTransport = true, want false")
	}
	if stdioCfg.BackendURL != "http://127.0.0.1:9090" || stdioCfg.APIToken != "fhk_y" {
		t.Errorf("stdio branch: passthrough wrong: %+v", stdioCfg)
	}
}

// --- proactive refresh (#2393 / ADR-076) ------------------------------------

// expiringRefreshable builds a credential 90s from expiry on a 15m
// lifetime — inside the derived 2m skew — carrying everything a refresh
// needs, keyed to the given token endpoint.
func expiringRefreshable(tokenEndpoint string) credstore.Credential {
	now := time.Now()
	issued := now.Add(-13*time.Minute - 30*time.Second)
	exp := issued.Add(15 * time.Minute)
	return credstore.Credential{
		Token:         "fho_stale",
		RefreshToken:  "fhr_seed",
		ClientID:      "fishhawk-cli",
		TokenEndpoint: tokenEndpoint,
		IssuedAt:      &issued,
		ExpiresAt:     &exp,
	}
}

// TestLoadConfigRefreshesExpiringCredential drives the ladder through
// the loadCred/refreshCred seams: a credential inside its skew is
// refreshed, and the RESOLVED apiToken is the refreshed one, never the
// stale bearer.
func TestLoadConfigRefreshesExpiringCredential(t *testing.T) {
	stale := expiringRefreshable("http://localhost:8080/v0/oauth/token")
	var refreshedURL string
	refresh := func(u string) (credstore.Credential, error) {
		refreshedURL = u
		fresh := stale
		fresh.Token = "fho_fresh"
		fresh.RefreshToken = "fhr_rotated"
		exp := time.Now().Add(15 * time.Minute)
		fresh.ExpiresAt = &exp
		return fresh, nil
	}
	cfg, err := loadConfig(envFunc(ladderEnv()), credFunc(stale, nil), refresh)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.apiToken != "fho_fresh" {
		t.Fatalf("apiToken = %q, want the REFRESHED fho_fresh, not the stale bearer", cfg.apiToken)
	}
	if refreshedURL != ladderURL {
		t.Errorf("refreshCred called with %q, want the normalized backend key %q", refreshedURL, ladderURL)
	}
}

// A credential OUTSIDE its skew is used as-is and refreshCred is never
// called (noRefresh t.Fatals) — the control against a ladder that
// refreshes on every start and burns rotations.
func TestLoadConfigOutsideSkewDoesNotRefresh(t *testing.T) {
	c := expiringRefreshable("http://localhost:8080/v0/oauth/token")
	issued := time.Now().Add(-5 * time.Minute)
	exp := issued.Add(15 * time.Minute) // 10m left, well outside the 2m skew
	c.IssuedAt, c.ExpiresAt = &issued, &exp
	cfg, err := loadConfig(envFunc(ladderEnv()), credFunc(c, nil), noRefresh(t))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.apiToken != "fho_stale" {
		t.Errorf("apiToken = %q, want the still-valid stored token", cfg.apiToken)
	}
}

// An EXPIRED but refreshable credential is refreshed, not refused: the
// refresh token outlives the access token, so the pre-#2393 hard fail
// on rung 3c must not fire ahead of the refresh.
func TestLoadConfigExpiredRefreshableIsRefreshedNotRefused(t *testing.T) {
	c := expiringRefreshable("http://localhost:8080/v0/oauth/token")
	issued := time.Now().Add(-time.Hour)
	exp := issued.Add(15 * time.Minute)
	c.IssuedAt, c.ExpiresAt = &issued, &exp
	refresh := func(string) (credstore.Credential, error) {
		fresh := c
		fresh.Token = "fho_after_expiry"
		e := time.Now().Add(15 * time.Minute)
		fresh.ExpiresAt = &e
		return fresh, nil
	}
	cfg, err := loadConfig(envFunc(ladderEnv()), credFunc(c, nil), refresh)
	if err != nil {
		t.Fatalf("an expired refreshable credential must be refreshed, got %v", err)
	}
	if cfg.apiToken != "fho_after_expiry" {
		t.Errorf("apiToken = %q, want fho_after_expiry", cfg.apiToken)
	}
}

// TestLoadConfigRefreshFailureNamesLogin: a failed refresh is FATAL
// (binding condition 1) — a distinct actionable error naming the login
// command and the backend URL, and NEVER the stale bearer. The wording
// is asserted distinct from every other rung's so no two collapse.
func TestLoadConfigRefreshFailureNamesLogin(t *testing.T) {
	stale := expiringRefreshable("http://localhost:8080/v0/oauth/token")
	refuse := func(string) (credstore.Credential, error) {
		return credstore.Credential{}, &credstore.RefreshError{StatusCode: 400, Code: "invalid_grant", Description: "refresh token revoked"}
	}
	cfg, err := loadConfig(envFunc(ladderEnv()), credFunc(stale, nil), refuse)
	assertRemediation(t, err, ladderURL)
	if cfg.apiToken != "" {
		t.Fatalf("a failed refresh must not resolve a token; got %q", cfg.apiToken)
	}
	if !strings.Contains(err.Error(), "could not be refreshed") {
		t.Errorf("refresh-failure error missing its unique substring; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("refresh-failure error should carry the AS's error code; got %q", err.Error())
	}

	// Distinctness against every sibling rung.
	_, notFound := loadConfig(envFunc(ladderEnv()), credFunc(credstore.Credential{}, credstore.ErrNotFound), noRefresh(t))
	_, empty := loadConfig(envFunc(ladderEnv()), credFunc(credstore.Credential{Token: ""}, nil), noRefresh(t))
	past := time.Now().Add(-time.Hour)
	_, expired := loadConfig(envFunc(ladderEnv()), credFunc(credstore.Credential{Token: "fhk_old", ExpiresAt: &past}, nil), noRefresh(t))
	_, corrupt := loadConfig(envFunc(ladderEnv()), credFunc(credstore.Credential{}, errors.New("credstore: parse x: bad")), noRefresh(t))
	for name, other := range map[string]error{"not-found": notFound, "empty-token": empty, "expired": expired, "corrupt": corrupt} {
		if other == nil {
			t.Fatalf("%s rung must error", name)
		}
		if other.Error() == err.Error() {
			t.Errorf("refresh-failure wording collapses into the %s rung: %q", name, err.Error())
		}
		if strings.Contains(other.Error(), "could not be refreshed") {
			t.Errorf("%s rung must not claim a refresh was attempted: %q", name, other.Error())
		}
	}
}

// A refresh that returns an empty token is refused like any empty bearer.
func TestLoadConfigRefreshedEmptyTokenRefused(t *testing.T) {
	stale := expiringRefreshable("http://localhost:8080/v0/oauth/token")
	empty := func(string) (credstore.Credential, error) { return credstore.Credential{}, nil }
	cfg, err := loadConfig(envFunc(ladderEnv()), credFunc(stale, nil), empty)
	assertRemediation(t, err, ladderURL)
	if cfg.apiToken != "" {
		t.Fatalf("an empty refreshed token must not resolve; got %q", cfg.apiToken)
	}
	if !strings.Contains(err.Error(), "refreshed Fishhawk credential") || !strings.Contains(err.Error(), "empty token") {
		t.Errorf("empty-refreshed-token error missing its unique wording; got %q", err.Error())
	}
}

// TestLoadConfigPersistsRotatedRefreshToken crosses the module seam with
// the REAL credstore and the PRODUCTION refresh seam against an httptest
// token endpoint: after loadConfig returns, the store on disk carries
// the ROTATED refresh token (committed state, read back), and the
// resolved token is the new access token. Deleting the persist step in
// credstore leaves the OLD refresh token on disk → RED.
func TestLoadConfigPersistsRotatedRefreshToken(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Header.Get("Authorization") != "" || r.PostForm.Get("client_secret") != "" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
			return
		}
		if r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("refresh_token") != "fhr_seed" || r.PostForm.Get("client_id") != "fishhawk-cli" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fho_rotated","token_type":"Bearer","expires_in":900,"refresh_token":"fhr_rotated"}`))
	}))
	t.Cleanup(as.Close)

	const url = "http://localhost:8080"
	if err := credstore.Store(url, expiringRefreshable(as.URL+"/v0/oauth/token")); err != nil {
		t.Fatalf("credstore.Store: %v", err)
	}
	getenv := func(k string) string {
		if k == "FISHHAWK_BACKEND_URL" {
			return url
		}
		return ""
	}
	cfg, err := loadConfig(getenv, credstore.Load, refreshStoredCredential)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.apiToken != "fho_rotated" {
		t.Fatalf("cfg.apiToken = %q, want fho_rotated", cfg.apiToken)
	}
	stored, err := credstore.Load(url)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != "fhr_rotated" {
		t.Fatalf("stored RefreshToken = %q, want the ROTATED fhr_rotated (an unpersisted rotation is burned)", stored.RefreshToken)
	}
	if stored.Token != "fho_rotated" {
		t.Errorf("stored Token = %q, want fho_rotated", stored.Token)
	}
}
