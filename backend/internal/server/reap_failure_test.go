package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// reapServer wires a server with the shared orchestratorRepo fake + a real
// orchestrator (so Advance's run-terminal walk is observable) and an auditFake.
// It seeds a run with a single stage in the given state and returns the pieces
// the reap-failure tests assert on.
func reapServer(t *testing.T, stageState run.StageState) (*Server, *orchestratorRepo, *auditFake, uuid.UUID, uuid.UUID) {
	t.Helper()
	rr := newOrchestratorRepo()
	au := newAuditFake()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 0, stageState)
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		AuditRepo:    au,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})
	return s, rr, au, runRow.ID, stage.ID
}

// withReapOperator injects an operator token identity carrying write:runs — the
// scope the reap-failure endpoint requires.
func withReapOperator(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
		Subject: "github:ops", TokenID: "tok-op", Scopes: []string{"write:runs"},
	}))
}

// postReapFailure posts a reap-failure request with the given identity mutator
// and typed body.
func postReapFailure(t *testing.T, s *Server, runID, stageID uuid.UUID, body reapFailureRequest,
	withID func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	return postReapFailureRaw(t, s, runID, stageID, raw, withID)
}

func postReapFailureRaw(t *testing.T, s *Server, runID, stageID uuid.UUID, raw []byte,
	withID func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/v0/runs/"+runID.String()+"/stages/"+stageID.String()+"/reap-failure", bytes.NewReader(raw))
	req.SetPathValue("run_id", runID.String())
	req.SetPathValue("stage_id", stageID.String())
	w := httptest.NewRecorder()
	s.handleReapStageFailure(w, withID(req))
	return w
}

// reapAudit returns the dispatch_reaper_failed entries appended during a test.
func reapAudit(au *auditFake) []audit.ChainAppendParams {
	au.mu.Lock()
	defer au.mu.Unlock()
	var out []audit.ChainAppendParams
	for i := range au.appended {
		if au.appended[i].Category == CategoryDispatchReaperFailed {
			out = append(out, au.appended[i])
		}
	}
	return out
}

// (a) Happy path: category C on a dispatched stage → failed with FailureC,
// exactly one dispatch_reaper_failed audit entry naming the reason, Advance
// invoked (the run walks to failed), {transitioned:true}.
func TestReapStageFailure_HappyPathCategoryC(t *testing.T) {
	s, rr, au, runID, stageID := reapServer(t, run.StageStateDispatched)

	w := postReapFailure(t, s, runID, stageID,
		reapFailureRequest{Category: "C", Reason: "acceptance_preview_provision_failed", Detail: "no port", ExitCode: 3},
		withReapOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	var resp reapFailureResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Transitioned {
		t.Error("transitioned = false, want true")
	}
	if resp.StageState != string(run.StageStateFailed) {
		t.Errorf("stage_state = %q, want failed", resp.StageState)
	}

	// Stage is failed with category C.
	cur, _ := rr.GetStage(context.Background(), stageID)
	if cur.State != run.StageStateFailed {
		t.Errorf("stage state = %q, want failed", cur.State)
	}
	if cur.FailureCategory == nil || *cur.FailureCategory != run.FailureC {
		t.Errorf("failure category = %v, want C", cur.FailureCategory)
	}

	// Exactly one dispatch_reaper_failed audit entry naming the reason, actor system.
	entries := reapAudit(au)
	if len(entries) != 1 {
		t.Fatalf("dispatch_reaper_failed entries = %d, want 1", len(entries))
	}
	if entries[0].ActorKind == nil || *entries[0].ActorKind != audit.ActorSystem {
		t.Errorf("actor kind = %v, want system", entries[0].ActorKind)
	}
	var payload struct {
		Reason          string `json:"reason"`
		Detail          string `json:"detail"`
		ExitCode        int    `json:"exit_code"`
		FailureCategory string `json:"failure_category"`
	}
	if err := json.Unmarshal(entries[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Reason != "acceptance_preview_provision_failed" {
		t.Errorf("payload reason = %q", payload.Reason)
	}
	if payload.Detail != "no port" || payload.ExitCode != 3 || payload.FailureCategory != "C" {
		t.Errorf("payload = %+v", payload)
	}

	// Advance invoked: the run's only stage is now failed, so the orchestrator
	// walked the run to failed. This is the observable that Advance ran.
	curRun, _ := rr.GetRun(context.Background(), runID)
	if curRun.State != run.StateFailed {
		t.Errorf("run state = %q, want failed (Advance invoked)", curRun.State)
	}
}

// Regression (core done-means): once the stage is failed, retry_stage is
// applicable — failed → pending is a valid retry transition (category C is
// retryable). Before the fix the stage stayed 'dispatched' and retry 422'd.
func TestReapStageFailure_RetryApplicableAfterFail(t *testing.T) {
	s, rr, _, runID, stageID := reapServer(t, run.StageStateDispatched)

	w := postReapFailure(t, s, runID, stageID,
		reapFailureRequest{Category: "C", Reason: "boom"}, withReapOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	cur, _ := rr.GetStage(context.Background(), stageID)
	if cur.State != run.StageStateFailed {
		t.Fatalf("stage state = %q, want failed", cur.State)
	}
	if !run.ValidStageRetryTransition(cur.State, run.StageStatePending) {
		t.Error("retry_stage not applicable after reap; failed → pending must be a valid retry transition")
	}
}

// (b) Already-terminal stage → 200 {transitioned:false}, NO new audit, Advance
// NOT invoked (the run stays running).
func TestReapStageFailure_AlreadyTerminalNoOp(t *testing.T) {
	s, rr, au, runID, stageID := reapServer(t, run.StageStateSucceeded)

	w := postReapFailure(t, s, runID, stageID,
		reapFailureRequest{Category: "C", Reason: "late report"}, withReapOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp reapFailureResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Transitioned {
		t.Error("transitioned = true, want false for an already-terminal stage")
	}
	if resp.StageState != string(run.StageStateSucceeded) {
		t.Errorf("stage_state = %q, want succeeded (unchanged)", resp.StageState)
	}
	if got := reapAudit(au); len(got) != 0 {
		t.Errorf("dispatch_reaper_failed entries = %d, want 0 (no-op)", len(got))
	}
	// Advance NOT invoked: the run is untouched.
	curRun, _ := rr.GetRun(context.Background(), runID)
	if curRun.State != run.StateRunning {
		t.Errorf("run state = %q, want running (Advance not invoked)", curRun.State)
	}
	// Stage state unchanged.
	cur, _ := rr.GetStage(context.Background(), stageID)
	if cur.State != run.StageStateSucceeded {
		t.Errorf("stage state = %q, want succeeded (unchanged)", cur.State)
	}
}

// raceReapRepo simulates the double-report / watchdog race the pre-check alone
// can't cover: a report passes the non-terminal pre-check, but by the time
// FailStage attempts the transition another writer (a concurrent reap report or
// the dispatch watchdog / runner's own terminal report) has already driven the
// stage terminal. It flips the target stage to succeeded on the FIRST
// TransitionStageFrom attempt (FailStage's CAS path), then delegates — so the
// embedded repo refuses the move with StageStateChangedError, exactly as the
// real row-locked CAS refuses a stage another writer already settled.
type raceReapRepo struct {
	*orchestratorRepo
	stageID uuid.UUID
	flipped bool
}

func (r *raceReapRepo) TransitionStageFrom(ctx context.Context, id uuid.UUID, from, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	if !r.flipped && id == r.stageID {
		r.flipped = true
		r.mu.Lock()
		if st := r.stagesByID[id]; st != nil {
			st.State = run.StageStateSucceeded // the concurrent winner already settled it
		}
		r.mu.Unlock()
	}
	return r.orchestratorRepo.TransitionStageFrom(ctx, id, from, to, c)
}

// (b2) Concurrent-terminal race: the pre-check sees a non-terminal stage, but a
// concurrent writer drives it terminal before FailStage's transition lands. The
// loser must still return the benign {transitioned:false} no-op — NOT a 500 —
// with NO audit entry and NO advance. Guards the idempotency race the plain
// already-terminal test (which only exercises the non-racy pre-check) misses.
func TestReapStageFailure_ConcurrentTerminalRace(t *testing.T) {
	rr := newOrchestratorRepo()
	au := newAuditFake()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 0, run.StageStateDispatched)
	race := &raceReapRepo{orchestratorRepo: rr, stageID: stage.ID}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      race,
		AuditRepo:    au,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})

	w := postReapFailure(t, s, runRow.ID, stage.ID,
		reapFailureRequest{Category: "C", Reason: "loser"}, withReapOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (benign no-op, not 500):\n%s", w.Code, w.Body.String())
	}
	var resp reapFailureResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Transitioned {
		t.Error("transitioned = true, want false for a stage won by a concurrent writer")
	}
	if resp.StageState != string(run.StageStateSucceeded) {
		t.Errorf("stage_state = %q, want succeeded (the winner's terminal state)", resp.StageState)
	}
	// No dispatch_reaper_failed audit entry: the loser wrote nothing.
	if got := reapAudit(au); len(got) != 0 {
		t.Errorf("dispatch_reaper_failed entries = %d, want 0 (loser no-op)", len(got))
	}
	// Advance NOT invoked by the loser: the run is untouched by this call.
	curRun, _ := rr.GetRun(context.Background(), runRow.ID)
	if curRun.State != run.StateRunning {
		t.Errorf("run state = %q, want running (loser did not advance)", curRun.State)
	}
}

// (b3) Protected-park no-op (#1891): a report against an awaiting_children stage
// is a benign no-op — that state is a live decomposition park owned by its
// children, and failing it would destroy the fan-in park a doomed mis-dispatched
// runner never owned. 200 {transitioned:false}, stage unchanged, NO audit, NO
// advance.
func TestReapStageFailure_AwaitingChildrenNoOp(t *testing.T) {
	s, rr, au, runID, stageID := reapServer(t, run.StageStateAwaitingChildren)

	w := postReapFailure(t, s, runID, stageID,
		reapFailureRequest{Category: "C", Reason: "doomed spawn against a decomposed parent"},
		withReapOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp reapFailureResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Transitioned {
		t.Error("transitioned = true, want false for an awaiting_children park")
	}
	if resp.StageState != string(run.StageStateAwaitingChildren) {
		t.Errorf("stage_state = %q, want awaiting_children (unchanged)", resp.StageState)
	}
	// No audit entry: the park was preserved, nothing failed.
	if got := reapAudit(au); len(got) != 0 {
		t.Errorf("dispatch_reaper_failed entries = %d, want 0 (park preserved)", len(got))
	}
	// Advance NOT invoked: the run and stage are untouched.
	cur, _ := rr.GetStage(context.Background(), stageID)
	if cur.State != run.StageStateAwaitingChildren {
		t.Errorf("stage state = %q, want awaiting_children (park preserved)", cur.State)
	}
	curRun, _ := rr.GetRun(context.Background(), runID)
	if curRun.State != run.StateRunning {
		t.Errorf("run state = %q, want running (Advance not invoked)", curRun.State)
	}
}

// parkRaceReapRepo models the concurrent-fanout race: a report passes the
// non-terminal, non-awaiting_children pre-check, but by the time FailStage
// attempts its transition a concurrent fanout has PARKED the stage
// awaiting_children. It flips the target stage to awaiting_children on the first
// TransitionStageFrom attempt (FailStage's CAS path), then delegates — so the
// row-locked compare-and-swap refuses the move with StageStateChangedError
// exactly as the real repo would, driving the handler's post-FailStage re-load
// branch. (This is the same interleaving as TestReapStageFailure_ParkLandsAfter-
// FailStageLoad; kept as a pre-existing regression pin.)
type parkRaceReapRepo struct {
	*orchestratorRepo
	stageID uuid.UUID
	flipped bool
}

func (r *parkRaceReapRepo) TransitionStageFrom(ctx context.Context, id uuid.UUID, from, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	if !r.flipped && id == r.stageID {
		r.flipped = true
		r.mu.Lock()
		if st := r.stagesByID[id]; st != nil {
			st.State = run.StageStateAwaitingChildren // a concurrent fanout parked it
		}
		r.mu.Unlock()
	}
	return r.orchestratorRepo.TransitionStageFrom(ctx, id, from, to, c)
}

// (b4) Post-FailStage park race: the pre-check sees a dispatched (non-terminal,
// non-park) stage, but a concurrent fanout parks it awaiting_children before
// FailStage's transition lands. The re-load must return the benign
// {transitioned:false} no-op — NOT a 500 and NOT a destroyed park — with NO
// audit entry and NO advance.
func TestReapStageFailure_ConcurrentParkRace(t *testing.T) {
	rr := newOrchestratorRepo()
	au := newAuditFake()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 0, run.StageStateDispatched)
	race := &parkRaceReapRepo{orchestratorRepo: rr, stageID: stage.ID}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      race,
		AuditRepo:    au,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})

	w := postReapFailure(t, s, runRow.ID, stage.ID,
		reapFailureRequest{Category: "C", Reason: "raced by a fanout park"}, withReapOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (benign no-op, not 500):\n%s", w.Code, w.Body.String())
	}
	var resp reapFailureResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Transitioned {
		t.Error("transitioned = true, want false for a stage parked by a concurrent fanout")
	}
	if resp.StageState != string(run.StageStateAwaitingChildren) {
		t.Errorf("stage_state = %q, want awaiting_children (the fanout's park)", resp.StageState)
	}
	if got := reapAudit(au); len(got) != 0 {
		t.Errorf("dispatch_reaper_failed entries = %d, want 0 (park preserved)", len(got))
	}
	// The park survived: the stage was not failed out from under the fanout.
	cur, _ := rr.GetStage(context.Background(), stage.ID)
	if cur.State != run.StageStateAwaitingChildren {
		t.Errorf("stage state = %q, want awaiting_children (park preserved, not failed)", cur.State)
	}
}

