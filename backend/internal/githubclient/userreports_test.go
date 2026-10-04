package githubclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// activityNode is one node the fake activity listing holds: its updated_at
// (the sort and since key) plus the rest of its JSON fields.
type activityNode struct {
	id      int
	updated time.Time
	fields  map[string]any
}

// fakeActivity is an httptest GitHub serving ONE activity listing with
// GitHub's documented semantics: since is inclusive, sort=updated with
// direction=asc orders oldest-first, page/per_page are offsets into the
// filtered list, and Link rel="next" is advertised while nodes remain.
// beforeRequest runs before the n-th request (1-based) is answered so a
// test can mutate the node set BETWEEN page requests.
type fakeActivity struct {
	t             *testing.T
	path          string
	mu            sync.Mutex
	nodes         []activityNode
	queries       []url.Values
	beforeRequest func(n int)
	date          string // "" suppresses the Date header entirely
	status        int    // non-zero answers every request with this status
	maxRequests   int    // > 0: a request past it fails the test and answers 500
	generate      func(since time.Time, offset, limit int) ([]activityNode, bool)
}

const fixedForgeDate = "Sun, 04 Oct 2026 12:00:00 GMT"

func newFakeActivity(t *testing.T, path string) (*fakeActivity, *httptest.Server) {
	t.Helper()
	fa := &fakeActivity{t: t, path: path, date: fixedForgeDate}
	srv := httptest.NewServer(http.HandlerFunc(fa.serve))
	t.Cleanup(srv.Close)
	return fa, srv
}

