package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
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
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: newScopeParkDeciderRepo(rr, au), AuditRepo: au})
	return s, rr, au, runRow, stage
}

// scopeParkDeciderRepo gives the orchestratorRepo fake the run.ScopeParkDecider
// capability the exempt and fail arms hard-require. It is all-or-nothing like the
// postgres verbs: state and attempt are compared and the entry appended before the
// stage moves, all under the fake's lock, so an append error leaves the stage
// parked. Embedding keeps every other method and optional capability of the fake.
type scopeParkDeciderRepo struct {
	*orchestratorRepo
	au audit.Repository
	// deciderErr, when set, makes both verbs fail without touching anything.
	deciderErr error
	// loseDecision, when set, makes both verbs report a lost decision without
	// touching anything, whatever the stage's state.
	loseDecision bool
}

func newScopeParkDeciderRepo(rr *orchestratorRepo, au audit.Repository) *scopeParkDeciderRepo {
	return &scopeParkDeciderRepo{orchestratorRepo: rr, au: au}
}

func (r *scopeParkDeciderRepo) ExemptScopeParkAndAppend(ctx context.Context, stageID uuid.UUID, expectedAttempt string, p audit.ChainAppendParams) (*run.Stage, bool, error) {
	return r.decide(ctx, stageID, expectedAttempt, run.StageStatePending, nil, p)
}

func (r *scopeParkDeciderRepo) FailScopeParkAndAppend(ctx context.Context, stageID uuid.UUID, expectedAttempt, reason string, p audit.ChainAppendParams) (*run.Stage, bool, error) {
	category := run.FailureB
	return r.decide(ctx, stageID, expectedAttempt, run.StageStateFailed,
		&run.StageCompletion{FailureCategory: &category, FailureReason: &reason}, p)
}

