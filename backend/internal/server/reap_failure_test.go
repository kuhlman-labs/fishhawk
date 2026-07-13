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
// TransitionStage attempt, then delegates — so the embedded repo refuses the
// move exactly as the real repo refuses a terminal → running transition.
type raceReapRepo struct {
	*orchestratorRepo
	stageID uuid.UUID
	flipped bool
}

func (r *raceReapRepo) TransitionStage(ctx context.Context, id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	if !r.flipped && id == r.stageID {
		r.flipped = true
		r.mu.Lock()
		if st := r.stagesByID[id]; st != nil {
			st.State = run.StageStateSucceeded // the concurrent winner already settled it
		}
		r.mu.Unlock()
	}
	return r.orchestratorRepo.TransitionStage(ctx, id, to, c)
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
// TransitionStage attempt, then delegates — so the embedded repo refuses the
// move (awaiting_children → running is not a valid edge) exactly as the real
// repo would, driving the handler's post-FailStage re-load branch.
type parkRaceReapRepo struct {
	*orchestratorRepo
	stageID uuid.UUID
	flipped bool
}

func (r *parkRaceReapRepo) TransitionStage(ctx context.Context, id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	if !r.flipped && id == r.stageID {
		r.flipped = true
		r.mu.Lock()
		if st := r.stagesByID[id]; st != nil {
			st.State = run.StageStateAwaitingChildren // a concurrent fanout parked it
		}
		r.mu.Unlock()
	}
	return r.orchestratorRepo.TransitionStage(ctx, id, to, c)
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