func (fa *fakeActivity) serve(w http.ResponseWriter, r *http.Request) {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	if r.URL.Path != fa.path {
		fa.t.Errorf("unexpected path %q, want %q", r.URL.Path, fa.path)
		http.Error(w, "unexpected path", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	fa.queries = append(fa.queries, q)
	n := len(fa.queries)
	if fa.maxRequests > 0 && n > fa.maxRequests {
		fa.t.Errorf("request %d exceeds the expected %d", n, fa.maxRequests)
		http.Error(w, "too many requests", http.StatusInternalServerError)
		return
	}
	if fa.beforeRequest != nil {
		fa.beforeRequest(n)
	}
	if fa.date == "" {
		w.Header()["Date"] = nil
	} else {
		w.Header().Set("Date", fa.date)
	}
	if fa.status != 0 {
		http.Error(w, "canned status", fa.status)
		return
	}
	var since time.Time
	if s := q.Get("since"); s != "" {
		var err error
		if since, err = time.Parse(time.RFC3339Nano, s); err != nil {
			fa.t.Errorf("since %q: %v", s, err)
		}
	}
	perPage, _ := strconv.Atoi(q.Get("per_page"))
	page := 1
	if p := q.Get("page"); p != "" {
		page, _ = strconv.Atoi(p)
	}
	offset := (page - 1) * perPage

	var window []activityNode
	var more bool
	if fa.generate != nil {
		window, more = fa.generate(since, offset, perPage)
	} else {
		var filtered []activityNode
		for _, nd := range fa.nodes {
			if !nd.updated.Before(since) {
				filtered = append(filtered, nd)
			}
		}
		sort.SliceStable(filtered, func(i, j int) bool {
			if !filtered[i].updated.Equal(filtered[j].updated) {
				return filtered[i].updated.Before(filtered[j].updated)
			}
			return filtered[i].id < filtered[j].id
		})
		if offset < len(filtered) {
			end := min(offset+perPage, len(filtered))
			window = filtered[offset:end]
			more = end < len(filtered)
		}
	}
	if more {
		w.Header().Set("Link", fmt.Sprintf(`<https://api.github.invalid%s?page=%d>; rel="next"`, fa.path, page+1))
	}
	body := make([]map[string]any, 0, len(window))
	for _, nd := range window {
		m := map[string]any{"updated_at": nd.updated.UTC().Format(time.RFC3339), "created_at": "2026-01-01T00:00:00Z"}
		for k, v := range nd.fields {
			m[k] = v
		}
		body = append(body, m)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (fa *fakeActivity) requests() []url.Values {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	return append([]url.Values(nil), fa.queries...)
}

// deleteID removes the node with id; the caller holds fa.mu (beforeRequest
// runs under it).
func (fa *fakeActivity) deleteID(id int) {
	for i, nd := range fa.nodes {
		if nd.id == id {
			fa.nodes = append(fa.nodes[:i], fa.nodes[i+1:]...)
			return
		}
	}
	fa.t.Fatalf("deleteID(%d): no such node", id)
}

var activityBaseTime = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// issueNodes returns count issue nodes numbered 1..count, each updated one
// second after the previous, starting one second after activityBaseTime.
func issueNodes(count int) []activityNode {
	out := make([]activityNode, 0, count)
	for i := 1; i <= count; i++ {
		out = append(out, activityNode{id: i, updated: activityBaseTime.Add(time.Duration(i) * time.Second), fields: map[string]any{
			"number": i, "title": fmt.Sprintf("issue %d", i), "user": map[string]any{"login": "u", "type": "User"},
		}})
	}
	return out
}

func activityClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, _ := newTestClient(t, srv, nil)
	return c
}

var activityRepo = RepoRef{Owner: "o", Name: "r"}

func activityScope() forge.CredentialScope { return forge.FromGitHubInstallationID(7) }

func distinctNumbers(issues []UpdatedIssue) map[int]int {
	out := map[int]int{}
	for _, is := range issues {
		out[is.Number]++
	}
	return out
}

// TestListIssuesUpdatedSince_WalksKeysetSkipsPullRequestsAndDecodesActivity
// pins the request shape (state=all, sort=updated, direction=asc,
// per_page=100, since on the first request; the bound MOVED to page 1's
// latest updated_at with no page offset on the second), PR exclusion, the
// full decode, and the first-page Date. Counterfactual: deleting the
// isPullRequestNode skip returns the PR node and reddens the PR assertion.
func TestListIssuesUpdatedSince_WalksKeysetSkipsPullRequestsAndDecodesActivity(t *testing.T) {
	fa, srv := newFakeActivity(t, "/repos/o/r/issues")
	fa.nodes = issueNodes(150)
	fa.nodes[0].fields = map[string]any{
		"number": 1, "title": "rich", "body": "the body", "state": "open",
		"labels":             []any{map[string]any{"name": "bug"}, "triage"},
		"html_url":           "https://github.com/o/r/issues/1",
		"user":               map[string]any{"login": "octocat", "type": "User"},
		"author_association": "OWNER",
		"reactions":          map[string]any{"total_count": 9, "+1": 1, "-1": 2, "laugh": 1, "hooray": 1, "confused": 1, "heart": 1, "rocket": 1, "eyes": 1},
		"created_at":         "2026-08-31T10:00:00Z",
	}
	fa.nodes = append(fa.nodes, activityNode{id: 999, updated: activityBaseTime.Add(10 * time.Second), fields: map[string]any{
		"number": 999, "title": "a pull request", "pull_request": map[string]any{"url": "https://api.github.com/repos/o/r/pulls/999"},
	}})
	since := activityBaseTime

	issues, meta, err := activityClient(t, srv).ListIssuesUpdatedSince(context.Background(), activityScope(), activityRepo, since)
	if err != nil {
		t.Fatalf("ListIssuesUpdatedSince: %v", err)
	}
	got := distinctNumbers(issues)
	if _, ok := got[999]; ok {
		t.Errorf("pull request #999 returned; nodes carrying pull_request must be skipped")
	}
	for i := 1; i <= 150; i++ {
		if got[i] == 0 {
			t.Errorf("issue #%d missing from the walk", i)
		}
	}
	wantDate, _ := http.ParseTime(fixedForgeDate)
	if !meta.Date.Equal(wantDate) || meta.Truncated || !meta.ResumeAt.IsZero() {
		t.Errorf("meta = %+v, want Date %v, not truncated", meta, wantDate)
	}

	reqs := fa.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	for k, want := range map[string]string{"state": "all", "sort": "updated", "direction": "asc", "per_page": "100", "since": "2026-09-01T00:00:00Z", "page": ""} {
		if g := reqs[0].Get(k); g != want {
			t.Errorf("first request %s = %q, want %q", k, g, want)
		}
	}
	// Page 1 holds 101 nodes' worth of the sorted list minus one: 100 nodes,
	// the PR (updated at +10s) among them, so its latest is issue #99 at +99s.
	if g := reqs[1].Get("since"); g != "2026-09-01T00:01:39Z" {
		t.Errorf("second request since = %q, want the bound moved to page 1's latest updated_at 2026-09-01T00:01:39Z", g)
	}
	if g := reqs[1].Get("page"); g != "" {
		t.Errorf("second request page = %q, want no offset under a moved bound", g)
	}

	var rich UpdatedIssue
	for _, is := range issues {
		if is.Number == 1 {
			rich = is
		}
	}
	want := UpdatedIssue{
		Number: 1, Title: "rich", Body: "the body", State: "open", Labels: []string{"bug", "triage"},
		HTMLURL: "https://github.com/o/r/issues/1", AuthorLogin: "octocat", AuthorType: "User",
		AuthorAssociation: "OWNER", AssociationPresent: true,
		Reactions:        ReactionRollup{TotalCount: 9, PlusOne: 1, MinusOne: 2, Laugh: 1, Hooray: 1, Confused: 1, Heart: 1, Rocket: 1, Eyes: 1},
		ReactionsPresent: true,
		CreatedAt:        time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC),
		UpdatedAt:        activityBaseTime.Add(time.Second),
	}
	if fmt.Sprintf("%+v", rich) != fmt.Sprintf("%+v", want) {
		t.Errorf("decoded issue =\n %+v\nwant\n %+v", rich, want)
	}
}

// TestListIssuesUpdatedSince_DeletionBetweenPagesSkipsNothing is the
// keyset-vs-offset counterfactual vehicle (approval condition 1): issue #1,
// already read on page 1, is deleted before page 2 is served. Under the
// keyset bound every surviving issue is still returned. Counterfactual: an
// offset walk (page 2 under the original since) skips the node the deletion
// shifted onto page 1's final offset (issue #101).
func TestListIssuesUpdatedSince_DeletionBetweenPagesSkipsNothing(t *testing.T) {
	fa, srv := newFakeActivity(t, "/repos/o/r/issues")
	fa.nodes = issueNodes(250)
	fa.beforeRequest = func(n int) {
		if n == 2 {
			fa.deleteID(1)
		}
	}
	issues, _, err := activityClient(t, srv).ListIssuesUpdatedSince(context.Background(), activityScope(), activityRepo, activityBaseTime)
	if err != nil {
		t.Fatalf("ListIssuesUpdatedSince: %v", err)
	}
	got := distinctNumbers(issues)
	for i := 1; i <= 250; i++ {
		if got[i] == 0 {
			t.Errorf("issue #%d skipped after a deletion between page requests", i)
		}
	}
}

// TestListIssuesUpdatedSince_EqualTimestampRunTakesOffsetPages pins the one
// case that pages by OFFSET: a full page sharing one updated_at. 150 issues
// share one timestamp; moving the bound to it would re-request the same
// page forever, so page 2 is requested under the SAME bound. Counterfactual:
// always moving the bound loops on page 1 until the cap and returns
// truncated with issues #101-#150 never read.
func TestListIssuesUpdatedSince_EqualTimestampRunTakesOffsetPages(t *testing.T) {
	fa, srv := newFakeActivity(t, "/repos/o/r/issues")
	fa.nodes = issueNodes(250)
	run := activityBaseTime.Add(time.Hour)
	for i := 0; i < 150; i++ {
		fa.nodes[i].updated = run
	}
	for i := 150; i < 250; i++ {
		fa.nodes[i].updated = run.Add(time.Duration(i) * time.Second)
	}
	issues, meta, err := activityClient(t, srv).ListIssuesUpdatedSince(context.Background(), activityScope(), activityRepo, activityBaseTime)
	if err != nil {
		t.Fatalf("ListIssuesUpdatedSince: %v", err)
	}
	if meta.Truncated {
		t.Fatalf("meta.Truncated = true, want the run walked by offset to completion")
	}
	got := distinctNumbers(issues)
	for i := 1; i <= 250; i++ {
		if got[i] == 0 {
			t.Errorf("issue #%d missing", i)
		}
	}
	reqs := fa.requests()
	if len(reqs) < 2 || reqs[1].Get("page") != "2" || reqs[1].Get("since") != reqs[0].Get("since") {
		t.Errorf("second request = %v, want page=2 under the unchanged since %q", reqs[1], reqs[0].Get("since"))
	}
}

// TestListIssuesUpdatedSince_PageCapTruncatesAtLastReadUpdatedAt pins the
// non-failing cap (approval condition 1): an endless distinct-timestamp
// listing stops at exactly 100 requests and returns what it read, Truncated,
// with ResumeAt = the last page's latest updated_at. Counterfactuals: a
// deleted cap keeps requesting (the fake fails request 101); a ResumeAt
// taken from the page's earliest node reddens the exact-value assertion.
func TestListIssuesUpdatedSince_PageCapTruncatesAtLastReadUpdatedAt(t *testing.T) {
	fa, srv := newFakeActivity(t, "/repos/o/r/issues")
	fa.maxRequests = userReportMaxPages
	fa.generate = func(since time.Time, offset, limit int) ([]activityNode, bool) {
		first := int(since.Sub(activityBaseTime)/time.Second) + offset
		out := make([]activityNode, 0, limit)
		for i := first; i < first+limit; i++ {
			out = append(out, activityNode{id: i, updated: activityBaseTime.Add(time.Duration(i) * time.Second), fields: map[string]any{"number": i + 1}})
		}
		return out, true
	}
	issues, meta, err := activityClient(t, srv).ListIssuesUpdatedSince(context.Background(), activityScope(), activityRepo, activityBaseTime)
	if err != nil {
		t.Fatalf("ListIssuesUpdatedSince: %v, want a truncated success", err)
	}
	if !meta.Truncated {
		t.Fatalf("meta.Truncated = false, want true at the page cap")
	}
	// Each page moves the bound to its latest node and re-reads it, so page
	// k (1-based) spans seconds 99(k-1) .. 99(k-1)+99; page 100 ends at 9900.
	if want := activityBaseTime.Add(9900 * time.Second); !meta.ResumeAt.Equal(want) {
		t.Errorf("meta.ResumeAt = %v, want the last read updated_at %v", meta.ResumeAt, want)
	}
	if len(issues) != userReportMaxPages*userReportPerPage {
		t.Errorf("len(issues) = %d, want %d (everything read is returned)", len(issues), userReportMaxPages*userReportPerPage)
	}
	if n := len(fa.requests()); n != userReportMaxPages {
		t.Errorf("requests = %d, want exactly the %d-page cap", n, userReportMaxPages)
	}
}

// TestListIssuesUpdatedSince_CapInsideEqualTimestampRun pins the single
// fail-closed cap case: a capped walk that never left one equal-timestamp
// run AT since has no resume point beyond since, so it returns the typed
// ErrEqualTimestampRunExceedsCap with no items — distinguishable from a
// transient failure. A run at a timestamp AFTER since still truncates with
// ResumeAt at the run (progress). Counterfactual: always truncating returns
// a nil error on the at-since row.
func TestListIssuesUpdatedSince_CapInsideEqualTimestampRun(t *testing.T) {
	for _, tc := range []struct {
		name    string
		runAt   time.Time
		wantErr bool
	}{
		{name: "run at since fails closed", runAt: activityBaseTime, wantErr: true},
		{name: "run after since truncates at the run", runAt: activityBaseTime.Add(time.Minute)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fa, srv := newFakeActivity(t, "/repos/o/r/issues")
			fa.maxRequests = userReportMaxPages
			fa.generate = func(_ time.Time, offset, limit int) ([]activityNode, bool) {
				out := make([]activityNode, 0, limit)
				for i := offset; i < offset+limit; i++ {
					out = append(out, activityNode{id: i, updated: tc.runAt, fields: map[string]any{"number": i + 1}})
				}
				return out, true
			}
			issues, meta, err := activityClient(t, srv).ListIssuesUpdatedSince(context.Background(), activityScope(), activityRepo, activityBaseTime)
			if tc.wantErr {
				if !errors.Is(err, ErrEqualTimestampRunExceedsCap) || issues != nil {
					t.Fatalf("got (%d issues, %v), want nil issues and ErrEqualTimestampRunExceedsCap", len(issues), err)
				}
				return
			}
			if err != nil || !meta.Truncated || !meta.ResumeAt.Equal(tc.runAt) {
				t.Fatalf("got (meta %+v, %v), want truncated at ResumeAt %v", meta, err, tc.runAt)
			}
		})
	}

	fa, srv := newFakeActivity(t, "/repos/o/r/issues")
	fa.status = http.StatusBadGateway
	_, _, err := activityClient(t, srv).ListIssuesUpdatedSince(context.Background(), activityScope(), activityRepo, activityBaseTime)
	if err == nil || errors.Is(err, ErrEqualTimestampRunExceedsCap) {
		t.Errorf("transient 502 err = %v, want a non-nil error that is NOT ErrEqualTimestampRunExceedsCap", err)
	}
}

// TestListIssuesUpdatedSince_ZeroSinceOmitsParam: a zero since sends no
// since parameter at all.
func TestListIssuesUpdatedSince_ZeroSinceOmitsParam(t *testing.T) {
	fa, srv := newFakeActivity(t, "/repos/o/r/issues")
	fa.nodes = issueNodes(2)
	if _, _, err := activityClient(t, srv).ListIssuesUpdatedSince(context.Background(), activityScope(), activityRepo, time.Time{}); err != nil {
		t.Fatalf("ListIssuesUpdatedSince: %v", err)
	}
	if q := fa.requests()[0]; q.Has("since") {
		t.Errorf("query %v carries since, want it omitted for a zero bound", q)
	}
}

// TestListIssuesUpdatedSince_AbsentReactionsAndAssociationAreUnresolved
// pins the three-state presence decode: an ABSENT key and a JSON null both
// read not-present, while a zero-valued rollup and an explicit NONE read
// present. Counterfactual: setting the presence flags unconditionally
// reddens the absent and null rows.
func TestListIssuesUpdatedSince_AbsentReactionsAndAssociationAreUnresolved(t *testing.T) {
	fa, srv := newFakeActivity(t, "/repos/o/r/issues")
	fa.nodes = issueNodes(3)
	fa.nodes[1].fields = map[string]any{"number": 2, "author_association": nil, "reactions": nil}
	fa.nodes[2].fields = map[string]any{"number": 3, "author_association": "NONE", "reactions": map[string]any{"total_count": 0}}
	issues, _, err := activityClient(t, srv).ListIssuesUpdatedSince(context.Background(), activityScope(), activityRepo, activityBaseTime)
	if err != nil {
		t.Fatalf("ListIssuesUpdatedSince: %v", err)
	}
	want := map[int]bool{1: false, 2: false, 3: true}
	for _, is := range issues {
		if is.AssociationPresent != want[is.Number] || is.ReactionsPresent != want[is.Number] {
			t.Errorf("issue #%d presence (association %v, reactions %v), want %v for both", is.Number, is.AssociationPresent, is.ReactionsPresent, want[is.Number])
		}
	}
}

// TestListIssuesUpdatedSince_MissingDateIsZero: no Date header yields a
// zero ListingMeta.Date, never a host-clock fallback.
func TestListIssuesUpdatedSince_MissingDateIsZero(t *testing.T) {
	fa, srv := newFakeActivity(t, "/repos/o/r/issues")
	fa.nodes = issueNodes(1)
	fa.date = ""
	_, meta, err := activityClient(t, srv).ListIssuesUpdatedSince(context.Background(), activityScope(), activityRepo, activityBaseTime)
	if err != nil {
		t.Fatalf("ListIssuesUpdatedSince: %v", err)
	}
	if !meta.Date.IsZero() {
		t.Errorf("meta.Date = %v, want zero when the Date header is absent", meta.Date)
	}
}

// TestListIssuesUpdatedSince_StatusErrorsClassify: 403 is ErrForbidden and
// 404 is ErrNotFound, with no partial result.
func TestListIssuesUpdatedSince_StatusErrorsClassify(t *testing.T) {
	for status, want := range map[int]error{http.StatusForbidden: ErrForbidden, http.StatusNotFound: ErrNotFound} {
		fa, srv := newFakeActivity(t, "/repos/o/r/issues")
		fa.status = status
		issues, _, err := activityClient(t, srv).ListIssuesUpdatedSince(context.Background(), activityScope(), activityRepo, activityBaseTime)
		if !errors.Is(err, want) || issues != nil {
			t.Errorf("status %d: got (%v, %v), want nil and %v", status, issues, err, want)
		}
	}
}

// TestListIssuesUpdatedSince_RejectsMalformedNodesAndBadArgs covers the
// remaining refusals: an unparseable updated_at or created_at (the keyset
// walk cannot order such a node), a non-JSON body, a transport failure, and
// the argument preflight.
func TestListIssuesUpdatedSince_RejectsMalformedNodesAndBadArgs(t *testing.T) {
	for name, fields := range map[string]map[string]any{
		"updated_at": {"number": 1, "updated_at": "yesterday"},
		"created_at": {"number": 1, "created_at": "yesterday"},
	} {
		fa, srv := newFakeActivity(t, "/repos/o/r/issues")
		fa.nodes = issueNodes(1)
		fa.nodes[0].fields = fields
		if _, _, err := activityClient(t, srv).ListIssuesUpdatedSince(context.Background(), activityScope(), activityRepo, activityBaseTime); err == nil {
			t.Errorf("malformed %s: err = nil, want a decode error", name)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("not json")) }))
	t.Cleanup(srv.Close)
	if _, _, err := activityClient(t, srv).ListIssuesUpdatedSince(context.Background(), activityScope(), activityRepo, activityBaseTime); err == nil {
		t.Error("non-JSON body: err = nil, want a decode error")
	}

	_, closed := newFakeActivity(t, "/repos/o/r/issues")
	c := activityClient(t, closed)
	closed.Close()
	if _, _, err := c.ListIssueCommentsUpdatedSince(context.Background(), activityScope(), activityRepo, activityBaseTime); err == nil {
		t.Error("closed server: err = nil, want a transport error")
	}

	c = &Client{Tokens: &stubTokens{}}
	if _, _, err := c.ListIssuesUpdatedSince(context.Background(), forge.CredentialScope{}, activityRepo, activityBaseTime); err == nil {
		t.Error("zero scope: err = nil, want a refusal")
	}
	if _, _, err := c.ListIssueCommentsUpdatedSince(context.Background(), activityScope(), RepoRef{Owner: "o"}, activityBaseTime); err == nil {
		t.Error("missing repo name: err = nil, want a refusal")
	}
	if _, _, err := (&Client{}).ListIssuesUpdatedSince(context.Background(), activityScope(), activityRepo, activityBaseTime); err == nil {
		t.Error("nil TokenProvider: err = nil, want a refusal")
	}
}

