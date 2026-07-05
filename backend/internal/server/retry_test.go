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

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
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

// --- Drive next_action recording (#1271) -----------------------------------

// newDriveRetryServer wires orchestratorRepo + the richer auditFake (which
// serves ListForRunByCategory over appended entries, so GET /v0/runs/{id}
// surfaces the drive next_action) + a real orchestrator, seeding a failed
// stage of the given type/category on a run with the given drive flag and
// runner kind. The run is seeded StateRunning so the retry handler's
// failed→running reopen is inert and GET /v0/runs/{id} sees a non-terminal
// run (applyDriveSurfaces suppresses next_action on terminal runs).
func newDriveRetryServer(t *testing.T, driveEnabled bool, runnerKind string, stageType run.StageType, cat run.FailureCategory, reason string) (*Server, *orchestratorRepo, *auditFake, uuid.UUID, uuid.UUID) {
	t.Helper()
	rr := newOrchestratorRepo()
	au := newAuditFake()
	r := rr.seedRun()
	r.Drive = driveEnabled
	r.RunnerKind = runnerKind
	stage := rr.seedStage(r.ID, 0, run.StageStateFailed)
	stage.Type = stageType
	c := cat
	rs := reason
	stage.FailureCategory = &c
	stage.FailureReason = &rs
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		AuditRepo:    au,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})
	return s, rr, au, r.ID, stage.ID
}

// getRunNextAction performs GET /v0/runs/{id} on the server and returns the
// decoded run resource so a cross-layer test can assert the authoritative
// REST run resource surfaces the drive next_action.
func getRunNextAction(t *testing.T, s *Server, runID uuid.UUID) runResponse {
	t.Helper()
	gw := httptest.NewRecorder()
	greq := httptest.NewRequest(http.MethodGet, "/v0/runs/"+runID.String(), nil)
	s.Handler().ServeHTTP(gw, greq)
	if gw.Code != http.StatusOK {
		t.Fatalf("GET run status = %d, want 200:\n%s", gw.Code, gw.Body.String())
	}
	var resp runResponse
	if err := json.Unmarshal(gw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode run response: %v", err)
	}
	return resp
}

// TestRetryStage_Drive_LocalImplement_ParksWithNextAction is the #1271
// done-means cross-layer test for the retry path: a category-A retry of an
// implement stage on a drive-mode LOCAL run records a run_auto_advanced
// entry with parked=true + next_action.action=run_implement_stage, AND GET
// /v0/runs/{id} surfaces that same next_action on the authoritative REST
// run resource.
func TestRetryStage_Drive_LocalImplement_ParksWithNextAction(t *testing.T) {
	s, _, au, runID, stageID := newDriveRetryServer(t, true, run.RunnerKindLocal,
		run.StageTypeImplement, run.FailureA, "agent crashed: SIGSEGV")

	w := postRetry(t, s, stageID)
	if w.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	advances := reviseDriveAdvances(t, au)
	if len(advances) != 1 {
		t.Fatalf("run_auto_advanced entries = %+v, want exactly 1", advances)
	}
	adv := advances[0]
	if adv.Rule != drive.RuleRetryReopen {
		t.Errorf("rule = %q, want retry_reopen", adv.Rule)
	}
	if !adv.Parked {
		t.Error("parked = false, want true (local runner cannot be backend-dispatched, ADR-024)")
	}
	if adv.To != "implement:pending" {
		t.Errorf("to = %q, want implement:pending", adv.To)
	}
	if adv.NextAction == nil || adv.NextAction.Action != "run_implement_stage" {
		t.Fatalf("next_action = %+v, want action run_implement_stage", adv.NextAction)
	}

	if resp := getRunNextAction(t, s, runID); resp.NextAction == nil || resp.NextAction.Action != "run_implement_stage" {
		t.Fatalf("GET /v0/runs/{id} next_action = %+v, want action run_implement_stage", resp.NextAction)
	}
}

