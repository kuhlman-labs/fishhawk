package github

// Tests for the GitHub user-report reader (#3771). Most rows drive a fake API
// implementing the optional userReportLister extension; the keyset rows drive
// a REAL githubclient.Client against an httptest GitHub so the walk, the
// boundary re-reads and the shared dedupe are exercised together.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// userReportAPI is the base fakeAPI plus the optional activity listings.
type userReportAPI struct {
	*fakeAPI
	issues       []githubclient.UpdatedIssue
	issuesMeta   githubclient.ListingMeta
	issuesErr    error
	comments     []githubclient.UpdatedIssueComment
	commentsMeta githubclient.ListingMeta
	commentsErr  error
	gotSince     []time.Time
}

func (a *userReportAPI) ListIssuesUpdatedSince(_ context.Context, _ forge.CredentialScope, _ forge.RepoRef, since time.Time) ([]githubclient.UpdatedIssue, githubclient.ListingMeta, error) {
	a.gotSince = append(a.gotSince, since)
	if a.issuesErr != nil {
		return nil, githubclient.ListingMeta{}, a.issuesErr
	}
	return a.issues, a.issuesMeta, nil
}

func (a *userReportAPI) ListIssueCommentsUpdatedSince(_ context.Context, _ forge.CredentialScope, _ forge.RepoRef, since time.Time) ([]githubclient.UpdatedIssueComment, githubclient.ListingMeta, error) {
	a.gotSince = append(a.gotSince, since)
	if a.commentsErr != nil {
		return nil, githubclient.ListingMeta{}, a.commentsErr
	}
	return a.comments, a.commentsMeta, nil
}

var (
	urSince = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	urDate  = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
)

func newUserReportAPI() *userReportAPI {
	return &userReportAPI{
		fakeAPI:      &fakeAPI{},
		issuesMeta:   githubclient.ListingMeta{Date: urDate},
		commentsMeta: githubclient.ListingMeta{Date: urDate.Add(time.Minute)},
	}
}

func urIssue(n int, login, userType, assoc string, updated time.Time) githubclient.UpdatedIssue {
	return githubclient.UpdatedIssue{
		Number: n, Title: fmt.Sprintf("issue %d", n), Body: "body", HTMLURL: fmt.Sprintf("https://github.com/o/r/issues/%d", n),
		AuthorLogin: login, AuthorType: userType, AuthorAssociation: assoc, AssociationPresent: assoc != "",
		Reactions: githubclient.ReactionRollup{TotalCount: 1, PlusOne: 1}, ReactionsPresent: true,
		CreatedAt: updated, UpdatedAt: updated,
	}
}

func urComment(id int64, issue int, login, userType, assoc string, updated time.Time, onPR bool) githubclient.UpdatedIssueComment {
	return githubclient.UpdatedIssueComment{
		ID: id, IssueNumber: issue, Body: "comment", HTMLURL: fmt.Sprintf("https://github.com/o/r/issues/%d#issuecomment-%d", issue, id),
		AuthorLogin: login, AuthorType: userType, AuthorAssociation: assoc, AssociationPresent: assoc != "",
		ReactionsPresent: true, CreatedAt: updated, UpdatedAt: updated, OnPullRequest: onPR,
	}
}

func listReports(t *testing.T, api API) (*workmgmt.UserReportPage, error) {
	t.Helper()
	return New(api).ListUserReports(context.Background(), workmgmt.ListUserReportsRequest{Target: readerTarget(), Since: urSince})
}

func mustListReports(t *testing.T, api API) *workmgmt.UserReportPage {
	t.Helper()
	page, err := listReports(t, api)
	if err != nil {
		t.Fatalf("ListUserReports: %v", err)
	}
	return page
}

// assertUserReportUnavailable is assertUnavailable for the user-report
// capability (the shared helper pins the work-item read capability name).
func assertUserReportUnavailable(t *testing.T, err error, want workmgmt.UnavailableReason) *workmgmt.UnavailableError {
	t.Helper()
	var ue *workmgmt.UnavailableError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v (%T), want *workmgmt.UnavailableError", err, err)
	}
	if ue.Reason != want || ue.Provider != ProviderName || ue.Capability != workmgmt.UserReportCapability {
		t.Fatalf("unavailable = %+v, want reason %q for %q/%q", ue, want, ProviderName, workmgmt.UserReportCapability)
	}
	return ue
}

