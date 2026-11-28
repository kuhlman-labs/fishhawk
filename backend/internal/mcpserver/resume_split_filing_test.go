package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
)

// resumeSplitFilingBackend is an in-file fake of the resume route: it records
// every request and answers with a canned status + body.
type resumeSplitFilingBackend struct {
	mu       sync.Mutex
	status   int
	body     string
	requests []resumeSplitFilingRequest
}

type resumeSplitFilingRequest struct {
	method, path, auth string
}

func newResumeSplitFilingBackend(t *testing.T, status int, body string) (*resumeSplitFilingBackend, *runResolver) {
	t.Helper()
	fb := &resumeSplitFilingBackend{status: status, body: body}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.requests = append(fb.requests, resumeSplitFilingRequest{r.Method, r.URL.Path, r.Header.Get("Authorization")})
		fb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fb.status)
		_, _ = w.Write([]byte(fb.body))
	}))
	t.Cleanup(srv.Close)
	return fb, newResolver(srv, nil)
}

func (fb *resumeSplitFilingBackend) calls() []resumeSplitFilingRequest {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return append([]resumeSplitFilingRequest(nil), fb.requests...)
}

// TestResumeSplitFiling_Resuming202MapsEveryField pins the 202 mapping of every
// output field and the request shape (POST, the route path, the bearer).
func TestResumeSplitFiling_Resuming202MapsEveryField(t *testing.T) {
	runID := uuid.New()
	stageID := uuid.New()
	fb, r := newResumeSplitFilingBackend(t, http.StatusAccepted,
		`{"run_id":"`+runID.String()+`","stage_id":"`+stageID.String()+`","state":"resuming","phase_count":3,"already_filed":1,"budget_seconds":300}`)

	_, out, err := r.resumeSplitFiling(context.Background(), nil, ResumeSplitFilingInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("resumeSplitFiling: %v", err)
	}
	want := ResumeSplitFilingOutput{RunID: runID.String(), StageID: stageID.String(), State: "resuming", PhaseCount: 3, AlreadyFiled: 1, BudgetSeconds: 300}
	got := out
	got.Message = ""
	if got != want {
		t.Errorf("output = %+v, want %+v", got, want)
	}
	for _, s := range []string{"1 of 3 phases", "remaining 2", "budget_seconds (300)", "not proof of progress", "split_filing_in_progress", "fishhawk_get_plan", "split_children_filed"} {
		if !strings.Contains(out.Message, s) {
			t.Errorf("message missing %q: %q", s, out.Message)
		}
	}
	calls := fb.calls()
	if len(calls) != 1 {
		t.Fatalf("backend calls = %d, want 1", len(calls))
	}
	if c := calls[0]; c.method != http.MethodPost || c.path != "/v0/runs/"+runID.String()+"/split-filing/resume" || c.auth != "Bearer tok-test" {
		t.Errorf("request = %+v, want POST /v0/runs/%s/split-filing/resume with the bearer", c, runID)
	}
}

// TestResumeSplitFiling_Completed200 pins the completed path: no budget, and
// a message saying nothing was launched.
func TestResumeSplitFiling_Completed200(t *testing.T) {
	runID := uuid.New()
	_, r := newResumeSplitFilingBackend(t, http.StatusOK,
		`{"run_id":"`+runID.String()+`","stage_id":"`+uuid.NewString()+`","state":"completed","phase_count":3,"already_filed":3}`)

	_, out, err := r.resumeSplitFiling(context.Background(), nil, ResumeSplitFilingInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("resumeSplitFiling: %v", err)
	}
	if out.State != "completed" || out.PhaseCount != 3 || out.AlreadyFiled != 3 || out.BudgetSeconds != 0 {
		t.Errorf("output = %+v, want completed / 3 / 3 / no budget", out)
	}
	if !strings.Contains(out.Message, "already completed") || !strings.Contains(out.Message, "nothing was launched") {
		t.Errorf("message = %q, want the completed wording", out.Message)
	}
}

