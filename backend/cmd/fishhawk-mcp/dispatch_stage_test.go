package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The dispatch tests reuse the shared withFakeRunner / captureArgv /
// runStageCommand seam from run_stage_test.go UNCHANGED (same package) — the
// detached spawn redirects the child's stdout/stderr to a log file rather than
// reading a pipe, but the command-construction seam is identical.

// --- non-blocking contract (the core done-means) ---

// TestDispatchStage_NonBlockingReturnsHandle asserts the headline #1232
// property: dispatch returns the durable (run_id, stage_id) handle PROMPTLY —
// before a slow fake runner exits — with a non-terminal StageWaitStatus
// carrying poll_interval_seconds=30. A blocking implementation would hang on
// the child's sleep.
func TestDispatchStage_NonBlockingReturnsHandle(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	// The fake runner would block for 3s; the handler must return well before
	// that because the spawn is detached (only cmd.Start, not cmd.Wait).
	withFakeRunner(t, "sleep 3")

	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "implement", "running")

	start := time.Now()
	_, out, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID:      runID.String(),
		Workflow:   "feature_change",
		Stage:      "implement",
		GitHubRepo: "x/y",
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("dispatchStage: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("dispatch took %v; it must return without blocking on the runner (sleep 3)", elapsed)
	}
	if out.RunID != runID.String() {
		t.Errorf("RunID = %q, want %q", out.RunID, runID.String())
	}
	if out.StageID != stageID.String() {
		t.Errorf("StageID = %q, want resolved %q", out.StageID, stageID.String())
	}
	if out.StageWaitStatus == nil {
		t.Fatal("StageWaitStatus is nil; expected the freshly-dispatched (non-terminal) status")
	}
	if out.StageWaitStatus.Status != "running" {
		t.Errorf("StageWaitStatus.Status = %q, want running", out.StageWaitStatus.Status)
	}
	if out.StageWaitStatus.PollIntervalSeconds != suggestedStageWaitPollIntervalSeconds {
		t.Errorf("PollIntervalSeconds = %d, want %d (non-terminal stage advertises the poll cadence)",
			out.StageWaitStatus.PollIntervalSeconds, suggestedStageWaitPollIntervalSeconds)
	}
	if out.LogPath == "" {
		t.Error("LogPath should be set to the detached runner's redirected log")
	}
}

// --- argv parity with the synchronous run_stage path ---

// captureAllArgv records the argv of EVERY runStageCommand invocation (dispatch
// then run_stage) so the two paths' composed argv can be compared. Reuses the
// shared runStageCommand/runStageLookPath seam.
func captureAllArgv(t *testing.T) *[][]string {
	t.Helper()
	calls := new([][]string)
	origCmd := runStageCommand
	origLook := runStageLookPath
	runStageCommand = func(_ string, args ...string) *exec.Cmd {
		cp := append([]string(nil), args...)
		*calls = append(*calls, cp)
		return exec.Command("sh", "-c", "exit 0")
	}
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() {
		runStageCommand = origCmd
		runStageLookPath = origLook
	})
	return calls
}

// TestDispatchStage_ArgvParity_PlanStage asserts the dispatched argv is
// byte-identical to fishhawk_run_stage's for the SAME plan-stage input (shared
// composeRunnerArgv) AND carries the plan-only --plan-out flag (approval
// condition 1: every argv-affecting field, not just the common subset).
func TestDispatchStage_ArgvParity_PlanStage(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	calls := captureAllArgv(t)

	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "plan", "pending")

	if _, _, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID: runID.String(), Workflow: "feature_change", Stage: "plan",
		WorkingDir: "/tmp/checkout", GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
	}); err != nil {
		t.Fatalf("dispatchStage: %v", err)
	}
	if _, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID: runID.String(), Workflow: "feature_change", Stage: "plan",
		WorkingDir: "/tmp/checkout", GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
	}); err != nil {
		t.Fatalf("runStage: %v", err)
	}

	if len(*calls) != 2 {
		t.Fatalf("expected 2 spawn invocations (dispatch + run_stage), got %d", len(*calls))
	}
	dispatchArgv, runStageArgv := (*calls)[0], (*calls)[1]
	if strings.Join(dispatchArgv, " ") != strings.Join(runStageArgv, " ") {
		t.Errorf("dispatch argv != run_stage argv\n dispatch: %v\n run_stage: %v", dispatchArgv, runStageArgv)
	}
	if !strings.Contains(strings.Join(dispatchArgv, " "), "--plan-out /tmp/fishhawk-plan.json") {
		t.Errorf("plan-stage dispatch argv missing --plan-out: %v", dispatchArgv)
	}
	if strings.Contains(strings.Join(dispatchArgv, " "), "--check-base-ref") {
		t.Errorf("plan-stage dispatch argv should not carry --check-base-ref: %v", dispatchArgv)
	}
}