func degradation(page *workmgmt.UserReportPage, code workmgmt.UserReportDegradationCode) (workmgmt.UserReportDegradation, bool) {
	for _, d := range page.Degradations {
		if d.Code == code {
			return d, true
		}
	}
	return workmgmt.UserReportDegradation{}, false
}

// TestListUserReports_MapsAssociationToInternal: OWNER/MEMBER/COLLABORATOR are
// internal; every other association is not. Counterfactual: dropping
// COLLABORATOR from internalAssociations reddens its row.
func TestListUserReports_MapsAssociationToInternal(t *testing.T) {
	cases := map[string]bool{
		"OWNER": true, "MEMBER": true, "COLLABORATOR": true,
		"CONTRIBUTOR": false, "FIRST_TIME_CONTRIBUTOR": false, "FIRST_TIMER": false, "MANNEQUIN": false, "NONE": false,
	}
	for assoc, want := range cases {
		t.Run(assoc, func(t *testing.T) {
			api := newUserReportAPI()
			api.issues = []githubclient.UpdatedIssue{urIssue(1, "alice", "User", assoc, urSince.Add(time.Second))}
			page := mustListReports(t, api)
			if len(page.Items) != 1 {
				t.Fatalf("items = %d, want 1", len(page.Items))
			}
			a := page.Items[0].Author
			if a.Internal != want || a.Association != assoc || !a.AssociationResolved {
				t.Errorf("author = %+v, want Internal %v Association %q resolved", a, want, assoc)
			}
		})
	}
}

// TestListUserReports_BotDetectionTypeAndSuffixIsolated: each fixture row
// isolates ONE arm of the bot rule, so deleting either arm reddens its row.
func TestListUserReports_BotDetectionTypeAndSuffixIsolated(t *testing.T) {
	api := newUserReportAPI()
	api.issues = []githubclient.UpdatedIssue{
		urIssue(1, "some-app", "Bot", "NONE", urSince.Add(time.Second)),           // type arm only
		urIssue(2, "dependabot[bot]", "User", "NONE", urSince.Add(2*time.Second)), // suffix arm only
		urIssue(3, "alice", "User", "NONE", urSince.Add(3*time.Second)),           // neither
	}
	page := mustListReports(t, api)
	want := map[int]bool{1: true, 2: true, 3: false}
	for _, it := range page.Items {
		if it.Author.Bot != want[it.IssueNumber] {
			t.Errorf("issue #%d author %q Bot = %v, want %v", it.IssueNumber, it.Author.Login, it.Author.Bot, want[it.IssueNumber])
		}
	}
}

// TestListUserReports_DropsPullRequestComments: a PR-conversation comment is
// excluded; an issue comment is kept and mapped. Counterfactual: deleting the
// OnPullRequest filter returns the PR comment.
func TestListUserReports_DropsPullRequestComments(t *testing.T) {
	api := newUserReportAPI()
	api.comments = []githubclient.UpdatedIssueComment{
		urComment(11, 4, "alice", "User", "MEMBER", urSince.Add(time.Second), false),
		urComment(12, 5, "bob", "User", "NONE", urSince.Add(2*time.Second), true),
	}
	page := mustListReports(t, api)
	if len(page.Items) != 1 || page.Items[0].CommentID != 11 {
		t.Fatalf("items = %+v, want only comment 11", page.Items)
	}
	got := page.Items[0]
	if got.Kind != workmgmt.UserReportKindComment || got.IssueNumber != 4 || got.Title != "" || !got.Author.Internal ||
		got.URL != "https://github.com/o/r/issues/4#issuecomment-11" {
		t.Errorf("comment item = %+v", got)
	}
}

