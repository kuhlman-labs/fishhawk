package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

// fakeApprovalRepo is the in-memory approval.Repository for handler
// tests. It enforces the same idempotency contract as the postgres
// adapter: a re-submission for the same (stage_id, approver_subject)
// returns the existing row with Inserted=false.
type fakeApprovalRepo struct {
	mu        sync.Mutex
	all       []*approval.Approval
	submitErr error
}

func newFakeApprovalRepo() *fakeApprovalRepo {
	return &fakeApprovalRepo{}
}

func (f *fakeApprovalRepo) Submit(_ context.Context, p approval.SubmitParams) (*approval.SubmitResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.submitErr != nil {
		return nil, f.submitErr
	}
	for _, a := range f.all {
		if a.StageID == p.StageID && a.ApproverSubject == p.ApproverSubject {
			return &approval.SubmitResult{Approval: a, Inserted: false}, nil
		}
	}
	a := &approval.Approval{
		ID:              uuid.New(),
		StageID:         p.StageID,
		ApproverSubject: p.ApproverSubject,
		Decision:        p.Decision,
		Comment:         p.Comment,
		Surface:         p.Surface,
		SubmittedAt:     time.Now().UTC(),
	}
	f.all = append(f.all, a)
	return &approval.SubmitResult{Approval: a, Inserted: true}, nil
}

func (f *fakeApprovalRepo) ListForStage(_ context.Context, stageID uuid.UUID) ([]*approval.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*approval.Approval
	for _, a := range f.all {
		if a.StageID == stageID {
			out = append(out, a)
		}
	}
	return out, nil
}

// approvalRunRepo is a focused run.Repository for the approval
// tests: GetStage returns the seeded stage, TransitionStage records
// the transition.
type approvalRunRepo struct {
	mu             sync.Mutex
	stages         map[uuid.UUID]*run.Stage
	runs           map[uuid.UUID]*run.Run
	getErr         error
	transitionErr  error
	transitions    []approvalTransition
	rejectionFails bool
}

type approvalTransition struct {
	StageID    uuid.UUID
	To         run.StageState
	Completion *run.StageCompletion
}

func newApprovalRunRepo() *approvalRunRepo {
	return &approvalRunRepo{
		stages: map[uuid.UUID]*run.Stage{},
		runs:   map[uuid.UUID]*run.Run{},
	}
}

// seedRun lets tests stand up a *run.Run keyed by id so GetRun can
// surface it. Used by the ADR-018 prune tests to confirm the 409
// body includes the PR URL when one is stamped on the row.
func (r *approvalRunRepo) seedRun(runRow *run.Run) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs[runRow.ID] = runRow
}

func (r *approvalRunRepo) seedStage(state run.StageState) *run.Stage {
	st := &run.Stage{
		ID:           uuid.New(),
		RunID:        uuid.New(),
		Sequence:     0,
		Type:         run.StageTypePlan,
		ExecutorKind: run.ExecutorAgent,
		ExecutorRef:  "claude-code",
		State:        state,
		// Default to gated (RequiresApproval=true) — matches the
		// historical post-trace-upload semantics of every existing
		// test that calls this helper. Use seedGatelessStage when
		// the test specifically wants implement-stage behavior.
		RequiresApproval: true,
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
	}
	r.mu.Lock()
	r.stages[st.ID] = st
	r.mu.Unlock()
	return st
}

// seedGatelessStage seeds a stage whose workflow-spec definition
// has no approval gate — implement-stage semantics. Trace upload
// transitions these straight to succeeded. (#207)
func (r *approvalRunRepo) seedGatelessStage(state run.StageState) *run.Stage {
	st := r.seedStage(state)
	r.mu.Lock()
	st.RequiresApproval = false
	st.Type = run.StageTypeImplement
	r.mu.Unlock()
	return st
}

// seedReviewStage seeds a review-type stage in awaiting_approval.
// ADR-018 / #313: review-stage approval moved to GitHub, so the
// in-Fishhawk approval API refuses these stages. Tests use this
// helper to exercise the new 409 path.
func (r *approvalRunRepo) seedReviewStage() *run.Stage {
	st := r.seedStage(run.StageStateAwaitingApproval)
	r.mu.Lock()
	st.Type = run.StageTypeReview
	r.mu.Unlock()
	return st
}

func (r *approvalRunRepo) GetStage(_ context.Context, id uuid.UUID) (*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getErr != nil {
		return nil, r.getErr
	}
	st, ok := r.stages[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	return st, nil
}

func (r *approvalRunRepo) TransitionStage(_ context.Context, id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.transitionErr != nil {
		return nil, r.transitionErr
	}
	st, ok := r.stages[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	// Mirror postgresRepo: admit the fix-up override edge
	// (awaiting_approval → pending, #762) alongside the normal
	// transitions so the fix-up handler can reuse TransitionStage.
	if !run.ValidStageTransition(st.State, to) && !run.ValidStageFixupTransition(st.State, to) {
		return nil, run.InvalidTransitionError{
			Kind: "stage", From: string(st.State), To: string(to),
		}
	}
	st.State = to
	if c != nil {
		st.FailureCategory = c.FailureCategory
		st.FailureReason = c.FailureReason
	}
	st.UpdatedAt = time.Now().UTC()
	r.transitions = append(r.transitions, approvalTransition{
		StageID: id, To: to, Completion: c,
	})
	return st, nil
}

// Stub the rest of run.Repository — the approval handler doesn't
// touch them.
func (r *approvalRunRepo) CreateRun(context.Context, run.CreateRunParams) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *approvalRunRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rn, ok := r.runs[id]; ok {
		return rn, nil
	}
	return nil, run.ErrNotFound
}
func (r *approvalRunRepo) GetRunByIdempotencyKey(context.Context, string, string) (*run.Run, error) {
	return nil, run.ErrNotFound
}
func (r *approvalRunRepo) ListRuns(context.Context, run.ListRunsFilter) ([]*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *approvalRunRepo) TransitionRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *approvalRunRepo) RetryRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *approvalRunRepo) SetRunPullRequestURL(context.Context, uuid.UUID, string) (*run.Run, error) {
	return nil, errors.New("not used")
}

