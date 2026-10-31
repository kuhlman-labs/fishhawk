package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGitHub stands in for github.com + api.github.com. Handlers are
// registered per-test on the embedded mux; the OAuth device/token
// endpoints and the REST endpoints share one server (the provider's
// oauthBaseURL and apiBaseURL both point here in tests).
type fakeGitHub struct {
	*httptest.Server
	mux *http.ServeMux

	// requests counts every request the fake received, routed or not, so
	// a test can assert a gate refused BEFORE any HTTP call.
	requests atomic.Int64
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{mux: http.NewServeMux()}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		f.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

// testClientSecret is the client secret newTestProvider configures; the
// fake check-a-token route requires it as the Basic password.
const testClientSecret = "test-secret"

// checkTokenPath is the check-a-token route for newTestProvider's client.
const checkTokenPath = "/applications/test-client/token"

// allowCheckToken registers a check-a-token route that binds EVERY token to
// the configured client, for tests whose subject is the /user exchange.
func (f *fakeGitHub) allowCheckToken() {
	f.mux.HandleFunc("POST "+checkTokenPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"app":{"client_id":"test-client"}}`))
	})
}

// ghIssued records which OAuth client issued a token in the fake forge's
// token table ("" = not an OAuth-app token at all: a PAT) and whose login
// it resolves to.
type ghIssued struct {
	client string
	login  string
}

// serveTokenTable registers a fake forge over table: the check-a-token
// route requires Basic (test-client, testClientSecret) and answers 200
// naming the issuing client ONLY for tokens issued to test-client (404, the
// documented invalid-token answer, otherwise), while GET /user resolves
// EVERY token in the table — so subject resolution alone would accept all
// of them and only the client binding refuses.
func (f *fakeGitHub) serveTokenTable(t *testing.T, table map[string]ghIssued) {
	t.Helper()
	f.mux.HandleFunc("POST "+checkTokenPath, func(w http.ResponseWriter, r *http.Request) {
		id, secret, ok := r.BasicAuth()
		if !ok || id != "test-client" || secret != testClientSecret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body struct {
			AccessToken string `json:"access_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		iss, known := table[body.AccessToken]
		if !known || iss.client != "test-client" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"app":{"client_id":%q},"user":{"login":%q}}`, iss.client, iss.login)
	})
	f.mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		iss, known := table[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !known {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"login":%q}`, iss.login)
	})
}

// newTestProvider returns a GitHubIdentityProvider pointed at the fake,
// with instant sleeps so polling tests never wait on the wall clock.
func newTestProvider(f *fakeGitHub) *GitHubIdentityProvider {
	return &GitHubIdentityProvider{
		clientID:     "test-client",
		clientSecret: testClientSecret,
		apiBaseURL:   f.URL,
		oauthBaseURL: f.URL,
		httpClient:   f.Client(),
		pollInterval: time.Millisecond,
		sleep:        func(context.Context, time.Duration) error { return nil },
		now:          time.Now,
	}
}

func TestVerifyUser_Success(t *testing.T) {
	f := newFakeGitHub(t)
	f.mux.HandleFunc("/login/device/code", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_code":"DC","user_code":"WXYZ-1234","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`))
	})
	f.mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"gho_abc"}`))
	})
	f.mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gho_abc" {
			t.Errorf("GET /user Authorization = %q, want Bearer gho_abc", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	})

	p := newTestProvider(f)
	var gotCode, gotURI string
	subject, err := p.VerifyUser(context.Background(), func(userCode, verificationURI string) {
		gotCode, gotURI = userCode, verificationURI
	})
	if err != nil {
		t.Fatalf("VerifyUser: %v", err)
	}
	if subject != "github:octocat" {
		t.Errorf("subject = %q, want github:octocat", subject)
	}
	if gotCode != "WXYZ-1234" || gotURI != "https://github.com/login/device" {
		t.Errorf("prompt got (%q, %q), want (WXYZ-1234, https://github.com/login/device)", gotCode, gotURI)
	}
}

