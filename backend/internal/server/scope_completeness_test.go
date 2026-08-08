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
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// scopeCompletenessServer wires an orchestratorRepo (run repo) + auditCapture
// and seeds one implement stage parked in awaiting_scope_decision carrying a
// held-commit ScopeCompletenessPark — the state the decision endpoint acts on.
func scopeCompletenessServer(t *testing.T) (*Server, *orchestratorRepo, *auditCapture, *run.Run, *run.Stage) {
	t.Helper()
	rr := newOrchestratorRepo()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 1, run.StageStateAwaitingScopeDecision)
	stage.Type = run.StageTypeImplement
	stage.ScopeCompletenessPark = &run.ScopeCompletenessPark{
		HeldCommitSHA:   "1111111111111111111111111111111111111111",
		RunBranch:       "fishhawk/run-aaa/slice-0",
		VerifiedTreeSHA: "2222222222222222222222222222222222222222",
		MissingPaths:    []string{"backend/internal/foo/foo_test.go"},
	}
	au := &auditCapture{}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
	return s, rr, au, runRow, stage
}

func postScopeCompletenessDecision(t *testing.T, s *Server, pathRunID uuid.UUID, body string, decorate func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/v0/runs/"+pathRunID.String()+"/scope-completeness/decision",
		strings.NewReader(body))
	req.SetPathValue("run_id", pathRunID.String())
	if decorate != nil {
		req = decorate(req)
	}
	w := httptest.NewRecorder()
	s.handleDecideScopeCompleteness(w, req)
	return w
}

func operatorWriteStages(r *http.Request) *http.Request {
	return withOperatorIdentity(r, "write:stages")
}

// TestDecideScopeCompleteness_Exempt resumes the parked stage to running so
// the held commit's PR can be opened with NO agent re-run (the decision
// endpoint never fails the stage nor re-dispatches it), and appends the
// scope_completeness_exempted entry carrying the held commit + gate_evidence.
func TestDecideScopeCompleteness_Exempt(t *testing.T) {
	s, rr, au, runRow, stage := scopeCompletenessServer(t)

	w := postScopeCompletenessDecision(t, s, runRow.ID,
		`{"decision":"exempt","reason":"the coupled test file is genuinely already covered"}`,
		operatorWriteStages)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	var resp scopeCompletenessDecisionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Decision != "exempt" || resp.State != string(run.StageStatePending) {
		t.Errorf("response = %+v, want exempt/pending (#2501: a `running` stage is not dispatch-admissible)", resp)
	}
	if resp.HeldCommitSHA != "1111111111111111111111111111111111111111" {
		t.Errorf("response held_commit_sha = %q, want the parked held commit", resp.HeldCommitSHA)
	}

	got, _ := rr.GetStage(context.Background(), stage.ID)
	// #2501: the resolved state must be DISPATCH-ADMISSIBLE. host_dispatch.go's
	// admission switch accepts only {pending, awaiting_host_dispatch}, so the
	// pre-#2501 `running` transition was a dead end on both runner kinds and no
	// runner ever spawned to open the held commit's PR.
	if !dispatchAdmissible(got.State) {
		t.Errorf("stage state = %q, want a DISPATCH-ADMISSIBLE state (pending / awaiting_host_dispatch)", got.State)
	}
	// Zero-re-run at this layer: the decision endpoint must NOT fail the stage
	// (the full agent-called-once invariant is asserted by the cross-layer
	// e2e). A failure here would mean it dropped to category-B, not exempt.
	if got.FailureCategory != nil {
		t.Errorf("stage failure category = %v, want nil (exempt never fails the stage)", got.FailureCategory)
	}

	au.mu.Lock()
	defer au.mu.Unlock()
	if len(au.appended) != 1 || au.appended[0].Category != CategoryScopeCompletenessExempted {
		t.Fatalf("audit = %+v, want one scope_completeness_exempted entry", au.appended)
	}
	var payload map[string]any
	_ = json.Unmarshal(au.appended[0].Payload, &payload)
	if payload["held_commit_sha"] != "1111111111111111111111111111111111111111" {
		t.Errorf("exempt payload held_commit_sha = %v", payload["held_commit_sha"])
	}
	if payload["gate_evidence"] != CategoryScopeCompletenessExempted {
		t.Errorf("exempt payload gate_evidence = %v, want the #1153 channel marker", payload["gate_evidence"])
	}
}

