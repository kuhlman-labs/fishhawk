package mcpserver

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The per-verb tests pin #5095: every local spawn path passes the stage row's
// executor as --agent, refuses a value outside the closed provider set before
// committing anything, and keeps the runner default for an empty ref.

const unsupportedAgent = "not-a-provider"

// agentRouteCases are the row executors every verb pins: codex, the claude-code
// control, and the empty-ref absent-flag control.
var agentRouteCases = []struct {
	name     string
	executor StageExecutor
	want     string // "" means no --agent flag at all
}{
	{"codex", StageExecutor{Kind: "agent", Ref: "codex"}, "codex"},
	{"claude-code", StageExecutor{Kind: "agent", Ref: "claude-code"}, "claude-code"},
	{"empty ref keeps the runner default", StageExecutor{Kind: "agent"}, ""},
}

// assertAgentFlag asserts argv carries exactly one --agent with the wanted value,
// or none when want is empty.
func assertAgentFlag(t *testing.T, argv []string, want string) {
	t.Helper()
	count := 0
	for _, a := range argv {
		if a == "--agent" {
			count++
		}
	}
	if want == "" {
		if count != 0 {
			t.Fatalf("argv carries --agent for an unset provider: %v", argv)
		}
		return
	}
	if count != 1 || !argvHasPair(argv, "--agent", want) {
		t.Fatalf("argv should carry exactly one --agent %s: %v", want, argv)
	}
}

// assertAgentRefusal asserts the refusal names the value and the remedy, and does
// not send the operator to the run's stored workflow.
func assertAgentRefusal(t *testing.T, msg string) {
	t.Helper()
	for _, want := range []string{"unsupported_executor_agent", unsupportedAgent, "claude-code", "codex", "NOT spawning", "workflow source", "fishhawk-runner directly with --agent"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal should name %q: %s", want, msg)
		}
	}
	for _, reject := range []string{"cached", "stored", "edit "} {
		if strings.Contains(msg, reject) {
			t.Errorf("refusal must not direct the operator at the stored workflow (%q): %s", reject, msg)
		}
	}
}

func TestStageRunnerAgent(t *testing.T) {
	runID, stageID := uuid.NewString(), uuid.NewString()
	tests := []struct {
		name    string
		ex      StageExecutor
		want    string
		refused string // the value named in the refusal; "" means no refusal
	}{
		{name: "codex", ex: StageExecutor{Kind: "agent", Ref: "codex"}, want: "codex"},
		{name: "claude-code", ex: StageExecutor{Kind: "agent", Ref: "claude-code"}, want: "claude-code"},
		{name: "claudecode alias", ex: StageExecutor{Kind: "agent", Ref: "claudecode"}, want: "claude-code"},
		{name: "surrounding whitespace", ex: StageExecutor{Kind: "agent", Ref: "  codex\n"}, want: "codex"},
		{name: "empty ref", ex: StageExecutor{Kind: "agent"}, want: ""},
		{name: "whitespace-only ref", ex: StageExecutor{Kind: "agent", Ref: "  "}, want: ""},
		{name: "human kind", ex: StageExecutor{Kind: "human", Ref: "human"}, want: ""},
		{name: "absent kind", ex: StageExecutor{Ref: unsupportedAgent}, want: ""},
		{name: "unknown", ex: StageExecutor{Kind: "agent", Ref: unsupportedAgent}, refused: unsupportedAgent},
		{name: "bare claude is not an alias", ex: StageExecutor{Kind: "agent", Ref: "claude"}, refused: "claude"},
		{name: "the set is case-sensitive", ex: StageExecutor{Kind: "agent", Ref: "Codex"}, refused: "Codex"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := stageRunnerAgent(runID, stageID, tt.ex)
			if tt.refused == "" {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
				if got != tt.want {
					t.Fatalf("agent = %q, want %q", got, tt.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("agent = %q; want an unsupported_executor_agent refusal", got)
			}
			if got != "" {
				t.Errorf("a refusal must return no agent, got %q", got)
			}
			for _, want := range []string{"unsupported_executor_agent", runID, stageID, `"` + tt.refused + `"`, "claude-code", "codex"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal should name %q: %v", want, err)
				}
			}
		})
	}
}

