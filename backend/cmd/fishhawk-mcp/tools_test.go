package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeBackend is a thin httptest server that records the last
// /v0/runs query (so tests can assert filter forwarding) and a
// /v0/runs/{id} fetch path (so the FISHHAWK_RUN_ID branch has
// somewhere to land).
type fakeBackend struct {
	mu sync.Mutex

	lastListQuery string
	listResp      listRunsResult
	listStatus    int

	getResp   Run
	getStatus int

	// Per-call response overrides keyed by query string for tests
	// that exercise multiple resolution paths in one server.
	listByQuery map[string]listRunsResult
}

func newFakeBackend(t *testing.T) (*fakeBackend, *httptest.Server) {
	t.Helper()
	fb := &fakeBackend{
		listStatus:  http.StatusOK,
		getStatus:   http.StatusOK,
		listByQuery: map[string]listRunsResult{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/runs", func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.lastListQuery = r.URL.RawQuery
		resp, override := fb.listByQuery[r.URL.RawQuery]
		fb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fb.listStatus)
		if override {
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		_ = json.NewEncoder(w).Encode(fb.listResp)
	})
	mux.HandleFunc("GET /v0/runs/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fb.getStatus)
		_ = json.NewEncoder(w).Encode(fb.getResp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fb, srv
}

func newResolver(srv *httptest.Server, env map[string]string) *runResolver {
	return &runResolver{
		api: newAPIClient(config{
			backendURL: srv.URL,
			apiToken:   "tok-test",
		}),
		getenv: envFuncFromMap(env),
	}
}

func envFuncFromMap(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

func sampleRun(id uuid.UUID, repo string, age time.Duration) Run {
	pr := "https://github.com/" + repo + "/pull/42"
	tr := "issue:42"
	return Run{
		ID: id, Repo: repo, WorkflowID: "feature_change",
		TriggerSource:  "github_issue",
		TriggerRef:     &tr,
		State:          "running",
		PullRequestURL: &pr,
		CreatedAt:      time.Now().UTC().Add(-age),
		UpdatedAt:      time.Now().UTC().Add(-age),
	}
}

func TestGetActiveRun_ByPRNumber_QueriesPullRequestURL(t *testing.T) {
	fb, srv := newFakeBackend(t)
	id := uuid.New()
	fb.listResp = listRunsResult{Items: []Run{sampleRun(id, "x/y", time.Hour)}}
	r := newResolver(srv, nil)

	_, out, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{
		Repo:     "x/y",
		PRNumber: 42,
	})
	if err != nil {
		t.Fatalf("getActiveRun: %v", err)
	}
	if out.Run.ID != id {
		t.Errorf("Run.ID = %s, want %s", out.Run.ID, id)
	}
	// Verify the filter actually hit the backend.
	for _, want := range []string{
		"repo=x%2Fy",
		"pull_request_url=https%3A%2F%2Fgithub.com%2Fx%2Fy%2Fpull%2F42",
	} {
		if !strings.Contains(fb.lastListQuery, want) {
			t.Errorf("query missing %q: %s", want, fb.lastListQuery)
		}
	}
}

func TestGetActiveRun_ByPRNumber_RequiresRepo(t *testing.T) {
	// pr_number set, repo missing, GITHUB_REPOSITORY unset → the
	// tool can't build the canonical pull_request_url. Surface a
	// clean error rather than silently scoping the search to all
	// installations.
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{
		PRNumber: 42,
	})
	if err == nil {
		t.Fatal("expected error when repo and GITHUB_REPOSITORY are both unset")
	}
	if !strings.Contains(err.Error(), "repo required") {
		t.Errorf("error wording: %v", err)
	}
}

func TestGetActiveRun_ByPRNumber_FallsBackToGitHubRepositoryEnv(t *testing.T) {
	fb, srv := newFakeBackend(t)
	id := uuid.New()
	fb.listResp = listRunsResult{Items: []Run{sampleRun(id, "x/y", time.Hour)}}
	r := newResolver(srv, map[string]string{"GITHUB_REPOSITORY": "x/y"})

	_, out, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{
		PRNumber: 42,
	})
	if err != nil {
		t.Fatalf("getActiveRun: %v", err)
	}
	if out.Run.ID != id {
		t.Errorf("Run.ID = %s, want %s", out.Run.ID, id)
	}
}

func TestGetActiveRun_ByTriggerRef_QueriesTriggerRefFilter(t *testing.T) {
	fb, srv := newFakeBackend(t)
	id := uuid.New()
	fb.listResp = listRunsResult{Items: []Run{sampleRun(id, "x/y", time.Hour)}}
	r := newResolver(srv, map[string]string{"GITHUB_REPOSITORY": "x/y"})

	_, out, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{
		TriggerRef: "issue:42",
	})
	if err != nil {
		t.Fatalf("getActiveRun: %v", err)
	}
	if out.Run.ID != id {
		t.Errorf("Run.ID = %s, want %s", out.Run.ID, id)
	}
	for _, want := range []string{"repo=x%2Fy", "trigger_ref=issue%3A42"} {
		if !strings.Contains(fb.lastListQuery, want) {
			t.Errorf("query missing %q: %s", want, fb.lastListQuery)
		}
	}
}

