package githubclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// contentsClient wires a Client to a one-off httptest server that
// records the last request path + raw query so assertions can verify
// the contents-API path shape and ?ref= propagation.
func contentsClient(t *testing.T, status int, body string) (*Client, *string, *string) {
	t.Helper()
	var lastPath, lastQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/contents/{path...}",
		func(w http.ResponseWriter, r *http.Request) {
			lastPath = r.URL.Path
			lastQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{
		BaseURL: srv.URL,
		Tokens:  &stubTokens{token: "ghs_canned_token"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_app_jwt", nil },
	}
	return c, &lastPath, &lastQuery
}

func TestListDirectory_HappyPath(t *testing.T) {
	body := `[
		{"name":"upload.go","path":"backend/internal/server/upload.go","type":"file"},
		{"name":"upload_test.go","path":"backend/internal/server/upload_test.go","type":"file"},
		{"name":"testdata","path":"backend/internal/server/testdata","type":"dir"}
	]`
	c, lastPath, lastQuery := contentsClient(t, http.StatusOK, body)

	entries, err := c.ListDirectory(context.Background(), forge.FromGitHubInstallationID(42),
		RepoRef{Owner: "x", Name: "y"}, "backend/internal/server", "main")
	if err != nil {
		t.Fatalf("ListDirectory: %v", err)
	}
	want := []DirEntry{
		{Name: "upload.go", Path: "backend/internal/server/upload.go", Type: "file"},
		{Name: "upload_test.go", Path: "backend/internal/server/upload_test.go", Type: "file"},
		{Name: "testdata", Path: "backend/internal/server/testdata", Type: "dir"},
	}
	if len(entries) != len(want) {
		t.Fatalf("len(entries) = %d, want %d (%v)", len(entries), len(want), entries)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Errorf("entries[%d] = %+v, want %+v", i, entries[i], want[i])
		}
	}
	if want := "/repos/x/y/contents/backend/internal/server"; *lastPath != want {
		t.Errorf("request path = %q, want %q", *lastPath, want)
	}
	if want := "ref=main"; *lastQuery != want {
		t.Errorf("request query = %q, want %q", *lastQuery, want)
	}
}

func TestListDirectory_EmptyRefOmitsQuery(t *testing.T) {
	c, _, lastQuery := contentsClient(t, http.StatusOK, `[]`)

	entries, err := c.ListDirectory(context.Background(), forge.FromGitHubInstallationID(42),
		RepoRef{Owner: "x", Name: "y"}, "docs", "")
	if err != nil {
		t.Fatalf("ListDirectory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("entries = %v, want empty", entries)
	}
	if *lastQuery != "" {
		t.Errorf("request query = %q, want empty (default-branch listing)", *lastQuery)
	}
}

func TestListDirectory_NotFound(t *testing.T) {
	c, _, _ := contentsClient(t, http.StatusNotFound, `{"message":"Not Found"}`)
	_, err := c.ListDirectory(context.Background(), forge.FromGitHubInstallationID(42),
		RepoRef{Owner: "x", Name: "y"}, "no/such/dir", "")
	if err == nil || !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestListDirectory_FilePathReturnsObject pins the file-path case: the
// Contents API answers a file path with a single JSON OBJECT (GetFile's
// shape), not a listing array. ListDirectory must surface a descriptive
// error rather than panicking or silently returning zero entries.
func TestListDirectory_FilePathReturnsObject(t *testing.T) {
	body := `{"name":"upload.go","path":"backend/internal/server/upload.go","type":"file","content":"cGtn","encoding":"base64"}`
	c, _, _ := contentsClient(t, http.StatusOK, body)

	_, err := c.ListDirectory(context.Background(), forge.FromGitHubInstallationID(42),
		RepoRef{Owner: "x", Name: "y"}, "backend/internal/server/upload.go", "")
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("err = %v, want a 'not a directory' error", err)
	}
}

func TestListDirectory_ValidationErrors(t *testing.T) {
	c := &Client{Tokens: &stubTokens{}}
	cases := []struct {
		name      string
		repo      RepoRef
		path      string
		wantSubst string
	}{
		{"missing owner", RepoRef{Name: "y"}, "docs", "owner and name"},
		{"missing name", RepoRef{Owner: "x"}, "docs", "owner and name"},
		{"missing path", RepoRef{Owner: "x", Name: "y"}, "", "path required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.ListDirectory(context.Background(), forge.FromGitHubInstallationID(1), tc.repo, tc.path, "")
			if err == nil || !strings.Contains(err.Error(), tc.wantSubst) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSubst)
			}
		})
	}
}

func TestListDirectory_MissingTokens(t *testing.T) {
	c := &Client{} // no Tokens
	_, err := c.ListDirectory(context.Background(), forge.FromGitHubInstallationID(1),
		RepoRef{Owner: "x", Name: "y"}, "docs", "")
	if err == nil || !strings.Contains(err.Error(), "TokenProvider") {
		t.Errorf("err = %v, want TokenProvider error", err)
	}
}