func TestRunnerAgentArgv(t *testing.T) {
	if got := runnerAgentArgv(""); got != nil {
		t.Errorf("runnerAgentArgv(\"\") = %v, want nil", got)
	}
	if got := runnerAgentArgv("codex"); !reflect.DeepEqual(got, []string{"--agent", "codex"}) {
		t.Errorf("runnerAgentArgv(codex) = %v", got)
	}
}

// --- fishhawk_run_stage ---

func TestRunStage_RoutesStageExecutorAgent(t *testing.T) {
	for _, tt := range agentRouteCases {
		t.Run(tt.name, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			r := newResolver(srv, nil)
			calls := captureAllArgv(t)

			runID, stageID := uuid.New(), uuid.New()
			seedStages(fb, runID, Stage{ID: stageID.String(), RunID: runID.String(), Type: "plan", State: "pending", Executor: tt.executor})

			if _, _, err := r.runStage(context.Background(), nil, RunStageInput{
				RunID: runID.String(), Workflow: "w", Stage: "plan", WorkingDir: "/tmp/checkout", GitHubRepo: "x/y", BaseBranch: "main",
			}); err != nil {
				t.Fatalf("runStage: %v", err)
			}
			if len(*calls) != 1 {
				t.Fatalf("spawn count = %d, want 1", len(*calls))
			}
			assertAgentFlag(t, (*calls)[0], tt.want)
		})
	}
}

func TestRunStage_UnsupportedExecutorAgent_RefusesBeforeMarker(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	calls := captureAllArgv(t)

	runID, stageID := uuid.New(), uuid.New()
	seedStages(fb, runID, Stage{ID: stageID.String(), RunID: runID.String(), Type: "implement", State: "awaiting_host_dispatch",
		Executor: StageExecutor{Kind: "agent", Ref: unsupportedAgent}})

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID: runID.String(), Workflow: "w", Stage: "implement", WorkingDir: "/tmp/checkout", GitHubRepo: "x/y", BaseBranch: "main",
	})
	if err == nil {
		t.Fatal("expected an unsupported_executor_agent refusal")
	}
	assertAgentRefusal(t, err.Error())
	if len(*calls) != 0 {
		t.Fatalf("runner spawned despite the refusal: %v", *calls)
	}
	fb.mu.Lock()
	marker, state := fb.hostDispatchCalledByID[stageID], fb.stagesByRun[runID][0].State
	fb.mu.Unlock()
	if marker != 0 {
		t.Fatalf("host-dispatch marker POSTed %d times; the refusal must precede it", marker)
	}
	if state != "awaiting_host_dispatch" {
		t.Fatalf("stage state = %q, want it left parked", state)
	}
}

// TestRunStage_UnsupportedExecutorAgent_AcceptanceShortCircuitSettles pins the
// documented order: run_stage composes after the short-circuit, which settles
// the stage with no runner and so needs no agent.
func TestRunStage_UnsupportedExecutorAgent_AcceptanceShortCircuitSettles(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.admissionShortCircuit = true
	r := newResolver(srv, nil)
	calls := captureAllArgv(t)

	runID, stageID := uuid.New(), uuid.New()
	seedStages(fb, runID, Stage{ID: stageID.String(), RunID: runID.String(), Type: "acceptance", State: "pending",
		Executor: StageExecutor{Kind: "agent", Ref: unsupportedAgent}})

	_, out, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID: runID.String(), Workflow: "w", Stage: "acceptance", WorkingDir: "/tmp/checkout", GitHubRepo: "x/y", BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("a short-circuited stage spawns nothing and must not refuse: %v", err)
	}
	if out.Outcome != "short_circuited" {
		t.Fatalf("outcome = %q, want short_circuited", out.Outcome)
	}
	if len(*calls) != 0 {
		t.Fatalf("runner spawned on a short-circuit: %v", *calls)
	}
}

// --- fishhawk_dispatch_stage ---

// withDispatchSpawn swaps the detached spawn seam for a recorder, so no real
// runner or reaper goroutine is started.
func withDispatchSpawn(t *testing.T) *[][]string {
	t.Helper()
	calls := new([][]string)
	savedSpawn, savedLook := dispatchSpawnDetached, runStageLookPath
	dispatchSpawnDetached = func(_ string, argv, _ []string, _, _ string, _ detachedFailureReporter, _ detachedStageStateProbe) (string, error) {
		*calls = append(*calls, append([]string(nil), argv...))
		return "/tmp/fishhawk-runner.log", nil
	}
	runStageLookPath = func(string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() { dispatchSpawnDetached, runStageLookPath = savedSpawn, savedLook })
	return calls
}

