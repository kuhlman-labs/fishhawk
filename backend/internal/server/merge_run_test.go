package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// merge_run_test.go pins POST /v0/runs/{run_id}/merge (E48.7 / #1954): the
// operator merge verb. One behavioral test per enumerated failure mode (the
// #1199 rule) plus the happy path, the endpoint-idempotence contract (binding
// approval condition 1), and the deliberate divergence from the delegated arm
// (a review stage awaiting approval does NOT block the human merge).

// postMergeRun posts a {verdict} body with the given identity mutator.
func postMergeRun(t *testing.T, s *Server, runID uuid.UUID, body mergeRunRequest,
	withID func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	return postMergeRunRaw(t, s, runID, raw, withID)
}

// postMergeRunRaw posts an arbitrary (possibly malformed) body.
func postMergeRunRaw(t *testing.T, s *Server, runID uuid.UUID, raw []byte,
	withID func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID.String()+"/merge", bytes.NewReader(raw))
	req.SetPathValue("run_id", runID.String())
	w := httptest.NewRecorder()
	s.handleMergeRun(w, withID(req))
	return w
}

// withMergeOperator injects an operator token carrying write:approvals — the
// credential the merge verb accepts.
func withMergeOperator(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
		Subject: "github:ops", TokenID: "tok-op", Scopes: []string{"write:approvals"},
	}))
}

// seedMergeRun writes a run + stages directly into the autoDriveRepo backing
// store for the merge handler. A nil workflowSpec leaves the acceptance gate
// not-declared (merge admitted).
func seedMergeRun(t *testing.T, repo *autoDriveRepo, runID uuid.UUID, state run.State,
	prURL string, workflowSpec []byte, stages []*run.Stage) *run.Run {
	t.Helper()
	runRow := &run.Run{ID: runID, State: state, WorkflowID: "feature_change", WorkflowSpec: workflowSpec}
	if prURL != "" {
		runRow.PullRequestURL = &prURL
	}
	repo.mu.Lock()
	repo.runs[runID] = runRow
	repo.stagesByRun[runID] = stages
	repo.mu.Unlock()
	return runRow
}

const mergePR = "https://github.com/x/y/pull/7"

// mergeVerdictRows returns every appended merge_verdict_recorded param.
func mergeVerdictRows(au *auditFake) []audit.ChainAppendParams {
	au.mu.Lock()
	defer au.mu.Unlock()
	var out []audit.ChainAppendParams
	for i := range au.appended {
		if au.appended[i].Category == CategoryMergeVerdictRecorded {
			out = append(out, au.appended[i])
		}
	}
	return out
}

// orderMerger records, at dispatch time, whether the merge_verdict_recorded row
// was ALREADY appended — pinning the fail-closed ordering (verdict durable
// BEFORE the merge is queued).
type orderMerger struct {
	au               *auditFake
	called           int
	sawRowAtDispatch bool
	err              error
}

func (m *orderMerger) MergePullRequest(_ context.Context, _ *run.Run) error {
	m.called++
	for _, a := range mergeVerdictRows(m.au) {
		if a.Category == CategoryMergeVerdictRecorded {
			m.sawRowAtDispatch = true
		}
	}
	return m.err
}