// TestResumeSplitFiling_RefusalsPassThroughVerbatim pins that every backend
// refusal reaches the operator with its code and message verbatim, plus the
// remedy hint, rather than being swallowed into a false success.
func TestResumeSplitFiling_RefusalsPassThroughVerbatim(t *testing.T) {
	cases := []struct {
		status    int
		code, msg string
		hint      string
	}{
		{http.StatusConflict, "split_filing_in_progress", "a split filing for this run is still in flight", "still running"},
		{http.StatusConflict, "plan_not_approved", "the run's plan stage is awaiting_approval, not succeeded", "approve the run's own plan stage"},
		{http.StatusConflict, "run_terminal", "run is cancelled", resumeSplitFilingHandFiledWarning},
		{http.StatusConflict, "plan_changed_since_approval", "the plan stage's newest plan artifact was created after the plan gate closed", "start a new run, because the artifact changed after the plan gate closed"},
		{http.StatusConflict, "split_filing_no_parent_issue", "the run has no originating issue trigger_ref", "no parent to reference"},
		{http.StatusForbidden, "run_token_forbidden", "a run-bound agent token may not resume a split filing", "operator session"},
		{http.StatusForbidden, "insufficient_scope", "token is missing required scope: write:approvals", "write:approvals"},
		// A code with NO hint isolates the fallback passthrough branch.
		{http.StatusInternalServerError, "internal_error", "could not read split_children_filed; nothing was recorded or filed", ""},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"error": map[string]any{"code": tc.code, "message": tc.msg, "details": map[string]any{"run_id": "x"}}})
			_, r := newResumeSplitFilingBackend(t, tc.status, string(body))
			_, _, err := r.resumeSplitFiling(context.Background(), nil, ResumeSplitFilingInput{RunID: uuid.NewString()})
			if err == nil {
				t.Fatal("want a tool error for a backend refusal")
			}
			for _, s := range []string{"HTTP " + strconv.Itoa(tc.status), tc.code, tc.msg, tc.hint} {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error missing %q: %v", s, err)
				}
			}
		})
	}
}

// TestResumeSplitFiling_BadUUIDRefusedLocally: a malformed run id never
// reaches the backend.
func TestResumeSplitFiling_BadUUIDRefusedLocally(t *testing.T) {
	fb, r := newResumeSplitFilingBackend(t, http.StatusAccepted, `{}`)
	_, _, err := r.resumeSplitFiling(context.Background(), nil, ResumeSplitFilingInput{RunID: "not-a-uuid"})
	if err == nil || !strings.Contains(err.Error(), "not a valid UUID") {
		t.Fatalf("error = %v, want the UUID refusal", err)
	}
	if n := len(fb.calls()); n != 0 {
		t.Errorf("the malformed id reached the backend %d times", n)
	}
}

// TestResumeSplitFiling_DescriptionCarriesWarnings pins the AA1/AA5 and
// hand-filed text the operator reads before calling the tool.
func TestResumeSplitFiling_DescriptionCarriesWarnings(t *testing.T) {
	desc := resumeToolDescription(t, "fishhawk_resume_split_filing")
	for _, s := range []string{
		resumeSplitFilingHandFiledWarning,
		"NOT\nproof of progress",
		"budget_seconds",
		"409 split_filing_in_progress means the filing is still running",
		"start a new run, because the\n    artifact changed after the plan gate closed",
		"run_terminal",
		"OWN\napproved plan artifact",
		"write:approvals",
	} {
		if !strings.Contains(desc, s) {
			t.Errorf("description missing %q", s)
		}
	}
	if plan := getPlanOutputSchemaText(t); !strings.Contains(plan, "fishhawk_resume_split_filing") || !strings.Contains(plan, "NOT proof of progress") {
		t.Error("fishhawk_get_plan split_filing schema text does not name fishhawk_resume_split_filing for the absent-while-succeeded case")
	}
}

