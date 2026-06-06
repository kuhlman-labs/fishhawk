package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// retryServer wires the same fakes as the approvals tests but
// without the RoleResolver / GitHub-spec dependencies — retry
// doesn't gate on approver role.
func retryServer(t *testing.T) (*Server, *approvalRunRepo, *approvalAuditFake) {
	t.Helper()
	repo := newApprovalRunRepo()
	au := newApprovalAuditFake()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      repo,
		AuditRepo:    au,
		ApprovalRepo: newFakeApprovalRepo(),
	})
	return s, repo, au
}

// seedFailedStage builds a stage already in failed state with the
// given category + reason — the post-condition the retry handler
// expects to operate on.
func seedFailedStage(repo *approvalRunRepo, cat run.FailureCategory, reason string) *run.Stage {
	st := repo.seedStage(run.StageStateFailed)
	c := cat
	r := reason
	st.FailureCategory = &c
	st.FailureReason = &r
	return st
}

func postRetry(t *testing.T, s *Server, stageID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	url := "/v0/stages/" + stageID.String() + "/retry"
	req := httptest.NewRequest(http.MethodPost, url, nil)
	req.SetPathValue("stage_id", stageID.String())
	w := httptest.NewRecorder()
	s.handleRetryStage(w, withAuth(req))
	return w
}

func TestRetryStage_DTimeoutHappyPath(t *testing.T) {
	s, repo, au := retryServer(t)
	stage := seedFailedStage(repo, run.FailureD, "sla_timeout: 5h elapsed (deadline 4h)")

	w := postRetry(t, s, stage.ID)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	var body stageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.State != string(run.StageStateAwaitingApproval) {
		t.Errorf("body.State = %q, want awaiting_approval", body.State)
	}
	if body.FailureCategory != nil {
		t.Errorf("body.FailureCategory = %v, want nil after retry", body.FailureCategory)
	}

	// One stage_retried entry on the audit chain with the prior
	// metadata in the payload.
	if len(au.appended) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(au.appended))
	}
	got := au.appended[0]
	if got.Category != CategoryStageRetried {
		t.Errorf("audit category = %q, want stage_retried", got.Category)
	}
	var payload map[string]any
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	if payload["prior_category"] != "D" {
		t.Errorf("payload.prior_category = %v, want D", payload["prior_category"])
	}
	if !strings.Contains(payload["prior_reason"].(string), "sla_timeout") {
		t.Errorf("payload.prior_reason = %v", payload["prior_reason"])
	}
}

func TestRetryStage_DRejectedReturns422(t *testing.T) {
	s, repo, _ := retryServer(t)
	stage := seedFailedStage(repo, run.FailureD, "gate rejected by approver")

	w := postRetry(t, s, stage.ID)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", w.Code)
	}
	if !strings.Contains(w.Body.String(), "retry_not_applicable") {
		t.Errorf("body missing retry_not_applicable code: %s", w.Body.String())
	}
}

func TestRetryStage_BReturns422(t *testing.T) {
	s, repo, _ := retryServer(t)
	stage := seedFailedStage(repo, run.FailureB, "forbidden_paths violated")

	w := postRetry(t, s, stage.ID)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", w.Code)
	}
}

// E8.6 (#173): A and C retries now succeed. The state-machine
// move is failed → pending; the handler hands off to the
// orchestrator to fire workflow_dispatch. With no orchestrator
// wired into the Server, the stage stays at pending — operators
// can fire Advance manually later.
func TestRetryStage_AHappyPathWithoutOrchestrator(t *testing.T) {
	s, repo, au := retryServer(t)
	stage := seedFailedStage(repo, run.FailureA, "agent crashed: SIGSEGV")

	w := postRetry(t, s, stage.ID)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var body stageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.State != string(run.StageStatePending) {
		t.Errorf("body.State = %q, want pending (no orchestrator wired)", body.State)
	}
	if body.FailureCategory != nil {
		t.Errorf("body.FailureCategory = %v, want nil after retry", body.FailureCategory)
	}
	if len(au.appended) != 1 || au.appended[0].Category != CategoryStageRetried {
		t.Errorf("audit chain = %+v, want one stage_retried entry", au.appended)
	}
	var payload map[string]any
	_ = json.Unmarshal(au.appended[0].Payload, &payload)
	if payload["prior_category"] != "A" {
		t.Errorf("payload.prior_category = %v, want A", payload["prior_category"])
	}
}

