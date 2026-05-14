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
	"strconv"
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

	createIssueCommentStatus int
	createIssueCommentBody   string

	updateIssueCommentStatus int
	updateIssueCommentBody   string

	getWorkflowRunStatus int
	getWorkflowRunBody   string

	getBranchProtectionStatus int
	getBranchProtectionBody   string

	listRulesetsStatus int
	listRulesetsBody   string

	getRulesetStatus int
	getRulesetBody   map[int64]string

	getPullRequestStatus int
	getPullRequestBody   string

	graphqlStatus int
	graphqlBody   string

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
		getFileStatus:             http.StatusOK,
		dispatchStatus:            http.StatusNoContent,
		getIssueStatus:            http.StatusOK,
		createCheckRunStatus:      http.StatusCreated,
		createCheckRunBody:        `{"id":987654,"html_url":"https://github.com/x/y/runs/987654"}`,
		createIssueCommentStatus:  http.StatusCreated,
		createIssueCommentBody:    `{"id":11111}`,
		updateIssueCommentStatus:  http.StatusOK,
		updateIssueCommentBody:    `{"id":11111,"body":"edited body","html_url":"https://github.com/x/y/issues/17#issuecomment-11111"}`,
		getWorkflowRunStatus:      http.StatusOK,
		getWorkflowRunBody:        `{"id":987654321,"html_url":"https://github.com/x/y/actions/runs/987654321","conclusion":"failure","status":"completed","event":"workflow_dispatch","head_branch":"main","head_sha":"abc","inputs":{"stage_id":"22222222-2222-2222-2222-222222222222","run_id":"11111111-1111-1111-1111-111111111111"}}`,
		getBranchProtectionStatus: http.StatusOK,
		getBranchProtectionBody:   `{"required_status_checks":{"contexts":["ci/build","lint"]}}`,
		listRulesetsStatus:        http.StatusOK,
		listRulesetsBody:          `[]`,
		getRulesetStatus:          http.StatusOK,
		getRulesetBody:            map[int64]string{},
		getPullRequestStatus:      http.StatusOK,
		getPullRequestBody:        `{"number":42,"node_id":"PR_kwDOABcDEf","state":"open","merged":false,"head":{"sha":"abc123"}}`,
		graphqlStatus:             http.StatusOK,
		graphqlBody:               `{"data":{"enablePullRequestAutoMerge":{"pullRequest":{"number":42,"url":"https://github.com/x/y/pull/42","state":"OPEN"}}}}`,
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

	mux.HandleFunc("POST /repos/{owner}/{repo}/issues/{number}/comments",
		func(w http.ResponseWriter, r *http.Request) {
			capture(r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fg.createIssueCommentStatus)
			if fg.createIssueCommentBody != "" {
				_, _ = io.WriteString(w, fg.createIssueCommentBody)
			}
		})

	mux.HandleFunc("PATCH /repos/{owner}/{repo}/issues/comments/{comment_id}",
		func(w http.ResponseWriter, r *http.Request) {
			capture(r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fg.updateIssueCommentStatus)
			if fg.updateIssueCommentBody != "" {
				_, _ = io.WriteString(w, fg.updateIssueCommentBody)
			}
		})

	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/runs/{run_id}",
		func(w http.ResponseWriter, r *http.Request) {
			capture(r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fg.getWorkflowRunStatus)
			if fg.getWorkflowRunBody != "" {
				_, _ = io.WriteString(w, fg.getWorkflowRunBody)
			}
		})

	mux.HandleFunc("GET /repos/{owner}/{repo}/branches/{branch}/protection",
		func(w http.ResponseWriter, r *http.Request) {
			capture(r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fg.getBranchProtectionStatus)
			if fg.getBranchProtectionBody != "" {
				_, _ = io.WriteString(w, fg.getBranchProtectionBody)
			}
		})

	mux.HandleFunc("GET /repos/{owner}/{repo}/rulesets",
		func(w http.ResponseWriter, r *http.Request) {
			capture(r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fg.listRulesetsStatus)
			if fg.listRulesetsBody != "" {
				_, _ = io.WriteString(w, fg.listRulesetsBody)
			}
		})

	mux.HandleFunc("GET /repos/{owner}/{repo}/rulesets/{id}",
		func(w http.ResponseWriter, r *http.Request) {
			capture(r)
			w.Header().Set("Content-Type", "application/json")
			id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
			body, ok := fg.getRulesetBody[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(fg.getRulesetStatus)
			_, _ = io.WriteString(w, body)
		})

	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}",
		func(w http.ResponseWriter, r *http.Request) {
			capture(r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fg.getPullRequestStatus)
			if fg.getPullRequestBody != "" {
				_, _ = io.WriteString(w, fg.getPullRequestBody)
			}
		})

	mux.HandleFunc("POST /graphql",
		func(w http.ResponseWriter, r *http.Request) {
			capture(r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fg.graphqlStatus)
			if fg.graphqlBody != "" {
				_, _ = io.WriteString(w, fg.graphqlBody)
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

// --- CreateIssueComment ---

func TestCreateIssueComment_HappyPath(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	c, _ := newTestClient(t, srv, nil)

	if err := c.CreateIssueComment(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, 17, "Fishhawk picked this up."); err != nil {
		t.Fatalf("CreateIssueComment: %v", err)
	}
	if fg.gotMethod != http.MethodPost {
		t.Errorf("method = %q want POST", fg.gotMethod)
	}
	if fg.gotPath != "/repos/x/y/issues/17/comments" {
		t.Errorf("path = %q", fg.gotPath)
	}
	if fg.gotContentType != "application/json" {
		t.Errorf("content-type = %q", fg.gotContentType)
	}
	var body map[string]string
	if err := json.Unmarshal(fg.gotBody, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["body"] != "Fishhawk picked this up." {
		t.Errorf("body = %q", body["body"])
	}
}

func TestCreateIssueComment_ValidationErrors(t *testing.T) {
	c := &Client{Tokens: &stubTokens{}}
	cases := []struct {
		name      string
		repo      RepoRef
		number    int
		body      string
		wantSubst string
	}{
		{"missing owner", RepoRef{Name: "y"}, 1, "x", "owner and name"},
		{"missing name", RepoRef{Owner: "x"}, 1, "x", "owner and name"},
		{"zero number", RepoRef{Owner: "x", Name: "y"}, 0, "x", "issue number must be"},
		{"empty body", RepoRef{Owner: "x", Name: "y"}, 1, "", "body must be non-empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := c.CreateIssueComment(context.Background(), 1, tc.repo, tc.number, tc.body)
			if err == nil || !strings.Contains(err.Error(), tc.wantSubst) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSubst)
			}
		})
	}
}

func TestCreateIssueComment_GitHubError(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.createIssueCommentStatus = http.StatusForbidden
	fg.createIssueCommentBody = `{"message":"Resource not accessible by integration"}`
	c, _ := newTestClient(t, srv, nil)

	err := c.CreateIssueComment(context.Background(), 1,
		RepoRef{Owner: "x", Name: "y"}, 1, "hi")
	if err == nil || !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v want ErrForbidden", err)
	}
}

