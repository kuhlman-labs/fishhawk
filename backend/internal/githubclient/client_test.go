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

	listIssueCommentsStatus int
	// listIssueCommentsPages holds one JSON body per page; the handler
	// serves them in order and advertises a rel="next" Link to itself
	// until the last page, so a multi-page case proves pagination
	// accumulates. listIssueCommentsHits counts requests served.
	listIssueCommentsPages []string
	listIssueCommentsHits  int

	createCheckRunStatus int
	createCheckRunBody   string

	createIssueCommentStatus int
	createIssueCommentBody   string

	updateIssueCommentStatus int
	updateIssueCommentBody   string

	listReactionsStatus int
	listReactionsBody   string

	getWorkflowRunStatus int
	getWorkflowRunBody   string

	listWorkflowRunsStatus int
	listWorkflowRunsBody   string

	getBranchProtectionStatus int
	getBranchProtectionBody   string

	listRulesetsStatus int
	listRulesetsBody   string

	getRulesetStatus int
	getRulesetBody   map[int64]string

	getPullRequestStatus int
	getPullRequestBody   string

	createPullRequestStatus int
	createPullRequestBody   string

	closePullRequestStatus int
	closePullRequestBody   string

	listPullsStatus int
	listPullsBody   string

	graphqlStatus int
	graphqlBody   string

	getInstallationStatus int
	getInstallationBody   string

	getAppStatus int
	getAppBody   string

	getUserStatus int
	getUserBody   string

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
		listIssueCommentsStatus:   http.StatusOK,
		createCheckRunStatus:      http.StatusCreated,
		createCheckRunBody:        `{"id":987654,"html_url":"https://github.com/x/y/runs/987654"}`,
		createIssueCommentStatus:  http.StatusCreated,
		createIssueCommentBody:    `{"id":11111}`,
		updateIssueCommentStatus:  http.StatusOK,
		updateIssueCommentBody:    `{"id":11111,"body":"edited body","html_url":"https://github.com/x/y/issues/17#issuecomment-11111"}`,
		listReactionsStatus:       http.StatusOK,
		listReactionsBody:         `[]`,
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
		createPullRequestStatus:   http.StatusCreated,
		createPullRequestBody:     `{"number":99,"node_id":"PR_kwDOABcZ99","state":"open","html_url":"https://github.com/x/y/pull/99","head":{"sha":"def456"}}`,
		closePullRequestStatus:    http.StatusOK,
		closePullRequestBody:      `{"number":42,"node_id":"PR_kwDOABcDEf","state":"closed","head":{"sha":"abc123"}}`,
		listPullsStatus:           http.StatusOK,
		listPullsBody:             `[]`,
		graphqlStatus:             http.StatusOK,
		graphqlBody:               `{"data":{"enablePullRequestAutoMerge":{"pullRequest":{"number":42,"url":"https://github.com/x/y/pull/42","state":"OPEN"}}}}`,
		getInstallationStatus:     http.StatusOK,
		getInstallationBody:       `{"id":12345,"app_id":1}`,
		getAppStatus:              http.StatusOK,
		getAppBody:                `{"slug":"fishhawk","name":"Fishhawk"}`,
		getUserStatus:             http.StatusOK,
		getUserBody:               `{"id":41898282,"login":"fishhawk[bot]"}`,
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

	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/{number}/comments",
		func(w http.ResponseWriter, r *http.Request) {
			capture(r)
			w.Header().Set("Content-Type", "application/json")
			if fg.listIssueCommentsStatus != http.StatusOK {
				w.WriteHeader(fg.listIssueCommentsStatus)
				if len(fg.listIssueCommentsPages) > 0 {
					_, _ = io.WriteString(w, fg.listIssueCommentsPages[0])
				}
				return
			}
			idx := fg.listIssueCommentsHits
			fg.listIssueCommentsHits++
			if idx >= len(fg.listIssueCommentsPages) {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "[]")
				return
			}
			if idx+1 < len(fg.listIssueCommentsPages) {
				next := "http://" + r.Host + r.URL.Path + "?page=" + strconv.Itoa(idx+2)
				w.Header().Set("Link", "<"+next+`>; rel="next"`)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, fg.listIssueCommentsPages[idx])
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

	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/runs",
		func(w http.ResponseWriter, r *http.Request) {
			capture(r)
			w.Header().Set("Content-Type", "application/json")
			status := fg.listWorkflowRunsStatus
			if status == 0 {
				status = http.StatusOK
			}
			w.WriteHeader(status)
			body := fg.listWorkflowRunsBody
			if body == "" {
				body = `{"workflow_runs":[]}`
			}
			_, _ = io.WriteString(w, body)
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

	mux.HandleFunc("POST /repos/{owner}/{repo}/pulls",
		func(w http.ResponseWriter, r *http.Request) {
			capture(r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fg.createPullRequestStatus)
			if fg.createPullRequestBody != "" {
				_, _ = io.WriteString(w, fg.createPullRequestBody)
			}
		})

	mux.HandleFunc("PATCH /repos/{owner}/{repo}/pulls/{number}",
		func(w http.ResponseWriter, r *http.Request) {
			capture(r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fg.closePullRequestStatus)
			if fg.closePullRequestBody != "" {
				_, _ = io.WriteString(w, fg.closePullRequestBody)
			}
		})

	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls",
		func(w http.ResponseWriter, r *http.Request) {
			capture(r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fg.listPullsStatus)
			if fg.listPullsBody != "" {
				_, _ = io.WriteString(w, fg.listPullsBody)
			}
		})

	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/comments/{id}/reactions",
		func(w http.ResponseWriter, r *http.Request) {
			capture(r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fg.listReactionsStatus)
			if fg.listReactionsBody != "" {
				_, _ = io.WriteString(w, fg.listReactionsBody)
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

	mux.HandleFunc("GET /app",
		func(w http.ResponseWriter, r *http.Request) {
			capture(r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fg.getAppStatus)
			if fg.getAppBody != "" {
				_, _ = io.WriteString(w, fg.getAppBody)
			}
		})

	mux.HandleFunc("GET /users/{login}",
		func(w http.ResponseWriter, r *http.Request) {
			capture(r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fg.getUserStatus)
			if fg.getUserBody != "" {
				_, _ = io.WriteString(w, fg.getUserBody)
			}
		})

	mux.HandleFunc("GET /repos/{owner}/{repo}/installation",
		func(w http.ResponseWriter, r *http.Request) {
			capture(r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fg.getInstallationStatus)
			if fg.getInstallationBody != "" {
				_, _ = io.WriteString(w, fg.getInstallationBody)
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
		AppJWT:  func() (string, error) { return "ghs_app_jwt", nil },
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

// TestGetIssue_DecodesLabels is the #1616 label-decode pin: GitHub's REST
// issue payload carries `labels` as an array whose entries are label objects
// ({name,color,…}) or plain strings; GetIssue must decode BOTH forms into the
// bare names (the area-derivation path reads issue.Labels). The fixture mixes
// two object-form entries with one string-form entry.
func TestGetIssue_DecodesLabels(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.getIssueBody = `{"number":42,"title":"[E22] epic","state":"open",` +
		`"labels":[{"name":"epic","color":"ededed"},{"name":"area:backend"},"autonomy:low"]}`
	c, _ := newTestClient(t, srv, nil)

	got, err := c.GetIssue(context.Background(), 99, RepoRef{Owner: "x", Name: "y"}, 42)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if want := []string{"epic", "area:backend", "autonomy:low"}; strings.Join(got.Labels, ",") != strings.Join(want, ",") {
		t.Errorf("labels = %v, want %v (object + string forms)", got.Labels, want)
	}
}

// TestGetIssue_NoLabels pins the labelless issue to a nil Labels slice — the
// area-derivation path treats it as "no area to derive".
func TestGetIssue_NoLabels(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.getIssueBody = `{"number":42,"title":"Add foo","state":"open"}`
	c, _ := newTestClient(t, srv, nil)

	got, err := c.GetIssue(context.Background(), 99, RepoRef{Owner: "x", Name: "y"}, 42)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if got.Labels != nil {
		t.Errorf("labels = %v, want nil for a labelless issue", got.Labels)
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

// --- ListIssueComments ---

func TestListIssueComments_HappyPath(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.listIssueCommentsPages = []string{
		`[{"user":{"login":"alice"},"body":"First.","created_at":"2026-05-01T10:00:00Z"},` +
			`{"user":{"login":"bob"},"body":"Second.","created_at":"2026-05-01T11:00:00Z"}]`,
	}
	c, _ := newTestClient(t, srv, nil)

	got, err := c.ListIssueComments(context.Background(), 99, RepoRef{Owner: "x", Name: "y"}, 42)
	if err != nil {
		t.Fatalf("ListIssueComments: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d comments, want 2: %+v", len(got), got)
	}
	if got[0] != (FetchedIssueComment{Author: "alice", Body: "First.", CreatedAt: "2026-05-01T10:00:00Z"}) {
		t.Errorf("comment[0] = %+v", got[0])
	}
	if got[1].Author != "bob" || got[1].Body != "Second." {
		t.Errorf("comment[1] = %+v", got[1])
	}
	if fg.gotMethod != "GET" {
		t.Errorf("method = %q", fg.gotMethod)
	}
	if fg.gotPath != "/repos/x/y/issues/42/comments" {
		t.Errorf("path = %q", fg.gotPath)
	}
}

func TestListIssueComments_Paginates(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.listIssueCommentsPages = []string{
		`[{"user":{"login":"alice"},"body":"p1.","created_at":"2026-05-01T10:00:00Z"}]`,
		`[{"user":{"login":"bob"},"body":"p2.","created_at":"2026-05-01T11:00:00Z"}]`,
	}
	c, _ := newTestClient(t, srv, nil)

	got, err := c.ListIssueComments(context.Background(), 99, RepoRef{Owner: "x", Name: "y"}, 42)
	if err != nil {
		t.Fatalf("ListIssueComments: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d comments, want 2 across pages: %+v", len(got), got)
	}
	if got[0].Author != "alice" || got[1].Author != "bob" {
		t.Errorf("pagination did not accumulate in order: %+v", got)
	}
	if fg.listIssueCommentsHits != 2 {
		t.Errorf("server served %d pages, want 2", fg.listIssueCommentsHits)
	}
}

func TestListIssueComments_NotFound(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.listIssueCommentsStatus = http.StatusNotFound
	fg.listIssueCommentsPages = []string{`{"message":"Not Found"}`}
	c, _ := newTestClient(t, srv, nil)

	_, err := c.ListIssueComments(context.Background(), 1, RepoRef{Owner: "x", Name: "y"}, 1)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestListIssueComments_DecodeError(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.listIssueCommentsPages = []string{`not json`}
	c, _ := newTestClient(t, srv, nil)

	_, err := c.ListIssueComments(context.Background(), 1, RepoRef{Owner: "x", Name: "y"}, 1)
	if err == nil {
		t.Fatalf("expected decode error")
	}
}

func TestListIssueComments_ValidationErrors(t *testing.T) {
	c := &Client{Tokens: &stubTokens{}}
	if _, err := c.ListIssueComments(context.Background(), 1, RepoRef{}, 1); err == nil {
		t.Errorf("expected error for empty repo")
	}
	if _, err := c.ListIssueComments(context.Background(), 1, RepoRef{Owner: "x", Name: "y"}, 0); err == nil {
		t.Errorf("expected error for zero issue number")
	}
	c2 := &Client{}
	if _, err := c2.ListIssueComments(context.Background(), 1, RepoRef{Owner: "x", Name: "y"}, 1); err == nil {
		t.Errorf("expected error for missing TokenProvider")
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

	got, err := c.CreateIssueComment(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, 17, "Fishhawk picked this up.")
	if err != nil {
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
	// Caller can now read the created comment id back — used by the
	// sticky-status-comment flow (E20.2 / #328) and the plan-comment
	// update-on-change flow (E17.2 / #337). The fake server's
	// default body is `{"id":11111}`.
	if got == nil || got.ID != 11111 {
		t.Errorf("returned IssueComment = %+v, want id=11111", got)
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
			_, err := c.CreateIssueComment(context.Background(), 1, tc.repo, tc.number, tc.body)
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

	_, err := c.CreateIssueComment(context.Background(), 1,
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

const corrRunID = "11111111-1111-1111-1111-111111111111"
const corrStageID = "22222222-2222-2222-2222-222222222222"

func deployCorrelation() map[string]string {
	return map[string]string{"fishhawk_run_id": corrRunID, "fishhawk_stage_id": corrStageID}
}

// PRIMARY match: a listed run whose echoed inputs carry the exact correlation is
// resolved unambiguously even when other candidates share the branch.
func TestResolveDispatchedRun_CorrelationMatch(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.listWorkflowRunsBody = `{"workflow_runs":[
		{"id":111,"html_url":"https://github.com/x/y/actions/runs/111","status":"queued","event":"workflow_dispatch","head_branch":"main","inputs":{"fishhawk_run_id":"other","fishhawk_stage_id":"other"}},
		{"id":222,"html_url":"https://github.com/x/y/actions/runs/222","status":"in_progress","event":"workflow_dispatch","head_branch":"main","inputs":{"fishhawk_run_id":"` + corrRunID + `","fishhawk_stage_id":"` + corrStageID + `"}}
	]}`
	c, _ := newTestClient(t, srv, nil)

	got, err := c.ResolveDispatchedRun(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "main", deployCorrelation(), time.Time{})
	if err != nil {
		t.Fatalf("ResolveDispatchedRun: %v", err)
	}
	if got == nil || got.ID != 222 {
		t.Fatalf("resolved run = %+v, want id 222 (correlation match)", got)
	}
	// The request must filter by event=workflow_dispatch + branch.
	if !strings.Contains(fg.gotQuery, "event=workflow_dispatch") || !strings.Contains(fg.gotQuery, "branch=main") {
		t.Errorf("query = %q, want event + branch filters", fg.gotQuery)
	}
}

// FALLBACK single-match: no run echoes inputs, but exactly one candidate is on
// the branch+created window → safe to associate.
func TestResolveDispatchedRun_FallbackSingleCandidate(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.listWorkflowRunsBody = `{"workflow_runs":[
		{"id":333,"html_url":"https://github.com/x/y/actions/runs/333","status":"queued","event":"workflow_dispatch","head_branch":"main"}
	]}`
	c, _ := newTestClient(t, srv, nil)

	got, err := c.ResolveDispatchedRun(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "main", deployCorrelation(), time.Time{})
	if err != nil {
		t.Fatalf("ResolveDispatchedRun: %v", err)
	}
	if got == nil || got.ID != 333 {
		t.Fatalf("resolved run = %+v, want id 333 (single-candidate fallback)", got)
	}
}

// AMBIGUOUS (binding condition 1, #1386): multiple concurrent workflow_dispatch
// runs on the same branch with NO correlating inputs must resolve to (nil, nil)
// — never a guessed run. This is the no-mis-association assertion the operator
// condition specifically requires beyond the single-match fallback.
func TestResolveDispatchedRun_AmbiguousNoInputs_Indeterminate(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.listWorkflowRunsBody = `{"workflow_runs":[
		{"id":444,"status":"queued","event":"workflow_dispatch","head_branch":"main"},
		{"id":555,"status":"queued","event":"workflow_dispatch","head_branch":"main"}
	]}`
	c, _ := newTestClient(t, srv, nil)

	got, err := c.ResolveDispatchedRun(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "main", deployCorrelation(), time.Time{})
	if err != nil {
		t.Fatalf("ResolveDispatchedRun: %v", err)
	}
	if got != nil {
		t.Fatalf("resolved run = %+v, want nil (ambiguous, indeterminate — no mis-association)", got)
	}
}

// Some candidates carry inputs but none match the correlation → the dispatched
// run is not among the listed runs yet (eventual consistency); resolve to nil
// rather than fall back into the inputs-bearing crowd.
func TestResolveDispatchedRun_InputsPresentNoMatch_NotFound(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.listWorkflowRunsBody = `{"workflow_runs":[
		{"id":666,"status":"queued","event":"workflow_dispatch","head_branch":"main","inputs":{"fishhawk_run_id":"someone-else","fishhawk_stage_id":"x"}},
		{"id":777,"status":"queued","event":"workflow_dispatch","head_branch":"main"}
	]}`
	c, _ := newTestClient(t, srv, nil)

	got, err := c.ResolveDispatchedRun(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "main", deployCorrelation(), time.Time{})
	if err != nil {
		t.Fatalf("ResolveDispatchedRun: %v", err)
	}
	if got != nil {
		t.Fatalf("resolved run = %+v, want nil (inputs present but none match)", got)
	}
}

// Empty list (no run has appeared yet) → (nil, nil), the retry-later signal.
func TestResolveDispatchedRun_EmptyList_NotFound(t *testing.T) {
	_, srv := newFakeGitHub(t)
	c, _ := newTestClient(t, srv, nil)
	got, err := c.ResolveDispatchedRun(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "main", deployCorrelation(), time.Now())
	if err != nil {
		t.Fatalf("ResolveDispatchedRun: %v", err)
	}
	if got != nil {
		t.Fatalf("resolved run = %+v, want nil (empty list)", got)
	}
}

// A hard API failure surfaces as a typed error, not a silent nil.
func TestResolveDispatchedRun_APIError(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.listWorkflowRunsStatus = http.StatusForbidden
	fg.listWorkflowRunsBody = `{"message":"Forbidden"}`
	c, _ := newTestClient(t, srv, nil)
	_, err := c.ResolveDispatchedRun(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "main", deployCorrelation(), time.Time{})
	if err == nil || !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
}

func TestResolveDispatchedRun_ValidationErrors(t *testing.T) {
	c := &Client{Tokens: &stubTokens{}}
	cases := []struct {
		name        string
		repo        RepoRef
		correlation map[string]string
		wantSubst   string
	}{
		{"missing owner", RepoRef{Name: "y"}, deployCorrelation(), "owner and name"},
		{"empty correlation", RepoRef{Owner: "x", Name: "y"}, nil, "correlation token required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.ResolveDispatchedRun(context.Background(), 1, tc.repo, "main", tc.correlation, time.Time{})
			if err == nil || !strings.Contains(err.Error(), tc.wantSubst) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSubst)
			}
		})
	}
}

func TestResolveDispatchedRun_NilTokens(t *testing.T) {
	c := &Client{}
	_, err := c.ResolveDispatchedRun(context.Background(), 1,
		RepoRef{Owner: "x", Name: "y"}, "main", deployCorrelation(), time.Time{})
	if err == nil || !strings.Contains(err.Error(), "TokenProvider") {
		t.Errorf("err = %v", err)
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

// --- CreatePullRequest (#714 / ADR-032 consolidated decomposition PR) ---

func TestCreatePullRequest_HappyPath(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	c, _ := newTestClient(t, srv, nil)

	pr, err := c.CreatePullRequest(context.Background(), 42, RepoRef{Owner: "x", Name: "y"},
		"fishhawk/run-aaaaaaaa", "main", "Consolidated PR", "body text")
	if err != nil {
		t.Fatalf("CreatePullRequest: %v", err)
	}
	if fg.gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", fg.gotMethod)
	}
	if fg.gotPath != "/repos/x/y/pulls" {
		t.Errorf("path = %q", fg.gotPath)
	}
	if fg.gotContentType != "application/json" {
		t.Errorf("content-type = %q", fg.gotContentType)
	}
	var body map[string]string
	if err := json.Unmarshal(fg.gotBody, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["head"] != "fishhawk/run-aaaaaaaa" || body["base"] != "main" ||
		body["title"] != "Consolidated PR" || body["body"] != "body text" {
		t.Errorf("request body = %+v", body)
	}
	if pr.Number != 99 {
		t.Errorf("Number = %d, want 99", pr.Number)
	}
	if pr.HTMLURL != "https://github.com/x/y/pull/99" {
		t.Errorf("HTMLURL = %q", pr.HTMLURL)
	}
}

func TestCreatePullRequest_AlreadyExists(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.createPullRequestStatus = http.StatusUnprocessableEntity
	fg.createPullRequestBody = `{"message":"Validation Failed","errors":[{"message":"A pull request already exists for x:fishhawk/run-aaaaaaaa."}]}`
	c, _ := newTestClient(t, srv, nil)

	_, err := c.CreatePullRequest(context.Background(), 42, RepoRef{Owner: "x", Name: "y"},
		"fishhawk/run-aaaaaaaa", "main", "Consolidated PR", "body")
	if err == nil || !errors.Is(err, ErrPullRequestExists) {
		t.Errorf("err = %v, want ErrPullRequestExists", err)
	}
	// A duplicate 422 must NOT be mapped to ErrValidation — the caller
	// switches on ErrPullRequestExists to recover the existing URL.
	if errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, should not also be ErrValidation", err)
	}
}

func TestCreatePullRequest_OtherValidation(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.createPullRequestStatus = http.StatusUnprocessableEntity
	fg.createPullRequestBody = `{"message":"Validation Failed","errors":[{"message":"head branch not found"}]}`
	c, _ := newTestClient(t, srv, nil)

	_, err := c.CreatePullRequest(context.Background(), 42, RepoRef{Owner: "x", Name: "y"},
		"fishhawk/run-aaaaaaaa", "main", "Consolidated PR", "body")
	if err == nil || !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
	if errors.Is(err, ErrPullRequestExists) {
		t.Errorf("err = %v, should not be ErrPullRequestExists for a non-duplicate 422", err)
	}
}

func TestCreatePullRequest_ValidationErrors(t *testing.T) {
	c := &Client{Tokens: &stubTokens{}}
	cases := []struct {
		name       string
		repo       RepoRef
		head, base string
		title      string
		wantSubst  string
	}{
		{"missing owner", RepoRef{Name: "y"}, "h", "main", "t", "owner and name"},
		{"missing head", RepoRef{Owner: "x", Name: "y"}, "", "main", "t", "head and base"},
		{"missing base", RepoRef{Owner: "x", Name: "y"}, "h", "", "t", "head and base"},
		{"missing title", RepoRef{Owner: "x", Name: "y"}, "h", "main", "", "title required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.CreatePullRequest(context.Background(), 1, tc.repo, tc.head, tc.base, tc.title, "b")
			if err == nil || !strings.Contains(err.Error(), tc.wantSubst) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSubst)
			}
		})
	}
}

func TestClosePullRequest_HappyPath(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	c, _ := newTestClient(t, srv, nil)

	if err := c.ClosePullRequest(context.Background(), 42,
		RepoRef{Owner: "o", Name: "r"}, 7); err != nil {
		t.Fatalf("ClosePullRequest: %v", err)
	}
	if fg.gotMethod != http.MethodPatch {
		t.Errorf("method = %q, want PATCH", fg.gotMethod)
	}
	if fg.gotPath != "/repos/o/r/pulls/7" {
		t.Errorf("path = %q, want /repos/o/r/pulls/7", fg.gotPath)
	}
	if fg.gotContentType != "application/json" {
		t.Errorf("content-type = %q", fg.gotContentType)
	}
	var body map[string]string
	if err := json.Unmarshal(fg.gotBody, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["state"] != "closed" {
		t.Errorf("state = %q, want closed", body["state"])
	}
}

func TestClosePullRequest_ValidationErrors(t *testing.T) {
	c := &Client{Tokens: &stubTokens{}}
	cases := []struct {
		name      string
		repo      RepoRef
		number    int
		wantSubst string
	}{
		{"missing owner", RepoRef{Name: "r"}, 1, "owner and name"},
		{"missing name", RepoRef{Owner: "o"}, 1, "owner and name"},
		{"zero number", RepoRef{Owner: "o", Name: "r"}, 0, "pr number must be"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := c.ClosePullRequest(context.Background(), 1, tc.repo, tc.number)
			if err == nil || !strings.Contains(err.Error(), tc.wantSubst) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSubst)
			}
		})
	}
}

func TestClosePullRequest_GitHubError(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.closePullRequestStatus = http.StatusForbidden
	fg.closePullRequestBody = `{"message":"Resource not accessible by integration"}`
	c, _ := newTestClient(t, srv, nil)

	err := c.ClosePullRequest(context.Background(), 1,
		RepoRef{Owner: "o", Name: "r"}, 7)
	if err == nil || !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
}

func TestListOpenPullRequestsByHead_HappyPath(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.listPullsBody = `[{"number":99,"node_id":"PR_kw99","state":"open","html_url":"https://github.com/x/y/pull/99","head":{"sha":"def456"}}]`
	c, _ := newTestClient(t, srv, nil)

	got, err := c.ListOpenPullRequestsByHead(context.Background(), 42, RepoRef{Owner: "x", Name: "y"},
		"fishhawk/run-aaaaaaaa", "main")
	if err != nil {
		t.Fatalf("ListOpenPullRequestsByHead: %v", err)
	}
	if fg.gotPath != "/repos/x/y/pulls" {
		t.Errorf("path = %q", fg.gotPath)
	}
	// head filter must carry the "owner:branch" form and base + state.
	if !strings.Contains(fg.gotQuery, "head=x%3Afishhawk%2Frun-aaaaaaaa") {
		t.Errorf("query = %q, want owner:branch head filter", fg.gotQuery)
	}
	if !strings.Contains(fg.gotQuery, "base=main") || !strings.Contains(fg.gotQuery, "state=open") {
		t.Errorf("query = %q, want base=main & state=open", fg.gotQuery)
	}
	if len(got) != 1 || got[0].Number != 99 || got[0].HTMLURL != "https://github.com/x/y/pull/99" {
		t.Errorf("got = %+v", got)
	}
}

func TestListOpenPullRequestsByHead_ValidationErrors(t *testing.T) {
	c := &Client{Tokens: &stubTokens{}}
	cases := []struct {
		name      string
		repo      RepoRef
		head      string
		wantSubst string
	}{
		{"missing owner", RepoRef{Name: "y"}, "h", "owner and name"},
		{"missing head", RepoRef{Owner: "x", Name: "y"}, "", "head branch required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.ListOpenPullRequestsByHead(context.Background(), 1, tc.repo, tc.head, "main")
			if err == nil || !strings.Contains(err.Error(), tc.wantSubst) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSubst)
			}
		})
	}
}

func TestListIssueCommentReactions_HappyPath(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.listReactionsBody = `[
		{"id":1,"content":"+1","user":{"login":"alice"},"created_at":"2026-05-01T10:00:00Z"},
		{"id":2,"content":"rocket","user":{"login":"bob"}}
	]`

	c, _ := newTestClient(t, srv, nil)
	got, err := c.ListIssueCommentReactions(context.Background(), 99, RepoRef{Owner: "x", Name: "y"}, 4242)
	if err != nil {
		t.Fatalf("ListIssueCommentReactions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 reactions; got %d", len(got))
	}
	if got[0].Content != ReactPlusOne || got[0].User.Login != "alice" {
		t.Errorf("got[0] = %+v", got[0])
	}
	// created_at is decoded into CreatedAt (#1054) so the reaction-poller
	// can compare placement time against plan existence.
	wantCreated := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	if !got[0].CreatedAt.Equal(wantCreated) {
		t.Errorf("got[0].CreatedAt = %v, want %v", got[0].CreatedAt, wantCreated)
	}
	if got[1].Content != ReactRocket || got[1].User.Login != "bob" {
		t.Errorf("got[1] = %+v", got[1])
	}
	if !strings.Contains(fg.gotPath, "/issues/comments/4242/reactions") {
		t.Errorf("path = %q, want comment-id 4242", fg.gotPath)
	}
}

func TestListIssueCommentReactions_NotFound(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.listReactionsStatus = http.StatusNotFound
	fg.listReactionsBody = `{"message":"Not Found"}`

	c, _ := newTestClient(t, srv, nil)
	_, err := c.ListIssueCommentReactions(context.Background(), 99, RepoRef{Owner: "x", Name: "y"}, 4242)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestListIssueCommentReactions_ValidationErrors(t *testing.T) {
	c := &Client{Tokens: &stubTokens{}}
	cases := []struct {
		name      string
		repo      RepoRef
		commentID int64
		wantSubst string
	}{
		{"missing owner", RepoRef{Name: "y"}, 1, "owner and name"},
		{"missing name", RepoRef{Owner: "x"}, 1, "owner and name"},
		{"zero comment id", RepoRef{Owner: "x", Name: "y"}, 0, "comment id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.ListIssueCommentReactions(context.Background(), 1, tc.repo, tc.commentID)
			if err == nil || !strings.Contains(err.Error(), tc.wantSubst) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSubst)
			}
		})
	}
}

// --- GetRepoInstallation (#413) ---

func TestGetRepoInstallation_HappyPath(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	c, stub := newTestClient(t, srv, nil)

	id, err := c.GetRepoInstallation(context.Background(), RepoRef{Owner: "x", Name: "y"})
	if err != nil {
		t.Fatalf("GetRepoInstallation: %v", err)
	}
	if id != 12345 {
		t.Errorf("installation id = %d, want 12345", id)
	}
	// Must use App JWT, not an installation token. Token() should NOT
	// be called on the installation-token provider.
	if stub.installationCalled != 0 {
		t.Errorf("Token() called with installationID %d; App-JWT path must not use installation tokens",
			stub.installationCalled)
	}
	if fg.gotAuth != "Bearer ghs_app_jwt" {
		t.Errorf("Authorization = %q, want App JWT", fg.gotAuth)
	}
	if fg.gotPath != "/repos/x/y/installation" {
		t.Errorf("path = %q, want /repos/x/y/installation", fg.gotPath)
	}
	if fg.gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", fg.gotMethod)
	}
	if fg.gotAcceptHdr != "application/vnd.github+json" {
		t.Errorf("Accept = %q", fg.gotAcceptHdr)
	}
	if fg.gotAPIVersion != "2022-11-28" {
		t.Errorf("X-GitHub-Api-Version = %q", fg.gotAPIVersion)
	}
}