// TestListUserReports_DedupesShiftedItemExactlyOnce: the same issue at a page
// boundary (twice) and the same comment (twice, the later copy updated
// mid-scan) collapse to one item each, carrying the newest state.
// Counterfactual: deleting the DedupeUserReportItems call returns both copies.
func TestListUserReports_DedupesShiftedItemExactlyOnce(t *testing.T) {
	api := newUserReportAPI()
	shifted := urIssue(7, "alice", "User", "NONE", urSince.Add(5*time.Second))
	api.issues = []githubclient.UpdatedIssue{urIssue(6, "alice", "User", "NONE", urSince.Add(time.Second)), shifted, shifted}
	newer := urComment(20, 6, "bob", "User", "NONE", urSince.Add(9*time.Second), false)
	newer.Body = "edited"
	api.comments = []githubclient.UpdatedIssueComment{urComment(20, 6, "bob", "User", "NONE", urSince.Add(2*time.Second), false), newer}
	page := mustListReports(t, api)
	counts := map[string]int{}
	for _, it := range page.Items {
		counts[fmt.Sprintf("%s/%d/%d", it.Kind, it.IssueNumber, it.CommentID)]++
	}
	want := map[string]int{"issue/6/0": 1, "issue/7/0": 1, "comment/6/20": 1}
	if fmt.Sprint(counts) != fmt.Sprint(want) {
		t.Errorf("item counts = %v, want %v", counts, want)
	}
	last := page.Items[len(page.Items)-1]
	if last.CommentID != 20 || last.Body != "edited" {
		t.Errorf("last item = %+v, want the newest copy of comment 20 sorted last", last)
	}
}

// TestListUserReports_ForbiddenIsTypedUnavailableWithNilPage: a 403 from
// EITHER listing is a typed ReasonForbidden with the forge sentinel retained
// and a NIL page.
func TestListUserReports_ForbiddenIsTypedUnavailableWithNilPage(t *testing.T) {
	for _, which := range []string{"issues", "comments"} {
		t.Run(which, func(t *testing.T) {
			api := newUserReportAPI()
			forbidden := fmt.Errorf("list: %w", githubclient.ErrForbidden)
			if which == "issues" {
				api.issuesErr = forbidden
			} else {
				api.commentsErr = forbidden
			}
			page, err := listReports(t, api)
			if page != nil {
				t.Errorf("page = %+v, want NIL on a permission refusal", page)
			}
			assertUserReportUnavailable(t, err, workmgmt.ReasonForbidden)
			if !errors.Is(err, githubclient.ErrForbidden) {
				t.Errorf("errors.Is(err, ErrForbidden) = false; the typed wrapper dropped the cause: %v", err)
			}
		})
	}
}

// TestListUserReports_EqualTimestampCapIsUnresumable: a cap hit inside one
// equal-timestamp run is a nil page with an errors.Is-matchable
// workmgmt.ErrUserReportUnresumable, distinct from a transient failure (which
// is wrapped and matches neither sentinel). With a nil page there is no
// NextCursor, so the caller's cursor cannot move.
func TestListUserReports_EqualTimestampCapIsUnresumable(t *testing.T) {
	api := newUserReportAPI()
	api.commentsErr = fmt.Errorf("walk: %w", githubclient.ErrEqualTimestampRunExceedsCap)
	page, err := listReports(t, api)
	if page != nil {
		t.Errorf("page = %+v, want NIL", page)
	}
	if !errors.Is(err, workmgmt.ErrUserReportUnresumable) || !errors.Is(err, githubclient.ErrEqualTimestampRunExceedsCap) {
		t.Errorf("err = %v, want errors.Is ErrUserReportUnresumable AND the client sentinel", err)
	}

	transient := newUserReportAPI()
	transient.issuesErr = errors.New("connection reset by peer")
	page, err = listReports(t, transient)
	if page != nil || err == nil {
		t.Fatalf("transient = (%+v, %v), want nil page and an error", page, err)
	}
	if errors.Is(err, workmgmt.ErrUserReportUnresumable) {
		t.Errorf("a transient failure must not match ErrUserReportUnresumable: %v", err)
	}
	var ue *workmgmt.UnavailableError
	if errors.As(err, &ue) {
		t.Errorf("a transient failure must not masquerade as a capability degradation: %v", err)
	}
}

