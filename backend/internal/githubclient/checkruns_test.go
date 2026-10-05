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

// checkRunsServer serves the check-runs listing from a per-page handler and
// records every request's path + raw query.
type checkRunsServer struct {
	mu       sync.Mutex
	requests []string
}

func (s *checkRunsServer) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func checkRunsClient(t *testing.T, page func(w http.ResponseWriter, page int)) (*Client, *checkRunsServer) {
	t.Helper()
	rec := &checkRunsServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.requests = append(rec.requests, r.URL.Path+"?"+r.URL.RawQuery)
		rec.mu.Unlock()
		n, _ := strconv.Atoi(r.URL.Query().Get("page"))
		page(w, n)
	}))
	t.Cleanup(srv.Close)
	return &Client{
		BaseURL: srv.URL,
		Tokens:  &stubTokens{token: "ghs_canned_token"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}, rec
}

// checkRunsPage renders n check runs named prefix-<i> under total_count.
func checkRunsPage(total, n int, prefix string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"total_count":%d,"check_runs":[`, total)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":%d,"name":"%s-%d","status":"completed","conclusion":"success","check_suite":{"id":1},"app":{"slug":"github-actions"}}`, i+1, prefix, i)
	}
	b.WriteString("]}")
	return b.String()
}

func listCheckRuns(c *Client, ref string) ([]CheckRunSummary, bool, error) {
	return c.ListCheckRunsForRef(context.Background(), forge.FromGitHubInstallationID(42), RepoRef{Owner: "x", Name: "y"}, ref)
}

func TestListCheckRunsForRef_QueryAndDecode(t *testing.T) {
	c, rec := checkRunsClient(t, func(w http.ResponseWriter, _ int) {
		_, _ = w.Write([]byte(`{"total_count":2,"check_runs":[
			{"id":7,"name":"CI Pass","status":"completed","conclusion":"failure",
			 "started_at":"2026-09-01T12:00:00Z","completed_at":"2026-09-01T12:05:00Z",
			 "check_suite":{"id":99},"app":{"slug":"github-actions"},"output":{"title":"ignored"}},
			{"id":8,"name":"lint","status":"in_progress","conclusion":null,"started_at":"2026-09-01T12:01:00Z","completed_at":null,
			 "check_suite":{"id":99},"app":{"slug":"github-actions"}}
		]}`))
	})
	runs, truncated, err := listCheckRuns(c, "abc123")
	if err != nil || truncated {
		t.Fatalf("err = %v truncated = %v, want nil/false", err, truncated)
	}
	reqs := rec.recorded()
	if len(reqs) != 1 {
		t.Fatalf("requests = %v, want 1", reqs)
	}
	path, query, _ := strings.Cut(reqs[0], "?")
	if path != "/repos/x/y/commits/abc123/check-runs" {
		t.Errorf("path = %q", path)
	}
	for _, want := range []string{"filter=all", "per_page=100", "page=1"} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q missing %q", query, want)
		}
	}
	if strings.Contains(query, "filter=latest") {
		t.Errorf("query %q must list every attempt, not filter=latest", query)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %+v, want 2", runs)
	}
	r0 := runs[0]
	if r0.ID != 7 || r0.Name != "CI Pass" || r0.Status != "completed" || r0.Conclusion != "failure" ||
		r0.CheckSuiteID != 99 || r0.AppSlug != "github-actions" || r0.StartedAt == nil || r0.CompletedAt == nil ||
		!r0.CompletedAt.Equal(time.Date(2026, 9, 1, 12, 5, 0, 0, time.UTC)) {
		t.Errorf("run[0] = %+v", r0)
	}
	if runs[1].Conclusion != "" || runs[1].CompletedAt != nil || runs[1].Status != "in_progress" {
		t.Errorf("run[1] = %+v, want empty conclusion and nil completed_at", runs[1])
	}
}

func TestListCheckRunsForRef_PaginatesToTotalCount(t *testing.T) {
	c, rec := checkRunsClient(t, func(w http.ResponseWriter, page int) {
		switch page {
		case 1:
			_, _ = w.Write([]byte(checkRunsPage(130, 100, "a")))
		case 2:
			_, _ = w.Write([]byte(checkRunsPage(130, 30, "b")))
		default:
			t.Errorf("unexpected page %d", page)
			_, _ = w.Write([]byte(checkRunsPage(130, 0, "z")))
		}
	})
	runs, truncated, err := listCheckRuns(c, "abc")
	if err != nil || truncated || len(runs) != 130 {
		t.Fatalf("len = %d truncated = %v err = %v, want 130/false/nil", len(runs), truncated, err)
	}
	if n := len(rec.recorded()); n != 2 {
		t.Fatalf("requests = %d, want 2", n)
	}
}