// TestDecideScopeCompleteness_Fail drops the parked stage to category-B
// (today's restore path) and appends the scope_completeness_failed entry.
// dispatchAdmissible mirrors host_dispatch.go's admission switch: the states a
// runner can actually be dispatched from. A stage the exempt decision leaves in
// any OTHER state can never spawn a runner, which is the #2501 dead end.
func dispatchAdmissible(st run.StageState) bool {
	return st == run.StageStatePending || st == run.StageStateAwaitingHostDispatch
}

// TestDecideScopeCompleteness_ExemptAssertionClassPark pins the SECOND
// shortfall class (#2501): a park carrying only unsatisfied binding assertions
// resolves exempt exactly like a missing-scope park, echoes the assertions on
// the response, and carries them into the exempted audit payload.
func TestDecideScopeCompleteness_ExemptAssertionClassPark(t *testing.T) {
	s, rr, au, runRow, stage := scopeCompletenessServer(t)
	stage.ScopeCompletenessPark = &run.ScopeCompletenessPark{
		HeldCommitSHA:   "1111111111111111111111111111111111111111",
		RunBranch:       "fishhawk/run-aaa/slice-0",
		VerifiedTreeSHA: "2222222222222222222222222222222222222222",
		UnsatisfiedAssertions: []run.UnsatisfiedAssertion{
			{Type: "file_contains", Path: "backend/internal/run/run.go", Literal: "UnsatisfiedAssertion"},
		},
	}

	w := postScopeCompletenessDecision(t, s, runRow.ID,
		`{"decision":"exempt","reason":"the operator-authored literal was wrong, not the implement output"}`,
		operatorWriteStages)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var resp scopeCompletenessDecisionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.UnsatisfiedAssertions) != 1 || resp.UnsatisfiedAssertions[0].Literal != "UnsatisfiedAssertion" {
		t.Errorf("response must echo the exempted assertion shortfall, got %+v", resp.UnsatisfiedAssertions)
	}
	if len(resp.MissingPaths) != 0 {
		t.Errorf("an assertion-class park must echo no missing paths, got %v", resp.MissingPaths)
	}
	got, _ := rr.GetStage(context.Background(), stage.ID)
	if !dispatchAdmissible(got.State) {
		t.Errorf("stage state = %q, want a DISPATCH-ADMISSIBLE state", got.State)
	}
	au.mu.Lock()
	defer au.mu.Unlock()
	var payload map[string]any
	_ = json.Unmarshal(au.appended[0].Payload, &payload)
	if payload["unsatisfied_assertions"] == nil {
		t.Errorf("exempted audit payload must carry unsatisfied_assertions, got %v", payload)
	}
}

// TestDecideScopeCompleteness_FailIsNotReachableInPending is the operator's
// added assertion (#2501 binding condition 2): the widened
// awaiting_scope_decision → pending edge must NOT make a FAIL-decided park
// reachable in pending. A fail lands the stage FAILED, full stop — otherwise a
// rejected park could be re-dispatched into the exempt path.
func TestDecideScopeCompleteness_FailIsNotReachableInPending(t *testing.T) {
	s, rr, _, runRow, stage := scopeCompletenessServer(t)
	w := postScopeCompletenessDecision(t, s, runRow.ID,
		`{"decision":"fail","reason":"the shortfall is load-bearing; re-scope"}`,
		operatorWriteStages)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	got, _ := rr.GetStage(context.Background(), stage.ID)
	if got.State != run.StageStateFailed {
		t.Fatalf("stage state = %q, want failed", got.State)
	}
	if dispatchAdmissible(got.State) {
		t.Errorf("a FAIL-decided park must NOT be reachable in a dispatch-admissible state, got %q", got.State)
	}
}

