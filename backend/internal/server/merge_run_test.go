package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// unstableMergeErr is the multi-%w shape githubclient.EnableAutoMerge produces
// on a checks-not-all-passed refusal (E67.56 / #2717): ErrValidation AND
// ErrPullRequestUnstableStatus, carrying the verbatim production GraphQL message
// from run a152a0a5.
func unstableMergeErr() error {
	return fmt.Errorf("%w: %w: enable auto-merge: Pull request Pull request is in unstable status",
		forge.ErrValidation, forge.ErrPullRequestUnstableStatus)
}

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

// TestMergeRun_AcceptanceNotValidated_Proceeds pins the #2347 merge-admission
// branch: a short-circuited acceptance stage that verified ZERO criteria is
// merge-ELIGIBLE. This is the deliberate scope boundary of #2347 — the change
// makes the outcome HONEST (a distinct verdict, gate state, next_actions state
// and status-comment row), it does NOT make it obstructive. Blocking here would
// strand every no-live-target run at a 409 with no verb to clear it.
func TestMergeRun_AcceptanceNotValidated_Proceeds(t *testing.T) {
	merger := &fakeMerger{}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR,
		[]byte(autoDriveAcceptanceSpecYAML), acceptanceMergeStages(runID, run.StageStateSucceeded))
	seedAcceptanceOutcome(au, runID, 6, acceptanceVerdictNotValidated)

	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a not_validated verdict is merge-eligible (#2347):\n%s", w.Code, w.Body.String())
	}
	if merger.called != 1 {
		t.Errorf("merger called %d times, want 1 (not-validated acceptance proceeds)", merger.called)
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

// TestHandleMergeRun_UnstableDispatch_ChecksPending is the E67.56 / #2717 core:
// a merger returning the unstable-status-wrapped sentinel yields 409
// merge_checks_pending — an expected precondition, not a 502 fault. The body
// names the checks-not-all-passed precondition AND the already-FAILED
// possibility (binding condition 1), does NOT prescribe "retry the merge", and
// the verdict row is durable. Counterfactual: deleting the step-6 branch makes
// this test RED (the unstable error falls through to the generic 502).
func TestHandleMergeRun_UnstableDispatch_ChecksPending(t *testing.T) {
	merger := &fakeMerger{err: unstableMergeErr()}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, nil, nil)

	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Error.Code != "merge_checks_pending" {
		t.Errorf("error code = %q, want merge_checks_pending", env.Error.Code)
	}
	// Binding condition 1: honest wording. The message must name the
	// checks-not-all-passed precondition and the already-FAILED possibility, and
	// must NOT prescribe the remedy that cannot work.
	if !strings.Contains(env.Error.Message, "have not all passed") {
		t.Errorf("message must name the checks-not-all-passed precondition: %q", env.Error.Message)
	}
	if !strings.Contains(env.Error.Message, "FAILED") {
		t.Errorf("message must name the already-failed-check possibility: %q", env.Error.Message)
	}
	if strings.Contains(env.Error.Message, "retry the merge") {
		t.Errorf("message must NOT prescribe 'retry the merge' (a remedy that cannot work): %q", env.Error.Message)
	}
	if env.Error.Details["reason"] != "checks_pending" {
		t.Errorf("details.reason = %v, want checks_pending", env.Error.Details["reason"])
	}
	if env.Error.Details["pr_url"] != mergePR {
		t.Errorf("details.pr_url = %v, want %q", env.Error.Details["pr_url"], mergePR)
	}
	if _, ok := env.Error.Details["verdict_sequence"]; !ok {
		t.Errorf("details missing verdict_sequence: %v", env.Error.Details)
	}
	// The verdict row IS durable despite the checks-pending refusal.
	if rows := mergeVerdictRows(au); len(rows) != 1 {
		t.Errorf("merge_verdict_recorded rows = %d, want 1 (verdict durable on the 409)", len(rows))
	}
}

