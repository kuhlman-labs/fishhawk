package main

import (
	"context"
	"encoding/json"
	"errors"
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
	seedStageOfType(fb, runID, stageID, "implement", "pending")

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
	// A freshly-dispatched stage sits at 'pending' pre-run (the sibling-in-flight
	// guard, #1872, rejects dispatching a target already 'running'); the wait
	// status must still be non-terminal and carry the poll cadence.
	if out.StageWaitStatus.Status != "pending" {
		t.Errorf("StageWaitStatus.Status = %q, want pending", out.StageWaitStatus.Status)
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
	seedStageOfType(fb, runID, stageID, "implement", "pending")
	// Three /stages calls now fire: (1) resolveStageID, (2) the sibling-in-flight
	// guard (#1872), (3) the post-dispatch classify. Fail the THIRD so the
	// post-dispatch classify errors (the guard's call-2 succeeds — target pending,
	// no in-flight sibling — and allows the spawn).
	fb.mu.Lock()
	fb.stagesFailOnCall = 3
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
	seedStageOfType(fb, runID, stageID, "implement", "pending")
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

// TestDispatchStage_BlocksSiblingInFlight is the cross-boundary integration
// test proving the sibling-in-flight guard (#1872) is wired into the detached
// dispatch path: a dispatch while a sibling stage is still running must return a
// non-nil error AND spawn ZERO runners. It seeds a running implement sibling
// through the real GET /v0/runs/{run_id}/stages round-trip and uses
// captureAllArgv to prove no runner invocation happened.
func TestDispatchStage_BlocksSiblingInFlight(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	calls := captureAllArgv(t)

	runID := uuid.New()
	implID := uuid.NewString()
	acceptanceID := uuid.NewString()
	seedStages(fb, runID,
		Stage{ID: implID, RunID: runID.String(), Type: "implement", State: "running"},
		Stage{ID: acceptanceID, RunID: runID.String(), Type: "acceptance", State: "pending"},
	)

	_, _, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
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
	if len(*calls) != 0 {
		t.Errorf("a blocked dispatch must spawn ZERO runners, got %d invocations", len(*calls))
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
	seedStageOfType(fb, runID, stageID, "implement", "pending")

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
		seedStageOfType(fb, runID, stageID, "implement", "pending")

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
	seedStageOfType(fb, runID, stageID, "implement", "pending")

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
	if out.StageWaitStatus.Status != "pending" || out.StageWaitStatus.PollIntervalSeconds != suggestedStageWaitPollIntervalSeconds {
		t.Errorf("StageWaitStatus = %+v, want pending with poll=%d", out.StageWaitStatus, suggestedStageWaitPollIntervalSeconds)
	}
	if out.LogPath == "" {
		t.Error("LogPath did not round-trip")
	}
}

// TestDispatchStage_AcceptanceStage_ResolvesAndSpawns pins the E31.9 dispatch
// surface: dispatching a stage-type acceptance resolves the acceptance stage id
// from (run_id, "acceptance") and spawns the detached runner (fake binary),
// returning the durable handle with a non-terminal StageWaitStatus — exactly
// like an implement dispatch, no new argv path.
func TestDispatchStage_AcceptanceStage_ResolvesAndSpawns(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	calls := captureAllArgv(t)

	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "acceptance", "pending")

	_, out, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID:      runID.String(),
		Workflow:   "feature_change",
		Stage:      "acceptance",
		GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("dispatchStage: %v", err)
	}
	if out.StageID != stageID.String() {
		t.Errorf("StageID = %q, want resolved %q", out.StageID, stageID.String())
	}
	if out.StageWaitStatus == nil || out.StageWaitStatus.Status != "pending" {
		t.Fatalf("StageWaitStatus = %+v, want status pending", out.StageWaitStatus)
	}
	if len(*calls) == 0 {
		t.Fatal("expected the detached runner to be spawned")
	}
	joined := strings.Join((*calls)[0], " ")
	if !strings.Contains(joined, "--stage acceptance") {
		t.Errorf("dispatched argv missing --stage acceptance\nfull: %s", joined)
	}
	if strings.Contains(joined, "--plan-out") || strings.Contains(joined, "--check-base-ref") {
		t.Errorf("acceptance dispatch must not carry --plan-out/--check-base-ref: %s", joined)
	}
}

// TestDispatchStage_ReaperReportsSpawnFailure is the call-site wiring test for
// the detached-dispatch reaper (#1747): driving r.dispatchStage end-to-end with
// a fake runner that dies non-zero BEFORE reporting a terminal stage state makes
// the reaper POST /v0/runs/{id}/stages/{id}/reap-failure with the parsed reason
// and category C. It exercises the reporter closure dispatchStage threads into
// spawnRunnerStageDetached (the new signature).
func TestDispatchStage_ReaperReportsSpawnFailure(t *testing.T) {
	runID := uuid.New()
	stageID := uuid.New()

	gotCh := make(chan reapFailureRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reap-failure"):
			var b reapFailureRequest
			_ = json.NewDecoder(r.Body).Decode(&b)
			gotCh <- b
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"transitioned":true,"stage_state":"failed"}`))
		case strings.HasSuffix(r.URL.Path, "/stages"):
			// resolveStageID + the post-dispatch wait-status classify both read this.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []Stage{{ID: stageID.String(), RunID: runID.String(), Type: "implement", State: "pending"}},
			})
		default:
			// guardHostDispatch's GET /v0/runs/{id}: an unlocked run so the guard passes.
			_ = json.NewEncoder(w).Encode(Run{ID: runID.String(), Repo: "x/y", State: "running"})
		}
	}))
	defer srv.Close()

	r := &runResolver{
		api:    newAPIClient(config{backendURL: srv.URL, apiToken: "tok-test"}),
		getenv: func(string) string { return "" },
	}

	// Fake runner: emit a runner_failed line to stdout (redirected to the detached
	// log) and exit non-zero.
	withFakeRunner(t, `echo '{"event":"runner_failed","reason":"acceptance_preview_provision_failed","detail":"boom"}'; exit 7`)

	out, _, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID: runID.String(), Workflow: "feature_change", Stage: "implement",
		GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
	})
	_ = out
	if err != nil {
		t.Fatalf("dispatchStage: %v", err)
	}

	select {
	case b := <-gotCh:
		if b.Category != "C" {
			t.Errorf("category = %q, want C", b.Category)
		}
		if b.Reason != "acceptance_preview_provision_failed" {
			t.Errorf("reason = %q", b.Reason)
		}
		if b.Detail != "boom" {
			t.Errorf("detail = %q", b.Detail)
		}
		if b.ExitCode != 7 {
			t.Errorf("exit_code = %d, want 7", b.ExitCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reaper did not POST reap-failure within 5s")
	}
}

// --- (T9/T10) manual-dispatch spawn-evidence vocabulary pin (#1905) ----------

// dispatchAutoDriveFake is a self-contained backend for the record-act tests:
// it serves the endpoints the dispatch path touches (GET run unlocked, GET
// stages with one implement stage, POST /auto-drive/acts) and captures the
// recorded act. recordStatus, when non-2xx, drives the best-effort failure
// branch. Kept local to dispatch_stage_test.go so the shared fakeBackend stays
// unchanged.
type dispatchAutoDriveFake struct {
	mu           sync.Mutex
	runID        uuid.UUID
	stageID      uuid.UUID
	acts         []RecordAutoDriveAct
	actCalledN   int
	recordStatus int // 0 -> 200; non-2xx drives the failure branch
}

func (f *dispatchAutoDriveFake) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/auto-drive/acts"):
			var body RecordAutoDriveAct
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.acts = append(f.acts, body)
			f.actCalledN++
			status := f.recordStatus
			f.mu.Unlock()
			if status != 0 && status != http.StatusOK {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"code":"auto_drive_record_failed","message":"boom"}}`))
				return
			}
			_ = json.NewEncoder(w).Encode(RecordAutoDriveActResult{
				RunID: f.runID.String(), Category: CategoryRunAutoDriven, Act: "dispatch",
				Action: body.Action, Stage: body.Stage, Source: body.Source, Sequence: 1,
			})
		case strings.HasSuffix(r.URL.Path, "/stages"):
			// resolveStageID, the sibling-in-flight guard, and the post-dispatch
			// classify all read this.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []Stage{{ID: f.stageID.String(), RunID: f.runID.String(), Type: "implement", State: "pending"}},
			})
		default:
			// guardHostDispatch's GET /v0/runs/{id}: an unlocked run so the guard passes.
			_ = json.NewEncoder(w).Encode(Run{ID: f.runID.String(), Repo: "x/y", State: "running"})
		}
	}
}

