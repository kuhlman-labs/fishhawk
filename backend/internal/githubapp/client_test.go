package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeGitHub is a minimal httptest.Server that mimics
// POST /app/installations/{id}/access_tokens. Tests configure
// status + response shape via fields.
type fakeGitHub struct {
	status         int
	body           string
	gotAuth        string
	gotAcceptHdr   string
	gotPath        string
	gotAPIVersion  string
	requestCounter int
}

func fakeGitHubHandler(fg *fakeGitHub) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{installation_id}/access_tokens",
		func(w http.ResponseWriter, r *http.Request) {
			fg.requestCounter++
			fg.gotAuth = r.Header.Get("Authorization")
			fg.gotAcceptHdr = r.Header.Get("Accept")
			fg.gotPath = r.URL.Path
			fg.gotAPIVersion = r.Header.Get("X-GitHub-Api-Version")

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fg.status)
			if fg.body != "" {
				_, _ = io.WriteString(w, fg.body)
				return
			}
			if fg.status == http.StatusCreated {
				_ = json.NewEncoder(w).Encode(InstallationToken{
					Token:     "ghs_canned_token",
					ExpiresAt: time.Now().Add(time.Hour).UTC(),
				})
			}
		})
	return mux
}

func newFakeGitHub(t *testing.T) (*fakeGitHub, *httptest.Server) {
	t.Helper()
	fg := &fakeGitHub{status: http.StatusCreated}
	srv := httptest.NewServer(fakeGitHubHandler(fg))
	t.Cleanup(srv.Close)
	return fg, srv
}

// newFakeGitHubTLS is the https variant, used to exercise the resolved-override
// path whose validation requires an https target host (E44.2 / #1826). The
// returned server's cert is trusted only by srv.Client().
func newFakeGitHubTLS(t *testing.T) (*fakeGitHub, *httptest.Server) {
	t.Helper()
	fg := &fakeGitHub{status: http.StatusCreated}
	srv := httptest.NewTLSServer(fakeGitHubHandler(fg))
	t.Cleanup(srv.Close)
	return fg, srv
}

// hostRewriteClient returns an HTTP client that trusts srv's TLS cert but
// dials srv's listener for EVERY host, regardless of the URL's hostname. This
// lets a resolved override name a FAKE host (acme.ghe.com, notghe.com) while
// still reaching srv when the request is actually made — the piece that makes
// the allowlist tests non-vacuous end-to-end:
//
//   - on the ALLOW path it lets the leading-dot suffix branch decide the mint
//     (host acme.ghe.com under a .ghe.com entry, NOT a loopback exact match),
//     so the subtest fails if matchesHostAllowlist rejected genuine subdomains;
//   - on the REJECT path it makes the credential-never-shipped assertion real:
//     were the matcher to WRONGLY admit notghe.com, the request would land on
//     srv and set gotAuth, so the zero-request/empty-Authorization checks can
//     actually detect credential transmission before rejection.
//
// TLS name verification is skipped because the httptest cert is issued for
// 127.0.0.1/example.com, not the fake override hostname we dial under; these
// tests pin the host ALLOWLIST decision, not TLS verification.
func hostRewriteClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	client := srv.Client()
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("srv.Client() transport is %T, want *http.Transport", client.Transport)
	}
	addr := srv.Listener.Addr().String()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	tr.TLSClientConfig.InsecureSkipVerify = true
	return client
}

func newTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	_, pemBytes := generateTestKey(t)
	signer, err := NewSignerFromPEM(99999, pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	return &Client{
		BaseURL: baseURL,
		Signer:  signer,
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
}

func TestIssueInstallationToken_HappyPath(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	c := newTestClient(t, srv.URL)

	tok, err := c.IssueInstallationToken(context.Background(), 42)
	if err != nil {
		t.Fatalf("IssueInstallationToken: %v", err)
	}
	if tok.Token != "ghs_canned_token" {
		t.Errorf("Token = %q", tok.Token)
	}
	if tok.ExpiresAt.IsZero() {
		t.Error("ExpiresAt zero")
	}
	if !strings.HasPrefix(fg.gotAuth, "Bearer ") {
		t.Errorf("Authorization = %q, want Bearer prefix", fg.gotAuth)
	}
	if fg.gotAcceptHdr != "application/vnd.github+json" {
		t.Errorf("Accept = %q", fg.gotAcceptHdr)
	}
	if fg.gotAPIVersion != "2022-11-28" {
		t.Errorf("X-GitHub-Api-Version = %q", fg.gotAPIVersion)
	}
	if !strings.Contains(fg.gotPath, "/app/installations/42/access_tokens") {
		t.Errorf("path = %q", fg.gotPath)
	}
}

func TestIssueInstallationToken_Unauthorized(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.status = http.StatusUnauthorized
	c := newTestClient(t, srv.URL)
	_, err := c.IssueInstallationToken(context.Background(), 42)
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestIssueInstallationToken_NotFound(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.status = http.StatusNotFound
	c := newTestClient(t, srv.URL)
	_, err := c.IssueInstallationToken(context.Background(), 42)
	if !errors.Is(err, ErrInstallationNotFound) {
		t.Errorf("err = %v, want ErrInstallationNotFound", err)
	}
}

func TestIssueInstallationToken_OtherStatus(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.status = http.StatusServiceUnavailable
	fg.body = "GitHub down"
	c := newTestClient(t, srv.URL)
	_, err := c.IssueInstallationToken(context.Background(), 42)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "GitHub down") {
		t.Errorf("err = %v, want 503 + body", err)
	}
}

func TestIssueInstallationToken_MissingFields(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.body = `{"token":""}` // missing token + expires_at
	c := newTestClient(t, srv.URL)
	_, err := c.IssueInstallationToken(context.Background(), 42)
	if err == nil || !strings.Contains(err.Error(), "missing required") {
		t.Errorf("err = %v, want missing-fields error", err)
	}
}

func TestIssueInstallationToken_BadJSON(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.body = "not json"
	c := newTestClient(t, srv.URL)
	_, err := c.IssueInstallationToken(context.Background(), 42)
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Errorf("err = %v, want decode error", err)
	}
}

func TestIssueInstallationToken_NilSigner(t *testing.T) {
	c := &Client{HTTP: &http.Client{}}
	_, err := c.IssueInstallationToken(context.Background(), 42)
	if err == nil || !strings.Contains(err.Error(), "Signer") {
		t.Errorf("err = %v, want missing-signer error", err)
	}
}

func TestNewClient_Defaults(t *testing.T) {
	_, pemBytes := generateTestKey(t)
	signer, _ := NewSignerFromPEM(1, pemBytes)
	c := NewClient(signer)
	if c.HTTP.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v", c.HTTP.Timeout)
	}
	if c.BaseURL != "" {
		t.Errorf("BaseURL = %q, want empty (defaults at use time)", c.BaseURL)
	}
}

func TestIssueInstallationToken_DefaultBaseURL(t *testing.T) {
	// Pin behavior: BaseURL == "" means we hit api.github.com. We
	// don't actually hit the network — just verify the client
	// builds the request URL correctly when BaseURL is empty by
	// pointing at a fake that accepts ANY path.
	fg, srv := newFakeGitHub(t)
	c := newTestClient(t, srv.URL)
	// Force the empty-baseurl branch: since we can't hit
	// api.github.com in tests, just assert the code path doesn't
	// crash on c.BaseURL == "" — the production case is that
	// api.github.com responds.
	c.BaseURL = ""
	_, err := c.IssueInstallationToken(context.Background(), 42)
	// We expect a network error since api.github.com isn't going
	// to accept our test JWT. Just confirm the URL building didn't
	// blow up before the network attempt.
	if err == nil {
		t.Skip("test machine unexpectedly resolved api.github.com")
	}
	_ = fg
}