func TestRetryStage_CHappyPathWithoutOrchestrator(t *testing.T) {
	s, repo, _ := retryServer(t)
	stage := seedFailedStage(repo, run.FailureC, "dispatch_watchdog: 70m elapsed")

	w := postRetry(t, s, stage.ID)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body stageResponse
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.State != string(run.StageStatePending) {
		t.Errorf("body.State = %q, want pending", body.State)
	}
}

// E8.6: with an Orchestrator wired, an A/C retry transitions
// failed → pending → dispatched (the orchestrator advances the
// pending stage). The fake orchestrator has no GitHub client, so
// the actual workflow_dispatch is a no-op — but the state
// transition still happens.
func TestRetryStage_AHappyPathWithOrchestrator(t *testing.T) {
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	stage := rr.seedStage(r.ID, 0, run.StageStateFailed)
	cat := run.FailureA
	reason := "agent crashed: SIGSEGV"
	stage.FailureCategory = &cat
	stage.FailureReason = &reason

	au := newApprovalAuditFake()
	o := &orchestrator.Orchestrator{Runs: rr} // no GitHub: dispatch is skipped, transition happens
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		AuditRepo:    au,
		Orchestrator: o,
	})

	w := postRetry(t, s, stage.ID)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var body stageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Orchestrator transitioned pending → dispatched after the
	// retry's failed → pending move; no GitHub means workflow_dispatch
	// was skipped silently inside the orchestrator.
	if body.State != string(run.StageStateDispatched) {
		t.Errorf("body.State = %q, want dispatched (orchestrator should have advanced)", body.State)
	}
	if body.FailureCategory != nil {
		t.Errorf("body.FailureCategory = %v, want nil after retry", body.FailureCategory)
	}
	if len(au.appended) != 1 || au.appended[0].Category != CategoryStageRetried {
		t.Errorf("audit chain = %+v, want one stage_retried entry", au.appended)
	}
}