// TestRetryStage_Drive_LocalPlan_ParksWithNextAction covers the plan-stage
// branch: a category-A retry of a PLAN stage on a drive-mode local run parks
// with next_action.action=run_plan_stage, surfaced on the run resource.
func TestRetryStage_Drive_LocalPlan_ParksWithNextAction(t *testing.T) {
	s, _, au, runID, stageID := newDriveRetryServer(t, true, run.RunnerKindLocal,
		run.StageTypePlan, run.FailureA, "agent crashed: SIGSEGV")

	w := postRetry(t, s, stageID)
	if w.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	advances := reviseDriveAdvances(t, au)
	if len(advances) != 1 {
		t.Fatalf("run_auto_advanced entries = %+v, want exactly 1", advances)
	}
	adv := advances[0]
	if adv.Rule != drive.RuleRetryReopen {
		t.Errorf("rule = %q, want retry_reopen", adv.Rule)
	}
	if !adv.Parked || adv.To != "plan:pending" {
		t.Errorf("parked/to = %v/%q, want true/plan:pending", adv.Parked, adv.To)
	}
	if adv.NextAction == nil || adv.NextAction.Action != "run_plan_stage" {
		t.Fatalf("next_action = %+v, want action run_plan_stage", adv.NextAction)
	}

	if resp := getRunNextAction(t, s, runID); resp.NextAction == nil || resp.NextAction.Action != "run_plan_stage" {
		t.Fatalf("GET /v0/runs/{id} next_action = %+v, want action run_plan_stage", resp.NextAction)
	}
}

// TestRetryStage_Drive_GitHubActions_Advances asserts the advancing arm: a
// retry on a drive-mode github_actions run records an advanced (not parked)
// run_auto_advanced entry to implement:dispatched — the orchestrator's
// workflow_dispatch edge IS the re-run, so no operator next action.
func TestRetryStage_Drive_GitHubActions_Advances(t *testing.T) {
	s, _, au, _, stageID := newDriveRetryServer(t, true, run.RunnerKindGitHubActions,
		run.StageTypeImplement, run.FailureA, "agent crashed: SIGSEGV")

	w := postRetry(t, s, stageID)
	if w.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	advances := reviseDriveAdvances(t, au)
	if len(advances) != 1 {
		t.Fatalf("run_auto_advanced entries = %+v, want exactly 1", advances)
	}
	adv := advances[0]
	if adv.Rule != drive.RuleRetryReopen {
		t.Errorf("rule = %q, want retry_reopen", adv.Rule)
	}
	if adv.Parked {
		t.Error("parked = true, want false (github_actions auto-advances via workflow_dispatch)")
	}
	if adv.To != "implement:dispatched" {
		t.Errorf("to = %q, want implement:dispatched", adv.To)
	}
	if adv.NextAction != nil {
		t.Errorf("next_action = %+v, want nil (nothing for the operator to do)", adv.NextAction)
	}
}

// TestRetryStage_NonDrive_RecordsNoDriveEntry exercises the guard branch: a
// retry on a NON-drive run records no run_auto_advanced entry
// (recordDriveRetryStage no-ops on !runRow.Drive), leaving only the
// stage_retried entry the handler always writes.
func TestRetryStage_NonDrive_RecordsNoDriveEntry(t *testing.T) {
	s, _, au, _, stageID := newDriveRetryServer(t, false, run.RunnerKindLocal,
		run.StageTypeImplement, run.FailureA, "agent crashed: SIGSEGV")

	w := postRetry(t, s, stageID)
	if w.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if advances := reviseDriveAdvances(t, au); len(advances) != 0 {
		t.Errorf("run_auto_advanced entries = %+v, want none on a non-drive run", advances)
	}
}