// parkAtLoadReapRepo models the interleaving where a concurrent fanout parks the
// stage awaiting_children exactly as the handler reads it — the fanout write
// lands DURING the handler's load GetStage (#1903, re-expressed for #2630's
// reap-scoped transition). The stage is seeded non-park; the fanout flips it to
// awaiting_children on that first (and only) load, so the handler observes the
// park and GUARD 1 refuses it before any transition is attempted. Under the
// reap-scoped CAS there is no separate second "FailStage load" to race — the
// transition anchors to the handler's observed state — so the pre-#2630
// "park-before-FailStage's-own-load" sub-window collapses into this GUARD-1
// load-visible race.
type parkAtLoadReapRepo struct {
	*orchestratorRepo
	stageID  uuid.UUID
	getCount int
}

func (r *parkAtLoadReapRepo) GetStage(ctx context.Context, id uuid.UUID) (*run.Stage, error) {
	if id == r.stageID {
		r.getCount++
		if r.getCount == 1 {
			r.mu.Lock()
			if st := r.stagesByID[id]; st != nil {
				st.State = run.StageStateAwaitingChildren
			}
			r.mu.Unlock()
		}
	}
	return r.orchestratorRepo.GetStage(ctx, id)
}

// parkAfterLoadReapRepo models the interleaving where the fanout parks the
// stage awaiting_children AFTER the handler's load but BEFORE the reap-scoped CAS
// (#1903/#2630) — the mid-transition window GUARD 2 owns. It flips the stage on
// the first TransitionStageFrom call, so the row-locked compare-and-swap
// (anchored to the observed pending state) refuses with StageStateChangedError
// and reapReanchor declines to re-anchor into the park.
type parkAfterLoadReapRepo struct {
	*orchestratorRepo
	stageID uuid.UUID
	flipped bool
}

func (r *parkAfterLoadReapRepo) TransitionStageFrom(ctx context.Context, id uuid.UUID, from, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	if !r.flipped && id == r.stageID {
		r.flipped = true
		r.mu.Lock()
		if st := r.stagesByID[id]; st != nil {
			st.State = run.StageStateAwaitingChildren
		}
		r.mu.Unlock()
	}
	return r.orchestratorRepo.TransitionStageFrom(ctx, id, from, to, c)
}

// assertBenignParkNoOp checks the reap handler returned the benign
// {transitioned:false, stage_state:awaiting_children} no-op with the park
// intact, no dispatch_reaper_failed audit entry, and no orchestrator advance.
func assertBenignParkNoOp(t *testing.T, w *httptest.ResponseRecorder, rr *orchestratorRepo, au *auditFake, runID, stageID uuid.UUID) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (benign no-op, not 500):\n%s", w.Code, w.Body.String())
	}
	var resp reapFailureResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Transitioned {
		t.Error("transitioned = true, want false for a mid-window fanout park")
	}
	if resp.StageState != string(run.StageStateAwaitingChildren) {
		t.Errorf("stage_state = %q, want awaiting_children (the fanout's park)", resp.StageState)
	}
	if got := reapAudit(au); len(got) != 0 {
		t.Errorf("dispatch_reaper_failed entries = %d, want 0 (park preserved)", len(got))
	}
	// The park survived: not failed out from under the fanout.
	cur, _ := rr.GetStage(context.Background(), stageID)
	if cur.State != run.StageStateAwaitingChildren {
		t.Errorf("stage state = %q, want awaiting_children (park preserved, not failed)", cur.State)
	}
	// Advance not invoked by the loser: the run is untouched.
	curRun, _ := rr.GetRun(context.Background(), runID)
	if curRun.State != run.StateRunning {
		t.Errorf("run state = %q, want running (Advance not invoked)", curRun.State)
	}
}

// assertReapRefusedPark is the state-parametrized sibling of assertBenignParkNoOp
// (#2630): it asserts the reap handler returned the benign
// {transitioned:false, stage_state:wantPark} no-op with the given park intact, no
// dispatch_reaper_failed audit entry, and no orchestrator advance — for ANY of
// the five protected parks, not only awaiting_children. Used by the four-park
// load-time and mid-transition guards the #2630 concern adds.
func assertReapRefusedPark(t *testing.T, w *httptest.ResponseRecorder, rr *orchestratorRepo, au *auditFake, runID, stageID uuid.UUID, wantPark run.StageState) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (benign no-op, not 500):\n%s", w.Code, w.Body.String())
	}
	var resp reapFailureResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Transitioned {
		t.Errorf("transitioned = true, want false for a stage in the %s park", wantPark)
	}
	if resp.StageState != string(wantPark) {
		t.Errorf("stage_state = %q, want %q (the park preserved)", resp.StageState, wantPark)
	}
	if got := reapAudit(au); len(got) != 0 {
		t.Errorf("dispatch_reaper_failed entries = %d, want 0 (park preserved)", len(got))
	}
	// The park survived: not failed out from under its owning resolver.
	cur, _ := rr.GetStage(context.Background(), stageID)
	if cur.State != wantPark {
		t.Errorf("stage state = %q, want %q (park preserved, not reaped)", cur.State, wantPark)
	}
	// Advance not invoked: the run is untouched.
	curRun, _ := rr.GetRun(context.Background(), runID)
	if curRun.State != run.StateRunning {
		t.Errorf("run state = %q, want running (Advance not invoked)", curRun.State)
	}
}

