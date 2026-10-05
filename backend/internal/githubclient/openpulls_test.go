package githubclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// openPullsServer serves the pulls listing from a per-page handler and
// records every request's path + raw query.
type openPullsServer struct {
	mu       sync.Mutex
	requests []string
}

func (s *openPullsServer) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func openPullsClient(t *testing.T, page func(w http.ResponseWriter, r *http.Request, page int)) (*Client, *openPullsServer) {
	t.Helper()
	rec := &openPullsServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.requests = append(rec.requests, r.URL.Path+"?"+r.URL.RawQuery)
		rec.mu.Unlock()
		n, _ := strconv.Atoi(r.URL.Query().Get("page"))
		page(w, r, n)
	}))
	t.Cleanup(srv.Close)
	return &Client{
		BaseURL: srv.URL,
		Tokens:  &stubTokens{token: "ghs_canned_token"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}, rec
}

// openPullsPage renders n pull requests numbered from first.
func openPullsPage(first, n int) string {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		num := first + i
		fmt.Fprintf(&b, `{"number":%d,"html_url":"https://github.com/x/y/pull/%d","title":"t%d","body":"b%d","user":{"login":"dependabot[bot]"},"head":{"ref":"dependabot/go_modules/h%d"}}`, num, num, num, num, num)
	}
	b.WriteString("]")
	return b.String()
}

// nextLink sets a Link header advertising a next page.
func nextLink(w http.ResponseWriter, r *http.Request, page int) {
	w.Header().Set("Link", fmt.Sprintf(`<http://%s/repos/x/y/pulls?state=open&per_page=100&page=%d>; rel="next"`, r.Host, page+1))
}

func listOpenPulls(c *Client, maxPulls int) ([]OpenPullRequest, bool, error) {
	return c.ListOpenPullRequests(context.Background(), forge.FromGitHubInstallationID(42), RepoRef{Owner: "x", Name: "y"}, maxPulls)
}

// TestListOpenPullRequests_QueryDecodeAndPaging: two pages joined by a Link
// rel="next" are read in full, every field maps, and a null body reads "".
func TestListOpenPullRequests_QueryDecodeAndPaging(t *testing.T) {
	c, rec := openPullsClient(t, func(w http.ResponseWriter, r *http.Request, page int) {
		switch page {
		case 1:
			nextLink(w, r, page)
			_, _ = w.Write([]byte(`[{"number":3823,"html_url":"https://github.com/x/y/pull/3823",
				"title":"deps(backend)(deps): bump github.com/aws/aws-sdk-go-v2 from 1.47.0 to 1.47.1 in /backend",
				"body":"Bumps [github.com/aws/aws-sdk-go-v2](https://github.com/aws/aws-sdk-go-v2) from 1.47.0 to 1.47.1.",
				"user":{"login":"dependabot[bot]"},"head":{"ref":"dependabot/go_modules/backend/github.com/aws/aws-sdk-go-v2-1.47.1"},
				"base":{"ref":"main","repo":{"default_branch":"main"}},
				"state":"open"}]`))
		case 2:
			_, _ = w.Write([]byte(`[{"number":7,"html_url":"https://github.com/x/y/pull/7","title":"human","body":null,
				"user":{"login":"octocat"},"head":{"ref":"feature"},"base":{"ref":"release-1.x","repo":null}}]`))
		default:
			t.Errorf("unexpected page %d", page)
			_, _ = w.Write([]byte(`[]`))
		}
	})
	pulls, truncated, err := listOpenPulls(c, 300)
	if err != nil || truncated {
		t.Fatalf("err = %v truncated = %v, want nil/false", err, truncated)
	}
	reqs := rec.recorded()
	if len(reqs) != 2 {
		t.Fatalf("requests = %v, want 2", reqs)
	}
	for i, r := range reqs {
		path, query, _ := strings.Cut(r, "?")
		if path != "/repos/x/y/pulls" {
			t.Errorf("request %d path = %q", i, path)
		}
		for _, want := range []string{"state=open", "per_page=100", "page=" + strconv.Itoa(i+1)} {
			if !strings.Contains(query, want) {
				t.Errorf("request %d query %q missing %q", i, query, want)
			}
		}
	}
	want := []OpenPullRequest{
		{
			Number:        3823,
			HTMLURL:       "https://github.com/x/y/pull/3823",
			Title:         "deps(backend)(deps): bump github.com/aws/aws-sdk-go-v2 from 1.47.0 to 1.47.1 in /backend",
			Body:          "Bumps [github.com/aws/aws-sdk-go-v2](https://github.com/aws/aws-sdk-go-v2) from 1.47.0 to 1.47.1.",
			UserLogin:     "dependabot[bot]",
			HeadRef:       "dependabot/go_modules/backend/github.com/aws/aws-sdk-go-v2-1.47.1",
			BaseRef:       "main",
			DefaultBranch: "main",
		},
		// A null base repo leaves DefaultBranch unknown.
		{Number: 7, HTMLURL: "https://github.com/x/y/pull/7", Title: "human", UserLogin: "octocat", HeadRef: "feature", BaseRef: "release-1.x"},
	}
	if len(pulls) != len(want) {
		t.Fatalf("pulls = %+v, want %+v", pulls, want)
	}
	for i := range want {
		if pulls[i] != want[i] {
			t.Errorf("pull[%d] = %+v, want %+v", i, pulls[i], want[i])
		}
	}
}