// TestVerifyAccessToken_Success covers the server-side re-verify half of
// the CLI-driven device flow (#1708): a CLI-obtained access token is
// exchanged at GET /user and resolves to the provider-qualified subject,
// with the token forwarded as the bearer credential.
func TestVerifyAccessToken_Success(t *testing.T) {
	f := newFakeGitHub(t)
	f.allowCheckToken()
	f.mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gho_cli" {
			t.Errorf("GET /user Authorization = %q, want Bearer gho_cli", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	})

	p := newTestProvider(f)
	subject, err := p.VerifyAccessToken(context.Background(), "gho_cli")
	if err != nil {
		t.Fatalf("VerifyAccessToken: %v", err)
	}
	if subject != "github:octocat" {
		t.Errorf("subject = %q, want github:octocat", subject)
	}
}

// TestVerifyAccessToken_EMULogin pins EMU handling (E44.2 / #1826): an
// Enterprise Managed User login carries a "<username>_<shortcode>" enterprise
// short-code suffix. It must parse without error and resolve to a subject that
// preserves the FULL login (short code included) — never stripped or split, so
// two EMU users on different enterprises stay distinct subjects.
func TestVerifyAccessToken_EMULogin(t *testing.T) {
	f := newFakeGitHub(t)
	f.allowCheckToken()
	f.mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login":"alice_acme"}`))
	})

	p := newTestProvider(f)
	subject, err := p.VerifyAccessToken(context.Background(), "gho_emu")
	if err != nil {
		t.Fatalf("VerifyAccessToken with an EMU login: %v", err)
	}
	if subject != "github:alice_acme" {
		t.Errorf("subject = %q, want github:alice_acme (full EMU login preserved)", subject)
	}
}