func TestGetRepoInstallation_NotInstalled(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.getInstallationStatus = http.StatusNotFound
	fg.getInstallationBody = `{"message":"Not Found"}`
	c, _ := newTestClient(t, srv, nil)

	_, err := c.GetRepoInstallation(context.Background(), RepoRef{Owner: "x", Name: "y"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrNotInstalled) {
		t.Errorf("err = %v, want ErrNotInstalled", err)
	}
	// ErrNotInstalled must NOT be ErrNotFound — callers switch on both.
	if errors.Is(err, ErrNotFound) {
		t.Errorf("ErrNotInstalled must be distinct from ErrNotFound; err = %v", err)
	}
}

func TestGetRepoInstallation_OtherError(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.getInstallationStatus = http.StatusInternalServerError
	fg.getInstallationBody = `{"message":"upstream timeout"}`
	c, _ := newTestClient(t, srv, nil)

	_, err := c.GetRepoInstallation(context.Background(), RepoRef{Owner: "x", Name: "y"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want status code 500 in message", err)
	}
	if errors.Is(err, ErrNotInstalled) {
		t.Errorf("non-404 must not become ErrNotInstalled; err = %v", err)
	}
}

func TestGetRepoInstallation_ValidationErrors(t *testing.T) {
	_, srv := newFakeGitHub(t)
	c, _ := newTestClient(t, srv, nil)
	cases := []struct {
		name      string
		repo      RepoRef
		wantSubst string
	}{
		{"missing owner", RepoRef{Name: "y"}, "owner and name"},
		{"missing name", RepoRef{Owner: "x"}, "owner and name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.GetRepoInstallation(context.Background(), tc.repo)
			if err == nil || !strings.Contains(err.Error(), tc.wantSubst) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSubst)
			}
		})
	}
}

