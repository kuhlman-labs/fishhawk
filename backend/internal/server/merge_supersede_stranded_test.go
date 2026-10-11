package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// This file pins reconcile-merge's #4222 arms — the opt-in stranded arm
// ({"supersede_stranded": true}), its idle-threshold liveness gate and the
// row-locked re-check behind it, and the settle-only arm — on a REAL pgtest
// database through the production postgres run repository, the chained audit
// repository and a real orchestrator.
//
// Every liveness fixture anchors s.nowFunc to a stage's DATABASE-stamped
// updated_at READ BACK from the repository, never to time.Now: the gate
// compares the process clock with Postgres-stamped columns, and anchoring the
// test clock in the database's domain keeps the boundary deterministic
// regardless of host/container skew (AGENTS.md cross-clock rule, #3048).

// strandedSeed is one stage of a stranded-arm fixture, in sequence order.
type strandedSeed struct {
	typ   run.StageType
	state run.StageState
}

// newStrandedFixture seeds a run with the given stages and NO TransitionRun:
// the run stays `pending`, the legacy shape the #4222 runs are in, so every
// settle these tests observe also exercises Advance's pending → running walk.
// It returns the shared supersedeFixture type so the helpers in
// merge_supersede_test.go apply, but it does not touch newSupersedeFixture.
func newStrandedFixture(t *testing.T, seeds ...strandedSeed) *supersedeFixture {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	orch := &orchestrator.Orchestrator{Runs: runRepo, Audit: auditRepo}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: runRepo, AuditRepo: auditRepo, Orchestrator: orch})

	r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	stages := map[run.StageType]*run.Stage{}
	for i, sd := range seeds {
		created, cerr := runRepo.CreateStage(ctx, run.CreateStageParams{
			RunID: r.ID, Sequence: i, Type: sd.typ,
			ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
		})
		if cerr != nil {
			t.Fatalf("create %s stage: %v", sd.typ, cerr)
		}
		if sd.state == run.StageStateFailed {
			running := driveStageTo(t, runRepo, created, run.StageStateRunning)
			cat := run.FailureC
			failed, ferr := runRepo.TransitionStage(ctx, running.ID, run.StageStateFailed, &run.StageCompletion{FailureCategory: &cat})
			if ferr != nil {
				t.Fatalf("%s stage -> failed: %v", sd.typ, ferr)
			}
			stages[sd.typ] = failed
			continue
		}
		stages[sd.typ] = driveStageTo(t, runRepo, created, sd.state)
	}
	return &supersedeFixture{s: s, runRepo: runRepo, audit: auditRepo, runID: r.ID, stages: stages}
}

// strandedShape is live run 74e8eade's shape: implement stranded `running`
// beside a review gate that never opened.
func strandedShape() []strandedSeed {
	return []strandedSeed{
		{run.StageTypePlan, run.StageStateSucceeded},
		{run.StageTypeImplement, run.StageStateRunning},
		{run.StageTypeReview, run.StageStatePending},
	}
}

const supersedeStrandedBody = `{"supersede_stranded":true}`

// postReconcileBody posts reconcile-merge with a write:runs bearer and the
// given raw body ("" sends no body at all).
func (f *supersedeFixture) postReconcileBody(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+f.runID.String()+"/reconcile-merge", rdr)
	req.SetPathValue("run_id", f.runID.String())
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, reconcileWriteRunsBearer()))
	w := httptest.NewRecorder()
	f.s.handleReconcileMerge(w, req)
	return w
}

// dbStage re-reads a stage so a test sees the DATABASE-stamped columns.
func (f *supersedeFixture) dbStage(t *testing.T, id uuid.UUID) *run.Stage {
	t.Helper()
	got, err := f.runRepo.GetStage(context.Background(), id)
	if err != nil {
		t.Fatalf("get stage: %v", err)
	}
	return got
}