// --- UpdateIssueComment (E20.1 / #327, ADR-019) ---

func TestUpdateIssueComment_HappyPath(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	c, _ := newTestClient(t, srv, nil)

	got, err := c.UpdateIssueComment(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, 11111, "edited body")
	if err != nil {
		t.Fatalf("UpdateIssueComment: %v", err)
	}
	if fg.gotMethod != http.MethodPatch {
		t.Errorf("method = %q want PATCH", fg.gotMethod)
	}
	if fg.gotPath != "/repos/x/y/issues/comments/11111" {
		t.Errorf("path = %q", fg.gotPath)
	}
	if fg.gotContentType != "application/json" {
		t.Errorf("content-type = %q", fg.gotContentType)
	}
	var body map[string]string
	if err := json.Unmarshal(fg.gotBody, &body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if body["body"] != "edited body" {
		t.Errorf("request body[\"body\"] = %q", body["body"])
	}
	// Returned struct mirrors the PATCH response body so callers can
	// verify the edit landed (e.g., assert html_url matches the
	// original comment).
	if got == nil || got.ID != 11111 || got.Body != "edited body" ||
		got.HTMLURL != "https://github.com/x/y/issues/17#issuecomment-11111" {
		t.Errorf("returned IssueComment = %+v", got)
	}
}

func TestUpdateIssueComment_ValidationErrors(t *testing.T) {
	c := &Client{Tokens: &stubTokens{}}
	cases := []struct {
		name      string
		repo      RepoRef
		commentID int64
		body      string
		wantSubst string
	}{
		{"missing owner", RepoRef{Name: "y"}, 1, "x", "owner and name"},
		{"missing name", RepoRef{Owner: "x"}, 1, "x", "owner and name"},
		{"zero comment id", RepoRef{Owner: "x", Name: "y"}, 0, "x", "comment id must be"},
		{"empty body", RepoRef{Owner: "x", Name: "y"}, 1, "", "body must be non-empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.UpdateIssueComment(context.Background(), 1, tc.repo, tc.commentID, tc.body)
			if err == nil || !strings.Contains(err.Error(), tc.wantSubst) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSubst)
			}
		})
	}
}