func TestGetRepoInstallation_NoAppJWT(t *testing.T) {
	_, srv := newFakeGitHub(t)
	c, _ := newTestClient(t, srv, nil)
	c.AppJWT = nil // remove the AppJWT configured by newTestClient

	_, err := c.GetRepoInstallation(context.Background(), RepoRef{Owner: "x", Name: "y"})
	if err == nil || !strings.Contains(err.Error(), "AppJWT") {
		t.Errorf("err = %v, want AppJWT-not-configured error", err)
	}
}

func TestGetApp_HappyPath(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	c, stub := newTestClient(t, srv, nil)

	app, err := c.GetApp(context.Background())
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if app.Slug != "fishhawk" {
		t.Errorf("slug = %q, want fishhawk", app.Slug)
	}
	// Must authenticate as the App (JWT), not an installation token.
	if stub.installationCalled != 0 {
		t.Errorf("Token() called with installationID %d; App-JWT path must not use installation tokens",
			stub.installationCalled)
	}
	if fg.gotAuth != "Bearer ghs_app_jwt" {
		t.Errorf("Authorization = %q, want App JWT", fg.gotAuth)
	}
	if fg.gotPath != "/app" {
		t.Errorf("path = %q, want /app", fg.gotPath)
	}
	if fg.gotAPIVersion != "2022-11-28" {
		t.Errorf("X-GitHub-Api-Version = %q", fg.gotAPIVersion)
	}
}

