package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/childcancel"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The #4186 decomposition-child cancel cascade, driven through every
// post-admission run-cancel sink. Every fixture builds on attnRunRepo (approval
// condition C1): its ListRuns honours DecomposedFrom (fakeRepo, #2546) and its
// stage table makes live_stage observable through ListStagesForRun and lets
// GET /v0/restart-blockers see undispatched_child items.

// cascadeErrRepo injects a TransitionRun error for configured CHILD ids only,
// so the parent's own transition still succeeds and the cancel returns 200
// (approval condition C1). Defined here rather than on the shared fakeRepo.
type cascadeErrRepo struct {
	*attnRunRepo
	transitionErr map[uuid.UUID]error
}

func (r *cascadeErrRepo) TransitionRun(ctx context.Context, id uuid.UUID, to run.State) (*run.Run, error) {
	if err, ok := r.transitionErr[id]; ok {
		return nil, err
	}
	return r.attnRunRepo.TransitionRun(ctx, id, to)
}

// cascadeParentID is the decomposed parent every fixture cancels.
var cascadeParentID = attnID(41860)

// cascadeChildID mints the n-th child id; cascadeStageID its implement stage.
func cascadeChildID(n int) uuid.UUID { return attnID(41870 + n) }
func cascadeStageID(n int) uuid.UUID { return attnID(41890 + n) }

// seedCascadeChild seeds child n of parentID in runState with one implement
// stage in implState.
func seedCascadeChild(rr *attnRunRepo, n int, parentID uuid.UUID, runState run.State, implState run.StageState) *run.Run {
	ru := rr.seed(cascadeChildID(n), "acme/app", runState, attnT0.Add(time.Duration(n)*time.Minute), "")
	p := parentID
	ru.DecomposedFrom = &p
	rr.addStage(ru.ID, cascadeStageID(n), run.StageTypeImplement, implState, attnT0)
	return ru
}

// newCascadeServer builds a server over rr + au with an INFO+ text logger
// captured in the returned LOCKED buffer: handleCancelRun's detached branch
// sweep logs from its own goroutine while the test reads the buffer.
func newCascadeServer(rr run.Repository, au audit.Repository) (*Server, *syncBuffer) {
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
	buf := &syncBuffer{}
	s.cfg.Logger = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return s, buf
}

// postCascadeCancel drives POST /v0/runs/{id}/cancel through the handler.
func postCascadeCancel(t *testing.T, s *Server, id uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v0/runs/%s/cancel", id), nil)
	req.SetPathValue("run_id", id.String())
	w := httptest.NewRecorder()
	s.handleCancelRun(w, withAuth(req))
	return w
}

// cascadeRow is the decoded decomposition_child_cancelled payload.
type cascadeRow struct {
	ParentRunID         string  `json:"parent_run_id"`
	ParentState         string  `json:"parent_state"`
	Reason              string  `json:"reason"`
	CancelSource        string  `json:"cancel_source"`
	FromState           string  `json:"from_state"`
	ImplementStageID    *string `json:"implement_stage_id"`
	ImplementStageState string  `json:"implement_stage_state"`
	LiveStage           bool    `json:"live_stage"`
	actor               *audit.ActorKind
}

// cascadeRowsFor returns every decomposition_child_cancelled row appended on
// runID's chain.
func cascadeRowsFor(t *testing.T, au *auditFake, runID uuid.UUID) []cascadeRow {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	var out []cascadeRow
	for _, p := range au.appended {
		if p.RunID != runID || p.Category != childcancel.Category {
			continue
		}
		var r cascadeRow
		if err := json.Unmarshal(p.Payload, &r); err != nil {
			t.Fatalf("decode %s payload: %v", childcancel.Category, err)
		}
		r.actor = p.ActorKind
		out = append(out, r)
	}
	return out
}

// countCascadeRows counts every decomposition_child_cancelled row on any chain.
func countCascadeRows(au *auditFake) int {
	au.mu.Lock()
	defer au.mu.Unlock()
	n := 0
	for _, p := range au.appended {
		if p.Category == childcancel.Category {
			n++
		}
	}
	return n
}

