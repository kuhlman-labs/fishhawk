package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan/planfixture"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// recordingOrchestratorRepo wraps orchestratorRepo to record every
// successful TransitionStage target, so a sequence test can assert that a
// plan stage never passed through awaiting_approval. The embedded repo
// validates the stage/run transition tables and mutates state, and a real
// orchestrator.Orchestrator can drive it, so the run advances exactly as
// in production. (#603)
type recordingOrchestratorRepo struct {
	*orchestratorRepo
	stageTransitions []run.StageState
}

func (r *recordingOrchestratorRepo) TransitionStage(ctx context.Context, id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	st, err := r.orchestratorRepo.TransitionStage(ctx, id, to, c)
	if err == nil {
		r.stageTransitions = append(r.stageTransitions, to)
	}
	return st, err
}

func (r *recordingOrchestratorRepo) sawTransitionTo(to run.StageState) bool {
	for _, s := range r.stageTransitions {
		if s == to {
			return true
		}
	}
	return false
}

// newPlanSequenceServer wires the full plan-stage path — signing, trace
// store, audit, artifacts, a transition-recording run repo, and a real
// orchestrator — so a test can drive the runner's true trace-then-plan
// upload order and observe the resulting stage + run states. (#603)
func newPlanSequenceServer(t *testing.T) (*Server, *recordingOrchestratorRepo, *fakeArtifactRepo, *signingFake) {
	t.Helper()
	rr := &recordingOrchestratorRepo{orchestratorRepo: newOrchestratorRepo()}
	art := newFakeArtifactRepo()
	sf := newSigningFake()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  sf,
		TraceStore:   newTraceStoreFake(),
		AuditRepo:    newAuditFake(),
		RunRepo:      rr,
		ArtifactRepo: art,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})
	return s, rr, art, sf
}

// TestPlanStage_TraceThenInvalidPlan_EndsFailed reproduces the #603
// sequence: the runner ships the trace (which no longer advances a plan
// stage past running) and then an invalid plan. The stage must end in
// failed — never awaiting_approval — and the run must be advanced to
// terminal failed rather than stranded.
func TestPlanStage_TraceThenInvalidPlan_EndsFailed(t *testing.T) {
	s, rr, _, sf := newPlanSequenceServer(t)
	runRow := rr.seedRun()
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateDispatched)
	planStage.RequiresApproval = true // gated plan stage — the #603 shape
	priv, _ := sf.issue(t, runRow.ID)

	// 1. Trace upload (raw). With no plan artifact yet, the trace handler
	//    must leave the plan stage in running.
	wt := shipRequest(t, s, runRow.ID, planStage.ID, "raw", priv, []byte("b"), "")
	if wt.Code != http.StatusAccepted {
		t.Fatalf("trace status = %d, want 202:\n%s", wt.Code, wt.Body.String())
	}
	if got := rr.stagesByID[planStage.ID].State; got != run.StageStateRunning {
		t.Fatalf("after trace: stage state = %q, want running (plan stage must not advance without a plan artifact)", got)
	}

	// 2. Plan upload with an invalid body.
	wp := shipPlanRequest(t, s, runRow.ID, planStage.ID, priv,
		[]byte(`{"plan_version":"standard_v1"}`), "")
	if wp.Code != http.StatusBadRequest {
		t.Fatalf("plan status = %d, want 400:\n%s", wp.Code, wp.Body.String())
	}

	// The plan stage never reached a human gate with no plan to review.
	if rr.sawTransitionTo(run.StageStateAwaitingApproval) {
		t.Errorf("stage transitioned to awaiting_approval; #603 requires it stay out of the gate with no valid plan\ntransitions: %+v", rr.stageTransitions)
	}
	// The stage ends failed.
	if got := rr.stagesByID[planStage.ID].State; got != run.StageStateFailed {
		t.Errorf("final stage state = %q, want failed", got)
	}
	// The run is advanced to terminal failed (the stranded-run half).
	if got := rr.runs[runRow.ID].State; got != run.StateFailed {
		t.Errorf("final run state = %q, want failed (run must not strand once the stage fails)", got)
	}
}