func (f *supersedeFixture) runState(t *testing.T) run.State {
	t.Helper()
	got, err := f.runRepo.GetRun(context.Background(), f.runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	return got.State
}

// anchorNow pins the server clock to stage id's DB-stamped updated_at plus
// offset, and returns that updated_at.
func (f *supersedeFixture) anchorNow(t *testing.T, id uuid.UUID, offset time.Duration) time.Time {
	t.Helper()
	anchor := f.dbStage(t, id).UpdatedAt
	f.s.nowFunc = func() time.Time { return anchor.Add(offset) }
	return anchor
}

// supersedePayloads decodes the run's stage_superseded_by_merge rows keyed by
// stage id, failing on a second row for any stage.
func (f *supersedeFixture) supersedePayloads(t *testing.T) map[uuid.UUID]map[string]any {
	t.Helper()
	out := map[uuid.UUID]map[string]any{}
	for _, e := range f.supersedeRows(t) {
		var p map[string]any
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode supersede payload: %v", err)
		}
		id, err := uuid.Parse(p["stage_id"].(string))
		if err != nil {
			t.Fatalf("payload stage_id: %v", err)
		}
		if _, dup := out[id]; dup {
			t.Errorf("stage %s carries more than one stage_superseded_by_merge row", id)
		}
		out[id] = p
	}
	return out
}

func decodeReconcileOK(t *testing.T, w *httptest.ResponseRecorder) reconcileMergeResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp reconcileMergeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

func decodeReconcileErr(t *testing.T, w *httptest.ResponseRecorder, status int, code string) errorBody {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d (%s):\n%s", w.Code, status, code, w.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if env.Error.Code != code {
		t.Fatalf("code = %q, want %q:\n%s", env.Error.Code, code, w.Body.String())
	}
	return env.Error
}

// strandedProbeRepo wraps the real postgres repository, delegating the two
// capabilities reconcile-merge probes for, and records every stranded-supersede
// call. before, when set, runs ahead of the delegated call — the seam the
// row-locked re-check test uses to land a heartbeat between the handler's
// liveness gate and the write.
type strandedProbeRepo struct {
	run.Repository
	cas    run.StageCASTransitioner
	inner  run.StrandedStageMergeSuperseder
	before func(stageID uuid.UUID)

	mu    sync.Mutex
	calls []uuid.UUID
}

func installStrandedProbe(f *supersedeFixture) *strandedProbeRepo {
	p := &strandedProbeRepo{
		Repository: f.runRepo,
		cas:        f.runRepo.(run.StageCASTransitioner),
		inner:      f.runRepo.(run.StrandedStageMergeSuperseder),
	}
	f.s.cfg.RunRepo = p
	return p
}

func (p *strandedProbeRepo) TransitionStageFrom(ctx context.Context, id uuid.UUID, from, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	return p.cas.TransitionStageFrom(ctx, id, from, to, c)
}

func (p *strandedProbeRepo) SupersedeStrandedStageOnMerge(ctx context.Context, id uuid.UUID, from run.StageState, attempt string, cutoff time.Time) (*run.Stage, error) {
	p.mu.Lock()
	p.calls = append(p.calls, id)
	p.mu.Unlock()
	if p.before != nil {
		p.before(id)
	}
	return p.inner.SupersedeStrandedStageOnMerge(ctx, id, from, attempt, cutoff)
}

func (p *strandedProbeRepo) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

// (1) The 74e8eade shape settles: with the stage idle past the threshold and
// the opt-in set, implement@running and review@pending are both retired with
// reason operator_reconcile_stranded, and the pending run completes.
func TestReconcileMerge_SupersedeStranded_SettlesMergedRun(t *testing.T) {
	f := newStrandedFixture(t, strandedShape()...)
	f.observeMerge(t)
	impl, review := f.stages[run.StageTypeImplement], f.stages[run.StageTypeReview]
	anchor := f.anchorNow(t, impl.ID, 25*time.Hour)

	resp := decodeReconcileOK(t, f.postReconcileBody(t, supersedeStrandedBody))

	if got := f.stageState(t, impl.ID); got != run.StageStateSuperseded {
		t.Errorf("implement = %q, want superseded", got)
	}
	if got := f.stageState(t, review.ID); got != run.StageStateSuperseded {
		t.Errorf("review = %q, want superseded", got)
	}
	if resp.RunState != string(run.StateSucceeded) {
		t.Errorf("response run_state = %q, want succeeded", resp.RunState)
	}
	if got := f.runState(t); got != run.StateSucceeded {
		t.Errorf("run state = %q, want succeeded", got)
	}
	if len(resp.Superseded) != 2 || len(resp.Repaired) != 0 {
		t.Fatalf("superseded = %+v repaired = %+v, want 2 stranded moves and no repairs", resp.Superseded, resp.Repaired)
	}
	for _, m := range resp.Superseded {
		if m.Reason != supersedeReasonOperatorReconcileStranded {
			t.Errorf("response reason for %s = %q, want %q", m.StageType, m.Reason, supersedeReasonOperatorReconcileStranded)
		}
	}
	rows := f.supersedePayloads(t)
	if len(rows) != 2 {
		t.Fatalf("stage_superseded_by_merge rows = %d, want 2", len(rows))
	}
	for _, id := range []uuid.UUID{impl.ID, review.ID} {
		p, ok := rows[id]
		if !ok {
			t.Fatalf("no supersede row for stage %s", id)
		}
		if p["reason"] != supersedeReasonOperatorReconcileStranded {
			t.Errorf("row reason = %v, want %s", p["reason"], supersedeReasonOperatorReconcileStranded)
		}
		if p["idle_threshold_seconds"] != float64(86400) {
			t.Errorf("row idle_threshold_seconds = %v, want 86400", p["idle_threshold_seconds"])
		}
	}
	if got := rows[impl.ID]["last_activity_at"]; got != anchor.UTC().Format(time.RFC3339Nano) {
		t.Errorf("implement last_activity_at = %v, want its DB-stamped updated_at %s", got, anchor.UTC().Format(time.RFC3339Nano))
	}
	if got := rows[impl.ID]["from_state"]; got != string(run.StageStateRunning) {
		t.Errorf("implement from_state = %v, want running", got)
	}
}

