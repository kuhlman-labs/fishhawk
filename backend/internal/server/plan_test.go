package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
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

// storingAuditFake extends auditFake so ListForRunByCategory reads back
// the entries AppendChained stored. The plain auditFake errors on
// ListForRunByCategory; the #646 schema-retry budget counter
// (countSchemaRetries) and the cross-boundary seam test
// (writer→audit→reader→render) both need a fake that actually persists
// and returns entries by category.
type storingAuditFake struct {
	*auditFake
}

func newStoringAuditFake() *storingAuditFake {
	return &storingAuditFake{auditFake: newAuditFake()}
}

// ListForRunByCategory returns the stored AppendChained entries for the
// run+category in insertion order (≈ ts ASC), matching the production
// ordering loadPriorSchemaValidationError's newest-first scan assumes.
func (a *storingAuditFake) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*audit.Entry
	for _, p := range a.appended {
		if p.RunID == runID && p.Category == category {
			rid := p.RunID
			out = append(out, &audit.Entry{
				RunID:    &rid,
				StageID:  p.StageID,
				Category: p.Category,
				Payload:  p.Payload,
			})
		}
	}
	return out, nil
}

// newPlanSequenceServer wires the full plan-stage path — signing, trace
// store, audit, artifacts, a transition-recording run repo, and a real
// orchestrator — so a test can drive the runner's true trace-then-plan
// upload order and observe the resulting stage + run states. (#603)
//
// The audit fake is the storing variant so the #646 schema-retry budget
// counter (which reads plan_schema_retry entries back) behaves as in
// production rather than always seeing a zero count.
func newPlanSequenceServer(t *testing.T) (*Server, *recordingOrchestratorRepo, *fakeArtifactRepo, *signingFake, *storingAuditFake) {
	t.Helper()
	rr := &recordingOrchestratorRepo{orchestratorRepo: newOrchestratorRepo()}
	art := newFakeArtifactRepo()
	sf := newSigningFake()
	au := newStoringAuditFake()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  sf,
		TraceStore:   newTraceStoreFake(),
		AuditRepo:    au,
		RunRepo:      rr,
		ArtifactRepo: art,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})
	return s, rr, art, sf, au
}

// seedSchemaRetryEntry appends a plan_schema_retry audit entry directly,
// pre-loading the #646 budget so a test can exercise the
// budget-exhausted (fail-B) branch deterministically.
func seedSchemaRetryEntry(t *testing.T, au *storingAuditFake, runID, stageID uuid.UUID, validationErr string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"run_id":           runID.String(),
		"stage_id":         stageID.String(),
		"attempt":          1,
		"validation_error": validationErr,
	})
	kind := audit.ActorKind("system")
	if _, err := au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Category:  "plan_schema_retry",
		ActorKind: &kind,
		Payload:   payload,
	}); err != nil {
		t.Fatalf("seed plan_schema_retry: %v", err)
	}
}

// TestPlanStage_TraceThenInvalidPlan_EndsFailed reproduces the #603
// sequence: the runner ships the trace (which no longer advances a plan
// stage past running) and then an invalid plan. The stage must end in
// failed — never awaiting_approval — and the run must be advanced to
// terminal failed rather than stranded.
func TestPlanStage_TraceThenInvalidPlan_EndsFailed(t *testing.T) {
	s, rr, _, sf, au := newPlanSequenceServer(t)
	runRow := rr.seedRun()
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateDispatched)
	planStage.RequiresApproval = true // gated plan stage — the #603 shape
	priv, _ := sf.issue(t, runRow.ID)

	// Exhaust the #646 in-run schema-retry budget up front so this upload
	// takes the terminal fail-B path the #603 invariant guards (an invalid
	// plan never reaches the gate and never strands the run). The
	// first-failure re-dispatch behavior is covered by
	// TestShipPlan_SchemaRetry_FirstFailure_ReopensStage.
	seedSchemaRetryEntry(t, au, runRow.ID, planStage.ID, "prior validation error")

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
	s, rr, _, sf, _ := newPlanSequenceServer(t)
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

// invalidPlanBytes is a standard_v1 plan that fails validation and is
// NOT coercible (missing required fields, not a string-elision slip), so
// it reaches the #646 schema-retry backstop rather than being auto-fixed
// by plan.TryCoerce.
func invalidPlanBytes() []byte { return []byte(`{"plan_version":"standard_v1"}`) }

