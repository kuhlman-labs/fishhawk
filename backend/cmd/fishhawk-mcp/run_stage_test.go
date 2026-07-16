package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// --- helpers ---

// withFakeRunner swaps in a fake fishhawk-runner subprocess. The
// caller supplies a shell snippet that becomes the runner's body.
// Mirrors the CLI's runner_test.go pattern.
func withFakeRunner(t *testing.T, shellBody string) {
	t.Helper()
	origCmd := runStageCommand
	origLook := runStageLookPath
	runStageCommand = func(_ string, args ...string) *exec.Cmd {
		// Append the runner argv after a sentinel so the test body
		// can inspect $@ if it wants.
		cmd := exec.Command("sh", "-c", shellBody, "--")
		cmd.Args = append(cmd.Args, args...)
		return cmd
	}
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() {
		runStageCommand = origCmd
		runStageLookPath = origLook
	})
}

// withFakeRunnerMissing makes the runner binary appear absent.
func withFakeRunnerMissing(t *testing.T) {
	t.Helper()
	orig := runStageLookPath
	runStageLookPath = func(_ string) (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { runStageLookPath = orig })
}

// withFakeExecutable stubs runStageExecutable to return a fake fishhawk-mcp
// path inside dir. When withSibling is true, it also creates a fishhawk-runner
// file in dir so the sibling-binary probe (os.Stat) succeeds.
func withFakeExecutable(t *testing.T, dir string, withSibling bool) {
	t.Helper()
	if withSibling {
		f, err := os.Create(filepath.Join(dir, "fishhawk-runner"))
		if err != nil {
			t.Fatalf("create fake fishhawk-runner sibling: %v", err)
		}
		f.Close()
	}
	orig := runStageExecutable
	fakeExe := filepath.Join(dir, "fishhawk-mcp")
	runStageExecutable = func() (string, error) { return fakeExe, nil }
	t.Cleanup(func() { runStageExecutable = orig })
}

// withFakeGitRemote stubs the origin-detect helper.
func withFakeGitRemote(t *testing.T, url string, err error) {
	t.Helper()
	orig := runStageGitRemoteOriginURL
	runStageGitRemoteOriginURL = func(_ string) (string, error) {
		return url, err
	}
	t.Cleanup(func() { runStageGitRemoteOriginURL = orig })
}

// withShortGrace lets the cancellation test exercise the SIGKILL
// escalation path without waiting 30s. Restored on test exit.
func withShortGrace(t *testing.T, d time.Duration) {
	t.Helper()
	orig := runStageGracePeriod
	runStageGracePeriod = d
	t.Cleanup(func() { runStageGracePeriod = orig })
}

// seedStage seeds a single fake-backend "plan" stage so the post-run
// fetch populates StageState in the tool output. Thin wrapper over
// seedStageOfType for the common plan case.
func seedStage(fb *fakeBackend, runID, stageID uuid.UUID, state string) {
	seedStageOfType(fb, runID, stageID, "plan", state)
}

// seedStageOfType seeds a single fake-backend stage of the given type.
// stage_id is now resolved tool-side from (run_id, stage type), so the
// seeded stage's Type must match the input's Stage for resolution to
// succeed.
func seedStageOfType(fb *fakeBackend, runID, stageID uuid.UUID, stageType, state string) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.stagesByRun[runID] = []Stage{{ID: stageID.String(), RunID: runID.String(), State: state, Type: stageType}}
}

// seedStages seeds an arbitrary set of stages on a run. Used by the
// resolution tests (absent / ambiguous / multi-type cases).
func seedStages(fb *fakeBackend, runID uuid.UUID, stages ...Stage) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.stagesByRun[runID] = stages
}

// TestRunStage_HostDispatchMarkerFails_NoSpawn pins the #1912 fail-closed
// contract (plan test c): when the host-dispatch marker call 4xxes, blocking
// fishhawk_run_stage returns a tool error and spawns NO runner.
func TestRunStage_HostDispatchMarkerFails_NoSpawn(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	calls := captureAllArgv(t)

	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "implement", "awaiting_host_dispatch")
	fb.hostDispatchStatus = http.StatusConflict // the marker 4xx -> fail closed

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID: runID.String(), Workflow: "feature_change", Stage: "implement",
		GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
	})
	if err == nil {
		t.Fatal("expected a fail-closed error when the host-dispatch marker 4xxes")
	}
	if !strings.Contains(err.Error(), "host-dispatch marker") || !strings.Contains(err.Error(), "NOT spawning") {
		t.Errorf("error should name the fail-closed marker; got %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("runner spawned despite a failed host-dispatch marker: %v (must fail closed)", *calls)
	}
	if n := fb.hostDispatchCalledByID[stageID]; n != 1 {
		t.Errorf("host-dispatch marker called %d times, want 1 (attempted once, then fail-closed)", n)
	}
}

// --- input validation ---

// TestRunStage_RequiresRunWorkflowStage asserts the three remaining
// required inputs. stage_id is now optional (resolved tool-side from
// (run_id, stage type)), so its absence is no longer a "required"
// error — see TestRunStage_ResolvesStageIDWhenOmitted and the
// absent/ambiguous resolution tests for that path.
func TestRunStage_RequiresRunWorkflowStage(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeRunner(t, "exit 0")

	cases := []struct {
		name string
		in   RunStageInput
	}{
		{"missing run_id", RunStageInput{StageID: uuid.NewString(), Workflow: "w", Stage: "plan"}},
		{"missing workflow", RunStageInput{RunID: uuid.NewString(), StageID: uuid.NewString(), Stage: "plan"}},
		{"missing stage", RunStageInput{RunID: uuid.NewString(), StageID: uuid.NewString(), Workflow: "w"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := r.runStage(context.Background(), nil, tc.in)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), "required") {
				t.Errorf("err should say 'required': %v", err)
			}
		})
	}
}

func TestRunStage_InvalidRunUUIDFails(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeRunner(t, "exit 0")

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:    "not-a-uuid",
		StageID:  uuid.NewString(),
		Workflow: "w",
		Stage:    "plan",
	})
	if err == nil || !strings.Contains(err.Error(), "run_id") {
		t.Fatalf("expected run_id UUID error, got %v", err)
	}
}

// --- stage_id resolution (#602) ---

// captureArgv swaps in a runStageCommand fake that records the runner
// argv and exits 0, plus a LookPath stub. Returns a pointer to the
// captured slice.
func captureArgv(t *testing.T) *[]string {
	t.Helper()
	captured := new([]string)
	origCmd := runStageCommand
	origLook := runStageLookPath
	runStageCommand = func(_ string, args ...string) *exec.Cmd {
		*captured = args
		return exec.Command("sh", "-c", "exit 0")
	}
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() {
		runStageCommand = origCmd
		runStageLookPath = origLook
	})
	return captured
}

// TestRunStage_ResolvesStageIDWhenOmitted verifies step 6a: when
// stage_id is omitted, it resolves from (run_id, stage type) and the
// composed argv carries the resolved --stage-id.
func TestRunStage_ResolvesStageIDWhenOmitted(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	argv := captureArgv(t)

	runID := uuid.New()
	planStage := uuid.New()
	implStage := uuid.New()
	seedStages(fb, runID,
		Stage{ID: planStage.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
		Stage{ID: implStage.String(), RunID: runID.String(), Type: "implement", State: "pending"},
	)

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		Workflow:   "w",
		Stage:      "implement",
		GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if !strings.Contains(strings.Join(*argv, " "), "--stage-id "+implStage.String()) {
		t.Errorf("argv should carry the resolved implement stage id %s\nfull: %v", implStage, *argv)
	}
}

// TestRunStage_BlocksSiblingInFlight proves the sibling-in-flight guard (#1872)
// is wired into the synchronous run_stage path too: a run_stage while a sibling
// stage is running must return a non-nil error AND spawn ZERO runners.
func TestRunStage_BlocksSiblingInFlight(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	spawned := 0
	origCmd := runStageCommand
	runStageCommand = func(_ string, _ ...string) *exec.Cmd {
		spawned++
		return exec.Command("sh", "-c", "exit 0")
	}
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() { runStageCommand = origCmd })

	runID := uuid.New()
	implID := uuid.NewString()
	acceptanceID := uuid.NewString()
	seedStages(fb, runID,
		Stage{ID: implID, RunID: runID.String(), Type: "implement", State: "running"},
		Stage{ID: acceptanceID, RunID: runID.String(), Type: "acceptance", State: "pending"},
	)

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		Workflow:   "feature_change",
		Stage:      "acceptance",
		GitHubRepo: "x/y",
	})
	if err == nil {
		t.Fatal("expected a pre-dispatch block when a sibling stage is running")
	}
	if !strings.Contains(err.Error(), "implement") {
		t.Errorf("block error should name the in-flight sibling: %v", err)
	}
	if spawned != 0 {
		t.Errorf("a blocked run_stage must spawn ZERO runners, got %d", spawned)
	}
}

// TestRunStage_AbsentStageTypeErrors verifies step 6b: a stage type
// not present in the run errors, naming the available stage types.
func TestRunStage_AbsentStageTypeErrors(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeRunner(t, "exit 0")

	runID := uuid.New()
	seedStages(fb, runID,
		Stage{ID: uuid.NewString(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	)

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		Workflow:   "w",
		Stage:      "implement",
		GitHubRepo: "x/y",
	})
	if err == nil {
		t.Fatal("expected stage-not-found error")
	}
	if !strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), "plan") {
		t.Errorf("err should say the type is not found and name available types (plan): %v", err)
	}
}

// TestRunStage_AmbiguousStageTypeErrors verifies step 6c: two stages
// of the same type with no explicit stage_id errors, naming the
// duplicate ids rather than picking one.
func TestRunStage_AmbiguousStageTypeErrors(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeRunner(t, "exit 0")

	runID := uuid.New()
	dup1 := uuid.NewString()
	dup2 := uuid.NewString()
	seedStages(fb, runID,
		Stage{ID: dup1, RunID: runID.String(), Type: "implement", State: "running"},
		Stage{ID: dup2, RunID: runID.String(), Type: "implement", State: "running"},
	)

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		Workflow:   "w",
		Stage:      "implement",
		GitHubRepo: "x/y",
	})
	if err == nil {
		t.Fatal("expected ambiguous-stage error")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("err should say ambiguous: %v", err)
	}
	if !strings.Contains(err.Error(), dup1) || !strings.Contains(err.Error(), dup2) {
		t.Errorf("err should name both duplicate ids %s, %s: %v", dup1, dup2, err)
	}
}

// TestRunStage_ExplicitStageIDDisagreesErrors verifies step 6d: an
// explicit stage_id that is not a stage of the requested type errors.
func TestRunStage_ExplicitStageIDDisagreesErrors(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeRunner(t, "exit 0")

	runID := uuid.New()
	planStage := uuid.New()
	implStage := uuid.New()
	seedStages(fb, runID,
		Stage{ID: planStage.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
		Stage{ID: implStage.String(), RunID: runID.String(), Type: "implement", State: "pending"},
	)

	// Pass the plan stage id but ask to run the implement stage.
	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		StageID:    planStage.String(),
		Workflow:   "w",
		Stage:      "implement",
		GitHubRepo: "x/y",
	})
	if err == nil {
		t.Fatal("expected disagreement error")
	}
	if !strings.Contains(err.Error(), "does not match") || !strings.Contains(err.Error(), planStage.String()) {
		t.Errorf("err should say the explicit id does not match the requested type: %v", err)
	}
}

