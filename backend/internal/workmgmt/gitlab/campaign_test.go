package gitlab

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/gitlabclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// blockedBy / relatesTo build a same-project (42) link; foreign builds a
// cross-project one. The fake's default project resolves to id 42.
func blockedBy(iid int) gitlabclient.IssueLink {
	return gitlabclient.IssueLink{IID: iid, ProjectID: 42, LinkType: gitlabclient.LinkTypeIsBlockedBy}
}

func relatesTo(iid int) gitlabclient.IssueLink {
	return gitlabclient.IssueLink{IID: iid, ProjectID: 42, LinkType: gitlabclient.LinkTypeRelatesTo}
}

func foreign(iid, project int, linkType string) gitlabclient.IssueLink {
	return gitlabclient.IssueLink{IID: iid, ProjectID: project, LinkType: linkType}
}

func glTarget() workmgmt.Target {
	return workmgmt.Target{Repo: workmgmt.Repo{Owner: "acme", Name: "widgets"}, GitLab: &workmgmt.GitLabConnection{}}
}

func resolveItems(t *testing.T, api *fakeAPI, items ...string) (*workmgmt.EpicChildrenResult, error) {
	t.Helper()
	return New(api).ResolveDependencies(context.Background(), workmgmt.IssueSetRequest{Target: glTarget(), Items: items})
}

func edgeNums(es []workmgmt.DependsEdge) [][2]int {
	out := make([][2]int, 0, len(es))
	for _, e := range es {
		out = append(out, [2]int{e.From, e.To})
	}
	return out
}

func childNums(cs []workmgmt.EpicChild) []int {
	out := make([]int, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Number)
	}
	return out
}

// TestProvider_ImplementsCampaignSources pins that the gitlab provider now
// serves BOTH campaign sources — what server.campaignSourcesSupported reads.
func TestProvider_ImplementsCampaignSources(t *testing.T) {
	var p workmgmt.Provider = New(&fakeAPI{})
	if _, ok := p.(workmgmt.EpicChildrenQuerier); !ok {
		t.Error("gitlab provider does not satisfy workmgmt.EpicChildrenQuerier")
	}
	if _, ok := p.(workmgmt.IssueSetDependencyResolver); !ok {
		t.Error("gitlab provider does not satisfy workmgmt.IssueSetDependencyResolver")
	}
}

// TestResolveDependencies_ThreeIssueDAG is the done-means test for the issue's
// first acceptance criterion: three named issues, one is_blocked_by link
// between two of them, driven through the fake — the SHIPPED result carries
// the children ascending with their mapped fields and exactly one edge.
func TestResolveDependencies_ThreeIssueDAG(t *testing.T) {
	api := &fakeAPI{}
	api.issue(3, "opened", "three", []string{"autonomy:high"}, blockedBy(1), relatesTo(2)).
		issue(1, "closed", "one", []string{"runnable:no"}).
		issue(2, "opened", "two", nil, gitlabclient.IssueLink{IID: 3, ProjectID: 42, LinkType: gitlabclient.LinkTypeBlocks})

	res, err := resolveItems(t, api, "#3", "issue:1", "2")
	if err != nil {
		t.Fatalf("ResolveDependencies: %v", err)
	}
	want := &workmgmt.EpicChildrenResult{
		Children: []workmgmt.EpicChild{
			{Number: 1, Title: "issue 1", Complete: true, State: "CLOSED", Body: "one", URL: "https://gitlab.example/acme/widgets/-/issues/1", NotRunnable: true},
			{Number: 2, Title: "issue 2", State: "OPEN", Body: "two", URL: "https://gitlab.example/acme/widgets/-/issues/2"},
			{Number: 3, Title: "issue 3", Autonomy: "high", State: "OPEN", Body: "three", URL: "https://gitlab.example/acme/widgets/-/issues/3"},
		},
		Edges: []workmgmt.DependsEdge{{From: 3, To: 1}},
	}
	if !reflect.DeepEqual(res, want) {
		t.Fatalf("result = %+v\nwant     %+v", res, want)
	}
	if api.getPath != "acme/widgets" {
		t.Errorf("GetProject path = %q, want the repo owner/name fallback", api.getPath)
	}
}