// TestDispatchStage_RoutesStageExecutorAgent covers every stage type dispatch
// spawns, plus awaiting_host_dispatch: the state retry_stage and fixup_stage
// re-dispatch an implement stage from, so both share this path and this row.
func TestDispatchStage_RoutesStageExecutorAgent(t *testing.T) {
	stageRows := []struct {
		name, stage, state string
	}{
		{"plan", "plan", "pending"},
		{"implement", "implement", "pending"},
		{"acceptance", "acceptance", "pending"},
		{"review", "review", "pending"},
		{"implement re-dispatched from awaiting_host_dispatch", "implement", "awaiting_host_dispatch"},
	}
	for _, row := range stageRows {
		for _, tt := range agentRouteCases {
			t.Run(row.name+"/"+tt.name, func(t *testing.T) {
				fb, srv := newFakeBackend(t)
				r := newResolver(srv, nil)
				calls := withDispatchSpawn(t)

				runID, stageID := uuid.New(), uuid.New()
				seedStages(fb, runID, Stage{ID: stageID.String(), RunID: runID.String(), Type: row.stage, State: row.state, Executor: tt.executor})

				if _, _, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
					RunID: runID.String(), Workflow: "w", Stage: row.stage, WorkingDir: "/tmp/checkout", GitHubRepo: "x/y", BaseBranch: "main",
				}); err != nil {
					t.Fatalf("dispatchStage: %v", err)
				}
				if len(*calls) != 1 {
					t.Fatalf("spawn count = %d, want 1", len(*calls))
				}
				assertAgentFlag(t, (*calls)[0], tt.want)
			})
		}
	}
}

// TestDispatchStage_UnsupportedExecutorAgent_RefusesBeforeMarker also pins that
// prepareSpawn refuses before acceptance admission, the order the README states.
func TestDispatchStage_UnsupportedExecutorAgent_RefusesBeforeMarker(t *testing.T) {
	for _, stage := range []string{"implement", "acceptance"} {
		t.Run(stage, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			fb.admissionShortCircuit = true
			r := newResolver(srv, nil)
			calls := withDispatchSpawn(t)

			runID, stageID := uuid.New(), uuid.New()
			seedStages(fb, runID, Stage{ID: stageID.String(), RunID: runID.String(), Type: stage, State: "awaiting_host_dispatch",
				Executor: StageExecutor{Kind: "agent", Ref: unsupportedAgent}})

			_, _, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
				RunID: runID.String(), Workflow: "w", Stage: stage, WorkingDir: "/tmp/checkout", GitHubRepo: "x/y", BaseBranch: "main",
			})
			if err == nil {
				t.Fatal("expected an unsupported_executor_agent refusal")
			}
			assertAgentRefusal(t, err.Error())
			if len(*calls) != 0 {
				t.Fatalf("runner spawned despite the refusal: %v", *calls)
			}
			fb.mu.Lock()
			marker, admission, state := fb.hostDispatchCalledByID[stageID], fb.admissionCalledByID[stageID], fb.stagesByRun[runID][0].State
			fb.mu.Unlock()
			if marker != 0 {
				t.Fatalf("host-dispatch marker POSTed %d times; the refusal must precede it", marker)
			}
			if admission != 0 {
				t.Fatalf("acceptance admission POSTed %d times; prepareSpawn refuses before it", admission)
			}
			if state != "awaiting_host_dispatch" {
				t.Fatalf("stage state = %q, want it left parked", state)
			}
		})
	}
}

// --- fishhawk_drive_run ---

// parkedDriveFake is a local run whose earlier stages succeeded and whose stage
// of the given type is parked for a host dispatch under the given executor.
func parkedDriveFake(stageType string, ex StageExecutor) *driveFakeBackend {
	stages := []Stage{stg(drivePlanID, "plan", "succeeded", 0)}
	var parked Stage
	if stageType == "acceptance" {
		stages = append(stages, stg(driveImplID, "implement", "succeeded", 1))
		parked = stg(driveAccID, stageType, "awaiting_host_dispatch", 2)
	} else {
		parked = stg(driveImplID, stageType, "awaiting_host_dispatch", 1)
	}
	parked.Executor = ex
	f := newDriveFake("running", append(stages, parked))
	f.setOnSpawn(func(f *driveFakeBackend, typ string) {
		f.setState(typ, "succeeded")
		f.runState = "succeeded"
	})
	f.setOnGate(func(*driveFakeBackend) AutoDriveOutcome { return AutoDriveOutcome{Note: "observe-only"} })
	return f
}