// TestRetryStage_AReopensRunAndAdvancesToReview is the load-bearing
// #798 regression test. It crosses the full handler → run repo →
// orchestrator → stage seam: (a) retry a category-A-failed implement
// stage on a FAILED run and assert the run is reopened to running and
// the implement stage re-dispatched; then (b) drive the re-run to
// success and assert the run is STILL running AND the review gate
// reached awaiting_approval. Part (b) is the actual #798 regression —
// the orphan bug left run=failed / review=pending because Advance
// no-ops on a terminal run, so re-opening only the stage stranded the
// run. Per-layer unit checks (stage reopened to pending; run reopened
// to running) each pass while this seam breaks; only the end-to-end
// assertion catches it.
func TestRetryStage_AReopensRunAndAdvancesToReview(t *testing.T) {
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	r.State = run.StateFailed // a category-A failure left the run terminal-failed

	implement := rr.seedStage(r.ID, 0, run.StageStateFailed)
	implement.Type = run.StageTypeImplement
	cat := run.FailureA
	reason := "agent crashed: SIGSEGV"
	implement.FailureCategory = &cat
	implement.FailureReason = &reason

	// Pending review gate at sequence 1. Human executor so dispatchStage
	// walks it pending → dispatched → running → awaiting_approval with no
	// GitHub client wired.
	review := rr.seedStage(r.ID, 1, run.StageStatePending)
	review.Type = run.StageTypeReview
	review.ExecutorKind = run.ExecutorHuman

	au := newApprovalAuditFake()
	o := &orchestrator.Orchestrator{Runs: rr} // no GitHub: dispatch is skipped, transitions happen
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		AuditRepo:    au,
		Orchestrator: o,
	})

	// (a) Retry the failed implement stage.
	w := postRetry(t, s, implement.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	gotRun, err := rr.GetRun(context.Background(), r.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if gotRun.State != run.StateRunning {
		t.Fatalf("run state after retry = %q, want running (reopen failed: #798 orphan)", gotRun.State)
	}
	var body stageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.State != string(run.StageStateDispatched) {
		t.Fatalf("implement state after retry = %q, want dispatched", body.State)
	}

	// (b) Drive the re-run to success and Advance to the next gate.
	ctx := context.Background()
	if _, err := rr.TransitionStage(ctx, implement.ID, run.StageStateRunning, nil); err != nil {
		t.Fatalf("implement dispatched → running: %v", err)
	}
	if _, err := rr.TransitionStage(ctx, implement.ID, run.StageStateSucceeded, nil); err != nil {
		t.Fatalf("implement running → succeeded: %v", err)
	}
	if _, err := o.Advance(ctx, r.ID); err != nil {
		t.Fatalf("advance after implement success: %v", err)
	}

	// The #798 assertion: the run is still running (NOT failed) and the
	// review gate opened (NOT stuck at pending).
	gotRun, err = rr.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if gotRun.State != run.StateRunning {
		t.Errorf("run state after re-run success = %q, want running (#798: orphan would leave it failed)", gotRun.State)
	}
	gotReview, err := rr.GetStage(ctx, review.ID)
	if err != nil {
		t.Fatalf("get review stage: %v", err)
	}
	if gotReview.State != run.StageStateAwaitingApproval {
		t.Errorf("review state = %q, want awaiting_approval (#798: orphan would leave it pending)", gotReview.State)
	}
}

// retryRunSpy wraps orchestratorRepo to count RetryRun invocations and
// optionally force an error from it. The call count lets the guard tests
// distinguish "the State == failed guard prevented the RetryRun call"
// from "the guard was absent but RetryRun's error was silently swallowed"
// — a distinction the run-state assertion alone cannot make, because the
// reopen is best-effort (logged, not fatal) so a stray running → running
// call would also leave the run state unchanged (#798 fix-up).
type retryRunSpy struct {
	*orchestratorRepo
	retryRunCalls int
	forceErr      error
}

func (r *retryRunSpy) RetryRun(ctx context.Context, id uuid.UUID, to run.State) (*run.Run, error) {
	r.retryRunCalls++
	if r.forceErr != nil {
		return nil, r.forceErr
	}
	return r.orchestratorRepo.RetryRun(ctx, id, to)
}

// TestRetryStage_NonFailedRunLeavesRunStateUnchanged locks in that the
// run-reopen is gated on State == failed and is not a blanket RetryRun
// call. seedRun seeds StateRunning, so the guard must SKIP RetryRun
// entirely — asserted via the spy's call count being zero. The bare
// run-state check is insufficient on its own: because the reopen is
// best-effort, an unguarded running → running call would return
// InvalidTransitionError, be logged, and STILL leave the state unchanged,
// so only the call-count assertion proves the guard fired.
func TestRetryStage_NonFailedRunLeavesRunStateUnchanged(t *testing.T) {
	rr := &retryRunSpy{orchestratorRepo: newOrchestratorRepo()}
	r := rr.seedRun() // State == running

	implement := rr.seedStage(r.ID, 0, run.StageStateFailed)
	implement.Type = run.StageTypeImplement
	cat := run.FailureA
	reason := "agent crashed: SIGSEGV"
	implement.FailureCategory = &cat
	implement.FailureReason = &reason

	au := newApprovalAuditFake()
	o := &orchestrator.Orchestrator{Runs: rr}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		AuditRepo:    au,
		Orchestrator: o,
	})

	w := postRetry(t, s, implement.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if rr.retryRunCalls != 0 {
		t.Errorf("RetryRun calls = %d, want 0 (guard must skip the reopen on a non-failed run)", rr.retryRunCalls)
	}
	gotRun, err := rr.GetRun(context.Background(), r.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if gotRun.State != run.StateRunning {
		t.Errorf("run state = %q, want running unchanged (reopen must be gated on failed)", gotRun.State)
	}
}

// TestRetryStage_ReopenErrorIsBestEffort covers the guarded-but-failing
// path: the run IS failed (so the guard fires and RetryRun is called) but
// RetryRun itself returns an error. Per the approved plan the reopen is
// best-effort — a RetryRun error after the stage transition already
// committed must be LOGGED, not fail the request. So the handler must
// still return 200, and the spy must record exactly one RetryRun call
// (proving the error path was exercised, not skipped by the guard).
func TestRetryStage_ReopenErrorIsBestEffort(t *testing.T) {
	rr := &retryRunSpy{
		orchestratorRepo: newOrchestratorRepo(),
		forceErr:         errors.New("transient reopen failure"),
	}
	r := rr.seedRun()
	r.State = run.StateFailed // guard fires: run is terminal-failed

	implement := rr.seedStage(r.ID, 0, run.StageStateFailed)
	implement.Type = run.StageTypeImplement
	cat := run.FailureA
	reason := "agent crashed: SIGSEGV"
	implement.FailureCategory = &cat
	implement.FailureReason = &reason

	au := newApprovalAuditFake()
	o := &orchestrator.Orchestrator{Runs: rr}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		AuditRepo:    au,
		Orchestrator: o,
	})

	w := postRetry(t, s, implement.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (reopen error must be logged, not fatal):\n%s", w.Code, w.Body.String())
	}
	if rr.retryRunCalls != 1 {
		t.Errorf("RetryRun calls = %d, want 1 (guard fires on a failed run, then error path runs)", rr.retryRunCalls)
	}
}