// AddRunCost satisfies the trace handler's runCostRecorder optional
// capability (#649) so the cost-rollup seam test can assert the per-run
// total accumulates on the seeded run row.
func (r *approvalRunRepo) AddRunCost(_ context.Context, id uuid.UUID, deltaUSD float64, resolvedModel string) (*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rn, ok := r.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	rn.CostUSDTotal += deltaUSD
	if resolvedModel != "" {
		rn.ResolvedModel = resolvedModel
	}
	return rn, nil
}

// SumWorkflowCostInRange satisfies the trace handler's runCostSummer
// optional capability (#688): it sums CostUSDTotal across seeded runs
// matching (repo, workflowID) whose CreatedAt falls in [from, to). Lets
// the advisory-budget seam test seed runs straddling a period boundary
// and assert only the in-period spend is summed.
func (r *approvalRunRepo) SumWorkflowCostInRange(_ context.Context, repo, workflowID string, from, to time.Time) (float64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var total float64
	for _, rn := range r.runs {
		if rn.Repo != repo || rn.WorkflowID != workflowID {
			continue
		}
		if rn.CreatedAt.Before(from) || !rn.CreatedAt.Before(to) {
			continue
		}
		total += rn.CostUSDTotal
	}
	return total, nil
}
func (r *approvalRunRepo) CreateStage(context.Context, run.CreateStageParams) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *approvalRunRepo) ListStagesForRun(context.Context, uuid.UUID) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *approvalRunRepo) ListStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *approvalRunRepo) ListReviewStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}

func (r *approvalRunRepo) ListStagesAwaitingChildren(context.Context) ([]*run.Stage, error) {
	return nil, nil
}

func (r *approvalRunRepo) ListStagesDispatched(context.Context) ([]*run.Stage, error) {
	return nil, nil
}

// RetryStage mirrors postgresRepo: validates the retry-only
// transition table and clears the stage's failure metadata so the
// retry handler tests can drive the full happy-path flow.
func (r *approvalRunRepo) RetryStage(_ context.Context, id uuid.UUID, to run.StageState) (*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.transitionErr != nil {
		return nil, r.transitionErr
	}
	st, ok := r.stages[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	if !run.ValidStageRetryTransition(st.State, to) {
		return nil, run.InvalidTransitionError{Kind: "stage", From: string(st.State), To: string(to)}
	}
	st.State = to
	st.FailureCategory = nil
	st.FailureReason = nil
	st.EndedAt = nil
	r.transitions = append(r.transitions, approvalTransition{
		StageID: id,
		To:      to,
	})
	return st, nil
}

// approvalAuditFake records AppendChained calls so tests assert
// audit-entry shape and category.
type approvalAuditFake struct {
	mu        sync.Mutex
	appended  []audit.ChainAppendParams
	appendErr error
}

func newApprovalAuditFake() *approvalAuditFake { return &approvalAuditFake{} }

func (a *approvalAuditFake) Append(context.Context, audit.AppendParams) (*audit.Entry, error) {
	return nil, errors.New("not used")
}

func (a *approvalAuditFake) ChainsByParent(_ context.Context, _ uuid.UUID, _ bool) ([]*audit.Entry, error) {
	return nil, nil
}
func (a *approvalAuditFake) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.appendErr != nil {
		return nil, a.appendErr
	}
	a.appended = append(a.appended, p)
	rid := p.RunID
	return &audit.Entry{ID: uuid.New(), RunID: &rid}, nil
}

func (a *approvalAuditFake) AppendGlobalChained(context.Context, audit.GlobalChainAppendParams) (*audit.Entry, error) {
	return nil, errors.New("not used")
}

