package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// nonCASRunRepo is the interface-erasure vehicle the #2672 reap-refusal tests
// hand the handler. Embedding the run.Repository INTERFACE (not the concrete
// *orchestratorRepo) promotes ONLY Repository's method set, so
// TransitionStageFrom — declared on the SEPARATE run.StageCASTransitioner
// interface, not on Repository — is ERASED even though the wrapped dynamic value
// implements it. This is a faithful model of a decorator that wraps the repo in
// a Repository-typed layer and thereby drops the optional CAS capability. See
// the Go spec, Struct types / promoted methods and Method sets.
type nonCASRunRepo struct {
	run.Repository
}

// nonCASReloadParkRepo is the ordering vehicle: like nonCASRunRepo it erases the
// CAS capability, but it ALSO makes its SECOND GetStage (the handler's
// post-transition re-load) return a protected park, simulating a park landing
// after the handler's initial load. Because handleReapStageFailure classifies
// errReapRepoNotCAS BEFORE that re-load, the second GetStage must never be
// consulted on the refusal path — the test proves the 503 stands regardless of
// what the re-load would have said. (Moving the errors.Is arm after the re-load,
// counterfactual 2, makes this park mask the refusal as a benign 200.)
type nonCASReloadParkRepo struct {
	run.Repository
	mu        sync.Mutex
	calls     int
	stageID   uuid.UUID
	parkState run.StageState
}

func (r *nonCASReloadParkRepo) GetStage(ctx context.Context, id uuid.UUID) (*run.Stage, error) {
	st, err := r.Repository.GetStage(ctx, id)
	r.mu.Lock()
	r.calls++
	n := r.calls
	r.mu.Unlock()
	if err != nil {
		return st, err
	}
	if n >= 2 && id == r.stageID {
		cp := *st
		cp.State = r.parkState
		return &cp, nil
	}
	return st, nil
}

// assertVehicleErasesCAS fails fast if the vehicle unexpectedly satisfies
// run.StageCASTransitioner — a vehicle that silently regained the capability
// would let the test pass VACUOUSLY, so this makes a broken vehicle a vehicle
// bug, not a green result (the approve_advance_cas_test.go:815 precedent).
func assertVehicleErasesCAS(t *testing.T, vehicle run.Repository) {
	t.Helper()
	if _, ok := vehicle.(run.StageCASTransitioner); ok {
		t.Fatalf("vehicle %T unexpectedly implements run.StageCASTransitioner; the interface-erasure "+
			"model is broken and the refusal assertion would pass vacuously", vehicle)
	}
}

// TestReapStageFailure_NonCASRepoRefusesLoudly is the end-to-end refusal across
// the handler↔repository seam (#2672): a run repository that does NOT implement
// run.StageCASTransitioner makes the reap path refuse loudly (503
// reap_failure_repo_not_cas) rather than degrade to run.FailStage. The control's
// effect is COMMITTED STATE, not just an error, so the test re-reads the stage
// AFTER the call and asserts it is still `dispatched` (never `failed`), plus zero
// dispatch_reaper_failed audit entries and the run un-advanced. The bad state is
// seeded BY CONSTRUCTION (interface embedding erases the method), so RED lands on
// the behavioral assertion, not on fixture setup.
func TestReapStageFailure_NonCASRepoRefusesLoudly(t *testing.T) {
	s, rr, au, runID, stageID := reapServer(t, run.StageStateDispatched)
	vehicle := nonCASRunRepo{Repository: rr}
	assertVehicleErasesCAS(t, vehicle)
	s.cfg.RunRepo = vehicle

	w := postReapFailure(t, s, runID, stageID,
		reapFailureRequest{Category: "C", Reason: "acceptance_preview_provision_failed"},
		withReapOperator)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("unmarshal error body: %v\n%s", err, w.Body.String())
	}
	if errBody.Error.Code != "reap_failure_repo_not_cas" {
		t.Errorf("error code = %q, want reap_failure_repo_not_cas\n%s", errBody.Error.Code, w.Body.String())
	}

	// Committed-state assertion: the stage was NOT reaped to failed.
	cur, err := rr.GetStage(context.Background(), stageID)
	if err != nil {
		t.Fatalf("get stage: %v", err)
	}
	if cur.State != run.StageStateDispatched {
		t.Errorf("stage state = %q, want dispatched (the refusal must not transition it)", cur.State)
	}
	if cur.FailureCategory != nil || cur.FailureReason != nil {
		t.Errorf("failure metadata stamped on a refused reap: cat=%v reason=%v", cur.FailureCategory, cur.FailureReason)
	}

	// No audit entry and no advance on the wiring-fault path.
	if entries := reapAudit(au); len(entries) != 0 {
		t.Errorf("dispatch_reaper_failed entries = %d, want 0 (no audit on refusal)", len(entries))
	}
	runRow, err := rr.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if runRow.State != run.StateRunning {
		t.Errorf("run state = %q, want running (the refusal must not advance the run)", runRow.State)
	}
}