// TestShipPlan_SchemaRetry_FirstFailure_ReopensStage covers the #646
// happy path: with the orchestrator + audit wired and budget available,
// the first post-coercion invalid plan re-opens the plan stage (does NOT
// fail-B), records a plan_schema_retry audit entry, and signals
// retry_scheduled in the 400 response. The transient FailureA must not
// leak — the stage lands re-driveable (pending/dispatched, no failure
// metadata) and the run stays non-terminal.
func TestShipPlan_SchemaRetry_FirstFailure_ReopensStage(t *testing.T) {
	s, rr, _, sf, au := newPlanSequenceServer(t)
	runRow := rr.seedRun()
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateDispatched)
	planStage.RequiresApproval = true
	priv, _ := sf.issue(t, runRow.ID)

	wp := shipPlanRequest(t, s, runRow.ID, planStage.ID, priv, invalidPlanBytes(), "")
	if wp.Code != http.StatusBadRequest {
		t.Fatalf("plan status = %d, want 400:\n%s", wp.Code, wp.Body.String())
	}

	// retry_scheduled must be set so the local operator/driver knows a
	// re-attempt was set up rather than a terminal fail.
	var resp struct {
		Error struct {
			Details struct {
				RetryScheduled bool `json:"retry_scheduled"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(wp.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v\n%s", err, wp.Body.String())
	}
	if !resp.Error.Details.RetryScheduled {
		t.Errorf("response missing retry_scheduled=true:\n%s", wp.Body.String())
	}

	// The stage must NOT be terminally failed — it was re-opened and the
	// orchestrator (local path, no GitHub) walked it pending → dispatched.
	got := rr.stagesByID[planStage.ID]
	if got.State == run.StageStateFailed {
		t.Errorf("stage state = failed; #646 requires re-open on first failure")
	}
	if got.State != run.StageStateDispatched {
		t.Errorf("stage state = %q, want dispatched (orchestrator re-dispatch on local path)", got.State)
	}
	// The transient FailureA must have been cleared by RetryStage.
	if got.FailureCategory != nil {
		t.Errorf("stage still carries failure category %q; RetryStage must clear it", *got.FailureCategory)
	}
	// Never reached the gate; run not stranded/terminal.
	if rr.sawTransitionTo(run.StageStateAwaitingApproval) {
		t.Errorf("stage transitioned to awaiting_approval on an invalid plan")
	}
	if st := rr.runs[runRow.ID].State; st == run.StateFailed {
		t.Errorf("run state = failed; a scheduled retry must not fail the run")
	}

	// Exactly one plan_schema_retry audit entry was chained.
	entries, _ := au.ListForRunByCategory(context.Background(), runRow.ID, "plan_schema_retry")
	if len(entries) != 1 {
		t.Fatalf("plan_schema_retry entries = %d, want 1", len(entries))
	}
}

// TestShipPlan_SchemaRetry_BudgetExhausted_FailsB confirms that a second
// invalid upload (one plan_schema_retry already recorded) exhausts the
// budget and falls through to the unchanged fail-B + advance path (#646).
func TestShipPlan_SchemaRetry_BudgetExhausted_FailsB(t *testing.T) {
	s, rr, _, sf, au := newPlanSequenceServer(t)
	runRow := rr.seedRun()
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateDispatched)
	planStage.RequiresApproval = true
	priv, _ := sf.issue(t, runRow.ID)

	// Budget already spent by a prior attempt.
	seedSchemaRetryEntry(t, au, runRow.ID, planStage.ID, "prior validation error")

	wp := shipPlanRequest(t, s, runRow.ID, planStage.ID, priv, invalidPlanBytes(), "")
	if wp.Code != http.StatusBadRequest {
		t.Fatalf("plan status = %d, want 400:\n%s", wp.Code, wp.Body.String())
	}

	// No new retry scheduled — fail-B path.
	var resp struct {
		Error struct {
			Details struct {
				RetryScheduled bool `json:"retry_scheduled"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(wp.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error.Details.RetryScheduled {
		t.Errorf("retry_scheduled set on a budget-exhausted upload; want fail-B")
	}

	if got := rr.stagesByID[planStage.ID].State; got != run.StageStateFailed {
		t.Errorf("stage state = %q, want failed (budget exhausted → fail-B)", got)
	}
	if got := rr.runs[runRow.ID].State; got != run.StateFailed {
		t.Errorf("run state = %q, want failed (fail-B advances the run)", got)
	}
	// No second plan_schema_retry entry was written.
	entries, _ := au.ListForRunByCategory(context.Background(), runRow.ID, "plan_schema_retry")
	if len(entries) != 1 {
		t.Errorf("plan_schema_retry entries = %d, want 1 (no new entry on exhausted budget)", len(entries))
	}
}

// TestShipPlan_SchemaRetry_NilOrchestrator_FailsB confirms the
// no-regression fallback: without an orchestrator to re-dispatch, an
// invalid plan fails-B exactly as before #646.
func TestShipPlan_SchemaRetry_NilOrchestrator_FailsB(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	// newPlanServer wires signing+artifact+audit+run but NO orchestrator.
	s, sf, _, _, rr := newPlanServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)

	wp := shipPlanRequest(t, s, runID, stageID, priv, invalidPlanBytes(), "")
	if wp.Code != http.StatusBadRequest {
		t.Fatalf("plan status = %d, want 400:\n%s", wp.Code, wp.Body.String())
	}
	var resp struct {
		Error struct {
			Details struct {
				RetryScheduled bool `json:"retry_scheduled"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(wp.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error.Details.RetryScheduled {
		t.Errorf("retry_scheduled set with nil orchestrator; want fail-B fallback")
	}
	if got := rr.getStages[stageID].State; got != run.StageStateFailed {
		t.Errorf("stage state = %q, want failed (nil orchestrator → fail-B)", got)
	}
}

// TestShipPlan_SchemaRetry_SeamToPromptRender is the cross-boundary seam
// test (#646, cf. #618): it drives handleShipPlan with an invalid plan
// (the writer records validation_error to a plan_schema_retry entry),
// then calls handleGetStagePromptRender on the re-opened plan stage (the
// reader pulls it back) and asserts the rendered prompt carries the
// recorded validation error. This guards the validation_error payload-key
// contract between plan.go and prompt.go end-to-end — per-layer units pass
// while the seam could silently break.
func TestShipPlan_SchemaRetry_SeamToPromptRender(t *testing.T) {
	s, rr, _, sf, au := newPlanSequenceServer(t)
	s.promptIssueGetterOverride = &stubIssueGetter{}
	runRow := rr.seedRun()
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateDispatched)
	planStage.RequiresApproval = true
	priv, _ := sf.issue(t, runRow.ID)

	// Writer: invalid plan → schedules a retry, recording validation_error.
	wp := shipPlanRequest(t, s, runRow.ID, planStage.ID, priv, invalidPlanBytes(), "")
	if wp.Code != http.StatusBadRequest {
		t.Fatalf("plan status = %d, want 400:\n%s", wp.Code, wp.Body.String())
	}

	// Pull the exact validation_error that was persisted, so the assertion
	// pins the writer↔reader payload-key contract rather than a fixed string.
	entries, _ := au.ListForRunByCategory(context.Background(), runRow.ID, "plan_schema_retry")
	if len(entries) != 1 {
		t.Fatalf("plan_schema_retry entries = %d, want 1", len(entries))
	}
	var stored struct {
		ValidationError string `json:"validation_error"`
	}
	if err := json.Unmarshal(entries[0].Payload, &stored); err != nil {
		t.Fatalf("unmarshal stored payload: %v", err)
	}
	if stored.ValidationError == "" {
		t.Fatal("stored validation_error is empty")
	}

	// Reader + render: fetch the re-opened plan stage's prompt.
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/stages/%s/prompt-render", planStage.ID), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("prompt-render status = %d:\n%s", w.Code, w.Body.String())
	}
	var pr promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pr); err != nil {
		t.Fatalf("decode prompt response: %v", err)
	}
	if !strings.Contains(pr.Prompt, "### Prior plan-stage schema validation failure") {
		t.Errorf("rendered prompt missing schema-validation section:\n%s", pr.Prompt)
	}
	if !strings.Contains(pr.Prompt, stored.ValidationError) {
		t.Errorf("rendered prompt missing the recorded validation_error %q:\n%s", stored.ValidationError, pr.Prompt)
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

// singleReviewerSet wraps one PlanReviewer as a ReviewerSet for tests:
// Default() returns the wrapped reviewer (possibly nil) and For resolves
// every provider to it — the count-form equivalent of the pre-#955
// Config.PlanReviewer field.
type singleReviewerSet struct{ reviewer PlanReviewer }

func (s singleReviewerSet) Default() PlanReviewer { return s.reviewer }

func (s singleReviewerSet) For(provider, _ string) (PlanReviewer, error) {
	if s.reviewer == nil {
		return nil, fmt.Errorf("reviewer provider %q is not configured", provider)
	}
	return s.reviewer, nil
}

// fakeReviewerSet maps provider names to fake adapters for heterogeneous
// reviewers.agents tests (#955). def is the Default() adapter (may be
// nil); For errors for providers absent from the map.
type fakeReviewerSet struct {
	def       PlanReviewer
	providers map[string]PlanReviewer
}

func (s fakeReviewerSet) Default() PlanReviewer { return s.def }

func (s fakeReviewerSet) For(provider, _ string) (PlanReviewer, error) {
	r, ok := s.providers[provider]
	if !ok {
		return nil, fmt.Errorf("reviewer provider %q is not configured", provider)
	}
	return r, nil
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

	// specGatingReviewersWithConstraints adds an implement stage with path
	// constraints to the gating-reviewers shape, so the plan-gate scope
	// pre-check produces a result the #963 gate-evidence threading test can
	// observe in the review prompt.
	specGatingReviewersWithConstraints = []byte(`version: "0.3"
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
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - max_files_changed: 3
          - forbidden_paths:
              - ".github/workflows/**"
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
		Addr:          "127.0.0.1:0",
		SigningRepo:   sf,
		ArtifactRepo:  ar,
		AuditRepo:     au,
		RunRepo:       rr,
		PlanReviewers: singleReviewerSet{reviewer},
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

// TestShipPlan_ReviewAgents_IssueCommentsReachReviewPrompt is the #622
// cross-boundary check: a comment cached on the run's IssueContext must
// flow through the server's plan-review trigger mapping into the rendered
// plan-review prompt. This crosses the persistence(IssueContext) -> server
// trigger mapping -> prompt render seam that the per-layer prompt-package
// tests cannot exercise (the bug fixed here was precisely a missing mapping
// at that seam, cf. #618).
func TestShipPlan_ReviewAgents_IssueCommentsReachReviewPrompt(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-sonnet-4-6",
	}
	s, sf, _, _, rr := newPlanServerWithReviewer(t, runID, stageID, reviewer, specAdvisoryReviewers)
	// Seed the cached issue context with a comment that refines the body.
	rr.getRuns[runID].IssueContext = &run.IssueContext{
		Title:  "Add a foo flag",
		Body:   "We need a --foo flag that defaults to off.",
		Number: 622,
		Comments: []run.IssueComment{
			{Author: "carol", Body: "Correction: --foo must default to ON.", CreatedAt: "2026-05-02T00:00:00Z"},
		},
	}
	priv, _ := sf.issue(t, runID)
	body := validPlanBytes(t)

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	// Advisory review runs detached (#584); drain it before inspecting the
	// captured prompt.
	s.waitBackgroundReviews()

	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if len(reviewer.calls) != 1 {
		t.Fatalf("reviewer calls = %d, want 1", len(reviewer.calls))
	}
	got := reviewer.calls[0]
	if !strings.Contains(got, "Correction: --foo must default to ON.") {
		t.Errorf("plan-review prompt missing the cached issue comment body — mapping seam broken:\n%s", got)
	}
	if !strings.Contains(got, "### Issue comments") {
		t.Errorf("plan-review prompt missing the issue-comments section:\n%s", got)
	}
}

// TestShipPlan_ReviewAgents_GateEvidenceReachesReviewPrompt is the #963
// seam check for the plan loop: the scope-precheck and surface-sweep
// results handleShipPlan computes synchronously must flow through
// runPlanReviews' trigger mapping into the rendered plan-review prompt.
// This crosses the gate-evaluation → server trigger mapping → prompt
// render seam the per-layer prompt-package tests cannot exercise.
func TestShipPlan_ReviewAgents_GateEvidenceReachesReviewPrompt(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-sonnet-4-6",
	}
	s, sf, _, _, _ := newPlanServerWithReviewer(t, runID, stageID, reviewer, specGatingReviewersWithConstraints)
	priv, _ := sf.issue(t, runID)
	// A scope that hits the implement stage's forbidden_paths constraint
	// AND a surface-sweep pattern (notifier.go without the surfaces doc).
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: ".github/workflows/ci.yml", Operation: plan.FileOpModify},
		{Path: "backend/internal/issuecomment/notifier.go", Operation: plan.FileOpModify},
	})

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	// Gating review (human: 0) runs synchronously, so the captured prompt
	// is available as soon as the upload returns.

	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if len(reviewer.calls) != 1 {
		t.Fatalf("reviewer calls = %d, want 1", len(reviewer.calls))
	}
	got := reviewer.calls[0]
	wants := []string{
		"### Gate evidence (machine-verified — outranks text-level findings)",
		// Scope pre-check result: the forbidden_paths hit and the cap.
		"VIOLATION forbidden_paths",
		".github/workflows/ci.yml",
		"- max_files_changed cap: 3",
		// Surface-sweep result: notifier.go without the surfaces doc.
		"MISSING SIBLINGS",
		"docs/issue-comment-surfaces.md",
	}
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("plan-review prompt missing gate-evidence element %q — threading seam broken:\n%s", want, got)
		}
	}
}

