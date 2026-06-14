package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
)

// recoverRepo extends orchestratorRepo with working CreateRun /
// CreateStage / GetRunByIdempotencyKey so the recovery handler can
// mint its child run against the in-memory store the rest of the
// test (stage reads, prompt render) then consults.
type recoverRepo struct {
	*orchestratorRepo
	createRunErr   error
	createStageErr error
	// Error-injection hooks for the in-place decomposition-child recover
	// branch's post-eligibility failure mappings (#1081 fix-up):
	listStagesErr error // ListStagesForRun fails → handler 500 "list stages failed"
	retryRunErr   error // RedriveChild's RetryRun fails → handler default 500 "re-drive child failed"
	// failGetRunRunning, when set, makes GetRun fail for that id once the
	// run is in StateRunning — i.e. only the FINAL post-redrive re-fetch,
	// since the child is failed on every earlier GetRun. Exercises the
	// "get re-opened child failed" 500 mapping.
	failGetRunRunning uuid.UUID
}

func newRecoverRepo() *recoverRepo {
	return &recoverRepo{orchestratorRepo: newOrchestratorRepo()}
}

func (r *recoverRepo) ListStagesForRun(ctx context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	if r.listStagesErr != nil {
		return nil, r.listStagesErr
	}
	return r.orchestratorRepo.ListStagesForRun(ctx, runID)
}

func (r *recoverRepo) RetryRun(ctx context.Context, id uuid.UUID, to run.State) (*run.Run, error) {
	if r.retryRunErr != nil {
		return nil, r.retryRunErr
	}
	return r.orchestratorRepo.RetryRun(ctx, id, to)
}

func (r *recoverRepo) GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error) {
	rr, err := r.orchestratorRepo.GetRun(ctx, id)
	if err != nil {
		return nil, err
	}
	if r.failGetRunRunning != uuid.Nil && id == r.failGetRunRunning && rr.State == run.StateRunning {
		return nil, errors.New("get run boom")
	}
	return rr, nil
}

func (r *recoverRepo) CreateRun(_ context.Context, p run.CreateRunParams) (*run.Run, error) {
	if r.createRunErr != nil {
		return nil, r.createRunErr
	}
	now := time.Now().UTC()
	runnerKind := p.RunnerKind
	if runnerKind == "" {
		runnerKind = run.RunnerKindGitHubActions
	}
	rr := &run.Run{
		ID:                     uuid.New(),
		Repo:                   p.Repo,
		WorkflowID:             p.WorkflowID,
		WorkflowSHA:            p.WorkflowSHA,
		TriggerSource:          p.TriggerSource,
		TriggerRef:             p.TriggerRef,
		InstallationID:         p.InstallationID,
		IdempotencyKey:         p.IdempotencyKey,
		ParentRunID:            p.ParentRunID,
		RequiredChecksSnapshot: p.RequiredChecksSnapshot,
		WorkflowSpec:           p.WorkflowSpec,
		RetryAttempt:           p.RetryAttempt,
		MaxRetriesSnapshot:     p.MaxRetriesSnapshot,
		RunnerKind:             runnerKind,
		IssueContext:           p.IssueContext,
		State:                  run.StatePending,
		CreatedAt:              now,
		UpdatedAt:              now,
	}
	r.mu.Lock()
	r.runs[rr.ID] = rr
	r.mu.Unlock()
	return rr, nil
}

func (r *recoverRepo) CreateStage(_ context.Context, p run.CreateStageParams) (*run.Stage, error) {
	if r.createStageErr != nil {
		return nil, r.createStageErr
	}
	now := time.Now().UTC()
	st := &run.Stage{
		ID:               uuid.New(),
		RunID:            p.RunID,
		Sequence:         p.Sequence,
		Type:             p.Type,
		ExecutorKind:     p.ExecutorKind,
		ExecutorRef:      p.ExecutorRef,
		State:            run.StageStatePending,
		GateSLA:          p.GateSLA,
		RequiresApproval: p.RequiresApproval,
		Gate:             p.Gate,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	r.mu.Lock()
	r.stagesByID[st.ID] = st
	r.stagesByRunID[p.RunID] = append(r.stagesByRunID[p.RunID], st)
	r.mu.Unlock()
	return st, nil
}

func (r *recoverRepo) GetRunByIdempotencyKey(_ context.Context, repo, key string) (*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rr := range r.runs {
		if rr.Repo == repo && rr.IdempotencyKey != nil && *rr.IdempotencyKey == key {
			return rr, nil
		}
	}
	return nil, run.ErrNotFound
}

// recoverAuditRepo serves pre-seeded entries via feedbackAuditRepo's
// ListForRunByCategory and additionally records AppendChained and
// AppendGlobalChained calls so tests can assert the plan_reused_from
// provenance entry and the budget-gate audit entries.
type recoverAuditRepo struct {
	feedbackAuditRepo
	appended       []audit.ChainAppendParams
	globalAppended []audit.GlobalChainAppendParams
}

func (a *recoverAuditRepo) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	a.appended = append(a.appended, p)
	rid := p.RunID
	return &audit.Entry{ID: uuid.New(), RunID: &rid, Category: p.Category, Payload: p.Payload}, nil
}