// (2) A candidate active inside the threshold refuses the WHOLE call before
// any write. The probe's call count is the isolating assertion: the row-locked
// re-check behind the gate would also refuse this fixture, so the gate's own
// effect is that the capability is never even called.
func TestReconcileMerge_SupersedeStranded_RefusesLiveStage(t *testing.T) {
	f := newStrandedFixture(t, strandedShape()...)
	f.observeMerge(t)
	probe := installStrandedProbe(f)
	impl, review := f.stages[run.StageTypeImplement], f.stages[run.StageTypeReview]
	f.anchorNow(t, impl.ID, time.Hour)

	body := decodeReconcileErr(t, f.postReconcileBody(t, supersedeStrandedBody), http.StatusConflict, "reconcile_merge_stage_live")

	live, _ := body.Details["live_stages"].([]any)
	if len(live) != 1 {
		t.Fatalf("details.live_stages = %v, want exactly the implement stage", body.Details["live_stages"])
	}
	if got := live[0].(map[string]any)["stage_id"]; got != impl.ID.String() {
		t.Errorf("live stage_id = %v, want implement %s", got, impl.ID)
	}
	if body.Details["idle_threshold_seconds"] != float64(86400) {
		t.Errorf("idle_threshold_seconds = %v, want 86400", body.Details["idle_threshold_seconds"])
	}
	if n := probe.callCount(); n != 0 {
		t.Errorf("SupersedeStrandedStageOnMerge calls = %d, want 0 (the gate refuses before any write)", n)
	}
	if got := f.stageState(t, impl.ID); got != run.StageStateRunning {
		t.Errorf("implement = %q, want still running", got)
	}
	if got := f.stageState(t, review.ID); got != run.StageStatePending {
		t.Errorf("review = %q, want still pending", got)
	}
	if rows := f.supersedeRows(t); len(rows) != 0 {
		t.Errorf("stage_superseded_by_merge rows = %d, want 0", len(rows))
	}
	if got := f.runState(t); got != run.StatePending {
		t.Errorf("run state = %q, want still pending", got)
	}
}