// TestDispatchStage_ArgvParity_ImplementStage asserts byte-identical argv for
// an implement-stage input AND that the implement-only --check-base-ref flag is
// present (approval condition 1).
func TestDispatchStage_ArgvParity_ImplementStage(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	calls := captureAllArgv(t)

	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "implement", "pending")

	in := DispatchStageInput{
		RunID: runID.String(), Workflow: "feature_change", Stage: "implement",
		GitHubRepo: "x/y", BaseBranch: "develop", PushAndOpenPR: boolPtr(true),
	}
	if _, _, err := r.dispatchStage(context.Background(), nil, in); err != nil {
		t.Fatalf("dispatchStage: %v", err)
	}
	if _, _, err := r.runStage(context.Background(), nil, RunStageInput{
		RunID: in.RunID, Workflow: in.Workflow, Stage: in.Stage,
		GitHubRepo: in.GitHubRepo, BaseBranch: in.BaseBranch, PushAndOpenPR: in.PushAndOpenPR,
	}); err != nil {
		t.Fatalf("runStage: %v", err)
	}

	if len(*calls) != 2 {
		t.Fatalf("expected 2 spawn invocations, got %d", len(*calls))
	}
	dispatchArgv, runStageArgv := (*calls)[0], (*calls)[1]
	if strings.Join(dispatchArgv, " ") != strings.Join(runStageArgv, " ") {
		t.Errorf("dispatch argv != run_stage argv\n dispatch: %v\n run_stage: %v", dispatchArgv, runStageArgv)
	}
	joined := strings.Join(dispatchArgv, " ")
	if !strings.Contains(joined, "--check-base-ref develop") {
		t.Errorf("implement-stage dispatch argv missing --check-base-ref develop: %v", dispatchArgv)
	}
	if strings.Contains(joined, "--plan-out") {
		t.Errorf("implement-stage dispatch argv should not carry --plan-out: %v", dispatchArgv)
	}
}

// --- fail-soft: post-dispatch stage fetch failure ---

// TestDispatchStage_PostFetchFailureWarnsNoError asserts the fail-soft branch:
// when the post-dispatch stage fetch fails (the SECOND /stages call), the
// handler returns the handle with a nil StageWaitStatus + a warning, never a
// tool error — the spawn already happened and the handle is durable.
func TestDispatchStage_PostFetchFailureWarnsNoError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeRunner(t, "exit 0")

	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "implement", "running")
	// Call 1 (resolveStageID) succeeds; call 2 (post-dispatch classify) 500s.
	fb.mu.Lock()
	fb.stagesFailOnCall = 2
	fb.mu.Unlock()

	_, out, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID:      runID.String(),
		Workflow:   "feature_change",
		Stage:      "implement",
		GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("dispatchStage should not error on a post-dispatch fetch failure: %v", err)
	}
	if out.StageWaitStatus != nil {
		t.Errorf("StageWaitStatus should be nil on a post-dispatch fetch failure, got %+v", out.StageWaitStatus)
	}
	if out.StageID != stageID.String() {
		t.Errorf("StageID = %q, want %q (the handle is still returned)", out.StageID, stageID.String())
	}
	found := false
	for _, w := range out.Warnings {
		if strings.Contains(w, "post-dispatch stage fetch failed") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a post-dispatch-fetch-failure warning, got %v", out.Warnings)
	}
}