// TestCanonicalGitHubLogin pins the shared normalization: EMU short-code
// suffix preserved verbatim, plain login unchanged, surrounding whitespace
// trimmed, and the short code NEVER stripped.
func TestCanonicalGitHubLogin(t *testing.T) {
	cases := []struct{ in, want string }{
		{"octocat", "octocat"},
		{"alice_acme", "alice_acme"},
		{"  alice_acme  ", "alice_acme"},
		{"a_b_c", "a_b_c"},
	}
	for _, c := range cases {
		if got := canonicalGitHubLogin(c.in); got != c.want {
			t.Errorf("canonicalGitHubLogin(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestVerifyAccessToken_EmptyToken rejects an empty token before any HTTP
// call — the mint endpoint must never treat a blank credential as a
// verified subject.
func TestVerifyAccessToken_EmptyToken(t *testing.T) {
	f := newFakeGitHub(t)
	f.mux.HandleFunc("/user", func(http.ResponseWriter, *http.Request) {
		t.Error("GET /user hit despite an empty access token")
	})
	p := newTestProvider(f)
	if _, err := p.VerifyAccessToken(context.Background(), ""); err == nil {
		t.Fatal("VerifyAccessToken(\"\"): want error, got nil")
	}
}

// TestVerifyAccessToken_Unauthorized covers the forge-rejection branch: a
// 401 from GET /user (e.g. a revoked/garbage token) surfaces as an error,
// never a mint.
func TestVerifyAccessToken_Unauthorized(t *testing.T) {
	f := newFakeGitHub(t)
	f.allowCheckToken()
	f.mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	p := newTestProvider(f)
	if _, err := p.VerifyAccessToken(context.Background(), "gho_bad"); err == nil {
		t.Fatal("VerifyAccessToken with a 401 /user: want error, got nil")
	}
}

// TestVerifyAccessToken_ConfiguredClientAccepted pins the client-binding
// request shape (#4627): POST /applications/{client_id}/token with Basic
// (client_id, client_secret), a JSON {"access_token"} body and NO Bearer
// header (GitHub documents that a Bearer header replaces the Basic
// credential), and a 200 naming the configured client resolves the subject.
func TestVerifyAccessToken_ConfiguredClientAccepted(t *testing.T) {
	f := newFakeGitHub(t)
	var gotMethod, gotUser, gotPass, gotAuth, gotToken string
	var gotBasic bool
	f.mux.HandleFunc(checkTokenPath, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotUser, gotPass, gotBasic = r.BasicAuth()
		gotAuth = r.Header.Get("Authorization")
		var body struct {
			AccessToken string `json:"access_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotToken = body.AccessToken
		_, _ = w.Write([]byte(`{"app":{"client_id":"test-client"},"user":{"login":"octocat"}}`))
	})
	f.mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	})

	subject, err := newTestProvider(f).VerifyAccessToken(context.Background(), "gho_ours")
	if err != nil {
		t.Fatalf("VerifyAccessToken: %v", err)
	}
	if subject != "github:octocat" {
		t.Errorf("subject = %q, want github:octocat", subject)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("check-a-token method = %q, want POST", gotMethod)
	}
	if !gotBasic || gotUser != "test-client" || gotPass != testClientSecret {
		t.Errorf("check-a-token Basic = (%q, %q, ok=%v), want (test-client, %s)", gotUser, gotPass, gotBasic, testClientSecret)
	}
	if strings.HasPrefix(gotAuth, "Bearer") {
		t.Errorf("check-a-token Authorization = %q: a Bearer header replaces the Basic credential", gotAuth)
	}
	if gotToken != "gho_ours" {
		t.Errorf("check-a-token body access_token = %q, want gho_ours", gotToken)
	}
}

// TestVerifyAccessToken_RefusesTokenNotIssuedToClient: a token another OAuth
// client issued, a personal access token and an App/installation token all
// resolve a subject at GET /user in this fake, so only the client binding
// refuses them (counterfactual vehicle: skipping the verifyTokenClient call
// turns every subtest RED with a resolved subject).
func TestVerifyAccessToken_RefusesTokenNotIssuedToClient(t *testing.T) {
	table := map[string]ghIssued{
		"gho_ours":    {client: "test-client", login: "alice"},
		"gho_other":   {client: "Iv1.other-oauth-app", login: "alice"},
		"ghp_pat":     {client: "", login: "alice"},
		"ghs_install": {client: "Iv1.some-github-app", login: "alice"},
	}
	for _, tc := range []struct{ name, token string }{
		{"other-oauth-client", "gho_other"},
		{"personal-access-token", "ghp_pat"},
		{"app-installation-token", "ghs_install"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.serveTokenTable(t, table)
			subject, err := newTestProvider(f).VerifyAccessToken(context.Background(), tc.token)
			if err == nil {
				t.Fatalf("VerifyAccessToken(%s) = %q, want a refusal: the token was not issued to the configured client", tc.name, subject)
			}
			if subject != "" {
				t.Errorf("subject = %q on refusal, want empty", subject)
			}
			if !errors.Is(err, ErrIntrospectionRefused) {
				t.Errorf("err = %v, want ErrIntrospectionRefused", err)
			}
		})
	}
	t.Run("control: the configured client's token is accepted by the same fake", func(t *testing.T) {
		f := newFakeGitHub(t)
		f.serveTokenTable(t, table)
		if subject, err := newTestProvider(f).VerifyAccessToken(context.Background(), "gho_ours"); err != nil || subject != "github:alice" {
			t.Fatalf("VerifyAccessToken(gho_ours) = %q, %v; want github:alice", subject, err)
		}
	})
}

// TestVerifyAccessToken_RefusesForeignOrMissingIssuer: the check endpoint
// answers 200 but names another client, no app object, or an empty or
// case-variant client_id. Only the exact app.client_id equality refuses
// these (counterfactual vehicle for that comparison).
func TestVerifyAccessToken_RefusesForeignOrMissingIssuer(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"foreign client", `{"app":{"client_id":"Iv1.other-oauth-app"},"user":{"login":"alice"}}`},
		{"no app object", `{"user":{"login":"alice"}}`},
		{"null app object", `{"app":null,"user":{"login":"alice"}}`},
		{"empty client_id", `{"app":{"client_id":""},"user":{"login":"alice"}}`},
		{"case-variant client_id", `{"app":{"client_id":"TEST-CLIENT"},"user":{"login":"alice"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.mux.HandleFunc("POST "+checkTokenPath, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			})
			f.mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"login":"alice"}`))
			})
			subject, err := newTestProvider(f).VerifyAccessToken(context.Background(), "gho_x")
			if err == nil {
				t.Fatalf("VerifyAccessToken = %q, want a refusal for a 200 naming %s", subject, tc.name)
			}
			if !errors.Is(err, ErrTokenClientMismatch) {
				t.Errorf("err = %v, want ErrTokenClientMismatch", err)
			}
		})
	}
}

// TestVerifyAccessToken_IntrospectionFailureRefuses: every introspection
// outcome other than a 200 naming the configured client refuses, with the
// sentinel a caller classifies on; GET /user would accept each token.
func TestVerifyAccessToken_IntrospectionFailureRefuses(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    error
	}{
		// Non-200 bodies name the configured client, so only the status
		// check (not a decode or client mismatch) refuses them.
		{"server error", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"app":{"client_id":"test-client"}}`))
		}, ErrIntrospectionRefused},
		{"unprocessable", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"app":{"client_id":"test-client"}}`))
		}, ErrIntrospectionRefused},
		{"rate limited", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"app":{"client_id":"test-client"}}`))
		}, ErrRateLimited},
		{"undecodable 200 body", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`not json`))
		}, ErrIntrospectionFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.mux.HandleFunc("POST "+checkTokenPath, tc.handler)
			f.mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"login":"alice"}`))
			})
			subject, err := newTestProvider(f).VerifyAccessToken(context.Background(), "gho_x")
			if err == nil {
				t.Fatalf("VerifyAccessToken = %q, want a refusal", subject)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
	t.Run("unreachable forge", func(t *testing.T) {
		f := newFakeGitHub(t)
		p := newTestProvider(f)
		f.Close() // the listener is gone: every request is a transport error
		subject, err := p.VerifyAccessToken(context.Background(), "gho_x")
		if err == nil {
			t.Fatalf("VerifyAccessToken = %q, want a refusal", subject)
		}
		if !errors.Is(err, ErrIntrospectionFailed) {
			t.Errorf("err = %v, want ErrIntrospectionFailed", err)
		}
	})
}

// TestVerifyAccessToken_UnconfiguredClientCredential: without the client
// secret (or id) the binding cannot be checked, so VerifyAccessToken
// returns ErrClientCredentialUnconfigured BEFORE any HTTP call. The fake's
// check route WOULD bind any token to the configured client and /user
// would resolve it, so only the credential gate refuses.
func TestVerifyAccessToken_UnconfiguredClientCredential(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*GitHubIdentityProvider)
	}{
		{"empty client secret", func(p *GitHubIdentityProvider) { p.clientSecret = "" }},
		{"empty client id", func(p *GitHubIdentityProvider) { p.clientID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.mux.HandleFunc("/applications/", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"app":{"client_id":"test-client"}}`))
			})
			f.mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"login":"alice"}`))
			})
			p := newTestProvider(f)
			tc.mut(p)
			subject, err := p.VerifyAccessToken(context.Background(), "gho_x")
			if !errors.Is(err, ErrClientCredentialUnconfigured) {
				t.Fatalf("VerifyAccessToken = %q, %v; want ErrClientCredentialUnconfigured", subject, err)
			}
			if n := f.requests.Load(); n != 0 {
				t.Errorf("made %d request(s); the credential gate must refuse before any HTTP call", n)
			}
		})
	}
}

// TestVerifyAccessToken_IntrospectionRedirectRefused: the check-a-token
// call refuses ANY redirect. The fake 307-redirects to a second server
// that would bind the token to the configured client; Go's client would
// follow a 307 with the method, body and (same hostname) Authorization
// preserved, so without the refusal the token is accepted and the second
// server records a request.
func TestVerifyAccessToken_IntrospectionRedirectRefused(t *testing.T) {
	var redirected atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`{"app":{"client_id":"test-client"}}`))
	}))
	t.Cleanup(target.Close)

	f := newFakeGitHub(t)
	f.mux.HandleFunc("POST "+checkTokenPath, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+checkTokenPath, http.StatusTemporaryRedirect)
	})
	f.mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"login":"alice"}`))
	})

	subject, err := newTestProvider(f).VerifyAccessToken(context.Background(), "gho_x")
	if err == nil {
		t.Fatalf("VerifyAccessToken = %q, want a refusal: the check-a-token call must not follow a redirect", subject)
	}
	if !errors.Is(err, ErrIntrospectionRefused) {
		t.Errorf("err = %v, want ErrIntrospectionRefused (the 3xx refused as a non-200)", err)
	}
	if n := redirected.Load(); n != 0 {
		t.Errorf("redirect target received %d request(s), want 0", n)
	}
}

