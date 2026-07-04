package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// stubTokenProvider mints a fixed installation token so the cross-boundary
// test can distinguish it from the static projects PAT by Authorization
// header value.
type stubTokenProvider struct{ token string }

func (s stubTokenProvider) Token(_ context.Context, _ int64) (string, error) {
	return s.token, nil
}

// fakeAPI records calls and returns canned results so the provider's
// orchestration can be asserted without the wire.
type fakeAPI struct {
	createParams githubclient.CreateIssueParams
	createRepo   githubclient.RepoRef
	createErr    error
	created      *githubclient.CreatedIssue

	fieldsCoord githubclient.ProjectCoord
	fieldsName  string
	fieldsErr   error
	meta        *githubclient.ProjectMeta

	itemStatus          *githubclient.ProjectItemStatus
	itemStatusErr       error
	itemStatusIssueNode string
	itemStatusProjectID string

	addItemContent string
	addItemErr     error
	itemID         string

	setProjectID, setItemID, setFieldID, setOptionID string
	setErr                                           error

	nodeIDNumber int
	nodeIDErr    error
	parentNode   string

	subParent, subChild string
	subErr              error

	listSubParent  string
	listSubResults []githubclient.SubIssue
	listSubErr     error

	searchQuery   string
	searchResults []githubclient.IssueTitleResult
	// searchResultsFn, when set, computes the results from the composed query so
	// a test can return different result sets depending on whether the query
	// carries a label: qualifier (the #1522 recency-burial regression guard). It
	// takes precedence over searchResults.
	searchResultsFn func(query string) []githubclient.IssueTitleResult
	searchErr       error

	projectsTokenConfigured bool
}

func (f *fakeAPI) CreateIssue(_ context.Context, _ int64, repo githubclient.RepoRef, p githubclient.CreateIssueParams) (*githubclient.CreatedIssue, error) {
	f.createRepo, f.createParams = repo, p
	if f.createErr != nil {
		return nil, f.createErr
	}
	return f.created, nil
}

func (f *fakeAPI) IssueNodeID(_ context.Context, _ int64, _ githubclient.RepoRef, number int) (string, error) {
	f.nodeIDNumber = number
	if f.nodeIDErr != nil {
		return "", f.nodeIDErr
	}
	return f.parentNode, nil
}

func (f *fakeAPI) ProjectFields(_ context.Context, _ int64, coord githubclient.ProjectCoord, fieldName string) (*githubclient.ProjectMeta, error) {
	f.fieldsCoord, f.fieldsName = coord, fieldName
	if f.fieldsErr != nil {
		return nil, f.fieldsErr
	}
	return f.meta, nil
}

func (f *fakeAPI) ProjectItemStatus(_ context.Context, _ int64, issueNodeID, projectID, _ string) (*githubclient.ProjectItemStatus, error) {
	f.itemStatusIssueNode, f.itemStatusProjectID = issueNodeID, projectID
	if f.itemStatusErr != nil {
		return nil, f.itemStatusErr
	}
	return f.itemStatus, nil
}

func (f *fakeAPI) AddProjectItem(_ context.Context, _ int64, projectID, contentID string) (string, error) {
	f.addItemContent = contentID
	_ = projectID
	if f.addItemErr != nil {
		return "", f.addItemErr
	}
	return f.itemID, nil
}

func (f *fakeAPI) SetProjectItemSingleSelect(_ context.Context, _ int64, projectID, itemID, fieldID, optionID string) error {
	f.setProjectID, f.setItemID, f.setFieldID, f.setOptionID = projectID, itemID, fieldID, optionID
	return f.setErr
}

func (f *fakeAPI) AddSubIssue(_ context.Context, _ int64, parentNodeID, childNodeID string) error {
	f.subParent, f.subChild = parentNodeID, childNodeID
	return f.subErr
}

func (f *fakeAPI) ListSubIssues(_ context.Context, _ int64, parentNodeID string) ([]githubclient.SubIssue, error) {
	f.listSubParent = parentNodeID
	if f.listSubErr != nil {
		return nil, f.listSubErr
	}
	return f.listSubResults, nil
}

func (f *fakeAPI) SearchIssuesByTitle(_ context.Context, _ int64, query string) ([]githubclient.IssueTitleResult, error) {
	f.searchQuery = query
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	if f.searchResultsFn != nil {
		return f.searchResultsFn(query), nil
	}
	return f.searchResults, nil
}

func (f *fakeAPI) ProjectsTokenConfigured() bool { return f.projectsTokenConfigured }

func baseRequest() workmgmt.ProviderRequest {
	return workmgmt.ProviderRequest{
		Item: workmgmt.WorkItem{
			Type:           "feature",
			Title:          "[E22.7] do the thing",
			Body:           "## Summary\n\ndo the thing\n",
			Classification: workmgmt.Classification{Labels: []string{"type:feature"}, Complexity: "medium"},
			BoardPlacement: workmgmt.BoardPlacement{Status: "Backlog"},
		},
		Target: workmgmt.Target{
			InstallationID: 99,
			Repo:           workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"},
			Project:        &workmgmt.Project{Owner: "kuhlman-labs", OwnerType: "user", Number: 7},
		},
	}
}

func TestProvider_File_FullPath(t *testing.T) {
	api := &fakeAPI{
		created: &githubclient.CreatedIssue{Number: 1234, NodeID: "ISSUE_NODE", HTMLURL: "https://github.com/kuhlman-labs/fishhawk/issues/1234"},
		meta:    &githubclient.ProjectMeta{ProjectID: "PROJ", FieldID: "FIELD", StatusOptions: map[string]string{"Backlog": "OPT_BACKLOG"}},
		itemID:  "ITEM",
	}
	req := baseRequest()
	req.Item.Relations.ParentEpic = "#1005"
	api.parentNode = "EPIC_NODE"

	p := New(api)
	if p.Name() != ProviderName {
		t.Fatalf("Name = %q", p.Name())
	}
	created, err := p.File(context.Background(), req)
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	if created.Number != 1234 || created.URL == "" {
		t.Errorf("created = %+v", created)
	}
	// Happy path: both best-effort enrichment steps landed.
	if !created.Boarded || !created.EpicLinked {
		t.Errorf("boarded=%v epic_linked=%v, want both true", created.Boarded, created.EpicLinked)
	}
	if created.BoardingError != "" || created.EpicLinkError != "" {
		t.Errorf("unexpected enrichment errors: boarding=%q epic=%q", created.BoardingError, created.EpicLinkError)
	}
	if api.createParams.Title != "[E22.7] do the thing" || len(api.createParams.Labels) != 1 {
		t.Errorf("create params = %+v", api.createParams)
	}
	if api.fieldsCoord.Number != 7 || api.fieldsName != "Status" {
		t.Errorf("project fields lookup = %+v name=%q", api.fieldsCoord, api.fieldsName)
	}
	if api.addItemContent != "ISSUE_NODE" {
		t.Errorf("add project item content = %q, want ISSUE_NODE", api.addItemContent)
	}
	if api.setOptionID != "OPT_BACKLOG" || api.setFieldID != "FIELD" || api.setItemID != "ITEM" {
		t.Errorf("set field call = proj=%q item=%q field=%q opt=%q", api.setProjectID, api.setItemID, api.setFieldID, api.setOptionID)
	}
	if api.nodeIDNumber != 1005 {
		t.Errorf("parent epic resolved number = %d, want 1005", api.nodeIDNumber)
	}
	if api.subParent != "EPIC_NODE" || api.subChild != "ISSUE_NODE" {
		t.Errorf("sub-issue link = parent=%q child=%q", api.subParent, api.subChild)
	}
}

