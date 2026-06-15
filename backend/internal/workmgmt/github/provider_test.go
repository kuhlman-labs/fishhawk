package github

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

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

// Provider must satisfy workmgmt.Provider.
var _ workmgmt.Provider = (*Provider)(nil)