// TestShipPlan_ReviewAgents_BudgetEvidenceReachesReviewPrompt is the #994
// seam check: the resolved implement budget runPlanReviews computes via
// planBudgetEvidence must flow through the trigger mapping into the
// rendered plan-review prompt's Budget check block, citing the same
// number the approval gate enforces. The spec's implement stage declares
// no timeout and the workflow no policy, so the budget resolves to the
// 15m default with source "spec"; planfixture.Valid() predicts 20 → over.
func TestShipPlan_ReviewAgents_BudgetEvidenceReachesReviewPrompt(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-sonnet-4-6",
	}
	s, sf, _, _, _ := newPlanServerWithReviewer(t, runID, stageID, reviewer, specGatingReviewersWithConstraints)
	priv, _ := sf.issue(t, runID)
	body := validPlanBytes(t)

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	// Gating review (human: 0) runs synchronously, so the captured prompt
	// is available as soon as the upload returns.

	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if len(reviewer.calls) != 1 {
		t.Fatalf("reviewer calls = %d, want 1", len(reviewer.calls))
	}
	got := reviewer.calls[0]
	wants := []string{
		"Budget check (plan prediction vs the resolved implement-stage budget the approval gate enforces):",
		"- resolved implement budget: 15 minutes (source: spec)",
		"- plan predicted_runtime_minutes: 20",
		"- verdict: over budget",
	}
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("plan-review prompt missing budget-evidence element %q — threading seam broken:\n%s", want, got)
		}
	}
}