// TestPlanStage_TraceThenValidPlan_AdvancesToAwaitingApproval is the happy
// path of the #603 reordering: the trace leaves the gated plan stage in
// running, and the plan-upload handler drives it to awaiting_approval once
// the valid plan lands.
func TestPlanStage_TraceThenValidPlan_AdvancesToAwaitingApproval(t *testing.T) {
	s, rr, _, sf := newPlanSequenceServer(t)
	runRow := rr.seedRun()
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateDispatched)
	planStage.RequiresApproval = true
	priv, _ := sf.issue(t, runRow.ID)

	wt := shipRequest(t, s, runRow.ID, planStage.ID, "raw", priv, []byte("b"), "")
	if wt.Code != http.StatusAccepted {
		t.Fatalf("trace status = %d, want 202:\n%s", wt.Code, wt.Body.String())
	}
	if got := rr.stagesByID[planStage.ID].State; got != run.StageStateRunning {
		t.Fatalf("after trace: stage state = %q, want running", got)
	}

	wp := shipPlanRequest(t, s, runRow.ID, planStage.ID, priv, validPlanBytes(t), "")
	if wp.Code != http.StatusCreated {
		t.Fatalf("plan status = %d, want 201:\n%s", wp.Code, wp.Body.String())
	}

	if got := rr.stagesByID[planStage.ID].State; got != run.StageStateAwaitingApproval {
		t.Errorf("final stage state = %q, want awaiting_approval (plan handler must drive the terminal advance)", got)
	}
}

// fakePlanReviewer records each Review invocation and returns a canned verdict.
type fakePlanReviewer struct {
	mu      sync.Mutex
	calls   []string
	verdict *planreview.ReviewVerdict
	model   string
	err     error
}

func (f *fakePlanReviewer) Review(_ context.Context, promptText string) (*planreview.ReviewVerdict, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, promptText)
	if f.err != nil {
		return nil, "", f.err
	}
	return f.verdict, f.model, nil
}

// blockingPlanReviewer blocks each Review call until release is closed,
// so async-dispatch tests can assert ordering deterministically without
// sleeps. started carries a value the first time Review is entered, so a
// test can wait until the reviewer goroutine is actually in-flight before
// cancelling a context or asserting on the upload response.
//
// Review returns ctx.Err() when the context is cancelled — modelling a
// real reviewer adapter whose exec.CommandContext kills `claude` on
// cancellation. That makes it a load-bearing probe for the #584
// context-detach guarantee: if the detached goroutine were handed the
// request context instead of context.WithoutCancel'd one, a parent cancel
// would surface here as an error and no audit entry would land.
type blockingPlanReviewer struct {
	release chan struct{}
	started chan struct{}
	mu      sync.Mutex
	calls   int
	verdict *planreview.ReviewVerdict
	model   string
}

func newBlockingPlanReviewer(v *planreview.ReviewVerdict, model string) *blockingPlanReviewer {
	return &blockingPlanReviewer{
		release: make(chan struct{}),
		started: make(chan struct{}, 1),
		verdict: v,
		model:   model,
	}
}

func (b *blockingPlanReviewer) Review(ctx context.Context, _ string) (*planreview.ReviewVerdict, string, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	return b.verdict, b.model, nil
}

// reviewers YAML fragments for plan stage specs. These are complete
// minimal workflow specs accepted by spec.ParseBytes.
var (
	specGatingReviewers = []byte(`version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          agent: 1
          human: 0
        produces:
          - artifact: plan
            schema: standard_v1
`)

	specAdvisoryReviewers = []byte(`version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          agent: 1
          human: 1
        produces:
          - artifact: plan
            schema: standard_v1
`)

	specNoReviewers = []byte(`version: "0.3"
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
`)
)

// newPlanServerWithReviewer builds a plan server that also wires a
// PlanReviewer, run data (for GetRun), and a workflow spec on the run.
func newPlanServerWithReviewer(t *testing.T, runID, stageID uuid.UUID, reviewer PlanReviewer, workflowSpec []byte) (*Server, *signingFake, *fakeArtifactRepo, *auditFake, *promptRunRepo) {
	t.Helper()
	sf := newSigningFake()
	ar := newFakeArtifactRepo()
	au := newAuditFake()
	rr := newPromptRunRepo()
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID}
	rr.getRuns[runID] = &run.Run{
		ID:           runID,
		Repo:         "kuhlman-labs/example",
		WorkflowID:   "feature_change",
		WorkflowSpec: workflowSpec,
	}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  sf,
		ArtifactRepo: ar,
		AuditRepo:    au,
		RunRepo:      rr,
		PlanReviewer: reviewer,
	})
	return s, sf, ar, au, rr
}

// validPlanBytes returns a minimal standard_v1 plan that satisfies
// the schema. The same fixture is used across happy / idempotency
// tests so the content_hash matches.
func validPlanBytes(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(planfixture.Valid())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// newPlanServer wires SigningRepo + ArtifactRepo + AuditRepo + RunRepo
// for the plan handler. Stages are seeded into the repo so GetStage
// returns the right RunID.
func newPlanServer(t *testing.T, runID, stageID uuid.UUID) (*Server, *signingFake, *fakeArtifactRepo, *auditFake, *promptRunRepo) {
	t.Helper()
	sf := newSigningFake()
	ar := newFakeArtifactRepo()
	au := newAuditFake()
	rr := newPromptRunRepo()
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  sf,
		ArtifactRepo: ar,
		AuditRepo:    au,
		RunRepo:      rr,
	})
	return s, sf, ar, au, rr
}