func TestUpdateIssueComment_NotFound(t *testing.T) {
	// Operator deleted the comment between Create and Update. The
	// 404 maps to ErrNotFound so the caller (E20.2's NotifyStatusUpdate)
	// can fall back to creating a fresh comment.
	fg, srv := newFakeGitHub(t)
	fg.updateIssueCommentStatus = http.StatusNotFound
	fg.updateIssueCommentBody = `{"message":"Not Found"}`
	c, _ := newTestClient(t, srv, nil)

	_, err := c.UpdateIssueComment(context.Background(), 1,
		RepoRef{Owner: "x", Name: "y"}, 9999, "edited")
	if err == nil || !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v want ErrNotFound", err)
	}
}

func TestUpdateIssueComment_Forbidden(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.updateIssueCommentStatus = http.StatusForbidden
	fg.updateIssueCommentBody = `{"message":"Resource not accessible by integration"}`
	c, _ := newTestClient(t, srv, nil)

	_, err := c.UpdateIssueComment(context.Background(), 1,
		RepoRef{Owner: "x", Name: "y"}, 1, "hi")
	if err == nil || !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v want ErrForbidden", err)
	}
}

// --- GetWorkflowRun ---

func TestGetWorkflowRun_HappyPath(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	c, _ := newTestClient(t, srv, nil)

	got, err := c.GetWorkflowRun(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, 987654321)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if got.ID != 987654321 {
		t.Errorf("ID = %d want 987654321", got.ID)
	}
	if got.Conclusion != "failure" {
		t.Errorf("Conclusion = %q", got.Conclusion)
	}
	if got.Event != "workflow_dispatch" {
		t.Errorf("Event = %q", got.Event)
	}
	if got.Inputs["stage_id"] != "22222222-2222-2222-2222-222222222222" {
		t.Errorf("Inputs.stage_id = %q", got.Inputs["stage_id"])
	}
	if fg.gotMethod != http.MethodGet {
		t.Errorf("method = %q want GET", fg.gotMethod)
	}
	if fg.gotPath != "/repos/x/y/actions/runs/987654321" {
		t.Errorf("path = %q", fg.gotPath)
	}
}

func TestGetWorkflowRun_NotFound(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.getWorkflowRunStatus = http.StatusNotFound
	fg.getWorkflowRunBody = `{"message":"Not Found"}`
	c, _ := newTestClient(t, srv, nil)

	_, err := c.GetWorkflowRun(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, 987654321)
	if err == nil || !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v want ErrNotFound", err)
	}
}

func TestGetWorkflowRun_ValidationErrors(t *testing.T) {
	c := &Client{Tokens: &stubTokens{}}
	cases := []struct {
		name      string
		repo      RepoRef
		runID     int64
		wantSubst string
	}{
		{"missing owner", RepoRef{Name: "y"}, 1, "owner and name"},
		{"missing name", RepoRef{Owner: "x"}, 1, "owner and name"},
		{"zero run id", RepoRef{Owner: "x", Name: "y"}, 0, "workflow run id must be"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.GetWorkflowRun(context.Background(), 1, tc.repo, tc.runID)
			if err == nil || !strings.Contains(err.Error(), tc.wantSubst) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSubst)
			}
		})
	}
}

func TestGetBranchProtection_HappyPath(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.getBranchProtectionBody = `{"required_status_checks":{"contexts":["ci/build","lint"],"checks":[{"context":"ci/build","app_id":1}]}}`
	c, _ := newTestClient(t, srv, nil)

	got, err := c.GetBranchProtection(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "main")
	if err != nil {
		t.Fatalf("GetBranchProtection: %v", err)
	}
	wantContexts := []string{"ci/build", "lint"}
	if !slicesEq(got.RequiredStatusCheckContexts, wantContexts) {
		t.Errorf("contexts = %v, want %v", got.RequiredStatusCheckContexts, wantContexts)
	}

	if !strings.Contains(fg.gotPath, "/branches/main/protection") {
		t.Errorf("path = %q", fg.gotPath)
	}
	if fg.gotMethod != http.MethodGet {
		t.Errorf("method = %q", fg.gotMethod)
	}
	if fg.gotAuth != "Bearer ghs_canned_token" {
		t.Errorf("Authorization = %q", fg.gotAuth)
	}
}