// (b5) Fanout park visible at the handler's load — GUARD 1 (#1903/#2630): a
// concurrent fanout parks the stage awaiting_children exactly as the handler
// reads it, so the park is present at the load. GUARD 1 refuses it with the
// benign no-op, never taking the legal awaiting_children → failed edge. This is
// the awaiting_children counterpart of the b10 load-time matrix, kept as the
// #1903 regression pin; b6 covers the mid-transition (GUARD 2) awaiting_children
// case.
func TestReapStageFailure_AwaitingChildrenParkAtLoad(t *testing.T) {
	rr := newOrchestratorRepo()
	au := newAuditFake()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 0, run.StageStatePending)
	race := &parkAtLoadReapRepo{orchestratorRepo: rr, stageID: stage.ID}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      race,
		AuditRepo:    au,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})

	w := postReapFailure(t, s, runRow.ID, stage.ID,
		reapFailureRequest{Category: "C", Reason: "fanout parked it at load"}, withReapOperator)
	assertBenignParkNoOp(t, w, rr, au, runRow.ID, stage.ID)
}

// (b6) Fanout park lands mid-transition — GUARD 2 (#1903/#2630): the handler's
// load sees pending (non-park) and passes it; the fanout parks the stage
// awaiting_children between that load and the reap-scoped CAS. The row-locked
// compare-and-swap refuses (StageStateChangedError) rather than taking the legal
// awaiting_children → failed edge; reapReanchor declines to re-anchor into the
// park and the handler's error branch re-loads to the benign no-op. This is the
// awaiting_children counterpart of the b8 FlipRefused tests (GUARD 2 for the four
// other parks), and the #1903 residual-TOCTOU pin.
func TestReapStageFailure_ParkLandsMidTransition(t *testing.T) {
	rr := newOrchestratorRepo()
	au := newAuditFake()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 0, run.StageStatePending)
	race := &parkAfterLoadReapRepo{orchestratorRepo: rr, stageID: stage.ID}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      race,
		AuditRepo:    au,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})

	w := postReapFailure(t, s, runRow.ID, stage.ID,
		reapFailureRequest{Category: "C", Reason: "parked after FailStage load"}, withReapOperator)
	assertBenignParkNoOp(t, w, rr, au, runRow.ID, stage.ID)
}

// casCountReapRepo counts the reap path's compare-and-swap calls against ONE
// target stage (id == stageID, so an unrelated stage's traffic can never inflate
// the count). It mutates NO state — the seeded anchor stays put until
// reapFailCAS's own CAS moves it — so the count is purely the SHAPE of the walk
// the reap path took: 1 for a pending anchor's single pending → failed CAS, 2
// for a walk routed through an intermediate hop.
type casCountReapRepo struct {
	*orchestratorRepo
	stageID  uuid.UUID
	casCalls int
}

func (r *casCountReapRepo) TransitionStageFrom(ctx context.Context, id uuid.UUID, from, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	if id == r.stageID {
		// Counted under the embedded mutex, released before the delegate
		// re-locks it (sync.Mutex is not reentrant) — the livelockReapRepo
		// ListStagesForRun precedent.
		r.mu.Lock()
		r.casCalls++
		r.mu.Unlock()
	}
	return r.orchestratorRepo.TransitionStageFrom(ctx, id, from, to, c)
}

// (b11) SUCCEEDING pending-anchor reap — the single-CAS shape (#2678). Every
// other test that reaches reapFailCAS with from == pending asserts a REFUSAL
// (AwaitingChildrenParkAtLoad, ParkLandsMidTransition), so the pending anchor's
// SUCCESS path — the one reapFailCAS documents as taking a distinct shape,
// because the `from == run.StageStateDispatched` pre-walk in reapFailCAS
// (reap_failure.go) is skipped for it — had no coverage at all.
//
// The outcome assertions below (200 {transitioned:true, stage_state:failed}, the
// persisted stage at failed with category C and the posted reason, exactly one
// dispatch_reaper_failed entry with ActorSystem, the run advanced to failed) are
// shape-BLIND: they hold equally for a walk that reached failed via an
// intermediate hop. What makes this test specific to the pending anchor is the
// casCalls == 1 assertion — WITHOUT it the test would pass against a 2-hop
// shape. Its counterfactual sibling is TestReapStageFailure_ParkLandsMidTransition
// (same pending anchor, opposite outcome: a park landing mid-transition refuses).
func TestReapStageFailure_PendingAnchorSingleCAS(t *testing.T) {
	rr := newOrchestratorRepo()
	au := newAuditFake()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 0, run.StageStatePending)
	counter := &casCountReapRepo{orchestratorRepo: rr, stageID: stage.ID}
	s := New(Config{
		Addr:      "127.0.0.1:0",
		RunRepo:   counter,
		AuditRepo: au,
		// The RAW rr, deliberately NOT the counter: Advance's own repo traffic
		// must never inflate casCalls, or the single-CAS assertion stops being
		// scoped to the reap path and becomes unsound.
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})

	const reason = "pending stage never spawned a runner"
	w := postReapFailure(t, s, runRow.ID, stage.ID,
		reapFailureRequest{Category: "C", Reason: reason}, withReapOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (pending is a first-class reapable anchor):\n%s", w.Code, w.Body.String())
	}
	var resp reapFailureResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Transitioned {
		t.Error("transitioned = false, want true")
	}
	if resp.StageState != string(run.StageStateFailed) {
		t.Errorf("stage_state = %q, want failed", resp.StageState)
	}

	// Persisted state, read after the call returned: failed, category C, and the
	// posted reason recorded on the stage row.
	cur, _ := rr.GetStage(context.Background(), stage.ID)
	if cur.State != run.StageStateFailed {
		t.Errorf("stage state = %q, want failed", cur.State)
	}
	if cur.FailureCategory == nil || *cur.FailureCategory != run.FailureC {
		t.Errorf("failure category = %v, want C", cur.FailureCategory)
	}
	if cur.FailureReason == nil || *cur.FailureReason != reason {
		t.Errorf("failure reason = %v, want %q", cur.FailureReason, reason)
	}

	// Exactly one dispatch_reaper_failed entry, actor system, naming the reason.
	entries := reapAudit(au)
	if len(entries) != 1 {
		t.Fatalf("dispatch_reaper_failed entries = %d, want 1", len(entries))
	}
	if entries[0].ActorKind == nil || *entries[0].ActorKind != audit.ActorSystem {
		t.Errorf("actor kind = %v, want system", entries[0].ActorKind)
	}
	var payload struct {
		Reason          string `json:"reason"`
		FailureCategory string `json:"failure_category"`
	}
	if err := json.Unmarshal(entries[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Reason != reason || payload.FailureCategory != "C" {
		t.Errorf("payload = %+v, want reason %q / category C", payload, reason)
	}

	// Advance ran: the run's only stage is failed, so the run walked to failed.
	curRun, _ := rr.GetRun(context.Background(), runRow.ID)
	if curRun.State != run.StateFailed {
		t.Errorf("run state = %q, want failed (Advance invoked)", curRun.State)
	}

	// THE DISTINGUISHING ASSERTION: the pending anchor takes ONE CAS. A 2 means
	// the `from == dispatched` pre-walk was taken for a pending anchor — the
	// stage was routed through an intermediate hop rather than the single
	// pending → failed edge the shape documents.
	rr.mu.Lock()
	got := counter.casCalls
	rr.mu.Unlock()
	if got != 1 {
		t.Errorf("TransitionStageFrom calls on the target stage = %d, want 1 "+
			"(a pending anchor skips the dispatched pre-walk; %d means the reap "+
			"routed the stage through an intermediate hop)", got, got)
	}
}

// midFlightFlipReapRepo models a concurrent state flip landing between the
// handler's load and the reap transition's CAS. It flips the target stage to
// flipTo on the FIRST TransitionStageFrom attempt, then delegates — so that CAS
// refuses with StageStateChangedError, driving reapFailCAS's re-anchor loop. The
// OUTCOME depends on flipTo:
//   - a reapable state (dispatched → running): the loop ABSORBS the benign
//     advance and lands failed (#1907, still the reap contract for reapable
//     flips — see b7).
//   - a protected park (running → awaiting_approval/input/scope_decision): the
//     loop REFUSES to re-anchor into the park and surfaces the typed refusal, so
//     the handler re-loads to the benign no-op with the park intact (#2630 GUARD
//     2 — see the FlipRefused tests). This INVERTS the pre-#2630 behavior, where
//     run.FailStage's re-anchor absorbed the flip and reaped the park.
type midFlightFlipReapRepo struct {
	*orchestratorRepo
	stageID uuid.UUID
	flipTo  run.StageState
	flipped bool
}

func (r *midFlightFlipReapRepo) TransitionStageFrom(ctx context.Context, id uuid.UUID, from, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	if !r.flipped && id == r.stageID {
		r.flipped = true
		r.mu.Lock()
		if st := r.stagesByID[id]; st != nil {
			st.State = r.flipTo
		}
		r.mu.Unlock()
	}
	return r.orchestratorRepo.TransitionStageFrom(ctx, id, from, to, c)
}

// assertAbsorbedFailure checks the reap handler absorbed a benign mid-flight
// advance: 200 {transitioned:true, stage_state:failed}, the stage lands failed,
// exactly one dispatch_reaper_failed audit entry, and Advance ran (the run's
// only stage is failed, so it walked the run to failed).
func assertAbsorbedFailure(t *testing.T, w *httptest.ResponseRecorder, rr *orchestratorRepo, au *auditFake, runID, stageID uuid.UUID) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (advance absorbed, not 500):\n%s", w.Code, w.Body.String())
	}
	var resp reapFailureResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Transitioned {
		t.Error("transitioned = false, want true (mid-flight advance absorbed)")
	}
	if resp.StageState != string(run.StageStateFailed) {
		t.Errorf("stage_state = %q, want failed", resp.StageState)
	}
	cur, _ := rr.GetStage(context.Background(), stageID)
	if cur.State != run.StageStateFailed {
		t.Errorf("stage state = %q, want failed", cur.State)
	}
	if got := reapAudit(au); len(got) != 1 {
		t.Errorf("dispatch_reaper_failed entries = %d, want 1", len(got))
	}
	curRun, _ := rr.GetRun(context.Background(), runID)
	if curRun.State != run.StateFailed {
		t.Errorf("run state = %q, want failed (Advance invoked)", curRun.State)
	}
}

