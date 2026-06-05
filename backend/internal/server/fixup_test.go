package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// fixupServer wires the run + audit fakes the fix-up handler needs.
// auditFake (from trace_test.go) supports seeded + appended
// ListForRunByCategory, which the handler uses to resolve concerns and
// count prior fix-up passes.
func fixupServer(t *testing.T) (*Server, *approvalRunRepo, *auditFake) {
	t.Helper()
	repo := newApprovalRunRepo()
	au := newAuditFake()
	s := New(Config{
		Addr:      "127.0.0.1:0",
		RunRepo:   repo,
		AuditRepo: au,
	})
	return s, repo, au
}

// seedImplementGateStage seeds an implement stage parked at the review
// gate (awaiting_approval) — the precondition for a fix-up.
func seedImplementGateStage(repo *approvalRunRepo) *run.Stage {
	return repo.seedGatelessStage(run.StageStateAwaitingApproval)
}

// seedConcernsReview records an implement_reviewed audit entry carrying
// an approve_with_concerns verdict with the given concerns, so the
// handler resolves them as the addressable concern set.
func seedConcernsReview(au *auditFake, stage *run.Stage, concerns ...planreview.Concern) {
	payload, _ := json.Marshal(planreview.ImplementReviewedPayload{
		ReviewerKind: "agent",
		Authority:    planreview.AuthorityAdvisory,
		Verdict:      planreview.VerdictApproveWithConcerns,
		Concerns:     concerns,
	})
	rid := stage.RunID
	sid := stage.ID
	au.seeded = append(au.seeded, &audit.Entry{
		RunID:    &rid,
		StageID:  &sid,
		Category: "implement_reviewed",
		Payload:  payload,
	})
}

// seedPushOpenPRStages seeds the push_and_open_pr shape (#780): an
// implement stage that has SUCCEEDED (PR opened) plus a review stage in
// the given state, both sharing one RunID. Returns (implement, review).
func seedPushOpenPRStages(repo *approvalRunRepo, reviewState run.StageState) (*run.Stage, *run.Stage) {
	impl := repo.seedGatelessStage(run.StageStateSucceeded)
	review := repo.seedStage(reviewState)
	repo.mu.Lock()
	review.RunID = impl.RunID
	review.Type = run.StageTypeReview
	review.Sequence = 2
	repo.mu.Unlock()
	return impl, review
}

func postFixup(t *testing.T, s *Server, stageID uuid.UUID, body fixupRequest) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	url := "/v0/stages/" + stageID.String() + "/fixup"
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	req.SetPathValue("stage_id", stageID.String())
	w := httptest.NewRecorder()
	s.handleFixupStage(w, withAuth(req))
	return w
}

func TestFixupStage_HappyPath(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "edited an out-of-scope file"},
		planreview.Concern{Severity: planreview.SeverityLow, Category: "style", Note: "naming nit"},
	)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}, Reason: "address the scope drift"})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var body stageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// No orchestrator wired → stage stays in pending after the re-open.
	if body.State != string(run.StageStatePending) {
		t.Errorf("body.State = %q, want pending", body.State)
	}

	// One stage_fixup_triggered audit entry with the selected concern.
	if len(au.appended) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(au.appended))
	}
	got := au.appended[0]
	if got.Category != CategoryStageFixupTriggered {
		t.Errorf("audit category = %q, want %s", got.Category, CategoryStageFixupTriggered)
	}
	var payload map[string]any
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	if payload["pass_ordinal"].(float64) != 1 {
		t.Errorf("pass_ordinal = %v, want 1", payload["pass_ordinal"])
	}
	if payload["remaining_budget"].(float64) != 0 {
		t.Errorf("remaining_budget = %v, want 0", payload["remaining_budget"])
	}
	// The resolved selected concern must be persisted for the prompt
	// renderer to read back.
	concerns, ok := payload["concerns"].([]any)
	if !ok || len(concerns) != 1 {
		t.Fatalf("payload.concerns = %v, want one resolved concern", payload["concerns"])
	}
	c0 := concerns[0].(map[string]any)
	if c0["category"] != "scope" {
		t.Errorf("selected concern category = %v, want scope", c0["category"])
	}
}

