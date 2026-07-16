package githubclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// recordingCredentialProvider is a minimal forge.CredentialProvider for
// tests: it records the scope it was last asked for and returns a
// canned token or error, never touching GitHub.
type recordingCredentialProvider struct {
	token       string
	err         error
	called      bool
	calledScope forge.CredentialScope
}

func (p *recordingCredentialProvider) Token(_ context.Context, scope forge.CredentialScope) (string, error) {
	p.called = true
	p.calledScope = scope
	if p.err != nil {
		return "", p.err
	}
	return p.token, nil
}

// scopedFileServer stands in for a single-endpoint api.github.com contents
// route, recording the Authorization header of the last request and
// counting how many requests it served (so tests can assert NO outbound
// HTTP happened on a fail-closed path).
func scopedFileServer(t *testing.T) (*httptest.Server, *string, *int) {
	t.Helper()
	var lastAuth string
	hits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/contents/{path...}",
		func(w http.ResponseWriter, r *http.Request) {
			hits++
			lastAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"path":"f.txt","sha":"abc123","content":"aGk=","encoding":"base64","type":"file"}`))
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &lastAuth, &hits
}

func TestGetFileScoped_WireThroughCredentialProvider(t *testing.T) {
	srv, lastAuth, hits := scopedFileServer(t)
	provider := &recordingCredentialProvider{token: "provider-token"}
	c := NewWithCredentialProvider(provider)
	c.BaseURL = srv.URL
	c.HTTP = &http.Client{Timeout: 5 * time.Second}

	scope := forge.FromGitHubInstallationID(4242)
	fc, err := c.GetFileScoped(context.Background(), scope, RepoRef{Owner: "x", Name: "y"}, "f.txt", "main")
	if err != nil {
		t.Fatalf("GetFileScoped: %v", err)
	}
	if fc.SHA != "abc123" {
		t.Fatalf("SHA = %q, want %q", fc.SHA, "abc123")
	}
	if *hits != 1 {
		t.Fatalf("server hits = %d, want 1", *hits)
	}
	if !provider.called || provider.calledScope.Ref() != "4242" {
		t.Fatalf("provider called=%v scope=%q, want called with ref 4242", provider.called, provider.calledScope.Ref())
	}
	if want := "Bearer provider-token"; *lastAuth != want {
		t.Fatalf("Authorization header = %q, want %q", *lastAuth, want)
	}
}

func TestGetFileScoped_WireThroughClassicTokenProvider(t *testing.T) {
	srv, lastAuth, hits := scopedFileServer(t)
	c := New(&stubTokens{token: "classic-token"})
	c.BaseURL = srv.URL
	c.HTTP = &http.Client{Timeout: 5 * time.Second}

	scope := forge.FromGitHubInstallationID(42)
	fc, err := c.GetFileScoped(context.Background(), scope, RepoRef{Owner: "x", Name: "y"}, "f.txt", "main")
	if err != nil {
		t.Fatalf("GetFileScoped: %v", err)
	}
	if fc.SHA != "abc123" {
		t.Fatalf("SHA = %q, want %q", fc.SHA, "abc123")
	}
	if *hits != 1 {
		t.Fatalf("server hits = %d, want 1", *hits)
	}
	if want := "Bearer classic-token"; *lastAuth != want {
		t.Fatalf("Authorization header = %q, want %q", *lastAuth, want)
	}
}

func TestGetFileScoped_ZeroScopeNoRequest(t *testing.T) {
	srv, _, hits := scopedFileServer(t)
	c := New(&stubTokens{token: "unused"})
	c.BaseURL = srv.URL
	c.HTTP = &http.Client{Timeout: 5 * time.Second}

	_, err := c.GetFileScoped(context.Background(), forge.CredentialScope{}, RepoRef{Owner: "x", Name: "y"}, "f.txt", "main")
	if err == nil {
		t.Fatal("GetFileScoped with zero scope: got nil error, want non-nil")
	}
	if *hits != 0 {
		t.Fatalf("server hits = %d, want 0 (no outbound HTTP on zero scope)", *hits)
	}
}

func TestGetFileScoped_NonNumericRefNamesRefNoRequest(t *testing.T) {
	srv, _, hits := scopedFileServer(t)
	c := New(&stubTokens{token: "unused"})
	c.BaseURL = srv.URL
	c.HTTP = &http.Client{Timeout: 5 * time.Second}

	_, err := c.GetFileScoped(context.Background(), forge.FromRef("gitlab-group/42"), RepoRef{Owner: "x", Name: "y"}, "f.txt", "main")
	if err == nil {
		t.Fatal("GetFileScoped with non-numeric ref: got nil error, want non-nil")
	}
	if !strings.Contains(err.Error(), "gitlab-group/42") {
		t.Fatalf("error = %q, want it to name the offending ref", err.Error())
	}
	if *hits != 0 {
		t.Fatalf("server hits = %d, want 0 (no outbound HTTP on unparseable ref)", *hits)
	}
}

func TestNewWithCredentialProvider_Int64MethodRoundTripsThroughAdapter(t *testing.T) {
	srv, lastAuth, hits := scopedFileServer(t)
	provider := &recordingCredentialProvider{token: "adapter-token"}
	c := NewWithCredentialProvider(provider)
	c.BaseURL = srv.URL
	c.HTTP = &http.Client{Timeout: 5 * time.Second}

	fc, err := c.GetFile(context.Background(), 4242, RepoRef{Owner: "x", Name: "y"}, "f.txt", "main")
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if fc.SHA != "abc123" {
		t.Fatalf("SHA = %q, want %q", fc.SHA, "abc123")
	}
	if *hits != 1 {
		t.Fatalf("server hits = %d, want 1", *hits)
	}
	if !provider.called || provider.calledScope.Ref() != "4242" {
		t.Fatalf("provider called=%v scope=%q, want called with ref 4242", provider.called, provider.calledScope.Ref())
	}
	if want := "Bearer adapter-token"; *lastAuth != want {
		t.Fatalf("Authorization header = %q, want %q", *lastAuth, want)
	}
}

func TestGetFileScoped_NilTokensReturnsExistingErrorNotPanic(t *testing.T) {
	c := &Client{HTTP: &http.Client{Timeout: 5 * time.Second}}

	_, err := c.GetFileScoped(context.Background(), forge.FromGitHubInstallationID(4242), RepoRef{Owner: "x", Name: "y"}, "f.txt", "main")
	if err == nil {
		t.Fatal("GetFileScoped on a nil-Tokens client: got nil error, want non-nil")
	}
	if !strings.Contains(err.Error(), "missing TokenProvider") {
		t.Fatalf("error = %q, want it to be the existing missing-TokenProvider error", err.Error())
	}
}