func (a *approvalAuditFake) ListGlobal(context.Context) ([]*audit.Entry, error) {
	return nil, nil
}
func (a *approvalAuditFake) ListAll(context.Context, audit.ListAllParams) ([]*audit.Entry, error) {
	return nil, nil
}
func (a *approvalAuditFake) Get(context.Context, uuid.UUID) (*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (a *approvalAuditFake) ListForRun(context.Context, uuid.UUID) ([]*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (a *approvalAuditFake) LastForRun(context.Context, uuid.UUID) (*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (a *approvalAuditFake) ListForRunByCategory(context.Context, uuid.UUID, string) ([]*audit.Entry, error) {
	return nil, errors.New("not used")
}

// newApprovalServer builds a Server wired to all three fakes,
// returning each so tests can assert on captured state.
func newApprovalServer(t *testing.T) (*Server, *fakeApprovalRepo, *approvalRunRepo, *approvalAuditFake) {
	t.Helper()
	ar := newFakeApprovalRepo()
	rr := newApprovalRunRepo()
	au := newApprovalAuditFake()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		ApprovalRepo: ar,
		RunRepo:      rr,
		AuditRepo:    au,
	})
	return s, ar, rr, au
}

func submitApproval(t *testing.T, s *Server, stageID uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	url := fmt.Sprintf("/v0/stages/%s/approvals", stageID)
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("stage_id", stageID.String())
	w := httptest.NewRecorder()
	s.handleSubmitApproval(w, withAuth(req))
	return w
}

func TestSubmitApproval_Approve_AdvancesStage(t *testing.T) {
	s, ar, rr, au := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	var got stageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.State != string(run.StageStateSucceeded) {
		t.Errorf("State = %q, want succeeded", got.State)
	}
	if len(ar.all) != 1 {
		t.Errorf("approvals = %d, want 1", len(ar.all))
	}
	if ar.all[0].Decision != approval.DecisionApprove {
		t.Errorf("Decision = %q", ar.all[0].Decision)
	}
	if len(rr.transitions) != 1 || rr.transitions[0].To != run.StageStateSucceeded {
		t.Errorf("transitions = %+v", rr.transitions)
	}
	if len(au.appended) != 1 || au.appended[0].Category != "approval_submitted" {
		t.Errorf("audit = %+v", au.appended)
	}
}

func TestSubmitApproval_Reject_FailsCategoryD(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)

	w := submitApproval(t, s, stage.ID, `{"decision":"reject","comment":"plan looks risky"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	var got stageResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.State != string(run.StageStateFailed) {
		t.Errorf("State = %q, want failed", got.State)
	}
	if got.FailureCategory == nil || *got.FailureCategory != "D" {
		t.Errorf("FailureCategory = %v, want D", got.FailureCategory)
	}
	if len(rr.transitions) != 1 {
		t.Fatalf("transitions = %d, want 1", len(rr.transitions))
	}
	tr := rr.transitions[0]
	if tr.Completion == nil || tr.Completion.FailureCategory == nil ||
		*tr.Completion.FailureCategory != run.FailureD {
		t.Errorf("transition completion = %+v", tr.Completion)
	}
	if len(au.appended) != 1 {
		t.Errorf("audit entries = %d, want 1", len(au.appended))
	}
}

func TestSubmitApproval_BadDecision(t *testing.T) {
	s, _, rr, _ := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)

	w := submitApproval(t, s, stage.ID, `{"decision":"maybe"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"validation_failed"`) {
		t.Errorf("body missing validation_failed: %s", w.Body.String())
	}
}

func TestSubmitApproval_BadJSON(t *testing.T) {
	s, _, rr, _ := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)
	w := submitApproval(t, s, stage.ID, `{not json`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestSubmitApproval_BadUUID(t *testing.T) {
	s, _, _, _ := newApprovalServer(t)
	req := httptest.NewRequest(http.MethodPost,
		"/v0/stages/not-a-uuid/approvals",
		strings.NewReader(`{"decision":"approve"}`))
	req.SetPathValue("stage_id", "not-a-uuid")
	w := httptest.NewRecorder()
	s.handleSubmitApproval(w, withAuth(req))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestSubmitApproval_StageNotFound(t *testing.T) {
	s, _, _, _ := newApprovalServer(t)
	w := submitApproval(t, s, uuid.New(), `{"decision":"approve"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestSubmitApproval_Idempotent_SameApprover(t *testing.T) {
	// Second submission from the same approver returns 200 with
	// the prior decision; no second transition, no second audit
	// entry. First decision wins.
	s, _, rr, au := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)

	if w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`); w.Code != http.StatusOK {
		t.Fatalf("first status = %d", w.Code)
	}
	w := submitApproval(t, s, stage.ID, `{"decision":"reject"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200 (idempotent)", w.Code)
	}
	if len(rr.transitions) != 1 {
		t.Errorf("transitions = %d, want 1 (no second transition on idempotent submit)", len(rr.transitions))
	}
	if len(au.appended) != 1 {
		t.Errorf("audit = %d, want 1 (no second audit on idempotent submit)", len(au.appended))
	}
}

func TestSubmitApproval_ReviewStage_Refused(t *testing.T) {
	// ADR-018 / #313: the in-Fishhawk approval API rejects review-
	// stage submissions and points the caller at the PR. Plan-stage
	// approvals are unaffected (covered by TestSubmitApproval_Approve_AdvancesStage).
	s, _, rr, au := newApprovalServer(t)
	stage := rr.seedReviewStage()
	prURL := "https://github.com/x/y/pull/42"
	rr.seedRun(&run.Run{ID: stage.RunID, PullRequestURL: &prURL})

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"review_stage_managed_by_github"`) {
		t.Errorf("error code missing: %s", body)
	}
	if !strings.Contains(body, prURL) {
		t.Errorf("body should include the PR URL: %s", body)
	}
	// No stage transition + no audit row — the prune is purely a
	// guard that runs before submit.
	if len(rr.transitions) != 0 {
		t.Errorf("transitions = %d, want 0 (refused before submit)", len(rr.transitions))
	}
	if len(au.appended) != 0 {
		t.Errorf("audit rows = %d, want 0 (refused before submit)", len(au.appended))
	}
}

func TestSubmitApproval_ReviewStage_RefusesRejectToo(t *testing.T) {
	// Reject submissions against review stages are refused the same
	// way as approves — the surface is gone, not the verb.
	s, _, rr, _ := newApprovalServer(t)
	stage := rr.seedReviewStage()

	w := submitApproval(t, s, stage.ID, `{"decision":"reject"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"review_stage_managed_by_github"`) {
		t.Errorf("error code missing: %s", w.Body.String())
	}
}

func TestSubmitApproval_ReviewStage_RefusesEvenWithoutPRURL(t *testing.T) {
	// When the run row has no PullRequestURL stamped (legacy or
	// pre-PR-open state), the 409 still fires — the body just
	// omits the PR URL detail.
	s, _, rr, _ := newApprovalServer(t)
	stage := rr.seedReviewStage()
	// Intentionally not seeding the run row; GetRun returns ErrNotFound.

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"review_stage_managed_by_github"`) {
		t.Errorf("error code missing: %s", w.Body.String())
	}
}

func TestSubmitApproval_TerminalStage_Conflict(t *testing.T) {
	// A stage already in a terminal state can't transition to a
	// different terminal state. Reject-after-succeeded is the
	// canonical case: the gate already passed, but a late
	// approver tries to flip it.
	s, _, rr, _ := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateSucceeded)

	w := submitApproval(t, s, stage.ID, `{"decision":"reject"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"invalid_state_transition"`) {
		t.Errorf("body missing invalid_state_transition: %s", w.Body.String())
	}
}

func TestSubmitApproval_TerminalStage_SameDecisionIdempotent(t *testing.T) {
	// approve-after-succeeded is a same-state transition the
	// state machine treats as idempotent; the handler returns
	// 200 with the unchanged stage.
	s, _, rr, _ := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateSucceeded)

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (same-state idempotent): %s", w.Code, w.Body.String())
	}
}

func TestSubmitApproval_RepoSubmitError(t *testing.T) {
	s, ar, rr, _ := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)
	ar.submitErr = errors.New("db down")

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestSubmitApproval_TransitionError_Internal(t *testing.T) {
	s, _, rr, _ := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)
	rr.transitionErr = errors.New("db locked")

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestSubmitApproval_NilDeps_503(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"missing approval", Config{Addr: "127.0.0.1:0", RunRepo: newApprovalRunRepo(), AuditRepo: newApprovalAuditFake()}},
		{"missing run", Config{Addr: "127.0.0.1:0", ApprovalRepo: newFakeApprovalRepo(), AuditRepo: newApprovalAuditFake()}},
		{"missing audit", Config{Addr: "127.0.0.1:0", ApprovalRepo: newFakeApprovalRepo(), RunRepo: newApprovalRunRepo()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(tc.cfg)
			w := submitApproval(t, s, uuid.New(), `{"decision":"approve"}`)
			if w.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", w.Code)
			}
		})
	}
}

