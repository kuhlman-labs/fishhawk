package jiraclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// stubDoer is a programmable Doer. It records every request it sees and
// returns the queued response for the matching method+path, so each test
// can assert the exact request shape the client emitted.
type stubDoer struct {
	t        *testing.T
	requests []*recordedRequest
	// handler answers a request; tests set it to assert and respond.
	handler func(*recordedRequest) (*http.Response, error)
}

// recordedRequest is a snapshot of a request with its body already read.
type recordedRequest struct {
	method string
	path   string
	header http.Header
	body   []byte
}

func (s *stubDoer) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			s.t.Fatalf("read request body: %v", err)
		}
	}
	rec := &recordedRequest{
		method: req.Method,
		path:   req.URL.Path,
		header: req.Header.Clone(),
		body:   body,
	}
	s.requests = append(s.requests, rec)
	return s.handler(rec)
}

// jsonResponse builds an *http.Response with a JSON body and status.
func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

const (
	testBaseURL = "https://acme.atlassian.net"
	testEmail   = "bot@acme.example"
	testToken   = "s3cr3t-token"
)

// assertBasicAuth verifies the request carries the expected HTTP Basic
// credentials.
func assertBasicAuth(t *testing.T, h http.Header) {
	t.Helper()
	req := &http.Request{Header: h}
	user, pass, ok := req.BasicAuth()
	if !ok {
		t.Fatalf("Authorization header missing or not Basic: %q", h.Get("Authorization"))
	}
	if user != testEmail || pass != testToken {
		t.Fatalf("basic auth = %q:%q, want %q:%q", user, pass, testEmail, testToken)
	}
}

func TestCreateIssue_RequestShapeAndResult(t *testing.T) {
	stub := &stubDoer{t: t}
	stub.handler = func(rec *recordedRequest) (*http.Response, error) {
		if rec.method != http.MethodPost {
			t.Errorf("method = %s, want POST", rec.method)
		}
		if rec.path != "/rest/api/3/issue" {
			t.Errorf("path = %s, want /rest/api/3/issue", rec.path)
		}
		assertBasicAuth(t, rec.header)
		if ct := rec.header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}

		var got struct {
			Fields struct {
				Project   struct{ Key string }  `json:"project"`
				IssueType struct{ Name string } `json:"issuetype"`
				Summary   string                `json:"summary"`
				Labels    []string              `json:"labels"`
				Parent    *struct{ Key string } `json:"parent"`
				Desc      json.RawMessage       `json:"description"`
			} `json:"fields"`
		}
		if err := json.Unmarshal(rec.body, &got); err != nil {
			t.Fatalf("unmarshal request body: %v\nbody=%s", err, rec.body)
		}
		if got.Fields.Project.Key != "ENG" {
			t.Errorf("project.key = %q, want ENG", got.Fields.Project.Key)
		}
		if got.Fields.IssueType.Name != "Task" {
			t.Errorf("issuetype.name = %q, want Task", got.Fields.IssueType.Name)
		}
		if got.Fields.Summary != "Fix the thing" {
			t.Errorf("summary = %q", got.Fields.Summary)
		}
		if len(got.Fields.Labels) != 2 || got.Fields.Labels[0] != "area:backend" {
			t.Errorf("labels = %v", got.Fields.Labels)
		}
		if got.Fields.Parent != nil {
			t.Errorf("parent set without ParentKey: %+v", got.Fields.Parent)
		}
		// Description must be wrapped in ADF (a doc), not a plain string.
		if !bytes.Contains(got.Fields.Desc, []byte(`"type":"doc"`)) {
			t.Errorf("description not ADF: %s", got.Fields.Desc)
		}

		return jsonResponse(http.StatusCreated, `{"id":"10042","key":"ENG-7"}`), nil
	}

	c := New(testBaseURL, testEmail, testToken, WithHTTPClient(stub))
	out, err := c.CreateIssue(context.Background(), CreateIssueParams{
		ProjectKey:  "ENG",
		IssueType:   "Task",
		Summary:     "Fix the thing",
		Description: "line one\n\nline three",
		Labels:      []string{"area:backend", "type:bug"},
	})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if out.Key != "ENG-7" || out.ID != "10042" {
		t.Errorf("result = %+v, want key ENG-7 id 10042", out)
	}
	if out.URL != testBaseURL+"/browse/ENG-7" {
		t.Errorf("URL = %q", out.URL)
	}
}

