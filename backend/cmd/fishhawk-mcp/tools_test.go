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
// somewhere to land). E19.4 / #344 added the per-run stage list
// and per-stage artifact list endpoints so the get_plan tests can
// drive the parent-walk loop without a full backend.
type fakeBackend struct {
	mu sync.Mutex

	lastListQuery string
	listResp      listRunsResult
	listStatus    int

	// /v0/runs/{run_id} fetches consult getRunByID first; the
	// fallback getResp is the default when the id isn't keyed.
	getRunByID map[uuid.UUID]Run
	getResp    Run
	getStatus  int

	// Per-call response overrides keyed by query string for tests
	// that exercise multiple resolution paths in one server.
	listByQuery map[string]listRunsResult

	// E19.4 fixtures: stages keyed by run id, artifacts keyed by
	// stage id. Empty map → 200 with empty items list (mirrors the
	// backend's behavior for runs that haven't created stages yet).
	stagesByRun       map[uuid.UUID][]Stage
	artifactsByStage  map[uuid.UUID][]Artifact
	stagesStatus      int
	artifactsStatus   int
	stagesCalledByID  map[uuid.UUID]int
	artifactsCalledID map[uuid.UUID]int
}

func newFakeBackend(t *testing.T) (*fakeBackend, *httptest.Server) {
	t.Helper()
	fb := &fakeBackend{
		listStatus:        http.StatusOK,
		getStatus:         http.StatusOK,
		stagesStatus:      http.StatusOK,
		artifactsStatus:   http.StatusOK,
		listByQuery:       map[string]listRunsResult{},
		getRunByID:        map[uuid.UUID]Run{},
		stagesByRun:       map[uuid.UUID][]Stage{},
		artifactsByStage:  map[uuid.UUID][]Artifact{},
		stagesCalledByID:  map[uuid.UUID]int{},
		artifactsCalledID: map[uuid.UUID]int{},
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
		id, perr := uuid.Parse(r.PathValue("run_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		row, ok := fb.getRunByID[id]
		fb.mu.Unlock()
		w.WriteHeader(fb.getStatus)
		if ok {
			_ = json.NewEncoder(w).Encode(row)
			return
		}
		_ = json.NewEncoder(w).Encode(fb.getResp)
	})
	mux.HandleFunc("GET /v0/runs/{run_id}/stages", func(w http.ResponseWriter, r *http.Request) {
		id, perr := uuid.Parse(r.PathValue("run_id"))
		w.Header().Set("Content-Type", "application/json")
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.stagesCalledByID[id]++
		items := fb.stagesByRun[id]
		fb.mu.Unlock()
		w.WriteHeader(fb.stagesStatus)
		_ = json.NewEncoder(w).Encode(listStagesResult{Items: items})
	})
	mux.HandleFunc("GET /v0/stages/{stage_id}/artifacts", func(w http.ResponseWriter, r *http.Request) {
		id, perr := uuid.Parse(r.PathValue("stage_id"))
		w.Header().Set("Content-Type", "application/json")
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.artifactsCalledID[id]++
		items := fb.artifactsByStage[id]
		fb.mu.Unlock()
		w.WriteHeader(fb.artifactsStatus)
		_ = json.NewEncoder(w).Encode(listArtifactsResult{Items: items})
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

// --- get_plan (E19.4 / #344) ---

// samplePlanContent returns a small but complete standard_v1
// fixture. Used as the inline content on the plan artifact rows
// the fake backend serves.
func samplePlanContent() PlanContent {
	return PlanContent{
		PlanVersion: "standard_v1",
		TicketReference: PlanTicketRef{
			Type: "github_issue",
			URL:  "https://github.com/x/y/issues/42",
			ID:   "x/y#42",
		},
		GeneratedBy: PlanGeneratedBy{
			Agent:     "claude-code",
			Model:     "claude-opus-4-7",
			Timestamp: time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC),
		},
		Summary: "Add a dryRun flag to the dispatcher.",
		Scope: PlanScope{
			Files: []PlanScopeFile{
				{Path: "backend/internal/webhook/dispatcher.go", Operation: "modify"},
			},
			EstimatedLinesChanged: 40,
		},
		Approach: []PlanApproachStep{
			{Step: 1, Description: "Plumb dryRun through Handle."},
			{Step: 2, Description: "Add a unit test."},
		},
		Verification: PlanVerification{
			TestStrategy: "Run the dispatcher tests.",
			RollbackPlan: "Revert the PR.",
		},
		RisksAndAssumptions: []string{
			"Operators set dryRun via a feature flag.",
		},
	}
}

// seedPlanArtifact attaches a plan artifact to a stage in the fake
// backend. createdAge sets the artifact's CreatedAt so tests can
// distinguish older vs newer when the most-recent-wins rule fires.
func seedPlanArtifact(fb *fakeBackend, stageID uuid.UUID, content PlanContent, createdAge time.Duration) Artifact {
	v := "standard_v1"
	body, _ := json.Marshal(content)
	art := Artifact{
		ID:            uuid.New(),
		StageID:       stageID,
		Kind:          "plan",
		SchemaVersion: &v,
		ContentHash:   "h",
		Content:       body,
		CreatedAt:     time.Now().UTC().Add(-createdAge),
	}
	fb.mu.Lock()
	fb.artifactsByStage[stageID] = append(fb.artifactsByStage[stageID], art)
	fb.mu.Unlock()
	return art
}

func TestGetPlan_RejectsInvalidUUID(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: "not-a-uuid"})
	if err == nil {
		t.Fatal("expected error on malformed run_id")
	}
	if !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("error wording: %v", err)
	}
}