// orchestratorRepo is a run.Repository that supports both the
// approval handler (GetStage, TransitionStage) AND the orchestrator
// (GetRun, ListStagesForRun, TransitionRun, TransitionStage). Built
// inline so the orchestrator-on-approval tests can run end-to-end
// without interface gymnastics.
type orchestratorRepo struct {
	mu            sync.Mutex
	runs          map[uuid.UUID]*run.Run
	stagesByID    map[uuid.UUID]*run.Stage
	stagesByRunID map[uuid.UUID][]*run.Stage
	// addRunCostDeltas records every AddRunCost delta so the reviewer
	// cost-rollup seam test (#681) can assert the rollup was actually
	// driven with a non-zero delta — not silently skipped because the
	// RunRepo failed to satisfy runCostRecorder (the #647-fixture trap).
	addRunCostDeltas []float64
}

func newOrchestratorRepo() *orchestratorRepo {
	return &orchestratorRepo{
		runs:          map[uuid.UUID]*run.Run{},
		stagesByID:    map[uuid.UUID]*run.Stage{},
		stagesByRunID: map[uuid.UUID][]*run.Stage{},
	}
}

func (r *orchestratorRepo) seedRun() *run.Run {
	id := uuid.New()
	rr := &run.Run{
		ID: id, Repo: "x/y", WorkflowID: "w", WorkflowSHA: "s",
		TriggerSource: run.TriggerCLI, State: run.StateRunning,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	r.mu.Lock()
	r.runs[id] = rr
	r.mu.Unlock()
	return rr
}

func (r *orchestratorRepo) seedStage(runID uuid.UUID, seq int, state run.StageState) *run.Stage {
	st := &run.Stage{
		ID: uuid.New(), RunID: runID, Sequence: seq,
		Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
		State: state, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	r.mu.Lock()
	r.stagesByID[st.ID] = st
	r.stagesByRunID[runID] = append(r.stagesByRunID[runID], st)
	r.mu.Unlock()
	return st
}

func (r *orchestratorRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rr, ok := r.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	return rr, nil
}

// AddRunCost satisfies the trace handler's runCostRecorder optional
// capability (#649/#681) so the reviewer cost-rollup seam test can assert
// the per-run total accumulates AND that AddRunCost was genuinely called
// with a non-zero delta (the binding #647-fixture trap: a RunRepo that
// doesn't implement this would let recordReviewerCost silently skip the
// rollup, passing the assertion vacuously).
func (r *orchestratorRepo) AddRunCost(_ context.Context, id uuid.UUID, deltaUSD float64, resolvedModel string) (*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addRunCostDeltas = append(r.addRunCostDeltas, deltaUSD)
	rr, ok := r.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	rr.CostUSDTotal += deltaUSD
	if resolvedModel != "" {
		rr.ResolvedModel = resolvedModel
	}
	return rr, nil
}

func (r *orchestratorRepo) GetRunByIdempotencyKey(context.Context, string, string) (*run.Run, error) {
	return nil, run.ErrNotFound
}

func (r *orchestratorRepo) ListStagesForRun(_ context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stagesByRunID[runID], nil
}

func (r *orchestratorRepo) ListStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, nil
}

func (r *orchestratorRepo) ListReviewStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, nil
}

func (r *orchestratorRepo) ListStagesAwaitingChildren(context.Context) ([]*run.Stage, error) {
	return nil, nil
}

func (r *orchestratorRepo) ListStagesDispatched(context.Context) ([]*run.Stage, error) {
	return nil, nil
}