// shipPlanRequest builds a POST /v0/runs/{run_id}/plan request signed
// by `priv`. Returns the recorded response.
func shipPlanRequest(t *testing.T, s *Server, runID, stageID uuid.UUID, priv ed25519.PrivateKey, body []byte, sigOverride string) *httptest.ResponseRecorder {
	t.Helper()
	url := fmt.Sprintf("/v0/runs/%s/plan?stage_id=%s", runID, stageID)
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sigOverride != "" {
		req.Header.Set("X-Fishhawk-Signature", sigOverride)
	} else if priv != nil {
		sig := ed25519.Sign(priv, signing.ComputeMessage(body))
		req.Header.Set("X-Fishhawk-Signature", hex.EncodeToString(sig))
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func TestShipPlan_HappyPath(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, _ := newPlanServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := validPlanBytes(t)

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	var resp planResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SchemaVersion != "standard_v1" {
		t.Errorf("schema_version = %q", resp.SchemaVersion)
	}
	if resp.StageID != stageID {
		t.Errorf("stage_id = %s, want %s", resp.StageID, stageID)
	}
	if resp.Idempotent {
		t.Error("first upload should not be marked idempotent")
	}

	// One artifact row.
	if len(ar.all) != 1 {
		t.Errorf("artifacts = %d, want 1", len(ar.all))
	}

	// One audit entry, category plan_generated.
	if len(au.appended) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(au.appended))
	}
	if got := au.appended[0].Category; got != "plan_generated" {
		t.Errorf("audit category = %q, want plan_generated", got)
	}
}

func TestShipPlan_Idempotent_SecondUploadReturnsExisting(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, _ := newPlanServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := validPlanBytes(t)

	// First upload.
	w1 := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w1.Code != http.StatusCreated {
		t.Fatalf("first upload status = %d", w1.Code)
	}

	// Second upload of identical bytes.
	w2 := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w2.Code != http.StatusOK {
		t.Fatalf("second upload status = %d, want 200:\n%s", w2.Code, w2.Body.String())
	}
	var resp planResponse
	_ = json.NewDecoder(w2.Body).Decode(&resp)
	if !resp.Idempotent {
		t.Error("second upload should be marked idempotent=true")
	}
	if len(ar.all) != 1 {
		t.Errorf("artifacts = %d, want 1 (no duplicate row)", len(ar.all))
	}
	// No second audit entry — plan_generated fires once per artifact.
	if len(au.appended) != 1 {
		t.Errorf("audit entries = %d, want 1 (no second plan_generated)", len(au.appended))
	}
}

func TestShipPlan_CoercibleSchemaError_Returns201(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, rr := newPlanServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)

	// Build a plan where generated_by is a bare string (coercible schema error).
	m := planfixture.Valid()
	m["generated_by"] = "my-agent"
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (coercion should succeed):\n%s", w.Code, w.Body.String())
	}

	// Coerced plan should still produce an artifact row.
	if len(ar.all) != 1 {
		t.Errorf("artifacts = %d, want 1", len(ar.all))
	}

	// Two audit entries: plan_coerced then plan_generated.
	if len(au.appended) != 2 {
		t.Fatalf("audit entries = %d, want 2 (plan_coerced + plan_generated)", len(au.appended))
	}
	if got := au.appended[0].Category; got != "plan_coerced" {
		t.Errorf("audit[0].category = %q, want plan_coerced", got)
	}
	if got := au.appended[1].Category; got != "plan_generated" {
		t.Errorf("audit[1].category = %q, want plan_generated", got)
	}

	// Coerced plans must NOT transition to failed-B. (#603: the success
	// path now drives the plan stage's terminal advance, so a transition
	// to a terminal *success* state is expected — only failed is wrong.)
	for _, call := range rr.transitionStageCalls {
		if call.StageID == stageID && call.To == run.StageStateFailed {
			t.Errorf("coerced plan should not transition the stage to failed; got %+v", call)
		}
	}
}

func TestShipPlan_NonCoercibleSchemaError_400(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, _, rr := newPlanServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)

	// Build a plan where scope.files[0] is an integer — not coercible.
	m := planfixture.Valid()
	scope := m["scope"].(map[string]any)
	scope["files"] = []any{42}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0 on non-coercible schema fail", len(ar.all))
	}

	// Non-coercible failures must still transition to failed-B.
	if len(rr.transitionStageCalls) != 1 {
		t.Fatalf("transitionStage calls = %d, want 1", len(rr.transitionStageCalls))
	}
	call := rr.transitionStageCalls[0]
	if call.Completion == nil || call.Completion.FailureCategory == nil ||
		*call.Completion.FailureCategory != run.FailureB {
		t.Errorf("FailureCategory = %v, want B", call.Completion.FailureCategory)
	}
}

