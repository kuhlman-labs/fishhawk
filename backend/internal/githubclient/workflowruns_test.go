package githubclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// workflowRunsRequest is one request the workflow-runs fake recorded.
type workflowRunsRequest struct {
	method string
	path   string
	query  string
}

// workflowRunsServer records every request's method, path and raw query.
type workflowRunsServer struct {
	mu       sync.Mutex
	requests []workflowRunsRequest
}

func (s *workflowRunsServer) recorded() []workflowRunsRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]workflowRunsRequest(nil), s.requests...)
}

func workflowRunsClient(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) (*Client, *workflowRunsServer) {
	t.Helper()
	rec := &workflowRunsServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.requests = append(rec.requests, workflowRunsRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery})
		rec.mu.Unlock()
		handle(w, r)
	}))
	t.Cleanup(srv.Close)
	return &Client{
		BaseURL: srv.URL,
		Tokens:  &stubTokens{token: "ghs_canned_token"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}, rec
}

var workflowRunsRepo = RepoRef{Owner: "x", Name: "y"}

func TestListWorkflowRunsForHeadSHA_QueryAndDecode(t *testing.T) {
	c, rec := workflowRunsClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"total_count":3,"workflow_runs":[
			{"id":11,"html_url":"https://github.com/x/y/actions/runs/11","status":"completed","conclusion":"failure",
			 "event":"pull_request","head_branch":"run/b","head_sha":"aaa"},
			{"id":12,"status":"in_progress","conclusion":null,"event":"push","head_branch":"run/b","head_sha":"aaa"},
			{"id":13,"status":"completed","conclusion":"success","event":"workflow_dispatch","head_sha":"aaa"}
		]}`))
	})
	runs, err := c.ListWorkflowRunsForHeadSHA(context.Background(), forge.FromGitHubInstallationID(42), workflowRunsRepo, "aaa")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	reqs := rec.recorded()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1 (first page only)", len(reqs))
	}
	if reqs[0].method != http.MethodGet || reqs[0].path != "/repos/x/y/actions/runs" {
		t.Errorf("request = %s %s, want GET /repos/x/y/actions/runs", reqs[0].method, reqs[0].path)
	}
	if !strings.Contains(reqs[0].query, "head_sha=aaa") || !strings.Contains(reqs[0].query, "per_page=100") {
		t.Errorf("query = %q, want head_sha=aaa and per_page=100", reqs[0].query)
	}
	if len(runs) != 3 {
		t.Fatalf("runs = %d, want 3", len(runs))
	}
	if r := runs[0]; r.ID != 11 || r.Status != "completed" || r.Conclusion != "failure" || r.Event != "pull_request" ||
		r.HeadSHA != "aaa" || r.HeadBranch != "run/b" || r.HTMLURL != "https://github.com/x/y/actions/runs/11" {
		t.Errorf("runs[0] = %+v", r)
	}
	if runs[1].Conclusion != "" {
		t.Errorf("null conclusion decoded as %q, want empty", runs[1].Conclusion)
	}
	if runs[2].Event != "workflow_dispatch" {
		t.Errorf("runs[2].Event = %q", runs[2].Event)
	}
}

func TestListWorkflowRunsForHeadSHA_ErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusForbidden, ErrForbidden},
		{http.StatusNotFound, ErrNotFound},
	} {
		c, _ := workflowRunsClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status) })
		_, err := c.ListWorkflowRunsForHeadSHA(context.Background(), forge.FromGitHubInstallationID(42), workflowRunsRepo, "aaa")
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: err = %v, want %v", tc.status, err, tc.want)
		}
	}
}

func TestListWorkflowRunsForHeadSHA_DecodeAndTransportErrors(t *testing.T) {
	c, _ := workflowRunsClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{not json`)) })
	if _, err := c.ListWorkflowRunsForHeadSHA(context.Background(), forge.FromGitHubInstallationID(42), workflowRunsRepo, "aaa"); err == nil ||
		!strings.Contains(err.Error(), "decode workflow runs") {
		t.Errorf("err = %v, want a decode error", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()
	dead := &Client{BaseURL: srv.URL, Tokens: &stubTokens{token: "t"}, HTTP: &http.Client{Timeout: time.Second}}
	if _, err := dead.ListWorkflowRunsForHeadSHA(context.Background(), forge.FromGitHubInstallationID(42), workflowRunsRepo, "aaa"); err == nil ||
		!strings.Contains(err.Error(), "list workflow runs") {
		t.Errorf("err = %v, want a transport error", err)
	}
	if err := dead.RerunWorkflowRun(context.Background(), forge.FromGitHubInstallationID(42), workflowRunsRepo, 7); err == nil ||
		!strings.Contains(err.Error(), "rerun workflow run") {
		t.Errorf("err = %v, want a transport error", err)
	}
}

func TestRerunWorkflowRun_PostsRerun(t *testing.T) {
	c, rec := workflowRunsClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) })
	if err := c.RerunWorkflowRun(context.Background(), forge.FromGitHubInstallationID(42), workflowRunsRepo, 77); err != nil {
		t.Fatalf("err = %v, want nil on 201", err)
	}
	reqs := rec.recorded()
	if len(reqs) != 1 || reqs[0].method != http.MethodPost || reqs[0].path != "/repos/x/y/actions/runs/77/rerun" {
		t.Fatalf("requests = %+v, want one POST /repos/x/y/actions/runs/77/rerun", reqs)
	}
}

