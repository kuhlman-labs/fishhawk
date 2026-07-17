package githubclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// forceUpdateClient wires a Client to a one-off httptest server that
// records the last request path, method, and decoded body so assertions
// can verify the PATCH .../git/refs/heads/{branch} call carries
// {sha, force:true}.
func forceUpdateClient(t *testing.T, status int, respBody string) (*Client, *forceUpdateCapture) {
	t.Helper()
	cap := &forceUpdateCapture{}
	mux := http.NewServeMux()
	mux.HandleFunc("PATCH /repos/{owner}/{repo}/git/refs/heads/{branch...}",
		func(w http.ResponseWriter, r *http.Request) {
			cap.path = r.URL.Path
			cap.method = r.Method
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &cap.body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, respBody)
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{
		BaseURL: srv.URL,
		Tokens:  &stubTokens{token: "ghs_canned_token"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_app_jwt", nil },
	}
	return c, cap
}

type forceUpdateCapture struct {
	path   string
	method string
	body   struct {
		SHA   string `json:"sha"`
		Force bool   `json:"force"`
	}
}

func TestForceUpdateRef_HappyPath(t *testing.T) {
	c, cap := forceUpdateClient(t, http.StatusOK,
		`{"ref":"refs/heads/fishhawk/run/x","object":{"sha":"aaa111"}}`)

	err := c.ForceUpdateRef(context.Background(), forge.FromGitHubInstallationID(42),
		RepoRef{Owner: "x", Name: "y"}, "fishhawk/run/x", "aaa111")
	if err != nil {
		t.Fatalf("ForceUpdateRef: %v", err)
	}
	if cap.method != http.MethodPatch {
		t.Errorf("method = %q, want PATCH", cap.method)
	}
	if want := "/repos/x/y/git/refs/heads/fishhawk/run/x"; cap.path != want {
		t.Errorf("path = %q, want %q", cap.path, want)
	}
	// The rewind requires force:true — a non-fast-forward update is
	// rejected without it.
	if !cap.body.Force {
		t.Error("request body missing force:true")
	}
	if cap.body.SHA != "aaa111" {
		t.Errorf("body sha = %q, want %q", cap.body.SHA, "aaa111")
	}
}

func TestForceUpdateRef_ErrorPath(t *testing.T) {
	c, _ := forceUpdateClient(t, http.StatusUnprocessableEntity,
		`{"message":"Object does not exist"}`)
	err := c.ForceUpdateRef(context.Background(), forge.FromGitHubInstallationID(42),
		RepoRef{Owner: "x", Name: "y"}, "fishhawk/run/x", "deadbeef")
	if err == nil || !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}

func TestForceUpdateRef_NotFound(t *testing.T) {
	c, _ := forceUpdateClient(t, http.StatusNotFound, `{"message":"Not Found"}`)
	err := c.ForceUpdateRef(context.Background(), forge.FromGitHubInstallationID(42),
		RepoRef{Owner: "x", Name: "y"}, "branch", "aaa111")
	if err == nil || !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestForceUpdateRef_ValidationErrors(t *testing.T) {
	c := &Client{Tokens: &stubTokens{}}
	cases := []struct {
		name      string
		repo      RepoRef
		branch    string
		newSHA    string
		wantSubst string
	}{
		{"missing owner", RepoRef{Name: "y"}, "b", "s", "owner and name"},
		{"missing name", RepoRef{Owner: "x"}, "b", "s", "owner and name"},
		{"missing branch", RepoRef{Owner: "x", Name: "y"}, "", "s", "branch is required"},
		{"missing sha", RepoRef{Owner: "x", Name: "y"}, "b", "", "newSHA is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := c.ForceUpdateRef(context.Background(), forge.FromGitHubInstallationID(1), tc.repo, tc.branch, tc.newSHA)
			if err == nil || !strings.Contains(err.Error(), tc.wantSubst) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSubst)
			}
		})
	}
}

func TestForceUpdateRef_MissingTokens(t *testing.T) {
	c := &Client{} // no Tokens
	err := c.ForceUpdateRef(context.Background(), forge.FromGitHubInstallationID(1),
		RepoRef{Owner: "x", Name: "y"}, "b", "s")
	if err == nil || !strings.Contains(err.Error(), "TokenProvider") {
		t.Errorf("err = %v, want TokenProvider error", err)
	}
}