// TestShipPlan_ReviewAgents_BudgetEvidenceDecomposedReachesReviewPrompt
// is the #1029 seam check: a decomposed over-budget plan must reach the
// dispatched reviewer prompt with the gate-accurate "gate satisfied
// without override" verdict — including the sub-plan count and per-slice
// minutes, so the reviewer sees the decomposition in the authoritative
// evidence line rather than rejecting on a phantom refusal.
// planfixture.Decomposed() predicts 20 over the 15m spec-default budget
// with two 10-minute sub-plans.
func TestShipPlan_ReviewAgents_BudgetEvidenceDecomposedReachesReviewPrompt(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-sonnet-4-6",
	}
	s, sf, _, _, _ := newPlanServerWithReviewer(t, runID, stageID, reviewer, specGatingReviewersWithConstraints)
	priv, _ := sf.issue(t, runID)
	body, err := json.Marshal(planfixture.Decomposed())
	if err != nil {
		t.Fatal(err)
	}

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if len(reviewer.calls) != 1 {
		t.Fatalf("reviewer calls = %d, want 1", len(reviewer.calls))
	}
	got := reviewer.calls[0]
	wants := []string{
		"Budget check (plan prediction vs the resolved implement-stage budget the approval gate enforces):",
		"- resolved implement budget: 15 minutes (source: spec)",
		"- plan predicted_runtime_minutes: 20",
		"- verdict: over budget, decomposed into 2 sub-plans (10/10 min, max 10 <= budget 15) — gate satisfied without override",
	}
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("plan-review prompt missing decomposed budget-evidence element %q — threading seam broken:\n%s", want, got)
		}
	}
	if strings.Contains(got, "will be refused") {
		t.Errorf("dispatched prompt claims refusal for a decomposed plan — phantom refusal (#1029):\n%s", got)
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

// TestShipPlan_ReviewAgents_ReviewerError_EmitsPlanReviewFailed is the #664
// producer-contract test: a reviewer that errors (modelling a timeout — the
// adapter's context.WithTimeout cancellation surfaces as a Review error)
// must write exactly one terminal plan_review_failed audit entry carrying
// the error string as its reason, and zero plan_reviewed entries. It also
// pins that gating advance semantics are untouched (#574): an erroring
// gating reviewer does NOT transition the stage to failed-B (hasRejection
// stays false). The MCP review_test.go consumer test asserts the matching
// 'failed' status against this same category + payload.
func TestShipPlan_ReviewAgents_ReviewerError_EmitsPlanReviewFailed(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	reviewer := &fakePlanReviewer{
		// Model is irrelevant on the error path; the reviewer fails before
		// reporting a verdict. err models a timeout.
		err: fmt.Errorf("review timed out: context deadline exceeded"),
	}
	s, sf, _, au, rr := newPlanServerWithReviewer(t, runID, stageID, reviewer, specGatingReviewers)
	priv, _ := sf.issue(t, runID)
	body := validPlanBytes(t)

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	// Upload still returns 201 — the plan artifact is stored regardless of
	// reviewer health.
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	// Exactly one plan_review_failed entry, carrying the error string.
	var failedEntries []planreview.ReviewFailedPayload
	for _, e := range au.appended {
		switch e.Category {
		case "plan_review_failed":
			var p planreview.ReviewFailedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatalf("decode plan_review_failed payload: %v", err)
			}
			failedEntries = append(failedEntries, p)
		case "plan_reviewed":
			t.Errorf("unexpected plan_reviewed entry on the reviewer-error path")
		}
	}
	if len(failedEntries) != 1 {
		t.Fatalf("plan_review_failed entries = %d, want 1", len(failedEntries))
	}
	got := failedEntries[0]
	if got.Reason != "review timed out: context deadline exceeded" {
		t.Errorf("reason = %q, want the reviewer error string", got.Reason)
	}
	if got.Authority != planreview.AuthorityGating {
		t.Errorf("authority = %q, want gating", got.Authority)
	}
	// #747: a fast, non-deadline error is NOT a timeout-kill. The discriminator
	// must read false so a transport/decode failure is distinguishable from a
	// budget-deadline kill.
	if got.Timeout {
		t.Errorf("timeout = true, want false for a non-deadline reviewer error")
	}

	// #574: an erroring gating reviewer must NOT fail the stage — gating
	// advance/degrade semantics are unchanged by this observability-only PR.
	for _, call := range rr.transitionStageCalls {
		if call.StageID == stageID && call.To == run.StageStateFailed {
			t.Errorf("reviewer error must not transition the gating stage to failed-B")
		}
	}
}

// deadlineWaitingReviewer blocks each Review call until the invocation context
// deadline (the size-aware #747 budget applied at the server call site) fires,
// then returns ctx.Err(). It models a reviewer killed mid-inference by the
// budget — the exact #747 failure mode — so the server-level seam test can
// assert the resulting *_review_failed entry carries Timeout=true.
type deadlineWaitingReviewer struct{}

func (deadlineWaitingReviewer) Review(ctx context.Context, _ string) (*planreview.ReviewVerdict, string, error) {
	<-ctx.Done()
	return nil, "", ctx.Err()
}

// TestShipPlan_ReviewAgents_BudgetTimeout_EmitsTimeoutTrue is the #747
// server-level seam test: it injects a reviewer that blocks until its
// invocation deadline fires, sets a tiny review budget so the deadline fires
// quickly, and asserts exactly one plan_review_failed entry carrying
// Timeout=true. This exercises budget computation, deadline application at the
// call site, and the audit emit together (cf. #618), not per-layer in isolation.
func TestShipPlan_ReviewAgents_BudgetTimeout_EmitsTimeoutTrue(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, au, _ := newPlanServerWithReviewer(t, runID, stageID, deadlineWaitingReviewer{}, specGatingReviewers)
	// Collapse the budget to a tiny flat floor so the deadline fires fast.
	s.cfg.ReviewBudget = planreview.ReviewBudget{Floor: 20 * time.Millisecond}
	priv, _ := sf.issue(t, runID)
	body := validPlanBytes(t)

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	var failedEntries []planreview.ReviewFailedPayload
	for _, e := range au.appended {
		switch e.Category {
		case "plan_review_failed":
			var p planreview.ReviewFailedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatalf("decode plan_review_failed payload: %v", err)
			}
			failedEntries = append(failedEntries, p)
		case "plan_reviewed":
			t.Errorf("unexpected plan_reviewed entry on the budget-timeout path")
		}
	}
	if len(failedEntries) != 1 {
		t.Fatalf("plan_review_failed entries = %d, want 1", len(failedEntries))
	}
	if !failedEntries[0].Timeout {
		t.Errorf("timeout = false, want true for a budget-deadline kill")
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
	if s.runPlanReviews(ctx, runID, stageID, body, nil, nil, nil) {
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

// --- heterogeneous review agents (#955) ---

// specHeterogeneousPlanReviewers builds a plan-stage spec declaring the
// #955 heterogeneous agents list (anthropic + codex with explicit models)
// and the given human count (0 → gating, >0 → advisory).
func specHeterogeneousPlanReviewers(human int) []byte {
	return fmt.Appendf(nil, `version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          agents:
            - provider: anthropic
              model: claude-opus-4-8
            - provider: codex
              model: gpt-5.5
          human: %d
        produces:
          - artifact: plan
            schema: standard_v1
`, human)
}

// newPlanServerWithReviewerSet mirrors newPlanServerWithReviewer for tests
// that need full ReviewerSet control (heterogeneous providers, unresolvable
// providers). logger may be nil (Config defaults it).
func newPlanServerWithReviewerSet(t *testing.T, runID, stageID uuid.UUID, set ReviewerSet, workflowSpec []byte, logger *slog.Logger) (*Server, *signingFake, *fakeArtifactRepo, *auditFake, *promptRunRepo) {
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
		Addr:          "127.0.0.1:0",
		SigningRepo:   sf,
		ArtifactRepo:  ar,
		AuditRepo:     au,
		RunRepo:       rr,
		PlanReviewers: set,
		Logger:        logger,
	})
	return s, sf, ar, au, rr
}