func TestShipPlan_SchemaInvalid_400(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, _, rr := newPlanServer(t, runID, stageID)
	// #603: the trace handler no longer advances a plan stage past
	// running until a plan artifact exists, so at plan-ship time the
	// stage is in running. FailStage walks running → failed.
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID, State: run.StageStateRunning}
	priv, _ := sf.issue(t, runID)
	body := []byte(`{"plan_version":"standard_v1"}`) // missing required fields

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "plan_invalid") {
		t.Errorf("body missing plan_invalid code:\n%s", w.Body.String())
	}
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0 on schema fail", len(ar.all))
	}

	// #527 / #603: when the plan body fails standard_v1 validation, the
	// plan handler fails the stage as category-B (running → failed via
	// run.FailStage) so the run reflects the bad output rather than
	// stranding at awaiting_approval with no valid plan. The orchestrator
	// advance that walks the run to terminal failed is asserted in
	// TestPlanStage_TraceThenInvalidPlan_EndsFailed (this server wires no
	// orchestrator, so advanceAfterFailure is a no-op here).
	if len(rr.transitionStageCalls) != 1 {
		t.Fatalf("transitionStage calls = %d, want 1:\n%+v",
			len(rr.transitionStageCalls), rr.transitionStageCalls)
	}
	call := rr.transitionStageCalls[0]
	if call.StageID != stageID {
		t.Errorf("transitioned stage = %s, want %s", call.StageID, stageID)
	}
	if call.To != run.StageStateFailed {
		t.Errorf("transition.To = %q, want failed", call.To)
	}
	if call.Completion == nil {
		t.Fatal("transition.Completion is nil; failed transitions require StageCompletion")
	}
	if call.Completion.FailureCategory == nil || *call.Completion.FailureCategory != run.FailureB {
		t.Errorf("FailureCategory = %v, want B", call.Completion.FailureCategory)
	}
	if call.Completion.FailureReason == nil || !strings.HasPrefix(*call.Completion.FailureReason, "plan_invalid:") {
		t.Errorf("FailureReason = %v, want prefix 'plan_invalid:'", call.Completion.FailureReason)
	}
}

func TestShipPlan_SignatureMissing_401(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, _, _, _ := newPlanServer(t, runID, stageID)
	body := validPlanBytes(t)

	url := fmt.Sprintf("/v0/runs/%s/plan?stage_id=%s", runID, stageID)
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestShipPlan_SignatureInvalid_401(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, _, _ := newPlanServer(t, runID, stageID)
	sf.issue(t, runID) // server has the key, we sign with a different one
	body := validPlanBytes(t)

	// Sign with a wrong key.
	_, otherPriv, _ := ed25519.GenerateKey(nil)
	w := shipPlanRequest(t, s, runID, stageID, otherPriv, body, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401:\n%s", w.Code, w.Body.String())
	}
}

func TestShipPlan_StageMismatch_400(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, _, rr := newPlanServer(t, runID, stageID)

	// Re-seed the stage so it points at a *different* run.
	otherRun := uuid.New()
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: otherRun}
	priv, _ := sf.issue(t, runID)

	w := shipPlanRequest(t, s, runID, stageID, priv, validPlanBytes(t), "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (stage doesn't belong to run)", w.Code)
	}
}

func TestShipPlan_StageNotFound_404(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, _, rr := newPlanServer(t, runID, stageID)
	delete(rr.getStages, stageID)
	priv, _ := sf.issue(t, runID)

	w := shipPlanRequest(t, s, runID, stageID, priv, validPlanBytes(t), "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404:\n%s", w.Code, w.Body.String())
	}
}

func TestShipPlan_BodyTooLarge_413(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, _, _ := newPlanServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)

	// 257KB body — exceeds the 256KB cap, can't be valid JSON of
	// course but we expect the size check to fail before the
	// signature is verified anyway.
	body := bytes.Repeat([]byte("x"), maxPlanBundleBytes+1)
	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
}

func TestShipPlan_Unconfigured_503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	url := fmt.Sprintf("/v0/runs/%s/plan?stage_id=%s", uuid.New(), uuid.New())
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// --- Plan-review integration tests (ADR-027 Sub-plan D) ---