func runState(t *testing.T, rr run.Repository, id uuid.UUID) run.State {
	t.Helper()
	ru, err := rr.GetRun(context.Background(), id)
	if err != nil {
		t.Fatalf("GetRun %s: %v", id, err)
	}
	return ru.State
}

// assertOneCascadeRow asserts exactly one row on child's chain naming the
// parent, the reason and the source.
func assertOneCascadeRow(t *testing.T, au *auditFake, child uuid.UUID, source string) cascadeRow {
	t.Helper()
	rows := cascadeRowsFor(t, au, child)
	if len(rows) != 1 {
		t.Fatalf("child %s: %d %s rows, want 1: %+v", child, len(rows), childcancel.Category, rows)
	}
	r := rows[0]
	if r.ParentRunID != cascadeParentID.String() || r.Reason != childcancel.ReasonParentCancelled || r.CancelSource != source {
		t.Errorf("child %s row = %+v, want parent %s / %s / %s", child, r, cascadeParentID, childcancel.ReasonParentCancelled, source)
	}
	if r.actor == nil || *r.actor != audit.ActorSystem {
		t.Errorf("child %s row actor = %v, want system", child, r.actor)
	}
	return r
}

// TestCancelRun_CascadesToUndispatchedChildren: POST cancel on a decomposed
// parent cancels its three non-terminal children, one row each. A child of an
// UNRELATED parent is untouched (the DecomposedFrom filter is honoured).
// Counterfactual (1): deleting the cascade call in handleCancelRun leaves the
// children running.
func TestCancelRun_CascadesToUndispatchedChildren(t *testing.T) {
	rr := newAttnRunRepo()
	au := newAuditFake()
	rr.seed(cascadeParentID, "acme/app", run.StateRunning, attnT0, "")
	states := []run.StageState{run.StageStatePending, run.StageStateAwaitingHostDispatch, run.StageStatePending}
	for i, st := range states {
		seedCascadeChild(rr, i, cascadeParentID, run.StateRunning, st)
	}
	other := attnID(41861)
	rr.seed(other, "acme/app", run.StateRunning, attnT0, "")
	seedCascadeChild(rr, 9, other, run.StateRunning, run.StageStatePending)
	s, _ := newCascadeServer(rr, au)

	if w := postCascadeCancel(t, s, cascadeParentID); w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	for i, st := range states {
		id := cascadeChildID(i)
		if got := runState(t, rr, id); got != run.StateCancelled {
			t.Errorf("child %d state = %q, want cancelled", i, got)
		}
		r := assertOneCascadeRow(t, au, id, cancelSourceOperator)
		if r.FromState != string(run.StateRunning) || r.ParentState != string(run.StateCancelled) ||
			r.ImplementStageState != string(st) || r.LiveStage ||
			r.ImplementStageID == nil || *r.ImplementStageID != cascadeStageID(i).String() {
			t.Errorf("child %d row = %+v, want from running, parent cancelled, stage %s at %s, live false", i, r, cascadeStageID(i), st)
		}
	}
	if got := runState(t, rr, cascadeChildID(9)); got != run.StateRunning {
		t.Errorf("unrelated child state = %q, want running", got)
	}
	if n := countCascadeRows(au); n != 3 {
		t.Errorf("total cascade rows = %d, want 3", n)
	}
}

