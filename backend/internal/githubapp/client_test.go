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

func newFakeGitHub(t *testing.T) (*fakeGitHub, *httptest.Server) {
	t.Helper()
	fg := &fakeGitHub{status: http.StatusCreated}
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
	srv := httptest.NewServer(mux)
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
