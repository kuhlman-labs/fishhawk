package main

import (
	"context"
	"errors"
	"os/exec"
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

// seedStage seeds a fake-backend stage so the post-run fetch
// populates StageState in the tool output.
func seedStage(fb *fakeBackend, runID, stageID uuid.UUID, state string) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.stagesByRun[runID] = []Stage{{ID: stageID.String(), RunID: runID.String(), State: state, Type: "plan"}}
}

// --- input validation ---

func TestRunStage_RequiresAllFourIDs(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeRunner(t, "exit 0")

	cases := []struct {
		name string
		in   RunStageInput
	}{
		{"missing run_id", RunStageInput{StageID: uuid.NewString(), Workflow: "w", Stage: "plan"}},
		{"missing stage_id", RunStageInput{RunID: uuid.NewString(), Workflow: "w", Stage: "plan"}},
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

// --- binary resolution ---

func TestRunStage_MissingBinaryReturnsCleanError(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeRunnerMissing(t)

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:    uuid.NewString(),
		StageID:  uuid.NewString(),
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
	seedStage(fb, runID, stageID, "succeeded")

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
	if strings.Contains(strings.Join(capturedArgs, " "), "--plan-out") {
		t.Errorf("implement stage should not include --plan-out: %v", capturedArgs)
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
	seedStage(fb, runID, stageID, "succeeded")

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:         runID.String(),
		StageID:       stageID.String(),
		Workflow:      "feature_change",
		Stage:         "implement",
		GitHubRepo:    "x/y",
		PushAndOpenPR: true,
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if strings.Contains(strings.Join(capturedArgs, " "), "--no-pr") {
		t.Errorf("--no-pr should be absent when push_and_open_pr=true: %v", capturedArgs)
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
	})
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if len(out.Warnings) == 0 {
		t.Error("expected a warning about auto-detect")
	}
}

func TestRunStage_GitHubRepoAutoDetectFails_WithPushErrors(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeGitRemote(t, "", errors.New("nope"))
	withFakeRunner(t, "exit 0")

	_, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID:         uuid.NewString(),
		StageID:       uuid.NewString(),
		Workflow:      "w",
		Stage:         "implement",
		PushAndOpenPR: true,
	})
	if err == nil || !strings.Contains(err.Error(), "github_repo") {
		t.Fatalf("expected github_repo error, got %v", err)
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
	seedStage(fb, runID, stageID, "running")

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

// --- helpers + parsers ---

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
