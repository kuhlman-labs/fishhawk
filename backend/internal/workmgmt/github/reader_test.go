package github

// Tests for the optional workmgmt.WorkItemReader capability (#2230 / ADR-064).
//
// Structure: one cross-boundary integration test driving a REAL
// *githubclient.Client over httptest (transport decode + provider mapping in
// ONE assertion), the acceptance-criterion-3 filter tests, the ReadWorkItem
// ref/board tests, and ONE behavioral test per enumerated failure mode — each
// asserting errors.As yields *workmgmt.UnavailableError with the EXACT Reason
// AND that the returned result is nil, never an empty page or a zero-valued
// record a caller would read as a real answer.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// readerTarget is the canonical read target: the fishhawk repo on the
// USER-owned Project #7 board (the #1114 shape).
func readerTarget() workmgmt.Target {
	return workmgmt.Target{
		Scope:   forge.FromGitHubInstallationID(99),
		Repo:    workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"},
		Project: &workmgmt.Project{Owner: "kuhlman-labs", OwnerType: "user", Number: 7},
	}
}

// readerAPI returns a fakeAPI primed for a board-resolving read: the project
// fields resolve, and a projects token IS configured (the no-token degradation
// has its own test, which seeds the false BY CONSTRUCTION).
func readerAPI() *fakeAPI {
	return &fakeAPI{
		parentNode:              "ISSUE_NODE",
		meta:                    &githubclient.ProjectMeta{ProjectID: "PROJ", FieldID: "FIELD", StatusOptions: map[string]string{"Backlog": "OPT_B", "In Progress": "OPT_IP"}},
		projectsTokenConfigured: true,
	}
}

// listRequest is a board-resolving list over the canonical target.
func listRequest() workmgmt.ListWorkItemsRequest {
	return workmgmt.ListWorkItemsRequest{
		Target:            readerTarget(),
		ResolveBoardState: true,
		States:            canonicalStates,
	}
}

// ---------------------------------------------------------------------------
// Cross-boundary integration (transport + provider in one assertion)
// ---------------------------------------------------------------------------