func TestProvider_File_NoProjectSkipsBoard(t *testing.T) {
	api := &fakeAPI{created: &githubclient.CreatedIssue{Number: 5, NodeID: "N", HTMLURL: "u"}}
	req := baseRequest()
	req.Target.Project = nil
	req.Item.Relations = workmgmt.Relations{}

	created, err := New(api).File(context.Background(), req)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if created.Number != 5 {
		t.Errorf("created = %+v", created)
	}
	if api.fieldsName != "" {
		t.Errorf("project fields should not be queried when no project configured")
	}
	// No project configured: nothing to board, and no boarding error.
	if created.Boarded || created.BoardingError != "" {
		t.Errorf("boarded=%v boarding_error=%q, want false with no error", created.Boarded, created.BoardingError)
	}
}

func TestProvider_File_UnknownStatusBestEffort(t *testing.T) {
	api := &fakeAPI{
		created: &githubclient.CreatedIssue{Number: 5, NodeID: "N", HTMLURL: "https://x/5"},
		meta:    &githubclient.ProjectMeta{ProjectID: "P", FieldID: "F", StatusOptions: map[string]string{"Done": "OPT"}},
		itemID:  "ITEM",
	}
	req := baseRequest()
	req.Item.Relations = workmgmt.Relations{}
	// Status "Backlog" is not an option on the board. Board placement is
	// best-effort (#1107): the issue is the durable result, so File returns
	// it with a nil error and Boarded=false + a BoardingError naming the
	// cause rather than discarding the created issue.
	created, err := New(api).File(context.Background(), req)
	if err != nil {
		t.Fatalf("File should not error on a board-placement failure: %v", err)
	}
	if created == nil || created.Number != 5 || created.URL != "https://x/5" {
		t.Fatalf("created item not returned: %+v", created)
	}
	if created.Boarded {
		t.Errorf("boarded = true, want false when status is not a board option")
	}
	if !strings.Contains(created.BoardingError, "is not a Status option") {
		t.Errorf("boarding_error should name the cause, got %q", created.BoardingError)
	}
}

func TestProvider_File_EpicLinkBestEffort(t *testing.T) {
	api := &fakeAPI{
		created: &githubclient.CreatedIssue{Number: 6, NodeID: "N6", HTMLURL: "https://x/6"},
		meta:    &githubclient.ProjectMeta{ProjectID: "P", FieldID: "F", StatusOptions: map[string]string{"Backlog": "OPT"}},
		itemID:  "ITEM",
		subErr:  errors.New("sub-issue API rejected the link"),
	}
	req := baseRequest()
	req.Item.Relations.ParentEpic = "#1005"
	// Epic linking is best-effort: a link failure files the issue (and
	// boards it) with EpicLinked=false and an EpicLinkError naming the cause.
	created, err := New(api).File(context.Background(), req)
	if err != nil {
		t.Fatalf("File should not error on an epic-link failure: %v", err)
	}
	if !created.Boarded {
		t.Errorf("boarded = false, want true (board placement succeeded)")
	}
	if created.EpicLinked {
		t.Errorf("epic_linked = true, want false when the link failed")
	}
	if !strings.Contains(created.EpicLinkError, "sub-issue API rejected the link") {
		t.Errorf("epic_link_error should name the cause, got %q", created.EpicLinkError)
	}
}

func TestProvider_File_CreateIssueErrorPropagates(t *testing.T) {
	api := &fakeAPI{createErr: errors.New("boom")}
	_, err := New(api).File(context.Background(), baseRequest())
	if err == nil || !strings.Contains(err.Error(), "create issue") {
		t.Fatalf("want create-issue error, got %v", err)
	}
}

// TestProvider_File_StampsDependsOnMarker asserts File renders the depends_on
// body marker into the created issue body (the persist half of the campaign
// DAG round trip).
func TestProvider_File_StampsDependsOnMarker(t *testing.T) {
	api := &fakeAPI{created: &githubclient.CreatedIssue{Number: 7, NodeID: "N", HTMLURL: "u"}}
	req := baseRequest()
	req.Target.Project = nil // skip boarding; assert only the create body
	req.Item.Relations = workmgmt.Relations{DependsOn: []string{"#41", "42"}}

	if _, err := New(api).File(context.Background(), req); err != nil {
		t.Fatalf("File: %v", err)
	}
	if !strings.Contains(api.createParams.Body, "Depends on: #41, #42") {
		t.Errorf("created body missing depends_on marker:\n%s", api.createParams.Body)
	}
}

// TestProvider_File_DependsOnMarkerIdempotent asserts a body already carrying
// a marker is not double-stamped (the ensureDependsOnMarker idempotency
// branch).
func TestProvider_File_DependsOnMarkerIdempotent(t *testing.T) {
	api := &fakeAPI{created: &githubclient.CreatedIssue{Number: 7, NodeID: "N", HTMLURL: "u"}}
	req := baseRequest()
	req.Target.Project = nil
	req.Item.Body = "## Summary\n\nx\n\nDepends on: #41\n"
	req.Item.Relations = workmgmt.Relations{DependsOn: []string{"#41", "#42"}}

	if _, err := New(api).File(context.Background(), req); err != nil {
		t.Fatalf("File: %v", err)
	}
	if got := strings.Count(api.createParams.Body, "Depends on:"); got != 1 {
		t.Errorf("marker stamped %d times, want 1 (idempotent):\n%s", got, api.createParams.Body)
	}
}

