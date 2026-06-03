package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// redriveFixture wires a real orchestrator over the in-memory
// orchestratorRepo with a parent run parked in awaiting_children and a
// failed (category-C, retryable) decomposition child. This is the
// cross-boundary substrate: a redrive POST exercises HTTP handler →
// run.RedriveChild domain → RetryRun/RetryStage persistence →
// orchestrator dispatch, the seam per-layer unit tests would miss
// (cf. #618).
type redriveFixture struct {
	s         *Server
	repo      *orchestratorRepo
	au        *approvalAuditFake
	parent    *run.Run
	parentSt  *run.Stage
	child     *run.Run
	childImpl *run.Stage
}

func newRedriveFixture(t *testing.T, cat run.FailureCategory, reason string) *redriveFixture {
	t.Helper()
	repo := newOrchestratorRepo()

	// Parent: running, with a stage parked in awaiting_children (the
	// #698 park state) and a downstream review gate left pending so the
	// parent has somewhere to advance after the children settle.
	parent := repo.seedRun()
	parentSt := repo.seedStage(parent.ID, 0, run.StageStateAwaitingChildren)

	// Child: a decomposition child that failed with a retryable
	// implement-stage failure — the case the parent parked for.
	child := repo.seedRun()
	child.DecomposedFrom = &parent.ID
	child.State = run.StateFailed
	childImpl := repo.seedStage(child.ID, 0, run.StageStateFailed)
	childImpl.Type = run.StageTypeImplement
	c := cat
	rs := reason
	childImpl.FailureCategory = &c
	childImpl.FailureReason = &rs

	au := newApprovalAuditFake()
	o := &orchestrator.Orchestrator{Runs: repo, Audit: au} // no GitHub: dispatch transition happens, workflow_dispatch skipped
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      repo,
		AuditRepo:    au,
		Orchestrator: o,
	})
	return &redriveFixture{
		s: s, repo: repo, au: au,
		parent: parent, parentSt: parentSt, child: child, childImpl: childImpl,
	}
}

func postRedrive(t *testing.T, s *Server, runID uuid.UUID, decorate func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID.String()+"/redrive", nil)
	req.SetPathValue("run_id", runID.String())
	w := httptest.NewRecorder()
	s.handleRedriveChild(w, decorate(req))
	return w
}

// TestRedriveChild_EndToEnd is the cross-boundary integration test: a
// redrive re-opens the failed child + its implement stage, the
// orchestrator re-dispatches it, the child_redriven audit is chained,
// and a subsequently-succeeding child lets the parked parent reconcile
// to succeeded through Advance.
func TestRedriveChild_EndToEnd(t *testing.T) {
	f := newRedriveFixture(t, run.FailureC, "runner OOM")

	w := postRedrive(t, f.s, f.child.ID, withAuth)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	// The run is un-terminal (running) so Advance could act on it.
	var body runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.State != string(run.StateRunning) {
		t.Errorf("run state = %q, want running", body.State)
	}

	// The orchestrator walked the re-opened implement stage
	// pending → dispatched (no GitHub means workflow_dispatch was
	// skipped, but the transition happened).
	gotStage, _ := f.repo.GetStage(context.Background(), f.childImpl.ID)
	if gotStage.State != run.StageStateDispatched {
		t.Errorf("implement stage = %q, want dispatched (orchestrator should have advanced)", gotStage.State)
	}
	if gotStage.FailureCategory != nil {
		t.Errorf("implement stage still carries failure metadata after redrive")
	}

	// The child_redriven audit was chained with the prior failure detail.
	var redriven *audit.ChainAppendParams
	for i := range f.au.appended {
		if f.au.appended[i].Category == CategoryChildRedriven {
			redriven = &f.au.appended[i]
			break
		}
	}
	if redriven == nil {
		t.Fatalf("no child_redriven audit entry; got %+v", f.au.appended)
	}
	var payload map[string]any
	if err := json.Unmarshal(redriven.Payload, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	if payload["prior_category"] != "C" {
		t.Errorf("audit prior_category = %v, want C", payload["prior_category"])
	}

	// Drive the re-dispatched child to success, then Advance the child:
	// completeRun → child succeeded → maybeAdvanceDecomposedParent
	// resolves the parked parent (no failed children now) and advances
	// it to succeeded.
	ctx := context.Background()
	if _, err := f.repo.TransitionStage(ctx, f.childImpl.ID, run.StageStateRunning, nil); err != nil {
		t.Fatalf("stage → running: %v", err)
	}
	if _, err := f.repo.TransitionStage(ctx, f.childImpl.ID, run.StageStateSucceeded, nil); err != nil {
		t.Fatalf("stage → succeeded: %v", err)
	}
	if _, err := f.s.cfg.Orchestrator.Advance(ctx, f.child.ID); err != nil {
		t.Fatalf("advance child: %v", err)
	}

	parentSt, _ := f.repo.GetStage(ctx, f.parentSt.ID)
	if parentSt.State != run.StageStateSucceeded {
		t.Errorf("parent awaiting_children stage = %q, want succeeded (reconciled)", parentSt.State)
	}
	parent, _ := f.repo.GetRun(ctx, f.parent.ID)
	if parent.State != run.StateSucceeded {
		t.Errorf("parent run = %q, want succeeded after reconciliation", parent.State)
	}
}

// TestRedriveChild_AgentTokenRejected is the binding security guard
// (#698 condition 1): an MCP/agent subject-bound token may not re-drive
// any run, full stop — not its own, not a sibling's.
func TestRedriveChild_AgentTokenRejected(t *testing.T) {
	f := newRedriveFixture(t, run.FailureC, "runner OOM")

	// An agent token bound to the very run it is trying to re-drive is
	// still rejected: re-opening a terminal run is operator-only.
	withAgent := func(req *http.Request) *http.Request {
		id := Identity{
			Subject: "mcp:run:" + f.child.ID.String(),
			TokenID: "tok-agent",
			Scopes:  []string{"mcp:read", "write:retries"},
		}
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
	}

	w := postRedrive(t, f.s, f.child.ID, withAgent)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "agent_token_forbidden") {
		t.Errorf("body missing agent_token_forbidden code: %s", w.Body.String())
	}
	// No re-drive happened: the child run is still failed.
	got, _ := f.repo.GetRun(context.Background(), f.child.ID)
	if got.State != run.StateFailed {
		t.Errorf("child run = %q, want failed (redrive must not have run)", got.State)
	}
	for _, a := range f.au.appended {
		if a.Category == CategoryChildRedriven {
			t.Errorf("child_redriven audit written despite rejection")
		}
	}
}

