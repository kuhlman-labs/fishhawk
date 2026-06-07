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
)

// compareClient wires a Client to a one-off httptest server that
// records the last request path so assertions can verify the
// {base}...{head} three-dot form.
func compareClient(t *testing.T, status int, body string) (*Client, *string) {
	t.Helper()
	var lastPath string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/compare/{basehead...}",
		func(w http.ResponseWriter, r *http.Request) {
			lastPath = r.URL.Path
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
	return c, &lastPath
}

func TestCompareCommits_HappyPath(t *testing.T) {
	body := `{"commits":[{"sha":"aaa111"},{"sha":"bbb222"},{"sha":"ccc333"}]}`
	c, lastPath := compareClient(t, http.StatusOK, body)

	shas, err := c.CompareCommits(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "main", "ccc333")
	if err != nil {
		t.Fatalf("CompareCommits: %v", err)
	}
	want := []string{"aaa111", "bbb222", "ccc333"}
	if len(shas) != len(want) {
		t.Fatalf("len(shas) = %d, want %d (%v)", len(shas), len(want), shas)
	}
	for i := range want {
		if shas[i] != want[i] {
			t.Errorf("shas[%d] = %q, want %q", i, shas[i], want[i])
		}
	}
	// Three-dot {base}...{head} form.
	if want := "/repos/x/y/compare/main...ccc333"; *lastPath != want {
		t.Errorf("request path = %q, want %q", *lastPath, want)
	}
}

func TestCompareCommits_EmptyCommits(t *testing.T) {
	c, _ := compareClient(t, http.StatusOK, `{"commits":[]}`)
	shas, err := c.CompareCommits(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "main", "abc")
	if err != nil {
		t.Fatalf("CompareCommits: %v", err)
	}
	if len(shas) != 0 {
		t.Errorf("shas = %v, want empty", shas)
	}
}

func TestCompareCommits_NotFound(t *testing.T) {
	c, _ := compareClient(t, http.StatusNotFound, `{"message":"Not Found"}`)
	_, err := c.CompareCommits(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "main", "abc")
	if err == nil || !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestCompareCommits_Unprocessable(t *testing.T) {
	c, _ := compareClient(t, http.StatusUnprocessableEntity, `{"message":"no common ancestor"}`)
	_, err := c.CompareCommits(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "main", "abc")
	if err == nil || !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}

func TestCompareCommits_ValidationErrors(t *testing.T) {
	c := &Client{Tokens: &stubTokens{}}
	cases := []struct {
		name      string
		repo      RepoRef
		base      string
		head      string
		wantSubst string
	}{
		{"missing owner", RepoRef{Name: "y"}, "main", "h", "owner and name"},
		{"missing name", RepoRef{Owner: "x"}, "main", "h", "owner and name"},
		{"missing base", RepoRef{Owner: "x", Name: "y"}, "", "h", "base and head"},
		{"missing head", RepoRef{Owner: "x", Name: "y"}, "main", "", "base and head"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.CompareCommits(context.Background(), 1, tc.repo, tc.base, tc.head)
			if err == nil || !strings.Contains(err.Error(), tc.wantSubst) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSubst)
			}
		})
	}
}

func TestCompareCommits_MissingTokens(t *testing.T) {
	c := &Client{} // no Tokens
	_, err := c.CompareCommits(context.Background(), 1,
		RepoRef{Owner: "x", Name: "y"}, "main", "h")
	if err == nil || !strings.Contains(err.Error(), "TokenProvider") {
		t.Errorf("err = %v, want TokenProvider error", err)
	}
}