func TestListUserReports_ZeroScopeIsNoInstallation(t *testing.T) {
	target := readerTarget()
	target.Scope = forge.CredentialScope{}
	page, err := New(newUserReportAPI()).ListUserReports(context.Background(), workmgmt.ListUserReportsRequest{Target: target, Since: urSince})
	if page != nil {
		t.Errorf("page = %+v, want NIL", page)
	}
	// The shared preflight's typed error is re-stamped with the user-report
	// capability name.
	assertUserReportUnavailable(t, err, workmgmt.ReasonNoInstallation)
}

func TestListUserReports_APIWithoutExtensionIsNotImplemented(t *testing.T) {
	page, err := listReports(t, &fakeAPI{})
	if page != nil {
		t.Errorf("page = %+v, want NIL", page)
	}
	assertUserReportUnavailable(t, err, workmgmt.ReasonNotImplemented)
}

// TestListUserReports_AnchorsCursorOnIssuesListingDate: NextCursor is the
// ISSUES listing's first Date (the scan's earliest request) minus the overlap,
// not the later comments listing's.
func TestListUserReports_AnchorsCursorOnIssuesListingDate(t *testing.T) {
	page := mustListReports(t, newUserReportAPI())
	if want := urDate.Add(-workmgmt.UserReportCursorOverlap); !page.NextCursor.Equal(want) {
		t.Errorf("NextCursor = %v, want issues Date minus overlap %v", page.NextCursor, want)
	}
	if page.Forge != workmgmt.UserReportForgeGitHub || !page.Since.Equal(urSince) || len(page.Degradations) != 0 {
		t.Errorf("page = %+v, want forge github, since echoed, no degradations", page)
	}
}

func TestListUserReports_MissingDateHoldsCursorAndNamesDegradation(t *testing.T) {
	api := newUserReportAPI()
	api.issuesMeta.Date = time.Time{}
	page := mustListReports(t, api)
	if !page.NextCursor.Equal(urSince) {
		t.Errorf("NextCursor = %v, want since %v held", page.NextCursor, urSince)
	}
	if _, ok := degradation(page, workmgmt.UserReportCursorAnchorUnavailable); !ok {
		t.Errorf("degradations = %+v, want cursor_anchor_unavailable", page.Degradations)
	}
}

// TestListUserReports_TruncationAdvancesToLastReadAndNamesScanTruncated: a
// truncated comments listing pulls NextCursor back to its ResumeAt (earlier
// than the anchor) and names scan_truncated; the items read are returned.
func TestListUserReports_TruncationAdvancesToLastReadAndNamesScanTruncated(t *testing.T) {
	api := newUserReportAPI()
	resume := urSince.Add(time.Hour)
	api.comments = []githubclient.UpdatedIssueComment{urComment(1, 1, "a", "User", "NONE", resume, false)}
	api.commentsMeta = githubclient.ListingMeta{Date: urDate, Truncated: true, ResumeAt: resume}
	page := mustListReports(t, api)
	if !page.NextCursor.Equal(resume) {
		t.Errorf("NextCursor = %v, want the truncated listing's ResumeAt %v", page.NextCursor, resume)
	}
	if d, ok := degradation(page, workmgmt.UserReportScanTruncated); !ok || d.Count != 1 {
		t.Errorf("degradations = %+v, want scan_truncated Count 1", page.Degradations)
	}
	if len(page.Items) != 1 {
		t.Errorf("items = %d, want the read item returned", len(page.Items))
	}
}