// TestListOpenPullRequests_MaxCapTruncates: max=3 over pages of 2 returns 3
// with truncated=true and never requests page 3.
//
// Counterfactual: the second page exists and is full, so only the cap stops
// the walk — deleting the cap check returns 4 pulls with truncated=false.
func TestListOpenPullRequests_MaxCapTruncates(t *testing.T) {
	c, rec := openPullsClient(t, func(w http.ResponseWriter, r *http.Request, page int) {
		if page < 2 {
			nextLink(w, r, page)
		}
		_, _ = w.Write([]byte(openPullsPage(page*10, 2)))
	})
	pulls, truncated, err := listOpenPulls(c, 3)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(pulls) != 3 || !truncated {
		t.Fatalf("got %d pulls truncated=%v, want 3 truncated=true", len(pulls), truncated)
	}
	if pulls[2].Number != 20 {
		t.Errorf("pull[2].Number = %d, want 20 (first of page 2)", pulls[2].Number)
	}
	if n := len(rec.recorded()); n != 2 {
		t.Errorf("requests = %d, want 2", n)
	}
}

// TestListOpenPullRequests_CapAtPageBoundary: when the cap lands exactly at
// the end of a page that advertises a next page, the walk stops without
// requesting it and reports truncated; when the last page ends exactly at
// the cap with no next page, the listing is complete.
func TestListOpenPullRequests_CapAtPageBoundary(t *testing.T) {
	c, rec := openPullsClient(t, func(w http.ResponseWriter, r *http.Request, page int) {
		nextLink(w, r, page)
		_, _ = w.Write([]byte(openPullsPage(page*10, 2)))
	})
	pulls, truncated, err := listOpenPulls(c, 2)
	if err != nil || len(pulls) != 2 || !truncated {
		t.Fatalf("got %d pulls truncated=%v err=%v, want 2 truncated=true", len(pulls), truncated, err)
	}
	if n := len(rec.recorded()); n != 1 {
		t.Errorf("requests = %d, want 1 (the cap stops before page 2)", n)
	}

	c2, _ := openPullsClient(t, func(w http.ResponseWriter, _ *http.Request, page int) {
		_, _ = w.Write([]byte(openPullsPage(page*10, 2)))
	})
	pulls, truncated, err = listOpenPulls(c2, 2)
	if err != nil || len(pulls) != 2 || truncated {
		t.Fatalf("got %d pulls truncated=%v err=%v, want 2 truncated=false", len(pulls), truncated, err)
	}
}

// TestListOpenPullRequests_EmptyPageWithNextStops: an empty page that still
// advertises a next page ends the walk as truncated instead of looping.
//
// Counterfactual: every page is empty and advertises a next one, so deleting
// the empty-page stop walks until the test's page bound fails it.
func TestListOpenPullRequests_EmptyPageWithNextStops(t *testing.T) {
	c, rec := openPullsClient(t, func(w http.ResponseWriter, r *http.Request, page int) {
		if page > 5 {
			t.Errorf("walk did not stop: requested page %d", page)
			_, _ = w.Write([]byte(`[]`))
			return
		}
		nextLink(w, r, page)
		_, _ = w.Write([]byte(`[]`))
	})
	pulls, truncated, err := listOpenPulls(c, 300)
	if err != nil || len(pulls) != 0 || !truncated {
		t.Fatalf("got %d pulls truncated=%v err=%v, want 0 truncated=true", len(pulls), truncated, err)
	}
	if pulls == nil {
		t.Error("pulls = nil, want a non-nil empty slice")
	}
	if n := len(rec.recorded()); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}