// TestHandleMergeRun_GenericDispatchFailure_Still502 is the counterfactual for
// the not-swallowed criterion: a plain (non-sentinel) dispatch error still
// yields 502 merge_dispatch_failed. Widening the checks-pending branch past the
// unstable sentinel would turn this RED.
func TestHandleMergeRun_GenericDispatchFailure_Still502(t *testing.T) {
	merger := &fakeMerger{err: errBoom}
	s, repo, _ := newAutoDriveMergeServer(t, merger)
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, nil, nil)

	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("merge_dispatch_failed")) {
		t.Errorf("body missing merge_dispatch_failed: %s", w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("merge_checks_pending")) {
		t.Errorf("a generic dispatch error must NOT be classified as checks-pending: %s", w.Body.String())
	}
}

// TestHandleMergeRun_ChecksPending_IdempotentAcrossWait is binding condition 4's
// committed-state control: across a 409 (unstable), a second 409 (unstable), and
// a final 200 (healthy) there is EXACTLY ONE merge_verdict_recorded row — the
// durability/idempotence contract pinned on persisted state, not on a
// byte-identical error. The final POST re-dispatches with already_recorded:true.
// Deleting the step-6 branch makes this RED (the first two POSTs would 502).
func TestHandleMergeRun_ChecksPending_IdempotentAcrossWait(t *testing.T) {
	merger := &fakeMerger{err: unstableMergeErr()}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, nil, nil)

	// POST 1: checks pending → 409, verdict recorded.
	w1 := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w1.Code != http.StatusConflict {
		t.Fatalf("POST 1 status = %d, want 409:\n%s", w1.Code, w1.Body.String())
	}
	// POST 2: still checks pending → 409, no duplicate append.
	w2 := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go again"}, withMergeOperator)
	if w2.Code != http.StatusConflict {
		t.Fatalf("POST 2 status = %d, want 409:\n%s", w2.Code, w2.Body.String())
	}
	// POST 3: checks cleared → the merge queues.
	merger.err = nil
	w3 := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go final"}, withMergeOperator)
	if w3.Code != http.StatusOK {
		t.Fatalf("POST 3 status = %d, want 200:\n%s", w3.Code, w3.Body.String())
	}
	var resp3 mergeRunResponse
	if err := json.Unmarshal(w3.Body.Bytes(), &resp3); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp3.AlreadyRecorded {
		t.Error("POST 3 already_recorded = false, want true (the verdict was recorded on POST 1)")
	}
	if !resp3.MergeQueued {
		t.Error("POST 3 merge_queued = false, want true (the merge re-dispatches once checks clear)")
	}
	// EXACTLY ONE merge_verdict_recorded row across all three POSTs.
	if rows := mergeVerdictRows(au); len(rows) != 1 {
		t.Errorf("merge_verdict_recorded rows = %d after three POSTs, want 1 (no duplicate across the wait)", len(rows))
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

// TestEarliestMergeVerdictSequence pins the chain-stable min: the earliest
// (smallest) sequence wins regardless of slice order (the reassignment branch).
func TestEarliestMergeVerdictSequence(t *testing.T) {
	entries := []*audit.Entry{{Sequence: 9}, {Sequence: 3}, {Sequence: 7}}
	if got := earliestMergeVerdictSequence(entries); got != 3 {
		t.Errorf("earliestMergeVerdictSequence = %d, want 3", got)
	}
	if got := earliestMergeVerdictSequence([]*audit.Entry{{Sequence: 42}}); got != 42 {
		t.Errorf("earliestMergeVerdictSequence(single) = %d, want 42", got)
	}
}

// mergeAppendAudit wraps auditFake to drive the concurrent-merge-race branch of
// handleMergeRun without real Postgres. AppendChained returns appendErr for the
// merge_verdict_recorded category (letting a test inject audit.ErrMergeVerdictDuplicate
// or a foreign-constraint 23505); ListForRunByCategory returns empty on the FIRST
// merge-verdict read when firstMergeListEmpty is set, so the handler reaches the
// append branch even though the winner's row is already seeded (recovered by the
// re-read).
type mergeAppendAudit struct {
	*auditFake
	appendErr           error
	firstMergeListEmpty bool
	mergeListCalls      int
}

func (a *mergeAppendAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if p.Category == CategoryMergeVerdictRecorded && a.appendErr != nil {
		return nil, a.appendErr
	}
	return a.auditFake.AppendChained(ctx, p)
}

func (a *mergeAppendAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if category == CategoryMergeVerdictRecorded {
		a.mergeListCalls++
		if a.firstMergeListEmpty && a.mergeListCalls == 1 {
			return nil, nil
		}
	}
	return a.auditFake.ListForRunByCategory(ctx, runID, category)
}

// seedMergeVerdict seeds a committed merge_verdict_recorded row (the race
// winner) into the fake's history so the loser's re-read can recover it.
func seedMergeVerdict(au *auditFake, runID uuid.UUID, seq int64) {
	rid := runID
	p, _ := json.Marshal(map[string]any{"verdict": "winner", "pr_url": mergePR, "delegated": false})
	au.mu.Lock()
	au.seeded = append(au.seeded, &audit.Entry{
		RunID: &rid, Category: CategoryMergeVerdictRecorded, Sequence: seq, Payload: p,
	})
	au.mu.Unlock()
}

// TestMergeRun_ConcurrentRaceLoser_Benign is binding condition 1's benign path:
// AppendChained loses the partial-unique-index race (returns the
// merge-verdict-index duplicate). The handler re-reads the winner's row,
// responds 200 already_recorded with the WINNER's sequence, appends NO duplicate,
// and STILL dispatches the merge.
func TestMergeRun_ConcurrentRaceLoser_Benign(t *testing.T) {
	merger := &fakeMerger{}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, nil, nil)
	seedMergeVerdict(au, runID, 77) // the winner's committed row
	s.cfg.AuditRepo = &mergeAppendAudit{
		auditFake:           au,
		appendErr:           audit.ErrMergeVerdictDuplicate,
		firstMergeListEmpty: true,
	}

	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (benign concurrent-race loser):\n%s", w.Code, w.Body.String())
	}
	var resp mergeRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.AlreadyRecorded {
		t.Error("already_recorded = false, want true (race loser recovers the winner's verdict)")
	}
	if resp.VerdictSequence != 77 {
		t.Errorf("verdict_sequence = %d, want 77 (the winner's sequence)", resp.VerdictSequence)
	}
	if merger.called != 1 {
		t.Errorf("merger called %d times, want 1 (race loser still dispatches the merge)", merger.called)
	}
	// No duplicate appended — the winner is the seeded row, the loser's append
	// was rejected.
	if rows := mergeVerdictRows(au); len(rows) != 0 {
		t.Errorf("merge_verdict_recorded appended rows = %d, want 0 (no duplicate on the race loser)", len(rows))
	}
}