// TestWithClientSecret pins the option's effect on the provider.
func TestWithClientSecret(t *testing.T) {
	p, ok := NewGitHubIdentityProvider("cid", nil, WithClientSecret("s3cret")).(*GitHubIdentityProvider)
	if !ok {
		t.Fatal("NewGitHubIdentityProvider did not return *GitHubIdentityProvider")
	}
	if p.clientSecret != "s3cret" {
		t.Errorf("clientSecret = %q, want s3cret", p.clientSecret)
	}
}

// TestPermission_AtLeast covers the tier ordering the token-mint authz
// gate (#1708) uses: a subject satisfies a minimum only when its tier is
// at least as privileged, an unrecognized ACTUAL permission ranks as
// none, and an unrecognized MINIMUM fails closed (#1731) rather than
// being treated as an always-satisfied PermissionNone floor.
func TestPermission_AtLeast(t *testing.T) {
	cases := []struct {
		p    Permission
		min  Permission
		want bool
	}{
		{PermissionAdmin, PermissionWrite, true},
		{PermissionWrite, PermissionWrite, true},
		{PermissionTriage, PermissionWrite, false},
		{PermissionRead, PermissionWrite, false},
		{PermissionNone, PermissionRead, false},
		{PermissionMaintain, PermissionAdmin, false},
		{Permission("garbage"), PermissionRead, false},
		{PermissionAdmin, Permission("garbage"), false},
		{PermissionWrite, Permission("owner"), false},
		{PermissionNone, Permission("garbage"), false},
		{PermissionRead, PermissionNone, true},
	}
	for _, c := range cases {
		if got := c.p.AtLeast(c.min); got != c.want {
			t.Errorf("%q.AtLeast(%q) = %v, want %v", c.p, c.min, got, c.want)
		}
	}
}

