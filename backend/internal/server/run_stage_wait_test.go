package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// shortenRunStageWaitPoll lowers the stage long-poll re-read interval for
// the duration of a test so the ?wait loop reacts in milliseconds, and
// restores it on cleanup. Mirrors shortenScopeAmendmentPoll.
func shortenRunStageWaitPoll(t *testing.T) {
	t.Helper()
	prev := runStageWaitPollInterval
	runStageWaitPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { runStageWaitPollInterval = prev })
}

// waitStageRepo wraps the shared orchestratorRepo so the wait-loop tests
// can (a) read stages as copies — snapshotted under the orchestrator's
// lock — so a concurrent state mutation is race-free under -race, and
// (b) inject a transient GetStage error on a chosen re-poll to exercise
// the best-effort last-good return path. Embedding gives the full
// run.Repository for free.
type waitStageRepo struct {
	*orchestratorRepo
	mu         sync.Mutex
	getCalls   int
	failOnCall int // 1-based; once getCalls reaches this, GetStage errors. 0 = never.
}

func newWaitStageRepo() *waitStageRepo {
	return &waitStageRepo{orchestratorRepo: newOrchestratorRepo()}
}

func (r *waitStageRepo) GetStage(ctx context.Context, id uuid.UUID) (*run.Stage, error) {
	r.mu.Lock()
	r.getCalls++
	n := r.getCalls
	fail := r.failOnCall
	r.mu.Unlock()
	if fail > 0 && n >= fail {
		return nil, errors.New("transient db error")
	}
	st, err := r.orchestratorRepo.GetStage(ctx, id)
	if err != nil {
		return nil, err
	}
	// Snapshot under the orchestrator lock: the live pointer is mutated
	// by setStageState / TransitionStage from the test's goroutine.
	r.orchestratorRepo.mu.Lock()
	cp := *st
	r.orchestratorRepo.mu.Unlock()
	return &cp, nil
}

// setStageState mutates a seeded stage's state under the orchestrator's
// lock so it races cleanly with the wait loop's snapshotting GetStage.
func (r *waitStageRepo) setStageState(st *run.Stage, state run.StageState) {
	r.orchestratorRepo.mu.Lock()
	st.State = state
	r.orchestratorRepo.mu.Unlock()
}

// waitAuditRepo wraps auditCapture to return a seeded run_auto_advanced
// entry list (or an error) from ListForRunByCategory, so the
// NextActionPopulated test can drive the drive-surface distillation.
type waitAuditRepo struct {
	*auditCapture
	entries []*audit.Entry
	err     error
}

func newWaitAuditRepo() *waitAuditRepo {
	return &waitAuditRepo{auditCapture: &auditCapture{}}
}

func (a *waitAuditRepo) ListForRunByCategory(_ context.Context, _ uuid.UUID, category string) ([]*audit.Entry, error) {
	if a.err != nil {
		return nil, a.err
	}
	if category != drive.Category {
		return nil, nil
	}
	return a.entries, nil
}

// runStageWaitServer wires a server over a waitStageRepo with a run and a
// single seeded stage in the given state.
func runStageWaitServer(t *testing.T, state run.StageState) (*Server, *waitStageRepo, *waitAuditRepo, *run.Run, *run.Stage) {
	t.Helper()
	rr := newWaitStageRepo()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 1, state)
	stage.Type = run.StageTypeImplement
	au := newWaitAuditRepo()
	s := New(Config{
		Addr:      "127.0.0.1:0",
		RunRepo:   rr,
		AuditRepo: au,
	})
	return s, rr, au, runRow, stage
}

// getRunStage drives handleGetRunStage with explicit path values and an
// optional ?wait. waitSeconds < 0 omits the query param entirely.
func getRunStage(t *testing.T, s *Server, runID, stageID uuid.UUID, waitSeconds int, decorate func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	url := "/v0/runs/" + runID.String() + "/stages/" + stageID.String()
	if waitSeconds >= 0 {
		url += "?wait=" + strconv.Itoa(waitSeconds)
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.SetPathValue("run_id", runID.String())
	req.SetPathValue("stage_id", stageID.String())
	if decorate != nil {
		req = decorate(req)
	}
	w := httptest.NewRecorder()
	s.handleGetRunStage(w, req)
	return w
}

func decodeRunStageWait(t *testing.T, w *httptest.ResponseRecorder) runStageWaitResponse {
	t.Helper()
	var resp runStageWaitResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v; body = %s", err, w.Body.String())
	}
	return resp
}

func decodeErrCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error body: %v; body = %s", err, w.Body.String())
	}
	return env.Error.Code
}

// --- wait loop behavior ---

// TestGetRunStage_WaitReturnsOnSettle: a goroutine transitions the stage
// running -> succeeded ~30ms into a wait=10 call; the handler returns
// promptly (well before the cap) with terminal=true, state=succeeded.
func TestGetRunStage_WaitReturnsOnSettle(t *testing.T) {
	shortenRunStageWaitPoll(t)
	s, rr, _, runRow, stage := runStageWaitServer(t, run.StageStateRunning)

	go func() {
		time.Sleep(30 * time.Millisecond)
		rr.setStageState(stage, run.StageStateSucceeded)
	}()

	start := time.Now()
	w := getRunStage(t, s, runRow.ID, stage.ID, 10, func(r *http.Request) *http.Request {
		return withOperatorIdentity(r, "read:runs")
	})
	elapsed := time.Since(start)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", w.Code, w.Body.String())
	}
	if elapsed > 2*time.Second {
		t.Errorf("wait took %s; expected prompt return well under the 10s cap", elapsed)
	}
	resp := decodeRunStageWait(t, w)
	if !resp.Terminal || resp.State != string(run.StageStateSucceeded) {
		t.Errorf("terminal=%v state=%q, want terminal=true state=succeeded", resp.Terminal, resp.State)
	}
}

// TestGetRunStage_WaitReturnsAtCap: the stage stays running for a wait=1
// call; the handler holds ~1s and returns terminal=false, state=running.
func TestGetRunStage_WaitReturnsAtCap(t *testing.T) {
	shortenRunStageWaitPoll(t)
	s, _, _, runRow, stage := runStageWaitServer(t, run.StageStateRunning)

	start := time.Now()
	w := getRunStage(t, s, runRow.ID, stage.ID, 1, func(r *http.Request) *http.Request {
		return withOperatorIdentity(r, "read:runs")
	})
	elapsed := time.Since(start)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", w.Code, w.Body.String())
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("wait returned after %s; expected it to hold ~1s to the cap", elapsed)
	}
	resp := decodeRunStageWait(t, w)
	if resp.Terminal || resp.State != string(run.StageStateRunning) {
		t.Errorf("terminal=%v state=%q, want terminal=false state=running", resp.Terminal, resp.State)
	}
}

// TestGetRunStage_AlreadyParkedReturnsImmediately: a stage seeded
// awaiting_approval with wait=10 returns immediately (parked is settled).
func TestGetRunStage_AlreadyParkedReturnsImmediately(t *testing.T) {
	shortenRunStageWaitPoll(t)
	s, _, _, runRow, stage := runStageWaitServer(t, run.StageStateAwaitingApproval)

	start := time.Now()
	w := getRunStage(t, s, runRow.ID, stage.ID, 10, func(r *http.Request) *http.Request {
		return withOperatorIdentity(r, "read:runs")
	})
	elapsed := time.Since(start)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", w.Code, w.Body.String())
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("parked stage took %s; expected immediate return without entering the wait loop", elapsed)
	}
	resp := decodeRunStageWait(t, w)
	if !resp.Terminal || resp.State != string(run.StageStateAwaitingApproval) {
		t.Errorf("terminal=%v state=%q, want terminal=true state=awaiting_approval", resp.Terminal, resp.State)
	}
}

// TestGetRunStage_ClientDisconnect: cancel the request context mid-wait;
// the handler releases promptly with the last-good (still-running) stage.
func TestGetRunStage_ClientDisconnect(t *testing.T) {
	shortenRunStageWaitPoll(t)
	s, _, _, runRow, stage := runStageWaitServer(t, run.StageStateRunning)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	req := httptest.NewRequest(http.MethodGet,
		"/v0/runs/"+runRow.ID.String()+"/stages/"+stage.ID.String()+"?wait=10", nil)
	req.SetPathValue("run_id", runRow.ID.String())
	req.SetPathValue("stage_id", stage.ID.String())
	// Build the identity context ON TOP of the cancellable ctx so cancel()
	// propagates while the operator identity is preserved.
	req = withOperatorIdentity(req.WithContext(ctx), "read:runs")

	start := time.Now()
	w := httptest.NewRecorder()
	s.handleGetRunStage(w, req)
	elapsed := time.Since(start)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", w.Code, w.Body.String())
	}
	if elapsed > 1*time.Second {
		t.Errorf("disconnect release took %s; expected prompt release under 1s", elapsed)
	}
	resp := decodeRunStageWait(t, w)
	if resp.Terminal || resp.State != string(run.StageStateRunning) {
		t.Errorf("terminal=%v state=%q, want last-good terminal=false state=running", resp.Terminal, resp.State)
	}
}

