package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
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
	// nodeIDs maps issue number -> node id, so a test can give the CHILD and
	// the PARENT epic DISTINCT node ids and assert which one AddSubIssue
	// received in which position. With one shared parentNode for every number,
	// reversing AddSubIssue's arguments is unobservable (#2237 review). An
	// absent number falls back to parentNode, so existing single-id fixtures
	// are unaffected.
	nodeIDs map[int]string

	subParent, subChild string
	subErr              error

	// issueParents maps issue number -> its structural sub-issue PARENT number,
	// the direction ListSubIssues walks the other way. IssueParent reads it, and
	// AddSubIssue RECORDS into it (resolving the parent node id back through
	// nodeIDs) so a re-apply observes the link exactly as the real forge does —
	// the fake mirrors a SHIPPED write and nothing more (#2237's
	// statefulGroomingForge lesson). issueParentErr, when set, is returned by
	// IssueParent for every number (the forge-refusal / transport branch).
	issueParents      map[int]int
	issueParentErr    error
	issueParentNumber int

	listSubParent  string
	listSubResults []githubclient.SubIssue
	listSubErr     error

	// getIssues maps issue number -> the canned issue GetIssue returns, so the
	// no-epic ResolveDependencies path can be driven per named issue. getIssueErr,
	// when set, is returned for every GetIssue call (the fetch-fails branch).
	getIssues   map[int]*githubclient.Issue
	getIssueErr error
	// getIssueErrs maps a specific issue number -> an error GetIssue returns for
	// THAT number only. When the same number is also in getIssues, GetIssue
	// returns (that issue, that error) — a NON-nil issue ALONGSIDE the error — so
	// the classifier's error branch can be counterfactually distinguished from
	// its nil-issue branch (#2953 operator condition 4): deleting the error
	// branch would then fall through to a completed issue and wrongly satisfy.
	getIssueErrs map[int]error
	// getIssueCalls records how many times GetIssue was called per number, so a
	// test can assert the duplicate-ref dedup fetches each named issue exactly once.
	getIssueCalls map[int]int

	searchQuery   string
	searchResults []githubclient.IssueTitleResult
	// searchResultsFn, when set, computes the results from the composed query so
	// a test can return different result sets depending on whether the query
	// carries a label: qualifier (the #1522 recency-burial regression guard). It
	// takes precedence over searchResults.
	searchResultsFn func(query string) []githubclient.IssueTitleResult
	searchErr       error

	// listRepoIssues is the canned enumeration ListRepoIssues returns, and
	// listRepoIssuesErr the error it returns instead. listRepoIssuesOpts /
	// listRepoIssuesRepo record the arguments so a test can assert the label and
	// state filters were forwarded verbatim (#2230).
	listRepoIssues     []githubclient.RepoIssue
	listRepoIssuesErr  error
	listRepoIssuesOpts githubclient.ListRepoIssuesOptions
	listRepoIssuesRepo githubclient.RepoRef
	// listRepoIssuesProjectsToken records whether the context ListRepoIssues was
	// called with carried the projects-token opt-in, so the user-owned-board
	// routing is assertable without the wire.
	listRepoIssuesProjectsToken bool

	// updateIssueCalls records every UpdateIssue call in order, so a grooming
	// test asserts on COMMITTED STATE (what was actually written, and what was
	// NOT written) rather than on a returned error — a containment check that
	// fired and a mutation that silently did nothing return the same envelope.
	updateIssueCalls  []fakeUpdateIssueCall
	updateIssueResult *githubclient.Issue
	updateIssueErr    error

	// addLabelsCalls records every AddIssueLabels call in order — the ADDITIVE
	// label primitive the grooming label mutation writes through. Recording the
	// call (rather than only the returned result) is what lets a test assert
	// that a wholesale UpdateIssue(labels) PATCH was NOT sent instead.
	addLabelsCalls []fakeAddLabelsCall
	addLabelsErr   error

	// addProjectItemProjectsToken / setFieldProjectsToken record whether the
	// context each board WRITE was called with carried the projects-token
	// opt-in, so the user-owned-board credential routing is assertable at the
	// write and not only at the reads (#2237 review; the #1114 constraint).
	addProjectItemProjectsToken bool
	setFieldProjectsToken       bool

	projectsTokenConfigured bool

	// mu guards the fake's own call-recording state. Only GetIssue is reached
	// concurrently (by ResolveDependencies' bounded pool, #3113); the other
	// members are driven from the test goroutine.
	mu sync.Mutex
}

// fakeAddLabelsCall is one recorded AddIssueLabels dispatch: the issue number
// and the labels sent, so a test asserts that ONLY the new labels crossed the
// wire — a union payload would mean the caller was still doing a
// read-modify-write.
type fakeAddLabelsCall struct {
	number int
	labels []string
}

func (f *fakeAPI) AddIssueLabels(_ context.Context, _ forge.CredentialScope, _ githubclient.RepoRef,
	number int, labels []string) ([]string, error) {
	f.addLabelsCalls = append(f.addLabelsCalls, fakeAddLabelsCall{number: number,
		labels: append([]string(nil), labels...)})
	if f.addLabelsErr != nil {
		return nil, f.addLabelsErr
	}
	existing := []string(nil)
	if iss, ok := f.getIssues[number]; ok {
		existing = append(existing, iss.Labels...)
	}
	return append(existing, labels...), nil
}

// fakeUpdateIssueCall is one recorded UpdateIssue dispatch: the issue number
// and the params verbatim, so a test can assert BOTH what was set and — the
// pointer-omission invariant — what was left nil.
type fakeUpdateIssueCall struct {
	number int
	params githubclient.UpdateIssueParams
}

func (f *fakeAPI) UpdateIssue(_ context.Context, _ forge.CredentialScope, _ githubclient.RepoRef,
	number int, p githubclient.UpdateIssueParams) (*githubclient.Issue, error) {
	f.updateIssueCalls = append(f.updateIssueCalls, fakeUpdateIssueCall{number: number, params: p})
	if f.updateIssueErr != nil {
		return nil, f.updateIssueErr
	}
	if f.updateIssueResult != nil {
		return f.updateIssueResult, nil
	}
	return &githubclient.Issue{Number: number}, nil
}

func (f *fakeAPI) ListRepoIssues(ctx context.Context, _ forge.CredentialScope, repo githubclient.RepoRef, opts githubclient.ListRepoIssuesOptions) ([]githubclient.RepoIssue, error) {
	f.listRepoIssuesRepo, f.listRepoIssuesOpts = repo, opts
	f.listRepoIssuesProjectsToken = githubclient.ProjectsTokenRequested(ctx)
	if f.listRepoIssuesErr != nil {
		return nil, f.listRepoIssuesErr
	}
	return f.listRepoIssues, nil
}

func (f *fakeAPI) CreateIssue(_ context.Context, _ forge.CredentialScope, repo githubclient.RepoRef, p githubclient.CreateIssueParams) (*githubclient.CreatedIssue, error) {
	f.createRepo, f.createParams = repo, p
	if f.createErr != nil {
		return nil, f.createErr
	}
	return f.created, nil
}

func (f *fakeAPI) IssueNodeID(_ context.Context, _ forge.CredentialScope, _ githubclient.RepoRef, number int) (string, error) {
	f.nodeIDNumber = number
	if f.nodeIDErr != nil {
		return "", f.nodeIDErr
	}
	if id, ok := f.nodeIDs[number]; ok {
		return id, nil
	}
	return f.parentNode, nil
}

func (f *fakeAPI) ProjectFields(_ context.Context, _ forge.CredentialScope, coord githubclient.ProjectCoord, fieldName string) (*githubclient.ProjectMeta, error) {
	f.fieldsCoord, f.fieldsName = coord, fieldName
	if f.fieldsErr != nil {
		return nil, f.fieldsErr
	}
	return f.meta, nil
}

func (f *fakeAPI) ProjectItemStatus(_ context.Context, _ forge.CredentialScope, issueNodeID, projectID, _ string) (*githubclient.ProjectItemStatus, error) {
	f.itemStatusIssueNode, f.itemStatusProjectID = issueNodeID, projectID
	if f.itemStatusErr != nil {
		return nil, f.itemStatusErr
	}
	return f.itemStatus, nil
}

func (f *fakeAPI) AddProjectItem(ctx context.Context, _ forge.CredentialScope, projectID, contentID string) (string, error) {
	f.addItemContent = contentID
	f.addProjectItemProjectsToken = githubclient.ProjectsTokenRequested(ctx)
	_ = projectID
	if f.addItemErr != nil {
		return "", f.addItemErr
	}
	return f.itemID, nil
}

func (f *fakeAPI) SetProjectItemSingleSelect(ctx context.Context, _ forge.CredentialScope, projectID, itemID, fieldID, optionID string) error {
	f.setProjectID, f.setItemID, f.setFieldID, f.setOptionID = projectID, itemID, fieldID, optionID
	f.setFieldProjectsToken = githubclient.ProjectsTokenRequested(ctx)
	return f.setErr
}

func (f *fakeAPI) AddSubIssue(_ context.Context, _ forge.CredentialScope, parentNodeID, childNodeID string) error {
	f.subParent, f.subChild = parentNodeID, childNodeID
	if f.subErr != nil {
		return f.subErr
	}
	// Record the edge into issueParents so a re-apply's IssueParent observes it,
	// exactly as the real forge would. Resolve BOTH node ids back to their issue
	// numbers through nodeIDs (the inverse of IssueNodeID), so a genuinely
	// missing edge stays missing and only a real write shows up on the read side.
	child, childOK := f.numberForNode(childNodeID)
	parent, parentOK := f.numberForNode(parentNodeID)
	if childOK && parentOK {
		if f.issueParents == nil {
			f.issueParents = map[int]int{}
		}
		f.issueParents[child] = parent
	}
	return nil
}

// numberForNode inverts nodeIDs: node id -> issue number. It lets AddSubIssue
// record the structural edge in the same number-keyed shape IssueParent reads.
func (f *fakeAPI) numberForNode(nodeID string) (int, bool) {
	for n, id := range f.nodeIDs {
		if id == nodeID {
			return n, true
		}
	}
	return 0, false
}

// IssueParent resolves the structural sub-issue parent, mirroring
// *githubclient.Client.IssueParent: a number with no recorded parent (or a
// recorded parent <= 0) is the NORMAL nil answer, not a failure.
func (f *fakeAPI) IssueParent(_ context.Context, _ forge.CredentialScope, _ githubclient.RepoRef, number int) (*githubclient.IssueParent, error) {
	f.issueParentNumber = number
	if f.issueParentErr != nil {
		return nil, f.issueParentErr
	}
	if parent, ok := f.issueParents[number]; ok && parent > 0 {
		return &githubclient.IssueParent{Number: parent, Title: fmt.Sprintf("epic #%d", parent)}, nil
	}
	return nil, nil
}

func (f *fakeAPI) ListSubIssues(_ context.Context, _ forge.CredentialScope, parentNodeID string) ([]githubclient.SubIssue, error) {
	f.listSubParent = parentNodeID
	if f.listSubErr != nil {
		return nil, f.listSubErr
	}
	return f.listSubResults, nil
}

func (f *fakeAPI) SearchIssuesByTitle(_ context.Context, _ forge.CredentialScope, query string) ([]githubclient.IssueTitleResult, error) {
	f.searchQuery = query
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	if f.searchResultsFn != nil {
		return f.searchResultsFn(query), nil
	}
	return f.searchResults, nil
}

func (f *fakeAPI) GetIssue(_ context.Context, _ forge.CredentialScope, _ githubclient.RepoRef, number int) (*githubclient.Issue, error) {
	// ResolveDependencies fetches with bounded CONCURRENCY since #3113, so the
	// call-count bookkeeping this fake keeps is itself shared mutable state. The
	// mutex is the FAKE's own bookkeeping lock, not a lock in the production
	// path — the production path has no shared mutable state across goroutines
	// at all (workers return values over a channel and the parent merges).
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getIssueCalls == nil {
		f.getIssueCalls = map[int]int{}
	}
	f.getIssueCalls[number]++
	// A per-number error may accompany a canned issue: return BOTH so the
	// classifier's error branch is independently observable (#2953 condition 4).
	if err, ok := f.getIssueErrs[number]; ok && err != nil {
		return f.getIssues[number], err
	}
	if f.getIssueErr != nil {
		return nil, f.getIssueErr
	}
	if iss, ok := f.getIssues[number]; ok {
		return iss, nil
	}
	return nil, errors.New("githubclient: not found")
}

func (f *fakeAPI) ProjectsTokenConfigured() bool { return f.projectsTokenConfigured }

