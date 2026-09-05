package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
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

// supersedeFixture is the pg-backed seam every merge-supersede test drives.
// It is deliberately REAL end to end — a pgtest Postgres, the production
// postgres run repository (so the compare-and-swap, migration 0079's widened
// stages_state_check and the type-aware transition-boundary arm all execute),
// the real chained audit repository and a real orchestrator — because the thing
// under test spans a domain predicate, a CHECK constraint, an HTTP payload and
// a completion guard. A fake repository would prove none of that.
type supersedeFixture struct {
	s       *Server
	runRepo run.Repository
	audit   audit.Repository
	runID   uuid.UUID
	stages  map[run.StageType]*run.Stage
}

// newSupersedeFixture seeds the shape #3083 is about: a run whose plan and
// implement stages succeeded, whose acceptance stage a fix-up pass re-parked at
// awaiting_host_dispatch, and whose review gate is still awaiting_approval.
// stageStates lets a caller vary the parked states per test.
func newSupersedeFixture(t *testing.T, stageStates map[run.StageType]run.StageState) *supersedeFixture {
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
	if _, err := runRepo.TransitionRun(ctx, r.ID, run.StateRunning); err != nil {
		t.Fatalf("run -> running: %v", err)
	}

	order := []run.StageType{run.StageTypePlan, run.StageTypeImplement, run.StageTypeAcceptance, run.StageTypeReview}
	stages := map[run.StageType]*run.Stage{}
	for i, st := range order {
		created, cerr := runRepo.CreateStage(ctx, run.CreateStageParams{
			RunID: r.ID, Sequence: i, Type: st,
			ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
		})
		if cerr != nil {
			t.Fatalf("create %s stage: %v", st, cerr)
		}
		want, ok := stageStates[st]
		if !ok {
			want = run.StageStateSucceeded
		}
		stages[st] = driveStageTo(t, runRepo, created, want)
	}
	return &supersedeFixture{s: s, runRepo: runRepo, audit: auditRepo, runID: r.ID, stages: stages}
}

// driveStageTo walks a freshly created (pending) stage to `want` through the
// LEGAL state machine, one edge at a time. The seed has to be legal: forcing a
// state the machine refuses would prove the fixture wrong rather than the
// behaviour under test, and it is the real machine that decides which parks a
// merge can actually strand a stage in.
func driveStageTo(t *testing.T, repo run.Repository, stage *run.Stage, want run.StageState) *run.Stage {
	t.Helper()
	ctx := context.Background()
	var path []run.StageState
	switch want {
	case run.StageStatePending:
		path = nil
	case run.StageStateAwaitingHostDispatch:
		path = []run.StageState{run.StageStateAwaitingHostDispatch}
	case run.StageStateDispatched:
		path = []run.StageState{run.StageStateDispatched}
	case run.StageStateRunning:
		path = []run.StageState{run.StageStateDispatched, run.StageStateRunning}
	case run.StageStateAwaitingChildren:
		path = []run.StageState{run.StageStateAwaitingChildren}
	case run.StageStateAwaitingDeployApproval:
		path = []run.StageState{run.StageStateAwaitingDeployApproval}
	case run.StageStateAwaitingScopeDecision:
		path = []run.StageState{run.StageStateDispatched, run.StageStateRunning, run.StageStateAwaitingScopeDecision}
	case run.StageStateAwaitingApproval:
		path = []run.StageState{run.StageStateDispatched, run.StageStateRunning, run.StageStateAwaitingApproval}
	case run.StageStateSucceeded:
		path = []run.StageState{run.StageStateDispatched, run.StageStateRunning, run.StageStateSucceeded}
	default:
		t.Fatalf("driveStageTo: no legal path seeded for %q", want)
	}
	cur := stage
	for _, step := range path {
		moved, err := repo.TransitionStage(ctx, cur.ID, step, nil)
		if err != nil {
			t.Fatalf("%s stage -> %s: %v", stage.Type, step, err)
		}
		cur = moved
	}
	return cur
}

// parkedShape is the default seed: acceptance re-parked for a host spawn, the
// review gate still open — the exact residue a fix-up pass leaves behind.
func parkedShape() map[run.StageType]run.StageState {
	return map[run.StageType]run.StageState{
		run.StageTypeAcceptance: run.StageStateAwaitingHostDispatch,
		run.StageTypeReview:     run.StageStateAwaitingApproval,
	}
}

// observeMerge records the merge evidence the reconcile verb's precondition and
// the completion_blocked recovery discrimination both read.
func (f *supersedeFixture) observeMerge(t *testing.T) {
	t.Helper()
	if _, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:     f.runID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryPRMerged,
		Payload:   json.RawMessage(`{"merged":true}`),
	}); err != nil {
		t.Fatalf("append pr_merged: %v", err)
	}
}

func (f *supersedeFixture) stageState(t *testing.T, id uuid.UUID) run.StageState {
	t.Helper()
	got, err := f.runRepo.GetStage(context.Background(), id)
	if err != nil {
		t.Fatalf("get stage: %v", err)
	}
	return got.State
}

// supersedeRows returns the run's stage_superseded_by_merge entries.
func (f *supersedeFixture) supersedeRows(t *testing.T) []*audit.Entry {
	t.Helper()
	entries, err := f.audit.ListForRunByCategory(context.Background(), f.runID, CategoryStageSupersededByMerge)
	if err != nil {
		t.Fatalf("list supersede rows: %v", err)
	}
	return entries
}

func (f *supersedeFixture) postReconcile(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+f.runID.String()+"/reconcile-merge", nil)
	req.SetPathValue("run_id", f.runID.String())
	w := httptest.NewRecorder()
	f.s.handleReconcileMerge(w, req)
	return w
}

// getRunBody drives the real GET /v0/runs/{run_id} handler and returns the raw
// response BYTES — not a decoded struct — so an assertion can be made against
// the literal wire value.
func (f *supersedeFixture) getRunBody(t *testing.T) []byte {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+f.runID.String(), nil)
	req.SetPathValue("run_id", f.runID.String())
	w := httptest.NewRecorder()
	f.s.handleGetRun(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET run status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	return w.Body.Bytes()
}

