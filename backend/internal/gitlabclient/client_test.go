package gitlabclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// stubDoer is a programmable Doer. It records every request it sees and
// answers via handler, so each test can assert the exact request shape
// the client emitted.
type stubDoer struct {
	t        *testing.T
	requests []*recordedRequest
	handler  func(*recordedRequest) (*http.Response, error)
}

// recordedRequest is a snapshot of a request with its body already read.
// escapedPath preserves the on-the-wire percent-encoding (so a %2F in a
// namespaced project path is observable), which req.URL.Path decodes away.
type recordedRequest struct {
	method      string
	path        string
	escapedPath string
	header      http.Header
	body        []byte
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
		method:      req.Method,
		path:        req.URL.Path,
		escapedPath: req.URL.EscapedPath(),
		header:      req.Header.Clone(),
		body:        body,
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
	testBaseURL = "https://gitlab.example.com"
	testToken   = "glpat-s3cr3t"
)

// assertPrivateToken verifies the request carries the PRIVATE-TOKEN header
// with the configured token.
func assertPrivateToken(t *testing.T, h http.Header) {
	t.Helper()
	if got := h.Get("PRIVATE-TOKEN"); got != testToken {
		t.Fatalf("PRIVATE-TOKEN = %q, want %q", got, testToken)
	}
}

func TestGetProject_RequestShapeAndResult(t *testing.T) {
	stub := &stubDoer{t: t}
	stub.handler = func(rec *recordedRequest) (*http.Response, error) {
		if rec.method != http.MethodGet {
			t.Errorf("method = %s, want GET", rec.method)
		}
		assertPrivateToken(t, rec.header)
		// The namespaced path must be percent-encoded into one segment:
		// the group/project slashes become %2F on the wire.
		if want := "/api/v4/projects/group%2Fsub%2Fproj"; rec.escapedPath != want {
			t.Errorf("escaped path = %s, want %s", rec.escapedPath, want)
		}
		// The decoded path round-trips the original slashes.
		if want := "/api/v4/projects/group/sub/proj"; rec.path != want {
			t.Errorf("decoded path = %s, want %s", rec.path, want)
		}
		if len(rec.body) != 0 {
			t.Errorf("GET carried a body: %s", rec.body)
		}
		return jsonResponse(http.StatusOK, `{"id":42,"web_url":"https://gitlab.example.com/group/sub/proj"}`), nil
	}

	c := New(testBaseURL, testToken, WithHTTPClient(stub))
	proj, err := c.GetProject(context.Background(), "group/sub/proj")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if proj.ID != 42 {
		t.Errorf("id = %d, want 42", proj.ID)
	}
	if proj.WebURL != "https://gitlab.example.com/group/sub/proj" {
		t.Errorf("web_url = %q", proj.WebURL)
	}
}

func TestGetProject_APIError(t *testing.T) {
	stub := &stubDoer{t: t}
	stub.handler = func(*recordedRequest) (*http.Response, error) {
		return jsonResponse(http.StatusNotFound, `{"message":"404 Project Not Found"}`), nil
	}
	c := New(testBaseURL, testToken, WithHTTPClient(stub))
	_, err := c.GetProject(context.Background(), "group/missing")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", apiErr.StatusCode)
	}
	if !strings.Contains(apiErr.Body, "Project Not Found") {
		t.Errorf("body excerpt = %q, want it to carry the GitLab error body", apiErr.Body)
	}
}

func TestGetProject_ValidatesArgs(t *testing.T) {
	c := New(testBaseURL, testToken, WithHTTPClient(&stubDoer{
		t: t,
		handler: func(*recordedRequest) (*http.Response, error) {
			t.Fatal("transport called despite empty path")
			return nil, nil
		},
	}))
	if _, err := c.GetProject(context.Background(), "  "); err == nil {
		t.Error("expected error for empty project path")
	}
}

func TestCreateIssue_RequestShapeAndResult(t *testing.T) {
	stub := &stubDoer{t: t}
	stub.handler = func(rec *recordedRequest) (*http.Response, error) {
		if rec.method != http.MethodPost {
			t.Errorf("method = %s, want POST", rec.method)
		}
		if rec.path != "/api/v4/projects/42/issues" {
			t.Errorf("path = %s, want /api/v4/projects/42/issues", rec.path)
		}
		assertPrivateToken(t, rec.header)
		if ct := rec.header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}

		var got struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			Labels      string `json:"labels"`
		}
		if err := json.Unmarshal(rec.body, &got); err != nil {
			t.Fatalf("unmarshal request body: %v\nbody=%s", err, rec.body)
		}
		if got.Title != "Fix the thing" {
			t.Errorf("title = %q", got.Title)
		}
		if got.Description != "some detail" {
			t.Errorf("description = %q", got.Description)
		}
		// Labels are a single comma-joined string, the v4 issues API shape.
		if got.Labels != "area:backend,type:bug" {
			t.Errorf("labels = %q, want comma-joined", got.Labels)
		}
		return jsonResponse(http.StatusCreated,
			`{"iid":7,"web_url":"https://gitlab.example.com/group/proj/-/issues/7"}`), nil
	}

	c := New(testBaseURL, testToken, WithHTTPClient(stub))
	out, err := c.CreateIssue(context.Background(), 42, CreateIssueParams{
		Title:       "Fix the thing",
		Description: "some detail",
		Labels:      []string{"area:backend", "type:bug"},
	})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if out.IID != 7 {
		t.Errorf("iid = %d, want 7", out.IID)
	}
	if out.WebURL != "https://gitlab.example.com/group/proj/-/issues/7" {
		t.Errorf("web_url = %q", out.WebURL)
	}
}

