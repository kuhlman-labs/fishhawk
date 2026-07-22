package identity

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &fakeGitHub{Server: srv, mux: mux}
}

// newTestProvider returns a GitHubIdentityProvider pointed at the fake,
// with instant sleeps so polling tests never wait on the wall clock.
func newTestProvider(f *fakeGitHub) *GitHubIdentityProvider {
	return &GitHubIdentityProvider{
		clientID:     "test-client",
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
	f.mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	p := newTestProvider(f)
	if _, err := p.VerifyAccessToken(context.Background(), "gho_bad"); err == nil {
		t.Fatal("VerifyAccessToken with a 401 /user: want error, got nil")
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