// TestReconcileMerge_SupersedesParkedStagesAndCompletesRun is the primary
// CROSS-BOUNDARY test (#3083): it drives POST /v0/runs/{run_id}/reconcile-merge
// on the exact shape that stranded four live runs and asserts across every
// layer in ONE pass —
//
//   - PERSISTENCE: the acceptance and review rows read `superseded` from the
//     DATABASE, which proves migration 0079's widened stages_state_check admits
//     the value (a no-op migration fails here with SQLSTATE 23514, not a silent
//     pass) and that the type-aware transition-boundary arm passed the pair;
//   - AUDIT: exactly one stage_superseded_by_merge row per swept stage, each
//     naming the stage and the state it was parked in;
//   - ORCHESTRATION: Orchestrator.completeRun's #968 guard now passes — every
//     stage is terminal — so the run reaches `succeeded` with no merge
//     re-observation and no PR activity;
//   - THE READ SURFACE: completion_blocked is GONE from the subsequent
//     GET /v0/runs/{id}.
func TestReconcileMerge_SupersedesParkedStagesAndCompletesRun(t *testing.T) {
	f := newSupersedeFixture(t, parkedShape())
	f.observeMerge(t)

	// Before: the run is blocked and says so, naming the reconcile verb.
	before := f.getRunBody(t)
	if !strings.Contains(string(before), `"completion_blocked"`) {
		t.Fatalf("GET run before reconcile omits completion_blocked:\n%s", before)
	}

	w := f.postReconcile(t)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp reconcileMergeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if len(resp.Superseded) != 2 {
		t.Fatalf("superseded = %d entries, want 2 (acceptance + review):\n%s", len(resp.Superseded), w.Body.String())
	}
	for _, got := range resp.Superseded {
		if got.Reason != supersedeReasonOperatorReconcile {
			t.Errorf("reason = %q, want %q", got.Reason, supersedeReasonOperatorReconcile)
		}
	}

	// PERSISTENCE.
	for _, st := range []run.StageType{run.StageTypeAcceptance, run.StageTypeReview} {
		if got := f.stageState(t, f.stages[st].ID); got != run.StageStateSuperseded {
			t.Errorf("%s stage state = %q, want superseded", st, got)
		}
	}
	// The stages the pair table does NOT admit are untouched.
	for _, st := range []run.StageType{run.StageTypePlan, run.StageTypeImplement} {
		if got := f.stageState(t, f.stages[st].ID); got != run.StageStateSucceeded {
			t.Errorf("%s stage state = %q, want succeeded (untouched)", st, got)
		}
	}

	// AUDIT: one row per swept stage, each naming its from-state.
	rows := f.supersedeRows(t)
	if len(rows) != 2 {
		t.Fatalf("stage_superseded_by_merge rows = %d, want 2", len(rows))
	}
	fromByStage := map[uuid.UUID]string{}
	for _, e := range rows {
		var p struct {
			StageID   string `json:"stage_id"`
			StageType string `json:"stage_type"`
			FromState string `json:"from_state"`
			Reason    string `json:"reason"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode audit payload: %v", err)
		}
		if e.StageID == nil {
			t.Fatalf("audit row carries no stage_id column")
		}
		if p.Reason != supersedeReasonOperatorReconcile {
			t.Errorf("audit reason = %q, want %q", p.Reason, supersedeReasonOperatorReconcile)
		}
		fromByStage[*e.StageID] = p.FromState
	}
	if got := fromByStage[f.stages[run.StageTypeAcceptance].ID]; got != string(run.StageStateAwaitingHostDispatch) {
		t.Errorf("acceptance from_state = %q, want awaiting_host_dispatch", got)
	}
	if got := fromByStage[f.stages[run.StageTypeReview].ID]; got != string(run.StageStateAwaitingApproval) {
		t.Errorf("review from_state = %q, want awaiting_approval", got)
	}

	// ORCHESTRATION.
	after, err := f.runRepo.GetRun(context.Background(), f.runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if after.State != run.StateSucceeded {
		t.Errorf("run state = %q, want succeeded (the #968 guard passes a terminal superseded stage)", after.State)
	}
	if resp.RunState != string(run.StateSucceeded) {
		t.Errorf("response run_state = %q, want succeeded", resp.RunState)
	}

	// THE READ SURFACE.
	if body := f.getRunBody(t); strings.Contains(string(body), `"completion_blocked"`) {
		t.Errorf("GET run after reconcile still carries completion_blocked:\n%s", body)
	}
}

// TestSupersededStateCrossesTheWire is binding approval CONDITION 3: a
// `superseded` value persisted in the DATABASE must survive serialization all
// the way into the MCP client's decoded Run/Stage.
//
// Both assertions are made on the LITERAL WIRE STRING, never on a Go constant —
// a constant comparison passes even when the json tag is wrong, which is exactly
// the seam a missing enum member or an unmirrored tag hides in.
func TestSupersededStateCrossesTheWire(t *testing.T) {
	f := newSupersedeFixture(t, parkedShape())
	f.observeMerge(t)
	if w := f.postReconcile(t); w.Code != http.StatusOK {
		t.Fatalf("reconcile status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	// 1. The RESPONSE BYTES of GET /v0/runs/{run_id}/stages carry the literal
	//    string, so the state is not coerced or dropped on the way out.
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+f.runID.String()+"/stages", nil)
	req.SetPathValue("run_id", f.runID.String())
	w := httptest.NewRecorder()
	f.s.handleListRunStages(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list stages status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"state":"superseded"`) {
		t.Fatalf(`stage list bytes do not carry "state":"superseded":\n%s`, w.Body.String())
	}

	// 2. The MCP client's own decode of those same bytes. Decoding through the
	//    mirror's json tags is what proves the tag is right; the assertion is
	//    again on the literal wire value the mirror produced.
	var decoded struct {
		Items []struct {
			Type  string `json:"type"`
			State string `json:"state"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode stage list: %v", err)
	}
	found := false
	for _, st := range decoded.Items {
		if st.State == "superseded" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no decoded stage carries the literal state \"superseded\": %+v", decoded.Items)
	}

	// 3. The other half of the wire contract this slice adds: the
	//    completion_blocked projection. Asserted on the BYTES of
	//    GET /v0/runs/{run_id} for a run that IS blocked, again on literal
	//    strings, because a Go-constant comparison would pass with a wrong or
	//    missing json tag.
	//
	//    The MCP end of BOTH literals — "state":"superseded" decoding into
	//    mcpserver.Stage and this completion_blocked object decoding into the
	//    Run mirror, plus the stage-wait terminal classification of the literal
	//    string "superseded" — is asserted in
	//    backend/internal/mcpserver/{tools,stage_wait}_test.go. It cannot be
	//    asserted from here: backend/internal/server IMPORTS
	//    backend/internal/mcpserver (the /mcp route), and the mirror's api
	//    client is unexported, so there is no in-process path from this package
	//    to a decoded mirror. The two halves meet on the literal byte sequences
	//    both sides assert against.
	blocked := newSupersedeFixture(t, parkedShape())
	blocked.observeMerge(t)
	body := string(blocked.getRunBody(t))
	for _, literal := range []string{
		`"completion_blocked"`,
		`"stage_state":"awaiting_host_dispatch"`,
		`"recovery":"reconcile-merge"`,
	} {
		if !strings.Contains(body, literal) {
			t.Errorf("GET /v0/runs/{run_id} bytes do not carry %s:\n%s", literal, body)
		}
	}
}

// TestReconcileMerge_RefusesUnmergedRun pins the merged-PR PRECONDITION: without
// it the verb could manufacture a `succeeded` run for a change that never
// shipped. Error identity alone is insufficient here — the control's effect is
// COMMITTED STATE — so every stage is RE-READ after the call to prove none
// moved, and the audit chain is checked to prove no row was written.
func TestReconcileMerge_RefusesUnmergedRun(t *testing.T) {
	f := newSupersedeFixture(t, parkedShape())
	// Deliberately NO merge observation.

	w := f.postReconcile(t)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "reconcile_merge_pr_not_merged") {
		t.Errorf("body missing reconcile_merge_pr_not_merged: %s", w.Body.String())
	}
	if got := f.stageState(t, f.stages[run.StageTypeAcceptance].ID); got != run.StageStateAwaitingHostDispatch {
		t.Errorf("acceptance state = %q, want awaiting_host_dispatch (a refused reconcile moves nothing)", got)
	}
	if got := f.stageState(t, f.stages[run.StageTypeReview].ID); got != run.StageStateAwaitingApproval {
		t.Errorf("review state = %q, want awaiting_approval (a refused reconcile moves nothing)", got)
	}
	if rows := f.supersedeRows(t); len(rows) != 0 {
		t.Errorf("stage_superseded_by_merge rows = %d, want 0 on a refusal", len(rows))
	}
	after, err := f.runRepo.GetRun(context.Background(), f.runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if after.State != run.StateRunning {
		t.Errorf("run state = %q, want running (a refused reconcile completes nothing)", after.State)
	}
}

// TestReconcileMerge_RefusesWhenNothingApplicable pins the second refusal: a
// merged run whose only non-terminal stage is one the DEFAULT-DENY pair table
// does not admit has nothing for this verb to do, and it must say so rather than
// sweeping the stage.
func TestReconcileMerge_RefusesWhenNothingApplicable(t *testing.T) {
	f := newSupersedeFixture(t, map[run.StageType]run.StageState{
		run.StageTypeImplement: run.StageStateRunning,
	})
	f.observeMerge(t)

	w := f.postReconcile(t)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "reconcile_merge_not_applicable") {
		t.Errorf("body missing reconcile_merge_not_applicable: %s", w.Body.String())
	}
	if got := f.stageState(t, f.stages[run.StageTypeImplement].ID); got != run.StageStateRunning {
		t.Errorf("implement state = %q, want running (default deny)", got)
	}
	if rows := f.supersedeRows(t); len(rows) != 0 {
		t.Errorf("stage_superseded_by_merge rows = %d, want 0", len(rows))
	}
}

// TestSupersedeParkedStages_DefaultDenyLeavesRunningStageUntouched pins the
// #968 invariant FROM THE SWEEP SIDE: a merged run holding a genuinely
// non-terminal stage the merge did not supersede keeps that stage AND stays
// `running`. Widening the pair table to a state-only allow-list reddens this.
func TestSupersedeParkedStages_DefaultDenyLeavesRunningStageUntouched(t *testing.T) {
	f := newSupersedeFixture(t, map[run.StageType]run.StageState{
		run.StageTypeImplement:  run.StageStateRunning,
		run.StageTypeAcceptance: run.StageStateAwaitingHostDispatch,
	})
	f.observeMerge(t)

	moved := f.s.supersedeParkedStagesOnMerge(context.Background(), f.runID, nil, supersedeReasonMergeObserved)
	if len(moved) != 1 {
		t.Fatalf("moved = %d stages, want exactly 1 (acceptance only)", len(moved))
	}
	if moved[0].StageID != f.stages[run.StageTypeAcceptance].ID {
		t.Errorf("moved stage = %s, want the acceptance stage", moved[0].StageID)
	}
	if got := f.stageState(t, f.stages[run.StageTypeImplement].ID); got != run.StageStateRunning {
		t.Errorf("implement state = %q, want running: a running stage is NOT merge-supersedable", got)
	}
	// And the run must still refuse to complete.
	f.s.advanceRunAfterReviewResolve(context.Background(), f.runID)
	after, err := f.runRepo.GetRun(context.Background(), f.runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if after.State != run.StateRunning {
		t.Errorf("run state = %q, want running: completeRun must refuse around a genuinely non-terminal stage", after.State)
	}
}

// TestSupersedeParkedStages_SkipStageIDIsHonored pins the sweep's skip guard AT
// THE PRIMITIVE, where it is genuinely reachable: review@awaiting_approval is
// itself a pair-table row, so a sweep handed that stage's id as skipStageID must
// leave it alone while still superseding the acceptance stage.
//
// Reachability was ESTABLISHED, not assumed (binding condition 2(b)): the guard
// only does work when the skipped stage's (type, state) is admissible, which is
// exactly the state this test seeds. Deleting the skipStageID branch supersedes
// the review stage too and reddens the first assertion.
func TestSupersedeParkedStages_SkipStageIDIsHonored(t *testing.T) {
	f := newSupersedeFixture(t, parkedShape())
	reviewID := f.stages[run.StageTypeReview].ID

	moved := f.s.supersedeParkedStagesOnMerge(context.Background(), f.runID, &reviewID, supersedeReasonMergeObserved)

	if got := f.stageState(t, reviewID); got != run.StageStateAwaitingApproval {
		t.Errorf("review state = %q, want awaiting_approval: skipStageID must exempt the caller's own stage", got)
	}
	if len(moved) != 1 || moved[0].StageID != f.stages[run.StageTypeAcceptance].ID {
		t.Fatalf("moved = %+v, want exactly the acceptance stage", moved)
	}
	rows := f.supersedeRows(t)
	if len(rows) != 1 {
		t.Errorf("stage_superseded_by_merge rows = %d, want 1 (the skipped stage draws none)", len(rows))
	}
}

// staleStageListRepo hands the sweep a STALE view of the run's stages while the
// database holds something else. It seeds the classify→CAS race BY
// CONSTRUCTION — no call into the control under test — so the compare-and-swap
// refuses on a genuine premise mismatch rather than on a fixture failure.
type staleStageListRepo struct {
	run.Repository
	cas    run.StageCASTransitioner
	stales []*run.Stage
}

func (r *staleStageListRepo) ListStagesForRun(context.Context, uuid.UUID) ([]*run.Stage, error) {
	return r.stales, nil
}

func (r *staleStageListRepo) TransitionStageFrom(ctx context.Context, id uuid.UUID, from, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	return r.cas.TransitionStageFrom(ctx, id, from, to, c)
}

// TestSupersedeParkedStages_NoAuditRowWhenCASRefuses pins the TRANSITION-FIRST
// ordering. The audit chain is append-only, so a row written before a CAS that
// then refuses would be an immutable record of a supersession that never
// happened — the failure mode must be a MISSING row, never a false one.
//
// The mismatch is seeded by construction: the repository reports the acceptance
// stage as parked at awaiting_host_dispatch while the real row has already moved
// to running, so TransitionStageFrom's compare against the ROW-LOCKED current
// state refuses with StageStateChangedError. Moving the audit append ahead of
// the transition reddens this.
func TestSupersedeParkedStages_NoAuditRowWhenCASRefuses(t *testing.T) {
	f := newSupersedeFixture(t, parkedShape())
	ctx := context.Background()

	acc := f.stages[run.StageTypeAcceptance]
	// The DATABASE moves on...
	if _, err := f.runRepo.TransitionStage(ctx, acc.ID, run.StageStateDispatched, nil); err != nil {
		t.Fatalf("acceptance -> dispatched: %v", err)
	}
	// ...while the sweep still sees the pre-move snapshot.
	stale := &run.Stage{
		ID: acc.ID, RunID: f.runID, Sequence: acc.Sequence,
		Type: run.StageTypeAcceptance, State: run.StageStateAwaitingHostDispatch,
	}
	cas, ok := f.runRepo.(run.StageCASTransitioner)
	if !ok {
		t.Fatal("postgres run repository must implement run.StageCASTransitioner")
	}
	f.s.cfg.RunRepo = &staleStageListRepo{Repository: f.runRepo, cas: cas, stales: []*run.Stage{stale}}

	moved := f.s.supersedeParkedStagesOnMerge(ctx, f.runID, nil, supersedeReasonMergeObserved)

	if len(moved) != 0 {
		t.Errorf("moved = %+v, want none: the CAS refused", moved)
	}
	if rows := f.supersedeRows(t); len(rows) != 0 {
		t.Fatalf("stage_superseded_by_merge rows = %d, want 0: a refused CAS must leave a MISSING row, never a false one", len(rows))
	}
	if got := f.stageState(t, acc.ID); got != run.StageStateDispatched {
		t.Errorf("acceptance state = %q, want dispatched: a refused CAS must not destroy the concurrent writer's state", got)
	}
}

// nonCASRepo strips the run.StageCASTransitioner capability, which the sweep
// hard-REQUIRES: degrading to the non-CAS TransitionStage would apply the move
// on a stale premise and could destroy a live park.
type nonCASRepo struct{ run.Repository }

// TestSupersedeParkedStages_RefusesNonCASRepository pins that fail-closed
// requirement: a repository without the CAS capability sweeps NOTHING rather
// than falling back.
func TestSupersedeParkedStages_RefusesNonCASRepository(t *testing.T) {
	f := newSupersedeFixture(t, parkedShape())
	f.s.cfg.RunRepo = &nonCASRepo{f.runRepo}

	moved := f.s.supersedeParkedStagesOnMerge(context.Background(), f.runID, nil, supersedeReasonMergeObserved)

	if len(moved) != 0 {
		t.Errorf("moved = %+v, want none without the CAS capability", moved)
	}
	if got := f.stageState(t, f.stages[run.StageTypeAcceptance].ID); got != run.StageStateAwaitingHostDispatch {
		t.Errorf("acceptance state = %q, want awaiting_host_dispatch (nothing swept)", got)
	}
	if rows := f.supersedeRows(t); len(rows) != 0 {
		t.Errorf("stage_superseded_by_merge rows = %d, want 0", len(rows))
	}
}

// TestReconcileMerge_ExactlyOneAuditRowPerSweptStage pins the repair scan's
// SAME-INVOCATION EXCLUSION across two sequential POSTs. The repair scan
// compares against a PRE-sweep snapshot of the audit chain, so without the
// movedThisInvocation exclusion the very stages the first POST moved would look
// like un-recorded supersessions and draw a SECOND row. EXACTLY one, not
// at-least-one.
func TestReconcileMerge_ExactlyOneAuditRowPerSweptStage(t *testing.T) {
	f := newSupersedeFixture(t, parkedShape())
	f.observeMerge(t)

	if w := f.postReconcile(t); w.Code != http.StatusOK {
		t.Fatalf("first POST status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if rows := f.supersedeRows(t); len(rows) != 2 {
		t.Fatalf("after first POST rows = %d, want 2", len(rows))
	}

	// The second POST is IDEMPOTENT: nothing to move (superseded is not a
	// pair-table state), nothing to repair (both rows are present).
	w := f.postReconcile(t)
	if w.Code != http.StatusOK {
		t.Fatalf("second POST status = %d, want 200 (idempotent):\n%s", w.Code, w.Body.String())
	}
	var resp reconcileMergeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Superseded) != 0 {
		t.Errorf("second POST superseded = %+v, want none", resp.Superseded)
	}
	if len(resp.Repaired) != 0 {
		t.Errorf("second POST repaired = %+v, want none", resp.Repaired)
	}

	rows := f.supersedeRows(t)
	if len(rows) != 2 {
		t.Fatalf("after second POST rows = %d, want EXACTLY 2 (one per swept stage)", len(rows))
	}
	perStage := map[uuid.UUID]int{}
	for _, e := range rows {
		if e.StageID != nil {
			perStage[*e.StageID]++
		}
	}
	for id, n := range perStage {
		if n != 1 {
			t.Errorf("stage %s has %d supersede rows, want exactly 1", id, n)
		}
	}
}

// TestReconcileMerge_RepairsMissingAuditRow pins the repair branch: a stage that
// is already `superseded` but carries NO audit row — the residue of a sweep
// whose CAS committed and whose append then failed, the window the
// transition-first ordering deliberately allows — gets its row back, and the
// repair TRANSITIONS NOTHING.
func TestReconcileMerge_RepairsMissingAuditRow(t *testing.T) {
	f := newSupersedeFixture(t, parkedShape())
	f.observeMerge(t)
	ctx := context.Background()

	// Seed the residue directly through the repository — the stage is
	// superseded, the chain has no row for it.
	acc := f.stages[run.StageTypeAcceptance]
	cas, ok := f.runRepo.(run.StageCASTransitioner)
	if !ok {
		t.Fatal("postgres run repository must implement run.StageCASTransitioner")
	}
	if _, err := cas.TransitionStageFrom(ctx, acc.ID,
		run.StageStateAwaitingHostDispatch, run.StageStateSuperseded, nil); err != nil {
		t.Fatalf("seed superseded acceptance: %v", err)
	}
	if rows := f.supersedeRows(t); len(rows) != 0 {
		t.Fatalf("fixture already has %d supersede rows; the repair case needs none", len(rows))
	}

	w := f.postReconcile(t)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp reconcileMergeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Repaired) != 1 || resp.Repaired[0].StageID != acc.ID {
		t.Fatalf("repaired = %+v, want exactly the acceptance stage", resp.Repaired)
	}
	if resp.Repaired[0].Reason != supersedeReasonRepair {
		t.Errorf("repaired reason = %q, want %q", resp.Repaired[0].Reason, supersedeReasonRepair)
	}
	// The repair transitions nothing: the acceptance stage was ALREADY
	// superseded, and the review stage moved because it was admissible — not
	// because the repair touched it.
	if got := f.stageState(t, acc.ID); got != run.StageStateSuperseded {
		t.Errorf("acceptance state = %q, want superseded (unchanged by the repair)", got)
	}
	byStage := map[uuid.UUID]int{}
	for _, e := range f.supersedeRows(t) {
		if e.StageID != nil {
			byStage[*e.StageID]++
		}
	}
	if byStage[acc.ID] != 1 {
		t.Errorf("acceptance supersede rows = %d, want exactly 1 (the repair)", byStage[acc.ID])
	}
}

// TestReconcileMerge_RefusesUnknownRun and the id-shape refusal: the two guards
// that run before any repository work.
func TestReconcileMerge_RefusalsBeforeAnyWrite(t *testing.T) {
	f := newSupersedeFixture(t, parkedShape())

	t.Run("bad run id", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v0/runs/nope/reconcile-merge", nil)
		req.SetPathValue("run_id", "nope")
		w := httptest.NewRecorder()
		f.s.handleReconcileMerge(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "validation_failed") {
			t.Errorf("body missing validation_failed: %s", w.Body.String())
		}
	})

	t.Run("unknown run", func(t *testing.T) {
		id := uuid.New()
		req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+id.String()+"/reconcile-merge", nil)
		req.SetPathValue("run_id", id.String())
		w := httptest.NewRecorder()
		f.s.handleReconcileMerge(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404:\n%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "run_not_found") {
			t.Errorf("body missing run_not_found: %s", w.Body.String())
		}
	})

	t.Run("unconfigured", func(t *testing.T) {
		bare := New(Config{Addr: "127.0.0.1:0"})
		id := uuid.New()
		req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+id.String()+"/reconcile-merge", nil)
		req.SetPathValue("run_id", id.String())
		w := httptest.NewRecorder()
		bare.handleReconcileMerge(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "reconcile_merge_unconfigured") {
			t.Errorf("body missing reconcile_merge_unconfigured: %s", w.Body.String())
		}
	})
}

// TestReconcileMergeRouteRegistered guards the route table: POST
// /v0/runs/{run_id}/reconcile-merge must reach handleReconcileMerge through the
// mux. An UNregistered route answers with the mux's default 404 and an empty
// body; the handler's own refusal ladder answers with a typed error code, so a
// typed body here proves the route is wired in handlers.go.
func TestReconcileMergeRouteRegistered(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	mux := http.NewServeMux()
	s.registerRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+uuid.New().String()+"/reconcile-merge", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code == http.StatusNotFound && !strings.Contains(w.Body.String(), "reconcile_merge") {
		t.Fatalf("POST /v0/runs/{run_id}/reconcile-merge is not registered (mux default 404): %s", w.Body.String())
	}
}

// --- degrade / fail-closed branch fixtures -------------------------------
//
// Each wraps the REAL pg-backed repository and fails exactly ONE call, so the
// branch under test is the only thing that differs from a passing run.

type msListStagesErrRepo struct {
	run.Repository
	cas run.StageCASTransitioner
	err error
}

func (r *msListStagesErrRepo) ListStagesForRun(context.Context, uuid.UUID) ([]*run.Stage, error) {
	return nil, r.err
}

func (r *msListStagesErrRepo) TransitionStageFrom(ctx context.Context, id uuid.UUID, from, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	return r.cas.TransitionStageFrom(ctx, id, from, to, c)
}

type getRunErrRepoMS struct {
	run.Repository
	err error
}

func (r *getRunErrRepoMS) GetRun(context.Context, uuid.UUID) (*run.Run, error) { return nil, r.err }

type msAppendErrAudit struct {
	audit.Repository
	err error
}

func (a *msAppendErrAudit) AppendChained(context.Context, audit.ChainAppendParams) (*audit.Entry, error) {
	return nil, a.err
}

type msListCategoryErrAudit struct {
	audit.Repository
	failCategory string
	err          error
}

func (a *msListCategoryErrAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if category == a.failCategory {
		return nil, a.err
	}
	return a.Repository.ListForRunByCategory(ctx, runID, category)
}

// TestSupersedeParkedStages_DegradeBranches walks the sweep's best-effort
// degrades. Each must sweep NOTHING and write NOTHING rather than proceeding on
// partial information — the sweep can complete a run, so an unknown stage set is
// not a licence to guess.
func TestSupersedeParkedStages_DegradeBranches(t *testing.T) {
	t.Run("no run repository", func(t *testing.T) {
		f := newSupersedeFixture(t, parkedShape())
		f.s.cfg.RunRepo = nil
		if moved := f.s.supersedeParkedStagesOnMerge(context.Background(), f.runID, nil, supersedeReasonMergeObserved); moved != nil {
			t.Errorf("moved = %+v, want nil without a run repository", moved)
		}
	})

	t.Run("list stages fails", func(t *testing.T) {
		f := newSupersedeFixture(t, parkedShape())
		cas, _ := f.runRepo.(run.StageCASTransitioner)
		f.s.cfg.RunRepo = &msListStagesErrRepo{Repository: f.runRepo, cas: cas, err: errors.New("boom")}

		if moved := f.s.supersedeParkedStagesOnMerge(context.Background(), f.runID, nil, supersedeReasonMergeObserved); moved != nil {
			t.Errorf("moved = %+v, want nil when the stage list is unreadable", moved)
		}
		if got := f.stageState(t, f.stages[run.StageTypeAcceptance].ID); got != run.StageStateAwaitingHostDispatch {
			t.Errorf("acceptance state = %q, want untouched", got)
		}
		if rows := f.supersedeRows(t); len(rows) != 0 {
			t.Errorf("rows = %d, want 0", len(rows))
		}
	})

	t.Run("no audit repository: the transition still commits, the row is simply missing", func(t *testing.T) {
		// The sweep must NOT refuse to terminalize a stage just because the
		// chain is unwired — the transition is the correctness-bearing half and
		// the missing row is exactly what the reconcile repair scan restores.
		f := newSupersedeFixture(t, parkedShape())
		realAudit := f.s.cfg.AuditRepo
		f.s.cfg.AuditRepo = nil

		moved := f.s.supersedeParkedStagesOnMerge(context.Background(), f.runID, nil, supersedeReasonMergeObserved)
		if len(moved) != 2 {
			t.Fatalf("moved = %d, want 2", len(moved))
		}
		f.s.cfg.AuditRepo = realAudit
		if rows := f.supersedeRows(t); len(rows) != 0 {
			t.Errorf("rows = %d, want 0 with no audit repository wired", len(rows))
		}
	})

	t.Run("audit append fails: the transition is NOT unwound", func(t *testing.T) {
		f := newSupersedeFixture(t, parkedShape())
		realAudit := f.s.cfg.AuditRepo
		f.s.cfg.AuditRepo = &msAppendErrAudit{Repository: realAudit, err: errors.New("chain down")}

		moved := f.s.supersedeParkedStagesOnMerge(context.Background(), f.runID, nil, supersedeReasonMergeObserved)
		if len(moved) != 2 {
			t.Fatalf("moved = %d, want 2: an audit failure must never unwind a committed transition", len(moved))
		}
		f.s.cfg.AuditRepo = realAudit
		if got := f.stageState(t, f.stages[run.StageTypeAcceptance].ID); got != run.StageStateSuperseded {
			t.Errorf("acceptance state = %q, want superseded", got)
		}
		if rows := f.supersedeRows(t); len(rows) != 0 {
			t.Errorf("rows = %d, want 0 — the append failed, leaving a repairable missing row", len(rows))
		}
	})

	t.Run("a non-CAS transition error writes no row", func(t *testing.T) {
		// A stage the pair table admits whose CAS fails for a reason OTHER than
		// a state change (here: the run row is gone, so the repository errors).
		// Same contract as the CAS refusal — no row.
		f := newSupersedeFixture(t, parkedShape())
		cas, _ := f.runRepo.(run.StageCASTransitioner)
		bogus := &run.Stage{
			ID: uuid.New(), RunID: f.runID, Sequence: 9,
			Type: run.StageTypeAcceptance, State: run.StageStateAwaitingHostDispatch,
		}
		f.s.cfg.RunRepo = &staleStageListRepo{Repository: f.runRepo, cas: cas, stales: []*run.Stage{bogus}}

		if moved := f.s.supersedeParkedStagesOnMerge(context.Background(), f.runID, nil, supersedeReasonMergeObserved); len(moved) != 0 {
			t.Errorf("moved = %+v, want none", moved)
		}
		f.s.cfg.RunRepo = f.runRepo
		if rows := f.supersedeRows(t); len(rows) != 0 {
			t.Errorf("rows = %d, want 0 on a failed transition", len(rows))
		}
	})
}

// TestReconcileMerge_ReadFailuresFailClosed pins the endpoint's 500 branches.
// Every one of them is a READ the decision depends on: a verb that can complete
// a run must refuse on unknown evidence rather than proceed, so each returns 500
// and writes nothing.
func TestReconcileMerge_ReadFailuresFailClosed(t *testing.T) {
	boom := errors.New("boom")

	t.Run("get run fails", func(t *testing.T) {
		f := newSupersedeFixture(t, parkedShape())
		f.s.cfg.RunRepo = &getRunErrRepoMS{Repository: f.runRepo, err: boom}
		w := f.postReconcile(t)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
		}
	})

	t.Run("merge-evidence read fails", func(t *testing.T) {
		f := newSupersedeFixture(t, parkedShape())
		f.s.cfg.AuditRepo = &msListCategoryErrAudit{Repository: f.audit, failCategory: CategoryPRMerged, err: boom}
		w := f.postReconcile(t)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
		}
		f.s.cfg.AuditRepo = f.audit
		if got := f.stageState(t, f.stages[run.StageTypeAcceptance].ID); got != run.StageStateAwaitingHostDispatch {
			t.Errorf("acceptance state = %q, want untouched: an unreadable chain must not license a sweep", got)
		}
	})

	t.Run("stage list fails", func(t *testing.T) {
		f := newSupersedeFixture(t, parkedShape())
		f.observeMerge(t)
		cas, _ := f.runRepo.(run.StageCASTransitioner)
		f.s.cfg.RunRepo = &msListStagesErrRepo{Repository: f.runRepo, cas: cas, err: boom}
		w := f.postReconcile(t)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
		}
	})

	t.Run("prior supersede-row read fails", func(t *testing.T) {
		// The repair scan's whole job is deciding whether a row is MISSING;
		// treating an unreadable chain as "no rows" would duplicate every row.
		f := newSupersedeFixture(t, parkedShape())
		f.observeMerge(t)
		f.s.cfg.AuditRepo = &msListCategoryErrAudit{
			Repository: f.audit, failCategory: CategoryStageSupersededByMerge, err: boom,
		}
		w := f.postReconcile(t)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
		}
		f.s.cfg.AuditRepo = f.audit
		if got := f.stageState(t, f.stages[run.StageTypeAcceptance].ID); got != run.StageStateAwaitingHostDispatch {
			t.Errorf("acceptance state = %q, want untouched", got)
		}
		if rows := f.supersedeRows(t); len(rows) != 0 {
			t.Errorf("rows = %d, want 0", len(rows))
		}
	})
}

// seedSupersededAcceptanceNoRow drives the acceptance stage to `superseded`
// through the real CAS and asserts the chain carries no supersede row yet — the
// residue of a sweep whose transition committed and whose append then failed,
// the exact missing-row state the repair scan restores. The review stage is
// left `succeeded` (the fixture default) so the sweep has nothing to move and
// the repair path is isolated.
func (f *supersedeFixture) seedSupersededAcceptanceNoRow(t *testing.T) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	acc := f.stages[run.StageTypeAcceptance]
	cas, ok := f.runRepo.(run.StageCASTransitioner)
	if !ok {
		t.Fatal("postgres run repository must implement run.StageCASTransitioner")
	}
	if _, err := cas.TransitionStageFrom(ctx, acc.ID,
		run.StageStateAwaitingHostDispatch, run.StageStateSuperseded, nil); err != nil {
		t.Fatalf("seed superseded acceptance: %v", err)
	}
	if rows := f.supersedeRows(t); len(rows) != 0 {
		t.Fatalf("fixture already has %d supersede rows; the repair case needs none", len(rows))
	}
	return acc.ID
}

// TestReconcileMerge_RepairResponseExcludesUndurableAppend pins CONCERN 1
// (#3083 fix-up): the repaired list must carry ONLY stages whose audit row was
// DURABLY persisted. appendStageSupersededAudit is best-effort and swallows the
// append failure, so reporting a stage as repaired before checking that return
// would let POST /reconcile-merge claim a row was restored when none persisted —
// the response-contract violation this fix closes.
//
// The append is forced to fail with msAppendErrAudit AFTER the merge evidence
// and the superseded seed are in place, so the ONLY thing that fails is the
// repair's own append. The stage is superseded but its row cannot be written.
//
// COUNTERFACTUAL (run by deletion). Removing the `if aerr != nil { continue }`
// durability guard in repairMissingSupersedeRows.appendStageSupersededAudit
// check makes the stage be appended to `repaired` unconditionally, so
// resp.Repaired gains the acceptance stage despite the failed append and the
// len(resp.Repaired) == 0 assertion goes RED. Observed with the guard removed:
//
//	repaired = 1 entries, want 0 (a swallowed append failure must not be
//	reported as a durable repair)
func TestReconcileMerge_RepairResponseExcludesUndurableAppend(t *testing.T) {
	f := newSupersedeFixture(t, map[run.StageType]run.StageState{
		run.StageTypeAcceptance: run.StageStateAwaitingHostDispatch,
	})
	f.observeMerge(t)
	accID := f.seedSupersededAcceptanceNoRow(t)

	// Every subsequent AppendChained fails — including the repair's own — while
	// reads (merge evidence, prior rows, the fresh re-read) still delegate to
	// the real chain.
	f.s.cfg.AuditRepo = &msAppendErrAudit{Repository: f.audit, err: errors.New("chain down")}

	w := f.postReconcile(t)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp reconcileMergeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Repaired) != 0 {
		t.Fatalf("repaired = %d entries, want 0 (a swallowed append failure must not be reported as a durable repair): %+v", len(resp.Repaired), resp.Repaired)
	}

	// And the durable truth matches the response: no row persisted.
	f.s.cfg.AuditRepo = f.audit
	byStage := map[uuid.UUID]int{}
	for _, e := range f.supersedeRows(t) {
		if e.StageID != nil {
			byStage[*e.StageID]++
		}
	}
	if byStage[accID] != 0 {
		t.Errorf("acceptance supersede rows = %d, want 0 (the append failed)", byStage[accID])
	}
}

// TestReconcileMerge_ConcurrentRepairsWriteExactlyOneRow pins CONCERN 3 (#3083
// fix-up): the missing-audit repair must be idempotent against CONCURRENT
// reconcile requests, not only sequential ones. Two requests that both captured
// the pre-sweep snapshot before either wrote would each observe the row missing
// and each append one, breaking the documented exactly-one-row guarantee.
//
// The two racing requests are modelled by invoking repairMissingSupersedeRows
// directly from two goroutines with the SAME stale pre-sweep snapshot (empty
// priorRows, empty movedThisInvocation) — the exact input both concurrent
// handlers would carry into the repair, and the deterministic reproduction the
// handler-level race is not: nothing but the internal serialization gates the
// two goroutines from both reaching the append, so without the fix BOTH append
// regardless of scheduling.
//
// The fix serializes the repair under s.supersedeRepairMu AND re-reads the
// current rows INSIDE the lock, so the second goroutine observes the first's
// committed row and skips the stage: exactly one durable row, and exactly one
// goroutine returns the repair.
//
// COUNTERFACTUAL (run by deletion). Removing the s.supersedeRepairMu
// Lock/Unlock and the fresh under-lock `current` re-read in
// repairMissingSupersedeRows (reverting to the pre-sweep priorRows snapshot
// alone) lets both goroutines append, so f.supersedeRows returns 2 and the
// `== 1` assertion goes RED. Observed with the serialization removed:
//
//	acceptance supersede rows = 2, want exactly 1 across concurrent repairs
func TestReconcileMerge_ConcurrentRepairsWriteExactlyOneRow(t *testing.T) {
	f := newSupersedeFixture(t, map[run.StageType]run.StageState{
		run.StageTypeAcceptance: run.StageStateAwaitingHostDispatch,
	})
	f.observeMerge(t)
	accID := f.seedSupersededAcceptanceNoRow(t)

	const n = 2
	var wg sync.WaitGroup
	results := make([][]supersededStage, n)
	wg.Add(n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start // release both goroutines together to maximise the overlap
			results[i] = f.s.repairMissingSupersedeRows(context.Background(), f.runID,
				map[uuid.UUID]struct{}{}, map[uuid.UUID]struct{}{})
		}(i)
	}
	close(start)
	wg.Wait()

	// Exactly one durable row for the acceptance stage, no matter how the two
	// repairs interleaved.
	byStage := map[uuid.UUID]int{}
	for _, e := range f.supersedeRows(t) {
		if e.StageID != nil {
			byStage[*e.StageID]++
		}
	}
	if byStage[accID] != 1 {
		t.Fatalf("acceptance supersede rows = %d, want exactly 1 across concurrent repairs", byStage[accID])
	}

	// And exactly one goroutine reported the repair — the loser's fresh re-read
	// saw the row and skipped it.
	repairedTotal := 0
	for _, r := range results {
		for _, rep := range r {
			if rep.StageID == accID {
				repairedTotal++
			}
		}
	}
	if repairedTotal != 1 {
		t.Errorf("acceptance reported repaired %d times across the two goroutines, want exactly 1", repairedTotal)
	}
}

// TestRepairMissingSupersedeRows_ReListFailureRepairsNothing pins the repair
// scan's own degrade: it re-reads the stage rows, and an unreadable read must
// repair NOTHING rather than guess.
func TestRepairMissingSupersedeRows_ReListFailureRepairsNothing(t *testing.T) {
	f := newSupersedeFixture(t, parkedShape())
	cas, _ := f.runRepo.(run.StageCASTransitioner)
	f.s.cfg.RunRepo = &msListStagesErrRepo{Repository: f.runRepo, cas: cas, err: errors.New("boom")}

	repaired := f.s.repairMissingSupersedeRows(context.Background(), f.runID,
		map[uuid.UUID]struct{}{}, map[uuid.UUID]struct{}{})
	if repaired != nil {
		t.Errorf("repaired = %+v, want nil when the stage list is unreadable", repaired)
	}
	f.s.cfg.RunRepo = f.runRepo
	if rows := f.supersedeRows(t); len(rows) != 0 {
		t.Errorf("rows = %d, want 0", len(rows))
	}
}

// observeMergeVia appends merge evidence under the given category, so a test
// can seed EXACTLY one of the three qualifying categories and nothing else.
func (f *supersedeFixture) observeMergeVia(t *testing.T, category string) {
	t.Helper()
	if _, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:     f.runID,
		Timestamp: time.Now().UTC(),
		Category:  category,
		Payload:   json.RawMessage(`{"merged":true}`),
	}); err != nil {
		t.Fatalf("append %s: %v", category, err)
	}
}

// TestReconcileMergeAcceptsMergeObservationRecorded is the E64.32 / #3136
// evidence-widening half: a run whose chain carries ONLY a
// merge_observation_recorded entry — no pr_merged, no post_merge_observed — is
// ACCEPTED by reconcile-merge, where before this change it drew a 409
// reconcile_merge_pr_not_merged and the run was unreconcilable forever.
//
// This is the whole point of the observe/settle split: the settling verb still
// reads only the chain, and the new category is simply a third way a TRUE merge
// can be on it.
func TestReconcileMergeAcceptsMergeObservationRecorded(t *testing.T) {
	f := newSupersedeFixture(t, parkedShape())
	f.observeMergeVia(t, CategoryMergeObservationRecorded)

	w := f.postReconcile(t)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a merge_observation_recorded row IS merge evidence):\n%s",
			w.Code, w.Body.String())
	}
	if got := f.stageState(t, f.stages[run.StageTypeReview].ID); got != run.StageStateSuperseded {
		t.Errorf("review stage = %q, want superseded", got)
	}
}

// TestReconcileMergeStillRefusesWithNoEvidence is the COUNTERFACTUAL ANCHOR for
// the widening above: a run whose chain carries NONE of the three qualifying
// categories is still refused with reconcile_merge_pr_not_merged, and no stage
// moves. If the widening were ever mistakenly written as an unconditional
// `return true, nil` — the failure mode that turns "we added a category" into
// "we removed the gate" — this test goes RED while the acceptance test above
// stays green.
func TestReconcileMergeStillRefusesWithNoEvidence(t *testing.T) {
	f := newSupersedeFixture(t, parkedShape())
	// Deliberately NO evidence appended, of any category.

	w := f.postReconcile(t)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (no merge evidence of ANY category):\n%s", w.Code, w.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Error.Code != "reconcile_merge_pr_not_merged" {
		t.Errorf("error code = %q, want reconcile_merge_pr_not_merged", body.Error.Code)
	}
	if got := f.stageState(t, f.stages[run.StageTypeReview].ID); got != run.StageStateAwaitingApproval {
		t.Errorf("review stage = %q, want untouched awaiting_approval: an unevidenced reconcile must move nothing", got)
	}
}

// msDuplicateAppendAudit wraps the REAL pg-backed chain and returns the
// audit.ErrStageSupersededByMergeDuplicate SENTINEL from every AppendChained,
// modelling migration 0081's index refusing the second row because a concurrent
// writer — another fishhawkd process — already recorded it. Reads still delegate,
// so only the append branch differs from a passing run.
type msDuplicateAppendAudit struct {
	audit.Repository
	appends int
}

func (a *msDuplicateAppendAudit) AppendChained(context.Context, audit.ChainAppendParams) (*audit.Entry, error) {
	a.appends++
	return nil, audit.ErrStageSupersededByMergeDuplicate
}

// TestRepairMissingSupersedeRows_DuplicateOmitsStageFromRepaired pins the NEW
// benign-duplicate branch (E64.29 / #3133) at the exact level the OpenAPI text
// claims it: when migration 0081's index refuses the repair's append because a
// concurrent replica already wrote the row, the stage is OMITTED from Repaired
// and NOTHING is written by this invocation. `Repaired` means "this invocation
// restored the row", and a row a peer wrote was not restored by us.
//
// COUNTERFACTUAL (run by deletion). Deleting the
// `if audit.IsStageSupersededByMergeDuplicate(aerr)` branch in
// repairMissingSupersedeRows changes NOTHING observable here (the fall-through
// also omits the stage), which is why this test's job is the CONTRACT and the
// index's own counterfactual lives in merge_supersede_pg_test.go
// (TestSupersedeAudit_PG_ExactlyOneRowRequiresTheIndex, an index DROP against
// real Postgres). What this case DOES discriminate is the opposite regression:
// treating the duplicate as a success and reporting it as repaired.
func TestRepairMissingSupersedeRows_DuplicateOmitsStageFromRepaired(t *testing.T) {
	f := newSupersedeFixture(t, map[run.StageType]run.StageState{
		run.StageTypeAcceptance: run.StageStateAwaitingHostDispatch,
	})
	f.observeMerge(t)
	accID := f.seedSupersededAcceptanceNoRow(t)

	dup := &msDuplicateAppendAudit{Repository: f.audit}
	f.s.cfg.AuditRepo = dup

	repaired := f.s.repairMissingSupersedeRows(context.Background(), f.runID,
		map[uuid.UUID]struct{}{}, map[uuid.UUID]struct{}{})
	for _, r := range repaired {
		if r.StageID == accID {
			t.Fatalf("acceptance stage reported in Repaired after a duplicate collision; Repaired means THIS invocation restored the row, and this one wrote nothing: %+v", repaired)
		}
	}
	if dup.appends != 1 {
		t.Errorf("AppendChained calls = %d, want exactly 1 (the repair tried once and took the duplicate branch)", dup.appends)
	}

	// And the durable truth: this invocation wrote no row.
	f.s.cfg.AuditRepo = f.audit
	if rows := f.supersedeRows(t); len(rows) != 0 {
		t.Errorf("supersede rows = %d, want 0 (the duplicate branch must write nothing)", len(rows))
	}
}

// TestRepairMissingSupersedeRows_NonDuplicateErrorAlsoOmitsStage re-asserts the
// PRE-EXISTING durability branch alongside the new one, so the duplicate branch
// added above cannot have swallowed it: an append that fails for ANY OTHER reason
// still leaves the stage out of Repaired, for a later reconcile to retry.
func TestRepairMissingSupersedeRows_NonDuplicateErrorAlsoOmitsStage(t *testing.T) {
	f := newSupersedeFixture(t, map[run.StageType]run.StageState{
		run.StageTypeAcceptance: run.StageStateAwaitingHostDispatch,
	})
	f.observeMerge(t)
	accID := f.seedSupersededAcceptanceNoRow(t)

	f.s.cfg.AuditRepo = &msAppendErrAudit{Repository: f.audit, err: errors.New("chain down")}
	repaired := f.s.repairMissingSupersedeRows(context.Background(), f.runID,
		map[uuid.UUID]struct{}{}, map[uuid.UUID]struct{}{})
	for _, r := range repaired {
		if r.StageID == accID {
			t.Fatalf("acceptance stage reported in Repaired after a NON-duplicate append failure: %+v", repaired)
		}
	}
	f.s.cfg.AuditRepo = f.audit
	if rows := f.supersedeRows(t); len(rows) != 0 {
		t.Errorf("supersede rows = %d, want 0", len(rows))
	}
}

// TestSupersedeParkedStages_DuplicateAppendStillReportsStageMoved pins the SWEEP
// side of the duplicate branch (#3133): the compare-and-swap has ALREADY
// committed by the time the audit append runs, so a stage whose append collides
// with the 0081 index is still a stage this sweep MOVED and must still appear in
// the returned `moved` list. Suppressing it would make the response deny a
// transition that durably happened — the mirror-image lie of reporting an
// unwritten row as repaired.
func TestSupersedeParkedStages_DuplicateAppendStillReportsStageMoved(t *testing.T) {
	f := newSupersedeFixture(t, map[run.StageType]run.StageState{
		run.StageTypeAcceptance: run.StageStateAwaitingHostDispatch,
	})
	accID := f.stages[run.StageTypeAcceptance].ID
	dup := &msDuplicateAppendAudit{Repository: f.audit}
	f.s.cfg.AuditRepo = dup

	moved := f.s.supersedeParkedStagesOnMerge(context.Background(), f.runID, nil, supersedeReasonMergeObserved)

	found := false
	for _, m := range moved {
		if m.StageID == accID {
			found = true
		}
	}
	if !found {
		t.Fatalf("acceptance stage absent from the moved list after a duplicate audit collision; the CAS already committed, so the stage IS moved: %+v", moved)
	}
	f.s.cfg.AuditRepo = f.audit
	if got := f.stageState(t, accID); got != run.StageStateSuperseded {
		t.Errorf("acceptance stage state = %q, want %q (the CAS committed regardless of the audit collision)", got, run.StageStateSuperseded)
	}
}

// TestAppendStageSupersededAudit_DuplicateLogsInfoNotMissingRowWarn makes the
// emitter's benign-duplicate branch DELETION-DISCRIMINABLE (#3133). The branch's
// only observable effect is the log record it emits, and that record's CLAIM is
// the point: without the branch a duplicate collision falls through to the WARN
// that says the row is "MISSING (repairable via reconcile-merge)" — which is
// FALSE on this path. The row is durable; it was simply written by a peer. An
// operator or an alert reading that WARN would chase a repair that is already
// done.
//
// COUNTERFACTUAL (run by deletion). Deleting the
// `if audit.IsStageSupersededByMergeDuplicate(err)` branch in
// appendStageSupersededAudit makes the duplicate fall through to the WARN, so
// the "must not claim the row is MISSING" assertion goes RED.
func TestAppendStageSupersededAudit_DuplicateLogsInfoNotMissingRowWarn(t *testing.T) {
	f := newSupersedeFixture(t, map[run.StageType]run.StageState{
		run.StageTypeAcceptance: run.StageStateAwaitingHostDispatch,
	})
	var logBuf bytes.Buffer
	f.s.cfg.Logger = slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.s.cfg.AuditRepo = &msDuplicateAppendAudit{Repository: f.audit}

	acc := f.stages[run.StageTypeAcceptance]
	err := f.s.appendStageSupersededAudit(context.Background(), f.runID, supersededStage{
		StageID:   acc.ID,
		StageType: string(acc.Type),
		FromState: string(run.StageStateAwaitingHostDispatch),
		Reason:    supersedeReasonRepair,
	})
	// The error is returned UNCHANGED, not converted to nil: "the row exists"
	// and "this invocation wrote it" are different claims, and the repair scan
	// reads this return to decide which one to report.
	if !audit.IsStageSupersededByMergeDuplicate(err) {
		t.Fatalf("err = %v, want the duplicate returned UNCHANGED (an audit.IsStageSupersededByMergeDuplicate-recognized error)", err)
	}

	logged := logBuf.String()
	if strings.Contains(logged, "MISSING") {
		t.Errorf("the duplicate collision logged the missing-row WARN, which is FALSE on this path — the row is durable, it was written by a concurrent writer or replica:\n%s", logged)
	}
	if !strings.Contains(logged, "already recorded by a concurrent writer or replica") {
		t.Errorf("the duplicate collision did not log the benign already-recorded message:\n%s", logged)
	}
	if !strings.Contains(logged, `"level":"INFO"`) {
		t.Errorf("the duplicate collision was not logged at INFO:\n%s", logged)
	}
}