// TestRunStage_ExplicitStageIDAgreesWorks verifies step 6e
// (back-compat): an explicit stage_id that matches the resolved type
// still works and flows into the argv.
func TestRunStage_ExplicitStageIDAgreesWorks(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	argv := captureArgv(t)

	runID := uuid.New()
	planStage := uuid.New()
	implStage := uuid.New()
	seedStages(fb, runID,
		Stage{ID: planStage.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
		Stage{ID: implStage.String(), RunID: runID.String(), Type: "implement", State: "pending"},
	)

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		StageID:    implStage.String(),
		Workflow:   "w",
		Stage:      "implement",
		GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if !strings.Contains(strings.Join(*argv, " "), "--stage-id "+implStage.String()) {
		t.Errorf("argv should carry the agreed explicit stage id %s\nfull: %v", implStage, *argv)
	}
}

// --- binary resolution ---

func TestRunStage_MissingBinaryReturnsCleanError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeRunnerMissing(t)
	// os.Executable sibling probe must also fail so we reach the error rung.
	withFakeExecutable(t, t.TempDir(), false /* no sibling */)

	// Seed a matching stage so stage resolution passes and we reach the
	// binary-resolution error rung this test is about.
	runID := uuid.New()
	stageID := uuid.New()
	seedStage(fb, runID, stageID, "awaiting_approval")

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:    runID.String(),
		StageID:  stageID.String(),
		Workflow: "w",
		Stage:    "plan",
	})
	if err == nil {
		t.Fatal("expected missing-binary error")
	}
	if !strings.Contains(err.Error(), "fishhawk-runner not on PATH") {
		t.Errorf("err should mention PATH: %v", err)
	}
}

// TestRunStage_RunnerBinaryInputWins verifies rung (a): a runner_binary in
// the input beats env, sibling, and PATH.
func TestRunStage_RunnerBinaryInputWins(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, map[string]string{
		"FISHHAWK_RUNNER_BIN": "/from/env/fishhawk-runner",
	})
	withFakeExecutable(t, t.TempDir(), true /* sibling present — must be ignored */)
	origLook := runStageLookPath
	runStageLookPath = func(_ string) (string, error) { return "/from/path/fishhawk-runner", nil }
	t.Cleanup(func() { runStageLookPath = origLook })

	var capturedBinary string
	origCmd := runStageCommand
	runStageCommand = func(name string, _ ...string) *exec.Cmd {
		capturedBinary = name
		return exec.Command("sh", "-c", "exit 0")
	}
	t.Cleanup(func() { runStageCommand = origCmd })

	runID := uuid.New()
	stageID := uuid.New()
	seedStage(fb, runID, stageID, "succeeded")

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:        runID.String(),
		StageID:      stageID.String(),
		Workflow:     "w",
		Stage:        "plan",
		GitHubRepo:   "x/y",
		RunnerBinary: "/explicit/fishhawk-runner",
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if capturedBinary != "/explicit/fishhawk-runner" {
		t.Errorf("binary = %q, want /explicit/fishhawk-runner (input rung)", capturedBinary)
	}
}

// TestRunStage_ExecutableSiblingFallback verifies rung (c): when input and env
// are empty but fishhawk-runner lives beside fishhawk-mcp, the sibling path is
// used even when PATH lookup would fail.
func TestRunStage_ExecutableSiblingFallback(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	dir := t.TempDir()
	withFakeExecutable(t, dir, true /* sibling present */)
	withFakeRunnerMissing(t) // PATH lookup fails — sibling must win

	var capturedBinary string
	origCmd := runStageCommand
	runStageCommand = func(name string, _ ...string) *exec.Cmd {
		capturedBinary = name
		return exec.Command("sh", "-c", "exit 0")
	}
	t.Cleanup(func() { runStageCommand = origCmd })

	runID := uuid.New()
	stageID := uuid.New()
	seedStage(fb, runID, stageID, "succeeded")

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		StageID:    stageID.String(),
		Workflow:   "w",
		Stage:      "plan",
		GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	want := filepath.Join(dir, "fishhawk-runner")
	if capturedBinary != want {
		t.Errorf("binary = %q, want sibling %q", capturedBinary, want)
	}
}

// TestRunStage_PathFallback verifies rung (d): when input, env, and sibling
// are all absent, the PATH lookup result is used.
func TestRunStage_PathFallback(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	// Sibling absent so the os.Executable rung falls through.
	withFakeExecutable(t, t.TempDir(), false /* no sibling */)

	var capturedBinary string
	origCmd := runStageCommand
	origLook := runStageLookPath
	runStageCommand = func(name string, _ ...string) *exec.Cmd {
		capturedBinary = name
		return exec.Command("sh", "-c", "exit 0")
	}
	runStageLookPath = func(_ string) (string, error) { return "/from/path/fishhawk-runner", nil }
	t.Cleanup(func() {
		runStageCommand = origCmd
		runStageLookPath = origLook
	})

	runID := uuid.New()
	stageID := uuid.New()
	seedStage(fb, runID, stageID, "succeeded")

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		StageID:    stageID.String(),
		Workflow:   "w",
		Stage:      "plan",
		GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if capturedBinary != "/from/path/fishhawk-runner" {
		t.Errorf("binary = %q, want /from/path/fishhawk-runner (PATH rung)", capturedBinary)
	}
}

func TestRunStage_EnvOverridesLookPath(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, map[string]string{
		"FISHHAWK_RUNNER_BIN": "/from/env/fishhawk-runner",
	})
	origLook := runStageLookPath
	runStageLookPath = func(_ string) (string, error) { return "/from/path/fishhawk-runner", nil }
	t.Cleanup(func() { runStageLookPath = origLook })

	var capturedBinary string
	origCmd := runStageCommand
	runStageCommand = func(name string, _ ...string) *exec.Cmd {
		capturedBinary = name
		return exec.Command("sh", "-c", "exit 0")
	}
	t.Cleanup(func() { runStageCommand = origCmd })

	runID := uuid.New()
	stageID := uuid.New()
	seedStage(fb, runID, stageID, "succeeded")

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		StageID:    stageID.String(),
		Workflow:   "w",
		Stage:      "plan",
		GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if capturedBinary != "/from/env/fishhawk-runner" {
		t.Errorf("binary = %q, want env override", capturedBinary)
	}
}

// --- argv composition ---

func TestRunStage_ArgvComposition_PlanStage(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	var capturedArgs []string
	origCmd := runStageCommand
	runStageCommand = func(_ string, args ...string) *exec.Cmd {
		capturedArgs = args
		return exec.Command("sh", "-c", "exit 0")
	}
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() {
		runStageCommand = origCmd
		runStageLookPath = exec.LookPath
	})

	runID := uuid.New()
	stageID := uuid.New()
	seedStage(fb, runID, stageID, "awaiting_approval")

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		StageID:    stageID.String(),
		Workflow:   "feature_change",
		Stage:      "plan",
		WorkingDir: "/tmp/checkout",
		GitHubRepo: "x/y",
		// Explicit false: this test asserts --no-pr composes. The
		// MCP default flipped to true (ADR-031), so omitting the field
		// would now drop --no-pr.
		PushAndOpenPR: boolPtr(false),
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}

	joined := strings.Join(capturedArgs, " ")
	for _, want := range []string{
		"--run-id " + runID.String(),
		"--stage-id " + stageID.String(),
		"--workflow feature_change",
		"--stage plan",
		"--working-dir /tmp/checkout",
		"--fetch-prompt",
		"--upload-trace",
		"--plan-out /tmp/fishhawk-plan.json",
		"--github-repo x/y",
		"--base-branch main",
		"--no-pr",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q\nfull: %s", want, joined)
		}
	}
	// Plan stages produce no diff, so --check-base-ref must be omitted.
	if strings.Contains(joined, "--check-base-ref") {
		t.Errorf("plan stage should not include --check-base-ref\nfull: %s", joined)
	}
}

func TestRunStage_ArgvComposition_ImplementStageNoPlanOut(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	var capturedArgs []string
	origCmd := runStageCommand
	runStageCommand = func(_ string, args ...string) *exec.Cmd {
		capturedArgs = args
		return exec.Command("sh", "-c", "exit 0")
	}
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() {
		runStageCommand = origCmd
		runStageLookPath = exec.LookPath
	})

	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "implement", "succeeded")

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		StageID:    stageID.String(),
		Workflow:   "feature_change",
		Stage:      "implement",
		GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	joined := strings.Join(capturedArgs, " ")
	if strings.Contains(joined, "--plan-out") {
		t.Errorf("implement stage should not include --plan-out: %v", capturedArgs)
	}
	// Implement stages must carry --check-base-ref so the runner emits
	// the git_diff event (backend policy_evaluated + implement-review).
	if !strings.Contains(joined, "--check-base-ref main") {
		t.Errorf("implement stage missing --check-base-ref main\nfull: %s", joined)
	}
}

func TestRunStage_PushAndOpenPR_DropsNoPRFlag(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	var capturedArgs []string
	origCmd := runStageCommand
	runStageCommand = func(_ string, args ...string) *exec.Cmd {
		capturedArgs = args
		return exec.Command("sh", "-c", "exit 0")
	}
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() {
		runStageCommand = origCmd
		runStageLookPath = exec.LookPath
	})

	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "implement", "succeeded")

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:         runID.String(),
		StageID:       stageID.String(),
		Workflow:      "feature_change",
		Stage:         "implement",
		GitHubRepo:    "x/y",
		PushAndOpenPR: boolPtr(true),
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if strings.Contains(strings.Join(capturedArgs, " "), "--no-pr") {
		t.Errorf("--no-pr should be absent when push_and_open_pr=true: %v", capturedArgs)
	}
}

// boolPtr returns a pointer to b. The fishhawk_run_stage input's
// push_and_open_pr is *bool (ADR-031) so a test can express omitted
// (nil), explicit true, and explicit false distinctly.
func boolPtr(b bool) *bool { return &b }

func TestRunStage_PushAndOpenPR_OmittedDefaultsTrue(t *testing.T) {
	// ADR-031 Phase 1: the MCP-driven local loop defaults
	// push_and_open_pr to true so every run carries a pull_request_url.
	// An omitted field (nil) must drop --no-pr.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	var capturedArgs []string
	origCmd := runStageCommand
	runStageCommand = func(_ string, args ...string) *exec.Cmd {
		capturedArgs = args
		return exec.Command("sh", "-c", "exit 0")
	}
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() {
		runStageCommand = origCmd
		runStageLookPath = exec.LookPath
	})

	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "implement", "succeeded")

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		StageID:    stageID.String(),
		Workflow:   "feature_change",
		Stage:      "implement",
		GitHubRepo: "x/y",
		// PushAndOpenPR omitted (nil) -> resolves to true.
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if strings.Contains(strings.Join(capturedArgs, " "), "--no-pr") {
		t.Errorf("--no-pr should be absent when push_and_open_pr is omitted (defaults true): %v", capturedArgs)
	}
}