// TestDispatchStage_RecordsAutoDriveActBeforeSpawn is the canonical-vocabulary
// pin at the producer: a successful dispatch records EXACTLY ONE run_auto_driven
// spawn-evidence act whose Action == autoDriveDispatchActionName (the SHARED
// constant, not a duplicated literal — so the two callers cannot drift),
// Source == dispatchStageSourceTag ('fishhawk_dispatch_stage'), and Stage == the
// resolved stage type — recorded BEFORE the runner spawn (the record-before-
// spawn ordering). T11 (drive_run_test.go) then proves this row lets a re-invoked
// drive read the stage as live instead of dispatched_stale.
func TestDispatchStage_RecordsAutoDriveActBeforeSpawn(t *testing.T) {
	f := &dispatchAutoDriveFake{runID: uuid.New(), stageID: uuid.New()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	r := &runResolver{
		api:    newAPIClient(config{backendURL: srv.URL, apiToken: "tok"}),
		getenv: func(string) string { return "" },
	}

	// Capture how many acts were recorded at the moment the runner spawns, to
	// prove the record preceded the spawn (record-before-spawn ordering).
	recordSeenAtSpawn := -1
	origCmd := runStageCommand
	runStageCommand = func(_ string, _ ...string) *exec.Cmd {
		f.mu.Lock()
		recordSeenAtSpawn = f.actCalledN
		f.mu.Unlock()
		return exec.Command("sh", "-c", "exit 0")
	}
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() { runStageCommand = origCmd })

	_, _, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID: f.runID.String(), Workflow: "feature_change", Stage: "implement",
		GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
	})
	if err != nil {
		t.Fatalf("dispatchStage: %v", err)
	}

	f.mu.Lock()
	acts := append([]RecordAutoDriveAct(nil), f.acts...)
	f.mu.Unlock()
	if len(acts) != 1 {
		t.Fatalf("recorded %d acts, want exactly 1", len(acts))
	}
	if acts[0].Action != autoDriveDispatchActionName {
		t.Errorf("act Action = %q, want the shared constant autoDriveDispatchActionName (%q)", acts[0].Action, autoDriveDispatchActionName)
	}
	if acts[0].Source != dispatchStageSourceTag {
		t.Errorf("act Source = %q, want %q", acts[0].Source, dispatchStageSourceTag)
	}
	if acts[0].Stage != "implement" {
		t.Errorf("act Stage = %q, want implement (the resolved stage type)", acts[0].Stage)
	}
	if recordSeenAtSpawn != 1 {
		t.Errorf("record-before-spawn ordering violated: %d acts recorded when the runner spawned, want 1", recordSeenAtSpawn)
	}
}