func TestMergeRun_HappyPath(t *testing.T) {
	merger := &fakeMerger{}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, nil, nil)

	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "lgtm — merging"}, withMergeOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp mergeRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.MergeQueued {
		t.Error("merge_queued = false, want true")
	}
	if resp.AlreadyRecorded {
		t.Error("already_recorded = true on a first POST, want false")
	}
	if resp.PRURL != mergePR {
		t.Errorf("pr_url = %q, want %q", resp.PRURL, mergePR)
	}
	if merger.called != 1 {
		t.Errorf("merger called %d times, want 1", merger.called)
	}
	rows := mergeVerdictRows(au)
	if len(rows) != 1 {
		t.Fatalf("merge_verdict_recorded rows = %d, want 1", len(rows))
	}
	if rows[0].ActorKind == nil || *rows[0].ActorKind != audit.ActorUser {
		t.Errorf("actor kind = %v, want user", rows[0].ActorKind)
	}
	var payload struct {
		Verdict   string `json:"verdict"`
		PRURL     string `json:"pr_url"`
		Delegated bool   `json:"delegated"`
	}
	if err := json.Unmarshal(rows[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Verdict != "lgtm — merging" {
		t.Errorf("payload verdict = %q", payload.Verdict)
	}
	if payload.PRURL != mergePR {
		t.Errorf("payload pr_url = %q, want %q", payload.PRURL, mergePR)
	}
	if payload.Delegated {
		t.Error("payload delegated = true, want false (human merge path)")
	}
}

// TestMergeRun_VerdictAppendedBeforeDispatch pins the fail-closed ordering: the
// verdict row is durable BEFORE the merge helper is dispatched.
func TestMergeRun_VerdictAppendedBeforeDispatch(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, nil)
	merger := &orderMerger{au: au}
	s.cfg.GateMerger = merger
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, nil, nil)

	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if merger.called != 1 {
		t.Fatalf("merger called %d, want 1", merger.called)
	}
	if !merger.sawRowAtDispatch {
		t.Error("merge dispatched before the merge_verdict_recorded row was appended (ordering violated)")
	}
}

func TestMergeRun_Anonymous(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, nil, nil)

	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"},
		func(req *http.Request) *http.Request { return req }) // no identity → anonymous
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401:\n%s", w.Code, w.Body.String())
	}
	if len(mergeVerdictRows(au)) != 0 {
		t.Error("audit written despite anonymous")
	}
}

func TestMergeRun_MissingScope(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, nil, nil)

	withScopeless := func(req *http.Request) *http.Request {
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
			Subject: "github:ops", TokenID: "tok-x", Scopes: []string{"read:runs"},
		}))
	}
	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withScopeless)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("insufficient_scope")) {
		t.Errorf("body missing insufficient_scope: %s", w.Body.String())
	}
	if len(mergeVerdictRows(au)) != 0 {
		t.Error("audit written despite missing scope")
	}
}

// TestMergeRun_EmptyTokenIDNoScope: a cookie-session identity (empty TokenID,
// no scopes) is rejected 403 — write:approvals is enforced unconditionally, no
// bypass (mirrors vouch).
func TestMergeRun_EmptyTokenIDNoScope(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, nil, nil)

	withSessionNoScope := func(req *http.Request) *http.Request {
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
			Subject: "github:ops", UserID: "u-1", SessionID: "s-1",
		}))
	}
	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withSessionNoScope)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("insufficient_scope")) {
		t.Errorf("body missing insufficient_scope: %s", w.Body.String())
	}
	if len(mergeVerdictRows(au)) != 0 {
		t.Error("audit written despite cookie-session without scope")
	}
}

func TestMergeRun_RunBoundTokenForbidden(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, nil, nil)

	// A run-bound token for THIS run, even carrying write:approvals, is rejected.
	withOwnRunToken := func(req *http.Request) *http.Request {
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
			Subject: "mcp:run:" + runID.String(),
			TokenID: "tok-agent",
			Scopes:  []string{"mcp:read", "write:approvals"},
		}))
	}
	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withOwnRunToken)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("run_token_forbidden")) {
		t.Errorf("body missing run_token_forbidden: %s", w.Body.String())
	}
	if len(mergeVerdictRows(au)) != 0 {
		t.Error("audit written despite run-bound token rejection")
	}
}

func TestMergeRun_EmptyVerdict(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, nil, nil)

	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "   "}, withMergeOperator)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("validation_failed")) {
		t.Errorf("body missing validation_failed: %s", w.Body.String())
	}
	if len(mergeVerdictRows(au)) != 0 {
		t.Error("audit written despite empty verdict")
	}
}

func TestMergeRun_MalformedBody(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, nil, nil)

	w := postMergeRunRaw(t, s, runID, []byte("{not json"), withMergeOperator)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("validation_failed")) {
		t.Errorf("body missing validation_failed: %s", w.Body.String())
	}
	if len(mergeVerdictRows(au)) != 0 {
		t.Error("audit written despite malformed body")
	}
}