// noIssueParentAPI satisfies API by embedding it and deliberately does NOT
// implement the optional issueParentReader extension — its promoted method set
// is exactly API's, so IssueParent is absent and the reader/mutator must refuse
// with ReasonNotImplemented rather than fall back to the body marker (#2952).
// Mirrors noLabelAdderAPI. The embedded value is nil because no method is
// reached: the refusal is decided by the failed type assertion before any call.
type noIssueParentAPI struct{ API }

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
			Scope:   forge.FromGitHubInstallationID(99),
			Repo:    workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"},
			Project: &workmgmt.Project{Owner: "kuhlman-labs", OwnerType: "user", Number: 7},
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
			// out-of-order on purpose: EpicChildren sorts ascending. State/
			// stateReason exercise the Complete matrix: #41 CLOSED+COMPLETED
			// (complete), #42 OPEN (not complete), #43 CLOSED+NOT_PLANNED (closed
			// but its work did not land → not complete).
			{Number: 42, NodeID: "N42", Title: "slice B", Body: "## Summary\n\nDepends on: #41\n", Labels: []string{"type:feature", "autonomy:medium"}, State: "OPEN"},
			{Number: 41, NodeID: "N41", Title: "slice A", Body: "## Summary\n\nno deps\n", Labels: []string{"autonomy:low"}, State: "CLOSED", StateReason: "COMPLETED"},
			{Number: 43, NodeID: "N43", Title: "slice C", Body: "Depends on: #41, #42, #999\n", State: "CLOSED", StateReason: "NOT_PLANNED"},
		},
		// #999 is an out-of-epic (non-child) target the #43 body references. It is
		// classified by reading its state (#2953): OPEN → DropNotChild, preserving
		// the pre-#2953 "not a fellow child" refusal for a genuine cross-epic
		// dependency that has not landed.
		getIssues: map[int]*githubclient.Issue{
			999: {Number: 999, Title: "cross-epic", State: "open"},
		},
	}
	res, err := New(api).EpicChildren(context.Background(), workmgmt.EpicChildrenRequest{
		Target: workmgmt.Target{Scope: forge.FromGitHubInstallationID(99), Repo: workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"}},
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
	// Autonomy is parsed off each child's `autonomy:<tier>` label: #41 low,
	// #42 medium (its non-autonomy type:feature label is ignored), #43 unlabeled
	// -> "" (#1551).
	wantAutonomy := map[int]string{41: "low", 42: "medium", 43: ""}
	for _, c := range res.Children {
		if c.Autonomy != wantAutonomy[c.Number] {
			t.Errorf("child #%d Autonomy = %q, want %q", c.Number, c.Autonomy, wantAutonomy[c.Number])
		}
	}
	// Complete is true ONLY for the closed+completed child (#41). #42 is open
	// and #43 is closed-as-not_planned, so neither is complete (#2120).
	wantComplete := map[int]bool{41: true, 42: false, 43: false}
	for _, c := range res.Children {
		if c.Complete != wantComplete[c.Number] {
			t.Errorf("child #%d Complete = %v, want %v", c.Number, c.Complete, wantComplete[c.Number])
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
	// The mis-targeted #999 reference lands in DroppedEdges stamped
	// DropNotChild so assembly can fail closed on it (keeping the "not a fellow
	// child" wording) rather than the provider silently dropping it.
	wantDropped := []workmgmt.DependsEdge{{From: 43, To: 999, Reason: workmgmt.DropNotChild}}
	if len(res.DroppedEdges) != len(wantDropped) {
		t.Fatalf("dropped edges = %+v, want %+v (#999 must be surfaced)", res.DroppedEdges, wantDropped)
	}
	for i, e := range res.DroppedEdges {
		if e != wantDropped[i] {
			t.Errorf("dropped edge[%d] = %+v, want %+v", i, e, wantDropped[i])
		}
	}
}

// TestProvider_EpicChildren_CompletionThreadsToSubsetDrop is the binding
// source-to-consumer test (#2120, operator condition 1a): it drives the REAL
// EpicChildren mapping from a faked ListSubIssues carrying a CLOSED+COMPLETED
// child and an OPEN (incomplete) child, then feeds the mapped result straight
// into campaign.FilterToSubset — so SubIssue.State/StateReason →
// EpicChild.Complete → the subset-drop classification is exercised together in
// one flow, not only per-layer with a Complete-preset fake. It proves the two
// endpoints of the fix: an included item depending on an EXCLUDED-but-COMPLETED
// sibling is a satisfied (silently-dropped) dependency, while depending on an
// EXCLUDED-but-INCOMPLETE sibling is still dangling.
func TestProvider_EpicChildren_CompletionThreadsToSubsetDrop(t *testing.T) {
	api := &fakeAPI{
		parentNode: "EPIC_NODE",
		listSubResults: []githubclient.SubIssue{
			// #100: closed-and-completed → Complete. The already-merged wave-0
			// dependency the natural "campaign the remaining open children" call
			// excludes from the subset.
			{Number: 100, NodeID: "N100", Title: "done dep", Body: "no deps", State: "CLOSED", StateReason: "COMPLETED"},
			// #101: open, depends on the completed #100.
			{Number: 101, NodeID: "N101", Title: "open A", Body: "Depends on: #100", State: "OPEN"},
			// #103: open (incomplete) — the not-yet-done sibling.
			{Number: 103, NodeID: "N103", Title: "open dep", Body: "no deps", State: "OPEN"},
			// #104: open, depends on the incomplete #103.
			{Number: 104, NodeID: "N104", Title: "open B", Body: "Depends on: #103", State: "OPEN"},
		},
	}
	res, err := New(api).EpicChildren(context.Background(), workmgmt.EpicChildrenRequest{
		Target: workmgmt.Target{Scope: forge.FromGitHubInstallationID(99), Repo: workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"}},
		Epic:   "#99",
	})
	if err != nil {
		t.Fatalf("EpicChildren: %v", err)
	}

	// Included #101 depends on EXCLUDED #100, which the mapping marked Complete:
	// the edge is a satisfied dependency, dropped silently, and Assemble
	// succeeds over just {101}.
	t.Run("excluded_complete_dependency_satisfied", func(t *testing.T) {
		sub, err := campaign.FilterToSubset(res, []string{"issue:101"})
		if err != nil {
			t.Fatalf("FilterToSubset: %v", err)
		}
		if len(sub.DroppedEdges) != 0 {
			t.Fatalf("DroppedEdges = %+v, want none (excluded #100 is complete → satisfied)", sub.DroppedEdges)
		}
		if _, err := campaign.Assemble("issue:99", sub); err != nil {
			t.Fatalf("Assemble(subset {101}) = %v, want success", err)
		}
	})

	// Included #104 depends on EXCLUDED #103, which is open (not complete): the
	// edge is still dangling, so Assemble fails closed.
	t.Run("excluded_incomplete_dependency_dangling", func(t *testing.T) {
		sub, err := campaign.FilterToSubset(res, []string{"issue:104"})
		if err != nil {
			t.Fatalf("FilterToSubset: %v", err)
		}
		if len(sub.DroppedEdges) != 1 || sub.DroppedEdges[0].Reason != workmgmt.DropExcludedIncomplete {
			t.Fatalf("DroppedEdges = %+v, want one DropExcludedIncomplete edge", sub.DroppedEdges)
		}
		if _, err := campaign.Assemble("issue:99", sub); !errors.Is(err, campaign.ErrDanglingDependency) {
			t.Fatalf("Assemble(subset {104}) = %v, want ErrDanglingDependency", err)
		}
	})
}

// TestProvider_EpicChildren_CrossPageDependencyIsEdgeNotDropped is the #2102
// done-means at the consumer layer: once ListSubIssues paginates and returns
// the COMPLETE child set, a depends_on reference between two children that
// conceptually straddle a page boundary (child A on page 1, child B on page 2)
// classifies into Edges, NOT DroppedEdges. Before pagination, B would have been
// truncated at the first :100 page, failing the isChild test exactly like a
// typo'd number and landing a legitimate edge in DroppedEdges — feeding campaign
// assembly a partial DAG. The fake ListSubIssues returns the complete set (as
// pagination now guarantees), so a complete child set yields a complete edge
// set.
func TestProvider_EpicChildren_CrossPageDependencyIsEdgeNotDropped(t *testing.T) {
	api := &fakeAPI{
		parentNode: "EPIC_NODE",
		listSubResults: []githubclient.SubIssue{
			// #41: the "page 1" child. #142: the "page 2" overflow child whose body
			// depends on #41 — the edge that only survives once the second page is read.
			{Number: 41, NodeID: "N41", Title: "slice A", Body: "## Summary\n\nno deps\n", State: "OPEN"},
			{Number: 142, NodeID: "N142", Title: "slice B", Body: "## Summary\n\nDepends on: #41\n", State: "OPEN"},
		},
	}
	res, err := New(api).EpicChildren(context.Background(), workmgmt.EpicChildrenRequest{
		Target: workmgmt.Target{Scope: forge.FromGitHubInstallationID(99), Repo: workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"}},
		Epic:   "#1005",
	})
	if err != nil {
		t.Fatalf("EpicChildren: %v", err)
	}
	if len(res.Children) != 2 {
		t.Fatalf("children = %+v, want both 41 and 142 (complete set)", res.Children)
	}
	// The cross-page dependency is a real edge: 142 -> 41 in Edges, and nothing
	// dropped — the complete child set makes #41 a recognized sibling of #142.
	want := []workmgmt.DependsEdge{{From: 142, To: 41}}
	if len(res.Edges) != len(want) || res.Edges[0] != want[0] {
		t.Fatalf("edges = %+v, want %+v (cross-page dep classified as an edge)", res.Edges, want)
	}
	if len(res.DroppedEdges) != 0 {
		t.Errorf("dropped edges = %+v, want none (the overflow sibling is a child, not a dangling target)", res.DroppedEdges)
	}
}

// TestParseAutonomyLabel covers the tier extraction: the first autonomy:<tier>
// label's suffix wins, a non-autonomy label is ignored, no autonomy label
// yields "" (unknown/default), and an out-of-set tier normalizes to "" so a
// mislabeled child degrades to the non-human-led default (matching the
// fail-closed campaign_items.autonomy CHECK) rather than reaching Persist as a
// value the CHECK rejects.
func TestParseAutonomyLabel(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   string
	}{
		{"low", []string{"type:feature", "autonomy:low"}, "low"},
		{"medium", []string{"autonomy:medium"}, "medium"},
		{"high", []string{"autonomy:high", "area:backend"}, "high"},
		{"unlabeled", []string{"type:bug", "area:server"}, ""},
		{"nil labels", nil, ""},
		{"first autonomy wins", []string{"autonomy:high", "autonomy:low"}, "high"},
		{"known tier passes through", []string{"autonomy:low"}, "low"},
		{"out-of-set tier normalizes to empty", []string{"autonomy:bogus"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseAutonomyLabel(tc.labels); got != tc.want {
				t.Errorf("parseAutonomyLabel(%v) = %q, want %q", tc.labels, got, tc.want)
			}
		})
	}
}

// TestProvider_EpicChildren_FailClosed covers the defensive branches: a nil
// API, a missing repo, a zero installation, a malformed epic ref, and a
// ListSubIssues error each return an error rather than a partial result.
func TestProvider_EpicChildren_FailClosed(t *testing.T) {
	good := workmgmt.EpicChildrenRequest{
		Target: workmgmt.Target{Scope: forge.FromGitHubInstallationID(99), Repo: workmgmt.Repo{Owner: "o", Name: "r"}},
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
		req.Target.Scope = forge.CredentialScope{}
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

// resolveReq builds an IssueSetRequest with a valid target + the given items.
func resolveReq(items ...string) workmgmt.IssueSetRequest {
	return workmgmt.IssueSetRequest{
		Target: workmgmt.Target{Scope: forge.FromGitHubInstallationID(99), Repo: workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"}},
		Items:  items,
	}
}

// TestProvider_ResolveDependencies covers the no-epic issue-set source (#2051):
// each named issue's depends_on marker is resolved directly, an in-set ref
// becomes an Edge, an out-of-set ref becomes a DroppedEdge stamped DropNotChild,
// the autonomy label is parsed, and Complete is CLOSED+COMPLETED (case-insensitive
// on GetIssue's lowercase REST enums). It also asserts children come back
// ascending and both edge slices are deterministically sorted.
func TestProvider_ResolveDependencies(t *testing.T) {
	t.Run("single marker in-set is an edge", func(t *testing.T) {
		api := &fakeAPI{getIssues: map[int]*githubclient.Issue{
			100: {Number: 100, Title: "root", Body: "no deps", State: "open"},
			101: {Number: 101, Title: "leaf", Body: "Depends on: #100", State: "open"},
		}}
		res, err := New(api).ResolveDependencies(context.Background(), resolveReq("issue:101", "100"))
		if err != nil {
			t.Fatalf("ResolveDependencies: %v", err)
		}
		// Children ascending: 100 then 101 (regardless of item order).
		if len(res.Children) != 2 || res.Children[0].Number != 100 || res.Children[1].Number != 101 {
			t.Fatalf("children = %+v, want [100 101] ascending", res.Children)
		}
		want := []workmgmt.DependsEdge{{From: 101, To: 100}}
		if len(res.Edges) != 1 || res.Edges[0] != want[0] {
			t.Fatalf("edges = %+v, want %+v", res.Edges, want)
		}
		if len(res.DroppedEdges) != 0 {
			t.Errorf("dropped = %+v, want none (100 is in the named set)", res.DroppedEdges)
		}
	})

	t.Run("multiple markers, some in-set some out-of-set", func(t *testing.T) {
		api := &fakeAPI{getIssues: map[int]*githubclient.Issue{
			100: {Number: 100, Title: "a", Body: "no deps", State: "open"},
			101: {Number: 101, Title: "b", Body: "no deps", State: "open"},
			102: {Number: 102, Title: "c", Body: "Depends on: #100, #101, #999", State: "open"},
			// #999 is out of the named set and OPEN -> classified DropNotChild
			// (#2953: an open out-of-set target keeps the pre-#2953 refusal).
			999: {Number: 999, Title: "outside", State: "open"},
		}}
		res, err := New(api).ResolveDependencies(context.Background(), resolveReq("issue:100", "issue:101", "issue:102"))
		if err != nil {
			t.Fatalf("ResolveDependencies: %v", err)
		}
		// In-set edges 102->100 and 102->101, sorted by (From,To).
		wantEdges := []workmgmt.DependsEdge{{From: 102, To: 100}, {From: 102, To: 101}}
		if len(res.Edges) != len(wantEdges) || res.Edges[0] != wantEdges[0] || res.Edges[1] != wantEdges[1] {
			t.Fatalf("edges = %+v, want %+v", res.Edges, wantEdges)
		}
		// #999 is not in the named set and OPEN -> dropped, stamped DropNotChild.
		wantDropped := []workmgmt.DependsEdge{{From: 102, To: 999, Reason: workmgmt.DropNotChild}}
		if len(res.DroppedEdges) != 1 || res.DroppedEdges[0] != wantDropped[0] {
			t.Fatalf("dropped = %+v, want %+v", res.DroppedEdges, wantDropped)
		}
	})

	t.Run("no marker yields no edges", func(t *testing.T) {
		api := &fakeAPI{getIssues: map[int]*githubclient.Issue{
			100: {Number: 100, Title: "a", Body: "## Summary\n\nplain body\n", State: "open"},
		}}
		res, err := New(api).ResolveDependencies(context.Background(), resolveReq("100"))
		if err != nil {
			t.Fatalf("ResolveDependencies: %v", err)
		}
		if len(res.Children) != 1 || len(res.Edges) != 0 || len(res.DroppedEdges) != 0 {
			t.Fatalf("res = %+v, want one child no edges", res)
		}
	})

	t.Run("autonomy label parsed and Complete from closed+completed", func(t *testing.T) {
		api := &fakeAPI{getIssues: map[int]*githubclient.Issue{
			// closed+completed (lowercase REST enums) -> Complete; autonomy:low label parsed.
			100: {Number: 100, Title: "done", Body: "no deps", State: "closed", StateReason: "completed", Labels: []string{"type:feature", "autonomy:low"}},
			// closed as not_planned -> NOT complete.
			101: {Number: 101, Title: "abandoned", Body: "no deps", State: "closed", StateReason: "not_planned"},
			// open, no autonomy label.
			102: {Number: 102, Title: "open", Body: "no deps", State: "open"},
		}}
		res, err := New(api).ResolveDependencies(context.Background(), resolveReq("100", "101", "102"))
		if err != nil {
			t.Fatalf("ResolveDependencies: %v", err)
		}
		byNum := map[int]workmgmt.EpicChild{}
		for _, c := range res.Children {
			byNum[c.Number] = c
		}
		if !byNum[100].Complete {
			t.Errorf("#100 Complete = false, want true (closed+completed)")
		}
		if byNum[100].Autonomy != "low" {
			t.Errorf("#100 Autonomy = %q, want low", byNum[100].Autonomy)
		}
		if byNum[101].Complete {
			t.Errorf("#101 Complete = true, want false (closed as not_planned)")
		}
		if byNum[102].Complete {
			t.Errorf("#102 Complete = true, want false (open)")
		}
	})

	t.Run("duplicate ref resolves the issue once", func(t *testing.T) {
		api := &fakeAPI{getIssues: map[int]*githubclient.Issue{
			101: {Number: 101, Title: "dup", Body: "no deps", State: "open"},
		}}
		// The same issue named twice, once bare and once issue:-prefixed — both
		// resolve to 101, exercising the inSet dedup branch.
		res, err := New(api).ResolveDependencies(context.Background(), resolveReq("101", "issue:101"))
		if err != nil {
			t.Fatalf("ResolveDependencies: %v", err)
		}
		if len(res.Children) != 1 || res.Children[0].Number != 101 {
			t.Fatalf("children = %+v, want exactly one child #101 (duplicate deduped)", res.Children)
		}
		if got := api.getIssueCalls[101]; got != 1 {
			t.Errorf("GetIssue(#101) called %d times, want exactly 1 (each named issue fetched once)", got)
		}
	})
}

// TestProvider_ResolveDependencies_FailClosed covers the defensive branches: a
// nil API, a missing repo, a zero installation, an unparseable item ref, and a
// GetIssue error each return an error rather than a partial result.
func TestProvider_ResolveDependencies_FailClosed(t *testing.T) {
	good := resolveReq("issue:100")
	t.Run("nil api", func(t *testing.T) {
		if _, err := (&Provider{}).ResolveDependencies(context.Background(), good); err == nil || !strings.Contains(err.Error(), "missing API client") {
			t.Fatalf("want missing-API error, got %v", err)
		}
	})
	t.Run("missing repo", func(t *testing.T) {
		req := good
		req.Target.Repo = workmgmt.Repo{}
		if _, err := New(&fakeAPI{}).ResolveDependencies(context.Background(), req); err == nil || !strings.Contains(err.Error(), "repo owner and name required") {
			t.Fatalf("want repo-required error, got %v", err)
		}
	})
	t.Run("zero installation", func(t *testing.T) {
		req := good
		req.Target.Scope = forge.CredentialScope{}
		if _, err := New(&fakeAPI{}).ResolveDependencies(context.Background(), req); err == nil || !strings.Contains(err.Error(), "no installation id available") {
			t.Fatalf("want missing-installation error, got %v", err)
		}
	})
	t.Run("unparseable item ref", func(t *testing.T) {
		if _, err := New(&fakeAPI{}).ResolveDependencies(context.Background(), resolveReq("not-a-ref")); err == nil || !strings.Contains(err.Error(), "not a numeric issue reference") {
			t.Fatalf("want malformed-item error, got %v", err)
		}
	})
	t.Run("get issue error", func(t *testing.T) {
		api := &fakeAPI{getIssueErr: errors.New("github rejected the request")}
		if _, err := New(api).ResolveDependencies(context.Background(), good); err == nil || !strings.Contains(err.Error(), "get issue #100") {
			t.Fatalf("want get-issue error, got %v", err)
		}
	})
}

// resolveResult drives the REAL ResolveDependencies over a two-issue set where
// the in-set item #from depends on the out-of-set target #to, whose canned state
// is (state, stateReason). It returns the resolved result so a test can assert
// how the out-of-set target was classified (#2953).
func resolveResult(t *testing.T, from, to int, toState, toReason string) *workmgmt.EpicChildrenResult {
	t.Helper()
	api := &fakeAPI{getIssues: map[int]*githubclient.Issue{
		from: {Number: from, Title: "in-set", Body: "Depends on: #" + strconv.Itoa(to), State: "open"},
		to:   {Number: to, Title: "target", State: toState, StateReason: toReason},
	}}
	res, err := New(api).ResolveDependencies(context.Background(), resolveReq(strconv.Itoa(from)))
	if err != nil {
		t.Fatalf("ResolveDependencies: %v", err)
	}
	return res
}

// TestResolveDependenciesClosedCompletedTargetSatisfied is the DONE-MEANS
// behavioral test (#2953): the issue's reproducer shape — an in-set item (#2032)
// whose body carries `Depends on: #1639` where #1639 is CLOSED/COMPLETED and out
// of set — assembles instead of failing closed. DroppedEdges is EMPTY,
// SatisfiedEdges carries exactly that edge with the OBSERVED state, and
// campaign.Assemble over the result SUCCEEDS with the elided target absent from
// every item's DependsOn.
//
// COUNTERFACTUAL (operator condition 4 / plan B1): delete the closed-and-
// completed satisfied branch in classifyOutOfSetTarget → every out-of-set target
// drops → this test goes RED.
func TestResolveDependenciesClosedCompletedTargetSatisfied(t *testing.T) {
	res := resolveResult(t, 2032, 1639, "closed", "completed")
	if len(res.DroppedEdges) != 0 {
		t.Fatalf("DroppedEdges = %+v, want none (target closed+completed → satisfied)", res.DroppedEdges)
	}
	want := workmgmt.SatisfiedEdge{From: 2032, To: 1639, State: "closed", StateReason: "completed"}
	if len(res.SatisfiedEdges) != 1 || res.SatisfiedEdges[0] != want {
		t.Fatalf("SatisfiedEdges = %+v, want [%+v]", res.SatisfiedEdges, want)
	}
	// The elided target must not become a DAG dependency of any item.
	asm, err := campaign.Assemble("", res)
	if err != nil {
		t.Fatalf("Assemble over a satisfied-edge result: %v", err)
	}
	for _, it := range asm.Items {
		if len(it.DependsOn) != 0 {
			t.Errorf("item %s DependsOn = %v, want none (satisfied edge excluded from the DAG)", it.IssueRef, it.DependsOn)
		}
	}
	// The satisfied edge is carried onto the assembly for surfacing.
	if len(asm.SatisfiedDependencies) != 1 || asm.SatisfiedDependencies[0] != want {
		t.Errorf("Assembly.SatisfiedDependencies = %+v, want [%+v]", asm.SatisfiedDependencies, want)
	}
}

// TestResolveDependenciesOpenTargetStillRefuses is the preserved-refusal half of
// done-means: an OPEN out-of-set target keeps DropNotChild, so a genuine
// unshipped cross-batch dependency still fails assembly closed.
//
// COUNTERFACTUAL: delete the open-target arm's DropNotChild stamp → this goes RED.
func TestResolveDependenciesOpenTargetStillRefuses(t *testing.T) {
	res := resolveResult(t, 2032, 1639, "open", "")
	if len(res.SatisfiedEdges) != 0 {
		t.Fatalf("SatisfiedEdges = %+v, want none (open target is not satisfied)", res.SatisfiedEdges)
	}
	want := workmgmt.DependsEdge{From: 2032, To: 1639, Reason: workmgmt.DropNotChild}
	if len(res.DroppedEdges) != 1 || res.DroppedEdges[0] != want {
		t.Fatalf("DroppedEdges = %+v, want [%+v]", res.DroppedEdges, want)
	}
}

// TestResolveDependenciesClosedNotPlannedRefuses: a closed-but-not-completed
// (not_planned) out-of-set target is DropTargetClosedIncomplete — its work did
// not land, so the dependency is genuinely unsatisfied.
//
// COUNTERFACTUAL: delete the closed-but-not-completed branch (so a not_planned
// close is treated as satisfied) → this goes RED.
func TestResolveDependenciesClosedNotPlannedRefuses(t *testing.T) {
	res := resolveResult(t, 1642, 1641, "closed", "not_planned")
	if len(res.SatisfiedEdges) != 0 {
		t.Fatalf("SatisfiedEdges = %+v, want none (not_planned is not satisfied)", res.SatisfiedEdges)
	}
	want := workmgmt.DependsEdge{From: 1642, To: 1641, Reason: workmgmt.DropTargetClosedIncomplete}
	if len(res.DroppedEdges) != 1 || res.DroppedEdges[0] != want {
		t.Fatalf("DroppedEdges = %+v, want [%+v]", res.DroppedEdges, want)
	}
}

// TestResolveDependenciesUnreadableTargetRefuses: a GetIssue ERROR on the
// out-of-set target is DropTargetStateUnreadable — never satisfied. The fake
// returns a NON-nil closed+completed issue ALONGSIDE the error, so the error
// branch is independently observable (#2953 condition 4): deleting the error
// branch would fall through to that completed issue and wrongly satisfy, which
// this assertion (DroppedEdges non-empty + DropTargetStateUnreadable) catches.
//
// COUNTERFACTUAL: delete the GetIssue-error branch → this goes RED (it would
// otherwise satisfy on the non-nil completed issue the fake returns).
func TestResolveDependenciesUnreadableTargetRefuses(t *testing.T) {
	api := &fakeAPI{
		getIssues: map[int]*githubclient.Issue{
			2032: {Number: 2032, Title: "in-set", Body: "Depends on: #1639", State: "open"},
			// A NON-nil closed+completed issue returned alongside the error below:
			// if the error branch is deleted, the classifier would satisfy on this.
			1639: {Number: 1639, Title: "target", State: "closed", StateReason: "completed"},
		},
		getIssueErrs: map[int]error{1639: errors.New("github rejected the read")},
	}
	res, err := New(api).ResolveDependencies(context.Background(), resolveReq("2032"))
	if err != nil {
		t.Fatalf("ResolveDependencies: %v", err)
	}
	if len(res.SatisfiedEdges) != 0 {
		t.Fatalf("SatisfiedEdges = %+v, want none (unreadable target is never satisfied)", res.SatisfiedEdges)
	}
	want := workmgmt.DependsEdge{From: 2032, To: 1639, Reason: workmgmt.DropTargetStateUnreadable}
	if len(res.DroppedEdges) != 1 || res.DroppedEdges[0] != want {
		t.Fatalf("DroppedEdges = %+v, want [%+v]", res.DroppedEdges, want)
	}
}

// TestResolveDependenciesNilIssueRefuses: GetIssue returning (nil, nil) for the
// out-of-set target is DropTargetStateUnreadable via the nil-issue guard. The
// (nil, nil) is seeded BY CONSTRUCTION (the number is absent from getIssues, so
// the fake's default returns not-found... so we route through getIssueErrs=nil
// but present in getIssues=nil): here we use a target number present in NEITHER
// map but forced nil via a dedicated fake path.
//
// COUNTERFACTUAL: delete the nil-issue guard → a nil deref panics → RED.
func TestResolveDependenciesNilIssueRefuses(t *testing.T) {
	api := &fakeAPI{getIssues: map[int]*githubclient.Issue{
		2032: {Number: 2032, Title: "in-set", Body: "Depends on: #1639", State: "open"},
		// #1639 maps to a nil issue with NO error — the (nil, nil) forge return.
		1639: nil,
	}}
	res, err := New(api).ResolveDependencies(context.Background(), resolveReq("2032"))
	if err != nil {
		t.Fatalf("ResolveDependencies: %v", err)
	}
	if len(res.SatisfiedEdges) != 0 {
		t.Fatalf("SatisfiedEdges = %+v, want none (nil issue is never satisfied)", res.SatisfiedEdges)
	}
	want := workmgmt.DependsEdge{From: 2032, To: 1639, Reason: workmgmt.DropTargetStateUnreadable}
	if len(res.DroppedEdges) != 1 || res.DroppedEdges[0] != want {
		t.Fatalf("DroppedEdges = %+v, want [%+v]", res.DroppedEdges, want)
	}
}

// TestClassifyOutOfSetTargetInvalidRefFailsClosed pins operator condition 2 at the
// classifier unit boundary: the classifier calls GetIssue ONLY for a Resolvable
// positive same-repo numeric target. An UNRESOLVABLE ref (a cross-repo /
// owner-qualified / unparseable token) and a synthetic non-positive number both
// stamp DropTargetStateUnreadable WITHOUT any forge call — calling GetIssue with a
// locally-reduced/wrong target could read an unrelated issue's state and FALSELY
// satisfy the edge. This is the UNIT half; the real cross-repo marker is driven
// through both provider paths in TestResolveDependenciesCrossRepoRefFailsClosed /
// TestEpicChildrenCrossRepoRefFailsClosed.
//
// COUNTERFACTUAL (unresolvable arm): delete the `!ref.Resolvable` guard → an
// unresolvable ref (Number 0) falls to the number<=0 guard, still no forge call,
// so this unit test alone would stay green — which is exactly why the provider-path
// tests below are load-bearing. COUNTERFACTUAL (number guard): delete the
// number<=0 guard → GetIssue is called with 0/negative and the zero-call assertion
// goes RED.
func TestClassifyOutOfSetTargetInvalidRefFailsClosed(t *testing.T) {
	api := &fakeAPI{}
	repo := forge.RepoRef{Owner: "kuhlman-labs", Name: "fishhawk"}
	refs := []dependsOnRef{
		{Resolvable: false},            // a cross-repo / unparseable ref.
		{Number: 0, Resolvable: true},  // defensive: a resolvable-but-zero number.
		{Number: -7, Resolvable: true}, // defensive: a resolvable-but-negative number.
	}
	for _, ref := range refs {
		cache := map[int]targetState{}
		ts, err := New(api).classifyOutOfSetTarget(context.Background(), forge.FromGitHubInstallationID(99), repo, ref, cache)
		if err != nil {
			t.Fatalf("classifyOutOfSetTarget(%+v): %v", ref, err)
		}
		if ts.satisfied || ts.reason != workmgmt.DropTargetStateUnreadable {
			t.Errorf("classify(%+v) = %+v, want unsatisfied DropTargetStateUnreadable", ref, ts)
		}
	}
	if len(api.getIssueCalls) != 0 {
		t.Errorf("GetIssue called %v times, want ZERO for an unresolvable/non-positive ref (no forge read on an unresolvable target)", api.getIssueCalls)
	}
}

// TestResolveDependenciesCrossRepoRefFailsClosed drives a REAL ResolveDependencies
// provider path with an in-set item whose body carries a CROSS-REPO depends_on ref
// (`Depends on: other/repo#1639`) where #1639 ALSO exists as a closed+completed
// SAME-REPO issue. It is the load-bearing proof for the routed high+medium security
// concerns and operator condition 2: the untrusted marker boundary must NOT reduce
// the cross-repo token to the local number 1639 and read that unrelated issue's
// state to falsely satisfy the edge. Asserts ZERO GetIssue(#1639), no SatisfiedEdge,
// and a DropTargetStateUnreadable DroppedEdge from the depending item.
//
// COUNTERFACTUAL: revert parseDependsOnMarker to SKIP non-numeric tokens (the pre-
// fixup behavior) → the cross-repo edge VANISHES → DroppedEdges empty → this goes
// RED. Alternatively, make the parser extract 1639 from the cross-repo token →
// GetIssue(#1639) is called and a SatisfiedEdge appears → also RED.
func TestResolveDependenciesCrossRepoRefFailsClosed(t *testing.T) {
	api := &fakeAPI{getIssues: map[int]*githubclient.Issue{
		2032: {Number: 2032, Title: "in-set", Body: "Depends on: other/repo#1639", State: "open"},
		// A closed+completed SAME-REPO #1639: if the cross-repo token were reduced
		// to 1639, the classifier would read THIS and wrongly satisfy the edge.
		1639: {Number: 1639, Title: "unrelated same-repo issue", State: "closed", StateReason: "completed"},
	}}
	res, err := New(api).ResolveDependencies(context.Background(), resolveReq("2032"))
	if err != nil {
		t.Fatalf("ResolveDependencies: %v", err)
	}
	if got := api.getIssueCalls[1639]; got != 0 {
		t.Errorf("GetIssue(#1639) called %d times, want ZERO (a cross-repo ref must not read the local #1639)", got)
	}
	if len(res.SatisfiedEdges) != 0 {
		t.Fatalf("SatisfiedEdges = %+v, want none (a cross-repo ref is never satisfied)", res.SatisfiedEdges)
	}
	if len(res.DroppedEdges) != 1 ||
		res.DroppedEdges[0].From != 2032 ||
		res.DroppedEdges[0].Reason != workmgmt.DropTargetStateUnreadable {
		t.Fatalf("DroppedEdges = %+v, want one {From:2032 DropTargetStateUnreadable}", res.DroppedEdges)
	}
}

// TestEpicChildrenCrossRepoRefFailsClosed mirrors the cross-repo fail-closed proof
// onto the EPIC provider path: a child whose body carries `Depends on: other/repo#999`
// where #999 exists as a closed+completed same-repo issue must not read #999 and must
// stamp DropTargetStateUnreadable. Same COUNTERFACTUALS as the ResolveDependencies twin.
func TestEpicChildrenCrossRepoRefFailsClosed(t *testing.T) {
	api := &fakeAPI{
		parentNode:     "EPIC_NODE",
		listSubResults: []githubclient.SubIssue{{Number: 43, NodeID: "N", Title: "c", Body: "Depends on: other/repo#999", State: "OPEN"}},
		// closed+completed same-repo #999: a reduce-to-999 bug would satisfy on this.
		getIssues: map[int]*githubclient.Issue{999: {Number: 999, State: "closed", StateReason: "completed"}},
	}
	res, err := New(api).EpicChildren(context.Background(), workmgmt.EpicChildrenRequest{
		Target: workmgmt.Target{Scope: forge.FromGitHubInstallationID(99), Repo: workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"}},
		Epic:   "#1005",
	})
	if err != nil {
		t.Fatalf("EpicChildren: %v", err)
	}
	if got := api.getIssueCalls[999]; got != 0 {
		t.Errorf("GetIssue(#999) called %d times, want ZERO (a cross-repo ref must not read the local #999)", got)
	}
	if len(res.SatisfiedEdges) != 0 {
		t.Fatalf("SatisfiedEdges = %+v, want none (a cross-repo ref is never satisfied)", res.SatisfiedEdges)
	}
	if len(res.DroppedEdges) != 1 ||
		res.DroppedEdges[0].From != 43 ||
		res.DroppedEdges[0].Reason != workmgmt.DropTargetStateUnreadable {
		t.Fatalf("DroppedEdges = %+v, want one {From:43 DropTargetStateUnreadable}", res.DroppedEdges)
	}
}

// TestClassifyOutOfSetTarget_UnresolvableWithPositiveNumber_NeverReadsForge pins
// the `!ref.Resolvable` early return in classifyOutOfSetTarget as a DIRECTLY
// discriminating control (operator condition 2 / CF5). The parser-driven
// cross-repo tests above cannot vehicle it: parseDependsOnMarker never emits an
// unresolvable ref carrying a positive Number, so the separate `number <= 0`
// guard independently blocks the forge call there and deleting the resolvable
// guard leaves those tests GREEN. This test constructs the ref DIRECTLY with
// Resolvable:false AND a positive Number pointing at a closed+completed same-repo
// issue — exactly the reduce-to-local-number bug the guard exists to stop — and
// asserts GetIssue is NEVER called and the outcome is DropTargetStateUnreadable
// (not a false satisfaction). Deleting the `!ref.Resolvable` return turns this RED
// (GetIssue(#1639) is called and the edge is wrongly satisfied).
func TestClassifyOutOfSetTarget_UnresolvableWithPositiveNumber_NeverReadsForge(t *testing.T) {
	api := &fakeAPI{getIssues: map[int]*githubclient.Issue{
		// A closed+completed same-repo #1639: were the unresolvable ref reduced to
		// this number, the classifier would read it and FALSELY satisfy the edge.
		1639: {Number: 1639, State: "closed", StateReason: "completed"},
	}}
	p := New(api)
	ts, err := p.classifyOutOfSetTarget(
		context.Background(),
		forge.FromGitHubInstallationID(99),
		forge.RepoRef{Owner: "kuhlman-labs", Name: "fishhawk"},
		dependsOnRef{Resolvable: false, Number: 1639, RawDigest: "0123456789abcdef", RawDisplay: "other/repo#1639"},
		map[int]targetState{},
	)
	if err != nil {
		t.Fatalf("classifyOutOfSetTarget: %v", err)
	}
	if got := api.getIssueCalls[1639]; got != 0 {
		t.Errorf("GetIssue(#1639) called %d times, want ZERO (an unresolvable ref must never read a local number even when it carries one)", got)
	}
	if ts.satisfied {
		t.Errorf("targetState = %+v, want NOT satisfied (an unresolvable ref is never satisfied)", ts)
	}
	if ts.reason != workmgmt.DropTargetStateUnreadable {
		t.Errorf("reason = %q, want %q", ts.reason, workmgmt.DropTargetStateUnreadable)
	}
}

// TestResolveDependenciesMemoizesTargetRead pins the per-call memoization
// (#2953): three in-set items each depending on the SAME closed target cost
// exactly ONE GetIssue for that target.
//
// COUNTERFACTUAL: remove the cache lookup/store in classifyOutOfSetTarget → the
// call count for #1639 becomes 3 and this goes RED.
func TestResolveDependenciesMemoizesTargetRead(t *testing.T) {
	api := &fakeAPI{getIssues: map[int]*githubclient.Issue{
		1: {Number: 1, Title: "a", Body: "Depends on: #1639", State: "open"},
		2: {Number: 2, Title: "b", Body: "Depends on: #1639", State: "open"},
		3: {Number: 3, Title: "c", Body: "Depends on: #1639", State: "open"},
		// out-of-set closed+completed target referenced by all three.
		1639: {Number: 1639, Title: "done", State: "closed", StateReason: "completed"},
	}}
	res, err := New(api).ResolveDependencies(context.Background(), resolveReq("1", "2", "3"))
	if err != nil {
		t.Fatalf("ResolveDependencies: %v", err)
	}
	if len(res.SatisfiedEdges) != 3 {
		t.Fatalf("SatisfiedEdges = %+v, want three (one per depending item)", res.SatisfiedEdges)
	}
	if got := api.getIssueCalls[1639]; got != 1 {
		t.Errorf("GetIssue(#1639) called %d times, want exactly 1 (memoized)", got)
	}
}

// TestResolveDependenciesNoOutOfSetEdgesMakesNoExtraReads: a batch with NO
// out-of-set edges makes NO extra GetIssue call beyond the one-per-named-issue
// in-set fetches, so the common-path API cost is unchanged (#2953).
func TestResolveDependenciesNoOutOfSetEdgesMakesNoExtraReads(t *testing.T) {
	api := &fakeAPI{getIssues: map[int]*githubclient.Issue{
		100: {Number: 100, Title: "a", Body: "no deps", State: "open"},
		101: {Number: 101, Title: "b", Body: "Depends on: #100", State: "open"},
	}}
	if _, err := New(api).ResolveDependencies(context.Background(), resolveReq("100", "101")); err != nil {
		t.Fatalf("ResolveDependencies: %v", err)
	}
	// Exactly the two in-set fetches, nothing more.
	if api.getIssueCalls[100] != 1 || api.getIssueCalls[101] != 1 || len(api.getIssueCalls) != 2 {
		t.Errorf("getIssueCalls = %v, want exactly {100:1,101:1} (no out-of-set read)", api.getIssueCalls)
	}
}

// epicChildrenResult drives the REAL EpicChildren where child #from depends on
// the out-of-EPIC target #to, whose canned GetIssue state is (state, reason).
func epicChildrenResult(t *testing.T, from, to int, toState, toReason string) *workmgmt.EpicChildrenResult {
	t.Helper()
	api := &fakeAPI{
		parentNode: "EPIC_NODE",
		listSubResults: []githubclient.SubIssue{
			{Number: from, NodeID: "N", Title: "child", Body: "Depends on: #" + strconv.Itoa(to), State: "OPEN"},
		},
		getIssues: map[int]*githubclient.Issue{
			to: {Number: to, Title: "target", State: toState, StateReason: toReason},
		},
	}
	res, err := New(api).EpicChildren(context.Background(), workmgmt.EpicChildrenRequest{
		Target: workmgmt.Target{Scope: forge.FromGitHubInstallationID(99), Repo: workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"}},
		Epic:   "#1005",
	})
	if err != nil {
		t.Fatalf("EpicChildren: %v", err)
	}
	return res
}

// TestEpicChildrenOutOfEpicTargetClassification mirrors the five classified
// outcomes onto the EPIC source path, so both campaign sources are pinned to the
// SAME shared classifier (#2953 ratified judgement: extend the fix to the epic
// path too). Each case asserts THAT branch's observable output.
func TestEpicChildrenOutOfEpicTargetClassification(t *testing.T) {
	t.Run("closed+completed → satisfied", func(t *testing.T) {
		res := epicChildrenResult(t, 43, 999, "closed", "completed")
		if len(res.DroppedEdges) != 0 {
			t.Fatalf("DroppedEdges = %+v, want none", res.DroppedEdges)
		}
		want := workmgmt.SatisfiedEdge{From: 43, To: 999, State: "closed", StateReason: "completed"}
		if len(res.SatisfiedEdges) != 1 || res.SatisfiedEdges[0] != want {
			t.Fatalf("SatisfiedEdges = %+v, want [%+v]", res.SatisfiedEdges, want)
		}
	})
	t.Run("open → DropNotChild", func(t *testing.T) {
		res := epicChildrenResult(t, 43, 999, "open", "")
		want := workmgmt.DependsEdge{From: 43, To: 999, Reason: workmgmt.DropNotChild}
		if len(res.DroppedEdges) != 1 || res.DroppedEdges[0] != want || len(res.SatisfiedEdges) != 0 {
			t.Fatalf("res dropped=%+v satisfied=%+v, want [%+v] / none", res.DroppedEdges, res.SatisfiedEdges, want)
		}
	})
	t.Run("closed+not_planned → DropTargetClosedIncomplete", func(t *testing.T) {
		res := epicChildrenResult(t, 43, 999, "closed", "not_planned")
		want := workmgmt.DependsEdge{From: 43, To: 999, Reason: workmgmt.DropTargetClosedIncomplete}
		if len(res.DroppedEdges) != 1 || res.DroppedEdges[0] != want || len(res.SatisfiedEdges) != 0 {
			t.Fatalf("res dropped=%+v satisfied=%+v, want [%+v] / none", res.DroppedEdges, res.SatisfiedEdges, want)
		}
	})
	t.Run("GetIssue error → DropTargetStateUnreadable", func(t *testing.T) {
		api := &fakeAPI{
			parentNode:     "EPIC_NODE",
			listSubResults: []githubclient.SubIssue{{Number: 43, NodeID: "N", Title: "c", Body: "Depends on: #999", State: "OPEN"}},
			// non-nil completed issue alongside the error (condition 4).
			getIssues:    map[int]*githubclient.Issue{999: {Number: 999, State: "closed", StateReason: "completed"}},
			getIssueErrs: map[int]error{999: errors.New("read failed")},
		}
		res, err := New(api).EpicChildren(context.Background(), workmgmt.EpicChildrenRequest{
			Target: workmgmt.Target{Scope: forge.FromGitHubInstallationID(99), Repo: workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"}},
			Epic:   "#1005",
		})
		if err != nil {
			t.Fatalf("EpicChildren: %v", err)
		}
		want := workmgmt.DependsEdge{From: 43, To: 999, Reason: workmgmt.DropTargetStateUnreadable}
		if len(res.DroppedEdges) != 1 || res.DroppedEdges[0] != want || len(res.SatisfiedEdges) != 0 {
			t.Fatalf("res dropped=%+v satisfied=%+v, want [%+v] / none", res.DroppedEdges, res.SatisfiedEdges, want)
		}
	})
	t.Run("nil issue → DropTargetStateUnreadable", func(t *testing.T) {
		api := &fakeAPI{
			parentNode:     "EPIC_NODE",
			listSubResults: []githubclient.SubIssue{{Number: 43, NodeID: "N", Title: "c", Body: "Depends on: #999", State: "OPEN"}},
			getIssues:      map[int]*githubclient.Issue{999: nil},
		}
		r2, err := New(api).EpicChildren(context.Background(), workmgmt.EpicChildrenRequest{
			Target: workmgmt.Target{Scope: forge.FromGitHubInstallationID(99), Repo: workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"}},
			Epic:   "#1005",
		})
		if err != nil {
			t.Fatalf("EpicChildren: %v", err)
		}
		want := workmgmt.DependsEdge{From: 43, To: 999, Reason: workmgmt.DropTargetStateUnreadable}
		if len(r2.DroppedEdges) != 1 || r2.DroppedEdges[0] != want || len(r2.SatisfiedEdges) != 0 {
			t.Fatalf("res dropped=%+v satisfied=%+v, want [%+v] / none", r2.DroppedEdges, r2.SatisfiedEdges, want)
		}
	})
}

// countEdges tallies how many times each (From,To) pair appears across a slice
// of DependsEdge, so a test can assert AT-MOST-ONCE per pair.
func countEdges(es []workmgmt.DependsEdge) map[[2]int]int {
	m := map[[2]int]int{}
	for _, e := range es {
		m[[2]int{e.From, e.To}]++
	}
	return m
}

// countSatisfied tallies how many times each (From,To) pair appears across a
// slice of SatisfiedEdge.
func countSatisfied(es []workmgmt.SatisfiedEdge) map[[2]int]int {
	m := map[[2]int]int{}
	for _, e := range es {
		m[[2]int{e.From, e.To}]++
	}
	return m
}

// TestResolveDependenciesDuplicateTokenCollapses pins #2953 condition 3 at the
// no-epic provider path: an untrusted body carrying a REPEATED depends_on token
// (`Depends on: #1639, #1639`) yields AT MOST ONE edge per (From,To) in whichever
// slice it classifies into — satisfied, dropped, or in-set — never one entry per
// occurrence. Before the dedup, parseDependsOnMarker preserved both occurrences
// and the classify loop appended twice, doubling the visible elision/audit entry.
//
// COUNTERFACTUAL: delete the seenEdge guard in ResolveDependencies → each repeated
// token appends again → every "want exactly 1" assertion below goes RED.
func TestResolveDependenciesDuplicateTokenCollapses(t *testing.T) {
	t.Run("repeated satisfied token → one SatisfiedEdge", func(t *testing.T) {
		api := &fakeAPI{getIssues: map[int]*githubclient.Issue{
			2032: {Number: 2032, Title: "in-set", Body: "Depends on: #1639, #1639", State: "open"},
			1639: {Number: 1639, Title: "done", State: "closed", StateReason: "completed"},
		}}
		res, err := New(api).ResolveDependencies(context.Background(), resolveReq("2032"))
		if err != nil {
			t.Fatalf("ResolveDependencies: %v", err)
		}
		if got := countSatisfied(res.SatisfiedEdges); got[[2]int{2032, 1639}] != 1 || len(res.SatisfiedEdges) != 1 {
			t.Fatalf("SatisfiedEdges = %+v, want exactly one 2032->1639", res.SatisfiedEdges)
		}
		// The memoized read also holds: the repeated token costs one GetIssue.
		if got := api.getIssueCalls[1639]; got != 1 {
			t.Errorf("GetIssue(#1639) called %d times, want exactly 1 (memoized)", got)
		}
	})
	t.Run("repeated dropped token → one DroppedEdge", func(t *testing.T) {
		api := &fakeAPI{getIssues: map[int]*githubclient.Issue{
			2032: {Number: 2032, Title: "in-set", Body: "Depends on: #1641, #1641", State: "open"},
			1641: {Number: 1641, Title: "abandoned", State: "closed", StateReason: "not_planned"},
		}}
		res, err := New(api).ResolveDependencies(context.Background(), resolveReq("2032"))
		if err != nil {
			t.Fatalf("ResolveDependencies: %v", err)
		}
		if got := countEdges(res.DroppedEdges); got[[2]int{2032, 1641}] != 1 || len(res.DroppedEdges) != 1 {
			t.Fatalf("DroppedEdges = %+v, want exactly one 2032->1641", res.DroppedEdges)
		}
	})
	t.Run("repeated in-set token → one Edge", func(t *testing.T) {
		api := &fakeAPI{getIssues: map[int]*githubclient.Issue{
			100: {Number: 100, Title: "dep", Body: "no deps", State: "open"},
			101: {Number: 101, Title: "leaf", Body: "Depends on: #100, #100", State: "open"},
		}}
		res, err := New(api).ResolveDependencies(context.Background(), resolveReq("100", "101"))
		if err != nil {
			t.Fatalf("ResolveDependencies: %v", err)
		}
		if got := countEdges(res.Edges); got[[2]int{101, 100}] != 1 || len(res.Edges) != 1 {
			t.Fatalf("Edges = %+v, want exactly one 101->100", res.Edges)
		}
	})
}

// TestEpicChildrenDuplicateTokenCollapses mirrors the duplicate-token dedup onto
// the EPIC provider path (#2953 condition 3): a child body repeating a depends_on
// token yields at most one edge per (From,To).
//
// COUNTERFACTUAL: delete the seenEdge guard in EpicChildren → the repeated token
// doubles the SatisfiedEdges entry → this goes RED.
func TestEpicChildrenDuplicateTokenCollapses(t *testing.T) {
	res := epicChildrenResultBody(t, 43, "Depends on: #999, #999", map[int]*githubclient.Issue{
		999: {Number: 999, Title: "done", State: "closed", StateReason: "completed"},
	})
	want := workmgmt.SatisfiedEdge{From: 43, To: 999, State: "closed", StateReason: "completed"}
	if got := countSatisfied(res.SatisfiedEdges); got[[2]int{43, 999}] != 1 || len(res.SatisfiedEdges) != 1 || res.SatisfiedEdges[0] != want {
		t.Fatalf("SatisfiedEdges = %+v, want exactly one [%+v]", res.SatisfiedEdges, want)
	}
}

// epicChildrenResultBody drives the REAL EpicChildren for a single child #from
// with the given raw body and canned out-of-epic target issues.
func epicChildrenResultBody(t *testing.T, from int, body string, targets map[int]*githubclient.Issue) *workmgmt.EpicChildrenResult {
	t.Helper()
	api := &fakeAPI{
		parentNode:     "EPIC_NODE",
		listSubResults: []githubclient.SubIssue{{Number: from, NodeID: "N", Title: "child", Body: body, State: "OPEN"}},
		getIssues:      targets,
	}
	res, err := New(api).EpicChildren(context.Background(), workmgmt.EpicChildrenRequest{
		Target: workmgmt.Target{Scope: forge.FromGitHubInstallationID(99), Repo: workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"}},
		Epic:   "#1005",
	})
	if err != nil {
		t.Fatalf("EpicChildren: %v", err)
	}
	return res
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
	refs := parseDependsOnMarker(body)
	if len(refs) != 2 ||
		refs[0] != (dependsOnRef{Number: 41, Resolvable: true}) ||
		refs[1] != (dependsOnRef{Number: 42, Resolvable: true}) {
		t.Errorf("parse round trip = %+v, want two resolvable refs [41 42]", refs)
	}
	if parseDependsOnMarker("## Summary\n\nno marker here\n") != nil {
		t.Errorf("parse of a body with no marker should be nil")
	}
	// A cross-repo / owner-qualified token is carried through UNRESOLVABLE, not
	// silently dropped and not reduced to a local number (#2953, condition 2): so a
	// cross-repo dependency fails the campaign closed rather than vanishing. An
	// empty token (trailing comma) is skipped entirely. Each unresolvable token now
	// carries an IDENTITY (#2956): a 16-hex digest over its canonical form and the
	// sanitized display, so its dropped edge names WHICH token.
	mixed := parseDependsOnMarker("Depends on: #7, other/repo#1639, , owner/name#3")
	if len(mixed) != 3 {
		t.Fatalf("parse of mixed marker = %+v, want 3 refs", mixed)
	}
	if mixed[0] != (dependsOnRef{Number: 7, Resolvable: true}) {
		t.Errorf("mixed[0] = %+v, want resolvable #7", mixed[0])
	}
	for i, wantDisplay := range map[int]string{1: "other/repo#1639", 2: "owner/name#3"} {
		if mixed[i].Resolvable {
			t.Errorf("mixed[%d] = %+v, want unresolvable", i, mixed[i])
		}
		if mixed[i].RawDisplay != wantDisplay {
			t.Errorf("mixed[%d].RawDisplay = %q, want %q", i, mixed[i].RawDisplay, wantDisplay)
		}
		if !isHex16(mixed[i].RawDigest) {
			t.Errorf("mixed[%d].RawDigest = %q, want 16 hex chars", i, mixed[i].RawDigest)
		}
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
	req.Target.Scope = forge.CredentialScope{}
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
	// restBody is the RAW JSON body of the issue-create request the fake server
	// actually received. It exists so a test can assert on the OUTBOUND payload
	// (#2064) rather than on a body handed to a stub — the serialization
	// boundary is exactly where a marker would be lost.
	restBody string
}

func newRealClient(t *testing.T, projectsToken string) (*githubclient.Client, *realClientFixture) {
	t.Helper()
	fx := &realClientFixture{}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		fx.restAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		fx.restBody = string(raw)
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

// TestProvider_Transition_NotApplicableClassification pins which skips are
// NOT-APPLICABLE (there is no work item to act on, so the caller records no
// work_item_transitioned entry, #2494) and which are DECISIONS about a real
// work item (which keep auditing). Every outcome the Transition path can
// produce is asserted, so a future skip added without a deliberate
// classification shows up as a missing row here rather than as silent audit
// suppression or a re-flood.
func TestProvider_Transition_NotApplicableClassification(t *testing.T) {
	cases := []struct {
		name              string
		build             func() (*fakeAPI, workmgmt.TransitionRequest)
		wantMoved         bool
		wantNotApplicable bool
	}{
		{
			name: "no project configured",
			build: func() (*fakeAPI, workmgmt.TransitionRequest) {
				req := runStartedRequest()
				req.Target.Project = nil
				return transitionAPI("Backlog", true), req
			},
			wantNotApplicable: true,
		},
		{
			name: "issue not on the board",
			build: func() (*fakeAPI, workmgmt.TransitionRequest) {
				return transitionAPI("", false), runStartedRequest()
			},
			wantNotApplicable: true,
		},
		{
			name: "unreachable user-owned board",
			build: func() (*fakeAPI, workmgmt.TransitionRequest) {
				api := transitionAPI("Backlog", true)
				api.projectsTokenConfigured = false
				return api, runStartedRequest()
			},
			// Deliberately NOT not-applicable: an operator-actionable
			// misconfiguration worth one audit row per occurrence.
			wantNotApplicable: false,
		},
		{
			name: "unmapped canonical state",
			build: func() (*fakeAPI, workmgmt.TransitionRequest) {
				req := runStartedRequest()
				req.CanonicalState = workmgmt.CanonicalStateDone
				req.States = map[string]string{workmgmt.CanonicalStateInProgress: "In Progress"}
				return transitionAPI("Backlog", true), req
			},
			wantNotApplicable: false,
		},
		{
			name: "target not a board option",
			build: func() (*fakeAPI, workmgmt.TransitionRequest) {
				api := transitionAPI("Backlog", true)
				delete(api.meta.StatusOptions, "In Progress")
				return api, runStartedRequest()
			},
			wantNotApplicable: false,
		},
		{
			name: "never-fight-the-human source mismatch",
			build: func() (*fakeAPI, workmgmt.TransitionRequest) {
				return transitionAPI("Blocked", true), runStartedRequest()
			},
			wantNotApplicable: false,
		},
		{
			name: "already at target",
			build: func() (*fakeAPI, workmgmt.TransitionRequest) {
				req := runStartedRequest()
				req.ExpectedSourceStates = []string{workmgmt.CanonicalStateBacklog, workmgmt.CanonicalStateInProgress}
				return transitionAPI("In Progress", true), req
			},
			wantNotApplicable: false,
		},
		{
			name: "landed move",
			build: func() (*fakeAPI, workmgmt.TransitionRequest) {
				return transitionAPI("Backlog", true), runStartedRequest()
			},
			wantMoved:         true,
			wantNotApplicable: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, req := tc.build()
			res, err := New(api).Transition(context.Background(), req)
			if err != nil {
				t.Fatalf("Transition: %v", err)
			}
			if res.Moved != tc.wantMoved {
				t.Errorf("Moved = %v, want %v (%+v)", res.Moved, tc.wantMoved, res)
			}
			if res.Skipped == tc.wantMoved {
				t.Errorf("Skipped = %v alongside Moved = %v (%+v)", res.Skipped, res.Moved, res)
			}
			if res.NotApplicable != tc.wantNotApplicable {
				t.Errorf("NotApplicable = %v, want %v (skip_reason %q)",
					res.NotApplicable, tc.wantNotApplicable, res.SkipReason)
			}
		})
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
	req.Target.Scope = forge.CredentialScope{}
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
	req.Target.Scope = forge.CredentialScope{}
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

// TestProvider_EpicChildren_PopulatesBodyAndURLVerbatim pins the two ADDITIVE
// EpicChild fields the forge-state adoption path reads (#2064, E50.7): a served
// sub-issue's body — including one already carrying an idempotency marker — and
// its url reach EpicChild.Body / EpicChild.URL VERBATIM.
//
// The url fixture is deliberately a GitHub Enterprise Server host: a mapping
// that composed https://github.com/{owner}/{repo}/issues/{n} instead of
// carrying the forge's own value through would produce the WRONG url here, and
// the adoption path would then record a url no operator could follow.
func TestProvider_EpicChildren_PopulatesBodyAndURLVerbatim(t *testing.T) {
	key := workmgmt.MintIdempotencyKey("fishhawk-split-child", "run-abc", "0")
	keyedBody := workmgmt.StampIdempotencyKey("## Summary\n\nphase child\n", key)

	api := &fakeAPI{
		parentNode: "EPIC_NODE",
		listSubResults: []githubclient.SubIssue{
			{Number: 100, NodeID: "N100", Title: "keyed child", Body: keyedBody,
				URL: "https://ghe.example.com/kuhlman-labs/fishhawk/issues/100", State: "OPEN"},
			{Number: 101, NodeID: "N101", Title: "plain child", Body: "no marker here",
				URL: "https://ghe.example.com/kuhlman-labs/fishhawk/issues/101", State: "CLOSED", StateReason: "COMPLETED"},
		},
	}
	res, err := New(api).EpicChildren(context.Background(), workmgmt.EpicChildrenRequest{
		Target: workmgmt.Target{Scope: forge.FromGitHubInstallationID(99), Repo: workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"}},
		Epic:   "#99",
	})
	if err != nil {
		t.Fatalf("EpicChildren: %v", err)
	}
	if len(res.Children) != 2 {
		t.Fatalf("children = %+v, want 2", res.Children)
	}
	if res.Children[0].Body != keyedBody {
		t.Errorf("child[0].Body = %q, want the served body verbatim %q", res.Children[0].Body, keyedBody)
	}
	if res.Children[0].URL != "https://ghe.example.com/kuhlman-labs/fishhawk/issues/100" {
		t.Errorf("child[0].URL = %q, want the forge's own url verbatim", res.Children[0].URL)
	}
	if res.Children[1].Body != "no marker here" {
		t.Errorf("child[1].Body = %q", res.Children[1].Body)
	}
	if res.Children[1].URL != "https://ghe.example.com/kuhlman-labs/fishhawk/issues/101" {
		t.Errorf("child[1].URL = %q", res.Children[1].URL)
	}
	// The marker survives the mapping well enough to be DETECTED off the mapped
	// value — the actual thing the adoption lookup does.
	if !workmgmt.BodyHasIdempotencyKey(res.Children[0].Body, key) {
		t.Errorf("mapped child body no longer carries its idempotency key")
	}
	// A CLOSED+COMPLETED child is still returned (no state filter), so it stays
	// adoptable — the property TestSplitFiling_ClosedChildBearingKeyIsAdopted
	// depends on at the server layer.
	if !res.Children[1].Complete {
		t.Errorf("child[1].Complete = false, want true (CLOSED+COMPLETED)")
	}
}

// TestProviderFile_CreateRequestCarriesIdempotencyMarker closes the ADAPTER
// SERIALIZATION boundary (operator FIX 2). Every other case in this change
// observes a body handed to a fake provider, or an EpicChildren OUTPUT — none
// of them watches the OUTBOUND issue-create request, which is exactly where a
// serialization bug would live.
//
// It drives the REAL Provider.File against the REAL githubclient against the
// httptest mux, with a body rendered by the REAL workmgmt.Apply from a
// FilingRequest carrying an IdempotencyKey, and asserts the marker is present
// in the CREATE REQUEST BODY the fake server actually received — after
// Provider.File's ensureDependsOnMarker body rewrite and the JSON request
// assembly. Counterfactual c7: deleting the stamping call in workmgmt.Apply
// reddens this alongside the Apply unit test.
func TestProviderFile_CreateRequestCarriesIdempotencyMarker(t *testing.T) {
	key := workmgmt.MintIdempotencyKey("fishhawk-split-child", "run-abc", "2")

	item, _, err := workmgmt.Apply(workmgmt.FilingRequest{
		Type:           "feature",
		Summary:        "contract: delete the transitional Foo",
		Body:           "## Summary\n\ndelete Foo now that all consumers use NewFoo\n",
		TitleVars:      map[string]string{"epic": "2100", "n": "3"},
		Relations:      workmgmt.Relations{ParentEpic: "#2100", DependsOn: []string{"#3002"}},
		IdempotencyKey: key,
	}, workmgmt.Default())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	req := baseRequest()
	req.Item = item
	req.Item.Classification.Labels = []string{"type:feature"}
	req.Item.BoardPlacement.Status = "Backlog"
	// Apply resolved a depends_on relation onto the item, so the marker must
	// survive ensureDependsOnMarker's body REWRITE rather than only an
	// untouched pass-through.
	if len(req.Item.Relations.DependsOn) == 0 {
		t.Fatal("fixture error: no depends_on relation, so the body rewrite would not run")
	}

	c, fx := newRealClient(t, "pat_projects")
	if _, err := New(c).File(context.Background(), req); err != nil {
		t.Fatalf("File: %v", err)
	}

	if fx.restBody == "" {
		t.Fatal("fake server recorded no issue-create request body")
	}
	var sent struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.Unmarshal([]byte(fx.restBody), &sent); err != nil {
		t.Fatalf("decode recorded create body %q: %v", fx.restBody, err)
	}
	// The assertion is on the OUTBOUND request, decoded from the wire.
	if !workmgmt.BodyHasIdempotencyKey(sent.Body, key) {
		t.Fatalf("issue-create request body does not carry the idempotency marker for %q:\n%s", key, sent.Body)
	}
	// The depends_on rewrite ran (so this is not a no-rewrite pass-through) and
	// the original prose survived alongside the marker.
	if !strings.Contains(sent.Body, "#3002") {
		t.Errorf("create body lost the depends_on marker: %s", sent.Body)
	}
	if !strings.Contains(sent.Body, "delete Foo now that all consumers use NewFoo") {
		t.Errorf("create body lost the original prose: %s", sent.Body)
	}
	// Exactly one marker on the wire: no double-stamp across Apply + File.
	if got := strings.Count(sent.Body, key); got != 1 {
		t.Errorf("create body carries the key %d times, want exactly 1:\n%s", got, sent.Body)
	}
}

// TestRenderDependsOnMarker_RoutesShapeThroughTheSharedNormalizer asserts that
// routing renderDependsOnMarker's ref-SHAPE decision through
// workmgmt.NormalizeIssueRef (#2860) is byte-preserving for the numeric refs
// the marker actually carries, persists a NON-NUMERIC ref as written rather
// than hashing it, and still skips a token that is no reference at all.
//
// The non-numeric row is the load-bearing one: the old inline
// `"#"+TrimPrefix(...)` emitted `#other/repo#1639` where the membership read
// observes `other/repo#1639`, so the write and the later read could never
// agree — which is #2860 one level down.
//
// The bare-`#` row pins the EMPTINESS guard the normalizer does not provide:
// NormalizeIssueRef("#") returns the trimmed original `"#"`, which is
// non-empty, so without dependsOnRefStripped a bare `#` would start being
// emitted as a reference.
func TestRenderDependsOnMarker_RoutesShapeThroughTheSharedNormalizer(t *testing.T) {
	for _, tc := range []struct {
		name string
		refs []string
		want string
	}{
		{"numeric refs are byte-identical to before", []string{"#41", "42"}, "Depends on: #41, #42"},
		{"non-numeric ref persists as written", []string{"other/repo#1639"}, "Depends on: other/repo#1639"},
		{"hashed non-numeric ref persists as written", []string{"#other/repo#1639"}, "Depends on: #other/repo#1639"},
		{"a bare # is skipped, not emitted", []string{"#", "41"}, "Depends on: #41"},
		{"a bare # alone renders no marker", []string{"#"}, ""},
		{"whitespace tokens are skipped", []string{"  ", "\t"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderDependsOnMarker(tc.refs); got != tc.want {
				t.Errorf("renderDependsOnMarker(%q) = %q, want %q", tc.refs, got, tc.want)
			}
		})
	}
}

// TestEnsureDependsOnMarker_FilingPathStillNeverDoubleStamps pins the FILING
// path's contract (E34.3 / #1594) with its OWN test: ensureDependsOnMarker must
// NOT acquire the amend path's additive behaviour. A body already carrying a
// marker, handed a DIFFERENT ref, comes back BYTE-IDENTICAL.
func TestEnsureDependsOnMarker_FilingPathStillNeverDoubleStamps(t *testing.T) {
	body := "## Summary\n\nx\n\nDepends on: #5\n"
	if got := ensureDependsOnMarker(body, []string{"#6"}); got != body {
		t.Errorf("filing path became additive:\n got %q\nwant %q (unchanged)", got, body)
	}
}

// TestAppendDependsOnRef_MergesIntoExistingMarker asserts the amend path is
// ADDITIVE — the #2860 fix — and that the splice rewrites NOTHING outside the
// marker line's captured value. The whole resulting body is compared byte for
// byte, so surrounding prose, the blank lines and the final newline are all
// pinned.
func TestAppendDependsOnRef_MergesIntoExistingMarker(t *testing.T) {
	body := "## Summary\n\nWork item.\n\nDepends on: #1639, #1640, #1641\n\nParent epic: #1600\n"
	want := "## Summary\n\nWork item.\n\nDepends on: #1639, #1640, #1641, #2032\n\nParent epic: #1600\n"
	got, changed := appendDependsOnRef(body, "#2032")
	if !changed {
		t.Fatalf("changed = false, want true")
	}
	if got != want {
		t.Errorf("body =\n%q\nwant\n%q", got, want)
	}
}

// TestAppendDependsOnRef_PreservesCRLFLineEndings asserts the trailing-CR
// handling. dependsOnMarkerRE's `(.+)` capture ENDS WITH the carriage return on
// a CRLF body (Go's `(?m)$` anchors before `\n` only, and `.` matches `\r`), so
// a raw append at the capture end would emit `#1641<CR>, #2032` and corrupt the
// line. The whole body is compared byte for byte and the corrupt sequence is
// asserted absent.
func TestAppendDependsOnRef_PreservesCRLFLineEndings(t *testing.T) {
	body := "## Summary\r\n\r\nWork item.\r\n\r\nDepends on: #1639, #1640, #1641\r\n\r\nParent epic: #1600\r\n"
	want := "## Summary\r\n\r\nWork item.\r\n\r\nDepends on: #1639, #1640, #1641, #2032\r\n\r\nParent epic: #1600\r\n"
	got, changed := appendDependsOnRef(body, "#2032")
	if !changed {
		t.Fatalf("changed = false, want true")
	}
	if got != want {
		t.Errorf("body =\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(got, "#1641\r") {
		t.Errorf("carriage return spliced into the middle of the ref list:\n%q", got)
	}
	if !strings.Contains(got, "Depends on: #1639, #1640, #1641, #2032\r\n") {
		t.Errorf("marker line does not end in CRLF:\n%q", got)
	}
}

// TestAppendDependsOnRef_AlreadyAMemberIsANoOp asserts the genuine idempotent
// no-op: a ref the body ALREADY records returns the body unchanged with
// changed=false, whatever wire form it arrives in.
//
// This is a UNIT test on the helper and is NOT expected to surface end to end
// through ApplyGrooming: the core's groomingSatisfied asks the same membership
// question BEFORE dispatch and short-circuits to `already_applied`, so the
// provider-side skip is reachable only when the two layers DISAGREE — a body
// mutated between the read and the write.
func TestAppendDependsOnRef_AlreadyAMemberIsANoOp(t *testing.T) {
	body := "## Summary\n\nx\n\nDepends on: #1639, #2032\n"
	for _, ref := range []string{"2032", "#2032", "  #2032  "} {
		got, changed := appendDependsOnRef(body, ref)
		if changed {
			t.Errorf("appendDependsOnRef(_, %q) reported changed for a ref already recorded", ref)
		}
		if got != body {
			t.Errorf("appendDependsOnRef(_, %q) mutated the body:\n%q", ref, got)
		}
	}
}

// TestAppendDependsOnRef_MarkerFreeBodyDelegatesToEnsure asserts the
// no-marker branch delegates rather than hand-rolling the append-a-new-marker
// case, producing exactly what the filing path would.
func TestAppendDependsOnRef_MarkerFreeBodyDelegatesToEnsure(t *testing.T) {
	body := "## Summary\n\nx\n"
	got, changed := appendDependsOnRef(body, "2032")
	if !changed {
		t.Fatalf("changed = false, want true for a marker-free body")
	}
	if want := ensureDependsOnMarker(body, []string{"2032"}); got != want {
		t.Errorf("body =\n%q\nwant (ensureDependsOnMarker)\n%q", got, want)
	}
	if !strings.Contains(got, "Depends on: #2032") {
		t.Errorf("no marker line was appended:\n%q", got)
	}
}

// TestAppendDependsOnRef_InvalidRefIsANoOpOnBothBodyShapes pins operator
// approval condition 1. A ref whose stripped form is empty — a bare `#`, an
// empty string, whitespace — is NOT a reference, and the amend path must treat
// it as a no-op returning changed=false.
//
// BOTH body shapes are covered because the two paths fail DIFFERENTLY without
// the guard: with an existing marker the splice appends a meaningless `, #`,
// and WITHOUT one the delegation to ensureDependsOnMarker returns the body
// unchanged (renderDependsOnMarker skips the token) while still reporting
// changed=true — a body that did not change, audited as applied, which is the
// exact "refusal reads as success" failure #2860 exists to kill.
func TestAppendDependsOnRef_InvalidRefIsANoOpOnBothBodyShapes(t *testing.T) {
	bodies := map[string]string{
		"marker-bearing": "## Summary\n\nx\n\nDepends on: #1639\n",
		"marker-free":    "## Summary\n\nx\n",
	}
	for shape, body := range bodies {
		for _, ref := range []string{"#", "", "   ", " # ", "#  "} {
			got, changed := appendDependsOnRef(body, ref)
			if changed {
				t.Errorf("%s body: appendDependsOnRef(_, %q) reported changed for a non-reference", shape, ref)
			}
			if got != body {
				t.Errorf("%s body: appendDependsOnRef(_, %q) mutated the body:\n%q", shape, ref, got)
			}
		}
	}
}

// TestAppendDependsOnRef_SplicesOnlyTheFirstMarkerLine asserts the splice
// targets the FIRST marker line while the membership READ sees EVERY line —
// the same breadth the core's groomingMarkerObserved reads, so the two layers
// cannot disagree about membership.
func TestAppendDependsOnRef_SplicesOnlyTheFirstMarkerLine(t *testing.T) {
	body := "Depends on: #1\n\nprose\n\nDepends on: #2\n"
	got, changed := appendDependsOnRef(body, "#3")
	if !changed {
		t.Fatalf("changed = false, want true")
	}
	if want := "Depends on: #1, #3\n\nprose\n\nDepends on: #2\n"; got != want {
		t.Errorf("body =\n%q\nwant\n%q", got, want)
	}
	// A ref present ONLY on the second marker line is still a member.
	if got, changed := appendDependsOnRef(body, "2"); changed || got != body {
		t.Errorf("ref on the second marker line was not recognised as a member: changed=%t body=%q", changed, got)
	}
}

// TestDependsOnMarkerRefs_ReadsEveryLineNormalized pins the membership read
// itself: every marker line, every non-empty token, each normalized through the
// SHARED normalizer, in body order — and a CRLF body's last token carries no
// stray carriage return.
func TestDependsOnMarkerRefs_ReadsEveryLineNormalized(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{"no marker", "## Summary\n\nx\n", nil},
		{"both wire forms normalize", "Depends on: 41, #42\n", []string{"#41", "#42"}},
		{"every line, body order", "Depends on: #1\nprose\nDepends on: #2, #3\n", []string{"#1", "#2", "#3"}},
		{"non-numeric refs pass through unchanged", "Depends on: other/repo#1639\n", []string{"other/repo#1639"}},
		{"empty tokens and a bare # are skipped", "Depends on: #7, , #, 8\n", []string{"#7", "#8"}},
		{"CRLF leaves no stray carriage return", "Depends on: #7, #8\r\n", []string{"#7", "#8"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := dependsOnMarkerRefs(tc.body)
			if len(got) != len(tc.want) {
				t.Fatalf("dependsOnMarkerRefs = %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("dependsOnMarkerRefs = %q, want %q", got, tc.want)
				}
			}
		})
	}
}

// isHex16 reports whether s is exactly 16 lowercase hex characters — the shape
// dependsOnTokenDigest emits (#2956).
func isHex16(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// TestParseDependsOnMarker_UnresolvableTokenCarriesDigestAndDisplay (7.i): a
// mixed marker yields resolvable numeric refs plus an UNRESOLVABLE cross-repo ref
// carrying a 16-hex digest and the sanitized display (#2956).
func TestParseDependsOnMarker_UnresolvableTokenCarriesDigestAndDisplay(t *testing.T) {
	refs := parseDependsOnMarker("Depends on: #1639, other/repo#12, 42")
	if len(refs) != 3 {
		t.Fatalf("refs = %+v, want 3 (resolvable 1639, unresolvable cross-repo, resolvable 42)", refs)
	}
	if refs[0] != (dependsOnRef{Number: 1639, Resolvable: true}) {
		t.Errorf("refs[0] = %+v, want resolvable #1639", refs[0])
	}
	if refs[2] != (dependsOnRef{Number: 42, Resolvable: true}) {
		t.Errorf("refs[2] = %+v, want resolvable #42", refs[2])
	}
	if refs[1].Resolvable {
		t.Errorf("refs[1] = %+v, want unresolvable", refs[1])
	}
	if refs[1].RawDisplay != "other/repo#12" {
		t.Errorf("refs[1].RawDisplay = %q, want %q", refs[1].RawDisplay, "other/repo#12")
	}
	if !isHex16(refs[1].RawDigest) {
		t.Errorf("refs[1].RawDigest = %q, want 16 lowercase hex chars", refs[1].RawDigest)
	}
}

// TestParseDependsOnMarker_ControlRuneOnlyTokens_DistinctDigests (7.ii): two
// tokens made solely of DIFFERENT control runes both sanitize to "" yet carry
// DIFFERENT digests — the digest is over the canonical (pre-sanitization) form,
// so control-rune stripping cannot merge them (#2956, CF2 vehicle).
func TestParseDependsOnMarker_ControlRuneOnlyTokens_DistinctDigests(t *testing.T) {
	refs := parseDependsOnMarker("Depends on: \x01, \x02")
	if len(refs) != 2 {
		t.Fatalf("refs = %+v, want 2 control-rune tokens", refs)
	}
	for i, r := range refs {
		if r.Resolvable {
			t.Errorf("refs[%d] = %+v, want unresolvable", i, r)
		}
		if r.RawDisplay != "" {
			t.Errorf("refs[%d].RawDisplay = %q, want empty (control runes sanitize away)", i, r.RawDisplay)
		}
		if !isHex16(r.RawDigest) {
			t.Errorf("refs[%d].RawDigest = %q, want 16 hex chars", i, r.RawDigest)
		}
	}
	if refs[0].RawDigest == refs[1].RawDigest {
		t.Errorf("distinct control-rune tokens share a digest %q — digest must be over the canonical, not the sanitized, form", refs[0].RawDigest)
	}
}

// TestParseDependsOnMarker_TokensDifferingOnlyPastTruncation_DistinctDigests
// (7.iii): two 200-rune tokens identical for the first 64 runes carry DIFFERENT
// digests (the full canonical form is hashed) but EQUAL truncated displays.
func TestParseDependsOnMarker_TokensDifferingOnlyPastTruncation_DistinctDigests(t *testing.T) {
	a := strings.Repeat("a", 64) + strings.Repeat("b", 136)
	b := strings.Repeat("a", 64) + strings.Repeat("c", 136)
	refs := parseDependsOnMarker("Depends on: " + a + ", " + b)
	if len(refs) != 2 {
		t.Fatalf("refs = %+v, want 2", refs)
	}
	if refs[0].RawDigest == refs[1].RawDigest {
		t.Errorf("tokens differing past rune 64 share a digest — the FULL canonical form must be hashed, not the truncated display")
	}
	if refs[0].RawDisplay != refs[1].RawDisplay {
		t.Errorf("displays differ (%q vs %q); both should truncate to the same first-64-rune prefix", refs[0].RawDisplay, refs[1].RawDisplay)
	}
	// The display is rune-bounded.
	if n := len([]rune(refs[0].RawDisplay)); n > 64 {
		t.Errorf("RawDisplay is %d runes, want <= 64", n)
	}
}

// TestParseDependsOnMarker_CanonicalFormCollapsesWhitespaceVariants (7.iv): two
// tokens differing only in surrounding whitespace share ONE canonical form and
// therefore ONE digest — the identity relation asserted positively (#2956).
func TestParseDependsOnMarker_CanonicalFormCollapsesWhitespaceVariants(t *testing.T) {
	refs := parseDependsOnMarker("Depends on:   other/repo#12 , other/repo#12")
	if len(refs) != 2 {
		t.Fatalf("refs = %+v, want 2 whitespace variants of one token", refs)
	}
	if refs[0].RawDigest == "" || refs[0].RawDigest != refs[1].RawDigest {
		t.Errorf("whitespace variants have digests %q / %q, want ONE shared non-empty digest", refs[0].RawDigest, refs[1].RawDigest)
	}
}

// TestParseDependsOnMarker_OverflowAndWhitespaceTokens (routed #2956 untested
// paths): a regex-matching but int-OVERFLOWING digit string
// (`99999999999999999999`, which dependsOnRefRE accepts but strconv.Atoi rejects)
// is carried through UNRESOLVABLE with a digest and its verbatim display rather
// than silently dropped; and a token carrying an internal whitespace RUN of mixed
// classes — ASCII space, a tab, and a printable Unicode space (U+00A0) — has that
// run collapsed to a single ASCII space in RawDisplay.
func TestParseDependsOnMarker_OverflowAndWhitespaceTokens(t *testing.T) {
	refs := parseDependsOnMarker("Depends on: 99999999999999999999, other/repo\t #12")
	if len(refs) != 2 {
		t.Fatalf("refs = %+v, want 2", refs)
	}
	// The overflow token: matches the ref regex but Atoi overflows, so it lands in
	// the strconv.Atoi failure branch — unresolvable, Number 0, digest-bearing, and
	// with its digits preserved verbatim in the display (never issue:0).
	over := refs[0]
	if over.Resolvable || over.Number != 0 {
		t.Errorf("overflow ref = %+v, want unresolvable with Number 0", over)
	}
	if !isHex16(over.RawDigest) {
		t.Errorf("overflow RawDigest = %q, want 16 hex chars", over.RawDigest)
	}
	if over.RawDisplay != "99999999999999999999" {
		t.Errorf("overflow RawDisplay = %q, want the digit string verbatim", over.RawDisplay)
	}
	// The whitespace-run token: every whitespace class between owner/repo and #12
	// collapses to ONE ASCII space (a tab is normalized, not dropped; the Unicode
	// space is collapsed, not passed through).
	ws := refs[1]
	if ws.Resolvable {
		t.Errorf("whitespace ref = %+v, want unresolvable", ws)
	}
	if ws.RawDisplay != "other/repo #12" {
		t.Errorf("whitespace RawDisplay = %q, want %q (mixed whitespace run collapsed to one space)", ws.RawDisplay, "other/repo #12")
	}
	if !isHex16(ws.RawDigest) {
		t.Errorf("whitespace RawDigest = %q, want 16 hex chars", ws.RawDigest)
	}
}

// TestResolveDependencies_OverflowNumericToken_UnresolvableNoForgeLookup (routed
// #2956 verification): a depends_on marker whose sole token is a regex-matching but
// int-OVERFLOWING digit string produces ONE DroppedEdge stamped
// DropTargetStateUnreadable carrying a digest/display identity, triggers NO GetIssue
// for the (unresolvable) target, and never renders issue:0. This exercises the
// strconv.Atoi failure branch END TO END through the provider — diff-only evidence
// left it unverified.
func TestResolveDependencies_OverflowNumericToken_UnresolvableNoForgeLookup(t *testing.T) {
	api := &fakeAPI{getIssues: map[int]*githubclient.Issue{
		2032: {Number: 2032, Title: "in-set", Body: "Depends on: 99999999999999999999", State: "open"},
	}}
	res, err := New(api).ResolveDependencies(context.Background(), resolveReq("2032"))
	if err != nil {
		t.Fatalf("ResolveDependencies: %v", err)
	}
	if len(res.DroppedEdges) != 1 {
		t.Fatalf("DroppedEdges = %+v, want exactly one", res.DroppedEdges)
	}
	e := res.DroppedEdges[0]
	if e.Reason != workmgmt.DropTargetStateUnreadable {
		t.Errorf("Reason = %q, want %q", e.Reason, workmgmt.DropTargetStateUnreadable)
	}
	if !isHex16(e.ToRefDigest) {
		t.Errorf("ToRefDigest = %q, want 16 hex chars", e.ToRefDigest)
	}
	if e.ToRef != "99999999999999999999" {
		t.Errorf("ToRef = %q, want the overflow digit string verbatim", e.ToRef)
	}
	if ref := e.TargetRef(); strings.Contains(ref, "issue:0") {
		t.Errorf("TargetRef = %q, must never render issue:0 for an unresolvable overflow target", ref)
	}
	// The overflow target is UNRESOLVABLE, so classifyOutOfSetTarget returns before
	// any forge call: GetIssue is reached ONLY for the in-set fetch of 2032.
	if got := api.getIssueCalls[2032]; got != 1 {
		t.Errorf("GetIssue(2032) called %d times, want 1 (the in-set fetch)", got)
	}
	if len(api.getIssueCalls) != 1 {
		t.Errorf("GetIssue call set = %v, want exactly {2032} — the overflow target must never reach a forge lookup", api.getIssueCalls)
	}
}

// TestResolveDependencies_TwoDistinctCrossRepoTokens_ReportedSeparately (7.v): one
// item body naming TWO distinct cross-repo tokens yields TWO DroppedEdges, both
// DropTargetStateUnreadable, with DIFFERENT ToRefDigests (#2956, CF1 vehicle).
func TestResolveDependencies_TwoDistinctCrossRepoTokens_ReportedSeparately(t *testing.T) {
	api := &fakeAPI{getIssues: map[int]*githubclient.Issue{
		2032: {Number: 2032, Title: "in-set", Body: "Depends on: other/a#1, other/b#2", State: "open"},
	}}
	res, err := New(api).ResolveDependencies(context.Background(), resolveReq("2032"))
	if err != nil {
		t.Fatalf("ResolveDependencies: %v", err)
	}
	if len(res.DroppedEdges) != 2 {
		t.Fatalf("DroppedEdges = %+v, want 2 distinct cross-repo edges", res.DroppedEdges)
	}
	for _, e := range res.DroppedEdges {
		if e.From != 2032 || e.To != 0 || e.Reason != workmgmt.DropTargetStateUnreadable {
			t.Errorf("edge = %+v, want {From:2032 To:0 unreadable}", e)
		}
		if !isHex16(e.ToRefDigest) {
			t.Errorf("edge %+v ToRefDigest not 16 hex", e)
		}
	}
	if res.DroppedEdges[0].ToRefDigest == res.DroppedEdges[1].ToRefDigest {
		t.Errorf("two distinct cross-repo tokens collapsed to one digest %q", res.DroppedEdges[0].ToRefDigest)
	}
}

// TestResolveDependencies_RepeatedIdenticalCrossRepoToken_CollapsesToOneEdge
// (7.vi): the SAME cross-repo token repeated on one item collapses to exactly ONE
// dropped edge (the #2953 condition-3 dedup preserved under the new digest key).
func TestResolveDependencies_RepeatedIdenticalCrossRepoToken_CollapsesToOneEdge(t *testing.T) {
	api := &fakeAPI{getIssues: map[int]*githubclient.Issue{
		2032: {Number: 2032, Title: "in-set", Body: "Depends on: other/a#1, other/a#1", State: "open"},
	}}
	res, err := New(api).ResolveDependencies(context.Background(), resolveReq("2032"))
	if err != nil {
		t.Fatalf("ResolveDependencies: %v", err)
	}
	if len(res.DroppedEdges) != 1 {
		t.Fatalf("DroppedEdges = %+v, want exactly 1 (repeated identical token collapses)", res.DroppedEdges)
	}
}

// TestResolveDependencies_EmptySanitizingTokenAndNumericUnreadableEdgeCoexist
// (7.vii): an item naming BOTH a control-rune-only token (sanitizes to empty) AND
// a numeric ref whose GetIssue errors yields TWO distinct dropped edges whose
// rendered TargetRef strings differ and neither of which is `issue:0` (#2956).
func TestResolveDependencies_EmptySanitizingTokenAndNumericUnreadableEdgeCoexist(t *testing.T) {
	api := &fakeAPI{
		getIssues: map[int]*githubclient.Issue{
			2032: {Number: 2032, Title: "in-set", Body: "Depends on: \x01, #1700", State: "open"},
		},
		getIssueErrs: map[int]error{1700: errors.New("github rejected the read")},
	}
	res, err := New(api).ResolveDependencies(context.Background(), resolveReq("2032"))
	if err != nil {
		t.Fatalf("ResolveDependencies: %v", err)
	}
	if len(res.DroppedEdges) != 2 {
		t.Fatalf("DroppedEdges = %+v, want 2 (empty-sanitizing token + numeric unreadable)", res.DroppedEdges)
	}
	rendered := map[string]bool{}
	for _, e := range res.DroppedEdges {
		if e.Reason != workmgmt.DropTargetStateUnreadable {
			t.Errorf("edge %+v Reason = %q, want unreadable", e, e.Reason)
		}
		tr := e.TargetRef()
		if tr == "issue:0" {
			t.Errorf("edge %+v rendered issue:0 — an unresolvable target must never render issue:0", e)
		}
		rendered[tr] = true
	}
	if len(rendered) != 2 {
		t.Errorf("rendered targets = %v, want two DISTINCT non-issue:0 refs", rendered)
	}
	if !rendered["issue:1700"] {
		t.Errorf("rendered = %v, want the numeric unreadable edge to render issue:1700", rendered)
	}
}

// TestSortEdges_ThirteenUnresolvableEdges_DigestOrdered (7.viii / CF4): 13
// unresolvable edges sharing (From, To=0) with distinct digests, fed in
// NON-digest-ascending (reversed) order, come back digest-ascending. The input
// order is the actual control (operator condition 3): without the ToRefDigest
// tiebreak the comparator treats them as equal and sort.Slice does not preserve
// the reversed input above the insertion-sort threshold, so the exact-order
// assertion fails. 13 (> the 12-element insertion-sort threshold) is chosen so
// pdqsort's partitioning path — not the order-preserving small-slice path — runs.
func TestSortEdges_ThirteenUnresolvableEdges_DigestOrdered(t *testing.T) {
	const n = 13
	edges := make([]workmgmt.DependsEdge, 0, n)
	for i := 0; i < n; i++ {
		d := dependsOnTokenDigest("tok" + strconv.Itoa(i))
		edges = append(edges, workmgmt.DependsEdge{From: 2032, To: 0, Reason: workmgmt.DropTargetStateUnreadable, ToRefDigest: d})
	}
	// Ascending-by-digest expectation.
	wantDigests := make([]string, n)
	for i, e := range edges {
		wantDigests[i] = e.ToRefDigest
	}
	sort.Strings(wantDigests)
	// Feed the fixture in REVERSED (non-ascending) order so input order cannot
	// accidentally satisfy the assertion.
	sort.Slice(edges, func(i, j int) bool { return edges[i].ToRefDigest > edges[j].ToRefDigest })
	sortEdges(edges)
	for i := range edges {
		if edges[i].ToRefDigest != wantDigests[i] {
			t.Fatalf("edges[%d].ToRefDigest = %q, want %q (digest-ascending order not established)", i, edges[i].ToRefDigest, wantDigests[i])
		}
	}
}

// TestEpicChildren_TwoDistinctCrossRepoTokens_ReportedSeparately (7.ix, EpicChildren
// mirror of 7.v): a child naming two distinct cross-repo tokens yields two dropped
// edges with distinct digests.
func TestEpicChildren_TwoDistinctCrossRepoTokens_ReportedSeparately(t *testing.T) {
	res := epicChildrenResultBody(t, 43, "Depends on: other/a#1, other/b#2", nil)
	if len(res.DroppedEdges) != 2 {
		t.Fatalf("DroppedEdges = %+v, want 2 distinct cross-repo edges", res.DroppedEdges)
	}
	if res.DroppedEdges[0].ToRefDigest == res.DroppedEdges[1].ToRefDigest {
		t.Errorf("two distinct cross-repo tokens collapsed to one digest on the epic path")
	}
	for _, e := range res.DroppedEdges {
		if e.From != 43 || e.Reason != workmgmt.DropTargetStateUnreadable {
			t.Errorf("edge = %+v, want {From:43 unreadable}", e)
		}
	}
}

// TestEpicChildren_RepeatedIdenticalCrossRepoToken_CollapsesToOneEdge (7.ix,
// EpicChildren mirror of 7.vi).
func TestEpicChildren_RepeatedIdenticalCrossRepoToken_CollapsesToOneEdge(t *testing.T) {
	res := epicChildrenResultBody(t, 43, "Depends on: other/a#1, other/a#1", nil)
	if len(res.DroppedEdges) != 1 {
		t.Fatalf("DroppedEdges = %+v, want exactly 1 (repeated identical token collapses)", res.DroppedEdges)
	}
}

// ---------------------------------------------------------------------------
// #3113 — bounded-concurrency issue-set resolution.
//
// ResolveDependencies now fetches with bounded concurrency in three phases
// (fetch items -> fetch out-of-set targets -> serial classification). The tests
// below pin the four properties that shape depends on: the emitted result is
// BYTE-IDENTICAL to a serial golden regardless of completion order, the
// concurrency bound holds structurally in BOTH directions, a deadline surfaces
// as the typed *workmgmt.IssueSetResolutionTimeout with honest counts in
// preference to any wrapped fetch error, and a context-TERMINATED target is
// never cached (as distinct from an ordinary 404, which still classifies
// unreadable).
// ---------------------------------------------------------------------------

// jitterAPI perturbs completion ORDER without changing any result, so a
// determinism test observes many different worker interleavings over one
// fixture. It sleeps a sub-millisecond random amount before delegating; the
// sleep competes with no deadline (these tests set none), so it is deliberately
// NOT a timescale.D duration — scaling it would only slow the 50 repetitions.
type jitterAPI struct{ *fakeAPI }

func (a *jitterAPI) GetIssue(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, number int) (*githubclient.Issue, error) {
	time.Sleep(time.Duration(rand.IntN(300)) * time.Microsecond)
	return a.fakeAPI.GetIssue(ctx, scope, repo, number)
}

// jitterFixture is the shared determinism fixture: ten named issues in
// DESCENDING request order (so request order and the ascending Children order
// are different, and a phase that leaked completion order into the output would
// show), carrying one edge of every classification the resolver emits — an
// in-set edge, a satisfied out-of-set target, an open one, a
// closed-but-incomplete one, an unreadable one, and an unresolvable cross-repo
// token.
func jitterFixture() *fakeAPI {
	return &fakeAPI{
		getIssues: map[int]*githubclient.Issue{
			110: {Number: 110, Title: "one-ten", Body: "Depends on: #101\n"},
			109: {Number: 109, Title: "one-nine", Body: "Depends on: #900\n"},
			108: {Number: 108, Title: "one-eight", Body: "Depends on: #901\n"},
			107: {Number: 107, Title: "one-seven", Body: "Depends on: other/repo#5\n"},
			106: {Number: 106, Title: "one-six", Body: "Depends on: #902\n"},
			105: {Number: 105, Title: "one-five", Body: "Depends on: #903\n"},
			104: {Number: 104, Title: "one-four"},
			103: {Number: 103, Title: "one-three"},
			102: {Number: 102, Title: "one-two"},
			101: {Number: 101, Title: "one-one", State: "closed", StateReason: "completed"},
			900: {Number: 900, State: "closed", StateReason: "completed"},
			901: {Number: 901, State: "open"},
			902: {Number: 902, State: "closed", StateReason: "not_planned"},
		},
		// 903 is absent from getIssues, so GetIssue returns a not-found error:
		// an ORDINARY fetch failure on a TARGET, which still classifies unreadable.
		getIssueErrs: map[int]error{},
	}
}

func jitterItems() []string {
	return []string{"110", "109", "108", "107", "106", "105", "104", "103", "102", "101"}
}

// jitterGolden is the SERIAL expectation the concurrent implementation must
// reproduce byte for byte. The unresolvable edge's identity fields are derived
// through the same pure token helpers the parser uses (they are unrelated to
// concurrency), so the golden pins the SHAPE without re-deriving a digest by hand.
func jitterGolden() *workmgmt.EpicChildrenResult {
	canon := dependsOnTokenCanonical("other/repo#5")
	return &workmgmt.EpicChildrenResult{
		Children: []workmgmt.EpicChild{
			{Number: 101, Title: "one-one", Complete: true},
			{Number: 102, Title: "one-two"},
			{Number: 103, Title: "one-three"},
			{Number: 104, Title: "one-four"},
			{Number: 105, Title: "one-five"},
			{Number: 106, Title: "one-six"},
			{Number: 107, Title: "one-seven"},
			{Number: 108, Title: "one-eight"},
			{Number: 109, Title: "one-nine"},
			{Number: 110, Title: "one-ten"},
		},
		Edges: []workmgmt.DependsEdge{{From: 110, To: 101}},
		DroppedEdges: []workmgmt.DependsEdge{
			{From: 105, To: 903, Reason: workmgmt.DropTargetStateUnreadable},
			{From: 106, To: 902, Reason: workmgmt.DropTargetClosedIncomplete},
			{From: 107, To: 0, Reason: workmgmt.DropTargetStateUnreadable,
				ToRef: sanitizeDependsOnToken(canon), ToRefDigest: dependsOnTokenDigest(canon)},
			{From: 108, To: 901, Reason: workmgmt.DropNotChild},
		},
		SatisfiedEdges: []workmgmt.SatisfiedEdge{
			{From: 109, To: 900, State: "closed", StateReason: "completed"},
		},
	}
}

// TestResolveDependenciesByteIdenticalUnderJitter is the determinism pin: 50
// repetitions over a fixture whose fake jitters per-call ordering must all
// marshal to the SAME bytes as each other AND as the serial golden. A phase
// that let completion order reach the output — an append from a worker, a map
// iterated for ordering, a non-deterministic error pick — reddens here.
func TestResolveDependenciesByteIdenticalUnderJitter(t *testing.T) {
	want, err := json.Marshal(jitterGolden())
	if err != nil {
		t.Fatalf("marshal golden: %v", err)
	}
	var first []byte
	for i := 0; i < 50; i++ {
		res, rerr := New(&jitterAPI{fakeAPI: jitterFixture()}).
			ResolveDependencies(context.Background(), resolveReq(jitterItems()...))
		if rerr != nil {
			t.Fatalf("rep %d: ResolveDependencies: %v", i, rerr)
		}
		got, merr := json.Marshal(res)
		if merr != nil {
			t.Fatalf("rep %d: marshal: %v", i, merr)
		}
		if i == 0 {
			first = got
			if string(got) != string(want) {
				t.Fatalf("rep 0 differs from serial golden:\n got %s\nwant %s", got, want)
			}
		}
		if string(got) != string(first) {
			t.Fatalf("rep %d differs from rep 0:\n got %s\nwant %s", i, got, first)
		}
	}
}

// epicJitterGolden is the BY-HAND serial expectation for the
// epicJitterFixture, mirroring jitterGolden's role for the ResolveDependencies
// path. It is derived from documented EpicChildren semantics, NOT captured from
// current output (which would reintroduce the vacuity this pins closes):
//
//   - Children are the three sub-issues sorted ascending by Number (10, 11, 12).
//     None carries a CLOSED+COMPLETED state, so Complete is false for all three;
//     each carries the SubIssue's Body verbatim and no URL/Autonomy.
//   - 12's `Depends on: #11` resolves to a fellow child (isChild[11]), so it is
//     an in-set Edge 12->11.
//   - 11's `Depends on: #900` targets a CLOSED+COMPLETED out-of-set issue, so it
//     is a SatisfiedEdge carrying the target's (state, state_reason).
//   - 10's `Depends on: #901` targets an OPEN out-of-set issue, so it is a
//     DroppedEdge with DropNotChild and no ToRef/ToRefDigest (a numeric ref).
func epicJitterGolden() *workmgmt.EpicChildrenResult {
	return &workmgmt.EpicChildrenResult{
		Children: []workmgmt.EpicChild{
			{Number: 10, Title: "ten", Body: "Depends on: #901\n"},
			{Number: 11, Title: "eleven", Body: "Depends on: #900\n"},
			{Number: 12, Title: "twelve", Body: "Depends on: #11\n"},
		},
		Edges: []workmgmt.DependsEdge{{From: 12, To: 11}},
		DroppedEdges: []workmgmt.DependsEdge{
			{From: 10, To: 901, Reason: workmgmt.DropNotChild},
		},
		SatisfiedEdges: []workmgmt.SatisfiedEdge{
			{From: 11, To: 900, State: "closed", StateReason: "completed"},
		},
	}
}

// TestEpicChildrenByteIdenticalUnderJitter proves the EPIC path's output is
// unchanged by #3113: EpicChildren still resolves serially through
// classifyOutOfSetTarget (now delegating to the extracted pure
// classifyFetchedTarget), so 50 repetitions under the same jittering fake are
// byte-identical — AND byte-identical to a hand-written serial golden, so the
// test pins CONTENT (children, edges, dropped edges, satisfied edges,
// classification), not merely self-consistent DETERMINISM. A deterministic
// regression in any of those — the earlier weak substring check let one through
// — reddens against the golden. This is the guard on the "EpicChildren's output
// stays byte-identical" constraint; a future attempt to share the concurrent
// pool with the epic path would have to keep it green.
//
// COUNTERFACTUAL (operator condition, #3113 fix-up): mutating
// provider.go classifyFetchedTarget's closed+completed case from
// `targetState{satisfied: true, ...}` to `targetState{reason:
// workmgmt.DropNotChild, ...}` reclassifies edge 11->900 from a SatisfiedEdge
// to a DroppedEdge, and this test goes RED against the golden ("rep 0 differs
// from serial golden") — verified by executing the mutation (confirmed landed
// by re-reading the line) and restoring it byte-identically to green.
func TestEpicChildrenByteIdenticalUnderJitter(t *testing.T) {
	newFake := func() *fakeAPI {
		return &fakeAPI{
			parentNode: "EPIC_NODE",
			listSubResults: []githubclient.SubIssue{
				{Number: 12, Title: "twelve", Body: "Depends on: #11\n"},
				{Number: 11, Title: "eleven", Body: "Depends on: #900\n"},
				{Number: 10, Title: "ten", Body: "Depends on: #901\n"},
			},
			getIssues: map[int]*githubclient.Issue{
				900: {Number: 900, State: "closed", StateReason: "completed"},
				901: {Number: 901, State: "open"},
			},
		}
	}
	want, err := json.Marshal(epicJitterGolden())
	if err != nil {
		t.Fatalf("marshal golden: %v", err)
	}
	var first []byte
	for i := 0; i < 50; i++ {
		res, err := New(&jitterAPI{fakeAPI: newFake()}).EpicChildren(context.Background(), workmgmt.EpicChildrenRequest{
			Target: workmgmt.Target{Scope: forge.FromGitHubInstallationID(99), Repo: workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"}},
			Epic:   "#7",
		})
		if err != nil {
			t.Fatalf("rep %d: EpicChildren: %v", i, err)
		}
		got, merr := json.Marshal(res)
		if merr != nil {
			t.Fatalf("rep %d: marshal: %v", i, merr)
		}
		if i == 0 {
			first = got
			if string(got) != string(want) {
				t.Fatalf("rep 0 differs from serial golden:\n got %s\nwant %s", got, want)
			}
		}
		if string(got) != string(first) {
			t.Fatalf("rep %d differs from rep 0:\n got %s\nwant %s", i, got, first)
		}
	}
}

// probeAPI proves the concurrency bound STRUCTURALLY, in both directions, with
// no shared mutable state of its own beyond channels:
//
//   - AT MOST issueSetFetchConcurrency: each call does a NON-BLOCKING send on
//     slots (capacity issueSetFetchConcurrency) on entry and a receive on exit.
//     A failed send is the (N+1)-th concurrent call and records a violation.
//   - AT LEAST issueSetFetchConcurrency: each call announces itself on arrivals
//     and then waits for release, which the test closes only after collecting
//     issueSetFetchConcurrency arrivals. A serial implementation never reaches
//     that many, so the collector times out and closes abort — which unblocks
//     every call so the test FAILS on the missing-release assertion rather than
//     deadlocking.
type probeAPI struct {
	*fakeAPI
	slots      chan struct{}
	violations chan struct{}
	arrivals   chan struct{}
	release    chan struct{}
	abort      chan struct{}
}

func (a *probeAPI) GetIssue(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, number int) (*githubclient.Issue, error) {
	took := false
	select {
	case a.slots <- struct{}{}:
		took = true
	default:
		select {
		case a.violations <- struct{}{}:
		default:
		}
	}
	select {
	case a.arrivals <- struct{}{}:
	default:
	}
	select {
	case <-a.release:
	case <-a.abort:
	}
	if took {
		<-a.slots
	}
	return a.fakeAPI.GetIssue(ctx, scope, repo, number)
}

func TestResolveDependenciesConcurrencyBounded(t *testing.T) {
	const items = 24
	issues := map[int]*githubclient.Issue{}
	refs := make([]string, 0, items)
	for i := 0; i < items; i++ {
		n := 200 + i
		issues[n] = &githubclient.Issue{Number: n, Title: strconv.Itoa(n)}
		refs = append(refs, strconv.Itoa(n))
	}
	probe := &probeAPI{
		fakeAPI:    &fakeAPI{getIssues: issues},
		slots:      make(chan struct{}, issueSetFetchConcurrency),
		violations: make(chan struct{}, items),
		arrivals:   make(chan struct{}, items),
		release:    make(chan struct{}),
		abort:      make(chan struct{}),
	}
	// Every deadline-competing duration is derived through timescale.D so the
	// discrimination ratios hold at any factor.
	collectDeadline := timescale.D(3 * time.Second)
	runDeadline := timescale.D(20 * time.Second)
	go func() {
		for i := 0; i < issueSetFetchConcurrency; i++ {
			select {
			case <-probe.arrivals:
			case <-time.After(collectDeadline):
				close(probe.abort) // never reached the bound: unblock and let the assertion fail.
				return
			}
		}
		close(probe.release)
	}()

	done := make(chan error, 1)
	go func() {
		_, err := New(probe).ResolveDependencies(context.Background(), resolveReq(refs...))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ResolveDependencies: %v", err)
		}
	case <-time.After(runDeadline):
		t.Fatalf("ResolveDependencies did not return within %s", runDeadline)
	}

	select {
	case <-probe.release:
	default:
		t.Fatalf("never observed %d concurrent GetIssue calls: the fetch is not running concurrently", issueSetFetchConcurrency)
	}
	if n := len(probe.violations); n != 0 {
		t.Fatalf("observed %d call(s) beyond the %d-way concurrency bound", n, issueSetFetchConcurrency)
	}
}

// cancelingAPI makes deadline behaviour DETERMINISTIC under concurrency: a
// number in cancelOn cancels the resolution's context and fails; every other
// number succeeds from the canned fixture regardless of context state. So which
// items complete does not depend on which worker won a race.
type cancelingAPI struct {
	*fakeAPI
	cancel   context.CancelFunc
	cancelOn map[int]bool
	// failWith, when set for a number, is returned WITHOUT cancelling anything —
	// the "context-terminated fetch with a live parent context" case that
	// distinguishes an aborted target from an ordinary error.
	failWith map[int]error
}

func (a *cancelingAPI) GetIssue(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, number int) (*githubclient.Issue, error) {
	if err, ok := a.failWith[number]; ok {
		return nil, err
	}
	if a.cancelOn[number] {
		if a.cancel != nil {
			a.cancel()
		}
		return nil, context.Canceled
	}
	return a.fakeAPI.GetIssue(ctx, scope, repo, number)
}

// timeoutFixture: three named issues, none with out-of-set deps, so an item is
// fully resolved exactly when its own fetch completes.
func timeoutFixture() *fakeAPI {
	return &fakeAPI{getIssues: map[int]*githubclient.Issue{
		100: {Number: 100, Title: "a"},
		101: {Number: 101, Title: "b"},
		102: {Number: 102, Title: "c"},
	}}
}

func wantTimeout(t *testing.T, err error, resolved, total, suggested int, phase string) {
	t.Helper()
	var to *workmgmt.IssueSetResolutionTimeout
	if !errors.As(err, &to) {
		t.Fatalf("want *workmgmt.IssueSetResolutionTimeout, got %T: %v", err, err)
	}
	if to.Resolved != resolved || to.Total != total || to.SuggestedLimit != suggested {
		t.Fatalf("counts: got resolved=%d total=%d suggested=%d, want %d/%d/%d",
			to.Resolved, to.Total, to.SuggestedLimit, resolved, total, suggested)
	}
	if phase != "" && to.Phase != phase {
		t.Fatalf("phase: got %q want %q", to.Phase, phase)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("errors.Is(err, context.DeadlineExceeded) must hold, got %v", err)
	}
}

// TestResolveDependenciesPhase1DeadlineTypedTimeout: a deadline during the
// named-issue fetch surfaces as the typed timeout with a PREFIX suggestion, and
// NOT as the wrapped `get issue #N` error the phase-1 error path would return.
func TestResolveDependenciesPhase1DeadlineTypedTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := &cancelingAPI{fakeAPI: timeoutFixture(), cancel: cancel, cancelOn: map[int]bool{102: true}}
	_, err := New(api).ResolveDependencies(ctx, resolveReq("100", "101", "102"))
	if err == nil {
		t.Fatal("want a timeout error")
	}
	if strings.Contains(err.Error(), "get issue #") {
		t.Fatalf("deadline surfaced as a wrapped fetch error: %v", err)
	}
	wantTimeout(t, err, 2, 3, 2, "fetch_items")
}

// TestResolveDependenciesSuggestedLimitPrefixOnly: the fully-resolved set is
// non-empty but is NOT a prefix of the request order (the FIRST item is the one
// that failed), so counts are emitted and the suggestion is 0 — a suggestion is
// never derived from a non-prefix count.
func TestResolveDependenciesSuggestedLimitPrefixOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := &cancelingAPI{fakeAPI: timeoutFixture(), cancel: cancel, cancelOn: map[int]bool{100: true}}
	_, err := New(api).ResolveDependencies(ctx, resolveReq("100", "101", "102"))
	wantTimeout(t, err, 2, 3, 0, "fetch_items")
}

// TestResolveDependenciesPhase2DeadlineTypedTimeout: the named fetch completes,
// then the deadline trips the out-of-set TARGET fetch. The item names an
// unclassified target, so it is NOT counted resolved (the accounting rule is
// uniform across phases) and the phase names the target pass.
func TestResolveDependenciesPhase2DeadlineTypedTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := &cancelingAPI{
		fakeAPI:  &fakeAPI{getIssues: map[int]*githubclient.Issue{100: {Number: 100, Body: "Depends on: #900\n"}}},
		cancel:   cancel,
		cancelOn: map[int]bool{900: true},
	}
	_, err := New(api).ResolveDependencies(ctx, resolveReq("100"))
	wantTimeout(t, err, 0, 1, 0, "classify_targets")
}

// TestResolveDependenciesAbortedTargetNotCachedNotResolved is the
// context-termination distinction, and the phase-3 defensive branch's vehicle.
// The target's fetch returns context.Canceled while the PARENT context stays
// ALIVE, so phase 2's own deadline check does not fire: the target is left
// unclassified and phase 3 refuses with the typed timeout rather than guessing
// a classification. Resolved is 0 — a context-terminated target never counts
// its item resolved.
func TestResolveDependenciesAbortedTargetNotCachedNotResolved(t *testing.T) {
	api := &cancelingAPI{
		fakeAPI:  &fakeAPI{getIssues: map[int]*githubclient.Issue{100: {Number: 100, Body: "Depends on: #900\n"}}},
		failWith: map[int]error{900: context.Canceled},
	}
	_, err := New(api).ResolveDependencies(context.Background(), resolveReq("100"))
	wantTimeout(t, err, 0, 1, 0, "build_result")
}

// TestResolveDependenciesOrdinaryTargetErrorStillClassifies is the SIBLING of
// the case above over the SAME target: an ordinary (non-context) fetch failure
// still caches DropTargetStateUnreadable and the resolution SUCCEEDS. Pairing
// the two proves the distinction is context-TERMINATION, not error-vs-success —
// #2953's meaning of "unreadable" is not widened.
func TestResolveDependenciesOrdinaryTargetErrorStillClassifies(t *testing.T) {
	api := &cancelingAPI{
		fakeAPI:  &fakeAPI{getIssues: map[int]*githubclient.Issue{100: {Number: 100, Body: "Depends on: #900\n"}}},
		failWith: map[int]error{900: errors.New("githubclient: 404 not found")},
	}
	res, err := New(api).ResolveDependencies(context.Background(), resolveReq("100"))
	if err != nil {
		t.Fatalf("ResolveDependencies: %v", err)
	}
	if len(res.DroppedEdges) != 1 || res.DroppedEdges[0].Reason != workmgmt.DropTargetStateUnreadable {
		t.Fatalf("want one target_state_unreadable dropped edge, got %+v", res.DroppedEdges)
	}
}

// TestResolveDependenciesFetchErrorFirstInRequestOrder pins the deterministic
// error pick: with BOTH named items failing and the context ALIVE, the returned
// error names the item FIRST IN REQUEST ORDER. The fixture is DESCENDING (20
// then 10) on purpose — on ascending input the claim is invisible, because
// first-in-request-order and lowest-numbered coincide.
func TestResolveDependenciesFetchErrorFirstInRequestOrder(t *testing.T) {
	api := &cancelingAPI{
		fakeAPI:  &fakeAPI{},
		failWith: map[int]error{20: errors.New("boom-twenty"), 10: errors.New("boom-ten")},
	}
	for i := 0; i < 25; i++ {
		_, err := New(api).ResolveDependencies(context.Background(), resolveReq("20", "10"))
		if err == nil {
			t.Fatal("want a fetch error")
		}
		if !strings.Contains(err.Error(), "get issue #20") {
			t.Fatalf("rep %d: want the first item in request order (#20), got %v", i, err)
		}
		if strings.Contains(err.Error(), "#10") {
			t.Fatalf("rep %d: returned the later item's error: %v", i, err)
		}
	}
}

// TestResolveDependenciesNilNamedIssueFailsClosed: the forge returned neither
// an issue nor an error for a NAMED item. Every downstream phase reads that
// issue's number/title/body, so the resolution fails closed naming the item
// rather than inventing a child.
func TestResolveDependenciesNilNamedIssueFailsClosed(t *testing.T) {
	api := &fakeAPI{getIssues: map[int]*githubclient.Issue{100: nil}}
	_, err := New(api).ResolveDependencies(context.Background(), resolveReq("100"))
	if err == nil || !strings.Contains(err.Error(), "get issue #100: no issue returned") {
		t.Fatalf("want the no-issue-returned refusal, got %v", err)
	}
}

// TestResolveDependenciesUnresolvableRefMakesNoForgeCall / …NonPositive… are
// the preserved no-forge-call guards under the new phasing: an unresolvable
// cross-repo token must never be reduced to a local number and read (which
// could FALSELY satisfy the edge, #2953 condition 2), so phase 2 must not
// enqueue it as a target.
func TestResolveDependenciesUnresolvableRefMakesNoForgeCall(t *testing.T) {
	api := &fakeAPI{getIssues: map[int]*githubclient.Issue{
		100: {Number: 100, Body: "Depends on: other/repo#5\n"},
		5:   {Number: 5, State: "closed", StateReason: "completed"},
	}}
	res, err := New(api).ResolveDependencies(context.Background(), resolveReq("100"))
	if err != nil {
		t.Fatalf("ResolveDependencies: %v", err)
	}
	if n := api.getIssueCalls[5]; n != 0 {
		t.Fatalf("cross-repo token was reduced to local #5 and fetched %d time(s)", n)
	}
	if len(res.SatisfiedEdges) != 0 {
		t.Fatalf("cross-repo token must never satisfy an edge: %+v", res.SatisfiedEdges)
	}
	if len(res.DroppedEdges) != 1 || res.DroppedEdges[0].Reason != workmgmt.DropTargetStateUnreadable {
		t.Fatalf("want one target_state_unreadable dropped edge, got %+v", res.DroppedEdges)
	}
}

// TestResolveDependenciesTargetFetchedOncePerDistinctNumber: the phase-2 target
// set is DISTINCT and first-encounter-ordered, so N references to one target
// still cost ONE GetIssue (#2953's memoization, preserved by the cache being
// built from a deduped fetch rather than filled opportunistically).
func TestResolveDependenciesTargetFetchedOncePerDistinctNumber(t *testing.T) {
	api := &fakeAPI{getIssues: map[int]*githubclient.Issue{
		100: {Number: 100, Body: "Depends on: #900\n"},
		101: {Number: 101, Body: "Depends on: #900\n"},
		102: {Number: 102, Body: "Depends on: #900, #900\n"},
		900: {Number: 900, State: "closed", StateReason: "completed"},
	}}
	if _, err := New(api).ResolveDependencies(context.Background(), resolveReq("100", "101", "102")); err != nil {
		t.Fatalf("ResolveDependencies: %v", err)
	}
	if n := api.getIssueCalls[900]; n != 1 {
		t.Fatalf("target #900 fetched %d times, want exactly 1", n)
	}
}

// TestNeedsTargetFetchRefusesUnresolvableRef and its lookupTargetState twin pin
// the no-forge-call guards on a SYNTHETIC ref — Resolvable=false with a POSITIVE
// number. That shape is unreachable from parseDependsOnMarker today (an
// unresolvable token always carries Number 0), which is precisely why the
// through-the-parser test cannot serve as this control's counterfactual vehicle:
// deleting the Resolvable check there is a no-op. Asserted here directly, the
// guard is the one that stops a cross-repo token ever being reduced to a local
// number and read — the failure mode that could FALSELY satisfy an edge (#2953
// operator condition 2), preserved across the #3113 rephasing.
func TestNeedsTargetFetchRefusesUnresolvableRef(t *testing.T) {
	synthetic := dependsOnRef{Number: 5, Resolvable: false, RawDigest: "d", RawDisplay: "other/repo#5"}
	if needsTargetFetch(synthetic, map[int]bool{}) {
		t.Fatal("an unresolvable ref must never be enqueued for a forge read")
	}
	if needsTargetFetch(dependsOnRef{Number: 0, Resolvable: true}, map[int]bool{}) {
		t.Fatal("a non-positive number must never be enqueued for a forge read")
	}
	if !needsTargetFetch(dependsOnRef{Number: 900, Resolvable: true}, map[int]bool{}) {
		t.Fatal("a resolvable positive out-of-set target must be fetched")
	}
	if needsTargetFetch(dependsOnRef{Number: 900, Resolvable: true}, map[int]bool{900: true}) {
		t.Fatal("an IN-SET target must never be re-fetched as an out-of-set target")
	}
}

func TestLookupTargetStateAnswersNoForgeCallRefsWithoutCache(t *testing.T) {
	empty := map[int]targetState{}
	synthetic := dependsOnRef{Number: 5, Resolvable: false, RawDigest: "d", RawDisplay: "other/repo#5"}
	ts, ok := lookupTargetState(synthetic, empty)
	if !ok || ts.satisfied || ts.reason != workmgmt.DropTargetStateUnreadable {
		t.Fatalf("an unresolvable ref must classify unreadable WITHOUT a cache entry, got %+v ok=%v", ts, ok)
	}
	ts, ok = lookupTargetState(dependsOnRef{Number: 0, Resolvable: true}, empty)
	if !ok || ts.satisfied || ts.reason != workmgmt.DropTargetStateUnreadable {
		t.Fatalf("a non-positive number must classify unreadable WITHOUT a cache entry, got %+v ok=%v", ts, ok)
	}
	if _, ok := lookupTargetState(dependsOnRef{Number: 900, Resolvable: true}, empty); ok {
		t.Fatal("an uncached resolvable target must report unclassified, not a guessed classification")
	}
}

// TestFetchIssuesBoundedAbortedRequiresError pins the `err != nil` qualifier on
// the aborted flag directly, because it is unobservable through
// ResolveDependencies: whenever the parent context is dead, phase 2's own
// deadline check returns the typed timeout before a mislabelled result could be
// read. The guard is what keeps a fetch that SUCCEEDED just before the deadline
// from being discarded as context-terminated, so it is asserted at the helper.
func TestFetchIssuesBoundedAbortedRequiresError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // dead context, but the fake below ignores it and succeeds.
	api := &fakeAPI{getIssues: map[int]*githubclient.Issue{
		900: {Number: 900, State: "closed", StateReason: "completed"},
		901: nil,
	}}
	api.getIssueErrs = map[int]error{901: context.Canceled}
	got := New(api).fetchIssuesBounded(ctx, forge.FromGitHubInstallationID(99),
		forge.RepoRef{Owner: "kuhlman-labs", Name: "fishhawk"}, []int{900, 901})
	if len(got) != 2 {
		t.Fatalf("want one result per input, got %d", len(got))
	}
	if got[0].number != 900 || got[0].err != nil || got[0].aborted {
		t.Fatalf("a SUCCESSFUL fetch must never be marked aborted, got %+v", got[0])
	}
	if got[1].number != 901 || !got[1].aborted {
		t.Fatalf("a context-terminated fetch must be marked aborted, got %+v", got[1])
	}
}

// TestFetchIssuesBoundedIndexesByRequestOrder: every result slot is filled and
// indexed by REQUEST order, not completion order — the property the
// deterministic first-in-request-order error pick and the prefix-based
// SuggestedLimit both rest on.
func TestFetchIssuesBoundedIndexesByRequestOrder(t *testing.T) {
	issues := map[int]*githubclient.Issue{}
	nums := []int{}
	for i := 0; i < 20; i++ {
		n := 500 - i // descending, so request order != ascending order
		issues[n] = &githubclient.Issue{Number: n}
		nums = append(nums, n)
	}
	got := New(&jitterAPI{fakeAPI: &fakeAPI{getIssues: issues}}).fetchIssuesBounded(
		context.Background(), forge.FromGitHubInstallationID(99),
		forge.RepoRef{Owner: "kuhlman-labs", Name: "fishhawk"}, nums)
	for i, f := range got {
		if f.ordinal != i || f.number != nums[i] || f.issue == nil {
			t.Fatalf("slot %d: got %+v, want ordinal %d number %d", i, f, i, nums[i])
		}
	}
}