func TestGetActiveRun_ByEnvRunID_DirectFetch(t *testing.T) {
	// The runner case: FISHHAWK_RUN_ID stamped on the env →
	// fetch the run directly without a list scan.
	fb, srv := newFakeBackend(t)
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	fb.getResp = sampleRun(id, "x/y", time.Hour)
	r := newResolver(srv, map[string]string{"FISHHAWK_RUN_ID": id.String()})

	_, out, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{})
	if err != nil {
		t.Fatalf("getActiveRun: %v", err)
	}
	if out.Run.ID != id {
		t.Errorf("Run.ID = %s, want %s", out.Run.ID, id)
	}
}

func TestGetActiveRun_ByEnvRunID_RejectsInvalidUUID(t *testing.T) {
	// Defensive: if the runner stamps a malformed env, surface a
	// clear error rather than a generic 4xx from the GET path.
	_, srv := newFakeBackend(t)
	r := newResolver(srv, map[string]string{"FISHHAWK_RUN_ID": "not-a-uuid"})

	_, _, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{})
	if err == nil {
		t.Fatal("expected error on malformed FISHHAWK_RUN_ID")
	}
	if !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("error wording: %v", err)
	}
}

func TestGetActiveRun_NoResolutionPath_ReturnsStructuredError(t *testing.T) {
	// No pr_number, no trigger_ref, no FISHHAWK_RUN_ID. The error
	// message must list every input the caller could supply so an
	// agent reading it can ask the human for the missing piece.
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{})
	if err == nil {
		t.Fatal("expected error when no resolution path is available")
	}
	for _, want := range []string{"pr_number", "trigger_ref", "FISHHAWK_RUN_ID"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q as an option: %v", want, err)
		}
	}
}

func TestGetActiveRun_PRNumber_NoMatchingRun(t *testing.T) {
	// Empty list response → friendly error naming the repo + PR
	// number so the caller knows the lookup itself worked but
	// nothing matched.
	fb, srv := newFakeBackend(t)
	fb.listResp = listRunsResult{Items: nil}
	r := newResolver(srv, nil)

	_, _, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{
		Repo:     "x/y",
		PRNumber: 42,
	})
	if err == nil {
		t.Fatal("expected not-found error")
	}
	if !strings.Contains(err.Error(), "x/y") || !strings.Contains(err.Error(), "pull/42") {
		t.Errorf("error should name the repo + PR: %v", err)
	}
}

func TestGetActiveRun_PicksMostRecentByCreatedAt(t *testing.T) {
	// Two runs on the same PR (e.g., a retry chain). The resolver
	// returns the newer one. Defensive sort — even if the
	// backend ever stops ordering, we still pick correctly.
	fb, srv := newFakeBackend(t)
	older := uuid.New()
	newer := uuid.New()
	fb.listResp = listRunsResult{Items: []Run{
		sampleRun(older, "x/y", 24*time.Hour),
		sampleRun(newer, "x/y", time.Hour),
	}}
	r := newResolver(srv, nil)

	_, out, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{
		Repo:     "x/y",
		PRNumber: 42,
	})
	if err != nil {
		t.Fatalf("getActiveRun: %v", err)
	}
	if out.Run.ID != newer {
		t.Errorf("Run.ID = %s, want newer %s", out.Run.ID, newer)
	}
}

func TestGetActiveRun_BackendError_Surfaced(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.listStatus = http.StatusInternalServerError
	r := newResolver(srv, nil)

	_, _, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{
		Repo:     "x/y",
		PRNumber: 42,
	})
	if err == nil {
		t.Fatal("expected backend error")
	}
	// Both wrapped error and the underlying *apiError reach the
	// caller; just verify the surface message is helpful.
	if !strings.Contains(err.Error(), "list runs") {
		t.Errorf("error wording: %v", err)
	}
}

func TestGetActiveRun_ResolutionOrder_PRNumberBeatsTriggerRef(t *testing.T) {
	// Both pr_number and trigger_ref provided — the spec's
	// resolution order says pr_number wins. Verify the trigger_ref
	// branch isn't even consulted (it would otherwise hit the
	// backend with a different query).
	fb, srv := newFakeBackend(t)
	id := uuid.New()
	fb.listResp = listRunsResult{Items: []Run{sampleRun(id, "x/y", time.Hour)}}
	r := newResolver(srv, nil)

	_, out, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{
		Repo:       "x/y",
		PRNumber:   42,
		TriggerRef: "issue:42",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Run.ID != id {
		t.Errorf("Run.ID = %s, want %s", out.Run.ID, id)
	}
	if !strings.Contains(fb.lastListQuery, "pull_request_url=") {
		t.Errorf("expected pull_request_url filter (pr_number wins); got %s", fb.lastListQuery)
	}
	if strings.Contains(fb.lastListQuery, "trigger_ref=") {
		t.Errorf("trigger_ref filter should not have been used: %s", fb.lastListQuery)
	}
}

func TestRegisterTools_RegistersGetActiveRun(t *testing.T) {
	// Smoke test: registerTools doesn't panic and the SDK accepts
	// the tool definition. Full handshake verification lives in
	// the SDK; we just assert the registration call sequence
	// completes for v0's tool set.
	cfg := config{backendURL: "http://localhost:8080", apiToken: "tok"}
	srv := buildServer(cfg)
	resolver := &runResolver{
		api:    newAPIClient(cfg),
		getenv: envFuncFromMap(nil),
	}
	registerTools(srv, resolver)
}
