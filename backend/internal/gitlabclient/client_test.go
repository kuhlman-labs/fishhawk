package gitlabclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
// rawQuery preserves the query string so a required parameter (GetFile's
// ref) is assertable on the wire.
type recordedRequest struct {
	method      string
	path        string
	escapedPath string
	rawQuery    string
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
		rawQuery:    req.URL.RawQuery,
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

func TestGetFile_RequestShapeAndResult(t *testing.T) {
	content := base64.StdEncoding.EncodeToString([]byte("version: 1\nprovider: gitlab\n"))
	stub := &stubDoer{t: t}
	stub.handler = func(rec *recordedRequest) (*http.Response, error) {
		if rec.method != http.MethodGet {
			t.Errorf("method = %s, want GET", rec.method)
		}
		assertPrivateToken(t, rec.header)
		// Both the namespaced project path and the file path are
		// percent-encoded into single segments: every slash becomes %2F
		// on the wire.
		if want := "/api/v4/projects/group%2Fsub%2Fproj/repository/files/.fishhawk%2Fwork-management.yaml"; rec.escapedPath != want {
			t.Errorf("escaped path = %s, want %s", rec.escapedPath, want)
		}
		// The Repository Files API requires an explicit ref; pin it on
		// the wire.
		if rec.rawQuery != "ref=HEAD" {
			t.Errorf("query = %q, want ref=HEAD", rec.rawQuery)
		}
		if len(rec.body) != 0 {
			t.Errorf("GET carried a body: %s", rec.body)
		}
		return jsonResponse(http.StatusOK,
			`{"file_path":".fishhawk/work-management.yaml","blob_id":"abc123","encoding":"base64","content":"`+content+`"}`), nil
	}

	c := New(testBaseURL, testToken, WithHTTPClient(stub))
	file, err := c.GetFile(context.Background(), "group/sub/proj", ".fishhawk/work-management.yaml", "HEAD")
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if file.FilePath != ".fishhawk/work-management.yaml" {
		t.Errorf("file_path = %q", file.FilePath)
	}
	if got := string(file.Content); got != "version: 1\nprovider: gitlab\n" {
		t.Errorf("content = %q, want the decoded file body", got)
	}
	if file.BlobID != "abc123" {
		t.Errorf("blob_id = %q, want abc123", file.BlobID)
	}
}

func TestGetFile_APIError(t *testing.T) {
	stub := &stubDoer{t: t}
	stub.handler = func(*recordedRequest) (*http.Response, error) {
		return jsonResponse(http.StatusNotFound, `{"message":"404 File Not Found"}`), nil
	}
	c := New(testBaseURL, testToken, WithHTTPClient(stub))
	_, err := c.GetFile(context.Background(), "group/proj", "missing.yaml", "HEAD")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", apiErr.StatusCode)
	}
}