// TestRedriveChild_OperatorScopedToken confirms an operator bearer
// token carrying write:retries is accepted (the non-agent token path).
func TestRedriveChild_OperatorScopedToken(t *testing.T) {
	f := newRedriveFixture(t, run.FailureA, "agent crashed")

	withRetryScope := func(req *http.Request) *http.Request {
		id := Identity{
			Subject: "github:ops",
			TokenID: "tok-ops",
			Scopes:  []string{"write:retries"},
		}
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
	}

	w := postRedrive(t, f.s, f.child.ID, withRetryScope)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
}

func TestRedriveChild_InsufficientScopeReturns403(t *testing.T) {
	f := newRedriveFixture(t, run.FailureC, "runner OOM")

	withNoScope := func(req *http.Request) *http.Request {
		id := Identity{Subject: "github:ops", TokenID: "tok-ops", Scopes: []string{"read:runs"}}
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
	}

	w := postRedrive(t, f.s, f.child.ID, withNoScope)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestRedriveChild_NonDecomposedReturns422(t *testing.T) {
	f := newRedriveFixture(t, run.FailureC, "runner OOM")
	f.child.DecomposedFrom = nil // not a decomposition child

	w := postRedrive(t, f.s, f.child.ID, withAuth)
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "redrive_not_applicable") {
		t.Errorf("body missing redrive_not_applicable code: %s", w.Body.String())
	}
}

func TestRedriveChild_NotFoundReturns404(t *testing.T) {
	f := newRedriveFixture(t, run.FailureC, "runner OOM")

	w := postRedrive(t, f.s, uuid.New(), withAuth)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestRedriveChild_BadUUIDReturns400(t *testing.T) {
	f := newRedriveFixture(t, run.FailureC, "runner OOM")

	req := httptest.NewRequest(http.MethodPost, "/v0/runs/not-a-uuid/redrive", nil)
	req.SetPathValue("run_id", "not-a-uuid")
	w := httptest.NewRecorder()
	f.s.handleRedriveChild(w, withAuth(req))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestRedriveChild_UnconfiguredReturns503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	w := postRedrive(t, s, uuid.New(), withAuth)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestRedriveChild_AnonymousReturns401(t *testing.T) {
	f := newRedriveFixture(t, run.FailureC, "runner OOM")
	w := postRedrive(t, f.s, f.child.ID, func(req *http.Request) *http.Request { return req })
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}