// (3) Pins the liveness signal itself: a runner heartbeat bumps the stage's
// updated_at (stages_set_updated_at trigger), so a stage that would be idle by
// its pre-heartbeat clock is live after one heartbeat.
func TestReconcileMerge_SupersedeStranded_HeartbeatKeepsStageLive(t *testing.T) {
	f := newStrandedFixture(t, strandedShape()...)
	f.observeMerge(t)
	impl := f.stages[run.StageTypeImplement]
	ctx := context.Background()

	pre := f.dbStage(t, impl.ID).UpdatedAt
	// Idle by the pre-heartbeat clock: the cutoff sits 1ms after pre.
	f.s.nowFunc = func() time.Time { return pre.Add(strandedStageIdleThreshold + time.Millisecond) }
	time.Sleep(20 * time.Millisecond)

	store, ok := f.runRepo.(run.StageProgressStore)
	if !ok {
		t.Fatal("postgres run repo does not implement run.StageProgressStore")
	}
	applied, err := store.RecordStageProgress(ctx, impl.ID, run.StageProgress{LastEvent: "assistant", TurnsThisAttempt: 1})
	if err != nil || !applied {
		t.Fatalf("record heartbeat: applied=%v err=%v", applied, err)
	}
	progress, err := store.StageProgressByID(ctx, impl.ID)
	if err != nil || progress == nil {
		t.Fatalf("read heartbeat: %v", err)
	}
	post := f.dbStage(t, impl.ID).UpdatedAt
	if post.Before(progress.ReportedAt) {
		t.Fatalf("updated_at %s is before the heartbeat's reported_at %s: the stages_set_updated_at trigger did not fire, so the liveness gate's signal is gone",
			post.Format(time.RFC3339Nano), progress.ReportedAt.Format(time.RFC3339Nano))
	}
	if !post.After(pre.Add(time.Millisecond)) {
		t.Fatalf("precondition: post-heartbeat updated_at %s not after the cutoff %s", post, pre.Add(time.Millisecond))
	}

	decodeReconcileErr(t, f.postReconcileBody(t, supersedeStrandedBody), http.StatusConflict, "reconcile_merge_stage_live")
	if got := f.stageState(t, impl.ID); got != run.StageStateRunning {
		t.Errorf("implement = %q, want still running", got)
	}
}

// Condition 3 (#4222): the row-locked re-check. A heartbeat landing AFTER the
// handler's liveness gate passed but BEFORE the write is refused under the row
// lock against the handler's cutoff; the handler stops the stranded sweep (the
// pending review gate sequenced after implement is NOT retired), writes no row
// and answers reconcile_merge_stage_live.
func TestReconcileMerge_SupersedeStranded_HeartbeatAfterGateRefusedUnderRowLock(t *testing.T) {
	f := newStrandedFixture(t, strandedShape()...)
	f.observeMerge(t)
	impl, review := f.stages[run.StageTypeImplement], f.stages[run.StageTypeReview]
	ctx := context.Background()

	pre := f.dbStage(t, impl.ID).UpdatedAt
	cutoff := pre.Add(time.Millisecond)
	f.s.nowFunc = func() time.Time { return cutoff.Add(strandedStageIdleThreshold) }

	probe := installStrandedProbe(f)
	store := f.runRepo.(run.StageProgressStore)
	probe.before = func(id uuid.UUID) {
		if id != impl.ID {
			return
		}
		time.Sleep(20 * time.Millisecond)
		if applied, err := store.RecordStageProgress(ctx, id, run.StageProgress{LastEvent: "assistant"}); err != nil || !applied {
			t.Errorf("heartbeat between gate and write: applied=%v err=%v", applied, err)
		}
		if got := f.dbStage(t, id).UpdatedAt; !got.After(cutoff) {
			t.Errorf("precondition: heartbeat updated_at %s not after the cutoff %s", got, cutoff)
		}
	}

	body := decodeReconcileErr(t, f.postReconcileBody(t, supersedeStrandedBody), http.StatusConflict, "reconcile_merge_stage_live")

	assertStrandedRefusal(t, body, impl.ID, strandedRefusalRecentlyActive)
	if got := f.stageState(t, impl.ID); got != run.StageStateRunning {
		t.Errorf("implement = %q, want still running (refused under the row lock)", got)
	}
	if got := f.stageState(t, review.ID); got != run.StageStatePending {
		t.Errorf("review = %q, want still pending (the sweep stops at the live stage)", got)
	}
	if n := probe.callCount(); n != 1 {
		t.Errorf("SupersedeStrandedStageOnMerge calls = %d, want 1 (implement only)", n)
	}
	if rows := f.supersedeRows(t); len(rows) != 0 {
		t.Errorf("stage_superseded_by_merge rows = %d, want 0", len(rows))
	}
	if got := f.runState(t); got != run.StatePending {
		t.Errorf("run state = %q, want still pending", got)
	}
}

// assertStrandedRefusal asserts the 409's single live_stages entry names
// stageID with the given row-locked refusal discriminator.
func assertStrandedRefusal(t *testing.T, body errorBody, stageID uuid.UUID, refusal string) {
	t.Helper()
	live, _ := body.Details["live_stages"].([]any)
	if len(live) != 1 {
		t.Fatalf("details.live_stages = %v, want exactly one stage", body.Details["live_stages"])
	}
	entry := live[0].(map[string]any)
	if entry["stage_id"] != stageID.String() || entry["refusal"] != refusal {
		t.Errorf("live stage = %v, want stage_id %s refusal %s", entry, stageID, refusal)
	}
	if _, ok := body.Details["superseded"].([]any); !ok {
		t.Errorf("details.superseded = %v, want a (possibly empty) list", body.Details["superseded"])
	}
}

