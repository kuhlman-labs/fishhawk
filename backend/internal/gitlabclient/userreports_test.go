package gitlabclient

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
	"sync/atomic"
	"testing"
	"time"
)

// glIssueNode is one issue the fake listing holds: its updated_at (the sort
// and updated_after key) plus the rest of its JSON fields.
type glIssueNode struct {
	iid     int
	updated time.Time
	fields  map[string]any
}

// fakeIssueListing is an httptest GitLab serving the project issues
// listing with GitLab's documented semantics: updated_after is "on or
// after", order_by=updated_at&sort=asc orders oldest-first, page/per_page
// are offsets into the filtered list, and Link rel="next" is advertised
// while issues remain. beforeRequest runs before the n-th request (1-based)
// is answered so a test can mutate the issue set BETWEEN page requests.
type fakeIssueListing struct {
	t             *testing.T
	srv           *httptest.Server
	mu            sync.Mutex
	nodes         []glIssueNode
	queries       []url.Values
	tokens        []string
	beforeRequest func(n int)
	date          string // "" suppresses the Date header
	status        int
	maxRequests   int
	nextLinkHost  string // non-empty: the rel="next" Link names this origin
	generate      func(offset, limit int) []glIssueNode
}

const glForgeDate = "Sun, 04 Oct 2026 12:00:00 GMT"

func newFakeIssueListing(t *testing.T) *fakeIssueListing {
	t.Helper()
	f := &fakeIssueListing{t: t, date: glForgeDate}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIssueListing) client() *Client {
	return New(f.srv.URL, "glpat-test", WithHTTPClient(f.srv.Client()))
}

