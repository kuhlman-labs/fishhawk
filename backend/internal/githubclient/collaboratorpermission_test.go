package githubclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

type permFake struct {
	status   int
	body     string
	requests atomic.Int32
	path     string
	auth     string
}

func permClient(t *testing.T) (*Client, *permFake, *stubTokens) {
	t.Helper()
	f := &permFake{status: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		f.path = r.URL.EscapedPath()
		f.auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.body)
	}))
	t.Cleanup(srv.Close)
	tok := &stubTokens{token: "ghs_run_install"}
	return &Client{BaseURL: srv.URL, Tokens: tok, HTTP: &http.Client{Timeout: 5 * time.Second}}, f, tok
}

func TestCollaboratorPermission_RoleNamePrecedence(t *testing.T) {
	cases := []struct {
		name, body, wantRole, wantPerm, wantEffective string
	}{
		{"maintain over write", `{"permission":"write","role_name":"maintain"}`, "maintain", "write", "maintain"},
		{"triage over read", `{"permission":"read","role_name":"triage"}`, "triage", "read", "triage"},
		{"admin", `{"permission":"admin","role_name":"admin"}`, "admin", "admin", "admin"},
		{"custom role falls back to permission", `{"permission":"write","role_name":"security-engineer"}`, "security-engineer", "write", "write"},
		{"no role_name falls back", `{"permission":"read"}`, "", "read", "read"},
		{"none", `{"permission":"none","role_name":""}`, "", "none", "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, f, _ := permClient(t)
			f.body = tc.body
			got, err := c.CollaboratorPermission(context.Background(), forge.FromGitHubInstallationID(7), RepoRef{Owner: "x", Name: "y"}, "alice")
			if err != nil {
				t.Fatalf("CollaboratorPermission: %v", err)
			}
			if got.RoleName != tc.wantRole || got.Permission != tc.wantPerm {
				t.Errorf("got %+v, want role=%q perm=%q", got, tc.wantRole, tc.wantPerm)
			}
			if eff := got.EffectiveRole(); eff != tc.wantEffective {
				t.Errorf("EffectiveRole = %q, want %q", eff, tc.wantEffective)
			}
		})
	}
}

func TestCollaboratorPermission_InstallationTokenAndPath(t *testing.T) {
	c, f, tok := permClient(t)
	f.body = `{"permission":"write","role_name":"write"}`
	if _, err := c.CollaboratorPermission(context.Background(), forge.FromGitHubInstallationID(77), RepoRef{Owner: "x", Name: "y"}, "a?b"); err != nil {
		t.Fatalf("CollaboratorPermission: %v", err)
	}
	if tok.installationCalled != 77 {
		t.Errorf("token minted for installation %d, want the run's 77", tok.installationCalled)
	}
	if !strings.Contains(f.auth, "ghs_run_install") {
		t.Errorf("Authorization = %q, want the installation token", f.auth)
	}
	if want := "/repos/x/y/collaborators/a%3Fb/permission"; f.path != want {
		t.Errorf("path = %q, want %q (login path-escaped)", f.path, want)
	}
}

func TestCollaboratorPermission_NotFound(t *testing.T) {
	c, f, _ := permClient(t)
	f.status = http.StatusNotFound
	f.body = `{"message":"Not Found"}`
	_, err := c.CollaboratorPermission(context.Background(), forge.FromGitHubInstallationID(7), RepoRef{Owner: "x", Name: "y"}, "ghost-user")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestCollaboratorPermission_RefusedBeforeRequest(t *testing.T) {
	cases := []struct {
		name  string
		scope forge.CredentialScope
		repo  RepoRef
		login string
	}{
		{"slash login", forge.FromGitHubInstallationID(7), RepoRef{Owner: "x", Name: "y"}, "a/b"},
		{"blank login", forge.FromGitHubInstallationID(7), RepoRef{Owner: "x", Name: "y"}, "  "},
		{"blank owner", forge.FromGitHubInstallationID(7), RepoRef{Name: "y"}, "alice"},
		{"zero scope", forge.CredentialScope{}, RepoRef{Owner: "x", Name: "y"}, "alice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, f, _ := permClient(t)
			if _, err := c.CollaboratorPermission(context.Background(), tc.scope, tc.repo, tc.login); err == nil {
				t.Fatal("want refusal, got nil")
			}
			if n := f.requests.Load(); n != 0 {
				t.Errorf("requests = %d, want 0 (refused before any request)", n)
			}
		})
	}
	c := &Client{}
	if _, err := c.CollaboratorPermission(context.Background(), forge.FromGitHubInstallationID(7), RepoRef{Owner: "x", Name: "y"}, "alice"); err == nil ||
		!strings.Contains(err.Error(), "TokenProvider") {
		t.Errorf("missing tokens: %v", err)
	}
}

func TestCollaboratorPermission_DecodeError(t *testing.T) {
	c, f, _ := permClient(t)
	f.body = `{not json`
	if _, err := c.CollaboratorPermission(context.Background(), forge.FromGitHubInstallationID(7), RepoRef{Owner: "x", Name: "y"}, "alice"); err == nil ||
		!strings.Contains(err.Error(), "decode") {
		t.Errorf("err = %v, want a decode error", err)
	}
}
