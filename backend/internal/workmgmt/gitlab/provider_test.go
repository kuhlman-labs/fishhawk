package gitlab

import (
	"context"
	"errors"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/gitlabclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// fakeAPI is a gitlab API test double: it records the resolve/create/link
// calls and returns canned results or configured errors.
type fakeAPI struct {
	getPath    string
	project    *gitlabclient.Project
	getErr     error
	createID   int
	createParm gitlabclient.CreateIssueParams
	created    *gitlabclient.CreatedIssue
	createErr  error
	linkCalled bool
	linkProj   int
	linkIID    int
	linkTarget int
	linkErr    error
}

func (f *fakeAPI) GetProject(_ context.Context, path string) (*gitlabclient.Project, error) {
	f.getPath = path
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.project != nil {
		return f.project, nil
	}
	return &gitlabclient.Project{ID: 42, WebURL: "https://gitlab.com/acme/widgets"}, nil
}

func (f *fakeAPI) CreateIssue(_ context.Context, projectID int, p gitlabclient.CreateIssueParams) (*gitlabclient.CreatedIssue, error) {
	f.createID = projectID
	f.createParm = p
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.created != nil {
		return f.created, nil
	}
	return &gitlabclient.CreatedIssue{IID: 7, WebURL: "https://gitlab.com/acme/widgets/-/issues/7"}, nil
}

func (f *fakeAPI) LinkIssues(_ context.Context, projectID, iid, targetIID int) error {
	f.linkCalled = true
	f.linkProj = projectID
	f.linkIID = iid
	f.linkTarget = targetIID
	return f.linkErr
}

func req(item workmgmt.WorkItem, conn *workmgmt.GitLabConnection, repo workmgmt.Repo) workmgmt.ProviderRequest {
	return workmgmt.ProviderRequest{Item: item, Target: workmgmt.Target{Repo: repo, GitLab: conn}}
}

func containsLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

// TestFile_CreateOnly asserts the happy create path: the conventions-
// resolved title/body/labels reach CreateIssue, the repo owner/name path is
// resolved when no override is set, and the created iid + web URL are echoed
// (done-means behavioral assertions on shipped output).
func TestFile_CreateOnly(t *testing.T) {
	api := &fakeAPI{}
	p := New(api)

	item := workmgmt.WorkItem{
		Type:           "bug",
		Title:          "[E22.5] Crash on save",
		Body:           "## Summary\nIt crashes.",
		Classification: workmgmt.Classification{Labels: []string{"type:bug", "area:backend"}},
	}
	created, err := p.File(context.Background(), req(item, &workmgmt.GitLabConnection{}, workmgmt.Repo{Owner: "acme", Name: "widgets"}))
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	if api.getPath != "acme/widgets" {
		t.Errorf("GetProject path = %q, want acme/widgets (repo path fallback)", api.getPath)
	}
	if api.createID != 42 {
		t.Errorf("CreateIssue projectID = %d, want 42 (resolved id)", api.createID)
	}
	if api.createParm.Title != item.Title {
		t.Errorf("Title = %q, want %q", api.createParm.Title, item.Title)
	}
	if api.createParm.Description != item.Body {
		t.Errorf("Description = %q, want %q", api.createParm.Description, item.Body)
	}
	if len(api.createParm.Labels) != 2 || api.createParm.Labels[0] != "type:bug" {
		t.Errorf("Labels = %v, want the resolved labels", api.createParm.Labels)
	}
	if created.Provider != ProviderName {
		t.Errorf("Provider = %q, want %q", created.Provider, ProviderName)
	}
	if created.Number != 7 {
		t.Errorf("Number = %d, want 7 (issue iid)", created.Number)
	}
	if created.URL != "https://gitlab.com/acme/widgets/-/issues/7" {
		t.Errorf("URL = %q", created.URL)
	}
	if len(created.AppliedLabels) != 2 {
		t.Errorf("AppliedLabels = %v, want the two resolved labels echoed", created.AppliedLabels)
	}
	if created.EpicLinked {
		t.Error("EpicLinked = true, want false with no parent requested")
	}
	if created.Boarded {
		t.Error("Boarded = true, want false with no status configured")
	}
	if api.linkCalled {
		t.Error("LinkIssues called with no parent epic requested")
	}
}

// TestFile_ProjectOverride asserts the conventions gitlab.project override
// wins over the filing repo path.
func TestFile_ProjectOverride(t *testing.T) {
	api := &fakeAPI{}
	p := New(api)
	conn := &workmgmt.GitLabConnection{Project: "group/subgroup/tracker"}

	if _, err := p.File(context.Background(), req(workmgmt.WorkItem{Type: "bug", Title: "x"}, conn, workmgmt.Repo{Owner: "acme", Name: "widgets"})); err != nil {
		t.Fatalf("File: %v", err)
	}
	if api.getPath != "group/subgroup/tracker" {
		t.Errorf("GetProject path = %q, want the gitlab.project override", api.getPath)
	}
}

// TestFile_BoardStatusLabel asserts a configured board status rides the
// create as a label (GitLab boards are label-driven) and Boarded is true.
func TestFile_BoardStatusLabel(t *testing.T) {
	api := &fakeAPI{}
	p := New(api)
	item := workmgmt.WorkItem{
		Type:           "bug",
		Title:          "t",
		Classification: workmgmt.Classification{Labels: []string{"type:bug"}},
		BoardPlacement: workmgmt.BoardPlacement{Status: "workflow::in progress"},
	}

	created, err := p.File(context.Background(), req(item, &workmgmt.GitLabConnection{}, workmgmt.Repo{Owner: "acme", Name: "widgets"}))
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if !containsLabel(api.createParm.Labels, "workflow::in progress") {
		t.Errorf("create labels = %v, want the board-status label included", api.createParm.Labels)
	}
	if !containsLabel(created.AppliedLabels, "workflow::in progress") {
		t.Errorf("AppliedLabels = %v, want the board-status label echoed", created.AppliedLabels)
	}
	if !created.Boarded {
		t.Error("Boarded = false, want true when the status label rode the create")
	}
	if created.BoardingError != "" {
		t.Errorf("BoardingError = %q, want empty on a label-driven placement", created.BoardingError)
	}
	if created.Status != "workflow::in progress" {
		t.Errorf("Status = %q, want the configured status echoed", created.Status)
	}
}

// TestFile_NoStatus_NotBoarded asserts an empty board status leaves Boarded
// false with an empty BoardingError (nothing to board) and no status label.
func TestFile_NoStatus_NotBoarded(t *testing.T) {
	api := &fakeAPI{}
	p := New(api)
	item := workmgmt.WorkItem{Type: "bug", Title: "t", Classification: workmgmt.Classification{Labels: []string{"type:bug"}}}

	created, err := p.File(context.Background(), req(item, &workmgmt.GitLabConnection{}, workmgmt.Repo{Owner: "acme", Name: "widgets"}))
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if len(api.createParm.Labels) != 1 || api.createParm.Labels[0] != "type:bug" {
		t.Errorf("create labels = %v, want only the resolved labels (no status label)", api.createParm.Labels)
	}
	if created.Boarded {
		t.Error("Boarded = true, want false with no status configured")
	}
	if created.BoardingError != "" {
		t.Errorf("BoardingError = %q, want empty when there was nothing to board", created.BoardingError)
	}
}

// TestFile_ParentLink asserts a parent epic drives the Free-tier issue link
// with the parsed iid and reports EpicLinked true on success.
func TestFile_ParentLink(t *testing.T) {
	api := &fakeAPI{}
	p := New(api)
	item := workmgmt.WorkItem{Type: "feature", Title: "t", Relations: workmgmt.Relations{ParentEpic: "#100"}}

	created, err := p.File(context.Background(), req(item, &workmgmt.GitLabConnection{}, workmgmt.Repo{Owner: "acme", Name: "widgets"}))
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if !api.linkCalled || api.linkProj != 42 || api.linkIID != 7 || api.linkTarget != 100 {
		t.Errorf("LinkIssues not driven correctly: called=%v proj=%d iid=%d target=%d",
			api.linkCalled, api.linkProj, api.linkIID, api.linkTarget)
	}
	if !created.EpicLinked {
		t.Error("EpicLinked = false, want true when LinkIssues succeeded")
	}
	if created.EpicLinkError != "" {
		t.Errorf("EpicLinkError = %q, want empty on success", created.EpicLinkError)
	}
}

// TestFile_ParentLink_BestEffort asserts a LinkIssues failure records the
// cause in EpicLinkError, leaves EpicLinked false, and STILL returns the
// created item with a nil error (best-effort #1107).
func TestFile_ParentLink_BestEffort(t *testing.T) {
	api := &fakeAPI{linkErr: errors.New("link forbidden")}
	p := New(api)
	item := workmgmt.WorkItem{Type: "feature", Title: "t", Relations: workmgmt.Relations{ParentEpic: "100"}}

	created, err := p.File(context.Background(), req(item, &workmgmt.GitLabConnection{}, workmgmt.Repo{Owner: "acme", Name: "widgets"}))
	if err != nil {
		t.Fatalf("File returned error on a best-effort link failure: %v", err)
	}
	if created == nil || created.URL == "" || created.Number != 7 {
		t.Fatalf("durable issue not echoed on a link failure: %+v", created)
	}
	if created.EpicLinked {
		t.Error("EpicLinked = true, want false on a link failure")
	}
	if created.EpicLinkError == "" {
		t.Error("EpicLinkError empty, want the link cause")
	}
}

// TestFile_ParentLink_UnparseableRef asserts a non-numeric parent ref is a
// best-effort link failure: EpicLinkError set, EpicLinked false, no
// LinkIssues call, issue still returned.
func TestFile_ParentLink_UnparseableRef(t *testing.T) {
	api := &fakeAPI{}
	p := New(api)
	item := workmgmt.WorkItem{Type: "feature", Title: "t", Relations: workmgmt.Relations{ParentEpic: "EPIC-nope"}}

	created, err := p.File(context.Background(), req(item, &workmgmt.GitLabConnection{}, workmgmt.Repo{Owner: "acme", Name: "widgets"}))
	if err != nil {
		t.Fatalf("File returned error on an unparseable parent ref: %v", err)
	}
	if api.linkCalled {
		t.Error("LinkIssues called despite an unparseable parent ref")
	}
	if created.EpicLinked {
		t.Error("EpicLinked = true, want false on an unparseable ref")
	}
	if created.EpicLinkError == "" {
		t.Error("EpicLinkError empty, want the parse cause")
	}
}

// TestFile_NoParent_NoLink asserts an empty ParentEpic makes no LinkIssues
// call and leaves EpicLinked false with no error.
func TestFile_NoParent_NoLink(t *testing.T) {
	api := &fakeAPI{}
	p := New(api)
	item := workmgmt.WorkItem{Type: "feature", Title: "t"}

	created, err := p.File(context.Background(), req(item, &workmgmt.GitLabConnection{}, workmgmt.Repo{Owner: "acme", Name: "widgets"}))
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if api.linkCalled {
		t.Error("LinkIssues called with no parent epic requested")
	}
	if created.EpicLinked {
		t.Error("EpicLinked = true, want false with no parent requested")
	}
	if created.EpicLinkError != "" {
		t.Errorf("EpicLinkError = %q, want empty with no parent requested", created.EpicLinkError)
	}
}

// TestFile_GetProjectFatal asserts a GetProject failure is fatal: File
// returns a nil item and the wrapped error, and never attempts a create or
// link.
func TestFile_GetProjectFatal(t *testing.T) {
	api := &fakeAPI{getErr: errors.New("project not found")}
	p := New(api)

	created, err := p.File(context.Background(), req(workmgmt.WorkItem{Type: "bug", Title: "t"}, &workmgmt.GitLabConnection{}, workmgmt.Repo{Owner: "acme", Name: "widgets"}))
	if err == nil {
		t.Fatal("File returned nil error on a GetProject failure")
	}
	if created != nil {
		t.Errorf("File returned a non-nil item on a GetProject failure: %+v", created)
	}
	if api.createID != 0 {
		t.Error("CreateIssue attempted despite the GetProject failure")
	}
	if api.linkCalled {
		t.Error("LinkIssues attempted despite the GetProject failure")
	}
}

// TestFile_CreateFatal asserts a CreateIssue failure is fatal (no durable
// issue exists): File returns a nil item, the wrapped error, and no link.
func TestFile_CreateFatal(t *testing.T) {
	api := &fakeAPI{createErr: errors.New("insufficient scope")}
	p := New(api)
	item := workmgmt.WorkItem{Type: "bug", Title: "t", Relations: workmgmt.Relations{ParentEpic: "#100"}}

	created, err := p.File(context.Background(), req(item, &workmgmt.GitLabConnection{}, workmgmt.Repo{Owner: "acme", Name: "widgets"}))
	if err == nil {
		t.Fatal("File returned nil error on a create failure")
	}
	if created != nil {
		t.Errorf("File returned a non-nil item on a create failure: %+v", created)
	}
	if api.linkCalled {
		t.Error("LinkIssues attempted despite the create failure")
	}
}

// TestFile_MissingConnection asserts a nil gitlab connection errors before
// any API call.
func TestFile_MissingConnection(t *testing.T) {
	api := &fakeAPI{}
	p := New(api)
	created, err := p.File(context.Background(), req(workmgmt.WorkItem{Type: "bug", Title: "t"}, nil, workmgmt.Repo{Owner: "acme", Name: "widgets"}))
	if err == nil {
		t.Fatal("File returned nil error, want a missing-connection error")
	}
	if created != nil {
		t.Errorf("File returned a non-nil item on a missing connection: %+v", created)
	}
	if api.getPath != "" {
		t.Error("GetProject called despite the missing connection")
	}
}

// TestFile_NoTargetProject asserts that with no gitlab.project override AND
// no filing repo, File fails closed before any API call.
func TestFile_NoTargetProject(t *testing.T) {
	api := &fakeAPI{}
	p := New(api)
	created, err := p.File(context.Background(), req(workmgmt.WorkItem{Type: "bug", Title: "t"}, &workmgmt.GitLabConnection{}, workmgmt.Repo{}))
	if err == nil {
		t.Fatal("File returned nil error, want a no-target-project error")
	}
	if created != nil {
		t.Errorf("File returned a non-nil item with no target project: %+v", created)
	}
	if api.getPath != "" {
		t.Error("GetProject called despite the missing target project")
	}
}

// TestFile_MissingAPIClient asserts a Provider with no API client fails
// closed rather than panicking on a nil dispatch.
func TestFile_MissingAPIClient(t *testing.T) {
	p := New(nil)
	if _, err := p.File(context.Background(), req(workmgmt.WorkItem{Type: "bug", Title: "t"}, &workmgmt.GitLabConnection{}, workmgmt.Repo{Owner: "acme", Name: "widgets"})); err == nil {
		t.Fatal("File returned nil error with no API client")
	}
}

// TestName asserts the registry id.
func TestName(t *testing.T) {
	if got := New(nil).Name(); got != "gitlab" {
		t.Errorf("Name() = %q, want gitlab", got)
	}
	if ProviderName != "gitlab" {
		t.Errorf("ProviderName = %q, want gitlab", ProviderName)
	}
}

// TestParseIssueRef covers the numeric-ref parsing edge cases shared with
// the github/jira siblings.
func TestParseIssueRef(t *testing.T) {
	ok := map[string]int{"#123": 123, "123": 123, " #7 ": 7}
	for ref, want := range ok {
		got, err := parseIssueRef(ref)
		if err != nil || got != want {
			t.Errorf("parseIssueRef(%q) = %d, %v; want %d, nil", ref, got, err, want)
		}
	}
	for _, ref := range []string{"", "abc", "#0", "-3", "#-1"} {
		if _, err := parseIssueRef(ref); err == nil {
			t.Errorf("parseIssueRef(%q) = nil error, want a parse error", ref)
		}
	}
}