func TestDecideScopeCompleteness_Fail(t *testing.T) {
	s, rr, au, runRow, stage := scopeCompletenessServer(t)

	w := postScopeCompletenessDecision(t, s, runRow.ID,
		`{"decision":"fail","reason":"the missing files are load-bearing; re-scope"}`,
		operatorWriteStages)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	got, _ := rr.GetStage(context.Background(), stage.ID)
	if got.State != run.StageStateFailed {
		t.Errorf("stage state = %q, want failed", got.State)
	}
	if got.FailureCategory == nil || *got.FailureCategory != run.FailureB {
		t.Errorf("stage failure category = %v, want B", got.FailureCategory)
	}

	au.mu.Lock()
	defer au.mu.Unlock()
	if len(au.appended) != 1 || au.appended[0].Category != CategoryScopeCompletenessFailed {
		t.Fatalf("audit = %+v, want one scope_completeness_failed entry", au.appended)
	}
}

// TestDecideScopeCompleteness_NotParked409 pins the gate: a decision on a run
// whose implement stage is not parked in awaiting_scope_decision is rejected.
func TestDecideScopeCompleteness_NotParked409(t *testing.T) {
	rr := newOrchestratorRepo()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 1, run.StageStateRunning) // still running, not parked
	stage.Type = run.StageTypeImplement
	au := &auditCapture{}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})

	w := postScopeCompletenessDecision(t, s, runRow.ID,
		`{"decision":"exempt","reason":"r"}`, operatorWriteStages)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "stage_not_parked") {
		t.Errorf("body: %s", w.Body.String())
	}
}

// TestDecideScopeCompleteness_RunBoundToken403 pins the operator-only gate:
// the implement agent's own run-bound token may never decide its exemption.
func TestDecideScopeCompleteness_RunBoundToken403(t *testing.T) {
	s, _, _, runRow, _ := scopeCompletenessServer(t)
	w := postScopeCompletenessDecision(t, s, runRow.ID, `{"decision":"exempt","reason":"r"}`,
		func(r *http.Request) *http.Request {
			return withRunBoundIdentity(r, runRow.ID, "mcp:read", "write:scope-amendments")
		})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "self_decision") {
		t.Errorf("body: %s", w.Body.String())
	}
}

// TestDecideScopeCompleteness_MissingScope403 pins the token-scope gate.
func TestDecideScopeCompleteness_MissingScope403(t *testing.T) {
	s, _, _, runRow, _ := scopeCompletenessServer(t)
	w := postScopeCompletenessDecision(t, s, runRow.ID, `{"decision":"exempt","reason":"r"}`,
		func(r *http.Request) *http.Request { return withOperatorIdentity(r, "read:runs") })
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "insufficient_scope") {
		t.Errorf("body: %s", w.Body.String())
	}
}

// TestDecideScopeCompleteness_BadDecision400 / empty reason pin the input
// validation: only exempt|fail with a non-empty reason is accepted.
func TestDecideScopeCompleteness_BadInput400(t *testing.T) {
	s, _, _, runRow, _ := scopeCompletenessServer(t)
	for _, body := range []string{
		`{"decision":"maybe","reason":"r"}`,   // bad enum
		`{"decision":"exempt","reason":""}`,   // empty reason
		`{"decision":"exempt","reason":"  "}`, // whitespace-only reason
		`{"reason":"r"}`,                      // missing decision
	} {
		w := postScopeCompletenessDecision(t, s, runRow.ID, body, operatorWriteStages)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400; body = %s", body, w.Code, w.Body.String())
		}
	}
}

// TestDecideScopeCompleteness_UnknownRun404 pins the run lookup.
func TestDecideScopeCompleteness_UnknownRun404(t *testing.T) {
	s, _, _, _, _ := scopeCompletenessServer(t)
	w := postScopeCompletenessDecision(t, s, uuid.New(), `{"decision":"exempt","reason":"r"}`,
		operatorWriteStages)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", w.Code, w.Body.String())
	}
}