// TestParsePermission covers the serve.go flag gate: recognized tiers
// round-trip, and an unrecognized value reports ok=false so startup can
// fail closed rather than under-gate.
func TestParsePermission(t *testing.T) {
	for _, name := range []string{"none", "read", "triage", "write", "maintain", "admin"} {
		if p, ok := ParsePermission(name); !ok || string(p) != name {
			t.Errorf("ParsePermission(%q) = (%q, %v), want (%q, true)", name, p, ok, name)
		}
	}
	for _, bad := range []string{"", "owner", "push", "WRITE"} {
		if _, ok := ParsePermission(bad); ok {
			t.Errorf("ParsePermission(%q) ok = true, want false", bad)
		}
	}
}

func TestVerifyUser_SlowDownHonored(t *testing.T) {
	f := newFakeGitHub(t)
	f.mux.HandleFunc("/login/device/code", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"device_code":"DC","user_code":"UC","verification_uri":"URI","expires_in":900,"interval":1}`))
	})
	var polls int
	f.mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, _ *http.Request) {
		polls++
		switch polls {
		case 1:
			_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
		case 2:
			_, _ = w.Write([]byte(`{"error":"slow_down"}`))
		default:
			_, _ = w.Write([]byte(`{"access_token":"gho_x"}`))
		}
	})
	f.mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"login":"me"}`))
	})

	p := newTestProvider(f)
	// Record the interval passed to each sleep so we can assert the
	// slow_down response grew it.
	var intervals []time.Duration
	p.pollInterval = time.Second // start from the forge interval, not the test 1ms
	p.sleep = func(_ context.Context, d time.Duration) error {
		intervals = append(intervals, d)
		return nil
	}

	subject, err := p.VerifyUser(context.Background(), nil)
	if err != nil {
		t.Fatalf("VerifyUser: %v", err)
	}
	if subject != "github:me" {
		t.Errorf("subject = %q, want github:me", subject)
	}
	// Three sleeps: before poll 1 (pending), poll 2 (slow_down), poll 3
	// (token). The interval must grow by slowDownIncrement after the
	// slow_down response (observed on the third sleep).
	if len(intervals) < 3 {
		t.Fatalf("expected >=3 sleeps, got %d (%v)", len(intervals), intervals)
	}
	if intervals[2] <= intervals[0] {
		t.Errorf("interval did not grow after slow_down: before=%v after=%v", intervals[0], intervals[2])
	}
}