func TestCreateIssue_WithParentKey(t *testing.T) {
	stub := &stubDoer{t: t}
	stub.handler = func(rec *recordedRequest) (*http.Response, error) {
		var got struct {
			Fields struct {
				Parent struct{ Key string } `json:"parent"`
			} `json:"fields"`
		}
		if err := json.Unmarshal(rec.body, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Fields.Parent.Key != "ENG-1" {
			t.Errorf("parent.key = %q, want ENG-1", got.Fields.Parent.Key)
		}
		return jsonResponse(http.StatusCreated, `{"id":"1","key":"ENG-8"}`), nil
	}

	c := New(testBaseURL, testEmail, testToken, WithHTTPClient(stub))
	if _, err := c.CreateIssue(context.Background(), CreateIssueParams{
		ProjectKey: "ENG",
		IssueType:  "Task",
		Summary:    "child",
		ParentKey:  "ENG-1",
	}); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
}

func TestCreateIssue_OmitsEmptyOptionalFields(t *testing.T) {
	stub := &stubDoer{t: t}
	stub.handler = func(rec *recordedRequest) (*http.Response, error) {
		var got map[string]map[string]any
		if err := json.Unmarshal(rec.body, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		fields := got["fields"]
		if _, ok := fields["description"]; ok {
			t.Error("description present though empty")
		}
		if _, ok := fields["labels"]; ok {
			t.Error("labels present though empty")
		}
		if _, ok := fields["parent"]; ok {
			t.Error("parent present though empty")
		}
		return jsonResponse(http.StatusCreated, `{"id":"1","key":"ENG-9"}`), nil
	}

	c := New(testBaseURL, testEmail, testToken, WithHTTPClient(stub))
	if _, err := c.CreateIssue(context.Background(), CreateIssueParams{
		ProjectKey: "ENG",
		IssueType:  "Task",
		Summary:    "minimal",
	}); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
}

func TestCreateIssue_ValidatesRequiredFields(t *testing.T) {
	c := New(testBaseURL, testEmail, testToken, WithHTTPClient(&stubDoer{
		t: t,
		handler: func(*recordedRequest) (*http.Response, error) {
			t.Fatal("transport called despite invalid params")
			return nil, nil
		},
	}))
	cases := []CreateIssueParams{
		{IssueType: "Task", Summary: "s"},      // missing project
		{ProjectKey: "ENG", Summary: "s"},      // missing issue type
		{ProjectKey: "ENG", IssueType: "Task"}, // missing summary
	}
	for i, p := range cases {
		if _, err := c.CreateIssue(context.Background(), p); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
}

func TestCreateIssue_APIError(t *testing.T) {
	stub := &stubDoer{t: t}
	stub.handler = func(*recordedRequest) (*http.Response, error) {
		return jsonResponse(http.StatusBadRequest, `{"errorMessages":["bad project"]}`), nil
	}
	c := New(testBaseURL, testEmail, testToken, WithHTTPClient(stub))
	_, err := c.CreateIssue(context.Background(), CreateIssueParams{
		ProjectKey: "ENG", IssueType: "Task", Summary: "s",
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", apiErr.StatusCode)
	}
	if !strings.Contains(apiErr.Body, "bad project") {
		t.Errorf("body excerpt = %q, want it to carry the Jira error body", apiErr.Body)
	}
}

func TestTransition_DiscoversAndExecutes(t *testing.T) {
	stub := &stubDoer{t: t}
	stub.handler = func(rec *recordedRequest) (*http.Response, error) {
		assertBasicAuth(t, rec.header)
		if rec.path != "/rest/api/3/issue/ENG-7/transitions" {
			t.Errorf("path = %s", rec.path)
		}
		switch rec.method {
		case http.MethodGet:
			if len(rec.body) != 0 {
				t.Errorf("GET carried a body: %s", rec.body)
			}
			return jsonResponse(http.StatusOK, `{"transitions":[
				{"id":"11","name":"Start","to":{"name":"In Progress"}},
				{"id":"31","name":"Finish","to":{"name":"Done"}}
			]}`), nil
		case http.MethodPost:
			var got struct {
				Transition struct{ ID string } `json:"transition"`
			}
			if err := json.Unmarshal(rec.body, &got); err != nil {
				t.Fatalf("unmarshal transition: %v", err)
			}
			if got.Transition.ID != "31" {
				t.Errorf("transition id = %q, want 31 (the one whose target is Done)", got.Transition.ID)
			}
			return jsonResponse(http.StatusNoContent, ""), nil
		default:
			t.Fatalf("unexpected method %s", rec.method)
			return nil, nil
		}
	}

	c := New(testBaseURL, testEmail, testToken, WithHTTPClient(stub))
	if err := c.Transition(context.Background(), "ENG-7", "Done"); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if len(stub.requests) != 2 {
		t.Errorf("made %d requests, want 2 (GET then POST)", len(stub.requests))
	}
}

func TestTransition_MatchesByTransitionName(t *testing.T) {
	stub := &stubDoer{t: t}
	stub.handler = func(rec *recordedRequest) (*http.Response, error) {
		if rec.method == http.MethodGet {
			return jsonResponse(http.StatusOK, `{"transitions":[
				{"id":"21","name":"Close Issue","to":{"name":"Closed"}}
			]}`), nil
		}
		var got struct {
			Transition struct{ ID string } `json:"transition"`
		}
		_ = json.Unmarshal(rec.body, &got)
		if got.Transition.ID != "21" {
			t.Errorf("transition id = %q, want 21", got.Transition.ID)
		}
		return jsonResponse(http.StatusNoContent, ""), nil
	}

	c := New(testBaseURL, testEmail, testToken, WithHTTPClient(stub))
	// "close issue" matches the transition's own name case-insensitively.
	if err := c.Transition(context.Background(), "ENG-7", "close issue"); err != nil {
		t.Fatalf("Transition: %v", err)
	}
}

func TestTransition_NotFound(t *testing.T) {
	stub := &stubDoer{t: t}
	stub.handler = func(rec *recordedRequest) (*http.Response, error) {
		if rec.method == http.MethodPost {
			t.Fatal("POST should not fire when no transition matches")
		}
		return jsonResponse(http.StatusOK, `{"transitions":[
			{"id":"11","name":"Start","to":{"name":"In Progress"}}
		]}`), nil
	}
	c := New(testBaseURL, testEmail, testToken, WithHTTPClient(stub))
	err := c.Transition(context.Background(), "ENG-7", "Done")
	if !errors.Is(err, ErrTransitionNotFound) {
		t.Fatalf("error = %v, want ErrTransitionNotFound", err)
	}
}

func TestTransition_ListAPIError(t *testing.T) {
	stub := &stubDoer{t: t}
	stub.handler = func(*recordedRequest) (*http.Response, error) {
		return jsonResponse(http.StatusNotFound, `{"errorMessages":["issue does not exist"]}`), nil
	}
	c := New(testBaseURL, testEmail, testToken, WithHTTPClient(stub))
	err := c.Transition(context.Background(), "ENG-404", "Done")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", apiErr.StatusCode)
	}
}

func TestTransition_ValidatesArgs(t *testing.T) {
	c := New(testBaseURL, testEmail, testToken, WithHTTPClient(&stubDoer{
		t: t,
		handler: func(*recordedRequest) (*http.Response, error) {
			t.Fatal("transport called despite invalid args")
			return nil, nil
		},
	}))
	if err := c.Transition(context.Background(), "", "Done"); err == nil {
		t.Error("expected error for empty key")
	}
	if err := c.Transition(context.Background(), "ENG-7", ""); err == nil {
		t.Error("expected error for empty target status")
	}
}

func TestNew_TrimsTrailingSlash(t *testing.T) {
	stub := &stubDoer{t: t}
	stub.handler = func(rec *recordedRequest) (*http.Response, error) {
		// Path must not contain a doubled slash from the trailing-slash baseURL.
		if strings.Contains(rec.path, "//") {
			t.Errorf("path has doubled slash: %s", rec.path)
		}
		return jsonResponse(http.StatusCreated, `{"id":"1","key":"ENG-1"}`), nil
	}
	c := New(testBaseURL+"/", testEmail, testToken, WithHTTPClient(stub))
	out, err := c.CreateIssue(context.Background(), CreateIssueParams{
		ProjectKey: "ENG", IssueType: "Task", Summary: "s",
	})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if out.URL != testBaseURL+"/browse/ENG-1" {
		t.Errorf("URL = %q, want no doubled slash", out.URL)
	}
}