// TestDispatchStage_RecordActFailure_WarnsAndProceeds pins the best-effort
// branch (T10): when POST /auto-drive/acts fails (500 — including the
// insufficient_scope case on a token lacking write:approvals), the dispatch
// STILL proceeds (no tool error, runner spawned) and the output carries a
// warning naming the degraded stale detection. The record is staleness
// evidence, not an authorization gate, so it must never block the core manual
// recovery verb.
func TestDispatchStage_RecordActFailure_WarnsAndProceeds(t *testing.T) {
	f := &dispatchAutoDriveFake{runID: uuid.New(), stageID: uuid.New(), recordStatus: http.StatusInternalServerError}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	r := &runResolver{
		api:    newAPIClient(config{backendURL: srv.URL, apiToken: "tok"}),
		getenv: func(string) string { return "" },
	}

	spawned := 0
	origCmd := runStageCommand
	runStageCommand = func(_ string, _ ...string) *exec.Cmd {
		spawned++
		return exec.Command("sh", "-c", "exit 0")
	}
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() { runStageCommand = origCmd })

	_, out, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID: f.runID.String(), Workflow: "feature_change", Stage: "implement",
		GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
	})
	if err != nil {
		t.Fatalf("dispatchStage must proceed on a record-act failure (best-effort), got error: %v", err)
	}
	if spawned != 1 {
		t.Errorf("the dispatch must still spawn exactly one runner despite the record failure, got %d", spawned)
	}
	var warned bool
	for _, w := range out.Warnings {
		if strings.Contains(w, "dispatched_stale") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning naming the degraded stale detection; warnings: %v", out.Warnings)
	}
}
