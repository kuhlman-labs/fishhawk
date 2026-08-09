package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// forceOrchestratorStageState directly sets a seeded stage's state under the
// fake repo's lock, bypassing transition validation — the #1936 concurrency
// tests use it to simulate an admission walk settling a stage to 'succeeded'
// (an otherwise-invalid direct transition) without threading a full walk.
func forceOrchestratorStageState(rr *orchestratorRepo, id uuid.UUID, st run.StageState) {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	rr.stagesByID[id].State = st
}

// hostDispatchServer wires a server with the shared orchestratorRepo fake (which
// implements the StageCASTransitioner capability, so the endpoint's production
// CAS path is exercised) and seeds a run with a single stage in the given state.
func hostDispatchServer(t *testing.T, stageState run.StageState) (*Server, *orchestratorRepo, uuid.UUID, uuid.UUID) {
	t.Helper()
	rr := newOrchestratorRepo()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 0, stageState)
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})
	return s, rr, runRow.ID, stage.ID
}

// withHostDispatchOperator injects an operator token identity carrying
// write:runs — the scope the host-dispatch endpoint requires.
func withHostDispatchOperator(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
		Subject: "github:ops", TokenID: "tok-op", Scopes: []string{"write:runs"},
	}))
}

func postHostDispatch(t *testing.T, s *Server, runID, stageID uuid.UUID,
	withID func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/v0/runs/"+runID.String()+"/stages/"+stageID.String()+"/host-dispatch", nil)
	req.SetPathValue("run_id", runID.String())
	req.SetPathValue("stage_id", stageID.String())
	w := httptest.NewRecorder()
	s.handleHostDispatchStage(w, withID(req))
	return w
}

func decodeHostDispatch(t *testing.T, w *httptest.ResponseRecorder) hostDispatchResponse {
	t.Helper()
	var resp hostDispatchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, w.Body.String())
	}
	return resp
}

// TestHostDispatchRouteRegistered guards the route table: POST
// /v0/runs/{run_id}/stages/{stage_id}/host-dispatch (#1912) must reach
// handleHostDispatchStage through the mux. The anonymous request reaches the
// handler's auth ladder and returns 401 — an UNregistered route would instead
// 404 with a default not-found body, so a 401 here proves the route is wired in
// handlers.go (the auth ladder runs BEFORE the nil-dependency guard, matching
// the #1915 TestReviveRouteRegistered convention). This is the ONLY test that
// exercises the host-dispatch registration through the ServeMux; every other
// host_dispatch_test.go case calls s.handleHostDispatchStage directly.
func TestHostDispatchRouteRegistered(t *testing.T) {
	s := New(Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/v0/runs/"+"00000000-0000-0000-0000-000000000000"+"/stages/"+"00000000-0000-0000-0000-000000000000"+"/host-dispatch", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (route reaches handler auth ladder)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "authentication_required") {
		t.Errorf("body = %s, want authentication_required (handleHostDispatchStage reached)", rec.Body.String())
	}
}