func (a *recoverAuditRepo) AppendGlobalChained(_ context.Context, p audit.GlobalChainAppendParams) (*audit.Entry, error) {
	a.globalAppended = append(a.globalAppended, p)
	return &audit.Entry{ID: uuid.New(), Category: p.Category, Payload: p.Payload}, nil
}

func (a *recoverAuditRepo) countGlobal(category string) int {
	n := 0
	for _, p := range a.globalAppended {
		if p.Category == category {
			n++
		}
	}
	return n
}

// budgetRecoverRepo adds the SumWorkflowCostInRange capability
// (webhook.CostSummer) to recoverRepo so the recovery handler's
// blocking-budget admission gate has a cost source to evaluate.
type budgetRecoverRepo struct {
	*recoverRepo
	spent float64
}

func (r *budgetRecoverRepo) SumWorkflowCostInRange(_ context.Context, _, _ string, _, _ time.Time) (float64, error) {
	return r.spent, nil
}

// recoverBudgetSpecYAML mirrors gatedSpecYAML's plan+implement shape
// with one weekly blocking budget (limit $50) so recovery tests can
// drive the admission gate.
const recoverBudgetSpecYAML = `version: "0.4"
workflows:
  feature_change:
    budgets:
      - period: weekly
        limit_usd: 50
        enforcement: blocking
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`

// planOnlySpecYAML defines a workflow with no non-plan stages, so a
// recovery against it has nothing to re-create.
const planOnlySpecYAML = `version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
`

// seedRecoverableParent seeds a parent run with a cached workflow
// spec, a succeeded plan stage, and an implement stage failed with
// the given category. Returns the parent run plus its two stages.
func seedRecoverableParent(rr *recoverRepo, implementState run.StageState, cat *run.FailureCategory) (*run.Run, *run.Stage, *run.Stage) {
	parent := rr.seedRun()
	parent.WorkflowID = "feature_change"
	parent.WorkflowSpec = []byte(gatedSpecYAML)
	planStage := rr.seedStage(parent.ID, 0, run.StageStateSucceeded)
	implStage := rr.seedStage(parent.ID, 1, implementState)
	implStage.Type = run.StageTypeImplement
	implStage.FailureCategory = cat
	return parent, planStage, implStage
}

func failureCat(c run.FailureCategory) *run.FailureCategory { return &c }

func postRecover(t *testing.T, s *Server, pathRunID string, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/v0/runs/"+pathRunID+"/recover", strings.NewReader(body))
	req.SetPathValue("run_id", pathRunID)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req = withAuth(req)
	w := httptest.NewRecorder()
	s.handleRecoverRun(w, req)
	return w
}

func newRecoverServer(t *testing.T) (*Server, *recoverRepo, *fakeScopeAmendmentRepo, *recoverAuditRepo) {
	t.Helper()
	rr := newRecoverRepo()
	sa := newFakeScopeAmendmentRepo()
	au := &recoverAuditRepo{}
	s := New(Config{
		Addr:               "127.0.0.1:0",
		RunRepo:            rr,
		ScopeAmendmentRepo: sa,
		AuditRepo:          au,
	})
	return s, rr, sa, au
}