// newRepoIssuesClient builds a real *githubclient.Client whose GraphQL
// endpoint serves the repository.issues connection as two cursor pages
// totalling `total` issues, plus the ProjectFields lookup. It records the
// documents and variables so the enumeration can be asserted at the wire.
func newRepoIssuesClient(t *testing.T, total int, projectsToken string) (*githubclient.Client, *repoIssuesFixture) {
	t.Helper()
	fx := &repoIssuesFixture{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch {
		case strings.Contains(body.Query, "ProjectFields"):
			fx.projectFieldsAuth = r.Header.Get("Authorization")
			_, _ = io.WriteString(w, `{"data":{"user":{"projectV2":{"id":"PROJ","field":{"id":"FIELD","options":[{"id":"OPT_B","name":"Backlog"},{"id":"OPT_IP","name":"In Progress"}]}}}}}`)
		case strings.Contains(body.Query, "ListRepoIssues"):
			fx.listAuth = r.Header.Get("Authorization")
			fx.listQuery = body.Query
			fx.listVars = body.Variables
			fx.listReqs++
			after, _ := body.Variables["after"].(string)
			lo, hi := 1, 100
			hasNext := total > 100
			if after == "C1" {
				lo, hi, hasNext = 101, total, false
			}
			var nodes []string
			for n := lo; n <= hi && n <= total; n++ {
				// Every third issue sits on the board In Progress; the rest Backlog.
				status := "Backlog"
				if n%3 == 0 {
					status = "In Progress"
				}
				nodes = append(nodes, fmt.Sprintf(
					`{"number":%d,"title":"issue %d","url":"https://github.com/kuhlman-labs/fishhawk/issues/%d","body":"b%d","state":"OPEN","stateReason":null,"labels":{"nodes":[{"name":"type:feature"}]},"projectItems":{"nodes":[{"project":{"id":"PROJ"},"fieldValueByName":{"name":%q}}]}}`,
					n, n, n, n, status))
			}
			_, _ = io.WriteString(w, fmt.Sprintf(`{"data":{"repository":{"issues":{"pageInfo":{"hasNextPage":%t,"endCursor":"C1"},"nodes":[%s]}}}}`,
				hasNext, strings.Join(nodes, ",")))
		default:
			_, _ = io.WriteString(w, `{"data":{}}`)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := githubclient.New(stubTokenProvider{token: "ghs_install"})
	c.BaseURL = srv.URL
	c.HTTP = &http.Client{Timeout: 5 * time.Second}
	c.ProjectsToken = projectsToken
	return c, fx
}

type repoIssuesFixture struct {
	listQuery         string
	listVars          map[string]any
	listReqs          int
	listAuth          string
	projectFieldsAuth string
}

// TestProvider_ListWorkItems_EndToEndOverHTTP is the mandatory cross-boundary
// integration test: the provider drives a REAL *githubclient.Client against an
// httptest GraphQL mux, so the transport decode, the provider's mapping, and
// the workmgmt result vocabulary are asserted in ONE test. Per-layer units
// alone would pass while the GraphQL-node-to-WorkItemRecord seam broke (#618).
func TestProvider_ListWorkItems_EndToEndOverHTTP(t *testing.T) {
	c, fx := newRepoIssuesClient(t, 3, "pat_projects")
	page, err := New(c).ListWorkItems(context.Background(), listRequest())
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(page.Items))
	}
	if !page.BoardStateResolved {
		t.Error("BoardStateResolved = false, want true (board state was requested)")
	}
	got := page.Items[2]
	want := workmgmt.WorkItemRecord{
		Number: 3, Title: "issue 3", URL: "https://github.com/kuhlman-labs/fishhawk/issues/3",
		Body: "b3", Labels: []string{"type:feature"}, State: "OPEN",
		OnBoard: true, BoardColumn: "In Progress", BoardState: workmgmt.CanonicalStateInProgress,
	}
	if got.Number != want.Number || got.Title != want.Title || got.URL != want.URL || got.Body != want.Body ||
		got.State != want.State || got.OnBoard != want.OnBoard || got.BoardColumn != want.BoardColumn ||
		got.BoardState != want.BoardState || len(got.Labels) != 1 || got.Labels[0] != "type:feature" {
		t.Errorf("record = %+v\nwant   = %+v", got, want)
	}
	// A user-owned board routes BOTH board-touching calls onto the projects PAT.
	if fx.projectFieldsAuth != "Bearer pat_projects" || fx.listAuth != "Bearer pat_projects" {
		t.Errorf("auth = fields %q / list %q, want the projects token on both (#1114)", fx.projectFieldsAuth, fx.listAuth)
	}
	// Open-only by default: the states filter reached the wire.
	states, _ := fx.listVars["states"].([]any)
	if len(states) != 1 || states[0] != "OPEN" {
		t.Errorf("states variable = %v, want [OPEN] (IncludeClosed defaults false)", fx.listVars["states"])
	}
}

// TestProvider_ListWorkItems_ReturnsBeyondBoardItemListCap replays the
// acceptance-criterion-2 corpus at the CONSUMER surface: 150 issues across two
// cursor pages must all reach the caller. A board-item-list enumeration (or a
// one-page read) would cap at 100 — the silent truncation this issue exists to
// eliminate.
func TestProvider_ListWorkItems_ReturnsBeyondBoardItemListCap(t *testing.T) {
	c, fx := newRepoIssuesClient(t, 150, "pat_projects")
	page, err := New(c).ListWorkItems(context.Background(), listRequest())
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(page.Items) != 150 {
		t.Fatalf("items = %d, want 150 (a truncating read returns 100)", len(page.Items))
	}
	for i, it := range page.Items {
		if it.Number != i+1 {
			t.Fatalf("items[%d].Number = %d, want %d (ascending)", i, it.Number, i+1)
		}
	}
	if fx.listReqs != 2 {
		t.Errorf("enumeration requests = %d, want 2 (cursor walk)", fx.listReqs)
	}
	if strings.Contains(strings.ToLower(fx.listQuery), "projectv2(") {
		t.Errorf("provider enumerated a ProjectV2 board item list:\n%s", fx.listQuery)
	}
}