// Happy path (a): a parked awaiting_host_dispatch stage flips to dispatched and
// returns {transitioned:true}. This is the spawn marker's core job.
func TestHostDispatch_AwaitingHostDispatch_MarksDispatched(t *testing.T) {
	s, rr, runID, stageID := hostDispatchServer(t, run.StageStateAwaitingHostDispatch)

	w := postHostDispatch(t, s, runID, stageID, withHostDispatchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	resp := decodeHostDispatch(t, w)
	if !resp.Transitioned {
		t.Error("transitioned = false, want true")
	}
	if resp.StageState != string(run.StageStateDispatched) {
		t.Errorf("stage_state = %q, want dispatched", resp.StageState)
	}
	cur, _ := rr.GetStage(context.Background(), stageID)
	if cur.State != run.StageStateDispatched {
		t.Errorf("persisted state = %q, want dispatched", cur.State)
	}
}

// Happy path (b): the first plan-stage spawn marks a still-pending stage
// dispatched (the local first-stage sits at pending until trace time, #1030).
func TestHostDispatch_Pending_MarksDispatched(t *testing.T) {
	s, _, runID, stageID := hostDispatchServer(t, run.StageStatePending)

	w := postHostDispatch(t, s, runID, stageID, withHostDispatchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	resp := decodeHostDispatch(t, w)
	if !resp.Transitioned || resp.StageState != string(run.StageStateDispatched) {
		t.Errorf("resp = %+v, want transitioned:true dispatched", resp)
	}
}

// Idempotent (b): an already-'dispatched' stage returns {transitioned:false} —
// the legal manual re-dispatch of a stage whose spawned runner died. No state
// change, 200.
func TestHostDispatch_AlreadyDispatched_IdempotentNoOp(t *testing.T) {
	s, rr, runID, stageID := hostDispatchServer(t, run.StageStateDispatched)

	w := postHostDispatch(t, s, runID, stageID, withHostDispatchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	resp := decodeHostDispatch(t, w)
	if resp.Transitioned {
		t.Error("transitioned = true, want false (idempotent dead-runner re-dispatch)")
	}
	if resp.StageState != string(run.StageStateDispatched) {
		t.Errorf("stage_state = %q, want dispatched", resp.StageState)
	}
	cur, _ := rr.GetStage(context.Background(), stageID)
	if cur.State != run.StageStateDispatched {
		t.Errorf("persisted state = %q, want dispatched (unchanged)", cur.State)
	}
}

// 409 (a): a running stage, every terminal state, and awaiting_approval each
// return 409 dispatch_not_admissible — a live or settled stage can never be
// re-marked as a fresh spawn.
func TestHostDispatch_NonAdmissibleStates_Conflict(t *testing.T) {
	for _, st := range []run.StageState{
		run.StageStateRunning,
		run.StageStateSucceeded,
		run.StageStateFailed,
		run.StageStateCancelled,
		run.StageStateAwaitingApproval,
	} {
		t.Run(string(st), func(t *testing.T) {
			s, rr, runID, stageID := hostDispatchServer(t, st)

			w := postHostDispatch(t, s, runID, stageID, withHostDispatchOperator)
			if w.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "dispatch_not_admissible") {
				t.Errorf("body = %s, want dispatch_not_admissible", w.Body.String())
			}
			// The stage must be left untouched.
			cur, _ := rr.GetStage(context.Background(), stageID)
			if cur.State != st {
				t.Errorf("state = %q, want %q (untouched)", cur.State, st)
			}
		})
	}
}

// Auth: an anonymous caller is 401 (before any repo work).
func TestHostDispatch_Anonymous_Unauthorized(t *testing.T) {
	s, _, runID, stageID := hostDispatchServer(t, run.StageStateAwaitingHostDispatch)
	w := postHostDispatch(t, s, runID, stageID, func(req *http.Request) *http.Request { return req })
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "authentication_required") {
		t.Errorf("body = %s, want authentication_required", w.Body.String())
	}
}

// Auth: an authenticated token WITHOUT write:runs is 403.
func TestHostDispatch_MissingScope_Forbidden(t *testing.T) {
	s, _, runID, stageID := hostDispatchServer(t, run.StageStateAwaitingHostDispatch)
	withID := func(req *http.Request) *http.Request {
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
			Subject: "github:ops", TokenID: "tok-op", Scopes: []string{"read:runs"},
		}))
	}
	w := postHostDispatch(t, s, runID, stageID, withID)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "insufficient_scope") {
		t.Errorf("body = %s, want insufficient_scope", w.Body.String())
	}
}

// 503: an authenticated caller reaching the endpoint with no RunRepo configured
// gets host_dispatch_unconfigured (the auth ladder is passed first).
func TestHostDispatch_NoRunRepo_ServiceUnavailable(t *testing.T) {
	s := New(Config{})
	w := postHostDispatch(t, s, uuid.New(), uuid.New(), withHostDispatchOperator)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "host_dispatch_unconfigured") {
		t.Errorf("body = %s, want host_dispatch_unconfigured", w.Body.String())
	}
}