func TestVerifyUser_ExpiryTimeout(t *testing.T) {
	f := newFakeGitHub(t)
	f.mux.HandleFunc("/login/device/code", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"device_code":"DC","user_code":"UC","verification_uri":"URI","expires_in":900,"interval":1}`))
	})
	f.mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, _ *http.Request) {
		// Never authorizes.
		_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
	})

	p := newTestProvider(f)
	// Clock jumps past the deadline on the second read (deadline is
	// computed on the first), so the loop's expiry guard fires before
	// any successful poll — deterministic, no real waiting.
	var calls int
	base := time.Now()
	p.now = func() time.Time {
		calls++
		if calls == 1 {
			return base
		}
		return base.Add(time.Hour)
	}

	_, err := p.VerifyUser(context.Background(), nil)
	if !errors.Is(err, ErrVerificationTimeout) {
		t.Fatalf("VerifyUser err = %v, want ErrVerificationTimeout", err)
	}
}

func TestVerifyUser_ExpiredTokenResponse(t *testing.T) {
	f := newFakeGitHub(t)
	f.mux.HandleFunc("/login/device/code", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"device_code":"DC","user_code":"UC","verification_uri":"URI","expires_in":900,"interval":1}`))
	})
	f.mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"error":"expired_token"}`))
	})

	p := newTestProvider(f)
	_, err := p.VerifyUser(context.Background(), nil)
	if !errors.Is(err, ErrVerificationTimeout) {
		t.Fatalf("VerifyUser err = %v, want ErrVerificationTimeout", err)
	}
}

func TestVerifyUser_CtxCancelledDuringPoll(t *testing.T) {
	f := newFakeGitHub(t)
	f.mux.HandleFunc("/login/device/code", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"device_code":"DC","user_code":"UC","verification_uri":"URI","expires_in":900,"interval":1}`))
	})
	f.mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
	})

	p := newTestProvider(f)
	// The ctx-aware sleep reports cancellation; the loop maps it to a
	// verification timeout.
	p.sleep = func(context.Context, time.Duration) error { return context.Canceled }

	_, err := p.VerifyUser(context.Background(), nil)
	if !errors.Is(err, ErrVerificationTimeout) {
		t.Fatalf("VerifyUser err = %v, want ErrVerificationTimeout", err)
	}
}

func TestVerifyUser_AccessDenied(t *testing.T) {
	f := newFakeGitHub(t)
	f.mux.HandleFunc("/login/device/code", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"device_code":"DC","user_code":"UC","verification_uri":"URI","expires_in":900,"interval":1}`))
	})
	f.mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"error":"access_denied"}`))
	})

	p := newTestProvider(f)
	_, err := p.VerifyUser(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("VerifyUser err = %v, want an access-denied error", err)
	}
}