// TestCancelRun_CascadeSkipsTerminalChildren: a pre-cancelled and a succeeded
// child are not transitioned and get no row; the running control child does.
// Counterfactual (3): the PRE-CANCELLED child is the vehicle — with the
// IsTerminal skip deleted, the same-state TransitionRun succeeds and a row
// lands (no prior row, so dedup cannot mask it). The succeeded child is not a
// vehicle: TransitionRun refuses succeeded→cancelled, which already maps to
// skipped.
func TestCancelRun_CascadeSkipsTerminalChildren(t *testing.T) {
	rr := newAttnRunRepo()
	au := newAuditFake()
	rr.seed(cascadeParentID, "acme/app", run.StateRunning, attnT0, "")
	seedCascadeChild(rr, 0, cascadeParentID, run.StateCancelled, run.StageStatePending)
	seedCascadeChild(rr, 1, cascadeParentID, run.StateSucceeded, run.StageStateSucceeded)
	seedCascadeChild(rr, 2, cascadeParentID, run.StateRunning, run.StageStatePending)
	s, _ := newCascadeServer(rr, au)

	if w := postCascadeCancel(t, s, cascadeParentID); w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if n := len(cascadeRowsFor(t, au, cascadeChildID(0))); n != 0 {
		t.Errorf("pre-cancelled child rows = %d, want 0", n)
	}
	if n := len(cascadeRowsFor(t, au, cascadeChildID(1))); n != 0 {
		t.Errorf("succeeded child rows = %d, want 0", n)
	}
	if got := runState(t, rr, cascadeChildID(1)); got != run.StateSucceeded {
		t.Errorf("succeeded child state = %q, want succeeded", got)
	}
	assertOneCascadeRow(t, au, cascadeChildID(2), cancelSourceOperator)
}

// TestCancelRun_CascadeLiveRunnerChildCancelledStageUntouched: a child whose
// implement stage is running is cancelled at the run level (cancel, not
// refuse); its stage row stays running and its row carries live_stage:true.
// Counterfactual (7): forcing LiveStage false fails the live_stage assertion.
func TestCancelRun_CascadeLiveRunnerChildCancelledStageUntouched(t *testing.T) {
	rr := newAttnRunRepo()
	au := newAuditFake()
	rr.seed(cascadeParentID, "acme/app", run.StateRunning, attnT0, "")
	seedCascadeChild(rr, 0, cascadeParentID, run.StateRunning, run.StageStateRunning)
	s, _ := newCascadeServer(rr, au)

	if w := postCascadeCancel(t, s, cascadeParentID); w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if got := runState(t, rr, cascadeChildID(0)); got != run.StateCancelled {
		t.Errorf("child state = %q, want cancelled", got)
	}
	stages, _ := rr.ListStagesForRun(context.Background(), cascadeChildID(0))
	if len(stages) != 1 || stages[0].State != run.StageStateRunning {
		t.Errorf("child stages = %+v, want the implement stage still running", stages)
	}
	r := assertOneCascadeRow(t, au, cascadeChildID(0), cancelSourceOperator)
	if !r.LiveStage || r.ImplementStageState != string(run.StageStateRunning) {
		t.Errorf("row = %+v, want live_stage true at running", r)
	}
}

// TestCancelRun_ReentryConvergesOrphanedChild: a parent ALREADY cancelled with
// a still-running child (the pre-#4186 orphan shape) — re-POSTing cancel is
// the idempotent 200 and now converges the child. Counterfactual (8): gating
// the cascade on the transition having changed state leaves the child running.
func TestCancelRun_ReentryConvergesOrphanedChild(t *testing.T) {
	rr := newAttnRunRepo()
	au := newAuditFake()
	rr.seed(cascadeParentID, "acme/app", run.StateCancelled, attnT0, "")
	seedCascadeChild(rr, 0, cascadeParentID, run.StateRunning, run.StageStatePending)
	s, _ := newCascadeServer(rr, au)

	if w := postCascadeCancel(t, s, cascadeParentID); w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if got := runState(t, rr, cascadeChildID(0)); got != run.StateCancelled {
		t.Errorf("child state = %q, want cancelled", got)
	}
	assertOneCascadeRow(t, au, cascadeChildID(0), cancelSourceOperator)
}