func TestRecoverRun_HappyPath(t *testing.T) {
	s, rr, sa, au := newRecoverServer(t)
	parent, _, _ := seedRecoverableParent(rr, run.StageStateFailed, failureCat(run.FailureB))
	parent.RetryAttempt = 1

	w := postRecover(t, s, parent.ID.String(),
		`{"add_scope_files":[{"path":"docs/extra.md"},{"path":"backend/new_file.go","operation":"create"}],"reason":"runner dropped the doc companion"}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var resp runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ParentRunID == nil || *resp.ParentRunID != parent.ID {
		t.Errorf("ParentRunID = %v, want %s", resp.ParentRunID, parent.ID)
	}
	if resp.RetryAttempt != parent.RetryAttempt {
		t.Errorf("RetryAttempt = %d, want %d (carried UNCHANGED — recovery must not consume the on_ci_failure cap)",
			resp.RetryAttempt, parent.RetryAttempt)
	}

	// Exactly the non-plan stages were created on the child.
	childStages, err := rr.ListStagesForRun(context.Background(), resp.ID)
	if err != nil {
		t.Fatalf("list child stages: %v", err)
	}
	if len(childStages) != 1 || childStages[0].Type != run.StageTypeImplement {
		t.Fatalf("child stages = %+v, want exactly one implement stage", childStages)
	}
	childImplement := childStages[0]

	// The operator's paths landed as an APPROVED amendment row on the
	// child implement stage, operation defaulted to modify.
	amendments, err := sa.ListByRun(context.Background(), resp.ID)
	if err != nil {
		t.Fatalf("list amendments: %v", err)
	}
	if len(amendments) != 1 {
		t.Fatalf("amendments = %d, want 1", len(amendments))
	}
	a := amendments[0]
	if a.StageID != childImplement.ID {
		t.Errorf("amendment StageID = %s, want child implement %s", a.StageID, childImplement.ID)
	}
	if a.Status != scopeamendment.StatusApproved {
		t.Errorf("amendment Status = %q, want approved", a.Status)
	}
	wantPaths := []scopeamendment.PathEntry{
		{Path: "docs/extra.md", Operation: scopeamendment.OperationModify},
		{Path: "backend/new_file.go", Operation: scopeamendment.OperationCreate},
	}
	if len(a.Paths) != len(wantPaths) {
		t.Fatalf("amendment Paths = %+v, want %+v", a.Paths, wantPaths)
	}
	for i, want := range wantPaths {
		if a.Paths[i] != want {
			t.Errorf("amendment Paths[%d] = %+v, want %+v", i, a.Paths[i], want)
		}
	}

	// Exactly one plan_reused_from provenance entry on the child, and
	// no scope_amendment_* entries from the recovery path (the audit
	// emission stays with the HTTP amendment handlers).
	var reused []audit.ChainAppendParams
	for _, p := range au.appended {
		if strings.HasPrefix(p.Category, "scope_amendment") {
			t.Errorf("unexpected %s audit entry from the recovery path", p.Category)
		}
		if p.Category == CategoryPlanReusedFrom {
			reused = append(reused, p)
		}
	}
	if len(reused) != 1 {
		t.Fatalf("plan_reused_from entries = %d, want 1", len(reused))
	}
	if reused[0].RunID != resp.ID {
		t.Errorf("plan_reused_from RunID = %s, want child %s", reused[0].RunID, resp.ID)
	}
	var payload struct {
		ParentRunID           string                     `json:"parent_run_id"`
		ParentFailureCategory string                     `json:"parent_failure_category"`
		AddedPaths            []scopeamendment.PathEntry `json:"added_paths"`
		Source                string                     `json:"source"`
	}
	if err := json.Unmarshal(reused[0].Payload, &payload); err != nil {
		t.Fatalf("decode plan_reused_from payload: %v", err)
	}
	if payload.ParentRunID != parent.ID.String() {
		t.Errorf("payload parent_run_id = %q, want %q", payload.ParentRunID, parent.ID)
	}
	if payload.ParentFailureCategory != "B" || payload.Source != "operator_recovery" {
		t.Errorf("payload category/source = %q/%q, want B/operator_recovery",
			payload.ParentFailureCategory, payload.Source)
	}
	if len(payload.AddedPaths) != 2 || payload.AddedPaths[0].Path != "docs/extra.md" {
		t.Errorf("payload added_paths = %+v, want the two operator paths", payload.AddedPaths)
	}
}

// TestRecoverRun_PromptRenderCrossesAllSeams is the required
// cross-boundary integration test (#618 discipline): HTTP recover
// handler → run/stage persistence → scope-amendment store → prompt
// construction. After the recovery POST, the child implement stage's
// prompt-render must carry (a) the parent plan's scope.files —
// loadApprovedPlanForRun's parent walk served the reused plan — (b)
// the operator's add_scope_files paths — the pre-approved amendment
// folded via mergeApprovedScopeAmendments — and (c) the parent's
// approval-conditions text — the step-6 ParentRunID fallback.
// Per-layer units would pass while any of these seams broke.
func TestRecoverRun_PromptRenderCrossesAllSeams(t *testing.T) {
	const plannedFile = "backend/internal/server/handlers.go"
	const operatorFile = "docs/recovered-extra.md"
	const parentCondition = "Keep the recover endpoint idempotent; do NOT touch the dispatcher."

	rr := newRecoverRepo()
	sa := newFakeScopeAmendmentRepo()
	art := newFakeArtifactRepo()

	parent, planStage, _ := seedRecoverableParent(rr, run.StageStateFailed, failureCat(run.FailureB))

	// The parent's approved plan artifact on its plan stage.
	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "recoverable plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{
			Files: []plan.ScopeFile{{Path: plannedFile, Operation: plan.FileOpModify}},
		},
	}
	planBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       planStage.ID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	// The parent's binding approve-with-conditions entry.
	au := &recoverAuditRepo{feedbackAuditRepo: feedbackAuditRepo{
		byRunID: map[uuid.UUID][]*audit.Entry{
			parent.ID: {makeApproveWithCommentEntry(parent.ID, parentCondition)},
		},
	}}

	s := New(Config{
		Addr:               "127.0.0.1:0",
		RunRepo:            rr,
		ScopeAmendmentRepo: sa,
		AuditRepo:          au,
		ArtifactRepo:       art,
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	w := postRecover(t, s, parent.ID.String(),
		`{"add_scope_files":[{"path":"`+operatorFile+`"}],"reason":"fold the dropped doc"}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("recover status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var created runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode recover response: %v", err)
	}
	childStages, err := rr.ListStagesForRun(context.Background(), created.ID)
	if err != nil || len(childStages) != 1 {
		t.Fatalf("child stages = %v (err %v), want exactly one", childStages, err)
	}
	childImplement := childStages[0]

	req := httptest.NewRequest(http.MethodGet,
		"/v0/stages/"+childImplement.ID.String()+"/prompt-render", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("prompt-render status = %d, want 200:\n%s", rec.Code, rec.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode prompt-render: %v", err)
	}

	got := make(map[string]bool, len(resp.ScopeFiles))
	for _, f := range resp.ScopeFiles {
		got[f.Path] = true
	}
	if !got[plannedFile] {
		t.Errorf("ScopeFiles missing the parent plan's %q (parent-walk plan reuse broke); got %#v",
			plannedFile, resp.ScopeFiles)
	}
	if !got[operatorFile] {
		t.Errorf("ScopeFiles missing the operator's %q (pre-approved amendment fold broke); got %#v",
			operatorFile, resp.ScopeFiles)
	}
	if !strings.Contains(resp.Prompt, parentCondition) {
		t.Errorf("prompt missing the parent's approval-conditions text %q (ParentRunID fallback broke)\n---\n%s",
			parentCondition, resp.Prompt)
	}
}

// seedRecoverableDecompositionChild seeds a parent run carrying an
// approved plan artifact on its plan stage, plus a decomposition child
// (DecomposedFrom + ParentRunID = parent) whose own implement stage
// failed with the given category. The child is left in run.StateFailed.
// Returns the child run and its failed implement stage.
func seedRecoverableDecompositionChild(t *testing.T, rr *recoverRepo, art *fakeArtifactRepo, cat *run.FailureCategory) (*run.Run, *run.Stage) {
	t.Helper()
	parent := rr.seedRun()
	parent.WorkflowID = "feature_change"
	parent.WorkflowSpec = []byte(gatedSpecYAML)
	planStage := rr.seedStage(parent.ID, 0, run.StageStateSucceeded)

	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "decomposition child plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{
			Files: []plan.ScopeFile{{Path: "backend/internal/server/handlers.go", Operation: plan.FileOpModify}},
		},
	}
	planBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       planStage.ID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	child := rr.seedRun()
	child.WorkflowID = "feature_change"
	child.WorkflowSpec = []byte(gatedSpecYAML)
	child.State = run.StateFailed
	pid := parent.ID
	child.ParentRunID = &pid
	child.DecomposedFrom = &pid
	implStage := rr.seedStage(child.ID, 0, run.StageStateFailed)
	implStage.Type = run.StageTypeImplement
	implStage.FailureCategory = cat
	reason := "scope violation: edited an out-of-scope file"
	implStage.FailureReason = &reason
	return child, implStage
}