// RetryStage validates against the retry-only transition table
// and clears failure metadata + ended_at. Mirrors postgresRepo so
// the retry handler tests in retry_test.go can drive the full
// orchestrator handoff.
func (r *orchestratorRepo) RetryStage(_ context.Context, id uuid.UUID, to run.StageState) (*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.stagesByID[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	if !run.ValidStageRetryTransition(st.State, to) {
		return nil, run.InvalidTransitionError{Kind: "stage", From: string(st.State), To: string(to)}
	}
	st.State = to
	st.FailureCategory = nil
	st.FailureReason = nil
	st.EndedAt = nil
	return st, nil
}

func (r *orchestratorRepo) GetStage(_ context.Context, id uuid.UUID) (*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.stagesByID[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	return s, nil
}

func (r *orchestratorRepo) TransitionRun(_ context.Context, id uuid.UUID, to run.State) (*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rr := r.runs[id]
	if rr == nil {
		return nil, run.ErrNotFound
	}
	if !run.ValidRunTransition(rr.State, to) {
		return nil, run.InvalidTransitionError{Kind: "run", From: string(rr.State), To: string(to)}
	}
	rr.State = to
	return rr, nil
}

// RetryRun mirrors postgresRepo's run-level reopen override (#698):
// only failed → running is permitted. The redrive integration test
// drives the full handler → RedriveChild → RetryRun → orchestrator
// seam through this fake.
func (r *orchestratorRepo) RetryRun(_ context.Context, id uuid.UUID, to run.State) (*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rr := r.runs[id]
	if rr == nil {
		return nil, run.ErrNotFound
	}
	if !run.ValidRunRetryTransition(rr.State, to) {
		return nil, run.InvalidTransitionError{Kind: "run", From: string(rr.State), To: string(to)}
	}
	rr.State = to
	return rr, nil
}

func (r *orchestratorRepo) SetRunPullRequestURL(_ context.Context, id uuid.UUID, url string) (*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rr, ok := r.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	u := url
	rr.PullRequestURL = &u
	return rr, nil
}

func (r *orchestratorRepo) TransitionStage(_ context.Context, id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.stagesByID[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	if !run.ValidStageTransition(s.State, to) {
		return nil, run.InvalidTransitionError{Kind: "stage", From: string(s.State), To: string(to)}
	}
	s.State = to
	if c != nil {
		s.FailureCategory = c.FailureCategory
		s.FailureReason = c.FailureReason
	}
	return s, nil
}

// Unused.
func (r *orchestratorRepo) CreateRun(context.Context, run.CreateRunParams) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *orchestratorRepo) ListRuns(_ context.Context, f run.ListRunsFilter) ([]*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*run.Run, 0, len(r.runs))
	for _, rr := range r.runs {
		if f.Repo != "" && rr.Repo != f.Repo {
			continue
		}
		if f.WorkflowID != "" && rr.WorkflowID != f.WorkflowID {
			continue
		}
		if f.State != "" && string(rr.State) != f.State {
			continue
		}
		if f.DecomposedFrom != nil && (rr.DecomposedFrom == nil || *rr.DecomposedFrom != *f.DecomposedFrom) {
			continue
		}
		out = append(out, rr)
	}
	return out, nil
}
func (r *orchestratorRepo) CreateStage(context.Context, run.CreateStageParams) (*run.Stage, error) {
	return nil, errors.New("not used")
}

func TestSubmitApproval_OrchestratorAdvancesNextStage(t *testing.T) {
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	first := rr.seedStage(r.ID, 0, run.StageStateAwaitingApproval)
	second := rr.seedStage(r.ID, 1, run.StageStatePending)

	ar := newFakeApprovalRepo()
	au := newApprovalAuditFake()
	o := &orchestrator.Orchestrator{Runs: rr} // no GitHub: dispatch skipped, transition still happens

	s := New(Config{
		Addr:         "127.0.0.1:0",
		ApprovalRepo: ar,
		RunRepo:      rr,
		AuditRepo:    au,
		Orchestrator: o,
	})

	w := submitApproval(t, s, first.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}

	// First stage transitioned by approval handler.
	if first.State != run.StageStateSucceeded {
		t.Errorf("first.State = %q, want succeeded", first.State)
	}
	// Second stage transitioned by orchestrator.
	if second.State != run.StageStateDispatched {
		t.Errorf("second.State = %q, want dispatched (orchestrator should have advanced)",
			second.State)
	}
}

func TestSubmitApproval_NoOrchestrator_LeavesNextStagePending(t *testing.T) {
	// Without an orchestrator wired, the approval handler still
	// completes the gate but the next stage stays in pending.
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	first := rr.seedStage(r.ID, 0, run.StageStateAwaitingApproval)
	second := rr.seedStage(r.ID, 1, run.StageStatePending)

	ar := newFakeApprovalRepo()
	au := newApprovalAuditFake()
	s := New(Config{
		Addr: "127.0.0.1:0", ApprovalRepo: ar, RunRepo: rr, AuditRepo: au,
	})

	w := submitApproval(t, s, first.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if first.State != run.StageStateSucceeded {
		t.Errorf("first.State = %q", first.State)
	}
	if second.State != run.StageStatePending {
		t.Errorf("second.State = %q, want pending (no orchestrator)", second.State)
	}
}

func TestSubmitApproval_Reject_AdvancesRunToFailed(t *testing.T) {
	// Reject must hand off to the orchestrator so the run walks
	// pending → running → failed. Without that the run stays
	// stuck in pending forever once an approver rejects (the bug
	// that drove this fix). Downstream stages stay pending — the
	// orchestrator short-circuits on the failed first stage rather
	// than dispatching anything past it.
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	first := rr.seedStage(r.ID, 0, run.StageStateAwaitingApproval)
	second := rr.seedStage(r.ID, 1, run.StageStatePending)

	ar := newFakeApprovalRepo()
	au := newApprovalAuditFake()
	o := &orchestrator.Orchestrator{Runs: rr}

	s := New(Config{
		Addr: "127.0.0.1:0", ApprovalRepo: ar, RunRepo: rr,
		AuditRepo: au, Orchestrator: o,
	})

	w := submitApproval(t, s, first.ID, `{"decision":"reject"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if first.State != run.StageStateFailed {
		t.Errorf("first.State = %q, want failed", first.State)
	}
	if second.State != run.StageStatePending {
		t.Errorf("second.State = %q, want pending (orchestrator shouldn't dispatch past a failed stage)", second.State)
	}
	got, err := rr.GetRun(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != run.StateFailed {
		t.Errorf("run.State = %q, want failed (reject should walk pending → running → failed)", got.State)
	}
}

func TestSubmitApproval_CommentOptional(t *testing.T) {
	s, ar, rr, _ := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)
	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if ar.all[0].Comment != nil {
		t.Errorf("Comment = %v, want nil", ar.all[0].Comment)
	}
}

func TestSubmitApproval_CommentForwarded(t *testing.T) {
	s, ar, rr, _ := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)
	w := submitApproval(t, s, stage.ID, `{"decision":"approve","comment":"lgtm"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if ar.all[0].Comment == nil || *ar.all[0].Comment != "lgtm" {
		t.Errorf("Comment = %v", ar.all[0].Comment)
	}
}

func TestSubmitApproval_UnknownField(t *testing.T) {
	s, _, rr, _ := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)
	w := submitApproval(t, s, stage.ID, `{"decision":"approve","extra":true}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 on unknown field", w.Code)
	}
}

// fakeStageCheckRepo lets the approval-handler tests exercise the
// blocking-check enforcement without touching Postgres. Returns
// canned states keyed by (stage_id, check_name).
type fakeStageCheckRepo struct {
	byKey map[string]*stagecheck.Check
}

func newFakeStageCheckRepo() *fakeStageCheckRepo {
	return &fakeStageCheckRepo{byKey: map[string]*stagecheck.Check{}}
}
func (f *fakeStageCheckRepo) keyFor(stageID uuid.UUID, name string) string {
	return stageID.String() + ":" + name
}
func (f *fakeStageCheckRepo) seed(stageID uuid.UUID, name string, state stagecheck.State) {
	f.byKey[f.keyFor(stageID, name)] = &stagecheck.Check{
		StageID: stageID, Name: name, State: state,
	}
}
func (f *fakeStageCheckRepo) Append(context.Context, stagecheck.AppendParams) (*stagecheck.Check, error) {
	return nil, errors.New("not used")
}
func (f *fakeStageCheckRepo) LatestForStage(_ context.Context, stageID uuid.UUID) ([]*stagecheck.Check, error) {
	var out []*stagecheck.Check
	for _, c := range f.byKey {
		if c.StageID == stageID {
			out = append(out, c)
		}
	}
	return out, nil
}
func (f *fakeStageCheckRepo) LatestForStageAndName(_ context.Context, stageID uuid.UUID, name string) (*stagecheck.Check, error) {
	if c, ok := f.byKey[f.keyFor(stageID, name)]; ok {
		return c, nil
	}
	return nil, stagecheck.ErrNotFound
}
func (f *fakeStageCheckRepo) FindMatchingStages(context.Context, int, string, string) ([]uuid.UUID, error) {
	return nil, errors.New("not used")
}

// TestSubmitApproval_Approve_SucceedsRegardlessOfCheckState pins
// the post-#253 (ADR-017) contract: the approval handler does NOT
// gate on stage_check state. Reviewers approve based on plan + diff;
// GitHub branch protection blocks the merge until the required
// checks (including fishhawk_audit_complete, published per #231)
// report green. Both pre-#253 failure modes — a failing observed
// check and a never-observed-yet check — now succeed at the
// approval API; protection is the merge gate.
func TestSubmitApproval_Approve_SucceedsRegardlessOfCheckState(t *testing.T) {
	cases := []struct {
		name string
		seed func(scs *fakeStageCheckRepo, stageID uuid.UUID)
	}{
		{
			name: "failing check no longer blocks approval",
			seed: func(scs *fakeStageCheckRepo, stageID uuid.UUID) {
				scs.seed(stageID, "ci_pass", stagecheck.StateFail)
			},
		},
		{
			name: "never-observed check no longer blocks approval",
			seed: func(_ *fakeStageCheckRepo, _ uuid.UUID) {
				// nothing seeded — ci_pass never observed
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := newOrchestratorRepo()
			r := rr.seedRun()
			stage := rr.seedStage(r.ID, 0, run.StageStateAwaitingApproval)
			stage.Gate = &run.Gate{
				Kind: run.GateKindApproval,
			}
			ar := newFakeApprovalRepo()
			au := newApprovalAuditFake()
			scs := newFakeStageCheckRepo()
			tc.seed(scs, stage.ID)
			o := &orchestrator.Orchestrator{Runs: rr}

			s := New(Config{
				Addr: "127.0.0.1:0", ApprovalRepo: ar, RunRepo: rr,
				AuditRepo: au, Orchestrator: o, StageCheckRepo: scs,
			})

			w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
			}
			if stage.State != run.StageStateSucceeded {
				t.Errorf("stage state = %q, want succeeded", stage.State)
			}
			if len(ar.all) != 1 {
				t.Errorf("approval not recorded: %+v", ar.all)
			}
			if strings.Contains(w.Body.String(), "blocking_checks_not_passed") {
				t.Errorf("response should not reference the dropped error code:\n%s", w.Body.String())
			}
		})
	}
}

func TestSubmitApproval_Approve_PassesWhenAllChecksPass(t *testing.T) {
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	stage := rr.seedStage(r.ID, 0, run.StageStateAwaitingApproval)
	stage.Gate = &run.Gate{
		Kind: run.GateKindApproval,
	}
	ar := newFakeApprovalRepo()
	au := newApprovalAuditFake()
	scs := newFakeStageCheckRepo()
	scs.seed(stage.ID, "ci_pass", stagecheck.StatePass)
	scs.seed(stage.ID, "fishhawk_audit_complete", stagecheck.StatePass)
	o := &orchestrator.Orchestrator{Runs: rr}

	s := New(Config{
		Addr: "127.0.0.1:0", ApprovalRepo: ar, RunRepo: rr,
		AuditRepo: au, Orchestrator: o, StageCheckRepo: scs,
	})
	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if stage.State != run.StageStateSucceeded {
		t.Errorf("stage state = %q, want succeeded", stage.State)
	}
}

func TestSubmitApproval_Reject_NotBlockedByFailingChecks(t *testing.T) {
	// Reject is the path failing checks were intended to surface.
	// Refusing rejection on a failing check would defeat the
	// purpose — let the reviewer reject the stage and the run
	// walks to failed.
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	stage := rr.seedStage(r.ID, 0, run.StageStateAwaitingApproval)
	stage.Gate = &run.Gate{
		Kind: run.GateKindApproval,
	}
	ar := newFakeApprovalRepo()
	au := newApprovalAuditFake()
	scs := newFakeStageCheckRepo()
	scs.seed(stage.ID, "ci_pass", stagecheck.StateFail)
	o := &orchestrator.Orchestrator{Runs: rr}

	s := New(Config{
		Addr: "127.0.0.1:0", ApprovalRepo: ar, RunRepo: rr,
		AuditRepo: au, Orchestrator: o, StageCheckRepo: scs,
	})
	w := submitApproval(t, s, stage.ID, `{"decision":"reject"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (reject ignores blocking checks)", w.Code)
	}
	if stage.State != run.StageStateFailed {
		t.Errorf("stage state = %q, want failed", stage.State)
	}
}

func TestSubmitApproval_Approve_FallsOpenWhenStageCheckRepoNil(t *testing.T) {
	// Legacy v0 deployments without check ingestion shouldn't
	// refuse every approve. The handler falls open when
	// StageCheckRepo is nil.
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	stage := rr.seedStage(r.ID, 0, run.StageStateAwaitingApproval)
	stage.Gate = &run.Gate{
		Kind: run.GateKindApproval,
	}
	ar := newFakeApprovalRepo()
	au := newApprovalAuditFake()
	o := &orchestrator.Orchestrator{Runs: rr}

	s := New(Config{
		Addr: "127.0.0.1:0", ApprovalRepo: ar, RunRepo: rr,
		AuditRepo: au, Orchestrator: o,
	})
	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no StageCheckRepo wired)", w.Code)
	}
}

// seedBudgetPlanArtifact inserts a standard_v1 plan artifact with real content
// into art for stageID. Uses fakeArtifactRepo.Create so the ListForStage path
// in loadApprovedPlanForRun finds the decoded plan fields.
func seedBudgetPlanArtifact(t *testing.T, art *fakeArtifactRepo, stageID uuid.UUID, p *plan.Plan) {
	t.Helper()
	b, _ := json.Marshal(p)
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       stageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       b,
		ContentHash:   "hash",
	}); err != nil {
		t.Fatalf("seedBudgetPlanArtifact: %v", err)
	}
}