// TestCancelRun_CascadeClearsRestartBlockers: before the cancel the restart
// blockers list N undispatched_child items; after it, zero — and the children
// have LEFT the candidate scan (scanned_runs 0). The scanned_runs assertion is
// the discriminating one: since #4184 a terminal parent's children are already
// dropped from the ITEMS, so without the cascade the items would read zero but
// the children would still be scanned as running.
func TestCancelRun_CascadeClearsRestartBlockers(t *testing.T) {
	rr := newAttnRunRepo()
	au := newAuditFake()
	rr.seed(cascadeParentID, "acme/app", run.StateRunning, attnT0, "")
	const n = 3
	for i := 0; i < n; i++ {
		seedCascadeChild(rr, i, cascadeParentID, run.StateRunning, run.StageStatePending)
	}
	s, _ := newCascadeServer(rr, au)

	before := decodeRestartBlockers(t, getRestartBlockers(t, s, anonymous()))
	undispatched := 0
	for _, it := range before.Items {
		if it.Reason == restartBlockerUndispatchedChild && it.ParentRunID == cascadeParentID.String() {
			undispatched++
		}
	}
	if undispatched != n || before.ScannedRuns != n+1 {
		t.Fatalf("before: %d undispatched_child items, scanned %d; want %d / %d: %+v", undispatched, before.ScannedRuns, n, n+1, before.Items)
	}

	if w := postCascadeCancel(t, s, cascadeParentID); w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	after := decodeRestartBlockers(t, getRestartBlockers(t, s, anonymous()))
	if len(after.Items) != 0 || after.ScannedRuns != 0 {
		t.Errorf("after: items = %+v, scanned %d; want none / 0", after.Items, after.ScannedRuns)
	}
}

// cascadeBudgetRepo seeds a running decomposed parent carrying a v2 spec with
// a BLOCKING implement-stage ceiling, its implement stage (in BOTH the attn
// stage table ListStagesForRun reads and fakeRepo's table GetStage reads), and
// two non-terminal children. Approval condition C3: a DecomposedFrom-honouring
// repo, not approvalRunRepo (whose ListRuns errors).
func cascadeBudgetRepo(cost float64) (*attnRunRepo, uuid.UUID) {
	rr := newAttnRunRepo()
	parent := rr.seed(cascadeParentID, "acme/app", run.StateRunning, attnT0, "")
	parent.WorkflowSpec = sbV2ImplementSpec("2.0", "blocking")
	parent.CostUSDTotal = cost
	stageID := attnID(41880)
	st := rr.addStage(cascadeParentID, stageID, run.StageTypeImplement, run.StageStateRunning, attnT0)
	rr.mu.Lock()
	rr.stagesByRun[cascadeParentID] = append(rr.stagesByRun[cascadeParentID], st)
	rr.mu.Unlock()
	seedCascadeChild(rr, 0, cascadeParentID, run.StateRunning, run.StageStatePending)
	seedCascadeChild(rr, 1, cascadeParentID, run.StateRunning, run.StageStateRunning)
	return rr, stageID
}

// TestRunBudget_CascadesToChildren: a per-run budget breach on the parent
// cancels it and cascades with cancel_source run_budget_exceeded.
// Counterfactual (2): deleting the call in checkRunBudget leaves both children
// running.
func TestRunBudget_CascadesToChildren(t *testing.T) {
	rr, stageID := cascadeBudgetRepo(5.0)
	au := newAuditFake()
	s, _ := newCascadeServer(rr, au)
	s.cfg.MaxRunUSD = 1.0

	if !s.checkRunBudget(context.Background(), cascadeParentID, stageID) {
		t.Fatal("checkRunBudget = false, want a breach")
	}
	if got := runState(t, rr, cascadeParentID); got != run.StateCancelled {
		t.Fatalf("parent state = %q, want cancelled", got)
	}
	for i := 0; i < 2; i++ {
		if got := runState(t, rr, cascadeChildID(i)); got != run.StateCancelled {
			t.Errorf("child %d state = %q, want cancelled", i, got)
		}
		assertOneCascadeRow(t, au, cascadeChildID(i), cancelSourceRunBudget)
	}
}

// TestStageBudgetBlocking_CascadesToChildren: a BLOCKING per-stage breach on
// the parent cancels it and cascades with cancel_source stage_budget_exceeded.
// Counterfactual (2): deleting the call in checkStageBudget's blocking arm
// leaves both children running.
func TestStageBudgetBlocking_CascadesToChildren(t *testing.T) {
	rr, stageID := cascadeBudgetRepo(0)
	au := newAuditFake()
	seedCostRow(au, cascadeParentID, stageID, 3.00, "h1")
	s, _ := newCascadeServer(rr, au)

	if !s.checkStageBudget(context.Background(), cascadeParentID, stageID) {
		t.Fatal("checkStageBudget = false, want a blocking breach")
	}
	if got := runState(t, rr, cascadeParentID); got != run.StateCancelled {
		t.Fatalf("parent state = %q, want cancelled", got)
	}
	for i := 0; i < 2; i++ {
		if got := runState(t, rr, cascadeChildID(i)); got != run.StateCancelled {
			t.Errorf("child %d state = %q, want cancelled", i, got)
		}
		assertOneCascadeRow(t, au, cascadeChildID(i), cancelSourceStageBudget)
	}
}