// TestProvider_ListWorkItems_LimitCapsAfterFiltering proves Limit is a caller
// cap applied to the FILTERED set, not to the enumerated one.
//
// The corpus is ORDERING-DISCRIMINATING by construction: the first two
// enumerated issues FAIL the board-state filter and the next three PASS it, so
// a Limit of 2 yields #3 and #4 under filter-then-limit and an EMPTY page
// under limit-then-filter. A no-filter fixture would pass under either
// ordering and leave the contract unpinned.
func TestProvider_ListWorkItems_LimitCapsAfterFiltering(t *testing.T) {
	api := readerAPI()
	api.listRepoIssues = []githubclient.RepoIssue{
		{Number: 1, State: "OPEN", OnBoard: true, BoardStatus: "Backlog"},
		{Number: 2, State: "OPEN", OnBoard: true, BoardStatus: "Backlog"},
		{Number: 3, State: "OPEN", OnBoard: true, BoardStatus: "In Progress"},
		{Number: 4, State: "OPEN", OnBoard: true, BoardStatus: "In Progress"},
		{Number: 5, State: "OPEN", OnBoard: true, BoardStatus: "In Progress"},
	}
	req := listRequest()
	req.BoardStates = []string{workmgmt.CanonicalStateInProgress}
	req.Limit = 2
	page, err := New(api).ListWorkItems(context.Background(), req)
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(page.Items) != 2 || page.Items[0].Number != 3 || page.Items[1].Number != 4 {
		t.Fatalf("items = %+v, want exactly #3 and #4 — an empty page means the limit was applied BEFORE the filter", page.Items)
	}

	// Limit 0 means no cap: the whole filtered set comes back.
	req.Limit = 0
	all, err := New(api).ListWorkItems(context.Background(), req)
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(all.Items) != 3 {
		t.Fatalf("uncapped items = %d, want 3 (Limit 0 is no cap)", len(all.Items))
	}
}

// ---------------------------------------------------------------------------
// Acceptance criterion 3: the two filters
// ---------------------------------------------------------------------------

// TestProvider_ListWorkItems_ForwardsLabelFilter proves the caller's labels
// reach the client verbatim (GitHub AND-s them server-side — see the README
// note that the FORWARDING is tested here but the AND semantics are not, since
// an httptest fixture cannot prove server behaviour).
func TestProvider_ListWorkItems_ForwardsLabelFilter(t *testing.T) {
	api := readerAPI()
	req := listRequest()
	req.Labels = []string{"type:feature", "area:backend"}
	req.IncludeClosed = true
	if _, err := New(api).ListWorkItems(context.Background(), req); err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	got := api.listRepoIssuesOpts
	if len(got.Labels) != 2 || got.Labels[0] != "type:feature" || got.Labels[1] != "area:backend" {
		t.Errorf("forwarded labels = %v, want the caller's filter verbatim", got.Labels)
	}
	if len(got.States) != 0 {
		t.Errorf("states = %v, want unfiltered when IncludeClosed is set", got.States)
	}
	if !got.WantBoardStatus || got.ProjectID != "PROJ" {
		t.Errorf("board opts = %v/%q, want the resolved project node id", got.WantBoardStatus, got.ProjectID)
	}
	if api.listRepoIssuesRepo.Owner != "kuhlman-labs" || api.listRepoIssuesRepo.Name != "fishhawk" {
		t.Errorf("repo = %+v, want the target repo", api.listRepoIssuesRepo)
	}
	// A user-owned board opts the enumeration into the projects token.
	if !api.listRepoIssuesProjectsToken {
		t.Error("enumeration context did not carry the projects-token opt-in for a user-owned board")
	}
}

// TestProvider_ListWorkItems_FiltersByCanonicalBoardState is the board-state
// post-filter: a 5-item corpus spanning Backlog / In Progress / an UNMAPPED
// provider option / off-board. A BoardStates=[in_progress] query returns
// EXACTLY the In Progress items; the unmapped and off-board items are excluded
// and their BoardState is empty rather than guessed.
func TestProvider_ListWorkItems_FiltersByCanonicalBoardState(t *testing.T) {
	api := readerAPI()
	api.listRepoIssues = []githubclient.RepoIssue{
		{Number: 1, State: "OPEN", OnBoard: true, BoardStatus: "Backlog"},
		{Number: 2, State: "OPEN", OnBoard: true, BoardStatus: "In Progress"},
		{Number: 3, State: "OPEN", OnBoard: true, BoardStatus: "Icebox"}, // maps to no canonical state
		{Number: 4, State: "OPEN", OnBoard: false},                       // off-board
		{Number: 5, State: "OPEN", OnBoard: true, BoardStatus: "In Progress"},
	}

	// Unfiltered first: every item comes back, and the unmapped / off-board
	// items report an EMPTY BoardState rather than a guessed one.
	all, err := New(api).ListWorkItems(context.Background(), listRequest())
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(all.Items) != 5 {
		t.Fatalf("unfiltered items = %d, want 5", len(all.Items))
	}
	if all.Items[2].BoardState != "" || all.Items[2].BoardColumn != "Icebox" {
		t.Errorf("unmapped item = %+v, want an empty BoardState with the raw column preserved", all.Items[2])
	}
	if all.Items[3].BoardState != "" || all.Items[3].OnBoard {
		t.Errorf("off-board item = %+v, want off-board with an empty BoardState", all.Items[3])
	}

	req := listRequest()
	req.BoardStates = []string{workmgmt.CanonicalStateInProgress}
	page, err := New(api).ListWorkItems(context.Background(), req)
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(page.Items) != 2 || page.Items[0].Number != 2 || page.Items[1].Number != 5 {
		t.Fatalf("filtered items = %+v, want exactly #2 and #5 (In Progress)", page.Items)
	}
	for _, it := range page.Items {
		if it.BoardState != workmgmt.CanonicalStateInProgress {
			t.Errorf("item #%d BoardState = %q, want %q", it.Number, it.BoardState, workmgmt.CanonicalStateInProgress)
		}
	}
}