// TestListIssueCommentsUpdatedSince_DerivesIssueNumberAndMarksPullRequestComments
// pins the comment listing's path and query (no state parameter), the
// issue number taken from issue_url, and the /pull/ html_url detector.
// Counterfactual: an isPullRequestURL body returning false reddens the PR
// comment row.
func TestListIssueCommentsUpdatedSince_DerivesIssueNumberAndMarksPullRequestComments(t *testing.T) {
	fa, srv := newFakeActivity(t, "/repos/o/r/issues/comments")
	fa.nodes = []activityNode{
		{id: 1, updated: activityBaseTime.Add(time.Second), fields: map[string]any{
			"id": 11, "issue_url": "https://api.github.com/repos/o/r/issues/5", "html_url": "https://github.com/o/r/issues/5#issuecomment-11",
			"body": "on an issue", "user": map[string]any{"login": "dependabot[bot]", "type": "Bot"}, "author_association": "NONE",
			"reactions": map[string]any{"total_count": 1, "heart": 1},
		}},
		{id: 2, updated: activityBaseTime.Add(2 * time.Second), fields: map[string]any{
			"id": 12, "issue_url": "https://api.github.com/repos/o/r/issues/6", "html_url": "https://github.com/o/r/pull/6#issuecomment-12",
		}},
		{id: 3, updated: activityBaseTime.Add(3 * time.Second), fields: map[string]any{
			"id": 13, "issue_url": "https://api.github.com/repos/o/r/issues/not-a-number", "html_url": "%zz",
		}},
	}
	comments, meta, err := activityClient(t, srv).ListIssueCommentsUpdatedSince(context.Background(), activityScope(), activityRepo, activityBaseTime)
	if err != nil {
		t.Fatalf("ListIssueCommentsUpdatedSince: %v", err)
	}
	if len(comments) != 3 || meta.Date.IsZero() {
		t.Fatalf("got %d comments, meta %+v; want 3 and a parsed Date", len(comments), meta)
	}
	first := comments[0]
	if first.ID != 11 || first.IssueNumber != 5 || first.OnPullRequest || first.Body != "on an issue" ||
		first.AuthorLogin != "dependabot[bot]" || first.AuthorType != "Bot" || first.AuthorAssociation != "NONE" ||
		!first.AssociationPresent || !first.ReactionsPresent || first.Reactions.Heart != 1 {
		t.Errorf("comments[0] = %+v, want the issue comment decoded", first)
	}
	if !comments[1].OnPullRequest || comments[1].IssueNumber != 6 {
		t.Errorf("comments[1] = %+v, want OnPullRequest for a /pull/ html_url and issue 6", comments[1])
	}
	if comments[2].OnPullRequest || comments[2].IssueNumber != 0 {
		t.Errorf("comments[2] = %+v, want issue 0 and not-PR for unparseable URLs", comments[2])
	}
	q := fa.requests()[0]
	if q.Has("state") || q.Get("sort") != "updated" || q.Get("direction") != "asc" || q.Get("since") != "2026-09-01T00:00:00Z" {
		t.Errorf("comment query = %v, want sort=updated direction=asc since and no state", q)
	}
}

// TestLastPathInt pins the issue_url number parser's refusals.
func TestLastPathInt(t *testing.T) {
	for in, want := range map[string]int{
		"https://api.github.com/repos/o/r/issues/42": 42,
		"https://api.github.com/repos/o/r/issues/0":  0,
		"https://api.github.com/repos/o/r/issues/-3": 0,
		"https://api.github.com/repos/o/r/issues/":   0,
		"%zz": 0,
	} {
		if got := lastPathInt(in); got != want {
			t.Errorf("lastPathInt(%q) = %d, want %d", in, got, want)
		}
	}
}