func TestRunStage_PushAndOpenPR_ExplicitFalseKeepsNoPR(t *testing.T) {
	// An explicit false is honored (commit-yourself flow): --no-pr
	// must compose even though the default is now true.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	var capturedArgs []string
	origCmd := runStageCommand
	runStageCommand = func(_ string, args ...string) *exec.Cmd {
		capturedArgs = args
		return exec.Command("sh", "-c", "exit 0")
	}
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() {
		runStageCommand = origCmd
		runStageLookPath = exec.LookPath
	})

	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "implement", "succeeded")

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:         runID.String(),
		StageID:       stageID.String(),
		Workflow:      "feature_change",
		Stage:         "implement",
		GitHubRepo:    "x/y",
		PushAndOpenPR: boolPtr(false),
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if !strings.Contains(strings.Join(capturedArgs, " "), "--no-pr") {
		t.Errorf("--no-pr should be present when push_and_open_pr is explicitly false: %v", capturedArgs)
	}
}

// --- github repo resolution ---

func TestRunStage_GitHubRepoAutoDetectWhenEmpty(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeGitRemote(t, "git@github.com:owner/name.git", nil)

	var capturedArgs []string
	origCmd := runStageCommand
	runStageCommand = func(_ string, args ...string) *exec.Cmd {
		capturedArgs = args
		return exec.Command("sh", "-c", "exit 0")
	}
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() {
		runStageCommand = origCmd
		runStageLookPath = exec.LookPath
	})

	runID := uuid.New()
	stageID := uuid.New()
	seedStage(fb, runID, stageID, "succeeded")

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:    runID.String(),
		StageID:  stageID.String(),
		Workflow: "w",
		Stage:    "plan",
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if !strings.Contains(strings.Join(capturedArgs, " "), "--github-repo owner/name") {
		t.Errorf("auto-detected repo not in argv: %v", capturedArgs)
	}
}

func TestRunStage_GitHubRepoAutoDetectFails_WarnsWithoutPush(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeGitRemote(t, "", errors.New("not in a git repo"))
	withFakeRunner(t, "exit 0")

	runID := uuid.New()
	stageID := uuid.New()
	seedStage(fb, runID, stageID, "succeeded")

	_, out, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:    runID.String(),
		StageID:  stageID.String(),
		Workflow: "w",
		Stage:    "plan",
		// Explicit false: a missing repo is only a soft warning when
		// the run won't push. The MCP default flipped to true
		// (ADR-031), under which a repo-detect failure is a hard error.
		PushAndOpenPR: boolPtr(false),
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if len(out.Warnings) == 0 {
		t.Error("expected a warning about auto-detect")
	}
}

func TestRunStage_GitHubRepoAutoDetectFails_WithPushErrors(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeGitRemote(t, "", errors.New("nope"))
	withFakeRunner(t, "exit 0")

	// Seed a matching implement stage so stage resolution passes and the
	// test reaches the github_repo auto-detect failure it actually
	// exercises (resolveStageID runs before the github_repo check).
	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "implement", "pending")

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:         runID.String(),
		StageID:       stageID.String(),
		Workflow:      "w",
		Stage:         "implement",
		PushAndOpenPR: boolPtr(true),
	})
	if err == nil || !strings.Contains(err.Error(), "github_repo") {
		t.Fatalf("expected github_repo error, got %v", err)
	}
}

// TestRunStage_PopulatesStageWaitStatus asserts RunStageOutput.StageWaitStatus
// is populated from the post-run stage fetch (#879/#880): a synchronous return
// on a succeeded stage records the terminal status on the handle and omits the
// poll interval.
func TestRunStage_PopulatesStageWaitStatus(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeRunner(t, "exit 0")

	runID := uuid.New()
	stageID := uuid.New()
	seedStage(fb, runID, stageID, "succeeded")

	_, out, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:         runID.String(),
		StageID:       stageID.String(),
		Workflow:      "w",
		Stage:         "plan",
		PushAndOpenPR: boolPtr(false),
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if out.StageWaitStatus == nil {
		t.Fatal("StageWaitStatus is nil; expected it populated from the post-run fetch")
	}
	if out.StageWaitStatus.Stage != "plan" {
		t.Errorf("StageWaitStatus.Stage = %q, want plan", out.StageWaitStatus.Stage)
	}
	if out.StageWaitStatus.Status != "succeeded" {
		t.Errorf("StageWaitStatus.Status = %q, want succeeded", out.StageWaitStatus.Status)
	}
	if out.StageWaitStatus.PollIntervalSeconds != 0 {
		t.Errorf("StageWaitStatus.PollIntervalSeconds = %d, want 0 (terminal stage omits it)", out.StageWaitStatus.PollIntervalSeconds)
	}
}

// --- JSONL accumulation ---

func TestRunStage_JSONLAccumulation_OrderPreserved(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeRunner(t, `printf '%s\n' '{"kind":"runner_started"}' '{"kind":"signing_key_issued"}' '{"kind":"trace_uploaded"}' >&2`)

	runID := uuid.New()
	stageID := uuid.New()
	seedStage(fb, runID, stageID, "succeeded")

	_, out, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		StageID:    stageID.String(),
		Workflow:   "w",
		Stage:      "plan",
		GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if len(out.Events) != 3 {
		t.Fatalf("events = %d, want 3 (got %+v)", len(out.Events), out.Events)
	}
	kinds := []string{}
	for _, ev := range out.Events {
		m, ok := ev.Payload.(map[string]any)
		if !ok {
			t.Fatalf("event payload not an object: %T", ev.Payload)
		}
		kinds = append(kinds, m["kind"].(string))
	}
	want := []string{"runner_started", "signing_key_issued", "trace_uploaded"}
	for i, k := range kinds {
		if k != want[i] {
			t.Errorf("event[%d] kind = %q, want %q", i, k, want[i])
		}
	}
	if out.StageState != "succeeded" {
		t.Errorf("StageState = %q, want succeeded", out.StageState)
	}
}

func TestRunStage_NonJSONLineWarns(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeRunner(t, `printf '%s\n' 'Running plan stage...' '{"kind":"runner_started"}' >&2`)

	runID := uuid.New()
	stageID := uuid.New()
	seedStage(fb, runID, stageID, "succeeded")

	_, out, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		StageID:    stageID.String(),
		Workflow:   "w",
		Stage:      "plan",
		GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if len(out.Events) != 1 {
		t.Errorf("events = %d, want 1 (the JSON line)", len(out.Events))
	}
	found := false
	for _, w := range out.Warnings {
		if strings.Contains(w, "Running plan stage") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("non-JSON line should be in warnings: %+v", out.Warnings)
	}
}

// --- exit code propagation ---

func TestRunStage_NonZeroExitPropagated(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeRunner(t, `printf '%s\n' '{"kind":"runner_started"}' >&2; exit 7`)

	runID := uuid.New()
	stageID := uuid.New()
	seedStage(fb, runID, stageID, "failed")

	_, out, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		StageID:    stageID.String(),
		Workflow:   "w",
		Stage:      "plan",
		GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("runStage: %v (a non-zero exit is a successful tool call with ExitCode set)", err)
	}
	if out.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", out.ExitCode)
	}
	if out.StageState != "failed" {
		t.Errorf("StageState = %q, want failed", out.StageState)
	}
}

// --- cancellation ---

func TestRunStage_ContextCancelSendsSIGTERM(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	// Trap TERM and exit immediately so the test runs fast. Without
	// the trap, sleep is uninterruptible on some shells.
	withFakeRunner(t, `trap 'exit 143' TERM; sleep 10`)
	withShortGrace(t, 2*time.Second)

	runID := uuid.New()
	stageID := uuid.New()
	// 'pending' is the realistic pre-spawn state: the sibling-in-flight guard
	// (#1872) rejects dispatching a target already 'running'.
	seedStage(fb, runID, stageID, "pending")

	ctx, cancel := context.WithCancel(context.Background())

	// Cancel after a short delay so the runner is definitely
	// started before SIGTERM fires.
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, out, err := r.runStage(ctx, nil, RunStageInput{
		RunID:      runID.String(),
		StageID:    stageID.String(),
		Workflow:   "w",
		Stage:      "plan",
		GitHubRepo: "x/y",
	})
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Errorf("cancellation took %v; runner was not signalled", elapsed)
	}
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("err should say 'cancelled': %v", err)
	}
	// Exit code 143 is 128 + SIGTERM(15). Some platforms collapse
	// to -1 if SIGKILL escalates; either is acceptable.
	if out.ExitCode != 143 && out.ExitCode != -1 {
		t.Logf("note: ExitCode = %d (expected 143 from SIGTERM trap, or -1 if SIGKILL escalated)", out.ExitCode)
	}
}

// TestSpawnRunnerStage_ParsesEventsAndExitCode pins the extracted
// spawnRunnerStage contract directly (the shared spawn-to-exit core
// fishhawk_run_stage and fishhawk_run_children both delegate to): it parses
// each stderr JSONL line into an event, returns the process exit code as DATA
// (a non-zero exit is NOT a Go error), and a non-JSON line lands in warnings
// rather than failing. The fishhawk_run_stage regressions above exercise it
// through the tool; this asserts the helper in isolation.
func TestSpawnRunnerStage_ParsesEventsAndExitCode(t *testing.T) {
	withFakeRunner(t, `printf '%s\n' '{"kind":"runner_started"}'>&2; printf '%s\n' 'not json'>&2; printf '%s\n' '{"event":"runner_completed","outcome":"failed"}'>&2; exit 5`)

	events, warnings, exitCode, err := spawnRunnerStage(
		context.Background(), "/fake/fishhawk-runner",
		[]string{"--run-id", "x"}, nil, nil, nil)
	if err != nil {
		t.Fatalf("spawnRunnerStage returned an error for a non-zero exit: %v", err)
	}
	if exitCode != 5 {
		t.Errorf("exitCode = %d, want 5", exitCode)
	}
	if len(events) != 2 {
		t.Errorf("parsed %d events, want 2 (the JSON lines)", len(events))
	}
	var sawNonJSON bool
	for _, wmsg := range warnings {
		if strings.Contains(wmsg, "non-JSON runner stderr") {
			sawNonJSON = true
		}
	}
	if !sawNonJSON {
		t.Errorf("warnings = %v, want a non-JSON-line warning", warnings)
	}
}

// --- helpers + parsers ---