// 400: a malformed run_id / stage_id path value is a validation_failed 400.
func TestHostDispatch_BadUUIDs_BadRequest(t *testing.T) {
	s, _, _, stageID := hostDispatchServer(t, run.StageStateAwaitingHostDispatch)

	// Bad run_id.
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/not-a-uuid/stages/"+stageID.String()+"/host-dispatch", nil)
	req.SetPathValue("run_id", "not-a-uuid")
	req.SetPathValue("stage_id", stageID.String())
	w := httptest.NewRecorder()
	s.handleHostDispatchStage(w, withHostDispatchOperator(req))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "validation_failed") {
		t.Fatalf("bad run_id: status = %d body = %s, want 400 validation_failed", w.Code, w.Body.String())
	}

	// Bad stage_id.
	req = httptest.NewRequest(http.MethodPost, "/v0/runs/"+uuid.New().String()+"/stages/nope/host-dispatch", nil)
	req.SetPathValue("run_id", uuid.New().String())
	req.SetPathValue("stage_id", "nope")
	w = httptest.NewRecorder()
	s.handleHostDispatchStage(w, withHostDispatchOperator(req))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "validation_failed") {
		t.Fatalf("bad stage_id: status = %d body = %s, want 400 validation_failed", w.Code, w.Body.String())
	}
}

// 404: an unknown stage id, and a stage whose run_id differs from the path (the
// handle validation), both return stage_not_found.
func TestHostDispatch_UnknownAndMismatchedHandle_NotFound(t *testing.T) {
	s, _, runID, stageID := hostDispatchServer(t, run.StageStateAwaitingHostDispatch)

	// Unknown stage.
	w := postHostDispatch(t, s, runID, uuid.New(), withHostDispatchOperator)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "stage_not_found") {
		t.Fatalf("unknown stage: status = %d body = %s, want 404 stage_not_found", w.Code, w.Body.String())
	}

	// Real stage, wrong run in the path → handle mismatch 404.
	w = postHostDispatch(t, s, uuid.New(), stageID, withHostDispatchOperator)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "stage_not_found") {
		t.Fatalf("mismatched handle: status = %d body = %s, want 404 stage_not_found", w.Code, w.Body.String())
	}
}

// Eligibility (#1912 fix-up): a run LOCKED to a non-local runner_kind is never
// host-spawned, so a marker against its parked stage is refused 409 and the
// stage is left untouched — a write:runs caller cannot mark a github_actions
// stage 'dispatched' without a host spawn.
func TestHostDispatch_LockedNonLocalRun_Conflict(t *testing.T) {
	s, rr, runID, stageID := hostDispatchServer(t, run.StageStateAwaitingHostDispatch)
	// Lock the seeded run to github_actions.
	locked, _ := rr.GetRun(context.Background(), runID)
	locked.RunnerKind = run.RunnerKindGitHubActions
	locked.RunnerKindResolved = true

	w := postHostDispatch(t, s, runID, stageID, withHostDispatchOperator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "dispatch_not_admissible") {
		t.Errorf("body = %s, want dispatch_not_admissible", w.Body.String())
	}
	cur, _ := rr.GetStage(context.Background(), stageID)
	if cur.State != run.StageStateAwaitingHostDispatch {
		t.Errorf("state = %q, want awaiting_host_dispatch (untouched)", cur.State)
	}
}