// --- pre-dispatch runner_kind mismatch guardrail (#1355) ---

// TestDispatchStage_BlocksHostDispatchAgainstActionsLockedRun is the
// cross-boundary integration test for the #1355 host-dispatch guardrail: a
// detached dispatch against a run already LOCKED to runner_kind=github_actions
// must return a non-nil error AND spawn ZERO runners. It seeds the run on the
// fake backend so the guard reads the lock state through the real GET /v0/runs
// round-trip (api client -> MCP Run decode -> guard), and uses captureAllArgv
// to prove no runner invocation happened.
func TestDispatchStage_BlocksHostDispatchAgainstActionsLockedRun(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	calls := captureAllArgv(t)

	runID := uuid.New()
	stageID := uuid.New()
	// A stage exists, but the guard fires before stage resolution / spawn.
	seedStageOfType(fb, runID, stageID, "implement", "pending")
	fb.getRunByID[runID] = Run{
		ID:                 runID.String(),
		State:              "running",
		RunnerKind:         "github_actions",
		RunnerKindResolved: true,
	}

	_, _, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
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
	if len(*calls) != 0 {
		t.Errorf("a blocked dispatch must spawn ZERO runners, got %d invocations", len(*calls))
	}
}

// TestDispatchStage_LocalLockedRunPassesThrough asserts the allow side of the
// guardrail: a run LOCKED to runner_kind=local proceeds to spawn exactly one
// runner (the host dispatch matches the resolved local channel).
func TestDispatchStage_LocalLockedRunPassesThrough(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	calls := captureAllArgv(t)

	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "implement", "running")
	fb.getRunByID[runID] = Run{
		ID:                 runID.String(),
		State:              "running",
		RunnerKind:         "local",
		RunnerKindResolved: true,
	}

	_, _, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID:      runID.String(),
		Workflow:   "feature_change",
		Stage:      "implement",
		GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("a local-locked run must dispatch, got %v", err)
	}
	if len(*calls) != 1 {
		t.Errorf("a local-locked dispatch must spawn exactly one runner, got %d", len(*calls))
	}
}

// --- fail-closed: missing binary ---

// TestDispatchStage_MissingBinaryReturnsCleanError asserts the fail-closed
// resolver-error branch when fishhawk-runner does not resolve.
func TestDispatchStage_MissingBinaryReturnsCleanError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeRunnerMissing(t)
	withFakeExecutable(t, t.TempDir(), false /* no sibling */)

	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "implement", "pending")

	_, _, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID:      runID.String(),
		Workflow:   "feature_change",
		Stage:      "implement",
		GitHubRepo: "x/y",
	})
	if err == nil {
		t.Fatal("expected missing-binary error")
	}
	if !strings.Contains(err.Error(), "fishhawk-runner not on PATH") {
		t.Errorf("err should mention PATH: %v", err)
	}
}

// --- fail-closed: invalid UUIDs (error before spawn) ---

func TestDispatchStage_InvalidUUIDsErrorBeforeSpawn(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	// A fake runner is wired, but the UUID validation must error before any
	// spawn — a spawn here would be the bug.
	spawned := false
	origCmd := runStageCommand
	runStageCommand = func(_ string, _ ...string) *exec.Cmd {
		spawned = true
		return exec.Command("sh", "-c", "exit 0")
	}
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() { runStageCommand = origCmd })

	t.Run("invalid run_id", func(t *testing.T) {
		_, _, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
			RunID: "not-a-uuid", Workflow: "w", Stage: "plan",
		})
		if err == nil || !strings.Contains(err.Error(), "run_id") {
			t.Fatalf("expected run_id UUID error, got %v", err)
		}
	})

	t.Run("invalid stage_id", func(t *testing.T) {
		_, _, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
			RunID: uuid.NewString(), StageID: "bad", Workflow: "w", Stage: "plan",
		})
		if err == nil || !strings.Contains(err.Error(), "stage_id") {
			t.Fatalf("expected stage_id UUID error, got %v", err)
		}
	})

	if spawned {
		t.Error("the runner must not be spawned when a UUID is invalid")
	}
}