// TestJSONInt covers each jsonInt branch directly (#1181 concern 3). The
// git_diff JSON-decode feeder only ever delivers float64 (encoding/json
// decodes every JSON number to float64), so the int / json.Number / nil /
// non-numeric branches are never reached through that path — these assertions
// pin them.
func TestJSONInt(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int
	}{
		{"float64 whole", float64(7), 7},
		// Go spec, Conversions: converting float→int discards the fraction
		// (truncation toward zero), which the float64 branch relies on.
		{"float64 truncates toward zero", float64(7.9), 7},
		{"int", 5, 5},
		{"json.Number", json.Number("42"), 42},
		{"nil", nil, 0},
		{"non-numeric string", "x", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := jsonInt(tc.in); got != tc.want {
				t.Errorf("jsonInt(%#v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestRunStageParseGitHubRemote(t *testing.T) {
	cases := []struct {
		in    string
		owner string
		name  string
		err   bool
	}{
		{"https://github.com/owner/name.git", "owner", "name", false},
		{"https://github.com/owner/name", "owner", "name", false},
		{"https://github.com/owner/name/", "owner", "name", false},
		{"git@github.com:owner/name.git", "owner", "name", false},
		{"ssh://git@github.com/owner/name.git", "owner", "name", false},
		{"https://gitlab.com/owner/name", "", "", true},
		{"not-a-url", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			o, n, err := runStageParseGitHubRemote(tc.in)
			if (err != nil) != tc.err {
				t.Errorf("err = %v, wantErr=%v", err, tc.err)
			}
			if o != tc.owner || n != tc.name {
				t.Errorf("got %s/%s, want %s/%s", o, n, tc.owner, tc.name)
			}
		})
	}
}

func TestRunStageEventMessage_PrefersKind(t *testing.T) {
	m := map[string]any{"kind": "runner_started", "foo": "bar"}
	if got := runStageEventMessage(m); got != "runner_started" {
		t.Errorf("message = %q, want runner_started", got)
	}
}

func TestRunStageEventMessage_FallsBackToJSON(t *testing.T) {
	m := map[string]any{"foo": "bar"}
	got := runStageEventMessage(m)
	if got == "" || !strings.Contains(got, "foo") {
		t.Errorf("message = %q, want a JSON-ish fallback", got)
	}
}

// TestRunStageEventMessage_StageProgress verifies a stage_progress
// heartbeat (#580) renders its coarse counters into the progress
// message (turns / tokens / elapsed / last), while a normal event
// still falls through to its kind.
func TestRunStageEventMessage_StageProgress(t *testing.T) {
	// JSON numbers decode as float64, mirroring the relay's
	// json.Unmarshal-into-any path.
	progress := map[string]any{
		"event":           "stage_progress",
		"elapsed_seconds": float64(42),
		"turns":           float64(7),
		"tokens_so_far":   float64(13402),
		"last_event_kind": "assistant",
	}
	got := runStageEventMessage(progress)
	for _, want := range []string{"turns=7", "tokens=13402", "elapsed=42s", "last=assistant"} {
		if !strings.Contains(got, want) {
			t.Errorf("message = %q, want it to contain %q", got, want)
		}
	}

	// A normal event still returns its kind, unaffected by the
	// stage_progress special-case.
	normal := map[string]any{"kind": "runner_started"}
	if msg := runStageEventMessage(normal); msg != "runner_started" {
		t.Errorf("normal event message = %q, want runner_started", msg)
	}
}

// TestRunStageEventMessage_ScopeAmendmentPending pins the runner->relay
// seam for the mid-stage scope-amendment in-band signal (#1035). It feeds
// the EXACT literal-JSONL line the runner watcher emits (the seam contract
// shared with the runner-emit test, cf. #618 — the field set is
// {event, amendment_id, paths}) through the relay's json.Unmarshal-into-any
// path and asserts the surfaced message carries the actionable amendment id
// and paths rather than the bare event name the generic fallback would
// yield.
func TestRunStageEventMessage_ScopeAmendmentPending(t *testing.T) {
	// The literal line the runner's watchScopeAmendments goroutine emits.
	// Keep the field names byte-identical to the runner emitter.
	const line = `{"event":"scope_amendment_pending","run_id":"run-1","stage_id":"stage-1","amendment_id":"amd-7","paths":[{"path":"docs/ARCHITECTURE.md","operation":"modify"},{"path":"x/new.go","operation":"create"}]}`
	var payload any
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		t.Fatalf("unmarshal seam line: %v", err)
	}
	got := runStageEventMessage(payload)
	for _, want := range []string{
		"scope_amendment_pending",
		"id=amd-7",
		"docs/ARCHITECTURE.md (modify)",
		"x/new.go (create)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message = %q, want it to contain %q", got, want)
		}
	}
}

// --- compact-by-default summary (#647) ---

// compactFixtureBody is a fake-runner stderr stream mixing
// stage_progress heartbeats with non-heartbeat events. The terminal
// event is the REAL relayed wire shape
// {"event":"runner_completed","outcome":"ok","tokens_used":N} — NOT a
// synthetic invocation_end, which is appended to the signed trace
// bundle only and never reaches the JSONL stderr the relay reads.
const compactFixtureBody = `printf '%s\n' ` +
	`'{"event":"stage_progress","turns":1,"tokens_so_far":100,"elapsed_seconds":15,"last_event_kind":"assistant"}' ` +
	`'{"kind":"runner_started"}' ` +
	`'{"event":"stage_progress","turns":3,"tokens_so_far":500,"elapsed_seconds":30,"last_event_kind":"tool_use"}' ` +
	`'{"kind":"git_diff","changed_files":["a.go"]}' ` +
	`'{"event":"runner_completed","outcome":"ok","tokens_used":20733}' >&2`

// TestRunStage_CompactByDefault verifies the default (Verbose=false)
// result OMITS every stage_progress heartbeat from Events while
// RETAINING all non-heartbeat events (runner_started, git_diff, and
// the terminal runner_completed) in arrival order, and populates the
// scalar summary from the last heartbeat + the runner_completed event.
func TestRunStage_CompactByDefault(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeRunner(t, compactFixtureBody)

	runID := uuid.New()
	stageID := uuid.New()
	seedStage(fb, runID, stageID, "succeeded")

	_, out, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		StageID:    stageID.String(),
		Workflow:   "w",
		Stage:      "plan",
		GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}

	// Heartbeats dropped; the three non-heartbeat events retained in order.
	wantKinds := []string{"runner_started", "git_diff", "runner_completed"}
	if len(out.Events) != len(wantKinds) {
		t.Fatalf("compact Events = %d, want %d (%+v)", len(out.Events), len(wantKinds), out.Events)
	}
	for i, ev := range out.Events {
		m, ok := ev.Payload.(map[string]any)
		if !ok {
			t.Fatalf("event[%d] payload not an object: %T", i, ev.Payload)
		}
		if m["event"] == "stage_progress" {
			t.Errorf("compact Events should not contain a stage_progress heartbeat: %+v", m)
		}
		// runner_completed carries `event`, the rest carry `kind`.
		got, _ := m["kind"].(string)
		if got == "" {
			got, _ = m["event"].(string)
		}
		if got != wantKinds[i] {
			t.Errorf("event[%d] = %q, want %q", i, got, wantKinds[i])
		}
	}

	// Summary populated: turns/elapsed/last from the last heartbeat,
	// outcome + tokens_used from runner_completed (overriding the
	// heartbeat's tokens_so_far=500).
	if out.Outcome != "ok" {
		t.Errorf("Outcome = %q, want ok", out.Outcome)
	}
	if out.Turns != 3 {
		t.Errorf("Turns = %d, want 3", out.Turns)
	}
	if out.TokensUsed != 20733 {
		t.Errorf("TokensUsed = %d, want 20733 (runner_completed overrides the heartbeat)", out.TokensUsed)
	}
	if out.ElapsedSeconds != 30 {
		t.Errorf("ElapsedSeconds = %d, want 30", out.ElapsedSeconds)
	}
	if out.LastEventKind != "tool_use" {
		t.Errorf("LastEventKind = %q, want tool_use", out.LastEventKind)
	}
}

// TestRunStage_VerboseRetainsHeartbeats verifies Verbose=true restores
// the full event list including every stage_progress heartbeat, while
// the summary scalars are still populated.
func TestRunStage_VerboseRetainsHeartbeats(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeRunner(t, compactFixtureBody)

	runID := uuid.New()
	stageID := uuid.New()
	seedStage(fb, runID, stageID, "succeeded")

	_, out, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		StageID:    stageID.String(),
		Workflow:   "w",
		Stage:      "plan",
		GitHubRepo: "x/y",
		Verbose:    true,
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}

	// All five events retained (two heartbeats + three others).
	if len(out.Events) != 5 {
		t.Fatalf("verbose Events = %d, want 5 (%+v)", len(out.Events), out.Events)
	}
	heartbeats := 0
	for _, ev := range out.Events {
		if m, ok := ev.Payload.(map[string]any); ok && m["event"] == "stage_progress" {
			heartbeats++
		}
	}
	if heartbeats != 2 {
		t.Errorf("verbose Events should retain both heartbeats; got %d", heartbeats)
	}
	// Summary still populated under verbose.
	if out.Outcome != "ok" || out.TokensUsed != 20733 {
		t.Errorf("verbose summary = (%q, %d), want (ok, 20733)", out.Outcome, out.TokensUsed)
	}
}

// TestSummarizeRunStageEvents unit-tests the filter/summary helper in
// isolation across the three salient cases.
func TestSummarizeRunStageEvents(t *testing.T) {
	hb := func(turns, tokens, elapsed float64, last string) RunnerEvent {
		return RunnerEvent{Payload: map[string]any{
			"event": "stage_progress", "turns": turns,
			"tokens_so_far": tokens, "elapsed_seconds": elapsed,
			"last_event_kind": last,
		}}
	}

	t.Run("no heartbeats: zero summary, all retained", func(t *testing.T) {
		in := []RunnerEvent{
			{Payload: map[string]any{"kind": "runner_started"}},
			{Payload: map[string]any{"kind": "trace_uploaded"}},
		}
		summary, filtered := summarizeRunStageEvents(in)
		if summary != (runStageSummary{}) {
			t.Errorf("summary = %+v, want zero", summary)
		}
		if len(filtered) != len(in) {
			t.Errorf("filtered = %d, want %d (all retained)", len(filtered), len(in))
		}
	})

	t.Run("only heartbeats: filtered empty, last heartbeat wins", func(t *testing.T) {
		in := []RunnerEvent{
			hb(1, 100, 15, "assistant"),
			hb(4, 900, 45, "tool_use"),
		}
		summary, filtered := summarizeRunStageEvents(in)
		if len(filtered) != 0 {
			t.Errorf("filtered = %d, want 0 (all heartbeats dropped)", len(filtered))
		}
		if summary.Turns != 4 || summary.TokensUsed != 900 || summary.ElapsedSeconds != 45 || summary.LastEventKind != "tool_use" {
			t.Errorf("summary = %+v, want last heartbeat (4/900/45/tool_use)", summary)
		}
		if summary.Outcome != "" {
			t.Errorf("Outcome = %q, want empty (no runner_completed)", summary.Outcome)
		}
	})

	t.Run("runner_completed overrides heartbeat token count", func(t *testing.T) {
		in := []RunnerEvent{
			hb(2, 500, 20, "assistant"),
			{Payload: map[string]any{"event": "runner_completed", "outcome": "ok", "tokens_used": float64(20733)}},
		}
		summary, filtered := summarizeRunStageEvents(in)
		// The heartbeat is dropped; runner_completed is retained.
		if len(filtered) != 1 {
			t.Fatalf("filtered = %d, want 1 (runner_completed retained)", len(filtered))
		}
		if m, _ := filtered[0].Payload.(map[string]any); m["event"] != "runner_completed" {
			t.Errorf("filtered[0] = %+v, want runner_completed", filtered[0].Payload)
		}
		if summary.Outcome != "ok" {
			t.Errorf("Outcome = %q, want ok", summary.Outcome)
		}
		if summary.TokensUsed != 20733 {
			t.Errorf("TokensUsed = %d, want 20733 (override), not 500", summary.TokensUsed)
		}
		// Turns still come from the heartbeat.
		if summary.Turns != 2 {
			t.Errorf("Turns = %d, want 2 (from heartbeat)", summary.Turns)
		}
	})

	t.Run("implement_fixup_no_changes sets the FixupNoChanges flag", func(t *testing.T) {
		// A no-change fix-up pass (#967): the relayed stream carries the
		// runner's implement_fixup_no_changes event followed by a plain-ok
		// runner_completed. The summary must surface the no-op as a
		// dedicated flag (Outcome alone reads as plain success) and retain
		// the event in the filtered slice.
		in := []RunnerEvent{
			{Payload: map[string]any{"event": "implement_fixup_no_changes",
				"branch": "fishhawk/run-aaaaaaaa/stage-bbbbbbbb", "base_sha": "cafe"}},
			{Payload: map[string]any{"event": "runner_completed", "outcome": "ok", "tokens_used": float64(7)}},
		}
		summary, filtered := summarizeRunStageEvents(in)
		if !summary.FixupNoChanges {
			t.Error("FixupNoChanges = false, want true after an implement_fixup_no_changes event")
		}
		if summary.Outcome != "ok" {
			t.Errorf("Outcome = %q, want ok (the flag complements, not replaces, the outcome)", summary.Outcome)
		}
		if len(filtered) != 2 {
			t.Errorf("filtered = %d, want 2 (the no-changes event is retained)", len(filtered))
		}
	})

	t.Run("no fixup event leaves FixupNoChanges false", func(t *testing.T) {
		in := []RunnerEvent{
			{Payload: map[string]any{"event": "runner_completed", "outcome": "ok"}},
		}
		summary, _ := summarizeRunStageEvents(in)
		if summary.FixupNoChanges {
			t.Error("FixupNoChanges = true, want false without an implement_fixup_no_changes event")
		}
	})
}