// TestResolveDependencies_FreeTierRelatesToOnly is the honest Free-tier
// outcome: every link is relates_to, so the children resolve with ZERO edges.
// It is the vehicle for the link_type == is_blocked_by filter.
func TestResolveDependencies_FreeTierRelatesToOnly(t *testing.T) {
	api := &fakeAPI{}
	api.issue(1, "opened", "", nil, relatesTo(2)).
		issue(2, "opened", "", nil, relatesTo(1), relatesTo(9)).
		issue(9, "opened", "", nil)

	res, err := resolveItems(t, api, "1", "2")
	if err != nil {
		t.Fatalf("ResolveDependencies: %v", err)
	}
	if got := childNums(res.Children); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Errorf("children = %v, want [1 2]", got)
	}
	if len(res.Edges) != 0 || len(res.DroppedEdges) != 0 || len(res.SatisfiedEdges) != 0 {
		t.Errorf("edges=%v dropped=%v satisfied=%v, want all empty (relates_to is not a dependency)", res.Edges, res.DroppedEdges, res.SatisfiedEdges)
	}
	if n := api.getIssueCalls(9); n != 0 {
		t.Errorf("GetIssue(#9) called %d times, want 0 (a relates_to target is never classified)", n)
	}
}

func TestResolveDependencies_OutOfSetClassification(t *testing.T) {
	tests := []struct {
		name          string
		setup         func(api *fakeAPI)
		wantDropped   []workmgmt.DependsEdge
		wantSatisfied []workmgmt.SatisfiedEdge
	}{
		{
			name:        "open target keeps DropNotChild",
			setup:       func(api *fakeAPI) { api.issue(9, "opened", "", nil) },
			wantDropped: []workmgmt.DependsEdge{{From: 1, To: 9, Reason: workmgmt.DropNotChild}},
		},
		{
			name:          "closed target is satisfied with an empty StateReason",
			setup:         func(api *fakeAPI) { api.issue(9, "closed", "", nil) },
			wantSatisfied: []workmgmt.SatisfiedEdge{{From: 1, To: 9, State: "closed", StateReason: ""}},
		},
		{
			name:        "target fetch error is unreadable, not an error",
			setup:       func(api *fakeAPI) { api.issueErrs = map[int]error{9: errors.New("403 forbidden")} },
			wantDropped: []workmgmt.DependsEdge{{From: 1, To: 9, Reason: workmgmt.DropTargetStateUnreadable}},
		},
		{
			// A CLOSED issue returned alongside an error: without the error
			// branch it would be read as satisfied.
			name: "target error alongside a closed issue is unreadable, never satisfied",
			setup: func(api *fakeAPI) {
				api.issue(9, "closed", "", nil)
				api.issueErrs = map[int]error{9: errors.New("partial read")}
				api.errWithIssue = map[int]bool{9: true}
			},
			wantDropped: []workmgmt.DependsEdge{{From: 1, To: 9, Reason: workmgmt.DropTargetStateUnreadable}},
		},
		{
			name:        "nil target issue is unreadable",
			setup:       func(api *fakeAPI) { api.nilIssue = map[int]bool{9: true} },
			wantDropped: []workmgmt.DependsEdge{{From: 1, To: 9, Reason: workmgmt.DropTargetStateUnreadable}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAPI{}
			api.issue(1, "opened", "", nil, blockedBy(9))
			tc.setup(api)
			res, err := resolveItems(t, api, "1")
			if err != nil {
				t.Fatalf("ResolveDependencies: %v", err)
			}
			if len(res.Edges) != 0 {
				t.Errorf("edges = %v, want none", res.Edges)
			}
			if !reflect.DeepEqual(res.DroppedEdges, tc.wantDropped) {
				t.Errorf("dropped = %+v, want %+v", res.DroppedEdges, tc.wantDropped)
			}
			if !reflect.DeepEqual(res.SatisfiedEdges, tc.wantSatisfied) {
				t.Errorf("satisfied = %+v, want %+v", res.SatisfiedEdges, tc.wantSatisfied)
			}
		})
	}
}