func TestRerunWorkflowRun_ErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusForbidden, ErrForbidden},
		{http.StatusNotFound, ErrNotFound},
	} {
		c, _ := workflowRunsClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status) })
		err := c.RerunWorkflowRun(context.Background(), forge.FromGitHubInstallationID(42), workflowRunsRepo, 77)
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: err = %v, want %v", tc.status, err, tc.want)
		}
	}
}

func TestWorkflowRuns_RefuseBadInput(t *testing.T) {
	c, rec := workflowRunsClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	ctx := context.Background()
	scope := forge.FromGitHubInstallationID(42)
	noTokens := &Client{BaseURL: "http://unused"}
	cases := map[string]func() error{
		"list empty sha": func() error {
			_, err := c.ListWorkflowRunsForHeadSHA(ctx, scope, workflowRunsRepo, "")
			return err
		},
		"list empty repo": func() error {
			_, err := c.ListWorkflowRunsForHeadSHA(ctx, scope, RepoRef{Owner: "x"}, "aaa")
			return err
		},
		"list zero scope": func() error {
			_, err := c.ListWorkflowRunsForHeadSHA(ctx, forge.CredentialScope{}, workflowRunsRepo, "aaa")
			return err
		},
		"list no tokens": func() error {
			_, err := noTokens.ListWorkflowRunsForHeadSHA(ctx, scope, workflowRunsRepo, "aaa")
			return err
		},
		"rerun zero id": func() error {
			return c.RerunWorkflowRun(ctx, scope, workflowRunsRepo, 0)
		},
		"rerun negative id": func() error {
			return c.RerunWorkflowRun(ctx, scope, workflowRunsRepo, -1)
		},
		"rerun empty repo": func() error {
			return c.RerunWorkflowRun(ctx, scope, RepoRef{Name: "y"}, 7)
		},
		"rerun zero scope": func() error {
			return c.RerunWorkflowRun(ctx, forge.CredentialScope{}, workflowRunsRepo, 7)
		},
		"rerun no tokens": func() error {
			return noTokens.RerunWorkflowRun(ctx, scope, workflowRunsRepo, 7)
		},
	}
	for name, call := range cases {
		if err := call(); err == nil {
			t.Errorf("%s: err = nil, want a refusal", name)
		}
	}
	if n := len(rec.recorded()); n != 0 {
		t.Fatalf("requests = %d, want 0 (every refusal precedes the call)", n)
	}
}