func newDecompositionRecoverServer(t *testing.T) (*Server, *recoverRepo, *fakeScopeAmendmentRepo, *recoverAuditRepo, *fakeArtifactRepo) {
	t.Helper()
	rr := newRecoverRepo()
	sa := newFakeScopeAmendmentRepo()
	au := &recoverAuditRepo{}
	art := newFakeArtifactRepo()
	s := New(Config{
		Addr:               "127.0.0.1:0",
		RunRepo:            rr,
		ScopeAmendmentRepo: sa,
		AuditRepo:          au,
		ArtifactRepo:       art,
	})
	return s, rr, sa, au, art
}

// TestRecoverRun_DecompositionChildInPlace covers the slice-2 branch:
// pointing recover at a failed decomposition CHILD re-opens that child
// in place (same id, no new run minted) against the parent-walked plan,
// folding add_scope_files onto the EXISTING implement stage and emitting
// a decomposition_child_recovery provenance entry.
func TestRecoverRun_DecompositionChildInPlace(t *testing.T) {
	s, rr, sa, au, art := newDecompositionRecoverServer(t)
	child, implStage := seedRecoverableDecompositionChild(t, rr, art, failureCat(run.FailureB))

	w := postRecover(t, s, child.ID.String(),
		`{"add_scope_files":[{"path":"docs/extra.md"},{"path":"backend/new.go","operation":"create"}],"reason":"fold the dropped companion"}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var resp runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// In place: same run id, and only the parent + the re-opened child
	// exist — no second DecomposedFrom row was minted.
	if resp.ID != child.ID {
		t.Errorf("re-drive minted a new run %s, want in-place %s", resp.ID, child.ID)
	}
	if resp.State != string(run.StateRunning) {
		t.Errorf("child state = %q, want running (re-opened)", resp.State)
	}
	rows, _ := rr.ListRuns(context.Background(), run.ListRunsFilter{})
	if len(rows) != 2 {
		t.Errorf("runs = %d, want 2 (parent + the re-opened child, no new run)", len(rows))
	}

	// The existing implement stage was re-opened failed → pending.
	reopened, err := rr.GetStage(context.Background(), implStage.ID)
	if err != nil {
		t.Fatalf("get implement stage: %v", err)
	}
	if reopened.State != run.StageStatePending {
		t.Errorf("implement stage state = %q, want pending", reopened.State)
	}

	// The operator's paths landed APPROVED on the EXISTING implement
	// stage id (so mergeApprovedScopeAmendments, keyed by run+stage id,
	// folds them on the re-driven prompt).
	amendments, err := sa.ListByRun(context.Background(), child.ID)
	if err != nil {
		t.Fatalf("list amendments: %v", err)
	}
	if len(amendments) != 1 {
		t.Fatalf("amendments = %d, want 1", len(amendments))
	}
	if amendments[0].StageID != implStage.ID {
		t.Errorf("amendment StageID = %s, want existing implement %s", amendments[0].StageID, implStage.ID)
	}
	if amendments[0].Status != scopeamendment.StatusApproved {
		t.Errorf("amendment status = %q, want approved", amendments[0].Status)
	}

	// Exactly one plan_reused_from with the decomposition source.
	var reused *audit.ChainAppendParams
	for i := range au.appended {
		if au.appended[i].Category == CategoryPlanReusedFrom {
			reused = &au.appended[i]
		}
	}
	if reused == nil {
		t.Fatalf("no plan_reused_from audit entry")
	}
	if reused.RunID != child.ID {
		t.Errorf("plan_reused_from RunID = %s, want re-opened child %s", reused.RunID, child.ID)
	}
	var payload struct {
		ParentRunID string                     `json:"parent_run_id"`
		Source      string                     `json:"source"`
		AddedPaths  []scopeamendment.PathEntry `json:"added_paths"`
	}
	if err := json.Unmarshal(reused.Payload, &payload); err != nil {
		t.Fatalf("decode plan_reused_from payload: %v", err)
	}
	if payload.Source != "decomposition_child_recovery" {
		t.Errorf("source = %q, want decomposition_child_recovery", payload.Source)
	}
	if payload.ParentRunID != child.DecomposedFrom.String() {
		t.Errorf("parent_run_id = %q, want parent %q", payload.ParentRunID, child.DecomposedFrom)
	}
	if len(payload.AddedPaths) != 2 {
		t.Errorf("added_paths = %+v, want the two operator paths", payload.AddedPaths)
	}
}

// TestRecoverRun_DecompositionChildNoAmendment confirms the branch
// recovers with an empty body (no add_scope_files) — the amendment is
// optional; the re-drive against the parent-walked plan is the point.
func TestRecoverRun_DecompositionChildNoAmendment(t *testing.T) {
	s, rr, sa, _, art := newDecompositionRecoverServer(t)
	child, _ := seedRecoverableDecompositionChild(t, rr, art, failureCat(run.FailureB))

	w := postRecover(t, s, child.ID.String(), `{}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	amendments, _ := sa.ListByRun(context.Background(), child.ID)
	if len(amendments) != 0 {
		t.Errorf("amendments = %d, want 0 (none requested)", len(amendments))
	}
}

// TestRecoverRun_DecompositionChildGateMatrix exercises the two
// eligibility legs of the in-place branch: the child's own implement
// must be failed category-B, and its plan must resolve via the parent
// walk. Anything else is recovery_not_eligible with no re-drive.
func TestRecoverRun_DecompositionChildGateMatrix(t *testing.T) {
	t.Run("category A child not eligible", func(t *testing.T) {
		s, rr, _, _, art := newDecompositionRecoverServer(t)
		child, implStage := seedRecoverableDecompositionChild(t, rr, art, failureCat(run.FailureA))

		w := postRecover(t, s, child.ID.String(), `{}`, nil)
		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
		}
		assertErrorCode(t, w, "recovery_not_eligible")
		// No re-drive: the child stays failed.
		got, _ := rr.GetStage(context.Background(), implStage.ID)
		if got.State != run.StageStateFailed {
			t.Errorf("implement stage = %q, want still failed (no re-drive)", got.State)
		}
	})

	t.Run("plan unresolvable not eligible", func(t *testing.T) {
		// No ArtifactRepo → loadApprovedPlanForRun returns nil, so leg 2
		// of eligibility fails even though the child failed category-B.
		rr := newRecoverRepo()
		au := &recoverAuditRepo{}
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
		parent := rr.seedRun()
		child := rr.seedRun()
		child.State = run.StateFailed
		pid := parent.ID
		child.ParentRunID = &pid
		child.DecomposedFrom = &pid
		impl := rr.seedStage(child.ID, 0, run.StageStateFailed)
		impl.Type = run.StageTypeImplement
		impl.FailureCategory = failureCat(run.FailureB)

		w := postRecover(t, s, child.ID.String(), `{}`, nil)
		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
		}
		assertErrorCode(t, w, "recovery_not_eligible")
	})
}