// orchestratorRepoOrderProbe records, at the moment the exempt decision
// transitions the parked stage out of awaiting_scope_decision, how many audit
// entries the decision had already appended. It is the observation seam for the
// ORDERING invariant: the exemption must be provable from the audit chain
// BEFORE the stage becomes dispatchable, because the transition (and the
// Advance that follows it) is what can spawn a runner whose prompt fetch reads
// that chain.
type orchestratorRepoOrderProbe struct {
	*orchestratorRepo
	au                     *auditCapture
	auditCountAtTransition int
	transitioned           bool
}

func (r *orchestratorRepoOrderProbe) TransitionStage(ctx context.Context, id uuid.UUID,
	to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	r.au.mu.Lock()
	r.auditCountAtTransition = len(r.au.appended)
	r.au.mu.Unlock()
	r.transitioned = true
	return r.orchestratorRepo.TransitionStage(ctx, id, to, c)
}

// TestDecideScopeCompleteness_ExemptAuditPrecedesDispatchability pins the
// happens-before the zero-re-run path rests on (#2501): the
// scope_completeness_exempted entry — the emission gate's authorization proof
// (prompt.go::resolveHeldCommitExemption) — is in the chain BEFORE the stage
// leaves awaiting_scope_decision.
//
// Without that ordering, the transition + Orchestrator.Advance can start a
// runner whose prompt fetch races the append, sees only the older `parked`
// entry, omits the held-commit fields, and re-invokes the agent — the exact
// full re-run the park exists to avoid. Advance runs strictly after the
// transition, so pinning the transition pins Advance transitively.
func TestDecideScopeCompleteness_ExemptAuditPrecedesDispatchability(t *testing.T) {
	base := newOrchestratorRepo()
	runRow := base.seedRun()
	stage := base.seedStage(runRow.ID, 1, run.StageStateAwaitingScopeDecision)
	stage.Type = run.StageTypeImplement
	stage.ScopeCompletenessPark = &run.ScopeCompletenessPark{
		HeldCommitSHA:   "1111111111111111111111111111111111111111",
		RunBranch:       "fishhawk/run-aaa/slice-0",
		VerifiedTreeSHA: "2222222222222222222222222222222222222222",
		MissingPaths:    []string{"backend/internal/foo/foo_test.go"},
	}
	au := &auditCapture{}
	rr := &orchestratorRepoOrderProbe{orchestratorRepo: base, au: au}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})

	w := postScopeCompletenessDecision(t, s, runRow.ID,
		`{"decision":"exempt","reason":"the assertion literal was operator-authored and wrong"}`,
		operatorWriteStages)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if !rr.transitioned {
		t.Fatal("the exempt decision never transitioned the stage; the ordering probe observed nothing")
	}
	if rr.auditCountAtTransition != 1 {
		t.Fatalf("audit entries present when the stage became dispatchable = %d, want 1: "+
			"the scope_completeness_exempted entry MUST be in the chain before the stage leaves "+
			"awaiting_scope_decision, or a runner started by the follow-on Advance can fetch its "+
			"prompt, read the older `parked` entry, and re-invoke the agent", rr.auditCountAtTransition)
	}
	au.mu.Lock()
	defer au.mu.Unlock()
	if len(au.appended) != 1 || au.appended[0].Category != CategoryScopeCompletenessExempted {
		t.Fatalf("audit = %+v, want one scope_completeness_exempted entry", au.appended)
	}
}