func TestGetApp_NoAppJWT(t *testing.T) {
	_, srv := newFakeGitHub(t)
	c, _ := newTestClient(t, srv, nil)
	c.AppJWT = nil // remove the AppJWT configured by newTestClient

	_, err := c.GetApp(context.Background())
	if err == nil || !strings.Contains(err.Error(), "AppJWT") {
		t.Errorf("err = %v, want AppJWT-not-configured error", err)
	}
}

func TestGetApp_Forbidden(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.getAppStatus = http.StatusForbidden
	fg.getAppBody = `{"message":"Bad credentials"}`
	c, _ := newTestClient(t, srv, nil)

	_, err := c.GetApp(context.Background())
	if !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
}

func TestGetUser_HappyPath(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	c, stub := newTestClient(t, srv, nil)

	user, err := c.GetUser(context.Background(), "fishhawk[bot]")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if user.ID != 41898282 {
		t.Errorf("id = %d, want 41898282", user.ID)
	}
	if user.Login != "fishhawk[bot]" {
		t.Errorf("login = %q", user.Login)
	}
	if stub.installationCalled != 0 {
		t.Errorf("Token() called with installationID %d; public-user lookup must not use installation tokens",
			stub.installationCalled)
	}
	// Pins the auth shape (regression for #750): GET /users/{login} is a
	// public endpoint and MUST carry no Authorization header. The App JWT
	// is only valid for /app* endpoints; routing this call through it 401'd
	// in production while the in-process server stub never exercised the
	// wire, so #722 passed but the real fetch failed.
	if fg.gotAuth != "" {
		t.Errorf("Authorization = %q, want no auth header on public /users lookup", fg.gotAuth)
	}
	if fg.gotPath != "/users/fishhawk[bot]" {
		t.Errorf("path = %q, want /users/fishhawk[bot]", fg.gotPath)
	}
}