// TestShipPlan_ReviewAgents_Gating_NilReviewer_SkippedAudit confirms
// that when the spec requests agent-gated review (agent>0, human==0)
// but PlanReviewer is nil, the plan-upload path writes a
// plan_review_skipped audit entry (rather than silently skipping) and
// the upload still returns 201. The hard create-time block for gating
// mode lives at handleCreateRun (covered separately); the upload path
// only audits the degradation (#574).
func TestShipPlan_ReviewAgents_Gating_NilReviewer_SkippedAudit(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	// PlanReviewer is nil but spec requests gating agent review.
	s, sf, _, au, rr := newPlanServerWithReviewer(t, runID, stageID, nil, specGatingReviewers)
	priv, _ := sf.issue(t, runID)
	body := validPlanBytes(t)

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	// No plan_reviewed entry (no reviewer ran) but a plan_review_skipped
	// entry recording the gating degradation.
	var skipped []planreview.ReviewSkippedPayload
	for _, e := range au.appended {
		if e.Category == "plan_reviewed" {
			t.Errorf("unexpected plan_reviewed audit entry when PlanReviewer is nil")
		}
		if e.Category == "plan_review_started" {
			t.Errorf("unexpected plan_review_started audit entry when PlanReviewer is nil (#600: skipped branch must not emit the started proxy)")
		}
		if e.Category != "plan_review_skipped" {
			continue
		}
		var p planreview.ReviewSkippedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode plan_review_skipped payload: %v", err)
		}
		skipped = append(skipped, p)
	}
	if len(skipped) != 1 {
		t.Fatalf("plan_review_skipped entries = %d, want 1", len(skipped))
	}
	got := skipped[0]
	if got.Reason != "reviewer_not_configured" {
		t.Errorf("reason = %q, want reviewer_not_configured", got.Reason)
	}
	if got.ConfiguredAgents != 1 {
		t.Errorf("configured_agents = %d, want 1", got.ConfiguredAgents)
	}
	if got.Authority != planreview.AuthorityGating {
		t.Errorf("authority = %q, want gating", got.Authority)
	}

	// Skip does not fail the stage — no failed-B transition.
	for _, call := range rr.transitionStageCalls {
		if call.StageID == stageID && call.To == run.StageStateFailed {
			t.Errorf("nil-reviewer skip should not transition the stage to failed-B")
		}
	}
}

// TestShipPlan_ReviewAgents_Advisory_NilReviewer_SkippedAudit confirms
// that in advisory mode (agent>0, human>0) with PlanReviewer nil, a
// plan_review_skipped entry is written and the stage is not failed —
// the human gate remains authoritative (#574).
func TestShipPlan_ReviewAgents_Advisory_NilReviewer_SkippedAudit(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, au, rr := newPlanServerWithReviewer(t, runID, stageID, nil, specAdvisoryReviewers)
	priv, _ := sf.issue(t, runID)
	body := validPlanBytes(t)

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	var skipped []planreview.ReviewSkippedPayload
	for _, e := range au.appended {
		if e.Category != "plan_review_skipped" {
			continue
		}
		var p planreview.ReviewSkippedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode plan_review_skipped payload: %v", err)
		}
		skipped = append(skipped, p)
	}
	if len(skipped) != 1 {
		t.Fatalf("plan_review_skipped entries = %d, want 1", len(skipped))
	}
	if skipped[0].Authority != planreview.AuthorityAdvisory {
		t.Errorf("authority = %q, want advisory", skipped[0].Authority)
	}
	if skipped[0].Reason != "reviewer_not_configured" {
		t.Errorf("reason = %q, want reviewer_not_configured", skipped[0].Reason)
	}

	// Advisory skip: stage must NOT be failed; the human gate decides.
	for _, call := range rr.transitionStageCalls {
		if call.StageID == stageID && call.To == run.StageStateFailed {
			t.Errorf("advisory nil-reviewer skip should not transition the stage to failed-B")
		}
	}
}

// TestShipPlan_ReviewAgents_Gateless_NoReviewersInSpec confirms that
// when the spec has no reviewers (agent==0), no plan_reviewed entries
// are written even when PlanReviewer is configured.
func TestShipPlan_ReviewAgents_Gateless_NoReviewersInSpec(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-sonnet-4-6",
	}
	s, sf, _, au, _ := newPlanServerWithReviewer(t, runID, stageID, reviewer, specNoReviewers)
	priv, _ := sf.issue(t, runID)
	body := validPlanBytes(t)

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	// Reviewer must not have been called.
	reviewer.mu.Lock()
	callCount := len(reviewer.calls)
	reviewer.mu.Unlock()
	if callCount != 0 {
		t.Errorf("reviewer called %d times, want 0 (no reviewers in spec)", callCount)
	}
	for _, e := range au.appended {
		if e.Category == "plan_reviewed" {
			t.Errorf("unexpected plan_reviewed audit entry when spec has agent=0")
		}
		if e.Category == "plan_review_started" {
			t.Errorf("unexpected plan_review_started audit entry when spec has agent=0 (#600: none branch must not emit the started proxy)")
		}
	}
}