func TestDriveRun_RoutesStageExecutorAgent(t *testing.T) {
	for _, tt := range agentRouteCases {
		t.Run(tt.name, func(t *testing.T) {
			f := parkedDriveFake("implement", tt.executor)
			rec := &spawnRecorder{}
			r, srv := newDriveResolver(t, f, rec)
			defer srv.Close()
			r.driveMaxWallclock = 5 * time.Second

			_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
			if err != nil {
				t.Fatalf("driveRun: %v", err)
			}
			if out.StoppedReason != stoppedMerged {
				t.Fatalf("stopped_reason = %q, want merged; warnings=%v", out.StoppedReason, out.Warnings)
			}
			argv := rec.argvFor("implement")
			if argv == nil {
				t.Fatalf("no implement spawn recorded; spawned %v", rec.list())
			}
			assertAgentFlag(t, argv, tt.want)
		})
	}
}

func TestDriveRun_UnsupportedExecutorAgent_StopsBeforeRecordAct(t *testing.T) {
	f := parkedDriveFake("implement", StageExecutor{Kind: "agent", Ref: unsupportedAgent})
	rec := &spawnRecorder{}
	r, srv := newDriveResolver(t, f, rec)
	defer srv.Close()
	r.driveMaxWallclock = 5 * time.Second

	_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if out.StoppedReason != stoppedExecutorAgentUnsupported {
		t.Fatalf("stopped_reason = %q, want %s; warnings=%v", out.StoppedReason, stoppedExecutorAgentUnsupported, out.Warnings)
	}
	assertAgentRefusal(t, strings.Join(out.Warnings, " "))
	if got := rec.list(); len(got) != 0 {
		t.Fatalf("spawned %v; want nothing on a refusal", got)
	}
	if len(out.StepsTaken) != 0 {
		t.Fatalf("steps_taken = %+v; a refusal took no act", out.StepsTaken)
	}
	f.mu.Lock()
	marker, acts, state := f.hostDispatchCalledN, len(f.recordedActs), f.stateOf("implement")
	f.mu.Unlock()
	if marker != 0 || acts != 0 {
		t.Fatalf("marker calls = %d, recorded acts = %d; want 0/0", marker, acts)
	}
	if state != "awaiting_host_dispatch" {
		t.Fatalf("stage state = %q, want it left parked", state)
	}
	if out.NextActions == nil || out.NextActions.State != stoppedExecutorAgentUnsupported {
		t.Fatalf("next_actions = %+v, want state %s", out.NextActions, stoppedExecutorAgentUnsupported)
	}
}

