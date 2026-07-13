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
// The headline is the clean-path full-audit-trail test: EXACTLY one
// run_auto_driven act:dispatch row per driver dispatch and one act:gate row
// per acted gate, in order, none missing and none extra — the driver's ACT
// SEQUENCE. The AUTHORITATIVE check that the real server write path stamps
// delegation provenance (actor + delegated_rule) onto those rows lives in
// backend/internal/server/autodrive_http_test.go against the real AuditRepo;
// this file's fake mirrors that payload shape (driveFakeGateRule) so the loop
// tests exercise a faithful model, but it cannot substitute for that check.

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
	mu         sync.Mutex
	runID      uuid.UUID
	runState   string
	runnerKind string // GET run response runner_kind; defaults to "local"
	stages     []Stage
	audit      []AuditEntry
	seq        int64

	recordActErr bool // /acts returns 500 when true
	gateErr      bool // /auto-drive returns 500 when true
	auditErr     bool // /audit returns 500 when true (drives the amendment-poll fail-closed path)

	// auditErrCategory, when set, returns 500 only for /audit reads of that
	// category — so a run_auto_driven read can fail while the amendment-poll
	// reads (scope_amendment_requested/decided) still succeed.
	auditErrCategory string

	onGate  func(f *driveFakeBackend) AutoDriveOutcome
	onSpawn func(f *driveFakeBackend, stageType string)

	gateCalls    int
	recordedActs []RecordAutoDriveAct
}

func newDriveFake(runState string, stages []Stage) *driveFakeBackend {
	// runner_kind defaults to "local" — the drive verb's local-only guard
	// requires it, so every happy-path fixture is a local run unless a test
	// overrides runnerKind to exercise the rejection path.
	return &driveFakeBackend{runID: uuid.New(), runState: runState, runnerKind: "local", stages: stages}
}