// TestProvider_ListWorkItems_MapsCompletionFromGraphQLEnums proves the
// closed-AND-completed rule on the list path's UPPERCASE GraphQL enums: a
// not_planned close is NOT complete.
func TestProvider_ListWorkItems_MapsCompletionFromGraphQLEnums(t *testing.T) {
	api := readerAPI()
	api.listRepoIssues = []githubclient.RepoIssue{
		{Number: 1, State: "CLOSED", StateReason: "COMPLETED"},
		{Number: 2, State: "CLOSED", StateReason: "NOT_PLANNED"},
		{Number: 3, State: "OPEN"},
	}
	req := listRequest()
	req.ResolveBoardState = false
	req.Target.Project = nil // board state not requested, so no project is needed
	req.IncludeClosed = true
	page, err := New(api).ListWorkItems(context.Background(), req)
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if page.BoardStateResolved {
		t.Error("BoardStateResolved = true, want false when board state was not requested")
	}
	want := []bool{true, false, false}
	for i, w := range want {
		if page.Items[i].Complete != w {
			t.Errorf("item #%d Complete = %v, want %v", page.Items[i].Number, page.Items[i].Complete, w)
		}
	}
}

// ---------------------------------------------------------------------------
// ReadWorkItem
// ---------------------------------------------------------------------------

// TestProvider_ReadWorkItem_AcceptsEveryRefForm proves the three reference
// conventions (`#N`, `N`, `issue:N`) all resolve to the same issue through the
// SAME parseIssueRef helper ResolveDependencies uses.
func TestProvider_ReadWorkItem_AcceptsEveryRefForm(t *testing.T) {
	for _, ref := range []string{"#2230", "2230", "issue:2230", " issue:2230 "} {
		api := readerAPI()
		api.getIssues = map[int]*githubclient.Issue{2230: {
			Number: 2230, Title: "grow the abstraction", Body: "body",
			State: "closed", StateReason: "completed", Labels: []string{"type:feature"},
		}}
		rec, err := New(api).ReadWorkItem(context.Background(), workmgmt.ReadWorkItemRequest{
			Target: readerTarget(), Ref: ref, States: canonicalStates,
		})
		if err != nil {
			t.Fatalf("ReadWorkItem(%q): %v", ref, err)
		}
		if rec.Number != 2230 || rec.Title != "grow the abstraction" {
			t.Errorf("ReadWorkItem(%q) = %+v, want issue 2230", ref, rec)
		}
		// REST payloads are lowercase; completion matches case-insensitively.
		if !rec.Complete {
			t.Errorf("ReadWorkItem(%q).Complete = false, want true for closed+completed", ref)
		}
		// Board state was NOT requested, so no board call was made and the
		// record's board fields stay zero (unambiguously "not asked").
		if rec.OnBoard || rec.BoardState != "" {
			t.Errorf("ReadWorkItem(%q) board = %v/%q, want unresolved", ref, rec.OnBoard, rec.BoardState)
		}
	}
}

// TestProvider_ReadWorkItem_MalformedRefRejected proves a non-numeric ref is a
// hard error naming the ref, not a zero-valued record.
func TestProvider_ReadWorkItem_MalformedRefRejected(t *testing.T) {
	rec, err := New(readerAPI()).ReadWorkItem(context.Background(), workmgmt.ReadWorkItemRequest{
		Target: readerTarget(), Ref: "not-a-number",
	})
	if err == nil || rec != nil {
		t.Fatalf("want an error and a nil record, got %+v / %v", rec, err)
	}
	if !strings.Contains(err.Error(), "not-a-number") {
		t.Errorf("error should name the ref, got %v", err)
	}
}