// TestRetryStage_Drive_DTimeoutAwaitingApproval_RecordsNoDriveEntry pins the
// reopened-to-pending guard: a D-timeout retry re-opens the stage to
// awaiting_approval (not pending), so recordDriveRetryStage must not fire —
// zero run_auto_advanced entries even on a drive-mode local run.
func TestRetryStage_Drive_DTimeoutAwaitingApproval_RecordsNoDriveEntry(t *testing.T) {
	s, _, au, _, stageID := newDriveRetryServer(t, true, run.RunnerKindLocal,
		run.StageTypeReview, run.FailureD, "sla_timeout: 5h elapsed (deadline 4h)")

	w := postRetry(t, s, stageID)
	if w.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if advances := reviseDriveAdvances(t, au); len(advances) != 0 {
		t.Errorf("run_auto_advanced entries = %+v, want none on a D-timeout awaiting_approval re-open", advances)
	}
}

func payloadKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// --- Delegated retry (ADR-040 / #1026) --------------------------------------

// delegatedRetryServer wires the auditFake + concern fake the
// delegation evaluator reads (the plain retryServer's
// approvalAuditFake and missing concern store would fail-close every
// delegated request).
func delegatedRetryServer(t *testing.T) (*Server, *approvalRunRepo, *auditFake) {
	t.Helper()
	repo := newApprovalRunRepo()
	au := newAuditFake()
	s := New(Config{
		Addr:        "127.0.0.1:0",
		RunRepo:     repo,
		AuditRepo:   au,
		ConcernRepo: newFakeConcernRepo(),
	})
	return s, repo, au
}