// resumeToolDescription lists the registered tools over the SDK and returns name's
// description.
func resumeToolDescription(t *testing.T, name string) string {
	t.Helper()
	for _, tool := range listRegisteredTools(t) {
		if tool.Name == name {
			return tool.Description
		}
	}
	t.Fatalf("tool %s is not registered", name)
	return ""
}

// listRegisteredTools lists every registered tool over the SDK's in-memory
// transport.
func listRegisteredTools(t *testing.T) []*mcp.Tool {
	t.Helper()
	ctx := context.Background()
	srv := NewServer(Config{BackendURL: "http://127.0.0.1:1", APIToken: "tok-test"})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "resume-split-desc", Version: "0"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	return res.Tools
}

// getPlanOutputSchemaText returns fishhawk_get_plan's output-schema JSON, which
// carries the split_filing jsonschema text.
func getPlanOutputSchemaText(t *testing.T) string {
	t.Helper()
	for _, tool := range listRegisteredTools(t) {
		if tool.Name == "fishhawk_get_plan" {
			raw, err := json.Marshal(tool.OutputSchema)
			if err != nil {
				t.Fatalf("marshal get_plan output schema: %v", err)
			}
			return string(raw)
		}
	}
	t.Fatal("fishhawk_get_plan is not registered")
	return ""
}

// TestResumeSplitFiling_RealServerDecodes202 is the cross-boundary MCP → REST
// test over pgtest Postgres: the REAL server.New with run, artifact, audit and
// approval repositories serves the route, and the tool decodes its ACTUAL 202
// body. Its database comes ONLY from pgtest.NewPool, which skips the test when
// FISHHAWK_SKIP_INTEGRATION is set or Docker is unreachable; it adds no skip of
// its own. The hermetic twin is
// backend/internal/server/split_filing_resume_mcp_test.go.
func TestResumeSplitFiling_RealServerDecodes202(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := runpkg.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	artRepo := artifact.NewPostgresRepository(pool)
	apprRepo := approval.NewPostgresRepository(pool)

	const bearer = "fhk_resume_split_e2e"
	s := server.New(server.Config{
		RunRepo: runRepo, AuditRepo: auditRepo, ArtifactRepo: artRepo, ApprovalRepo: apprRepo,
		APITokenRepo: &stubMCPAPITokens{tok: &apitoken.Token{
			ID: uuid.New(), Subject: "github:op", Scopes: []string{"write:approvals"}, PlainText: bearer,
		}},
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ts.Close()
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = s.Shutdown(sctx)
	})

	trigger := "issue:2100"
	rn, err := runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo: "o/r", WorkflowID: "feature_change", WorkflowSHA: "abc",
		TriggerSource: runpkg.TriggerCLI, TriggerRef: &trigger,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := runRepo.TransitionRun(ctx, rn.ID, runpkg.StateRunning); err != nil {
		t.Fatalf("run → running: %v", err)
	}
	st, err := runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: rn.ID, Sequence: 1, Type: runpkg.StageTypePlan,
		ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "claude-code", RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("create plan stage: %v", err)
	}
	for _, to := range []runpkg.StageState{runpkg.StageStateDispatched, runpkg.StageStateRunning, runpkg.StageStateAwaitingApproval} {
		if _, err := runRepo.TransitionStage(ctx, st.ID, to, nil); err != nil {
			t.Fatalf("plan stage → %s: %v", to, err)
		}
	}
	// The artifact is created BEFORE the approve row and the stage success. Its
	// created_at is the DATABASE's now() while EndedAt is stamped from this
	// process's clock, so the AA1 EndedAt check compares two clocks: wait until
	// this process's clock has passed the artifact's created_at before the
	// stage succeeds, so a database clock running ahead of the test process
	// cannot refuse the resume. The approve-row check is database-clock only.
	sv := "standard_v1"
	art, err := artRepo.Create(ctx, artifact.CreateParams{
		StageID: st.ID, Kind: artifact.KindPlan, SchemaVersion: &sv, Content: resumeSplitPlanJSON(t),
	})
	if err != nil {
		t.Fatalf("create plan artifact: %v", err)
	}
	waitClockPast(t, art.CreatedAt, 10*time.Second)
	if _, err := apprRepo.Submit(ctx, approval.SubmitParams{
		StageID: st.ID, ApproverSubject: "github:op", Decision: approval.DecisionApprove, Surface: approval.SurfaceAPI,
	}); err != nil {
		t.Fatalf("submit approve row: %v", err)
	}
	if _, err := runRepo.TransitionStage(ctx, st.ID, runpkg.StageStateSucceeded, nil); err != nil {
		t.Fatalf("plan stage → succeeded: %v", err)
	}
	idx := 0
	marker, _ := json.Marshal(map[string]any{"split_phase_index": idx, "created_number": 4001, "created_url": "https://github.com/o/r/issues/4001"})
	if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID: rn.ID, Timestamp: time.Now().UTC(), Category: "work_item_filed", Payload: marker,
	}); err != nil {
		t.Fatalf("seed phase-0 marker: %v", err)
	}

	r := &runResolver{api: newAPIClient(config{backendURL: ts.URL, apiToken: bearer}), getenv: func(string) string { return "" }}
	_, out, err := r.resumeSplitFiling(ctx, nil, ResumeSplitFilingInput{RunID: rn.ID.String()})
	if err != nil {
		t.Fatalf("resumeSplitFiling against the real server: %v", err)
	}
	if out.State != "resuming" || out.PhaseCount != 3 || out.AlreadyFiled != 1 || out.BudgetSeconds <= 0 {
		t.Errorf("output = %+v, want resuming / phase_count 3 / already_filed 1 / budget_seconds > 0", out)
	}
	if out.StageID != st.ID.String() || out.RunID != rn.ID.String() {
		t.Errorf("ids = %s/%s, want %s/%s", out.RunID, out.StageID, rn.ID, st.ID)
	}
	if art.ID == uuid.Nil {
		t.Error("seeded plan artifact has no id")
	}
}

