package githubclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubTokens is a minimal TokenProvider for tests that doesn't
// touch GitHub for token issuance. Returns a canned token, or
// errs deterministically.
type stubTokens struct {
	token string
	err   error

	// installationCalled records the most recent installationID
	// passed to Token(). Tests assert it matches expectations.
	installationCalled int64
}

func (s *stubTokens) Token(_ context.Context, installationID int64) (string, error) {
	s.installationCalled = installationID
	if s.err != nil {
		return "", s.err
	}
	return s.token, nil
}

// fakeGitHub stands in for api.github.com. Tests configure paths
// + responses via fields; the server captures the last request so
// assertions can verify wiring.
type fakeGitHub struct {
	getFileStatus int
	getFileBody   string

	dispatchStatus int
	dispatchBody   string

	getIssueStatus int
	getIssueBody   string

	createCheckRunStatus int
	createCheckRunBody   string

	gotAuth        string
	gotPath        string
	gotQuery       string
	gotMethod      string
	gotAcceptHdr   string
	gotAPIVersion  string
	gotContentType string
	gotBody        []byte
}

func newFakeGitHub(t *testing.T) (*fakeGitHub, *httptest.Server) {
	t.Helper()
	fg := &fakeGitHub{
		getFileStatus:        http.StatusOK,
		dispatchStatus:       http.StatusNoContent,
		getIssueStatus:       http.StatusOK,
		createCheckRunStatus: http.StatusCreated,
		createCheckRunBody:   `{"id":987654,"html_url":"https://github.com/x/y/runs/987654"}`,
	}
	mux := http.NewServeMux()

	// Capture every request that lands.
	capture := func(r *http.Request) {
		fg.gotAuth = r.Header.Get("Authorization")
		fg.gotPath = r.URL.Path
		fg.gotQuery = r.URL.RawQuery
		fg.gotMethod = r.Method
		fg.gotAcceptHdr = r.Header.Get("Accept")
		fg.gotAPIVersion = r.Header.Get("X-GitHub-Api-Version")
		fg.gotContentType = r.Header.Get("Content-Type")
		fg.gotBody, _ = io.ReadAll(r.Body)
	}

	mux.HandleFunc("GET /repos/{owner}/{repo}/contents/", func(w http.ResponseWriter, r *http.Request) {
		capture(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fg.getFileStatus)
		if fg.getFileBody != "" {
			_, _ = io.WriteString(w, fg.getFileBody)
		}
	})

	mux.HandleFunc("POST /repos/{owner}/{repo}/actions/workflows/{file}/dispatches",
		func(w http.ResponseWriter, r *http.Request) {
			capture(r)
			w.WriteHeader(fg.dispatchStatus)
			if fg.dispatchBody != "" {
				_, _ = io.WriteString(w, fg.dispatchBody)
			}
		})

	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/{number}",
		func(w http.ResponseWriter, r *http.Request) {
			capture(r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fg.getIssueStatus)
			if fg.getIssueBody != "" {
				_, _ = io.WriteString(w, fg.getIssueBody)
			}
		})

	mux.HandleFunc("POST /repos/{owner}/{repo}/check-runs",
		func(w http.ResponseWriter, r *http.Request) {
			capture(r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fg.createCheckRunStatus)
			if fg.createCheckRunBody != "" {
				_, _ = io.WriteString(w, fg.createCheckRunBody)
			}
		})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fg, srv
}

// Build a Client wired to fg/srv with stub tokens.
func newTestClient(t *testing.T, srv *httptest.Server, tokenErr error) (*Client, *stubTokens) {
	t.Helper()
	stub := &stubTokens{token: "ghs_canned_token", err: tokenErr}
	return &Client{
		BaseURL: srv.URL,
		Tokens:  stub,
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}, stub
}