func (r *scopeParkDeciderRepo) decide(ctx context.Context, stageID uuid.UUID, expectedAttempt string, to run.StageState, c *run.StageCompletion, p audit.ChainAppendParams) (*run.Stage, bool, error) {
	if r.deciderErr != nil {
		return nil, false, r.deciderErr
	}
	if r.loseDecision {
		return nil, false, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.stagesByID[stageID]
	if !ok {
		return nil, false, run.ErrNotFound
	}
	if st.State != run.StageStateAwaitingScopeDecision {
		return nil, false, nil
	}
	if run.StageAttemptToken(st.DispatchedAt) != expectedAttempt {
		return nil, false, nil
	}
	if _, err := r.au.AppendChained(ctx, p); err != nil {
		return nil, false, err
	}
	st.State = to
	if c != nil {
		st.FailureCategory = c.FailureCategory
		st.FailureReason = c.FailureReason
	}
	return st, true, nil
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
// validation: only exempt|amend|fail with a non-empty reason is accepted.
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

// TestDecideScopeCompleteness_BadDecisionMessageNamesEveryDecision pins the
// enum error TEXT, not just its status (#2591). The message IS the operator's
// discovery surface for the decision set: one that still reads
// `must be "exempt" or "fail"` would tell an operator holding a build-required
// park — the class whose exempt is refused outright — that their only remaining
// move is to fail the pass, which is exactly the dead end amend closes.
func TestDecideScopeCompleteness_BadDecisionMessageNamesEveryDecision(t *testing.T) {
	s, _, _, runRow, _ := scopeCompletenessServer(t)
	w := postScopeCompletenessDecision(t, s, runRow.ID, `{"decision":"maybe","reason":"r"}`, operatorWriteStages)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
	for _, want := range []string{"exempt", "amend", "fail"} {
		if !strings.Contains(w.Body.String(), `\"`+want+`\"`) {
			t.Errorf("the enum error must name %q as an admissible decision: %s", want, w.Body.String())
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

// TestDecideScopeCompleteness_ExemptAuditAppendFailure_RefusesAndLeavesParked
// pins the fail-closed exempt arm (#2501, #2559): when the exempted entry cannot
// be written, the decision is REFUSED (500) and the stage stays parked — never
// moved to a dispatch-admissible state.
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
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: newScopeParkDeciderRepo(rr, au), AuditRepo: au})

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

// scopeParkE2E is an implement stage parked in awaiting_scope_decision through
// the runner's real scope_park report: real Postgres (pgtest), the real ship
// handler, and a run signing key so the test can fetch the real signed /prompt.
type scopeParkE2E struct {
	pool      *pgxpool.Pool
	s         *Server
	runRepo   run.Repository
	auditRepo audit.Repository
	runID     uuid.UUID
	stageID   uuid.UUID
	priv      ed25519.PrivateKey
}

func newScopeParkE2E(t *testing.T) *scopeParkE2E {
	t.Helper()
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

	w := shipPRRequest(t, s, realRun.ID, implStage.ID, key.PrivateKey,
		scopeParkReport("1111111111111111111111111111111111111111"), "")
	if w.Code != http.StatusOK {
		t.Fatalf("scope_park report status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	return &scopeParkE2E{
		pool: pool, s: s, runRepo: runRepo, auditRepo: auditRepo,
		runID: realRun.ID, stageID: implStage.ID, priv: key.PrivateKey,
	}
}

// scopeParkReport is the runner's exact scope_park bytes for an ASSERTION-class
// park: the field order and tags of upload.pullRequestScopeParkBody, with
// missing_paths omitted (omitempty) exactly as the runner ships it. With the golden
// fixture's held commit, an emitted projection is byte-comparable against the
// shared cross-module literal.
func scopeParkReport(heldCommitSHA string) []byte {
	return []byte(`{"outcome":"scope_park","branch":"fishhawk/run-11112222/stage-99990000",` +
		`"head_sha":"` + heldCommitSHA + `",` +
		`"base_sha":"3333333333333333333333333333333333333333",` +
		`"verified_tree_sha":"2222222222222222222222222222222222222222",` +
		`"unsatisfied_assertions":[{"type":"file_contains","path":"backend/internal/run/run.go","literal":"UnsatisfiedAssertion"}]}`)
}

func (e *scopeParkE2E) stage(t *testing.T) *run.Stage {
	t.Helper()
	st, err := e.runRepo.GetStage(context.Background(), e.stageID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	return st
}

func (e *scopeParkE2E) entries(t *testing.T, category string) []*audit.Entry {
	t.Helper()
	all, err := e.auditRepo.ListForRunByCategory(context.Background(), e.runID, category)
	if err != nil {
		t.Fatalf("ListForRunByCategory(%s): %v", category, err)
	}
	return all
}

func (e *scopeParkE2E) promptKeys(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	return exemptBodyKeys(t, promptRequest(t, e.s, e.runID, e.stageID, e.priv, ""), "/prompt")
}

// staleDecisionRequest is the request a decision arm receives directly, with a
// stage row loaded before another decision committed: the deterministic form of
// two in-flight decisions that both passed resolveParkedScopeStage's read.
func staleDecisionRequest(e *scopeParkE2E) *http.Request {
	req := httptest.NewRequest(http.MethodPost,
		"/v0/runs/"+e.runID.String()+"/scope-completeness/decision", nil)
	return operatorWriteStages(req)
}

// refuseCommitOfPendingStages installs a test-only DEFERRED constraint trigger:
// any transaction moving a stage to `pending` fails at COMMIT, after every
// statement in it succeeded. pgtest gives each test its own database, so the
// trigger is isolated to this test. The returned func drops it.
func refuseCommitOfPendingStages(t *testing.T, pool *pgxpool.Pool) (drop func()) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
CREATE FUNCTION fishhawk_test_refuse_pending() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	IF NEW.state = 'pending' THEN
		RAISE EXCEPTION 'test-only: refusing to commit a stage moved to pending';
	END IF;
	RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER fishhawk_test_refuse_pending AFTER UPDATE ON stages
	DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
	EXECUTE FUNCTION fishhawk_test_refuse_pending();`); err != nil {
		t.Fatalf("install deferred trigger: %v", err)
	}
	return func() {
		t.Helper()
		if _, err := pool.Exec(ctx, `DROP TRIGGER fishhawk_test_refuse_pending ON stages`); err != nil {
			t.Fatalf("drop deferred trigger: %v", err)
		}
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
// pinned by the SINGLE shared fixture testdata/wire/exempt_prompt_fields.json,
// read here via goldenExemptPromptJSON(t) and read verbatim by
// runner/cmd/fishhawk-runner/main_test.go, which decodes it into
// upload.FetchedPrompt. One source of truth (#2558): any drift in what the
// backend emits forces a fixture edit, which then breaks the RUNNER decode test,
// not merely a backend assertion. The struct-level tag parity is additionally
// pinned by backend/internal/wirecontract's TestCrossModuleWireParity.
func TestScopeCompleteness_ParkToPromptEmission_EndToEnd(t *testing.T) {
	e := newScopeParkE2E(t)
	ctx := context.Background()
	s, runRepo := e.s, e.runRepo

	// (2) The park persisted through real Postgres, shortfall intact.
	parked, err := runRepo.GetStage(ctx, e.stageID)
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
	// #2563: the base SHA the runner reported must persist on the park (the
	// value the exempt resume ships as the artifact's base_sha), proven through
	// the real JSONB write/read — this is what makes every park recorded from
	// now on resumable without the audit fallback.
	if got := parked.ScopeCompletenessPark.BaseSHA; got != "3333333333333333333333333333333333333333" {
		t.Errorf("persisted park base_sha = %q, want the reported base SHA", got)
	}

	// (3) The operator exempts it through the real decision handler.
	dw := postScopeCompletenessDecision(t, s, e.runID,
		`{"decision":"exempt","reason":"the operator-authored assertion literal was wrong, not the implement output"}`,
		operatorWriteStages)
	if dw.Code != http.StatusOK {
		t.Fatalf("decision status = %d, want 200; body = %s", dw.Code, dw.Body.String())
	}
	resolved, err := runRepo.GetStage(ctx, e.stageID)
	if err != nil {
		t.Fatalf("GetStage after decision: %v", err)
	}
	if !dispatchAdmissible(resolved.State) {
		t.Fatalf("resolved stage state = %q, want a DISPATCH-ADMISSIBLE state — emitting the "+
			"held-commit fields on a stage no runner can be dispatched from delivers nothing", resolved.State)
	}

	// (4) The prompt the re-dispatched runner fetches carries exactly the golden
	// three-key projection.
	pw := promptRequest(t, s, e.runID, e.stageID, e.priv, "")
	if pw.Code != http.StatusOK {
		t.Fatalf("prompt status = %d, want 200:\n%s", pw.Code, pw.Body.String())
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(pw.Body.Bytes(), &keys); err != nil {
		t.Fatalf("decode prompt body: %v", err)
	}
	golden := goldenExemptPromptJSON(t)
	var goldenKeys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(golden), &goldenKeys); err != nil {
		t.Fatal(err)
	}
	projection := map[string]json.RawMessage{}
	for k := range goldenKeys {
		if raw, ok := keys[k]; ok {
			projection[k] = raw
		}
	}
	got, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != golden {
		t.Errorf("prompt emitted after a REAL park→exempt walk drifted from the cross-module golden fixture:\n got: %s\nwant: %s",
			got, golden)
	}
}

// TestScopeCompleteness_StaleExemptAfterFail_NeverEmits reproduces the
// fail-then-exempt interleaving #2559's note called unreachable: the 409 guard
// is a read ahead of the arms, so two in-flight decisions can both pass it. The
// exempt that loses must leave no `exempted` entry, because an override retry
// keeps the park row and a newest `exempted` would then open a PR from a commit
// the operator rejected. Committed state and emission are asserted BEFORE the
// status code: the status alone cannot show what the chain now authorizes.
func TestScopeCompleteness_StaleExemptAfterFail_NeverEmits(t *testing.T) {
	e := newScopeParkE2E(t)
	ctx := context.Background()

	stale := e.stage(t) // the row both in-flight decisions resolved

	fw := postScopeCompletenessDecision(t, e.s, e.runID,
		`{"decision":"fail","reason":"the shortfall is load-bearing"}`, operatorWriteStages)
	if fw.Code != http.StatusOK {
		t.Fatalf("fail status = %d, want 200; body = %s", fw.Code, fw.Body.String())
	}

	w := httptest.NewRecorder()
	e.s.exemptScopeCompleteness(w, staleDecisionRequest(e), e.runID, stale, "a stale exempt", "operator")

	if got := e.stage(t).State; got != run.StageStateFailed {
		t.Errorf("stage state = %q, want it still failed", got)
	}
	if rows := e.entries(t, CategoryScopeCompletenessExempted); len(rows) != 0 {
		t.Errorf("%d scope_completeness_exempted rows, want 0: the losing decision must record nothing", len(rows))
	}

	if _, err := run.RetryStage(ctx, e.runRepo, e.stageID, run.RetryOptions{OverrideB: true}); err != nil {
		t.Fatalf("override retry: %v", err)
	}
	for _, to := range []run.StageState{run.StageStateDispatched, run.StageStateRunning} {
		if _, err := e.runRepo.TransitionStage(ctx, e.stageID, to, nil); err != nil {
			t.Fatalf("transition to %s: %v", to, err)
		}
	}
	assertNoExemptKeys(t, e.promptKeys(t), "an override retry of a FAIL-decided park")

	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "stage_not_parked") {
		t.Errorf("stale exempt response = %d %s, want 409 stage_not_parked", w.Code, w.Body.String())
	}
}

// TestScopeCompleteness_StaleFailAfterExempt_Refused is the mirror interleaving:
// a fail that loses to a committed exempt must not fail the resumed stage nor
// demote the exemption with a `failed` entry.
func TestScopeCompleteness_StaleFailAfterExempt_Refused(t *testing.T) {
	e := newScopeParkE2E(t)

	stale := e.stage(t)

	ew := postScopeCompletenessDecision(t, e.s, e.runID,
		`{"decision":"exempt","reason":"the assertion literal was wrong"}`, operatorWriteStages)
	if ew.Code != http.StatusOK {
		t.Fatalf("exempt status = %d, want 200; body = %s", ew.Code, ew.Body.String())
	}

	w := httptest.NewRecorder()
	e.s.failScopeCompleteness(w, staleDecisionRequest(e), e.runID, stale, "a stale fail", "operator")

	if got := e.stage(t).State; got != run.StageStatePending {
		t.Errorf("stage state = %q, want it still pending", got)
	}
	if rows := e.entries(t, CategoryScopeCompletenessFailed); len(rows) != 0 {
		t.Errorf("%d scope_completeness_failed rows, want 0: the losing decision must record nothing", len(rows))
	}
	assertGoldenExemptProjection(t, e.promptKeys(t), "/prompt after a losing fail")

	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "stage_not_parked") {
		t.Errorf("stale fail response = %d %s, want 409 stage_not_parked", w.Code, w.Body.String())
	}
}

// TestScopeCompleteness_TransitionFailureAfterAppend_LeavesNoExemption is the
// issue's first scenario: the stage move cannot commit after the entry was
// written. The fixture's trigger raises at COMMIT, so every statement of the
// decision succeeds first; an exempted entry in a separate transaction would
// survive it and stand as authorization for a stage that never left the park.
func TestScopeCompleteness_TransitionFailureAfterAppend_LeavesNoExemption(t *testing.T) {
	e := newScopeParkE2E(t)
	drop := refuseCommitOfPendingStages(t, e.pool)

	const body = `{"decision":"exempt","reason":"the assertion literal was wrong"}`
	w := postScopeCompletenessDecision(t, e.s, e.runID, body, operatorWriteStages)

	if got := e.stage(t).State; got != run.StageStateAwaitingScopeDecision {
		t.Errorf("stage state = %q, want it left parked", got)
	}
	if rows := e.entries(t, CategoryScopeCompletenessExempted); len(rows) != 0 {
		t.Errorf("%d scope_completeness_exempted rows, want 0: the entry must not outlive its failed transition", len(rows))
	}
	assertNoExemptKeys(t, e.promptKeys(t), "a parked stage whose exemption failed to commit")
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "exemption_unrecorded") {
		t.Errorf("exempt response = %d %s, want 500 exemption_unrecorded", w.Code, w.Body.String())
	}

	// Recovery: the operator re-POSTs after the fault clears.
	drop()
	rw := postScopeCompletenessDecision(t, e.s, e.runID, body, operatorWriteStages)
	if rw.Code != http.StatusOK {
		t.Fatalf("re-POST status = %d, want 200; body = %s", rw.Code, rw.Body.String())
	}
	if got := e.stage(t).State; got != run.StageStatePending {
		t.Errorf("stage state after re-POST = %q, want pending", got)
	}
	if rows := e.entries(t, CategoryScopeCompletenessExempted); len(rows) != 1 {
		t.Errorf("%d scope_completeness_exempted rows after re-POST, want exactly 1", len(rows))
	}
	assertGoldenExemptProjection(t, e.promptKeys(t), "/prompt after the re-POST")
}

// TestScopeCompleteness_StaleDecisionAcrossReDispatch_Refused pins the attempt
// token the handler hands the store. A decision resolved against the first park,
// stalled through an exempt, a re-dispatch and a re-park, sees the same parked
// state on the row; only the attempt tells them apart. The stale row's token must
// reach the compare-and-swap, or the decision would authorize the NEWER held commit
// no operator looked at.
func TestScopeCompleteness_StaleDecisionAcrossReDispatch_Refused(t *testing.T) {
	const newerHeldCommit = "4444444444444444444444444444444444444444"
	for _, tc := range []struct {
		name     string
		decide   func(*Server, http.ResponseWriter, *http.Request, uuid.UUID, *run.Stage, string, string)
		category string
		wantRows int
	}{
		{"exempt", (*Server).exemptScopeCompleteness, CategoryScopeCompletenessExempted, 1},
		{"fail", (*Server).failScopeCompleteness, CategoryScopeCompletenessFailed, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newScopeParkE2E(t)
			ctx := context.Background()

			stale := e.stage(t)

			ew := postScopeCompletenessDecision(t, e.s, e.runID,
				`{"decision":"exempt","reason":"the assertion literal was wrong"}`, operatorWriteStages)
			if ew.Code != http.StatusOK {
				t.Fatalf("exempt status = %d, want 200; body = %s", ew.Code, ew.Body.String())
			}
			for _, to := range []run.StageState{run.StageStateDispatched, run.StageStateRunning} {
				if _, err := e.runRepo.TransitionStage(ctx, e.stageID, to, nil); err != nil {
					t.Fatalf("transition to %s: %v", to, err)
				}
			}
			if pw := shipPRRequest(t, e.s, e.runID, e.stageID, e.priv, scopeParkReport(newerHeldCommit), ""); pw.Code != http.StatusOK {
				t.Fatalf("re-park status = %d, want 200:\n%s", pw.Code, pw.Body.String())
			}
			reparked := e.stage(t)
			if reparked.State != run.StageStateAwaitingScopeDecision || reparked.ScopeCompletenessPark.HeldCommitSHA != newerHeldCommit {
				t.Fatalf("fixture: stage = %q holding %q, want re-parked on the newer commit",
					reparked.State, reparked.ScopeCompletenessPark.HeldCommitSHA)
			}

			w := httptest.NewRecorder()
			tc.decide(e.s, w, staleDecisionRequest(e), e.runID, stale, "a stale decision", "operator")

			if got := e.stage(t).State; got != run.StageStateAwaitingScopeDecision {
				t.Errorf("stage state = %q, want the newer park left undecided", got)
			}
			if rows := e.entries(t, tc.category); len(rows) != tc.wantRows {
				t.Errorf("%d %s rows, want %d: the stale decision must record nothing", len(rows), tc.category, tc.wantRows)
			}
			assertNoExemptKeys(t, e.promptKeys(t), "a re-parked stage a stale decision tried to decide")
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "stage_not_parked") {
				t.Errorf("stale decision response = %d %s, want 409 stage_not_parked", w.Code, w.Body.String())
			}
		})
	}
}

// exemptAndFailDecisions is the two arms that go through run.ScopeParkDecider.
var exemptAndFailDecisions = []struct{ name, body, unrecordedCode string }{
	{"exempt", `{"decision":"exempt","reason":"the coupled test file is genuinely already covered"}`, "exemption_unrecorded"},
	{"fail", `{"decision":"fail","reason":"the shortfall is load-bearing"}`, "fail_unrecorded"},
}

// assertRefusalOnly pins that a refused decision stops at its refusal. The status
// alone cannot show this, because the first WriteHeader wins: an arm that fell
// through would run Advance and append the success body, whose `decision` key
// no refusal carries.
func assertRefusalOnly(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if strings.Contains(w.Body.String(), `"decision"`) {
		t.Errorf("response body = %s, want the refusal alone", w.Body.String())
	}
}

// TestDecideScopeCompleteness_LostRace pins the handler's refusal of a lost
// decision. The decider reports won=false WITHOUT moving the stage, so the handler
// is the only thing between that result and a 200 claiming a decision landed.
func TestDecideScopeCompleteness_LostRace(t *testing.T) {
	for _, tc := range exemptAndFailDecisions {
		t.Run(tc.name, func(t *testing.T) {
			s, rr, au, runRow, stage := scopeCompletenessServer(t)
			s.cfg.RunRepo.(*scopeParkDeciderRepo).loseDecision = true

			w := postScopeCompletenessDecision(t, s, runRow.ID, tc.body, operatorWriteStages)

			if got, _ := rr.GetStage(context.Background(), stage.ID); got.State != run.StageStateAwaitingScopeDecision {
				t.Errorf("stage state = %q, want it untouched", got.State)
			}
			au.mu.Lock()
			defer au.mu.Unlock()
			if len(au.appended) != 0 {
				t.Errorf("audit = %+v, want no row from a lost decision", au.appended)
			}
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "stage_not_parked") {
				t.Errorf("response = %d %s, want 409 stage_not_parked", w.Code, w.Body.String())
			}
			assertRefusalOnly(t, w)
		})
	}
}

// TestDecideScopeCompleteness_DeciderError_Refuses pins the refusal of a decision
// that could not commit: a 500 naming the arm's code, with the stage left parked so
// the operator can re-POST.
func TestDecideScopeCompleteness_DeciderError_Refuses(t *testing.T) {
	for _, tc := range exemptAndFailDecisions {
		t.Run(tc.name, func(t *testing.T) {
			s, rr, au, runRow, stage := scopeCompletenessServer(t)
			s.cfg.RunRepo.(*scopeParkDeciderRepo).deciderErr = errors.New("commit failed")

			w := postScopeCompletenessDecision(t, s, runRow.ID, tc.body, operatorWriteStages)

			if got, _ := rr.GetStage(context.Background(), stage.ID); got.State != run.StageStateAwaitingScopeDecision {
				t.Errorf("stage state = %q, want it left parked", got.State)
			}
			au.mu.Lock()
			defer au.mu.Unlock()
			if len(au.appended) != 0 {
				t.Errorf("audit = %+v, want no row from a failed commit", au.appended)
			}
			if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), tc.unrecordedCode) {
				t.Errorf("response = %d %s, want 500 %s", w.Code, w.Body.String(), tc.unrecordedCode)
			}
			assertRefusalOnly(t, w)
		})
	}
}

// TestDecideScopeCompleteness_FailAuditAppendFailure_RefusesAndLeavesParked pins
// the #2559 behavior change on the fail arm: its entry used to be best-effort, but
// it is what demotes an earlier `exempted` in the gate's newest-wins walk, so a
// fail the chain cannot record no longer fails the stage.
func TestDecideScopeCompleteness_FailAuditAppendFailure_RefusesAndLeavesParked(t *testing.T) {
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
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: newScopeParkDeciderRepo(rr, au), AuditRepo: au})

	w := postScopeCompletenessDecision(t, s, runRow.ID,
		`{"decision":"fail","reason":"the shortfall is load-bearing"}`, operatorWriteStages)

	got, _ := rr.GetStage(context.Background(), stage.ID)
	if got.State != run.StageStateAwaitingScopeDecision {
		t.Errorf("stage state = %q, want it left parked", got.State)
	}
	if got.FailureCategory != nil {
		t.Errorf("failure category = %v, want nil on a refused decision", *got.FailureCategory)
	}
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "fail_unrecorded") {
		t.Errorf("response = %d %s, want 500 fail_unrecorded", w.Code, w.Body.String())
	}
}

// TestDecideScopeCompleteness_DeciderCapabilityAbsent_Refuses pins the hard
// requirement. A repo without the capability (a future decorator around RunRepo,
// say) must refuse rather than fall back to two separate writes. This fixture is
// deliberately the BARE orchestratorRepo, not scopeCompletenessServer's wrapper.
func TestDecideScopeCompleteness_DeciderCapabilityAbsent_Refuses(t *testing.T) {
	for _, tc := range exemptAndFailDecisions {
		t.Run(tc.name, func(t *testing.T) {
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

			w := postScopeCompletenessDecision(t, s, runRow.ID, tc.body, operatorWriteStages)

			if got, _ := rr.GetStage(context.Background(), stage.ID); got.State != run.StageStateAwaitingScopeDecision {
				t.Errorf("stage state = %q, want it left parked", got.State)
			}
			au.mu.Lock()
			defer au.mu.Unlock()
			if len(au.appended) != 0 {
				t.Errorf("audit = %+v, want no row without the capability", au.appended)
			}
			if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "scope_completeness_unconfigured") {
				t.Errorf("response = %d %s, want 503 scope_completeness_unconfigured", w.Code, w.Body.String())
			}
			assertRefusalOnly(t, w)
		})
	}
}

// TestDecideScopeCompleteness_ExemptRefusedForBuildRequiredPark is the #2548
// EXEMPT-REFUSAL counterfactual (c1). The control's effect is COMMITTED STATE,
// not error identity — a refusal that fired and was then rolled back would
// return a byte-identical error — so this reads the state AFTER the call
// returns: the stage must STILL be parked in awaiting_scope_decision and NO
// scope_completeness_exempted entry may exist. Deleting the refusal transitions
// the stage to pending and appends the entry, so both assertions go red.
//
// The park fixture is seeded BY CONSTRUCTION (a park struct written with
// non-empty BuildRequiredPaths), never by invoking the refusal in setup, so the
// RED lands on the behavioral assertions and not on a fixture-setup failure.
func TestDecideScopeCompleteness_ExemptRefusedForBuildRequiredPark(t *testing.T) {
	s, rr, au, runRow, stage := scopeCompletenessServer(t)
	sliceOne := 1
	stage.ScopeCompletenessPark = &run.ScopeCompletenessPark{
		HeldCommitSHA:      "1111111111111111111111111111111111111111",
		RunBranch:          "fishhawk/run-aaa/slice-0",
		VerifiedTreeSHA:    "2222222222222222222222222222222222222222",
		BaseSHA:            "3333333333333333333333333333333333333333",
		BuildRequiredPaths: []string{"backend/internal/server/prompt.go"},
		OwningSlices: []run.BuildRequiredOwner{
			{Path: "backend/internal/server/prompt.go", SliceIndex: &sliceOne, SliceTitle: "prompt plumbing"},
		},
	}

	w := postScopeCompletenessDecision(t, s, runRow.ID,
		`{"decision":"exempt","reason":"ship it anyway"}`,
		operatorWriteStages)
	// Deliberately NON-fatal: the load-bearing assertions are the COMMITTED-STATE
	// ones below, and a t.Fatalf here would stop the counterfactual short of them.
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "exempt_refused_build_required") {
		t.Errorf("body must carry the exempt_refused_build_required code: %s", w.Body.String())
	}

	// COMMITTED STATE, read after the call returns — this is what goes red when
	// the refusal is deleted.
	got, err := rr.GetStage(context.Background(), stage.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != run.StageStateAwaitingScopeDecision {
		t.Errorf("stage state = %q, want it STILL parked in awaiting_scope_decision — the refusal must not resume a red-tree park", got.State)
	}
	au.mu.Lock()
	defer au.mu.Unlock()
	for _, e := range au.appended {
		if e.Category == CategoryScopeCompletenessExempted {
			t.Fatalf("no scope_completeness_exempted entry may be appended for a build-required park; got %+v", au.appended)
		}
	}
}

// TestDecideScopeCompleteness_FailBuildRequiredParkEchoesAttribution pins the
// ADMISSIBLE decision for the class: `fail` still works, lands the stage failed,
// and echoes the class + its sibling-slice attribution so the operator's client
// already names the boundary to correct before re-running.
func TestDecideScopeCompleteness_FailBuildRequiredParkEchoesAttribution(t *testing.T) {
	s, rr, au, runRow, stage := scopeCompletenessServer(t)
	sliceOne := 1
	stage.ScopeCompletenessPark = &run.ScopeCompletenessPark{
		HeldCommitSHA:      "1111111111111111111111111111111111111111",
		RunBranch:          "fishhawk/run-aaa/slice-0",
		VerifiedTreeSHA:    "2222222222222222222222222222222222222222",
		BuildRequiredPaths: []string{"backend/internal/server/prompt.go"},
		OwningSlices: []run.BuildRequiredOwner{
			{Path: "backend/internal/server/prompt.go", SliceIndex: &sliceOne, SliceTitle: "prompt plumbing"},
		},
	}

	w := postScopeCompletenessDecision(t, s, runRow.ID,
		`{"decision":"fail","reason":"the decomposition boundary cut a coupling; re-plan the slices"}`,
		operatorWriteStages)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var resp scopeCompletenessDecisionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.BuildRequiredPaths) != 1 || resp.BuildRequiredPaths[0] != "backend/internal/server/prompt.go" {
		t.Errorf("response must echo build_required_paths, got %v", resp.BuildRequiredPaths)
	}
	if len(resp.OwningSlices) != 1 || resp.OwningSlices[0].SliceIndex == nil || *resp.OwningSlices[0].SliceIndex != 1 {
		t.Errorf("response must echo the owning-slice attribution, got %+v", resp.OwningSlices)
	}
	got, _ := rr.GetStage(context.Background(), stage.ID)
	if got.State != run.StageStateFailed {
		t.Errorf("stage state = %q, want failed", got.State)
	}
	au.mu.Lock()
	defer au.mu.Unlock()
	var found bool
	for _, e := range au.appended {
		if e.Category != CategoryScopeCompletenessFailed {
			continue
		}
		found = true
		// The VALUES, not merely the keys — the failed entry is the operator's
		// permanent record of which boundary was cut, and a presence-only check
		// would stay green on a wrong non-empty path or a dropped slice identity.
		assertBuildRequiredAuditAttribution(t, e.Payload,
			"backend/internal/server/prompt.go", 1, "prompt plumbing")
	}
	if !found {
		t.Fatalf("want a scope_completeness_failed entry, got %+v", au.appended)
	}
}

// TestDecideScopeCompleteness_FailBuildRequiredParkDrivesParentResolution is
// p4 delivered END TO END, in one test, through the real seam — the earlier
// split into "server POSTs and asserts the stage failed" plus "orchestrator
// hand-mutates the child and calls maybeAdvanceDecomposedParent" asserted
// everything EXCEPT the join, so the one defect that matters (the fail decision
// transitions the STAGE but never drives the child RUN terminal, leaving the
// parent waiting forever) would have passed both halves.
//
// So: a REAL POST of the `fail` decision on a parked decomposition child, with
// a REAL *orchestrator.Orchestrator wired into the server, and the assertions
// walk the whole chain:
//
//  1. the child's implement stage lands failed (category B);
//  2. the child RUN reaches TERMINAL — the seam. failScopeCompleteness's
//     Advance must run completeRun, which is the only thing that calls
//     maybeAdvanceDecomposedParent;
//  3. the parent's awaiting_children stage is RESOLVED rather than left
//     silently waiting: with every sibling now terminal the parent-side hook
//     fires and, because a `fail` decision is category B (recoverable in
//     decomposition, #698/#1081), it emits parent_awaiting_redrive naming the
//     failed child. That entry IS the resolution for this failure class — the
//     operator-discoverable re-drive signal — and it exists only if (2) held.
func TestDecideScopeCompleteness_FailBuildRequiredParkDrivesParentResolution(t *testing.T) {
	rr := newOrchestratorRepo()
	au := &auditCapture{}

	// Parent: implement parked in awaiting_children.
	parent := rr.seedRun()
	parentStage := rr.seedStage(parent.ID, 1, run.StageStateAwaitingChildren)
	parentStage.Type = run.StageTypeImplement

	// Child slice 0: settled-but-not-terminal, parked on a build-required park.
	child := rr.seedRun()
	pid := parent.ID
	sliceZero := 0
	child.DecomposedFrom = &pid
	child.SliceIndex = &sliceZero
	child.State = run.StateRunning
	childStage := rr.seedStage(child.ID, 1, run.StageStateAwaitingScopeDecision)
	childStage.Type = run.StageTypeImplement
	sliceOne := 1
	childStage.ScopeCompletenessPark = &run.ScopeCompletenessPark{
		HeldCommitSHA:      "1111111111111111111111111111111111111111",
		RunBranch:          "fishhawk/run-aaa/slice-0",
		VerifiedTreeSHA:    "2222222222222222222222222222222222222222",
		BuildRequiredPaths: []string{"backend/internal/server/prompt.go"},
		OwningSlices: []run.BuildRequiredOwner{
			{Path: "backend/internal/server/prompt.go", SliceIndex: &sliceOne, SliceTitle: "prompt plumbing"},
		},
	}

	// Sibling slice 1 already terminal, so the parked child is the LAST
	// non-terminal sibling — the shape in which an unresolved parent waits
	// indefinitely.
	sibling := rr.seedRun()
	sibIdx := 1
	sibling.DecomposedFrom = &pid
	sibling.SliceIndex = &sibIdx
	sibling.State = run.StateSucceeded

	o := &orchestrator.Orchestrator{Runs: rr, Audit: au}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: newScopeParkDeciderRepo(rr, au), AuditRepo: au, Orchestrator: o})

	w := postScopeCompletenessDecision(t, s, child.ID,
		`{"decision":"fail","reason":"the decomposition boundary cut a coupling; re-plan the slices"}`,
		operatorWriteStages)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	// (1) the child's stage failed.
	gotStage, _ := rr.GetStage(context.Background(), childStage.ID)
	if gotStage.State != run.StageStateFailed {
		t.Fatalf("child implement stage = %q, want failed", gotStage.State)
	}

	// (2) THE SEAM: the child RUN went terminal off the back of that decision.
	gotChild, _ := rr.GetRun(context.Background(), child.ID)
	if !gotChild.State.IsTerminal() {
		t.Fatalf("child run state = %q, want terminal — the fail decision must drive the child run terminal, "+
			"otherwise the parent never leaves awaiting_children", gotChild.State)
	}
	if gotChild.State != run.StateFailed {
		t.Errorf("child run state = %q, want failed", gotChild.State)
	}

	// (3) the parent-side hook fired and resolved the wait.
	au.mu.Lock()
	defer au.mu.Unlock()
	var redrive *audit.ChainAppendParams
	for i := range au.appended {
		if au.appended[i].Category == "parent_awaiting_redrive" && au.appended[i].RunID == parent.ID {
			redrive = &au.appended[i]
		}
	}
	if redrive == nil {
		t.Fatalf("the parent must be resolved off its silent wait once the decision lands; "+
			"want a parent_awaiting_redrive entry on run %s, got %+v", parent.ID, au.appended)
	}
	var payload struct {
		ParentStageID string   `json:"parent_stage_id"`
		Children      []string `json:"retryable_child_run_ids"`
	}
	if err := json.Unmarshal(redrive.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ParentStageID != parentStage.ID.String() {
		t.Errorf("parent_stage_id = %q, want the awaiting_children stage %s", payload.ParentStageID, parentStage.ID)
	}
	if len(payload.Children) != 1 || payload.Children[0] != child.ID.String() {
		t.Errorf("retryable_child_run_ids = %v, want [%s]", payload.Children, child.ID)
	}
}

// TestScopeCompletenessDecisionAudit_OlderClassesPayloadUnchanged is the
// byte-identity pin for the two PRE-#2548 park classes: a missing-paths park's
// decision entry must NOT grow build_required_paths / owning_slices keys.
// Writing them unconditionally emitted two JSON nulls onto every older-class
// payload — invisible to a key-presence-tolerant reader, wire-visible to a
// consumer diffing bytes. Asserted on both the exempted and failed entries.
func TestScopeCompletenessDecisionAudit_OlderClassesPayloadUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name, body, category string
	}{
		{"exempt", `{"decision":"exempt","reason":"the coupled test file is genuinely already covered"}`, CategoryScopeCompletenessExempted},
		{"fail", `{"decision":"fail","reason":"the declared edit was dropped"}`, CategoryScopeCompletenessFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, au, runRow, _ := scopeCompletenessServer(t) // park carries MissingPaths only
			w := postScopeCompletenessDecision(t, s, runRow.ID, tc.body, operatorWriteStages)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
			au.mu.Lock()
			defer au.mu.Unlock()
			var found bool
			for _, e := range au.appended {
				if e.Category != tc.category {
					continue
				}
				found = true
				var payload map[string]any
				if err := json.Unmarshal(e.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				if _, ok := payload["build_required_paths"]; ok {
					t.Errorf("%s payload must not carry build_required_paths for a missing-paths park; got %v", tc.category, payload)
				}
				if _, ok := payload["owning_slices"]; ok {
					t.Errorf("%s payload must not carry owning_slices for a missing-paths park; got %v", tc.category, payload)
				}
			}
			if !found {
				t.Fatalf("want a %s entry, got %+v", tc.category, au.appended)
			}
		})
	}
}
