package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/modeloracle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
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

// ListStagesForRun returns every seeded stage sharing the queried
// RunID, so the fix-up handler's push_and_open_pr applicability check
// (run.FixupStage) can locate the run's review stage (#780).
func (r *approvalRunRepo) ListStagesForRun(_ context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*run.Stage
	for _, st := range r.stages {
		if st.RunID == runID {
			out = append(out, st)
		}
	}
	return out, nil
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
// audit-entry shape and category. allEntries seeds ListAll, which
// implementCalibrationP95 scans for runtime_observed samples when the
// budget gate resolves the p95 term (#994).
type approvalAuditFake struct {
	mu         sync.Mutex
	appended   []audit.ChainAppendParams
	appendErr  error
	allEntries []*audit.Entry
}

func (a *approvalAuditFake) seedAll(entries ...*audit.Entry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.allEntries = append(a.allEntries, entries...)
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
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.allEntries, nil
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
	// Return shallow copies under the lock, mirroring the postgres repo's
	// per-query struct isolation: a caller that reads a returned stage's
	// fields (e.g. auditcomplete.Compute's mid-flight State check, now
	// reachable from the detached advisory implement-review goroutine via
	// recomputeAndPublishAuditComplete) must not share the live pointer a
	// concurrent TransitionStage mutates. Live identity stays available via
	// GetStage for the seedStage-pointer reads the tests assert on.
	src := r.stagesByRunID[runID]
	out := make([]*run.Stage, len(src))
	for i, st := range src {
		cp := *st
		out[i] = &cp
	}
	return out, nil
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

// TestSubmitApproval_AddScopeFiles_RecordedInAuditPayload pins the #824
// persistence seam: an approve carrying the structured add_scope_files slice
// (including a directory and an extensionless root file) records those exact
// paths under the `add_scope_files` key of the approval_submitted audit
// payload, where the prompt builder reads them back. The DisallowUnknownFields
// decoder accepting the field is implicitly proven by the 200; an adjacent
// unknown field still 400s (TestSubmitApproval_UnknownField).
func TestSubmitApproval_AddScopeFiles_RecordedInAuditPayload(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)

	w := submitApproval(t, s, stage.ID,
		`{"decision":"approve","add_scope_files":["backend/internal/agenteval/testdata/corpus/newcase/","go.work","backend/cmd/fishhawk-mcp/README.md"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	payload := findApprovalSubmittedPayload(t, au.appended)
	raw, ok := payload["add_scope_files"].([]any)
	if !ok {
		t.Fatalf("add_scope_files missing or not an array: %v", payload["add_scope_files"])
	}
	got := make([]string, len(raw))
	for i, v := range raw {
		got[i] = v.(string)
	}
	want := []string{"backend/internal/agenteval/testdata/corpus/newcase/", "go.work", "backend/cmd/fishhawk-mcp/README.md"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("add_scope_files = %v, want %v", got, want)
	}
}

// TestSubmitApproval_AddScopeFiles_OmittedWhenEmpty confirms the key is absent
// when no paths are supplied (omitempty on the wire, and the handler only
// records on a non-empty slice) so the old loader is undisturbed.
func TestSubmitApproval_AddScopeFiles_OmittedWhenEmpty(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)

	w := submitApproval(t, s, stage.ID, `{"decision":"approve","comment":"lgtm"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	payload := findApprovalSubmittedPayload(t, au.appended)
	if _, ok := payload["add_scope_files"]; ok {
		t.Errorf("add_scope_files should be absent when not supplied: %v", payload)
	}
}

// TestSubmitApproval_BindingAssertions_RecordedInAuditPayload pins the #1171
// persistence seam: an approve carrying binding_assertions records those exact
// typed checks under the `binding_assertions` key of the approval_submitted
// audit payload, where the prompt builder reads them back. The
// DisallowUnknownFields decoder accepting the field is implicitly proven by
// the 200.
func TestSubmitApproval_BindingAssertions_RecordedInAuditPayload(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)

	w := submitApproval(t, s, stage.ID,
		`{"decision":"approve","binding_assertions":[{"type":"file_contains","path":"backend/internal/yaml/pad.go","literal":"pad: 3"},{"type":"test_asserts","path":"backend/internal/yaml/pad_test.go","literal":"TestPad"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	payload := findApprovalSubmittedPayload(t, au.appended)
	raw, ok := payload["binding_assertions"].([]any)
	if !ok {
		t.Fatalf("binding_assertions missing or not an array: %v", payload["binding_assertions"])
	}
	if len(raw) != 2 {
		t.Fatalf("binding_assertions len = %d, want 2: %v", len(raw), raw)
	}
	first, ok := raw[0].(map[string]any)
	if !ok {
		t.Fatalf("binding_assertions[0] not an object: %v", raw[0])
	}
	if first["type"] != "file_contains" || first["path"] != "backend/internal/yaml/pad.go" || first["literal"] != "pad: 3" {
		t.Errorf("binding_assertions[0] = %v, want {file_contains, pad.go, pad: 3}", first)
	}
}

// TestSubmitApproval_BindingAssertions_MalformedRejected confirms a malformed
// declaration (here, an unknown type) is rejected 400 validation_failed BEFORE
// any approval row is recorded, so a retry with a corrected declaration flows
// normally.
func TestSubmitApproval_BindingAssertions_MalformedRejected(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)

	w := submitApproval(t, s, stage.ID,
		`{"decision":"approve","binding_assertions":[{"type":"file_matches","path":"a/b.go","literal":"x"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 on malformed binding_assertions:\n%s", w.Code, w.Body.String())
	}
	for _, e := range au.appended {
		if e.Category == "approval_submitted" {
			t.Errorf("approval_submitted recorded despite malformed declaration: %v", e)
		}
	}
}

// TestSubmitApproval_BindingAssertions_OmittedWhenEmpty confirms the key is
// absent when no assertions are supplied — the byte-identical no-declaration
// path.
func TestSubmitApproval_BindingAssertions_OmittedWhenEmpty(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)

	w := submitApproval(t, s, stage.ID, `{"decision":"approve","comment":"lgtm"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	payload := findApprovalSubmittedPayload(t, au.appended)
	if _, ok := payload["binding_assertions"]; ok {
		t.Errorf("binding_assertions should be absent when not supplied: %v", payload)
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
func newBudgetCheckServer(t *testing.T, ar artifact.Repository) (*Server, *orchestratorRepo, *approvalAuditFake, *fakeApprovalRepo) {
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
	return s, rr, au, app
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
	s, rr, au, app := newBudgetCheckServer(t, art)

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

	// #986: the 422 fires PRE-Submit, so no approval row may exist —
	// the submission slot stays free for the --override-budget retry.
	if rows, err := app.ListForStage(context.Background(), stage.ID); err != nil || len(rows) != 0 {
		t.Errorf("approval rows after 422 = %d (err=%v), want 0 (refused before insert)", len(rows), err)
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

// --- Model gate (#1013) -------------------------------------------------

// specImplementAgentModel builds a workflow "w" spec whose implement stage
// declares the given executor.agent and (optionally) executor.model. Mirrors
// the orchestratorRepo.seedRun WorkflowID "w".
func specImplementAgentModel(agent, model string) []byte {
	var modelLine string
	if model != "" {
		modelLine = "\n          model: " + model
	}
	return []byte(`version: "0.3"
workflows:
  w:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
      - id: implement
        type: implement
        executor:
          agent: ` + agent + modelLine + `
`)
}

// newModelGateServer mirrors newBudgetCheckServer but wires an allow-list and
// deployment default so the model gate (#1013) is exercisable end to end.
func newModelGateServer(t *testing.T, art artifact.Repository, allow AllowedModels, deflt string) (*Server, *orchestratorRepo, *approvalAuditFake, *fakeApprovalRepo) {
	t.Helper()
	rr := newOrchestratorRepo()
	app := newFakeApprovalRepo()
	au := newApprovalAuditFake()
	o := &orchestrator.Orchestrator{Runs: rr}
	s := New(Config{
		Addr:                   "127.0.0.1:0",
		ApprovalRepo:           app,
		RunRepo:                rr,
		AuditRepo:              au,
		Orchestrator:           o,
		ArtifactRepo:           art,
		ImplementAllowedModels: allow,
		ImplementModelDefault:  deflt,
	})
	return s, rr, au, app
}

// planWithRecommendation returns a small (under-budget) standard_v1 plan whose
// model_recommendation.implement_model is the given model (omitted when empty).
func planWithRecommendation(implementModel string) *plan.Plan {
	p := &plan.Plan{
		PlanVersion:             "standard_v1",
		PredictedRuntimeMinutes: 5, // under the 15m default budget
	}
	if implementModel != "" {
		p.ModelRecommendation = &plan.ModelRecommendation{
			ImplementModel:     implementModel,
			Rationale:          "complexity-informed",
			ComplexityAssessed: plan.ComplexityHigh,
		}
	}
	return p
}

// findModelResolvedPayload returns the decoded ResolvedModel from the single
// model_resolved audit entry, failing when zero or more than one exists.
func findModelResolvedPayload(t *testing.T, appended []audit.ChainAppendParams) ResolvedModel {
	t.Helper()
	var found *ResolvedModel
	for _, e := range appended {
		if e.Category != CategoryModelResolved {
			continue
		}
		if found != nil {
			t.Fatalf("expected exactly one model_resolved entry, got more than one")
		}
		var rm ResolvedModel
		if err := json.Unmarshal(e.Payload, &rm); err != nil {
			t.Fatalf("unmarshal model_resolved payload: %v", err)
		}
		found = &rm
	}
	if found == nil {
		t.Fatalf("expected a model_resolved audit entry, got %+v", appended)
	}
	return *found
}

// TestSubmitApproval_ModelGate_RejectedSources covers binding condition 1: an
// unknown RESOLVED model is rejected 422 plan_invalid_model at the gate, with
// the message naming the resolved source — for EACH of the four rungs
// (default, spec, plan, operator), not just the operator field. A 422 must
// insert no approval row and emit no model_resolved audit.
func TestSubmitApproval_ModelGate_RejectedSources(t *testing.T) {
	allow := AllowedModels{"claudecode": {"good-model": true}}
	tests := []struct {
		name       string
		deflt      string
		specModel  string
		planModel  string
		operator   string
		wantSource string
	}{
		{name: "operator source rejected", operator: "bad-op", wantSource: "operator"},
		{name: "plan source rejected", planModel: "bad-plan", wantSource: "plan"},
		{name: "spec source rejected", specModel: "bad-spec", wantSource: "spec"},
		{name: "default source rejected", deflt: "bad-default", wantSource: "default"},
		{
			// Operator override wins even over a plan recommendation, so an
			// invalid operator value is the rejected source despite a (valid)
			// plan rung below it.
			name:      "operator override rejected over a valid plan rung",
			planModel: "good-model", operator: "bad-op", wantSource: "operator",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			art := newFakeArtifactRepo()
			s, rr, au, app := newModelGateServer(t, art, allow, tt.deflt)
			r, stage := seedBudgetRun(t, rr, art, planWithRecommendation(tt.planModel))
			r.WorkflowSpec = specImplementAgentModel("claude-code", tt.specModel)

			body := `{"decision":"approve"}`
			if tt.operator != "" {
				body = fmt.Sprintf(`{"decision":"approve","implement_model":%q}`, tt.operator)
			}
			w := submitApproval(t, s, stage.ID, body)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), `"plan_invalid_model"`) {
				t.Errorf("body missing plan_invalid_model: %s", w.Body.String())
			}
			if !strings.Contains(w.Body.String(), `"model_source":"`+tt.wantSource+`"`) {
				t.Errorf("body missing model_source=%s: %s", tt.wantSource, w.Body.String())
			}
			// PRE-Submit: no approval row, stage unchanged.
			if rows, err := app.ListForStage(context.Background(), stage.ID); err != nil || len(rows) != 0 {
				t.Errorf("approval rows after 422 = %d (err=%v), want 0", len(rows), err)
			}
			if stage.State != run.StageStateAwaitingApproval {
				t.Errorf("stage.State = %q, want awaiting_approval", stage.State)
			}
			for _, e := range au.appended {
				if e.Category == CategoryModelResolved || e.Category == "approval_submitted" {
					t.Errorf("unexpected %s audit entry after a 422", e.Category)
				}
			}
		})
	}
}

// TestSubmitApproval_ModelGate_AcceptedSources covers the audit-payload
// assertion for ALL FOUR sources: an allowed RESOLVED model approves (200) and
// the emitted model_resolved entry carries the correct {value, source}.
func TestSubmitApproval_ModelGate_AcceptedSources(t *testing.T) {
	allow := AllowedModels{"claudecode": {
		"m-default": true, "m-spec": true, "m-plan": true, "m-operator": true,
	}}
	tests := []struct {
		name       string
		deflt      string
		specModel  string
		planModel  string
		operator   string
		wantValue  string
		wantSource ModelSource
	}{
		{name: "default source", deflt: "m-default", wantValue: "m-default", wantSource: ModelSourceDefault},
		{name: "spec source", specModel: "m-spec", wantValue: "m-spec", wantSource: ModelSourceSpec},
		{name: "plan source", planModel: "m-plan", wantValue: "m-plan", wantSource: ModelSourcePlan},
		{name: "operator source", operator: "m-operator", wantValue: "m-operator", wantSource: ModelSourceOperator},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			art := newFakeArtifactRepo()
			s, rr, au, _ := newModelGateServer(t, art, allow, tt.deflt)
			r, stage := seedBudgetRun(t, rr, art, planWithRecommendation(tt.planModel))
			r.WorkflowSpec = specImplementAgentModel("claude-code", tt.specModel)

			body := `{"decision":"approve"}`
			if tt.operator != "" {
				body = fmt.Sprintf(`{"decision":"approve","implement_model":%q}`, tt.operator)
			}
			w := submitApproval(t, s, stage.ID, body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
			}
			if stage.State != run.StageStateSucceeded {
				t.Errorf("stage.State = %q, want succeeded", stage.State)
			}
			rm := findModelResolvedPayload(t, au.appended)
			if rm.Value != tt.wantValue || rm.Source != tt.wantSource {
				t.Errorf("model_resolved = {%q,%q}, want {%q,%q}", rm.Value, rm.Source, tt.wantValue, tt.wantSource)
			}
		})
	}
}

// TestSubmitApproval_ModelGate_FailOpenEmptyAllowlist covers binding condition
// 2: an empty/unconfigured allow-list accepts ANY model (byte-identical to
// today). The override still resolves + records as model_resolved.
func TestSubmitApproval_ModelGate_FailOpenEmptyAllowlist(t *testing.T) {
	art := newFakeArtifactRepo()
	s, rr, au, _ := newModelGateServer(t, art, nil, "") // nil allow-list
	_, st := seedBudgetRun(t, rr, art, planWithRecommendation(""))

	w := submitApproval(t, s, st.ID, `{"decision":"approve","implement_model":"any-unlisted-model"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fail-open empty allow-list):\n%s", w.Code, w.Body.String())
	}
	rm := findModelResolvedPayload(t, au.appended)
	if rm.Value != "any-unlisted-model" || rm.Source != ModelSourceOperator {
		t.Errorf("model_resolved = {%q,%q}, want {any-unlisted-model,operator}", rm.Value, rm.Source)
	}
}

// TestSubmitApproval_ModelGate_EmptyResolutionStillEmits covers binding
// condition 4: with no default, spec model, plan recommendation, or operator
// override, the resolution is empty (ModelSourceNone) and STILL emits a
// model_resolved entry recording the deliberate default spawn — byte-identical
// to today's no-`--model` spawn (the slice-1 reader returns it as none/ok).
func TestSubmitApproval_ModelGate_EmptyResolutionStillEmits(t *testing.T) {
	art := newFakeArtifactRepo()
	s, rr, au, _ := newModelGateServer(t, art, nil, "")
	_, stage := seedBudgetRun(t, rr, art, planWithRecommendation(""))

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	rm := findModelResolvedPayload(t, au.appended)
	if rm.Value != "" || rm.Source != ModelSourceNone {
		t.Errorf("model_resolved = {%q,%q}, want {\"\",none}", rm.Value, rm.Source)
	}
}

// TestSubmitApproval_ModelGate_GetRunError_FailsOpen pins checkPlanModelAllowed's
// fail-OPEN branch on a RunRepo.GetRun read failure (approvals.go:763-770, the
// PR #1261 review gap tracked by #1262). GetRun is the function's FIRST statement,
// so when the run row is gone the gate returns (nil, true) BEFORE any resolution
// or allow-list logic: the approve proceeds (200, stage succeeded) and — because
// the branch returns a nil ResolvedModel rather than an empty one — NO
// model_resolved audit is emitted, leaving the prompt path to fall through to
// live resolution instead of a shadowing empty audit. Mirrors the sibling
// TestSubmitApproval_PlanReviewGate_GetRunError_FailsOpen. The non-empty
// allow-list + default is deliberate: it proves the branch short-circuits before
// the resolve+validate path, not because the allow-list is empty.
func TestSubmitApproval_ModelGate_GetRunError_FailsOpen(t *testing.T) {
	art := newFakeArtifactRepo()
	s, rr, au, _ := newModelGateServer(t, art, AllowedModels{"claudecode": {"m-default": true}}, "m-default")
	_, stage := seedBudgetRun(t, rr, art, planWithRecommendation(""))

	rr.mu.Lock()
	delete(rr.runs, stage.RunID) // GetRun now returns run.ErrNotFound
	rr.mu.Unlock()

	w := submitApproval(t, s, stage.ID, `{"decision":"approve","implement_model":"m-default"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (GetRun failure fails open):\n%s", w.Code, w.Body.String())
	}
	if stage.State != run.StageStateSucceeded {
		t.Errorf("stage.State = %q, want succeeded", stage.State)
	}
	// 'Emit nothing on read-failure': the fail-open branch returns a nil
	// ResolvedModel, so NO model_resolved audit may appear.
	for _, e := range au.appended {
		if e.Category == CategoryModelResolved {
			t.Errorf("unexpected model_resolved audit entry on the GetRun fail-open path: %+v", e)
		}
	}
}

func TestSubmitApproval_BudgetCheck_OverBudgetWithDecomp_Proceeds(t *testing.T) {
	// Plan is over-budget but includes a decomposition block → proceed (200).
	art := newFakeArtifactRepo()
	s, rr, au, _ := newBudgetCheckServer(t, art)

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
	s, rr, au, _ := newBudgetCheckServer(t, art)

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
	s, rr, au, _ := newBudgetCheckServer(t, art)

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

// TestSubmitApproval_BudgetCheck_EvidenceVerdictMatchesGate is the #1029
// contract test: across every budget scenario, the plan-review prompt's
// rendered Budget-check verdict must agree with checkPlanBudget's outcome
// at the approval gate. Each case drives the real approval endpoint (the
// gate leg) AND the real evidence-build + prompt-render path (the
// evidence leg) on the same plan artifact, so the two surfaces cannot
// drift apart silently (#618 cross-boundary rule; same drift class as
// #994). In particular: a decomposition with an oversized slice still
// satisfies the gate — the gate checks only presence — so the evidence
// must claim "gate satisfied", never refusal.
func TestSubmitApproval_BudgetCheck_EvidenceVerdictMatchesGate(t *testing.T) {
	// Budget resolves to the 15m backend default (seedRun carries no
	// workflow spec), matching the other budget-check tests.
	cases := []struct {
		name string
		plan *plan.Plan
		// wantRefused: the gate 422s plan_violates_budget AND the rendered
		// verdict says "will be refused".
		wantRefused bool
		// wantDecompSatisfied: the gate proceeds on the decomposition
		// branch AND the rendered verdict says "gate satisfied".
		wantDecompSatisfied bool
	}{
		{
			name: "under",
			plan: &plan.Plan{PlanVersion: "standard_v1", PredictedRuntimeMinutes: 10},
		},
		{
			name: "over_decomposed",
			plan: &plan.Plan{
				PlanVersion:             "standard_v1",
				PredictedRuntimeMinutes: 20,
				Decomposition: &plan.Decomposition{
					Rationale: "too big",
					SubPlans: []plan.SubPlanSummary{
						{Title: "part1", PredictedRuntimeMinutes: 10},
						{Title: "part2", PredictedRuntimeMinutes: 10},
					},
				},
			},
			wantDecompSatisfied: true,
		},
		{
			name:        "over_undecomposed",
			plan:        &plan.Plan{PlanVersion: "standard_v1", PredictedRuntimeMinutes: 20},
			wantRefused: true,
		},
		{
			name: "over_oversized_slice",
			plan: &plan.Plan{
				PlanVersion:             "standard_v1",
				PredictedRuntimeMinutes: 20,
				Decomposition: &plan.Decomposition{
					Rationale: "too big",
					SubPlans: []plan.SubPlanSummary{
						{Title: "big slice", PredictedRuntimeMinutes: 18},
						{Title: "small slice", PredictedRuntimeMinutes: 4},
					},
				},
			},
			wantDecompSatisfied: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			art := newFakeArtifactRepo()
			s, rr, _, _ := newBudgetCheckServer(t, art)
			r, stage := seedBudgetRun(t, rr, art, tc.plan)

			// Gate leg: drive the real approval endpoint.
			w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
			gateRefused := w.Code == http.StatusUnprocessableEntity &&
				strings.Contains(w.Body.String(), `"plan_violates_budget"`)
			if gateRefused != tc.wantRefused {
				t.Fatalf("gate refused = %v (status %d), want refused = %v:\n%s",
					gateRefused, w.Code, tc.wantRefused, w.Body.String())
			}
			if !tc.wantRefused && w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (gate proceeds):\n%s", w.Code, w.Body.String())
			}

			// Evidence leg: the same server builds the budget evidence and
			// the prompt package renders it — the path runPlanReviews
			// dispatches to the reviewer.
			ev := s.planBudgetEvidence(context.Background(), r, tc.plan)
			if ev == nil {
				t.Fatal("planBudgetEvidence = nil, want evidence (no spec resolves to the backend default)")
			}
			rendered, err := prompt.Build("plan_review", prompt.Trigger{
				Repo:             r.Repo,
				PlanGateEvidence: &prompt.PlanGateEvidence{BudgetCheck: ev},
			})
			if err != nil {
				t.Fatalf("prompt.Build: %v", err)
			}
			if got := strings.Contains(rendered, "will be refused"); got != tc.wantRefused {
				t.Errorf("rendered verdict claims refusal = %v, gate refused = %v — evidence and gate disagree (#1029):\n%s",
					got, tc.wantRefused, rendered)
			}
			if got := strings.Contains(rendered, "gate satisfied"); got != tc.wantDecompSatisfied {
				t.Errorf("rendered verdict claims gate-satisfied-by-decomposition = %v, want %v:\n%s",
					got, tc.wantDecompSatisfied, rendered)
			}
		})
	}
}

// specBudgetDistinctTimeouts declares DIFFERENT executor timeouts for the
// plan (10m) and implement (30m) stages of workflow "w" (the id
// orchestratorRepo.seedRun uses). Every test built on it also proves the
// #994 stage-type fix: the gate must resolve the IMPLEMENT stage's 30m,
// not the plan stage under approval's 10m.
var specBudgetDistinctTimeouts = []byte(`version: "0.3"
workflows:
  w:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
          timeout: "10m"
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
          timeout: "30m"
        produces:
          - artifact: pull_request
`)

// TestSubmitApproval_BudgetCheck_P95WidensBudget_Approves is the #994
// repro: spec implement budget 30m, one calibration sample of 26 actual
// minutes → resolved budget 26×1.5 = 39m. A plan predicting 35 minutes —
// over the spec floor but under the resolved budget — must be approved
// with no override and no budget audit entries.
func TestSubmitApproval_BudgetCheck_P95WidensBudget_Approves(t *testing.T) {
	art := newFakeArtifactRepo()
	s, rr, au, _ := newBudgetCheckServer(t, art)

	p := &plan.Plan{
		PlanVersion:             "standard_v1",
		PredictedRuntimeMinutes: 35,
	}
	r, stage := seedBudgetRun(t, rr, art, p)
	r.WorkflowSpec = specBudgetDistinctTimeouts
	au.seedAll(runtimeObservedImplementEntry(r.ID, 26))

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (35 ≤ resolved 39m budget):\n%s", w.Code, w.Body.String())
	}
	if stage.State != run.StageStateSucceeded {
		t.Errorf("stage.State = %q, want succeeded", stage.State)
	}
	for _, e := range au.appended {
		if e.Category == "plan_violates_budget" || e.Category == "plan_budget_override_acknowledged" {
			t.Errorf("unexpected budget audit entry within the resolved budget: %s", e.Category)
		}
	}
}

// TestSubmitApproval_BudgetCheck_OverResolvedBudget_422CarriesResolvedPayload
// asserts the 422 and audit payloads carry the RESOLVED budget (#994):
// budget_minutes is the p95-widened value, budget_source names the winning
// term, and spec_budget_minutes records the raw spec floor. Also asserts
// the plan-review prompt's budget evidence (planBudgetEvidence) cites the
// identical number for the same seeded data — gate, payload, and reviewer
// prompt agree by construction.
func TestSubmitApproval_BudgetCheck_OverResolvedBudget_422CarriesResolvedPayload(t *testing.T) {
	art := newFakeArtifactRepo()
	s, rr, au, _ := newBudgetCheckServer(t, art)

	p := &plan.Plan{
		PlanVersion:             "standard_v1",
		PredictedRuntimeMinutes: 45, // over the resolved 39m budget
	}
	r, stage := seedBudgetRun(t, rr, art, p)
	r.WorkflowSpec = specBudgetDistinctTimeouts
	au.seedAll(runtimeObservedImplementEntry(r.ID, 26))

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}

	var violation *audit.ChainAppendParams
	for i, e := range au.appended {
		if e.Category == "plan_violates_budget" {
			violation = &au.appended[i]
		}
	}
	if violation == nil {
		t.Fatalf("expected plan_violates_budget audit entry, got %+v", au.appended)
	}
	var payload struct {
		PredictedMinutes  int    `json:"predicted_minutes"`
		BudgetMinutes     int    `json:"budget_minutes"`
		BudgetSource      string `json:"budget_source"`
		SpecBudgetMinutes int    `json:"spec_budget_minutes"`
		TimeoutSource     string `json:"timeout_source"`
	}
	if err := json.Unmarshal(violation.Payload, &payload); err != nil {
		t.Fatalf("decode plan_violates_budget payload: %v", err)
	}
	if payload.BudgetMinutes != 39 {
		t.Errorf("budget_minutes = %d, want 39 (p95 26 × 1.5)", payload.BudgetMinutes)
	}
	if payload.BudgetSource != "p95" {
		t.Errorf("budget_source = %q, want p95", payload.BudgetSource)
	}
	if payload.SpecBudgetMinutes != 30 {
		t.Errorf("spec_budget_minutes = %d, want 30", payload.SpecBudgetMinutes)
	}
	if payload.TimeoutSource != "stage_executor_timeout" {
		t.Errorf("timeout_source = %q, want stage_executor_timeout", payload.TimeoutSource)
	}
	// The 422 details mirror the audit payload.
	for _, want := range []string{`"budget_minutes":39`, `"budget_source":"p95"`, `"spec_budget_minutes":30`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("422 body missing %s:\n%s", want, w.Body.String())
		}
	}

	// Cross-surface agreement (#994): the budget evidence runPlanReviews
	// renders into the plan-review prompt must cite the same resolved
	// number the gate just enforced.
	ev := s.planBudgetEvidence(context.Background(), r, p)
	if ev == nil {
		t.Fatal("planBudgetEvidence = nil, want budget evidence")
	}
	if ev.ResolvedBudgetMinutes != payload.BudgetMinutes {
		t.Errorf("prompt evidence budget = %d, gate budget = %d — surfaces disagree",
			ev.ResolvedBudgetMinutes, payload.BudgetMinutes)
	}
	if ev.BudgetSource != payload.BudgetSource {
		t.Errorf("prompt evidence source = %q, gate source = %q", ev.BudgetSource, payload.BudgetSource)
	}
}

// TestSubmitApproval_BudgetCheck_AntiCircularity_PlanTermExcluded proves
// the gate budget ignores the plan's own predicted_runtime_minutes: with
// no calibration data the budget must stay at the 30m spec floor, so a
// prediction of 40 (which the kill cap's predicted×2 term would widen to
// 60m and self-justify) and a pathological 100 both 422 against 30.
func TestSubmitApproval_BudgetCheck_AntiCircularity_PlanTermExcluded(t *testing.T) {
	for _, predicted := range []int{40, 100} {
		t.Run(fmt.Sprintf("predicted_%d", predicted), func(t *testing.T) {
			art := newFakeArtifactRepo()
			s, rr, au, _ := newBudgetCheckServer(t, art)

			p := &plan.Plan{
				PlanVersion:             "standard_v1",
				PredictedRuntimeMinutes: predicted,
			}
			r, stage := seedBudgetRun(t, rr, art, p)
			r.WorkflowSpec = specBudgetDistinctTimeouts
			// No calibration samples seeded: the budget must stay at the
			// spec floor, not predicted×2.

			w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (gate must not self-satisfy from the plan term):\n%s",
					w.Code, w.Body.String())
			}
			var found bool
			for _, e := range au.appended {
				if e.Category != "plan_violates_budget" {
					continue
				}
				found = true
				var payload struct {
					BudgetMinutes int    `json:"budget_minutes"`
					BudgetSource  string `json:"budget_source"`
				}
				if err := json.Unmarshal(e.Payload, &payload); err != nil {
					t.Fatalf("decode payload: %v", err)
				}
				if payload.BudgetMinutes != 30 {
					t.Errorf("budget_minutes = %d, want 30 (spec floor)", payload.BudgetMinutes)
				}
				if payload.BudgetSource != "spec" {
					t.Errorf("budget_source = %q, want spec", payload.BudgetSource)
				}
			}
			if !found {
				t.Error("expected plan_violates_budget audit entry")
			}
		})
	}
}

// TestSubmitApproval_BudgetCheck_UsesImplementStageTimeout pins the #994
// stage-type fix in isolation: with distinct plan (10m) and implement
// (30m) executor timeouts and no calibration data, a plan predicting 20
// minutes must be approved — the gate compares against the implement
// stage's 30m, not the 10m of the plan stage under approval (which the
// pre-#994 code resolved via stage.Type).
func TestSubmitApproval_BudgetCheck_UsesImplementStageTimeout(t *testing.T) {
	art := newFakeArtifactRepo()
	s, rr, au, _ := newBudgetCheckServer(t, art)

	p := &plan.Plan{
		PlanVersion:             "standard_v1",
		PredictedRuntimeMinutes: 20, // over plan-stage 10m, under implement 30m
	}
	r, stage := seedBudgetRun(t, rr, art, p)
	r.WorkflowSpec = specBudgetDistinctTimeouts

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (implement-stage 30m budget governs):\n%s", w.Code, w.Body.String())
	}
	for _, e := range au.appended {
		if e.Category == "plan_violates_budget" {
			t.Errorf("unexpected plan_violates_budget — gate compared against the wrong stage's timeout")
		}
	}
}

func TestSubmitApproval_BudgetCheck_OverrideRetryAfter422_Succeeds(t *testing.T) {
	// THE #986 regression: over-budget approve → 422 with NO approval row
	// recorded → the documented retry by the SAME subject with
	// --override-budget flows normally through Submit → advanceStage.
	// On pre-#986 code the Submit preceded checkPlanBudget, so the 422
	// left a row behind and this retry dead-ended as a silent duplicate
	// 200 with the stage still parked in awaiting_approval.
	art := newFakeArtifactRepo()
	s, rr, au, app := newBudgetCheckServer(t, art)

	p := &plan.Plan{
		PlanVersion:             "standard_v1",
		PredictedRuntimeMinutes: 20, // exceeds 15m default
	}
	_, stage := seedBudgetRun(t, rr, art, p)

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("first status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if rows, _ := app.ListForStage(context.Background(), stage.ID); len(rows) != 0 {
		t.Fatalf("approval rows after 422 = %d, want 0", len(rows))
	}
	if stage.State != run.StageStateAwaitingApproval {
		t.Fatalf("stage.State after 422 = %q, want awaiting_approval", stage.State)
	}

	w = submitApproval(t, s, stage.ID, `{"decision":"approve","comment":"lgtm --override-budget"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("override retry status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if stage.State != run.StageStateSucceeded {
		t.Errorf("stage.State after override retry = %q, want succeeded", stage.State)
	}
	// The retry is a genuine first insert, not a duplicate.
	if strings.Contains(w.Body.String(), `"duplicate_submission"`) {
		t.Errorf("override retry body labeled duplicate: %s", w.Body.String())
	}
	var foundOverride, foundSubmitted bool
	for _, e := range au.appended {
		switch e.Category {
		case "plan_budget_override_acknowledged":
			foundOverride = true
		case "approval_submitted":
			foundSubmitted = true
		}
	}
	if !foundOverride || !foundSubmitted {
		t.Errorf("audit missing override ack (%v) or approval_submitted (%v): %+v",
			foundOverride, foundSubmitted, au.appended)
	}
}

func TestSubmitApproval_BudgetCheck_DuplicateAfterOverride_LabeledNoGates(t *testing.T) {
	// codex's failing case from the plan review: an over-budget plan
	// approved with --override-budget, then re-submitted by the same
	// subject WITHOUT the override. The duplicate pre-check answers
	// before any gate runs: labeled duplicate 200, no
	// plan_violates_budget, no new audit entries of any kind.
	art := newFakeArtifactRepo()
	s, rr, au, _ := newBudgetCheckServer(t, art)

	p := &plan.Plan{
		PlanVersion:             "standard_v1",
		PredictedRuntimeMinutes: 20,
	}
	_, stage := seedBudgetRun(t, rr, art, p)

	if w := submitApproval(t, s, stage.ID, `{"decision":"approve","comment":"lgtm --override-budget"}`); w.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	auditAfterFirst := len(au.appended)

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("re-submission status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var got approvalSubmitResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.DuplicateSubmission || got.PriorDecision != "approve" || got.PriorSubmittedAt == "" {
		t.Errorf("duplicate labeling = %+v, want duplicate_submission=true prior_decision=approve prior_submitted_at set", got)
	}
	if stage.State != run.StageStateSucceeded {
		t.Errorf("stage.State = %q, want succeeded (unchanged)", stage.State)
	}
	if len(au.appended) != auditAfterFirst {
		t.Errorf("audit entries after duplicate = %d, want %d (no new entries)", len(au.appended), auditAfterFirst)
	}
	for _, e := range au.appended {
		if e.Category == "plan_violates_budget" {
			t.Errorf("unexpected plan_violates_budget — the duplicate must run no gates")
		}
	}
}

func TestSubmitApproval_Duplicate_LabeledResponse(t *testing.T) {
	// #986: the duplicate 200 carries duplicate_submission/prior_decision/
	// prior_submitted_at from the EXISTING row (approve-then-reject pins
	// provenance: prior_decision is the first submission's), while the
	// first-submission 200 body omits the keys entirely (additive-only
	// contract for existing clients).
	s, _, rr, au := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("first status = %d:\n%s", w.Code, w.Body.String())
	}
	var first map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"duplicate_submission", "prior_decision", "prior_submitted_at"} {
		if _, ok := first[k]; ok {
			t.Errorf("first-submission body must omit %q: %s", k, w.Body.String())
		}
	}

	w = submitApproval(t, s, stage.ID, `{"decision":"reject"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("duplicate status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var got approvalSubmitResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.DuplicateSubmission {
		t.Errorf("duplicate_submission = false, want true")
	}
	if got.PriorDecision != "approve" {
		t.Errorf("prior_decision = %q, want approve (the existing row's, not the new request's)", got.PriorDecision)
	}
	if got.PriorSubmittedAt == "" {
		t.Errorf("prior_submitted_at empty, want the existing row's timestamp")
	}
	if got.State != string(run.StageStateSucceeded) {
		t.Errorf("State = %q, want succeeded (unchanged by the duplicate)", got.State)
	}
	if len(au.appended) != 1 {
		t.Errorf("audit entries = %d, want 1 (no entry for the duplicate)", len(au.appended))
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
// only the bare token subject (no resolved login) renders the subject
// verbatim inside a code span (#1053) — never an `@`-mention. The seam
// crossed is HTTP body → handler → approval_submitted audit payload →
// notifier footer render.
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
	if got := issuecomment.PlanStatusFooterForAuditPayload(rawBare); got != "_Status: approved by `brett@local-mcp` · implementing now_" {
		t.Errorf("bare-subject footer = %q, want the verbatim code-span form (no ping, #1053)", got)
	}
}

// TestSubmitApproval_Delegated_FooterNamesRoleAndRule extends the
// wire→handler→audit-payload→render seam (#751 shape) to the ADR-040
// delegated path (#1053): a delegated approval submitted under the
// operator-agent token subject must render the plan-status footer
// naming the role instance AND the delegation rule from the exact
// `approval_submitted` payload the handler wrote — not the anonymous
// "an approver" reduction that motivated the issue.
func TestSubmitApproval_Delegated_FooterNamesRoleAndRule(t *testing.T) {
	s, repo, au, _, _ := newDelegatedApprovalServer(t)
	runID, planStage := startDriveE2ERun(t, s, repo, map[string]any{
		"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": "cli", "workflow_spec": delegationSpecYAML,
	})

	// Satisfy clean_dual_approval: every configured verdict is an
	// approve and no concern is open.
	seedReviewEntry(t, au, runID, 1, "plan_review_started",
		planreview.ReviewStartedPayload{ConfiguredAgents: 2})
	seedReviewEntry(t, au, runID, 2, "plan_reviewed",
		planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
	seedReviewEntry(t, au, runID, 3, "plan_reviewed",
		planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})

	const subject = "operator-agent/operator-role-v0"
	w := submitApprovalAs(t, s, planStage.ID, subject, `{"decision":"approve","delegated":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	payload := findApprovalSubmittedPayload(t, au.appended)
	if got := payload["approver"]; got != subject {
		t.Errorf("audit approver = %v, want %q", got, subject)
	}
	if got := payload["delegated"]; got != "clean_dual_approval" {
		t.Errorf("audit delegated = %v, want clean_dual_approval", got)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("re-marshal payload: %v", err)
	}
	footer := issuecomment.PlanStatusFooterForAuditPayload(raw)
	want := "_Status: approved by the operator agent (`operator-agent/operator-role-v0`, delegated: `clean_dual_approval`) · implementing now_"
	if footer != want {
		t.Errorf("delegated footer = %q, want %q", footer, want)
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

// --- ADR-036 (#875): plan-approval completion gate -------------------------

// specPlanReviewers builds a feature_change workflow whose plan stage declares
// the given reviewers.agent / reviewers.human counts. agent>0 is what arms the
// completion gate; human distinguishes advisory (human>0) from gating (==0).
func specPlanReviewers(agent, human int) []byte {
	return []byte(fmt.Sprintf(`version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          agent: %d
          human: %d
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
`, agent, human))
}

// newPlanReviewGateServer wires the REAL approval handler against an
// orchestratorRepo whose seeded run carries workflowSpec, plus an auditFake
// that replays seeded + appended entries by category (the count read the gate
// performs). Returns the plan stage parked at awaiting_approval.
func newPlanReviewGateServer(t *testing.T, workflowSpec []byte) (*Server, *orchestratorRepo, *auditFake, *run.Stage) {
	t.Helper()
	rr := newOrchestratorRepo()
	ar := newFakeApprovalRepo()
	au := newAuditFake()
	o := &orchestrator.Orchestrator{Runs: rr}
	runRow := rr.seedRun()
	runRow.WorkflowID = "feature_change"
	runRow.WorkflowSpec = workflowSpec
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateAwaitingApproval)
	s := New(Config{
		Addr:         "127.0.0.1:0",
		ApprovalRepo: ar,
		RunRepo:      rr,
		AuditRepo:    au,
		Orchestrator: o,
	})
	return s, rr, au, planStage
}

// seedReviewAudit appends a review-lifecycle audit entry directly to the fake's
// seeded history so the gate's ListForRunByCategory count reads it back.
func seedReviewAudit(au *auditFake, runID uuid.UUID, category string, ts time.Time) {
	au.mu.Lock()
	defer au.mu.Unlock()
	rid := runID
	au.seeded = append(au.seeded, &audit.Entry{
		RunID:     &rid,
		Category:  category,
		Timestamp: ts,
	})
}

func TestSubmitApproval_PlanReviewGate_InFlight_Refused(t *testing.T) {
	// Advisory reviewers (agent:1, human:1): a plan_review_started entry is
	// present but no terminal verdict has landed → refuse with 409
	// agent_review_pending, no approval row, no stage transition.
	s, rr, au, stage := newPlanReviewGateServer(t, specPlanReviewers(1, 1))
	seedReviewAudit(au, stage.RunID, "plan_review_started", time.Now().UTC())

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"agent_review_pending"`) {
		t.Errorf("error code missing: %s", body)
	}
	if !strings.Contains(body, `"landed_terminal":0`) || !strings.Contains(body, `"configured_agents":1`) {
		t.Errorf("body missing landed/configured detail: %s", body)
	}
	if stage.State != run.StageStateAwaitingApproval {
		t.Errorf("stage.State = %q, want awaiting_approval (refused before submit)", stage.State)
	}
	// Refused before ApprovalRepo.Submit: no approval_submitted audit row.
	for _, e := range au.appended {
		if e.Category == "approval_submitted" {
			t.Errorf("unexpected approval_submitted audit on a refused approval")
		}
	}
	if len(rr.runs) == 0 { // sanity: run still present
		t.Fatal("run vanished")
	}
}

func TestSubmitApproval_PlanReviewGate_TerminalUnblocks(t *testing.T) {
	// Each terminal review kind independently satisfies the gate (agent:1):
	// once one terminal entry of any kind lands, the approve proceeds.
	for _, terminal := range []string{"plan_reviewed", "plan_review_failed", "plan_review_skipped"} {
		t.Run(terminal, func(t *testing.T) {
			s, _, au, stage := newPlanReviewGateServer(t, specPlanReviewers(1, 1))
			now := time.Now().UTC()
			seedReviewAudit(au, stage.RunID, "plan_review_started", now)
			seedReviewAudit(au, stage.RunID, terminal, now)

			w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
			}
			if stage.State != run.StageStateSucceeded {
				t.Errorf("stage.State = %q, want succeeded", stage.State)
			}
		})
	}
}

func TestSubmitApproval_PlanReviewGate_MixedTerminalKinds_Unblock(t *testing.T) {
	// Two configured agents (agent:2): a plan_reviewed + a plan_review_failed
	// sum to the configured count and unblock the approval.
	s, _, au, stage := newPlanReviewGateServer(t, specPlanReviewers(2, 1))
	now := time.Now().UTC()
	seedReviewAudit(au, stage.RunID, "plan_review_started", now)
	seedReviewAudit(au, stage.RunID, "plan_reviewed", now)
	seedReviewAudit(au, stage.RunID, "plan_review_failed", now)

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if stage.State != run.StageStateSucceeded {
		t.Errorf("stage.State = %q, want succeeded", stage.State)
	}
}

func TestSubmitApproval_PlanReviewGate_TwoAgents_OneLanded_StillRefused(t *testing.T) {
	// agent:2 with only ONE terminal entry landed → still in-flight, refuse.
	s, _, au, stage := newPlanReviewGateServer(t, specPlanReviewers(2, 1))
	now := time.Now().UTC()
	seedReviewAudit(au, stage.RunID, "plan_review_started", now)
	seedReviewAudit(au, stage.RunID, "plan_reviewed", now)

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"landed_terminal":1`) {
		t.Errorf("body should report landed_terminal=1: %s", w.Body.String())
	}
}

func TestSubmitApproval_PlanReviewGate_Backstop_AllowsAndLogs(t *testing.T) {
	// A reviewer that died emitting NO terminal entry: only a plan_review_started
	// entry exists, aged past the backstop bound (Cap × agentCount). The gate
	// allows the approval AND records a plan_review_backstop_elapsed degrade.
	s, _, au, stage := newPlanReviewGateServer(t, specPlanReviewers(1, 1))
	// Small Cap so the bound is tiny; the started entry is aged well past it.
	s.cfg.ReviewBudget = planreview.ReviewBudget{Floor: 10 * time.Millisecond, Cap: 50 * time.Millisecond}
	seedReviewAudit(au, stage.RunID, "plan_review_started", time.Now().UTC().Add(-time.Second))

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (backstop elapsed):\n%s", w.Code, w.Body.String())
	}
	if stage.State != run.StageStateSucceeded {
		t.Errorf("stage.State = %q, want succeeded", stage.State)
	}
	var foundBackstop bool
	for _, e := range au.appended {
		if e.Category == "plan_review_backstop_elapsed" {
			foundBackstop = true
			var payload map[string]any
			if err := json.Unmarshal(e.Payload, &payload); err != nil {
				t.Fatalf("unmarshal backstop payload: %v", err)
			}
			if payload["configured_agents"] != float64(1) || payload["landed_terminal"] != float64(0) {
				t.Errorf("backstop payload counts = %v", payload)
			}
		}
	}
	if !foundBackstop {
		t.Errorf("expected plan_review_backstop_elapsed audit entry, got %+v", au.appended)
	}
}

func TestSubmitApproval_PlanReviewGate_NoAgentReviewer_PassThrough(t *testing.T) {
	// agent:0 → the completion gate never arms; a stray plan_review_started
	// entry does not block. Byte-for-byte the pre-ADR-036 approve path.
	s, _, au, stage := newPlanReviewGateServer(t, specPlanReviewers(0, 1))
	seedReviewAudit(au, stage.RunID, "plan_review_started", time.Now().UTC())

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no agent reviewer):\n%s", w.Code, w.Body.String())
	}
	if stage.State != run.StageStateSucceeded {
		t.Errorf("stage.State = %q, want succeeded", stage.State)
	}
}

func TestSubmitApproval_PlanReviewGate_NotDispatched_PassThrough(t *testing.T) {
	// agent:1 but NO plan_review_started entry → the review was configured but
	// never dispatched; nothing to wait for, the approve proceeds.
	s, _, _, stage := newPlanReviewGateServer(t, specPlanReviewers(1, 1))

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (not dispatched):\n%s", w.Code, w.Body.String())
	}
	if stage.State != run.StageStateSucceeded {
		t.Errorf("stage.State = %q, want succeeded", stage.State)
	}
}

func TestSubmitApproval_PlanReviewGate_GatingReviewers_Unaffected(t *testing.T) {
	// Gating reviewers (agent:1, human:0): the gate treats gating identically
	// to advisory — a landed terminal verdict unblocks the approve. The
	// completion gate adds no human>0 special-case, so a properly-sequenced
	// gating run (review lands, then approve) flows through unchanged.
	s, _, au, stage := newPlanReviewGateServer(t, specPlanReviewers(1, 0))
	now := time.Now().UTC()
	seedReviewAudit(au, stage.RunID, "plan_review_started", now)
	seedReviewAudit(au, stage.RunID, "plan_reviewed", now)

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (gating, review landed):\n%s", w.Code, w.Body.String())
	}
	if stage.State != run.StageStateSucceeded {
		t.Errorf("stage.State = %q, want succeeded", stage.State)
	}
}

// TestSubmitApproval_PlanReviewGate_IdempotentRetryAfterLanding pins the
// load-bearing pre-Submit placement (plan risk #1): an approve refused while
// in-flight inserts NO approval row, so the SAME subject's retry once a
// terminal verdict lands flows normally through Submit → advanceStage and the
// stage reaches succeeded. A post-Submit gate would have inserted a row on the
// first refused attempt, stranding the retry on Inserted=false.
func TestSubmitApproval_PlanReviewGate_IdempotentRetryAfterLanding(t *testing.T) {
	s, _, au, stage := newPlanReviewGateServer(t, specPlanReviewers(1, 1))
	now := time.Now().UTC()
	seedReviewAudit(au, stage.RunID, "plan_review_started", now)

	// First attempt: in-flight → refused.
	if w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`); w.Code != http.StatusConflict {
		t.Fatalf("first attempt status = %d, want 409:\n%s", w.Code, w.Body.String())
	}

	// Terminal verdict lands; the same subject retries.
	seedReviewAudit(au, stage.RunID, "plan_reviewed", now)
	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if stage.State != run.StageStateSucceeded {
		t.Errorf("stage.State = %q, want succeeded after retry", stage.State)
	}
}

// The three tests below pin the fail-OPEN posture of checkPlanReviewSettled's
// error branches (the #875 implement-review gap): every read failure WARN-logs
// and returns true so a transient backend hiccup can never brick the approval
// gate. Each asserts the approve proceeds (200, stage succeeded) despite the
// injected read error, mirroring checkPlanBudget / checkApproverAuthorization.

func TestSubmitApproval_PlanReviewGate_GetRunError_FailsOpen(t *testing.T) {
	// GetRun fails (run row gone) → the gate cannot resolve reviewers, so it
	// fails open and the approve proceeds rather than stranding on a transient
	// read error.
	s, rr, _, stage := newPlanReviewGateServer(t, specPlanReviewers(1, 1))
	rr.mu.Lock()
	delete(rr.runs, stage.RunID) // GetRun now returns run.ErrNotFound
	rr.mu.Unlock()

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (GetRun failure fails open):\n%s", w.Code, w.Body.String())
	}
	if stage.State != run.StageStateSucceeded {
		t.Errorf("stage.State = %q, want succeeded", stage.State)
	}
}

func TestSubmitApproval_PlanReviewGate_StartedListError_FailsOpen(t *testing.T) {
	// The plan_review_started count read fails → fail open, approve proceeds.
	s, _, au, stage := newPlanReviewGateServer(t, specPlanReviewers(1, 1))
	au.listByCategoryErr = errors.New("audit read boom")

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (started-list failure fails open):\n%s", w.Code, w.Body.String())
	}
	if stage.State != run.StageStateSucceeded {
		t.Errorf("stage.State = %q, want succeeded", stage.State)
	}
}

func TestSubmitApproval_PlanReviewGate_TerminalListError_FailsOpen(t *testing.T) {
	// plan_review_started reads fine (review dispatched), but the terminal-
	// category count read fails → fail open, approve proceeds. A categoryErrAudit
	// fails ONLY the terminal categories so the started read still returns the
	// seeded dispatch entry, isolating the terminal-list branch.
	rr := newOrchestratorRepo()
	ar := newFakeApprovalRepo()
	au := &categoryErrAudit{
		auditFake: newAuditFake(),
		errFor: map[string]error{
			"plan_reviewed":       errors.New("terminal read boom"),
			"plan_review_failed":  errors.New("terminal read boom"),
			"plan_review_skipped": errors.New("terminal read boom"),
		},
	}
	o := &orchestrator.Orchestrator{Runs: rr}
	runRow := rr.seedRun()
	runRow.WorkflowID = "feature_change"
	runRow.WorkflowSpec = specPlanReviewers(1, 1)
	stage := rr.seedStage(runRow.ID, 0, run.StageStateAwaitingApproval)
	s := New(Config{
		Addr:         "127.0.0.1:0",
		ApprovalRepo: ar,
		RunRepo:      rr,
		AuditRepo:    au,
		Orchestrator: o,
	})
	seedReviewAudit(au.auditFake, stage.RunID, "plan_review_started", time.Now().UTC())

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (terminal-list failure fails open):\n%s", w.Code, w.Body.String())
	}
	if stage.State != run.StageStateSucceeded {
		t.Errorf("stage.State = %q, want succeeded", stage.State)
	}
}

// --- Scope-cap gate (#983) ---

// newScopeCapServer wires the full approval path against a run whose
// cached workflow spec sets implement-stage max_files_changed (3 via
// specImplementPathConstraints) and whose plan stage carries a plan
// artifact with the given scope.files. Returns every fake so tests can
// assert refused approvals insert NO row (the gate is PRE-Submit).
func newScopeCapServer(t *testing.T, workflowSpec []byte, scopeFiles []plan.ScopeFile) (*Server, *fakeApprovalRepo, *orchestratorRepo, *approvalAuditFake, *run.Stage) {
	t.Helper()
	rr := newOrchestratorRepo()
	art := newFakeArtifactRepo()
	app := newFakeApprovalRepo()
	au := newApprovalAuditFake()
	o := &orchestrator.Orchestrator{Runs: rr}

	runRow := rr.seedRun()
	runRow.WorkflowID = "feature_change"
	runRow.WorkflowSpec = workflowSpec
	stage := rr.seedStage(runRow.ID, 0, run.StageStateAwaitingApproval)
	if scopeFiles != nil {
		seedBudgetPlanArtifact(t, art, stage.ID, &plan.Plan{
			PlanVersion: "standard_v1",
			Scope:       plan.Scope{Files: scopeFiles},
		})
	}

	s := New(Config{
		Addr:         "127.0.0.1:0",
		ApprovalRepo: app,
		RunRepo:      rr,
		AuditRepo:    au,
		Orchestrator: o,
		ArtifactRepo: art,
	})
	return s, app, rr, au, stage
}

func scopeCapFiles(n int) []plan.ScopeFile {
	out := make([]plan.ScopeFile, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, plan.ScopeFile{
			Path:      fmt.Sprintf("backend/file%d.go", i),
			Operation: plan.FileOpModify,
		})
	}
	return out
}

// TestSubmitApproval_ScopeCap_OverCapPlanAlone_Returns422AndRetryWithOverrideSucceeds
// is the #983 end-to-end gate test: an over-cap plan (4 files vs cap 3)
// is refused 422 plan_violates_scope_cap, the refusal appends the audit
// entry and inserts NO approval row (PRE-Submit — the ADR-036
// stranded-retry hazard), and an immediate retry with
// --override-scope-cap succeeds, advances the stage, and lands
// plan_scope_cap_override_acknowledged.
func TestSubmitApproval_ScopeCap_OverCapPlanAlone_Returns422AndRetryWithOverrideSucceeds(t *testing.T) {
	s, app, _, au, stage := newScopeCapServer(t, specImplementPathConstraints, scopeCapFiles(4))

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"plan_violates_scope_cap"`) {
		t.Errorf("body missing plan_violates_scope_cap: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"scoped_files":4`) ||
		!strings.Contains(w.Body.String(), `"max_files_changed":3`) {
		t.Errorf("details missing scoped_files/max_files_changed: %s", w.Body.String())
	}
	if stage.State != run.StageStateAwaitingApproval {
		t.Errorf("stage.State = %q, want awaiting_approval (refusal must not advance)", stage.State)
	}
	app.mu.Lock()
	rows := len(app.all)
	app.mu.Unlock()
	if rows != 0 {
		t.Fatalf("approval rows = %d, want 0 (PRE-Submit refusal must insert nothing)", rows)
	}
	var foundViolation bool
	for _, e := range au.appended {
		if e.Category == "plan_violates_scope_cap" {
			foundViolation = true
		}
		if e.Category == "approval_submitted" {
			t.Errorf("unexpected approval_submitted audit entry on refusal")
		}
	}
	if !foundViolation {
		t.Errorf("expected plan_violates_scope_cap audit entry, got %+v", au.appended)
	}

	// Immediate retry with the override must flow through Submit normally.
	w = submitApproval(t, s, stage.ID, `{"decision":"approve","comment":"cap is about to change --override-scope-cap"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("override retry status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if stage.State != run.StageStateSucceeded {
		t.Errorf("stage.State = %q, want succeeded after override", stage.State)
	}
	app.mu.Lock()
	rows = len(app.all)
	app.mu.Unlock()
	if rows != 1 {
		t.Errorf("approval rows = %d, want 1 after override retry", rows)
	}
	var foundAck bool
	for _, e := range au.appended {
		if e.Category == "plan_scope_cap_override_acknowledged" {
			foundAck = true
		}
	}
	if !foundAck {
		t.Errorf("expected plan_scope_cap_override_acknowledged audit entry, got %+v", au.appended)
	}
}

// TestSubmitApproval_ScopeCap_AddScopeFilesPushOverCap_Returns422 covers
// the run-575e6a74 shape: the plan is AT the cap (3/3, precheck clean)
// and the approval-time add_scope_files fold pushes the effective scope
// past it.
func TestSubmitApproval_ScopeCap_AddScopeFilesPushOverCap_Returns422(t *testing.T) {
	s, app, _, _, stage := newScopeCapServer(t, specImplementPathConstraints, scopeCapFiles(3))

	w := submitApproval(t, s, stage.ID,
		`{"decision":"approve","add_scope_files":["backend/extra.go"]}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"plan_violates_scope_cap"`) {
		t.Errorf("body missing plan_violates_scope_cap: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"add_scope_files_count":1`) {
		t.Errorf("details missing add_scope_files_count: %s", w.Body.String())
	}
	app.mu.Lock()
	rows := len(app.all)
	app.mu.Unlock()
	if rows != 0 {
		t.Errorf("approval rows = %d, want 0", rows)
	}
}

// TestSubmitApproval_ScopeCap_AddScopeFilesDedupedAgainstPlan asserts
// the gate counts by exact path the way foldScopePaths dedupes: an
// add_scope_files entry already in the plan scope does not consume
// headroom.
func TestSubmitApproval_ScopeCap_AddScopeFilesDedupedAgainstPlan(t *testing.T) {
	s, _, _, _, stage := newScopeCapServer(t, specImplementPathConstraints, scopeCapFiles(3))

	w := submitApproval(t, s, stage.ID,
		`{"decision":"approve","add_scope_files":["backend/file0.go"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (duplicate path must not push over cap):\n%s", w.Code, w.Body.String())
	}
}

// TestSubmitApproval_ScopeCap_UnderCap_Proceeds asserts the happy path
// is untouched: under-cap approves flow with no scope-cap audit noise.
func TestSubmitApproval_ScopeCap_UnderCap_Proceeds(t *testing.T) {
	s, _, _, au, stage := newScopeCapServer(t, specImplementPathConstraints, scopeCapFiles(2))

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	for _, e := range au.appended {
		if e.Category == "plan_violates_scope_cap" || e.Category == "plan_scope_cap_override_acknowledged" {
			t.Errorf("unexpected %s audit entry on an under-cap approve", e.Category)
		}
	}
}

// TestSubmitApproval_ScopeCap_NoCapConfigured_Proceeds asserts a
// workflow without max_files_changed never trips the gate regardless
// of scope size.
func TestSubmitApproval_ScopeCap_NoCapConfigured_Proceeds(t *testing.T) {
	specNoCap := []byte(`version: "0.3"
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
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - forbidden_paths:
              - ".github/workflows/**"
`)
	s, _, _, _, stage := newScopeCapServer(t, specNoCap, scopeCapFiles(50))

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no cap configured):\n%s", w.Code, w.Body.String())
	}
}

// TestSubmitApproval_ScopeCap_NoPlanArtifact_FailsOpen asserts the
// fail-open contract: a run with a cap but no readable plan artifact
// skips the check rather than bricking the gate.
func TestSubmitApproval_ScopeCap_NoPlanArtifact_FailsOpen(t *testing.T) {
	s, _, _, au, stage := newScopeCapServer(t, specImplementPathConstraints, nil)

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fail-open with no plan):\n%s", w.Code, w.Body.String())
	}
	for _, e := range au.appended {
		if e.Category == "plan_violates_scope_cap" {
			t.Errorf("unexpected plan_violates_scope_cap on fail-open")
		}
	}
}

// driveAdvanceFor decodes the run_auto_advanced entries from the audit
// fake's appended params, asserting the drive payload shape (#1023).
func driveAdvanceFor(t *testing.T, au *approvalAuditFake) []drive.Advance {
	t.Helper()
	var out []drive.Advance
	for _, e := range au.appended {
		if e.Category != drive.Category {
			continue
		}
		var adv drive.Advance
		if err := json.Unmarshal(e.Payload, &adv); err != nil {
			t.Fatalf("run_auto_advanced payload unmarshal: %v", err)
		}
		out = append(out, adv)
	}
	return out
}

// TestSubmitApproval_Drive_GitHubActions_StampsAutoAdvance pins the
// plan_approved_dispatch stamp (#1023): a plan-gate approval on a
// drive-enabled github_actions run records the auto-advance the
// orchestrator handoff performs.
func TestSubmitApproval_Drive_GitHubActions_StampsAutoAdvance(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)
	rr.seedRun(&run.Run{ID: stage.RunID, Drive: true, RunnerKind: run.RunnerKindGitHubActions})

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	advances := driveAdvanceFor(t, au)
	if len(advances) != 1 {
		t.Fatalf("run_auto_advanced entries = %d, want 1 (%+v)", len(advances), advances)
	}
	adv := advances[0]
	if adv.Rule != drive.RulePlanApprovedDispatch {
		t.Errorf("Rule = %q, want plan_approved_dispatch", adv.Rule)
	}
	if adv.Parked {
		t.Error("Parked = true, want false: github_actions dispatch is the auto-advance")
	}
	if adv.To != "implement:dispatched" {
		t.Errorf("To = %q, want implement:dispatched", adv.To)
	}
	if adv.NextAction != nil {
		t.Errorf("NextAction = %+v, want nil", adv.NextAction)
	}
}

// TestSubmitApproval_Drive_Local_ParksWithNextAction pins the
// runner_kind local branch: the backend cannot spawn the host-side
// runner (ADR-024), so the stamp records a park with a ready-to-run
// next action instead of an executed dispatch.
func TestSubmitApproval_Drive_Local_ParksWithNextAction(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)
	rr.seedRun(&run.Run{ID: stage.RunID, Drive: true, RunnerKind: run.RunnerKindLocal})

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	advances := driveAdvanceFor(t, au)
	if len(advances) != 1 {
		t.Fatalf("run_auto_advanced entries = %d, want 1 (%+v)", len(advances), advances)
	}
	adv := advances[0]
	if adv.Rule != drive.RulePlanApprovedDispatch {
		t.Errorf("Rule = %q, want plan_approved_dispatch", adv.Rule)
	}
	if !adv.Parked {
		t.Error("Parked = false, want true for runner_kind local")
	}
	if adv.NextAction == nil || adv.NextAction.Action != "run_implement_stage" {
		t.Fatalf("NextAction = %+v, want action run_implement_stage", adv.NextAction)
	}
}

// TestSubmitApproval_NonDrive_NoAutoAdvanceStamp is the control: the
// same approval on a drive:false run leaves the audit trail exactly
// as before #1023 — no run_auto_advanced entry.
func TestSubmitApproval_NonDrive_NoAutoAdvanceStamp(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)
	rr.seedRun(&run.Run{ID: stage.RunID, Drive: false, RunnerKind: run.RunnerKindGitHubActions})

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if advances := driveAdvanceFor(t, au); len(advances) != 0 {
		t.Errorf("run_auto_advanced entries = %+v, want none on a non-drive run", advances)
	}
}

// TestSubmitApproval_Drive_Reject_NoStamp asserts a rejection never
// stamps plan_approved_dispatch — only an approve is the transition.
func TestSubmitApproval_Drive_Reject_NoStamp(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)
	rr.seedRun(&run.Run{ID: stage.RunID, Drive: true, RunnerKind: run.RunnerKindGitHubActions})

	w := submitApproval(t, s, stage.ID, `{"decision":"reject"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if advances := driveAdvanceFor(t, au); len(advances) != 0 {
		t.Errorf("run_auto_advanced entries = %+v, want none on reject", advances)
	}
}

// --- Delegated approval (ADR-040 / #1026) -----------------------------------

// newDelegatedApprovalServer wires the full stack the delegated
// approval path reads: the drive-capable repo (working
// ListStagesForRun), audit + concern fakes for the delegation
// evaluator, the approval repo, and the orchestrator for the
// post-approve dispatch.
func newDelegatedApprovalServer(t *testing.T) (*Server, *driveE2ERepo, *auditFake, *fakeConcernRepo, *fakeApprovalRepo) {
	t.Helper()
	repo := &driveE2ERepo{fakeRepo: newFakeRepo()}
	au := newAuditFake()
	cr := newFakeConcernRepo()
	ar := newFakeApprovalRepo()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      repo,
		AuditRepo:    au,
		ConcernRepo:  cr,
		ApprovalRepo: ar,
		Orchestrator: &orchestrator.Orchestrator{Runs: repo},
	})
	return s, repo, au, cr, ar
}

// decodeErrorEnvelope unmarshals a non-2xx response body.
func decodeErrorEnvelope(t *testing.T, w *httptest.ResponseRecorder) errorBody {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal error envelope: %v\n%s", err, w.Body.String())
	}
	return env.Error
}

// delegatedAuditRule extracts the `delegated` payload field from the
// single appended entry of the given category, or "" when absent.
func delegatedAuditRule(t *testing.T, au *auditFake, category string) string {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	var match *audit.ChainAppendParams
	for i := range au.appended {
		if au.appended[i].Category == category {
			if match != nil {
				t.Fatalf("more than one %s entry appended", category)
			}
			match = &au.appended[i]
		}
	}
	if match == nil {
		t.Fatalf("no %s entry appended", category)
	}
	var payload struct {
		Delegated string `json:"delegated"`
	}
	if err := json.Unmarshal(match.Payload, &payload); err != nil {
		t.Fatalf("unmarshal %s payload: %v", category, err)
	}
	return payload.Delegated
}

// TestSubmitApproval_Delegated_EndToEnd is the slice's required
// cross-boundary test: a run whose cached workflow spec carries an
// operator_agent block (may_approve: clean_dual_approval) refuses a
// delegated approval while the condition is unmet — naming the exact
// failed predicate, inserting no approval row — and accepts it once
// every reviewer verdict is an approve and no concern is open, stamping
// `delegated: "clean_dual_approval"` into the approval_submitted audit
// payload.
func TestSubmitApproval_Delegated_EndToEnd(t *testing.T) {
	s, repo, au, cr, ar := newDelegatedApprovalServer(t)
	runID, planStage := startDriveE2ERun(t, s, repo, map[string]any{
		"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": "cli", "workflow_spec": delegationSpecYAML,
	})

	// Phase 1: no reviewer verdicts yet — refused, predicate named.
	w := submitApproval(t, s, planStage.ID, `{"decision":"approve","delegated":true}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	errBody := decodeErrorEnvelope(t, w)
	if errBody.Code != "delegation_condition_unmet" {
		t.Fatalf("code = %q, want delegation_condition_unmet", errBody.Code)
	}
	reason, _ := errBody.Details["unmet_reason"].(string)
	if !strings.Contains(reason, "clean_dual_approval") || !strings.Contains(reason, "0 of 2 reviewer verdicts") {
		t.Errorf("unmet_reason = %q, want the named verdict-count predicate", reason)
	}
	if rows, _ := ar.ListForStage(context.Background(), planStage.ID); len(rows) != 0 {
		t.Fatalf("approval rows = %d after refusal, want 0 (a refused delegation must insert no row)", len(rows))
	}

	// Phase 2: both verdicts in, but a concern is open — still refused.
	seedReviewEntry(t, au, runID, 1, "plan_review_started",
		planreview.ReviewStartedPayload{ConfiguredAgents: 2})
	seedReviewEntry(t, au, runID, 2, "plan_reviewed",
		planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
	seedReviewEntry(t, au, runID, 3, "plan_reviewed",
		planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
	openRow := seedConcernRow(t, cr, runID, planStage.ID, "plan", 2, "tighten the integration test")

	w = submitApproval(t, s, planStage.ID, `{"decision":"approve","delegated":true}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	errBody = decodeErrorEnvelope(t, w)
	reason, _ = errBody.Details["unmet_reason"].(string)
	if errBody.Code != "delegation_condition_unmet" || !strings.Contains(reason, "1 open concern(s)") {
		t.Errorf("error = %+v, want delegation_condition_unmet on the open concern", errBody)
	}

	// Phase 3: concern resolved — the delegated approval proceeds and
	// the audit payload carries the rule.
	if err := cr.MarkAddressedPending(context.Background(), []uuid.UUID{openRow.ID}, "routed"); err != nil {
		t.Fatalf("MarkAddressedPending: %v", err)
	}
	if _, err := cr.ApplyResolution(context.Background(), openRow.ID, concern.StateAddressed, "confirmed"); err != nil {
		t.Fatalf("ApplyResolution: %v", err)
	}
	w = submitApproval(t, s, planStage.ID, `{"decision":"approve","delegated":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var got stageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.State != string(run.StageStateSucceeded) {
		t.Errorf("stage state = %q, want succeeded", got.State)
	}
	if rule := delegatedAuditRule(t, au, "approval_submitted"); rule != "clean_dual_approval" {
		t.Errorf("audit delegated = %q, want clean_dual_approval", rule)
	}
}

// TestSubmitApproval_Delegated_NotConfigured pins fail-closed: a spec
// with NO operator_agent block refuses a delegated approval outright
// with delegation_not_configured, even though a plain human approval of
// the same stage would proceed.
func TestSubmitApproval_Delegated_NotConfigured(t *testing.T) {
	s, repo, _, _, ar := newDelegatedApprovalServer(t)
	_, planStage := startDriveE2ERun(t, s, repo, map[string]any{
		"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": "cli", "workflow_spec": gatedSpecYAML,
	})

	w := submitApproval(t, s, planStage.ID, `{"decision":"approve","delegated":true}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if errBody := decodeErrorEnvelope(t, w); errBody.Code != "delegation_not_configured" {
		t.Errorf("code = %q, want delegation_not_configured", errBody.Code)
	}
	if rows, _ := ar.ListForStage(context.Background(), planStage.ID); len(rows) != 0 {
		t.Errorf("approval rows = %d after refusal, want 0", len(rows))
	}
}

// TestSubmitApproval_Delegated_RejectRefused: delegation covers the
// approve verb only — a delegated reject is a contradiction
// (reviewer_reject pages the human) and is a 400 before any
// evaluation.
func TestSubmitApproval_Delegated_RejectRefused(t *testing.T) {
	s, repo, _, _, _ := newDelegatedApprovalServer(t)
	_, planStage := startDriveE2ERun(t, s, repo, map[string]any{
		"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": "cli", "workflow_spec": delegationSpecYAML,
	})

	w := submitApproval(t, s, planStage.ID, `{"decision":"reject","delegated":true,"comment":"no"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if errBody := decodeErrorEnvelope(t, w); errBody.Code != "validation_failed" {
		t.Errorf("code = %q, want validation_failed", errBody.Code)
	}
}

// TestSubmitApproval_NoDelegatedField_Unchanged pins the opt-in
// contract: without `delegated`, a human approval on a
// delegation-configured spec proceeds exactly as today — no delegation
// evaluation gates it (the condition is UNMET here: no verdicts) and
// the audit payload carries no `delegated` key.
func TestSubmitApproval_NoDelegatedField_Unchanged(t *testing.T) {
	s, repo, au, _, _ := newDelegatedApprovalServer(t)
	_, planStage := startDriveE2ERun(t, s, repo, map[string]any{
		"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": "cli", "workflow_spec": delegationSpecYAML,
	})

	w := submitApproval(t, s, planStage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	au.mu.Lock()
	defer au.mu.Unlock()
	for _, e := range au.appended {
		if e.Category != "approval_submitted" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal(e.Payload, &raw); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if _, present := raw["delegated"]; present {
			t.Errorf("delegated key present on a non-delegated approval payload: %s", e.Payload)
		}
		return
	}
	t.Fatal("no approval_submitted entry appended")
}

// TestSubmitApproval_OperatorAgentActorAttribution is the ADR-040 D4
// done-means clause (#1027): a decision taken under an operator-agent
// token and one taken by a human on the SAME run must be
// distinguishable on the audit chain — actor_kind=agent vs user, with
// actor_subject carrying the full token subject in both cases.
// Exercised across the wire → handler → audit-repo seam.
func TestSubmitApproval_OperatorAgentActorAttribution(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	agentStage := rr.seedStage(run.StageStateAwaitingApproval)
	humanStage := rr.seedStage(run.StageStateAwaitingApproval)
	rr.mu.Lock()
	humanStage.RunID = agentStage.RunID
	humanStage.Sequence = 1
	rr.mu.Unlock()

	// Decision by the role instance, via its bearer token.
	url := fmt.Sprintf("/v0/stages/%s/approvals", agentStage.ID)
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(`{"decision":"approve"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("stage_id", agentStage.ID.String())
	w := httptest.NewRecorder()
	s.handleSubmitApproval(w, withOperatorAgentAuth(req))
	if w.Code != http.StatusOK {
		t.Fatalf("agent-token approve status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	// Decision by a human on the same run.
	w = submitApproval(t, s, humanStage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("human approve status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	if len(au.appended) != 2 {
		t.Fatalf("audit entries = %d, want 2", len(au.appended))
	}
	agentEntry, humanEntry := au.appended[0], au.appended[1]
	if agentEntry.RunID != humanEntry.RunID {
		t.Fatalf("entries landed on different runs: %s vs %s", agentEntry.RunID, humanEntry.RunID)
	}
	if agentEntry.ActorKind == nil || *agentEntry.ActorKind != audit.ActorAgent {
		t.Errorf("agent-token entry ActorKind = %v, want agent", agentEntry.ActorKind)
	}
	if agentEntry.ActorSubject == nil || *agentEntry.ActorSubject != operatorAgentSubject {
		t.Errorf("agent-token entry ActorSubject = %v, want %q", agentEntry.ActorSubject, operatorAgentSubject)
	}
	if humanEntry.ActorKind == nil || *humanEntry.ActorKind != audit.ActorUser {
		t.Errorf("human entry ActorKind = %v, want user", humanEntry.ActorKind)
	}
	if humanEntry.ActorSubject == nil || *humanEntry.ActorSubject != testOperatorIdentity().Subject {
		t.Errorf("human entry ActorSubject = %v, want %q", humanEntry.ActorSubject, testOperatorIdentity().Subject)
	}
}

// --- Model validity gate (#1339) ---------------------------------------------

// newModelValidityServer mirrors newModelGateServer but injects a snapshot
// oracle and leaves the allow-list empty, so the VALIDITY layer (not the
// allow-list) is what gates. A fresh+ok oracle keyed by "claudecode" exercises
// the layered validity -> policy ordering end to end via the approval path.
func newModelValidityServer(t *testing.T, art artifact.Repository, oracle modeloracle.ModelOracle) (*Server, *orchestratorRepo, *approvalAuditFake, *fakeApprovalRepo) {
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
		ArtifactRepo: art,
		ModelOracle:  oracle,
	})
	return s, rr, au, app
}

// reject: a spec executor.model absent from a fresh+ok snapshot is refused 422
// model_invalid at the approval gate with NO approval row and NO model_resolved
// audit (PRE-Submit), proving the validity layer runs before the allow-list.
func TestSubmitApproval_ModelValidity_RejectOnFreshAbsence(t *testing.T) {
	oracle := modeloracle.Static{
		Models: map[string][]string{"claudecode": {"claude-opus-4-8"}},
		Fresh:  true,
	}
	art := newFakeArtifactRepo()
	s, rr, au, app := newModelValidityServer(t, art, oracle)
	r, stage := seedBudgetRun(t, rr, art, planWithRecommendation(""))
	r.WorkflowSpec = specImplementAgentModel("claude-code", "claude-typo-9")

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"model_invalid"`) {
		t.Errorf("body missing model_invalid: %s", w.Body.String())
	}
	if rows, err := app.ListForStage(context.Background(), stage.ID); err != nil || len(rows) != 0 {
		t.Errorf("approval rows after 422 = %d (err=%v), want 0", len(rows), err)
	}
	for _, e := range au.appended {
		if e.Category == CategoryModelResolved || e.Category == "approval_submitted" {
			t.Errorf("unexpected %s audit entry after a 422", e.Category)
		}
	}
}

// accept: a spec model present in a fresh+ok snapshot passes the validity layer
// and the (empty) allow-list, approving 200.
func TestSubmitApproval_ModelValidity_AcceptOnFreshPresent(t *testing.T) {
	oracle := modeloracle.Static{
		Models: map[string][]string{"claudecode": {"claude-opus-4-8"}},
		Fresh:  true,
	}
	art := newFakeArtifactRepo()
	s, rr, au, _ := newModelValidityServer(t, art, oracle)
	r, stage := seedBudgetRun(t, rr, art, planWithRecommendation(""))
	r.WorkflowSpec = specImplementAgentModel("claude-code", "claude-opus-4-8")

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	_ = au
}

// fail-open-stale: a stale snapshot (Fresh=false) cannot reject the unknown
// model — the approval proceeds to 200.
func TestSubmitApproval_ModelValidity_FailOpenStale(t *testing.T) {
	oracle := modeloracle.Static{
		Models: map[string][]string{"claudecode": {"claude-opus-4-8"}},
		Fresh:  false,
	}
	art := newFakeArtifactRepo()
	s, rr, _, _ := newModelValidityServer(t, art, oracle)
	r, stage := seedBudgetRun(t, rr, art, planWithRecommendation(""))
	r.WorkflowSpec = specImplementAgentModel("claude-code", "claude-typo-9")

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stale → fail open):\n%s", w.Code, w.Body.String())
	}
}

// fail-open-no-snapshot: with no oracle wired (the existing allow-list tests'
// posture) the validity layer is inert — an unknown model is NOT rejected by it.
func TestSubmitApproval_ModelValidity_FailOpenNoOracle(t *testing.T) {
	art := newFakeArtifactRepo()
	s, rr, _, _ := newModelValidityServer(t, art, nil)
	r, stage := seedBudgetRun(t, rr, art, planWithRecommendation(""))
	r.WorkflowSpec = specImplementAgentModel("claude-code", "anything-goes")

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (nil oracle → fail open):\n%s", w.Code, w.Body.String())
	}
}

// --- Deploy stage pre-execution gate (ADR-038 / E23.4 / #1384) ---

// Deploy gate spec fixtures: each isolates one pre-flight constraint kind so a
// behavioral test can assert exactly one branch of checkDeployPreflight. The
// workflow id is "release" throughout.

const deploySpecEnvOnly = `
version: "1.0"
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
        constraints:
          - allowed_environments: [production, staging]
        produces:
          - artifact: deployment
`

const deploySpecFreezeOnly = `
version: "1.0"
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
        constraints:
          - change_freeze: true
`

const deploySpecUpstreamReviewMerged = `
version: "1.0"
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
        constraints:
          - required_upstream: [review_merged]
`

const deploySpecUpstreamCIGreen = `
version: "1.0"
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
        constraints:
          - required_upstream: [ci_green]
`

const deploySpecNoConstraints = `
version: "1.0"
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
`

const deploySpecAllConstraints = `
version: "1.0"
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
        constraints:
          - allowed_environments: [production]
          - change_freeze: true
          - required_upstream: [review_merged]
        produces:
          - artifact: deployment
`

// a non-deploy workflow: used for the fail-closed "deploy stage absent" branch.
const deploySpecNoDeployStage = `
version: "1.0"
workflows:
  release:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`

// seedDeployRun stands up a deploy stage parked at awaiting_deploy_approval and
// its run (carrying specYAML as the cached workflow spec) on a shared run id.
func seedDeployRun(rr *approvalRunRepo, workflowID, specYAML string) (*run.Stage, *run.Run) {
	runID := uuid.New()
	st := &run.Stage{
		ID:               uuid.New(),
		RunID:            runID,
		Sequence:         0,
		Type:             run.StageTypeDeploy,
		ExecutorKind:     run.ExecutorAgent,
		ExecutorRef:      "deploy",
		State:            run.StageStateAwaitingDeployApproval,
		RequiresApproval: true,
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
	}
	rr.mu.Lock()
	rr.stages[st.ID] = st
	rr.mu.Unlock()
	runRow := &run.Run{
		ID:           runID,
		Repo:         "kuhlman-labs/example",
		WorkflowID:   workflowID,
		WorkflowSHA:  "sha",
		WorkflowSpec: []byte(specYAML),
	}
	rr.seedRun(runRow)
	return st, runRow
}

// seedStageOnRun adds an extra stage (e.g. a succeeded review stage) to an
// existing run so the review_merged proxy can find it.
func (r *approvalRunRepo) seedStageOnRun(runID uuid.UUID, typ run.StageType, state run.StageState) *run.Stage {
	st := &run.Stage{
		ID:        uuid.New(),
		RunID:     runID,
		Type:      typ,
		State:     state,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	r.mu.Lock()
	r.stages[st.ID] = st
	r.mu.Unlock()
	return st
}

// countAppendedCategory returns how many entries of the given category the
// approvalAuditFake recorded.
func countAppendedCategory(au *approvalAuditFake, category string) int {
	au.mu.Lock()
	defer au.mu.Unlock()
	n := 0
	for _, p := range au.appended {
		if p.Category == category {
			n++
		}
	}
	return n
}

// assertDeployRefused asserts a 422 with the expected code, a
// deploy_preflight_refused audit, and that the deploy stage did NOT advance
// (still parked at awaiting_deploy_approval — a pre-Submit refusal records no
// approval row).
func assertDeployRefused(t *testing.T, w *httptest.ResponseRecorder, rr *approvalRunRepo, au *approvalAuditFake, stage *run.Stage, wantCode string) {
	t.Helper()
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	body := decodeErrorEnvelope(t, w)
	if body.Code != wantCode {
		t.Errorf("error code = %q, want %q", body.Code, wantCode)
	}
	if got := countAppendedCategory(au, "deploy_preflight_refused"); got != 1 {
		t.Errorf("deploy_preflight_refused audit entries = %d, want 1", got)
	}
	if cur, _ := rr.GetStage(context.Background(), stage.ID); cur.State != run.StageStateAwaitingDeployApproval {
		t.Errorf("deploy stage advanced to %q on a refused approval; want it parked at awaiting_deploy_approval", cur.State)
	}
}

// (a) allowed_environments — a requested env not in the allow-list refuses.
func TestDeployGate_DisallowedEnvironment(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stage, _ := seedDeployRun(rr, "release", deploySpecEnvOnly)
	w := submitApproval(t, s, stage.ID, `{"decision":"approve","comment":"--environment=qa"}`)
	assertDeployRefused(t, w, rr, au, stage, "deploy_environment_not_allowed")
}

// (b) allowed_environments — no --environment flag when the constraint is set
// refuses (empty env is not a member).
func TestDeployGate_MissingEnvironment(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stage, _ := seedDeployRun(rr, "release", deploySpecEnvOnly)
	w := submitApproval(t, s, stage.ID, `{"decision":"approve","comment":"ship it"}`)
	assertDeployRefused(t, w, rr, au, stage, "deploy_environment_not_allowed")
}

// (c) change_freeze active without --override-freeze refuses.
func TestDeployGate_ChangeFreezeActive(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stage, _ := seedDeployRun(rr, "release", deploySpecFreezeOnly)
	w := submitApproval(t, s, stage.ID, `{"decision":"approve","comment":"deploy now"}`)
	assertDeployRefused(t, w, rr, au, stage, "deploy_change_freeze_active")
}

// (d) change_freeze WITH --override-freeze proceeds (no other constraints).
func TestDeployGate_ChangeFreezeOverride_Proceeds(t *testing.T) {
	s, _, rr, _ := newApprovalServer(t)
	stage, _ := seedDeployRun(rr, "release", deploySpecFreezeOnly)
	w := submitApproval(t, s, stage.ID, `{"decision":"approve","comment":"--override-freeze emergency hotfix"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (override-freeze proceeds):\n%s", w.Code, w.Body.String())
	}
	// A deploy approve advances the stage to dispatched, NOT succeeded.
	cur, _ := rr.GetStage(context.Background(), stage.ID)
	if cur.State != run.StageStateDispatched {
		t.Errorf("deploy stage state = %q, want dispatched", cur.State)
	}
}

// (e) required_upstream review_merged unmet (no PR url) refuses.
func TestDeployGate_RequiredUpstreamReviewMergedUnmet(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stage, _ := seedDeployRun(rr, "release", deploySpecUpstreamReviewMerged)
	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	assertDeployRefused(t, w, rr, au, stage, "deploy_upstream_not_satisfied")
}

// (f) required_upstream ci_green unmet (no StageCheckRepo / snapshot) refuses.
func TestDeployGate_RequiredUpstreamCIGreenUnmet(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stage, _ := seedDeployRun(rr, "release", deploySpecUpstreamCIGreen)
	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	assertDeployRefused(t, w, rr, au, stage, "deploy_upstream_not_satisfied")
}

// (g) all constraints satisfied → happy path proceeds to dispatch.
func TestDeployGate_AllConstraintsSatisfied(t *testing.T) {
	s, _, rr, _ := newApprovalServer(t)
	stage, runRow := seedDeployRun(rr, "release", deploySpecAllConstraints)
	// Satisfy review_merged: a PR url plus a succeeded review stage.
	prURL := "https://github.com/kuhlman-labs/example/pull/7"
	runRow.PullRequestURL = &prURL
	rr.seedStageOnRun(runRow.ID, run.StageTypeReview, run.StageStateSucceeded)

	w := submitApproval(t, s, stage.ID,
		`{"decision":"approve","comment":"--environment=production --override-freeze"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (all pre-flight satisfied):\n%s", w.Code, w.Body.String())
	}
	cur, _ := rr.GetStage(context.Background(), stage.ID)
	if cur.State != run.StageStateDispatched {
		t.Errorf("deploy stage state = %q, want dispatched", cur.State)
	}
}

// A deploy stage with NO pre-flight constraints passes (nothing to enforce) —
// the fail-closed NUANCE (#1384 condition 1): fail-closed targets the
// can't-evaluate path, not the nothing-declared case.
func TestDeployGate_NoConstraints_Proceeds(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stage, _ := seedDeployRun(rr, "release", deploySpecNoConstraints)
	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no constraints to enforce):\n%s", w.Code, w.Body.String())
	}
	if got := countAppendedCategory(au, "deploy_preflight_refused"); got != 0 {
		t.Errorf("deploy_preflight_refused entries = %d, want 0 (nothing refused)", got)
	}
	cur, _ := rr.GetStage(context.Background(), stage.ID)
	if cur.State != run.StageStateDispatched {
		t.Errorf("deploy stage state = %q, want dispatched", cur.State)
	}
}

// FAIL CLOSED (#1384 condition 1): a run whose cached spec does not parse is a
// can't-evaluate branch → 422 deploy_preflight_unevaluable, never a pass.
func TestDeployGate_FailClosed_UnparseableSpec(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stage, _ := seedDeployRun(rr, "release", "this: is: not: valid: yaml: ::::")
	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	assertDeployRefused(t, w, rr, au, stage, "deploy_preflight_unevaluable")
}

// FAIL CLOSED: an absent cached spec (legacy run) is a can't-evaluate branch.
func TestDeployGate_FailClosed_AbsentSpec(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stage, runRow := seedDeployRun(rr, "release", deploySpecEnvOnly)
	runRow.WorkflowSpec = nil // strip the cached spec
	w := submitApproval(t, s, stage.ID, `{"decision":"approve","comment":"--environment=production"}`)
	assertDeployRefused(t, w, rr, au, stage, "deploy_preflight_unevaluable")
}

// FAIL CLOSED: a spec that parses but contains NO deploy stage is a
// can't-evaluate branch.
func TestDeployGate_FailClosed_DeployStageAbsent(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stage, _ := seedDeployRun(rr, "release", deploySpecNoDeployStage)
	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	assertDeployRefused(t, w, rr, au, stage, "deploy_preflight_unevaluable")
}

// FAIL CLOSED: the run's workflow id is not present in the cached spec.
func TestDeployGate_FailClosed_WorkflowNotInSpec(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stage, _ := seedDeployRun(rr, "nonexistent_workflow", deploySpecEnvOnly)
	w := submitApproval(t, s, stage.ID, `{"decision":"approve","comment":"--environment=production"}`)
	assertDeployRefused(t, w, rr, au, stage, "deploy_preflight_unevaluable")
}

// FAIL CLOSED: a run-read failure (GetStage succeeds, GetRun returns
// ErrNotFound for an unseeded run) is a can't-evaluate branch.
func TestDeployGate_FailClosed_RunReadFailure(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	// Seed only the stage, NOT the run, so GetStage succeeds but GetRun fails.
	st := &run.Stage{
		ID:               uuid.New(),
		RunID:            uuid.New(),
		Type:             run.StageTypeDeploy,
		ExecutorKind:     run.ExecutorAgent,
		State:            run.StageStateAwaitingDeployApproval,
		RequiresApproval: true,
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
	}
	rr.mu.Lock()
	rr.stages[st.ID] = st
	rr.mu.Unlock()
	w := submitApproval(t, s, st.ID, `{"decision":"approve"}`)
	assertDeployRefused(t, w, rr, au, st, "deploy_preflight_unevaluable")
}

// FAIL CLOSED: an unrecognized required_upstream token denies. The
// workflow-v1 schema enum-validates required_upstream, so such a token is
// rejected at the spec-parse layer (deploy_preflight_unevaluable) — still a
// fail-closed refusal, never a pass. The default arm of checkDeployPreflight's
// required_upstream switch is the belt-and-suspenders backstop for the same
// case should the schema ever loosen.
func TestDeployGate_FailClosed_UnknownUpstreamToken(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	const spec = `
version: "1.0"
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
        constraints:
          - required_upstream: [some_unknown_signal]
`
	stage, _ := seedDeployRun(rr, "release", spec)
	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	assertDeployRefused(t, w, rr, au, stage, "deploy_preflight_unevaluable")
}

// FAIL CLOSED: nil RunRepo is a defensive can't-evaluate branch. It is
// unreachable via handleSubmitApproval (an earlier guard returns 503), so it
// is exercised by calling checkDeployPreflight directly.
func TestDeployGate_FailClosed_NilRunRepo(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: newApprovalAuditFake()})
	stage := &run.Stage{ID: uuid.New(), RunID: uuid.New(), Type: run.StageTypeDeploy}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
	w := httptest.NewRecorder()
	if s.checkDeployPreflight(w, withAuth(req), stage, "") {
		t.Fatal("checkDeployPreflight passed with a nil RunRepo; it must fail closed")
	}
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
	if body := decodeErrorEnvelope(t, w); body.Code != "deploy_preflight_unevaluable" {
		t.Errorf("error code = %q, want deploy_preflight_unevaluable", body.Code)
	}
}

// A deploy-gate REJECT fails the stage category-D and never runs the
// pre-flight gate (deploy delegation covers approve only).
func TestDeployGate_Reject_FailsCategoryD(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stage, _ := seedDeployRun(rr, "release", deploySpecAllConstraints)
	w := submitApproval(t, s, stage.ID, `{"decision":"reject","comment":"not now"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	cur, _ := rr.GetStage(context.Background(), stage.ID)
	if cur.State != run.StageStateFailed {
		t.Errorf("deploy stage state = %q, want failed", cur.State)
	}
	if cur.FailureCategory == nil || *cur.FailureCategory != run.FailureD {
		t.Errorf("failure category = %v, want D", cur.FailureCategory)
	}
	// The pre-flight gate never ran on a reject.
	if got := countAppendedCategory(au, "deploy_preflight_refused"); got != 0 {
		t.Errorf("deploy_preflight_refused entries = %d on a reject, want 0", got)
	}
}