func TestGetFile_HappyPath(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	content := []byte("workflows:\n  feature_change:\n    description: test\n")
	encoded := base64.StdEncoding.EncodeToString(content)
	// GitHub wraps base64 at 60 chars; insert an escaped newline
	// (JSON's \n) so the parsed string carries a literal newline
	// the unwrap path then strips.
	wrapped := encoded[:30] + `\n` + encoded[30:]
	fg.getFileBody = `{"path":".fishhawk/workflows.yaml","sha":"abc123","content":"` +
		wrapped + `","encoding":"base64","type":"file"}`

	c, stub := newTestClient(t, srv, nil)
	got, err := c.GetFile(context.Background(), 42, RepoRef{Owner: "x", Name: "y"},
		".fishhawk/workflows.yaml", "main")
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if !bytes.Equal(got.Content, content) {
		t.Errorf("Content = %q, want %q", got.Content, content)
	}
	if got.SHA != "abc123" {
		t.Errorf("SHA = %q", got.SHA)
	}
	if got.Path != ".fishhawk/workflows.yaml" {
		t.Errorf("Path = %q", got.Path)
	}

	// Wiring assertions.
	if stub.installationCalled != 42 {
		t.Errorf("installation = %d, want 42", stub.installationCalled)
	}
	if fg.gotAuth != "Bearer ghs_canned_token" {
		t.Errorf("Authorization = %q", fg.gotAuth)
	}
	if fg.gotAcceptHdr != "application/vnd.github+json" {
		t.Errorf("Accept = %q", fg.gotAcceptHdr)
	}
	if fg.gotAPIVersion != "2022-11-28" {
		t.Errorf("X-GitHub-Api-Version = %q", fg.gotAPIVersion)
	}
	if !strings.Contains(fg.gotPath, "/contents/.fishhawk/workflows.yaml") {
		t.Errorf("path = %q", fg.gotPath)
	}
	if fg.gotQuery != "ref=main" {
		t.Errorf("query = %q, want ref=main", fg.gotQuery)
	}
}

func TestGetFile_NoRefOmitsQuery(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.getFileBody = `{"path":"x","sha":"a","content":"YQ==","encoding":"base64","type":"file"}`
	c, _ := newTestClient(t, srv, nil)
	if _, err := c.GetFile(context.Background(), 1, RepoRef{Owner: "x", Name: "y"}, "x", ""); err != nil {
		t.Fatal(err)
	}
	if fg.gotQuery != "" {
		t.Errorf("query = %q, want empty when ref is empty", fg.gotQuery)
	}
}

func TestGetFile_NotFound(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.getFileStatus = http.StatusNotFound
	fg.getFileBody = `{"message":"Not Found"}`
	c, _ := newTestClient(t, srv, nil)
	_, err := c.GetFile(context.Background(), 1, RepoRef{Owner: "x", Name: "y"}, "x", "main")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestGetFile_Forbidden(t *testing.T) {
	cases := []int{http.StatusUnauthorized, http.StatusForbidden}
	for _, status := range cases {
		t.Run(http.StatusText(status), func(t *testing.T) {
			fg, srv := newFakeGitHub(t)
			fg.getFileStatus = status
			c, _ := newTestClient(t, srv, nil)
			_, err := c.GetFile(context.Background(), 1, RepoRef{Owner: "x", Name: "y"}, "x", "main")
			if !errors.Is(err, ErrForbidden) {
				t.Errorf("err = %v, want ErrForbidden", err)
			}
		})
	}
}

func TestGetFile_OtherStatus(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.getFileStatus = http.StatusInternalServerError
	fg.getFileBody = `{"message":"upstream timeout"}`
	c, _ := newTestClient(t, srv, nil)
	_, err := c.GetFile(context.Background(), 1, RepoRef{Owner: "x", Name: "y"}, "x", "main")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want contains 500", err)
	}
}

func TestGetFile_NotAFile(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.getFileBody = `{"path":"x","sha":"a","content":"","encoding":"none","type":"dir"}`
	c, _ := newTestClient(t, srv, nil)
	_, err := c.GetFile(context.Background(), 1, RepoRef{Owner: "x", Name: "y"}, "x", "main")
	if err == nil || !strings.Contains(err.Error(), `"dir"`) {
		t.Errorf("err = %v, want dir-not-file error", err)
	}
}