// TestOnRunCancelled_CascadesToChildren: orchestrator.completeRun's
// stage_cancelled resolution (the PR-closed-without-merge path) reaches the
// cascade through the RunCancelledObserver seam. Counterfactual (2): deleting
// the call in OnRunCancelled leaves the child running.
func TestOnRunCancelled_CascadesToChildren(t *testing.T) {
	rr := newAttnRunRepo()
	au := newAuditFake()
	rr.seed(cascadeParentID, "acme/app", run.StateCancelled, attnT0, "")
	seedCascadeChild(rr, 0, cascadeParentID, run.StateRunning, run.StageStatePending)
	s, _ := newCascadeServer(rr, au)

	s.OnRunCancelled(context.Background(), cascadeParentID, cancelSourceStageCancelled)
	if got := runState(t, rr, cascadeChildID(0)); got != run.StateCancelled {
		t.Errorf("child state = %q, want cancelled", got)
	}
	assertOneCascadeRow(t, au, cascadeChildID(0), cancelSourceStageCancelled)
}

// TestCancelRun_NonDecomposedRunAppendsNoCascadeRow: a plain run's cancel
// lists zero children and writes no cascade row, and logs no cascade summary.
func TestCancelRun_NonDecomposedRunAppendsNoCascadeRow(t *testing.T) {
	rr := newAttnRunRepo()
	au := newAuditFake()
	plain := attnID(41862)
	rr.seed(plain, "acme/app", run.StateRunning, attnT0, "")
	rr.seed(cascadeParentID, "acme/app", run.StateRunning, attnT0, "")
	seedCascadeChild(rr, 0, cascadeParentID, run.StateRunning, run.StageStatePending)
	s, logs := newCascadeServer(rr, au)

	if w := postCascadeCancel(t, s, plain); w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if n := countCascadeRows(au); n != 0 {
		t.Errorf("cascade rows = %d, want 0", n)
	}
	if got := runState(t, rr, cascadeChildID(0)); got != run.StateRunning {
		t.Errorf("another parent's child state = %q, want running", got)
	}
	if strings.Contains(logs.String(), "decomposition child cascade") {
		t.Errorf("log carries a cascade line for a non-decomposed run:\n%s", logs.String())
	}
}