// --- DiffSummary, AuditPointer, RunURL enrichment (#442) ---

// TestRunStage_DiffSummary_NilWhenNoGitDiffEvent verifies acceptance
// criterion (a): when the runner emits no git_diff event (e.g. a plan
// stage), DiffSummary is nil.
func TestRunStage_DiffSummary_NilWhenNoGitDiffEvent(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	// Runner emits events but none are git_diff.
	withFakeRunner(t, `printf '%s\n' '{"kind":"runner_started"}' >&2`)

	runID := uuid.New()
	stageID := uuid.New()
	seedStage(fb, runID, stageID, "awaiting_approval")

	_, out, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		StageID:    stageID.String(),
		Workflow:   "w",
		Stage:      "plan",
		GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if out.DiffSummary != nil {
		t.Errorf("DiffSummary should be nil when no git_diff event; got %+v", out.DiffSummary)
	}
}

// TestRunStage_DiffSummary_PopulatedFromGitDiffEvent verifies
// acceptance criterion (b): when the runner emits a git_diff event,
// DiffSummary is populated with FilesChanged, Insertions, and Deletions
// read straight from the event payload (#1137) — NOT recomputed by
// shelling git in working_dir, which per-run worktree isolation makes
// stale (the run's commit no longer lands on this process's HEAD).
func TestRunStage_DiffSummary_PopulatedFromGitDiffEvent(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	// Runner emits git_diff event with two changed files and the staged
	// numstat totals carried on the event itself.
	withFakeRunner(t, `printf '%s\n' '{"kind":"git_diff","changed_files":["a.go","b.go"],"insertions":15,"deletions":10}' >&2`)

	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "implement", "succeeded")

	_, out, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		StageID:    stageID.String(),
		Workflow:   "w",
		Stage:      "implement",
		GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if out.DiffSummary == nil {
		t.Fatal("DiffSummary should be non-nil when git_diff event is present")
	}
	if out.DiffSummary.FilesChanged != 2 {
		t.Errorf("FilesChanged = %d, want 2", out.DiffSummary.FilesChanged)
	}
	if out.DiffSummary.Insertions != 15 { // 5 + 10
		t.Errorf("Insertions = %d, want 15 (5+10)", out.DiffSummary.Insertions)
	}
	if out.DiffSummary.Deletions != 10 { // 3 + 7
		t.Errorf("Deletions = %d, want 10 (3+7)", out.DiffSummary.Deletions)
	}
}

// TestRunStage_AuditPointer_NilOnBackend500 verifies acceptance
// criterion (c): when the backend returns HTTP 500 for the audit
// endpoint, AuditPointer is nil and no warning is added. Also asserts
// the request went to /v0/audit (the cross-chain endpoint) with
// run_id and limit=1, not /v0/runs/{id}/audit.
func TestRunStage_AuditPointer_NilOnBackend500(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeRunner(t, "exit 0")

	fb.auditStatus = http.StatusInternalServerError

	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "implement", "succeeded")

	warningsBefore := 0
	_, out, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		StageID:    stageID.String(),
		Workflow:   "w",
		Stage:      "implement",
		GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if out.AuditPointer != nil {
		t.Errorf("AuditPointer should be nil on backend 500; got %+v", out.AuditPointer)
	}
	// Warnings count should not grow due to the audit failure.
	if len(out.Warnings) != warningsBefore {
		for _, w := range out.Warnings {
			if strings.Contains(strings.ToLower(w), "audit") {
				t.Errorf("audit failure should not add a warning; got: %q", w)
			}
		}
	}
	// The request must have hit /v0/audit (cross-chain endpoint), not
	// /v0/runs/{id}/audit. auditCalledByID is incremented only by
	// the /v0/audit handler.
	if got := fb.auditCalledByID[runID]; got == 0 {
		t.Errorf("expected /v0/audit to be called; auditCalledByID[runID] = %d", got)
	}
	if fb.lastAuditLimit != "1" {
		t.Errorf("audit limit = %q, want 1", fb.lastAuditLimit)
	}
}

// TestRunStage_RunURL_ContainsRunID verifies acceptance criterion (d):
// RunURL equals the server's base URL + '/runs/' + runID.
func TestRunStage_RunURL_ContainsRunID(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeRunner(t, "exit 0")

	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "implement", "succeeded")

	_, out, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		StageID:    stageID.String(),
		Workflow:   "w",
		Stage:      "implement",
		GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	want := srv.URL + "/runs/" + runID.String()
	if out.RunURL != want {
		t.Errorf("RunURL = %q, want %q", out.RunURL, want)
	}
}

// TestRunStage_SurfacesBudgetBlock is the run_stage half of the #693
// wire-to-tool seam: after the runner exits the handler fetches
// GET /v0/runs/{id}/budget and surfaces it on RunStageOutput.Budget.
func TestRunStage_SurfacesBudgetBlock(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	captureArgv(t)

	runID := uuid.New()
	stageID := uuid.New()
	seedStage(fb, runID, stageID, "succeeded")
	warn := 0.8
	seedBudget(fb, runID, BudgetStatus{
		Period: "weekly", LimitUSD: 50, SpentUSD: 165.86, Fraction: 3.3172,
		WarnAt: &warn, Tier: "over", Enforcement: "advisory",
	})

	_, out, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		StageID:    stageID.String(),
		Workflow:   "w",
		Stage:      "plan",
		GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if out.Budget == nil {
		t.Fatal("expected budget block surfaced after the stage ran; got nil")
	}
	if out.Budget.Tier != "over" {
		t.Errorf("budget tier = %q, want over", out.Budget.Tier)
	}
}

// TestRunStage_BudgetFetchError_WarnsNeverFails verifies the best-effort
// contract (#693): a failing GET /v0/runs/{id}/budget must NOT fail the
// stage — the stage still succeeds, Budget is nil, and the fetch error is
// surfaced as a warning rather than a tool error.
func TestRunStage_BudgetFetchError_WarnsNeverFails(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	captureArgv(t)

	runID := uuid.New()
	stageID := uuid.New()
	seedStage(fb, runID, stageID, "succeeded")
	fb.budgetStatus = 500 // GET /budget returns 500 → GetRunBudget errors

	_, out, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		StageID:    stageID.String(),
		Workflow:   "w",
		Stage:      "plan",
		GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("runStage must not fail on a budget-fetch error: %v", err)
	}
	if out.Budget != nil {
		t.Errorf("budget should be nil on fetch error, got %+v", out.Budget)
	}
	var sawBudgetWarn bool
	for _, w := range out.Warnings {
		if strings.Contains(strings.ToLower(w), "budget") {
			sawBudgetWarn = true
		}
	}
	if !sawBudgetWarn {
		t.Errorf("expected a budget warning on fetch error; warnings = %v", out.Warnings)
	}
}

func TestRunStage_OmitsBudgetWhenNoBudget(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	captureArgv(t)

	runID := uuid.New()
	stageID := uuid.New()
	seedStage(fb, runID, stageID, "succeeded")
	// No seedBudget → backend returns {} → field omitted.

	_, out, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		StageID:    stageID.String(),
		Workflow:   "w",
		Stage:      "plan",
		GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if out.Budget != nil {
		t.Errorf("expected no budget block; got %+v", out.Budget)
	}
}

// TestRunStage_NextActions_SurfacedAfterStage is the run_stage half of
// the #1024 wiring: after the runner exits, the handler computes
// next_actions from the post-stage fetches (run row + stage list +
// review statuses) so a terminal run_stage call hands the operator the
// legal next move directly. A plan stage parked at its gate with no
// review entries classifies as plan_gate_parked → approve/reject.
func TestRunStage_NextActions_SurfacedAfterStage(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	captureArgv(t)

	runID := uuid.New()
	stageID := uuid.New()
	fb.mu.Lock()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
	fb.mu.Unlock()
	seedStage(fb, runID, stageID, "awaiting_approval")

	_, out, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		StageID:    stageID.String(),
		Workflow:   "w",
		Stage:      "plan",
		GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if out.NextActions == nil {
		t.Fatal("NextActions is nil; want the #1024 block on the run-terminal result")
	}
	if out.NextActions.State != "plan_gate_parked" {
		t.Errorf("next_actions.state = %q, want plan_gate_parked", out.NextActions.State)
	}
	var sawApprove bool
	for _, a := range out.NextActions.Actions {
		if a.Action == "fishhawk_approve_plan" {
			sawApprove = true
		}
	}
	if !sawApprove {
		t.Errorf("next_actions should offer fishhawk_approve_plan at the parked plan gate; got %+v", out.NextActions.Actions)
	}
}

// TestRunStage_BlocksHostDispatchAgainstActionsLockedRun asserts the #1355
// guardrail on the SYNCHRONOUS host-dispatch verb: a run already LOCKED to
// runner_kind=github_actions returns a non-nil error and spawns ZERO runners,
// just like the detached fishhawk_dispatch_stage path. Both host-dispatch entry
// points must enforce the same pre-execution block.
func TestRunStage_BlocksHostDispatchAgainstActionsLockedRun(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	argv := captureArgv(t)

	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "implement", "pending")
	fb.getRunByID[runID] = Run{
		ID:                 runID.String(),
		State:              "running",
		RunnerKind:         "github_actions",
		RunnerKindResolved: true,
	}

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		Workflow:   "feature_change",
		Stage:      "implement",
		GitHubRepo: "x/y",
	})
	if err == nil {
		t.Fatal("expected a pre-dispatch block error for a github_actions-locked run")
	}
	if !strings.Contains(err.Error(), "github_actions") {
		t.Errorf("block error should name the locked kind: %v", err)
	}
	if len(*argv) != 0 {
		t.Errorf("a blocked run_stage must spawn ZERO runners, got argv %v", *argv)
	}
}

