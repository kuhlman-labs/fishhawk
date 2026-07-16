package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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
	// derivedStatus, when non-empty, is encoded as the run's derived_status on
	// the GET /v0/runs/{id} response so the drive loop's fetchRunDriveView decodes
	// the backend's authoritative acceptance_pending signal (#1961). Empty (the
	// zero value every legacy fixture carries) leaves derived_status absent, so
	// those fixtures keep exercising the unchanged path.
	derivedStatus string
	stages        []Stage
	audit         []AuditEntry
	seq           int64

	recordActErr bool // /acts returns 500 when true
	gateErr      bool // /auto-drive returns 500 when true
	auditErr     bool // /audit returns 500 when true (drives the amendment-poll fail-closed path)

	// #1928 acceptance-admission fixtures. admissionShortCircuit drives the
	// short_circuited response (and flips the acceptance stage to succeeded on a
	// hit); admissionStatus (0 -> 200) drives the fail-open error branch;
	// admissionCalledN counts admission POSTs.
	admissionShortCircuit bool
	admissionStatus       int
	admissionCalledN      int
	// #1953 needs_target fixtures: on a non-short-circuit admission, serve
	// needs_target + hosts + head SHA so the drive's verb-side gate probes.
	admissionNeedsTarget     bool
	admissionTargetHosts     []string
	admissionExpectedHeadSHA string

	// auditErrCategory, when set, returns 500 only for /audit reads of that
	// category — so a run_auto_driven read can fail while the amendment-poll
	// reads (scope_amendment_requested/decided) still succeed.
	auditErrCategory string

	// #1912 host-dispatch marker fixtures. hostDispatchStatus (0 -> 200) drives
	// the fail-closed error branch the loop's marker call hits; on a 200 the route
	// CAS-flips the matching stage pending|awaiting_host_dispatch -> dispatched and
	// counts the call. hostDispatchCalledN counts marker POSTs so the auto-dispatch
	// test can assert the marker fired before the spawn.
	hostDispatchStatus  int
	hostDispatchCalledN int

	onGate  func(f *driveFakeBackend) AutoDriveOutcome
	onSpawn func(f *driveFakeBackend, stageType string)
	// onAudit, when non-nil, fires under fb.mu on every /audit read with the
	// requested category BEFORE the items are built — the review-settlement
	// tests use it to land a second reviewer verdict mid-poll without wall-clock
	// sleeps (it mutates f.audit directly; the caller already holds fb.mu).
	onAudit func(f *driveFakeBackend, category string)
	// onStages, when non-nil, fires under fb.mu on every /stages read BEFORE the
	// snapshot is copied — the convergence test uses it to advance a polled
	// dispatched stage to running then succeeded across poll iterations without
	// wall-clock sleeps (it mutates f.stages/f.runState directly; the caller
	// already holds fb.mu).
	onStages func(f *driveFakeBackend)

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
	f.appendAutoAt(fields, time.Now())
}