// A stage that MOVED between the classification and the write (here: the
// runner finished implement) is refused under the row lock, writes no row and
// stops the sweep, so the pending review gate behind it is not retired on the
// stale premise.
func TestReconcileMerge_SupersedeStranded_StateDriftStopsSweep(t *testing.T) {
	f := newStrandedFixture(t, strandedShape()...)
	f.observeMerge(t)
	impl, review := f.stages[run.StageTypeImplement], f.stages[run.StageTypeReview]
	f.anchorNow(t, impl.ID, 25*time.Hour)
	ctx := context.Background()

	probe := installStrandedProbe(f)
	probe.before = func(id uuid.UUID) {
		if id != impl.ID {
			return
		}
		if _, err := f.runRepo.TransitionStage(ctx, id, run.StageStateSucceeded, nil); err != nil {
			t.Errorf("concurrent implement -> succeeded: %v", err)
		}
	}

	body := decodeReconcileErr(t, f.postReconcileBody(t, supersedeStrandedBody), http.StatusConflict, "reconcile_merge_stage_live")
	assertStrandedRefusal(t, body, impl.ID, strandedRefusalStateChanged)
	if got := f.stageState(t, impl.ID); got != run.StageStateSucceeded {
		t.Errorf("implement = %q, want the concurrent writer's succeeded preserved", got)
	}
	if got := f.stageState(t, review.ID); got != run.StageStatePending {
		t.Errorf("review = %q, want still pending (the sweep stops at the drifted stage)", got)
	}
	if rows := f.supersedeRows(t); len(rows) != 0 {
		t.Errorf("stage_superseded_by_merge rows = %d, want 0", len(rows))
	}
}

// A stage RE-DISPATCHED between the classification and the write — same
// `running` state, a new attempt — is refused by the attempt pin the handler
// passes (StageAttemptToken of the classified dispatched_at). The re-dispatch's
// DB-stamped updated_at stays before the anchored cutoff, so the attempt pin is
// the only control in the path.
func TestReconcileMerge_SupersedeStranded_AttemptDriftStopsSweep(t *testing.T) {
	f := newStrandedFixture(t, strandedShape()...)
	f.observeMerge(t)
	impl, review := f.stages[run.StageTypeImplement], f.stages[run.StageTypeReview]
	f.anchorNow(t, impl.ID, 25*time.Hour)
	ctx := context.Background()
	cat := run.FailureC

	probe := installStrandedProbe(f)
	probe.before = func(id uuid.UUID) {
		if id != impl.ID {
			return
		}
		if _, err := f.runRepo.TransitionStage(ctx, id, run.StageStateFailed, &run.StageCompletion{FailureCategory: &cat}); err != nil {
			t.Errorf("implement -> failed: %v", err)
			return
		}
		if _, err := f.runRepo.RetryStage(ctx, id, run.StageStatePending); err != nil {
			t.Errorf("implement retry -> pending: %v", err)
			return
		}
		for _, step := range []run.StageState{run.StageStateDispatched, run.StageStateRunning} {
			if _, err := f.runRepo.TransitionStage(ctx, id, step, nil); err != nil {
				t.Errorf("implement re-dispatch -> %s: %v", step, err)
				return
			}
		}
	}

	body := decodeReconcileErr(t, f.postReconcileBody(t, supersedeStrandedBody), http.StatusConflict, "reconcile_merge_stage_live")
	assertStrandedRefusal(t, body, impl.ID, strandedRefusalAttemptChanged)
	after := f.dbStage(t, impl.ID)
	if after.State != run.StageStateRunning {
		t.Errorf("implement = %q, want the fresh attempt still running", after.State)
	}
	if run.StageAttemptToken(after.DispatchedAt) == run.StageAttemptToken(impl.DispatchedAt) {
		t.Fatalf("precondition: the re-dispatch did not change the attempt token")
	}
	if got := f.stageState(t, review.ID); got != run.StageStatePending {
		t.Errorf("review = %q, want still pending", got)
	}
	if rows := f.supersedeRows(t); len(rows) != 0 {
		t.Errorf("stage_superseded_by_merge rows = %d, want 0", len(rows))
	}
}