func TestGetFile_BadEncoding(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.getFileBody = `{"path":"x","sha":"a","content":"raw","encoding":"utf-8","type":"file"}`
	c, _ := newTestClient(t, srv, nil)
	_, err := c.GetFile(context.Background(), 1, RepoRef{Owner: "x", Name: "y"}, "x", "main")
	if err == nil || !strings.Contains(err.Error(), "encoding") {
		t.Errorf("err = %v, want encoding error", err)
	}
}

func TestGetFile_CorruptBase64(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.getFileBody = `{"path":"x","sha":"a","content":"!!!!","encoding":"base64","type":"file"}`
	c, _ := newTestClient(t, srv, nil)
	_, err := c.GetFile(context.Background(), 1, RepoRef{Owner: "x", Name: "y"}, "x", "main")
	if err == nil || !strings.Contains(err.Error(), "decode content") {
		t.Errorf("err = %v, want decode error", err)
	}
}

func TestGetFile_TokenError(t *testing.T) {
	_, srv := newFakeGitHub(t)
	c, _ := newTestClient(t, srv, errors.New("no installation"))
	_, err := c.GetFile(context.Background(), 1, RepoRef{Owner: "x", Name: "y"}, "x", "main")
	if err == nil || !strings.Contains(err.Error(), "no installation") {
		t.Errorf("err = %v, want token error wrapped", err)
	}
}

func TestGetFile_ValidationErrors(t *testing.T) {
	c := New(&stubTokens{})
	cases := []struct {
		name      string
		repo      RepoRef
		path      string
		wantSubst string
	}{
		{"missing owner", RepoRef{Name: "y"}, "x", "owner and name"},
		{"missing name", RepoRef{Owner: "x"}, "x", "owner and name"},
		{"missing path", RepoRef{Owner: "x", Name: "y"}, "", "path required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.GetFile(context.Background(), 1, tc.repo, tc.path, "main")
			if err == nil || !strings.Contains(err.Error(), tc.wantSubst) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSubst)
			}
		})
	}
}

func TestGetWorkflowSpec_DelegatesToCanonicalPath(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.getFileBody = `{"path":".fishhawk/workflows.yaml","sha":"feedf00d","content":"YQ==","encoding":"base64","type":"file"}`
	c, _ := newTestClient(t, srv, nil)
	got, err := c.GetWorkflowSpec(context.Background(), 42, RepoRef{Owner: "x", Name: "y"}, "main")
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA != "feedf00d" {
		t.Errorf("SHA = %q", got.SHA)
	}
	if !strings.Contains(fg.gotPath, ".fishhawk/workflows.yaml") {
		t.Errorf("path = %q, want canonical workflow path", fg.gotPath)
	}
}

func TestDispatchWorkflow_HappyPath(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	c, _ := newTestClient(t, srv, nil)
	err := c.DispatchWorkflow(context.Background(), 42, RepoRef{Owner: "x", Name: "y"},
		"fishhawk.yml", "main", DispatchInputs{"run_id": "abc-123"})
	if err != nil {
		t.Fatalf("DispatchWorkflow: %v", err)
	}
	if fg.gotMethod != http.MethodPost {
		t.Errorf("method = %q", fg.gotMethod)
	}
	if !strings.Contains(fg.gotPath, "/actions/workflows/fishhawk.yml/dispatches") {
		t.Errorf("path = %q", fg.gotPath)
	}
	if fg.gotContentType != "application/json" {
		t.Errorf("Content-Type = %q", fg.gotContentType)
	}
	// Body shape: {"ref":"main","inputs":{"run_id":"abc-123"}}
	var body struct {
		Ref    string            `json:"ref"`
		Inputs map[string]string `json:"inputs"`
	}
	if err := json.Unmarshal(fg.gotBody, &body); err != nil {
		t.Fatalf("body unmarshal: %v", err)
	}
	if body.Ref != "main" {
		t.Errorf("ref = %q", body.Ref)
	}
	if body.Inputs["run_id"] != "abc-123" {
		t.Errorf("inputs = %v", body.Inputs)
	}
}