// TestProvider_File_NoDependsOnNoMarker asserts an item without depends_on
// carries no marker (the empty-refs branch).
func TestProvider_File_NoDependsOnNoMarker(t *testing.T) {
	api := &fakeAPI{created: &githubclient.CreatedIssue{Number: 7, NodeID: "N", HTMLURL: "u"}}
	req := baseRequest()
	req.Target.Project = nil
	req.Item.Relations = workmgmt.Relations{}

	if _, err := New(api).File(context.Background(), req); err != nil {
		t.Fatalf("File: %v", err)
	}
	if strings.Contains(api.createParams.Body, "Depends on:") {
		t.Errorf("body should carry no marker when depends_on is empty:\n%s", api.createParams.Body)
	}
}

// TestProvider_EpicChildren_ResolvesChildrenAndEdges drives EpicChildren
// against a fake sub-issues list: children are returned ascending, the
// depends_on edges parsed from a child body are restricted to the sibling
// set, and a reference to a NON-child is dropped.
func TestProvider_EpicChildren_ResolvesChildrenAndEdges(t *testing.T) {
	api := &fakeAPI{
		parentNode: "EPIC_NODE",
		listSubResults: []githubclient.SubIssue{
			// out-of-order on purpose: EpicChildren sorts ascending.
			{Number: 42, NodeID: "N42", Title: "slice B", Body: "## Summary\n\nDepends on: #41\n"},
			{Number: 41, NodeID: "N41", Title: "slice A", Body: "## Summary\n\nno deps\n"},
			{Number: 43, NodeID: "N43", Title: "slice C", Body: "Depends on: #41, #42, #999\n"},
		},
	}
	res, err := New(api).EpicChildren(context.Background(), workmgmt.EpicChildrenRequest{
		Target: workmgmt.Target{InstallationID: 99, Repo: workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"}},
		Epic:   "#1005",
	})
	if err != nil {
		t.Fatalf("EpicChildren: %v", err)
	}
	if api.nodeIDNumber != 1005 {
		t.Errorf("epic resolved number = %d, want 1005", api.nodeIDNumber)
	}
	if api.listSubParent != "EPIC_NODE" {
		t.Errorf("list sub-issues parent = %q, want EPIC_NODE", api.listSubParent)
	}
	wantChildren := []int{41, 42, 43}
	if len(res.Children) != len(wantChildren) {
		t.Fatalf("children = %+v, want ascending 41,42,43", res.Children)
	}
	for i, c := range res.Children {
		if c.Number != wantChildren[i] {
			t.Errorf("children[%d].Number = %d, want %d", i, c.Number, wantChildren[i])
		}
	}
	// Edges: 42->41, 43->41, 43->42. The #999 reference is not a child → it is
	// kept out of Edges and surfaced in DroppedEdges (not silently discarded).
	want := []workmgmt.DependsEdge{{From: 42, To: 41}, {From: 43, To: 41}, {From: 43, To: 42}}
	if len(res.Edges) != len(want) {
		t.Fatalf("edges = %+v, want %+v (#999 must be dropped)", res.Edges, want)
	}
	for i, e := range res.Edges {
		if e != want[i] {
			t.Errorf("edge[%d] = %+v, want %+v", i, e, want[i])
		}
	}
	// The mis-targeted #999 reference lands in DroppedEdges so assembly can
	// fail closed on it rather than the provider silently dropping it.
	wantDropped := []workmgmt.DependsEdge{{From: 43, To: 999}}
	if len(res.DroppedEdges) != len(wantDropped) {
		t.Fatalf("dropped edges = %+v, want %+v (#999 must be surfaced)", res.DroppedEdges, wantDropped)
	}
	for i, e := range res.DroppedEdges {
		if e != wantDropped[i] {
			t.Errorf("dropped edge[%d] = %+v, want %+v", i, e, wantDropped[i])
		}
	}
}

// TestProvider_EpicChildren_DerivesAutonomy asserts each child's Autonomy is
// sourced from its `autonomy:<tier>` label: a child carrying autonomy:low maps
// to "low", one with no autonomy label maps to "" (#1551).
func TestProvider_EpicChildren_DerivesAutonomy(t *testing.T) {
	api := &fakeAPI{
		parentNode: "EPIC_NODE",
		listSubResults: []githubclient.SubIssue{
			{Number: 41, NodeID: "N41", Title: "human-led", Body: "no deps", Labels: []string{"type:feature", "autonomy:low"}},
			{Number: 42, NodeID: "N42", Title: "agent-drivable", Body: "no deps", Labels: []string{"autonomy:high"}},
			{Number: 43, NodeID: "N43", Title: "unlabelled", Body: "no deps", Labels: nil},
		},
	}
	res, err := New(api).EpicChildren(context.Background(), workmgmt.EpicChildrenRequest{
		Target: workmgmt.Target{InstallationID: 99, Repo: workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"}},
		Epic:   "#1005",
	})
	if err != nil {
		t.Fatalf("EpicChildren: %v", err)
	}
	want := map[int]string{41: "low", 42: "high", 43: ""}
	if len(res.Children) != len(want) {
		t.Fatalf("children = %+v, want %d", res.Children, len(want))
	}
	for _, c := range res.Children {
		if c.Autonomy != want[c.Number] {
			t.Errorf("child #%d Autonomy = %q, want %q", c.Number, c.Autonomy, want[c.Number])
		}
	}
}

// TestAutonomyFromLabels covers the helper directly: the first autonomy:<tier>
// label wins, a non-autonomy label is ignored, and no autonomy label yields "".
func TestAutonomyFromLabels(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   string
	}{
		{"low", []string{"area:server", "autonomy:low"}, "low"},
		{"medium", []string{"autonomy:medium"}, "medium"},
		{"high", []string{"autonomy:high", "type:feature"}, "high"},
		{"absent", []string{"area:server", "type:feature"}, ""},
		{"nil", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := autonomyFromLabels(tc.labels); got != tc.want {
				t.Errorf("autonomyFromLabels(%v) = %q, want %q", tc.labels, got, tc.want)
			}
		})
	}
}