func TestListCheckRunsForRef_PageCapTruncates(t *testing.T) {
	c, rec := checkRunsClient(t, func(w http.ResponseWriter, _ int) {
		_, _ = w.Write([]byte(checkRunsPage(1000, 100, "p")))
	})
	runs, truncated, err := listCheckRuns(c, "abc")
	if err != nil || !truncated || len(runs) != 500 {
		t.Fatalf("len = %d truncated = %v err = %v, want 500/true/nil", len(runs), truncated, err)
	}
	if n := len(rec.recorded()); n != checkRunsPageCap {
		t.Fatalf("requests = %d, want %d", n, checkRunsPageCap)
	}
}

func TestListCheckRunsForRef_ShortPageBeforeTotalTruncates(t *testing.T) {
	c, _ := checkRunsClient(t, func(w http.ResponseWriter, _ int) {
		_, _ = w.Write([]byte(checkRunsPage(50, 10, "s")))
	})
	runs, truncated, err := listCheckRuns(c, "abc")
	if err != nil || !truncated || len(runs) != 10 {
		t.Fatalf("len = %d truncated = %v err = %v, want 10/true/nil", len(runs), truncated, err)
	}
}

func TestListCheckRunsForRef_EmptyListing(t *testing.T) {
	c, _ := checkRunsClient(t, func(w http.ResponseWriter, _ int) {
		_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
	})
	runs, truncated, err := listCheckRuns(c, "abc")
	if err != nil || truncated || len(runs) != 0 {
		t.Fatalf("runs = %v truncated = %v err = %v, want empty/false/nil", runs, truncated, err)
	}
}

func TestListCheckRunsForRef_StatusClassification(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusNotFound, ErrNotFound},
		{http.StatusForbidden, ErrForbidden},
		{http.StatusUnprocessableEntity, ErrValidation},
	} {
		c, _ := checkRunsClient(t, func(w http.ResponseWriter, _ int) { w.WriteHeader(tc.status) })
		if _, _, err := listCheckRuns(c, "abc"); !errors.Is(err, tc.want) {
			t.Errorf("status %d: err = %v, want %v", tc.status, err, tc.want)
		}
	}
	c, _ := checkRunsClient(t, func(w http.ResponseWriter, _ int) { w.WriteHeader(http.StatusBadGateway) })
	_, _, err := listCheckRuns(c, "abc")
	if err == nil || !strings.Contains(err.Error(), "githubclient: list check runs: 502: ") {
		t.Errorf("502: err = %v, want the untyped status error", err)
	}
}

func TestListCheckRunsForRef_DecodeError(t *testing.T) {
	c, _ := checkRunsClient(t, func(w http.ResponseWriter, _ int) { _, _ = w.Write([]byte(`{not json`)) })
	if _, _, err := listCheckRuns(c, "abc"); err == nil || !strings.Contains(err.Error(), "decode check runs") {
		t.Fatalf("err = %v, want a decode error", err)
	}
}

func TestListCheckRunsForRef_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()
	c := &Client{BaseURL: srv.URL, Tokens: &stubTokens{token: "t"}, HTTP: &http.Client{Timeout: time.Second}}
	if _, _, err := listCheckRuns(c, "abc"); err == nil || !strings.Contains(err.Error(), "list check runs") {
		t.Fatalf("err = %v, want a transport error", err)
	}
}

func TestListCheckRunsForRef_RefusesBadInput(t *testing.T) {
	c, rec := checkRunsClient(t, func(w http.ResponseWriter, _ int) { w.WriteHeader(http.StatusOK) })
	ctx := context.Background()
	scope := forge.FromGitHubInstallationID(42)
	cases := map[string]func() error{
		"empty ref": func() error {
			_, _, err := c.ListCheckRunsForRef(ctx, scope, RepoRef{Owner: "x", Name: "y"}, "")
			return err
		},
		"empty repo": func() error {
			_, _, err := c.ListCheckRunsForRef(ctx, scope, RepoRef{Owner: "x"}, "abc")
			return err
		},
		"zero scope": func() error {
			_, _, err := c.ListCheckRunsForRef(ctx, forge.CredentialScope{}, RepoRef{Owner: "x", Name: "y"}, "abc")
			return err
		},
		"no tokens": func() error {
			_, _, err := (&Client{BaseURL: "http://unused"}).ListCheckRunsForRef(ctx, scope, RepoRef{Owner: "x", Name: "y"}, "abc")
			return err
		},
	}
	for name, call := range cases {
		if err := call(); err == nil {
			t.Errorf("%s: err = nil, want a refusal", name)
		}
	}
	if n := len(rec.recorded()); n != 0 {
		t.Fatalf("requests = %d, want 0 (every refusal precedes the call)", n)
	}
}