func TestGetPlan_FromCurrentRun_StatusAvailableResolvedViaSelf(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID, RunID: runID, Type: "plan", State: "succeeded"},
		{ID: uuid.New(), RunID: runID, Type: "implement", State: "pending"},
	}
	expectedSummary := "Add a dryRun flag to the dispatcher."
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Status != "available" {
		t.Errorf("Status = %q, want available", out.Status)
	}
	if out.ResolvedVia != "self" {
		t.Errorf("ResolvedVia = %q, want self", out.ResolvedVia)
	}
	if out.Plan == nil {
		t.Fatal("Plan should be non-nil when Status=available")
	}
	if out.Plan.Summary != expectedSummary {
		t.Errorf("summary = %q", out.Plan.Summary)
	}
	if got := len(out.Plan.Scope.Files); got != 1 {
		t.Errorf("scope.files count = %d", got)
	}
}

func TestGetPlan_PicksMostRecentArtifactWhenMultipleExist(t *testing.T) {
	// Same plan stage carries two standard_v1 artifacts (a re-upload
	// after a plan edit). The resolver must pick the newer one.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{{ID: planStageID, RunID: runID, Type: "plan", State: "succeeded"}}

	older := samplePlanContent()
	older.Summary = "stale plan"
	seedPlanArtifact(fb, planStageID, older, 24*time.Hour)

	newer := samplePlanContent()
	newer.Summary = "fresh plan"
	seedPlanArtifact(fb, planStageID, newer, time.Hour)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if out.Plan == nil || out.Plan.Summary != "fresh plan" {
		t.Errorf("Plan.Summary = %v, want 'fresh plan'", out.Plan)
	}
}

func TestGetPlan_RetryChain_WalksParentRunID(t *testing.T) {
	// Child run has no plan stage (CI-retry shape per #279 / E16);
	// parent run has the plan. The walk should resolve the parent's
	// plan and stamp ResolvedVia=parent:<id>.
	fb, srv := newFakeBackend(t)
	parentID := uuid.New()
	childID := uuid.New()
	parentPlanStage := uuid.New()

	fb.getRunByID[childID] = Run{
		ID:          childID,
		ParentRunID: &parentID,
		State:       "running",
		Repo:        "x/y",
	}
	fb.getRunByID[parentID] = Run{ID: parentID, State: "running", Repo: "x/y"}
	// Child has only an implement stage (the retry's shape).
	fb.stagesByRun[childID] = []Stage{
		{ID: uuid.New(), RunID: childID, Type: "implement", State: "running"},
	}
	// Parent has the plan stage carrying the artifact.
	fb.stagesByRun[parentID] = []Stage{
		{ID: parentPlanStage, RunID: parentID, Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, parentPlanStage, samplePlanContent(), time.Hour)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: childID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Status != "available" {
		t.Errorf("Status = %q, want available", out.Status)
	}
	if out.ResolvedVia != "parent:"+parentID.String() {
		t.Errorf("ResolvedVia = %q, want parent:%s", out.ResolvedVia, parentID)
	}
	if out.Plan == nil {
		t.Fatal("Plan should be non-nil")
	}
}

func TestGetPlan_NoPlanYet_ChainRootReached(t *testing.T) {
	// Run has no plan stage AND no parent. The structured
	// no_plan_yet response names the chain depth searched (0,
	// since the root is the requested run itself).
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID, State: "running", Repo: "x/y"}
	fb.stagesByRun[runID] = nil // no stages — plan stage absent

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Status != "no_plan_yet" {
		t.Errorf("Status = %q, want no_plan_yet", out.Status)
	}
	if out.Plan != nil {
		t.Errorf("Plan should be nil on no_plan_yet; got %+v", out.Plan)
	}
	if !strings.Contains(out.Message, "chain root reached") {
		t.Errorf("Message should explain the chain shape: %q", out.Message)
	}
}

