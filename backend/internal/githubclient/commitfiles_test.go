package githubclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// commitFilesClient serves GET /repos/x/y/commits/{sha} with a canned status,
// body and optional Link header, recording the request path.
func commitFilesClient(t *testing.T, status int, body, link string) (*Client, *string) {
	t.Helper()
	var lastPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path
		if link != "" {
			w.Header().Set("Link", link)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Client{
		BaseURL: srv.URL,
		Tokens:  &stubTokens{token: "ghs_canned_token"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}, &lastPath
}

func getCommitFiles(c *Client, sha string) (*CommitFiles, error) {
	return c.GetCommitFiles(context.Background(), forge.FromGitHubInstallationID(42), RepoRef{Owner: "x", Name: "y"}, sha)
}

func TestGetCommitFiles_DecodesFilesAndCounts(t *testing.T) {
	c, lastPath := commitFilesClient(t, http.StatusOK, `{"sha":"abc123","files":[
		{"filename":"a.go","status":"modified","additions":3,"deletions":1},
		{"filename":"new.go","previous_filename":"old.go","status":"renamed","additions":0,"deletions":0}
	]}`, "")
	got, err := getCommitFiles(c, "abc123")
	if err != nil {
		t.Fatalf("GetCommitFiles: %v", err)
	}
	if *lastPath != "/repos/x/y/commits/abc123" {
		t.Errorf("path = %q, want /repos/x/y/commits/abc123", *lastPath)
	}
	if got.SHA != "abc123" || got.Truncated || got.TruncationReason != "" {
		t.Fatalf("got %+v, want sha abc123, not truncated", got)
	}
	want := []CommitFile{
		{Path: "a.go", Status: "modified", Additions: 3, Deletions: 1},
		{Path: "new.go", PreviousPath: "old.go", Status: "renamed"},
	}
	if len(got.Files) != len(want) {
		t.Fatalf("files = %+v, want %+v", got.Files, want)
	}
	for i := range want {
		if got.Files[i] != want[i] {
			t.Errorf("file[%d] = %+v, want %+v", i, got.Files[i], want[i])
		}
	}
}

// TestGetCommitFiles_MissingCountsIsTruncated pins the amendment condition: a
// file entry without additions/deletions makes the list incomplete, never a
// zero count.
func TestGetCommitFiles_MissingCountsIsTruncated(t *testing.T) {
	for _, entry := range []string{
		`{"filename":"a.go","status":"modified","deletions":1}`,
		`{"filename":"a.go","status":"modified","additions":1}`,
	} {
		c, _ := commitFilesClient(t, http.StatusOK, `{"sha":"s","files":[`+entry+`]}`, "")
		got, err := getCommitFiles(c, "s")
		if err != nil {
			t.Fatalf("GetCommitFiles: %v", err)
		}
		if !got.Truncated || !strings.Contains(got.TruncationReason, "additions/deletions") {
			t.Errorf("entry %s: got %+v, want Truncated with a counts reason", entry, got)
		}
	}
}

func TestGetCommitFiles_LinkNextIsTruncated(t *testing.T) {
	c, _ := commitFilesClient(t, http.StatusOK, `{"sha":"s","files":[{"filename":"a","additions":1,"deletions":0}]}`,
		`<https://api.github.com/repositories/1/commits/s?page=2>; rel="next"`)
	got, err := getCommitFiles(c, "s")
	if err != nil {
		t.Fatalf("GetCommitFiles: %v", err)
	}
	if !got.Truncated || !strings.Contains(got.TruncationReason, "paginated") {
		t.Errorf("got %+v, want Truncated (paginated)", got)
	}
}

func TestGetCommitFiles_FullPageIsTruncated(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"sha":"s","files":[`)
	for i := 0; i < commitFilesCap; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"filename":"f%d","additions":1,"deletions":0}`, i)
	}
	b.WriteString(`]}`)
	c, _ := commitFilesClient(t, http.StatusOK, b.String(), "")
	got, err := getCommitFiles(c, "s")
	if err != nil {
		t.Fatalf("GetCommitFiles: %v", err)
	}
	if !got.Truncated {
		t.Errorf("a %d-file page must be Truncated; got %+v", commitFilesCap, got.TruncationReason)
	}
}

func TestGetCommitFiles_StatusClassification(t *testing.T) {
	c, _ := commitFilesClient(t, http.StatusNotFound, `{"message":"Not Found"}`, "")
	if _, err := getCommitFiles(c, "s"); !errors.Is(err, ErrNotFound) {
		t.Errorf("404 err = %v, want ErrNotFound", err)
	}
	c, _ = commitFilesClient(t, http.StatusBadGateway, `bad gateway`, "")
	_, err := getCommitFiles(c, "s")
	if err == nil || !strings.Contains(err.Error(), "get commit files: 502") {
		t.Errorf("502 err = %v, want an untyped status error naming 502", err)
	}
	c, _ = commitFilesClient(t, http.StatusOK, `{not json`, "")
	if _, err := getCommitFiles(c, "s"); err == nil {
		t.Error("malformed body: want a decode error")
	}
}

func TestGetCommitFiles_ValidationErrors(t *testing.T) {
	c := &Client{Tokens: &stubTokens{}}
	scope := forge.FromGitHubInstallationID(42)
	if _, err := c.GetCommitFiles(context.Background(), scope, RepoRef{Owner: "x"}, "s"); err == nil {
		t.Error("empty repo name: want error")
	}
	if _, err := c.GetCommitFiles(context.Background(), scope, RepoRef{Owner: "x", Name: "y"}, ""); err == nil {
		t.Error("empty sha: want error")
	}
	if _, err := (&Client{}).GetCommitFiles(context.Background(), scope, RepoRef{Owner: "x", Name: "y"}, "s"); err == nil {
		t.Error("missing TokenProvider: want error")
	}
	if _, err := c.GetCommitFiles(context.Background(), forge.CredentialScope{}, RepoRef{Owner: "x", Name: "y"}, "s"); err == nil {
		t.Error("zero scope: want error")
	}
}