func TestGetBranchProtection_EmptyContexts(t *testing.T) {
	// A branch with protection but no required_status_checks rule
	// returns 200 with the field absent / nil. The dispatcher should
	// fall through to rulesets, so the contract here is "empty
	// slice, no error" — distinct from "ErrNotFound, branch has no
	// protection at all".
	fg, srv := newFakeGitHub(t)
	fg.getBranchProtectionBody = `{}`
	c, _ := newTestClient(t, srv, nil)

	got, err := c.GetBranchProtection(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "main")
	if err != nil {
		t.Fatalf("GetBranchProtection: %v", err)
	}
	if len(got.RequiredStatusCheckContexts) != 0 {
		t.Errorf("contexts = %v, want empty", got.RequiredStatusCheckContexts)
	}
}

func TestGetBranchProtection_NotFound(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.getBranchProtectionStatus = http.StatusNotFound
	fg.getBranchProtectionBody = `{"message":"Branch not protected"}`
	c, _ := newTestClient(t, srv, nil)

	_, err := c.GetBranchProtection(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "main")
	if err == nil || !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestGetBranchProtection_Forbidden(t *testing.T) {
	// Missing administration:read scope (existing install pre-#252)
	// surfaces as 403. Maps to ErrForbidden so the dispatcher can
	// distinguish "no protection" (ErrNotFound, fall through) from
	// "missing scope" (ErrForbidden, refuse with a clear audit).
	fg, srv := newFakeGitHub(t)
	fg.getBranchProtectionStatus = http.StatusForbidden
	c, _ := newTestClient(t, srv, nil)

	_, err := c.GetBranchProtection(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "main")
	if err == nil || !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
}

func TestGetBranchProtection_ValidationErrors(t *testing.T) {
	c := &Client{Tokens: &stubTokens{}}
	cases := []struct {
		name      string
		repo      RepoRef
		branch    string
		wantSubst string
	}{
		{"missing owner", RepoRef{Name: "y"}, "main", "owner and name"},
		{"missing name", RepoRef{Owner: "x"}, "main", "owner and name"},
		{"empty branch", RepoRef{Owner: "x", Name: "y"}, "", "branch is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.GetBranchProtection(context.Background(), 1, tc.repo, tc.branch)
			if err == nil || !strings.Contains(err.Error(), tc.wantSubst) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSubst)
			}
		})
	}
}

func TestListRulesetRequiredChecks_HappyPath(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.listRulesetsBody = `[
		{"id":1,"target":"branch","enforcement":"active"},
		{"id":2,"target":"tag","enforcement":"active"},
		{"id":3,"target":"branch","enforcement":"disabled"},
		{"id":4,"target":"branch","enforcement":"active"}
	]`
	fg.getRulesetBody = map[int64]string{
		1: `{"conditions":{"ref_name":{"include":["~DEFAULT_BRANCH"]}},"rules":[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"audit_complete"}]}}]}`,
		// 2 (tag) is filtered before fetch.
		// 3 (disabled) is filtered before fetch.
		4: `{"conditions":{"ref_name":{"include":["refs/heads/main","refs/heads/release/*"]}},"rules":[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"e2e"}]}},{"type":"deletion","parameters":null}]}`,
	}
	c, _ := newTestClient(t, srv, nil)

	got, err := c.ListRulesetRequiredChecks(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "main")
	if err != nil {
		t.Fatalf("ListRulesetRequiredChecks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d (rulesets included), want 2: %+v", len(got), got)
	}
	if got[0].RulesetID != 1 || !slicesEq(got[0].Contexts, []string{"audit_complete"}) {
		t.Errorf("ruleset[0] = %+v", got[0])
	}
	if got[1].RulesetID != 4 || !slicesEq(got[1].Contexts, []string{"e2e"}) {
		t.Errorf("ruleset[1] = %+v", got[1])
	}
}

func TestListRulesetRequiredChecks_NoneApply(t *testing.T) {
	// Repo has rulesets but none target the branch / require status
	// checks. Expect (nil, nil) — dispatcher then falls through to
	// classic protection alone.
	fg, srv := newFakeGitHub(t)
	fg.listRulesetsBody = `[{"id":7,"target":"branch","enforcement":"active"}]`
	fg.getRulesetBody = map[int64]string{
		7: `{"conditions":{"ref_name":{"include":["refs/heads/release/*"]}},"rules":[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"e2e"}]}}]}`,
	}
	c, _ := newTestClient(t, srv, nil)

	got, err := c.ListRulesetRequiredChecks(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "main")
	if err != nil {
		t.Fatalf("ListRulesetRequiredChecks: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got = %+v, want empty", got)
	}
}