// postRecoverAs posts to the recover endpoint under the given identity
// (instead of the default session-operator from withAuth), so the
// agent-token rejection on the in-place re-drive path can be exercised.
func postRecoverAs(t *testing.T, s *Server, pathRunID string, body string, id Identity) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/v0/runs/"+pathRunID+"/recover", strings.NewReader(body))
	req.SetPathValue("run_id", pathRunID)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
	w := httptest.NewRecorder()
	s.handleRecoverRun(w, req)
	return w
}

// TestRecoverRun_DecompositionChildAgentTokenRejected is the binding
// security guard for the #1081 fix-up: the in-place re-drive branch
// reaches run.RedriveChild — the same operator-only action POST
// /v0/runs/{id}/redrive performs — so an MCP/agent subject-bound token
// (subject mcp:run:<uuid>) must be rejected 403 agent_token_forbidden
// even when it clears the enclosing write:runs gate. The two paths to
// RedriveChild must enforce the identical contract.
func TestRecoverRun_DecompositionChildAgentTokenRejected(t *testing.T) {
	s, rr, sa, _, art := newDecompositionRecoverServer(t)
	child, implStage := seedRecoverableDecompositionChild(t, rr, art, failureCat(run.FailureB))

	// An agent token carrying write:runs (the enclosing gate) is still
	// rejected: re-opening a terminal run is operator-only.
	agent := Identity{
		Subject: "mcp:run:" + child.ID.String(),
		TokenID: "tok-agent",
		Scopes:  []string{"mcp:read", "write:runs"},
	}
	w := postRecoverAs(t, s, child.ID.String(), `{}`, agent)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	assertErrorCode(t, w, "agent_token_forbidden")

	// No re-drive happened: the child's implement stage is still failed,
	// and no amendment row was minted.
	got, _ := rr.GetStage(context.Background(), implStage.ID)
	if got.State != run.StageStateFailed {
		t.Errorf("implement stage = %q, want still failed (no re-drive)", got.State)
	}
	if amendments, _ := sa.ListByRun(context.Background(), child.ID); len(amendments) != 0 {
		t.Errorf("amendments = %d, want 0 (rejected before any fold)", len(amendments))
	}
}