func TestGetPlan_NoPlanYet_PlanStagePending(t *testing.T) {
	// Plan stage exists but has no terminal plan artifact yet
	// (mid-upload race). Same no_plan_yet response shape so the
	// agent can branch without parsing prose.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID, State: "running"}
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID, RunID: runID, Type: "plan", State: "running"},
	}
	// Artifacts map: empty — no plan uploaded yet.

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "no_plan_yet" {
		t.Errorf("Status = %q, want no_plan_yet", out.Status)
	}
}

func TestGetPlan_RetryChain_DepthCap_NoPlanYet(t *testing.T) {
	// Build a chain of 10 runs, no plan stage on any of them. The
	// walk stops at retryPlanChainDepth (8) and returns
	// no_plan_yet with a "depth cap" message rather than looping
	// forever.
	fb, srv := newFakeBackend(t)
	const chainLen = 10
	ids := make([]uuid.UUID, chainLen)
	for i := range ids {
		ids[i] = uuid.New()
	}
	for i := 0; i < chainLen; i++ {
		row := Run{ID: ids[i], Repo: "x/y", State: "running"}
		if i+1 < chainLen {
			row.ParentRunID = &ids[i+1]
		}
		fb.getRunByID[ids[i]] = row
		fb.stagesByRun[ids[i]] = nil
	}

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: ids[0].String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Status != "no_plan_yet" {
		t.Errorf("Status = %q, want no_plan_yet", out.Status)
	}
	if !strings.Contains(out.Message, "chain depth cap") {
		t.Errorf("Message should mention chain depth cap: %q", out.Message)
	}
	// Defensive: the walk visited at most retryPlanChainDepth
	// stages-fetches, never the 9th id in the chain.
	if got := fb.stagesCalledByID[ids[retryPlanChainDepth]]; got != 0 {
		t.Errorf("walk visited id[%d] %d times; expected 0 (past the cap)",
			retryPlanChainDepth, got)
	}
}

func TestGetPlan_BackendError_StagesList_Surfaced(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID}
	fb.stagesStatus = http.StatusInternalServerError

	r := newResolver(srv, nil)
	_, _, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err == nil {
		t.Fatal("expected error on stages 500")
	}
	if !strings.Contains(err.Error(), "list stages") {
		t.Errorf("error wording: %v", err)
	}
}

func TestGetPlan_IgnoresNonStandardV1PlanArtifacts(t *testing.T) {
	// A plan stage might carry future-schema artifacts. The
	// resolver only returns standard_v1 — anything else is invisible
	// to v0's MCP tools.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{{ID: planStageID, RunID: runID, Type: "plan", State: "succeeded"}}

	v := "future_v2"
	body, _ := json.Marshal(map[string]any{"plan_version": "future_v2"})
	fb.artifactsByStage[planStageID] = []Artifact{{
		ID: uuid.New(), StageID: planStageID, Kind: "plan",
		SchemaVersion: &v, Content: body,
		CreatedAt: time.Now().UTC(),
	}}

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "no_plan_yet" {
		t.Errorf("Status = %q, want no_plan_yet (future schema is invisible)", out.Status)
	}
}
