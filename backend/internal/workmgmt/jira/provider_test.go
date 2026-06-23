package jira

import (
	"context"
	"errors"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/jiraclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// fakeAPI is a jira API test double: it records the create/transition
// calls and returns canned results or configured errors.
type fakeAPI struct {
	createParams jiraclient.CreateIssueParams
	created      *jiraclient.CreatedIssue
	createErr    error
	linkCalled   bool
	linkKey      string
	linkField    string
	linkEpic     string
	linkErr      error
	transKey     string
	transTarget  string
	transCalled  bool
	transErr     error
}

func (f *fakeAPI) CreateIssue(_ context.Context, p jiraclient.CreateIssueParams) (*jiraclient.CreatedIssue, error) {
	f.createParams = p
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.created != nil {
		return f.created, nil
	}
	return &jiraclient.CreatedIssue{Key: "FISH-7", ID: "10007", URL: "https://acme.atlassian.net/browse/FISH-7"}, nil
}

func (f *fakeAPI) LinkParent(_ context.Context, issueKey, fieldName, epicKey string) error {
	f.linkCalled = true
	f.linkKey = issueKey
	f.linkField = fieldName
	f.linkEpic = epicKey
	return f.linkErr
}

func (f *fakeAPI) Transition(_ context.Context, key, target string) error {
	f.transCalled = true
	f.transKey = key
	f.transTarget = target
	return f.transErr
}

func req(item workmgmt.WorkItem, conn *workmgmt.JiraConnection) workmgmt.ProviderRequest {
	return workmgmt.ProviderRequest{Item: item, Target: workmgmt.Target{Jira: conn}}
}

// TestFile_CreateOnly asserts the happy create path: the conventions-
// resolved title/body/labels and the mapped issue type reach CreateIssue,
// and the created key's numeric suffix + browse URL are echoed.
func TestFile_CreateOnly(t *testing.T) {
	api := &fakeAPI{}
	p := New(api)

	item := workmgmt.WorkItem{
		Type:           "bug",
		Title:          "[E22.5] Crash on save",
		Body:           "## Summary\nIt crashes.",
		Classification: workmgmt.Classification{Labels: []string{"type:bug", "area:backend"}},
	}
	created, err := p.File(context.Background(), req(item, &workmgmt.JiraConnection{ProjectKey: "FISH"}))
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	if api.createParams.ProjectKey != "FISH" {
		t.Errorf("ProjectKey = %q, want FISH", api.createParams.ProjectKey)
	}
	if api.createParams.IssueType != "Bug" {
		t.Errorf("IssueType = %q, want Bug (title-cased default)", api.createParams.IssueType)
	}
	if api.createParams.Summary != item.Title {
		t.Errorf("Summary = %q, want %q", api.createParams.Summary, item.Title)
	}
	if api.createParams.Description != item.Body {
		t.Errorf("Description = %q, want %q", api.createParams.Description, item.Body)
	}
	if len(api.createParams.Labels) != 2 || api.createParams.Labels[0] != "type:bug" {
		t.Errorf("Labels = %v, want the resolved labels", api.createParams.Labels)
	}
	if created.Provider != ProviderName {
		t.Errorf("Provider = %q, want %q", created.Provider, ProviderName)
	}
	if created.Number != 7 {
		t.Errorf("Number = %d, want 7 (suffix of FISH-7)", created.Number)
	}
	if created.URL != "https://acme.atlassian.net/browse/FISH-7" {
		t.Errorf("URL = %q", created.URL)
	}
	if created.EpicLinked {
		t.Error("EpicLinked = true, want false with no parent requested")
	}
	if created.Boarded {
		t.Error("Boarded = true, want false with no status configured")
	}
	if api.transCalled {
		t.Error("Transition called with no status configured")
	}
}

// TestFile_IssueTypeOverride asserts an explicit issue_types entry wins
// over the title-cased default.
func TestFile_IssueTypeOverride(t *testing.T) {
	api := &fakeAPI{}
	p := New(api)
	conn := &workmgmt.JiraConnection{ProjectKey: "FISH", IssueTypes: map[string]string{"adr": "Decision Record"}}

	if _, err := p.File(context.Background(), req(workmgmt.WorkItem{Type: "adr", Title: "x"}, conn)); err != nil {
		t.Fatalf("File: %v", err)
	}
	if api.createParams.IssueType != "Decision Record" {
		t.Errorf("IssueType = %q, want the issue_types override", api.createParams.IssueType)
	}
}

// TestFile_ParentLink_ConfiguredField asserts a configured custom parent_field
// is forwarded verbatim to the post-create LinkParent call and EpicLinked is
// reported true on success (the create call no longer carries the parent).
func TestFile_ParentLink_ConfiguredField(t *testing.T) {
	api := &fakeAPI{}
	p := New(api)
	item := workmgmt.WorkItem{Type: "feature", Title: "t", Relations: workmgmt.Relations{ParentEpic: "FISH-100"}}
	conn := &workmgmt.JiraConnection{ProjectKey: "FISH", ParentField: "customfield_10014"}

	created, err := p.File(context.Background(), req(item, conn))
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if !api.linkCalled || api.linkKey != "FISH-7" || api.linkField != "customfield_10014" || api.linkEpic != "FISH-100" {
		t.Errorf("LinkParent not driven correctly: called=%v key=%q field=%q epic=%q",
			api.linkCalled, api.linkKey, api.linkField, api.linkEpic)
	}
	if !created.EpicLinked {
		t.Error("EpicLinked = false, want true when LinkParent succeeded")
	}
	if created.EpicLinkError != "" {
		t.Errorf("EpicLinkError = %q, want empty on success", created.EpicLinkError)
	}
}

// TestFile_ParentLink_DefaultsToParentField asserts an empty parent_field
// defaults to the team-managed "parent" reference at the LinkParent seam.
func TestFile_ParentLink_DefaultsToParentField(t *testing.T) {
	api := &fakeAPI{}
	p := New(api)
	item := workmgmt.WorkItem{Type: "feature", Title: "t", Relations: workmgmt.Relations{ParentEpic: "FISH-100"}}

	created, err := p.File(context.Background(), req(item, &workmgmt.JiraConnection{ProjectKey: "FISH"}))
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if !api.linkCalled || api.linkField != "parent" || api.linkEpic != "FISH-100" {
		t.Errorf("LinkParent field = %q (epic %q), want default \"parent\"", api.linkField, api.linkEpic)
	}
	if !created.EpicLinked {
		t.Error("EpicLinked = false, want true when LinkParent succeeded")
	}
}

// TestFile_ParentLink_BestEffort asserts a LinkParent failure records the
// cause in EpicLinkError, leaves EpicLinked false, and STILL returns the
// created item with a nil error (best-effort #1107).
func TestFile_ParentLink_BestEffort(t *testing.T) {
	api := &fakeAPI{linkErr: errors.New("field not on screen")}
	p := New(api)
	item := workmgmt.WorkItem{Type: "feature", Title: "t", Relations: workmgmt.Relations{ParentEpic: "FISH-100"}}

	created, err := p.File(context.Background(), req(item, &workmgmt.JiraConnection{ProjectKey: "FISH"}))
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

// TestFile_NoParent_NoLink asserts an empty ParentEpic makes no LinkParent
// call and leaves EpicLinked false with no error.
func TestFile_NoParent_NoLink(t *testing.T) {
	api := &fakeAPI{}
	p := New(api)
	item := workmgmt.WorkItem{Type: "feature", Title: "t"}

	created, err := p.File(context.Background(), req(item, &workmgmt.JiraConnection{ProjectKey: "FISH"}))
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if api.linkCalled {
		t.Error("LinkParent called with no parent epic requested")
	}
	if created.EpicLinked {
		t.Error("EpicLinked = true, want false with no parent requested")
	}
	if created.EpicLinkError != "" {
		t.Errorf("EpicLinkError = %q, want empty with no parent requested", created.EpicLinkError)
	}
}

// TestFile_Transition asserts a configured board status drives a
// best-effort workflow transition and reports Boarded on success.
func TestFile_Transition(t *testing.T) {
	api := &fakeAPI{}
	p := New(api)
	item := workmgmt.WorkItem{Type: "bug", Title: "t", BoardPlacement: workmgmt.BoardPlacement{Status: "In Progress"}}

	created, err := p.File(context.Background(), req(item, &workmgmt.JiraConnection{ProjectKey: "FISH"}))
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if !api.transCalled || api.transKey != "FISH-7" || api.transTarget != "In Progress" {
		t.Errorf("transition not driven correctly: called=%v key=%q target=%q", api.transCalled, api.transKey, api.transTarget)
	}
	if !created.Boarded {
		t.Error("Boarded = false, want true on a successful transition")
	}
	if created.BoardingError != "" {
		t.Errorf("BoardingError = %q, want empty on success", created.BoardingError)
	}
}

// TestFile_TransitionBestEffort asserts a transition failure leaves the
// created issue durable: File returns the item with a nil error,
// Boarded=false, and the cause in BoardingError (#1107).
func TestFile_TransitionBestEffort(t *testing.T) {
	api := &fakeAPI{transErr: errors.New("no transition to target status")}
	p := New(api)
	item := workmgmt.WorkItem{Type: "bug", Title: "t", BoardPlacement: workmgmt.BoardPlacement{Status: "Done"}}

	created, err := p.File(context.Background(), req(item, &workmgmt.JiraConnection{ProjectKey: "FISH"}))
	if err != nil {
		t.Fatalf("File returned error on a best-effort transition failure: %v", err)
	}
	if created.URL == "" || created.Number != 7 {
		t.Errorf("durable issue not echoed: %+v", created)
	}
	if created.Boarded {
		t.Error("Boarded = true, want false on a transition failure")
	}
	if created.BoardingError == "" {
		t.Error("BoardingError empty, want the transition cause")
	}
}

// TestFile_CreateFatal asserts a CreateIssue failure is fatal (no durable
// issue exists): File returns a nil item and the wrapped error.
func TestFile_CreateFatal(t *testing.T) {
	api := &fakeAPI{createErr: errors.New("project FISH not found")}
	p := New(api)

	created, err := p.File(context.Background(), req(workmgmt.WorkItem{Type: "bug", Title: "t"}, &workmgmt.JiraConnection{ProjectKey: "FISH"}))
	if err == nil {
		t.Fatal("File returned nil error on a create failure")
	}
	if created != nil {
		t.Errorf("File returned a non-nil item on a create failure: %+v", created)
	}
}

// TestFile_MissingConnection asserts the pre-create guards: a nil jira
// connection and an empty project_key both error before any API call.
func TestFile_MissingConnection(t *testing.T) {
	cases := []struct {
		name string
		conn *workmgmt.JiraConnection
	}{
		{"nil connection", nil},
		{"empty project key", &workmgmt.JiraConnection{ProjectKey: "  "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAPI{}
			p := New(api)
			_, err := p.File(context.Background(), req(workmgmt.WorkItem{Type: "bug", Title: "t"}, tc.conn))
			if err == nil {
				t.Fatal("File returned nil error, want a missing-connection error")
			}
			if api.createParams.ProjectKey != "" {
				t.Error("CreateIssue called despite the failed guard")
			}
		})
	}
}

// TestFile_MissingAPIClient asserts a Provider with no API client fails
// closed rather than panicking on a nil dispatch.
func TestFile_MissingAPIClient(t *testing.T) {
	p := New(nil)
	if _, err := p.File(context.Background(), req(workmgmt.WorkItem{Type: "bug", Title: "t"}, &workmgmt.JiraConnection{ProjectKey: "FISH"})); err == nil {
		t.Fatal("File returned nil error with no API client")
	}
}

// TestNumberFromKey covers the key-suffix parsing edge cases.
func TestNumberFromKey(t *testing.T) {
	cases := map[string]int{
		"FISH-7":       7,
		"ENG-1234":     1234,
		"NODASH":       0,
		"TRAILING-":    0,
		"FISH-abc":     0,
		"MULTI-PART-9": 9,
	}
	for key, want := range cases {
		if got := numberFromKey(key); got != want {
			t.Errorf("numberFromKey(%q) = %d, want %d", key, got, want)
		}
	}
}