// waitClockPast blocks until this process's clock is strictly after at, a
// database-assigned timestamp, failing when the skew exceeds bound.
func waitClockPast(t *testing.T, at time.Time, bound time.Duration) {
	t.Helper()
	deadline := time.Now().Add(bound)
	for !time.Now().After(at) {
		if time.Now().After(deadline) {
			t.Fatalf("database clock leads this process by more than %v (created_at %v); the fixture cannot order the two clocks", bound, at)
		}
		time.Sleep(time.Millisecond)
	}
}

// resumeSplitPlanJSON is a valid standard_v1 plan carrying a 3-phase split.
func resumeSplitPlanJSON(t *testing.T) []byte {
	t.Helper()
	p := &plan.Plan{
		PlanVersion: "standard_v1",
		Summary:     "rename Foo to NewFoo in three phases",
		Scope:       plan.Scope{Files: []plan.ScopeFile{{Path: "backend/internal/foo/foo.go", Operation: plan.FileOpModify}}},
		Verification: plan.Verification{
			TestStrategy: "unit", RollbackPlan: "revert the PR",
			AcceptanceCriteria: []plan.AcceptanceCriterion{{ID: "ac1", Statement: "Foo is gone"}},
		},
		SplitProposal: &plan.SplitProposal{
			Rationale: "scope exceeds the implement cap",
			Phases: []plan.SplitPhase{
				{Title: "expand", ScopeHint: "add NewFoo", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "backend/internal/foo/foo.go", Operation: plan.FileOpModify}}}},
				{Title: "migrate", ScopeHint: "migrate consumers", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "backend/internal/bar/bar.go", Operation: plan.FileOpModify}}}, DependsOn: []int{0}},
				{Title: "contract", ScopeHint: "delete Foo", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "backend/internal/foo/foo.go", Operation: plan.FileOpDelete}}}, DependsOn: []int{1}},
			},
		},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	return b
}