// TestDriveRun_UnsupportedExecutorAgent_AfterAcceptanceAdmission pins the order
// on an acceptance stage: admission runs first, so a short-circuit settles with
// no refusal, while a stage that would spawn refuses before the act.
func TestDriveRun_UnsupportedExecutorAgent_AfterAcceptanceAdmission(t *testing.T) {
	refused := StageExecutor{Kind: "agent", Ref: unsupportedAgent}

	t.Run("a spawning stage refuses after admission", func(t *testing.T) {
		f := parkedDriveFake("acceptance", refused)
		rec := &spawnRecorder{}
		r, srv := newDriveResolver(t, f, rec)
		defer srv.Close()
		r.driveMaxWallclock = 5 * time.Second

		_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
		if err != nil {
			t.Fatalf("driveRun: %v", err)
		}
		if out.StoppedReason != stoppedExecutorAgentUnsupported {
			t.Fatalf("stopped_reason = %q, want %s", out.StoppedReason, stoppedExecutorAgentUnsupported)
		}
		f.mu.Lock()
		admission, marker, acts := f.admissionCalledN, f.hostDispatchCalledN, len(f.recordedActs)
		f.mu.Unlock()
		if admission != 1 || marker != 0 || acts != 0 {
			t.Fatalf("admission = %d, marker = %d, acts = %d; want 1/0/0", admission, marker, acts)
		}
	})

	t.Run("a short-circuit settles without refusing", func(t *testing.T) {
		f := parkedDriveFake("acceptance", refused)
		f.admissionShortCircuit = true
		f.setOnStages(func(f *driveFakeBackend) {
			if f.stateOf("acceptance") == "succeeded" {
				f.runState = "succeeded"
			}
		})
		rec := &spawnRecorder{}
		r, srv := newDriveResolver(t, f, rec)
		defer srv.Close()
		r.driveMaxWallclock = 5 * time.Second

		_, out, err := r.driveRun(context.Background(), nil, DriveRunInput{RunID: f.runID.String(), GitHubRepo: "x/y"})
		if err != nil {
			t.Fatalf("driveRun: %v", err)
		}
		if out.StoppedReason == stoppedExecutorAgentUnsupported {
			t.Fatalf("a short-circuited stage spawns nothing and must not refuse: %v", out.Warnings)
		}
		if got := rec.list(); len(got) != 0 {
			t.Fatalf("spawned %v on a short-circuit", got)
		}
		f.mu.Lock()
		admission, state := f.admissionCalledN, f.stateOf("acceptance")
		f.mu.Unlock()
		if admission != 1 || state != "succeeded" {
			t.Fatalf("admission = %d, stage state = %q; want the short-circuit to have settled the stage", admission, state)
		}
	})
}

// TestDriveRun_StoppedReasonVocabulary_ExecutorAgentUnsupported pins the stop's
// three vocabulary sites and the recovery next_actions point at. The remedy must
// correct the source and use a new run or the direct override, never the stored
// workflow.
func TestDriveRun_StoppedReasonVocabulary_ExecutorAgentUnsupported(t *testing.T) {
	const reason = "executor_agent_unsupported"
	if stoppedExecutorAgentUnsupported != reason {
		t.Fatalf("constant = %q, want %q", stoppedExecutorAgentUnsupported, reason)
	}
	field, ok := reflect.TypeOf(DriveRunOutput{}).FieldByName("StoppedReason")
	if !ok {
		t.Fatal("DriveRunOutput has no StoppedReason field")
	}
	if tag := field.Tag.Get("jsonschema"); !strings.Contains(tag, "| "+reason+" |") {
		t.Errorf("stopped_reason jsonschema tag does not list %s: %s", reason, tag)
	}
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	if !strings.Contains(string(readme), "`"+reason+"`") {
		t.Errorf("README.md's stopped_reason vocabulary does not list %s", reason)
	}

	runID := uuid.New()
	na := driveDecisionActions(stoppedExecutorAgentUnsupported, runID, "/tmp/checkout")
	if na == nil || na.State != reason || len(na.Actions) != 1 {
		t.Fatalf("next_actions = %+v, want one action in state %s", na, reason)
	}
	action := na.Actions[0]
	if action.Action != "fishhawk_cancel_run" || action.Consumes != consumesNone {
		t.Errorf("action = %+v, want fishhawk_cancel_run consuming none", action)
	}
	if action.Params["run_id"] != runID.String() {
		t.Errorf("params = %v, want run_id %s", action.Params, runID)
	}
	for _, want := range []string{"workflow source", "fishhawk_start_run", "fishhawk-runner directly with --agent"} {
		if !strings.Contains(action.Reason, want) {
			t.Errorf("reason should name %q: %s", want, action.Reason)
		}
	}
	for _, reject := range []string{"cached", "stored", "edit "} {
		if strings.Contains(action.Reason, reject) {
			t.Errorf("reason must not direct the operator at the stored workflow (%q): %s", reject, action.Reason)
		}
	}
}

// --- fishhawk_run_children ---

// seedChildWithExecutor seeds one dispatchable child under the given executor and
// returns its implement stage id.
func seedChildWithExecutor(fb *fakeBackend, child uuid.UUID, ex StageExecutor) uuid.UUID {
	seedChildRunStage(fb, child, "running", "pending")
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.stagesByRun[child][0].Executor = ex
	id, err := uuid.Parse(fb.stagesByRun[child][0].ID)
	if err != nil {
		panic(err)
	}
	return id
}