// TestMergeRun_InvalidRunID covers the 400 on a non-UUID run_id path value.
func TestMergeRun_InvalidRunID(t *testing.T) {
	s, _, au := newAutoDriveMergeServer(t, &fakeMerger{})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/not-a-uuid/merge",
		bytes.NewReader([]byte(`{"verdict":"go"}`)))
	req.SetPathValue("run_id", "not-a-uuid")
	w := httptest.NewRecorder()
	s.handleMergeRun(w, withMergeOperator(req))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("validation_failed")) {
		t.Errorf("body missing validation_failed: %s", w.Body.String())
	}
	if len(mergeVerdictRows(au)) != 0 {
		t.Error("audit written despite invalid run_id")
	}
}

// TestMergeRun_GetRunInternalError covers the 500 on a non-NotFound GetRun error.
func TestMergeRun_GetRunInternalError(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, nil, nil)
	repo.getErr = errBoom

	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
	if len(mergeVerdictRows(au)) != 0 {
		t.Error("audit written despite GetRun error")
	}
}

// TestMergeRun_PriorVerdictReadError covers the 500 when the idempotency
// prior-verdict read fails (a nil-WorkflowSpec run keeps the acceptance gate
// not-declared so the read error surfaces from the merge_verdict_recorded scan,
// not the acceptance classifier).
func TestMergeRun_PriorVerdictReadError(t *testing.T) {
	merger := &fakeMerger{}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, nil, nil)
	au.listByCategoryErr = errBoom

	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
	if merger.called != 0 {
		t.Errorf("merger called %d times on a prior-verdict read error, want 0", merger.called)
	}
}

// TestMergeRun_AppendError covers the 500 when the verdict append fails — no
// merge is dispatched (the append error precedes dispatch).
func TestMergeRun_AppendError(t *testing.T) {
	merger := &fakeMerger{}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, nil, nil)
	au.appendErrCategory = CategoryMergeVerdictRecorded

	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
	if merger.called != 0 {
		t.Errorf("merger called %d times despite an append failure, want 0", merger.called)
	}
}

func TestMergeRun_RunNotFound(t *testing.T) {
	s, _, au := newAutoDriveMergeServer(t, &fakeMerger{})

	w := postMergeRun(t, s, uuid.New(), mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("run_not_found")) {
		t.Errorf("body missing run_not_found: %s", w.Body.String())
	}
	if len(mergeVerdictRows(au)) != 0 {
		t.Error("audit written for an unknown run")
	}
}

func TestMergeRun_NoPullRequestURL(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, "", nil, nil) // no PR url

	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("run_not_mergeable")) {
		t.Errorf("body missing run_not_mergeable: %s", w.Body.String())
	}
	if len(mergeVerdictRows(au)) != 0 {
		t.Error("audit written despite no PR url")
	}
}

func TestMergeRun_TerminalRun(t *testing.T) {
	for _, st := range []run.State{run.StateFailed, run.StateCancelled} {
		t.Run(string(st), func(t *testing.T) {
			s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
			runID := uuid.New()
			seedMergeRun(t, repo, runID, st, mergePR, nil, nil)

			w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
			if w.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
			}
			if !bytes.Contains(w.Body.Bytes(), []byte("run_not_mergeable")) {
				t.Errorf("body missing run_not_mergeable: %s", w.Body.String())
			}
			if len(mergeVerdictRows(au)) != 0 {
				t.Errorf("audit written despite %s run", st)
			}
		})
	}
}

// --- acceptance gate matrix -------------------------------------------------

// acceptanceMergeStages materializes the plan/implement/acceptance stages the
// acceptance spec declares, with the acceptance stage in accState.
func acceptanceMergeStages(runID uuid.UUID, accState run.StageState) []*run.Stage {
	return []*run.Stage{
		{ID: uuid.New(), RunID: runID, Sequence: 0, Type: run.StageTypePlan, State: run.StageStateSucceeded},
		{ID: uuid.New(), RunID: runID, Sequence: 1, Type: run.StageTypeImplement, State: run.StageStateSucceeded},
		{ID: uuid.New(), RunID: runID, Sequence: 2, Type: run.StageTypeAcceptance, State: accState},
	}
}