// newBudgetCheckServer wires a Server with an orchestratorRepo and a
// fakeArtifactRepo so budget-check tests can drive the full approval path.
func newBudgetCheckServer(t *testing.T, ar artifact.Repository) (*Server, *orchestratorRepo, *approvalAuditFake) {
	t.Helper()
	rr := newOrchestratorRepo()
	app := newFakeApprovalRepo()
	au := newApprovalAuditFake()
	o := &orchestrator.Orchestrator{Runs: rr}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		ApprovalRepo: app,
		RunRepo:      rr,
		AuditRepo:    au,
		Orchestrator: o,
		ArtifactRepo: ar,
	})
	return s, rr, au
}

// seedBudgetRun creates a run and a plan stage in awaiting_approval, seeds the
// given plan artifact onto the plan stage, and returns them.
func seedBudgetRun(t *testing.T, rr *orchestratorRepo, art *fakeArtifactRepo, p *plan.Plan) (*run.Run, *run.Stage) {
	t.Helper()
	r := rr.seedRun()
	stage := rr.seedStage(r.ID, 0, run.StageStateAwaitingApproval)
	// stage.Type is StageTypePlan by default (see orchestratorRepo.seedStage)
	seedBudgetPlanArtifact(t, art, stage.ID, p)
	return r, stage
}

