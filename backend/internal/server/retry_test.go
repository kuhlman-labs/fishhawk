package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

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
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
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

func TestRetryStage_AReturns501(t *testing.T) {
	s, repo, _ := retryServer(t)
	stage := seedFailedStage(repo, run.FailureA, "agent crashed")

	w := postRetry(t, s, stage.ID)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", w.Code)
	}
	if !strings.Contains(w.Body.String(), "retry_not_implemented") {
		t.Errorf("body missing retry_not_implemented code: %s", w.Body.String())
	}
}

func TestRetryStage_CReturns501(t *testing.T) {
	s, repo, _ := retryServer(t)
	stage := seedFailedStage(repo, run.FailureC, "dispatch_watchdog: 70m elapsed")

	w := postRetry(t, s, stage.ID)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", w.Code)
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