// TestProvider_ReadWorkItem_ResolvesBoardState proves the opt-in board read:
// the issue's project item supplies the column, reverse-mapped to a canonical
// state through the request's States.
func TestProvider_ReadWorkItem_ResolvesBoardState(t *testing.T) {
	api := readerAPI()
	api.getIssues = map[int]*githubclient.Issue{2230: {Number: 2230, Title: "t", State: "open"}}
	api.itemStatus = &githubclient.ProjectItemStatus{OnBoard: true, ItemID: "ITEM", Status: "In Progress"}
	rec, err := New(api).ReadWorkItem(context.Background(), workmgmt.ReadWorkItemRequest{
		Target: readerTarget(), Ref: "#2230", ResolveBoardState: true, States: canonicalStates,
	})
	if err != nil {
		t.Fatalf("ReadWorkItem: %v", err)
	}
	if !rec.OnBoard || rec.BoardColumn != "In Progress" || rec.BoardState != workmgmt.CanonicalStateInProgress {
		t.Errorf("record board = %v/%q/%q, want on-board In Progress -> in_progress", rec.OnBoard, rec.BoardColumn, rec.BoardState)
	}
	if api.itemStatusProjectID != "PROJ" || api.itemStatusIssueNode != "ISSUE_NODE" {
		t.Errorf("project-item lookup used %q/%q, want the resolved node and project ids", api.itemStatusIssueNode, api.itemStatusProjectID)
	}
}

// TestProvider_ReadWorkItem_OffBoardLeavesStateEmpty proves an off-board item
// reports an empty BoardState rather than a guessed one.
func TestProvider_ReadWorkItem_OffBoardLeavesStateEmpty(t *testing.T) {
	api := readerAPI()
	api.getIssues = map[int]*githubclient.Issue{7: {Number: 7, Title: "t", State: "open"}}
	api.itemStatus = &githubclient.ProjectItemStatus{OnBoard: false}
	rec, err := New(api).ReadWorkItem(context.Background(), workmgmt.ReadWorkItemRequest{
		Target: readerTarget(), Ref: "7", ResolveBoardState: true, States: canonicalStates,
	})
	if err != nil {
		t.Fatalf("ReadWorkItem: %v", err)
	}
	if rec.OnBoard || rec.BoardState != "" || rec.BoardColumn != "" {
		t.Errorf("record = %+v, want off-board with no board state", rec)
	}
}

// ---------------------------------------------------------------------------
// Failure modes (one behavioral test per enumerated degradation, #1182)
// Every one asserts errors.As -> *workmgmt.UnavailableError with the EXACT
// Reason AND a nil result — never an empty page or a zero-valued record.
// ---------------------------------------------------------------------------

// assertUnavailable is the shared assertion: the error is a typed
// *workmgmt.UnavailableError carrying want, and it names this provider.
func assertUnavailable(t *testing.T, err error, want workmgmt.UnavailableReason) *workmgmt.UnavailableError {
	t.Helper()
	var ue *workmgmt.UnavailableError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v (%T), want *workmgmt.UnavailableError", err, err)
	}
	if ue.Reason != want {
		t.Fatalf("Reason = %q, want %q (err: %v)", ue.Reason, want, err)
	}
	if ue.Provider != ProviderName || ue.Capability != workmgmt.ReaderCapability {
		t.Errorf("error names %q/%q, want %q/%q", ue.Provider, ue.Capability, ProviderName, workmgmt.ReaderCapability)
	}
	if ue.Detail == "" {
		t.Errorf("Reason %q carries no operator-facing detail", want)
	}
	return ue
}

// TestListWorkItems_ZeroScopeFailsClosed — mode 1: no installation scope.
func TestListWorkItems_ZeroScopeFailsClosed(t *testing.T) {
	req := listRequest()
	req.Target.Scope = forge.CredentialScope{}
	page, err := New(readerAPI()).ListWorkItems(context.Background(), req)
	if page != nil {
		t.Errorf("page = %+v, want NIL — an empty page reads as an empty backlog", page)
	}
	assertUnavailable(t, err, workmgmt.ReasonNoInstallation)
}