func TestGetUser_NoLogin(t *testing.T) {
	_, srv := newFakeGitHub(t)
	c, _ := newTestClient(t, srv, nil)

	_, err := c.GetUser(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "login required") {
		t.Errorf("err = %v, want login-required error", err)
	}
}

// TestGetUser_NoAppJWT_StillResolves proves GetUser is independent of the
// App JWT (#750): with AppJWT nil it must still SUCCEED, because the public
// /users/{login} lookup sends no Authorization header at all.
func TestGetUser_NoAppJWT_StillResolves(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	c, _ := newTestClient(t, srv, nil)
	c.AppJWT = nil

	user, err := c.GetUser(context.Background(), "fishhawk[bot]")
	if err != nil {
		t.Fatalf("GetUser with nil AppJWT: %v", err)
	}
	if user.ID != 41898282 || user.Login != "fishhawk[bot]" {
		t.Errorf("decoded user = %+v, want id=41898282 login=fishhawk[bot]", user)
	}
	if fg.gotAuth != "" {
		t.Errorf("Authorization = %q, want no auth header on public /users lookup", fg.gotAuth)
	}
}

func TestGetUser_NotFound(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	fg.getUserStatus = http.StatusNotFound
	fg.getUserBody = `{"message":"Not Found"}`
	c, _ := newTestClient(t, srv, nil)

	_, err := c.GetUser(context.Background(), "ghost[bot]")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// fakeSigner is a minimal AppJWTSigner for wiring tests: returns a
// canned JWT (or an error) and records the TTL it was called with.
type fakeSigner struct {
	jwt    string
	err    error
	gotTTL time.Duration
	calls  int
}

func (s *fakeSigner) Sign(ttl time.Duration) (string, error) {
	s.calls++
	s.gotTTL = ttl
	if s.err != nil {
		return "", s.err
	}
	return s.jwt, nil
}

// TestNewWithSigner_WiresAppJWT is the regression test for #721: it
// drives GetRepoInstallation through the PRODUCTION constructor
// (NewWithSigner) rather than a hand-wired Client, so it would have
// caught the missing AppJWT wiring that left serve.go's GetRepoInstallation
// hitting the nil guard and stamping installation_id=NULL. The earlier
// hand-built test set AppJWT directly and so masked the omission.
func TestNewWithSigner_WiresAppJWT(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	signer := &fakeSigner{jwt: "fake.app.jwt"}

	// Construct exactly as production does, then point at the fake.
	c := NewWithSigner(&stubTokens{token: "ghs_canned_token"}, signer)
	c.BaseURL = srv.URL
	if c.AppJWT == nil {
		t.Fatal("NewWithSigner left AppJWT nil; App-level endpoints would hit the nil guard")
	}

	// 200 → installation id, via the App-JWT path (not the nil guard).
	id, err := c.GetRepoInstallation(context.Background(), RepoRef{Owner: "x", Name: "y"})
	if err != nil {
		t.Fatalf("GetRepoInstallation through NewWithSigner: %v", err)
	}
	if id != 12345 {
		t.Errorf("installation id = %d, want 12345", id)
	}
	// The outbound request must carry the App JWT minted by the signer.
	if fg.gotAuth != "Bearer fake.app.jwt" {
		t.Errorf("Authorization = %q, want %q (App JWT from signer)", fg.gotAuth, "Bearer fake.app.jwt")
	}
	if signer.calls == 0 {
		t.Error("signer.Sign was never called; AppJWT did not reach the wire")
	}
	// signer.Sign(0) → DefaultJWTTTL (9m), under GitHub's 10m cap.
	if signer.gotTTL != 0 {
		t.Errorf("signer called with ttl = %v, want 0 (clamps to DefaultJWTTTL)", signer.gotTTL)
	}

	// 404 → ErrNotInstalled, still through the production-wired client.
	fg.getInstallationStatus = http.StatusNotFound
	fg.getInstallationBody = `{"message":"Not Found"}`
	if _, err := c.GetRepoInstallation(context.Background(), RepoRef{Owner: "x", Name: "y"}); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("err = %v, want ErrNotInstalled", err)
	}
}