// TestVerifyUser_PollIntervalFloor covers the security fix: when the forge
// omits `interval` (or returns 0) and no test seam overrides it, the poll
// interval is floored to minPollInterval rather than collapsing to 0 — which
// would busy-poll the OAuth token endpoint. The polling tests elsewhere always
// inject a positive pollInterval, so this branch was previously uncovered.
func TestVerifyUser_PollIntervalFloor(t *testing.T) {
	f := newFakeGitHub(t)
	// Device-code response omits `interval` entirely → device.Interval == 0.
	f.mux.HandleFunc("/login/device/code", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"device_code":"DC","user_code":"UC","verification_uri":"URI","expires_in":900}`))
	})
	f.mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"gho_x"}`))
	})
	f.mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"login":"me"}`))
	})

	p := newTestProvider(f)
	// Do NOT override the forge interval: production leaves pollInterval 0, so
	// the floor — not the test seam — must supply the wait.
	p.pollInterval = 0
	var slept []time.Duration
	p.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}

	if _, err := p.VerifyUser(context.Background(), nil); err != nil {
		t.Fatalf("VerifyUser: %v", err)
	}
	if len(slept) == 0 {
		t.Fatal("expected at least one sleep before polling the token endpoint")
	}
	if slept[0] < minPollInterval {
		t.Errorf("poll interval = %v, want >= %v (floor guards a 0/omitted forge interval)", slept[0], minPollInterval)
	}
}

// TestGet_AuthenticatedToken exercises the authenticated REST-read path in
// get(): a non-nil token accessor resolves a bearer token and sets the
// Authorization header. Every other test constructs the provider anonymously,
// so this branch was previously uncovered.
func TestGet_AuthenticatedToken(t *testing.T) {
	f := newFakeGitHub(t)
	var gotAuth string
	f.mux.HandleFunc("/repos/owner/repo/collaborators/octocat/permission",
		func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"role_name":"write"}`))
		})
	p := newTestProvider(f)
	p.token = func(context.Context) (string, error) { return "tok-123", nil }

	got, err := p.PermissionLevel(context.Background(), "owner/repo", "github:octocat")
	if err != nil {
		t.Fatalf("PermissionLevel: %v", err)
	}
	if got != PermissionWrite {
		t.Errorf("PermissionLevel = %q, want write", got)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want Bearer tok-123", gotAuth)
	}
}

// TestGet_TokenAccessorError covers the token-accessor error-wrap branch:
// a failing accessor aborts the request with an "identity: resolve token" wrap
// before any HTTP call.
func TestGet_TokenAccessorError(t *testing.T) {
	f := newFakeGitHub(t)
	f.mux.HandleFunc("/orgs/acme/members/octocat", func(http.ResponseWriter, *http.Request) {
		t.Error("REST endpoint hit despite a token-accessor failure")
	})
	p := newTestProvider(f)
	p.token = func(context.Context) (string, error) { return "", errors.New("boom") }

	_, err := p.ResolveMembership(context.Background(), "acme", "github:octocat")
	if err == nil || !strings.Contains(err.Error(), "resolve token") {
		t.Fatalf("err = %v, want an 'identity: resolve token' wrap", err)
	}
}

func TestPermissionLevel(t *testing.T) {
	tests := []struct {
		name     string
		roleName string
		status   int
		want     Permission
	}{
		{"maintain", "maintain", http.StatusOK, PermissionMaintain},
		{"admin", "admin", http.StatusOK, PermissionAdmin},
		{"unknown role denies", "goofy", http.StatusOK, PermissionNone},
		{"404 no access", "", http.StatusNotFound, PermissionNone},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.mux.HandleFunc("/repos/owner/repo/collaborators/octocat/permission",
				func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tc.status)
					if tc.status == http.StatusOK {
						_, _ = w.Write([]byte(`{"role_name":"` + tc.roleName + `"}`))
					}
				})
			p := newTestProvider(f)
			got, err := p.PermissionLevel(context.Background(), "owner/repo", "github:octocat")
			if err != nil {
				t.Fatalf("PermissionLevel: %v", err)
			}
			if got != tc.want {
				t.Errorf("PermissionLevel = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPermissionLevel_RateLimited(t *testing.T) {
	f := newFakeGitHub(t)
	f.mux.HandleFunc("/repos/owner/repo/collaborators/octocat/permission",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "1700000000")
			w.WriteHeader(http.StatusForbidden)
		})
	p := newTestProvider(f)
	_, err := p.PermissionLevel(context.Background(), "owner/repo", "github:octocat")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("PermissionLevel err = %v, want ErrRateLimited", err)
	}
}