// TestMergeRun_DuplicateOnDifferentConstraint_500 is binding condition 1's
// scoping guard: a 23505 on a DIFFERENT constraint (e.g. the (run_id, sequence)
// / hash-chain uniqueness) is NOT the benign merge-verdict race and must stay a
// 500 — never swallowed, never dispatched.
func TestMergeRun_DuplicateOnDifferentConstraint_500(t *testing.T) {
	merger := &fakeMerger{}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, nil, nil)
	s.cfg.AuditRepo = &mergeAppendAudit{
		auditFake: au,
		// A unique_violation on an unrelated constraint — not the merge-verdict index.
		appendErr: &pgconn.PgError{Code: "23505", ConstraintName: "audit_entries_run_id_sequence_key"},
	}

	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (foreign-constraint 23505 must not be swallowed):\n%s", w.Code, w.Body.String())
	}
	if merger.called != 0 {
		t.Errorf("merger called %d times on an unrelated integrity failure, want 0", merger.called)
	}
}

// TestMergeRun_RaceLoserNoWinnerOnReread_500 is binding condition 1's defensive
// fail-closed branch: the merge-verdict duplicate is caught but the re-read
// finds NO winner row (unreachable in practice — the index guarantees the
// winner is committed — but the handler must fail closed rather than fabricate a
// verdict_sequence and dispatch on a phantom winner).
func TestMergeRun_RaceLoserNoWinnerOnReread_500(t *testing.T) {
	merger := &fakeMerger{}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, nil, nil)
	// No winner seeded: both the first read and the re-read come back empty.
	s.cfg.AuditRepo = &mergeAppendAudit{
		auditFake: au,
		appendErr: audit.ErrMergeVerdictDuplicate,
	}

	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (duplicate caught but no winner on re-read → fail closed):\n%s", w.Code, w.Body.String())
	}
	if merger.called != 0 {
		t.Errorf("merger called %d times on a phantom-winner re-read, want 0 (no dispatch)", merger.called)
	}
}