// collectPlanReviewed decodes every plan_reviewed entry in the audit fake.
func collectPlanReviewed(t *testing.T, au *auditFake) []planreview.PlanReviewedPayload {
	t.Helper()
	var out []planreview.PlanReviewedPayload
	for _, e := range au.appended {
		if e.Category != "plan_reviewed" {
			continue
		}
		var p planreview.PlanReviewedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode plan_reviewed payload: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// TestShipPlan_ReviewAgents_Heterogeneous_CrossBoundary is the #955
// cross-boundary integration test for the plan loop: a real workflow-spec
// YAML declaring two heterogeneous agent reviewers ships through
// spec.ParseBytes → resolveStageReviewers → resolveReviewerInvocations →
// runPlanReviewLoop → audit repo, and produces exactly two plan_reviewed
// entries with the two distinct ReviewerModel values plus two reviewer-cost
// recordings. It fails if any seam (schema property name, struct tag,
// AgentCount supersession, For() mapping, audit payload) breaks while
// per-layer units pass.
func TestShipPlan_ReviewAgents_Heterogeneous_CrossBoundary(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	anthropicFake := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-8",
	}
	codexFake := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "gpt-5.5",
	}
	set := fakeReviewerSet{providers: map[string]PlanReviewer{
		"anthropic": anthropicFake,
		"codex":     codexFake,
	}, def: anthropicFake}
	s, sf, _, au, rr := newPlanServerWithReviewerSet(t, runID, stageID, set, specHeterogeneousPlanReviewers(0), nil)
	priv, _ := sf.issue(t, runID)

	w := shipPlanRequest(t, s, runID, stageID, priv, validPlanBytes(t), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	// Each declared provider's adapter was invoked exactly once.
	anthropicFake.mu.Lock()
	codexFake.mu.Lock()
	if len(anthropicFake.calls) != 1 || len(codexFake.calls) != 1 {
		t.Errorf("adapter calls = anthropic:%d codex:%d, want 1 each",
			len(anthropicFake.calls), len(codexFake.calls))
	}
	codexFake.mu.Unlock()
	anthropicFake.mu.Unlock()

	reviewed := collectPlanReviewed(t, au)
	if len(reviewed) != 2 {
		t.Fatalf("plan_reviewed entries = %d, want 2", len(reviewed))
	}
	models := map[string]bool{}
	for _, p := range reviewed {
		models[p.ReviewerModel] = true
		if p.Authority != planreview.AuthorityGating {
			t.Errorf("authority = %q, want gating (agents list, human 0)", p.Authority)
		}
	}
	if !models["claude-opus-4-8"] || !models["gpt-5.5"] {
		t.Errorf("reviewer models = %v, want both claude-opus-4-8 and gpt-5.5", models)
	}

	// Two reviewer-cost recordings (#681), one per invocation.
	costs := 0
	for _, e := range au.appended {
		if e.Category == "cost_recorded" && strings.Contains(string(e.Payload), `"source":"plan_review"`) {
			costs++
		}
	}
	if costs != 2 {
		t.Errorf("plan_review cost_recorded entries = %d, want 2", costs)
	}

	// The started proxy reports the effective count, len(agents) == 2.
	for _, e := range au.appended {
		if e.Category != "plan_review_started" {
			continue
		}
		var p planreview.ReviewStartedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode plan_review_started payload: %v", err)
		}
		if p.ConfiguredAgents != 2 {
			t.Errorf("started configured_agents = %d, want 2 (len(agents))", p.ConfiguredAgents)
		}
	}

	// Both approved — gating must not fail the stage.
	for _, call := range rr.transitionStageCalls {
		if call.StageID == stageID && call.To == run.StageStateFailed {
			t.Errorf("stage failed despite both heterogeneous reviewers approving")
		}
	}
}

// TestShipPlan_ReviewAgents_Heterogeneous_GatingRejectBlocks pins that a
// single heterogeneous reviewer's reject under gating authority still
// fails the stage category-B (#955 preserves ADR-027 semantics).
func TestShipPlan_ReviewAgents_Heterogeneous_GatingRejectBlocks(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	approve := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-8",
	}
	reject := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictReject},
		model:   "gpt-5.5",
	}
	set := fakeReviewerSet{providers: map[string]PlanReviewer{
		"anthropic": approve,
		"codex":     reject,
	}, def: approve}
	s, sf, _, _, rr := newPlanServerWithReviewerSet(t, runID, stageID, set, specHeterogeneousPlanReviewers(0), nil)
	priv, _ := sf.issue(t, runID)

	w := shipPlanRequest(t, s, runID, stageID, priv, validPlanBytes(t), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	var failed bool
	for _, call := range rr.transitionStageCalls {
		if call.StageID == stageID && call.To == run.StageStateFailed {
			failed = true
		}
	}
	if !failed {
		t.Error("gating reject from a heterogeneous reviewer must transition the stage to failed-B")
	}
}

// TestShipPlan_ReviewAgents_CountForm_InvokesDefaultTwice pins #955
// back-compat: a bare `agent: 2` count with no agents list invokes the
// set's Default() adapter twice and never resolves through For() — the
// fakeReviewerSet's empty provider map errors on any For() call, so a
// regression that routes the count form through For() fails loudly here.
func TestShipPlan_ReviewAgents_CountForm_InvokesDefaultTwice(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	def := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-sonnet-4-6",
	}
	set := fakeReviewerSet{def: def}
	spec := []byte(`version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          agent: 2
          human: 0
        produces:
          - artifact: plan
            schema: standard_v1
`)
	s, sf, _, au, _ := newPlanServerWithReviewerSet(t, runID, stageID, set, spec, nil)
	priv, _ := sf.issue(t, runID)

	w := shipPlanRequest(t, s, runID, stageID, priv, validPlanBytes(t), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	def.mu.Lock()
	calls := len(def.calls)
	def.mu.Unlock()
	if calls != 2 {
		t.Errorf("default adapter calls = %d, want 2", calls)
	}
	if reviewed := collectPlanReviewed(t, au); len(reviewed) != 2 {
		t.Errorf("plan_reviewed entries = %d, want 2", len(reviewed))
	}
	for _, e := range au.appended {
		if e.Category == "plan_review_failed" {
			t.Errorf("unexpected plan_review_failed entry — count form must not resolve via For(): %s", e.Payload)
		}
	}
}

// TestShipPlan_ReviewAgents_Heterogeneous_SelfReviewPerReviewer pins that
// the ADR-027 self-review guard applies per-invocation: with two
// heterogeneous reviewers where only reviewer #1's model matches the plan
// author's model (planfixture.Valid() → claude-opus-4-7), exactly one WARN
// fires and BOTH verdicts are still recorded.
func TestShipPlan_ReviewAgents_Heterogeneous_SelfReviewPerReviewer(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	selfReviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7", // matches the fixture's generated_by.model
	}
	other := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "gpt-5.5",
	}
	set := fakeReviewerSet{providers: map[string]PlanReviewer{
		"anthropic": selfReviewer,
		"codex":     other,
	}, def: selfReviewer}
	var logBuf bytes.Buffer
	logMu := &syncWriter{w: &logBuf}
	logger := slog.New(slog.NewTextHandler(logMu, nil))
	s, sf, _, au, _ := newPlanServerWithReviewerSet(t, runID, stageID, set, specHeterogeneousPlanReviewers(0), logger)
	priv, _ := sf.issue(t, runID)

	w := shipPlanRequest(t, s, runID, stageID, priv, validPlanBytes(t), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (self-review is warn-only):\n%s", w.Code, w.Body.String())
	}

	if reviewed := collectPlanReviewed(t, au); len(reviewed) != 2 {
		t.Errorf("plan_reviewed entries = %d, want 2 (self-review must not drop a verdict)", len(reviewed))
	}
	if got := strings.Count(logMu.String(), "self-review detected"); got != 1 {
		t.Errorf("self-review WARN count = %d, want exactly 1 (only reviewer #1 matches the author model)", got)
	}
}

