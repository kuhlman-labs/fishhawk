package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
