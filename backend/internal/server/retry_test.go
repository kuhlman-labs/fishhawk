package server

import (
	"context"
	"encoding/json"
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

func payloadKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