// TestDecideScopeCompleteness_ExemptAuditAppendFailure_RefusesAndLeavesParked
// pins the fail-closed half of the same invariant (#2501): when the exempted
// entry cannot be written, the decision is REFUSED (500) and the stage stays
// parked — never moved to a dispatch-admissible state.
//
// A best-effort append here would return 200 and dispatch a stage the emission
// gate can never prove exempt: the runner would re-invoke the agent (the loss
// the park exists to avoid) and no authorization for the held commit would
// exist in the audit chain at all. Leaving the stage parked is recoverable — the
// operator simply re-POSTs the decision.
func TestDecideScopeCompleteness_ExemptAuditAppendFailure_RefusesAndLeavesParked(t *testing.T) {
	rr := newOrchestratorRepo()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 1, run.StageStateAwaitingScopeDecision)
	stage.Type = run.StageTypeImplement
	stage.ScopeCompletenessPark = &run.ScopeCompletenessPark{
		HeldCommitSHA:   "1111111111111111111111111111111111111111",
		RunBranch:       "fishhawk/run-aaa/slice-0",
		VerifiedTreeSHA: "2222222222222222222222222222222222222222",
		MissingPaths:    []string{"backend/internal/foo/foo_test.go"},
	}
	au := newAuditFake()
	au.appendErr = errors.New("audit chain write failed")
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})

	w := postScopeCompletenessDecision(t, s, runRow.ID,
		`{"decision":"exempt","reason":"the coupled test file is genuinely already covered"}`,
		operatorWriteStages)

	// COMMITTED STATE FIRST: a 500 that still moved the stage would dispatch an
	// exemption the gate can never prove, so the state — not the status code —
	// is the load-bearing assertion and is checked before it.
	got, _ := rr.GetStage(context.Background(), stage.ID)
	if got.State != run.StageStateAwaitingScopeDecision {
		t.Fatalf("stage state = %q, want it left parked in awaiting_scope_decision "+
			"(an exemption that could not be recorded must never become dispatchable)", got.State)
	}
	if dispatchAdmissible(got.State) {
		t.Errorf("an exemption that could not be recorded must NOT leave the stage dispatch-admissible, got %q", got.State)
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (an unrecordable exemption must be refused); body = %s",
			w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "exemption_unrecorded") {
		t.Errorf("body must name the exemption_unrecorded code: %s", w.Body.String())
	}
}

