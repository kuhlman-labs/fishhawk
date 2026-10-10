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

// pullReviewsServer serves the reviews listing from a per-page handler and
// records every request's path + raw query.
type pullReviewsServer struct {
	mu       sync.Mutex
	requests []string
}

func (s *pullReviewsServer) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func pullReviewsClient(t *testing.T, page func(w http.ResponseWriter, r *http.Request, page int)) (*Client, *pullReviewsServer, *stubTokens) {
	t.Helper()
	rec := &pullReviewsServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.requests = append(rec.requests, r.URL.Path+"?"+r.URL.RawQuery)
		rec.mu.Unlock()
		n, _ := strconv.Atoi(r.URL.Query().Get("page"))
		page(w, r, n)
	}))
	t.Cleanup(srv.Close)
	tokens := &stubTokens{token: "ghs_canned_token"}
	return &Client{
		BaseURL: srv.URL,
		Tokens:  tokens,
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}, rec, tokens
}

// pullReviewsPage renders n APPROVED reviews with ids from first.
func pullReviewsPage(first, n int) string {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		id := first + i
		fmt.Fprintf(&b, `{"id":%d,"user":{"login":"r%d"},"state":"APPROVED","commit_id":"c%d","submitted_at":"2026-10-01T12:00:00Z"}`, id, id, id)
	}
	b.WriteString("]")
	return b.String()
}

// reviewsNextLink sets a Link header advertising a next page.
func reviewsNextLink(w http.ResponseWriter, r *http.Request, page int) {
	w.Header().Set("Link", fmt.Sprintf(`<http://%s/repos/x/y/pulls/12/reviews?per_page=100&page=%d>; rel="next"`, r.Host, page+1))
}

func listPullReviews(c *Client) ([]PullRequestReview, bool, error) {
	return c.ListPullRequestReviews(context.Background(), forge.FromGitHubInstallationID(42), RepoRef{Owner: "x", Name: "y"}, 12)
}