func TestSubmitApproval_BudgetCheck_OverBudgetNoDecompNoOverride_Returns422(t *testing.T) {
	// Plan predicts 20 minutes; default budget is 15. No decomposition,
	// no --override-budget → 422 + plan_violates_budget in audit + orchestrator NOT called.
	art := newFakeArtifactRepo()
	s, rr, au := newBudgetCheckServer(t, art)

	p := &plan.Plan{
		PlanVersion:             "standard_v1",
		PredictedRuntimeMinutes: 20, // exceeds 15m default
	}
	_, stage := seedBudgetRun(t, rr, art, p)

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"plan_violates_budget"`) {
		t.Errorf("body missing plan_violates_budget: %s", w.Body.String())
	}

	// Stage must NOT have advanced (orchestrator not called).
	if stage.State != run.StageStateAwaitingApproval {
		t.Errorf("stage.State = %q, want awaiting_approval (orchestrator must not be called)", stage.State)
	}

	// Audit must contain plan_violates_budget but NOT approval_submitted.
	var foundViolation bool
	for _, e := range au.appended {
		if e.Category == "plan_violates_budget" {
			foundViolation = true
		}
		if e.Category == "approval_submitted" {
			t.Errorf("unexpected approval_submitted audit entry (advance was blocked)")
		}
	}
	if !foundViolation {
		t.Errorf("expected plan_violates_budget audit entry, got %+v", au.appended)
	}
}

func TestSubmitApproval_BudgetCheck_OverBudgetWithDecomp_Proceeds(t *testing.T) {
	// Plan is over-budget but includes a decomposition block → proceed (200).
	art := newFakeArtifactRepo()
	s, rr, au := newBudgetCheckServer(t, art)

	p := &plan.Plan{
		PlanVersion:             "standard_v1",
		PredictedRuntimeMinutes: 20,
		Decomposition: &plan.Decomposition{
			Rationale: "too big",
			SubPlans:  []plan.SubPlanSummary{{Title: "part1", PredictedRuntimeMinutes: 10}},
		},
	}
	_, stage := seedBudgetRun(t, rr, art, p)

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (decomposition satisfies budget):\n%s", w.Code, w.Body.String())
	}
	if stage.State != run.StageStateSucceeded {
		t.Errorf("stage.State = %q, want succeeded", stage.State)
	}
	// Must not emit plan_violates_budget.
	for _, e := range au.appended {
		if e.Category == "plan_violates_budget" {
			t.Errorf("unexpected plan_violates_budget audit entry when decomposition present")
		}
	}
}

func TestSubmitApproval_BudgetCheck_OverBudgetWithOverrideComment_Proceeds(t *testing.T) {
	// Over-budget, no decomposition, but --override-budget in comment → 200 +
	// plan_budget_override_acknowledged audit + orchestrator called.
	art := newFakeArtifactRepo()
	s, rr, au := newBudgetCheckServer(t, art)

	p := &plan.Plan{
		PlanVersion:             "standard_v1",
		PredictedRuntimeMinutes: 20,
	}
	_, stage := seedBudgetRun(t, rr, art, p)

	w := submitApproval(t, s, stage.ID, `{"decision":"approve","comment":"lgtm --override-budget"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (override comment):\n%s", w.Code, w.Body.String())
	}
	if stage.State != run.StageStateSucceeded {
		t.Errorf("stage.State = %q, want succeeded", stage.State)
	}

	var foundOverride bool
	for _, e := range au.appended {
		if e.Category == "plan_budget_override_acknowledged" {
			foundOverride = true
		}
		if e.Category == "plan_violates_budget" {
			t.Errorf("unexpected plan_violates_budget when --override-budget present")
		}
	}
	if !foundOverride {
		t.Errorf("expected plan_budget_override_acknowledged audit entry, got %+v", au.appended)
	}
}

func TestSubmitApproval_BudgetCheck_WithinBudget_Proceeds(t *testing.T) {
	// Plan predicts 10 minutes, within 15m default budget → 200, no budget audit.
	art := newFakeArtifactRepo()
	s, rr, au := newBudgetCheckServer(t, art)

	p := &plan.Plan{
		PlanVersion:             "standard_v1",
		PredictedRuntimeMinutes: 10,
	}
	_, stage := seedBudgetRun(t, rr, art, p)

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (within budget):\n%s", w.Code, w.Body.String())
	}
	if stage.State != run.StageStateSucceeded {
		t.Errorf("stage.State = %q, want succeeded", stage.State)
	}
	for _, e := range au.appended {
		if e.Category == "plan_violates_budget" || e.Category == "plan_budget_override_acknowledged" {
			t.Errorf("unexpected budget audit entry when within budget: %s", e.Category)
		}
	}
}