// TestMergeRun_RaceLoserRereadError_500 covers the re-read failure branch: the
// merge-verdict duplicate is caught, but the re-read of ListForRunByCategory
// itself errors. The handler surfaces a 500 and does NOT dispatch the merge.
func TestMergeRun_RaceLoserRereadError_500(t *testing.T) {
	merger := &fakeMerger{}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, nil, nil)
	// The first merge-verdict read is short-circuited to empty; the re-read
	// delegates to auditFake, which now fails.
	au.listByCategoryErr = errBoom
	s.cfg.AuditRepo = &mergeAppendAudit{
		auditFake:           au,
		appendErr:           audit.ErrMergeVerdictDuplicate,
		firstMergeListEmpty: true,
	}

	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (re-read after duplicate failed):\n%s", w.Code, w.Body.String())
	}
	if merger.called != 0 {
		t.Errorf("merger called %d times on a failed re-read, want 0 (no dispatch)", merger.called)
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

// TestMergeRun_AcceptanceArbitrated_Proceeds pins the E66.37 / #2474 merge
// admission: a FAILED acceptance verdict whose paged triage an operator
// discharged is merge-eligible, and the merge still records the operator's
// merge_verdict_recorded entry — which is the whole point of the verb. Before
// this the operator's only route was to leave the blessed path and hand-merge,
// losing that entry.
func TestMergeRun_AcceptanceArbitrated_Proceeds(t *testing.T) {
	merger := &fakeMerger{}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR,
		[]byte(autoDriveAcceptanceSpecYAML), acceptanceMergeStages(runID, run.StageStateSucceeded))
	seedAcceptanceOutcome(au, runID, 6, acceptanceVerdictFailed)
	seedArbitration(au, runID, 7, 6)

	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "merging on an arbitrated acceptance failure"}, withMergeOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — an arbitrated acceptance failure is merge-eligible (#2474):\n%s", w.Code, w.Body.String())
	}
	if merger.called != 1 {
		t.Errorf("merger called %d times, want 1 (arbitrated acceptance proceeds)", merger.called)
	}
	if rows := mergeVerdictRows(au); len(rows) != 1 {
		t.Fatalf("merge_verdict_recorded rows = %d, want 1 — the audited merge verdict is the reason to stay on the blessed path", len(rows))
	}
}

// TestMergeRun_AcceptanceArbitrationForOlderOutcome_Blocks pins the invalidation
// at the MERGE surface: an acceptance re-run recorded a NEWER failed verdict, so
// the prior arbitration no longer names the current outcome and the merge is
// refused again. Without the sequence binding this would merge on a stale
// discharge.
func TestMergeRun_AcceptanceArbitrationForOlderOutcome_Blocks(t *testing.T) {
	merger := &fakeMerger{}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR,
		[]byte(autoDriveAcceptanceSpecYAML), acceptanceMergeStages(runID, run.StageStateSucceeded))
	seedAcceptanceOutcome(au, runID, 6, acceptanceVerdictFailed)
	seedArbitration(au, runID, 7, 6)
	seedAcceptanceOutcome(au, runID, 20, acceptanceVerdictFailed) // re-run

	w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "go"}, withMergeOperator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if merger.called != 0 || len(mergeVerdictRows(au)) != 0 {
		t.Errorf("merger.called=%d rows=%d, want 0/0 (a superseded arbitration must not admit a merge)",
			merger.called, len(mergeVerdictRows(au)))
	}
}
