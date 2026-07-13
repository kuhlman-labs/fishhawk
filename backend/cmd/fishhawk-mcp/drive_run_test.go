package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// drive_run_test.go pins the fishhawk_drive_run loop (#1700) per-stop-mode
// against a stateful httptest fake backend + an injectable recording spawner.
// The headline is the clean-path full-audit-trail test the binding constraint
// requires: EXACTLY one delegated-context run_auto_driven act:dispatch row per
// driver dispatch and one act:gate row per acted gate, in order, none missing
// and none extra.

// fixed per-type stage IDs so the recording spawner can map a spawned
// stage_id back to its type.
var (
	drivePlanID = uuid.NewSHA1(uuid.Nil, []byte("plan")).String()
	driveImplID = uuid.NewSHA1(uuid.Nil, []byte("implement")).String()
	driveAccID  = uuid.NewSHA1(uuid.Nil, []byte("acceptance")).String()
)

// driveFakeBackend is a stateful in-memory backend modeling exactly the
// endpoints the drive loop calls: GET run, GET stages, GET audit, POST
// auto-drive, POST auto-drive/acts.
type driveFakeBackend struct {
	mu       sync.Mutex
	runID    uuid.UUID
	runState string
	stages   []Stage
	audit    []AuditEntry
	seq      int64

	recordActErr bool // /acts returns 500 when true
	gateErr      bool // /auto-drive returns 500 when true

	onGate  func(f *driveFakeBackend) AutoDriveOutcome
	onSpawn func(f *driveFakeBackend, stageType string)

	gateCalls    int
	recordedActs []RecordAutoDriveAct
}

func newDriveFake(runState string, stages []Stage) *driveFakeBackend {
	return &driveFakeBackend{runID: uuid.New(), runState: runState, stages: stages}
}

func (f *driveFakeBackend) stateOf(typ string) string {
	for i := range f.stages {
		if f.stages[i].Type == typ {
			return f.stages[i].State
		}
	}
	return ""
}

func (f *driveFakeBackend) setState(typ, state string) {
	for i := range f.stages {
		if f.stages[i].Type == typ {
			f.stages[i].State = state
			return
		}
	}
}

func (f *driveFakeBackend) allSucceeded(types ...string) bool {
	for _, t := range types {
		if f.stateOf(t) != "succeeded" {
			return false
		}
	}
	return true
}

func (f *driveFakeBackend) typeForStageID(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.stages {
		if f.stages[i].ID == id {
			return f.stages[i].Type
		}
	}
	return ""
}

func (f *driveFakeBackend) appendAuto(fields map[string]any) {
	f.seq++
	f.audit = append(f.audit, AuditEntry{
		ID:       uuid.New().String(),
		Sequence: f.seq,
		RunID:    f.runID.String(),
		Category: CategoryRunAutoDriven,
		Payload:  fields,
	})
}

// autoRows returns every run_auto_driven row's decoded payload, in order.
func (f *driveFakeBackend) autoRows() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, e := range f.audit {
		if e.Category != CategoryRunAutoDriven {
			continue
		}
		if m, ok := e.Payload.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func (f *driveFakeBackend) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/auto-drive/acts"):
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.recordActErr {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"code":"auto_drive_record_failed","message":"boom"}}`))
				return
			}
			var body RecordAutoDriveAct
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.recordedActs = append(f.recordedActs, body)
			f.appendAuto(map[string]any{"act": "dispatch", "action": body.Action, "stage": body.Stage, "source": body.Source, "note": body.Note})
			_ = json.NewEncoder(w).Encode(RecordAutoDriveActResult{RunID: f.runID.String(), Category: CategoryRunAutoDriven, Act: "dispatch", Action: body.Action, Stage: body.Stage, Source: body.Source, Sequence: f.seq})

		case r.Method == http.MethodPost && strings.HasSuffix(path, "/auto-drive"):
			f.mu.Lock()
			defer f.mu.Unlock()
			f.gateCalls++
			if f.gateErr {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"code":"auto_drive_record_failed","message":"gate boom"}}`))
				return
			}
			out := f.onGate(f)
			if out.Acted {
				f.appendAuto(map[string]any{"act": "gate", "action": out.Action, "source": "run_auto_drive_endpoint", "note": out.Note})
			}
			_ = json.NewEncoder(w).Encode(out)

		case strings.HasSuffix(path, "/stages"):
			f.mu.Lock()
			defer f.mu.Unlock()
			cp := make([]Stage, len(f.stages))
			copy(cp, f.stages)
			_ = json.NewEncoder(w).Encode(map[string]any{"items": cp})

		case strings.HasSuffix(path, "/audit"):
			f.mu.Lock()
			defer f.mu.Unlock()
			cat := r.URL.Query().Get("category")
			var items []AuditEntry
			for _, e := range f.audit {
				if cat == "" || e.Category == cat {
					items = append(items, e)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "next_cursor": ""})

		default: // GET /v0/runs/{id}
			f.mu.Lock()
			defer f.mu.Unlock()
			pr := "https://github.com/x/y/pull/7"
			_ = json.NewEncoder(w).Encode(Run{ID: f.runID.String(), Repo: "x/y", WorkflowID: "feature_change", State: f.runState, PullRequestURL: &pr})
		}
	}
}