// Non-host kind (E45.8 / #1861): a run LOCKED to gitlab_ci — now a KNOWN
// non-host-dispatched kind (KindHostDispatched reports (false, known=true)) — is
// STILL refused 409. This endpoint rejects any resolved kind that is not
// known-AND-host-dispatched, so both the former unknown gitlab_ci and the now-
// known non-host gitlab_ci resolve to the same `!known || !hostDispatched` block
// (fishhawkd fires gitlab_ci's pipeline, never a LOCAL host spawn). Pins that the
// classification flip did not change this site's 409 posture.
func TestHostDispatch_LockedGitLabCI_Conflict(t *testing.T) {
	s, rr, runID, stageID := hostDispatchServer(t, run.StageStateAwaitingHostDispatch)
	locked, _ := rr.GetRun(context.Background(), runID)
	locked.RunnerKind = "gitlab_ci"
	locked.RunnerKindResolved = true

	w := postHostDispatch(t, s, runID, stageID, withHostDispatchOperator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "dispatch_not_admissible") {
		t.Errorf("body = %s, want dispatch_not_admissible", w.Body.String())
	}
	cur, _ := rr.GetStage(context.Background(), stageID)
	if cur.State != run.StageStateAwaitingHostDispatch {
		t.Errorf("state = %q, want awaiting_host_dispatch (untouched)", cur.State)
	}
}

// Eligibility (#1912 fix-up): a locked-LOCAL run is admitted (the normal parked
// path); an UN-resolved run is also admitted (its first host dispatch
// auto-resolves it to local), mirroring the MCP guardHostDispatch posture — the
// default seeded run is un-resolved and every happy-path test above covers it.
func TestHostDispatch_LockedLocalRun_Admitted(t *testing.T) {
	s, rr, runID, stageID := hostDispatchServer(t, run.StageStateAwaitingHostDispatch)
	locked, _ := rr.GetRun(context.Background(), runID)
	locked.RunnerKind = run.RunnerKindLocal
	locked.RunnerKindResolved = true

	w := postHostDispatch(t, s, runID, stageID, withHostDispatchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if resp := decodeHostDispatch(t, w); !resp.Transitioned {
		t.Errorf("resp = %+v, want transitioned:true", resp)
	}
}

// Eligibility (#1912 fix-up): a human-executed stage is never host-spawned (the
// orchestrator walks it to awaiting_approval), so the marker refuses it 409 and
// leaves it untouched rather than silently flipping pending → dispatched. This
// also closes the review-arm behavior the marker's unconditional call exposed:
// the feature_change review stage is human-executed and now cleanly 409s.
func TestHostDispatch_HumanStage_Conflict(t *testing.T) {
	s, rr, runID, stageID := hostDispatchServer(t, run.StageStatePending)
	st, _ := rr.GetStage(context.Background(), stageID)
	st.ExecutorKind = run.ExecutorHuman

	w := postHostDispatch(t, s, runID, stageID, withHostDispatchOperator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "dispatch_not_admissible") {
		t.Errorf("body = %s, want dispatch_not_admissible", w.Body.String())
	}
	cur, _ := rr.GetStage(context.Background(), stageID)
	if cur.State != run.StageStatePending {
		t.Errorf("state = %q, want pending (untouched)", cur.State)
	}
}

// Eligibility (#1912 fix-up): an auto-merge review stage (a review-typed stage
// with a check-only gate, walked straight to succeeded by the orchestrator) is
// never host-spawned, so the marker refuses it 409.
func TestHostDispatch_AutoMergeReviewStage_Conflict(t *testing.T) {
	s, rr, runID, stageID := hostDispatchServer(t, run.StageStatePending)
	st, _ := rr.GetStage(context.Background(), stageID)
	st.Type = run.StageTypeReview
	st.Gate = &run.Gate{Kind: run.GateKindCheck}

	w := postHostDispatch(t, s, runID, stageID, withHostDispatchOperator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "dispatch_not_admissible") {
		t.Errorf("body = %s, want dispatch_not_admissible", w.Body.String())
	}
}

// casRaceRepo embeds the CAS-capable orchestratorRepo but forces its
// TransitionStageFrom to lose a compare-and-swap race: it flips the stage to
// raceTo (a concurrent winner) and returns StageStateChangedError, so the
// handler's re-load-and-classify branch is exercised deterministically.
type casRaceRepo struct {
	*orchestratorRepo
	raceStageID uuid.UUID
	raceTo      run.StageState
}

func (r *casRaceRepo) TransitionStageFrom(_ context.Context, id uuid.UUID, from, _ run.StageState, _ *run.StageCompletion) (*run.Stage, error) {
	r.mu.Lock()
	actual := from
	if s, ok := r.stagesByID[id]; ok && id == r.raceStageID {
		s.State = r.raceTo
		actual = r.raceTo
	}
	r.mu.Unlock()
	return nil, run.StageStateChangedError{StageID: id, Expected: from, Actual: actual}
}

// (race → idempotent): a CAS refusal whose concurrent winner already marked the
// stage 'dispatched' is re-classified as the benign {transitioned:false} no-op.
func TestHostDispatch_CASRace_ConcurrentDispatched_IdempotentNoOp(t *testing.T) {
	rr := newOrchestratorRepo()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 0, run.StageStateAwaitingHostDispatch)
	race := &casRaceRepo{orchestratorRepo: rr, raceStageID: stage.ID, raceTo: run.StageStateDispatched}
	s := New(Config{RunRepo: race})

	w := postHostDispatch(t, s, runRow.ID, stage.ID, withHostDispatchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	resp := decodeHostDispatch(t, w)
	if resp.Transitioned || resp.StageState != string(run.StageStateDispatched) {
		t.Errorf("resp = %+v, want transitioned:false dispatched (concurrent winner)", resp)
	}
}

// (race → conflict): a CAS refusal whose concurrent winner moved the stage to a
// non-admissible state (running) is re-classified as 409.
func TestHostDispatch_CASRace_ConcurrentRunning_Conflict(t *testing.T) {
	rr := newOrchestratorRepo()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 0, run.StageStateAwaitingHostDispatch)
	race := &casRaceRepo{orchestratorRepo: rr, raceStageID: stage.ID, raceTo: run.StageStateRunning}
	s := New(Config{RunRepo: race})

	w := postHostDispatch(t, s, runRow.ID, stage.ID, withHostDispatchOperator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "dispatch_not_admissible") {
		t.Errorf("body = %s, want dispatch_not_admissible", w.Body.String())
	}
}

