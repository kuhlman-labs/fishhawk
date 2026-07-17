package github_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	forgegithub "github.com/kuhlman-labs/fishhawk/backend/internal/forge/github"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
)

// stubTokens is a no-op githubapp.TokenProvider. ResolveRepoScope's only
// GitHub call (GetRepoInstallation) authenticates with the App JWT, not
// an installation token, so Token is never invoked — but the field must
// be non-nil to build a Client.
type stubTokens struct{}

func (stubTokens) Token(context.Context, int64) (string, error) { return "unused", nil }

// newAdapter builds a *forgegithub.Forge whose embedded client points at
// an httptest server that answers GET /repos/{owner}/{repo}/installation
// with the given status and body.
func newAdapter(t *testing.T, status int, body string) *forgegithub.Forge {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != "" {
			_, _ = io.WriteString(w, body)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  stubTokens{},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_app_jwt", nil },
	}
	return forgegithub.New(c)
}

// TestSatisfiesForgeInterface is the compile-time contract restated as a
// runtime assertion: the embedded client plus Name/ResolveRepoScope must
// cover the whole forge.Forge surface. Reaching a non-empty Name through
// the interface value proves the adapter is dispatchable as a forge.Forge.
func TestSatisfiesForgeInterface(t *testing.T) {
	var f forge.Forge = newAdapter(t, http.StatusOK, `{"id":1}`)
	if f.Name() != "github" {
		t.Errorf("forge.Forge.Name() = %q, want %q", f.Name(), "github")
	}
}

// TestName pins the registry id.
func TestName(t *testing.T) {
	if got := newAdapter(t, http.StatusOK, `{"id":1}`).Name(); got != "github" {
		t.Errorf("Name() = %q, want %q", got, "github")
	}
}

// TestResolveRepoScopeSuccess is the happy path: a resolved installation
// id becomes a CredentialScope whose ref is the stringified id.
func TestResolveRepoScopeSuccess(t *testing.T) {
	f := newAdapter(t, http.StatusOK, `{"id":12345}`)

	scope, err := f.ResolveRepoScope(context.Background(), forge.RepoRef{Owner: "o", Name: "n"})
	if err != nil {
		t.Fatalf("ResolveRepoScope: %v", err)
	}
	if scope.Ref() != "12345" {
		t.Errorf("scope.Ref() = %q, want %q", scope.Ref(), "12345")
	}
	// Round-trips back to the installation id via the GitHub accessor.
	id, err := scope.GitHubInstallationID()
	if err != nil {
		t.Fatalf("GitHubInstallationID: %v", err)
	}
	if id != 12345 {
		t.Errorf("installation id = %d, want 12345", id)
	}
}

// TestResolveRepoScopeNotInstalled is the failure path: a not-installed
// repo (404 on the installation endpoint) propagates githubclient's
// ErrNotInstalled UNMODIFIED — the adapter must not launder it into a
// generic error or the zero scope with a nil error.
func TestResolveRepoScopeNotInstalled(t *testing.T) {
	f := newAdapter(t, http.StatusNotFound, `{"message":"Not Found"}`)

	scope, err := f.ResolveRepoScope(context.Background(), forge.RepoRef{Owner: "o", Name: "n"})
	if err == nil {
		t.Fatal("expected an error for a not-installed repo")
	}
	if !errors.Is(err, forge.ErrNotInstalled) {
		t.Errorf("err = %v, want forge.ErrNotInstalled", err)
	}
	// The propagated ErrNotInstalled is distinct from ErrNotFound — the
	// distinction callers switch on survives the adapter.
	if errors.Is(err, forge.ErrNotFound) {
		t.Errorf("ErrNotInstalled must stay distinct from ErrNotFound; err = %v", err)
	}
	if !scope.IsZero() {
		t.Errorf("scope = %v on error, want the zero scope", scope)
	}
}

// TestResolveRepoScopeOtherError confirms a non-404 upstream failure
// also propagates (not misclassified as not-installed) and yields the
// zero scope.
func TestResolveRepoScopeOtherError(t *testing.T) {
	f := newAdapter(t, http.StatusInternalServerError, `{"message":"boom"}`)

	scope, err := f.ResolveRepoScope(context.Background(), forge.RepoRef{Owner: "o", Name: "n"})
	if err == nil {
		t.Fatal("expected an error for a 500 response")
	}
	if errors.Is(err, forge.ErrNotInstalled) {
		t.Errorf("a 500 must not become ErrNotInstalled; err = %v", err)
	}
	if !scope.IsZero() {
		t.Errorf("scope = %v on error, want the zero scope", scope)
	}
}
