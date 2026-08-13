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