// TestRecoverRun_DecompositionChildErrorMappings covers the in-place
// branch's post-eligibility error legs (#1081 fix-up): the
// ListStagesForRun read, the parent-plan resolve, the RedriveChild
// default error, and the final re-fetch GetRun all map to 500
// internal_error. The eligibility gate makes a RedriveChild failure
// unlikely in practice, but the mappings should not be untested.
func TestRecoverRun_DecompositionChildErrorMappings(t *testing.T) {
	t.Run("list stages error", func(t *testing.T) {
		s, rr, _, _, art := newDecompositionRecoverServer(t)
		child, _ := seedRecoverableDecompositionChild(t, rr, art, failureCat(run.FailureB))
		rr.listStagesErr = errors.New("stage list boom")

		w := postRecover(t, s, child.ID.String(), `{}`, nil)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
		}
		assertErrorCode(t, w, "internal_error")
	})

	t.Run("plan resolve error", func(t *testing.T) {
		s, rr, _, _, art := newDecompositionRecoverServer(t)
		child, _ := seedRecoverableDecompositionChild(t, rr, art, failureCat(run.FailureB))
		// ArtifactRepo present (so it's not the "plan nil → 409" gate leg)
		// but ListForStage errors → loadApprovedPlanForRun returns an error.
		art.listErr = errors.New("artifact list boom")

		w := postRecover(t, s, child.ID.String(), `{}`, nil)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
		}
		assertErrorCode(t, w, "internal_error")
	})

	t.Run("redrive default error", func(t *testing.T) {
		s, rr, _, _, art := newDecompositionRecoverServer(t)
		child, _ := seedRecoverableDecompositionChild(t, rr, art, failureCat(run.FailureB))
		// A plain RetryRun error inside RedriveChild is neither
		// ErrRedriveNotApplicable, ErrNotFound, nor InvalidTransitionError,
		// so it hits the switch default → 500 "re-drive child failed".
		rr.retryRunErr = errors.New("reopen boom")

		w := postRecover(t, s, child.ID.String(), `{}`, nil)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
		}
		assertErrorCode(t, w, "internal_error")
	})

	t.Run("final get run error", func(t *testing.T) {
		s, rr, _, _, art := newDecompositionRecoverServer(t)
		child, _ := seedRecoverableDecompositionChild(t, rr, art, failureCat(run.FailureB))
		// The re-drive succeeds (child → running); only the final re-fetch
		// of the re-opened child fails → 500 "get re-opened child failed".
		rr.failGetRunRunning = child.ID

		w := postRecover(t, s, child.ID.String(), `{}`, nil)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
		}
		assertErrorCode(t, w, "internal_error")
	})
}