// TestRunStage_ArgvComposition_AcceptanceStage pins the E31.9 runner-argv
// contract: an acceptance stage carries --stage acceptance and, like a review
// stage, neither --plan-out (plan-only) nor --check-base-ref (implement-only) —
// its egress hosts + criteria ids arrive via --fetch-prompt, not argv, so no
// new flag is required.
func TestRunStage_ArgvComposition_AcceptanceStage(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	var capturedArgs []string
	origCmd := runStageCommand
	runStageCommand = func(_ string, args ...string) *exec.Cmd {
		capturedArgs = args
		return exec.Command("sh", "-c", "exit 0")
	}
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() {
		runStageCommand = origCmd
		runStageLookPath = exec.LookPath
	})

	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "acceptance", "succeeded")

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:      runID.String(),
		StageID:    stageID.String(),
		Workflow:   "feature_change",
		Stage:      "acceptance",
		GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	joined := strings.Join(capturedArgs, " ")
	if !strings.Contains(joined, "--stage acceptance") {
		t.Errorf("acceptance stage missing --stage acceptance\nfull: %s", joined)
	}
	if strings.Contains(joined, "--plan-out") {
		t.Errorf("acceptance stage must not include --plan-out: %v", capturedArgs)
	}
	if strings.Contains(joined, "--check-base-ref") {
		t.Errorf("acceptance stage must not include --check-base-ref: %v", capturedArgs)
	}
}

// --- detached-dispatch reaper (#1747) ---

// capturedReap records what the detached reaper's report closure was called
// with, so the reaper-branch tests can assert the parsed reason/detail/category.
type capturedReap struct {
	called   bool
	category string
	reason   string
	detail   string
	exitCode int
}

// startedShellCmd builds and Start()s a shell subprocess so reapDetachedRunner
// can Wait() on it. The body controls the exit code.
func startedShellCmd(t *testing.T, body string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sh", "-c", body)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake runner: %v", err)
	}
	return cmd
}

// writeReapLog writes a runner log file with the given lines and returns its path.
func writeReapLog(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "runner.log")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write reap log: %v", err)
	}
	return p
}

// (1) Non-zero exit with a parseable runner_failed line → reason/detail parsed,
// category C, the child's exit code reported.
func TestReapDetachedRunner_ParsesRunnerFailed(t *testing.T) {
	logPath := writeReapLog(t,
		`{"event":"runner_failed","reason":"acceptance_preview_provision_failed","detail":"no port"}`)
	cmd := startedShellCmd(t, "exit 7")

	var got capturedReap
	report := func(_ context.Context, category, reason, detail string, exitCode int) error {
		got = capturedReap{called: true, category: category, reason: reason, detail: detail, exitCode: exitCode}
		return nil
	}
	reapDetachedRunner(cmd, logPath, "run-1", "stage-1", report)

	if !got.called {
		t.Fatal("report was not called on a non-zero exit with a runner_failed line")
	}
	if got.category != "C" {
		t.Errorf("category = %q, want C", got.category)
	}
	if got.reason != "acceptance_preview_provision_failed" {
		t.Errorf("reason = %q", got.reason)
	}
	if got.detail != "no port" {
		t.Errorf("detail = %q", got.detail)
	}
	if got.exitCode != 7 {
		t.Errorf("exit_code = %d, want 7", got.exitCode)
	}
}

// The risk-item test: a runner_failed line LACKING a category still yields
// category C (the reaper never relies on a category field).
func TestReapDetachedRunner_NoCategoryFieldDefaultsC(t *testing.T) {
	logPath := writeReapLog(t, `{"event":"runner_failed","reason":"worktree_provision","detail":"disk full"}`)
	cmd := startedShellCmd(t, "exit 1")

	var got capturedReap
	reapDetachedRunner(cmd, logPath, "r", "s", func(_ context.Context, category, reason, detail string, exitCode int) error {
		got = capturedReap{called: true, category: category, reason: reason}
		return nil
	})
	if !got.called || got.category != "C" {
		t.Errorf("category = %q (called=%v), want C", got.category, got.called)
	}
	if got.reason != "worktree_provision" {
		t.Errorf("reason = %q", got.reason)
	}
}

// (2) Non-zero exit with NO runner_failed line → synthesized reason, category C.
func TestReapDetachedRunner_SynthesizesReasonWhenNoLine(t *testing.T) {
	logPath := writeReapLog(t, `not json`, `{"event":"stage_progress","turns":1}`)
	cmd := startedShellCmd(t, "exit 5")

	var got capturedReap
	reapDetachedRunner(cmd, logPath, "r", "s", func(_ context.Context, category, reason, detail string, exitCode int) error {
		got = capturedReap{called: true, category: category, reason: reason, exitCode: exitCode}
		return nil
	})
	if !got.called {
		t.Fatal("report was not called on a non-zero exit with no runner_failed line")
	}
	if got.category != "C" {
		t.Errorf("category = %q, want C", got.category)
	}
	if !strings.Contains(got.reason, "runner exited 5 before reporting a terminal state") {
		t.Errorf("reason = %q, want synthesized 'runner exited 5 before...'", got.reason)
	}
}

// (3) Zero exit → no report (the runner reported its own outcome).
func TestReapDetachedRunner_ZeroExitNoReport(t *testing.T) {
	logPath := writeReapLog(t, `{"event":"runner_completed","outcome":"ok"}`)
	cmd := startedShellCmd(t, "exit 0")

	called := false
	reapDetachedRunner(cmd, logPath, "r", "s", func(context.Context, string, string, string, int) error {
		called = true
		return nil
	})
	if called {
		t.Error("report was called on a zero exit; it must be a no-op")
	}
}

// A nil reporter is a safe no-op even on a non-zero exit (a caller with no
// backend client). Nothing to assert but the absence of a panic.
func TestReapDetachedRunner_NilReporterNoPanic(t *testing.T) {
	cmd := startedShellCmd(t, "exit 1")
	reapDetachedRunner(cmd, writeReapLog(t, `{"event":"runner_failed","reason":"x"}`), "r", "s", nil)
}

// zeroReapBackoff overrides reapReportBackoff with a same-length slice of
// zero-duration sleeps under t.Cleanup, so retry tests exercise the loop without
// real sleeps while preserving the attempt bound (len+1). Restored on cleanup.
func zeroReapBackoff(t *testing.T) {
	t.Helper()
	saved := reapReportBackoff
	zeroed := make([]time.Duration, len(saved))
	reapReportBackoff = zeroed
	t.Cleanup(func() { reapReportBackoff = saved })
}

// (4) The report fails K times then succeeds → it is retried and the eventually
// successful call carries category C / the parsed reason / the exit code, proving
// the report DID land on retry rather than dropping the stuck-'dispatched' report
// (#1763).
func TestReapDetachedRunner_ReportRetriedThenSucceeds(t *testing.T) {
	zeroReapBackoff(t)
	const failFirst = 2 // fail attempts 0 and 1, succeed on attempt 2
	logPath := writeReapLog(t,
		`{"event":"runner_failed","reason":"worktree_provision","detail":"disk full"}`)
	cmd := startedShellCmd(t, "exit 9")

	calls := 0
	var got capturedReap
	reapDetachedRunner(cmd, logPath, "r", "s", func(_ context.Context, category, reason, detail string, exitCode int) error {
		calls++
		if calls <= failFirst {
			return fmt.Errorf("transient POST failure %d", calls)
		}
		got = capturedReap{called: true, category: category, reason: reason, detail: detail, exitCode: exitCode}
		return nil
	})

	if calls != failFirst+1 {
		t.Fatalf("report called %d times, want %d (K failures then success)", calls, failFirst+1)
	}
	if !got.called || got.category != "C" || got.reason != "worktree_provision" || got.detail != "disk full" || got.exitCode != 9 {
		t.Errorf("successful retry carried %+v, want category C / worktree_provision / disk full / exit 9", got)
	}
}

// (5) The report fails on EVERY attempt → the loop is bounded to
// len(reapReportBackoff)+1 attempts and the reaper returns cleanly via the
// fail-closed stderr fallback (previously untested) without panicking (#1763).
func TestReapDetachedRunner_ReportExhaustsRetries(t *testing.T) {
	zeroReapBackoff(t)
	logPath := writeReapLog(t, `{"event":"runner_failed","reason":"x","detail":"y"}`)
	cmd := startedShellCmd(t, "exit 1")

	calls := 0
	reapDetachedRunner(cmd, logPath, "r", "s", func(context.Context, string, string, string, int) error {
		calls++
		return fmt.Errorf("always fails")
	})

	if want := len(reapReportBackoff) + 1; calls != want {
		t.Errorf("report called %d times, want %d (the bound must hold)", calls, want)
	}
}

// parseDetachedRunnerFailure: the LAST runner_failed line wins, non-matching
// lines are ignored, and an unreadable path yields ("", "").
func TestParseDetachedRunnerFailure(t *testing.T) {
	t.Run("last runner_failed wins", func(t *testing.T) {
		p := writeReapLog(t,
			`{"event":"runner_failed","reason":"first","detail":"a"}`,
			`{"event":"stage_progress","turns":2}`,
			`{"event":"runner_failed","reason":"second","detail":"b"}`)
		reason, detail := parseDetachedRunnerFailure(p)
		if reason != "second" || detail != "b" {
			t.Errorf("(%q,%q), want (second,b)", reason, detail)
		}
	})
	t.Run("no runner_failed line", func(t *testing.T) {
		p := writeReapLog(t, `{"event":"runner_completed","outcome":"ok"}`)
		if reason, detail := parseDetachedRunnerFailure(p); reason != "" || detail != "" {
			t.Errorf("(%q,%q), want empty", reason, detail)
		}
	})
	t.Run("unreadable path", func(t *testing.T) {
		if reason, detail := parseDetachedRunnerFailure(filepath.Join(t.TempDir(), "absent.log")); reason != "" || detail != "" {
			t.Errorf("(%q,%q), want empty for a missing file", reason, detail)
		}
	})
}