// TestProvider_EpicChildren_FailClosed covers the defensive branches: a nil
// API, a missing repo, a zero installation, a malformed epic ref, and a
// ListSubIssues error each return an error rather than a partial result.
func TestProvider_EpicChildren_FailClosed(t *testing.T) {
	good := workmgmt.EpicChildrenRequest{
		Target: workmgmt.Target{InstallationID: 99, Repo: workmgmt.Repo{Owner: "o", Name: "r"}},
		Epic:   "#1005",
	}
	t.Run("nil api", func(t *testing.T) {
		if _, err := (&Provider{}).EpicChildren(context.Background(), good); err == nil || !strings.Contains(err.Error(), "missing API client") {
			t.Fatalf("want missing-API error, got %v", err)
		}
	})
	t.Run("missing repo", func(t *testing.T) {
		req := good
		req.Target.Repo = workmgmt.Repo{}
		if _, err := New(&fakeAPI{}).EpicChildren(context.Background(), req); err == nil || !strings.Contains(err.Error(), "repo owner and name required") {
			t.Fatalf("want repo-required error, got %v", err)
		}
	})
	t.Run("zero installation", func(t *testing.T) {
		req := good
		req.Target.InstallationID = 0
		if _, err := New(&fakeAPI{}).EpicChildren(context.Background(), req); err == nil || !strings.Contains(err.Error(), "no installation id available") {
			t.Fatalf("want missing-installation error, got %v", err)
		}
	})
	t.Run("malformed epic ref", func(t *testing.T) {
		req := good
		req.Epic = "not-a-ref"
		if _, err := New(&fakeAPI{}).EpicChildren(context.Background(), req); err == nil || !strings.Contains(err.Error(), "not a numeric issue reference") {
			t.Fatalf("want malformed-epic error, got %v", err)
		}
	})
	t.Run("list sub-issues error", func(t *testing.T) {
		api := &fakeAPI{parentNode: "EPIC_NODE", listSubErr: errors.New("graphql rejected the query")}
		if _, err := New(api).EpicChildren(context.Background(), good); err == nil || !strings.Contains(err.Error(), "list epic #1005 children") {
			t.Fatalf("want list-sub-issues error, got %v", err)
		}
	})
	t.Run("node id error", func(t *testing.T) {
		api := &fakeAPI{nodeIDErr: errors.New("issue not found")}
		if _, err := New(api).EpicChildren(context.Background(), good); err == nil || !strings.Contains(err.Error(), "resolve epic #1005") {
			t.Fatalf("want node-id error, got %v", err)
		}
	})
}

// TestDependsOnMarker_RoundTrip asserts the render/parse helper pair is a
// faithful round trip (the single-source-of-truth drift guard) and that an
// empty ref list renders no marker.
func TestDependsOnMarker_RoundTrip(t *testing.T) {
	if got := renderDependsOnMarker(nil); got != "" {
		t.Errorf("empty refs rendered %q, want empty", got)
	}
	if got := renderDependsOnMarker([]string{"#41", "42", "  "}); got != "Depends on: #41, #42" {
		t.Errorf("render = %q", got)
	}
	body := "## Summary\n\nx\n\n" + renderDependsOnMarker([]string{"41", "#42"})
	nums := parseDependsOnMarker(body)
	if len(nums) != 2 || nums[0] != 41 || nums[1] != 42 {
		t.Errorf("parse round trip = %v, want [41 42]", nums)
	}
	if parseDependsOnMarker("## Summary\n\nno marker here\n") != nil {
		t.Errorf("parse of a body with no marker should be nil")
	}
}

func TestProvider_File_MissingRepoRejected(t *testing.T) {
	req := baseRequest()
	req.Target.Repo = workmgmt.Repo{}
	_, err := New(&fakeAPI{}).File(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "repo owner and name required") {
		t.Fatalf("want repo-required error, got %v", err)
	}
}

func TestProvider_File_MissingInstallationRejected(t *testing.T) {
	// #1005 concern-2: the run-absent path leaves InstallationID 0; the
	// provider must fail closed with an actionable error naming the v0
	// run-scoped constraint rather than dispatching an untokened REST call.
	api := &fakeAPI{created: &githubclient.CreatedIssue{Number: 1, NodeID: "N", HTMLURL: "u"}}
	req := baseRequest()
	req.Target.InstallationID = 0
	_, err := New(api).File(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "no installation id available") {
		t.Fatalf("want missing-installation error, got %v", err)
	}
	if !strings.Contains(err.Error(), "run-scoped in v0") {
		t.Errorf("error should name the v0 run-scoped constraint: %v", err)
	}
	// Must fail closed before any issue is created.
	if api.createParams.Title != "" {
		t.Errorf("issue should not be created when installation id is absent")
	}
}

func TestParseIssueRef(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"#1005", 1005, false},
		{"1005", 1005, false},
		{" #42 ", 42, false},
		{"abc", 0, true},
		{"#0", 0, true},
	} {
		got, err := parseIssueRef(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseIssueRef(%q) want error", tc.in)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("parseIssueRef(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
}

// realClientFixture builds a real *githubclient.Client pointed at an
// httptest mux, recording the Authorization header the REST issue-create
// call and the GraphQL board-placement calls each carried. projectsToken
// empty exercises the #1107 unconfigured path.
type realClientFixture struct {
	restAuth    string
	graphqlAuth string
}

func newRealClient(t *testing.T, projectsToken string) (*githubclient.Client, *realClientFixture) {
	t.Helper()
	fx := &realClientFixture{}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		fx.restAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"number":1234,"node_id":"ISSUE_NODE","html_url":"https://github.com/kuhlman-labs/fishhawk/issues/1234"}`)
	})

	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		fx.graphqlAuth = r.Header.Get("Authorization")
		var body struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch {
		case strings.Contains(body.Query, "ProjectFields"):
			_, _ = io.WriteString(w, `{"data":{"user":{"projectV2":{"id":"PROJ","field":{"id":"FIELD","options":[{"id":"OPT_BACKLOG","name":"Backlog"}]}}}}}`)
		case strings.Contains(body.Query, "AddItem"):
			_, _ = io.WriteString(w, `{"data":{"addProjectV2ItemById":{"item":{"id":"ITEM"}}}}`)
		case strings.Contains(body.Query, "SetField"):
			_, _ = io.WriteString(w, `{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"ITEM"}}}}`)
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

func TestProvider_File_CrossBoundary_ProjectsTokenBoardsUserProject(t *testing.T) {
	// End-to-end seam (config -> client token selection -> provider): a
	// real *githubclient.Client with a projects PAT boards a USER-owned
	// project. The board-placement GraphQL must carry the PAT while the
	// issue-create REST call stays on the installation token (#1114).
	c, fx := newRealClient(t, "pat_projects")
	created, err := New(c).File(context.Background(), baseRequest())
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if !created.Boarded {
		t.Errorf("boarded = false (%q), want true", created.BoardingError)
	}
	if fx.restAuth != "Bearer ghs_install" {
		t.Errorf("issue-create REST Authorization = %q, want installation token", fx.restAuth)
	}
	if fx.graphqlAuth != "Bearer pat_projects" {
		t.Errorf("board GraphQL Authorization = %q, want projects token", fx.graphqlAuth)
	}
}

func TestProvider_File_CrossBoundary_NoProjectsTokenDegradesBoarded(t *testing.T) {
	// #1107 preserved: with no projects token, a user-owned board placement
	// falls back to the installation token. GitHub answers an installation
	// token's user-Projects GraphQL with "Could not resolve to a ProjectV2",
	// so board placement degrades to boarded:false with a BoardingError — the
	// change is inert until the operator sets the token.
	mux := http.NewServeMux()
	var graphqlAuth string
	mux.HandleFunc("POST /repos/{owner}/{repo}/issues", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"number":1234,"node_id":"ISSUE_NODE","html_url":"https://x/1234"}`)
	})
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		graphqlAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"errors":[{"message":"Could not resolve to a ProjectV2 with the number 7"}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := githubclient.New(stubTokenProvider{token: "ghs_install"})
	c.BaseURL = srv.URL
	c.HTTP = &http.Client{Timeout: 5 * time.Second}

	created, err := New(c).File(context.Background(), baseRequest())
	if err != nil {
		t.Fatalf("File should not error on a board-placement failure: %v", err)
	}
	if created.Boarded {
		t.Errorf("boarded = true, want false (#1107 degradation)")
	}
	if created.BoardingError == "" {
		t.Errorf("want a BoardingError naming the cause")
	}
	if graphqlAuth != "Bearer ghs_install" {
		t.Errorf("board GraphQL Authorization = %q, want installation-token fallback", graphqlAuth)
	}
}