func TestListRulesetRequiredChecks_NoRulesets(t *testing.T) {
	// Repo with no rulesets at all — list returns []. Result is
	// nil, nil; rulesets aren't an error condition.
	_, srv := newFakeGitHub(t)
	c, _ := newTestClient(t, srv, nil)

	got, err := c.ListRulesetRequiredChecks(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "main")
	if err != nil {
		t.Fatalf("ListRulesetRequiredChecks: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got = %+v, want empty", got)
	}
}

func TestListRulesetRequiredChecks_Forbidden(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.listRulesetsStatus = http.StatusForbidden
	c, _ := newTestClient(t, srv, nil)

	_, err := c.ListRulesetRequiredChecks(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "main")
	if err == nil || !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
}

func TestRulesetMatchesBranch(t *testing.T) {
	cases := []struct {
		name    string
		include []string
		exclude []string
		branch  string
		want    bool
	}{
		{"nil conditions matches all", nil, nil, "main", true},
		{"~ALL include", []string{"~ALL"}, nil, "main", true},
		{"~DEFAULT_BRANCH on main", []string{"~DEFAULT_BRANCH"}, nil, "main", true},
		{"~DEFAULT_BRANCH on develop", []string{"~DEFAULT_BRANCH"}, nil, "develop", false},
		{"refs/heads/<branch>", []string{"refs/heads/main"}, nil, "main", true},
		{"plain branch name", []string{"main"}, nil, "main", true},
		{"non-matching include", []string{"refs/heads/release/*"}, nil, "main", false},
		{"empty include treated as all", []string{}, nil, "main", true},
		{"exclude wins over include", []string{"~ALL"}, []string{"refs/heads/main"}, "main", false},
	}
	type cond struct {
		RefName *struct {
			Include []string `json:"include"`
			Exclude []string `json:"exclude"`
		} `json:"ref_name"`
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c *cond
			if tc.include != nil || tc.exclude != nil {
				c = &cond{RefName: &struct {
					Include []string `json:"include"`
					Exclude []string `json:"exclude"`
				}{Include: tc.include, Exclude: tc.exclude}}
			}
			got := rulesetMatchesBranch((*struct {
				RefName *struct {
					Include []string `json:"include"`
					Exclude []string `json:"exclude"`
				} `json:"ref_name"`
			})(c), tc.branch)
			if got != tc.want {
				t.Errorf("rulesetMatchesBranch(%v, %q) = %v, want %v",
					tc, tc.branch, got, tc.want)
			}
		})
	}
}

func slicesEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestEnableAutoMerge_HappyPath(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	c, _ := newTestClient(t, srv, nil)

	err := c.EnableAutoMerge(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, 42, MergeMethodSquash)
	if err != nil {
		t.Fatalf("EnableAutoMerge: %v", err)
	}
	// The capture only retains the LAST request — confirm the
	// graphql mutation hit the /graphql endpoint and carried the
	// node id from the prior REST lookup.
	if fg.gotPath != "/graphql" {
		t.Errorf("final path = %q, want /graphql", fg.gotPath)
	}
	if fg.gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", fg.gotMethod)
	}
	if !strings.Contains(string(fg.gotBody), `"id":"PR_kwDOABcDEf"`) {
		t.Errorf("graphql body missing PR node id:\n%s", fg.gotBody)
	}
	if !strings.Contains(string(fg.gotBody), `"method":"SQUASH"`) {
		t.Errorf("graphql body missing merge method:\n%s", fg.gotBody)
	}
	if !strings.Contains(string(fg.gotBody), "enablePullRequestAutoMerge") {
		t.Errorf("graphql body missing mutation name:\n%s", fg.gotBody)
	}
}

func TestEnableAutoMerge_DefaultsMethodToSquash(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	c, _ := newTestClient(t, srv, nil)

	if err := c.EnableAutoMerge(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, 42, ""); err != nil {
		t.Fatalf("EnableAutoMerge: %v", err)
	}
	if !strings.Contains(string(fg.gotBody), `"method":"SQUASH"`) {
		t.Errorf("empty method should default to SQUASH:\n%s", fg.gotBody)
	}
}