// --- no-pipe-deadlock: high-output detached runner ---

// TestDispatchStage_HighOutputDoesNotBlock asserts the redirect-to-file
// decision is load-bearing: a fake runner emitting far more than a pipe's
// kernel buffer (~64KiB) (a) lets the handler return promptly AND (b) actually
// FINISHES writing ALL of its output. The second assertion is what makes the
// test non-vacuous: an implementation that attached stdout/stderr to an UNREAD
// pipe would also let cmd.Start and the handler return promptly, but the child
// would block forever once the ~64KiB pipe buffer filled and would never write
// the full ~203KiB. By waiting for the log file to reach the complete output
// size we prove the writer got past the pipe-buffer block point — i.e. the
// no-pipe-deadlock mitigation, not merely a prompt return.
func TestDispatchStage_HighOutputDoesNotBlock(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	// 3200 lines of 64 'x' + newline = 65 bytes each = 208000 bytes (~203KiB),
	// well over a 64KiB pipe buffer. A pipe with no reader would block the writer
	// at ~64KiB; a file does not, so the full byte count must eventually land.
	const (
		lineBytes  = 65
		lineCount  = 3200
		wantOutput = lineBytes * lineCount
	)
	withFakeRunner(t, `i=0; while [ $i -lt 3200 ]; do printf '%s\n' 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx'; i=$((i+1)); done`)

	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "implement", "running")

	start := time.Now()
	_, out, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID:      runID.String(),
		Workflow:   "feature_change",
		Stage:      "implement",
		GitHubRepo: "x/y",
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("dispatchStage: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("dispatch took %v with a high-output runner; the redirect-to-file must not block", elapsed)
	}
	if out.LogPath == "" {
		t.Fatal("LogPath should be set")
	}

	// The detached child writes asynchronously; poll the log until it reaches the
	// full output size. If output went to an unread pipe the writer would deadlock
	// at the kernel buffer (~64KiB) and the file would never reach wantOutput.
	deadline := time.Now().Add(5 * time.Second)
	var size int64
	for time.Now().Before(deadline) {
		fi, statErr := os.Stat(out.LogPath)
		if statErr == nil {
			size = fi.Size()
			if size >= wantOutput {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if size < wantOutput {
		t.Errorf("log reached only %d bytes, want >= %d: the high-output writer did not finish, "+
			"i.e. it blocked on a full pipe instead of redirecting to a file", size, wantOutput)
	}
}

// --- repo soft-fail: github_repo empty + origin auto-detect fails ---

// TestDispatchStage_RepoDetectSoftFail exercises the empty-github_repo branch
// that mirrors run_stage's soft-fail rule: when origin auto-detect fails,
// push_and_open_pr=true is a hard error (a PR needs a repo) while
// push_and_open_pr=false appends a warning and proceeds to spawn. The other
// dispatch tests all set github_repo:"x/y", so this is the only case that runs
// runStageDetectGitHubRepo.
func TestDispatchStage_RepoDetectSoftFail(t *testing.T) {
	t.Run("push true is a hard error", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		withFakeRunner(t, "exit 0")
		// Detector fails (no github origin). A spawn here would be the bug.
		withFakeGitRemote(t, "", errors.New("no origin"))
		spawned := false
		origCmd := runStageCommand
		runStageCommand = func(_ string, _ ...string) *exec.Cmd {
			spawned = true
			return exec.Command("sh", "-c", "exit 0")
		}
		t.Cleanup(func() { runStageCommand = origCmd })

		// Seed a stage so stage resolution (step 2) succeeds and execution reaches
		// the repo-detect branch (step 4) — the path under test.
		runID := uuid.New()
		stageID := uuid.New()
		seedStageOfType(fb, runID, stageID, "implement", "pending")

		_, _, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
			RunID: runID.String(), Workflow: "feature_change", Stage: "implement",
			PushAndOpenPR: boolPtr(true),
		})
		if err == nil || !strings.Contains(err.Error(), "could not detect from origin") {
			t.Fatalf("expected a hard repo-detect error when push_and_open_pr=true, got %v", err)
		}
		if spawned {
			t.Error("the runner must not be spawned when repo detection fails under push_and_open_pr=true")
		}
	})

	t.Run("push false warns and proceeds", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		withFakeRunner(t, "exit 0")
		withFakeGitRemote(t, "", errors.New("no origin"))

		runID := uuid.New()
		stageID := uuid.New()
		seedStageOfType(fb, runID, stageID, "implement", "running")

		_, out, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
			RunID: runID.String(), Workflow: "feature_change", Stage: "implement",
			PushAndOpenPR: boolPtr(false),
		})
		if err != nil {
			t.Fatalf("dispatchStage should soft-fail (warn + proceed) when push_and_open_pr=false: %v", err)
		}
		if out.StageID != stageID.String() {
			t.Errorf("StageID = %q, want %q (the handle is still returned)", out.StageID, stageID.String())
		}
		found := false
		for _, w := range out.Warnings {
			if strings.Contains(w, "origin auto-detect failed") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected an origin-auto-detect-failed warning, got %v", out.Warnings)
		}
	})
}