// TestHostDispatch_WithOrchestrator_HappyPathMarks proves the #1936 admission
// fence does not break the normal spawn marker: with an orchestrator wired (so
// the per-stage lock IS taken), a parked awaiting_host_dispatch stage still flips
// to dispatched. The lock is acquired and released within the handler.
func TestHostDispatch_WithOrchestrator_HappyPathMarks(t *testing.T) {
	rr := newOrchestratorRepo()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 0, run.StageStateAwaitingHostDispatch)
	o := &orchestrator.Orchestrator{Runs: rr}
	s := New(Config{RunRepo: rr, Orchestrator: o})

	w := postHostDispatch(t, s, runRow.ID, stage.ID, withHostDispatchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	resp := decodeHostDispatch(t, w)
	if !resp.Transitioned || resp.StageState != string(run.StageStateDispatched) {
		t.Errorf("resp = %+v, want transitioned:true dispatched", resp)
	}
}

// TestHostDispatch_NilOrchestrator_NoLock is failure mode f: with NO orchestrator
// wired the marker takes NO admission lock and behaves byte-identically to
// pre-#1936. An already-'dispatched' stage returns the idempotent
// {transitioned:false} with no blocking. (Every other host_dispatch_test.go case
// also runs the nil-orchestrator path; this pins the no-lock contract explicitly.)
func TestHostDispatch_NilOrchestrator_NoLock(t *testing.T) {
	s, _, runID, stageID := hostDispatchServer(t, run.StageStateDispatched) // wires no orchestrator
	if s.cfg.Orchestrator != nil {
		t.Fatal("test precondition: orchestrator must be nil")
	}
	w := postHostDispatch(t, s, runID, stageID, withHostDispatchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	resp := decodeHostDispatch(t, w)
	if resp.Transitioned || resp.StageState != string(run.StageStateDispatched) {
		t.Errorf("resp = %+v, want transitioned:false dispatched (idempotent no-op, no lock)", resp)
	}
}

// TestHostDispatch_AdmissionWalkInFlight_MarkerBlocksThen409 is failure mode b
// (walk-wins interleaving, #1936): while an acceptance-admission short-circuit
// walk holds the per-stage lock mid-walk (the stage transiently at the
// walk-intermediate 'dispatched'), a concurrent host-dispatch marker must NOT
// return the {transitioned:false} idempotent-proceed. It serializes behind the
// lock and, once the walk settles the stage to succeeded and releases the lock,
// returns 409 dispatch_not_admissible — so the MCP verb's fail-closed marker
// handling spawns nothing.
func TestHostDispatch_AdmissionWalkInFlight_MarkerBlocksThen409(t *testing.T) {
	rr := newOrchestratorRepo()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 0, run.StageStateDispatched) // walk-intermediate
	stage.Type = run.StageTypeAcceptance
	o := &orchestrator.Orchestrator{Runs: rr}
	s := New(Config{RunRepo: rr, Orchestrator: o})

	// Simulate the admission walk holding the shared per-stage admission lock.
	unlock := o.LockStageAdmission(stage.ID)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- postHostDispatch(t, s, runRow.ID, stage.ID, withHostDispatchOperator) }()

	// The marker MUST block on the lock — it cannot return {transitioned:false}
	// from the walk-intermediate 'dispatched' state while the walk holds the lock.
	select {
	case w := <-done:
		t.Fatalf("marker returned (code=%d body=%s) while the admission walk held the lock; it did not serialize behind it", w.Code, w.Body.String())
	case <-time.After(timescale.D(150 * time.Millisecond)):
	}

	// Walk completes: the stage settles to succeeded, the lock releases.
	forceOrchestratorStageState(rr, stage.ID, run.StageStateSucceeded)
	unlock()

	w := <-done
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 dispatch_not_admissible:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "dispatch_not_admissible") {
		t.Errorf("body missing dispatch_not_admissible: %s", w.Body.String())
	}
}