func TestDispatchWorkflow_NoInputs(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	c, _ := newTestClient(t, srv, nil)
	if err := c.DispatchWorkflow(context.Background(), 1, RepoRef{Owner: "x", Name: "y"},
		"fishhawk.yml", "main", nil); err != nil {
		t.Fatal(err)
	}
	// inputs should be omitted when nil.
	var body map[string]any
	_ = json.Unmarshal(fg.gotBody, &body)
	if _, present := body["inputs"]; present {
		t.Errorf("inputs key present when nil: %v", body)
	}
}

func TestDispatchWorkflow_Validation(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.dispatchStatus = http.StatusUnprocessableEntity
	fg.dispatchBody = `{"message":"No ref found"}`
	c, _ := newTestClient(t, srv, nil)
	err := c.DispatchWorkflow(context.Background(), 1, RepoRef{Owner: "x", Name: "y"},
		"fishhawk.yml", "no-such-branch", nil)
	if !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}

func TestDispatchWorkflow_NotFound(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.dispatchStatus = http.StatusNotFound
	c, _ := newTestClient(t, srv, nil)
	err := c.DispatchWorkflow(context.Background(), 1, RepoRef{Owner: "x", Name: "y"},
		"missing.yml", "main", nil)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestDispatchWorkflow_ValidationErrors(t *testing.T) {
	c := New(&stubTokens{})
	cases := []struct {
		name      string
		repo      RepoRef
		file      string
		ref       string
		wantSubst string
	}{
		{"missing owner", RepoRef{Name: "y"}, "f.yml", "main", "owner and name"},
		{"missing file", RepoRef{Owner: "x", Name: "y"}, "", "main", "workflowFile"},
		{"missing ref", RepoRef{Owner: "x", Name: "y"}, "f.yml", "", "ref required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := c.DispatchWorkflow(context.Background(), 1, tc.repo, tc.file, tc.ref, nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantSubst) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSubst)
			}
		})
	}
}

func TestDispatchWorkflow_TokenError(t *testing.T) {
	_, srv := newFakeGitHub(t)
	c, _ := newTestClient(t, srv, errors.New("install revoked"))
	err := c.DispatchWorkflow(context.Background(), 1, RepoRef{Owner: "x", Name: "y"},
		"f.yml", "main", nil)
	if err == nil || !strings.Contains(err.Error(), "install revoked") {
		t.Errorf("err = %v", err)
	}
}

func TestDispatchWorkflow_NilTokens(t *testing.T) {
	c := &Client{} // no Tokens
	err := c.DispatchWorkflow(context.Background(), 1, RepoRef{Owner: "x", Name: "y"},
		"f.yml", "main", nil)
	if err == nil || !strings.Contains(err.Error(), "TokenProvider") {
		t.Errorf("err = %v", err)
	}
}

func TestGetFile_NilTokens(t *testing.T) {
	c := &Client{}
	_, err := c.GetFile(context.Background(), 1, RepoRef{Owner: "x", Name: "y"}, "x", "main")
	if err == nil || !strings.Contains(err.Error(), "TokenProvider") {
		t.Errorf("err = %v", err)
	}
}

func TestRepoRef_String(t *testing.T) {
	if got := (RepoRef{Owner: "x", Name: "y"}).String(); got != "x/y" {
		t.Errorf("String() = %q", got)
	}
}

func TestEscapePath_PreservesSlashes(t *testing.T) {
	if got := escapePath(".fishhawk/workflows.yaml"); got != ".fishhawk/workflows.yaml" {
		t.Errorf("escapePath = %q", got)
	}
	if got := escapePath("path with spaces/sub.yaml"); got != "path%20with%20spaces/sub.yaml" {
		t.Errorf("escapePath = %q", got)
	}
}

func TestNew_Defaults(t *testing.T) {
	c := New(&stubTokens{})
	if c.HTTP.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v", c.HTTP.Timeout)
	}
	if c.BaseURL != "" {
		t.Errorf("BaseURL = %q, want empty", c.BaseURL)
	}
}

func TestEndpoint_DefaultsToGitHub(t *testing.T) {
	c := &Client{}
	if got := c.endpoint("/x"); got != DefaultBaseURL+"/x" {
		t.Errorf("endpoint = %q", got)
	}
}