// TestNewWithSigner_SignError_Propagates covers the path where the App
// signer fails to mint a JWT: GetRepoInstallation must surface that error
// (wrapped) instead of reaching the wire. Exercises the fakeSigner.err
// injection the wiring test leaves unused.
func TestNewWithSigner_SignError_Propagates(t *testing.T) {
	fg, srv := newFakeGitHub(t)
	signErr := errors.New("sign boom")
	signer := &fakeSigner{err: signErr}

	c := NewWithSigner(&stubTokens{token: "ghs_canned_token"}, signer)
	c.BaseURL = srv.URL

	_, err := c.GetRepoInstallation(context.Background(), RepoRef{Owner: "x", Name: "y"})
	if err == nil {
		t.Fatal("GetRepoInstallation: want error when signer.Sign fails, got nil")
	}
	if !errors.Is(err, signErr) {
		t.Errorf("err = %v, want it to wrap the signer error", err)
	}
	// The App-JWT mint failed, so no request should have reached the wire.
	if fg.gotAuth != "" {
		t.Errorf("Authorization = %q, want empty (request must not be sent when Sign fails)", fg.gotAuth)
	}
}

// comparePatchServer spins up an httptest server that answers the compare
// endpoint with the canned body and records the request line for the
// wiring assertions. Mirrors newTestClient's plumbing for ComparePatch.
func comparePatchServer(t *testing.T, status int, body string) (*Client, *struct {
	path   string
	accept string
}) {
	t.Helper()
	rec := &struct {
		path   string
		accept string
	}{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.path = r.URL.Path
		rec.accept = r.Header.Get("Accept")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	c, _ := newTestClient(t, srv, nil)
	return c, rec
}

func TestComparePatch_HappyPath(t *testing.T) {
	body := `{
		"total_commits": 2,
		"commits": [{"sha":"c1"},{"sha":"c2"}],
		"files": [
			{"filename":"a.go","status":"modified","changes":3,"patch":"@@ -1 +1 @@\n-a\n+b"},
			{"filename":"b.go","status":"added","changes":1,"patch":"@@ -0,0 +1 @@\n+new"}
		]
	}`
	c, rec := comparePatchServer(t, http.StatusOK, body)
	got, err := c.ComparePatch(context.Background(), 7, RepoRef{Owner: "o", Name: "r"}, "main", "fishhawk/run-abcd1234")
	if err != nil {
		t.Fatalf("ComparePatch: %v", err)
	}
	if got.HeadSHA != "c2" {
		t.Errorf("HeadSHA = %q, want c2 (last commit)", got.HeadSHA)
	}
	if len(got.Files) != 2 {
		t.Fatalf("Files = %d, want 2", len(got.Files))
	}
	if got.Files[0].Path != "a.go" || got.Files[0].Status != "modified" {
		t.Errorf("Files[0] = %+v", got.Files[0])
	}
	if got.Files[1].Path != "b.go" || got.Files[1].Status != "added" {
		t.Errorf("Files[1] = %+v", got.Files[1])
	}
	if !strings.Contains(got.Patch, "diff --git a/a.go b/a.go") ||
		!strings.Contains(got.Patch, "diff --git a/b.go b/b.go") {
		t.Errorf("Patch missing synthetic git headers: %q", got.Patch)
	}
	if !strings.Contains(got.Patch, "+b") || !strings.Contains(got.Patch, "+new") {
		t.Errorf("Patch missing hunk bodies: %q", got.Patch)
	}
	if got.Truncated {
		t.Errorf("Truncated = true, want false for a small comparison (reason %q)", got.TruncationReason)
	}
	// Wiring: three-dot compare path + default JSON media type.
	if !strings.Contains(rec.path, "/compare/main...fishhawk/run-abcd1234") {
		t.Errorf("compare path = %q", rec.path)
	}
	if rec.accept != "application/vnd.github+json" {
		t.Errorf("Accept = %q", rec.accept)
	}
}

func TestComparePatch_TruncatedFileCap(t *testing.T) {
	var files []string
	for i := 0; i < compareFilesCap; i++ {
		files = append(files, `{"filename":"f`+strconv.Itoa(i)+`.go","status":"modified","changes":1,"patch":"@@ -1 +1 @@\n-x\n+y"}`)
	}
	body := `{"total_commits":1,"commits":[{"sha":"head"}],"files":[` + strings.Join(files, ",") + `]}`
	c, _ := comparePatchServer(t, http.StatusOK, body)
	got, err := c.ComparePatch(context.Background(), 7, RepoRef{Owner: "o", Name: "r"}, "main", "head")
	if err != nil {
		t.Fatalf("ComparePatch: %v", err)
	}
	if !got.Truncated {
		t.Fatal("Truncated = false, want true at the 300-file cap")
	}
	if !strings.Contains(got.TruncationReason, "300-file compare cap") {
		t.Errorf("TruncationReason = %q, want the file-cap reason", got.TruncationReason)
	}
}

func TestComparePatch_TruncatedOmittedPatch(t *testing.T) {
	// A changed file (changes>0) whose patch body GitHub dropped (oversized).
	body := `{
		"total_commits": 1,
		"commits": [{"sha":"head"}],
		"files": [
			{"filename":"huge.go","status":"modified","changes":99999,"patch":""},
			{"filename":"img.png","status":"added","changes":0,"patch":""}
		]
	}`
	c, _ := comparePatchServer(t, http.StatusOK, body)
	got, err := c.ComparePatch(context.Background(), 7, RepoRef{Owner: "o", Name: "r"}, "main", "head")
	if err != nil {
		t.Fatalf("ComparePatch: %v", err)
	}
	if !got.Truncated {
		t.Fatal("Truncated = false, want true for an omitted oversized patch")
	}
	if !strings.Contains(got.TruncationReason, "omitted") {
		t.Errorf("TruncationReason = %q, want the omitted-patch reason", got.TruncationReason)
	}
	// The binary file (changes==0, no patch) alone must NOT flag truncation —
	// covered implicitly: only huge.go trips it. Sanity: both files listed.
	if len(got.Files) != 2 {
		t.Errorf("Files = %d, want 2", len(got.Files))
	}
}

func TestComparePatch_BinaryOnlyNotTruncated(t *testing.T) {
	body := `{"total_commits":1,"commits":[{"sha":"h"}],"files":[{"filename":"img.png","status":"added","changes":0,"patch":""}]}`
	c, _ := comparePatchServer(t, http.StatusOK, body)
	got, err := c.ComparePatch(context.Background(), 7, RepoRef{Owner: "o", Name: "r"}, "main", "h")
	if err != nil {
		t.Fatalf("ComparePatch: %v", err)
	}
	if got.Truncated {
		t.Errorf("Truncated = true for a binary-only diff (changes==0); want false")
	}
}

func TestComparePatch_NotFound(t *testing.T) {
	c, _ := comparePatchServer(t, http.StatusNotFound, `{"message":"Not Found"}`)
	_, err := c.ComparePatch(context.Background(), 7, RepoRef{Owner: "o", Name: "r"}, "main", "head")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestComparePatch_Validation(t *testing.T) {
	c := New(&stubTokens{})
	if _, err := c.ComparePatch(context.Background(), 1, RepoRef{Owner: "o", Name: "r"}, "", "head"); err == nil {
		t.Error("empty base: want error")
	}
	if _, err := c.ComparePatch(context.Background(), 1, RepoRef{}, "main", "head"); err == nil {
		t.Error("empty repo: want error")
	}
}

// fanInClient wires a Client to a one-off httptest server that serves the
// three fan-in primitives (GetBranchSHA / CreateRef / MergeBranch) with
// per-route programmable status + body, recording the last request seen
// on each route. The single mux lets one test exercise the create-then-
// merge sequence while asserting the exact paths/bodies.
func fanInClient(t *testing.T) (*Client, *fanInCapture) {
	t.Helper()
	cap := &fanInCapture{
		getRefStatus: http.StatusOK,
		createStatus: http.StatusCreated,
		mergeStatus:  http.StatusCreated,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/ref/heads/{branch...}",
		func(w http.ResponseWriter, r *http.Request) {
			cap.getRefPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(cap.getRefStatus)
			_, _ = io.WriteString(w, cap.getRefBody)
		})
	mux.HandleFunc("POST /repos/{owner}/{repo}/git/refs",
		func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &cap.createBody)
			cap.createCalls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(cap.createStatus)
			_, _ = io.WriteString(w, cap.createRespBody)
		})
	mux.HandleFunc("POST /repos/{owner}/{repo}/merges",
		func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &cap.mergeBody)
			cap.mergeCalls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(cap.mergeStatus)
			_, _ = io.WriteString(w, cap.mergeRespBody)
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{
		BaseURL: srv.URL,
		Tokens:  &stubTokens{token: "ghs_canned_token"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
	return c, cap
}

type fanInCapture struct {
	getRefStatus int
	getRefBody   string
	getRefPath   string

	createStatus   int
	createRespBody string
	createCalls    int
	createBody     struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}

	mergeStatus   int
	mergeRespBody string
	mergeCalls    int
	mergeBody     struct {
		Base          string `json:"base"`
		Head          string `json:"head"`
		CommitMessage string `json:"commit_message"`
	}
}

func TestGetBranchSHA_Found(t *testing.T) {
	c, cap := fanInClient(t)
	cap.getRefStatus = http.StatusOK
	cap.getRefBody = `{"ref":"refs/heads/fishhawk/run-abc","object":{"sha":"sha123"}}`

	sha, exists, err := c.GetBranchSHA(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "fishhawk/run-abc")
	if err != nil {
		t.Fatalf("GetBranchSHA: %v", err)
	}
	if !exists {
		t.Fatal("exists = false, want true")
	}
	if sha != "sha123" {
		t.Errorf("sha = %q, want sha123", sha)
	}
	if want := "/repos/x/y/git/ref/heads/fishhawk/run-abc"; cap.getRefPath != want {
		t.Errorf("path = %q, want %q", cap.getRefPath, want)
	}
}

func TestGetBranchSHA_Absent(t *testing.T) {
	c, cap := fanInClient(t)
	cap.getRefStatus = http.StatusNotFound
	cap.getRefBody = `{"message":"Not Found"}`

	sha, exists, err := c.GetBranchSHA(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "missing")
	if err != nil {
		t.Fatalf("GetBranchSHA on 404 should not error, got %v", err)
	}
	if exists {
		t.Error("exists = true, want false on 404")
	}
	if sha != "" {
		t.Errorf("sha = %q, want empty on 404", sha)
	}
}

func TestGetBranchSHA_Forbidden(t *testing.T) {
	c, cap := fanInClient(t)
	cap.getRefStatus = http.StatusForbidden
	cap.getRefBody = `{"message":"Forbidden"}`

	_, _, err := c.GetBranchSHA(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "b")
	if err == nil || !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
}

func TestCreateRef_Created(t *testing.T) {
	c, cap := fanInClient(t)
	cap.createStatus = http.StatusCreated
	cap.createRespBody = `{"ref":"refs/heads/fishhawk/run-abc"}`

	err := c.CreateRef(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "fishhawk/run-abc", "basesha")
	if err != nil {
		t.Fatalf("CreateRef: %v", err)
	}
	if cap.createBody.Ref != "refs/heads/fishhawk/run-abc" {
		t.Errorf("ref = %q, want refs/heads/fishhawk/run-abc", cap.createBody.Ref)
	}
	if cap.createBody.SHA != "basesha" {
		t.Errorf("sha = %q, want basesha", cap.createBody.SHA)
	}
}

func TestCreateRef_AlreadyExistsIsNoOp(t *testing.T) {
	c, cap := fanInClient(t)
	cap.createStatus = http.StatusUnprocessableEntity
	cap.createRespBody = `{"message":"Reference already exists"}`

	// A re-entrant fan-in pass that finds the consolidated branch already
	// created must treat the 422 as a benign no-op, not a failure.
	err := c.CreateRef(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "fishhawk/run-abc", "basesha")
	if err != nil {
		t.Fatalf("CreateRef on 'already exists' should be a no-op, got %v", err)
	}
}

func TestCreateRef_OtherValidationError(t *testing.T) {
	c, cap := fanInClient(t)
	cap.createStatus = http.StatusUnprocessableEntity
	cap.createRespBody = `{"message":"Invalid request: sha is not a valid object"}`

	err := c.CreateRef(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "fishhawk/run-abc", "bad")
	if err == nil || !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation for a non-duplicate 422", err)
	}
}