// (b7) Concurrent mid-flight flip absorbed (#1907, review interleaving (a)): a
// dispatched stage is advanced to running by a concurrent writer before
// FailStage's first CAS. The re-anchor loop absorbs it, so the report settles
// 200 {transitioned:true} — NOT a 500 — with one audit entry and an Advance.
func TestReapStageFailure_ConcurrentMidFlightFlipAbsorbed(t *testing.T) {
	rr := newOrchestratorRepo()
	au := newAuditFake()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 0, run.StageStateDispatched)
	race := &midFlightFlipReapRepo{orchestratorRepo: rr, stageID: stage.ID, flipTo: run.StageStateRunning}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      race,
		AuditRepo:    au,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})

	w := postReapFailure(t, s, runRow.ID, stage.ID,
		reapFailureRequest{Category: "C", Reason: "raced by a concurrent advance"}, withReapOperator)
	assertAbsorbedFailure(t, w, rr, au, runRow.ID, stage.ID)
}

// (b8) Awaiting-approval flip REFUSED — GUARD 2 (#2630; INVERTS the pre-#2630
// #1907 "absorbed" behavior this test formerly pinned). This is the
// mid-transition half of the stale-probe TOCTOU the concern names: the detached
// reaper probed 'running' at the end of its settle window and sent the strand
// report; before the reap-failure CAS lands, the stage LEGITIMATELY parks at a
// gate (running → awaiting_approval). Pre-#2630 run.FailStage's re-anchor loop
// ABSORBED the flip via the legal awaiting_approval → failed edge and REAPED the
// live gate. GUARD 2 (reapFailCAS) instead REFUSES to re-anchor into the park;
// the handler re-loads and returns the benign no-op with the gate INTACT.
//
// run.FailStage is unchanged — the approval-SLA and gate-rejection paths still
// fail an awaiting_approval stage; only the REAP path refuses it.
//
// COUNTERFACTUAL (condition 5, GUARD 2): calling run.FailStage here instead of
// failStageForReap — or reverting reapReanchor's isReapProtectedPark to run's
// awaiting_children-only check — reddens this: the gate is reaped
// (transitioned:true, stage failed, one audit entry, Advance fires). See the
// recorded output in the PR Notes. Deleting GUARD 1 (the load-time fast path)
// does NOT redden this — the park lands mid-transition, after the load — which is
// why both guards are needed and are counterfactualled separately.
func TestReapStageFailure_AwaitingApprovalFlipRefused(t *testing.T) {
	rr := newOrchestratorRepo()
	au := newAuditFake()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	race := &midFlightFlipReapRepo{orchestratorRepo: rr, stageID: stage.ID, flipTo: run.StageStateAwaitingApproval}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      race,
		AuditRepo:    au,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})

	w := postReapFailure(t, s, runRow.ID, stage.ID,
		reapFailureRequest{Category: "C", Reason: "raced by a gate opening"}, withReapOperator)
	assertReapRefusedPark(t, w, rr, au, runRow.ID, stage.ID, run.StageStateAwaitingApproval)
}

// (b8b) The other two running-reachable parks refused mid-transition — GUARD 2
// (#2630): the same interleaving as b8 for awaiting_input (a clarification park)
// and awaiting_scope_decision (a scope-completeness park), the two remaining
// states running can legally move into (see transition.go). Each must be
// preserved, not reaped. awaiting_host_dispatch is NOT reachable from
// running/dispatched (only from pending), so its guard is load-time only — see
// the reapProtectedParkLoadTime matrix.
func TestReapStageFailure_RunningParkFlipsRefused(t *testing.T) {
	for _, park := range []run.StageState{
		run.StageStateAwaitingInput,
		run.StageStateAwaitingScopeDecision,
	} {
		t.Run(string(park), func(t *testing.T) {
			rr := newOrchestratorRepo()
			au := newAuditFake()
			runRow := rr.seedRun()
			stage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
			race := &midFlightFlipReapRepo{orchestratorRepo: rr, stageID: stage.ID, flipTo: park}
			s := New(Config{
				Addr:         "127.0.0.1:0",
				RunRepo:      race,
				AuditRepo:    au,
				Orchestrator: &orchestrator.Orchestrator{Runs: rr},
			})

			w := postReapFailure(t, s, runRow.ID, stage.ID,
				reapFailureRequest{Category: "C", Reason: "raced into a " + string(park) + " park"}, withReapOperator)
			assertReapRefusedPark(t, w, rr, au, runRow.ID, stage.ID, park)
		})
	}
}

// livelockReapRepo models pathological livelock (#1907): it alternates the
// target stage between two live REAPABLE states (running ↔ dispatched) on EVERY
// TransitionStageFrom call, so no CAS attempt ever succeeds and reapFailCAS
// exhausts its bounded retries. The states must both be REAPABLE (#2630): a
// running ↔ awaiting_approval alternation — this test's pre-#2630 shape — no
// longer livelocks, because GUARD 2 refuses the park on the FIRST flip and
// returns immediately rather than re-anchoring; the exhaustion contract is only
// reachable via two states the reap re-anchor loop actually retries between.
// advanceReached records whether orchestrator.Advance ran: the handler reaches
// ListStagesForRun ONLY through Advance (its own path uses GetStage, never
// ListStagesForRun), and Advance always calls it for a non-terminal run — so a
// false flag load-bearingly proves Advance was never invoked, which the
// run-still-running check alone cannot (a live stage leaves the run running
// whether or not Advance ran).
type livelockReapRepo struct {
	*orchestratorRepo
	stageID        uuid.UUID
	advanceReached bool
}

// ListStagesForRun flags that orchestrator.Advance entered its stage walk, then
// delegates. The flag is set under the embedded mutex and released before the
// delegate re-locks it (sync.Mutex is not reentrant).
func (r *livelockReapRepo) ListStagesForRun(ctx context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	r.mu.Lock()
	r.advanceReached = true
	r.mu.Unlock()
	return r.orchestratorRepo.ListStagesForRun(ctx, runID)
}

func (r *livelockReapRepo) TransitionStageFrom(ctx context.Context, id uuid.UUID, from, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	if id == r.stageID {
		r.mu.Lock()
		if st := r.stagesByID[id]; st != nil {
			if st.State == run.StageStateRunning {
				st.State = run.StageStateDispatched
			} else {
				st.State = run.StageStateRunning
			}
		}
		r.mu.Unlock()
	}
	return r.orchestratorRepo.TransitionStageFrom(ctx, id, from, to, c)
}

// (b9) Livelock exhaustion → 500 (#1907, preserved under #2630): reapFailCAS's
// re-anchor loop never converges because a concurrent writer flips the stage
// between two live REAPABLE states (running ↔ dispatched) before every CAS. The
// report must return 500 internal_error — the documented, retryable exhaustion
// contract — with NO dispatch_reaper_failed audit entry and NO Advance (the stage
// is still live and non-park, so the re-load does not classify it benign). The
// flip states are both reapable by design: a park flip would be REFUSED on the
// first attempt (GUARD 2) and never reach exhaustion.
func TestReapStageFailure_LivelockExhaustion500(t *testing.T) {
	rr := newOrchestratorRepo()
	au := newAuditFake()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	race := &livelockReapRepo{orchestratorRepo: rr, stageID: stage.ID}
	s := New(Config{
		Addr:      "127.0.0.1:0",
		RunRepo:   race,
		AuditRepo: au,
		// Route Advance through race so its entry is observable via the
		// advanceReached spy — the run-still-running check below cannot see it.
		Orchestrator: &orchestrator.Orchestrator{Runs: race},
	})

	w := postReapFailure(t, s, runRow.ID, stage.ID,
		reapFailureRequest{Category: "C", Reason: "perpetual livelock"}, withReapOperator)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (documented exhaustion contract):\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("internal_error")) {
		t.Errorf("body missing internal_error: %s", w.Body.String())
	}
	// No audit entry and no Advance: the stage never reached failed. The
	// advanceReached spy is the load-bearing no-Advance proof — the stage is
	// deliberately left live, so the run stays running whether or not Advance
	// ran, and the run-state check below cannot distinguish the two.
	if got := reapAudit(au); len(got) != 0 {
		t.Errorf("dispatch_reaper_failed entries = %d, want 0 (nothing failed)", len(got))
	}
	if race.advanceReached {
		t.Errorf("orchestrator.Advance was invoked, want NOT invoked (500 exhaustion path)")
	}
	curRun, _ := rr.GetRun(context.Background(), runRow.ID)
	if curRun.State != run.StateRunning {
		t.Errorf("run state = %q, want running (stage still live)", curRun.State)
	}
}