// TestShipPlan_ReviewAgents_Advisory_RecordsVerdictDoesNotBlock verifies
// that in advisory mode (agent>0 && human>0) a reject verdict is recorded
// in the audit log but does NOT transition the stage to failed-B.
func TestShipPlan_ReviewAgents_Advisory_RecordsVerdictDoesNotBlock(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{
			Verdict: planreview.VerdictReject,
			Concerns: []planreview.Concern{
				{Severity: planreview.SeverityHigh, Category: "scope", Note: "missing files"},
			},
		},
		// Different model from planfixture.Valid() "claude-opus-4-7" so
		// self-review WARN doesn't fire in this advisory-mode test.
		model: "claude-sonnet-4-6",
	}
	s, sf, _, au, rr := newPlanServerWithReviewer(t, runID, stageID, reviewer, specAdvisoryReviewers)
	priv, _ := sf.issue(t, runID)
	body := validPlanBytes(t)

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	// Advisory review runs detached (#584); drain it before asserting on
	// the audit entry it writes.
	s.waitBackgroundReviews()

	// plan_reviewed audit entry must exist.
	var reviewedEntries []planreview.PlanReviewedPayload
	for _, e := range au.appended {
		if e.Category != "plan_reviewed" {
			continue
		}
		var p planreview.PlanReviewedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode plan_reviewed payload: %v", err)
		}
		reviewedEntries = append(reviewedEntries, p)
	}
	if len(reviewedEntries) != 1 {
		t.Fatalf("plan_reviewed entries = %d, want 1", len(reviewedEntries))
	}
	got := reviewedEntries[0]
	if got.Verdict != planreview.VerdictReject {
		t.Errorf("verdict = %q, want reject", got.Verdict)
	}
	if got.ReviewerKind != "agent" {
		t.Errorf("reviewer_kind = %q, want agent", got.ReviewerKind)
	}
	if got.Authority != planreview.AuthorityAdvisory {
		t.Errorf("authority = %q, want advisory", got.Authority)
	}

	// Advisory mode: stage must NOT be transitioned to failed-B.
	for _, call := range rr.transitionStageCalls {
		if call.StageID == stageID && call.To == run.StageStateFailed {
			t.Errorf("advisory: stage should not be transitioned to failed-B on reject")
		}
	}
}

// TestShipPlan_ReviewAgents_GatingReject_StageFailedB verifies that in
// gating mode (agent>0 && human==0) a reject verdict transitions the
// stage to failed-B so trace-driven awaiting_approval is blocked.
func TestShipPlan_ReviewAgents_GatingReject_StageFailedB(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{
			Verdict:  planreview.VerdictReject,
			FreeForm: "plan is incomplete",
		},
		// Different model from planfixture.Valid() "claude-opus-4-7".
		model: "claude-sonnet-4-6",
	}
	s, sf, _, au, rr := newPlanServerWithReviewer(t, runID, stageID, reviewer, specGatingReviewers)
	priv, _ := sf.issue(t, runID)
	body := validPlanBytes(t)

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	// Upload still returns 201 — the plan artifact is stored.
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	// plan_reviewed audit entry with gating authority.
	var sawReviewed bool
	for _, e := range au.appended {
		if e.Category != "plan_reviewed" {
			continue
		}
		sawReviewed = true
		var p planreview.PlanReviewedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode plan_reviewed payload: %v", err)
		}
		if p.Authority != planreview.AuthorityGating {
			t.Errorf("authority = %q, want gating", p.Authority)
		}
		if p.Verdict != planreview.VerdictReject {
			t.Errorf("verdict = %q, want reject", p.Verdict)
		}
	}
	if !sawReviewed {
		t.Error("no plan_reviewed audit entry found")
	}

	// Gating reject: stage must be transitioned to failed-B.
	var found bool
	for _, call := range rr.transitionStageCalls {
		if call.StageID != stageID || call.To != run.StageStateFailed {
			continue
		}
		found = true
		if call.Completion == nil || call.Completion.FailureCategory == nil ||
			*call.Completion.FailureCategory != run.FailureB {
			t.Errorf("FailureCategory = %v, want B", call.Completion.FailureCategory)
		}
		if call.Completion.FailureReason == nil ||
			!strings.Contains(*call.Completion.FailureReason, "plan_review_rejected") {
			t.Errorf("FailureReason = %v, want plan_review_rejected prefix", call.Completion.FailureReason)
		}
	}
	if !found {
		t.Error("gating reject: stage was not transitioned to failed-B")
	}
}