// spawnRecorder records the sequence of stage types the driver spawned.
type spawnRecorder struct {
	mu     sync.Mutex
	stages []string
	fail   bool
}

func (rec *spawnRecorder) add(typ string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.stages = append(rec.stages, typ)
}

func (rec *spawnRecorder) list() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := make([]string, len(rec.stages))
	copy(out, rec.stages)
	return out
}

// newDriveResolver wires a resolver against the fake backend with a recording
// spawner + sub-ms poll interval.
func newDriveResolver(t *testing.T, f *driveFakeBackend, rec *spawnRecorder) (*runResolver, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	r := &runResolver{
		api:               newAPIClient(config{backendURL: srv.URL, apiToken: "tok"}),
		getenv:            func(string) string { return "" },
		drivePollInterval: time.Millisecond,
		driveSpawn: func(binary string, argv, env []string, runID, stageID string, report detachedFailureReporter) (string, error) {
			typ := f.typeForStageID(stageID)
			rec.add(typ)
			if rec.fail {
				return "", errStub("spawn boom")
			}
			f.mu.Lock()
			if f.onSpawn != nil {
				f.onSpawn(f, typ)
			}
			f.mu.Unlock()
			return "/tmp/log", nil
		},
	}
	return r, srv
}

type errStub string

func (e errStub) Error() string { return string(e) }

func stg(id, typ, state string, seq int) Stage {
	return Stage{ID: id, Type: typ, State: state, Sequence: seq, Executor: StageExecutor{Kind: "agent"}}
}

// --- (a) clean-path full-audit-trail --------------------------------------

func TestDriveRun_CleanPath_FullAuditTrail(t *testing.T) {
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "pending", 0),
		stg(driveImplID, "implement", "blocked", 1),
		stg(driveAccID, "acceptance", "blocked", 2),
	})
	f.onSpawn = func(f *driveFakeBackend, typ string) {
		switch typ {
		case "plan":
			f.setState("plan", "awaiting_approval")
		case "implement":
			f.setState("implement", "succeeded")
			f.setState("acceptance", "pending")
		case "acceptance":
			f.setState("acceptance", "succeeded")
		}
	}
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome {
		switch {
		case f.stateOf("plan") == "awaiting_approval":
			f.setState("plan", "succeeded")
			f.setState("implement", "pending")
			return AutoDriveOutcome{Acted: true, Action: "approve", Note: "auto-approved"}
		case f.allSucceeded("plan", "implement", "acceptance"):
			f.runState = "succeeded" // webhook-settled on the next poll
			return AutoDriveOutcome{Acted: true, Action: "merge", Note: "enabled auto-merge"}
		default:
			return AutoDriveOutcome{Note: "observe-only"}
		}
	}
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != stoppedMerged {
		t.Fatalf("stopped_reason = %q, want merged (steps: %+v; warnings: %v)", out.StoppedReason, out.StepsTaken, out.Warnings)
	}
	// Exactly the spawn sequence, no decision stop.
	if got := rec.list(); strings.Join(got, ",") != "plan,implement,acceptance" {
		t.Errorf("spawn sequence = %v, want [plan implement acceptance]", got)
	}
	// EXACTLY one act:dispatch row per driver dispatch and one act:gate row per
	// acted gate, in order (the binding constraint).
	rows := f.autoRows()
	var seq []string
	for _, m := range rows {
		seq = append(seq, m["act"].(string)+":"+actOrAction(m))
	}
	want := []string{"dispatch:plan", "gate:approve", "dispatch:implement", "dispatch:acceptance", "gate:merge"}
	if strings.Join(seq, ",") != strings.Join(want, ",") {
		t.Errorf("run_auto_driven rows = %v, want %v", seq, want)
	}
}

func actOrAction(m map[string]any) string {
	if m["act"] == "dispatch" {
		return m["stage"].(string)
	}
	return m["action"].(string)
}

// --- (b) record-act failure -> unrecorded_act, NO spawn ---------------------