func TestRetryStage_NotFailedReturns422(t *testing.T) {
	s, repo, _ := retryServer(t)
	stage := repo.seedStage(run.StageStateAwaitingApproval) // never failed

	w := postRetry(t, s, stage.ID)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", w.Code)
	}
}

func TestRetryStage_NotFoundReturns404(t *testing.T) {
	s, _, _ := retryServer(t)
	w := postRetry(t, s, uuid.New())

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestRetryStage_UnconfiguredReturns503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	w := postRetry(t, s, uuid.New())

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// withMCPRetryAuth injects an MCP identity with write:retries scope
// bound to runID into req's context.
func withMCPRetryAuth(req *http.Request, runID uuid.UUID) *http.Request {
	id := Identity{
		Subject: "mcp:run:" + runID.String(),
		TokenID: "tok-test",
		Scopes:  []string{"mcp:read", "write:retries"},
	}
	return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
}

func postRetryMCP(t *testing.T, s *Server, stageID, runID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	url := "/v0/stages/" + stageID.String() + "/retry"
	req := httptest.NewRequest(http.MethodPost, url, nil)
	req.SetPathValue("stage_id", stageID.String())
	w := httptest.NewRecorder()
	s.handleRetryStage(w, withMCPRetryAuth(req, runID))
	return w
}

// --- Subject-binding guard ---

func TestRetryStage_MCPTokenMatchingRunAllowed(t *testing.T) {
	s, repo, au := retryServer(t)
	stage := seedFailedStage(repo, run.FailureA, "agent crashed")

	w := postRetryMCP(t, s, stage.ID, stage.RunID)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if len(au.appended) != 1 || au.appended[0].Category != CategoryStageRetried {
		t.Errorf("expected one stage_retried audit entry, got %+v", au.appended)
	}
}

func TestRetryStage_MCPTokenMismatchedRunReturns403(t *testing.T) {
	s, repo, _ := retryServer(t)
	stage := seedFailedStage(repo, run.FailureA, "agent crashed")
	otherRunID := uuid.New() // does not match stage.RunID

	w := postRetryMCP(t, s, stage.ID, otherRunID)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "cross_run_retry") {
		t.Errorf("body missing cross_run_retry code: %s", w.Body.String())
	}
}

func TestRetryStage_MCPTokenMalformedSubjectReturns401(t *testing.T) {
	s, repo, _ := retryServer(t)
	stage := seedFailedStage(repo, run.FailureA, "agent crashed")

	// Inject an MCP identity with a malformed subject (no valid UUID suffix).
	req := httptest.NewRequest(http.MethodPost, "/v0/stages/"+stage.ID.String()+"/retry", nil)
	req.SetPathValue("stage_id", stage.ID.String())
	badID := Identity{
		Subject: "mcp:run:not-a-uuid",
		TokenID: "tok-test",
		Scopes:  []string{"mcp:read", "write:retries"},
	}
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, badID))
	w := httptest.NewRecorder()
	s.handleRetryStage(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

// --- Category filter with write:retries scope ---

func TestRetryStage_WriteRetriesScope_BReturns422(t *testing.T) {
	s, repo, _ := retryServer(t)
	stage := seedFailedStage(repo, run.FailureB, "forbidden_paths violated")

	w := postRetryMCP(t, s, stage.ID, stage.RunID)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", w.Code)
	}
}

func TestRetryStage_WriteRetriesScope_DRejectedReturns422(t *testing.T) {
	s, repo, _ := retryServer(t)
	stage := seedFailedStage(repo, run.FailureD, "gate rejected by approver")

	w := postRetryMCP(t, s, stage.ID, stage.RunID)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", w.Code)
	}
}

// --- Receipt shape ---

func TestRetryStage_AuditReceiptShape(t *testing.T) {
	s, repo, au := retryServer(t)
	stage := seedFailedStage(repo, run.FailureA, "agent crashed: SIGSEGV")

	w := postRetry(t, s, stage.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	if len(au.appended) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(au.appended))
	}
	var payload map[string]any
	if err := json.Unmarshal(au.appended[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	for _, key := range []string{"prior_failure_class", "retry_ordinal", "admissibility_reason"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("audit payload missing key %q; got keys: %v", key, payloadKeys(payload))
		}
	}
}