// (4) Without the opt-in a stranded stage is left alone and the verb answers
// exactly as before #4222 — for no body, a whitespace-only body and an
// explicit false.
func TestReconcileMerge_SupersedeStranded_FlagAbsentLeavesStrandedStageAlone(t *testing.T) {
	for name, body := range map[string]string{
		"no body":         "",
		"whitespace body": "  \n\t",
		"explicit false":  `{"supersede_stranded":false}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newStrandedFixture(t, strandedShape()...)
			f.observeMerge(t)
			impl, review := f.stages[run.StageTypeImplement], f.stages[run.StageTypeReview]
			f.anchorNow(t, impl.ID, 25*time.Hour)

			decodeReconcileErr(t, f.postReconcileBody(t, body), http.StatusConflict, "reconcile_merge_not_applicable")
			if got := f.stageState(t, impl.ID); got != run.StageStateRunning {
				t.Errorf("implement = %q, want still running", got)
			}
			if got := f.stageState(t, review.ID); got != run.StageStatePending {
				t.Errorf("review = %q, want still pending", got)
			}
			if rows := f.supersedeRows(t); len(rows) != 0 {
				t.Errorf("stage_superseded_by_merge rows = %d, want 0", len(rows))
			}
		})
	}
}

// (5) The merge-evidence guard still comes first under the opt-in.
func TestReconcileMerge_SupersedeStranded_RequiresMergeEvidence(t *testing.T) {
	f := newStrandedFixture(t, strandedShape()...)
	probe := installStrandedProbe(f)
	impl := f.stages[run.StageTypeImplement]
	f.anchorNow(t, impl.ID, 25*time.Hour)

	decodeReconcileErr(t, f.postReconcileBody(t, supersedeStrandedBody), http.StatusConflict, "reconcile_merge_pr_not_merged")
	if n := probe.callCount(); n != 0 {
		t.Errorf("SupersedeStrandedStageOnMerge calls = %d, want 0", n)
	}
	if got := f.stageState(t, impl.ID); got != run.StageStateRunning {
		t.Errorf("implement = %q, want still running", got)
	}
	if rows := f.supersedeRows(t); len(rows) != 0 {
		t.Errorf("stage_superseded_by_merge rows = %d, want 0", len(rows))
	}
}

// (6) A malformed body is a 400 before any read. Every case would otherwise
// carry supersede_stranded=true (or decode to nothing) on an idle, merged
// fixture, so a decoder that tolerated it would move the stranded stages.
func TestReconcileMerge_SupersedeStranded_MalformedBodyRefused(t *testing.T) {
	for name, body := range map[string]string{
		"wrong type":    `{"supersede_stranded":"yes"}`,
		"unknown field": `{"supersede_stranded":true,"force":true}`,
		"trailing data": `{"supersede_stranded":true}{"supersede_stranded":true}`,
		"not an object": `[true]`,
		"truncated":     `{"supersede_stranded":`,
		"over the cap":  `{"supersede_stranded":true}` + strings.Repeat(" ", maxReconcileMergeBodyBytes),
	} {
		t.Run(name, func(t *testing.T) {
			f := newStrandedFixture(t, strandedShape()...)
			f.observeMerge(t)
			impl, review := f.stages[run.StageTypeImplement], f.stages[run.StageTypeReview]
			f.anchorNow(t, impl.ID, 25*time.Hour)

			eb := decodeReconcileErr(t, f.postReconcileBody(t, body), http.StatusBadRequest, "validation_failed")
			if eb.Details["field"] != "body" {
				t.Errorf("details.field = %v, want body", eb.Details["field"])
			}
			if got := f.stageState(t, impl.ID); got != run.StageStateRunning {
				t.Errorf("implement = %q, want still running", got)
			}
			if got := f.stageState(t, review.ID); got != run.StageStatePending {
				t.Errorf("review = %q, want still pending", got)
			}
			if rows := f.supersedeRows(t); len(rows) != 0 {
				t.Errorf("stage_superseded_by_merge rows = %d, want 0", len(rows))
			}
		})
	}
}

// (7) A run repository without the stranded capability refuses the opt-in
// with 503 rather than degrading to an ordinary transition.
func TestReconcileMerge_SupersedeStranded_CapabilityAbsentIs503(t *testing.T) {
	f := newStrandedFixture(t, strandedShape()...)
	f.observeMerge(t)
	impl, review := f.stages[run.StageTypeImplement], f.stages[run.StageTypeReview]
	f.anchorNow(t, impl.ID, 25*time.Hour)
	f.s.cfg.RunRepo = &nonCASRepo{f.runRepo}

	eb := decodeReconcileErr(t, f.postReconcileBody(t, supersedeStrandedBody), http.StatusServiceUnavailable, "reconcile_merge_unconfigured")
	if !strings.Contains(eb.Message, "run.StrandedStageMergeSuperseder") {
		t.Errorf("message = %q, want it to name the missing capability", eb.Message)
	}
	if got := f.stageState(t, impl.ID); got != run.StageStateRunning {
		t.Errorf("implement = %q, want still running", got)
	}
	if got := f.stageState(t, review.ID); got != run.StageStatePending {
		t.Errorf("review = %q, want still pending", got)
	}
	if rows := f.supersedeRows(t); len(rows) != 0 {
		t.Errorf("stage_superseded_by_merge rows = %d, want 0", len(rows))
	}
}

// (8) Parked and stranded stages in one call: each is retired once under its
// own reason, the run settles, and a repeat POST moves and repairs nothing.
func TestReconcileMerge_SupersedeStranded_ParkedAndStrandedTogether(t *testing.T) {
	f := newStrandedFixture(t,
		strandedSeed{run.StageTypePlan, run.StageStateSucceeded},
		strandedSeed{run.StageTypeImplement, run.StageStateRunning},
		strandedSeed{run.StageTypeReview, run.StageStateAwaitingApproval},
	)
	f.observeMerge(t)
	impl, review := f.stages[run.StageTypeImplement], f.stages[run.StageTypeReview]
	f.anchorNow(t, impl.ID, 25*time.Hour)

	resp := decodeReconcileOK(t, f.postReconcileBody(t, supersedeStrandedBody))
	if len(resp.Superseded) != 2 || len(resp.Repaired) != 0 {
		t.Fatalf("superseded = %+v repaired = %+v, want 2 moves and no repairs", resp.Superseded, resp.Repaired)
	}
	rows := f.supersedePayloads(t)
	if len(rows) != 2 {
		t.Fatalf("stage_superseded_by_merge rows = %d, want exactly one per stage", len(rows))
	}
	if got := rows[review.ID]["reason"]; got != supersedeReasonOperatorReconcile {
		t.Errorf("review reason = %v, want %s", got, supersedeReasonOperatorReconcile)
	}
	if got := rows[impl.ID]["reason"]; got != supersedeReasonOperatorReconcileStranded {
		t.Errorf("implement reason = %v, want %s", got, supersedeReasonOperatorReconcileStranded)
	}
	if got := f.runState(t); got != run.StateSucceeded {
		t.Errorf("run state = %q, want succeeded", got)
	}

	again := decodeReconcileOK(t, f.postReconcileBody(t, supersedeStrandedBody))
	if len(again.Superseded) != 0 || len(again.Repaired) != 0 {
		t.Errorf("repeat POST superseded = %+v repaired = %+v, want two empty lists", again.Superseded, again.Repaired)
	}
	if rows := f.supersedePayloads(t); len(rows) != 2 {
		t.Errorf("rows after repeat = %d, want still 2", len(rows))
	}
}

// strandedFailFirstAppendAudit fails the FIRST stage_superseded_by_merge append
// for one stage and passes everything else through, leaving the missing-row
// residue the transition-first ordering allows.
type strandedFailFirstAppendAudit struct {
	audit.Repository
	stageID uuid.UUID
	failed  bool
}

func (a *strandedFailFirstAppendAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if !a.failed && p.Category == CategoryStageSupersededByMerge && p.StageID != nil && *p.StageID == a.stageID {
		a.failed = true
		return nil, errors.New("injected append failure")
	}
	return a.Repository.AppendChained(ctx, p)
}

// (8b) The repair scan must exclude this invocation's STRANDED moves. With the
// stranded row's append failing, the stage is superseded with no row; the same
// call must not "repair" its own move (that is a later call's job, under reason
// repair), and the later call restores exactly one row.
func TestReconcileMerge_SupersedeStranded_SameCallRepairExcludesStrandedMove(t *testing.T) {
	f := newStrandedFixture(t, strandedShape()...)
	f.observeMerge(t)
	impl := f.stages[run.StageTypeImplement]
	f.anchorNow(t, impl.ID, 25*time.Hour)
	f.s.cfg.AuditRepo = &strandedFailFirstAppendAudit{Repository: f.audit, stageID: impl.ID}

	resp := decodeReconcileOK(t, f.postReconcileBody(t, supersedeStrandedBody))
	if got := f.stageState(t, impl.ID); got != run.StageStateSuperseded {
		t.Fatalf("implement = %q, want superseded", got)
	}
	if len(resp.Repaired) != 0 {
		t.Errorf("repaired = %+v, want empty: the same call must not repair its own stranded move", resp.Repaired)
	}
	if _, has := f.supersedePayloads(t)[impl.ID]; has {
		t.Fatalf("implement carries a row after its append was injected to fail")
	}

	later := decodeReconcileOK(t, f.postReconcileBody(t, ""))
	if len(later.Repaired) != 1 || later.Repaired[0].StageID != impl.ID || later.Repaired[0].Reason != supersedeReasonRepair {
		t.Errorf("later repaired = %+v, want exactly the implement stage under reason repair", later.Repaired)
	}
	if got := f.supersedePayloads(t)[impl.ID]["reason"]; got != supersedeReasonRepair {
		t.Errorf("implement row reason = %v, want %s", got, supersedeReasonRepair)
	}
}

// (9) The settle-only arm: a merged, still-pending run whose stages ALL
// succeeded moves nothing and settles, with no body.
func TestReconcileMerge_SettleOnlyArm(t *testing.T) {
	f := newStrandedFixture(t,
		strandedSeed{run.StageTypePlan, run.StageStateSucceeded},
		strandedSeed{run.StageTypeImplement, run.StageStateSucceeded},
		strandedSeed{run.StageTypeReview, run.StageStateSucceeded},
	)
	f.observeMerge(t)

	resp := decodeReconcileOK(t, f.postReconcileBody(t, ""))
	if len(resp.Superseded) != 0 || len(resp.Repaired) != 0 {
		t.Errorf("superseded = %+v repaired = %+v, want two empty lists", resp.Superseded, resp.Repaired)
	}
	if resp.RunState != string(run.StateSucceeded) {
		t.Errorf("response run_state = %q, want succeeded", resp.RunState)
	}
	if got := f.runState(t); got != run.StateSucceeded {
		t.Errorf("run state = %q, want succeeded", got)
	}

	// The run is now terminal, so the settle-only shape no longer holds and a
	// repeat POST has nothing to do.
	decodeReconcileErr(t, f.postReconcileBody(t, ""), http.StatusConflict, "reconcile_merge_not_applicable")
}

// (9b) The settle-only arm requires EVERY stage succeeded: a failed stage keeps
// the 409 and the run is not completed.
func TestReconcileMerge_SettleOnlyRefusesFailedStage(t *testing.T) {
	f := newStrandedFixture(t,
		strandedSeed{run.StageTypePlan, run.StageStateSucceeded},
		strandedSeed{run.StageTypeImplement, run.StageStateFailed},
		strandedSeed{run.StageTypeReview, run.StageStateSucceeded},
	)
	f.observeMerge(t)

	decodeReconcileErr(t, f.postReconcileBody(t, ""), http.StatusConflict, "reconcile_merge_not_applicable")
	if got := f.runState(t); got != run.StatePending {
		t.Errorf("run state = %q, want still pending", got)
	}
}

// (10) The b50f7945 shape: a pending run with a parked review gate settles via
// the ordinary parked sweep plus Advance's pending → running walk.
func TestReconcileMerge_LegacyPendingRunWithParkedReview(t *testing.T) {
	f := newStrandedFixture(t,
		strandedSeed{run.StageTypePlan, run.StageStateSucceeded},
		strandedSeed{run.StageTypeImplement, run.StageStateSucceeded},
		strandedSeed{run.StageTypeReview, run.StageStateAwaitingApproval},
	)
	f.observeMerge(t)

	resp := decodeReconcileOK(t, f.postReconcileBody(t, ""))
	if len(resp.Superseded) != 1 || resp.Superseded[0].Reason != supersedeReasonOperatorReconcile {
		t.Errorf("superseded = %+v, want the review stage under operator_reconcile", resp.Superseded)
	}
	if resp.RunState != string(run.StateSucceeded) {
		t.Errorf("response run_state = %q, want succeeded", resp.RunState)
	}
}