func TestMergeBranch_Merged(t *testing.T) {
	c, cap := fanInClient(t)
	cap.mergeStatus = http.StatusCreated
	cap.mergeRespBody = `{"sha":"mergecommit"}`

	sha, err := c.MergeBranch(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "fishhawk/run-abc", "fishhawk/run-abc/slice-0", "Integrate slice 0")
	if err != nil {
		t.Fatalf("MergeBranch: %v", err)
	}
	// The 201 body's sha is the integration merge commit recorded for the
	// ADR-035 lineage ledger (#1459).
	if sha != "mergecommit" {
		t.Errorf("merge sha = %q, want mergecommit", sha)
	}
	if cap.mergeBody.Base != "fishhawk/run-abc" || cap.mergeBody.Head != "fishhawk/run-abc/slice-0" {
		t.Errorf("merge body base/head = %q/%q", cap.mergeBody.Base, cap.mergeBody.Head)
	}
	if cap.mergeBody.CommitMessage != "Integrate slice 0" {
		t.Errorf("commit_message = %q", cap.mergeBody.CommitMessage)
	}
}

func TestMergeBranch_MergedMissingSHAIsBenign(t *testing.T) {
	c, cap := fanInClient(t)
	cap.mergeStatus = http.StatusCreated
	// A 201 whose body lacks/garbles the sha must NOT wedge a fan-in whose
	// merge already happened — decode defensively to ("", nil) (#1459).
	cap.mergeRespBody = `{"not_a_sha":true}`

	sha, err := c.MergeBranch(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "base", "head", "msg")
	if err != nil {
		t.Fatalf("MergeBranch 201 with absent sha should be nil error, got %v", err)
	}
	if sha != "" {
		t.Errorf("merge sha = %q, want empty when 201 body has no sha", sha)
	}
}