func TestRecoverRun_GateMatrix(t *testing.T) {
	tests := []struct {
		name           string
		implementState run.StageState
		category       *run.FailureCategory
		planState      run.StageState
		wantStatus     int
		wantCode       string
	}{
		{"implement failed A", run.StageStateFailed, failureCat(run.FailureA), run.StageStateSucceeded, http.StatusConflict, "recovery_not_eligible"},
		{"implement failed C", run.StageStateFailed, failureCat(run.FailureC), run.StageStateSucceeded, http.StatusConflict, "recovery_not_eligible"},
		{"implement failed D", run.StageStateFailed, failureCat(run.FailureD), run.StageStateSucceeded, http.StatusConflict, "recovery_not_eligible"},
		{"implement succeeded", run.StageStateSucceeded, nil, run.StageStateSucceeded, http.StatusConflict, "recovery_not_eligible"},
		{"implement still running", run.StageStateRunning, nil, run.StageStateSucceeded, http.StatusConflict, "recovery_not_eligible"},
		{"plan not succeeded", run.StageStateFailed, failureCat(run.FailureB), run.StageStateFailed, http.StatusConflict, "recovery_not_eligible"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, rr, _, _ := newRecoverServer(t)
			parent, planStage, _ := seedRecoverableParent(rr, tc.implementState, tc.category)
			planStage.State = tc.planState

			w := postRecover(t, s, parent.ID.String(), `{}`, nil)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d:\n%s", w.Code, tc.wantStatus, w.Body.String())
			}
			assertErrorCode(t, w, tc.wantCode)
		})
	}

	t.Run("run not found", func(t *testing.T) {
		s, _, _, _ := newRecoverServer(t)
		w := postRecover(t, s, uuid.NewString(), `{}`, nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404:\n%s", w.Code, w.Body.String())
		}
		assertErrorCode(t, w, "run_not_found")
	})

	t.Run("nil workflow spec", func(t *testing.T) {
		s, rr, _, _ := newRecoverServer(t)
		parent, _, _ := seedRecoverableParent(rr, run.StageStateFailed, failureCat(run.FailureB))
		parent.WorkflowSpec = nil
		w := postRecover(t, s, parent.ID.String(), `{}`, nil)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
		}
		assertErrorCode(t, w, "recovery_unsupported")
	})

	t.Run("unparseable workflow spec", func(t *testing.T) {
		s, rr, _, _ := newRecoverServer(t)
		parent, _, _ := seedRecoverableParent(rr, run.StageStateFailed, failureCat(run.FailureB))
		parent.WorkflowSpec = []byte("workflows: [unclosed")
		w := postRecover(t, s, parent.ID.String(), `{}`, nil)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
		}
		assertErrorCode(t, w, "recovery_unsupported")
		assertNoRunMinted(t, rr)
	})

	t.Run("workflow_id not in spec", func(t *testing.T) {
		s, rr, _, _ := newRecoverServer(t)
		parent, _, _ := seedRecoverableParent(rr, run.StageStateFailed, failureCat(run.FailureB))
		parent.WorkflowID = "not_in_spec"
		w := postRecover(t, s, parent.ID.String(), `{}`, nil)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
		}
		assertErrorCode(t, w, "recovery_unsupported")
		assertNoRunMinted(t, rr)
	})

	t.Run("no non-plan stages", func(t *testing.T) {
		s, rr, _, _ := newRecoverServer(t)
		parent, _, _ := seedRecoverableParent(rr, run.StageStateFailed, failureCat(run.FailureB))
		parent.WorkflowSpec = []byte(planOnlySpecYAML)
		w := postRecover(t, s, parent.ID.String(), `{}`, nil)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
		}
		assertErrorCode(t, w, "recovery_unsupported")
		assertNoRunMinted(t, rr)
	})

	t.Run("bad paths", func(t *testing.T) {
		s, rr, _, _ := newRecoverServer(t)
		parent, _, _ := seedRecoverableParent(rr, run.StageStateFailed, failureCat(run.FailureB))
		for _, body := range []string{
			`{"add_scope_files":[{"path":"/etc/passwd"}]}`,
			`{"add_scope_files":[{"path":"../escape.go"}]}`,
			`{"add_scope_files":[{"path":"  "}]}`,
		} {
			w := postRecover(t, s, parent.ID.String(), body, nil)
			if w.Code != http.StatusBadRequest {
				t.Errorf("body %s: status = %d, want 400:\n%s", body, w.Code, w.Body.String())
			}
		}
	})

	t.Run("unknown body field", func(t *testing.T) {
		s, rr, _, _ := newRecoverServer(t)
		parent, _, _ := seedRecoverableParent(rr, run.StageStateFailed, failureCat(run.FailureB))
		w := postRecover(t, s, parent.ID.String(), `{"nope":true}`, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
		}
	})

	t.Run("amendment requested with no ScopeAmendmentRepo", func(t *testing.T) {
		rr := newRecoverRepo()
		au := &recoverAuditRepo{}
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
		parent, _, _ := seedRecoverableParent(rr, run.StageStateFailed, failureCat(run.FailureB))
		w := postRecover(t, s, parent.ID.String(),
			`{"add_scope_files":[{"path":"docs/extra.md"}]}`, nil)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
		}
		assertErrorCode(t, w, "scope_amendment_unconfigured")
		// No half-formed run was minted.
		if rows, _ := rr.ListRuns(context.Background(), run.ListRunsFilter{}); len(rows) != 1 {
			t.Errorf("runs = %d, want 1 (the parent only)", len(rows))
		}
	})

	t.Run("no amendment requested works without ScopeAmendmentRepo", func(t *testing.T) {
		rr := newRecoverRepo()
		au := &recoverAuditRepo{}
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
		parent, _, _ := seedRecoverableParent(rr, run.StageStateFailed, failureCat(run.FailureB))
		w := postRecover(t, s, parent.ID.String(), `{}`, nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
		}
	})
}

func TestRecoverRun_IdempotencyKeyReplay(t *testing.T) {
	s, rr, _, _ := newRecoverServer(t)
	parent, _, _ := seedRecoverableParent(rr, run.StageStateFailed, failureCat(run.FailureB))

	headers := map[string]string{"Idempotency-Key": "recover-once"}
	w1 := postRecover(t, s, parent.ID.String(), `{}`, headers)
	if w1.Code != http.StatusCreated {
		t.Fatalf("first call status = %d, want 201:\n%s", w1.Code, w1.Body.String())
	}
	var first runResponse
	if err := json.Unmarshal(w1.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode: %v", err)
	}

	w2 := postRecover(t, s, parent.ID.String(), `{}`, headers)
	if w2.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200:\n%s", w2.Code, w2.Body.String())
	}
	var second runResponse
	if err := json.Unmarshal(w2.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("replay minted a second run: %s vs %s", first.ID, second.ID)
	}
}

