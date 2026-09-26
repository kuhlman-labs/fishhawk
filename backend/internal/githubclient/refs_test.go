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

// deleteRefClient wires a Client to a one-off httptest server answering
// DELETE .../git/refs/heads/{branch...} with status + body, recording the
// method and the RAW (still-escaped) request path so a slash-escaping
// regression is observable.
func deleteRefClient(t *testing.T, status int, respBody string) (*Client, *deleteRefCapture) {
	t.Helper()
	cap := &deleteRefCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.calls++
		cap.method = r.Method
		cap.rawPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	c := &Client{
		BaseURL: srv.URL,
		Tokens:  &stubTokens{token: "ghs_canned_token"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
	return c, cap
}

type deleteRefCapture struct {
	calls   int
	method  string
	rawPath string
}

// TestDeleteRef_SlashedBranchPathPreserved pins that a slice branch's
// slashes reach GitHub as ref-path separators: escapePath, not
// url.PathEscape (which would send fishhawk%2Frun-...%2Fslice-2).
func TestDeleteRef_SlashedBranchPathPreserved(t *testing.T) {
	c, cap := deleteRefClient(t, http.StatusNoContent, "")
	err := c.DeleteRef(context.Background(), forge.FromGitHubInstallationID(42),
		RepoRef{Owner: "x", Name: "y"}, "fishhawk/run-abc12345/slice-2")
	if err != nil {
		t.Fatalf("DeleteRef: %v", err)
	}
	if cap.method != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", cap.method)
	}
	if want := "/repos/x/y/git/refs/heads/fishhawk/run-abc12345/slice-2"; cap.rawPath != want {
		t.Errorf("path = %q, want %q", cap.rawPath, want)
	}
}

// TestDeleteRef_MissingReferenceIsBenign pins the RefDeleter idempotency
// contract: GitHub's absent-ref 422 ("Reference does not exist") and a 404
// both return nil, so a re-sweep is a no-op rather than an error.
func TestDeleteRef_MissingReferenceIsBenign(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"422 reference does not exist", http.StatusUnprocessableEntity, `{"message":"Reference does not exist"}`},
		{"404 not found", http.StatusNotFound, `{"message":"Not Found"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, cap := deleteRefClient(t, tc.status, tc.body)
			err := c.DeleteRef(context.Background(), forge.FromGitHubInstallationID(42),
				RepoRef{Owner: "x", Name: "y"}, "fishhawk/run-abc12345/stage-def67890")
			if err != nil {
				t.Errorf("err = %v, want nil for an already-absent ref", err)
			}
			if cap.calls != 1 {
				t.Errorf("calls = %d, want 1", cap.calls)
			}
		})
	}
}

// TestDeleteRef_OtherValidationErrorSurfaces pins that ONLY the absent-ref
// 422 is benign: a 422 carrying any other message stays ErrValidation, so
// a real refusal is never reported as an already-absent branch.
func TestDeleteRef_OtherValidationErrorSurfaces(t *testing.T) {
	c, _ := deleteRefClient(t, http.StatusUnprocessableEntity, `{"message":"Invalid request"}`)
	err := c.DeleteRef(context.Background(), forge.FromGitHubInstallationID(42),
		RepoRef{Owner: "x", Name: "y"}, "fishhawk/run-abc12345/slice-1")
	if !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}

// TestDeleteRef_ForbiddenSurfacesErrForbidden pins that a protected-ref
// refusal is surfaced, not swallowed.
func TestDeleteRef_ForbiddenSurfacesErrForbidden(t *testing.T) {
	c, _ := deleteRefClient(t, http.StatusForbidden, `{"message":"Cannot delete protected branch"}`)
	err := c.DeleteRef(context.Background(), forge.FromGitHubInstallationID(42),
		RepoRef{Owner: "x", Name: "y"}, "fishhawk/run-abc12345/slice-1")
	if !errors.Is(err, forge.ErrForbidden) {
		t.Errorf("err = %v, want forge.ErrForbidden", err)
	}
}

// TestDeleteRef_ServerErrorSurfaces pins that an unclassified failure is
// a non-nil error (never a silent success).
func TestDeleteRef_ServerErrorSurfaces(t *testing.T) {
	c, _ := deleteRefClient(t, http.StatusInternalServerError, `{"message":"boom"}`)
	err := c.DeleteRef(context.Background(), forge.FromGitHubInstallationID(42),
		RepoRef{Owner: "x", Name: "y"}, "fishhawk/run-abc12345/slice-1")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want a 500 error", err)
	}
}

// TestDeleteRef_ArgumentGuards pins each argument guard fails before any
// HTTP call (the server records zero calls).
func TestDeleteRef_ArgumentGuards(t *testing.T) {
	cases := []struct {
		name      string
		noTokens  bool
		scope     forge.CredentialScope
		repo      RepoRef
		branch    string
		wantSubst string
	}{
		{"zero scope", false, forge.CredentialScope{}, RepoRef{Owner: "x", Name: "y"}, "b", "credential scope is empty"},
		{"missing tokens", true, forge.FromGitHubInstallationID(1), RepoRef{Owner: "x", Name: "y"}, "b", "TokenProvider"},
		{"missing owner", false, forge.FromGitHubInstallationID(1), RepoRef{Name: "y"}, "b", "owner and name"},
		{"missing name", false, forge.FromGitHubInstallationID(1), RepoRef{Owner: "x"}, "b", "owner and name"},
		{"missing branch", false, forge.FromGitHubInstallationID(1), RepoRef{Owner: "x", Name: "y"}, "", "branch is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, cap := deleteRefClient(t, http.StatusNoContent, "")
			if tc.noTokens {
				c.Tokens = nil
			}
			err := c.DeleteRef(context.Background(), tc.scope, tc.repo, tc.branch)
			if err == nil || !strings.Contains(err.Error(), tc.wantSubst) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSubst)
			}
			if cap.calls != 0 {
				t.Errorf("calls = %d, want 0 (guard must fire before HTTP)", cap.calls)
			}
		})
	}
}