// appendAutoAt appends a run_auto_driven row with an explicit server-set
// Timestamp — the staleness-anchor input (#1905). appendAuto stamps time.Now()
// (fresh evidence); the stale-branch fixtures pass an old timestamp so a
// dispatch row can model spawn evidence past the liveness threshold.
func (f *driveFakeBackend) appendAutoAt(fields map[string]any, ts time.Time) {
	f.seq++
	f.audit = append(f.audit, AuditEntry{
		ID:        uuid.New().String(),
		Sequence:  f.seq,
		RunID:     f.runID.String(),
		Category:  CategoryRunAutoDriven,
		Payload:   fields,
		Timestamp: ts,
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
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/host-dispatch"):
			f.mu.Lock()
			defer f.mu.Unlock()
			f.hostDispatchCalledN++
			if f.hostDispatchStatus != 0 && f.hostDispatchStatus != http.StatusOK {
				w.WriteHeader(f.hostDispatchStatus)
				_, _ = w.Write([]byte(`{"error":{"code":"dispatch_not_admissible","message":"host-dispatch boom"}}`))
				return
			}
			// Extract the stage id from .../stages/{stage_id}/host-dispatch and CAS-flip
			// pending|awaiting_host_dispatch -> dispatched (transitioned), else the
			// idempotent no-op for an already-dispatched stage.
			segs := strings.Split(strings.TrimSuffix(path, "/host-dispatch"), "/")
			stageID := segs[len(segs)-1]
			transitioned := false
			state := "dispatched"
			for i := range f.stages {
				if f.stages[i].ID != stageID {
					continue
				}
				switch f.stages[i].State {
				case "pending", "awaiting_host_dispatch":
					f.stages[i].State = "dispatched"
					transitioned = true
				}
				state = f.stages[i].State
			}
			_ = json.NewEncoder(w).Encode(HostDispatchResult{Transitioned: transitioned, StageState: state})

		case r.Method == http.MethodPost && strings.HasSuffix(path, "/acceptance-admission"):
			f.mu.Lock()
			defer f.mu.Unlock()
			f.admissionCalledN++
			sc := f.admissionShortCircuit
			status := f.admissionStatus
			if sc {
				f.setState("acceptance", "succeeded")
			}
			if status != 0 && status != http.StatusOK {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"admission boom"}}`))
				return
			}
			res := AcceptanceAdmissionResult{ShortCircuited: sc}
			if sc {
				res.Kind = "all_skip_with_basis"
				res.Basis = "all-skip-with-basis"
				res.CriteriaTotal = 2
			} else if f.admissionNeedsTarget {
				res.NeedsTarget = true
				res.TargetHosts = f.admissionTargetHosts
				res.ExpectedHeadSHA = f.admissionExpectedHeadSHA
			}
			_ = json.NewEncoder(w).Encode(res)

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
			if f.onStages != nil {
				f.onStages(f)
			}
			cp := make([]Stage, len(f.stages))
			copy(cp, f.stages)
			_ = json.NewEncoder(w).Encode(map[string]any{"items": cp})

		case strings.HasSuffix(path, "/audit"):
			f.mu.Lock()
			defer f.mu.Unlock()
			cat := r.URL.Query().Get("category")
			if f.onAudit != nil {
				f.onAudit(f, cat)
			}
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
			// Embed Run plus derived_status so runDriveView decodes both from one
			// body (#1961); a zero-value derivedStatus is omitted, so every legacy
			// fixture keeps exercising the no-derived-signal path.
			_ = json.NewEncoder(w).Encode(struct {
				Run
				DerivedStatus string `json:"derived_status,omitempty"`
			}{
				Run:           Run{ID: f.runID.String(), Repo: "x/y", WorkflowID: "feature_change", State: f.runState, RunnerKind: f.runnerKind, PullRequestURL: &pr},
				DerivedStatus: f.derivedStatus,
			})
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
	// A stage was dispatched by a MANUAL fishhawk_dispatch_stage, whose host-
	// dispatch marker CAS-flipped it to 'dispatched' stamping a FRESH updated_at
	// (#1912). It sits in the 'dispatched' window; a fresh drive invocation's
	// per-run `spawned` map cannot see it and driveDispatchableStage returns it —
	// the guard must STILL treat it as in-flight (a fresh spawn timestamp) and
	// POLL, never host-spawn a SECOND runner and never re-record. Post-#1912 the
	// staleness anchor is the stage's own max(updated_at, started_at), NOT a
	// dispatch-row timestamp; a fresh updated_at is what keeps it live.
	impl := stg(driveImplID, "implement", "dispatched", 1)
	impl.UpdatedAt = time.Now() // the marker flip stamped a fresh updated_at, so the anchor reads live
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		impl,
	})
	// A manual-dispatch ATTRIBUTION row (source fishhawk_dispatch_stage) — kept for
	// provenance; post-#1912 it no longer anchors staleness (the stage's updated_at
	// does), so it must NOT trigger a re-record or double-spawn.
	f.appendAuto(map[string]any{"act": "dispatch", "action": "dispatch_stage", "stage": "implement", "source": "fishhawk_dispatch_stage", "note": "manual host dispatch"})
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()
	r.driveDispatchedStaleAfter = 10 * time.Second // large threshold: the FRESH updated_at anchor reads live, so it polls
	r.driveMaxWallclock = 40 * time.Millisecond    // the in-flight stage never advances -> times out while polling

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
	// Only the seeded manual row remains — no driver re-record.
	if n := len(f.autoRows()); n != 1 {
		t.Errorf("run_auto_driven rows = %d, want 1 (the seeded manual row; no driver re-record)", n)
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

// --- (#1961) derived acceptance_pending unblocks a parked human review row ---

// driveAcceptancePendingStages is the production shape the fix targets: plan +
// implement succeeded, the human review row parked at awaiting_approval (LOWER
// sequence than acceptance, so it is the earliest non-terminal stage), and
// acceptance pending. Without the derived signal the driver picks the review
// stage as earliest and returns nil → the run stalls at a mislabeled review
// gate. The observed runs 99170a0e/73b492ae/27da3ecc reached exactly this state.
func driveAcceptancePendingStages() []Stage {
	return []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		stg(driveImplID, "implement", "succeeded", 1),
		stg(driveReviewID, "review", "awaiting_approval", 2),
		stg(driveAccID, "acceptance", "pending", 3),
	}
}

// TestDriveRun_DerivedAcceptancePending_DispatchesAcceptance (#1961, done-means
// (a)): a review stage parked at awaiting_approval + derived acceptance_pending +
// acceptance pending → the driver treats the review row as non-blocking, records
// the acceptance dispatch act, fires the host-dispatch marker, spawns the
// acceptance runner, and drives on — no decision_required:review_gate_parked stop.
func TestDriveRun_DerivedAcceptancePending_DispatchesAcceptance(t *testing.T) {
	f := newDriveFake("running", driveAcceptancePendingStages())
	f.derivedStatus = "acceptance_pending"
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

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if strings.HasPrefix(out.StoppedReason, "decision_required") {
		t.Fatalf("stopped_reason = %q, want NO decision_required stop under derived acceptance_pending", out.StoppedReason)
	}
	if got := rec.list(); len(got) != 1 || got[0] != "acceptance" {
		t.Fatalf("spawn sequence = %v, want [acceptance] (the mechanical dispatch the derived signal unblocks)", got)
	}
	f.mu.Lock()
	var accActs int
	for _, a := range f.recordedActs {
		if a.Stage == "acceptance" {
			accActs++
		}
	}
	hostN := f.hostDispatchCalledN
	f.mu.Unlock()
	if accActs != 1 {
		t.Errorf("recorded acceptance dispatch acts = %d, want 1 (record-before-spawn)", accActs)
	}
	if hostN < 1 {
		t.Errorf("host-dispatch marker calls = %d, want >= 1 (marker before spawn)", hostN)
	}
}

// TestDriveRun_DerivedAcceptancePending_ShortCircuit_NoSpawn (#1961, done-means
// (b)): the same unblocked fixture with an admission short-circuit takes the
// zero-cost server-side path — no spawn, no recorded act — and the loop continues
// to the terminal run. Proves the unblocked acceptance stage flows the existing
// #1928 admission short-circuit unchanged.
func TestDriveRun_DerivedAcceptancePending_ShortCircuit_NoSpawn(t *testing.T) {
	f := newDriveFake("running", driveAcceptancePendingStages())
	f.derivedStatus = "acceptance_pending"
	f.admissionShortCircuit = true
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome {
		if f.stateOf("acceptance") == "succeeded" {
			f.runState = "succeeded"
			return AutoDriveOutcome{Acted: true, Action: "merge", Note: "enabled auto-merge"}
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
		t.Fatalf("stopped_reason = %q, want merged:\n%+v", out.StoppedReason, out)
	}
	for _, typ := range rec.list() {
		if typ == "acceptance" {
			t.Errorf("a short-circuited acceptance stage must NOT spawn; spawned: %v", rec.list())
		}
	}
	f.mu.Lock()
	var accActs, admissionN int
	for _, a := range f.recordedActs {
		if a.Stage == "acceptance" {
			accActs++
		}
	}
	admissionN = f.admissionCalledN
	f.mu.Unlock()
	if accActs != 0 {
		t.Errorf("recorded acceptance acts = %d on the short-circuit path, want 0", accActs)
	}
	if admissionN < 1 {
		t.Errorf("admission calls = %d, want >= 1 (the derived signal still reached the acceptance dispatch path)", admissionN)
	}
}

// TestDriveRun_ReviewParked_NoDerivedStatus_StopsDecisionRequired (#1961,
// done-means (c) — the binding no-signal-no-change pin): the IDENTICAL stage
// fixture WITHOUT a derived_status stops decision_required:review_gate_parked
// byte-identically to today, so non-drive runs and every other derived value
// change nothing.
func TestDriveRun_ReviewParked_NoDerivedStatus_StopsDecisionRequired(t *testing.T) {
	f := newDriveFake("running", driveAcceptancePendingStages())
	// derivedStatus intentionally left empty — the legacy no-signal path.
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != "decision_required:review_gate_parked" {
		t.Fatalf("stopped_reason = %q, want decision_required:review_gate_parked (no derived signal → unchanged)", out.StoppedReason)
	}
	if got := rec.list(); len(got) != 0 {
		t.Fatalf("nothing must spawn without the derived signal; spawned: %v", got)
	}
}

// TestDriveRun_DerivedAcceptancePending_ReviewInFlight_StillHeld (#1961,
// done-means (d)): the derived signal never overrides in-flight review evidence.
// A review stage 'running' (not awaiting_approval) still blocks acceptance even
// with derived acceptance_pending — the backend would not have stamped the signal
// with a review in flight, so the driver's guard is conservative.
func TestDriveRun_DerivedAcceptancePending_ReviewInFlight_StillHeld(t *testing.T) {
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		stg(driveImplID, "implement", "succeeded", 1),
		stg(driveAccID, "acceptance", "pending", 2),
		stg(driveReviewID, "review", "running", 3),
	})
	f.derivedStatus = "acceptance_pending" // present, but a review is genuinely in flight
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
		t.Fatalf("stopped_reason = %q, want timeout (acceptance held on the in-flight review)", out.StoppedReason)
	}
	if got := rec.list(); len(got) != 0 {
		t.Fatalf("acceptance (or anything) spawned while a review stage was running: %v", got)
	}
}

// TestDriveRun_DerivedAcceptancePending_ObserveOnlyFallThrough_PollsToStalled
// (#1961) directly exercises the modified observe-only gate-branch condition
// `state != "" && (!acceptancePendingDerived || state != driveReviewGateParkedState)`
// in its fall-through (poll, NOT stop) mode — the branch every other new test
// skips because the dispatch branch consumes the acceptance stage before it is
// reached. The production shape: acceptance has already SUCCEEDED (terminal, so
// nothing is dispatchable) but a checks_green stamp has not yet superseded the
// derived acceptance_pending signal, while the human review row still sits
// awaiting_approval. driveParkedGateState names review_gate_parked, but under the
// derived signal the branch condition is FALSE, so the loop must NOT compose
// decision_required:review_gate_parked — it falls through to the signature/stall
// logic and returns a driveStallThreshold-bounded 'stalled' stop carrying
// next_actions. A regression that inverted the condition would stop
// decision_required here instead of polling.
func TestDriveRun_DerivedAcceptancePending_ObserveOnlyFallThrough_PollsToStalled(t *testing.T) {
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		stg(driveImplID, "implement", "succeeded", 1),
		stg(driveAccID, "acceptance", "succeeded", 2), // terminal → nothing dispatchable
		stg(driveReviewID, "review", "awaiting_approval", 3),
	})
	f.derivedStatus = "acceptance_pending" // still stamped; no checks_green stamp yet
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	// The observe-only branch fell through to poll (NOT decision_required), and
	// the stall guard bounded it to a 'stalled' stop.
	if out.StoppedReason != stoppedStalled {
		t.Fatalf("stopped_reason = %q, want stalled (observe-only fall-through polled, not stopped decision_required)", out.StoppedReason)
	}
	if strings.HasPrefix(out.StoppedReason, "decision_required") {
		t.Fatalf("stopped_reason = %q; the derived signal must suppress decision_required:review_gate_parked here", out.StoppedReason)
	}
	if out.NextActions == nil || len(out.NextActions.Actions) == 0 {
		t.Error("the stalled stop carried no next_actions")
	}
	if got := rec.list(); len(got) != 0 {
		t.Fatalf("nothing must spawn (acceptance already terminal); spawned: %v", got)
	}
}

// TestDriveRun_DerivedAcceptancePending_AgreementTable (#1961, done-means: the
// stop-taxonomy agreement pin, satisfying the binding condition's seam guard).
// It (1) pins the driver's derivedAcceptancePending literal EQUAL to the
// next_actions acceptance_pending state string the classifier emits — so
// producer and consumer cannot drift silently across the HTTP boundary — and
// (2) for each parked-gate fixture drives the loop to its stop AND classifies
// the equivalent inputs through the same-package nextActionsFor, asserting a
// decision_required:* stop occurs IFF the classifier names an operator-judgment
// action, and that the acceptance_pending fixture DISPATCHES rather than stops.
func TestDriveRun_DerivedAcceptancePending_AgreementTable(t *testing.T) {
	// (1) The literal-equality drift guard: the state acceptanceStageNextActions
	// emits for a non-terminal acceptance stage IS the driver's constant. Renaming
	// either side fails here.
	accNA := nextActionsFor(
		&Run{State: "running", RunnerKind: "local"},
		[]Stage{
			stg(drivePlanID, "plan", "succeeded", 0),
			stg(driveImplID, "implement", "succeeded", 1),
			stg(driveAccID, "acceptance", "pending", 2),
		},
		nil, nil, nil, nil, false, false, "", "", releaseSignals{})
	if accNA == nil || accNA.State != derivedAcceptancePending {
		t.Fatalf("next_actions acceptance state = %+v, want state == derivedAcceptancePending (%q) — producer/consumer literal drift", accNA, derivedAcceptancePending)
	}

	// isOperatorJudgment reports whether a classifier state names a move that
	// requires operator judgment (approve/reject/revise, scope amendment) rather
	// than a bare dispatch or poll.
	isOperatorJudgment := func(state string) bool {
		switch state {
		case "plan_gate_parked", "plan_awaiting_input",
			"scope_amendment_requested", "implement_concerns_open":
			return true
		}
		return false
	}

	// The table enumerates ALL FOUR parked-gate fixtures the plan's step 6 named,
	// so the [decision-stop-matches-operator-judgment] criterion is delivered as a
	// table rather than partially (2 rows) with the retained decision paths pinned
	// only as standalone stop-reason tests (#1961 review concerns). Each row pins
	// BOTH surfaces explicitly — the classifier's state (wantClassifierState) and
	// the driver's stop (wantDriverStopReason) — so a cross-surface taxonomy
	// disagreement on ANY of the four is caught, not only the two that AGREE.
	//
	// Two of the four are DELIBERATE, DOCUMENTED divergences (divergence:true): the
	// driver stops decision_required while the classifier — routing on impl.State +
	// implementReviewStatus and ignoring the review stage row / the scope-amendment
	// audit (verified in next_actions.go: implementStageNextActions) — names a
	// mechanical/poll state. A strict iff-agreement CANNOT hold there and asserting
	// one would be wrong: the driver is conservative BY DESIGN because it has
	// audit/parked-row visibility the classifier inputs lack.
	//   - review_parked_NO_derived_signal: without the derived acceptance_pending
	//     signal the driver cannot confirm the human-review evidence settled, so it
	//     treats the parked review as a genuine gate and stops
	//     decision_required:review_gate_parked; the classifier names acceptance_pending
	//     (it never gates on the parked review row). This is the SAME retained path
	//     TestDriveRun_ReviewParked_NoDerivedStatus_StopsDecisionRequired pins as a
	//     standalone stop; here it additionally gets the classifier cross-check.
	//   - pending_scope_amendment: a pending amendment is ALWAYS an operator decision
	//     (no delegation knob covers amendments), so the driver stops
	//     decision_required:scope_amendment_requested; the classifier models no
	//     amendment state and names implement_running for the in-flight implement.
	cases := []struct {
		name          string
		stages        []Stage
		derivedStatus string
		// seedAudit optionally seeds audit rows the driver reads (the scope
		// amendment fixture seeds a pending scope_amendment_requested row).
		seedAudit func(f *driveFakeBackend)
		// naStages builds the classifier-equivalent inputs for this parked gate.
		naStages       []Stage
		naReviewStatus *ReviewStatus
		// wantClassifierState pins the exact state nextActionsFor names for the
		// equivalent inputs — the producer half of the cross-surface pin.
		wantClassifierState string
		// wantDriverStopReason pins the exact driver stopped_reason for a
		// decision/amendment stop; "" means the loop must DISPATCH acceptance.
		wantDriverStopReason string
		// divergence:true documents a deliberate driver-conservative divergence
		// (driver stops decision_required, classifier names a non-judgment state);
		// false requires the two surfaces to AGREE (decision_required IFF the
		// classifier names an operator-judgment state).
		divergence bool
	}{
		{
			// A plan gate parked with its review settled: the classifier names
			// plan_gate_parked (approve/revise/reject — operator judgment) and the
			// loop stops decision_required. They AGREE.
			name: "plan_gate_parked_settled_reviews",
			stages: []Stage{
				stg(drivePlanID, "plan", "awaiting_approval", 0),
				stg(driveImplID, "implement", "blocked", 1),
			},
			naStages: []Stage{
				stg(drivePlanID, "plan", "awaiting_approval", 0),
				stg(driveImplID, "implement", "blocked", 1),
			},
			wantClassifierState:  "plan_gate_parked",
			wantDriverStopReason: "decision_required:plan_gate_parked",
		},
		{
			// The fix: a review row parked at awaiting_approval under derived
			// acceptance_pending. The classifier (implement succeeded + acceptance
			// pending, review settled) names acceptance_pending — a mechanical
			// dispatch, NOT operator judgment — and the loop now DISPATCHES
			// acceptance instead of stopping. They AGREE.
			name:          "review_parked_derived_acceptance_pending",
			stages:        driveAcceptancePendingStages(),
			derivedStatus: "acceptance_pending",
			naStages: []Stage{
				stg(drivePlanID, "plan", "succeeded", 0),
				stg(driveImplID, "implement", "succeeded", 1),
				stg(driveAccID, "acceptance", "pending", 2),
			},
			wantClassifierState:  "acceptance_pending",
			wantDriverStopReason: "", // dispatches
		},
		{
			// The retained decision path (concern 3): the IDENTICAL review-parked
			// fixture WITHOUT the derived signal. The driver conservatively stops
			// decision_required:review_gate_parked; the classifier still names
			// acceptance_pending (non-judgment) — a documented, deliberate divergence.
			name:   "review_parked_no_derived_signal",
			stages: driveAcceptancePendingStages(),
			// derivedStatus intentionally empty — the legacy no-signal path.
			naStages: []Stage{
				stg(drivePlanID, "plan", "succeeded", 0),
				stg(driveImplID, "implement", "succeeded", 1),
				stg(driveAccID, "acceptance", "pending", 2),
			},
			wantClassifierState:  "acceptance_pending",
			wantDriverStopReason: "decision_required:review_gate_parked",
			divergence:           true,
		},
		{
			// The other retained decision path (concern 3): a pending scope
			// amendment during a running implement stage. The driver stops
			// decision_required:scope_amendment_requested (branch (b), audit-derived);
			// the classifier names implement_running (non-judgment) — a documented,
			// deliberate divergence (the classifier models no amendment state).
			name: "pending_scope_amendment",
			stages: []Stage{
				stg(drivePlanID, "plan", "succeeded", 0),
				stg(driveImplID, "implement", "running", 1),
			},
			seedAudit: func(f *driveFakeBackend) {
				f.seq++
				f.audit = append(f.audit, AuditEntry{
					ID: uuid.New().String(), Sequence: f.seq, RunID: f.runID.String(),
					Category: "scope_amendment_requested", Payload: map[string]any{},
				})
			},
			naStages: []Stage{
				stg(drivePlanID, "plan", "succeeded", 0),
				stg(driveImplID, "implement", "running", 1),
			},
			wantClassifierState:  "implement_running",
			wantDriverStopReason: "decision_required:scope_amendment_requested",
			divergence:           true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Drive the classifier over the equivalent inputs and pin its state —
			// the producer half of the cross-surface taxonomy pin.
			na := nextActionsFor(&Run{State: "running", RunnerKind: "local"}, tc.naStages,
				nil, tc.naReviewStatus, nil, nil, false, false, "", "", releaseSignals{})
			if na == nil {
				t.Fatalf("nextActionsFor returned nil for %s", tc.name)
			}
			if na.State != tc.wantClassifierState {
				t.Fatalf("classifier state = %q, want %q (pinned)", na.State, tc.wantClassifierState)
			}
			classifierJudgment := isOperatorJudgment(na.State)

			f := newDriveFake("running", tc.stages)
			f.derivedStatus = tc.derivedStatus
			if tc.seedAudit != nil {
				tc.seedAudit(f)
			}
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

			_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
			if err != nil {
				t.Fatalf("driveRun: %v", err)
			}
			gotDecision := strings.HasPrefix(out.StoppedReason, "decision_required")
			// Pin the driver half of the cross-surface pin: the exact stop, and
			// that a decision stop always carries next_actions.
			if tc.wantDriverStopReason != "" {
				if out.StoppedReason != tc.wantDriverStopReason {
					t.Fatalf("stopped_reason = %q, want %q", out.StoppedReason, tc.wantDriverStopReason)
				}
				if out.NextActions == nil || len(out.NextActions.Actions) == 0 {
					t.Fatalf("decision stop %q carried no next_actions", out.StoppedReason)
				}
			} else {
				// The acceptance_pending fixture must DISPATCH, not merely not-stop.
				if got := rec.list(); len(got) != 1 || got[0] != "acceptance" {
					t.Fatalf("spawn sequence = %v, want [acceptance] (dispatch, not stop)", got)
				}
			}
			if tc.divergence {
				// Documented deliberate divergence: the driver is conservative
				// (stops decision_required) while the classifier names a
				// non-operator-judgment state. Assert BOTH halves so the gap stays a
				// KNOWN, pinned divergence rather than silent drift — a change that
				// made these agree (either surface moving) fails here.
				if !gotDecision {
					t.Fatalf("%s: expected a driver decision_required stop, got %q", tc.name, out.StoppedReason)
				}
				if classifierJudgment {
					t.Fatalf("%s: documented divergence expects a NON-operator-judgment classifier state, got %q", tc.name, na.State)
				}
			} else {
				// Agreement invariant: decision_required IFF the classifier names an
				// operator-judgment state.
				if gotDecision != classifierJudgment {
					t.Fatalf("agreement broken: classifier state %q isOperatorJudgment=%v, driver decision_required=%v",
						na.State, classifierJudgment, gotDecision)
				}
			}
		})
	}
}

// --- (t3) resume of a runner-less STALE 'dispatched' stage -> distinct stop --

func TestDriveRun_ResumeDispatchedStale_StopsDistinct(t *testing.T) {
	// (T8) The genuine-stale branch (b): a stage sits in 'dispatched' with SPAWN
	// EVIDENCE — an hour-old dispatch-evidence row AND an hour-old UpdatedAt — and
	// no runner ever advanced it (a crashed/killed runner). Past the (lowered)
	// liveness threshold the driver must STOP dispatched_stale off the evidence
	// ANCHOR — never poll silently to timeout, and never auto-spawn — handing the
	// manual re-dispatch to the operator via next_actions.
	impl := stg(driveImplID, "implement", "dispatched", 1)
	impl.UpdatedAt = time.Now().Add(-time.Hour)
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		impl,
	})
	// An OLD dispatch-evidence row (past the threshold): the anchor is genuinely stale.
	f.appendAutoAt(map[string]any{"act": "dispatch", "action": "dispatch_stage", "stage": "implement", "source": "fishhawk_drive_run", "note": ""}, time.Now().Add(-time.Hour))
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()
	r.driveDispatchedStaleAfter = time.Millisecond // lower the threshold so the hour-old stage trips it
	// #1955: inject an UNKNOWN-verdict probe so this test pins the DEGRADED
	// manual-stop contract (pgrep absent / unprobeable) rather than the real
	// pgrep (which would find no process → DEAD → auto-re-dispatch).
	r.driveProbeRunnerLiveness = func(context.Context, string) runnerLivenessVerdict { return runnerUnknown }

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
	// #1924: the stale-stop message must no longer assert the unprobed "no
	// runner appears live" fact, and must instruct the operator to verify
	// runner-process liveness FIRST before any manual re-dispatch.
	assertDispatchedStaleWarning(t, out.Warnings)
}

// assertDispatchedStaleWarning pins the #1924-revised dispatched_stale stop
// message contract behaviorally: it drops the unprobed "no runner appears
// live" assertion and instructs the operator to verify runner-process
// liveness (pgrep / the dispatch log_path) before re-dispatching by hand.
func assertDispatchedStaleWarning(t *testing.T, warnings []string) {
	t.Helper()
	joined := strings.Join(warnings, "\n")
	if strings.Contains(joined, "no runner appears live") {
		t.Errorf("dispatched_stale warning still asserts the unprobed 'no runner appears live' fact:\n%s", joined)
	}
	for _, want := range []string{"pgrep -f fishhawk-runner", "log_path", "flipped it to 'running'"} {
		if !strings.Contains(joined, want) {
			t.Errorf("dispatched_stale warning missing %q (verify-liveness-first instruction):\n%s", want, joined)
		}
	}
}

// --- (#1955) dead probe -> auto-re-dispatch (primary done-means) --------------

func TestDriveRun_DispatchedStale_DeadProbe_AutoRedispatches(t *testing.T) {
	// The issue's primary behavior: an hour-old 'dispatched' implement this
	// invocation did not spawn crosses the liveness threshold; the driver's OWN
	// probe returns DEAD (no runner process), so it does NOT stop dispatched_stale
	// — it auto-recovers by falling through to the record-act -> host-dispatch
	// marker -> spawn path, attributes the record-act + DriveStep as a stale
	// re-dispatch, and drives on to merged. EXACTLY one spawn: dispatchedCount>0
	// after the respawn, so the stale guard cannot re-trip this invocation.
	impl := stg(driveImplID, "implement", "dispatched", 1)
	impl.UpdatedAt = time.Now().Add(-time.Hour) // past the threshold -> probed
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		impl,
	})
	// A prior implement dispatch row (an earlier invocation spawned the now-dead
	// runner): it makes the fixup-attribution block RUN on the fall-through path
	// (priorRow=true) — with no newer stage_fixup_triggered row it does NOT rename
	// to fixup_redispatch, so recordName stays 'implement' and the stale note wins.
	f.appendAutoAt(map[string]any{"act": "dispatch", "action": "dispatch_stage", "stage": "implement", "source": "fishhawk_drive_run", "note": ""}, time.Now().Add(-time.Hour))
	// The re-dispatched runner reaches its prompt fetch and settles the run.
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
	r.driveDispatchedStaleAfter = time.Millisecond
	r.driveMaxWallclock = 5 * time.Second
	r.driveProbeRunnerLiveness = func(context.Context, string) runnerLivenessVerdict { return runnerDead }

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	// No dispatched_stale stop — the dead probe auto-recovered and the run merged.
	if out.StoppedReason != stoppedMerged {
		t.Fatalf("stopped_reason = %q, want merged (dead probe -> auto-re-dispatch -> merged)", out.StoppedReason)
	}
	// SINGLE-SPAWN-PER-INVOCATION invariant (safety core): exactly one spawn.
	if got := rec.list(); len(got) != 1 || got[0] != "implement" {
		t.Fatalf("spawn sequence = %v, want exactly [implement] (one stale re-dispatch, guard cannot re-trip)", got)
	}
	f.mu.Lock()
	markerN := f.hostDispatchCalledN
	acts := append([]RecordAutoDriveAct(nil), f.recordedActs...)
	f.mu.Unlock()
	// record-act -> host-dispatch marker -> spawn: the marker fired exactly once.
	if markerN != 1 {
		t.Errorf("host-dispatch marker calls = %d, want 1 (record -> marker -> spawn)", markerN)
	}
	if len(acts) != 1 {
		t.Fatalf("recorded acts = %d, want 1 (a single stale re-dispatch)", len(acts))
	}
	// The record-act note marks the stale re-dispatch; recordName stayed implement.
	if acts[0].Note != staleRedispatchNote {
		t.Errorf("record-act note = %q, want %q", acts[0].Note, staleRedispatchNote)
	}
	if acts[0].Stage != "implement" {
		t.Errorf("record-act stage = %q, want implement (fixup attribution ran but did not rename)", acts[0].Stage)
	}
	// A DriveStep notes the auto-recovery.
	var sawStaleStep bool
	for _, s := range out.StepsTaken {
		if s.Kind == "dispatch" && strings.Contains(s.Note, "stale re-dispatch") {
			sawStaleStep = true
		}
	}
	if !sawStaleStep {
		t.Errorf("no DriveStep noting the stale re-dispatch auto-recovery; steps: %+v", out.StepsTaken)
	}
	// A warning names the negative liveness probe.
	joined := strings.Join(out.Warnings, "\n")
	if !strings.Contains(joined, "found NO matching runner process") {
		t.Errorf("no warning naming the negative liveness probe; warnings: %v", out.Warnings)
	}
}

// --- (#1955) dead probe on a FIXUP-re-opened stage -> fixup_redispatch survives ---

func TestDriveRun_DispatchedStale_DeadProbe_FixupRedispatchAttribution(t *testing.T) {
	// The combination the fall-through preserves but the plain dead-probe test does
	// not exercise: a fix-up re-opened implement stage (a stage_fixup_triggered row
	// NEWER than the newest implement dispatch row, so the fixup-attribution block
	// renames recordName to fixup_redispatch) whose fresh runner then dies past the
	// liveness threshold. The dead probe sets staleRedispatch, so the stale note must
	// OVERWRITE the (blanked) fixup note WHILE the fixup_redispatch recordName is
	// preserved — the record lands stage="fixup_redispatch" AND note=staleRedispatchNote.
	impl := stg(driveImplID, "implement", "dispatched", 1)
	impl.UpdatedAt = time.Now().Add(-time.Hour) // past the threshold -> probed
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		impl,
	})
	// A prior implement dispatch row (the fix-up's first dispatch that then died) —
	// makes priorRow=true so the fixup-attribution block runs on the fall-through.
	f.appendAutoAt(map[string]any{"act": "dispatch", "action": "dispatch_stage", "stage": "implement", "source": "fishhawk_drive_run", "note": ""}, time.Now().Add(-time.Hour))
	// A stage_fixup_triggered row NEWER than that dispatch row -> fixupSeq >
	// newestDispatchSeq, so recordName renames to fixup_redispatch.
	f.seq++
	f.audit = append(f.audit, AuditEntry{ID: uuid.New().String(), Sequence: f.seq, RunID: f.runID.String(), Category: categoryStageFixupTriggered, Payload: map[string]any{}})
	// The re-dispatched runner reaches its prompt fetch and settles the run.
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
	r.driveDispatchedStaleAfter = time.Millisecond
	r.driveMaxWallclock = 5 * time.Second
	r.driveProbeRunnerLiveness = func(context.Context, string) runnerLivenessVerdict { return runnerDead }

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != stoppedMerged {
		t.Fatalf("stopped_reason = %q, want merged (dead probe -> fixup re-dispatch -> merged)", out.StoppedReason)
	}
	// SINGLE-SPAWN-PER-INVOCATION invariant: exactly one spawn.
	if got := rec.list(); len(got) != 1 || got[0] != "implement" {
		t.Fatalf("spawn sequence = %v, want exactly [implement] (one stale re-dispatch)", got)
	}
	f.mu.Lock()
	acts := append([]RecordAutoDriveAct(nil), f.recordedActs...)
	f.mu.Unlock()
	if len(acts) != 1 {
		t.Fatalf("recorded acts = %d, want 1 (a single stale re-dispatch)", len(acts))
	}
	// The exact interaction: fixup_redispatch recordName survives AND the stale note
	// overwrites the blanked fixup note.
	if acts[0].Stage != "fixup_redispatch" {
		t.Errorf("record-act stage = %q, want fixup_redispatch (fixup attribution renamed and survived the stale note)", acts[0].Stage)
	}
	if acts[0].Note != staleRedispatchNote {
		t.Errorf("record-act note = %q, want %q (stale note overwrote the fixup note)", acts[0].Note, staleRedispatchNote)
	}
}

// --- (#1955) live probe -> stop dispatched_stale, NEVER spawn (safety core) ---

func TestDriveRun_DispatchedStale_LiveProbe_StopsWithoutSpawn(t *testing.T) {
	// LIVE-never-spawns invariant (the safety core): a 'dispatched' stage past the
	// threshold whose probe finds a LIVE process matching the stage id (yet it
	// never flipped 'running') is anomalous — the driver STOPS dispatched_stale,
	// spawns nothing, records nothing, and its warning names the live process +
	// log_path without instructing an immediate re-dispatch. A second runner into
	// the same lineage lock stays impossible by construction.
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
	r.driveDispatchedStaleAfter = time.Millisecond
	r.driveProbeRunnerLiveness = func(context.Context, string) runnerLivenessVerdict { return runnerLive }

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != stoppedDispatchedStale {
		t.Fatalf("stopped_reason = %q, want dispatched_stale (live probe)", out.StoppedReason)
	}
	if got := rec.list(); len(got) != 0 {
		t.Fatalf("driver spawned on a LIVE probe: %v (a second runner into the same lineage lock must be impossible)", got)
	}
	f.mu.Lock()
	nActs := len(f.recordedActs)
	f.mu.Unlock()
	if nActs != 0 {
		t.Errorf("driver recorded %d acts on a live-probe stop; want 0", nActs)
	}
	joined := strings.Join(out.Warnings, "\n")
	for _, want := range []string{"IS live", "log_path"} {
		if !strings.Contains(joined, want) {
			t.Errorf("live-probe warning missing %q; warnings: %v", want, out.Warnings)
		}
	}
	// It must NOT instruct an immediate hand re-dispatch (unlike the unknown path).
	if strings.Contains(joined, "re-dispatch by hand") {
		t.Errorf("live-probe warning wrongly instructs an immediate re-dispatch:\n%s", joined)
	}
	if out.NextActions == nil || len(out.NextActions.Actions) == 0 {
		t.Fatal("live-probe dispatched_stale stop carried no next_actions")
	}
	// The manual verify-and-re-dispatch instruction survives ONLY on the
	// UNKNOWN/unprobeable branch: a confirmed-LIVE process must NEVER hand back a
	// fishhawk_dispatch_stage action (a re-dispatch would spawn the second runner
	// this driver exists to prevent). The LIVE arm offers inspect-only.
	if got := out.NextActions.Actions[0].Action; got != "fishhawk_get_run_status" {
		t.Errorf("next_actions[0] = %q, want fishhawk_get_run_status (inspect-only, NOT re-dispatch)", got)
	}
	for _, a := range out.NextActions.Actions {
		if a.Action == "fishhawk_dispatch_stage" {
			t.Errorf("live-probe next_actions wrongly offers fishhawk_dispatch_stage (manual re-dispatch): %+v", a)
		}
		joined := a.Precondition + " " + a.Reason
		if strings.Contains(joined, "pgrep") || strings.Contains(joined, "re-dispatch by hand") {
			t.Errorf("live-probe next_action wrongly carries the manual pgrep/re-dispatch instruction: %+v", a)
		}
	}
}

// --- (#1955) pure pgrep exit-code -> verdict mapping --------------------------

func TestClassifyPgrepResult(t *testing.T) {
	// Pin the pgrep exit-code contract (procps-ng/BSD pgrep(1) EXIT STATUS) using
	// REAL *exec.ExitError values from `sh -c 'exit N'`, plus a fabricated
	// pgrep-absent error, a context-timeout error, and an arbitrary non-ExitError
	// exec failure — the last three all degrade to runnerUnknown (binding
	// condition 1). This is what fails if a platform's pgrep semantics diverge.
	tctx, tcancel := context.WithCancel(context.Background())
	tcancel() // pre-cancel so the exec is killed -> a context-timeout-shaped error
	timeoutErr := exec.CommandContext(tctx, "sleep", "5").Run()

	cases := []struct {
		name string
		err  error
		want runnerLivenessVerdict
	}{
		{"exit0_live", exec.Command("sh", "-c", "exit 0").Run(), runnerLive},
		{"exit1_dead", exec.Command("sh", "-c", "exit 1").Run(), runnerDead},
		{"exit2_syntax_unknown", exec.Command("sh", "-c", "exit 2").Run(), runnerUnknown},
		{"exit3_fatal_unknown", exec.Command("sh", "-c", "exit 3").Run(), runnerUnknown},
		{"pgrep_absent_unknown", exec.ErrNotFound, runnerUnknown},
		{"context_timeout_unknown", timeoutErr, runnerUnknown},
		{"arbitrary_error_unknown", errStub("boom"), runnerUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyPgrepResult(tc.err); got != tc.want {
				t.Errorf("classifyPgrepResult(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// --- (#1955 binding condition 2) README staleness-bullet is a pass/fail doc ---

func TestDriveRunREADME_StalenessBulletDocumentsProbe(t *testing.T) {
	// The README "Anchor past the liveness threshold" bullet is a named
	// deliverable of #1955: it must document the driver's own host-liveness probe
	// and the three-way DEAD/LIVE/UNKNOWN outcome. Anchored on load-bearing
	// phrases (not full sentences) so a reword does not fail spuriously, but a
	// DROPPED rewrite does. This makes the doc change pass/fail per the operator's
	// binding approval condition.
	body, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	readme := string(body)
	for _, anchor := range []string{
		"Anchor past the liveness threshold",
		"probes host liveness itself",
		"--stage-id",
		"auto-recovered in place",
		"stale re-dispatch: liveness probe found no runner process",
		"never spawns", // the LIVE-never-spawns safety-core statement
		"UNKNOWN",      // the pgrep-absent / exec-error degrade to the manual stop
	} {
		if !strings.Contains(readme, anchor) {
			t.Errorf("README drive_run staleness bullet missing %q (#1955 doc deliverable)", anchor)
		}
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

// --- (t5b) fixup-attribution read error -> fail-closed, never dispatch -------

func TestDriveRun_FixupAttributionCheckError_FailsClosed(t *testing.T) {
	// The LATER audit read in the dispatch path — driveNewestFixupTriggeredSeq,
	// reached only when a fresh invocation (dispatchedCount==0) re-dispatches a
	// still-'pending' implement stage that a prior invocation already dispatched
	// (priorRow==true) — errors. Like the earlier prior-dispatch-row read, an
	// unreadable stage_fixup_triggered state precedes a record + host-spawn and
	// must NEVER be downgraded to "no fix-up trigger": halt fail-closed, never
	// record, never spawn.
	//
	// The error is scoped to categoryStageFixupTriggered ONLY: the
	// CategoryRunAutoDriven read (driveHasPriorDispatchRow) succeeds and returns
	// priorRow==true, so the loop reaches the NEW branch rather than halting at
	// driveHasPriorDispatchRow (the existing (l3) fixture errors
	// CategoryRunAutoDriven and stops earlier). This pins the branch (l3) cannot.
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		stg(driveImplID, "implement", "pending", 1),
	})
	// A prior implement dispatch row: makes driveHasPriorDispatchRow return
	// priorRow==true so the fixup-attribution branch is entered.
	f.appendAuto(map[string]any{"act": "dispatch", "action": "dispatch_stage", "stage": "implement", "source": "fishhawk_drive_run", "note": ""})
	f.auditErrCategory = categoryStageFixupTriggered // only the fix-up trigger read errors
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != stoppedDispatchCheckFailed {
		t.Fatalf("stopped_reason = %q, want dispatch_check_failed (fail-closed on the fixup-attribution read)", out.StoppedReason)
	}
	if got := rec.list(); len(got) != 0 {
		t.Fatalf("driver spawned despite an unreadable stage_fixup_triggered state: %v", got)
	}
	f.mu.Lock()
	nActs := len(f.recordedActs)
	f.mu.Unlock()
	if nActs != 0 {
		t.Errorf("driver recorded %d acts despite the fail-closed fixup-attribution read error; want 0", nActs)
	}
	var warned bool
	for _, w := range out.Warnings {
		if strings.Contains(w, "fixup-attribution poll failed") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no fail-closed warning about the fixup-attribution read; warnings: %v", out.Warnings)
	}
	if out.NextActions == nil {
		t.Error("fail-closed fixup-attribution stop carried no next_actions")
	}
}

// --- (t5c) stale threshold crossed MID-invocation -> distinct stop -----------

func TestDriveRun_ResumeDispatchedStale_MidInvocation_StopsDistinct(t *testing.T) {
	// The dispatched-guard rework replaced the one-shot spawned[] mark with a
	// poll+continue that RE-EVALUATES the stale threshold every iteration. The
	// existing stale test seeds a stage already stale at loop start; this one
	// seeds a stage FRESH at loop start (UpdatedAt == now, threshold not yet
	// crossed) and lets the threshold trip DURING the poll loop — pinning the
	// "can also trip mid-invocation" property the comment advertises. The stage
	// never advances, so once time.Since(UpdatedAt) passes the (tiny, but
	// non-zero at start) threshold, the guard re-trips and stops dispatched_stale
	// rather than polling silently to the wall-clock deadline.
	impl := stg(driveImplID, "implement", "dispatched", 1)
	impl.UpdatedAt = time.Now() // FRESH at loop start: not stale on the first check
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		impl,
	})
	// A FRESH dispatch-evidence row (spawn evidence, fresh anchor at loop start):
	// the anchor is live on the first check and only crosses the threshold as the
	// loop polls, so branch (b) trips MID-invocation rather than immediately.
	f.appendAuto(map[string]any{"act": "dispatch", "action": "dispatch_stage", "stage": "implement", "source": "fishhawk_drive_run", "note": ""})
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()
	// Threshold comfortably larger than the first check's sub-ms latency (so the
	// first iteration polls, not stops) yet far smaller than the wall-clock
	// budget (so the stale stop, not timeout, is what fires).
	r.driveDispatchedStaleAfter = 40 * time.Millisecond
	r.driveMaxWallclock = 10 * time.Second
	// #1955: an UNKNOWN probe keeps the mid-invocation trip a manual stop (the
	// point of this test is the threshold re-trip, not the probe branch).
	r.driveProbeRunnerLiveness = func(context.Context, string) runnerLivenessVerdict { return runnerUnknown }

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != stoppedDispatchedStale {
		t.Fatalf("stopped_reason = %q, want dispatched_stale (threshold crossed mid-invocation)", out.StoppedReason)
	}
	if got := rec.list(); len(got) != 0 {
		t.Fatalf("driver spawned a stage whose stale threshold tripped mid-invocation: %v (must never auto-spawn)", got)
	}
	f.mu.Lock()
	nActs := len(f.recordedActs)
	f.mu.Unlock()
	if nActs != 0 {
		t.Errorf("driver recorded %d acts on a mid-invocation stale stop; want 0", nActs)
	}
	if out.NextActions == nil || len(out.NextActions.Actions) == 0 {
		t.Fatal("mid-invocation dispatched_stale stop carried no next_actions")
	}
	if out.NextActions.Actions[0].Action != "fishhawk_dispatch_stage" {
		t.Errorf("next_actions[0] = %q, want fishhawk_dispatch_stage", out.NextActions.Actions[0].Action)
	}
	assertDispatchedStaleWarning(t, out.Warnings)
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

// driveReviewID is the fixed stage id for the human 'review' row a
// feature_change run creates pending at run creation.
var driveReviewID = uuid.NewSHA1(uuid.Nil, []byte("review")).String()

// seedPlanReviewStarted appends a plan_review_started row with the given
// configured-agent count (the #1127 count gate reviewStatusFor reads).
func (f *driveFakeBackend) seedPlanReviewStarted(configured int) {
	f.seq++
	f.audit = append(f.audit, AuditEntry{
		ID: uuid.New().String(), Sequence: f.seq, RunID: f.runID.String(),
		Category: "plan_review_started", Payload: map[string]any{"configured_agents": configured},
	})
}

// seedPlanReviewed appends one plan_reviewed terminal verdict row.
func (f *driveFakeBackend) seedPlanReviewed(verdict string) {
	f.seq++
	f.audit = append(f.audit, AuditEntry{
		ID: uuid.New().String(), Sequence: f.seq, RunID: f.runID.String(),
		Category: "plan_reviewed", Payload: map[string]any{"verdict": verdict},
	})
}

// seedImplementReviewStarted appends an implement_review_started row with the
// given configured-agent count — the implement-stage analog of
// seedPlanReviewStarted, feeding the #1127 count gate reviewStatusFor("implement")
// reads.
func (f *driveFakeBackend) seedImplementReviewStarted(configured int) {
	f.seq++
	f.audit = append(f.audit, AuditEntry{
		ID: uuid.New().String(), Sequence: f.seq, RunID: f.runID.String(),
		Category: "implement_review_started", Payload: map[string]any{"configured_agents": configured},
	})
}

// seedImplementReviewed appends one implement_reviewed terminal verdict row.
func (f *driveFakeBackend) seedImplementReviewed(verdict string) {
	f.seq++
	f.audit = append(f.audit, AuditEntry{
		ID: uuid.New().String(), Sequence: f.seq, RunID: f.runID.String(),
		Category: "implement_reviewed", Payload: map[string]any{"verdict": verdict},
	})
}

// --- (T1) the #1905 incident: plan parked + settled reviews on a fixture that
// INCLUDES the pending human review stage row -> decision_required, NOT a poll
// to timeout ----------------------------------------------------------------

func TestDriveRun_PlanGateParked_SettledReviews_DecisionRequired(t *testing.T) {
	// The exact #1905 shape: a feature_change run creates the human 'review'
	// stage row 'pending' at run creation. Once the plan stage parks
	// awaiting_approval, the OLD driveAnyInFlight treated that pending review as
	// in-flight forever (branch d), so the gate/decision branch (e) was
	// unreachable — a silent poll to the client timeout. With the reachability
	// fix the pending review (behind a non-terminal plan) is NOT in-flight, so
	// the loop reaches the parked gate, waits for the settled reviews, and
	// returns decision_required:plan_gate_parked.
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "awaiting_approval", 0),
		stg(driveImplID, "implement", "pending", 1),
		stg(driveReviewID, "review", "pending", 2),
		stg(driveAccID, "acceptance", "pending", 3),
	})
	f.seedPlanReviewStarted(2)
	f.seedPlanReviewed("approve")
	f.seedPlanReviewed("approve_with_concerns")
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "no delegated knob"} }
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()
	// Bounded: the pre-fix code would poll to this deadline (a timeout stop). The
	// fix must return decision_required well before it.
	r.driveMaxWallclock = 5 * time.Second

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != "decision_required:plan_gate_parked" {
		t.Fatalf("stopped_reason = %q, want decision_required:plan_gate_parked (NOT a poll-to-timeout)", out.StoppedReason)
	}
	if out.NextActions == nil || len(out.NextActions.Actions) == 0 {
		t.Error("decision_required stop carried no next_actions")
	}
	if got := rec.list(); len(got) != 0 {
		t.Errorf("no stage should have been dispatched at a parked plan gate: %v", got)
	}
}

// --- (T2) reviews still pending at a parked gate -> POLL with ZERO gate calls,
// then gate once the reviews settle ------------------------------------------

func TestDriveRun_PlanGateParked_ReviewsPending_PollsThenGates(t *testing.T) {
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "awaiting_approval", 0),
		stg(driveImplID, "implement", "pending", 1),
		stg(driveReviewID, "review", "pending", 2),
		stg(driveAccID, "acceptance", "pending", 3),
	})
	f.seedPlanReviewStarted(2)
	f.seedPlanReviewed("approve") // only ONE of two verdicts landed -> pending
	// Land the SECOND verdict on the 2nd plan_reviewed read (the 2nd loop
	// iteration), so the first iteration observes 'pending' and must NOT gate.
	reads := 0
	f.onAudit = func(f *driveFakeBackend, category string) {
		if category != "plan_reviewed" {
			return
		}
		reads++
		if reads == 2 {
			f.seq++
			f.audit = append(f.audit, AuditEntry{
				ID: uuid.New().String(), Sequence: f.seq, RunID: f.runID.String(),
				Category: "plan_reviewed", Payload: map[string]any{"verdict": "approve"},
			})
		}
	}
	// LOAD-BEARING ordering check (concern: the fake's observe-only outcome would
	// otherwise mask a premature gate call — a regression gate-calling on the
	// first still-pending iteration returns the same decision_required with
	// gateCalls==1). At the instant the gate is called, the advisory reviews MUST
	// already be settled; record a violation if the driver gate-called while the
	// round was still pending.
	var gatedWhilePending bool
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome {
		configured, verdicts := 0, 0
		for _, e := range f.audit {
			switch e.Category {
			case "plan_review_started":
				if m, ok := e.Payload.(map[string]any); ok {
					if c, ok := m["configured_agents"].(int); ok && c > configured {
						configured = c
					}
				}
			case "plan_reviewed":
				verdicts++
			}
		}
		if configured > 0 && verdicts < configured {
			gatedWhilePending = true
		}
		return AutoDriveOutcome{Note: "observe-only"}
	}
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()
	r.driveMaxWallclock = 5 * time.Second

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != "decision_required:plan_gate_parked" {
		t.Fatalf("stopped_reason = %q, want decision_required:plan_gate_parked (settled then gated)", out.StoppedReason)
	}
	// The gate was called EXACTLY once — zero calls while the reviews were
	// pending (iteration 1), one after settlement (iteration 2).
	f.mu.Lock()
	gateCalls := f.gateCalls
	f.mu.Unlock()
	if gateCalls != 1 {
		t.Errorf("gate called %d times; want exactly 1 (zero while reviews pending, one after settlement)", gateCalls)
	}
	// The single gate call happened AFTER the advisory round settled, proving the
	// review-settlement wait is load-bearing (not a decision_required the fake's
	// observe-only faked past a premature, still-pending gate call).
	if gatedWhilePending {
		t.Error("driver gate-called while the advisory reviews were still pending; the review-settlement wait did not hold zero gate calls during the pending round")
	}
}

// --- (T2b) the review->implement mapping branch: a parked REVIEW-type gate
// waits on the IMPLEMENT advisory round -> POLL with ZERO gate calls, then gate
// once that round settles. Pins driveReviewStageForParkedType("review") ->
// "implement" (drive_run.go:806-807), the branch T1-T3 never exercise (they
// park a PLAN gate). Concern deferred from #1908 / #1909. ---------------------

func TestDriveRun_ReviewGateParked_ImplementReviewsPending_PollsThenGates(t *testing.T) {
	// The production shape this branch was written for: a review-type stage
	// parked awaiting_approval behind succeeded plan/implement stages, whose
	// gate waits on the implement advisory round. Mirrors T2's choreography for
	// the review->implement mapping instead of plan->plan.
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		stg(driveImplID, "implement", "succeeded", 1),
		stg(driveReviewID, "review", "awaiting_approval", 2),
		stg(driveAccID, "acceptance", "pending", 3),
	})
	f.seedImplementReviewStarted(2)
	f.seedImplementReviewed("approve") // only ONE of two verdicts landed -> pending
	// Land the SECOND verdict on the 2nd implement_reviewed read (the 2nd loop
	// iteration), so the first iteration observes 'pending' and must NOT gate.
	reads := 0
	f.onAudit = func(f *driveFakeBackend, category string) {
		if category != "implement_reviewed" {
			return
		}
		reads++
		if reads == 2 {
			f.seq++
			f.audit = append(f.audit, AuditEntry{
				ID: uuid.New().String(), Sequence: f.seq, RunID: f.runID.String(),
				Category: "implement_reviewed", Payload: map[string]any{"verdict": "approve"},
			})
		}
	}
	// LOAD-BEARING ordering check (same as T2, adapted to the implement round): at
	// the instant the gate is called, the implement advisory round MUST already be
	// settled. A regression that gate-calls on the first still-pending iteration
	// returns the same decision_required with gateCalls==1, so without this probe
	// the assertion would pass vacuously.
	var gatedWhilePending bool
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome {
		configured, verdicts := 0, 0
		for _, e := range f.audit {
			switch e.Category {
			case "implement_review_started":
				if m, ok := e.Payload.(map[string]any); ok {
					if c, ok := m["configured_agents"].(int); ok && c > configured {
						configured = c
					}
				}
			case "implement_reviewed":
				verdicts++
			}
		}
		if configured > 0 && verdicts < configured {
			gatedWhilePending = true
		}
		return AutoDriveOutcome{Note: "observe-only"}
	}
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()
	r.driveMaxWallclock = 5 * time.Second

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	// The eventual decision return — NOT the silent poll-to-timeout hang the
	// concern describes (a regression to that shape fails on stoppedTimeout here).
	if out.StoppedReason != "decision_required:review_gate_parked" {
		t.Fatalf("stopped_reason = %q, want decision_required:review_gate_parked (settled then gated)", out.StoppedReason)
	}
	// Zero gate calls while the implement round was pending (iteration 1), exactly
	// one after settlement (iteration 2).
	f.mu.Lock()
	gateCalls := f.gateCalls
	f.mu.Unlock()
	if gateCalls != 1 {
		t.Errorf("gate called %d times; want exactly 1 (zero while the implement round was pending, one after settlement)", gateCalls)
	}
	if gatedWhilePending {
		t.Error("driver gate-called while the implement advisory round was still pending; the review-settlement wait did not hold zero gate calls during the pending round")
	}
	if out.NextActions == nil || len(out.NextActions.Actions) == 0 {
		t.Error("decision_required stop carried no next_actions")
	}
	if got := rec.list(); len(got) != 0 {
		t.Errorf("no stage should have been dispatched at a parked review gate: %v", got)
	}
}

// TestDriveRun_TerminalFailed_InFlightReview_StopsNotPolls pins the #1915
// division of labor from the drive_run vantage the operator named: the
// in-flight-review-aware keep-polling on a terminal run lives in the await
// primitives (fishhawk_await_audit / fishhawk_await_review), NOT in
// fishhawk_drive_run. Even with a review stage parked awaiting_approval whose
// advisory round is still pending (only one of two verdicts landed), a
// terminal-FAILED run makes drive_run bail immediately with stopped_reason
// run_failed — it does NOT inherit the await primitives' keep-polling. A
// regression that made drive_run also keep polling on a terminal run would blow
// past the terminal check and hit the wall-clock timeout (a DIFFERENT
// stopped_reason), so asserting run_failed here is load-bearing. No gate is
// called and no stage is spawned once the run is terminal.
func TestDriveRun_TerminalFailed_InFlightReview_StopsNotPolls(t *testing.T) {
	f := newDriveFake("failed", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		stg(driveImplID, "implement", "succeeded", 1),
		stg(driveReviewID, "review", "awaiting_approval", 2),
	})
	// In-flight implement advisory round: two configured, only one verdict —
	// reviewCategoryInFlight("implement") would report pending. Irrelevant to a
	// terminal run's bail, which is exactly what this test pins.
	f.seedImplementReviewStarted(2)
	f.seedImplementReviewed("approve")

	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()
	r.driveMaxWallclock = 5 * time.Second

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != stoppedRunFailed {
		t.Fatalf("stopped_reason = %q, want %q (terminal-failed run bails, does not keep polling for the in-flight verdict)", out.StoppedReason, stoppedRunFailed)
	}
	f.mu.Lock()
	gateCalls := f.gateCalls
	f.mu.Unlock()
	if gateCalls != 0 {
		t.Errorf("gate called %d times; want 0 on a terminal run", gateCalls)
	}
	if got := rec.list(); len(got) != 0 {
		t.Errorf("no stage should be spawned on a terminal run: %v", got)
	}
}

// --- TestDriveReviewStageForParkedType pins the pure mapping table so a future
// edit to the switch cannot silently drop the review->implement branch. --------

func TestDriveReviewStageForParkedType(t *testing.T) {
	cases := []struct {
		parkedType string
		want       string
	}{
		{"plan", "plan"},        // a parked plan gate waits on the plan review
		{"review", "implement"}, // a parked review-type gate waits on the implement review
		{"implement", ""},       // everything else skips the wait -> falls through to the gate
		{"acceptance", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := driveReviewStageForParkedType(tc.parkedType); got != tc.want {
			t.Errorf("driveReviewStageForParkedType(%q) = %q, want %q", tc.parkedType, got, tc.want)
		}
	}
}

// --- (T3) review-status read error at a parked gate -> fail-toward-operator:
// decision_required + a warning (binding condition 2) -----------------------

func TestDriveRun_PlanGateParked_ReviewReadError_DecisionWithWarning(t *testing.T) {
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "awaiting_approval", 0),
		stg(driveImplID, "implement", "pending", 1),
		stg(driveReviewID, "review", "pending", 2),
		stg(driveAccID, "acceptance", "pending", 3),
	})
	// Only the plan_reviewed read errors — the amendment poll
	// (scope_amendment_*) and the run_auto_driven reads still succeed, so the
	// stop is the review-read fall-through, not amendment_check_failed.
	f.auditErrCategory = "plan_reviewed"
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()
	r.driveMaxWallclock = 5 * time.Second

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != "decision_required:plan_gate_parked" {
		t.Fatalf("stopped_reason = %q, want decision_required:plan_gate_parked (fail toward the operator)", out.StoppedReason)
	}
	var warned bool
	for _, w := range out.Warnings {
		if strings.Contains(w, "review-status poll for the parked plan gate failed") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no fail-toward-operator warning about the review-status read; warnings: %v", out.Warnings)
	}
	if got := rec.list(); len(got) != 0 {
		t.Errorf("no stage should have been dispatched: %v", got)
	}
}

// --- (T4) reachability of the driveAnyInFlight change (both branches) --------

func TestDriveRun_ReviewReachability(t *testing.T) {
	t.Run("pending_review_behind_nonterminal_predecessor_not_in_flight", func(t *testing.T) {
		// A pending review whose lower-sequence plan gate is still parked
		// (non-terminal) is NOT reachable, so it must NOT count as in-flight —
		// otherwise the loop hangs (the #1905 incident). With no delegated knob the
		// loop reaches the gate and returns decision_required rather than polling
		// to the deadline.
		f := newDriveFake("running", []Stage{
			stg(drivePlanID, "plan", "awaiting_approval", 0),
			stg(driveReviewID, "review", "pending", 1),
		})
		f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
		rec := &spawnRecorder{}
		r, srv := newDriveResolver(t, f, rec)
		defer srv.Close()
		r.driveMaxWallclock = 5 * time.Second

		_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
		if err != nil {
			t.Fatalf("driveRun: %v", err)
		}
		if out.StoppedReason != "decision_required:plan_gate_parked" {
			t.Fatalf("stopped_reason = %q, want decision_required:plan_gate_parked (pending review behind a parked plan is NOT in-flight)", out.StoppedReason)
		}
	})

	t.Run("pending_review_with_predecessors_terminal_is_in_flight", func(t *testing.T) {
		// A pending review whose every lower-sequence stage is terminal IS
		// reachable — it counts as in-flight and the loop polls it (never gates,
		// never dispatches). Bounded to a timeout since it never settles.
		f := newDriveFake("running", []Stage{
			stg(drivePlanID, "plan", "succeeded", 0),
			stg(driveImplID, "implement", "succeeded", 1),
			stg(driveReviewID, "review", "pending", 2),
		})
		f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Acted: true, Action: "approve"} }
		rec := &spawnRecorder{}
		r, srv := newDriveResolver(t, f, rec)
		defer srv.Close()
		r.driveMaxWallclock = 40 * time.Millisecond

		_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
		if err != nil {
			t.Fatalf("driveRun: %v", err)
		}
		if out.StoppedReason != stoppedTimeout {
			t.Fatalf("stopped_reason = %q, want timeout (reachable pending review polled as in-flight)", out.StoppedReason)
		}
		f.mu.Lock()
		gateCalls := f.gateCalls
		f.mu.Unlock()
		if gateCalls != 0 {
			t.Errorf("gate called %d times; want 0 (a reachable in-flight review is polled, not gated)", gateCalls)
		}
	})
}

// --- (T5) progress heartbeat at the REAL MCP boundary (binding condition 1) --

func TestDriveRun_ProgressHeartbeat_RealMCPBoundary(t *testing.T) {
	// Binding condition 1: exercise the supplied-progressToken heartbeat at the
	// REAL MCP boundary. No in-process run_stage progress harness exists to
	// reuse, so this stands one up: register fishhawk_drive_run on an MCP server,
	// connect an in-memory client with a ProgressNotificationHandler, CallTool
	// with a progressToken, and assert the driver extracted the request token and
	// emitted session progress notifications on the heartbeat cadence carrying
	// driveProgressMessage content.
	ctx := context.Background()
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "running", 0), // in-flight: the loop polls, emitting a heartbeat per iteration
	})
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
	// Deterministic iteration count (concern: a >=1 assertion accepts a single
	// notification for an otherwise multi-iteration drive, leaving the cadence
	// unpinned). The plan stage is polled as in-flight, so exactly one heartbeat
	// fires per loop iteration. Settle the run to succeeded on the 3rd stages read
	// so the loop runs EXACTLY wantHeartbeats heartbeat-emitting iterations — the
	// next GetRun observes terminal and exits before its heartbeat.
	const wantHeartbeats = 3
	stageReads := 0
	f.onStages = func(f *driveFakeBackend) {
		stageReads++
		if stageReads == wantHeartbeats {
			f.runState = "succeeded"
		}
	}
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()
	r.driveMaxWallclock = 5 * time.Second // safety net; the deterministic settle fires first

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0"}, nil)
	registerDriveRun(server, r)

	var mu sync.Mutex
	var notes []*mcp.ProgressNotificationParams
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			mu.Lock()
			notes = append(notes, req.Params)
			mu.Unlock()
		},
	})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer clientSession.Close()

	params := &mcp.CallToolParams{
		Name:      "fishhawk_drive_run",
		Arguments: map[string]any{"run_id": f.runID.String(), "github_repo": "x/y"},
	}
	params.SetProgressToken("drive-tok-1")
	res, err := clientSession.CallTool(ctx, params)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
	}

	// Notifications are delivered async; wait for all wantHeartbeats to flush.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(notes)
		mu.Unlock()
		if n >= wantHeartbeats {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	// EXACTLY one heartbeat per poll iteration — not merely "at least one". The
	// deterministic settle bounds the drive to wantHeartbeats iterations, so a
	// regression emitting a single (or per-drive) notification fails here.
	if len(notes) != wantHeartbeats {
		t.Fatalf("received %d progress notifications at the real MCP boundary; want exactly %d (one per poll iteration)", len(notes), wantHeartbeats)
	}
	for i, n := range notes {
		if n.ProgressToken != "drive-tok-1" {
			t.Errorf("notification progressToken = %v, want the request token drive-tok-1", n.ProgressToken)
		}
		if !strings.HasPrefix(n.Message, "drive: run ") {
			t.Errorf("notification message = %q, want driveProgressMessage content", n.Message)
		}
		// Progress increments once per iteration (drive_run.go: progress++ before
		// each emit), so the i-th notification carries progress i+1 — a monotone
		// per-iteration cadence a single lumped emission cannot satisfy.
		if n.Progress != float64(i+1) {
			t.Errorf("notification[%d] progress = %v, want %d (one increment per poll iteration)", i, n.Progress, i+1)
		}
	}
}

// --- (T5b) no progressToken -> no emission (opt-in) at the real MCP boundary --

func TestDriveRun_ProgressHeartbeat_NoToken_NoEmission(t *testing.T) {
	// MCP progress is opt-in: a real CallTool that supplies NO progressToken must
	// receive ZERO progress notifications — the same real-boundary harness as the
	// emission test, minus SetProgressToken, so the client's handler proves the
	// driver emitted nothing.
	ctx := context.Background()
	f := newDriveFake("running", []Stage{stg(drivePlanID, "plan", "running", 0)})
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()
	r.driveMaxWallclock = 60 * time.Millisecond

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0"}, nil)
	registerDriveRun(server, r)

	var mu sync.Mutex
	var notes int
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, _ *mcp.ProgressNotificationClientRequest) {
			mu.Lock()
			notes++
			mu.Unlock()
		},
	})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer clientSession.Close()

	// No SetProgressToken: the opt-in is not exercised.
	res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_drive_run",
		Arguments: map[string]any{"run_id": f.runID.String(), "github_repo": "x/y"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
	}
	// Give any (erroneous) notifications a moment to arrive, then assert none did.
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if notes != 0 {
		t.Errorf("received %d progress notifications with no progressToken; want 0 (opt-in)", notes)
	}
}

// --- (T5c) driveProgressMessage pure-function table -------------------------

func TestDriveProgressMessage(t *testing.T) {
	run := &Run{State: "running"}
	cases := []struct {
		name   string
		stages []Stage
		steps  int
		want   string
	}{
		{
			name:   "earliest non-terminal stage summarized",
			stages: []Stage{stg(drivePlanID, "plan", "succeeded", 0), stg(driveImplID, "implement", "running", 1)},
			steps:  2,
			want:   "drive: run running; next implement:running; steps 2; elapsed 5s",
		},
		{
			name:   "all terminal -> no non-terminal stage",
			stages: []Stage{stg(drivePlanID, "plan", "succeeded", 0)},
			steps:  0,
			want:   "drive: run running; next no non-terminal stage; steps 0; elapsed 5s",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := driveProgressMessage(run, tc.stages, tc.steps, 5*time.Second)
			if got != tc.want {
				t.Errorf("driveProgressMessage = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- (g) DONE-MEANS: the #1905 no-spawn-evidence inference arm is DELETED -----

func TestDriveRun_DispatchedInferenceArmDeleted_AnchorsOnStageTimestamp(t *testing.T) {
	// The behavioral done-means for the inference deletion (plan test (g)): the
	// #1905 "'dispatched' with no spawn evidence -> immediate parked-for-host-
	// dispatch handoff" arm is GONE. Post-#1912 'dispatched' means a spawn attempt
	// EXISTS, so a 'dispatched' stage this invocation did not spawn is classified
	// PURELY on the runner-liveness threshold, anchored on the stage's own
	// max(updated_at, started_at) — NOT on any inferred no-evidence signal. Two
	// rows prove the classification behavior CHANGED, not just the code:
	//
	//   - FRESH updated_at, NO dispatch row, nil started_at: under the old inference
	//     arm this was an IMMEDIATE dispatched_stale handoff; now it reads LIVE and
	//     POLLS. This is the shipped-behavior change the deletion produced.
	//   - STALE updated_at past the threshold: stops dispatched_stale via the
	//     surviving liveness arm (anchored on updated_at), NOT the deleted inference
	//     arm — and never auto-spawns.
	t.Run("fresh_updated_at_no_row_polls", func(t *testing.T) {
		impl := stg(driveImplID, "implement", "dispatched", 1)
		impl.UpdatedAt = time.Now() // fresh spawn timestamp (the marker flip stamped it)
		f := newDriveFake("running", []Stage{
			stg(drivePlanID, "plan", "succeeded", 0),
			impl,
		})
		// Deliberately NO dispatch row and nil StartedAt — the exact shape the
		// deleted inference arm treated as an immediate handoff.
		f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
		rec := &spawnRecorder{}
		r, srv := newDriveResolver(t, f, rec)
		defer srv.Close()
		r.driveDispatchedStaleAfter = 10 * time.Second // fresh anchor < threshold -> live
		r.driveMaxWallclock = 40 * time.Millisecond    // never advances -> times out while polling

		_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
		if err != nil {
			t.Fatalf("driveRun: %v", err)
		}
		if out.StoppedReason != stoppedTimeout {
			t.Fatalf("stopped_reason = %q, want timeout — a fresh-updated_at 'dispatched' stage is LIVE now (the inference-arm handoff is deleted), so it polls", out.StoppedReason)
		}
		if got := rec.list(); len(got) != 0 {
			t.Fatalf("driver spawned a fresh 'dispatched' stage: %v (must never auto-spawn)", got)
		}
	})

	t.Run("stale_updated_at_stops_dispatched_stale", func(t *testing.T) {
		impl := stg(driveImplID, "implement", "dispatched", 1)
		impl.UpdatedAt = time.Now().Add(-time.Hour) // past the threshold: a dead runner
		f := newDriveFake("running", []Stage{
			stg(drivePlanID, "plan", "succeeded", 0),
			impl,
		})
		f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
		rec := &spawnRecorder{}
		r, srv := newDriveResolver(t, f, rec)
		defer srv.Close()
		r.driveDispatchedStaleAfter = time.Millisecond // the hour-old anchor is past it
		r.driveMaxWallclock = 5 * time.Second
		// #1955: UNKNOWN probe -> the anchor-past-threshold path keeps its manual
		// dispatched_stale stop (this test pins the anchor classification, not the
		// probe branches).
		r.driveProbeRunnerLiveness = func(context.Context, string) runnerLivenessVerdict { return runnerUnknown }

		_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
		if err != nil {
			t.Fatalf("driveRun: %v", err)
		}
		if out.StoppedReason != stoppedDispatchedStale {
			t.Fatalf("stopped_reason = %q, want dispatched_stale (stale updated_at anchor, dead runner)", out.StoppedReason)
		}
		if got := rec.list(); len(got) != 0 {
			t.Fatalf("driver spawned a stale 'dispatched' stage: %v (must never auto-spawn)", got)
		}
		// The rewritten message drops the parked-for-host-dispatch handoff framing
		// and names the dead-runner case (a spawn attempt existed).
		var msg string
		for _, w := range out.Warnings {
			if strings.Contains(w, "runner-liveness threshold") {
				msg = w
			}
		}
		if msg == "" {
			t.Fatalf("no dispatched_stale warning; warnings: %v", out.Warnings)
		}
		for _, want := range []string{
			"the spawned runner never reached its prompt fetch",
			"re-dispatch by hand with fishhawk_dispatch_stage",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("stale message missing %q; got: %q", want, msg)
			}
		}
		// The message must NOT resurrect the deleted parked-for-host-dispatch
		// handoff framing.
		if strings.Contains(msg, "plan approval parks") || strings.Contains(msg, "no spawn evidence") {
			t.Errorf("stale message resurrects the deleted inference-arm framing: %q", msg)
		}
		if out.NextActions == nil || len(out.NextActions.Actions) == 0 || out.NextActions.Actions[0].Action != "fishhawk_dispatch_stage" {
			t.Errorf("next_actions[0] should be fishhawk_dispatch_stage; got %+v", out.NextActions)
		}
	})
}

// --- (g2) the dispatch-row timestamp is NO LONGER a staleness anchor ----------

func TestDriveRun_DispatchRowNoLongerAnchorsStaleness(t *testing.T) {
	// The #1905 dispatch-row timestamp max-in is REMOVED (#1912): staleness anchors
	// PURELY on the stage's own max(updated_at, started_at). A stale-updated_at
	// 'dispatched' stage with a FRESH dispatch-evidence row therefore now STOPS
	// dispatched_stale — the fresh row no longer rescues it (it is attribution
	// only). Asserted for BOTH source values, since the row match itself stays
	// source-agnostic for the retry/fixup attribution it still feeds.
	for _, source := range []string{"fishhawk_drive_run", "fishhawk_dispatch_stage"} {
		t.Run(source, func(t *testing.T) {
			impl := stg(driveImplID, "implement", "dispatched", 1)
			impl.UpdatedAt = time.Now().Add(-time.Hour) // stale; the fresh row must NOT rescue it now
			f := newDriveFake("running", []Stage{
				stg(drivePlanID, "plan", "succeeded", 0),
				impl,
			})
			// A FRESH dispatch-evidence row — under the OLD model this reset the
			// anchor and made it poll; post-#1912 it no longer does.
			f.appendAuto(map[string]any{"act": "dispatch", "action": "dispatch_stage", "stage": "implement", "source": source, "note": ""})
			f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
			rec := &spawnRecorder{}
			r, srv := newDriveResolver(t, f, rec)
			defer srv.Close()
			r.driveDispatchedStaleAfter = time.Millisecond // the hour-old updated_at anchor is past it
			r.driveMaxWallclock = 5 * time.Second
			// #1955: UNKNOWN probe -> the stale anchor still stops dispatched_stale
			// (this test pins that a fresh dispatch row no longer anchors staleness).
			r.driveProbeRunnerLiveness = func(context.Context, string) runnerLivenessVerdict { return runnerUnknown }

			_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
			if err != nil {
				t.Fatalf("driveRun: %v", err)
			}
			if out.StoppedReason != stoppedDispatchedStale {
				t.Fatalf("stopped_reason = %q, want dispatched_stale (a fresh %s dispatch row no longer anchors staleness)", out.StoppedReason, source)
			}
			if got := rec.list(); len(got) != 0 {
				t.Fatalf("driver spawned a stale 'dispatched' stage with a fresh %s row: %v", source, got)
			}
		})
	}
}

// --- (2) DONE-MEANS: a parked implement (awaiting_host_dispatch) auto-dispatches

func TestDriveRun_AwaitingHostDispatch_AutoDispatched(t *testing.T) {
	// The primary #1912 done-means (plan test 2): a run whose implement stage is
	// awaiting_host_dispatch after a delegated plan approval is AUTO-DISPATCHED by
	// the loop — record-act, the host-dispatch marker call, then exactly ONE
	// spawn, with NO manual handoff and NO dispatched_stale stop. It fails on any
	// no-op touch of driveDispatchableStage (a parked implement that is not
	// spawnable would never reach the spawn seam).
	impl := stg(driveImplID, "implement", "awaiting_host_dispatch", 1)
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		impl,
	})
	f.onSpawn = func(f *driveFakeBackend, typ string) {
		if typ == "implement" {
			f.setState("implement", "running") // the spawned runner reached its prompt fetch
		}
	}
	converge := 0
	f.onStages = func(f *driveFakeBackend) {
		converge++
		if converge >= 2 && f.stateOf("implement") == "running" {
			f.setState("implement", "succeeded")
			f.runState = "succeeded"
		}
	}
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()
	r.driveMaxWallclock = 5 * time.Second

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != stoppedMerged {
		t.Fatalf("stopped_reason = %q, want merged (the parked implement auto-dispatched and settled); no dispatched_stale", out.StoppedReason)
	}
	if got := rec.list(); len(got) != 1 || got[0] != "implement" {
		t.Fatalf("spawned %v, want exactly [implement] (one auto-dispatch of the parked stage)", got)
	}
	f.mu.Lock()
	marker := f.hostDispatchCalledN
	acts := append([]RecordAutoDriveAct(nil), f.recordedActs...)
	f.mu.Unlock()
	if marker != 1 {
		t.Errorf("host-dispatch marker called %d times, want 1 (once, before the spawn)", marker)
	}
	if len(acts) != 1 || acts[0].Stage != "implement" {
		t.Errorf("recorded acts = %+v, want exactly one implement dispatch (record-before-spawn)", acts)
	}
}

// --- (c) DONE-MEANS: the host-dispatch marker fails closed -> zero spawns ------

func TestDriveRun_HostDispatchMarkerFails_NoSpawn(t *testing.T) {
	// Fail-closed (plan test c) for the driver: a 4xx/transport error from the
	// host-dispatch marker between record-act and spawn means NO runner is spawned
	// (an unmarked spawn would recreate the ambiguity #1912 removes). The record-
	// act still landed (the marker is after it), but the recording spawner saw
	// nothing, and the stop is the distinct host_dispatch_failed.
	impl := stg(driveImplID, "implement", "awaiting_host_dispatch", 1)
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		impl,
	})
	f.hostDispatchStatus = http.StatusConflict // the marker 4xx -> fail closed
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()
	r.driveMaxWallclock = 5 * time.Second

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != stoppedHostDispatchFailed {
		t.Fatalf("stopped_reason = %q, want host_dispatch_failed (fail-closed marker)", out.StoppedReason)
	}
	if got := rec.list(); len(got) != 0 {
		t.Fatalf("driver spawned despite a failed host-dispatch marker: %v (must fail closed)", got)
	}
	f.mu.Lock()
	marker := f.hostDispatchCalledN
	f.mu.Unlock()
	if marker != 1 {
		t.Errorf("host-dispatch marker called %d times, want 1 (attempted once, then fail-closed)", marker)
	}
}

// --- (T8b) evidence-bearing 'dispatched' with a ZERO anchor -> degrade to poll

func TestDriveRun_DispatchedZeroAnchor_DegradesToPolling(t *testing.T) {
	// The zero-value-anchor degrade branch (drive_run.go: `!anchor.IsZero() && ...`).
	// The 'dispatched' stage carries no usable spawn timestamp: UpdatedAt is the
	// zero value (stg() leaves it unset) and StartedAt is nil. The anchor
	// max(UpdatedAt, StartedAt) is therefore zero, and both drive_run.go's comment
	// and the README promise "A zero-value anchor degrades to polling (fail toward
	// polling, never toward a stale stop or a spawn)". A regression that dropped
	// the !anchor.IsZero() guard would make time.Since(zero) enormous and trip an
	// instant stale stop (or worse, reorder the branch toward a spawn) — this test
	// fails loudly if it does.
	impl := stg(driveImplID, "implement", "dispatched", 1) // UpdatedAt zero, StartedAt nil by default
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		impl,
	})
	// A dispatch-evidence row (attribution only, post-#1912) with a ZERO Timestamp:
	// it no longer feeds the anchor, so the anchor stays zero.
	f.appendAutoAt(map[string]any{"act": "dispatch", "action": "dispatch_stage", "stage": "implement", "source": "fishhawk_drive_run", "note": ""}, time.Time{})
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()
	r.driveDispatchedStaleAfter = time.Millisecond // tiny: a NON-zero anchor would trip instantly; the zero anchor must NOT
	r.driveMaxWallclock = 40 * time.Millisecond    // degrade-to-poll -> polls to the deadline

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != stoppedTimeout {
		t.Fatalf("stopped_reason = %q, want timeout (zero anchor degrades to polling, never a stale stop)", out.StoppedReason)
	}
	if got := rec.list(); len(got) != 0 {
		t.Fatalf("driver spawned a zero-anchor 'dispatched' stage: %v (must never auto-spawn)", got)
	}
	f.mu.Lock()
	nActs := len(f.recordedActs)
	f.mu.Unlock()
	if nActs != 0 {
		t.Errorf("driver recorded %d acts on a zero-anchor degrade; want 0", nActs)
	}
}

// --- (T11) MANDATED end-to-end stale-recovery convergence -------------------

func TestDriveRun_StaleRecoveryConvergence_EndToEnd(t *testing.T) {
	// The recovery-convergence contract: chain fishhawk_dispatch_stage and
	// fishhawk_drive_run against ONE shared stateful fake. A first driveRun on a
	// stage sat 'dispatched' past the threshold (stale updated_at) returns
	// dispatched_stale; a manual dispatchStage then re-spawns a fresh runner (and
	// records an attribution act row); a re-invoked driveRun must NOT re-report
	// stale — the fresh runner flips the stage dispatched->running (#1912/#1924),
	// which the loop reads as in-flight and polls, settling cleanly once the fake
	// advances running->succeeded. This is the recovery loop CONVERGING rather than
	// insta-tripping stale on the manual verb's own recommended recovery.
	impl := stg(driveImplID, "implement", "dispatched", 1)
	impl.UpdatedAt = time.Now().Add(-time.Hour)
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		impl,
	})
	// Old driver-sourced spawn evidence (past the threshold): the first drive is
	// genuinely stale.
	f.appendAutoAt(map[string]any{"act": "dispatch", "action": "dispatch_stage", "stage": "implement", "source": "fishhawk_drive_run", "note": ""}, time.Now().Add(-time.Hour))
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()
	// Large enough that a FRESH manual row reads live, small enough that the
	// hour-old evidence is past it.
	r.driveDispatchedStaleAfter = 10 * time.Second
	// #1955: an UNKNOWN probe keeps drive #1 on the manual-recovery path this test
	// exercises (dispatched_stale -> manual re-dispatch -> converge). Drive #2's
	// onStages flips the stage to 'running' before the guard is reached, so the
	// same injected seam never fires there.
	r.driveProbeRunnerLiveness = func(context.Context, string) runnerLivenessVerdict { return runnerUnknown }

	// (1) First drive: genuinely stale + ambiguous probe -> dispatched_stale.
	_, out1, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun #1: %v", err)
	}
	if out1.StoppedReason != stoppedDispatchedStale {
		t.Fatalf("driveRun #1 stopped_reason = %q, want dispatched_stale", out1.StoppedReason)
	}

	// (2) Manual re-dispatch: records a FRESH act row into the shared audit log
	// (spawn stubbed via runStageCommand / runStageLookPath). This is the manual
	// recovery the first drive recommended.
	origCmd := runStageCommand
	runStageCommand = func(_ string, _ ...string) *exec.Cmd { return exec.Command("sh", "-c", "exit 0") }
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() { runStageCommand = origCmd })
	if _, _, derr := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID: f.runID.String(), Workflow: "feature_change", Stage: "implement",
		GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
	}); derr != nil {
		t.Fatalf("dispatchStage: %v", derr)
	}
	// The manual verb landed exactly one FRESH act row into the shared log.
	var manualRows int
	for _, m := range f.autoRows() {
		if m["source"] == "fishhawk_dispatch_stage" {
			manualRows++
		}
	}
	if manualRows != 1 {
		t.Fatalf("manual dispatch recorded %d fishhawk_dispatch_stage rows, want 1", manualRows)
	}

	// Advance the stage as the fresh runner from the manual re-dispatch would.
	// Post-#1912 the convergence mechanism is the runner's prompt-fetch flip
	// dispatched->running (#1924): the fresh runner reaches its prompt fetch and
	// the backend flips the stage to 'running', so drive #2 reads it as in-flight
	// and polls rather than re-reporting stale (it never re-anchors off a stale
	// updated_at). It then advances running -> succeeded (with the run settling) —
	// the convergence tail.
	converge := 0
	f.onStages = func(f *driveFakeBackend) {
		converge++
		switch converge {
		case 1:
			// The fresh runner reached its prompt fetch: dispatched -> running.
			f.setState("implement", "running")
		case 2:
			f.setState("implement", "succeeded")
			f.runState = "succeeded"
		}
	}

	// (3) Re-invoked drive: must NOT re-report stale — the fresh manual row reset
	// the anchor. It polls the now-live stage and settles.
	_, out2, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun #2: %v", err)
	}
	if out2.StoppedReason == stoppedDispatchedStale {
		t.Fatalf("driveRun #2 re-reported dispatched_stale after a fresh manual re-dispatch — the recovery loop did NOT converge")
	}
	if out2.StoppedReason != stoppedMerged {
		t.Fatalf("driveRun #2 stopped_reason = %q, want merged (polled to convergence)", out2.StoppedReason)
	}
	if got := rec.list(); len(got) != 0 {
		t.Errorf("re-invoked drive spawned a stage it did not own: %v (must never auto-spawn)", got)
	}
}

// --- (T12) StartedAt is spawn evidence AND a valid staleness anchor ----------

func TestDriveRun_StartedAtStalenessAnchor(t *testing.T) {
	// Direct coverage for the StartedAt path in the dispatched-guard staleness
	// anchor (drive_run.go): the `disp.StartedAt.After(anchor)` max over
	// max(UpdatedAt, StartedAt). The zero-anchor and stale-updated_at cases drive
	// the UpdatedAt path — these exercise StartedAt as the freshest anchor.

	t.Run("fresh_started_at_no_dispatch_row_polls", func(t *testing.T) {
		// A 'dispatched' stage with a STALE UpdatedAt, a FRESH StartedAt, and NO
		// dispatch row. StartedAt is the freshest anchor (so the stale-threshold
		// stop must NOT fire): the driver must POLL. A regression dropping StartedAt
		// from the anchor max would trip the stale threshold off the hour-old
		// UpdatedAt — this timeout fails if it does.
		started := time.Now()
		impl := stg(driveImplID, "implement", "dispatched", 1)
		impl.UpdatedAt = time.Now().Add(-time.Hour)
		impl.StartedAt = &started
		f := newDriveFake("running", []Stage{
			stg(drivePlanID, "plan", "succeeded", 0),
			impl,
		})
		// Deliberately NO dispatch row: StartedAt is the only spawn evidence.
		f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
		rec := &spawnRecorder{}
		r, srv := newDriveResolver(t, f, rec)
		defer srv.Close()
		r.driveDispatchedStaleAfter = 10 * time.Second // fresh StartedAt < threshold -> live
		r.driveMaxWallclock = 40 * time.Millisecond    // never advances -> times out while polling

		_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
		if err != nil {
			t.Fatalf("driveRun: %v", err)
		}
		if out.StoppedReason != stoppedTimeout {
			t.Fatalf("stopped_reason = %q, want timeout (fresh StartedAt is live spawn evidence, so it polls)", out.StoppedReason)
		}
		if got := rec.list(); len(got) != 0 {
			t.Fatalf("driver spawned a stage whose fresh StartedAt marks it live: %v (must never auto-spawn)", got)
		}
		if n := len(f.autoRows()); n != 0 {
			t.Errorf("run_auto_driven rows = %d, want 0 (no dispatch row seeded, no driver re-record)", n)
		}
	})

	t.Run("started_at_wins_over_older_updated_at", func(t *testing.T) {
		// StartedAt must win the anchor max when it is newer than UpdatedAt. Old
		// UpdatedAt (and an old attribution dispatch row, which post-#1912 no longer
		// anchors), but a FRESH StartedAt: the anchor resolves to StartedAt and the
		// stage reads live -> polls. A regression that omitted StartedAt from the
		// max would take the older UpdatedAt anchor and stop dispatched_stale.
		started := time.Now()
		impl := stg(driveImplID, "implement", "dispatched", 1)
		impl.UpdatedAt = time.Now().Add(-time.Hour)
		impl.StartedAt = &started
		f := newDriveFake("running", []Stage{
			stg(drivePlanID, "plan", "succeeded", 0),
			impl,
		})
		// An OLD dispatch-evidence row (older than StartedAt): StartedAt must still win.
		f.appendAutoAt(map[string]any{"act": "dispatch", "action": "dispatch_stage", "stage": "implement", "source": "fishhawk_drive_run", "note": ""}, time.Now().Add(-time.Hour))
		f.onGate = func(f *driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} }
		rec := &spawnRecorder{}
		r, srv := newDriveResolver(t, f, rec)
		defer srv.Close()
		r.driveDispatchedStaleAfter = 10 * time.Second
		r.driveMaxWallclock = 40 * time.Millisecond

		_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
		if err != nil {
			t.Fatalf("driveRun: %v", err)
		}
		if out.StoppedReason != stoppedTimeout {
			t.Fatalf("stopped_reason = %q, want timeout (fresh StartedAt wins over the older audit-row anchor)", out.StoppedReason)
		}
		if got := rec.list(); len(got) != 0 {
			t.Fatalf("driver spawned a stage StartedAt marks live: %v (must never auto-spawn)", got)
		}
	})
}

// --- (#1928) acceptance-dispatch admission in the drive loop ------------------

// TestDriveRun_AcceptanceShortCircuit_NoSpawn_ContinuesToMerge: when the drive
// loop reaches a dispatchable acceptance stage whose admission short-circuits,
// it records NO act, spawns NO runner, appends a short-circuit DriveStep, and
// the loop continues to merge after the stage settles server-side.
func TestDriveRun_AcceptanceShortCircuit_NoSpawn_ContinuesToMerge(t *testing.T) {
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		stg(driveImplID, "implement", "succeeded", 1),
		stg(driveAccID, "acceptance", "pending", 2),
	})
	f.admissionShortCircuit = true
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome {
		if f.allSucceeded("plan", "implement", "acceptance") {
			f.runState = "succeeded" // webhook-settled on the next poll
			return AutoDriveOutcome{Acted: true, Action: "merge", Note: "enabled auto-merge"}
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
		t.Fatalf("stopped_reason = %q, want merged:\n%+v", out.StoppedReason, out)
	}
	// NO acceptance runner spawned.
	for _, typ := range rec.list() {
		if typ == "acceptance" {
			t.Errorf("a short-circuited acceptance stage must NOT spawn a runner; spawned: %v", rec.list())
		}
	}
	// NO acceptance dispatch act recorded.
	f.mu.Lock()
	acts := append([]RecordAutoDriveAct(nil), f.recordedActs...)
	admissionN := f.admissionCalledN
	f.mu.Unlock()
	for _, a := range acts {
		if a.Stage == "acceptance" {
			t.Errorf("a short-circuited acceptance stage must record NO dispatch act; acts: %+v", acts)
		}
	}
	if admissionN < 1 {
		t.Errorf("admission endpoint calls = %d, want >= 1", admissionN)
	}
	// A short-circuit DriveStep was appended.
	var noted bool
	for _, s := range out.StepsTaken {
		if s.Kind == "dispatch" && s.Stage == "acceptance" && strings.Contains(s.Note, "short-circuited server-side") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("missing the acceptance short-circuit DriveStep; steps: %+v", out.StepsTaken)
	}
}

// TestDriveRun_AcceptanceNeedsTarget_StopsResumable (#1953): a needs_target
// admission whose declared target is unreachable STOPS the drive with
// acceptance_needs_target — no record-act, no spawn — leaving the stage
// awaiting_host_dispatch so a re-invocation dispatches cleanly.
func TestDriveRun_AcceptanceNeedsTarget_StopsResumable(t *testing.T) {
	origAttempts := acceptanceQuickProbeAttempts
	acceptanceQuickProbeAttempts = 1
	t.Cleanup(func() { acceptanceQuickProbeAttempts = origAttempts })

	target := healthzServer(t, http.StatusOK, `{"git_sha":"abc1234def"}`)
	targetHost := hostOf(target.URL)
	target.Close()

	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		stg(driveImplID, "implement", "succeeded", 1),
		stg(driveAccID, "acceptance", "pending", 2),
	})
	f.admissionNeedsTarget = true
	f.admissionTargetHosts = []string{targetHost}
	f.admissionExpectedHeadSHA = probeExpectedSHA
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome {
		return AutoDriveOutcome{Note: "observe-only"}
	}
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != stoppedAcceptanceNeedsTarget {
		t.Fatalf("stopped_reason = %q, want acceptance_needs_target:\n%+v", out.StoppedReason, out)
	}
	// No acceptance runner spawned.
	for _, typ := range rec.list() {
		if typ == "acceptance" {
			t.Errorf("a needs_target refusal must NOT spawn a runner; spawned: %v", rec.list())
		}
	}
	// No acceptance dispatch act recorded.
	f.mu.Lock()
	acts := append([]RecordAutoDriveAct(nil), f.recordedActs...)
	f.mu.Unlock()
	for _, a := range acts {
		if a.Stage == "acceptance" {
			t.Errorf("a needs_target refusal must record NO dispatch act; acts: %+v", acts)
		}
	}
	// A DriveStep note names the host + head SHA.
	var noted bool
	for _, s := range out.StepsTaken {
		if s.Kind == "dispatch" && s.Stage == "acceptance" &&
			strings.Contains(s.Note, targetHost) && strings.Contains(s.Note, probeExpectedSHA) {
			noted = true
		}
	}
	if !noted {
		t.Errorf("missing the needs_target DriveStep naming host+SHA; steps: %+v", out.StepsTaken)
	}
	if out.NextActions == nil || out.NextActions.State != stoppedAcceptanceNeedsTarget {
		t.Errorf("NextActions = %+v, want state=%s", out.NextActions, stoppedAcceptanceNeedsTarget)
	}
}

// TestDriveRun_AcceptanceNeedsTargetVerified_Dispatches (#1953): a needs_target
// admission whose target is VERIFIED proceeds to record + spawn the acceptance
// stage exactly as today.
func TestDriveRun_AcceptanceNeedsTargetVerified_Dispatches(t *testing.T) {
	target := healthzServer(t, http.StatusOK, `{"git_sha":"abc1234def"}`)

	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		stg(driveImplID, "implement", "succeeded", 1),
		stg(driveAccID, "acceptance", "pending", 2),
	})
	f.admissionNeedsTarget = true
	f.admissionTargetHosts = []string{hostOf(target.URL)}
	f.admissionExpectedHeadSHA = probeExpectedSHA
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome {
		if f.allSucceeded("plan", "implement", "acceptance") {
			f.runState = "succeeded"
			return AutoDriveOutcome{Acted: true, Action: "merge", Note: "enabled auto-merge"}
		}
		return AutoDriveOutcome{Note: "observe-only"}
	}
	// Advance the acceptance stage to succeeded once its runner spawns, so the
	// loop can proceed to merge.
	f.onSpawn = func(f *driveFakeBackend, stageType string) {
		if stageType == "acceptance" {
			f.setState("acceptance", "succeeded")
		}
	}
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason == stoppedAcceptanceNeedsTarget {
		t.Fatalf("a verified target must dispatch, got acceptance_needs_target:\n%+v", out)
	}
	var spawnedAcc bool
	for _, typ := range rec.list() {
		if typ == "acceptance" {
			spawnedAcc = true
		}
	}
	if !spawnedAcc {
		t.Errorf("a verified target must spawn the acceptance runner; spawned: %v", rec.list())
	}
}

// TestDriveRun_AcceptanceAdmissionError_FailsOpen: an admission error appends a
// warning and the drive falls through to today's record + spawn for the
// acceptance stage.
func TestDriveRun_AcceptanceAdmissionError_FailsOpen(t *testing.T) {
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		stg(driveImplID, "implement", "succeeded", 1),
		stg(driveAccID, "acceptance", "pending", 2),
	})
	f.admissionStatus = http.StatusInternalServerError
	f.onSpawn = func(f *driveFakeBackend, typ string) {
		if typ == "acceptance" {
			f.setState("acceptance", "succeeded")
		}
	}
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome {
		if f.allSucceeded("plan", "implement", "acceptance") {
			f.runState = "succeeded"
			return AutoDriveOutcome{Acted: true, Action: "merge", Note: "enabled auto-merge"}
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
	// The acceptance stage spawned as today (fail-open).
	var spawnedAcceptance bool
	for _, typ := range rec.list() {
		if typ == "acceptance" {
			spawnedAcceptance = true
		}
	}
	if !spawnedAcceptance {
		t.Errorf("an admission error must fall through to spawn the acceptance stage; spawned: %v", rec.list())
	}
	var warned bool
	for _, w := range out.Warnings {
		if strings.Contains(w, "acceptance-admission pre-check failed") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("missing the fail-open admission warning; warnings: %v", out.Warnings)
	}
}

// TestDriveRun_AcceptanceAdmissionAuthzRejection_FailsClosed pins the #1928 authz
// concern for the drive verb: a 4xx admission rejection (403 cross_run_admission)
// is NOT fail-open — the drive HALTS with an error and spawns NO acceptance runner
// rather than proceed after the run-subject authorization boundary rejected it.
func TestDriveRun_AcceptanceAdmissionAuthzRejection_FailsClosed(t *testing.T) {
	f := newDriveFake("running", []Stage{
		stg(drivePlanID, "plan", "succeeded", 0),
		stg(driveImplID, "implement", "succeeded", 1),
		stg(driveAccID, "acceptance", "pending", 2),
	})
	f.admissionStatus = http.StatusForbidden
	f.onGate = func(f *driveFakeBackend) AutoDriveOutcome {
		return AutoDriveOutcome{Note: "observe-only"}
	}
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()

	_, _, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err == nil {
		t.Fatal("a 4xx admission rejection must halt the drive with an error, got nil")
	}
	if !strings.Contains(err.Error(), "rejected the dispatch") {
		t.Errorf("error = %q, want it to name the admission rejection", err)
	}
	for _, typ := range rec.list() {
		if typ == "acceptance" {
			t.Errorf("a fail-closed admission rejection must NOT spawn the acceptance runner; spawned: %v", rec.list())
		}
	}
	f.mu.Lock()
	acts := append([]RecordAutoDriveAct(nil), f.recordedActs...)
	f.mu.Unlock()
	for _, a := range acts {
		if a.Stage == "acceptance" {
			t.Errorf("a fail-closed admission rejection must record NO acceptance dispatch act; acts: %+v", acts)
		}
	}
}