// TestGetRunStage_TransientRepoErrorReturnsLastGood: a GetStage that
// errors on the first re-poll returns the last-good stage at 200, not a
// 500.
func TestGetRunStage_TransientRepoErrorReturnsLastGood(t *testing.T) {
	shortenRunStageWaitPoll(t)
	s, rr, _, runRow, stage := runStageWaitServer(t, run.StageStateRunning)
	// First GetStage (the pre-wait read) succeeds; the second (first
	// re-poll) errors, so the loop returns the last-good running stage.
	rr.mu.Lock()
	rr.failOnCall = 2
	rr.mu.Unlock()

	w := getRunStage(t, s, runRow.ID, stage.ID, 10, func(r *http.Request) *http.Request {
		return withOperatorIdentity(r, "read:runs")
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", w.Code, w.Body.String())
	}
	resp := decodeRunStageWait(t, w)
	if resp.Terminal || resp.State != string(run.StageStateRunning) {
		t.Errorf("terminal=%v state=%q, want last-good terminal=false state=running", resp.Terminal, resp.State)
	}
}

// --- auth ---

func TestGetRunStage_Anonymous401(t *testing.T) {
	s, _, _, runRow, stage := runStageWaitServer(t, run.StageStateRunning)
	w := getRunStage(t, s, runRow.ID, stage.ID, -1, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", w.Code, w.Body.String())
	}
	if code := decodeErrCode(t, w); code != "authentication_required" {
		t.Errorf("code = %q, want authentication_required", code)
	}
}

func TestGetRunStage_CrossRunToken403(t *testing.T) {
	s, _, _, runRow, stage := runStageWaitServer(t, run.StageStateRunning)
	other := uuid.New()
	w := getRunStage(t, s, runRow.ID, stage.ID, -1, func(r *http.Request) *http.Request {
		return withRunBoundIdentity(r, other, "mcp:read")
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", w.Code, w.Body.String())
	}
	if code := decodeErrCode(t, w); code != "cross_run_stage" {
		t.Errorf("code = %q, want cross_run_stage", code)
	}
}

func TestGetRunStage_RunBoundMissingScope403(t *testing.T) {
	s, _, _, runRow, stage := runStageWaitServer(t, run.StageStateRunning)
	w := getRunStage(t, s, runRow.ID, stage.ID, -1, func(r *http.Request) *http.Request {
		return withRunBoundIdentity(r, runRow.ID) // no mcp:read
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", w.Code, w.Body.String())
	}
	if code := decodeErrCode(t, w); code != "insufficient_scope" {
		t.Errorf("code = %q, want insufficient_scope", code)
	}
}

func TestGetRunStage_OperatorIdentity200(t *testing.T) {
	s, _, _, runRow, stage := runStageWaitServer(t, run.StageStateSucceeded)
	w := getRunStage(t, s, runRow.ID, stage.ID, -1, func(r *http.Request) *http.Request {
		return withOperatorIdentity(r, "read:runs")
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
}

func TestGetRunStage_RunBoundOwnRun200(t *testing.T) {
	s, _, _, runRow, stage := runStageWaitServer(t, run.StageStateSucceeded)
	w := getRunStage(t, s, runRow.ID, stage.ID, -1, func(r *http.Request) *http.Request {
		return withRunBoundIdentity(r, runRow.ID, "mcp:read")
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	resp := decodeRunStageWait(t, w)
	if !resp.Terminal || resp.State != string(run.StageStateSucceeded) {
		t.Errorf("terminal=%v state=%q, want terminal=true state=succeeded", resp.Terminal, resp.State)
	}
}

// --- not-found / handle consistency ---

func TestGetRunStage_UnknownStage404(t *testing.T) {
	s, _, _, runRow, _ := runStageWaitServer(t, run.StageStateRunning)
	w := getRunStage(t, s, runRow.ID, uuid.New(), -1, func(r *http.Request) *http.Request {
		return withOperatorIdentity(r, "read:runs")
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", w.Code, w.Body.String())
	}
	if code := decodeErrCode(t, w); code != "stage_not_found" {
		t.Errorf("code = %q, want stage_not_found", code)
	}
}

// TestGetRunStage_StageBelongsToDifferentRun404: a stage that exists but
// whose RunID != the path run_id is 404 (the handle must be consistent).
func TestGetRunStage_StageBelongsToDifferentRun404(t *testing.T) {
	s, rr, _, _, stage := runStageWaitServer(t, run.StageStateRunning)
	// A different run in the path; the stage belongs to the seeded run.
	otherRun := rr.seedRun()
	w := getRunStage(t, s, otherRun.ID, stage.ID, -1, func(r *http.Request) *http.Request {
		return withOperatorIdentity(r, "read:runs")
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", w.Code, w.Body.String())
	}
	if code := decodeErrCode(t, w); code != "stage_not_found" {
		t.Errorf("code = %q, want stage_not_found", code)
	}
}

// --- next_action ---

// TestGetRunStage_NextActionPopulated: a drive run with a seeded
// run_auto_advanced entry carrying a next_action yields a non-nil
// next_action; a non-drive run omits it.
func TestGetRunStage_NextActionPopulated(t *testing.T) {
	s, rr, au, runRow, stage := runStageWaitServer(t, run.StageStateAwaitingApproval)
	// Mark the run drive-enabled and seed one run_auto_advanced entry.
	rr.orchestratorRepo.mu.Lock()
	runRow.Drive = true
	rr.orchestratorRepo.mu.Unlock()
	adv := drive.Advance{
		Rule: drive.RulePlanApprovedDispatch,
		From: "awaiting_approval",
		To:   "running",
		NextAction: &drive.NextAction{
			Action: "run_implement_stage",
			Detail: "dispatch the implement stage from the operator host",
		},
	}
	payload, err := json.Marshal(adv)
	if err != nil {
		t.Fatalf("marshal advance: %v", err)
	}
	au.entries = []*audit.Entry{{Sequence: 1, Timestamp: time.Now().UTC(), Category: drive.Category, Payload: payload}}

	w := getRunStage(t, s, runRow.ID, stage.ID, -1, func(r *http.Request) *http.Request {
		return withOperatorIdentity(r, "read:runs")
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", w.Code, w.Body.String())
	}
	resp := decodeRunStageWait(t, w)
	if resp.NextAction == nil || resp.NextAction.Action != "run_implement_stage" {
		t.Fatalf("next_action = %+v, want action run_implement_stage", resp.NextAction)
	}

	// A non-drive run omits next_action even with entries present.
	rr.orchestratorRepo.mu.Lock()
	runRow.Drive = false
	rr.orchestratorRepo.mu.Unlock()
	w2 := getRunStage(t, s, runRow.ID, stage.ID, -1, func(r *http.Request) *http.Request {
		return withOperatorIdentity(r, "read:runs")
	})
	resp2 := decodeRunStageWait(t, w2)
	if resp2.NextAction != nil {
		t.Errorf("next_action = %+v on non-drive run, want nil", resp2.NextAction)
	}
}

// --- bad UUID / nil deps ---

func TestGetRunStage_BadUUID400(t *testing.T) {
	s, _, _, runRow, stage := runStageWaitServer(t, run.StageStateRunning)
	cases := []struct {
		name           string
		runID, stageID string
		field          string
	}{
		{"bad run_id", "not-a-uuid", stage.ID.String(), "run_id"},
		{"bad stage_id", runRow.ID.String(), "not-a-uuid", "stage_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet,
				"/v0/runs/"+tc.runID+"/stages/"+tc.stageID, nil)
			req.SetPathValue("run_id", tc.runID)
			req.SetPathValue("stage_id", tc.stageID)
			req = withOperatorIdentity(req, "read:runs")
			w := httptest.NewRecorder()
			s.handleGetRunStage(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
			}
			if code := decodeErrCode(t, w); code != "validation_failed" {
				t.Errorf("code = %q, want validation_failed", code)
			}
		})
	}
}

func TestGetRunStage_NilRunRepo503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	w := getRunStage(t, s, uuid.New(), uuid.New(), -1, func(r *http.Request) *http.Request {
		return withOperatorIdentity(r, "read:runs")
	})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", w.Code, w.Body.String())
	}
	if code := decodeErrCode(t, w); code != "run_repo_unconfigured" {
		t.Errorf("code = %q, want run_repo_unconfigured", code)
	}
}