// (b10) Protected-park LOAD-TIME matrix — GUARD 1 (#2630): a stage ALREADY in
// each protected park when the reap-failure POST lands is a benign no-op. This is
// the concern's primary ordering — the detached reaper probed 'running' at the
// end of its settle window, the stage then parked, and by the time the POST
// reaches the handler the park is visible at load. It also completes the
// enumeration the blocking criterion names: awaiting_children was already
// protected; this adds awaiting_approval, awaiting_input, awaiting_scope_decision,
// and awaiting_host_dispatch — the last reachable ONLY at load (never mid-flight
// from running/dispatched), so this matrix is its sole guard.
//
// COUNTERFACTUAL (condition 5, GUARD 1): removing the isReapProtectedPark
// fast-path check reddens the four NON-children rows here — run.FailStage /
// failStageForReap would drive each park to failed (transitioned:true). The
// awaiting_children row stays green even without GUARD 1 (failStageForReap's
// up-front load refuses it), which is why the load-time counterfactual is
// measured on the four added parks. See the recorded output in the PR Notes.
// Deleting GUARD 2 (the mid-transition refusal) does NOT redden this matrix — the
// park is already present at load — which is how the two guards are distinguished.
func TestReapStageFailure_ProtectedParkLoadTimeMatrix(t *testing.T) {
	for _, park := range []run.StageState{
		run.StageStateAwaitingChildren,
		run.StageStateAwaitingApproval,
		run.StageStateAwaitingInput,
		run.StageStateAwaitingScopeDecision,
		run.StageStateAwaitingHostDispatch,
	} {
		t.Run(string(park), func(t *testing.T) {
			s, rr, au, runID, stageID := reapServer(t, park)
			w := postReapFailure(t, s, runID, stageID,
				reapFailureRequest{Category: "C", Reason: "reap landed on a live " + string(park) + " park"}, withReapOperator)
			assertReapRefusedPark(t, w, rr, au, runID, stageID, park)
		})
	}
}

// (b11) Park arrives BETWEEN reap-report retry attempts — GUARD 1 (#2630,
// condition 4). The detached reaper's report POST is retried when an earlier
// attempt fails in transit (reportDetachedFailureWithRetry). Between attempts the
// stage parks at a gate. Because GUARD 1 re-reads the CURRENT state on EVERY POST,
// the retry that finally lands is a benign no-op — the gate is not reaped even
// though the runner's probe (and any earlier attempt) saw 'running'. This pins
// that the guard is per-POST, not a one-shot admission check.
func TestReapStageFailure_ParkArrivesBetweenRetryAttempts(t *testing.T) {
	s, rr, au, runID, stageID := reapServer(t, run.StageStateRunning)

	// The first report attempt failed in transit (never reached the server) while
	// the stage was still 'running'. Before the retry lands, the stage parks at a
	// gate — modeled by a direct state write, the same interleaving the fanout
	// fakes use.
	rr.mu.Lock()
	rr.stagesByID[stageID].State = run.StageStateAwaitingApproval
	rr.mu.Unlock()

	// The retry POST finally lands, now against the parked gate.
	w := postReapFailure(t, s, runID, stageID,
		reapFailureRequest{Category: "C", Reason: "retry after the gate opened"}, withReapOperator)
	assertReapRefusedPark(t, w, rr, au, runID, stageID, run.StageStateAwaitingApproval)
}

// (c) Invalid category (A) → 400. An empty category is covered by the sub-test.
func TestReapStageFailure_InvalidCategory(t *testing.T) {
	for _, cat := range []string{"A", ""} {
		t.Run("category="+cat, func(t *testing.T) {
			s, _, au, runID, stageID := reapServer(t, run.StageStateDispatched)
			w := postReapFailure(t, s, runID, stageID,
				reapFailureRequest{Category: cat, Reason: "x"}, withReapOperator)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
			}
			if !bytes.Contains(w.Body.Bytes(), []byte("validation_failed")) {
				t.Errorf("body missing validation_failed: %s", w.Body.String())
			}
			if len(reapAudit(au)) != 0 {
				t.Error("audit written despite invalid category")
			}
		})
	}
}

// (d) Empty reason → 400.
func TestReapStageFailure_EmptyReason(t *testing.T) {
	s, _, au, runID, stageID := reapServer(t, run.StageStateDispatched)
	w := postReapFailure(t, s, runID, stageID,
		reapFailureRequest{Category: "C", Reason: "   "}, withReapOperator)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("validation_failed")) {
		t.Errorf("body missing validation_failed: %s", w.Body.String())
	}
	if len(reapAudit(au)) != 0 {
		t.Error("audit written despite empty reason")
	}
}

// (e) Bearer without write:runs → 403.
func TestReapStageFailure_MissingScope(t *testing.T) {
	s, _, au, runID, stageID := reapServer(t, run.StageStateDispatched)
	withScopeless := func(req *http.Request) *http.Request {
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
			Subject: "github:ops", TokenID: "tok-x", Scopes: []string{"read:runs"},
		}))
	}
	w := postReapFailure(t, s, runID, stageID,
		reapFailureRequest{Category: "C", Reason: "x"}, withScopeless)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("insufficient_scope")) {
		t.Errorf("body missing insufficient_scope: %s", w.Body.String())
	}
	if len(reapAudit(au)) != 0 {
		t.Error("audit written despite missing scope")
	}
}

// Anonymous → 401 authentication_required (the auth ladder's first rung).
func TestReapStageFailure_Anonymous(t *testing.T) {
	s, _, _, runID, stageID := reapServer(t, run.StageStateDispatched)
	withAnon := func(req *http.Request) *http.Request { return req } // no identity in context
	w := postReapFailure(t, s, runID, stageID,
		reapFailureRequest{Category: "C", Reason: "x"}, withAnon)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("authentication_required")) {
		t.Errorf("body missing authentication_required: %s", w.Body.String())
	}
}

// (f) stage_id not in run → 404 stage_not_found.
func TestReapStageFailure_StageNotInRun(t *testing.T) {
	s, _, au, _, stageID := reapServer(t, run.StageStateDispatched)
	otherRun := uuid.New() // does not match the seeded stage's run
	w := postReapFailure(t, s, otherRun, stageID,
		reapFailureRequest{Category: "C", Reason: "x"}, withReapOperator)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("stage_not_found")) {
		t.Errorf("body missing stage_not_found: %s", w.Body.String())
	}
	if len(reapAudit(au)) != 0 {
		t.Error("audit written despite handle mismatch")
	}
}

// Unknown stage → 404 stage_not_found.
func TestReapStageFailure_StageNotFound(t *testing.T) {
	s, _, _, runID, _ := reapServer(t, run.StageStateDispatched)
	w := postReapFailure(t, s, runID, uuid.New(),
		reapFailureRequest{Category: "C", Reason: "x"}, withReapOperator)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("stage_not_found")) {
		t.Errorf("body missing stage_not_found: %s", w.Body.String())
	}
}

// Malformed body → 400 validation_failed.
func TestReapStageFailure_MalformedBody(t *testing.T) {
	s, _, au, runID, stageID := reapServer(t, run.StageStateDispatched)
	w := postReapFailureRaw(t, s, runID, stageID, []byte("{not json"), withReapOperator)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("validation_failed")) {
		t.Errorf("body missing validation_failed: %s", w.Body.String())
	}
	if len(reapAudit(au)) != 0 {
		t.Error("audit written despite malformed body")
	}
}

// Unknown-field body → 400 (DisallowUnknownFields).
func TestReapStageFailure_UnknownField(t *testing.T) {
	s, _, _, runID, stageID := reapServer(t, run.StageStateDispatched)
	w := postReapFailureRaw(t, s, runID, stageID,
		[]byte(`{"category":"C","reason":"x","bogus":1}`), withReapOperator)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("validation_failed")) {
		t.Errorf("body missing validation_failed: %s", w.Body.String())
	}
}

// Unconfigured (nil RunRepo/AuditRepo) → 503.
func TestReapStageFailure_Unconfigured(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"}) // no RunRepo / AuditRepo
	w := postReapFailure(t, s, uuid.New(), uuid.New(),
		reapFailureRequest{Category: "C", Reason: "x"}, withReapOperator)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("reap_failure_unconfigured")) {
		t.Errorf("body missing reap_failure_unconfigured: %s", w.Body.String())
	}
}

// --- OPTIONAL expected_state precondition (E67.51 / #2699) ---
//
// The conditional reap closes the check/use race fishhawk_reap_stage can only
// narrow client-side: the caller pins the state IT observed, and the server
// refuses — atomically, at the row-locked CAS — if the stage is not (or no longer)
// in it. Every test below seeds its bad state BY CONSTRUCTION (a directly-written
// state or a fake that flips under a numbered CAS), never by calling the control
// inside its own setup.