// TestReadWorkItem_ZeroScopeFailsClosed — mode 1 on the READ path (C4).
func TestReadWorkItem_ZeroScopeFailsClosed(t *testing.T) {
	rec, err := New(readerAPI()).ReadWorkItem(context.Background(), workmgmt.ReadWorkItemRequest{
		Target: workmgmt.Target{Repo: workmgmt.Repo{Owner: "o", Name: "r"}}, Ref: "#7",
	})
	if rec != nil {
		t.Errorf("record = %+v, want NIL — a zero-valued record reads as a real item", rec)
	}
	assertUnavailable(t, err, workmgmt.ReasonNoInstallation)
}

// TestListWorkItems_NoProjectConfigured — mode 2: board state requested with
// no project connection on the target.
func TestListWorkItems_NoProjectConfigured(t *testing.T) {
	req := listRequest()
	req.Target.Project = nil
	req.BoardStates = []string{workmgmt.CanonicalStateInProgress}
	page, err := New(readerAPI()).ListWorkItems(context.Background(), req)
	if page != nil {
		t.Errorf("page = %+v, want NIL", page)
	}
	assertUnavailable(t, err, workmgmt.ReasonNoProjectConfigured)
}

// TestReadWorkItem_NoProjectConfigured — mode 2 on the READ path (C4).
func TestReadWorkItem_NoProjectConfigured(t *testing.T) {
	target := readerTarget()
	target.Project = nil
	rec, err := New(readerAPI()).ReadWorkItem(context.Background(), workmgmt.ReadWorkItemRequest{
		Target: target, Ref: "#7", ResolveBoardState: true, States: canonicalStates,
	})
	if rec != nil {
		t.Errorf("record = %+v, want NIL", rec)
	}
	assertUnavailable(t, err, workmgmt.ReasonNoProjectConfigured)
}

// TestListWorkItems_UserOwnedBoardNoProjectsToken — mode 3, the #1114 case.
// ProjectsTokenConfigured() is seeded false BY CONSTRUCTION on the fake (not
// via the control under test), so deleting the guard lands the RED on the
// behavioral assertion rather than on fixture setup.
func TestListWorkItems_UserOwnedBoardNoProjectsToken(t *testing.T) {
	api := readerAPI()
	api.projectsTokenConfigured = false
	page, err := New(api).ListWorkItems(context.Background(), listRequest())
	if page != nil {
		t.Errorf("page = %+v, want NIL — a user-owned board with no token is unreachable, not empty", page)
	}
	ue := assertUnavailable(t, err, workmgmt.ReasonNoProjectsToken)
	if !strings.Contains(ue.Detail, "FISHHAWKD_PROJECTS_TOKEN") {
		t.Errorf("detail %q should name the remedy env var", ue.Detail)
	}
}

// TestReadWorkItem_UserOwnedBoardNoProjectsToken — mode 3 on the READ path (C4).
func TestReadWorkItem_UserOwnedBoardNoProjectsToken(t *testing.T) {
	api := readerAPI()
	api.projectsTokenConfigured = false
	rec, err := New(api).ReadWorkItem(context.Background(), workmgmt.ReadWorkItemRequest{
		Target: readerTarget(), Ref: "#7", ResolveBoardState: true, States: canonicalStates,
	})
	if rec != nil {
		t.Errorf("record = %+v, want NIL", rec)
	}
	assertUnavailable(t, err, workmgmt.ReasonNoProjectsToken)
}