// TestResolveDependencies_CrossProjectLinkNeverFetched seeds the bad state BY
// CONSTRUCTION: an is_blocked_by link whose project id (99) differs from the
// queried project, whose iid (5) COLLIDES with a closed LOCAL issue. Reducing
// it to a local number would read #5 and falsely SATISFY the edge; the guard
// stamps it unreadable with no fetch.
func TestResolveDependencies_CrossProjectLinkNeverFetched(t *testing.T) {
	api := &fakeAPI{}
	api.issue(1, "opened", "", nil, foreign(5, 99, gitlabclient.LinkTypeIsBlockedBy)).
		issue(5, "closed", "an unrelated local issue", nil)

	res, err := resolveItems(t, api, "1")
	if err != nil {
		t.Fatalf("ResolveDependencies: %v", err)
	}
	if n := api.getIssueCalls(5); n != 0 {
		t.Errorf("GetIssue(#5) called %d times, want 0 (a cross-project iid must never be read locally)", n)
	}
	if len(res.SatisfiedEdges) != 0 {
		t.Errorf("satisfied = %+v, want none", res.SatisfiedEdges)
	}
	if len(res.DroppedEdges) != 1 {
		t.Fatalf("dropped = %+v, want exactly one", res.DroppedEdges)
	}
	d := res.DroppedEdges[0]
	if d.From != 1 || d.To != 0 || d.Reason != workmgmt.DropTargetStateUnreadable || d.ToRef != "project:99#5" {
		t.Errorf("dropped edge = %+v, want From 1, To 0, unreadable, ToRef project:99#5", d)
	}
	if got := d.TargetRef(); strings.HasPrefix(got, "issue:") || !strings.Contains(got, "project:99#5") {
		t.Errorf("TargetRef = %q, want the foreign identity, never issue:<n>", got)
	}
}

func TestResolveDependencies_DuplicateAndSelfLinks(t *testing.T) {
	api := &fakeAPI{}
	api.issue(1, "opened", "", nil, blockedBy(2), blockedBy(2), blockedBy(1), blockedBy(9), blockedBy(9)).
		issue(2, "opened", "", nil).
		issue(9, "opened", "", nil)

	res, err := resolveItems(t, api, "1", "2")
	if err != nil {
		t.Fatalf("ResolveDependencies: %v", err)
	}
	if got := edgeNums(res.Edges); !reflect.DeepEqual(got, [][2]int{{1, 2}}) {
		t.Errorf("edges = %v, want [[1 2]] (duplicate collapsed, self-link dropped)", got)
	}
	if got := edgeNums(res.DroppedEdges); !reflect.DeepEqual(got, [][2]int{{1, 9}}) {
		t.Errorf("dropped = %v, want [[1 9]] (duplicate out-of-set link classified once)", got)
	}
	if n := api.getIssueCalls(9); n != 1 {
		t.Errorf("GetIssue(#9) = %d calls, want 1 (distinct targets fetched once)", n)
	}
}

func TestResolveDependencies_DuplicateRefsResolveOnce(t *testing.T) {
	api := &fakeAPI{}
	api.issue(1, "opened", "", nil)
	res, err := resolveItems(t, api, "1", "#1", "issue:1")
	if err != nil {
		t.Fatalf("ResolveDependencies: %v", err)
	}
	if got := childNums(res.Children); !reflect.DeepEqual(got, []int{1}) {
		t.Errorf("children = %v, want [1]", got)
	}
	if n := api.getIssueCalls(1); n != 1 {
		t.Errorf("GetIssue(#1) = %d calls, want 1", n)
	}
}