// TestShipPlan_ReviewAgents_GatingApprove_StageNotFailed verifies that
// in gating mode an approve verdict does NOT fail the stage.
func TestShipPlan_ReviewAgents_GatingApprove_StageNotFailed(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		// Different model from planfixture.Valid() "claude-opus-4-7".
		model: "claude-sonnet-4-6",
	}
	s, sf, _, au, rr := newPlanServerWithReviewer(t, runID, stageID, reviewer, specGatingReviewers)
	priv, _ := sf.issue(t, runID)
	body := validPlanBytes(t)

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	// plan_reviewed audit entry exists.
	var sawReviewed bool
	for _, e := range au.appended {
		if e.Category == "plan_reviewed" {
			sawReviewed = true
		}
	}
	if !sawReviewed {
		t.Error("no plan_reviewed audit entry found on approve")
	}

	// No failed-B transition.
	for _, call := range rr.transitionStageCalls {
		if call.StageID == stageID && call.To == run.StageStateFailed {
			t.Errorf("gating approve: stage should NOT be transitioned to failed-B")
		}
	}
}

// TestShipPlan_ReviewAgents_SelfReviewLogsWarn verifies that when the
// reviewer model matches the plan author model, the server logs a WARN
// but still records the verdict (no block).
func TestShipPlan_ReviewAgents_SelfReviewLogsWarn(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	// Model matches planfixture.Valid() generated_by.model ("claude-opus-4-7").
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7", // same as planfixture.Valid generated_by.model
	}
	s, sf, _, au, _ := newPlanServerWithReviewer(t, runID, stageID, reviewer, specGatingReviewers)
	priv, _ := sf.issue(t, runID)
	body := validPlanBytes(t)

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (self-review is warn-only):\n%s", w.Code, w.Body.String())
	}

	// Verdict is still recorded despite self-review.
	var sawReviewed bool
	for _, e := range au.appended {
		if e.Category == "plan_reviewed" {
			sawReviewed = true
		}
	}
	if !sawReviewed {
		t.Error("plan_reviewed entry missing — self-review guard should warn but not skip")
	}
}

// TestShipPlan_ReviewStarted_PrecedesReviewed asserts the #600 ordering
// invariant: the plan_review_started audit entry is appended BEFORE the
// terminal plan_reviewed entry under both gating (synchronous) and advisory
// (detached goroutine) authority. The MCP review_status proxy depends on
// this — started is the 'pending' signal and reviewed is the terminal one;
// a started that landed after reviewed would let a consumer read 'pending'
// on an already-complete review. The test fails if the emit is misordered
// or skipped on either path.
func TestShipPlan_ReviewStarted_PrecedesReviewed(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec []byte
	}{
		{"gating", specGatingReviewers},
		{"advisory", specAdvisoryReviewers},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runID, stageID := uuid.New(), uuid.New()
			reviewer := &fakePlanReviewer{
				verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
				model:   "claude-sonnet-4-6",
			}
			s, sf, _, au, _ := newPlanServerWithReviewer(t, runID, stageID, reviewer, tc.spec)
			priv, _ := sf.issue(t, runID)
			body := validPlanBytes(t)

			w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
			}
			// Advisory dispatches the reviewer detached (#584); drain it so
			// the plan_reviewed entry it writes has landed before asserting.
			s.waitBackgroundReviews()

			au.mu.Lock()
			defer au.mu.Unlock()
			startedIdx, reviewedIdx := -1, -1
			for i, e := range au.appended {
				switch e.Category {
				case "plan_review_started":
					if startedIdx == -1 {
						startedIdx = i
					}
				case "plan_reviewed":
					if reviewedIdx == -1 {
						reviewedIdx = i
					}
				}
			}
			if startedIdx == -1 {
				t.Fatal("no plan_review_started audit entry emitted")
			}
			if reviewedIdx == -1 {
				t.Fatal("no plan_reviewed audit entry emitted")
			}
			if startedIdx >= reviewedIdx {
				t.Errorf("plan_review_started index %d must precede plan_reviewed index %d", startedIdx, reviewedIdx)
			}
			var p planreview.ReviewStartedPayload
			if err := json.Unmarshal(au.appended[startedIdx].Payload, &p); err != nil {
				t.Fatalf("decode plan_review_started payload: %v", err)
			}
			if p.ConfiguredAgents != 1 {
				t.Errorf("configured_agents = %d, want 1", p.ConfiguredAgents)
			}
		})
	}
}

// countAuditCategory returns how many appended audit entries match cat.
// Holds the auditFake mutex so it is safe to call while a detached
// review goroutine may still be appending.
func countAuditCategory(au *auditFake, cat string) int {
	au.mu.Lock()
	defer au.mu.Unlock()
	n := 0
	for _, e := range au.appended {
		if e.Category == cat {
			n++
		}
	}
	return n
}