// condReq builds a CONDITIONAL request body pinning the given state. Taking a
// *string is the point: the presence of the pointer is what makes the call
// conditional, so a test can express "explicitly empty" as ptr("").
func condReq(category, reason string, expected *string) reapFailureRequest {
	return reapFailureRequest{Category: category, Reason: reason, ExpectedState: expected}
}

func strptr(s string) *string { return &s }

// decodeReapError decodes the handler's error envelope so a test can assert the
// CODE and the details map rather than substring-matching the body.
func decodeReapError(t *testing.T, w *httptest.ResponseRecorder) (string, map[string]any) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body %s)", err, w.Body.String())
	}
	return env.Error.Code, env.Error.Details
}

// (n1) CONDITIONAL HAPPY PATH — a dispatched stage pinned to 'dispatched' reaps
// exactly as an unconditional call does, and the audit payload RECORDS the pin.
// Paired with the mismatch tests below so the conditional arm cannot be
// satisfied by a handler that refuses everything.
func TestReapStageFailure_ConditionalHappyPathDispatched(t *testing.T) {
	s, rr, au, runID, stageID := reapServer(t, run.StageStateDispatched)

	w := postReapFailure(t, s, runID, stageID,
		condReq("C", "pinned reap of a stranded dispatched stage", strptr("dispatched")),
		withReapOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp reapFailureResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Transitioned || resp.StageState != string(run.StageStateFailed) {
		t.Errorf("transitioned=%v stage_state=%q, want true/failed", resp.Transitioned, resp.StageState)
	}
	cur, _ := rr.GetStage(context.Background(), stageID)
	if cur.State != run.StageStateFailed {
		t.Errorf("stage state = %q, want failed", cur.State)
	}
	if cur.FailureCategory == nil || *cur.FailureCategory != run.FailureC {
		t.Errorf("failure category = %v, want C", cur.FailureCategory)
	}
	entries := reapAudit(au)
	if len(entries) != 1 {
		t.Fatalf("dispatch_reaper_failed entries = %d, want 1", len(entries))
	}
	var payload map[string]any
	if err := json.Unmarshal(entries[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload["expected_state"] != "dispatched" {
		t.Errorf("audit payload expected_state = %v, want dispatched (a pinned reap must be recorded as pinned)", payload["expected_state"])
	}
	curRun, _ := rr.GetRun(context.Background(), runID)
	if curRun.State != run.StateFailed {
		t.Errorf("run state = %q, want failed (Advance invoked)", curRun.State)
	}
}

// (n2) CONDITIONAL HAPPY PATH for the other common anchor — a running stage
// pinned to 'running'. Distinct from n1 because the walk shape differs (no
// dispatched pre-hop), which is exactly where the second-leg residual below does
// NOT apply.
func TestReapStageFailure_ConditionalHappyPathRunning(t *testing.T) {
	s, rr, au, runID, stageID := reapServer(t, run.StageStateRunning)

	w := postReapFailure(t, s, runID, stageID,
		condReq("C", "pinned reap of a stranded running stage", strptr("running")),
		withReapOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	cur, _ := rr.GetStage(context.Background(), stageID)
	if cur.State != run.StageStateFailed {
		t.Errorf("stage state = %q, want failed", cur.State)
	}
	if got := reapAudit(au); len(got) != 1 {
		t.Errorf("dispatch_reaper_failed entries = %d, want 1", len(got))
	}
}

// (n3) LOAD-TIME MISMATCH → 409, and NOTHING transitioned. The stage is
// 'running' and the caller pinned 'dispatched' — the reviewer's ordering where
// the concurrent dispatch's advance is already visible when the POST lands.
//
// COUNTERFACTUAL VEHICLE for the load-time precondition branch: deleting it lets
// the reap absorb the advance and fail the live stage (200 transitioned:true).
// The assertions are COMMITTED-STATE reads, not error identity alone.
func TestReapStageFailure_ConditionalMismatchAtLoad(t *testing.T) {
	s, rr, au, runID, stageID := reapServer(t, run.StageStateRunning)

	w := postReapFailure(t, s, runID, stageID,
		condReq("C", "pinned to the state the verb observed", strptr("dispatched")),
		withReapOperator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	code, details := decodeReapError(t, w)
	if code != "stage_state_precondition_failed" {
		t.Errorf("error code = %q, want stage_state_precondition_failed", code)
	}
	if details["expected_state"] != "dispatched" || details["actual_state"] != "running" {
		t.Errorf("details = %v, want expected_state=dispatched actual_state=running", details)
	}
	// COMMITTED STATE: the stage is untouched — still running, never failed.
	cur, _ := rr.GetStage(context.Background(), stageID)
	if cur.State != run.StageStateRunning {
		t.Errorf("stage state = %q, want running (a lost precondition transitions NOTHING)", cur.State)
	}
	if got := reapAudit(au); len(got) != 0 {
		t.Errorf("dispatch_reaper_failed entries = %d, want 0", len(got))
	}
	// Advance NOT invoked: the run is untouched.
	curRun, _ := rr.GetRun(context.Background(), runID)
	if curRun.State != run.StateRunning {
		t.Errorf("run state = %q, want running (Advance not invoked)", curRun.State)
	}
}

// (n4) THE DONE-MEANS TEST — the reviewer's exact interleaving, refused
// ATOMICALLY at the CAS. The stage LOADS 'running' (so the load-time check
// passes: the caller pinned 'running'), and the repo fake flips it to
// 'awaiting_deployment' under the FIRST CAS — modelling a concurrent transition
// landing in the window between the handler's load and its compare-and-swap.
// Pre-#2699 the unconditional loop would have re-anchored and reaped whatever it
// found; a conditional caller must LOSE.
//
// The assertion is a COMMITTED-STATE read: the stage must never reach 'failed'.
//
// COUNTERFACTUAL VEHICLE for the conditional single-attempt / no-re-anchor branch
// in reapFailCAS.
func TestReapStageFailure_ConditionalRefusesMidFlightAdvance(t *testing.T) {
	rr := newOrchestratorRepo()
	au := newAuditFake()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	race := &midFlightFlipReapRepo{orchestratorRepo: rr, stageID: stage.ID, flipTo: run.StageStateAwaitingDeployment}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      race,
		AuditRepo:    au,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})

	w := postReapFailure(t, s, runRow.ID, stage.ID,
		condReq("C", "pinned reap raced by a concurrent advance", strptr("running")),
		withReapOperator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (the pinned state moved under the CAS):\n%s", w.Code, w.Body.String())
	}
	code, details := decodeReapError(t, w)
	if code != "stage_state_precondition_failed" {
		t.Errorf("error code = %q, want stage_state_precondition_failed", code)
	}
	// actual_state comes from the ROW-LOCKED sce.Actual — the authoritative
	// mid-flight value, not a second stale read.
	if details["expected_state"] != "running" || details["actual_state"] != string(run.StageStateAwaitingDeployment) {
		t.Errorf("details = %v, want expected_state=running actual_state=awaiting_deployment", details)
	}
	// COMMITTED STATE — the whole point of the control: the stage was NOT reaped.
	cur, _ := rr.GetStage(context.Background(), stage.ID)
	if cur.State == run.StageStateFailed {
		t.Fatalf("stage state = failed: the conditional reap ABSORBED the concurrent advance and failed a live stage")
	}
	if cur.State != run.StageStateAwaitingDeployment {
		t.Errorf("stage state = %q, want awaiting_deployment (the concurrent writer's state, untouched)", cur.State)
	}
	if got := reapAudit(au); len(got) != 0 {
		t.Errorf("dispatch_reaper_failed entries = %d, want 0", len(got))
	}
	curRun, _ := rr.GetRun(context.Background(), runRow.ID)
	if curRun.State != run.StateRunning {
		t.Errorf("run state = %q, want running (Advance not invoked)", curRun.State)
	}
}

// (n5) The reviewer's NAMED interleaving in its original form: the stage loads
// 'dispatched' (the state the verb observed and pinned) and a concurrent
// dispatch advances it to 'running' under the FIRST CAS. Unconditionally this is
// the #1907 benign absorption — TestReapStageFailure_ConcurrentMidFlightFlipAbsorbed
// pins that it still IS, unchanged, for the detached reaper. Conditionally it
// must LOSE: that advance is exactly the "a runner appeared" signature.
func TestReapStageFailure_ConditionalDispatchedToRunningRefused(t *testing.T) {
	rr := newOrchestratorRepo()
	au := newAuditFake()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 0, run.StageStateDispatched)
	race := &midFlightFlipReapRepo{orchestratorRepo: rr, stageID: stage.ID, flipTo: run.StageStateRunning}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      race,
		AuditRepo:    au,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})

	w := postReapFailure(t, s, runRow.ID, stage.ID,
		condReq("C", "pinned dispatched, raced by a dispatch that spawned a runner", strptr("dispatched")),
		withReapOperator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	code, details := decodeReapError(t, w)
	if code != "stage_state_precondition_failed" {
		t.Errorf("error code = %q, want stage_state_precondition_failed", code)
	}
	if details["actual_state"] != string(run.StageStateRunning) {
		t.Errorf("details actual_state = %v, want running", details["actual_state"])
	}
	// COMMITTED STATE: the newly-live runner's stage was NOT failed.
	cur, _ := rr.GetStage(context.Background(), stage.ID)
	if cur.State != run.StageStateRunning {
		t.Errorf("stage state = %q, want running (the live runner's stage must survive)", cur.State)
	}
	if got := reapAudit(au); len(got) != 0 {
		t.Errorf("dispatch_reaper_failed entries = %d, want 0", len(got))
	}
}

// secondLegFlipReapRepo flips the target stage on the Nth TransitionStageFrom
// call rather than the first, so a test can inject a concurrent transition under
// a SPECIFIC leg of the dispatched → running → failed walk.
type secondLegFlipReapRepo struct {
	*orchestratorRepo
	stageID uuid.UUID
	flipOn  int
	flipTo  run.StageState
	calls   int
	// stateAtFlip is the stage's COMMITTED state read immediately before the
	// injected flip overwrote it. It is the only way to observe what the walk's
	// EARLIER legs committed, since the flip itself replaces the row's state.
	stateAtFlip run.StageState
}

func (r *secondLegFlipReapRepo) TransitionStageFrom(ctx context.Context, id uuid.UUID, from, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	if id == r.stageID {
		r.mu.Lock()
		r.calls++
		if r.calls == r.flipOn {
			if st := r.stagesByID[id]; st != nil {
				r.stateAtFlip = st.State
				st.State = r.flipTo
			}
		}
		r.mu.Unlock()
	}
	return r.orchestratorRepo.TransitionStageFrom(ctx, id, from, to, c)
}

// (n6) THE SECOND-LEG INTERLEAVING — the operator's binding CONDITION 4, pinned
// rather than papered over.
//
// A dispatched-anchored conditional reap walks dispatched → running → failed. The
// FIRST leg is the compare-and-set against the pinned state; it COMMITS. If a
// concurrent transition lands between the legs, the SECOND leg loses and the
// caller gets a 409 — with that intermediate dispatched → running hop already
// committed by the reap itself. So the blanket claim "a 409 means nothing
// transitioned" is NOT true for this interleaving, and the handler comment and
// the API docs say so.
//
// Exit from 'running' mid-walk is REACHABLE, not theoretical: transition.go's
// StageStateRunning row admits awaiting_approval, awaiting_input,
// awaiting_scope_decision, awaiting_deployment, succeeded, failed and cancelled.
// This test injects awaiting_approval under the second leg.
//
// The load-bearing assertion is still the one that matters for safety: the stage
// NEVER reaches 'failed', no audit entry is written, and no advance runs.
func TestReapStageFailure_ConditionalSecondLegFlipIs409(t *testing.T) {
	rr := newOrchestratorRepo()
	au := newAuditFake()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 0, run.StageStateDispatched)
	race := &secondLegFlipReapRepo{
		orchestratorRepo: rr, stageID: stage.ID, flipOn: 2, flipTo: run.StageStateAwaitingApproval,
	}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      race,
		AuditRepo:    au,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})

	w := postReapFailure(t, s, runRow.ID, stage.ID,
		condReq("C", "pinned dispatched, gate opened between the legs", strptr("dispatched")),
		withReapOperator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	code, details := decodeReapError(t, w)
	if code != "stage_state_precondition_failed" {
		t.Errorf("error code = %q, want stage_state_precondition_failed", code)
	}
	// The 409 carries the row-locked sce.Actual from the SECOND leg's refusal.
	if details["actual_state"] != string(run.StageStateAwaitingApproval) {
		t.Errorf("details actual_state = %v, want awaiting_approval (the row-locked mid-flight value)", details["actual_state"])
	}
	// THE SAFETY PROPERTY: never failed, nothing recorded, nothing advanced.
	cur, _ := rr.GetStage(context.Background(), stage.ID)
	if cur.State == run.StageStateFailed {
		t.Fatalf("stage state = failed: the second leg must not reap a stage that left the pinned walk")
	}
	if got := reapAudit(au); len(got) != 0 {
		t.Errorf("dispatch_reaper_failed entries = %d, want 0", len(got))
	}
	curRun, _ := rr.GetRun(context.Background(), runRow.ID)
	if curRun.State != run.StateRunning {
		t.Errorf("run state = %q, want running (Advance not invoked)", curRun.State)
	}
	// THE HONEST RESIDUAL, asserted rather than left to prose: the reap's FIRST
	// leg COMMITTED dispatched → running before the second leg lost. Read from
	// the fake's snapshot of the committed row taken immediately before the
	// injected flip overwrote it — the stage row itself cannot show this, because
	// the concurrent writer replaced its state. This is the documented narrowing
	// of the guarantee: a 409 means the stage was never FAILED, not that the walk
	// committed nothing.
	rr.mu.Lock()
	atFlip := race.stateAtFlip
	rr.mu.Unlock()
	if atFlip != run.StageStateRunning {
		t.Errorf("committed state under the second leg = %q, want running — this test no longer "+
			"exercises the SECOND leg (the walk never committed its dispatched → running hop), so "+
			"the CONDITION 4 residual it exists to pin is not being exercised", atFlip)
	}
}