// TestListUserReports_UnresolvedAssociationAndReactionsNamed: an absent
// author_association is unresolved and NEVER internal; an absent reactions
// object is unresolved, not zero; both are named with counts.
func TestListUserReports_UnresolvedAssociationAndReactionsNamed(t *testing.T) {
	api := newUserReportAPI()
	noAssoc := urIssue(1, "alice", "User", "", urSince.Add(time.Second))
	noReactions := urIssue(2, "bob", "User", "OWNER", urSince.Add(2*time.Second))
	noReactions.ReactionsPresent = false
	api.issues = []githubclient.UpdatedIssue{noAssoc, noReactions}
	page := mustListReports(t, api)
	a := page.Items[0]
	if a.Author.AssociationResolved || a.Author.Internal || a.Author.Association != "" {
		t.Errorf("issue #1 author = %+v, want unresolved and not internal", a.Author)
	}
	if !a.Reactions.Resolved || a.Reactions.PlusOne != 1 {
		t.Errorf("issue #1 reactions = %+v, want the present rollup mapped", a.Reactions)
	}
	if b := page.Items[1]; b.Reactions.Resolved || b.Reactions.Total != 0 {
		t.Errorf("issue #2 reactions = %+v, want unresolved", b.Reactions)
	}
	if d, ok := degradation(page, workmgmt.UserReportAssociationUnresolved); !ok || d.Count != 1 {
		t.Errorf("degradations = %+v, want association_unresolved Count 1", page.Degradations)
	}
	if d, ok := degradation(page, workmgmt.UserReportReactionsPartial); !ok || d.Count != 1 {
		t.Errorf("degradations = %+v, want reactions_partial Count 1", page.Degradations)
	}
}

func TestListUserReports_OtherListingErrorIsWrapped(t *testing.T) {
	api := newUserReportAPI()
	api.issuesErr = errors.New("boom")
	page, err := listReports(t, api)
	if page != nil || err == nil {
		t.Fatalf("= (%+v, %v), want nil page and an error", page, err)
	}
}

// ghNode is one node the keyset fake serves.
type ghNode struct {
	id      int
	updated time.Time
	fields  map[string]any
}

// keysetGitHub is an httptest GitHub serving the issues and issue-comments
// listings with GitHub's documented semantics (since inclusive, sort=updated
// direction=asc, page offsets over the filtered list, Link rel=next while
// nodes remain). beforeRequest runs before the n-th request (1-based, across
// both listings) so a test can mutate the node sets between page requests.
type keysetGitHub struct {
	mu            sync.Mutex
	nodes         map[string][]ghNode
	requests      int
	beforeRequest func(n int, g *keysetGitHub)
}