func TestResolveDependencies_InvalidItemRef(t *testing.T) {
	api := &fakeAPI{}
	_, err := resolveItems(t, api, "1", "owner/repo#2")
	if !errors.Is(err, workmgmt.ErrInvalidItemRef) {
		t.Fatalf("err = %v, want it to wrap workmgmt.ErrInvalidItemRef", err)
	}
	if !strings.Contains(err.Error(), `"owner/repo#2"`) || !strings.Contains(err.Error(), "not a numeric issue reference") {
		t.Errorf("err = %v, want the ref and the parse cause named", err)
	}
	if n := api.getIssueCalls(1); n != 0 {
		t.Errorf("GetIssue called %d times, want 0 (refs are validated before any read)", n)
	}
}

func TestResolveDependencies_FetchFailuresFailClosed(t *testing.T) {
	tests := []struct {
		name  string
		setup func(api *fakeAPI)
		want  string
	}{
		{
			name: "fetch error names the first item in request order",
			setup: func(api *fakeAPI) {
				api.issueErrs = map[int]error{2: errors.New("boom two"), 3: errors.New("boom three")}
			},
			want: "get issue #2: boom two",
		},
		{
			name:  "nil issue fails closed",
			setup: func(api *fakeAPI) { api.nilIssue = map[int]bool{2: true} },
			want:  "get issue #2: no issue returned",
		},
		{
			name:  "link list error fails closed",
			setup: func(api *fakeAPI) { api.linkErrs = map[int]error{3: errors.New("links 500")} },
			want:  "get issue #3: list issue links: links 500",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAPI{}
			api.issue(1, "opened", "", nil).issue(2, "opened", "", nil).issue(3, "opened", "", nil)
			tc.setup(api)
			res, err := resolveItems(t, api, "1", "2", "3")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ResolveDependencies = %+v, %v; want error containing %q", res, err, tc.want)
			}
			var to *workmgmt.IssueSetResolutionTimeout
			if errors.As(err, &to) {
				t.Errorf("err = %v, want a provider error, not a timeout", err)
			}
		})
	}
}

// TestCampaignSources_TargetFailures covers the project-resolution guards on
// BOTH capabilities.
func TestCampaignSources_TargetFailures(t *testing.T) {
	tests := []struct {
		name   string
		api    API
		target workmgmt.Target
		want   string
	}{
		{"nil api", nil, glTarget(), "provider missing API client"},
		{"missing gitlab connection", &fakeAPI{}, workmgmt.Target{Repo: workmgmt.Repo{Owner: "acme", Name: "widgets"}}, "target gitlab connection required"},
		{"unresolvable project path", &fakeAPI{}, workmgmt.Target{GitLab: &workmgmt.GitLabConnection{}}, "no target project"},
		{"GetProject failure", &fakeAPI{getErr: errors.New("404 project")}, glTarget(), `resolve project "acme/widgets": 404 project`},
		{"no project id", &fakeAPI{project: &gitlabclient.Project{ID: 0}}, glTarget(), "no project id returned"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &Provider{api: tc.api}
			if _, err := p.ResolveDependencies(context.Background(), workmgmt.IssueSetRequest{Target: tc.target, Items: []string{"1"}}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ResolveDependencies err = %v, want %q", err, tc.want)
			}
			if _, err := p.EpicChildren(context.Background(), workmgmt.EpicChildrenRequest{Target: tc.target, Epic: "#100"}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("EpicChildren err = %v, want %q", err, tc.want)
			}
		})
	}
}

// --- epic mode ------------------------------------------------------------

// epicFixture: epic #100's relates_to links name #1 (no marker), #2 (marker
// naming THIS epic, trailing period as the filing paths write it), #3 (marker
// naming ANOTHER epic), #100 itself (defensive), a CROSS-PROJECT #4 in project
// 99 whose iid collides with a local issue, plus a non-relates_to link to #6.
// #1 is_blocked_by #2.
func epicFixture() *fakeAPI {
	api := &fakeAPI{}
	api.issue(100, "opened", "the epic", nil,
		relatesTo(1), relatesTo(2), relatesTo(3), relatesTo(100),
		foreign(4, 99, gitlabclient.LinkTypeRelatesTo),
		gitlabclient.IssueLink{IID: 6, ProjectID: 42, LinkType: gitlabclient.LinkTypeBlocks}).
		issue(1, "opened", "hand-filed child", nil, blockedBy(2)).
		issue(2, "closed", "## Summary\nParent epic: #100.\n", nil).
		issue(3, "opened", "Parent epic: #200\n", nil).
		issue(4, "opened", "an unrelated LOCAL #4", nil).
		issue(6, "opened", "", nil)
	return api
}