// (n7) CONDITIONAL against an already-TERMINAL stage → 409, NOT the 200
// {transitioned:false} idempotent no-op. Deliberate divergence for conditional
// callers only: a caller that pinned a state wants to learn its precondition
// lost. TestReapStageFailure_AlreadyTerminalNoOp pins that UNCONDITIONAL callers
// keep the no-op.
func TestReapStageFailure_ConditionalTerminalIs409(t *testing.T) {
	s, rr, au, runID, stageID := reapServer(t, run.StageStateSucceeded)

	w := postReapFailure(t, s, runID, stageID,
		condReq("C", "pinned running, but the stage already settled", strptr("running")),
		withReapOperator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (not the unconditional 200 no-op):\n%s", w.Code, w.Body.String())
	}
	code, details := decodeReapError(t, w)
	if code != "stage_state_precondition_failed" {
		t.Errorf("error code = %q, want stage_state_precondition_failed", code)
	}
	if details["actual_state"] != string(run.StageStateSucceeded) {
		t.Errorf("details actual_state = %v, want succeeded", details["actual_state"])
	}
	cur, _ := rr.GetStage(context.Background(), stageID)
	if cur.State != run.StageStateSucceeded {
		t.Errorf("stage state = %q, want succeeded (unchanged)", cur.State)
	}
	if got := reapAudit(au); len(got) != 0 {
		t.Errorf("dispatch_reaper_failed entries = %d, want 0", len(got))
	}
}

// (n8) CONDITIONAL against EACH protected park → 409 with the park intact. One
// subtest per park, so a sixth park added later fails here too.
func TestReapStageFailure_ConditionalParkIs409(t *testing.T) {
	for _, park := range []run.StageState{
		run.StageStateAwaitingChildren,
		run.StageStateAwaitingApproval,
		run.StageStateAwaitingInput,
		run.StageStateAwaitingScopeDecision,
		run.StageStateAwaitingHostDispatch,
	} {
		t.Run(string(park), func(t *testing.T) {
			s, rr, au, runID, stageID := reapServer(t, park)
			w := postReapFailure(t, s, runID, stageID,
				condReq("C", "pinned running, but the stage parked", strptr("running")),
				withReapOperator)
			if w.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
			}
			code, details := decodeReapError(t, w)
			if code != "stage_state_precondition_failed" {
				t.Errorf("error code = %q, want stage_state_precondition_failed", code)
			}
			if details["actual_state"] != string(park) {
				t.Errorf("details actual_state = %v, want %q", details["actual_state"], park)
			}
			cur, _ := rr.GetStage(context.Background(), stageID)
			if cur.State != park {
				t.Errorf("stage state = %q, want %q (park intact)", cur.State, park)
			}
			if got := reapAudit(au); len(got) != 0 {
				t.Errorf("dispatch_reaper_failed entries = %d, want 0", len(got))
			}
		})
	}
}

// (n9) OUT-OF-SET expected_state → 400 validation_failed, with NOTHING
// transitioned — one case per class the allow-list rejects:
//
//   - a protected park          (awaiting_children)
//   - a terminal state          (succeeded)
//   - an unknown string         (bogus)
//   - the EXPLICITLY EMPTY value (""), the operator's binding CONDITION 1: the
//     field PRESENT and empty is a validation failure, never an unconditional
//     reap. This is what the *string presence check buys — collapse it back to a
//     `req.ExpectedState == ""` comparison and this row goes RED with a 200.
//
// The stage is seeded 'dispatched' so an UNVALIDATED handler would happily reap
// it; every row therefore discriminates on committed state, not just on a code.
func TestReapStageFailure_ConditionalRejectsNonAnchorState(t *testing.T) {
	for _, tc := range []struct {
		name string
		val  string
	}{
		{"protected_park", "awaiting_children"},
		{"terminal", "succeeded"},
		{"unknown", "bogus"},
		{"explicitly_empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, rr, au, runID, stageID := reapServer(t, run.StageStateDispatched)
			w := postReapFailure(t, s, runID, stageID,
				condReq("C", "x", strptr(tc.val)), withReapOperator)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for expected_state=%q:\n%s", w.Code, tc.val, w.Body.String())
			}
			code, details := decodeReapError(t, w)
			if code != "validation_failed" {
				t.Errorf("error code = %q, want validation_failed", code)
			}
			if details["field"] != "expected_state" {
				t.Errorf("details field = %v, want expected_state", details["field"])
			}
			// NOTHING transitioned: the stage is untouched and no audit entry exists.
			cur, _ := rr.GetStage(context.Background(), stageID)
			if cur.State != run.StageStateDispatched {
				t.Errorf("stage state = %q, want dispatched (a rejected precondition transitions NOTHING)", cur.State)
			}
			if got := reapAudit(au); len(got) != 0 {
				t.Errorf("dispatch_reaper_failed entries = %d, want 0", len(got))
			}
		})
	}
}