// postRetryBody posts to the retry endpoint with a raw JSON body —
// used to exercise the {override, reason} escape hatch (#698).
func postRetryBody(t *testing.T, s *Server, stageID uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	url := "/v0/stages/" + stageID.String() + "/retry"
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.SetPathValue("stage_id", stageID.String())
	w := httptest.NewRecorder()
	s.handleRetryStage(w, withAuth(req))
	return w
}

// #698: a category-B failure with {override:true, reason:...} re-opens
// the stage to pending and writes the DISTINCT stage_override_retried
// audit entry (not a plain stage_retried). The override re-runs the
// stage so the gate re-evaluates the new diff — it does not bypass the
// gate.
func TestRetryStage_BOverrideHappyPath(t *testing.T) {
	s, repo, au := retryServer(t)
	stage := seedFailedStage(repo, run.FailureB, "forbidden_paths violated")

	w := postRetryBody(t, s, stage.ID, `{"override":true,"reason":"forbidden path was a generated file; regenerating"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var body stageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.State != string(run.StageStatePending) {
		t.Errorf("body.State = %q, want pending", body.State)
	}

	if len(au.appended) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(au.appended))
	}
	got := au.appended[0]
	if got.Category != CategoryStageOverrideRetried {
		t.Errorf("audit category = %q, want stage_override_retried", got.Category)
	}
	var payload map[string]any
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	if payload["prior_category"] != "B" {
		t.Errorf("payload.prior_category = %v, want B", payload["prior_category"])
	}
	if !strings.Contains(payload["override_reason"].(string), "generated file") {
		t.Errorf("payload.override_reason = %v, want the operator reason", payload["override_reason"])
	}
	if _, ok := payload["override_effect"]; !ok {
		t.Error("payload missing override_effect framing (gate not bypassed)")
	}
}

// The category-B override is an OPERATOR-only escape hatch: an agent
// (MCP subject-bound) token is rejected outright even for its OWN run, so
// an agent cannot self-override a genuine policy-gate failure and the
// stage_override_retried audit's operator attribution holds (#698). A
// normal (non-override) retry from the same token stays allowed — that
// path is covered by TestRetryStage_MCPTokenMatchingRunAllowed.
func TestRetryStage_BOverrideAgentTokenForbidden(t *testing.T) {
	s, repo, au := retryServer(t)
	stage := seedFailedStage(repo, run.FailureB, "forbidden_paths violated")

	req := httptest.NewRequest(http.MethodPost, "/v0/stages/"+stage.ID.String()+"/retry",
		strings.NewReader(`{"override":true,"reason":"agent attempting self-override"}`))
	req.SetPathValue("stage_id", stage.ID.String())
	w := httptest.NewRecorder()
	// Agent token bound to the stage's OWN run — still rejected for override.
	s.handleRetryStage(w, withMCPRetryAuth(req, stage.RunID))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "agent_token_forbidden") {
		t.Errorf("body missing agent_token_forbidden code: %s", w.Body.String())
	}
	if len(au.appended) != 0 {
		t.Errorf("audit chain = %+v, want no entry on a rejected agent override", au.appended)
	}
}

// The reason is mandatory when override is set: a bare {override:true}
// is a 400 validation_failed, not a silent override.
func TestRetryStage_BOverrideWithoutReasonReturns400(t *testing.T) {
	s, repo, au := retryServer(t)
	stage := seedFailedStage(repo, run.FailureB, "forbidden_paths violated")

	w := postRetryBody(t, s, stage.ID, `{"override":true}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "validation_failed") {
		t.Errorf("body missing validation_failed code: %s", w.Body.String())
	}
	if len(au.appended) != 0 {
		t.Errorf("audit chain = %+v, want no entry on a rejected override", au.appended)
	}
}

// Without the override flag a category-B retry is still refused with a
// 422 — the default behavior is unchanged (cf. TestRetryStage_BReturns422,
// which exercises the no-body path; this one sends an explicit
// {override:false}).
func TestRetryStage_BOverrideFalseStill422(t *testing.T) {
	s, repo, _ := retryServer(t)
	stage := seedFailedStage(repo, run.FailureB, "forbidden_paths violated")

	w := postRetryBody(t, s, stage.ID, `{"override":false}`)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", w.Code)
	}
}

func payloadKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