// TestIssueInstallationToken_ResolveBaseURL_Override pins Mode 2 (E44.2 /
// #1826): a resolver returning a non-empty override host makes the mint target
// THAT host, not the client's default BaseURL. Two fakes prove routing: the
// override server receives the request; the default (BaseURL) server does not.
func TestIssueInstallationToken_ResolveBaseURL_Override(t *testing.T) {
	overrideFake, overrideSrv := newFakeGitHubTLS(t) // https: validation requires it
	defaultFake, defaultSrv := newFakeGitHub(t)

	c := newTestClient(t, defaultSrv.URL) // BaseURL = the default host
	c.HTTP = overrideSrv.Client()         // trust the override host's TLS cert
	c.ResolveBaseURL = func(_ context.Context, ref string) (string, error) {
		if ref != "42" {
			t.Errorf("ResolveBaseURL got installationRef = %q, want \"42\"", ref)
		}
		return overrideSrv.URL, nil
	}

	tok, err := c.IssueInstallationToken(context.Background(), 42)
	if err != nil {
		t.Fatalf("IssueInstallationToken: %v", err)
	}
	if tok.Token != "ghs_canned_token" {
		t.Errorf("Token = %q", tok.Token)
	}
	if overrideFake.requestCounter != 1 {
		t.Errorf("override host received %d requests, want 1", overrideFake.requestCounter)
	}
	if defaultFake.requestCounter != 0 {
		t.Errorf("default host received %d requests, want 0 (override should win)", defaultFake.requestCounter)
	}
}

// TestIssueInstallationToken_ResolveBaseURL_Empty pins the NULL-column /
// unknown-installation fallback: a resolver returning ("", nil) is the
// intentional absence of an override, so the mint stays on the client's
// BaseURL (deployment default).
func TestIssueInstallationToken_ResolveBaseURL_Empty(t *testing.T) {
	defaultFake, defaultSrv := newFakeGitHub(t)
	c := newTestClient(t, defaultSrv.URL)
	c.ResolveBaseURL = func(context.Context, string) (string, error) { return "", nil }

	tok, err := c.IssueInstallationToken(context.Background(), 42)
	if err != nil {
		t.Fatalf("IssueInstallationToken: %v", err)
	}
	if tok.Token != "ghs_canned_token" {
		t.Errorf("Token = %q", tok.Token)
	}
	if defaultFake.requestCounter != 1 {
		t.Errorf("default host received %d requests, want 1 (empty override → default)", defaultFake.requestCounter)
	}
}

// TestIssueInstallationToken_ResolveBaseURL_Error pins the FAIL-CLOSED
// contract (E44.2 / #1826, binding condition 1): a resolver error FAILS the
// mint and surfaces the error — it must NOT silently fall back to the default
// host. The default server must receive NO request.
func TestIssueInstallationToken_ResolveBaseURL_Error(t *testing.T) {
	defaultFake, defaultSrv := newFakeGitHub(t)
	c := newTestClient(t, defaultSrv.URL)
	sentinel := errors.New("db unavailable")
	c.ResolveBaseURL = func(context.Context, string) (string, error) { return "", sentinel }

	_, err := c.IssueInstallationToken(context.Background(), 42)
	if err == nil {
		t.Fatal("IssueInstallationToken succeeded, want a failure (resolver error must fail the mint)")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want it to wrap the resolver error", err)
	}
	if defaultFake.requestCounter != 0 {
		t.Errorf("default host received %d requests, want 0 (a resolver error must not fall back to the default host)", defaultFake.requestCounter)
	}
}