// TestListOpenPullRequests_LinkHostIsNotFollowed: a next link naming another
// host is read only as "a next page exists"; the next request goes to the
// client's own base.
func TestListOpenPullRequests_LinkHostIsNotFollowed(t *testing.T) {
	c, rec := openPullsClient(t, func(w http.ResponseWriter, _ *http.Request, page int) {
		if page == 1 {
			w.Header().Set("Link", `<https://attacker.invalid/steal?page=2>; rel="next"`)
		}
		_, _ = w.Write([]byte(openPullsPage(page*10, 1)))
	})
	pulls, truncated, err := listOpenPulls(c, 300)
	if err != nil || truncated || len(pulls) != 2 {
		t.Fatalf("got %d pulls truncated=%v err=%v, want 2 complete", len(pulls), truncated, err)
	}
	reqs := rec.recorded()
	if len(reqs) != 2 || !strings.HasPrefix(reqs[1], "/repos/x/y/pulls?") {
		t.Errorf("requests = %v, want page 2 on the client's own base", reqs)
	}
}

// TestListOpenPullRequests_ErrorMapping: non-2xx statuses classify through
// classifyStatus, and an undecodable body fails.
func TestListOpenPullRequests_ErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   error
		text   string
	}{
		{http.StatusForbidden, `{"message":"Resource not accessible by integration"}`, ErrForbidden, ""},
		{http.StatusNotFound, `{"message":"Not Found"}`, ErrNotFound, ""},
		{http.StatusInternalServerError, `boom`, nil, "list open pulls: 500"},
		{http.StatusOK, `{"not":"an array"}`, nil, "decode open pulls"},
	} {
		t.Run(strconv.Itoa(tc.status)+tc.text, func(t *testing.T) {
			c, _ := openPullsClient(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			// A bare http.Client: the retry transport is New()'s, so a 500
			// returns on the first attempt.
			pulls, truncated, err := listOpenPulls(c, 300)
			if err == nil || pulls != nil || truncated {
				t.Fatalf("got pulls=%v truncated=%v err=%v, want an error", pulls, truncated, err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want errors.Is %v", err, tc.want)
			}
			if tc.text != "" && !strings.Contains(err.Error(), tc.text) {
				t.Errorf("err = %v, want it to contain %q", err, tc.text)
			}
		})
	}
}

// TestListOpenPullRequests_RefusesBadInput: argument guards fire before any
// request.
func TestListOpenPullRequests_RefusesBadInput(t *testing.T) {
	c, rec := openPullsClient(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		_, _ = w.Write([]byte(`[]`))
	})
	scope := forge.FromGitHubInstallationID(42)
	repo := RepoRef{Owner: "x", Name: "y"}
	cases := []struct {
		name string
		call func() error
	}{
		{"zero scope", func() error {
			_, _, err := c.ListOpenPullRequests(context.Background(), forge.CredentialScope{}, repo, 10)
			return err
		}},
		{"no token provider", func() error {
			_, _, err := (&Client{BaseURL: c.BaseURL, HTTP: c.HTTP}).ListOpenPullRequests(context.Background(), scope, repo, 10)
			return err
		}},
		{"blank repo", func() error {
			_, _, err := c.ListOpenPullRequests(context.Background(), scope, RepoRef{Owner: "x"}, 10)
			return err
		}},
		{"max zero", func() error {
			_, _, err := c.ListOpenPullRequests(context.Background(), scope, repo, 0)
			return err
		}},
	}
	for _, tc := range cases {
		if err := tc.call(); err == nil {
			t.Errorf("%s: err = nil, want a refusal", tc.name)
		}
	}
	if n := len(rec.recorded()); n != 0 {
		t.Errorf("requests = %d, want 0 (guards fire before any request)", n)
	}
}