func TestMergeRun_AcceptancePending_Blocks(t *testing.T) {
	merger := &fakeMerger{}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR,
		[]byte(autoDriveAcceptanceSpecYAML), acceptanceMergeStages(runID, run.StageStateRunning))

	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("acceptance_gate_not_passed")) {
		t.Errorf("body missing acceptance_gate_not_passed: %s", w.Body.String())
	}
	if merger.called != 0 || len(mergeVerdictRows(au)) != 0 {
		t.Errorf("merger.called=%d rows=%d, want 0/0 (acceptance pending blocks)", merger.called, len(mergeVerdictRows(au)))
	}
}

func TestMergeRun_AcceptanceFailed_Blocks(t *testing.T) {
	merger := &fakeMerger{}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR,
		[]byte(autoDriveAcceptanceSpecYAML), acceptanceMergeStages(runID, run.StageStateSucceeded))
	seedAcceptanceOutcome(au, runID, 6, acceptanceVerdictFailed)

	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if merger.called != 0 || len(mergeVerdictRows(au)) != 0 {
		t.Errorf("merger.called=%d rows=%d, want 0/0 (failed verdict blocks)", merger.called, len(mergeVerdictRows(au)))
	}
}

func TestMergeRun_AcceptanceOutcomeUnknown_Blocks(t *testing.T) {
	merger := &fakeMerger{}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR,
		[]byte(autoDriveAcceptanceSpecYAML), acceptanceMergeStages(runID, run.StageStateSucceeded)) // terminal, no verdict

	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if merger.called != 0 || len(mergeVerdictRows(au)) != 0 {
		t.Errorf("merger.called=%d rows=%d, want 0/0 (outcome unknown blocks)", merger.called, len(mergeVerdictRows(au)))
	}
}

func TestMergeRun_AcceptanceReadError_Blocks(t *testing.T) {
	merger := &fakeMerger{}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR,
		[]byte(autoDriveAcceptanceSpecYAML), acceptanceMergeStages(runID, run.StageStateSucceeded))
	au.listByCategoryErr = errBoom

	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if merger.called != 0 {
		t.Errorf("merger called %d times on an acceptance read error, want 0 (fail-closed)", merger.called)
	}
}

func TestMergeRun_AcceptancePassed_Proceeds(t *testing.T) {
	merger := &fakeMerger{}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR,
		[]byte(autoDriveAcceptanceSpecYAML), acceptanceMergeStages(runID, run.StageStateSucceeded))
	seedAcceptanceOutcome(au, runID, 6, acceptanceVerdictPassed)

	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if merger.called != 1 {
		t.Errorf("merger called %d times, want 1 (passed acceptance proceeds)", merger.called)
	}
}

func TestMergeRun_AcceptanceSkippedOutOfScope_Proceeds(t *testing.T) {
	merger := &fakeMerger{}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := uuid.New()
	stages := acceptanceMergeStages(runID, run.StageStateSucceeded)
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR,
		[]byte(autoDriveAcceptanceSpecYAML), stages)
	seedAcceptanceSkipMarker(au, runID, stages[2].ID)

	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if merger.called != 1 {
		t.Errorf("merger called %d times, want 1 (skip-settled acceptance proceeds)", merger.called)
	}
}

// TestMergeRun_ReviewAwaitingApprovalDoesNotBlock is the deliberate divergence
// from the delegated arm: a review stage parked at awaiting_approval does NOT
// block the human merge (resolveReviewStageOnMerge settles it ON merge; blocking
// would deadlock the merge path).
func TestMergeRun_ReviewAwaitingApprovalDoesNotBlock(t *testing.T) {
	merger := &fakeMerger{}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := uuid.New()
	stages := []*run.Stage{
		{ID: uuid.New(), RunID: runID, Sequence: 0, Type: run.StageTypeImplement, State: run.StageStateSucceeded},
		{ID: uuid.New(), RunID: runID, Sequence: 1, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval},
	}
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, nil, stages) // no acceptance spec → not-declared

	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (review awaiting_approval must not block):\n%s", w.Code, w.Body.String())
	}
	if merger.called != 1 {
		t.Errorf("merger called %d times, want 1", merger.called)
	}
	if len(mergeVerdictRows(au)) != 1 {
		t.Errorf("merge_verdict_recorded rows = %d, want 1", len(mergeVerdictRows(au)))
	}
}