// (n10) The OTHER arm of CONDITION 1: the field OMITTED ENTIRELY is the
// unconditional request and succeeds. Paired with the explicitly_empty row of
// n9 — same handler, same seeded stage, opposite outcomes — so the two together
// prove the handler distinguishes ABSENCE from EMPTINESS rather than treating
// both as one.
//
// The body is a RAW literal with no expected_state key at all, not a typed
// struct with a nil pointer, so the wire form being tested is unambiguous.
func TestReapStageFailure_OmittedExpectedStateIsUnconditional(t *testing.T) {
	s, rr, au, runID, stageID := reapServer(t, run.StageStateDispatched)

	w := postReapFailureRaw(t, s, runID, stageID,
		[]byte(`{"category":"C","reason":"unconditional detached reaper report"}`), withReapOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (an omitted expected_state is UNCONDITIONAL):\n%s", w.Code, w.Body.String())
	}
	var resp reapFailureResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Transitioned {
		t.Error("transitioned = false, want true")
	}
	cur, _ := rr.GetStage(context.Background(), stageID)
	if cur.State != run.StageStateFailed {
		t.Errorf("stage state = %q, want failed", cur.State)
	}
	if got := reapAudit(au); len(got) != 1 {
		t.Errorf("dispatch_reaper_failed entries = %d, want 1", len(got))
	}
}

// unconditionalAuditPayloadKeys is the EXACT key set the dispatch_reaper_failed
// payload carried before #2699 — transcribed from the pre-change
// handleReapStageFailure literal, so it is a recorded observation of the old
// shape and not a re-derivation of the new one.
var unconditionalAuditPayloadKeys = []string{
	"auth_method", "detail", "exit_code", "failure_category", "reason", "reported_at", "run_id", "stage_id",
}

// (n11) The operator's binding CONDITION 2: an UNCONDITIONAL call's audit
// payload key set is unchanged, byte-for-byte, by #2699 — no
// "expected_state":"" key appears. Comparing the SORTED key set (not a
// substring) is what makes the claim a fact: adding the key unconditionally
// reddens this immediately.
func TestReapStageFailure_UnconditionalAuditPayloadKeySet(t *testing.T) {
	s, _, au, runID, stageID := reapServer(t, run.StageStateDispatched)

	w := postReapFailureRaw(t, s, runID, stageID,
		[]byte(`{"category":"C","reason":"unconditional detached reaper report","detail":"d","exit_code":9}`),
		withReapOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	entries := reapAudit(au)
	if len(entries) != 1 {
		t.Fatalf("dispatch_reaper_failed entries = %d, want 1", len(entries))
	}
	var payload map[string]any
	if err := json.Unmarshal(entries[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	got := make([]string, 0, len(payload))
	for k := range payload {
		got = append(got, k)
	}
	sort.Strings(got)
	want := append([]string(nil), unconditionalAuditPayloadKeys...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unconditional dispatch_reaper_failed payload keys = %v, want %v "+
			"(an unconditional call's payload must be unchanged by the expected_state feature)", got, want)
	}
}

// (n12) The errReapRepoNotCAS 503 wins over the conditional 409: a WIRING FAULT
// must never be reported as a lost precondition. The vehicle is the same
// interface-erasure repo the #2672 tests use (reap_cas_required_test.go), so the
// capability is erased BY CONSTRUCTION.
//
// COUNTERFACTUAL VEHICLE for the errReapRepoNotCAS-before-409 ordering.
func TestReapStageFailure_ConditionalRepoNotCASStill503(t *testing.T) {
	rr := newOrchestratorRepo()
	au := newAuditFake()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 0, run.StageStateDispatched)
	vehicle := &nonCASRunRepo{Repository: rr}
	assertVehicleErasesCAS(t, vehicle)
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      vehicle,
		AuditRepo:    au,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})

	w := postReapFailure(t, s, runRow.ID, stage.ID,
		condReq("C", "pinned reap against a mis-wired repo", strptr("dispatched")),
		withReapOperator)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (a wiring fault must not be reported as a lost precondition):\n%s",
			w.Code, w.Body.String())
	}
	code, _ := decodeReapError(t, w)
	if code != "reap_failure_repo_not_cas" {
		t.Errorf("error code = %q, want reap_failure_repo_not_cas", code)
	}
	cur, _ := rr.GetStage(context.Background(), stage.ID)
	if cur.State != run.StageStateDispatched {
		t.Errorf("stage state = %q, want dispatched (nothing transitioned)", cur.State)
	}
	if got := reapAudit(au); len(got) != 0 {
		t.Errorf("dispatch_reaper_failed entries = %d, want 0", len(got))
	}
}

// errReapRepo returns a NON-typed repo error from the CAS — neither a
// StageStateChangedError nor the not-CAS sentinel — so the conditional path's
// fall-through to the documented retryable 500 is exercised.
type errReapRepo struct {
	*orchestratorRepo
	stageID uuid.UUID
}

func (r *errReapRepo) TransitionStageFrom(ctx context.Context, id uuid.UUID, from, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	if id == r.stageID {
		return nil, errors.New("connection reset by peer")
	}
	return r.orchestratorRepo.TransitionStageFrom(ctx, id, from, to, c)
}

// (n13) A genuine (non-typed) repo error under a CONDITIONAL call still yields
// the documented retryable 500 — it is NOT laundered into a 409. Without this
// the conditional error classifier could report every failure as a lost
// precondition, which would tell an operator to re-read a stage when the real
// problem is the database.
func TestReapStageFailure_ConditionalRepoErrorStill500(t *testing.T) {
	rr := newOrchestratorRepo()
	au := newAuditFake()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      &errReapRepo{orchestratorRepo: rr, stageID: stage.ID},
		AuditRepo:    au,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})

	w := postReapFailure(t, s, runRow.ID, stage.ID,
		condReq("C", "pinned reap against a broken repo", strptr("running")),
		withReapOperator)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (a repo error is not a lost precondition):\n%s", w.Code, w.Body.String())
	}
	code, _ := decodeReapError(t, w)
	if code != "internal_error" {
		t.Errorf("error code = %q, want internal_error", code)
	}
	if got := reapAudit(au); len(got) != 0 {
		t.Errorf("dispatch_reaper_failed entries = %d, want 0", len(got))
	}
}

// reapConditionalGoldenPath is the ONE shared cross-boundary artifact (the
// operator's binding CONDITION 3). The client serializer and this handler live
// in different packages, so a literal per package could drift apart silently
// while LOOKING like a seam test; instead a SINGLE golden request body is READ
// BY BOTH SIDES. mcpserver/client_test.go asserts these exact bytes are what
// apiClient.ReportStageFailureFrom writes on the wire, and the test below feeds
// the SAME bytes through the REAL handler. A JSON-tag or field rename on either
// side reddens one of the two. Neither side may inline a copy: editing this file
// must break whichever side has drifted.
//
// ACCURACY NOTE on the import direction, because the reverse is often assumed:
// backend/internal/server does NOT import backend/internal/mcpserver — the /mcp
// route is an INJECTED seam precisely so that edge does not exist (mcproute.go
// says why: mcpserver's in-package tests drive a real server.New, so a
// server -> mcpserver import would close a cycle in mcpserver's TEST binary). So
// the direction that works is mcpserver's test binary importing server, and a
// FULL in-process end-to-end test is therefore possible from that side (see
// mcpserver/client_test.go's TestStageProgress_CrossBoundaryEndToEnd, which
// drives a real server.Handler over httptest). This golden body is the artifact
// CONDITION 3 specified, not a workaround for an impossible test; the stronger
// end-to-end variant is called out in the PR body as an available follow-up.
const reapConditionalGoldenPath = "testdata/reap_conditional_request.json"

// (n14) The server half of the golden-body seam. The stage is seeded 'running'
// while the golden body pins 'dispatched', so a 409 is only reachable if the
// handler actually DECODED expected_state and took the CONDITIONAL path — an
// unconditional decode would reap the stage and answer 200.
func TestReapStageFailure_GoldenConditionalBodyTakesConditionalPath(t *testing.T) {
	raw, err := os.ReadFile(reapConditionalGoldenPath)
	if err != nil {
		t.Fatalf("read golden body: %v", err)
	}
	raw = bytes.TrimSpace(raw)

	s, rr, au, runID, stageID := reapServer(t, run.StageStateRunning)
	w := postReapFailureRaw(t, s, runID, stageID, raw, withReapOperator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 — the golden conditional body must decode and take the "+
			"conditional path:\n%s", w.Code, w.Body.String())
	}
	code, details := decodeReapError(t, w)
	if code != "stage_state_precondition_failed" {
		t.Errorf("error code = %q, want stage_state_precondition_failed", code)
	}
	if details["expected_state"] != "dispatched" {
		t.Errorf("details expected_state = %v, want dispatched (the pin carried by the golden body)", details["expected_state"])
	}
	cur, _ := rr.GetStage(context.Background(), stageID)
	if cur.State != run.StageStateRunning {
		t.Errorf("stage state = %q, want running (nothing transitioned)", cur.State)
	}
	if got := reapAudit(au); len(got) != 0 {
		t.Errorf("dispatch_reaper_failed entries = %d, want 0", len(got))
	}
}