func epicChildren(t *testing.T, api *fakeAPI, epic string) (*workmgmt.EpicChildrenResult, error) {
	t.Helper()
	return New(api).EpicChildren(context.Background(), workmgmt.EpicChildrenRequest{Target: glTarget(), Epic: epic})
}

func TestEpicChildren_RelatesToWithParentMarker(t *testing.T) {
	api := epicFixture()
	res, err := epicChildren(t, api, "issue:100")
	if err != nil {
		t.Fatalf("EpicChildren: %v", err)
	}
	if got := childNums(res.Children); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Errorf("children = %v, want [1 2] (no marker + marker naming this epic)", got)
	}
	if got := edgeNums(res.Edges); !reflect.DeepEqual(got, [][2]int{{1, 2}}) {
		t.Errorf("edges = %v, want [[1 2]]", got)
	}
	if !res.Children[1].Complete || res.Children[1].State != "CLOSED" {
		t.Errorf("child #2 = %+v, want Complete/CLOSED", res.Children[1])
	}
	wantExcluded := []workmgmt.ExcludedCandidate{
		{Number: 3, Ref: "#3", Reason: workmgmt.ExcludeForeignParentMarker},
		{Number: 4, Ref: "project:99#4", Reason: workmgmt.ExcludeCrossProject},
	}
	if !reflect.DeepEqual(res.ExcludedCandidates, wantExcluded) {
		t.Errorf("excluded = %+v, want %+v", res.ExcludedCandidates, wantExcluded)
	}
	if len(res.DroppedEdges) != 0 {
		t.Errorf("dropped = %+v, want none (an excluded candidate is not a dependency)", res.DroppedEdges)
	}
	for _, iid := range []int{6, 100} {
		if n := api.getIssueCalls(iid); n != 0 {
			t.Errorf("GetIssue(#%d) = %d calls, want 0 (not a relates_to candidate)", iid, n)
		}
	}
}

// TestEpicChildren_CrossProjectCandidateNeverFetched is operator condition 1:
// a cross-project relates_to candidate is (a) absent from the children and (b)
// never read — the fake's GetIssue count for that iid is ZERO even though a
// local issue with the same iid exists and would be admitted (it has no
// marker) had the iid been reduced to a local number.
func TestEpicChildren_CrossProjectCandidateNeverFetched(t *testing.T) {
	api := epicFixture()
	res, err := epicChildren(t, api, "#100")
	if err != nil {
		t.Fatalf("EpicChildren: %v", err)
	}
	for _, c := range res.Children {
		if c.Number == 4 {
			t.Errorf("children contain #4 (%+v); a cross-project candidate must never become a child", c)
		}
	}
	if n := api.getIssueCalls(4); n != 0 {
		t.Errorf("GetIssue(#4) = %d calls, want 0 (a cross-project iid must never be read locally)", n)
	}
}