// syncWriter serializes writes for a logger shared across goroutines.
type syncWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func (s *syncWriter) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.String()
}

// TestShipPlan_ReviewAgents_Heterogeneous_UnresolvableProvider_Advisory
// pins the #955 degradation path: in advisory mode, an agents-list entry
// whose provider is not configured emits a plan_review_failed entry with
// the resolve error, the loop continues to the resolvable reviewer, and
// the stage is never failed.
func TestShipPlan_ReviewAgents_Heterogeneous_UnresolvableProvider_Advisory(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	anthropicFake := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-8",
	}
	// codex deliberately absent from the set → For("codex") errors.
	set := fakeReviewerSet{providers: map[string]PlanReviewer{
		"anthropic": anthropicFake,
	}, def: anthropicFake}
	s, sf, _, au, rr := newPlanServerWithReviewerSet(t, runID, stageID, set, specHeterogeneousPlanReviewers(1), nil)
	priv, _ := sf.issue(t, runID)

	w := shipPlanRequest(t, s, runID, stageID, priv, validPlanBytes(t), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	// Advisory review runs detached (#584); drain before asserting.
	s.waitBackgroundReviews()

	if reviewed := collectPlanReviewed(t, au); len(reviewed) != 1 {
		t.Fatalf("plan_reviewed entries = %d, want 1 (the resolvable anthropic reviewer)", len(reviewed))
	}
	var failedEntries []planreview.ReviewFailedPayload
	for _, e := range au.appended {
		if e.Category != "plan_review_failed" {
			continue
		}
		var p planreview.ReviewFailedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode plan_review_failed payload: %v", err)
		}
		failedEntries = append(failedEntries, p)
	}
	if len(failedEntries) != 1 {
		t.Fatalf("plan_review_failed entries = %d, want 1 (the unresolvable codex reviewer)", len(failedEntries))
	}
	if !strings.Contains(failedEntries[0].Reason, "not configured") {
		t.Errorf("failed reason = %q, want the resolve error", failedEntries[0].Reason)
	}
	if failedEntries[0].Timeout {
		t.Error("resolve failure must not set the timeout discriminator")
	}

	for _, call := range rr.transitionStageCalls {
		if call.StageID == stageID && call.To == run.StageStateFailed {
			t.Errorf("advisory resolve failure must not fail the stage")
		}
	}
}

// TestShipPlan_ReviewAgents_Heterogeneous_UnresolvableProvider_Gating is the
// gating analog of the advisory degradation test: when one of two declared
// gating reviewers cannot be resolved (config drift on an in-flight run — the
// runs.go dispatch pre-check blocks fresh gating runs from entering this
// state), the resolve failure emits exactly one plan_review_failed entry,
// leaves hasRejection untouched, and the resolvable reviewer's approve
// verdict still governs: the stage advances rather than failing.
func TestShipPlan_ReviewAgents_Heterogeneous_UnresolvableProvider_Gating(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	anthropicFake := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-8",
	}
	// codex deliberately absent from the set → For("codex") errors.
	set := fakeReviewerSet{providers: map[string]PlanReviewer{
		"anthropic": anthropicFake,
	}, def: anthropicFake}
	s, sf, _, au, rr := newPlanServerWithReviewerSet(t, runID, stageID, set, specHeterogeneousPlanReviewers(0), nil)
	priv, _ := sf.issue(t, runID)

	w := shipPlanRequest(t, s, runID, stageID, priv, validPlanBytes(t), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	reviewed := collectPlanReviewed(t, au)
	if len(reviewed) != 1 {
		t.Fatalf("plan_reviewed entries = %d, want 1 (the resolvable anthropic reviewer)", len(reviewed))
	}
	if reviewed[0].ReviewerModel != "claude-opus-4-8" || reviewed[0].Authority != planreview.AuthorityGating {
		t.Errorf("reviewed entry = model %q authority %q, want claude-opus-4-8 / gating",
			reviewed[0].ReviewerModel, reviewed[0].Authority)
	}

	var failedEntries []planreview.ReviewFailedPayload
	for _, e := range au.appended {
		if e.Category != "plan_review_failed" {
			continue
		}
		var p planreview.ReviewFailedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode plan_review_failed payload: %v", err)
		}
		failedEntries = append(failedEntries, p)
	}
	if len(failedEntries) != 1 {
		t.Fatalf("plan_review_failed entries = %d, want 1 (the unresolvable codex reviewer)", len(failedEntries))
	}
	if !strings.Contains(failedEntries[0].Reason, "not configured") {
		t.Errorf("failed reason = %q, want the resolve error", failedEntries[0].Reason)
	}
	if failedEntries[0].Authority != planreview.AuthorityGating {
		t.Errorf("failed authority = %q, want gating", failedEntries[0].Authority)
	}

	// A resolve failure must not count as a rejection: with the resolvable
	// reviewer approving, the gating stage must never transition to failed.
	for _, call := range rr.transitionStageCalls {
		if call.StageID == stageID && call.To == run.StageStateFailed {
			t.Errorf("gating resolve failure must not fail the stage when the resolvable reviewer approves")
		}
	}
}

// TestPlanReviewLoop_PersistsConcernsWithOriginSequence is the plan-side
// #964 persistence test: a plan_reviewed verdict's concerns land in the
// durable store with stage_kind plan and origin_review_sequence equal to
// the appended entry's returned sequence.
func TestPlanReviewLoop_PersistsConcernsWithOriginSequence(t *testing.T) {
	au := newSeqAuditFake()
	cr := newFakeConcernRepo()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, ConcernRepo: cr})
	runID, stageID := uuid.New(), uuid.New()

	rev := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{
			Verdict:  planreview.VerdictApproveWithConcerns,
			Concerns: []planreview.Concern{{Severity: planreview.SeverityMedium, Category: "verification", Note: "missing integration test"}},
		},
		model: "gpt-5.5",
	}
	s.runPlanReviewLoop(context.Background(), runID, stageID,
		[]reviewerInvocation{{reviewer: rev}}, planreview.AuthorityAdvisory, "prompt", "author-model")

	reviewed := au.entriesByCategory("plan_reviewed")
	if len(reviewed) != 1 {
		t.Fatalf("plan_reviewed entries = %d, want 1", len(reviewed))
	}
	rows, err := cr.ListByRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("persisted concerns = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.StageKind != concern.StageKindPlan {
		t.Errorf("StageKind = %q, want plan", row.StageKind)
	}
	if row.OriginReviewSequence != reviewed[0].Sequence {
		t.Errorf("OriginReviewSequence = %d, want %d", row.OriginReviewSequence, reviewed[0].Sequence)
	}
	if row.State != concern.StateRaised {
		t.Errorf("State = %q, want raised", row.State)
	}
	if row.ReviewerModel == nil || *row.ReviewerModel != "gpt-5.5" {
		t.Errorf("ReviewerModel = %v, want gpt-5.5", row.ReviewerModel)
	}
}