// TestPermissionLevel_ServerError covers the generic non-2xx branch
// ("identity: permission: %d") — distinct from the 404 and rate-limit paths.
func TestPermissionLevel_ServerError(t *testing.T) {
	f := newFakeGitHub(t)
	f.mux.HandleFunc("/repos/owner/repo/collaborators/octocat/permission",
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	p := newTestProvider(f)
	_, err := p.PermissionLevel(context.Background(), "owner/repo", "github:octocat")
	if err == nil || !strings.Contains(err.Error(), "permission: 500") {
		t.Fatalf("err = %v, want 'identity: permission: 500'", err)
	}
}

// TestPermissionLevel_RateLimited_429RetryAfter covers rateLimitError's
// secondary signature (429 + Retry-After) — the only rate-limit case the other
// test does not exercise (it asserts 403 + X-RateLimit-Remaining:0).
func TestPermissionLevel_RateLimited_429RetryAfter(t *testing.T) {
	f := newFakeGitHub(t)
	f.mux.HandleFunc("/repos/owner/repo/collaborators/octocat/permission",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
		})
	p := newTestProvider(f)
	_, err := p.PermissionLevel(context.Background(), "owner/repo", "github:octocat")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited (429 + Retry-After signature)", err)
	}
}

func TestResolveMembership_Org(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   bool
	}{
		{"member 204", http.StatusNoContent, true},
		{"non-member 404", http.StatusNotFound, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.mux.HandleFunc("/orgs/acme/members/octocat",
				func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status) })
			p := newTestProvider(f)
			got, err := p.ResolveMembership(context.Background(), "acme", "github:octocat")
			if err != nil {
				t.Fatalf("ResolveMembership: %v", err)
			}
			if got != tc.want {
				t.Errorf("ResolveMembership = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResolveMembership_Team(t *testing.T) {
	tests := []struct {
		name   string
		status int
		state  string
		want   bool
	}{
		{"active member", http.StatusOK, "active", true},
		{"pending not active", http.StatusOK, "pending", false},
		{"non-member 404", http.StatusNotFound, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.mux.HandleFunc("/orgs/acme/teams/reviewers/memberships/octocat",
				func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tc.status)
					if tc.status == http.StatusOK {
						_, _ = w.Write([]byte(`{"state":"` + tc.state + `"}`))
					}
				})
			p := newTestProvider(f)
			got, err := p.ResolveMembership(context.Background(), "acme/reviewers", "github:octocat")
			if err != nil {
				t.Fatalf("ResolveMembership: %v", err)
			}
			if got != tc.want {
				t.Errorf("ResolveMembership = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestResolveMembership_Org_ServerError covers orgMembership's default branch
// (a status that is neither 204 nor 404).
func TestResolveMembership_Org_ServerError(t *testing.T) {
	f := newFakeGitHub(t)
	f.mux.HandleFunc("/orgs/acme/members/octocat",
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	p := newTestProvider(f)
	_, err := p.ResolveMembership(context.Background(), "acme", "github:octocat")
	if err == nil || !strings.Contains(err.Error(), "org membership: 500") {
		t.Fatalf("err = %v, want 'identity: org membership: 500'", err)
	}
}

// TestResolveMembership_Team_ServerError covers teamMembership's non-2xx branch
// (a status that is neither 200 nor 404).
func TestResolveMembership_Team_ServerError(t *testing.T) {
	f := newFakeGitHub(t)
	f.mux.HandleFunc("/orgs/acme/teams/reviewers/memberships/octocat",
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	p := newTestProvider(f)
	_, err := p.ResolveMembership(context.Background(), "acme/reviewers", "github:octocat")
	if err == nil || !strings.Contains(err.Error(), "team membership: 500") {
		t.Fatalf("err = %v, want 'identity: team membership: 500'", err)
	}
}

func TestResolveMembership_BadRef(t *testing.T) {
	p := newTestProvider(newFakeGitHub(t))
	_, err := p.ResolveMembership(context.Background(), "a/b/c", "github:octocat")
	if err == nil {
		t.Fatal("ResolveMembership with a 3-part ref: want error, got nil")
	}
}