// TestCascade_ListChildrenErrorLeavesParentCancelled: a child-list failure
// leaves the parent's 200 cancelled, touches no child, and WARN-logs.
func TestCascade_ListChildrenErrorLeavesParentCancelled(t *testing.T) {
	rr := newAttnRunRepo()
	au := newAuditFake()
	rr.seed(cascadeParentID, "acme/app", run.StateRunning, attnT0, "")
	seedCascadeChild(rr, 0, cascadeParentID, run.StateRunning, run.StageStatePending)
	rr.listErr = errors.New("injected list failure")
	s, logs := newCascadeServer(rr, au)

	w := postCascadeCancel(t, s, cascadeParentID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var body runResponse
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.State != string(run.StateCancelled) {
		t.Errorf("body state = %q, want cancelled", body.State)
	}
	if got := runState(t, rr, cascadeChildID(0)); got != run.StateRunning {
		t.Errorf("child state = %q, want running (untouched)", got)
	}
	if n := countCascadeRows(au); n != 0 {
		t.Errorf("cascade rows = %d, want 0", n)
	}
	if l := logs.String(); !strings.Contains(l, "list children failed; children NOT cancelled") || !strings.Contains(l, "injected list failure") {
		t.Errorf("WARN log missing the list-failure line:\n%s", l)
	}
}

// TestCascade_ParentReadErrorWarns: the cascade's own parent re-read failing
// (the run vanished between the transition and the cascade) WARN-logs and
// cancels no child. Driven directly, since the handler's transition needs the
// same run to exist.
func TestCascade_ParentReadErrorWarns(t *testing.T) {
	rr := newAttnRunRepo()
	au := newAuditFake()
	s, logs := newCascadeServer(rr, au)
	s.cascadeCancelToDecomposedChildren(context.Background(), attnID(41869), cancelSourceOperator)
	if l := logs.String(); !strings.Contains(l, "get parent run failed; children NOT cancelled") {
		t.Errorf("WARN log missing the parent-read line:\n%s", l)
	}
	if n := countCascadeRows(au); n != 0 {
		t.Errorf("cascade rows = %d, want 0", n)
	}
}

// TestCascade_ChildTransitionErrorOthersStillCancelled: one child's transition
// failing leaves it running with no row, WARN-logs it, and the others are
// still cancelled.
func TestCascade_ChildTransitionErrorOthersStillCancelled(t *testing.T) {
	base := newAttnRunRepo()
	au := newAuditFake()
	base.seed(cascadeParentID, "acme/app", run.StateRunning, attnT0, "")
	for i := 0; i < 3; i++ {
		seedCascadeChild(base, i, cascadeParentID, run.StateRunning, run.StageStatePending)
	}
	rr := &cascadeErrRepo{attnRunRepo: base, transitionErr: map[uuid.UUID]error{cascadeChildID(1): errors.New("injected transition failure")}}
	s, logs := newCascadeServer(rr, au)

	if w := postCascadeCancel(t, s, cascadeParentID); w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if got := runState(t, rr, cascadeChildID(1)); got != run.StateRunning {
		t.Errorf("failed child state = %q, want running", got)
	}
	if n := len(cascadeRowsFor(t, au, cascadeChildID(1))); n != 0 {
		t.Errorf("failed child rows = %d, want 0", n)
	}
	for _, i := range []int{0, 2} {
		if got := runState(t, rr, cascadeChildID(i)); got != run.StateCancelled {
			t.Errorf("child %d state = %q, want cancelled", i, got)
		}
		assertOneCascadeRow(t, au, cascadeChildID(i), cancelSourceOperator)
	}
	l := logs.String()
	if !strings.Contains(l, "child failed with error") || !strings.Contains(l, cascadeChildID(1).String()) {
		t.Errorf("WARN log missing the failed child:\n%s", l)
	}
	if !strings.Contains(l, "cancelled=2") || !strings.Contains(l, "failed=1") {
		t.Errorf("INFO summary missing cancelled=2 failed=1:\n%s", l)
	}
}

// TestCascade_RacedTerminalChildSkippedNoRow: a child listed non-terminal whose
// transition the state machine refuses (it raced to succeeded) is skipped
// with no row and no WARN.
func TestCascade_RacedTerminalChildSkippedNoRow(t *testing.T) {
	base := newAttnRunRepo()
	au := newAuditFake()
	base.seed(cascadeParentID, "acme/app", run.StateRunning, attnT0, "")
	seedCascadeChild(base, 0, cascadeParentID, run.StateRunning, run.StageStateRunning)
	rr := &cascadeErrRepo{attnRunRepo: base, transitionErr: map[uuid.UUID]error{
		cascadeChildID(0): run.InvalidTransitionError{Kind: "run", From: string(run.StateSucceeded), To: string(run.StateCancelled)},
	}}
	s, logs := newCascadeServer(rr, au)

	if w := postCascadeCancel(t, s, cascadeParentID); w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if n := countCascadeRows(au); n != 0 {
		t.Errorf("cascade rows = %d, want 0", n)
	}
	l := logs.String()
	if strings.Contains(l, "with error") || !strings.Contains(l, "skipped_terminal=1") {
		t.Errorf("want no per-child WARN and skipped_terminal=1:\n%s", l)
	}
}

// TestCascade_AppendErrorChildStillCancelledWarns: the row append failing
// leaves the child cancelled (the state change landed) and WARN-logs.
func TestCascade_AppendErrorChildStillCancelledWarns(t *testing.T) {
	rr := newAttnRunRepo()
	au := newAuditFake()
	au.appendErrCategory = childcancel.Category
	rr.seed(cascadeParentID, "acme/app", run.StateRunning, attnT0, "")
	seedCascadeChild(rr, 0, cascadeParentID, run.StateRunning, run.StageStatePending)
	s, logs := newCascadeServer(rr, au)

	if w := postCascadeCancel(t, s, cascadeParentID); w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if got := runState(t, rr, cascadeChildID(0)); got != run.StateCancelled {
		t.Errorf("child state = %q, want cancelled", got)
	}
	if l := logs.String(); !strings.Contains(l, "child cancelled with error") || !strings.Contains(l, "injected append error") {
		t.Errorf("WARN log missing the append failure:\n%s", l)
	}
}

// TestCascade_DuplicateAppendDeduped: a row for (child, parent) ALREADY on the
// child's chain — the shape a racing second cancel sink leaves — collapses the
// cascade's append to no new row, on both legs. Counterfactual (4): replacing
// the deduped append with a plain AppendChained (capability leg), or mutating
// the fallback scan to always-false (fallback leg), yields TWO rows; the
// fixture pre-holds one row for the parent so that mutation is observable
// (approval condition C6).
func TestCascade_DuplicateAppendDeduped(t *testing.T) {
	prior := func(t *testing.T, au *auditFake) {
		t.Helper()
		actor := audit.ActorSystem
		payload, _ := json.Marshal(map[string]any{"parent_run_id": cascadeParentID.String(), "reason": childcancel.ReasonParentCancelled})
		if _, err := au.AppendChained(context.Background(), audit.ChainAppendParams{
			RunID: cascadeChildID(0), Category: childcancel.Category, ActorKind: &actor, Payload: payload,
		}); err != nil {
			t.Fatalf("seed prior row: %v", err)
		}
	}
	seed := func() *attnRunRepo {
		rr := newAttnRunRepo()
		rr.seed(cascadeParentID, "acme/app", run.StateRunning, attnT0, "")
		seedCascadeChild(rr, 0, cascadeParentID, run.StateRunning, run.StageStatePending)
		return rr
	}

	t.Run("capability", func(t *testing.T) {
		rr := seed()
		dau := newDedupedAuditFake()
		prior(t, dau.auditFake)
		s, _ := newCascadeServer(rr, dau)
		if w := postCascadeCancel(t, s, cascadeParentID); w.Code != http.StatusOK {
			t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
		}
		if got := runState(t, rr, cascadeChildID(0)); got != run.StateCancelled {
			t.Errorf("child state = %q, want cancelled", got)
		}
		if n := len(cascadeRowsFor(t, dau.auditFake, cascadeChildID(0))); n != 1 {
			t.Errorf("rows = %d, want 1 (deduped)", n)
		}
		if dau.dedupedCalls != 1 {
			t.Errorf("AppendChainedDeduped calls = %d, want 1 (capability path)", dau.dedupedCalls)
		}
	})
	t.Run("fallback", func(t *testing.T) {
		rr := seed()
		au := newAuditFake()
		prior(t, au)
		s, _ := newCascadeServer(rr, au)
		if w := postCascadeCancel(t, s, cascadeParentID); w.Code != http.StatusOK {
			t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
		}
		if n := len(cascadeRowsFor(t, au, cascadeChildID(0))); n != 1 {
			t.Errorf("rows = %d, want 1 (deduped by the fallback scan)", n)
		}
	})
}

// TestCascade_NilAuditRepoIsNoOp: without an audit repository the cascade does
// nothing (it could not record why it cancelled), and never panics.
func TestCascade_NilAuditRepoIsNoOp(t *testing.T) {
	rr := newAttnRunRepo()
	rr.seed(cascadeParentID, "acme/app", run.StateCancelled, attnT0, "")
	seedCascadeChild(rr, 0, cascadeParentID, run.StateRunning, run.StageStatePending)
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})
	s.cascadeCancelToDecomposedChildren(context.Background(), cascadeParentID, cancelSourceOperator)
	if got := runState(t, rr, cascadeChildID(0)); got != run.StateRunning {
		t.Errorf("child state = %q, want running (no audit repo, no cascade)", got)
	}
}