// canonicalStates is the conventions states map the transition tests
// resolve canonical states to board options through.
var canonicalStates = map[string]string{
	workmgmt.CanonicalStateBacklog:    "Backlog",
	workmgmt.CanonicalStateInProgress: "In Progress",
	workmgmt.CanonicalStateInReview:   "In Review",
	workmgmt.CanonicalStateBlocked:    "Blocked",
	workmgmt.CanonicalStateDone:       "Done",
}

// transitionAPI returns a fakeAPI primed for a run_started move: the issue
// resolves to a node id, the board exposes every Status option, and the
// issue's current item carries currentStatus.
func transitionAPI(currentStatus string, onBoard bool) *fakeAPI {
	return &fakeAPI{
		parentNode: "ISSUE_NODE",
		meta: &githubclient.ProjectMeta{ProjectID: "PROJ", FieldID: "FIELD", StatusOptions: map[string]string{
			"Backlog": "OPT_BACKLOG", "In Progress": "OPT_IP", "In Review": "OPT_IR", "Blocked": "OPT_BLOCKED", "Done": "OPT_DONE",
		}},
		itemStatus: &githubclient.ProjectItemStatus{OnBoard: onBoard, ItemID: "ITEM", Status: currentStatus},
		// The transition fixtures target a user-owned board (baseRequest's
		// Project), so a projects token must be configured for the move to be
		// dispatched; the no-token degradation has its own test.
		projectsTokenConfigured: true,
	}
}

// runStartedRequest is the canonical run_started move: advance to In
// Progress from an expected source of Backlog (unset counts as Backlog).
func runStartedRequest() workmgmt.TransitionRequest {
	return workmgmt.TransitionRequest{
		IssueNumber:          1012,
		Trigger:              "run_started",
		Target:               baseRequest().Target,
		CanonicalState:       workmgmt.CanonicalStateInProgress,
		ExpectedSourceStates: []string{workmgmt.CanonicalStateBacklog},
		States:               canonicalStates,
	}
}

func TestProvider_Transition_MovesFromExpectedSource(t *testing.T) {
	api := transitionAPI("Backlog", true)
	res, err := New(api).Transition(context.Background(), runStartedRequest())
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if !res.Moved || res.From != "Backlog" || res.To != "In Progress" {
		t.Fatalf("result = %+v, want moved Backlog->In Progress", res)
	}
	// The Status single-select must be set to the In Progress option on the
	// resolved item — and nothing else (Status-only scope, #1005 split).
	if api.setOptionID != "OPT_IP" || api.setItemID != "ITEM" || api.setFieldID != "FIELD" {
		t.Errorf("set field call = item=%q field=%q opt=%q", api.setItemID, api.setFieldID, api.setOptionID)
	}
	if api.itemStatusIssueNode != "ISSUE_NODE" || api.itemStatusProjectID != "PROJ" {
		t.Errorf("status read = node=%q project=%q", api.itemStatusIssueNode, api.itemStatusProjectID)
	}
	// Transition must never create an issue or touch epic links.
	if api.createParams.Title != "" || api.subParent != "" {
		t.Errorf("transition must not file or link: create=%+v sub=%q", api.createParams, api.subParent)
	}
}

func TestProvider_Transition_UnsetStatusCountsAsBacklog(t *testing.T) {
	// A fresh card with no Status set still advances on run_started, because
	// unset is treated as Backlog (an expected source).
	api := transitionAPI("", true)
	res, err := New(api).Transition(context.Background(), runStartedRequest())
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if !res.Moved || res.To != "In Progress" {
		t.Errorf("result = %+v, want moved to In Progress from unset", res)
	}
	if api.setOptionID != "OPT_IP" {
		t.Errorf("set option = %q, want OPT_IP", api.setOptionID)
	}
}

func TestProvider_Transition_NeverFightsHumanParkedCard(t *testing.T) {
	// A human parked the card in Blocked. run_started expects a Backlog
	// source, so the move is SKIPPED with no mutation — never-fight-the-human.
	api := transitionAPI("Blocked", true)
	res, err := New(api).Transition(context.Background(), runStartedRequest())
	if err != nil {
		t.Fatalf("Transition should not error on a skip: %v", err)
	}
	if res.Moved || !res.Skipped {
		t.Fatalf("result = %+v, want skipped (not moved)", res)
	}
	if res.From != "Blocked" || !strings.Contains(res.SkipReason, "expected source") {
		t.Errorf("skip = from=%q reason=%q", res.From, res.SkipReason)
	}
	if api.setOptionID != "" {
		t.Errorf("Status must not be mutated on a skip, got set opt %q", api.setOptionID)
	}
}

func TestProvider_Transition_SkipsWhenOffBoard(t *testing.T) {
	api := transitionAPI("", false)
	res, err := New(api).Transition(context.Background(), runStartedRequest())
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if res.Moved || !res.Skipped || !strings.Contains(res.SkipReason, "not on the project board") {
		t.Errorf("result = %+v, want off-board skip", res)
	}
	if api.setOptionID != "" {
		t.Errorf("off-board skip must not set Status, got %q", api.setOptionID)
	}
}