// Cross-boundary integration (#1747): a detached runner that writes a
// runner_failed line to its redirected log and exits non-zero drives the reaper
// to POST /v0/runs/{id}/stages/{id}/reap-failure with the parsed reason and
// category C — crossing the MCP-host → HTTP → handler seam via a real apiClient.
func TestSpawnRunnerStageDetached_ReaperReportsOverHTTP(t *testing.T) {
	type captured struct {
		path string
		body reapFailureRequest
	}
	gotCh := make(chan captured, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reap-failure") {
			var b reapFailureRequest
			_ = json.NewDecoder(r.Body).Decode(&b)
			gotCh <- captured{path: r.URL.Path, body: b}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"transitioned":true,"stage_state":"failed"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	api := newAPIClient(config{backendURL: srv.URL, apiToken: "tok-test"})
	runID, stageID := uuid.New(), uuid.New()
	report := func(ctx context.Context, category, reason, detail string, exitCode int) error {
		_, err := api.ReportStageFailure(ctx, runID, stageID, category, reason, detail, exitCode)
		return err
	}

	// Fake runner: emit a runner_failed line to stdout (redirected to the log by
	// spawnRunnerStageDetached) and exit non-zero.
	origCmd := runStageCommand
	runStageCommand = func(_ string, _ ...string) *exec.Cmd {
		return exec.Command("sh", "-c",
			`echo '{"event":"runner_failed","reason":"acceptance_preview_provision_failed","detail":"no port"}'; exit 7`)
	}
	t.Cleanup(func() { runStageCommand = origCmd })

	logPath, err := spawnRunnerStageDetached("/fake/runner", nil, os.Environ(),
		runID.String(), stageID.String(), report)
	if err != nil {
		t.Fatalf("spawnRunnerStageDetached: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(logPath) })

	select {
	case c := <-gotCh:
		wantPath := "/v0/runs/" + runID.String() + "/stages/" + stageID.String() + "/reap-failure"
		if c.path != wantPath {
			t.Errorf("path = %q, want %q", c.path, wantPath)
		}
		if c.body.Category != "C" {
			t.Errorf("category = %q, want C", c.body.Category)
		}
		if c.body.Reason != "acceptance_preview_provision_failed" {
			t.Errorf("reason = %q", c.body.Reason)
		}
		if c.body.Detail != "no port" {
			t.Errorf("detail = %q", c.body.Detail)
		}
		if c.body.ExitCode != 7 {
			t.Errorf("exit_code = %d, want 7", c.body.ExitCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reaper did not POST reap-failure within 5s")
	}
}

// --- (#1928) acceptance-dispatch admission on run_stage -----------------------

// TestRunStage_AcceptanceShortCircuit_NoSpawn: a synchronous acceptance
// run_stage whose admission short-circuits spawns NO runner and returns the
// settled stage.
func TestRunStage_AcceptanceShortCircuit_NoSpawn(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.admissionShortCircuit = true
	r := newResolver(srv, nil)

	spawned := 0
	origCmd := runStageCommand
	runStageCommand = func(_ string, _ ...string) *exec.Cmd {
		spawned++
		return exec.Command("sh", "-c", "exit 0")
	}
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() { runStageCommand = origCmd })

	runID := uuid.New()
	acceptanceID := uuid.NewString()
	seedStages(fb, runID,
		Stage{ID: uuid.NewString(), RunID: runID.String(), Type: "plan", State: "succeeded"},
		Stage{ID: uuid.NewString(), RunID: runID.String(), Type: "implement", State: "succeeded"},
		Stage{ID: acceptanceID, RunID: runID.String(), Type: "acceptance", State: "pending"},
	)

	_, out, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID: runID.String(), Workflow: "feature_change", Stage: "acceptance", GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if spawned != 0 {
		t.Errorf("a short-circuited acceptance run_stage must spawn NO runner, got %d", spawned)
	}
	if out.StageState != "succeeded" {
		t.Errorf("StageState = %q, want succeeded (settled)", out.StageState)
	}
	if out.Outcome != "short_circuited" {
		t.Errorf("Outcome = %q, want short_circuited", out.Outcome)
	}
	var noted bool
	for _, w := range out.Warnings {
		if strings.Contains(w, "short-circuited to a passed verdict") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("missing the short-circuit note; warnings: %v", out.Warnings)
	}
}

// TestRunStage_AcceptanceNeedsTarget_NoSpawnNoMarker (#1953): a needs_target
// admission whose declared target is unreachable REFUSES to spawn — no
// host-dispatch marker, no runner — and returns outcome=needs_target naming the
// expected head SHA. The stage stays pending for a clean re-dispatch.
func TestRunStage_AcceptanceNeedsTarget_NoSpawnNoMarker(t *testing.T) {
	origAttempts := acceptanceQuickProbeAttempts
	acceptanceQuickProbeAttempts = 1
	t.Cleanup(func() { acceptanceQuickProbeAttempts = origAttempts })

	// A target host that nothing listens on -> probe unreachable -> refuse.
	target := healthzServer(t, http.StatusOK, `{"git_sha":"abc1234def"}`)
	targetHost := hostOf(target.URL)
	target.Close()

	fb, srv := newFakeBackend(t)
	fb.admissionShortCircuit = false
	fb.admissionNeedsTarget = true
	fb.admissionTargetHosts = []string{targetHost}
	fb.admissionExpectedHeadSHA = probeExpectedSHA
	r := newResolver(srv, nil)

	spawned := 0
	origCmd := runStageCommand
	runStageCommand = func(_ string, _ ...string) *exec.Cmd {
		spawned++
		return exec.Command("sh", "-c", "exit 0")
	}
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() { runStageCommand = origCmd })

	runID := uuid.New()
	acceptanceID := uuid.NewString()
	accUUID, _ := uuid.Parse(acceptanceID)
	seedStages(fb, runID,
		Stage{ID: uuid.NewString(), RunID: runID.String(), Type: "plan", State: "succeeded"},
		Stage{ID: uuid.NewString(), RunID: runID.String(), Type: "implement", State: "succeeded"},
		Stage{ID: acceptanceID, RunID: runID.String(), Type: "acceptance", State: "pending"},
	)

	_, out, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID: runID.String(), Workflow: "feature_change", Stage: "acceptance", GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if spawned != 0 {
		t.Errorf("a needs_target refusal must spawn NO runner, got %d", spawned)
	}
	if n := fb.hostDispatchCalledByID[accUUID]; n != 0 {
		t.Errorf("host-dispatch marker calls = %d, want 0 (refusal fires before the marker)", n)
	}
	if out.Outcome != "needs_target" {
		t.Errorf("Outcome = %q, want needs_target", out.Outcome)
	}
	if out.NeedsTarget == nil {
		t.Fatal("NeedsTarget = nil, want the structured refusal")
	}
	if out.NeedsTarget.ExpectedHeadSHA != probeExpectedSHA || out.NeedsTarget.TargetHost != targetHost {
		t.Errorf("NeedsTarget = %+v, want host=%q sha=%q", out.NeedsTarget, targetHost, probeExpectedSHA)
	}
}

// TestRunStage_AcceptanceNeedsTargetVerified_SpawnsAsToday (#1953): a
// needs_target admission whose declared target is VERIFIED (serves the merge
// candidate) proceeds to spawn exactly as today.
func TestRunStage_AcceptanceNeedsTargetVerified_SpawnsAsToday(t *testing.T) {
	target := healthzServer(t, http.StatusOK, `{"git_sha":"abc1234def"}`)

	fb, srv := newFakeBackend(t)
	fb.admissionNeedsTarget = true
	fb.admissionTargetHosts = []string{hostOf(target.URL)}
	fb.admissionExpectedHeadSHA = probeExpectedSHA
	r := newResolver(srv, nil)
	argv := captureArgv(t)

	runID := uuid.New()
	acceptanceID := uuid.NewString()
	accUUID, _ := uuid.Parse(acceptanceID)
	seedStages(fb, runID,
		Stage{ID: uuid.NewString(), RunID: runID.String(), Type: "plan", State: "succeeded"},
		Stage{ID: uuid.NewString(), RunID: runID.String(), Type: "implement", State: "succeeded"},
		Stage{ID: acceptanceID, RunID: runID.String(), Type: "acceptance", State: "pending"},
	)

	_, out, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID: runID.String(), Workflow: "feature_change", Stage: "acceptance", GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if out.Outcome == "needs_target" || out.NeedsTarget != nil {
		t.Errorf("a verified target must proceed, got needs_target refusal: %+v", out.NeedsTarget)
	}
	if joined := strings.Join(*argv, " "); !strings.Contains(joined, "--stage acceptance") {
		t.Errorf("spawned argv missing --stage acceptance: %s", joined)
	}
	if n := fb.hostDispatchCalledByID[accUUID]; n != 1 {
		t.Errorf("host-dispatch marker calls = %d, want 1 (verified proceeds to spawn)", n)
	}
}

// TestRunStage_AcceptanceAdmissionFalse_SpawnsAsToday: short_circuited:false
// spawns exactly as today with the acceptance argv and no short-circuit warning.
func TestRunStage_AcceptanceAdmissionFalse_SpawnsAsToday(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.admissionShortCircuit = false
	r := newResolver(srv, nil)
	argv := captureArgv(t)

	runID := uuid.New()
	acceptanceID := uuid.NewString()
	seedStages(fb, runID,
		Stage{ID: uuid.NewString(), RunID: runID.String(), Type: "plan", State: "succeeded"},
		Stage{ID: uuid.NewString(), RunID: runID.String(), Type: "implement", State: "succeeded"},
		Stage{ID: acceptanceID, RunID: runID.String(), Type: "acceptance", State: "pending"},
	)

	_, out, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID: runID.String(), Workflow: "feature_change", Stage: "acceptance", GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if joined := strings.Join(*argv, " "); !strings.Contains(joined, "--stage acceptance") {
		t.Errorf("spawned argv missing --stage acceptance: %s", joined)
	}
	for _, w := range out.Warnings {
		if strings.Contains(w, "short-circuited") || strings.Contains(w, "fail-open") {
			t.Errorf("no short-circuit / fail-open warning on the normal no-op path; got %q", w)
		}
	}
}

// TestRunStage_AcceptanceAdmissionError_FailsOpen: an admission error appends a
// warning and the run_stage spawns as today.
func TestRunStage_AcceptanceAdmissionError_FailsOpen(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.admissionStatus = http.StatusInternalServerError
	r := newResolver(srv, nil)

	spawned := 0
	origCmd := runStageCommand
	runStageCommand = func(_ string, _ ...string) *exec.Cmd {
		spawned++
		return exec.Command("sh", "-c", "exit 0")
	}
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() { runStageCommand = origCmd })

	runID := uuid.New()
	acceptanceID := uuid.NewString()
	seedStages(fb, runID,
		Stage{ID: uuid.NewString(), RunID: runID.String(), Type: "plan", State: "succeeded"},
		Stage{ID: uuid.NewString(), RunID: runID.String(), Type: "implement", State: "succeeded"},
		Stage{ID: acceptanceID, RunID: runID.String(), Type: "acceptance", State: "pending"},
	)

	_, out, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID: runID.String(), Workflow: "feature_change", Stage: "acceptance", GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("runStage must fail open on an admission error, got: %v", err)
	}
	if spawned != 1 {
		t.Errorf("an admission error must fall through to spawn, got %d spawns", spawned)
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

// TestRunStage_AcceptanceAdmissionAuthzRejection_FailsClosed pins the #1928 authz
// concern: a 4xx admission rejection (403 cross_run_admission) is NOT a fail-open
// condition — the run_stage must HALT with a tool error and spawn NO runner rather
// than proceed after the run-subject authorization boundary rejected the request.
func TestRunStage_AcceptanceAdmissionAuthzRejection_FailsClosed(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.admissionStatus = http.StatusForbidden
	r := newResolver(srv, nil)

	spawned := 0
	origCmd := runStageCommand
	runStageCommand = func(_ string, _ ...string) *exec.Cmd {
		spawned++
		return exec.Command("sh", "-c", "exit 0")
	}
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() { runStageCommand = origCmd })

	runID := uuid.New()
	acceptanceID := uuid.NewString()
	seedStages(fb, runID,
		Stage{ID: uuid.NewString(), RunID: runID.String(), Type: "plan", State: "succeeded"},
		Stage{ID: uuid.NewString(), RunID: runID.String(), Type: "implement", State: "succeeded"},
		Stage{ID: acceptanceID, RunID: runID.String(), Type: "acceptance", State: "pending"},
	)

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID: runID.String(), Workflow: "feature_change", Stage: "acceptance", GitHubRepo: "x/y",
	})
	if err == nil {
		t.Fatal("a 4xx admission rejection must fail closed with a tool error, got nil")
	}
	if !strings.Contains(err.Error(), "rejected the dispatch") {
		t.Errorf("error = %q, want it to name the admission rejection", err)
	}
	if spawned != 0 {
		t.Errorf("a fail-closed admission rejection must spawn NO runner, got %d", spawned)
	}
}