// TestRetryStage_Delegated_InfraFlakeMet: a category-A failure whose
// recorded reason carries the testcontainers infra-flake signature
// satisfies may_retry's infra_flake condition — the delegated retry
// proceeds and the stage_retried payload records the rule.
func TestRetryStage_Delegated_InfraFlakeMet(t *testing.T) {
	s, repo, au := delegatedRetryServer(t)
	stage := seedFailedStage(repo, run.FailureA,
		`verify command "scripts/test" still failing after 9 iteration(s):\n`+
			`failed to start container: context deadline exceeded after 9 retries`)
	seedDelegatedRun(repo, stage)

	w := postRetryBody(t, s, stage.ID, `{"delegated":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if rule := delegatedAuditRule(t, au, CategoryStageRetried); rule != "infra_flake" {
		t.Errorf("audit delegated = %q, want infra_flake", rule)
	}
}

// TestRetryStage_Delegated_CategoryBUnmet: infra_flake requires a
// category-A failure — a delegated retry of a category-B (policy)
// failure is refused with the named predicate before any state change.
func TestRetryStage_Delegated_CategoryBUnmet(t *testing.T) {
	s, repo, au := delegatedRetryServer(t)
	stage := seedFailedStage(repo, run.FailureB, "forbidden_paths violated")
	seedDelegatedRun(repo, stage)

	w := postRetryBody(t, s, stage.ID, `{"delegated":true}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	errBody := decodeErrorEnvelope(t, w)
	reason, _ := errBody.Details["unmet_reason"].(string)
	if errBody.Code != "delegation_condition_unmet" ||
		!strings.Contains(reason, "failed stage category is B") {
		t.Errorf("error = %+v, want delegation_condition_unmet naming the category", errBody)
	}
	if stage.State != run.StageStateFailed {
		t.Errorf("stage state = %q, want failed (no state change on refusal)", stage.State)
	}
	if idx := auditEntriesByCategory(au, CategoryStageRetried); len(idx) != 0 {
		t.Errorf("stage_retried entries = %d after refusal, want 0", len(idx))
	}
}

// TestRetryStage_Delegated_NotConfigured pins fail-closed: a run whose
// cached spec is absent refuses a delegated retry outright.
func TestRetryStage_Delegated_NotConfigured(t *testing.T) {
	s, repo, _ := delegatedRetryServer(t)
	stage := seedFailedStage(repo, run.FailureA,
		"failed to start container: context deadline exceeded")
	repo.seedRun(&run.Run{ID: stage.RunID, State: run.StateRunning})

	w := postRetryBody(t, s, stage.ID, `{"delegated":true}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if errBody := decodeErrorEnvelope(t, w); errBody.Code != "delegation_not_configured" {
		t.Errorf("code = %q, want delegation_not_configured", errBody.Code)
	}
}

// TestRetryStage_OperatorAgentActorAttribution: a retry triggered under
// an operator-agent token records actor_kind=agent with the full token
// subject on the stage_retried entry (ADR-040 D4, #1027).
func TestRetryStage_OperatorAgentActorAttribution(t *testing.T) {
	s, repo, au := retryServer(t)
	stage := seedFailedStage(repo, run.FailureD, "sla_timeout: 5h elapsed (deadline 4h)")

	req := httptest.NewRequest(http.MethodPost, "/v0/stages/"+stage.ID.String()+"/retry", nil)
	req.SetPathValue("stage_id", stage.ID.String())
	w := httptest.NewRecorder()
	s.handleRetryStage(w, withOperatorAgentAuth(req))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if len(au.appended) != 1 || au.appended[0].Category != CategoryStageRetried {
		t.Fatalf("audit entries = %+v, want one stage_retried", au.appended)
	}
	entry := au.appended[0]
	if entry.ActorKind == nil || *entry.ActorKind != audit.ActorAgent {
		t.Errorf("ActorKind = %v, want agent", entry.ActorKind)
	}
	if entry.ActorSubject == nil || *entry.ActorSubject != operatorAgentSubject {
		t.Errorf("ActorSubject = %v, want %q", entry.ActorSubject, operatorAgentSubject)
	}
}

// --- Acceptance-reopen arm (E31.16 / #1567) ---

// reopenAuditFake wraps approvalAuditFake to make ListForRunByCategory
// seedable/erroring for the acceptance-reopen retry tests, without touching the
// shared fake. Every other audit.Repository method delegates to the embedded
// fake (field promotion), so au.appended still captures the acceptance_reopened
// write.
type reopenAuditFake struct {
	*approvalAuditFake
	byCategory    map[string][]*audit.Entry
	byCategoryErr error
	// catErr injects a read error on exactly one category (the #1682 Option C
	// tests drive a head-category read failure while the outcome read succeeds).
	catErr map[string]error
}

func (a *reopenAuditFake) ListForRunByCategory(_ context.Context, _ uuid.UUID, category string) ([]*audit.Entry, error) {
	if err := a.catErr[category]; err != nil {
		return nil, err
	}
	if a.byCategoryErr != nil {
		return nil, a.byCategoryErr
	}
	return a.byCategory[category], nil
}

// acceptanceOutcomeEntryWithHead builds an acceptance_outcome_recorded entry
// carrying the given stage id AND a head_sha payload (the #1682 binding the
// verdict to the head it validated).
func acceptanceOutcomeEntryWithHead(stageID uuid.UUID, headSHA string) *audit.Entry {
	sid := stageID
	payload, _ := json.Marshal(map[string]any{"head_sha": headSHA})
	return &audit.Entry{
		ID:       uuid.New(),
		StageID:  &sid,
		Category: CategoryAcceptanceOutcomeRecorded,
		Payload:  payload,
		Sequence: 1,
	}
}

// headReportEntry builds a head-report audit entry (fixup_pushed /
// child_pushed / pull_request_opened) carrying a head_sha payload.
func headReportEntry(category, headSHA string, seq int64) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{"head_sha": headSHA})
	return &audit.Entry{ID: uuid.New(), Category: category, Payload: payload, Sequence: seq}
}

// reopenRetryServer wires the retry handler with a reopenAuditFake so the
// acceptance-reopen branch's ListForRunByCategory existence check is
// controllable.
func reopenRetryServer(t *testing.T) (*Server, *approvalRunRepo, *reopenAuditFake) {
	t.Helper()
	repo := newApprovalRunRepo()
	au := &reopenAuditFake{approvalAuditFake: newApprovalAuditFake()}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      repo,
		AuditRepo:    au,
		ApprovalRepo: newFakeApprovalRepo(),
	})
	return s, repo, au
}

// seedSucceededAcceptanceStage seeds an acceptance stage settled `succeeded`
// with a non-terminal run so run.ReopenAcceptanceStage's type/state/run-terminal
// gate passes on the admitted path. runState lets a test seed a terminal run
// (b5) to exercise the reopen refusal.
func seedSucceededAcceptanceStage(repo *approvalRunRepo, runState run.State) *run.Stage {
	st := repo.seedStage(run.StageStateSucceeded)
	repo.mu.Lock()
	st.Type = run.StageTypeAcceptance
	repo.mu.Unlock()
	repo.seedRun(&run.Run{ID: st.RunID, State: runState})
	return st
}

// acceptanceOutcomeEntryFor builds an acceptance_outcome_recorded audit entry
// carrying the given stage id — the existence signal the reopen guard checks.
func acceptanceOutcomeEntryFor(stageID uuid.UUID) *audit.Entry {
	sid := stageID
	return &audit.Entry{
		ID:       uuid.New(),
		StageID:  &sid,
		Category: CategoryAcceptanceOutcomeRecorded,
	}
}

// b1: a succeeded acceptance stage with NO recorded outcome re-opens to pending,
// writes an acceptance_reopened audit entry with prior_state=succeeded, and
// leaves the stage pending (no orchestrator wired → the handoff is skipped).
func TestRetryStage_AcceptanceReopen_HappyPath(t *testing.T) {
	s, repo, au := reopenRetryServer(t)
	stage := seedSucceededAcceptanceStage(repo, run.StateRunning)

	w := postRetry(t, s, stage.ID)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var body stageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.State != string(run.StageStatePending) {
		t.Errorf("body.State = %q, want pending after reopen", body.State)
	}
	if len(au.appended) != 1 || au.appended[0].Category != CategoryAcceptanceReopened {
		t.Fatalf("audit chain = %+v, want one acceptance_reopened entry", au.appended)
	}
	var payload map[string]any
	if err := json.Unmarshal(au.appended[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	if payload["prior_state"] != string(run.StageStateSucceeded) {
		t.Errorf("payload.prior_state = %v, want succeeded", payload["prior_state"])
	}
	if payload["stage_id"] != stage.ID.String() {
		t.Errorf("payload.stage_id = %v, want %s", payload["stage_id"], stage.ID)
	}
}

// b1b: with an orchestrator wired, the reopen hands off to it — the reopened
// pending acceptance stage walks pending → dispatched (the agent dispatch path;
// fireDispatch no-ops with no GitHub client) and the response reflects the
// POST-advance GetStage refresh (dispatched), not the intermediate pending
// state. This is the recovery behavior b1 leaves untested by running with no
// orchestrator: both the Orchestrator.Advance call and the dec.Stage = updated
// refresh only execute when an orchestrator is present.
func TestRetryStage_AcceptanceReopen_OrchestratorAdvances(t *testing.T) {
	repo := newApprovalRunRepo()
	au := &reopenAuditFake{approvalAuditFake: newApprovalAuditFake()}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      repo,
		AuditRepo:    au,
		ApprovalRepo: newFakeApprovalRepo(),
		// No GitHub: the agent dispatch's workflow_dispatch is skipped, but the
		// pending → dispatched state transition still happens.
		Orchestrator: &orchestrator.Orchestrator{Runs: repo},
	})
	stage := seedSucceededAcceptanceStage(repo, run.StateRunning)

	w := postRetry(t, s, stage.ID)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var body stageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The orchestrator advanced the reopened stage; the response reflects the
	// post-advance refresh. Without the Advance handoff OR the GetStage
	// re-fetch, this would still read pending.
	if body.State != string(run.StageStateDispatched) {
		t.Errorf("body.State = %q, want dispatched (orchestrator advance + post-advance refresh)", body.State)
	}
	// The stage genuinely moved in the repo, not just in the response.
	if got, _ := repo.GetStage(context.Background(), stage.ID); got.State != run.StageStateDispatched {
		t.Errorf("stage state = %q, want dispatched after orchestrator advance", got.State)
	}
	// The acceptance_reopened audit entry is still written (audit-first, before
	// the handoff).
	if len(au.appended) != 1 || au.appended[0].Category != CategoryAcceptanceReopened {
		t.Fatalf("audit chain = %+v, want one acceptance_reopened entry", au.appended)
	}
}

// b2: an acceptance_outcome_recorded entry EXISTS for the stage → 422
// retry_not_applicable, no transition, no audit write (verdict-ful routing
// belongs to the deterministic triage).
func TestRetryStage_AcceptanceReopen_VerdictRecorded_422(t *testing.T) {
	s, repo, au := reopenRetryServer(t)
	stage := seedSucceededAcceptanceStage(repo, run.StateRunning)
	au.byCategory = map[string][]*audit.Entry{
		CategoryAcceptanceOutcomeRecorded: {acceptanceOutcomeEntryFor(stage.ID)},
	}

	w := postRetry(t, s, stage.ID)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "retry_not_applicable") {
		t.Errorf("body missing retry_not_applicable:\n%s", w.Body.String())
	}
	if got, _ := repo.GetStage(context.Background(), stage.ID); got.State != run.StageStateSucceeded {
		t.Errorf("stage state = %q, want unchanged succeeded", got.State)
	}
	if len(au.appended) != 0 {
		t.Errorf("audit chain = %+v, want no write on the 422 refusal", au.appended)
	}
}

// b2b: an acceptance_outcome_recorded entry for a DIFFERENT stage does NOT block
// the reopen — the guard filters by stage id (locks the StageID membership
// check, not a bare category-non-empty check).
func TestRetryStage_AcceptanceReopen_OtherStageOutcome_Reopens(t *testing.T) {
	s, repo, au := reopenRetryServer(t)
	stage := seedSucceededAcceptanceStage(repo, run.StateRunning)
	au.byCategory = map[string][]*audit.Entry{
		CategoryAcceptanceOutcomeRecorded: {acceptanceOutcomeEntryFor(uuid.New())},
	}

	w := postRetry(t, s, stage.ID)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (other-stage outcome must not block):\n%s", w.Code, w.Body.String())
	}
	if len(au.appended) != 1 || au.appended[0].Category != CategoryAcceptanceReopened {
		t.Fatalf("audit chain = %+v, want one acceptance_reopened entry", au.appended)
	}
}

// b3: an mcp:run:* subject (agent) token invoking the reopen → 403
// agent_token_forbidden (operator-only verb).
func TestRetryStage_AcceptanceReopen_AgentTokenForbidden(t *testing.T) {
	s, repo, au := reopenRetryServer(t)
	stage := seedSucceededAcceptanceStage(repo, run.StateRunning)

	w := postRetryMCP(t, s, stage.ID, stage.RunID) // mcp:run:<runID> subject

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "agent_token_forbidden") {
		t.Errorf("body missing agent_token_forbidden:\n%s", w.Body.String())
	}
	if len(au.appended) != 0 {
		t.Errorf("audit chain = %+v, want no write on the 403 refusal", au.appended)
	}
}

// b4: an audit ListForRunByCategory error → 500, no transition (fail closed on
// unknown evidence state — never reopen when we can't prove the stage is
// outcome-less).
func TestRetryStage_AcceptanceReopen_AuditListError_500(t *testing.T) {
	s, repo, au := reopenRetryServer(t)
	stage := seedSucceededAcceptanceStage(repo, run.StateRunning)
	au.byCategoryErr = errors.New("audit read down")

	w := postRetry(t, s, stage.ID)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
	if got, _ := repo.GetStage(context.Background(), stage.ID); got.State != run.StageStateSucceeded {
		t.Errorf("stage state = %q, want unchanged succeeded (fail closed)", got.State)
	}
	if len(au.appended) != 0 {
		t.Errorf("audit chain = %+v, want no write on the 500 fail-closed path", au.appended)
	}
}

// b5: a terminal run makes run.ReopenAcceptanceStage refuse with
// ErrAcceptanceReopenNotApplicable → 422.
func TestRetryStage_AcceptanceReopen_TerminalRun_422(t *testing.T) {
	s, repo, _ := reopenRetryServer(t)
	stage := seedSucceededAcceptanceStage(repo, run.StateSucceeded) // terminal run

	w := postRetry(t, s, stage.ID)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "retry_not_applicable") {
		t.Errorf("body missing retry_not_applicable:\n%s", w.Body.String())
	}
}

// b5b: a non-sentinel error from run.ReopenAcceptanceStage (here a
// TransitionStage failure) maps to 500, not 422 — only
// ErrAcceptanceReopenNotApplicable is a 422; any other reopen error surfaces
// as internal_error without a transition.
func TestRetryStage_AcceptanceReopen_ReopenError_500(t *testing.T) {
	s, repo, au := reopenRetryServer(t)
	stage := seedSucceededAcceptanceStage(repo, run.StateRunning)
	repo.transitionErr = errors.New("transition boom")

	w := postRetry(t, s, stage.ID)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
	if len(au.appended) != 0 {
		t.Errorf("audit chain = %+v, want no write when the reopen errored", au.appended)
	}
}

// --- Option C: head-aware retry admit (#1682) ---

// C1: a verdict recorded against a STALE head (recorded head != run's current
// head, because a fix-up push landed a new commit) ADMITS the re-open, on
// Option C's own logic — constructed by seeding the stale-head state directly
// (recorded H1, current head H2), NOT via a pre-reopen timing window that
// Option A would have closed (binding condition 3).
func TestRetryStage_AcceptanceReopen_StaleHead_Admits(t *testing.T) {
	s, repo, au := reopenRetryServer(t)
	stage := seedSucceededAcceptanceStage(repo, run.StateRunning)
	au.byCategory = map[string][]*audit.Entry{
		CategoryAcceptanceOutcomeRecorded: {acceptanceOutcomeEntryWithHead(stage.ID, "H1oldhead")},
		// A fix-up pushed a NEW head after the verdict — the run's current head.
		"fixup_pushed": {headReportEntry("fixup_pushed", "H2newhead", 5)},
	}

	w := postRetry(t, s, stage.ID)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stale-head verdict admits):\n%s", w.Code, w.Body.String())
	}
	if got, _ := repo.GetStage(context.Background(), stage.ID); got.State != run.StageStatePending {
		t.Errorf("stage state = %q, want pending after stale-head reopen", got.State)
	}
	if len(au.appended) != 1 || au.appended[0].Category != CategoryAcceptanceReopened {
		t.Fatalf("audit chain = %+v, want one acceptance_reopened entry", au.appended)
	}
}

// C2: a verdict recorded against the run's CURRENT head (recorded == current)
// keeps the 422 — the verdict still corresponds to the head, so deterministic
// triage owns it, not a re-open.
func TestRetryStage_AcceptanceReopen_CurrentHead_422(t *testing.T) {
	s, repo, au := reopenRetryServer(t)
	stage := seedSucceededAcceptanceStage(repo, run.StateRunning)
	au.byCategory = map[string][]*audit.Entry{
		CategoryAcceptanceOutcomeRecorded: {acceptanceOutcomeEntryWithHead(stage.ID, "H1same")},
		"pull_request_opened":             {headReportEntry("pull_request_opened", "H1same", 1)},
	}

	w := postRetry(t, s, stage.ID)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (verdict matches current head):\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "retry_not_applicable") {
		t.Errorf("body missing retry_not_applicable:\n%s", w.Body.String())
	}
	if len(au.appended) != 0 {
		t.Errorf("audit chain = %+v, want no write on the 422", au.appended)
	}
}

// C3: a verdict with NO head_sha (a pre-#1682 record) fails closed to the 422
// even when a current head resolves — an empty recorded head cannot prove
// staleness, so the conservative refusal holds.
func TestRetryStage_AcceptanceReopen_NoRecordedHead_422(t *testing.T) {
	s, repo, au := reopenRetryServer(t)
	stage := seedSucceededAcceptanceStage(repo, run.StateRunning)
	au.byCategory = map[string][]*audit.Entry{
		CategoryAcceptanceOutcomeRecorded: {acceptanceOutcomeEntryFor(stage.ID)}, // no head_sha payload
		"fixup_pushed":                    {headReportEntry("fixup_pushed", "H2newhead", 5)},
	}

	w := postRetry(t, s, stage.ID)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (empty recorded head fails closed):\n%s", w.Code, w.Body.String())
	}
	if len(au.appended) != 0 {
		t.Errorf("audit chain = %+v, want no write on the 422", au.appended)
	}
}

// C4: a verdict with a recorded head but NO resolvable current head (no
// head-report entries) fails closed to the 422 — an unresolvable current head
// cannot prove staleness.
func TestRetryStage_AcceptanceReopen_CurrentHeadUnresolvable_422(t *testing.T) {
	s, repo, au := reopenRetryServer(t)
	stage := seedSucceededAcceptanceStage(repo, run.StateRunning)
	au.byCategory = map[string][]*audit.Entry{
		CategoryAcceptanceOutcomeRecorded: {acceptanceOutcomeEntryWithHead(stage.ID, "H1oldhead")},
		// No fixup_pushed / child_pushed / pull_request_opened entries seeded.
	}

	w := postRetry(t, s, stage.ID)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (current head unresolvable fails closed):\n%s", w.Code, w.Body.String())
	}
	if len(au.appended) != 0 {
		t.Errorf("audit chain = %+v, want no write on the 422", au.appended)
	}
}

// C5: a read error resolving the CURRENT head (the outcome read succeeds, but a
// head-category read errors) surfaces as 500 — never a spurious admit on an
// unreadable head.
func TestRetryStage_AcceptanceReopen_CurrentHeadReadError_500(t *testing.T) {
	s, repo, au := reopenRetryServer(t)
	stage := seedSucceededAcceptanceStage(repo, run.StateRunning)
	au.byCategory = map[string][]*audit.Entry{
		CategoryAcceptanceOutcomeRecorded: {acceptanceOutcomeEntryWithHead(stage.ID, "H1oldhead")},
	}
	au.catErr = map[string]error{"fixup_pushed": errors.New("head read down")}

	w := postRetry(t, s, stage.ID)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (head read error):\n%s", w.Code, w.Body.String())
	}
	if got, _ := repo.GetStage(context.Background(), stage.ID); got.State != run.StageStateSucceeded {
		t.Errorf("stage state = %q, want unchanged succeeded (fail closed)", got.State)
	}
	if len(au.appended) != 0 {
		t.Errorf("audit chain = %+v, want no write on the 500", au.appended)
	}
}

// b6: a SUCCEEDED non-acceptance stage is NOT caught by the reopen branch — it
// falls through to run.RetryStage and 422s (failed-stages-only invariant intact).
func TestRetryStage_SucceededNonAcceptance_Still422(t *testing.T) {
	s, repo, _ := retryServer(t)
	stage := repo.seedStage(run.StageStateSucceeded) // Type=plan, succeeded

	w := postRetry(t, s, stage.ID)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 (non-acceptance succeeded stage rides the retry path):\n%s", w.Code, w.Body.String())
	}
}