func TestProvider_Transition_SkipsWhenAlreadyAtTarget(t *testing.T) {
	// Idempotency: a card already In Progress is a no-op skip, so a
	// reconciler re-assertion never thrashes the board.
	api := transitionAPI("In Progress", true)
	req := runStartedRequest()
	// In Progress is an acceptable source for the re-assertion too.
	req.ExpectedSourceStates = []string{workmgmt.CanonicalStateBacklog, workmgmt.CanonicalStateInProgress}
	res, err := New(api).Transition(context.Background(), req)
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if res.Moved || !res.Skipped || !strings.Contains(res.SkipReason, "already at target") {
		t.Errorf("result = %+v, want already-at-target skip", res)
	}
	if api.setOptionID != "" {
		t.Errorf("no mutation expected, got set opt %q", api.setOptionID)
	}
}

func TestProvider_Transition_SkipsWhenNoProject(t *testing.T) {
	req := runStartedRequest()
	req.Target.Project = nil
	res, err := New(transitionAPI("Backlog", true)).Transition(context.Background(), req)
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if !res.Skipped || !strings.Contains(res.SkipReason, "no project configured") {
		t.Errorf("result = %+v, want no-project skip", res)
	}
}

func TestProvider_Transition_SkipsUserProjectWhenNoProjectsToken(t *testing.T) {
	// Edge (approval condition): a user-owned board (Project #7) with no
	// projects token configured is unreachable with the installation token.
	// The move must degrade to a best-effort SKIP — never an error — so the
	// lifecycle hook still writes a work_item_transitioned audit. No board
	// GraphQL is dispatched (no status read, no mutation).
	api := transitionAPI("Backlog", true)
	api.projectsTokenConfigured = false
	res, err := New(api).Transition(context.Background(), runStartedRequest())
	if err != nil {
		t.Fatalf("Transition should degrade to a skip, not error: %v", err)
	}
	if res.Moved || !res.Skipped {
		t.Fatalf("result = %+v, want skipped (not moved)", res)
	}
	if !strings.Contains(res.SkipReason, "no projects token") {
		t.Errorf("skip reason = %q, want it to name the missing projects token", res.SkipReason)
	}
	if api.itemStatusIssueNode != "" {
		t.Errorf("no board read expected on the no-token skip, got status read for %q", api.itemStatusIssueNode)
	}
	if api.setOptionID != "" {
		t.Errorf("no mutation expected on the no-token skip, got set opt %q", api.setOptionID)
	}
}

func TestProvider_Transition_SkipsWhenTargetNotABoardOption(t *testing.T) {
	api := transitionAPI("Backlog", true)
	// Drop In Progress from the board's options: the target can't be set.
	delete(api.meta.StatusOptions, "In Progress")
	res, err := New(api).Transition(context.Background(), runStartedRequest())
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if !res.Skipped || !strings.Contains(res.SkipReason, "not a Status option") {
		t.Errorf("result = %+v, want target-not-an-option skip", res)
	}
}

func TestProvider_Transition_SkipsWhenCanonicalStateUnmapped(t *testing.T) {
	req := runStartedRequest()
	req.CanonicalState = workmgmt.CanonicalStateDone
	delete(req.States, workmgmt.CanonicalStateDone)
	res, err := New(transitionAPI("Backlog", true)).Transition(context.Background(), req)
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if !res.Skipped || !strings.Contains(res.SkipReason, "no configured provider option") {
		t.Errorf("result = %+v, want unmapped-canonical-state skip", res)
	}
}

func TestProvider_Transition_ResolveErrorsPropagate(t *testing.T) {
	api := transitionAPI("Backlog", true)
	api.nodeIDErr = errors.New("issue gone")
	_, err := New(api).Transition(context.Background(), runStartedRequest())
	if err == nil || !strings.Contains(err.Error(), "resolve issue") {
		t.Fatalf("want resolve-issue error, got %v", err)
	}
}

func TestProvider_Transition_MissingInstallationRejected(t *testing.T) {
	req := runStartedRequest()
	req.Target.InstallationID = 0
	_, err := New(transitionAPI("Backlog", true)).Transition(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "no installation id available") {
		t.Fatalf("want missing-installation error, got %v", err)
	}
}

// discoverRequest is the canonical adr number-discovery request: the ADR
// title_format + prefix against the baseRequest target (installation 99).
func discoverRequest() workmgmt.DiscoverNumbersRequest {
	return workmgmt.DiscoverNumbersRequest{
		Target:      baseRequest().Target,
		Prefix:      "ADR-",
		TitleFormat: "[ADR-{number}] {summary}",
	}
}

func TestProvider_DiscoverNumbers_ParsesPaddedAndClosedTitles(t *testing.T) {
	// Padded ([ADR-041]) and unpadded ([ADR-9]) titles both parse, and a
	// closed-issue title counts (decided ADRs are closed → the query carries no
	// is:open). The search term must be the literal prefix before {number}.
	api := &fakeAPI{searchResults: []githubclient.IssueTitleResult{
		{Number: 200, Title: "[ADR-041] padded decision"},
		{Number: 201, Title: "[ADR-9] a closed, decided ADR"},
	}}
	got, err := New(api).DiscoverNumbers(context.Background(), discoverRequest())
	if err != nil {
		t.Fatalf("DiscoverNumbers: %v", err)
	}
	if len(got) != 2 || got[0] != 41 || got[1] != 9 {
		t.Errorf("numbers = %v, want [41 9]", got)
	}
	if !strings.Contains(api.searchQuery, `in:title "[ADR-"`) {
		t.Errorf("search query = %q, want it to carry the literal [ADR- in:title term", api.searchQuery)
	}
	if strings.Contains(api.searchQuery, "is:open") {
		t.Errorf("search query = %q must NOT restrict to is:open (closed ADRs count)", api.searchQuery)
	}
	if !strings.Contains(api.searchQuery, "repo:kuhlman-labs/fishhawk") {
		t.Errorf("search query = %q, want it scoped to the target repo", api.searchQuery)
	}
}