// TestListWorkItems_ForbiddenFromClient — mode 4: the forge refuses the
// enumeration. The provider drives a REAL client against an httptest server
// returning 403, so githubclient's ErrForbidden classification is exercised
// rather than a hand-made sentinel; the wrapper must carry Reason
// ReasonForbidden AND keep errors.Is(err, githubclient.ErrForbidden) true on
// the same value (condition C2).
func TestListWorkItems_ForbiddenFromClient(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.Contains(body.Query, "ProjectFields") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"data":{"user":{"projectV2":{"id":"PROJ","field":{"id":"FIELD","options":[]}}}}}`)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"Resource not accessible by integration"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := githubclient.New(stubTokenProvider{token: "ghs_install"})
	c.BaseURL = srv.URL
	c.HTTP = &http.Client{Timeout: 5 * time.Second}
	c.ProjectsToken = "pat_projects"

	page, err := New(c).ListWorkItems(context.Background(), listRequest())
	if page != nil {
		t.Errorf("page = %+v, want NIL — a permissions refusal is not an empty backlog", page)
	}
	assertUnavailable(t, err, workmgmt.ReasonForbidden)
	// C2: the SAME value must still match the forge sentinel through Unwrap.
	if !errors.Is(err, githubclient.ErrForbidden) {
		t.Errorf("errors.Is(err, githubclient.ErrForbidden) = false; the typed wrapper dropped the cause: %v", err)
	}
}

// TestReadWorkItem_ForbiddenFromClient — mode 4 on the READ path (C4), driven
// through a real client whose REST issue read returns 403.
func TestReadWorkItem_ForbiddenFromClient(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/{number}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"Resource not accessible by integration"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := githubclient.New(stubTokenProvider{token: "ghs_install"})
	c.BaseURL = srv.URL
	c.HTTP = &http.Client{Timeout: 5 * time.Second}

	rec, err := New(c).ReadWorkItem(context.Background(), workmgmt.ReadWorkItemRequest{
		Target: readerTarget(), Ref: "#2230",
	})
	if rec != nil {
		t.Errorf("record = %+v, want NIL", rec)
	}
	assertUnavailable(t, err, workmgmt.ReasonForbidden)
	if !errors.Is(err, githubclient.ErrForbidden) {
		t.Errorf("errors.Is(err, githubclient.ErrForbidden) = false through the typed wrapper: %v", err)
	}
}

// TestReadWorkItem_ForbiddenFromProjectItemRead covers the third read-path
// forbidden branch: the issue read succeeds but the PROJECT-ITEM read is
// refused. It must still be a typed ReasonForbidden with a nil record, not a
// record carrying a silently-absent board state.
func TestReadWorkItem_ForbiddenFromProjectItemRead(t *testing.T) {
	api := readerAPI()
	api.getIssues = map[int]*githubclient.Issue{7: {Number: 7, Title: "t", State: "open"}}
	api.itemStatusErr = fmt.Errorf("read item: %w", githubclient.ErrForbidden)
	rec, err := New(api).ReadWorkItem(context.Background(), workmgmt.ReadWorkItemRequest{
		Target: readerTarget(), Ref: "7", ResolveBoardState: true, States: canonicalStates,
	})
	if rec != nil {
		t.Errorf("record = %+v, want NIL", rec)
	}
	assertUnavailable(t, err, workmgmt.ReasonForbidden)
	if !errors.Is(err, githubclient.ErrForbidden) {
		t.Errorf("errors.Is lost the sentinel: %v", err)
	}
}

// TestReadWorkItem_ForbiddenFromNodeIDRead covers the remaining read-path
// forbidden branch: the node-id resolution is refused.
func TestReadWorkItem_ForbiddenFromNodeIDRead(t *testing.T) {
	api := readerAPI()
	api.getIssues = map[int]*githubclient.Issue{7: {Number: 7, Title: "t", State: "open"}}
	api.nodeIDErr = fmt.Errorf("resolve node: %w", githubclient.ErrForbidden)
	rec, err := New(api).ReadWorkItem(context.Background(), workmgmt.ReadWorkItemRequest{
		Target: readerTarget(), Ref: "7", ResolveBoardState: true, States: canonicalStates,
	})
	if rec != nil {
		t.Errorf("record = %+v, want NIL", rec)
	}
	assertUnavailable(t, err, workmgmt.ReasonForbidden)
}

// TestResolveBoard_ForbiddenFromProjectFields covers the shared board
// precondition's forbidden branch (both paths reach it).
func TestResolveBoard_ForbiddenFromProjectFields(t *testing.T) {
	api := readerAPI()
	api.fieldsErr = fmt.Errorf("resolve fields: %w", githubclient.ErrForbidden)
	page, err := New(api).ListWorkItems(context.Background(), listRequest())
	if page != nil {
		t.Errorf("page = %+v, want NIL", page)
	}
	assertUnavailable(t, err, workmgmt.ReasonForbidden)
	if !errors.Is(err, githubclient.ErrForbidden) {
		t.Errorf("errors.Is lost the sentinel: %v", err)
	}
}

// TestReader_NonForbiddenErrorsStayPlain proves the typed wrapper is NOT
// over-broad: a non-permissions provider failure propagates as a plain wrapped
// error, so a caller does not mistake a transport fault for a capability
// degradation it should prompt the operator about.
func TestReader_NonForbiddenErrorsStayPlain(t *testing.T) {
	api := readerAPI()
	api.listRepoIssuesErr = errors.New("boom")
	page, err := New(api).ListWorkItems(context.Background(), listRequest())
	if page != nil || err == nil {
		t.Fatalf("want a nil page and an error, got %+v / %v", page, err)
	}
	var ue *workmgmt.UnavailableError
	if errors.As(err, &ue) {
		t.Errorf("a transport fault must not masquerade as a capability degradation: %v", err)
	}

	api2 := readerAPI()
	api2.getIssueErr = errors.New("boom")
	rec, err := New(api2).ReadWorkItem(context.Background(), workmgmt.ReadWorkItemRequest{
		Target: readerTarget(), Ref: "7",
	})
	if rec != nil || err == nil {
		t.Fatalf("want a nil record and an error, got %+v / %v", rec, err)
	}
	if errors.As(err, &ue) {
		t.Errorf("a transport fault must not masquerade as a capability degradation: %v", err)
	}

	// The shared board precondition has the same non-forbidden branch: a plain
	// ProjectFields failure stays a plain wrapped error on BOTH paths.
	api3 := readerAPI()
	api3.fieldsErr = errors.New("boom")
	page3, err := New(api3).ListWorkItems(context.Background(), listRequest())
	if page3 != nil || err == nil {
		t.Fatalf("want a nil page and an error, got %+v / %v", page3, err)
	}
	if errors.As(err, &ue) {
		t.Errorf("a plain project-fields fault must not masquerade as a capability degradation: %v", err)
	}
	api4 := readerAPI()
	api4.fieldsErr = errors.New("boom")
	rec4, err := New(api4).ReadWorkItem(context.Background(), workmgmt.ReadWorkItemRequest{
		Target: readerTarget(), Ref: "7", ResolveBoardState: true, States: canonicalStates,
	})
	if rec4 != nil || err == nil {
		t.Fatalf("want a nil record and an error, got %+v / %v", rec4, err)
	}
	if errors.As(err, &ue) {
		t.Errorf("a plain project-fields fault must not masquerade as a capability degradation: %v", err)
	}
}

// TestReader_MissingAPIAndRepoRejected pins the two programming-error guards
// both entry points share.
func TestReader_MissingAPIAndRepoRejected(t *testing.T) {
	if _, err := New(nil).ListWorkItems(context.Background(), listRequest()); err == nil {
		t.Error("want an error for a nil API client on ListWorkItems")
	}
	if _, err := New(nil).ReadWorkItem(context.Background(), workmgmt.ReadWorkItemRequest{Ref: "1"}); err == nil {
		t.Error("want an error for a nil API client on ReadWorkItem")
	}
	req := listRequest()
	req.Target.Repo = workmgmt.Repo{Owner: "kuhlman-labs"}
	if _, err := New(readerAPI()).ListWorkItems(context.Background(), req); err == nil {
		t.Error("want an error for a missing repo name on ListWorkItems")
	}
	if _, err := New(readerAPI()).ReadWorkItem(context.Background(), workmgmt.ReadWorkItemRequest{
		Target: workmgmt.Target{Scope: forge.FromGitHubInstallationID(1)}, Ref: "1",
	}); err == nil {
		t.Error("want an error for a missing repo on ReadWorkItem")
	}
}

// readerRegistryName is a unique test-only registry id. workmgmt.Register
// REPLACES any prior registration for a name, so registering a fake-backed
// provider under the real ProviderName would clobber the production
// registration for every other test sharing this package's test binary (the
// same clobber refinement_file_test.go avoids with its own named wrapper).
// ReaderFor only needs an id that maps to a WorkItemReader.
const readerRegistryName = ProviderName + "_reader_capability_test"

// namedReaderProvider wraps the real provider under readerRegistryName. The
// embedded *Provider promotes ListWorkItems/ReadWorkItem, so the
// WorkItemReader type assertion inside ReaderFor still resolves.
type namedReaderProvider struct{ *Provider }

func (*namedReaderProvider) Name() string { return readerRegistryName }

// TestProvider_ReaderFor_ResolvesGitHubProvider proves the registry chokepoint
// resolves THIS provider to a usable reader — the positive counterpart to the
// gitlab not-implemented assertion.
func TestProvider_ReaderFor_ResolvesGitHubProvider(t *testing.T) {
	workmgmt.Register(&namedReaderProvider{Provider: New(readerAPI())})
	r, err := workmgmt.ReaderFor(readerRegistryName)
	if err != nil || r == nil {
		t.Fatalf("ReaderFor(%q) = %v / %v, want a usable reader", readerRegistryName, r, err)
	}
}