func TestReadBriefBody(t *testing.T) {
	long := strings.Repeat("a", 1000)
	got := readBriefBody(strings.NewReader(long))
	if len(got) != 256 {
		t.Errorf("len = %d, want 256", len(got))
	}
	got2 := readBriefBody(strings.NewReader("  trim  "))
	if got2 != "trim" {
		t.Errorf("got = %q, want trim", got2)
	}
}

func TestGetIssue_HappyPath(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.getIssueBody = `{"number":42,"title":"Add foo","body":"Body text","state":"open"}`
	c, _ := newTestClient(t, srv, nil)

	got, err := c.GetIssue(context.Background(), 99, RepoRef{Owner: "x", Name: "y"}, 42)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if got.Number != 42 || got.Title != "Add foo" || got.Body != "Body text" || got.State != "open" {
		t.Errorf("decoded issue = %+v", got)
	}
	if fg.gotMethod != "GET" {
		t.Errorf("method = %q", fg.gotMethod)
	}
	if fg.gotPath != "/repos/x/y/issues/42" {
		t.Errorf("path = %q", fg.gotPath)
	}
	if fg.gotAuth != "Bearer ghs_canned_token" {
		t.Errorf("auth = %q", fg.gotAuth)
	}
}

func TestGetIssue_NotFound(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.getIssueStatus = http.StatusNotFound
	fg.getIssueBody = `{"message":"Not Found"}`
	c, _ := newTestClient(t, srv, nil)

	_, err := c.GetIssue(context.Background(), 1, RepoRef{Owner: "x", Name: "y"}, 1)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestGetIssue_ValidationErrors(t *testing.T) {
	c := &Client{Tokens: &stubTokens{}}
	if _, err := c.GetIssue(context.Background(), 1, RepoRef{}, 1); err == nil {
		t.Errorf("expected error for empty repo")
	}
	if _, err := c.GetIssue(context.Background(), 1, RepoRef{Owner: "x", Name: "y"}, 0); err == nil {
		t.Errorf("expected error for zero issue number")
	}
	c2 := &Client{}
	if _, err := c2.GetIssue(context.Background(), 1, RepoRef{Owner: "x", Name: "y"}, 1); err == nil {
		t.Errorf("expected error for missing TokenProvider")
	}
}

func TestGetIssue_DecodeError(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.getIssueBody = `not json`
	c, _ := newTestClient(t, srv, nil)

	_, err := c.GetIssue(context.Background(), 1, RepoRef{Owner: "x", Name: "y"}, 1)
	if err == nil {
		t.Fatalf("expected decode error")
	}
}

// --- CreateCheckRun ---

func TestCreateCheckRun_Completed_HappyPath(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	c, _ := newTestClient(t, srv, nil)

	got, err := c.CreateCheckRun(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"},
		CreateCheckRunParams{
			Name:          "fishhawk_audit_complete",
			HeadSHA:       "abc123",
			Status:        CheckRunStatusCompleted,
			Conclusion:    CheckRunConclusionSuccess,
			DetailsURL:    "https://app.fishhawk.example.com/runs/RID",
			OutputSummary: "All rules passed.",
		})
	if err != nil {
		t.Fatalf("CreateCheckRun: %v", err)
	}
	if got.ID != 987654 {
		t.Errorf("ID = %d, want 987654", got.ID)
	}
	if fg.gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", fg.gotMethod)
	}
	if fg.gotPath != "/repos/x/y/check-runs" {
		t.Errorf("path = %q, want /repos/x/y/check-runs", fg.gotPath)
	}
	if fg.gotContentType != "application/json" {
		t.Errorf("Content-Type = %q", fg.gotContentType)
	}

	var body map[string]any
	if err := json.Unmarshal(fg.gotBody, &body); err != nil {
		t.Fatalf("body decode: %v", err)
	}
	if body["name"] != "fishhawk_audit_complete" {
		t.Errorf("name = %v", body["name"])
	}
	if body["head_sha"] != "abc123" {
		t.Errorf("head_sha = %v", body["head_sha"])
	}
	if body["status"] != "completed" {
		t.Errorf("status = %v", body["status"])
	}
	if body["conclusion"] != "success" {
		t.Errorf("conclusion = %v", body["conclusion"])
	}
	if body["details_url"] != "https://app.fishhawk.example.com/runs/RID" {
		t.Errorf("details_url = %v", body["details_url"])
	}
	output, ok := body["output"].(map[string]any)
	if !ok {
		t.Fatalf("output not a map: %v", body["output"])
	}
	if output["title"] != "fishhawk_audit_complete" {
		t.Errorf("output.title = %v (should default to name)", output["title"])
	}
	if output["summary"] != "All rules passed." {
		t.Errorf("output.summary = %v", output["summary"])
	}
}