func newRecoverBudgetServer(t *testing.T, spent float64) (*Server, *budgetRecoverRepo, *recoverAuditRepo) {
	t.Helper()
	rr := &budgetRecoverRepo{recoverRepo: newRecoverRepo(), spent: spent}
	au := &recoverAuditRepo{}
	s := New(Config{
		Addr:               "127.0.0.1:0",
		RunRepo:            rr,
		ScopeAmendmentRepo: newFakeScopeAmendmentRepo(),
		AuditRepo:          au,
	})
	return s, rr, au
}

// TestRecoverRun_BlockingBudget covers the recovery-specific wiring of
// the #688 admission gate: recovery is new spend, so an exhausted
// blocking budget refuses with 402 (no run minted), and
// budget_override forces past it exactly like POST /v0/runs.
func TestRecoverRun_BlockingBudget(t *testing.T) {
	t.Run("exhausted refused", func(t *testing.T) {
		s, rr, au := newRecoverBudgetServer(t, 100) // over the 50 limit
		parent, _, _ := seedRecoverableParent(rr.recoverRepo, run.StageStateFailed, failureCat(run.FailureB))
		parent.WorkflowSpec = []byte(recoverBudgetSpecYAML)

		w := postRecover(t, s, parent.ID.String(), `{}`, nil)
		if w.Code != http.StatusPaymentRequired {
			t.Fatalf("status = %d, want 402:\n%s", w.Code, w.Body.String())
		}
		assertErrorCode(t, w, "budget_exhausted")
		if n := au.countGlobal("run_rejected_budget"); n != 1 {
			t.Errorf("run_rejected_budget audits = %d, want 1", n)
		}
		assertNoRunMinted(t, rr.recoverRepo)
	})

	t.Run("override admitted", func(t *testing.T) {
		s, rr, au := newRecoverBudgetServer(t, 100)
		parent, _, _ := seedRecoverableParent(rr.recoverRepo, run.StageStateFailed, failureCat(run.FailureB))
		parent.WorkflowSpec = []byte(recoverBudgetSpecYAML)

		w := postRecover(t, s, parent.ID.String(), `{"budget_override":true}`, nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
		}
		var resp runResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.ParentRunID == nil || *resp.ParentRunID != parent.ID {
			t.Errorf("ParentRunID = %v, want %s", resp.ParentRunID, parent.ID)
		}
		if n := au.countGlobal("run_admitted_budget_override"); n != 1 {
			t.Errorf("run_admitted_budget_override audits = %d, want 1", n)
		}
		if n := au.countGlobal("run_rejected_budget"); n != 0 {
			t.Errorf("run_rejected_budget audits = %d, want 0 on override", n)
		}
	})
}

// TestRecoverRun_IdempotencyKeyReplay_BudgetExhaustedBetweenCalls pins
// the replay contract under changed budget state: a successful
// recovery followed by a network retry must return the existing run
// with 200 even when the blocking budget tripped between the two
// calls — replay is honored BEFORE the budget gate because it is not
// new spend.
func TestRecoverRun_IdempotencyKeyReplay_BudgetExhaustedBetweenCalls(t *testing.T) {
	s, rr, au := newRecoverBudgetServer(t, 10) // under the 50 limit
	parent, _, _ := seedRecoverableParent(rr.recoverRepo, run.StageStateFailed, failureCat(run.FailureB))
	parent.WorkflowSpec = []byte(recoverBudgetSpecYAML)

	headers := map[string]string{"Idempotency-Key": "recover-then-exhaust"}
	w1 := postRecover(t, s, parent.ID.String(), `{}`, headers)
	if w1.Code != http.StatusCreated {
		t.Fatalf("first call status = %d, want 201:\n%s", w1.Code, w1.Body.String())
	}
	var first runResponse
	if err := json.Unmarshal(w1.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode: %v", err)
	}

	rr.spent = 100 // budget now blocking

	w2 := postRecover(t, s, parent.ID.String(), `{}`, headers)
	if w2.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200 (replay wins over the budget gate):\n%s",
			w2.Code, w2.Body.String())
	}
	var second runResponse
	if err := json.Unmarshal(w2.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("replay returned a different run: %s vs %s", first.ID, second.ID)
	}
	if n := au.countGlobal("run_rejected_budget"); n != 0 {
		t.Errorf("run_rejected_budget audits = %d, want 0 (replay never reaches the gate)", n)
	}
}

// assertNoRunMinted asserts the store holds only the seeded parent —
// a refusing guard must fire before CreateRun.
func assertNoRunMinted(t *testing.T, rr *recoverRepo) {
	t.Helper()
	rows, err := rr.ListRuns(context.Background(), run.ListRunsFilter{})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("runs = %d, want 1 (the parent only)", len(rows))
	}
}

// assertErrorCode decodes the OpenAPI error envelope and asserts the
// machine code.
func assertErrorCode(t *testing.T, w *httptest.ResponseRecorder, want string) {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v\n%s", err, w.Body.String())
	}
	if env.Error.Code != want {
		t.Errorf("error code = %q, want %q\n%s", env.Error.Code, want, w.Body.String())
	}
}