// TestIssueInstallationToken_ResolveBaseURL_RejectsInvalid pins the hardening
// (E44.2 / #1826): a resolved override that is not a well-formed https URL FAILS
// the mint before any request ships the App JWT. An http:// value (JWT without
// TLS), a hostless value, and a malformed value must all be rejected, and the
// default host must receive NO request.
func TestIssueInstallationToken_ResolveBaseURL_RejectsInvalid(t *testing.T) {
	cases := []struct {
		name, override string
	}{
		{"http scheme (no TLS)", "http://evil.example.com"},
		{"missing host", "https://"},
		{"malformed url", "https://\x00bad"},
		{"empty scheme", "://evil.example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defaultFake, defaultSrv := newFakeGitHub(t)
			c := newTestClient(t, defaultSrv.URL)
			c.ResolveBaseURL = func(context.Context, string) (string, error) { return tc.override, nil }

			_, err := c.IssueInstallationToken(context.Background(), 42)
			if err == nil {
				t.Fatalf("override %q: mint succeeded, want failure (invalid override must not ship the App JWT)", tc.override)
			}
			if defaultFake.requestCounter != 0 {
				t.Errorf("override %q: default host received %d requests, want 0 (an invalid override must fail closed, never fall back)", tc.override, defaultFake.requestCounter)
			}
		})
	}
}

// TestIssueInstallationToken_AllowlistAllows pins binding condition 1
// (E44.15 / #2093): with FISHHAWKD_GITHUB_INSTALLATION_HOST_ALLOWLIST
// configured, a resolved override whose host is allowlisted STILL mints — both
// an exact-host entry and a leading-dot suffix matching a subdomain. Together
// with the reject test, this jointly falsifies a matcher that would reject
// every host when the allowlist is non-empty (locking out legitimate
// operators). Driven end-to-end through IssueInstallationToken so the override
// host receives the mint request.
//
// The suffix case resolves to a genuine subdomain (acme.ghe.com) allowed ONLY
// by the .ghe.com suffix entry — no loopback exact match to fall back on — and
// dials it through hostRewriteClient, so the mint reaches the fake IFF the
// suffix branch admits it. A matcher that rejected genuine .ghe.com subdomains
// would fail this subtest end-to-end, not merely at the matcher-unit level.
func TestIssueInstallationToken_AllowlistAllows(t *testing.T) {
	cases := []struct {
		name      string
		resolved  string
		allowlist []string
	}{
		{"exact host", "https://acme.ghe.com", []string{"acme.ghe.com"}},
		{"leading-dot suffix subdomain", "https://acme.ghe.com", []string{".ghe.com"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			overrideFake, overrideSrv := newFakeGitHubTLS(t)
			c := newTestClient(t, "")
			c.HTTP = hostRewriteClient(t, overrideSrv)
			c.AllowedInstallationHosts = tc.allowlist
			c.ResolveBaseURL = func(context.Context, string) (string, error) { return tc.resolved, nil }

			tok, err := c.IssueInstallationToken(context.Background(), 42)
			if err != nil {
				t.Fatalf("IssueInstallationToken: %v (allowlisted host must still mint)", err)
			}
			if tok.Token != "ghs_canned_token" {
				t.Errorf("Token = %q", tok.Token)
			}
			if overrideFake.requestCounter != 1 {
				t.Errorf("override host received %d requests, want 1", overrideFake.requestCounter)
			}
		})
	}
}

// TestIssueInstallationToken_AllowlistSuffixMatchesSubdomain proves the
// leading-dot suffix admits a real subdomain (acme.ghe.com under .ghe.com) at
// the matcher level, complementing the end-to-end suffix-mint assertion in
// TestIssueInstallationToken_AllowlistAllows. This is binding condition 1(b).
func TestIssueInstallationToken_AllowlistSuffixMatchesSubdomain(t *testing.T) {
	if !matchesHostAllowlist("acme.ghe.com", []string{".ghe.com"}) {
		t.Error("matchesHostAllowlist(acme.ghe.com, .ghe.com) = false, want true (subdomain must match)")
	}
	if !matchesHostAllowlist("acme.ghe.com", []string{"acme.ghe.com"}) {
		t.Error("matchesHostAllowlist(acme.ghe.com, acme.ghe.com) = false, want true (exact must match)")
	}
	// The bare apex is NOT admitted by the dotted suffix alone.
	if matchesHostAllowlist("ghe.com", []string{".ghe.com"}) {
		t.Error("matchesHostAllowlist(ghe.com, .ghe.com) = true, want false (apex needs explicit entry)")
	}
}