func (g *keysetGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests++
	if g.beforeRequest != nil {
		g.beforeRequest(g.requests, g)
	}
	q := r.URL.Query()
	var since time.Time
	if s := q.Get("since"); s != "" {
		since, _ = time.Parse(time.RFC3339Nano, s)
	}
	perPage, _ := strconv.Atoi(q.Get("per_page"))
	page := 1
	if p := q.Get("page"); p != "" {
		page, _ = strconv.Atoi(p)
	}
	var filtered []ghNode
	for _, nd := range g.nodes[r.URL.Path] {
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
	offset := (page - 1) * perPage
	var window []ghNode
	if offset < len(filtered) {
		window = filtered[offset:min(offset+perPage, len(filtered))]
		if offset+perPage < len(filtered) {
			w.Header().Set("Link", fmt.Sprintf(`<https://api.github.invalid%s?page=%d>; rel="next"`, r.URL.Path, page+1))
		}
	}
	body := make([]map[string]any, 0, len(window))
	for _, nd := range window {
		m := map[string]any{"updated_at": nd.updated.UTC().Format(time.RFC3339), "created_at": "2026-01-01T00:00:00Z"}
		for k, v := range nd.fields {
			m[k] = v
		}
		body = append(body, m)
	}
	w.Header().Set("Date", urDate.Format(http.TimeFormat))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (g *keysetGitHub) bump(path string, id int, to time.Time) {
	for i := range g.nodes[path] {
		if g.nodes[path][i].id == id {
			g.nodes[path][i].updated = to
			return
		}
	}
	panic(fmt.Sprintf("bump: no node %d", id))
}

func (g *keysetGitHub) remove(path string, id int) {
	ns := g.nodes[path]
	for i := range ns {
		if ns[i].id == id {
			g.nodes[path] = append(ns[:i], ns[i+1:]...)
			return
		}
	}
	panic(fmt.Sprintf("remove: no node %d", id))
}

// TestListUserReports_RealClientKeysetWalkReturnsEverySurvivorExactlyOnce
// drives the provider through a REAL client (approval condition 1, GitHub
// half). 250 issues walk in three keyset pages, so every page boundary is
// re-read inclusively (a shifted duplicate); issue #1, already read, is DELETED
// before page 2; issue #50, already read, is UPDATED before page 2 and so
// reappears at the walk's tail. Every surviving issue must come back EXACTLY
// once, #50 with its newer updated_at. Counterfactuals: deleting the dedupe
// call returns the boundary issues twice; an offset walk (page N under the
// original since, never moving the bound) skips #101, which the deletion
// shifted onto page 1's final offset.
func TestListUserReports_RealClientKeysetWalkReturnsEverySurvivorExactlyOnce(t *testing.T) {
	const issuesPath, commentsPath = "/repos/o/r/issues", "/repos/o/r/issues/comments"
	g := &keysetGitHub{nodes: map[string][]ghNode{}}
	for i := 1; i <= 250; i++ {
		g.nodes[issuesPath] = append(g.nodes[issuesPath], ghNode{id: i, updated: urSince.Add(time.Duration(i) * time.Second), fields: map[string]any{
			"number": i, "title": fmt.Sprintf("issue %d", i), "user": map[string]any{"login": "u", "type": "User"}, "author_association": "NONE", "reactions": map[string]any{"total_count": 0},
		}})
	}
	g.nodes[commentsPath] = []ghNode{
		{id: 9001, updated: urSince.Add(time.Second), fields: map[string]any{"id": 9001, "issue_url": "https://api.github.com/repos/o/r/issues/3", "html_url": "https://github.com/o/r/issues/3#issuecomment-9001", "user": map[string]any{"login": "dependabot[bot]", "type": "Bot"}, "author_association": "NONE", "reactions": map[string]any{"total_count": 0}}},
		{id: 9002, updated: urSince.Add(2 * time.Second), fields: map[string]any{"id": 9002, "issue_url": "https://api.github.com/repos/o/r/issues/77", "html_url": "https://github.com/o/r/pull/77#issuecomment-9002", "user": map[string]any{"login": "v", "type": "User"}}},
	}
	tail := urSince.Add(time.Hour)
	g.beforeRequest = func(n int, g *keysetGitHub) {
		if n == 2 {
			g.remove(issuesPath, 1)
			g.bump(issuesPath, 50, tail)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(srv.Close)
	c := githubclient.New(stubTokenProvider{token: "ghs_install"})
	c.BaseURL = srv.URL
	c.HTTP = &http.Client{Timeout: 10 * time.Second}

	page, err := New(c).ListUserReports(context.Background(), workmgmt.ListUserReportsRequest{
		Target: workmgmt.Target{Scope: forge.FromGitHubInstallationID(99), Repo: workmgmt.Repo{Owner: "o", Name: "r"}},
		Since:  urSince,
	})
	if err != nil {
		t.Fatalf("ListUserReports: %v", err)
	}
	issueCount := map[int]int{}
	var comments []int64
	for _, it := range page.Items {
		switch it.Kind {
		case workmgmt.UserReportKindIssue:
			issueCount[it.IssueNumber]++
			if it.IssueNumber == 50 && !it.UpdatedAt.Equal(tail) {
				t.Errorf("issue #50 UpdatedAt = %v, want the mid-scan update %v", it.UpdatedAt, tail)
			}
		case workmgmt.UserReportKindComment:
			comments = append(comments, it.CommentID)
		}
	}
	for i := 2; i <= 250; i++ {
		if issueCount[i] != 1 {
			t.Errorf("issue #%d returned %d times, want exactly once", i, issueCount[i])
		}
	}
	if fmt.Sprint(comments) != "[9001]" {
		t.Errorf("comments = %v, want [9001] (the PR comment 9002 dropped)", comments)
	}
	if want := urDate.Add(-workmgmt.UserReportCursorOverlap); !page.NextCursor.Equal(want) {
		t.Errorf("NextCursor = %v, want forge Date minus overlap %v", page.NextCursor, want)
	}
	if len(page.Degradations) != 0 {
		t.Errorf("degradations = %+v, want none", page.Degradations)
	}
}