// TestListPullRequestReviews_QueryDecodeAndPaging: two pages joined by a Link
// rel="next" are read in full and in order, every field maps, a null user
// reads "" and a null/absent submitted_at reads the zero time.
func TestListPullRequestReviews_QueryDecodeAndPaging(t *testing.T) {
	c, rec, tokens := pullReviewsClient(t, func(w http.ResponseWriter, r *http.Request, page int) {
		switch page {
		case 1:
			reviewsNextLink(w, r, page)
			_, _ = w.Write([]byte(`[
				{"id":80,"node_id":"PRR_1","user":{"login":"octocat","id":1},"body":"lgtm","state":"DISMISSED",
				 "html_url":"https://github.com/x/y/pull/12#pullrequestreview-80","commit_id":"aaa111",
				 "submitted_at":"2026-10-01T12:00:00Z","author_association":"OWNER"},
				{"id":81,"user":{"login":"hubot"},"state":"COMMENTED","commit_id":"bbb222","submitted_at":"2026-10-02T08:30:00Z"}]`))
		case 2:
			_, _ = w.Write([]byte(`[
				{"id":9000000000,"user":null,"state":"APPROVED","commit_id":"ccc333","submitted_at":"2026-10-03T00:00:00Z"},
				{"id":83,"user":{"login":"octocat"},"state":"PENDING","commit_id":"ddd444","submitted_at":null},
				{"id":84,"user":{"login":"monalisa"},"state":"CHANGES_REQUESTED","commit_id":"eee555"}]`))
		default:
			t.Errorf("unexpected page %d", page)
			_, _ = w.Write([]byte(`[]`))
		}
	})
	reviews, truncated, err := listPullReviews(c)
	if err != nil || truncated {
		t.Fatalf("err = %v truncated = %v, want nil/false", err, truncated)
	}
	if tokens.installationCalled != 42 {
		t.Errorf("token installation = %d, want 42", tokens.installationCalled)
	}
	reqs := rec.recorded()
	if len(reqs) != 2 {
		t.Fatalf("requests = %v, want 2", reqs)
	}
	for i, r := range reqs {
		path, query, _ := strings.Cut(r, "?")
		if path != "/repos/x/y/pulls/12/reviews" {
			t.Errorf("request %d path = %q", i, path)
		}
		for _, want := range []string{"per_page=100", "page=" + strconv.Itoa(i+1)} {
			if !strings.Contains(query, want) {
				t.Errorf("request %d query %q missing %q", i, query, want)
			}
		}
	}
	at := func(s string) time.Time {
		ts, perr := time.Parse(time.RFC3339, s)
		if perr != nil {
			t.Fatalf("parse %q: %v", s, perr)
		}
		return ts
	}
	want := []PullRequestReview{
		{ID: 80, UserLogin: "octocat", State: "DISMISSED", CommitID: "aaa111", SubmittedAt: at("2026-10-01T12:00:00Z")},
		{ID: 81, UserLogin: "hubot", State: "COMMENTED", CommitID: "bbb222", SubmittedAt: at("2026-10-02T08:30:00Z")},
		// A null user leaves UserLogin "", and an id past int32 decodes.
		{ID: 9000000000, State: "APPROVED", CommitID: "ccc333", SubmittedAt: at("2026-10-03T00:00:00Z")},
		// A null submitted_at (PENDING) and an absent one read the zero time.
		{ID: 83, UserLogin: "octocat", State: "PENDING", CommitID: "ddd444"},
		{ID: 84, UserLogin: "monalisa", State: "CHANGES_REQUESTED", CommitID: "eee555"},
	}
	if len(reviews) != len(want) {
		t.Fatalf("reviews = %+v, want %+v", reviews, want)
	}
	for i := range want {
		got := reviews[i]
		if got.ID != want[i].ID || got.UserLogin != want[i].UserLogin || got.State != want[i].State ||
			got.CommitID != want[i].CommitID || !got.SubmittedAt.Equal(want[i].SubmittedAt) {
			t.Errorf("review[%d] = %+v, want %+v", i, got, want[i])
		}
	}
}

// TestListPullRequestReviews_EmptyListing: a pull request with no reviews
// returns a non-nil empty slice, complete.
func TestListPullRequestReviews_EmptyListing(t *testing.T) {
	c, rec, _ := pullReviewsClient(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		_, _ = w.Write([]byte(`[]`))
	})
	reviews, truncated, err := listPullReviews(c)
	if err != nil || truncated || reviews == nil || len(reviews) != 0 {
		t.Fatalf("got reviews=%v truncated=%v err=%v, want a non-nil empty complete listing", reviews, truncated, err)
	}
	if n := len(rec.recorded()); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}

// TestListPullRequestReviews_PageCapTruncates: every page is full and
// advertises a next one, so the walk stops at pullReviewsMaxPages with every
// review read so far and truncated=true, never requesting the page past the
// cap.
//
// Counterfactual: only the cap stops this walk — deleting the cap check walks
// past page pullReviewsMaxPages and the server's page bound fails the test.
func TestListPullRequestReviews_PageCapTruncates(t *testing.T) {
	c, rec, _ := pullReviewsClient(t, func(w http.ResponseWriter, r *http.Request, page int) {
		if page > pullReviewsMaxPages {
			t.Errorf("walk passed the cap: requested page %d", page)
			_, _ = w.Write([]byte(`[]`))
			return
		}
		reviewsNextLink(w, r, page)
		_, _ = w.Write([]byte(pullReviewsPage(page*10, 2)))
	})
	reviews, truncated, err := listPullReviews(c)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !truncated || len(reviews) != 2*pullReviewsMaxPages {
		t.Fatalf("got %d reviews truncated=%v, want %d truncated=true", len(reviews), truncated, 2*pullReviewsMaxPages)
	}
	if last := reviews[len(reviews)-1].ID; last != int64(pullReviewsMaxPages*10+1) {
		t.Errorf("last review id = %d, want %d (the cap page is kept)", last, pullReviewsMaxPages*10+1)
	}
	if n := len(rec.recorded()); n != pullReviewsMaxPages {
		t.Errorf("requests = %d, want %d", n, pullReviewsMaxPages)
	}
}