// TestIssueInstallationToken_AllowlistEmptyInert pins the default posture: an
// empty allowlist leaves the pre-#2093 behavior untouched — a well-formed HTTPS
// override host still mints (scheme/parse validation only). This is a
// behavioral done-means test that fails on a no-op/comment-only touch.
func TestIssueInstallationToken_AllowlistEmptyInert(t *testing.T) {
	overrideFake, overrideSrv := newFakeGitHubTLS(t)
	c := newTestClient(t, "")
	c.HTTP = overrideSrv.Client()
	c.AllowedInstallationHosts = nil // default: allowlist inert
	c.ResolveBaseURL = func(context.Context, string) (string, error) { return overrideSrv.URL, nil }

	tok, err := c.IssueInstallationToken(context.Background(), 42)
	if err != nil {
		t.Fatalf("IssueInstallationToken: %v (empty allowlist must not gate a well-formed host)", err)
	}
	if tok.Token != "ghs_canned_token" {
		t.Errorf("Token = %q", tok.Token)
	}
	if overrideFake.requestCounter != 1 {
		t.Errorf("override host received %d requests, want 1 (empty allowlist → mint proceeds)", overrideFake.requestCounter)
	}
}

// TestIssueInstallationToken_AllowlistRejects pins binding condition 2 and the
// credential-never-shipped invariant: with a non-empty allowlist, a well-formed
// but non-allowlisted HTTPS host FAILS the mint before the App JWT ships, and
// the override fake receives NO request (no Authorization header ever reaches
// it). Includes the look-alike substring host (notghe.com) rejected by a
// .ghe.com/ghe.com entry — true label-boundary matching, not strings.HasSuffix.
func TestIssueInstallationToken_AllowlistRejects(t *testing.T) {
	cases := []struct {
		name      string
		allowlist []string
	}{
		{"non-allowlisted host", []string{"acme.ghe.com"}},
		{"look-alike substring vs dotted suffix", []string{".ghe.com"}},
		{"look-alike substring vs exact apex", []string{"ghe.com"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The override resolves to a look-alike / non-allowlisted host. We
			// point the resolver at a well-formed https URL for a host that is
			// NOT in the allowlist; the mint must fail before any request ships.
			overrideFake, overrideSrv := newFakeGitHubTLS(t)
			c := newTestClient(t, "")
			// hostRewriteClient dials overrideSrv for ANY hostname, so the
			// look-alike notghe.com WOULD reach the fake IF the matcher wrongly
			// admitted it — this makes the credential-never-shipped assertion
			// below detect real transmission, not a DNS/connection failure to an
			// unrelated host. The matcher rejects, so nothing ever dials.
			c.HTTP = hostRewriteClient(t, overrideSrv)
			c.AllowedInstallationHosts = tc.allowlist
			// Resolve to a well-formed https host that is a look-alike / not
			// allowlisted. Use a fixed hostname so the matcher — not the httptest
			// port — decides. The request must never reach overrideSrv.
			c.ResolveBaseURL = func(context.Context, string) (string, error) {
				return "https://notghe.com", nil
			}

			_, err := c.IssueInstallationToken(context.Background(), 42)
			if err == nil {
				t.Fatal("IssueInstallationToken succeeded, want failure (non-allowlisted host must not ship the App JWT)")
			}
			if !strings.Contains(err.Error(), "allowlist") {
				t.Errorf("err = %v, want an allowlist-rejection error", err)
			}
			if overrideFake.requestCounter != 0 {
				t.Errorf("override host received %d requests, want 0 (credential-never-shipped invariant)", overrideFake.requestCounter)
			}
			if overrideFake.gotAuth != "" {
				t.Errorf("override host saw Authorization = %q, want empty (JWT must never ship on reject)", overrideFake.gotAuth)
			}
		})
	}
}