// TestShipPlan_SubPlanCoupling_EndToEnd is the #1077 cross-boundary check:
// a decomposed plan whose sub-plans under-scope a migration (one slice) and
// a canonical schema's cli mirror (another slice) is POSTed through
// handleShipPlan, and the FULL path is asserted end to end — the persisted
// plan_surface_sweep / plan_test_sweep audit payloads carry the
// sub-plan-attributed findings, AND the SAME findings are consumed by
// planGateEvidence and rendered into the captured plan-review prompt with
// the "(sub-plan: <title>)" prefix. This covers the audit-persist →
// planGateEvidence → prompt-render consumer boundary so the SubPlanTitle
// threading cannot silently drop between persistence and render.
func TestShipPlan_SubPlanCoupling_EndToEnd(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-sonnet-4-6",
	}
	s, sf, _, au, rr := newPlanServerWithReviewer(t, runID, stageID, reviewer, specGatingReviewersWithConstraints)
	// The parent scope's only directory; listed clean so no parent finding
	// competes with the sub-plan-attributed ones.
	cf := &contentsFake{dirs: map[string][]string{
		"backend/internal/server": {"upload.go"},
	}}
	instID := int64(42)
	rr.getRuns[runID].InstallationID = &instID
	s.cfg.GitHub = newTestSweepGitHub(t, cf)
	priv, _ := sf.issue(t, runID)

	body := decomposedScopePlanBody(t,
		[]plan.ScopeFile{{Path: "backend/internal/server/upload.go", Operation: plan.FileOpModify}},
		[]subPlanScope{
			{
				title: "migration slice",
				files: []plan.ScopeFile{{Path: "backend/internal/postgres/migrations/0032_x.up.sql", Operation: plan.FileOpCreate}},
			},
			{
				title: "schema slice",
				files: []plan.ScopeFile{
					{Path: "docs/spec/workflow-v0.schema.json", Operation: plan.FileOpModify},
					{Path: "backend/internal/spec/schemas/workflow-v0.schema.json", Operation: plan.FileOpModify},
				},
			},
		},
	)

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	// (a) the surface-sweep audit entry carries the schema-slice finding.
	surface := lastSurfaceSweepEntry(t, au)
	var surfaceFinding *SurfaceSweepFinding
	for i := range surface.Findings {
		if surface.Findings[i].SubPlanTitle == "schema slice" {
			surfaceFinding = &surface.Findings[i]
		}
	}
	if surfaceFinding == nil {
		t.Fatalf("surface sweep payload missing the schema-slice finding: %+v", surface.Findings)
	}
	if surfaceFinding.Pattern != "workflow schema requires every mirror" ||
		len(surfaceFinding.MissingSiblings) != 1 ||
		surfaceFinding.MissingSiblings[0] != "cli/internal/spec/schemas/workflow-v0.schema.json" {
		t.Errorf("surface finding = %+v", surfaceFinding)
	}

	// (b) the test-sweep audit entry carries the migration-slice finding.
	test := lastTestSweepEntry(t, au)
	var testFinding *TestSweepFinding
	for i := range test.Findings {
		if test.Findings[i].SubPlanTitle == "migration slice" {
			testFinding = &test.Findings[i]
		}
	}
	if testFinding == nil {
		t.Fatalf("test sweep payload missing the migration-slice finding: %+v", test.Findings)
	}
	if testFinding.Rule != testSweepRuleMigrationWalk ||
		len(testFinding.MissingTests) != 1 ||
		testFinding.MissingTests[0] != "backend/internal/postgres/postgres_test.go" {
		t.Errorf("test finding = %+v", testFinding)
	}

	// (c) the SAME findings are consumed by planGateEvidence and rendered
	// into the captured plan-review prompt with the sub-plan prefix — the
	// audit-persist → planGateEvidence → prompt-render consumer boundary.
	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if len(reviewer.calls) != 1 {
		t.Fatalf("reviewer calls = %d, want 1", len(reviewer.calls))
	}
	got := reviewer.calls[0]
	wants := []string{
		"(sub-plan: schema slice) MISSING SIBLINGS (workflow schema requires every mirror)",
		"cli/internal/spec/schemas/workflow-v0.schema.json",
		"(sub-plan: migration slice) EXISTING TESTS NOT IN SCOPE (migration_walk)",
		"backend/internal/postgres/postgres_test.go",
	}
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("plan-review prompt missing sub-plan-attributed element %q — persist→evidence→render seam broken:\n%s", want, got)
		}
	}
}

// TestShipPlan_CrossSliceCoupling_EndToEnd is the #1102 cross-boundary
// check: a decomposed plan that SPLITS a lockstep pattern's members across
// two slices (the work-management schema's canonical in one slice, its
// mirror in another) is POSTed through handleShipPlan, and the full path is
// asserted end to end — the persisted plan_surface_sweep payload carries the
// cross_slice_findings, AND the SAME finding is consumed by planGateEvidence
// (result.CrossSliceFindings -> prompt.SurfaceSweepEvidence.CrossSliceFindings)
// and rendered into the captured plan-review prompt's CROSS-SLICE COUPLING
// line. This covers the audit-persist -> planGateEvidence -> prompt-render
// consumer boundary so the mapping cannot silently drop.
func TestShipPlan_CrossSliceCoupling_EndToEnd(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-sonnet-4-6",
	}
	s, sf, _, au, rr := newPlanServerWithReviewer(t, runID, stageID, reviewer, specGatingReviewersWithConstraints)
	cf := &contentsFake{dirs: map[string][]string{
		"backend/internal/server": {"upload.go"},
	}}
	instID := int64(42)
	rr.getRuns[runID].InstallationID = &instID
	s.cfg.GitHub = newTestSweepGitHub(t, cf)
	priv, _ := sf.issue(t, runID)

	body := decomposedScopePlanBody(t,
		[]plan.ScopeFile{{Path: "backend/internal/server/upload.go", Operation: plan.FileOpModify}},
		[]subPlanScope{
			{
				title: "schema slice",
				files: []plan.ScopeFile{{Path: "docs/spec/work-management-v0.schema.json", Operation: plan.FileOpModify}},
			},
			{
				title: "wiring slice",
				files: []plan.ScopeFile{{Path: "backend/internal/workmgmt/schemas/work-management-v0.schema.json", Operation: plan.FileOpModify}},
			},
		},
	)

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	// (a) the surface-sweep audit entry carries the cross-slice finding.
	surface := lastSurfaceSweepEntry(t, au)
	if len(surface.CrossSliceFindings) != 1 {
		t.Fatalf("surface sweep payload missing cross-slice finding: %+v", surface.CrossSliceFindings)
	}
	f := surface.CrossSliceFindings[0]
	if f.Pattern != "work-management schema requires every mirror" || len(f.Slices) != 2 {
		t.Errorf("cross-slice finding = %+v", f)
	}

	// (b) the SAME finding is consumed by planGateEvidence and rendered into
	// the captured plan-review prompt — the persist -> evidence -> render seam.
	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if len(reviewer.calls) != 1 {
		t.Fatalf("reviewer calls = %d, want 1", len(reviewer.calls))
	}
	got := reviewer.calls[0]
	wants := []string{
		"CROSS-SLICE COUPLING (work-management schema requires every mirror)",
		"\"schema slice\" owns [docs/spec/work-management-v0.schema.json]",
		"\"wiring slice\" owns [backend/internal/workmgmt/schemas/work-management-v0.schema.json]",
	}
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("plan-review prompt missing cross-slice element %q — persist→evidence→render seam broken:\n%s", want, got)
		}
	}
}