// TestScopeCompleteness_ParkToPromptEmission_EndToEnd is the BACKEND half of the
// #2501 cross-module seam, covered in ONE test (operator binding condition 1(b))
// rather than piecewise per layer: it posts the runner's exact scope_park bytes,
// persists the park through REAL Postgres (pgtest), decides `exempt` through the
// real decision handler, then fetches the prompt through the real signed
// /prompt endpoint and asserts BOTH that the emitted held-commit projection
// equals the cross-module golden fixture AND that the resolved stage state is
// dispatch-admissible (an inadmissible state emits fields no runner ever reads).
//
// The one hop this test cannot make is the runner's DECODE — the runner and the
// backend are separate Go modules and neither may import the other. That hop is
// pinned by the shared bytes: goldenExemptPromptJSON, duplicated verbatim in
// runner/cmd/fishhawk-runner/main_test.go, which decodes it into
// upload.FetchedPrompt. Any drift in what the backend emits breaks the RUNNER
// test, not merely a backend assertion.
func TestScopeCompleteness_ParkToPromptEmission_EndToEnd(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()

	runRepo := run.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	signingRepo := signing.NewPostgresRepository(pool)

	realRun, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	implStage, err := runRepo.CreateStage(ctx, run.CreateStageParams{
		RunID:        realRun.ID,
		Sequence:     0,
		Type:         run.StageTypeImplement,
		ExecutorKind: run.ExecutorAgent,
		ExecutorRef:  "claude-code",
	})
	if err != nil {
		t.Fatalf("create implement stage: %v", err)
	}
	// Walk the stage to `running` — the state the runner's ship report parks from.
	for _, to := range []run.StageState{run.StageStateDispatched, run.StageStateRunning} {
		if _, err := runRepo.TransitionStage(ctx, implStage.ID, to, nil); err != nil {
			t.Fatalf("transition implement stage to %s: %v", to, err)
		}
	}

	key, err := signingRepo.Issue(ctx, realRun.ID, signing.DefaultTTL)
	if err != nil {
		t.Fatalf("issue signing key: %v", err)
	}

	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      runRepo,
		AuditRepo:    auditRepo,
		SigningRepo:  signingRepo,
		ArtifactRepo: newFakeArtifactRepo(),
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	// (1) The runner's exact scope_park bytes for an ASSERTION-class park: the
	// field order and tags of upload.pullRequestScopeParkBody, with missing_paths
	// omitted (omitempty) exactly as the runner ships it. The held commit and
	// branch are the golden fixture's, so the projection asserted in (4) is
	// byte-comparable against the shared cross-module literal.
	parkBytes := []byte(`{"outcome":"scope_park","branch":"fishhawk/run-11112222/stage-99990000",` +
		`"head_sha":"1111111111111111111111111111111111111111",` +
		`"base_sha":"3333333333333333333333333333333333333333",` +
		`"verified_tree_sha":"2222222222222222222222222222222222222222",` +
		`"unsatisfied_assertions":[{"type":"file_contains","path":"backend/internal/run/run.go","literal":"UnsatisfiedAssertion"}]}`)
	w := shipPRRequest(t, s, realRun.ID, implStage.ID, key.PrivateKey, parkBytes, "")
	if w.Code != http.StatusOK {
		t.Fatalf("scope_park report status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	// (2) The park persisted through real Postgres, shortfall intact.
	parked, err := runRepo.GetStage(ctx, implStage.ID)
	if err != nil {
		t.Fatalf("GetStage after park: %v", err)
	}
	if parked.State != run.StageStateAwaitingScopeDecision {
		t.Fatalf("stage state after the park report = %q, want awaiting_scope_decision", parked.State)
	}
	if parked.ScopeCompletenessPark == nil ||
		len(parked.ScopeCompletenessPark.UnsatisfiedAssertions) != 1 ||
		parked.ScopeCompletenessPark.HeldCommitSHA != "1111111111111111111111111111111111111111" {
		t.Fatalf("persisted park = %+v, want the held commit + one unsatisfied assertion",
			parked.ScopeCompletenessPark)
	}

	// (3) The operator exempts it through the real decision handler.
	dw := postScopeCompletenessDecision(t, s, realRun.ID,
		`{"decision":"exempt","reason":"the operator-authored assertion literal was wrong, not the implement output"}`,
		operatorWriteStages)
	if dw.Code != http.StatusOK {
		t.Fatalf("decision status = %d, want 200; body = %s", dw.Code, dw.Body.String())
	}
	resolved, err := runRepo.GetStage(ctx, implStage.ID)
	if err != nil {
		t.Fatalf("GetStage after decision: %v", err)
	}
	if !dispatchAdmissible(resolved.State) {
		t.Fatalf("resolved stage state = %q, want a DISPATCH-ADMISSIBLE state — emitting the "+
			"held-commit fields on a stage no runner can be dispatched from delivers nothing", resolved.State)
	}

	// (4) The prompt the re-dispatched runner fetches carries exactly the golden
	// three-key projection.
	pw := promptRequest(t, s, realRun.ID, implStage.ID, key.PrivateKey, "")
	if pw.Code != http.StatusOK {
		t.Fatalf("prompt status = %d, want 200:\n%s", pw.Code, pw.Body.String())
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(pw.Body.Bytes(), &keys); err != nil {
		t.Fatalf("decode prompt body: %v", err)
	}
	var golden map[string]json.RawMessage
	if err := json.Unmarshal([]byte(goldenExemptPromptJSON), &golden); err != nil {
		t.Fatal(err)
	}
	projection := map[string]json.RawMessage{}
	for k := range golden {
		if raw, ok := keys[k]; ok {
			projection[k] = raw
		}
	}
	got, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != goldenExemptPromptJSON {
		t.Errorf("prompt emitted after a REAL park→exempt walk drifted from the cross-module golden fixture:\n got: %s\nwant: %s",
			got, goldenExemptPromptJSON)
	}
}