func TestRunChildren_RoutesChildExecutorAgent(t *testing.T) {
	for _, tt := range agentRouteCases {
		t.Run(tt.name, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			r := newResolver(srv, nil)
			parent, child := uuid.New(), uuid.New()
			seedChildWithExecutor(fb, child, tt.executor)
			seedPlanDecomposed(fb, parent, []string{child.String()}, 0)
			spawn := withFakeDetachedSpawn(t)

			_, out, err := r.runChildren(context.Background(), nil, runChildrenIn(parent))
			if err != nil {
				t.Fatalf("runChildren: %v", err)
			}
			if out.DispatchedCount != 1 {
				t.Fatalf("dispatched_count = %d, want 1; children=%+v", out.DispatchedCount, out.Children)
			}
			assertAgentFlag(t, spawn.snapshot()[0].argv, tt.want)
		})
	}

	// The flag follows the forge flags, the position the plan names.
	t.Run("gitlab parent places --agent after the forge flags", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		parent := uuid.New()
		children := seedGitLabDecomposition(fb, parent, "https://gitlab.example.com")
		fb.mu.Lock()
		for _, c := range children {
			fb.stagesByRun[c][0].Executor = StageExecutor{Kind: "agent", Ref: "codex"}
		}
		fb.mu.Unlock()
		spawn := withFakeDetachedSpawn(t)

		in := runChildrenIn(parent)
		in.BaseBranch = "main"
		if _, _, err := r.runChildren(context.Background(), nil, in); err != nil {
			t.Fatalf("runChildren: %v", err)
		}
		calls := spawn.snapshot()
		if len(calls) == 0 {
			t.Fatal("no child spawned")
		}
		for _, c := range calls {
			if joined := strings.Join(c.argv, " "); !strings.Contains(joined, "--gitlab-base-url https://gitlab.example.com --agent codex --base-branch") {
				t.Errorf("--agent should sit right after the forge flags: %v", c.argv)
			}
		}
	})
}

// TestRunChildren_UnsupportedExecutorAgent_SkipsChildOnly runs under a cap of 1
// with the refused child first: a refusal spends no budget, so the sibling still
// dispatches this call.
func TestRunChildren_UnsupportedExecutorAgent_SkipsChildOnly(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent, refused, sibling := uuid.New(), uuid.New(), uuid.New()
	refusedStage := seedChildWithExecutor(fb, refused, StageExecutor{Kind: "agent", Ref: unsupportedAgent})
	siblingStage := seedChildWithExecutor(fb, sibling, StageExecutor{Kind: "agent", Ref: "codex"})
	seedPlanDecomposed(fb, parent, []string{refused.String(), sibling.String()}, 1)
	spawn := withFakeDetachedSpawn(t)

	_, out, err := r.runChildren(context.Background(), nil, runChildrenIn(parent))
	if err != nil {
		t.Fatalf("a per-child refusal is data, not a tool error: %v", err)
	}
	if out.DispatchedCount != 1 {
		t.Fatalf("dispatched_count = %d, want 1; children=%+v", out.DispatchedCount, out.Children)
	}
	byID := childByID(out)
	got := byID[refused.String()]
	if got.Dispatched || got.LogPath != "" {
		t.Errorf("refused child = %+v, want not dispatched", got)
	}
	if got.StageState != "pending" {
		t.Errorf("refused child stage_state = %q, want pending", got.StageState)
	}
	assertAgentRefusal(t, strings.Join(got.Warnings, " "))
	if !byID[sibling.String()].Dispatched {
		t.Errorf("sibling = %+v, want dispatched", byID[sibling.String()])
	}
	calls := spawn.snapshot()
	if len(calls) != 1 || calls[0].runID != sibling.String() {
		t.Fatalf("spawned %v, want only the sibling", spawn.runIDs())
	}
	assertAgentFlag(t, calls[0].argv, "codex")
	fb.mu.Lock()
	refusedMarker, siblingMarker, refusedState := fb.hostDispatchCalledByID[refusedStage], fb.hostDispatchCalledByID[siblingStage], fb.stagesByRun[refused][0].State
	fb.mu.Unlock()
	if refusedMarker != 0 {
		t.Fatalf("refused child's marker POSTed %d times, want 0", refusedMarker)
	}
	if siblingMarker != 1 {
		t.Fatalf("sibling's marker POSTed %d times, want 1", siblingMarker)
	}
	if refusedState != "pending" {
		t.Fatalf("refused child's stage state = %q, want it left parked", refusedState)
	}
}