// validClarificationBytes returns a minimal clarification_request artifact
// (#1057) that validates against clarification-request-v1: the top-level kind
// discriminator plus ticket_reference / generated_by / summary / questions.
func validClarificationBytes(t *testing.T) []byte {
	t.Helper()
	return []byte(`{
  "kind": "clarification_request",
  "ticket_reference": {"type": "github_issue", "url": "https://github.com/kuhlman-labs/fishhawk/issues/1057", "id": "kuhlman-labs/fishhawk#1057"},
  "generated_by": {"agent": "claude-code", "model": "claude-opus-4-8", "timestamp": "2026-06-14T00:00:00Z"},
  "summary": "Issue needs an operator policy decision before a concrete plan can be written.",
  "questions": [
    {"id": "auth-backend", "question": "Which auth backend should the token store use?", "what_i_can_infer": "Both an in-memory and a Postgres store exist.", "recommended_default": "Postgres", "tradeoffs": "Postgres survives restarts but adds a migration; in-memory is simpler but loses tokens."}
  ]
}`)
}

// TestShipPlan_ClarificationRequest_ParksAwaitingInput is the slice-3 backend
// seam (#1057): when the runner ships a clarification_request sibling to
// POST /plan instead of a standard_v1 plan, handleShipPlan discriminates it by
// the top-level kind, validates it, persists the full document in a
// clarification_requested audit entry (NOT the artifacts table), and parks the
// plan stage at awaiting_input — a D-category judgment, not an approval and not
// a failure.
func TestShipPlan_ClarificationRequest_ParksAwaitingInput(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, rr := newPlanServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)

	w := shipPlanRequest(t, s, runID, stageID, priv, validClarificationBytes(t), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	var resp planResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SchemaVersion != "clarification-request-v1" {
		t.Errorf("schema_version = %q, want clarification-request-v1", resp.SchemaVersion)
	}
	if resp.Idempotent {
		t.Error("first clarification upload should not be marked idempotent")
	}

	// Persisted in the audit log, not the artifacts table.
	if len(ar.all) != 0 {
		t.Errorf("artifacts = %d, want 0 (clarification rides the audit log)", len(ar.all))
	}
	if len(au.appended) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(au.appended))
	}
	if got := au.appended[0].Category; got != "clarification_requested" {
		t.Errorf("audit category = %q, want clarification_requested", got)
	}
	// The full document must ride in the entry payload (the ping renderer and
	// resume prompt read it back).
	if !bytes.Contains(au.appended[0].Payload, []byte("auth-backend")) {
		t.Errorf("audit payload missing the clarification document: %s", au.appended[0].Payload)
	}

	// Parked at awaiting_input — never awaiting_approval, never failed.
	var sawPark bool
	for _, c := range rr.transitionStageCalls {
		if c.To == run.StageStateAwaitingInput {
			sawPark = true
		}
		if c.To == run.StageStateAwaitingApproval {
			t.Errorf("clarification stage wrongly transitioned to awaiting_approval")
		}
	}
	if !sawPark {
		t.Errorf("stage was not parked at awaiting_input; transitions=%v", rr.transitionStageCalls)
	}
}

// TestShipPlan_ClarificationRequest_DuplicateID_400 confirms the unique-question-id
// semantic (#1057): operator answers are keyed by question id on resume, so a
// clarification_request whose questions share an id is rejected at ingest
// (400 clarification_request_invalid) rather than parked with an ambiguous
// artifact.
func TestShipPlan_ClarificationRequest_DuplicateID_400(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, au, rr := newPlanServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)

	dup := []byte(`{
  "kind": "clarification_request",
  "ticket_reference": {"type": "github_issue", "url": "https://github.com/kuhlman-labs/fishhawk/issues/1057", "id": "kuhlman-labs/fishhawk#1057"},
  "generated_by": {"agent": "claude-code", "model": "claude-opus-4-8", "timestamp": "2026-06-14T00:00:00Z"},
  "summary": "Two questions reuse the same id.",
  "questions": [
    {"id": "dupe", "question": "First?", "recommended_default": "a", "tradeoffs": "x"},
    {"id": "dupe", "question": "Second?", "recommended_default": "b", "tradeoffs": "y"}
  ]
}`)

	w := shipPlanRequest(t, s, runID, stageID, priv, dup, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("clarification_request_invalid")) {
		t.Errorf("error code missing clarification_request_invalid: %s", w.Body.String())
	}
	// An invalid park is the agent's bad output: the stage fails (category-B),
	// it is never parked at awaiting_input.
	for _, c := range rr.transitionStageCalls {
		if c.To == run.StageStateAwaitingInput {
			t.Errorf("invalid clarification was wrongly parked at awaiting_input")
		}
	}
	// No clarification_requested entry for a rejected artifact.
	for _, p := range au.appended {
		if p.Category == "clarification_requested" {
			t.Errorf("rejected clarification should not append a clarification_requested entry")
		}
	}
}

// TestShipPlan_ClarificationRequest_Idempotent confirms a runner retry after a
// successful park is idempotent (#1057): the stage is already awaiting_input, so
// a re-POST of the same sibling returns 200 idempotent with no second audit
// entry and no second transition.
func TestShipPlan_ClarificationRequest_Idempotent(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, au, _ := newPlanServer(t, runID, stageID)
	priv, _ := sf.issue(t, runID)
	body := validClarificationBytes(t)

	w1 := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w1.Code != http.StatusCreated {
		t.Fatalf("first upload status = %d, want 201:\n%s", w1.Code, w1.Body.String())
	}

	w2 := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w2.Code != http.StatusOK {
		t.Fatalf("second upload status = %d, want 200:\n%s", w2.Code, w2.Body.String())
	}
	var resp planResponse
	_ = json.NewDecoder(w2.Body).Decode(&resp)
	if !resp.Idempotent {
		t.Error("second clarification upload should be marked idempotent=true")
	}
	if len(au.appended) != 1 {
		t.Errorf("audit entries = %d, want 1 (no second clarification_requested)", len(au.appended))
	}
}