func TestMergeBranch_NothingToMerge(t *testing.T) {
	c, cap := fanInClient(t)
	cap.mergeStatus = http.StatusNoContent

	// 204 = base already contains head — idempotent success for a
	// re-entrant fan-in pass over an already-integrated slice; no merge
	// commit was created, so the SHA is empty.
	sha, err := c.MergeBranch(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "base", "head", "msg")
	if err != nil {
		t.Fatalf("MergeBranch 204 should be nil, got %v", err)
	}
	if sha != "" {
		t.Errorf("merge sha = %q, want empty on 204", sha)
	}
}

func TestMergeBranch_Conflict(t *testing.T) {
	c, cap := fanInClient(t)
	cap.mergeStatus = http.StatusConflict
	cap.mergeRespBody = `{"message":"Merge conflict"}`

	sha, err := c.MergeBranch(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "base", "head", "msg")
	if err == nil || !errors.Is(err, ErrMergeConflict) {
		t.Errorf("err = %v, want ErrMergeConflict on 409", err)
	}
	if sha != "" {
		t.Errorf("merge sha = %q, want empty on conflict", sha)
	}
}

func TestMergeBranch_NotFound(t *testing.T) {
	c, cap := fanInClient(t)
	cap.mergeStatus = http.StatusNotFound
	cap.mergeRespBody = `{"message":"Not Found"}`

	sha, err := c.MergeBranch(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "base", "missing", "msg")
	if err == nil || !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound on 404", err)
	}
	if sha != "" {
		t.Errorf("merge sha = %q, want empty on not found", sha)
	}
}

func TestMergeBranch_Validation(t *testing.T) {
	c, cap := fanInClient(t)
	cap.mergeStatus = http.StatusUnprocessableEntity
	cap.mergeRespBody = `{"message":"Validation Failed"}`

	sha, err := c.MergeBranch(context.Background(), 42,
		RepoRef{Owner: "x", Name: "y"}, "base", "head", "msg")
	if err == nil || !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation on 422", err)
	}
	if sha != "" {
		t.Errorf("merge sha = %q, want empty on validation error", sha)
	}
}