func TestCreateIssue_OmitsEmptyOptionalFields(t *testing.T) {
	stub := &stubDoer{t: t}
	stub.handler = func(rec *recordedRequest) (*http.Response, error) {
		var got map[string]any
		if err := json.Unmarshal(rec.body, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, ok := got["description"]; ok {
			t.Error("description present though empty")
		}
		if _, ok := got["labels"]; ok {
			t.Error("labels present though empty")
		}
		return jsonResponse(http.StatusCreated, `{"iid":9,"web_url":"u"}`), nil
	}

	c := New(testBaseURL, testToken, WithHTTPClient(stub))
	if _, err := c.CreateIssue(context.Background(), 42, CreateIssueParams{Title: "minimal"}); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
}

func TestCreateIssue_APIError(t *testing.T) {
	stub := &stubDoer{t: t}
	stub.handler = func(*recordedRequest) (*http.Response, error) {
		return jsonResponse(http.StatusBadRequest, `{"message":"title is missing"}`), nil
	}
	c := New(testBaseURL, testToken, WithHTTPClient(stub))
	_, err := c.CreateIssue(context.Background(), 42, CreateIssueParams{Title: "s"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", apiErr.StatusCode)
	}
	if !strings.Contains(apiErr.Body, "title is missing") {
		t.Errorf("body excerpt = %q", apiErr.Body)
	}
}

func TestCreateIssue_ValidatesRequiredFields(t *testing.T) {
	c := New(testBaseURL, testToken, WithHTTPClient(&stubDoer{
		t: t,
		handler: func(*recordedRequest) (*http.Response, error) {
			t.Fatal("transport called despite invalid params")
			return nil, nil
		},
	}))
	if _, err := c.CreateIssue(context.Background(), 0, CreateIssueParams{Title: "s"}); err == nil {
		t.Error("expected error for missing project id")
	}
	if _, err := c.CreateIssue(context.Background(), 42, CreateIssueParams{Title: "  "}); err == nil {
		t.Error("expected error for empty title")
	}
}

func TestLinkIssues_RequestShapeAndResult(t *testing.T) {
	stub := &stubDoer{t: t}
	stub.handler = func(rec *recordedRequest) (*http.Response, error) {
		if rec.method != http.MethodPost {
			t.Errorf("method = %s, want POST", rec.method)
		}
		if rec.path != "/api/v4/projects/42/issues/7/links" {
			t.Errorf("path = %s, want /api/v4/projects/42/issues/7/links", rec.path)
		}
		assertPrivateToken(t, rec.header)
		var got struct {
			TargetProjectID int `json:"target_project_id"`
			TargetIssueIID  int `json:"target_issue_iid"`
		}
		if err := json.Unmarshal(rec.body, &got); err != nil {
			t.Fatalf("unmarshal: %v\nbody=%s", err, rec.body)
		}
		if got.TargetProjectID != 42 {
			t.Errorf("target_project_id = %d, want 42 (same project)", got.TargetProjectID)
		}
		if got.TargetIssueIID != 3 {
			t.Errorf("target_issue_iid = %d, want 3", got.TargetIssueIID)
		}
		return jsonResponse(http.StatusCreated, `{}`), nil
	}

	c := New(testBaseURL, testToken, WithHTTPClient(stub))
	if err := c.LinkIssues(context.Background(), 42, 7, 3); err != nil {
		t.Fatalf("LinkIssues: %v", err)
	}
}

func TestLinkIssues_APIError(t *testing.T) {
	stub := &stubDoer{t: t}
	stub.handler = func(*recordedRequest) (*http.Response, error) {
		return jsonResponse(http.StatusForbidden, `{"message":"403 Forbidden"}`), nil
	}
	c := New(testBaseURL, testToken, WithHTTPClient(stub))
	err := c.LinkIssues(context.Background(), 42, 7, 3)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", apiErr.StatusCode)
	}
}

func TestLinkIssues_ValidatesArgs(t *testing.T) {
	c := New(testBaseURL, testToken, WithHTTPClient(&stubDoer{
		t: t,
		handler: func(*recordedRequest) (*http.Response, error) {
			t.Fatal("transport called despite invalid args")
			return nil, nil
		},
	}))
	if err := c.LinkIssues(context.Background(), 0, 7, 3); err == nil {
		t.Error("expected error for missing project id")
	}
	if err := c.LinkIssues(context.Background(), 42, 0, 3); err == nil {
		t.Error("expected error for missing source iid")
	}
	if err := c.LinkIssues(context.Background(), 42, 7, 0); err == nil {
		t.Error("expected error for missing target iid")
	}
}

func TestNew_TrimsTrailingSlash(t *testing.T) {
	stub := &stubDoer{t: t}
	stub.handler = func(rec *recordedRequest) (*http.Response, error) {
		// A trailing-slash baseURL must not produce a doubled slash.
		if strings.Contains(rec.escapedPath, "//") {
			t.Errorf("path has doubled slash: %s", rec.escapedPath)
		}
		return jsonResponse(http.StatusOK, `{"id":1,"web_url":"u"}`), nil
	}
	c := New(testBaseURL+"/", testToken, WithHTTPClient(stub))
	if _, err := c.GetProject(context.Background(), "g/p"); err != nil {
		t.Fatalf("GetProject: %v", err)
	}
}