func (f *fakeIssueListing) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path != "/api/v4/projects/42/issues" {
		f.t.Errorf("unexpected path %q", r.URL.Path)
		http.Error(w, "unexpected path", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	f.queries = append(f.queries, q)
	f.tokens = append(f.tokens, r.Header.Get("PRIVATE-TOKEN"))
	n := len(f.queries)
	if f.maxRequests > 0 && n > f.maxRequests {
		f.t.Errorf("request %d exceeds the expected %d", n, f.maxRequests)
		http.Error(w, "too many requests", http.StatusInternalServerError)
		return
	}
	if f.beforeRequest != nil {
		f.beforeRequest(n)
	}
	if f.date == "" {
		w.Header()["Date"] = nil
	} else {
		w.Header().Set("Date", f.date)
	}
	if f.status != 0 {
		http.Error(w, "canned status", f.status)
		return
	}
	var after time.Time
	if s := q.Get("updated_after"); s != "" {
		var err error
		if after, err = time.Parse(time.RFC3339Nano, s); err != nil {
			f.t.Errorf("updated_after %q: %v", s, err)
		}
	}
	perPage, _ := strconv.Atoi(q.Get("per_page"))
	page := 1
	if p := q.Get("page"); p != "" {
		page, _ = strconv.Atoi(p)
	}
	offset := (page - 1) * perPage

	var window []glIssueNode
	more := true
	if f.generate != nil {
		window = f.generate(offset, perPage)
	} else {
		var filtered []glIssueNode
		for _, nd := range f.nodes {
			if !nd.updated.Before(after) {
				filtered = append(filtered, nd)
			}
		}
		sort.SliceStable(filtered, func(i, j int) bool {
			if !filtered[i].updated.Equal(filtered[j].updated) {
				return filtered[i].updated.Before(filtered[j].updated)
			}
			return filtered[i].iid < filtered[j].iid
		})
		more = false
		if offset < len(filtered) {
			end := min(offset+perPage, len(filtered))
			window = filtered[offset:end]
			more = end < len(filtered)
		}
	}
	if more {
		host := f.srv.URL
		if f.nextLinkHost != "" {
			host = f.nextLinkHost
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/api/v4/projects/42/issues?page=%d&per_page=%d>; rel="next"`, host, page+1, perPage))
	}
	body := make([]map[string]any, 0, len(window))
	for _, nd := range window {
		m := map[string]any{"iid": nd.iid, "updated_at": nd.updated.UTC().Format(time.RFC3339Nano), "created_at": "2026-01-01T00:00:00.000Z"}
		for k, v := range nd.fields {
			m[k] = v
		}
		body = append(body, m)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeIssueListing) requests() []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]url.Values(nil), f.queries...)
}

var glBase = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// glIssues returns count issues numbered 1..count, each updated 500ms after
// the previous — sub-second spacing, so a bound formatted at whole seconds
// would be observably wrong.
func glIssues(count int) []glIssueNode {
	out := make([]glIssueNode, 0, count)
	for i := 1; i <= count; i++ {
		out = append(out, glIssueNode{iid: i, updated: glBase.Add(time.Duration(i) * 500 * time.Millisecond)})
	}
	return out
}

func glDistinct(issues []UpdatedIssue) map[int]int {
	out := map[int]int{}
	for _, is := range issues {
		out[is.IID]++
	}
	return out
}

// TestListIssuesUpdatedAfter_WalksKeysetAndDecodes pins the request shape
// (scope=all, order_by=updated_at, sort=asc, per_page=100, updated_after on
// the first request; the bound MOVED to page 1's latest updated_at at full
// sub-second precision with no page offset on the second), the decode, the
// first-page Date, and PRIVATE-TOKEN on every page. Counterfactual: a bound
// formatted at whole seconds reddens the second-request assertion.
func TestListIssuesUpdatedAfter_WalksKeysetAndDecodes(t *testing.T) {
	f := newFakeIssueListing(t)
	f.nodes = glIssues(150)
	f.nodes[0].fields = map[string]any{
		"title": "rich", "description": "the body", "state": "opened", "labels": []string{"bug", "triage"},
		"web_url": "https://gitlab.example/g/p/-/issues/1", "author": map[string]any{"id": 77, "username": "alice"},
		"upvotes": 3, "downvotes": 1, "confidential": true, "created_at": "2026-08-31T10:00:00.250Z",
	}
	issues, meta, err := f.client().ListIssuesUpdatedAfter(context.Background(), 42, glBase)
	if err != nil {
		t.Fatalf("ListIssuesUpdatedAfter: %v", err)
	}
	got := glDistinct(issues)
	for i := 1; i <= 150; i++ {
		if got[i] == 0 {
			t.Errorf("issue #%d missing", i)
		}
	}
	wantDate, _ := http.ParseTime(glForgeDate)
	if !meta.Date.Equal(wantDate) || meta.Truncated {
		t.Errorf("meta = %+v, want Date %v and not truncated", meta, wantDate)
	}
	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	for k, want := range map[string]string{"scope": "all", "order_by": "updated_at", "sort": "asc", "per_page": "100", "updated_after": "2026-09-01T00:00:00Z", "page": ""} {
		if g := reqs[0].Get(k); g != want {
			t.Errorf("first request %s = %q, want %q", k, g, want)
		}
	}
	if g := reqs[1].Get("updated_after"); g != "2026-09-01T00:00:50Z" {
		t.Errorf("second request updated_after = %q, want page 1's latest (issue 100 at +50s)", g)
	}
	f.mu.Lock()
	for i, tok := range f.tokens {
		if tok != "glpat-test" {
			t.Errorf("request %d PRIVATE-TOKEN = %q, want glpat-test", i, tok)
		}
	}
	f.mu.Unlock()
	// A page boundary at a FRACTIONAL timestamp: 101 issues with issue 100
	// moved to +50.25s, so the moved bound must keep the sub-second part.
	frac := newFakeIssueListing(t)
	frac.nodes = glIssues(101)
	frac.nodes[99].updated = glBase.Add(50250 * time.Millisecond)
	if _, _, err := frac.client().ListIssuesUpdatedAfter(context.Background(), 42, glBase); err != nil {
		t.Fatalf("fractional walk: %v", err)
	}
	if g := frac.requests()[1].Get("updated_after"); g != "2026-09-01T00:00:50.25Z" {
		t.Errorf("fractional bound = %q, want 2026-09-01T00:00:50.25Z (sub-second precision kept)", g)
	}

	var rich UpdatedIssue
	for _, is := range issues {
		if is.IID == 1 {
			rich = is
		}
	}
	want := UpdatedIssue{
		IID: 1, Title: "rich", Description: "the body", State: "opened", Labels: []string{"bug", "triage"},
		WebURL: "https://gitlab.example/g/p/-/issues/1", AuthorID: 77, AuthorUsername: "alice",
		Upvotes: 3, Downvotes: 1, Confidential: true,
		CreatedAt: time.Date(2026, 8, 31, 10, 0, 0, 250e6, time.UTC), UpdatedAt: glBase.Add(500 * time.Millisecond),
	}
	if fmt.Sprintf("%+v", rich) != fmt.Sprintf("%+v", want) {
		t.Errorf("decoded issue =\n %+v\nwant\n %+v", rich, want)
	}
}

// TestListIssuesUpdatedAfter_DeletionBetweenPagesSkipsNothing is the
// keyset-vs-offset counterfactual vehicle (approval condition 1): issue #1,
// already read on page 1, is deleted before page 2 is served, and every
// surviving issue is still returned. Counterfactual: an offset walk under
// the original bound skips issue #101.
func TestListIssuesUpdatedAfter_DeletionBetweenPagesSkipsNothing(t *testing.T) {
	f := newFakeIssueListing(t)
	f.nodes = glIssues(250)
	f.beforeRequest = func(n int) {
		if n == 2 {
			f.nodes = f.nodes[1:]
		}
	}
	issues, _, err := f.client().ListIssuesUpdatedAfter(context.Background(), 42, glBase)
	if err != nil {
		t.Fatalf("ListIssuesUpdatedAfter: %v", err)
	}
	got := glDistinct(issues)
	for i := 1; i <= 250; i++ {
		if got[i] == 0 {
			t.Errorf("issue #%d skipped after a deletion between page requests", i)
		}
	}
}

// TestListIssuesUpdatedAfter_EqualTimestampRunTakesOffsetPages: a full page
// sharing one updated_at is followed by an OFFSET page under the SAME
// bound. Counterfactual: always moving the bound loops on page 1 to the cap.
func TestListIssuesUpdatedAfter_EqualTimestampRunTakesOffsetPages(t *testing.T) {
	f := newFakeIssueListing(t)
	f.nodes = glIssues(250)
	run := glBase.Add(time.Hour)
	for i := range f.nodes {
		if i < 150 {
			f.nodes[i].updated = run
		} else {
			f.nodes[i].updated = run.Add(time.Duration(i) * time.Second)
		}
	}
	issues, meta, err := f.client().ListIssuesUpdatedAfter(context.Background(), 42, glBase)
	if err != nil || meta.Truncated {
		t.Fatalf("got (meta %+v, %v), want an untruncated walk", meta, err)
	}
	got := glDistinct(issues)
	for i := 1; i <= 250; i++ {
		if got[i] == 0 {
			t.Errorf("issue #%d missing", i)
		}
	}
	reqs := f.requests()
	if reqs[1].Get("page") != "2" || reqs[1].Get("updated_after") != reqs[0].Get("updated_after") {
		t.Errorf("second request = %v, want page=2 under the unchanged bound", reqs[1])
	}
}

// TestListIssuesUpdatedAfter_PageCap pins the cap's three outcomes: an
// endless distinct-timestamp listing truncates at exactly maxListPages with
// ResumeAt = the last read updated_at; a capped walk inside one
// equal-timestamp run AT the bound fails closed with the typed
// ErrEqualTimestampRunExceedsCap and no issues; a run AFTER the bound
// truncates at the run. A transient 502 is NOT the typed error.
// Counterfactuals: a deleted cap trips the fake's request ceiling; a
// deleted at-bound guard returns a truncation for the at-bound row.
func TestListIssuesUpdatedAfter_PageCap(t *testing.T) {
	t.Run("distinct timestamps truncate", func(t *testing.T) {
		f := newFakeIssueListing(t)
		f.maxRequests = maxListPages
		// An effectively endless listing: 20 000 issues one second apart.
		f.nodes = make([]glIssueNode, 0, 20000)
		for i := 1; i <= 20000; i++ {
			f.nodes = append(f.nodes, glIssueNode{iid: i, updated: glBase.Add(time.Duration(i) * time.Second)})
		}
		issues, meta, err := f.client().ListIssuesUpdatedAfter(context.Background(), 42, glBase)
		if err != nil || !meta.Truncated {
			t.Fatalf("got (meta %+v, %v), want a truncated success", meta, err)
		}
		// Page k ends at issue 99k+1 (each moved bound re-reads one issue).
		if want := glBase.Add(9901 * time.Second); !meta.ResumeAt.Equal(want) {
			t.Errorf("ResumeAt = %v, want the last read updated_at %v", meta.ResumeAt, want)
		}
		if len(issues) != maxListPages*userReportPerPage {
			t.Errorf("len(issues) = %d, want %d", len(issues), maxListPages*userReportPerPage)
		}
	})
	for _, tc := range []struct {
		name    string
		runAt   time.Time
		wantErr bool
	}{
		{name: "run at the bound fails closed", runAt: glBase, wantErr: true},
		{name: "run after the bound truncates at the run", runAt: glBase.Add(time.Minute)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeIssueListing(t)
			f.maxRequests = maxListPages
			f.generate = func(offset, limit int) []glIssueNode {
				out := make([]glIssueNode, 0, limit)
				for i := offset; i < offset+limit; i++ {
					out = append(out, glIssueNode{iid: i + 1, updated: tc.runAt})
				}
				return out
			}
			issues, meta, err := f.client().ListIssuesUpdatedAfter(context.Background(), 42, glBase)
			if tc.wantErr {
				if !errors.Is(err, ErrEqualTimestampRunExceedsCap) || issues != nil {
					t.Fatalf("got (%d issues, %v), want nil and ErrEqualTimestampRunExceedsCap", len(issues), err)
				}
				return
			}
			if err != nil || !meta.Truncated || !meta.ResumeAt.Equal(tc.runAt) {
				t.Fatalf("got (meta %+v, %v), want truncated at %v", meta, err, tc.runAt)
			}
		})
	}
	t.Run("transient failure is not the typed error", func(t *testing.T) {
		f := newFakeIssueListing(t)
		f.status = http.StatusBadGateway
		_, _, err := f.client().ListIssuesUpdatedAfter(context.Background(), 42, glBase)
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadGateway || errors.Is(err, ErrEqualTimestampRunExceedsCap) {
			t.Errorf("err = %v, want an *APIError 502 that is not ErrEqualTimestampRunExceedsCap", err)
		}
	})
}

// TestListIssuesUpdatedAfter_NeverRequestsForgeNextLink pins the credential
// boundary on the walk: page 1's rel="next" Link names a DIFFERENT, reachable
// origin, and that origin must see ZERO requests — the next page is built
// from the client's own base. Counterfactual: following the Link target
// sends PRIVATE-TOKEN to the foreign server and its counter goes non-zero.
func TestListIssuesUpdatedAfter_NeverRequestsForgeNextLink(t *testing.T) {
	var foreignHits atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		foreignHits.Add(1)
		writeIssueJSON(w, http.StatusOK, `[]`)
	}))
	t.Cleanup(foreign.Close)
	f := newFakeIssueListing(t)
	f.nodes = glIssues(150)
	f.nextLinkHost = foreign.URL
	issues, _, err := f.client().ListIssuesUpdatedAfter(context.Background(), 42, glBase)
	if err != nil {
		t.Fatalf("ListIssuesUpdatedAfter: %v", err)
	}
	if n := foreignHits.Load(); n != 0 {
		t.Errorf("foreign origin received %d requests, want 0", n)
	}
	if got := glDistinct(issues); len(got) != 150 {
		t.Errorf("distinct issues = %d, want 150 (page 2 read from the base origin)", len(got))
	}
}

// TestListIssuesUpdatedAfter_ZeroAfterMissingDateAndRefusals covers the
// zero bound (no updated_after sent), the absent Date (zero, never a host
// clock), unparseable timestamps, a non-JSON body, an *APIError, a refused
// off-origin base, and the project-id preflight.
func TestListIssuesUpdatedAfter_ZeroAfterMissingDateAndRefusals(t *testing.T) {
	f := newFakeIssueListing(t)
	f.nodes = glIssues(1)
	f.date = ""
	_, meta, err := f.client().ListIssuesUpdatedAfter(context.Background(), 42, time.Time{})
	if err != nil {
		t.Fatalf("ListIssuesUpdatedAfter: %v", err)
	}
	if f.requests()[0].Has("updated_after") {
		t.Errorf("zero bound sent updated_after: %v", f.requests()[0])
	}
	if !meta.Date.IsZero() {
		t.Errorf("meta.Date = %v, want zero when the Date header is absent", meta.Date)
	}

	for name, fields := range map[string]map[string]any{
		"updated_at": {"updated_at": "yesterday"},
		"created_at": {"created_at": "yesterday"},
	} {
		f := newFakeIssueListing(t)
		f.nodes = glIssues(1)
		f.nodes[0].fields = fields
		if _, _, err := f.client().ListIssuesUpdatedAfter(context.Background(), 42, glBase); err == nil {
			t.Errorf("malformed %s: err = nil, want an error", name)
		}
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeIssueJSON(w, http.StatusOK, `not json`) }))
	t.Cleanup(bad.Close)
	if _, _, err := New(bad.URL, "t", WithHTTPClient(bad.Client())).ListIssuesUpdatedAfter(context.Background(), 42, glBase); err == nil {
		t.Error("non-JSON body: err = nil, want a decode error")
	}

	f = newFakeIssueListing(t)
	f.status = http.StatusForbidden
	var apiErr *APIError
	if _, _, err := f.client().ListIssuesUpdatedAfter(context.Background(), 42, glBase); !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden {
		t.Errorf("403: err = %v, want *APIError 403", err)
	}

	if _, _, err := New("http://127.0.0.1:1", "t").ListIssuesUpdatedAfter(context.Background(), 0, glBase); err == nil {
		t.Error("project 0: err = nil, want a refusal")
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	c := New(closed.URL, "t", WithHTTPClient(closed.Client()))
	closed.Close()
	if _, _, err := c.ListIssuesUpdatedAfter(context.Background(), 42, glBase); err == nil {
		t.Error("closed server: err = nil, want a transport error")
	}
}

// TestGetProjectMemberAccessLevel_MemberNonMemberAndError pins the three
// outcomes: 200 is a member at its level, 404 is a RESOLVED non-member with
// a nil error, and any other status is an *APIError (could not tell).
// Counterfactual: mapping 404 to an error reddens the non-member row.
func TestGetProjectMemberAccessLevel_MemberNonMemberAndError(t *testing.T) {
	s := newIssueServer(t)
	s.mux.HandleFunc("GET /api/v4/projects/42/members/all/{uid}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("uid") {
		case "7":
			writeIssueJSON(w, http.StatusOK, `{"id":7,"username":"dev","access_level":30}`)
		case "8":
			writeIssueJSON(w, http.StatusNotFound, `{"message":"404 Not found"}`)
		case "9":
			writeIssueJSON(w, http.StatusForbidden, `{"message":"403 Forbidden"}`)
		default:
			writeIssueJSON(w, http.StatusOK, `not json`)
		}
	})
	c := s.client()
	if level, member, err := c.GetProjectMemberAccessLevel(context.Background(), 42, 7); err != nil || !member || level != 30 {
		t.Errorf("member: got (%d, %v, %v), want (30, true, nil)", level, member, err)
	}
	if level, member, err := c.GetProjectMemberAccessLevel(context.Background(), 42, 8); err != nil || member || level != 0 {
		t.Errorf("non-member: got (%d, %v, %v), want (0, false, nil)", level, member, err)
	}
	var apiErr *APIError
	if _, member, err := c.GetProjectMemberAccessLevel(context.Background(), 42, 9); !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden || member {
		t.Errorf("403: got (member %v, %v), want an *APIError 403", member, err)
	}
	if _, _, err := c.GetProjectMemberAccessLevel(context.Background(), 42, 10); err == nil {
		t.Error("non-JSON 200: err = nil, want a decode error")
	}
	if _, _, err := c.GetProjectMemberAccessLevel(context.Background(), 0, 7); err == nil {
		t.Error("project 0: err = nil, want a refusal")
	}
	if _, _, err := c.GetProjectMemberAccessLevel(context.Background(), 42, 0); err == nil {
		t.Error("user 0: err = nil, want a refusal")
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	cc := New(closed.URL, "t", WithHTTPClient(closed.Client()))
	closed.Close()
	if _, _, err := cc.GetProjectMemberAccessLevel(context.Background(), 42, 7); err == nil {
		t.Error("closed server: err = nil, want a transport error")
	}
}