// TestReapStageFailure_NonCASRepoParkedStageStillBenignNoOp proves the refusal
// does NOT swallow the pre-existing GUARD 1 benign no-op (#2672): a stage already
// in a protected park at load returns 200 {transitioned:false} — GUARD 1
// short-circuits BEFORE failStageForReap is reached, so the non-CAS repo never
// even attempts the transition. No audit entry, no advance.
func TestReapStageFailure_NonCASRepoParkedStageStillBenignNoOp(t *testing.T) {
	for _, park := range []run.StageState{
		run.StageStateAwaitingApproval,
		run.StageStateAwaitingChildren,
	} {
		t.Run(string(park), func(t *testing.T) {
			s, rr, au, runID, stageID := reapServer(t, park)
			vehicle := nonCASRunRepo{Repository: rr}
			assertVehicleErasesCAS(t, vehicle)
			s.cfg.RunRepo = vehicle

			w := postReapFailure(t, s, runID, stageID,
				reapFailureRequest{Category: "C", Reason: "raced a live park"},
				withReapOperator)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (GUARD 1 benign no-op):\n%s", w.Code, w.Body.String())
			}
			var resp reapFailureResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if resp.Transitioned {
				t.Error("transitioned = true, want false (the park is a live no-op)")
			}
			if resp.StageState != string(park) {
				t.Errorf("stage_state = %q, want %q", resp.StageState, park)
			}

			cur, _ := rr.GetStage(context.Background(), stageID)
			if cur.State != park {
				t.Errorf("stage state = %q, want %q (park preserved)", cur.State, park)
			}
			if entries := reapAudit(au); len(entries) != 0 {
				t.Errorf("dispatch_reaper_failed entries = %d, want 0", len(entries))
			}
		})
	}
}

// TestReapStageFailure_NonCASRepoRefusalPrecedesReload pins that the
// errors.Is(err, errReapRepoNotCAS) arm sits AHEAD of the post-transition
// re-load (#2672): a non-CAS repo whose SECOND GetStage would report a protected
// park (a park landing after the initial load) still answers 503
// reap_failure_repo_not_cas, never the benign 200 the re-load would otherwise
// produce. This is the ordering guarantee — a concurrent park must not mask a
// misconfiguration as a benign no-op.
func TestReapStageFailure_NonCASRepoRefusalPrecedesReload(t *testing.T) {
	s, rr, au, runID, stageID := reapServer(t, run.StageStateDispatched)
	vehicle := &nonCASReloadParkRepo{
		Repository: rr,
		stageID:    stageID,
		parkState:  run.StageStateAwaitingApproval,
	}
	assertVehicleErasesCAS(t, vehicle)
	s.cfg.RunRepo = vehicle

	w := postReapFailure(t, s, runID, stageID,
		reapFailureRequest{Category: "C", Reason: "reload would show a park"},
		withReapOperator)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (refusal precedes the re-load):\n%s", w.Code, w.Body.String())
	}
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("unmarshal error body: %v\n%s", err, w.Body.String())
	}
	if errBody.Error.Code != "reap_failure_repo_not_cas" {
		t.Errorf("error code = %q, want reap_failure_repo_not_cas (the re-load park must not mask the refusal)\n%s",
			errBody.Error.Code, w.Body.String())
	}

	// The stage was not reaped, and no benign-no-op side effects were produced.
	cur, _ := rr.GetStage(context.Background(), stageID)
	if cur.State != run.StageStateDispatched {
		t.Errorf("stage state = %q, want dispatched", cur.State)
	}
	if entries := reapAudit(au); len(entries) != 0 {
		t.Errorf("dispatch_reaper_failed entries = %d, want 0", len(entries))
	}
}