func TestDriveRun_RecordActFailure_NoSpawn(t *testing.T) {
	f := newDriveFake("running", []Stage{stg(drivePlanID, "plan", "pending", 0)})
	f.recordActErr = true
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != stoppedUnrecordedAct {
		t.Fatalf("stopped_reason = %q, want unrecorded_act", out.StoppedReason)
	}
	if len(rec.list()) != 0 {
		t.Errorf("spawn happened after a failed record: %v", rec.list())
	}
	if f.gateCalls != 0 {
		t.Errorf("gate called %d times after unrecorded_act; want 0 (no further acting)", f.gateCalls)
	}
}

// --- (c) resume after a recorded-but-unspawned attempt ----------------------

func TestDriveRun_ResumeRecordedNotSpawned_RetryNote(t *testing.T) {
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		stg(driveImplID, "implement", "pending", 1),
	})
	// Pre-seed a run_auto_driven dispatch row for implement (a prior crashed
	// attempt): the resume must re-record with a retry note, dispatch once.
	f.appendAuto(map[string]any{"act": "dispatch", "action": "dispatch_stage", "stage": "implement", "source": "fishhawk_drive_run", "note": ""})
	f.onSpawn = func(f *driveFakeBackend, typ string) {
		if typ == "implement" {
			f.setState("implement", "succeeded")
			f.runState = "succeeded"
		}
	}
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != stoppedMerged {
		t.Fatalf("stopped_reason = %q, want merged", out.StoppedReason)
	}
	if got := rec.list(); len(got) != 1 || got[0] != "implement" {
		t.Errorf("spawn sequence = %v, want exactly [implement] (no double-spawn)", got)
	}
	// The re-record carried the retry note.
	f.mu.Lock()
	defer f.mu.Unlock()
	var retryNoted bool
	for _, a := range f.recordedActs {
		if a.Stage == "implement" && a.Note == "retry" {
			retryNoted = true
		}
	}
	if !retryNoted {
		t.Errorf("no implement record carried note=retry; recorded acts: %+v", f.recordedActs)
	}
}

// --- (d) observe-only at the plan gate -> decision_required -----------------

func TestDriveRun_ObserveOnlyPlanGate_DecisionRequired(t *testing.T) {
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "pending", 0),
		stg(driveImplID, "implement", "blocked", 1),
	})
	f.onSpawn = func(f *driveFakeBackend, typ string) {
		if typ == "plan" {
			f.setState("plan", "awaiting_approval")
		}
	}
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "no delegated knob"} }
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if !strings.HasPrefix(out.StoppedReason, "decision_required:") {
		t.Fatalf("stopped_reason = %q, want decision_required:*", out.StoppedReason)
	}
	if out.NextActions == nil || len(out.NextActions.Actions) == 0 {
		t.Error("decision_required stop carried no next_actions")
	}
	if got := rec.list(); len(got) != 1 || got[0] != "plan" {
		t.Errorf("spawn sequence = %v, want [plan] then park", got)
	}
}

// --- (e) endpoint paged -> paged:<event>, loop halts unacted ----------------

func TestDriveRun_Paged_HaltsUnacted(t *testing.T) {
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		stg(driveImplID, "implement", "awaiting_approval", 1),
	})
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome {
		return AutoDriveOutcome{Paged: true, PageEvent: "reviewer_reject", Note: "must_page_human"}
	}
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != "paged:reviewer_reject" || out.PageEvent != "reviewer_reject" {
		t.Fatalf("out = %+v, want paged:reviewer_reject", out)
	}
	if len(rec.list()) != 0 {
		t.Errorf("a spawn happened on a paged run: %v", rec.list())
	}
}

// --- (f) scope_amendment_requested -> decision within one poll --------------

func TestDriveRun_ScopeAmendmentPending_Decision(t *testing.T) {
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		stg(driveImplID, "implement", "running", 1),
	})
	f.seq++
	f.audit = append(f.audit, AuditEntry{ID: uuid.New().String(), Sequence: f.seq, RunID: f.runID.String(), Category: "scope_amendment_requested", Payload: map[string]any{}})
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != "decision_required:scope_amendment_requested" {
		t.Fatalf("stopped_reason = %q, want decision_required:scope_amendment_requested", out.StoppedReason)
	}
	if defaultDrivePollInterval > 30*time.Second {
		t.Errorf("default drive poll interval %v exceeds the ~5-minute amendment window budget of 30s", defaultDrivePollInterval)
	}
}

// --- (g) max_minutes exhaustion -> timeout ----------------------------------