func TestMatchesHostAllowlist(t *testing.T) {
	cases := []struct {
		host      string
		allowlist []string
		want      bool
	}{
		{"acme.ghe.com", []string{"acme.ghe.com"}, true},           // exact
		{"acme.ghe.com", []string{".ghe.com"}, true},               // suffix subdomain
		{"deep.acme.ghe.com", []string{".ghe.com"}, true},          // multi-label subdomain
		{"ghe.com", []string{"ghe.com"}, true},                     // exact apex
		{"ghe.com", []string{".ghe.com"}, false},                   // apex not admitted by dotted suffix
		{"notghe.com", []string{".ghe.com"}, false},                // look-alike vs dotted suffix
		{"notghe.com", []string{"ghe.com"}, false},                 // look-alike vs exact apex
		{"other.example.com", []string{"acme.ghe.com"}, false},     // unrelated
		{"acme.ghe.com", []string{".other.com", ".ghe.com"}, true}, // second entry matches
		{"acme.ghe.com", nil, false},                               // empty allowlist matches nothing
	}
	for _, tc := range cases {
		if got := matchesHostAllowlist(tc.host, tc.allowlist); got != tc.want {
			t.Errorf("matchesHostAllowlist(%q, %v) = %v, want %v", tc.host, tc.allowlist, got, tc.want)
		}
	}
}

// TestHostAllowed covers hostAllowed directly, including the fail-closed
// parse-error branch (a malformed URL → NOT allowed) which is unreachable from
// IssueInstallationToken (validateResolvedBaseURL parses first) but must still
// fail closed if reached.
func TestHostAllowed(t *testing.T) {
	cases := []struct {
		name      string
		resolved  string
		allowlist []string
		want      bool
	}{
		{"exact host allowed", "https://acme.ghe.com", []string{"acme.ghe.com"}, true},
		{"suffix subdomain allowed", "https://acme.ghe.com/api/v3", []string{".ghe.com"}, true},
		{"host with port stripped", "https://acme.ghe.com:443", []string{"acme.ghe.com"}, true},
		{"uppercase host normalized", "https://ACME.GHE.COM", []string{"acme.ghe.com"}, true},
		{"look-alike rejected", "https://notghe.com", []string{".ghe.com"}, false},
		{"non-allowlisted rejected", "https://evil.example.com", []string{"acme.ghe.com"}, false},
		{"malformed url fails closed", "https://\x00bad", []string{"anything"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostAllowed(tc.resolved, tc.allowlist); got != tc.want {
				t.Errorf("hostAllowed(%q, %v) = %v, want %v", tc.resolved, tc.allowlist, got, tc.want)
			}
		})
	}
}

func TestValidateResolvedBaseURL(t *testing.T) {
	valid := []string{
		"https://acme.ghe.com",
		"https://acme.ghe.com/api/v3",
	}
	for _, s := range valid {
		if err := validateResolvedBaseURL(s); err != nil {
			t.Errorf("validateResolvedBaseURL(%q) = %v, want nil", s, err)
		}
	}
	invalid := []string{
		"http://acme.ghe.com", // not https
		"https://",            // no host
		"acme.ghe.com",        // no scheme
		"://acme.ghe.com",     // empty scheme
		"https://\x00bad",     // parse error
	}
	for _, s := range invalid {
		if err := validateResolvedBaseURL(s); err == nil {
			t.Errorf("validateResolvedBaseURL(%q) = nil, want error", s)
		}
	}
}

func TestReadBriefBody_Truncates(t *testing.T) {
	long := strings.Repeat("a", 1000)
	got := readBriefBody(strings.NewReader(long))
	if len(got) != 256 {
		t.Errorf("len = %d, want 256", len(got))
	}
}

func TestIssueInstallationToken_UsesContext(t *testing.T) {
	// A cancelled context should fail before the request lands.
	c := newTestClient(t, "http://127.0.0.1:1") // port 1 = unreachable
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.IssueInstallationToken(ctx, 42)
	if err == nil {
		t.Fatal("expected context error")
	}
}

func TestInstallationTokenPath(t *testing.T) {
	// Documents the URL shape so a maintainer changing it loudly
	// breaks this test.
	if got := fmt.Sprintf("/app/installations/%s/access_tokens", formatInt64(42)); got != "/app/installations/42/access_tokens" {
		t.Errorf("path = %q", got)
	}
}