func TestProvider_DiscoverNumbers_PrefixCannotBreakOutOfQuotedQualifier(t *testing.T) {
	// A title_format whose literal prefix carries a double quote (or backslash)
	// must not break out of the quoted in:title qualifier in the composed
	// search query — the dangerous characters are stripped from the search term
	// while the regex re-parse still matches real titles.
	req := discoverRequest()
	req.TitleFormat = `[A"D\R-{number}] {summary}`
	api := &fakeAPI{searchResults: []githubclient.IssueTitleResult{
		{Number: 5, Title: `[A"D\R-12] a decided one`},
	}}
	got, err := New(api).DiscoverNumbers(context.Background(), req)
	if err != nil {
		t.Fatalf("DiscoverNumbers: %v", err)
	}
	if strings.Contains(api.searchQuery, `"`+`]`) || strings.Count(api.searchQuery, `"`) != 2 {
		t.Errorf("search query = %q must keep exactly the two enclosing quotes (no breakout)", api.searchQuery)
	}
	if strings.Contains(api.searchQuery, `\`) {
		t.Errorf("search query = %q must not carry a backslash that could escape the closing quote", api.searchQuery)
	}
	if len(got) != 1 || got[0] != 12 {
		t.Errorf("numbers = %v, want [12] (regex still matches the real title)", got)
	}
}

func TestProvider_DiscoverNumbers_EmptyResultReturnsEmpty(t *testing.T) {
	// The genuine-first path: no matches → empty slice, no error. The handler
	// then seeds [0] → number 1, never a silent 001.
	api := &fakeAPI{searchResults: nil}
	got, err := New(api).DiscoverNumbers(context.Background(), discoverRequest())
	if err != nil {
		t.Fatalf("DiscoverNumbers: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("numbers = %v, want empty", got)
	}
}

func TestProvider_DiscoverNumbers_SkipsMalformedTitles(t *testing.T) {
	// GitHub in:title search is fuzzy, so a hit whose title lacks the [ADR-N]
	// token must be ignored rather than counted.
	api := &fakeAPI{searchResults: []githubclient.IssueTitleResult{
		{Number: 1, Title: "[ADR-007] a real one"},
		{Number: 2, Title: "ADR considerations without the token"},
		{Number: 3, Title: "[ADR-] missing the number"},
	}}
	got, err := New(api).DiscoverNumbers(context.Background(), discoverRequest())
	if err != nil {
		t.Fatalf("DiscoverNumbers: %v", err)
	}
	if len(got) != 1 || got[0] != 7 {
		t.Errorf("numbers = %v, want [7] (malformed titles skipped)", got)
	}
}

// TestProvider_DiscoverNumbers_EpicSkipsChildTitles is the #1508 load-bearing
// subtlety: the epic prefix "E" and title_format "[E{number}] {summary}" build
// an anchored regexp (^\[E(\d+)\] .*?) that requires "] " immediately after the
// captured number, so parent epic titles [E28]/[E29] parse while child titles
// [E28.3]/[E29.1] — which the label:"epic" query may still surface if a child
// carries the label — are skipped by the re-parse. This crosses the
// title_format→regexp→number-parse seam that the pure apply test does not
// exercise. The production epic query is label-only (#1522/#1523): it carries
// label:"epic" and OMITS the in:title term.
func TestProvider_DiscoverNumbers_EpicSkipsChildTitles(t *testing.T) {
	req := workmgmt.DiscoverNumbersRequest{
		Target:        baseRequest().Target,
		Prefix:        "E",
		TitleFormat:   "[E{number}] {summary}",
		DefaultLabels: []string{"epic"}, // production epic path carries label:"epic" (#1522)
	}
	api := &fakeAPI{searchResults: []githubclient.IssueTitleResult{
		{Number: 100, Title: "[E28] an epic"},
		{Number: 101, Title: "[E29] another epic"},
		{Number: 102, Title: "[E28.3] a child of E28"},
		{Number: 103, Title: "[E29.1] a child of E29"},
	}}
	got, err := New(api).DiscoverNumbers(context.Background(), req)
	if err != nil {
		t.Fatalf("DiscoverNumbers: %v", err)
	}
	if len(got) != 2 || got[0] != 28 || got[1] != 29 {
		t.Errorf("numbers = %v, want [28 29] (child titles skipped)", got)
	}
	if strings.Contains(api.searchQuery, "in:title") {
		t.Errorf("search query = %q, want NO in:title term on the label-qualified epic query (#1523)", api.searchQuery)
	}
	if !strings.Contains(api.searchQuery, `label:"epic"`) {
		t.Errorf("search query = %q, want it to carry the label:\"epic\" qualifier (#1522)", api.searchQuery)
	}
}

// TestProvider_DiscoverNumbers_LabelQualifierFindsBuriedMaxUnderRecencyLimit is
// the #1522/#1523 done-means test. The fake models the REAL search API's
// emptying behavior: it returns the full epic set (buried max [E29] present)
// ONLY for a label-qualified query that carries NO in:title term, and a
// truncated recency slice (max omitted, [E29.x] children present) otherwise.
// So it fails on the pre-#1522 title-only query, on #1523's broken
// label+in:title query (whose in:title "[E" AND collapses the real search to
// zero), AND on any regression that drops the label qualifier — passing only on
// the correct label-only composition. This closes the query-agnostic-fake gap
// that let #1523's broken query pass verification twice.
func TestProvider_DiscoverNumbers_LabelQualifierFindsBuriedMaxUnderRecencyLimit(t *testing.T) {
	req := workmgmt.DiscoverNumbersRequest{
		Target:        baseRequest().Target,
		Prefix:        "E",
		TitleFormat:   "[E{number}] {summary}",
		DefaultLabels: []string{"epic"},
	}
	fullEpicSet := []githubclient.IssueTitleResult{
		{Number: 100, Title: "[E25] an epic"},
		{Number: 101, Title: "[E29] the buried max epic"},
		{Number: 102, Title: "[E27] another epic"},
	}
	// Recency-ordered title-only results: the newest items are the children of
	// the most recently touched epic, burying [E29] out of the returned window.
	truncatedRecencySlice := []githubclient.IssueTitleResult{
		{Number: 200, Title: "[E29.1] a child of E29"},
		{Number: 201, Title: "[E29.2] another child of E29"},
		{Number: 202, Title: "[E25] an epic"},
	}
	api := &fakeAPI{searchResultsFn: func(query string) []githubclient.IssueTitleResult {
		// Model the real search API's emptying: the full epic set comes back ONLY
		// for a label-qualified query WITHOUT an in:title term. #1523's broken
		// label+in:title query (in:title present) falls to the truncated slice.
		if strings.Contains(query, `label:"epic"`) && !strings.Contains(query, "in:title") {
			return fullEpicSet
		}
		return truncatedRecencySlice
	}}
	got, err := New(api).DiscoverNumbers(context.Background(), req)
	if err != nil {
		t.Fatalf("DiscoverNumbers: %v", err)
	}
	max := 0
	for _, n := range got {
		if n > max {
			max = n
		}
	}
	if max != 29 {
		t.Errorf("max discovered = %d, want 29 (buried max found via label-only query → allocate 30); numbers=%v", max, got)
	}
	if !strings.Contains(api.searchQuery, `label:"epic"`) {
		t.Errorf("search query = %q, want it to carry the label:\"epic\" qualifier", api.searchQuery)
	}
	if strings.Contains(api.searchQuery, "in:title") {
		t.Errorf("search query = %q, want NO in:title term (label-only query, #1523)", api.searchQuery)
	}
}

// TestProvider_DiscoverNumbers_AdrPathUsesRealLabel drives the PRODUCTION adr
// path with its REAL DefaultLabels:["adr"] (the operator's binding condition):
// adr discovery now runs WITH label:"adr" ALONE, not the synthetic no-label
// branch. The fake models the real search API's emptying: it returns the full
// ADR set ONLY for a label-qualified query WITHOUT an in:title term, so the
// correct max is discovered — a strict improvement over the previous title-only
// adr discovery (which also surfaced fuzzy false positives lacking the label).
func TestProvider_DiscoverNumbers_AdrPathUsesRealLabel(t *testing.T) {
	req := discoverRequest()
	req.DefaultLabels = []string{"adr"}
	realADRs := []githubclient.IssueTitleResult{
		{Number: 300, Title: "[ADR-047] a decided one"},
		{Number: 301, Title: "[ADR-49] the max ADR"},
	}
	api := &fakeAPI{searchResultsFn: func(query string) []githubclient.IssueTitleResult {
		if strings.Contains(query, `label:"adr"`) && !strings.Contains(query, "in:title") {
			return realADRs
		}
		// A title-carrying query ALSO surfaces a fuzzy false positive lacking the
		// adr label, which would over-allocate off a bogus high number.
		return append([]githubclient.IssueTitleResult{
			{Number: 400, Title: "[ADR-77] a mis-included title without the adr label"},
		}, realADRs...)
	}}
	got, err := New(api).DiscoverNumbers(context.Background(), req)
	if err != nil {
		t.Fatalf("DiscoverNumbers: %v", err)
	}
	if !strings.Contains(api.searchQuery, `label:"adr"`) {
		t.Errorf("search query = %q, want it to carry the label:\"adr\" qualifier (production adr path)", api.searchQuery)
	}
	if strings.Contains(api.searchQuery, "in:title") {
		t.Errorf("search query = %q, want NO in:title term on the label-only adr query (#1523)", api.searchQuery)
	}
	max := 0
	for _, n := range got {
		if n > max {
			max = n
		}
	}
	if max != 49 {
		t.Errorf("max discovered = %d, want 49 (full ADR set via label:\"adr\", fuzzy false positive excluded → allocate 50); numbers=%v", max, got)
	}
}

// TestProvider_DiscoverNumbers_LabelQualifierHardened feeds a DefaultLabels
// value carrying a double quote and backslash and asserts the composed query
// keeps exactly the enclosing quotes of the label qualifier and no stray
// backslash — the breakout hardening in labelSearchQualifier, mirroring the
// in:title prefix hardening.
func TestProvider_DiscoverNumbers_LabelQualifierHardened(t *testing.T) {
	req := discoverRequest()
	req.DefaultLabels = []string{`ab"c\d`}
	api := &fakeAPI{searchResults: []githubclient.IssueTitleResult{
		{Number: 5, Title: "[ADR-12] a decided one"},
	}}
	got, err := New(api).DiscoverNumbers(context.Background(), req)
	if err != nil {
		t.Fatalf("DiscoverNumbers: %v", err)
	}
	if !strings.Contains(api.searchQuery, `label:"abcd"`) {
		t.Errorf("search query = %q, want a hardened label:\"abcd\" qualifier (quote/backslash stripped, enclosing quotes kept)", api.searchQuery)
	}
	if strings.Contains(api.searchQuery, `\`) {
		t.Errorf("search query = %q must not carry a backslash that could escape the closing quote", api.searchQuery)
	}
	if len(got) != 1 || got[0] != 12 {
		t.Errorf("numbers = %v, want [12] (regex still matches the real title)", got)
	}
}

// TestProvider_DiscoverNumbers_NoDefaultLabelsOmitsLabelQualifier pins the
// empty-DefaultLabels fall-through: a numbered type with no default label keeps
// the title-only query (NO label: qualifier), so a labelless numbered type is
// unaffected by the #1522 change.
func TestProvider_DiscoverNumbers_NoDefaultLabelsOmitsLabelQualifier(t *testing.T) {
	// discoverRequest() carries no DefaultLabels — the fall-through branch.
	api := &fakeAPI{searchResults: []githubclient.IssueTitleResult{
		{Number: 200, Title: "[ADR-041] a decision"},
	}}
	got, err := New(api).DiscoverNumbers(context.Background(), discoverRequest())
	if err != nil {
		t.Fatalf("DiscoverNumbers: %v", err)
	}
	if strings.Contains(api.searchQuery, "label:") {
		t.Errorf("search query = %q, want NO label: qualifier when DefaultLabels is empty", api.searchQuery)
	}
	if !strings.Contains(api.searchQuery, "in:title") {
		t.Errorf("search query = %q, want the in:title term on the labelless fall-through branch", api.searchQuery)
	}
	if len(got) != 1 || got[0] != 41 {
		t.Errorf("numbers = %v, want [41]", got)
	}
}

func TestProvider_DiscoverNumbers_MissingInstallationRejected(t *testing.T) {
	// Fail closed: a run-absent target leaves InstallationID 0; discovery must
	// error rather than dispatch an untokened search.
	req := discoverRequest()
	req.Target.InstallationID = 0
	api := &fakeAPI{}
	_, err := New(api).DiscoverNumbers(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "no installation id available") {
		t.Fatalf("want missing-installation error, got %v", err)
	}
	if api.searchQuery != "" {
		t.Errorf("search must not be dispatched without an installation id, got query %q", api.searchQuery)
	}
}

func TestProvider_DiscoverNumbers_SearchErrorPropagates(t *testing.T) {
	// Fail closed: a genuine search error propagates so the handler returns the
	// discovery_failed 422 rather than allocating off an empty result.
	api := &fakeAPI{searchErr: errors.New("search API rejected the query")}
	_, err := New(api).DiscoverNumbers(context.Background(), discoverRequest())
	if err == nil || !strings.Contains(err.Error(), "search issues by title") {
		t.Fatalf("want search error, got %v", err)
	}
}

// Provider must satisfy workmgmt.Provider and the optional board-sync
// (workmgmt.Transitioner) + number-discovery (workmgmt.NumberDiscoverer)
// capabilities.
var (
	_ workmgmt.Provider         = (*Provider)(nil)
	_ workmgmt.Transitioner     = (*Provider)(nil)
	_ workmgmt.NumberDiscoverer = (*Provider)(nil)
)