func TestDriveRun_Timeout(t *testing.T) {
	f := newDriveFake("running", []Stage{stg(drivePlanID, "plan", "blocked", 0)})
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()
	r.driveMaxWallclock = time.Nanosecond // deadline already elapsed

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != stoppedTimeout {
		t.Fatalf("stopped_reason = %q, want timeout", out.StoppedReason)
	}
	if len(rec.list()) != 0 {
		t.Errorf("a spawn happened past the deadline: %v", rec.list())
	}
}

// --- (h) stall guard --------------------------------------------------------

func TestDriveRun_StallGuard(t *testing.T) {
	// A stage in a non-dispatchable, non-in-flight, non-gate state with an
	// endpoint that always observes-only: the loop must not spin forever.
	f := newDriveFake("running", []Stage{stg(drivePlanID, "plan", "weird_wedged", 0)})
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != stoppedStalled {
		t.Fatalf("stopped_reason = %q, want stalled", out.StoppedReason)
	}
}

// --- (i) spawner failure -> stage_failed ------------------------------------

func TestDriveRun_SpawnFailure_StageFailed(t *testing.T) {
	f := newDriveFake("running", []Stage{stg(drivePlanID, "plan", "pending", 0)})
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
	rec := &spawnRecorder{fail: true}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != stoppedStageFailed {
		t.Fatalf("stopped_reason = %q, want stage_failed", out.StoppedReason)
	}
	// The record-before-dispatch row was still written (the driver recorded,
	// then the spawn failed) — an honest over-record, never an unaudited act.
	if len(f.autoRows()) != 1 {
		t.Errorf("run_auto_driven rows = %d, want 1 (the recorded-then-failed attempt)", len(f.autoRows()))
	}
}

// --- (j) acted:merge alone does NOT report merged ---------------------------

func TestDriveRun_MergeQueuedNotLanded(t *testing.T) {
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		stg(driveImplID, "implement", "succeeded", 1),
		stg(driveAccID, "acceptance", "succeeded", 2),
	})
	// Merge is queued (acted) but the run NEVER settles to succeeded — the
	// webhook hasn't fired. The driver must keep polling, then time out; it
	// must NOT report merged off the acted:merge alone.
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome {
		return AutoDriveOutcome{Acted: true, Action: "merge", Note: "enabled auto-merge; not landed"}
	}
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()
	r.driveMaxWallclock = 40 * time.Millisecond

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason == stoppedMerged {
		t.Fatalf("stopped_reason = merged off an unsettled acted:merge; want timeout")
	}
	if out.StoppedReason != stoppedTimeout {
		t.Fatalf("stopped_reason = %q, want timeout", out.StoppedReason)
	}
	// A gate:merge row DID land (the act was recorded), proving the driver
	// acted but correctly withheld the merged verdict.
	var sawMerge bool
	for _, m := range f.autoRows() {
		if m["act"] == "gate" && m["action"] == "merge" {
			sawMerge = true
		}
	}
	if !sawMerge {
		t.Error("no gate:merge row landed; the merge act was not recorded")
	}
}

// --- fixup variant: re-dispatch after a delegated route_fixup ---------------

func TestDriveRun_FixupRedispatch(t *testing.T) {
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		stg(driveImplID, "implement", "pending", 1),
	})
	fixedUp := false
	f.onSpawn = func(f *driveFakeBackend, typ string) {
		if typ != "implement" {
			return
		}
		if !fixedUp {
			f.setState("implement", "awaiting_approval") // first pass parks at gate
		} else {
			f.setState("implement", "succeeded")
			f.runState = "succeeded"
		}
	}
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome {
		if f.stateOf("implement") == "awaiting_approval" && !fixedUp {
			fixedUp = true
			f.setState("implement", "pending") // route_fixup re-opens
			return AutoDriveOutcome{Acted: true, Action: "route_fixup", Note: "auto-routed fix-up"}
		}
		return AutoDriveOutcome{Note: "observe-only"}
	}
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != stoppedMerged {
		t.Fatalf("stopped_reason = %q, want merged", out.StoppedReason)
	}
	if got := rec.list(); len(got) != 2 || got[0] != "implement" || got[1] != "implement" {
		t.Fatalf("spawn sequence = %v, want [implement implement] (dispatch + fixup re-dispatch)", got)
	}
	// The re-dispatch after route_fixup is recorded as fixup_redispatch.
	var sawImpl, sawFixup bool
	for _, m := range f.autoRows() {
		if m["act"] == "dispatch" && m["stage"] == "implement" {
			sawImpl = true
		}
		if m["act"] == "dispatch" && m["stage"] == "fixup_redispatch" {
			sawFixup = true
		}
	}
	if !sawImpl || !sawFixup {
		t.Errorf("dispatch rows: implement=%v fixup_redispatch=%v, want both", sawImpl, sawFixup)
	}
}