func TestCreateCheckRun_InProgress_OmitsConclusion(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	c, _ := newTestClient(t, srv, nil)

	_, err := c.CreateCheckRun(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"},
		CreateCheckRunParams{
			Name:    "fishhawk_audit_complete",
			HeadSHA: "abc123",
			Status:  CheckRunStatusInProgress,
		})
	if err != nil {
		t.Fatalf("CreateCheckRun: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(fg.gotBody, &body); err != nil {
		t.Fatalf("body decode: %v", err)
	}
	if _, present := body["conclusion"]; present {
		t.Errorf("conclusion should be absent for in_progress; got %v", body["conclusion"])
	}
}

func TestCreateCheckRun_RejectsCompletedWithoutConclusion(t *testing.T) {
	_, srv := newFakeGitHub(t)
	c, _ := newTestClient(t, srv, nil)

	_, err := c.CreateCheckRun(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"},
		CreateCheckRunParams{
			Name:    "fishhawk_audit_complete",
			HeadSHA: "abc123",
			Status:  CheckRunStatusCompleted,
		})
	if err == nil || !strings.Contains(err.Error(), "conclusion required") {
		t.Errorf("err = %v, want conclusion-required error", err)
	}
}

func TestCreateCheckRun_RejectsConclusionWithoutCompleted(t *testing.T) {
	_, srv := newFakeGitHub(t)
	c, _ := newTestClient(t, srv, nil)

	_, err := c.CreateCheckRun(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"},
		CreateCheckRunParams{
			Name:       "fishhawk_audit_complete",
			HeadSHA:    "abc123",
			Status:     CheckRunStatusInProgress,
			Conclusion: CheckRunConclusionSuccess,
		})
	if err == nil || !strings.Contains(err.Error(), "conclusion only allowed") {
		t.Errorf("err = %v, want conclusion-only-allowed error", err)
	}
}

func TestCreateCheckRun_ValidationErrors(t *testing.T) {
	c := &Client{Tokens: &stubTokens{}}
	cases := []struct {
		name      string
		repo      RepoRef
		params    CreateCheckRunParams
		wantSubst string
	}{
		{"missing owner", RepoRef{Name: "y"}, CreateCheckRunParams{Name: "n", HeadSHA: "s", Status: CheckRunStatusInProgress}, "owner and name"},
		{"missing name", RepoRef{Owner: "x", Name: "y"}, CreateCheckRunParams{HeadSHA: "s", Status: CheckRunStatusInProgress}, "name required"},
		{"missing head_sha", RepoRef{Owner: "x", Name: "y"}, CreateCheckRunParams{Name: "n", Status: CheckRunStatusInProgress}, "head_sha required"},
		{"missing status", RepoRef{Owner: "x", Name: "y"}, CreateCheckRunParams{Name: "n", HeadSHA: "s"}, "status required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.CreateCheckRun(context.Background(), 1, tc.repo, tc.params)
			if err == nil || !strings.Contains(err.Error(), tc.wantSubst) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSubst)
			}
		})
	}
}

func TestCreateCheckRun_GitHubError(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.createCheckRunStatus = http.StatusForbidden
	fg.createCheckRunBody = `{"message":"Resource not accessible by integration"}`
	c, _ := newTestClient(t, srv, nil)

	_, err := c.CreateCheckRun(context.Background(), 1,
		RepoRef{Owner: "x", Name: "y"},
		CreateCheckRunParams{
			Name:    "fishhawk_audit_complete",
			HeadSHA: "abc",
			Status:  CheckRunStatusInProgress,
		})
	if err == nil || !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
}