// TestMergeRun_NilMerger_NoWrite: the 503 fail-closed BEFORE any write — a nil
// merge seam returns 503 and appends NO verdict row.
func TestMergeRun_NilMerger_NoWrite(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, nil) // GateMerger nil
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, nil, nil)

	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("merge_seam_unconfigured")) {
		t.Errorf("body missing merge_seam_unconfigured: %s", w.Body.String())
	}
	if len(mergeVerdictRows(au)) != 0 {
		t.Error("verdict row appended despite nil merger (fail-closed ordering violated)")
	}
}

// TestMergeRun_Unconfigured: nil RunRepo/AuditRepo → 503.
func TestMergeRun_Unconfigured(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	w := postMergeRun(t, s, uuid.New(), mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("merge_unconfigured")) {
		t.Errorf("body missing merge_unconfigured: %s", w.Body.String())
	}
}

// TestMergeRun_MergeDispatchFailed_502 pins the 502: the verdict row is durable
// but the merge dispatch failed (retryable).
func TestMergeRun_MergeDispatchFailed_502(t *testing.T) {
	merger := &fakeMerger{err: errBoom}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, nil, nil)

	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("merge_dispatch_failed")) {
		t.Errorf("body missing merge_dispatch_failed: %s", w.Body.String())
	}
	// The verdict row IS durable despite the dispatch failure.
	if len(mergeVerdictRows(au)) != 1 {
		t.Errorf("merge_verdict_recorded rows = %d, want 1 (verdict durable on 502)", len(mergeVerdictRows(au)))
	}
}

// TestMergeRun_Idempotent_RepeatedPost is binding condition 1: a repeated POST
// appends NO duplicate row, responds already_recorded:true, and STILL
// re-dispatches the merge (so a 502-then-reinvoke re-queues without duplicating
// the verdict).
func TestMergeRun_Idempotent_RepeatedPost(t *testing.T) {
	merger := &fakeMerger{}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, nil, nil)

	w1 := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w1.Code != http.StatusOK {
		t.Fatalf("first POST status = %d, want 200:\n%s", w1.Code, w1.Body.String())
	}
	w2 := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go again"}, withMergeOperator)
	if w2.Code != http.StatusOK {
		t.Fatalf("second POST status = %d, want 200:\n%s", w2.Code, w2.Body.String())
	}
	var resp2 mergeRunResponse
	if err := json.Unmarshal(w2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp2.AlreadyRecorded {
		t.Error("second POST already_recorded = false, want true")
	}
	if rows := mergeVerdictRows(au); len(rows) != 1 {
		t.Errorf("merge_verdict_recorded rows = %d after two POSTs, want 1 (no duplicate)", len(rows))
	}
	if merger.called != 2 {
		t.Errorf("merger called %d times, want 2 (re-dispatch on every POST)", merger.called)
	}
}

// TestMergeRun_ReinvokeAfter502_ReQueues models the 502-then-reinvoke: the
// first POST's merge dispatch fails (502, verdict durable); the reinvoke finds
// the existing row, appends none, and RE-queues the merge successfully.
func TestMergeRun_ReinvokeAfter502_ReQueues(t *testing.T) {
	merger := &fakeMerger{err: errBoom}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, nil, nil)

	w1 := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w1.Code != http.StatusBadGateway {
		t.Fatalf("first POST status = %d, want 502:\n%s", w1.Code, w1.Body.String())
	}
	// The merge seam recovers; the reinvoke re-queues.
	merger.err = nil
	w2 := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w2.Code != http.StatusOK {
		t.Fatalf("reinvoke status = %d, want 200:\n%s", w2.Code, w2.Body.String())
	}
	var resp mergeRunResponse
	if err := json.Unmarshal(w2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.AlreadyRecorded {
		t.Error("reinvoke already_recorded = false, want true (no duplicate verdict)")
	}
	if rows := mergeVerdictRows(au); len(rows) != 1 {
		t.Errorf("merge_verdict_recorded rows = %d, want 1 (verdict recorded exactly once across a 502+reinvoke)", len(rows))
	}
	if merger.called != 2 {
		t.Errorf("merger called %d times, want 2 (failed dispatch + successful re-queue)", merger.called)
	}
}