// decomposedHostDispatchServer wires a server whose child (slice 1, depends on
// slice 0) is parked at awaiting_host_dispatch, its dependency sibling (slice 0)
// seeded at depSliceState. Reuses the guard-test helpers (same package); the
// counting repo implements StageCASTransitioner (promoted from orchestratorRepo)
// so the production CAS path is exercised. Returns the child run id + its
// implement stage id + the dependency sibling run id.
func decomposedHostDispatchServer(t *testing.T, depSliceState run.State) (*Server, *guardCountingRepo, uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	s, rr, art := newGuardServer(t)
	parentID := seedParentDecompPlan(t, rr, art, [][]int{nil, {0}})
	sib := seedGuardChild(rr, parentID, 0, depSliceState)
	child := seedGuardChild(rr, parentID, 1, run.StateRunning)
	stage := rr.seedStage(child.ID, 0, run.StageStateAwaitingHostDispatch)
	stage.Type = run.StageTypeImplement
	return s, rr, child.ID, stage.ID, sib.ID
}

// (14) the wave-order refusal drives the REAL route and asserts BOTH the 409
// body AND — reading COMMITTED state after the call returns — that the stage is
// STILL awaiting_host_dispatch. Error identity alone is insufficient: a guard
// placed AFTER the CAS would return a byte-identical 409 with the stage already
// flipped, so the committed-state read is the load-bearing assertion. The bad
// state (a pending dependency sibling) is seeded BY CONSTRUCTION.
func TestHostDispatch_DecompositionDependencyPending_RefusesAndParks(t *testing.T) {
	s, rr, childRunID, childStageID, sibID := decomposedHostDispatchServer(t, run.StatePending)

	w := postHostDispatch(t, s, childRunID, childStageID, withHostDispatchOperator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "dependency_not_satisfied") {
		t.Errorf("body missing dependency_not_satisfied: %s", body)
	}
	if !strings.Contains(body, sibID.String()) {
		t.Errorf("body should name the blocking sibling %s: %s", sibID, body)
	}
	if !strings.Contains(body, `"blocking_slice_index":0`) {
		t.Errorf("body should carry blocking_slice_index 0: %s", body)
	}
	// COMMITTED state: the stage must be untouched (the guard refused BEFORE the CAS).
	cur, _ := rr.GetStage(context.Background(), childStageID)
	if cur.State != run.StageStateAwaitingHostDispatch {
		t.Errorf("persisted stage state = %q, want awaiting_host_dispatch (no CAS on refusal)", cur.State)
	}
}