func TestParentMarkerAdmits(t *testing.T) {
	tests := []struct {
		body string
		want bool
	}{
		{"no marker at all", true},
		{"Parent epic: #100", true},
		{"parent EPIC:   100.", true},
		{"Parent epic: issue:100", true},
		{"Parent epic: #200", false},
		{"Parent epic: owner/repo#100", false},
		{"Parent epic: #200\nParent epic: #100", true},
		{"mentions Parent epic: #200 mid-line", true},
	}
	for _, tc := range tests {
		if got := parentMarkerAdmits(tc.body, 100); got != tc.want {
			t.Errorf("parentMarkerAdmits(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestEpicChildren_Failures(t *testing.T) {
	t.Run("unparseable epic ref", func(t *testing.T) {
		api := epicFixture()
		if _, err := epicChildren(t, api, "group/sub&5"); err == nil || !strings.Contains(err.Error(), `epic "group/sub&5"`) {
			t.Fatalf("err = %v, want the epic ref named", err)
		}
	})
	t.Run("epic link list error", func(t *testing.T) {
		api := epicFixture()
		api.linkErrs = map[int]error{100: errors.New("404 epic")}
		if _, err := epicChildren(t, api, "100"); err == nil || !strings.Contains(err.Error(), "list links of epic #100: 404 epic") {
			t.Fatalf("err = %v, want the epic link-list failure", err)
		}
	})
	t.Run("candidate fetch error fails closed", func(t *testing.T) {
		api := epicFixture()
		api.issueErrs = map[int]error{2: errors.New("boom")}
		if _, err := epicChildren(t, api, "100"); err == nil || !strings.Contains(err.Error(), "get issue #2: boom") {
			t.Fatalf("err = %v, want the candidate fetch failure", err)
		}
	})
}

func TestNormalizeState(t *testing.T) {
	for in, want := range map[string]string{"opened": "OPEN", "closed": "CLOSED", "CLOSED": "CLOSED", "locked": "", "": ""} {
		if got := normalizeState(in); got != want {
			t.Errorf("normalizeState(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- concurrency ------------------------------------------------------------

// TestResolveDependencies_PoolBound pins the worker pool STRUCTURALLY: with
// every GetIssue held open, the in-flight gauge reaches the bound and never
// exceeds it.
func TestResolveDependencies_PoolBound(t *testing.T) {
	api := &fakeAPI{hold: make(chan struct{})}
	var items []string
	for i := 1; i <= 20; i++ {
		api.issue(i, "opened", "", nil)
		items = append(items, fmt.Sprint(i))
	}
	done := make(chan error, 1)
	go func() {
		_, err := New(api).ResolveDependencies(context.Background(), workmgmt.IssueSetRequest{Target: glTarget(), Items: items})
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for api.peakInFlight() < issueSetFetchConcurrency && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // give an over-bound worker a chance to show
	if got := api.peakInFlight(); got != issueSetFetchConcurrency {
		t.Errorf("peak in-flight while held = %d, want exactly %d", got, issueSetFetchConcurrency)
	}
	close(api.hold)
	if err := <-done; err != nil {
		t.Fatalf("ResolveDependencies: %v", err)
	}
	if got := api.peakInFlight(); got > issueSetFetchConcurrency {
		t.Errorf("peak in-flight = %d, want <= %d", got, issueSetFetchConcurrency)
	}
}

// TestResolveDependencies_JitterDeterministic proves the emitted result is
// byte-identical regardless of worker completion order.
func TestResolveDependencies_JitterDeterministic(t *testing.T) {
	build := func() *fakeAPI {
		api := &fakeAPI{delay: func(int) time.Duration { return time.Duration(rand.IntN(2000)) * time.Microsecond }}
		for i := 1; i <= 12; i++ {
			var links []gitlabclient.IssueLink
			if i > 1 {
				links = append(links, blockedBy(i-1))
			}
			links = append(links, blockedBy(50+i%3), foreign(i, 99, gitlabclient.LinkTypeIsBlockedBy))
			api.issue(i, "opened", "", nil, links...)
		}
		api.issue(50, "closed", "", nil).issue(51, "opened", "", nil)
		api.issueErrs = map[int]error{52: errors.New("forbidden")}
		return api
	}
	items := []string{"7", "3", "12", "1", "9", "5", "11", "2", "8", "4", "10", "6"}
	var first string
	for rep := 0; rep < 50; rep++ {
		res, err := resolveItems(t, build(), items...)
		if err != nil {
			t.Fatalf("rep %d: %v", rep, err)
		}
		got := fmt.Sprintf("%#v", res)
		if rep == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("rep %d result differs:\n%s\nvs first:\n%s", rep, got, first)
		}
	}
}

// --- deadlines (#3113) --------------------------------------------------------

// TestResolveDependencies_DeadlineContract asserts the typed timeout is
// returned in PREFERENCE to a wrapped fetch error, with the documented counts,
// at each phase. The deadline is driven deterministically: the fake cancels
// the context from inside a specific GetIssue only after the named siblings'
// reads have completed.
func TestResolveDependencies_DeadlineContract(t *testing.T) {
	tests := []struct {
		name  string
		items []string
		setup func(t *testing.T, api *fakeAPI, cancel context.CancelFunc)
		want  workmgmt.IssueSetResolutionTimeout
	}{
		{
			name:  "fetch_items: resolved prefix is suggested",
			items: []string{"1", "2", "3"},
			setup: func(t *testing.T, api *fakeAPI, cancel context.CancelFunc) {
				api.onGetIssue = func(ctx context.Context, iid int) error {
					if iid == 3 {
						api.waitCompleted(t, 1, 2)
						cancel()
						return ctx.Err()
					}
					return nil
				}
			},
			want: workmgmt.IssueSetResolutionTimeout{Resolved: 2, Total: 3, SuggestedLimit: 2, Phase: "fetch_items"},
		},
		{
			name:  "fetch_items: non-prefix resolution suggests nothing",
			items: []string{"3", "1", "2"},
			setup: func(t *testing.T, api *fakeAPI, cancel context.CancelFunc) {
				api.onGetIssue = func(ctx context.Context, iid int) error {
					if iid == 3 {
						api.waitCompleted(t, 1, 2)
						cancel()
						return ctx.Err()
					}
					return nil
				}
			},
			want: workmgmt.IssueSetResolutionTimeout{Resolved: 2, Total: 3, SuggestedLimit: 0, Phase: "fetch_items"},
		},
		{
			name:  "classify_targets: an item with an unclassified target is not resolved",
			items: []string{"2", "1"},
			setup: func(t *testing.T, api *fakeAPI, cancel context.CancelFunc) {
				api.onGetIssue = func(ctx context.Context, iid int) error {
					if iid == 9 {
						cancel()
						return ctx.Err()
					}
					return nil
				}
			},
			want: workmgmt.IssueSetResolutionTimeout{Resolved: 1, Total: 2, SuggestedLimit: 1, Phase: "classify_targets"},
		},
		{
			name:  "build_result: a context-terminated target with a live context is never guessed",
			items: []string{"1", "2"},
			setup: func(t *testing.T, api *fakeAPI, cancel context.CancelFunc) {
				api.onGetIssue = func(ctx context.Context, iid int) error {
					if iid == 9 {
						return fmt.Errorf("transport: %w", context.DeadlineExceeded)
					}
					return nil
				}
			},
			want: workmgmt.IssueSetResolutionTimeout{Resolved: 1, Total: 2, SuggestedLimit: 0, Phase: "build_result"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAPI{}
			api.issue(1, "opened", "", nil, blockedBy(9)).
				issue(2, "opened", "", nil).
				issue(3, "opened", "", nil).
				issue(9, "closed", "", nil)
			if tc.want.Phase == "fetch_items" {
				// Phase-1 rows: no out-of-set targets, so a completed item is resolved.
				api.links[1] = nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tc.setup(t, api, cancel)

			res, err := New(api).ResolveDependencies(ctx, workmgmt.IssueSetRequest{Target: glTarget(), Items: tc.items})
			var to *workmgmt.IssueSetResolutionTimeout
			if !errors.As(err, &to) {
				t.Fatalf("ResolveDependencies = %+v, %v (%T); want *workmgmt.IssueSetResolutionTimeout", res, err, err)
			}
			if *to != tc.want {
				t.Errorf("timeout = %+v, want %+v", *to, tc.want)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("errors.Is(err, DeadlineExceeded) = false, want true")
			}
		})
	}
}