func TestGetFile_ValidatesArgs(t *testing.T) {
	c := New(testBaseURL, testToken, WithHTTPClient(&stubDoer{
		t: t,
		handler: func(*recordedRequest) (*http.Response, error) {
			t.Fatal("transport called despite invalid args")
			return nil, nil
		},
	}))
	for _, tc := range []struct {
		name                       string
		projectPath, filePath, ref string
	}{
		{"empty project path", "  ", "f.yaml", "HEAD"},
		{"empty file path", "group/proj", "  ", "HEAD"},
		// ref is required by the Repository Files API — an empty ref
		// fails closed before the wire.
		{"empty ref", "group/proj", "f.yaml", "  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.GetFile(context.Background(), tc.projectPath, tc.filePath, tc.ref); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

func TestGetFile_BadEncoding(t *testing.T) {
	stub := &stubDoer{t: t}
	stub.handler = func(*recordedRequest) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"file_path":"f.yaml","blob_id":"x","encoding":"text","content":"hi"}`), nil
	}
	c := New(testBaseURL, testToken, WithHTTPClient(stub))
	_, err := c.GetFile(context.Background(), "group/proj", "f.yaml", "HEAD")
	if err == nil || !strings.Contains(err.Error(), "encoding") {
		t.Errorf("err = %v, want an unexpected-encoding error", err)
	}
}

func TestGetFile_CorruptBase64(t *testing.T) {
	stub := &stubDoer{t: t}
	stub.handler = func(*recordedRequest) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"file_path":"f.yaml","blob_id":"x","encoding":"base64","content":"!!!not-base64!!!"}`), nil
	}
	c := New(testBaseURL, testToken, WithHTTPClient(stub))
	_, err := c.GetFile(context.Background(), "group/proj", "f.yaml", "HEAD")
	if err == nil || !strings.Contains(err.Error(), "decode content") {
		t.Errorf("err = %v, want a base64 decode error", err)
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

// --- c.do redirect boundary (E45.39 / #3305) ------------------------------
//
// These cases drive the REAL *http.Client through the same-package
// newIssueServer harness: the stubDoer cases above cannot exercise
// CheckRedirect at all (a hand-rolled stub never follows a redirect), so the
// hoisted guard on c.do needs a live listener to be observable. Every
// redirect target — foreign and same-origin alike — is a reachable in-test
// httptest listener, so deleting the guard fails these tests on the
// zero-hit / empty-token behavioral assertions rather than on a dial error.

// foreignRecorder is a reachable in-test stand-in for an off-instance host.
// It counts requests and records the PRIVATE-TOKEN it was handed, so a
// deleted guard is caught disclosing the credential rather than merely
// failing to connect.
type foreignRecorder struct {
	srv   *httptest.Server
	hits  atomic.Int64
	token atomic.Value // string
}

func newForeignRecorder(t *testing.T) *foreignRecorder {
	t.Helper()
	f := &foreignRecorder{}
	f.token.Store("")
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.token.Store(r.Header.Get("PRIVATE-TOKEN"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// assertNotReached fails unless the foreign host was never dialed and never
// saw the token — the two observables that redden when the guard is deleted.
func (f *foreignRecorder) assertNotReached(t *testing.T) {
	t.Helper()
	if got := f.hits.Load(); got != 0 {
		t.Errorf("foreign host received %d requests, want 0 (token must not follow a redirect off-instance)", got)
	}
	if tok := f.token.Load().(string); tok != "" {
		t.Errorf("foreign host saw PRIVATE-TOKEN %q, want it never disclosed", tok)
	}
}

// TestGitLabClient_Do_RefusesOffOriginRedirect_GetIssue pins the hoisted guard
// on a BODILESS GET dispatched through c.do: a same-origin endpoint answering
// 302 to a foreign host is refused before the redirected request is sent.
// Before the hoist this path called c.http.Do directly, so the default client
// would have chased the 302 with PRIVATE-TOKEN attached.
func TestGitLabClient_Do_RefusesOffOriginRedirect_GetIssue(t *testing.T) {
	foreign := newForeignRecorder(t)

	s := newIssueServer(t)
	s.mux.HandleFunc("GET /api/v4/projects/42/issues/7", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.srv.URL+"/api/v4/projects/42/issues/7", http.StatusFound)
	})

	issue, err := s.client().GetIssue(context.Background(), 42, 7)
	// Deliberately Errorf, not Fatalf: with the guard deleted the call
	// SUCCEEDS, and the zero-hit / empty-token assertions below are the ones
	// that prove the credential reached the foreign listener.
	if err == nil {
		t.Errorf("GetIssue = %v, nil; want a refusal of the off-origin redirect", issue)
	} else if !strings.Contains(err.Error(), "refusing redirect target") {
		t.Errorf("err = %v, want the same-origin refusal from CheckRedirect", err)
	}
	foreign.assertNotReached(t)
}

// TestGitLabClient_Do_RefusesOffOriginRedirect_CreateIssueNote pins the same
// boundary on the BODY-CARRYING POST path, using a 307 — the body-preserving
// redirect class, which net/http replays with the original body and headers.
func TestGitLabClient_Do_RefusesOffOriginRedirect_CreateIssueNote(t *testing.T) {
	foreign := newForeignRecorder(t)

	s := newIssueServer(t)
	s.mux.HandleFunc("POST /api/v4/projects/42/issues/7/notes", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.srv.URL+"/api/v4/projects/42/issues/7/notes", http.StatusTemporaryRedirect)
	})

	note, err := s.client().CreateIssueNote(context.Background(), 42, 7, "hello")
	// Deliberately Errorf, not Fatalf: with the guard deleted the call
	// SUCCEEDS, and the zero-hit / empty-token assertions below are the ones
	// that prove the credential reached the foreign listener.
	if err == nil {
		t.Errorf("CreateIssueNote = %v, nil; want a refusal of the off-origin redirect", note)
	} else if !strings.Contains(err.Error(), "refusing redirect target") {
		t.Errorf("err = %v, want the same-origin refusal from CheckRedirect", err)
	}
	foreign.assertNotReached(t)
}

// TestGitLabClient_Do_FollowsSameOriginRedirect pins that the guard is a
// BOUNDARY and not a blanket redirect ban: a redirect that stays on the
// configured origin is still followed to completion.
func TestGitLabClient_Do_FollowsSameOriginRedirect(t *testing.T) {
	s := newIssueServer(t)
	s.mux.HandleFunc("GET /api/v4/projects/42/issues/7", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/api/v4/projects/42/issues/7/moved", http.StatusFound)
	})
	s.mux.HandleFunc("GET /api/v4/projects/42/issues/7/moved", func(w http.ResponseWriter, r *http.Request) {
		writeIssueJSON(w, http.StatusOK, `{"iid":7,"title":"Moved","state":"opened"}`)
	})

	issue, err := s.client().GetIssue(context.Background(), 42, 7)
	if err != nil {
		t.Fatalf("GetIssue: %v; want a same-origin redirect to be followed", err)
	}
	if issue.IID != 7 || issue.Title != "Moved" {
		t.Errorf("issue = %+v, want the body served after the redirect", issue)
	}

	var sawRedirecting, sawFinal bool
	for _, r := range s.requests() {
		switch r.Path {
		case "/api/v4/projects/42/issues/7":
			sawRedirecting = true
		case "/api/v4/projects/42/issues/7/moved":
			sawFinal = true
		}
	}
	if !sawRedirecting || !sawFinal {
		t.Errorf("request log = %+v, want BOTH the redirecting and the final path (redirect genuinely followed)", s.requests())
	}
}

// TestGitLabClient_Do_StopsAfterRedirectCap pins that overriding CheckRedirect
// did not disable net/http's default chain bound: a same-origin handler that
// redirects to itself terminates rather than looping forever.
func TestGitLabClient_Do_StopsAfterRedirectCap(t *testing.T) {
	s := newIssueServer(t)
	s.mux.HandleFunc("GET /api/v4/projects/42/issues/7", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/api/v4/projects/42/issues/7", http.StatusFound)
	})

	issue, err := s.client().GetIssue(context.Background(), 42, 7)
	if err == nil {
		t.Fatalf("GetIssue = %v, nil; want the redirect chain to be capped", issue)
	}
	if !strings.Contains(err.Error(), "stopped after") {
		t.Errorf("err = %v, want the redirect-cap refusal", err)
	}
	if got := len(s.requests()); got > maxRedirects+1 {
		t.Errorf("server recorded %d requests, want at most %d (the chain cap must still bound the walk)", got, maxRedirects+1)
	}
}