// TestListPullRequestReviews_LastPageAtCapIsComplete: a walk whose final page
// is exactly the cap page and advertises no next page is complete, not
// truncated.
//
// Counterfactual: reporting truncated whenever the cap page is reached (the
// cap check ahead of the hasNext check) turns this RED.
func TestListPullRequestReviews_LastPageAtCapIsComplete(t *testing.T) {
	c, rec, _ := pullReviewsClient(t, func(w http.ResponseWriter, r *http.Request, page int) {
		if page < pullReviewsMaxPages {
			reviewsNextLink(w, r, page)
		}
		_, _ = w.Write([]byte(pullReviewsPage(page*10, 1)))
	})
	reviews, truncated, err := listPullReviews(c)
	if err != nil || truncated || len(reviews) != pullReviewsMaxPages {
		t.Fatalf("got %d reviews truncated=%v err=%v, want %d complete", len(reviews), truncated, err, pullReviewsMaxPages)
	}
	if n := len(rec.recorded()); n != pullReviewsMaxPages {
		t.Errorf("requests = %d, want %d", n, pullReviewsMaxPages)
	}
}

// TestListPullRequestReviews_EmptyPageWithNextStops: an empty page that still
// advertises a next page ends the walk as truncated after one request.
//
// Counterfactual: deleting the empty-page stop walks on to the page cap — the
// result is still truncated, so the request count is the assertion that
// isolates the control.
func TestListPullRequestReviews_EmptyPageWithNextStops(t *testing.T) {
	c, rec, _ := pullReviewsClient(t, func(w http.ResponseWriter, r *http.Request, page int) {
		reviewsNextLink(w, r, page)
		_, _ = w.Write([]byte(`[]`))
	})
	reviews, truncated, err := listPullReviews(c)
	if err != nil || len(reviews) != 0 || !truncated {
		t.Fatalf("got %d reviews truncated=%v err=%v, want 0 truncated=true", len(reviews), truncated, err)
	}
	if reviews == nil {
		t.Error("reviews = nil, want a non-nil empty slice")
	}
	if n := len(rec.recorded()); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}

// TestListPullRequestReviews_LinkHostIsNotFollowed: a next link naming another
// host is read only as "a next page exists"; the next request goes to the
// client's own base.
func TestListPullRequestReviews_LinkHostIsNotFollowed(t *testing.T) {
	c, rec, _ := pullReviewsClient(t, func(w http.ResponseWriter, _ *http.Request, page int) {
		if page == 1 {
			w.Header().Set("Link", `<https://attacker.invalid/steal?page=2>; rel="next"`)
		}
		_, _ = w.Write([]byte(pullReviewsPage(page*10, 1)))
	})
	reviews, truncated, err := listPullReviews(c)
	if err != nil || truncated || len(reviews) != 2 {
		t.Fatalf("got %d reviews truncated=%v err=%v, want 2 complete", len(reviews), truncated, err)
	}
	reqs := rec.recorded()
	if len(reqs) != 2 || !strings.HasPrefix(reqs[1], "/repos/x/y/pulls/12/reviews?") {
		t.Errorf("requests = %v, want page 2 on the client's own base", reqs)
	}
}