func TestSubmitApproval_Reject_RejectionCommentInAuditPayload(t *testing.T) {
	// Reject with non-empty comment → approval_submitted payload carries rejection_comment.
	s, _, rr, au := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)

	w := submitApproval(t, s, stage.ID, `{"decision":"reject","comment":"plan needs more detail"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	var foundSubmitted bool
	for _, e := range au.appended {
		if e.Category != "approval_submitted" {
			continue
		}
		foundSubmitted = true
		var payload map[string]any
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("unmarshal audit payload: %v", err)
		}
		got, ok := payload["rejection_comment"]
		if !ok {
			t.Errorf("rejection_comment missing from payload: %v", payload)
		} else if got != "plan needs more detail" {
			t.Errorf("rejection_comment = %v, want 'plan needs more detail'", got)
		}
	}
	if !foundSubmitted {
		t.Errorf("expected approval_submitted audit entry, got %+v", au.appended)
	}
}

// submitApprovalAs posts an approval with an explicit token subject,
// bypassing the muxer (which would run bearerAuth and overwrite the
// injected identity). Mirrors approveRequest in approvals_role_test.go
// but takes the raw JSON body so the #751 test can thread
// approver_github_login.
func submitApprovalAs(t *testing.T, s *Server, stageID uuid.UUID, subject, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v0/stages/%s/approvals", stageID), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("stage_id", stageID.String())
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{Subject: subject}))
	w := httptest.NewRecorder()
	s.handleSubmitApproval(w, req)
	return w
}

// TestSubmitApproval_ApproverGithubLogin_CrossBoundary is the #751
// cross-boundary seam: an MCP-loop approval whose token subject is the
// non-login "brett@local-mcp" but which threads a resolved
// approver_github_login must (a) record the token subject as the audit
// `approver` (provenance) and the resolved login as a SUPPLEMENTARY
// `approver_github_login`, and (b) render the issue-thread status
// footer with the resolved login's `@`-mention — not the token
// subject. It also pins the stop-the-ping guarantee: an approval with
// only the bare token subject (no resolved login) renders "an
// approver", never an `@`-mention. The seam crossed is HTTP body →
// handler → approval_submitted audit payload → notifier footer render.
func TestSubmitApproval_ApproverGithubLogin_CrossBoundary(t *testing.T) {
	const tokenSubject = "brett@local-mcp"

	// (1) Resolved-login path.
	s, _, rr, au := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)

	w := submitApprovalAs(t, s, stage.ID, tokenSubject,
		`{"decision":"approve","approver_github_login":"kuhlman-labs"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	payload := findApprovalSubmittedPayload(t, au.appended)
	if got := payload["approver"]; got != tokenSubject {
		t.Errorf("audit approver = %v, want %q (provenance must stay the token subject)", got, tokenSubject)
	}
	if got := payload["approver_github_login"]; got != "kuhlman-labs" {
		t.Errorf("audit approver_github_login = %v, want kuhlman-labs", got)
	}

	// Render the footer from the exact audit payload the handler wrote.
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("re-marshal payload: %v", err)
	}
	footer := issuecomment.PlanStatusFooterForAuditPayload(raw)
	if footer != "_Status: approved by @kuhlman-labs · implementing now_" {
		t.Errorf("footer = %q, want @kuhlman-labs mention", footer)
	}

	// (2) Stop-the-ping path: bare token subject, no resolved login.
	s2, _, rr2, au2 := newApprovalServer(t)
	stage2 := rr2.seedStage(run.StageStateAwaitingApproval)

	w2 := submitApprovalAs(t, s2, stage2.ID, tokenSubject, `{"decision":"approve"}`)
	if w2.Code != http.StatusOK {
		t.Fatalf("bare-subject status = %d, want 200:\n%s", w2.Code, w2.Body.String())
	}
	bare := findApprovalSubmittedPayload(t, au2.appended)
	if got := bare["approver"]; got != tokenSubject {
		t.Errorf("bare audit approver = %v, want %q", got, tokenSubject)
	}
	if _, ok := bare["approver_github_login"]; ok {
		t.Errorf("approver_github_login must be omitted when none was sent; payload: %v", bare)
	}
	rawBare, err := json.Marshal(bare)
	if err != nil {
		t.Fatalf("re-marshal bare payload: %v", err)
	}
	if got := issuecomment.PlanStatusFooterForAuditPayload(rawBare); got != "_Status: approved by an approver · implementing now_" {
		t.Errorf("bare-subject footer = %q, want \"an approver\" (no ping)", got)
	}
}

// findApprovalSubmittedPayload returns the decoded payload of the
// single approval_submitted audit entry, failing the test when absent.
func findApprovalSubmittedPayload(t *testing.T, appended []audit.ChainAppendParams) map[string]any {
	t.Helper()
	for _, e := range appended {
		if e.Category != "approval_submitted" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("unmarshal audit payload: %v", err)
		}
		return payload
	}
	t.Fatalf("expected an approval_submitted audit entry, got %+v", appended)
	return nil
}

func TestSubmitApproval_Reject_EmptyComment_NoRejectionCommentInPayload(t *testing.T) {
	// Reject with no comment → approval_submitted payload must not have rejection_comment.
	s, _, rr, au := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)

	w := submitApproval(t, s, stage.ID, `{"decision":"reject"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	for _, e := range au.appended {
		if e.Category != "approval_submitted" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("unmarshal audit payload: %v", err)
		}
		if _, ok := payload["rejection_comment"]; ok {
			t.Errorf("rejection_comment must not appear when comment is empty; payload: %v", payload)
		}
	}
}

func TestSubmitApproval_Reject_DecomposeComment_SetsRejectReason(t *testing.T) {
	// Reject with "--decompose" in comment → approval_submitted payload
	// contains reject_reason=decompose_required.
	s, _, rr, au := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)
	// stage.Type is StageTypePlan (default from seedStage)

	w := submitApproval(t, s, stage.ID, `{"decision":"reject","comment":"please decompose --decompose"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	var foundSubmitted bool
	for _, e := range au.appended {
		if e.Category != "approval_submitted" {
			continue
		}
		foundSubmitted = true
		var payload map[string]any
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("unmarshal audit payload: %v", err)
		}
		if got, ok := payload["reject_reason"]; !ok || got != "decompose_required" {
			t.Errorf("reject_reason = %v, want decompose_required; payload: %v", got, payload)
		}
	}
	if !foundSubmitted {
		t.Errorf("expected approval_submitted audit entry, got %+v", au.appended)
	}
}