func TestEnableAutoMerge_PRNotFound(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.getPullRequestStatus = http.StatusNotFound
	fg.getPullRequestBody = `{"message":"Not Found"}`
	c, _ := newTestClient(t, srv, nil)

	err := c.EnableAutoMerge(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, 42, MergeMethodSquash)
	if err == nil || !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestEnableAutoMerge_GraphQLError_AsValidation(t *testing.T) {
	// GitHub returns 200 even for application-level errors
	// ("auto-merge is not enabled for this repository", "the
	// pull request is in a clean state already", etc.). Surface
	// those as ErrValidation so the orchestrator can audit + skip
	// retry rather than retry-storm.
	fg, srv := newFakeGitHub(t)
	fg.graphqlBody = `{"errors":[{"message":"Pull request is in clean status","type":"UNPROCESSABLE"}]}`
	c, _ := newTestClient(t, srv, nil)

	err := c.EnableAutoMerge(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, 42, MergeMethodSquash)
	if err == nil || !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
	if !strings.Contains(err.Error(), "Pull request is in clean status") {
		t.Errorf("err should surface graphql message: %v", err)
	}
}

func TestEnableAutoMerge_MissingNodeID(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.getPullRequestBody = `{"number":42}` // node_id absent
	c, _ := newTestClient(t, srv, nil)

	err := c.EnableAutoMerge(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, 42, MergeMethodSquash)
	if err == nil || !strings.Contains(err.Error(), "node_id") {
		t.Errorf("err = %v, want missing-node_id error", err)
	}
}

func TestEnableAutoMerge_ValidationErrors(t *testing.T) {
	c := &Client{Tokens: &stubTokens{}}
	cases := []struct {
		name      string
		repo      RepoRef
		prNumber  int
		wantSubst string
	}{
		{"missing owner", RepoRef{Name: "y"}, 1, "owner and name"},
		{"missing name", RepoRef{Owner: "x"}, 1, "owner and name"},
		{"zero pr", RepoRef{Owner: "x", Name: "y"}, 0, "pr number must be"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := c.EnableAutoMerge(context.Background(), 1, tc.repo, tc.prNumber, MergeMethodSquash)
			if err == nil || !strings.Contains(err.Error(), tc.wantSubst) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSubst)
			}
		})
	}
}

func TestGetPullRequest_HappyPath(t *testing.T) {
	_, srv := newFakeGitHub(t)
	c, _ := newTestClient(t, srv, nil)
	pr, err := c.GetPullRequest(context.Background(), 42, RepoRef{Owner: "x", Name: "y"}, 42)
	if err != nil {
		t.Fatalf("GetPullRequest: %v", err)
	}
	if pr.NodeID != "PR_kwDOABcDEf" {
		t.Errorf("NodeID = %q", pr.NodeID)
	}
	if pr.HeadSHA != "abc123" {
		t.Errorf("HeadSHA = %q", pr.HeadSHA)
	}
	if pr.State != "open" {
		t.Errorf("State = %q", pr.State)
	}
	if pr.Merged {
		t.Errorf("Merged = true, want false")
	}
}

func TestGetPullRequest_NotFound(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.getPullRequestStatus = http.StatusNotFound
	fg.getPullRequestBody = `{"message":"Not Found"}`
	c, _ := newTestClient(t, srv, nil)
	_, err := c.GetPullRequest(context.Background(), 42, RepoRef{Owner: "x", Name: "y"}, 42)
	if err == nil || !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestGetPullRequest_MissingNodeID(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.getPullRequestBody = `{"number":42,"head":{"sha":"abc"}}`
	c, _ := newTestClient(t, srv, nil)
	_, err := c.GetPullRequest(context.Background(), 42, RepoRef{Owner: "x", Name: "y"}, 42)
	if err == nil || !strings.Contains(err.Error(), "node_id") {
		t.Errorf("err = %v, want missing-node_id error", err)
	}
}

func TestGetPullRequest_ValidationErrors(t *testing.T) {
	c := &Client{Tokens: &stubTokens{}}
	cases := []struct {
		name      string
		repo      RepoRef
		prNumber  int
		wantSubst string
	}{
		{"missing owner", RepoRef{Name: "y"}, 1, "owner and name"},
		{"missing name", RepoRef{Owner: "x"}, 1, "owner and name"},
		{"zero pr", RepoRef{Owner: "x", Name: "y"}, 0, "pr number must be"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.GetPullRequest(context.Background(), 1, tc.repo, tc.prNumber)
			if err == nil || !strings.Contains(err.Error(), tc.wantSubst) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSubst)
			}
		})
	}
}