// TestShipPlan_ReviewAgents_Advisory_RunsAsync asserts the #584 decoupling
// for the plan path: under advisory authority the upload handler returns
// 201 BEFORE the (blocked) reviewer finishes, then once the reviewer is
// released and the background goroutine drained, the plan_reviewed audit
// entry lands. The ordering is asserted deterministically via the
// blocking-reviewer channel + waitBackgroundReviews(), with no sleeps.
func TestShipPlan_ReviewAgents_Advisory_RunsAsync(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	reviewer := newBlockingPlanReviewer(
		&planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		"claude-sonnet-4-6",
	)
	s, sf, _, au, _ := newPlanServerWithReviewer(t, runID, stageID, reviewer, specAdvisoryReviewers)
	priv, _ := sf.issue(t, runID)
	body := validPlanBytes(t)

	// Handler returns while the reviewer is still blocked on release.
	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	// The async review cannot have completed yet: release is not closed,
	// so no plan_reviewed entry can exist. (plan_generated is synchronous.)
	if n := countAuditCategory(au, "plan_reviewed"); n != 0 {
		t.Fatalf("plan_reviewed entries = %d before release, want 0 (review was not async)", n)
	}

	// Release the reviewer and drain the detached goroutine.
	close(reviewer.release)
	s.waitBackgroundReviews()

	if n := countAuditCategory(au, "plan_reviewed"); n != 1 {
		t.Errorf("plan_reviewed entries = %d after release, want 1", n)
	}
}

// TestShipPlan_ReviewAgents_Advisory_ContextDetached asserts the detached
// review survives cancellation of the context passed into runPlanReviews
// (simulating the runner's upload client disconnecting and cancelling
// r.Context() mid-review). context.WithoutCancel is the load-bearing
// mechanism: the parent cancel must NOT propagate to the reviewer, so the
// verdict still records. The blocking reviewer returns ctx.Err() on a
// cancelled context, so this test fails if the goroutine were handed the
// raw request context.
func TestShipPlan_ReviewAgents_Advisory_ContextDetached(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	reviewer := newBlockingPlanReviewer(
		&planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		"claude-sonnet-4-6",
	)
	s, _, _, au, _ := newPlanServerWithReviewer(t, runID, stageID, reviewer, specAdvisoryReviewers)
	body := validPlanBytes(t)

	ctx, cancel := context.WithCancel(context.Background())
	// Advisory → dispatches a detached goroutine and returns false.
	if s.runPlanReviews(ctx, runID, stageID, body) {
		t.Fatal("advisory runPlanReviews returned true (advisory must never gate)")
	}

	// Wait until the reviewer goroutine is in-flight, then cancel the
	// parent context mid-review and let the reviewer proceed.
	<-reviewer.started
	cancel()
	close(reviewer.release)

	s.waitBackgroundReviews()

	if n := countAuditCategory(au, "plan_reviewed"); n != 1 {
		t.Errorf("plan_reviewed entries = %d, want 1 — detached review must survive parent-context cancel (context.WithoutCancel)", n)
	}
}

// TestShipPlan_ReviewAgents_GatingReject_FromAwaitingApproval asserts the
// state-machine edge the synchronous-gating guarantee relies on: when the
// trace handler has already advanced the plan stage to awaiting_approval
// (trace ships before plan), a later gating reject on the plan upload
// still transitions the stage to failed-B. The edge legality is asserted
// explicitly via run.ValidStageTransition rather than assumed.
func TestShipPlan_ReviewAgents_GatingReject_FromAwaitingApproval(t *testing.T) {
	// Explicit edge-legality guard: if awaiting_approval → failed is ever
	// removed from the transition table, the synchronous-gating design
	// breaks and this assertion catches it before the handler test below.
	if !run.ValidStageTransition(run.StageStateAwaitingApproval, run.StageStateFailed) {
		t.Fatal("awaiting_approval → failed is not a legal transition; synchronous gating reject can no longer fail the stage")
	}

	runID, stageID := uuid.New(), uuid.New()
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictReject},
		model:   "claude-sonnet-4-6",
	}
	s, sf, _, _, rr := newPlanServerWithReviewer(t, runID, stageID, reviewer, specGatingReviewers)
	// Seed the stage already in awaiting_approval — the state the trace
	// handler leaves it in before the plan upload arrives.
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID, State: run.StageStateAwaitingApproval}
	priv, _ := sf.issue(t, runID)
	body := validPlanBytes(t)

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	var found bool
	for _, call := range rr.transitionStageCalls {
		if call.StageID != stageID || call.To != run.StageStateFailed {
			continue
		}
		found = true
		if call.Completion == nil || call.Completion.FailureCategory == nil ||
			*call.Completion.FailureCategory != run.FailureB {
			t.Errorf("FailureCategory = %v, want B", call.Completion.FailureCategory)
		}
	}
	if !found {
		t.Error("gating reject from awaiting_approval did not transition the stage to failed-B")
	}
}