// TestRunStage_AcceptanceAdmissionBareRoute404_FailsOpen pins the #1937 version-skew
// carve-out: a NEW fishhawk-mcp pointed at an OLD fishhawkd that never registered the
// acceptance-admission route answers from its stdlib http.ServeMux with the plain-text
// "404 page not found" and NO JSON error envelope, so apiError.Code is empty. That bare
// route-absent 404 must be reclassified into the transport fail-open path (NOT the 4xx
// fail-closed halt) so a version skew does not wedge every acceptance dispatch: nil
// error, exactly one spawn, and a warning naming probable version skew.
func TestRunStage_AcceptanceAdmissionBareRoute404_FailsOpen(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.admissionStatus = http.StatusNotFound
	fb.admissionErrBody = "404 page not found\n" // exact stdlib http.NotFound body
	r := newResolver(srv, nil)

	spawned := 0
	origCmd := runStageCommand
	runStageCommand = func(_ string, _ ...string) *exec.Cmd {
		spawned++
		return exec.Command("sh", "-c", "exit 0")
	}
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() { runStageCommand = origCmd })

	runID := uuid.New()
	acceptanceID := uuid.NewString()
	seedStages(fb, runID,
		Stage{ID: uuid.NewString(), RunID: runID.String(), Type: "plan", State: "succeeded"},
		Stage{ID: uuid.NewString(), RunID: runID.String(), Type: "implement", State: "succeeded"},
		Stage{ID: acceptanceID, RunID: runID.String(), Type: "acceptance", State: "pending"},
	)

	_, out, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID: runID.String(), Workflow: "feature_change", Stage: "acceptance", GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("a bare route-absent 404 must fail OPEN (version skew), got: %v", err)
	}
	if spawned != 1 {
		t.Errorf("a bare 404 must fall through to spawn, got %d spawns", spawned)
	}
	var warned bool
	for _, w := range out.Warnings {
		if strings.Contains(w, "may predate the admission endpoint (version skew)") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("missing the version-skew fail-open warning; warnings: %v", out.Warnings)
	}
}

// TestRunStage_AcceptanceAdmissionStageNotFound404_FailsClosed pins the other side of
// the #1937 carve-out: a 404 whose body DID decode into the OpenAPI error envelope
// (Code=="stage_not_found") is the registered handler positively saying "no such
// stage" — the backend evaluated the request and rejected it, so it stays in the 4xx
// fail-closed class: non-nil "rejected the dispatch" error and NO spawn.
func TestRunStage_AcceptanceAdmissionStageNotFound404_FailsClosed(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.admissionStatus = http.StatusNotFound
	fb.admissionErrBody = `{"error":{"code":"stage_not_found","message":"no such stage"}}`
	r := newResolver(srv, nil)

	spawned := 0
	origCmd := runStageCommand
	runStageCommand = func(_ string, _ ...string) *exec.Cmd {
		spawned++
		return exec.Command("sh", "-c", "exit 0")
	}
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() { runStageCommand = origCmd })

	runID := uuid.New()
	acceptanceID := uuid.NewString()
	seedStages(fb, runID,
		Stage{ID: uuid.NewString(), RunID: runID.String(), Type: "plan", State: "succeeded"},
		Stage{ID: uuid.NewString(), RunID: runID.String(), Type: "implement", State: "succeeded"},
		Stage{ID: acceptanceID, RunID: runID.String(), Type: "acceptance", State: "pending"},
	)

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID: runID.String(), Workflow: "feature_change", Stage: "acceptance", GitHubRepo: "x/y",
	})
	if err == nil {
		t.Fatal("a body-decoded stage_not_found 404 must fail closed with a tool error, got nil")
	}
	if !strings.Contains(err.Error(), "rejected the dispatch") {
		t.Errorf("error = %q, want it to name the admission rejection", err)
	}
	if spawned != 0 {
		t.Errorf("a fail-closed stage_not_found 404 must spawn NO runner, got %d", spawned)
	}
}

// TestRunStage_AcceptanceAdmissionBareRoute404_StageNotDispatchable_Halts pins
// the #1999 precedence: even the #1937 bare-404 version-skew carve-out must not
// override a POSITIVELY OBSERVED non-dispatchable stage state. When the bare
// route-absent 404 combines with a stage left 'running' by a mid-walk failure,
// the acceptanceStageState re-check at run_stage.go:853 wins over the skewWarn
// fail-open return at run_stage.go:857 — the dispatch halts with no spawn,
// exactly as the non-skew #1928 case does.
func TestRunStage_AcceptanceAdmissionBareRoute404_StageNotDispatchable_Halts(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.admissionStatus = http.StatusNotFound
	fb.admissionErrBody = "404 page not found\n" // exact stdlib http.NotFound body
	fb.admissionLeavesRunning = true
	r := newResolver(srv, nil)

	spawned := 0
	origCmd := runStageCommand
	runStageCommand = func(_ string, _ ...string) *exec.Cmd {
		spawned++
		return exec.Command("sh", "-c", "exit 0")
	}
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() {
		runStageCommand = origCmd
		runStageLookPath = exec.LookPath
	})

	runID := uuid.New()
	acceptanceID := uuid.NewString()
	seedStages(fb, runID,
		Stage{ID: uuid.NewString(), RunID: runID.String(), Type: "plan", State: "succeeded"},
		Stage{ID: uuid.NewString(), RunID: runID.String(), Type: "implement", State: "succeeded"},
		Stage{ID: acceptanceID, RunID: runID.String(), Type: "acceptance", State: "pending"},
	)

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID: runID.String(), Workflow: "feature_change", Stage: "acceptance", GitHubRepo: "x/y",
	})
	if err == nil {
		t.Fatal("a bare-404 skew combined with a non-dispatchable stage must still halt, got nil")
	}
	if !strings.Contains(err.Error(), "double-driving") {
		t.Errorf("error = %q, want it to name the double-drive guard (stage-state re-check must win over skewWarn)", err)
	}
	if spawned != 0 {
		t.Errorf("a stage left 'running' must spawn NO runner even under the bare-404 skew carve-out, got %d", spawned)
	}
}

// TestRunStage_AcceptanceAdmissionError_StageLeftRunning_FailsClosed pins the
// #1928 mid-walk concern: when the admission call 500s AND the failed short-circuit
// walk left the target stage 'running', the fail-open re-check observes the
// non-dispatchable state and HALTS rather than spawning a second runner against a
// partially-settled acceptance stage.
func TestRunStage_AcceptanceAdmissionError_StageLeftRunning_FailsClosed(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.admissionStatus = http.StatusInternalServerError
	fb.admissionLeavesRunning = true
	r := newResolver(srv, nil)

	spawned := 0
	origCmd := runStageCommand
	runStageCommand = func(_ string, _ ...string) *exec.Cmd {
		spawned++
		return exec.Command("sh", "-c", "exit 0")
	}
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() { runStageCommand = origCmd })

	runID := uuid.New()
	acceptanceID := uuid.NewString()
	seedStages(fb, runID,
		Stage{ID: uuid.NewString(), RunID: runID.String(), Type: "plan", State: "succeeded"},
		Stage{ID: uuid.NewString(), RunID: runID.String(), Type: "implement", State: "succeeded"},
		Stage{ID: acceptanceID, RunID: runID.String(), Type: "acceptance", State: "pending"},
	)

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID: runID.String(), Workflow: "feature_change", Stage: "acceptance", GitHubRepo: "x/y",
	})
	if err == nil {
		t.Fatal("a mid-walk 500 that left the stage running must fail closed, got nil")
	}
	if !strings.Contains(err.Error(), "double-driving") {
		t.Errorf("error = %q, want it to name the double-drive guard", err)
	}
	if spawned != 0 {
		t.Errorf("a stage left 'running' must spawn NO runner, got %d", spawned)
	}
}

// TestRunStage_AcceptanceShortCircuit_PostFetchFailure pins the #1928
// untested-error-path concern: when the short-circuit fires but the
// post-short-circuit stage fetch fails, the run still returns success with NO
// spawn — the degraded output carries the warning and an empty StageState.
func TestRunStage_AcceptanceShortCircuit_PostFetchFailure(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.admissionShortCircuit = true
	fb.stagesFailOnCall = 3 // resolveStageID(1), sibling guard(2), post-short-circuit fetch(3)
	r := newResolver(srv, nil)

	spawned := 0
	origCmd := runStageCommand
	runStageCommand = func(_ string, _ ...string) *exec.Cmd {
		spawned++
		return exec.Command("sh", "-c", "exit 0")
	}
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() { runStageCommand = origCmd })

	runID := uuid.New()
	acceptanceID := uuid.NewString()
	seedStages(fb, runID,
		Stage{ID: uuid.NewString(), RunID: runID.String(), Type: "plan", State: "succeeded"},
		Stage{ID: uuid.NewString(), RunID: runID.String(), Type: "implement", State: "succeeded"},
		Stage{ID: acceptanceID, RunID: runID.String(), Type: "acceptance", State: "pending"},
	)

	_, out, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID: runID.String(), Workflow: "feature_change", Stage: "acceptance", GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("a post-short-circuit fetch failure must still return success, got: %v", err)
	}
	if spawned != 0 {
		t.Errorf("a short-circuited acceptance run_stage must spawn NO runner, got %d", spawned)
	}
	if out.StageState != "" {
		t.Errorf("StageState = %q, want empty (degraded fetch)", out.StageState)
	}
	if out.StageWaitStatus != nil {
		t.Errorf("StageWaitStatus = %+v, want nil (degraded fetch)", out.StageWaitStatus)
	}
	var fetchWarn, scNote bool
	for _, w := range out.Warnings {
		if strings.Contains(w, "post-short-circuit stage fetch failed") {
			fetchWarn = true
		}
		if strings.Contains(w, "short-circuited to a passed verdict") {
			scNote = true
		}
	}
	if !fetchWarn {
		t.Errorf("missing the degraded-fetch warning; warnings: %v", out.Warnings)
	}
	if !scNote {
		t.Errorf("missing the short-circuit note; warnings: %v", out.Warnings)
	}
}