// driveFakeGateRule mirrors the backend's action->delegated-condition mapping
// (backend/internal/server/autodrive.go dispatch sites) so the fake's
// supplementary gate rows carry the same delegation provenance the real
// endpoint attaches. The AUTHORITATIVE test of the real write path lives in
// backend/internal/server/autodrive_http_test.go
// (TestAutoDrive_ActedApprove_EndToEnd); this keeps the fake faithful.
func driveFakeGateRule(action string) string {
	switch action {
	case "approve":
		return "clean_dual_approval"
	case "route_fixup":
		return "convergent_concerns"
	case "retry":
		return "infra_flake"
	case driveActionMerge:
		return "gates_resolved_ci_green"
	}
	return ""
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
				fields := map[string]any{"act": "gate", "action": out.Action, "source": "run_auto_drive_endpoint", "note": out.Note}
				// Mirror the real endpoint: the supplementary gate row carries the
				// delegated rule for provenance (backend appendRunAutoDrivenGate),
				// so the fake stays a faithful model of the surface under test.
				if rule := driveFakeGateRule(out.Action); rule != "" {
					fields["delegated_rule"] = rule
				}
				f.appendAuto(fields)
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
			if f.auditErr || (f.auditErrCategory != "" && cat == f.auditErrCategory) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"audit boom"}}`))
				return
			}
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
			_ = json.NewEncoder(w).Encode(Run{ID: f.runID.String(), Repo: "x/y", WorkflowID: "feature_change", State: f.runState, RunnerKind: f.runnerKind, PullRequestURL: &pr})
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
	// Every gate row carries the delegated rule the endpoint attaches for
	// provenance — approve under clean_dual_approval, merge under
	// gates_resolved_ci_green (faithful-fake shape; the real write path is
	// asserted in autodrive_http_test.go).
	for _, m := range rows {
		if m["act"] != "gate" {
			continue
		}
		if m["delegated_rule"] != driveFakeGateRule(m["action"].(string)) || m["delegated_rule"] == "" {
			t.Errorf("gate row %v missing/wrong delegated_rule; want %q", m, driveFakeGateRule(m["action"].(string)))
		}
	}
}

func actOrAction(m map[string]any) string {
	if m["act"] == "dispatch" {
		return m["stage"].(string)
	}
	return m["action"].(string)
}

// --- (a2) non-local runner_kind -> rejected, NEVER records or spawns --------

func TestDriveRun_NonLocalRunnerKind_Rejected(t *testing.T) {
	// The drive verb is local-only (ADR-024): it records + host-spawns a LOCAL
	// runner for every dispatchable stage. A run whose runner_kind is NOT 'local'
	// must be rejected BEFORE anything reaches the record-act / composeRunnerArgv
	// / spawn seam — otherwise a github_actions (or unset) run expands the host
	// code-execution surface. The plan stage is 'pending' (dispatchable), so a
	// missing guard would record + spawn it.
	for _, kind := range []string{"github_actions", ""} {
		t.Run("kind="+kind, func(t *testing.T) {
			f := newDriveFake("running", []Stage{
				stg(drivePlanID, "plan", "pending", 0),
				stg(driveImplID, "implement", "blocked", 1),
			})
			f.runnerKind = kind
			f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
			rec := &spawnRecorder{}
			r, srv := newDriveResolver(t, f, rec)
			defer srv.Close()

			_, _, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
			if err == nil {
				t.Fatalf("driveRun on a runner_kind=%q run returned no error; want a local-only rejection", kind)
			}
			if !strings.Contains(err.Error(), "local-only") {
				t.Errorf("rejection error = %q, want it to name the local-only constraint", err.Error())
			}
			if got := rec.list(); len(got) != 0 {
				t.Fatalf("driver spawned a stage on a non-local run: %v", got)
			}
			f.mu.Lock()
			nActs := len(f.recordedActs)
			gateCalls := f.gateCalls
			f.mu.Unlock()
			if nActs != 0 {
				t.Errorf("driver recorded %d acts on a non-local run; want 0 (rejected before record)", nActs)
			}
			if gateCalls != 0 {
				t.Errorf("driver called the gate %d times on a non-local run; want 0", gateCalls)
			}
		})
	}
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

// --- (f2) scope-amendment poll ERROR -> fail-closed, never dispatch ---------

func TestDriveRun_ScopeAmendmentCheckError_FailsClosed(t *testing.T) {
	// The amendment audit read fails. A pending amendment is always a human
	// decision, so an unreadable amendment state must HALT the driver — it must
	// NOT downgrade to a warning and fall through to dispatch the pending stage
	// (which would run code execution past a possibly-parked amendment).
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "pending", 0), // dispatchable — the fail-open bug would spawn this
		stg(driveImplID, "implement", "blocked", 1),
	})
	f.auditErr = true
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != stoppedAmendmentCheckFailed {
		t.Fatalf("stopped_reason = %q, want amendment_check_failed (fail-closed)", out.StoppedReason)
	}
	if got := rec.list(); len(got) != 0 {
		t.Fatalf("driver spawned a stage despite an unreadable amendment state: %v", got)
	}
	if out.NextActions == nil {
		t.Error("fail-closed amendment stop carried no next_actions")
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
	// must NOT report merged off the acted:merge alone, and it must queue the
	// merge in memory: NO re-call of the gate on later polls (which would
	// duplicate the gate:merge row and re-enable auto-merge every interval).
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
	// The gate was called EXACTLY once — the queued-merge memory stops the
	// driver re-acting each poll interval while branch protection settles.
	f.mu.Lock()
	gateCalls := f.gateCalls
	f.mu.Unlock()
	if gateCalls != 1 {
		t.Errorf("gate called %d times; want exactly 1 (merge queued once, then poll-only)", gateCalls)
	}
	// EXACTLY one gate:merge row landed — the act was recorded once, not
	// duplicated on every poll.
	var mergeRows int
	for _, m := range f.autoRows() {
		if m["act"] == "gate" && m["action"] == "merge" {
			mergeRows++
		}
	}
	if mergeRows != 1 {
		t.Errorf("gate:merge rows = %d, want exactly 1 (no per-poll duplication)", mergeRows)
	}
}

// --- (k) gate endpoint fails loud -> gate_error, loop stops acting ----------

func TestDriveRun_GateError_HaltsActing(t *testing.T) {
	// The auto-drive endpoint fails loud (500, e.g. a supplementary-append
	// failure): binding approval condition 1 requires the loop to surface it
	// and STOP — no retry, no spawn, exactly one gate call.
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		stg(driveImplID, "implement", "awaiting_approval", 1),
	})
	f.gateErr = true
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{} } // unreached
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != stoppedGateError {
		t.Fatalf("stopped_reason = %q, want gate_error", out.StoppedReason)
	}
	f.mu.Lock()
	gateCalls := f.gateCalls
	f.mu.Unlock()
	if gateCalls != 1 {
		t.Errorf("gate called %d times; want exactly 1 (stop on the fail-loud error, no retry)", gateCalls)
	}
	if got := rec.list(); len(got) != 0 {
		t.Errorf("a spawn happened after gate_error: %v", got)
	}
}

// --- (l) resume of an in-flight 'dispatched' stage -> no double-spawn -------

func TestDriveRun_ResumeDispatchedInFlight_NoDoubleSpawn(t *testing.T) {
	// A prior invocation recorded + host-spawned the implement stage; it is now
	// in the 'dispatched' window (server advanced it; the prior runner is
	// starting). A fresh invocation's per-run `spawned` map cannot see that
	// spawn, and driveDispatchableStage treats 'dispatched' as dispatchable —
	// so the resume guard must treat it as in-flight and POLL, never host-spawn
	// a SECOND runner and never re-record. (Plan test (g) second half: a
	// re-invocation continues without double-spawning the in-flight stage.)
	//
	// A FRESH UpdatedAt models the live-runner case explicitly (#1890): a runner
	// flips dispatched->running within seconds, so a recently-updated 'dispatched'
	// stage is in-flight and must be polled, NEVER stopped stale.
	impl := stg(driveImplID, "implement", "dispatched", 1)
	impl.UpdatedAt = time.Now()
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		impl,
	})
	// The prior invocation's dispatch row — the cross-invocation resume signal.
	f.appendAuto(map[string]any{"act": "dispatch", "action": "dispatch_stage", "stage": "implement", "source": "fishhawk_drive_run", "note": ""})
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()
	r.driveMaxWallclock = 40 * time.Millisecond // the in-flight stage never advances in this harness

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	// The stage stayed 'dispatched' (the prior runner reports elsewhere), so the
	// driver polled to the deadline — the point is it NEVER re-spawned.
	if out.StoppedReason != stoppedTimeout {
		t.Fatalf("stopped_reason = %q, want timeout (polled the in-flight stage)", out.StoppedReason)
	}
	if got := rec.list(); len(got) != 0 {
		t.Fatalf("resumed invocation re-spawned an in-flight 'dispatched' stage: %v", got)
	}
	// No NEW dispatch row was recorded — only the prior invocation's remains.
	if n := len(f.autoRows()); n != 1 {
		t.Errorf("run_auto_driven rows = %d, want 1 (no re-record on resume of an in-flight dispatched stage)", n)
	}
}

// --- (l2) manual dispatch in flight -> no double-spawn, no driver row -------

func TestDriveRun_ManualDispatchInFlight_NoDoubleSpawn(t *testing.T) {
	// A stage was dispatched by a MANUAL fishhawk_dispatch_stage: it sits in the
	// 'dispatched' window but has NO run_auto_driven dispatch row (only the
	// driver writes those). A fresh drive invocation's per-run `spawned` map
	// cannot see it and driveDispatchableStage treats 'dispatched' as
	// dispatchable — the guard must STILL treat it as in-flight and POLL, never
	// host-spawn a SECOND runner against the same repo. The earlier guard keyed
	// on a driver dispatch row (priorRow) and would have double-spawned here.
	//
	// This is ALSO the zero-value-UpdatedAt degrade fixture (binding approval
	// condition (a)): the 'dispatched' stage carries a ZERO-value UpdatedAt
	// (field absent from the response), and the stale threshold is set TINY — yet
	// because UpdatedAt is zero the stale check must NOT fire: fail toward
	// polling, never toward a stale stop or a spawn.
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		stg(driveImplID, "implement", "dispatched", 1), // zero-value UpdatedAt
	})
	// Deliberately NO run_auto_driven row — a manual dispatch, not a driver one.
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()
	r.driveDispatchedStaleAfter = time.Millisecond // tiny threshold: zero UpdatedAt must still degrade to POLL
	r.driveMaxWallclock = 40 * time.Millisecond    // the in-flight stage never advances

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != stoppedTimeout {
		t.Fatalf("stopped_reason = %q, want timeout (polled the in-flight manual dispatch)", out.StoppedReason)
	}
	if got := rec.list(); len(got) != 0 {
		t.Fatalf("resumed invocation re-spawned a manually-dispatched in-flight stage: %v", got)
	}
	if n := len(f.autoRows()); n != 0 {
		t.Errorf("run_auto_driven rows = %d, want 0 (no re-record for a manual in-flight dispatch)", n)
	}
	f.mu.Lock()
	nActs := len(f.recordedActs)
	f.mu.Unlock()
	if nActs != 0 {
		t.Errorf("recorded %d acts for a manually-dispatched stage; want 0", nActs)
	}
}

// --- (l3) prior-dispatch-row read error -> fail-closed, never dispatch ------

func TestDriveRun_PriorDispatchRowCheckError_FailsClosed(t *testing.T) {
	// The run_auto_driven audit read that derives the crash-resume retry note
	// errors. The loop must NOT silently downgrade to "no prior row" and record
	// + spawn — an unreadable audit state on a resume could mean a prior
	// invocation already spawned this stage, so a spawn now would start a SECOND
	// concurrent runner. Fail closed: stop, never record, never spawn. The
	// amendment poll (a distinct category) still succeeds, so the stop is
	// dispatch_check_failed, not amendment_check_failed.
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "pending", 0),
		stg(driveImplID, "implement", "blocked", 1),
	})
	f.auditErrCategory = CategoryRunAutoDriven
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != stoppedDispatchCheckFailed {
		t.Fatalf("stopped_reason = %q, want dispatch_check_failed (fail-closed)", out.StoppedReason)
	}
	if got := rec.list(); len(got) != 0 {
		t.Fatalf("driver spawned despite an unreadable prior-dispatch-row state: %v", got)
	}
	f.mu.Lock()
	nActs := len(f.recordedActs)
	f.mu.Unlock()
	if nActs != 0 {
		t.Errorf("driver recorded %d acts despite the fail-closed read error; want 0", nActs)
	}
	if out.NextActions == nil {
		t.Error("fail-closed dispatch-check stop carried no next_actions")
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

// --- (t1) fresh run, ALL stages pending -> ONLY plan dispatches --------------

func TestDriveRun_FreshRunAllPending_DispatchesOnlyPlan(t *testing.T) {
	// The real fresh-run shape from live run fdcc17cd (#1890): start_run creates
	// EVERY stage as a 'pending' row. The prior lowest-sequence-dispatchable rule
	// dispatched implement + acceptance the instant plan was spawned — both died
	// category-C on the lineage lock the plan runner held. The earliest-non-
	// terminal + gate-precondition rule must dispatch EXACTLY plan and nothing
	// else while plan is still running.
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "pending", 0),
		stg(driveImplID, "implement", "pending", 1),
		stg(driveAccID, "acceptance", "pending", 2),
	})
	// The plan spawn leaves plan NON-terminal (running) so it stays the earliest
	// non-terminal stage — implement/acceptance must never become dispatchable.
	f.onSpawn = func(f *driveFakeBackend, typ string) {
		if typ == "plan" {
			f.setState("plan", "running")
		}
	}
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()
	r.driveMaxWallclock = 40 * time.Millisecond // plan never settles; poll to the deadline

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != stoppedTimeout {
		t.Fatalf("stopped_reason = %q, want timeout (plan runs, then poll to deadline)", out.StoppedReason)
	}
	// EXACTLY one spawn: plan. implement and acceptance never dispatch.
	if got := rec.list(); len(got) != 1 || got[0] != "plan" {
		t.Fatalf("spawn sequence = %v, want exactly [plan] (no premature implement/acceptance dispatch)", got)
	}
	// EXACTLY one dispatch record row (plan).
	rows := f.autoRows()
	var dispatchRows int
	for _, m := range rows {
		if m["act"] == "dispatch" {
			dispatchRows++
			if m["stage"] != "plan" {
				t.Errorf("dispatch row for %v; want only plan", m["stage"])
			}
		}
	}
	if dispatchRows != 1 {
		t.Errorf("dispatch rows = %d, want exactly 1 (plan only)", dispatchRows)
	}
}

// --- (t2) acceptance held while a review stage is non-terminal --------------

func TestDriveRun_AcceptanceHeldOnReview(t *testing.T) {
	// acceptance requires the implement stage succeeded AND every type=review
	// stage terminal (driveGatePreconditionsMet). A review stage placed at a
	// HIGHER sequence than acceptance makes acceptance the earliest non-terminal
	// stage, so ONLY the precondition can hold the dispatch — a direct test of
	// the belt-and-suspenders gate check.
	t.Run("review_non_terminal_holds", func(t *testing.T) {
		f := newDriveFake("running", []Stage{
			stg(drivePlanID, "plan", "succeeded", 0),
			stg(driveImplID, "implement", "succeeded", 1),
			stg(driveAccID, "acceptance", "pending", 2),
			stg(uuid.NewSHA1(uuid.Nil, []byte("review")).String(), "review", "running", 3),
		})
		f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
		rec := &spawnRecorder{}
		r, srv := newDriveResolver(t, f, rec)
		defer srv.Close()
		r.driveMaxWallclock = 40 * time.Millisecond // review never settles; poll to deadline

		_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
		if err != nil {
			t.Fatalf("driveRun: %v", err)
		}
		if out.StoppedReason != stoppedTimeout {
			t.Fatalf("stopped_reason = %q, want timeout (acceptance held on the pending review)", out.StoppedReason)
		}
		if got := rec.list(); len(got) != 0 {
			t.Fatalf("acceptance (or anything) spawned while a review stage was non-terminal: %v", got)
		}
	})

	t.Run("review_terminal_dispatches", func(t *testing.T) {
		// Same shape but the review stage is terminal: acceptance's preconditions
		// now hold, so it IS the earliest host-dispatchable stage and dispatches.
		f := newDriveFake("running", []Stage{
			stg(drivePlanID, "plan", "succeeded", 0),
			stg(driveImplID, "implement", "succeeded", 1),
			stg(driveAccID, "acceptance", "pending", 2),
			stg(uuid.NewSHA1(uuid.Nil, []byte("review")).String(), "review", "succeeded", 3),
		})
		f.onSpawn = func(f *driveFakeBackend, typ string) {
			if typ == "acceptance" {
				f.setState("acceptance", "succeeded")
				f.runState = "succeeded"
			}
		}
		f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
		rec := &spawnRecorder{}
		r, srv := newDriveResolver(t, f, rec)
		defer srv.Close()

		_, _, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
		if err != nil {
			t.Fatalf("driveRun: %v", err)
		}
		if got := rec.list(); len(got) != 1 || got[0] != "acceptance" {
			t.Fatalf("spawn sequence = %v, want [acceptance] once the review settled", got)
		}
	})
}

// --- (t3) resume of a runner-less STALE 'dispatched' stage -> distinct stop --

func TestDriveRun_ResumeDispatchedStale_StopsDistinct(t *testing.T) {
	// A stage sits in 'dispatched' with an hour-old UpdatedAt and no runner ever
	// advanced it (a crashed/killed runner). Past the (lowered) liveness
	// threshold the driver must STOP dispatched_stale — never poll silently to
	// timeout, and never auto-spawn — handing the manual re-dispatch to the
	// operator via next_actions.
	impl := stg(driveImplID, "implement", "dispatched", 1)
	impl.UpdatedAt = time.Now().Add(-time.Hour)
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		impl,
	})
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()
	r.driveDispatchedStaleAfter = time.Millisecond // lower the threshold so the hour-old stage trips it

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != stoppedDispatchedStale {
		t.Fatalf("stopped_reason = %q, want dispatched_stale", out.StoppedReason)
	}
	if got := rec.list(); len(got) != 0 {
		t.Fatalf("driver spawned a stale 'dispatched' stage: %v (must never auto-spawn)", got)
	}
	f.mu.Lock()
	nActs := len(f.recordedActs)
	f.mu.Unlock()
	if nActs != 0 {
		t.Errorf("driver recorded %d acts on a stale-dispatched stop; want 0", nActs)
	}
	if out.NextActions == nil || len(out.NextActions.Actions) == 0 {
		t.Fatal("dispatched_stale stop carried no next_actions")
	}
	if out.NextActions.Actions[0].Action != "fishhawk_dispatch_stage" {
		t.Errorf("next_actions[0] = %q, want fishhawk_dispatch_stage", out.NextActions.Actions[0].Action)
	}
}

// --- (t5) cross-invocation fixup re-open -> fixup_redispatch attribution -----

func TestDriveRun_CrossInvocationFixupRedispatch(t *testing.T) {
	// A fresh invocation observes implement 'pending' (dispatchedCount==0), the
	// audit carries a prior implement dispatch row AND a NEWER
	// stage_fixup_triggered row — a fix-up re-open from a prior invocation. The
	// re-record must be attributed fixup_redispatch, not a generic implement
	// retry.
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		stg(driveImplID, "implement", "pending", 1),
	})
	// Prior implement dispatch (seq 1), then a NEWER fixup trigger (seq 2).
	f.appendAuto(map[string]any{"act": "dispatch", "action": "dispatch_stage", "stage": "implement", "source": "fishhawk_drive_run", "note": ""})
	f.seq++
	f.audit = append(f.audit, AuditEntry{ID: uuid.New().String(), Sequence: f.seq, RunID: f.runID.String(), Category: categoryStageFixupTriggered, Payload: map[string]any{}})
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
	// The re-record row is attributed fixup_redispatch, NOT implement.
	f.mu.Lock()
	defer f.mu.Unlock()
	var sawFixupRedispatch bool
	for _, a := range f.recordedActs {
		if a.Stage == "fixup_redispatch" {
			sawFixupRedispatch = true
		}
		if a.Stage == "implement" {
			t.Errorf("recorded an 'implement' act; want 'fixup_redispatch' for a cross-invocation fix-up re-open")
		}
	}
	if !sawFixupRedispatch {
		t.Errorf("no fixup_redispatch record row; recorded acts: %+v", f.recordedActs)
	}
}

// --- (t6) context cancelled -> distinct context_cancelled stop --------------

func TestDriveRun_ContextCancelled(t *testing.T) {
	f := newDriveFake("running", []Stage{stg(drivePlanID, "plan", "blocked", 0)})
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	_, out, err := r.driveRun(ctx, nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != stoppedContextCancelled {
		t.Fatalf("stopped_reason = %q, want context_cancelled (distinct from timeout)", out.StoppedReason)
	}
}

// --- (t7) queued-merge memory persists across a resume ----------------------

func TestDriveRun_MergeQueuedPersistsAcrossResume(t *testing.T) {
	// A prior invocation queued the merge (its act:gate merge row is in the
	// audit) but the run has not yet settled. On resume the driver must SEED
	// mergeQueued from that row and poll for the webhook-settle — it must NOT
	// re-call the gate (which would duplicate the gate:merge row and re-enable
	// auto-merge every interval).
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		stg(driveImplID, "implement", "succeeded", 1),
		stg(driveAccID, "acceptance", "succeeded", 2),
	})
	// The prior invocation's queued-merge row.
	f.appendAuto(map[string]any{"act": "gate", "action": "merge", "source": "run_auto_drive_endpoint", "note": "enabled auto-merge"})
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome {
		// Would re-queue the merge if reached — the test asserts it is NOT reached.
		return AutoDriveOutcome{Acted: true, Action: "merge", Note: "should not be called"}
	}
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()
	r.driveMaxWallclock = 40 * time.Millisecond // run never settles; poll to deadline

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != stoppedTimeout {
		t.Fatalf("stopped_reason = %q, want timeout (poll-only after the seeded queued merge)", out.StoppedReason)
	}
	f.mu.Lock()
	gateCalls := f.gateCalls
	f.mu.Unlock()
	if gateCalls != 0 {
		t.Errorf("gate called %d times; want 0 (queued-merge memory seeded from the prior row)", gateCalls)
	}
	// EXACTLY the one seeded gate:merge row — no duplicate landed on resume.
	var mergeRows int
	for _, m := range f.autoRows() {
		if m["act"] == "gate" && m["action"] == "merge" {
			mergeRows++
		}
	}
	if mergeRows != 1 {
		t.Errorf("gate:merge rows = %d, want exactly 1 (no duplicate on resume)", mergeRows)
	}
}

// --- (t8) queued-merge seed read ERROR -> fail-OPEN, loop continues ----------

func TestDriveRun_MergeRowReadError_FailsOpen(t *testing.T) {
	// The loop-start queued-merge seed reads run_auto_driven and errors. Unlike
	// the dispatch-path read (fail-CLOSED, halts), this check opens no code-
	// execution surface — the worst case of a false negative is a benign
	// duplicate gate:merge row — so it must FAIL-OPEN: warn and continue with
	// today's behavior (mergeQueued=false), never halt.
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		stg(driveImplID, "implement", "succeeded", 1),
		stg(driveAccID, "acceptance", "succeeded", 2),
	})
	f.auditErrCategory = CategoryRunAutoDriven // the seed's read (and any prior-dispatch read) errors
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome {
		// The loop must REACH the gate (proving it did not halt on the read error);
		// acting merge here then engages the in-memory queued-merge guard.
		return AutoDriveOutcome{Acted: true, Action: "merge", Note: "enabled auto-merge"}
	}
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()
	r.driveMaxWallclock = 40 * time.Millisecond // run never settles; poll to deadline

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	// Not a fail-closed halt — the loop degraded to today's behavior and ran on.
	if out.StoppedReason != stoppedTimeout {
		t.Fatalf("stopped_reason = %q, want timeout (fail-open, loop continues)", out.StoppedReason)
	}
	var warned bool
	for _, w := range out.Warnings {
		if strings.Contains(w, "prior gate:merge poll failed") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no fail-open warning about the merge-row read; warnings: %v", out.Warnings)
	}
	// The loop reached the gate (proving no halt) and queued the merge exactly
	// once via the in-memory guard.
	f.mu.Lock()
	gateCalls := f.gateCalls
	f.mu.Unlock()
	if gateCalls != 1 {
		t.Errorf("gate called %d times; want exactly 1 (reached the gate, then in-memory queued-merge guard)", gateCalls)
	}
}