// TestListPullRequestReviews_ErrorMapping: non-2xx statuses classify through
// classifyStatus, an undecodable body fails, and an error on a later page
// discards the pages already read.
func TestListPullRequestReviews_ErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
		text   string
	}{
		{"403", http.StatusForbidden, `{"message":"Resource not accessible by integration"}`, ErrForbidden, ""},
		{"401", http.StatusUnauthorized, `{"message":"Bad credentials"}`, ErrForbidden, ""},
		{"404", http.StatusNotFound, `{"message":"Not Found"}`, ErrNotFound, ""},
		{"422", http.StatusUnprocessableEntity, `{"message":"Validation Failed"}`, ErrValidation, ""},
		{"500", http.StatusInternalServerError, `boom`, nil, "list pr reviews: 500"},
		{"undecodable", http.StatusOK, `{"not":"an array"}`, nil, "decode pr reviews"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _ := pullReviewsClient(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			// A bare http.Client: the retry transport is New()'s, so a 500
			// returns on the first attempt.
			reviews, truncated, err := listPullReviews(c)
			if err == nil || reviews != nil || truncated {
				t.Fatalf("got reviews=%v truncated=%v err=%v, want an error", reviews, truncated, err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want errors.Is %v", err, tc.want)
			}
			if tc.text != "" && !strings.Contains(err.Error(), tc.text) {
				t.Errorf("err = %v, want it to contain %q", err, tc.text)
			}
		})
	}

	t.Run("later page fails", func(t *testing.T) {
		c, _, _ := pullReviewsClient(t, func(w http.ResponseWriter, r *http.Request, page int) {
			if page == 1 {
				reviewsNextLink(w, r, page)
				_, _ = w.Write([]byte(pullReviewsPage(10, 2)))
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		})
		reviews, truncated, err := listPullReviews(c)
		if !errors.Is(err, ErrNotFound) || reviews != nil || truncated {
			t.Fatalf("got reviews=%v truncated=%v err=%v, want nil/false/ErrNotFound", reviews, truncated, err)
		}
	})

	t.Run("transport error", func(t *testing.T) {
		c, _, _ := pullReviewsClient(t, func(_ http.ResponseWriter, _ *http.Request, _ int) {})
		c.BaseURL = "http://127.0.0.1:1"
		if _, _, err := listPullReviews(c); err == nil || !strings.Contains(err.Error(), "list pr reviews") {
			t.Fatalf("err = %v, want a list pr reviews transport error", err)
		}
	})

	t.Run("token error", func(t *testing.T) {
		c, rec, tokens := pullReviewsClient(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
			_, _ = w.Write([]byte(`[]`))
		})
		tokens.err = errors.New("token mint failed")
		if _, _, err := listPullReviews(c); err == nil {
			t.Fatal("err = nil, want the token error")
		}
		if n := len(rec.recorded()); n != 0 {
			t.Errorf("requests = %d, want 0", n)
		}
	})
}

// TestListPullRequestReviews_RefusesBadInput: argument guards fire before any
// request.
func TestListPullRequestReviews_RefusesBadInput(t *testing.T) {
	c, rec, _ := pullReviewsClient(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		_, _ = w.Write([]byte(`[]`))
	})
	scope := forge.FromGitHubInstallationID(42)
	repo := RepoRef{Owner: "x", Name: "y"}
	cases := []struct {
		name string
		call func() error
	}{
		{"zero scope", func() error {
			_, _, err := c.ListPullRequestReviews(context.Background(), forge.CredentialScope{}, repo, 12)
			return err
		}},
		{"no token provider", func() error {
			_, _, err := (&Client{BaseURL: c.BaseURL, HTTP: c.HTTP}).ListPullRequestReviews(context.Background(), scope, repo, 12)
			return err
		}},
		{"blank owner", func() error {
			_, _, err := c.ListPullRequestReviews(context.Background(), scope, RepoRef{Name: "y"}, 12)
			return err
		}},
		{"blank name", func() error {
			_, _, err := c.ListPullRequestReviews(context.Background(), scope, RepoRef{Owner: "x"}, 12)
			return err
		}},
		{"pr number zero", func() error {
			_, _, err := c.ListPullRequestReviews(context.Background(), scope, repo, 0)
			return err
		}},
		{"pr number negative", func() error {
			_, _, err := c.ListPullRequestReviews(context.Background(), scope, repo, -3)
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