// --- MCP CallTool round-trip (schema binding, approval condition 2) ---

// TestDispatchStage_CallToolRoundTrip drives fishhawk_dispatch_stage through a
// real MCP CallTool over an in-memory transport, so a schema/serialization
// binding error cannot hide behind the handler-level tests: it asserts the
// INPUT schema decodes the new fields (run_id/workflow/stage/github_repo/
// base_branch/push_and_open_pr/runner_binary) and the OUTPUT — the
// (run_id, stage_id) handle + StageWaitStatus + log_path — serializes back over
// the wire.
func TestDispatchStage_CallToolRoundTrip(t *testing.T) {
	ctx := context.Background()
	fb, srv := newFakeBackend(t)
	resolver := newResolver(srv, nil)
	withFakeRunner(t, "exit 0")

	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "implement", "running")

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0"}, nil)
	registerDispatchStage(server, resolver)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
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

	res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_dispatch_stage",
		Arguments: map[string]any{
			"run_id":           runID.String(),
			"workflow":         "feature_change",
			"stage":            "implement",
			"github_repo":      "x/y",
			"base_branch":      "main",
			"push_and_open_pr": false,
			"runner_binary":    "/fake/fishhawk-runner",
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
	}
	if res.StructuredContent == nil {
		t.Fatal("StructuredContent is nil; the typed output did not serialize")
	}

	// Re-marshal the wire-decoded structured content and decode into the typed
	// output to assert the round-trip binding.
	raw, merr := json.Marshal(res.StructuredContent)
	if merr != nil {
		t.Fatalf("marshal StructuredContent: %v", merr)
	}
	var out DispatchStageOutput
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("decode DispatchStageOutput from wire: %v", uerr)
	}
	if out.RunID != runID.String() {
		t.Errorf("RunID = %q, want %q", out.RunID, runID.String())
	}
	if out.StageID != stageID.String() {
		t.Errorf("StageID = %q, want %q", out.StageID, stageID.String())
	}
	if out.StageWaitStatus == nil {
		t.Fatal("StageWaitStatus did not round-trip")
	}
	if out.StageWaitStatus.Status != "running" || out.StageWaitStatus.PollIntervalSeconds != suggestedStageWaitPollIntervalSeconds {
		t.Errorf("StageWaitStatus = %+v, want running with poll=%d", out.StageWaitStatus, suggestedStageWaitPollIntervalSeconds)
	}
	if out.LogPath == "" {
		t.Error("LogPath did not round-trip")
	}
}