func TestFixupStage_PushOpenPRReopensAndReparks(t *testing.T) {
	s, repo, au := fixupServer(t)
	impl, review := seedPushOpenPRStages(repo, run.StageStateAwaitingApproval)
	seedConcernsReview(au, impl,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "out-of-scope file"},
	)

	w := postFixup(t, s, impl.ID, fixupRequest{Concerns: []int{0}, Reason: "address scope drift on the PR branch"})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var body stageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Implement re-opened to pending (no orchestrator wired).
	if body.State != string(run.StageStatePending) {
		t.Errorf("implement state = %q, want pending", body.State)
	}
	// Review re-parked to pending.
	curReview, err := repo.GetStage(context.Background(), review.ID)
	if err != nil {
		t.Fatalf("GetStage(review): %v", err)
	}
	if curReview.State != run.StageStatePending {
		t.Errorf("review state = %q, want pending (re-parked)", curReview.State)
	}

	// One audit entry carrying the re-parked review stage id.
	if len(au.appended) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(au.appended))
	}
	var payload map[string]any
	if err := json.Unmarshal(au.appended[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	if payload["prior_state"] != string(run.StageStateSucceeded) {
		t.Errorf("prior_state = %v, want succeeded", payload["prior_state"])
	}
	if payload["reparked_review_stage_id"] != review.ID.String() {
		t.Errorf("reparked_review_stage_id = %v, want %s", payload["reparked_review_stage_id"], review.ID)
	}
}

func TestFixupStage_PushOpenPRReviewResolvedReturns422(t *testing.T) {
	s, repo, au := fixupServer(t)
	// Review gate already closed (merged/succeeded): no longer a fix-up
	// candidate even though the implement stage succeeded.
	impl, _ := seedPushOpenPRStages(repo, run.StageStateSucceeded)
	seedConcernsReview(au, impl,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	w := postFixup(t, s, impl.ID, fixupRequest{Concerns: []int{0}})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fixup_not_applicable") {
		t.Errorf("body missing fixup_not_applicable code: %s", w.Body.String())
	}
	// No fix-up audit entry written on the refusal.
	for _, e := range au.appended {
		if e.Category == CategoryStageFixupTriggered {
			t.Errorf("unexpected stage_fixup_triggered entry on refused fix-up")
		}
	}
}

func TestFixupStage_SecondPassRefused(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	// First pass succeeds (lands in pending, no orchestrator).
	if w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}}); w.Code != http.StatusOK {
		t.Fatalf("first fixup status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	// Re-park the stage at the gate to model the re-review landing on
	// awaiting_approval again, so the only thing blocking the 2nd pass
	// is the bound — not the state machine.
	repo.mu.Lock()
	repo.stages[stage.ID].State = run.StageStateAwaitingApproval
	repo.mu.Unlock()

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("second fixup status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fixup_budget_exhausted") {
		t.Errorf("body missing fixup_budget_exhausted code: %s", w.Body.String())
	}
	// Still exactly one fix-up audit entry — the refused pass wrote none.
	n := 0
	for _, e := range au.appended {
		if e.Category == CategoryStageFixupTriggered {
			n++
		}
	}
	if n != 1 {
		t.Errorf("stage_fixup_triggered entries = %d, want 1", n)
	}
}

func TestFixupStage_NoConcernsSelectedReturns400(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: nil})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
}

func TestFixupStage_OutOfRangeIndexReturns400(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{5}})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
}

func TestFixupStage_DuplicateIndexReturns400(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
		planreview.Concern{Severity: planreview.SeverityLow, Category: "verification", Note: "untested"},
	)

	// Both indices are in range, but the duplicate must be rejected by
	// selectConcerns (mapped to 400) — not silently deduplicated.
	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0, 0}})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a duplicate concern index:\n%s", w.Code, w.Body.String())
	}
}

func TestFixupStage_NoRecordedConcernsReturns422(t *testing.T) {
	s, repo, _ := fixupServer(t)
	stage := seedImplementGateStage(repo)
	// No implement_reviewed entry seeded.

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fixup_not_applicable") {
		t.Errorf("body missing fixup_not_applicable code: %s", w.Body.String())
	}
}

func TestFixupStage_WrongStateReturns422(t *testing.T) {
	s, repo, au := fixupServer(t)
	// Implement stage that is running, not parked at the gate.
	stage := repo.seedGatelessStage(run.StageStateRunning)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fixup_not_applicable") {
		t.Errorf("body missing fixup_not_applicable code: %s", w.Body.String())
	}
}

func TestFixupStage_NonImplementStageReturns422(t *testing.T) {
	s, repo, au := fixupServer(t)
	// Plan stage parked at the gate is not a fix-up candidate.
	stage := repo.seedStage(run.StageStateAwaitingApproval)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fixup_not_applicable") {
		t.Errorf("body missing fixup_not_applicable code: %s", w.Body.String())
	}
}

func TestFixupStage_UnauthenticatedReturns401(t *testing.T) {
	s, repo, _ := fixupServer(t)
	stage := seedImplementGateStage(repo)

	raw, _ := json.Marshal(fixupRequest{Concerns: []int{0}})
	req := httptest.NewRequest(http.MethodPost, "/v0/stages/"+stage.ID.String()+"/fixup", bytes.NewReader(raw))
	req.SetPathValue("stage_id", stage.ID.String())
	w := httptest.NewRecorder()
	s.handleFixupStage(w, req) // no identity injected → anonymous

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401:\n%s", w.Code, w.Body.String())
	}
}

// --- Subject-binding guard ---

func withMCPFixupAuth(req *http.Request, runID uuid.UUID) *http.Request {
	id := Identity{
		Subject: "mcp:run:" + runID.String(),
		TokenID: "tok-test",
		Scopes:  []string{"mcp:read", "write:fixups"},
	}
	return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
}

func TestFixupStage_MCPTokenMismatchedRunReturns403(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)
	otherRunID := uuid.New() // does not match stage.RunID

	raw, _ := json.Marshal(fixupRequest{Concerns: []int{0}})
	req := httptest.NewRequest(http.MethodPost, "/v0/stages/"+stage.ID.String()+"/fixup", bytes.NewReader(raw))
	req.SetPathValue("stage_id", stage.ID.String())
	w := httptest.NewRecorder()
	s.handleFixupStage(w, withMCPFixupAuth(req, otherRunID))

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "cross_run_fixup") {
		t.Errorf("body missing cross_run_fixup code: %s", w.Body.String())
	}
}