// (15) the dependency-satisfied child still transitions to dispatched
// (transitioned:true) — so the guard cannot pass by refusing everything.
func TestHostDispatch_DecompositionDependencySucceeded_Admits(t *testing.T) {
	s, rr, childRunID, childStageID, _ := decomposedHostDispatchServer(t, run.StateSucceeded)

	w := postHostDispatch(t, s, childRunID, childStageID, withHostDispatchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	resp := decodeHostDispatch(t, w)
	if !resp.Transitioned || resp.StageState != string(run.StageStateDispatched) {
		t.Errorf("resp = %+v, want transitioned:true dispatched", resp)
	}
	cur, _ := rr.GetStage(context.Background(), childStageID)
	if cur.State != run.StageStateDispatched {
		t.Errorf("persisted stage state = %q, want dispatched", cur.State)
	}
}

// (16, inert-off-path pin) a decomposed child whose slice declares NO
// dependencies (wave 0) is admitted — the guard does not over-block a
// dispatchable child.
func TestHostDispatch_DecompositionWave0Child_Admits(t *testing.T) {
	s, rr, art := newGuardServer(t)
	parentID := seedParentDecompPlan(t, rr, art, [][]int{nil, {0}})
	child := seedGuardChild(rr, parentID, 0, run.StateRunning) // slice 0, no deps
	stage := rr.seedStage(child.ID, 0, run.StageStateAwaitingHostDispatch)
	stage.Type = run.StageTypeImplement

	w := postHostDispatch(t, s, child.ID, stage.ID, withHostDispatchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	resp := decodeHostDispatch(t, w)
	if !resp.Transitioned {
		t.Errorf("resp = %+v, want transitioned:true (wave-0 child is dispatchable)", resp)
	}
}

// The guard's read-errored path surfaces as 500 dependency_check_failed at the
// handler, leaving the stage untouched (retryable, never a silent admit).
func TestHostDispatch_DecompositionDependencyCheckError_500(t *testing.T) {
	s, rr, art := newGuardServer(t)
	parentID := seedParentDecompPlan(t, rr, art, [][]int{nil, {0}})
	child := seedGuardChild(rr, parentID, 1, run.StateRunning)
	stage := rr.seedStage(child.ID, 0, run.StageStateAwaitingHostDispatch)
	stage.Type = run.StageTypeImplement
	art.listErr = errors.New("artifact store down")

	w := postHostDispatch(t, s, child.ID, stage.ID, withHostDispatchOperator)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "dependency_check_failed") {
		t.Errorf("body missing dependency_check_failed: %s", w.Body.String())
	}
	cur, _ := rr.GetStage(context.Background(), stage.ID)
	if cur.State != run.StageStateAwaitingHostDispatch {
		t.Errorf("persisted stage state = %q, want awaiting_host_dispatch (no CAS on 500)", cur.State)
	}
}

// TestHostDispatch_Decomposition_CrossLayer_PgBacked (#2546 verification
// CROSS-BOUNDARY) exercises the whole span — the postgres run repo's
// slice_index/decomposed_from column round-trip, the artifact-backed plan
// authority, and the handler — end to end against the shared testcontainers
// Postgres. It seeds a real parent run + approved plan artifact + two minted
// children through the postgres repos, drives POST host-dispatch through the
// handler, and asserts (a) the refusal leaves the PERSISTED stage row untouched
// and (b) the same call transitions it to dispatched once the dependency child
// reaches succeeded — proving the plan authority, the column round-trip, and
// the handler agree.
func TestHostDispatch_Decomposition_CrossLayer_PgBacked(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	artRepo := artifact.NewPostgresRepository(pool)
	s := New(Config{RunRepo: runRepo, ArtifactRepo: artRepo})

	newRun := func(sliceIdx *int, parent *uuid.UUID) *run.Run {
		t.Helper()
		r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
			Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc",
			TriggerSource: run.TriggerCLI, DecomposedFrom: parent, ParentRunID: parent, SliceIndex: sliceIdx,
		})
		if err != nil {
			t.Fatalf("create run: %v", err)
		}
		return r
	}

	parent := newRun(nil, nil)
	planStage, err := runRepo.CreateStage(ctx, run.CreateStageParams{
		RunID: parent.ID, Sequence: 0, Type: run.StageTypePlan,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create parent plan stage: %v", err)
	}
	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "s",
		Verification: plan.Verification{TestStrategy: "t", RollbackPlan: "r"},
		Decomposition: &plan.Decomposition{Rationale: "split", SubPlans: []plan.SubPlanSummary{
			{Title: "A", ScopeHint: "a"},
			{Title: "B", ScopeHint: "b", DependsOn: []int{0}},
		}},
	}
	pb, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := artRepo.Create(ctx, artifact.CreateParams{
		StageID: planStage.ID, Kind: artifact.KindPlan, SchemaVersion: &sv, Content: pb,
	}); err != nil {
		t.Fatalf("create plan artifact: %v", err)
	}

	i0, i1 := 0, 1
	child0 := newRun(&i0, &parent.ID)
	child1 := newRun(&i1, &parent.ID)
	child1Stage, err := runRepo.CreateStage(ctx, run.CreateStageParams{
		RunID: child1.ID, Sequence: 0, Type: run.StageTypeImplement,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create child1 implement stage: %v", err)
	}
	if _, err := runRepo.TransitionStage(ctx, child1Stage.ID, run.StageStateAwaitingHostDispatch, nil); err != nil {
		t.Fatalf("park child1 stage: %v", err)
	}

	// (a) child0 (slice 0) has not succeeded → dispatch of child1 refuses, and
	// the PERSISTED stage row is untouched.
	w := postHostDispatch(t, s, child1.ID, child1Stage.ID, withHostDispatchOperator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "dependency_not_satisfied") {
		t.Errorf("body missing dependency_not_satisfied: %s", w.Body.String())
	}
	cur, err := runRepo.GetStage(ctx, child1Stage.ID)
	if err != nil {
		t.Fatalf("get child1 stage: %v", err)
	}
	if cur.State != run.StageStateAwaitingHostDispatch {
		t.Errorf("persisted stage = %q, want awaiting_host_dispatch (refusal committed no state)", cur.State)
	}

	// (b) advance child0's RUN to succeeded → the same dispatch admits.
	if _, err := runRepo.TransitionRun(ctx, child0.ID, run.StateRunning); err != nil {
		t.Fatalf("child0 → running: %v", err)
	}
	if _, err := runRepo.TransitionRun(ctx, child0.ID, run.StateSucceeded); err != nil {
		t.Fatalf("child0 → succeeded: %v", err)
	}
	w = postHostDispatch(t, s, child1.ID, child1Stage.ID, withHostDispatchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after dependency succeeded:\n%s", w.Code, w.Body.String())
	}
	resp := decodeHostDispatch(t, w)
	if !resp.Transitioned || resp.StageState != string(run.StageStateDispatched) {
		t.Errorf("resp = %+v, want transitioned:true dispatched", resp)
	}
	cur, _ = runRepo.GetStage(ctx, child1Stage.ID)
	if cur.State != run.StageStateDispatched {
		t.Errorf("persisted stage = %q, want dispatched", cur.State)
	}
}
