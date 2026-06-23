package jira

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/jiraclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// recordingDoer is a jiraclient.Doer that answers the create POST with a
// canned issue and records that POST body, so the seam test can assert the
// REAL jiraclient-emitted wire shape — including the parent field linked at
// create time.
type recordingDoer struct {
	t       *testing.T
	postReq *struct {
		path string
		body []byte
	}
}

func (d *recordingDoer) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		if body, err = io.ReadAll(req.Body); err != nil {
			d.t.Fatalf("read request body: %v", err)
		}
	}
	switch req.Method {
	case http.MethodPost:
		d.postReq = &struct {
			path string
			body []byte
		}{path: req.URL.Path, body: body}
		return &http.Response{
			StatusCode: http.StatusCreated,
			Body:       io.NopCloser(strings.NewReader(`{"id":"10007","key":"FISH-7"}`)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	default:
		d.t.Fatalf("unexpected method %s", req.Method)
		return nil, nil
	}
}

// TestFile_LinkParentWireSeam threads a configured parent_field all the way
// to the REAL jiraclient-emitted create body: the provider runs on a real
// *jiraclient.Client backed by a recording Doer, not the fakeAPI double,
// proving the configured value's serialization across the provider->client
// seam in one flow — the epic is linked atomically at create time.
func TestFile_LinkParentWireSeam(t *testing.T) {
	cases := []struct {
		name        string
		parentField string
		assert      func(t *testing.T, fields map[string]any)
	}{
		{
			name:        "classic custom field -> bare string",
			parentField: "customfield_10014",
			assert: func(t *testing.T, fields map[string]any) {
				if v := fields["customfield_10014"]; v != "FISH-100" {
					t.Errorf("customfield_10014 = %v (%T), want bare string FISH-100", v, v)
				}
				if _, ok := fields["parent"]; ok {
					t.Errorf("parent set for a custom-field link: %v", fields)
				}
			},
		},
		{
			name:        "default/empty -> parent object",
			parentField: "",
			assert: func(t *testing.T, fields map[string]any) {
				parent, ok := fields["parent"].(map[string]any)
				if !ok {
					t.Fatalf("parent not an object: %v", fields["parent"])
				}
				if parent["key"] != "FISH-100" {
					t.Errorf("parent.key = %v, want FISH-100", parent["key"])
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := &recordingDoer{t: t}
			client := jiraclient.New("https://acme.atlassian.net", "bot@acme.example", "tok", jiraclient.WithHTTPClient(doer))
			p := New(client)
			item := workmgmt.WorkItem{Type: "feature", Title: "t", Relations: workmgmt.Relations{ParentEpic: "FISH-100"}}
			conn := &workmgmt.JiraConnection{ProjectKey: "FISH", ParentField: tc.parentField}

			created, err := p.File(context.Background(), req(item, conn))
			if err != nil {
				t.Fatalf("File: %v", err)
			}
			if !created.EpicLinked {
				t.Errorf("EpicLinked = false, want true; EpicLinkError=%q", created.EpicLinkError)
			}
			if doer.postReq == nil {
				t.Fatal("no create POST was emitted")
			}
			if doer.postReq.path != "/rest/api/3/issue" {
				t.Errorf("POST path = %s, want /rest/api/3/issue", doer.postReq.path)
			}
			var got struct {
				Fields map[string]any `json:"fields"`
			}
			if err := json.Unmarshal(doer.postReq.body, &got); err != nil {
				t.Fatalf("unmarshal POST body: %v\nbody=%s", err, doer.postReq.body)
			}
			tc.assert(t, got.Fields)
		})
	}
}

// fakeAPI is a jira API test double: it records the create/transition
// calls and returns canned results or configured errors.
type fakeAPI struct {
	createParams jiraclient.CreateIssueParams
	created      *jiraclient.CreatedIssue
	createErr    error
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

// TestFile_ParentLinkDefaultField asserts a requested parent with no
// configured parent_field is linked at create time through the resolved
// team-managed `parent` default and reports EpicLinked=true. This locks the
// shipped default-value behavior (#1169): an empty parent_field resolves to
// "parent", so a no-op of the default-resolution line fails here.
func TestFile_ParentLinkDefaultField(t *testing.T) {
	api := &fakeAPI{}
	p := New(api)
	item := workmgmt.WorkItem{Type: "feature", Title: "t", Relations: workmgmt.Relations{ParentEpic: "FISH-100"}}

	created, err := p.File(context.Background(), req(item, &workmgmt.JiraConnection{ProjectKey: "FISH"}))
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if api.createParams.ParentKey != "FISH-100" {
		t.Errorf("ParentKey = %q, want FISH-100 (linked at create)", api.createParams.ParentKey)
	}
	if api.createParams.ParentField != "parent" {
		t.Errorf("ParentField = %q, want the resolved team-managed default %q", api.createParams.ParentField, "parent")
	}
	if !created.EpicLinked {
		t.Error("EpicLinked = false, want true when a parent was requested and the create succeeded")
	}
	if created.EpicLinkError != "" {
		t.Errorf("EpicLinkError = %q, want empty on success", created.EpicLinkError)
	}
}

// TestFile_ParentLinkClassicField asserts a configured classic epic-link
// custom field is threaded through to the create call unchanged.
func TestFile_ParentLinkClassicField(t *testing.T) {
	api := &fakeAPI{}
	p := New(api)
	item := workmgmt.WorkItem{Type: "feature", Title: "t", Relations: workmgmt.Relations{ParentEpic: "FISH-100"}}
	conn := &workmgmt.JiraConnection{ProjectKey: "FISH", ParentField: "customfield_10014"}

	created, err := p.File(context.Background(), req(item, conn))
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if api.createParams.ParentField != "customfield_10014" {
		t.Errorf("ParentField = %q, want the configured custom field", api.createParams.ParentField)
	}
	if api.createParams.ParentKey != "FISH-100" {
		t.Errorf("ParentKey = %q, want FISH-100", api.createParams.ParentKey)
	}
	if !created.EpicLinked {
		t.Error("EpicLinked = false, want true on a successful classic-field link")
	}
}

// TestFile_NoParentNoLink asserts an absent parent epic sends no parent at
// create and leaves EpicLinked false with no error.
func TestFile_NoParentNoLink(t *testing.T) {
	api := &fakeAPI{}
	p := New(api)

	created, err := p.File(context.Background(), req(workmgmt.WorkItem{Type: "bug", Title: "t"}, &workmgmt.JiraConnection{ProjectKey: "FISH"}))
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if api.createParams.ParentKey != "" || api.createParams.ParentField != "" {
		t.Errorf("parent threaded with no parent requested: key=%q field=%q", api.createParams.ParentKey, api.createParams.ParentField)
	}
	if created.EpicLinked || created.EpicLinkError != "" {
		t.Errorf("EpicLinked/EpicLinkError = %v/%q, want false/empty with no parent", created.EpicLinked, created.EpicLinkError)
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
