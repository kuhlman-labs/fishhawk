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

// TestFile_ParentLink asserts the parent epic reference and the conventions'
// parent_field are threaded into the create call (the link is applied at
// create time, not a separate post-create call) and EpicLinked is reported true.
func TestFile_ParentLink(t *testing.T) {
	api := &fakeAPI{}
	p := New(api)
	item := workmgmt.WorkItem{Type: "feature", Title: "t", Relations: workmgmt.Relations{ParentEpic: "FISH-100"}}
	conn := &workmgmt.JiraConnection{ProjectKey: "FISH", ParentField: "customfield_10014"}

	created, err := p.File(context.Background(), req(item, conn))
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if api.createParams.ParentKey != "FISH-100" {
		t.Errorf("create ParentKey = %q, want FISH-100", api.createParams.ParentKey)
	}
	if api.createParams.ParentField != "customfield_10014" {
		t.Errorf("create ParentField = %q, want the threaded parent_field customfield_10014", api.createParams.ParentField)
	}
	if !created.EpicLinked {
		t.Error("EpicLinked = false, want true when a parent was requested and the create succeeded")
	}
	if created.EpicLinkError != "" {
		t.Errorf("EpicLinkError = %q, want empty on success", created.EpicLinkError)
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

// stubTransport is a jiraclient.Doer that records every request and answers
// it via respond. It lets the integrated test drive a REAL *jiraclient.Client
// through provider.File and assert the on-the-wire request body.
type stubTransport struct {
	requests []stubReq
	respond  func(stubReq) (*http.Response, error)
}

type stubReq struct {
	method string
	path   string
	body   []byte
}

func (s *stubTransport) Do(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	rec := stubReq{method: r.Method, path: r.URL.Path, body: body}
	s.requests = append(s.requests, rec)
	return s.respond(rec)
}

func stubResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

// TestFile_ParentFieldThreadedToWire is the cross-boundary seam check: it
// threads parent_field from the JiraConnection config through provider.File
// into a REAL *jiraclient.Client and asserts the parent link is emitted in the
// create POST body (no separate post-create call) carrying the parent for both
// field shapes (team-managed `parent` object vs classic bare-string custom
// field), with EpicLinked reported true.
func TestFile_ParentFieldThreadedToWire(t *testing.T) {
	const epicKey = "FISH-100"

	shapeCases := []struct {
		name        string
		parentField string
		assertBody  func(t *testing.T, fields map[string]any)
	}{
		{
			name:        "team-managed default emits parent object",
			parentField: "",
			assertBody: func(t *testing.T, fields map[string]any) {
				obj, ok := fields["parent"].(map[string]any)
				if !ok || obj["key"] != epicKey {
					t.Errorf("parent = %v, want object {key:%s}", fields["parent"], epicKey)
				}
			},
		},
		{
			name:        "classic custom field emits bare string",
			parentField: "customfield_10014",
			assertBody: func(t *testing.T, fields map[string]any) {
				if v, ok := fields["customfield_10014"].(string); !ok || v != epicKey {
					t.Errorf("customfield_10014 = %v, want bare string %s", fields["customfield_10014"], epicKey)
				}
				if _, ok := fields["parent"]; ok {
					t.Error("parent object present for a classic custom-field link")
				}
			},
		},
	}
	for _, tc := range shapeCases {
		t.Run(tc.name, func(t *testing.T) {
			st := &stubTransport{respond: func(r stubReq) (*http.Response, error) {
				if r.method != http.MethodPost { // create only
					t.Fatalf("unexpected method %s; parent must be set at create", r.method)
					return nil, nil
				}
				return stubResp(http.StatusCreated, `{"id":"1","key":"FISH-7"}`), nil
			}}
			client := jiraclient.New("https://acme.atlassian.net", "bot@acme.example", "token", jiraclient.WithHTTPClient(st))
			p := New(client)
			item := workmgmt.WorkItem{Type: "feature", Title: "t", Relations: workmgmt.Relations{ParentEpic: epicKey}}
			created, err := p.File(context.Background(), req(item, &workmgmt.JiraConnection{ProjectKey: "FISH", ParentField: tc.parentField}))
			if err != nil {
				t.Fatalf("File: %v", err)
			}
			if !created.EpicLinked {
				t.Error("EpicLinked = false, want true")
			}

			// The parent link must ride in the create POST body — no
			// separate post-create request.
			var createBody []byte
			for _, r := range st.requests {
				if r.method == http.MethodPost {
					if r.path != "/rest/api/3/issue" {
						t.Errorf("create path = %s, want /rest/api/3/issue", r.path)
					}
					createBody = r.body
				}
			}
			if createBody == nil {
				t.Fatal("no POST (create) request emitted")
			}
			var createGot struct {
				Fields map[string]any `json:"fields"`
			}
			if err := json.Unmarshal(createBody, &createGot); err != nil {
				t.Fatalf("unmarshal create body: %v\nbody=%s", err, createBody)
			}
			tc.assertBody(t, createGot.Fields)
		})
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
