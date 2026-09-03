package mcpserver

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

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
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

// TestDispatchStage_ReturnsAwaitPointer is the #2491 done-means: a successful
// dispatch returns next_step pointing at fishhawk_await_stage with the RESOLVED
// run_id + stage_id (not the raw input) pre-filled, so the durable handle comes
// with the terminal wait to call on it.
func TestDispatchStage_ReturnsAwaitPointer(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeRunner(t, "exit 0")

	runID := uuid.New()
	stageID := uuid.New()
	// Auto-resolve the stage id from (run_id, type): the pointer must carry the
	// RESOLVED id, not a raw input the caller never passed.
	seedStageOfType(fb, runID, stageID, "implement", "pending")

	_, out, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID:      runID.String(),
		Workflow:   "feature_change",
		Stage:      "implement",
		GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("dispatchStage: %v", err)
	}
	if out.NextStep == nil {
		t.Fatal("NextStep = nil; a successful dispatch must return the fishhawk_await_stage pointer")
	}
	if out.NextStep.Action != "fishhawk_await_stage" {
		t.Errorf("NextStep.Action = %q, want fishhawk_await_stage", out.NextStep.Action)
	}
	if got := out.NextStep.Params["run_id"]; got != runID.String() {
		t.Errorf("NextStep.Params[run_id] = %q, want %s", got, runID)
	}
	if got := out.NextStep.Params["stage_id"]; got != stageID.String() {
		t.Errorf("NextStep.Params[stage_id] = %q, want the RESOLVED %s", got, stageID)
	}
	if got := out.NextStep.Params["stage"]; got != "implement" {
		t.Errorf("NextStep.Params[stage] = %q, want implement", got)
	}
}

// TestDispatchStage_AwaitPointerNamesAmendmentRelease is the #2588 behavioural
// pin for proposal 3: the pointer dispatch returns must advertise what the wait
// can now OBSERVE — a mid-stage scope amendment — so this verb no longer
// recommends a follow-up call blind to the one event it exists to enable. A
// doc-only revert to "block until the dispatched stage settles" fails here
// instead of drifting silently.
func TestDispatchStage_AwaitPointerNamesAmendmentRelease(t *testing.T) {
	next := awaitStageNextStep(uuid.New().String(), uuid.New().String(), "implement")
	if next == nil {
		t.Fatal("awaitStageNextStep returned nil")
	}
	lower := strings.ToLower(next.Reason)
	for _, want := range []string{
		"scope amendment",       // the event the wait now observes
		"amendment_pending",     // the status it surfaces as
		"list_scope_amendments", // and that no companion poll is needed
	} {
		if !strings.Contains(lower, want) {
			t.Errorf("await next_step reason must carry %q; got %q", want, next.Reason)
		}
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

// TestDispatchStage_HostDispatchMarkerFails_NoSpawn pins the #1912 fail-closed
// contract (plan test c): when the host-dispatch marker call 4xxes (or errors),
// fishhawk_dispatch_stage returns a tool error and spawns NO runner — an unmarked
// spawn would recreate the ambiguity #1912 removes.
func TestDispatchStage_HostDispatchMarkerFails_NoSpawn(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	calls := captureAllArgv(t)

	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "implement", "awaiting_host_dispatch")
	fb.hostDispatchStatus = http.StatusConflict // the marker 4xx -> fail closed

	_, _, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
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

// postDispatchStagesCall is the ordinal (1-based, per run id) of the
// post-dispatch classify read of GET /v0/runs/{id}/stages: (1) resolveStageID,
// (2) the sibling-in-flight guard (#1872), (3) the runner self-host bootstrap
// advisory's plan resolution (guardRunnerSelfHost -> tryGetPlanForRun, E64.5 /
// #3086), (4) the post-dispatch classify. It is BOTH the value both
// ordinal-injection tests assign to fb.stagesFailOnCall and the expected total
// stages-read count they assert afterwards, so a future extra or shifted stages
// read fails with a NAMED count mismatch instead of silently re-targeting the
// injected 500 onto a different call. Shared by the dispatch and
// acceptance-short-circuit users of the knob (both take the same four reads).
const postDispatchStagesCall = 4

// withStubbedDispatchSpawn replaces the injectable detached-spawn seam
// (dispatchSpawnDetached) with a stub returning a fixed fake log path and a nil
// error WITHOUT forking a child and WITHOUT starting a reaper goroutine,
// restoring the original via t.Cleanup. Modeled on the inline override in
// TestDispatchStage_ThreadsNonNilProbeToSpawn.
//
// #2687: a REAL detached spawn's reaper calls reapZeroExitStrand, whose
// attempt-0 probe issues its OWN GET /v0/runs/{id}/stages concurrently with the
// handler's post-dispatch classify read. Any test doing ORDINAL fault injection
// on that endpoint (fb.stagesFailOnCall) must therefore NOT real-spawn — the
// concurrent reader makes which request consumes the injected 500 nondeterministic
// (the flake #2687 fixed). Stubbing the seam removes the reaper, so the endpoint
// stays single-reader and the ordinal lands deterministically.
func withStubbedDispatchSpawn(t *testing.T) {
	t.Helper()
	savedSpawn := dispatchSpawnDetached
	dispatchSpawnDetached = func(_ string, _, _ []string, _, _ string, _ detachedFailureReporter, _ detachedStageStateProbe) (string, error) {
		return "/dev/null", nil
	}
	// The dispatch path resolves the runner binary (runStageLookPath) BEFORE it
	// reaches the spawn seam, so stub that too — mirroring withFakeRunner minus the
	// real command and reaper.
	savedLook := runStageLookPath
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() {
		dispatchSpawnDetached = savedSpawn
		runStageLookPath = savedLook
	})
}

// TestDispatchStage_PostFetchFailureWarnsNoError asserts the fail-soft branch:
// when the post-dispatch stage fetch fails (the THIRD /stages call), the
// handler returns the handle with a nil StageWaitStatus + a warning, never a
// tool error — the spawn already happened and the handle is durable.
func TestDispatchStage_PostFetchFailureWarnsNoError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	// Stub the detached-spawn seam rather than real-spawn (withFakeRunner): a real
	// spawn starts a reaper whose attempt-0 zero-exit probe reads GET
	// /v0/runs/{id}/stages concurrently with the handler's post-dispatch classify,
	// racing for the ordinal-injected 500 (#2687). With the stub there is no reaper,
	// so the three /stages reads below are the ONLY reads and the count is
	// deterministic. See withStubbedDispatchSpawn.
	withStubbedDispatchSpawn(t)

	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "implement", "pending")
	// Three /stages calls fire: (1) resolveStageID, (2) the sibling-in-flight guard
	// (#1872), (3) the post-dispatch classify. Fail the THIRD so the post-dispatch
	// classify errors (the guard's call-2 succeeds — target pending, no in-flight
	// sibling — and allows the spawn).
	fb.mu.Lock()
	fb.stagesFailOnCall = postDispatchStagesCall
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
	// Ordinal precondition: exactly postDispatchStagesCall reads fired, so the
	// injected 500 landed on the post-dispatch classify. A shifted or extra read
	// (e.g. a re-introduced concurrent reaper probe, #2687) fails here by name
	// rather than silently moving the injection onto a different call.
	fb.mu.Lock()
	gotCalls := fb.stagesCalledByID[runID]
	fb.mu.Unlock()
	if gotCalls != postDispatchStagesCall {
		t.Errorf("GET /stages read %d times for the run, want %d — a shifted or extra stages read means the injected 500 no longer lands on the post-dispatch classify", gotCalls, postDispatchStagesCall)
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

// TestDispatchStage_StageStateProbe is the call-site wiring test for the #2630
// zero-exit strand probe: `stageStateProbe` is the production `detachedStageStateProbe`
// dispatchStage (and drive_run) thread into spawnRunnerStageDetached, so what the
// reaper observes on a zero exit is exactly what this builder reads. Driving it
// directly against an httptest backend is race-free (unlike a full detached-spawn
// end-to-end, whose reaper goroutine outlives the test and would race a global
// settle-window override) and pins the two branches the reaper's fail-open posture
// depends on: a present stage returns its state; an absent one returns an ERROR
// (so the reaper reports nothing rather than false-stranding). The reaper's
// strand DECISION over these states is pinned exhaustively by the unit-level
// TestReapDetachedRunner_ZeroExitStrandMatrix.
func TestDispatchStage_StageStateProbe(t *testing.T) {
	runID := uuid.New()
	stageID := uuid.New()

	var state string // served state for the target stage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every request is a stage-list read.
		items := []Stage{{ID: stageID.String(), RunID: runID.String(), Type: "implement", State: state}}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	}))
	defer srv.Close()

	r := &runResolver{
		api:    newAPIClient(config{backendURL: srv.URL, apiToken: "tok-test"}),
		getenv: func(string) string { return "" },
	}

	// Present stage → its state (the reaper classifies this against the allow-list).
	state = "dispatched"
	probe := r.stageStateProbe(runID, stageID.String())
	got, err := probe(context.Background())
	if err != nil {
		t.Fatalf("probe on a present stage: unexpected error %v", err)
	}
	if got != "dispatched" {
		t.Errorf("probe state = %q, want dispatched", got)
	}

	// Absent stage → an ERROR (the fail-open source: the reaper reports nothing on
	// a probe error rather than false-stranding a stage it cannot read).
	missing := r.stageStateProbe(runID, uuid.New().String())
	if _, err := missing(context.Background()); err == nil {
		t.Error("probe on an absent stage must return an error so the reaper fails open")
	}
}

// TestDispatchStage_ThreadsNonNilProbeToSpawn is the call-site wiring pin for the
// #2630 zero-exit strand probe at the dispatch_stage spawn (concern
// medium/test-coverage). TestDispatchStage_StageStateProbe pins the probe BUILDER
// and TestReapDetachedRunner_ZeroExitStrandMatrix pins the reaper's decision over
// an injected probe, but neither asserts dispatchStage THREADS a non-nil probe
// into spawnRunnerStageDetached. A regression that reverts the call site to pass
// nil (dropping `probe := r.stageStateProbe(...)`) would keep every other test
// green while silently disabling the entire #2630 recovery — reapZeroExitStrand
// short-circuits on a nil probe.
//
// This mirrors the sibling TestDriveRun_ThreadsNonNilProbeToSpawn: it wraps the
// injectable detached-spawn seam (the dispatchSpawnDetached package var — kept
// separate from the drive loop's r.driveSpawn so a manual dispatch stays
// distinguishable from a drive-loop spawn) and asserts the probe argument the
// call site threads is NON-NIL, WITHOUT launching a real exit-0 runner. The
// property under test (this call site threads a non-nil probe) does not require
// driving a real runner to observe, and driving one would spawn a reaper
// goroutine that races the package-global reap settle-window (reapProbeBackoff)
// against a reaper leaked from an earlier test — the -race failure this pin
// originally introduced (CI run 31660448999). The seam observes the argument with
// no runner, no reaper, no global mutation, and no race. The reaper's DECISION
// over the probe's states is pinned exhaustively by the unit-level
// TestReapDetachedRunner_ZeroExitStrandMatrix, and the probe BUILDER by
// TestDispatchStage_StageStateProbe.
//
// COUNTERFACTUAL (condition 5): reverting dispatch_stage.go's spawn call to pass a
// nil probe reddens this on the probeNonNil assertion.
func TestDispatchStage_ThreadsNonNilProbeToSpawn(t *testing.T) {
	runID := uuid.New()
	stageID := uuid.New()

	// A minimal stateful backend serving only the pre-spawn guard reads
	// dispatchStage makes before it reaches the spawn seam (GET run unlocked,
	// GET stages present+pending, the best-effort record-act POST, the
	// host-dispatch marker POST). No reap-failure endpoint and no runner: the
	// seam short-circuits the spawn.
	var mu sync.Mutex
	stageState := "pending"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/host-dispatch"):
			mu.Lock()
			stageState = "dispatched"
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"transitioned":true,"stage_state":"dispatched"}`))
		case strings.HasSuffix(r.URL.Path, "/stages"):
			mu.Lock()
			st := stageState
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []Stage{{ID: stageID.String(), RunID: runID.String(), Type: "implement", State: st}},
			})
		default:
			// guardHostDispatch's GET /v0/runs/{id} (unlocked so the guard passes)
			// and the best-effort POST /auto-drive record-act both tolerate this.
			_ = json.NewEncoder(w).Encode(Run{ID: runID.String(), Repo: "x/y", State: "running"})
		}
	}))
	defer srv.Close()

	r := &runResolver{
		api:    newAPIClient(config{backendURL: srv.URL, apiToken: "tok-test"}),
		getenv: func(string) string { return "" },
	}

	// Override the detached-spawn seam to observe the probe argument the dispatch
	// call site threads. Returning nil error WITHOUT spawning keeps this race-free
	// — no real runner, no reaper goroutine. Restored via t.Cleanup; the mcpserver
	// tests run serially (no t.Parallel), so the package-var override is race-free.
	var probeSeen, probeNonNil bool
	savedSpawn := dispatchSpawnDetached
	dispatchSpawnDetached = func(binary string, argv, env []string, runID, stageID string, report detachedFailureReporter, probe detachedStageStateProbe) (string, error) {
		probeSeen = true
		probeNonNil = probe != nil
		return "/dev/null", nil
	}
	t.Cleanup(func() { dispatchSpawnDetached = savedSpawn })

	if _, _, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID: runID.String(), Workflow: "feature_change", Stage: "implement",
		GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
		RunnerBinary: "/fake/fishhawk-runner",
	}); err != nil {
		t.Fatalf("dispatchStage: %v", err)
	}

	if !probeSeen {
		t.Fatal("dispatch never reached the spawn seam")
	}
	if !probeNonNil {
		t.Fatal("dispatch_stage spawn call site threaded a NIL probe — the #2630 zero-exit strand recovery is silently disabled at this call site")
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

	// Acceptance-admission fixtures (#1928). stageType/stageState default to
	// implement/pending; the acceptance short-circuit tests set stageType
	// "acceptance". admissionShortCircuit drives the short_circuited response;
	// admissionStatus (0 -> 200) drives the fail-open error branch;
	// admissionCalledN counts admission POSTs; on a short-circuit the handler
	// flips stageState to succeeded so the post-short-circuit stages read
	// reflects the settle.
	stageType             string
	stageState            string
	admissionShortCircuit bool
	admissionStatus       int
	admissionCalledN      int
	// Acceptance needs_target fixtures (#1953). When admissionNeedsTarget is set
	// (and not short-circuiting), the admission response carries needs_target +
	// hosts + head SHA so the verb-side gate probes. hostDispatchCalledN counts
	// host-dispatch marker POSTs so a needs_target refusal can assert none fired.
	admissionNeedsTarget     bool
	admissionTargetHosts     []string
	admissionExpectedHeadSHA string
	hostDispatchCalledN      int
	// stageStateAfterAdmission, when set, is the state /stages reads return AFTER
	// the admission POST — modelling a mid-walk 500 that left the target 'running'.
	// The earlier resolveStageID + sibling-guard reads (admissionCalledN==0) still
	// see stageState, so those guards pass before the fail-open re-check trips.
	stageStateAfterAdmission string
	// stagesFailAfterAdmission, when true, makes /stages reads 500 AFTER the
	// admission POST — used to fail the post-short-circuit stage fetch while the
	// pre-admission reads succeed (#1928 untested-error-path concern).
	stagesFailAfterAdmission bool
}

func (f *dispatchAutoDriveFake) stageTypeOrDefault() string {
	if f.stageType != "" {
		return f.stageType
	}
	return "implement"
}

func (f *dispatchAutoDriveFake) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/acceptance-admission"):
			f.mu.Lock()
			f.admissionCalledN++
			sc := f.admissionShortCircuit
			status := f.admissionStatus
			needsTarget := f.admissionNeedsTarget
			targetHosts := f.admissionTargetHosts
			expectedHeadSHA := f.admissionExpectedHeadSHA
			if sc {
				f.stageState = "succeeded"
			}
			f.mu.Unlock()
			if status != 0 && status != http.StatusOK {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"boom"}}`))
				return
			}
			res := AcceptanceAdmissionResult{ShortCircuited: sc}
			if sc {
				res.Kind = "all_skip_with_basis"
				res.Basis = "all-skip-with-basis"
				res.CriteriaTotal = 2
				res.Stage = &Stage{ID: f.stageID.String(), RunID: f.runID.String(), Type: "acceptance", State: "succeeded"}
			} else if needsTarget {
				res.NeedsTarget = true
				res.TargetHosts = targetHosts
				res.ExpectedHeadSHA = expectedHeadSHA
			}
			_ = json.NewEncoder(w).Encode(res)
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
			f.mu.Lock()
			postAdmission := f.admissionCalledN > 0
			failStages := f.stagesFailAfterAdmission && postAdmission
			state := f.stageState
			if postAdmission && f.stageStateAfterAdmission != "" {
				state = f.stageStateAfterAdmission
			}
			if state == "" {
				state = "pending"
			}
			stype := f.stageTypeOrDefault()
			f.mu.Unlock()
			if failStages {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"stages boom"}}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []Stage{{ID: f.stageID.String(), RunID: f.runID.String(), Type: stype, State: state}},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/host-dispatch"):
			f.mu.Lock()
			f.hostDispatchCalledN++
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(HostDispatchResult{Transitioned: true, StageState: "dispatched"})
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
// spawn ordering). Post-#1912 this row is ATTRIBUTION only; the host-dispatch
// marker (called between record and spawn) stamps the 'dispatched' spawn signal.
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
// warning naming the degraded attribution record. Post-#1912 the record is
// ATTRIBUTION (the host-dispatch marker is the staleness evidence), not an
// authorization gate, so it must never block the core manual recovery verb.
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
		// Post-#1912 the record row is ATTRIBUTION, not the staleness evidence
		// (the host-dispatch marker stamps that), so the degraded-record warning
		// names the missing attribution/provenance rather than stale detection.
		if strings.Contains(w, "attribution") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning naming the degraded attribution record; warnings: %v", out.Warnings)
	}
}

// --- (#1928) acceptance-dispatch admission at initial host dispatch ----------

// TestDispatchStage_AcceptanceShortCircuit_NoSpawn pins the short-circuit mode:
// when acceptance-admission returns short_circuited:true the dispatch spawns NO
// runner and records NO auto-drive spawn-evidence act — the output reflects the
// settled (succeeded) stage.
func TestDispatchStage_AcceptanceShortCircuit_NoSpawn(t *testing.T) {
	f := &dispatchAutoDriveFake{runID: uuid.New(), stageID: uuid.New(), stageType: "acceptance", admissionShortCircuit: true}
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
		RunID: f.runID.String(), Workflow: "feature_change", Stage: "acceptance",
		GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
	})
	if err != nil {
		t.Fatalf("dispatchStage: %v", err)
	}
	if spawned != 0 {
		t.Errorf("a short-circuited acceptance dispatch must spawn NO runner, got %d", spawned)
	}
	f.mu.Lock()
	acts := f.actCalledN
	admissionN := f.admissionCalledN
	f.mu.Unlock()
	if acts != 0 {
		t.Errorf("a short-circuited acceptance dispatch must record NO spawn-evidence act, got %d", acts)
	}
	if admissionN != 1 {
		t.Errorf("admission endpoint calls = %d, want 1", admissionN)
	}
	if out.LogPath != "" {
		t.Errorf("LogPath = %q, want empty (no runner spawned)", out.LogPath)
	}
	if out.StageWaitStatus == nil || out.StageWaitStatus.Status != "succeeded" {
		t.Errorf("StageWaitStatus = %+v, want the settled succeeded stage", out.StageWaitStatus)
	}
	var noted bool
	for _, w := range out.Warnings {
		if strings.Contains(w, plan.AcceptanceVerdictNotValidated) {
			noted = true
		}
		if passedWordRe.MatchString(w) {
			t.Errorf("short-circuit warning must not name a passed verdict (#2458): %q", w)
		}
	}
	if !noted {
		t.Errorf("missing the short-circuit note naming %q; warnings: %v", plan.AcceptanceVerdictNotValidated, out.Warnings)
	}
}

// TestDispatchStage_AcceptanceNeedsTarget_NoRecordNoSpawn (#1953): a needs_target
// admission whose declared target is unreachable REFUSES — no record-act, no
// host-dispatch marker, no spawn — and returns the structured NeedsTarget.
func TestDispatchStage_AcceptanceNeedsTarget_NoRecordNoSpawn(t *testing.T) {
	origAttempts := acceptanceQuickProbeAttempts
	acceptanceQuickProbeAttempts = 1
	t.Cleanup(func() { acceptanceQuickProbeAttempts = origAttempts })

	target := healthzServer(t, http.StatusOK, `{"git_sha":"abc1234def"}`)
	targetHost := hostOf(target.URL)
	target.Close()

	f := &dispatchAutoDriveFake{
		runID: uuid.New(), stageID: uuid.New(), stageType: "acceptance",
		admissionNeedsTarget:     true,
		admissionTargetHosts:     []string{targetHost},
		admissionExpectedHeadSHA: probeExpectedSHA,
	}
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
		RunID: f.runID.String(), Workflow: "feature_change", Stage: "acceptance",
		GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
	})
	if err != nil {
		t.Fatalf("dispatchStage: %v", err)
	}
	if spawned != 0 {
		t.Errorf("a needs_target refusal must spawn NO runner, got %d", spawned)
	}
	f.mu.Lock()
	acts, marks := f.actCalledN, f.hostDispatchCalledN
	f.mu.Unlock()
	if acts != 0 {
		t.Errorf("a needs_target refusal must record NO spawn-evidence act, got %d", acts)
	}
	if marks != 0 {
		t.Errorf("a needs_target refusal must NOT stamp the host-dispatch marker, got %d", marks)
	}
	if out.NeedsTarget == nil {
		t.Fatal("NeedsTarget = nil, want the structured refusal")
	}
	if out.NeedsTarget.TargetHost != targetHost || out.NeedsTarget.ExpectedHeadSHA != probeExpectedSHA {
		t.Errorf("NeedsTarget = %+v, want host=%q sha=%q", out.NeedsTarget, targetHost, probeExpectedSHA)
	}
	if out.LogPath != "" {
		t.Errorf("LogPath = %q, want empty (no runner spawned)", out.LogPath)
	}
	// #2491: no runner was spawned, so there is nothing to await — the
	// fishhawk_await_stage pointer must be nil on the pre-spawn refusal.
	if out.NextStep != nil {
		t.Errorf("NextStep = %+v, want nil on the needs_target pre-spawn refusal", out.NextStep)
	}
}

// TestDispatchStage_NeedsTarget_NoAwaitPointer is the #2491 done-means for the
// nil-pointer half: on the pre-spawn NeedsTarget acceptance refusal (no runner
// spawned) the dispatch output carries NO fishhawk_await_stage next_step, since
// there is nothing to await.
func TestDispatchStage_NeedsTarget_NoAwaitPointer(t *testing.T) {
	origAttempts := acceptanceQuickProbeAttempts
	acceptanceQuickProbeAttempts = 1
	t.Cleanup(func() { acceptanceQuickProbeAttempts = origAttempts })

	target := healthzServer(t, http.StatusOK, `{"git_sha":"abc1234def"}`)
	targetHost := hostOf(target.URL)
	target.Close()

	f := &dispatchAutoDriveFake{
		runID: uuid.New(), stageID: uuid.New(), stageType: "acceptance",
		admissionNeedsTarget:     true,
		admissionTargetHosts:     []string{targetHost},
		admissionExpectedHeadSHA: probeExpectedSHA,
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	r := &runResolver{
		api:    newAPIClient(config{backendURL: srv.URL, apiToken: "tok"}),
		getenv: func(string) string { return "" },
	}

	origCmd := runStageCommand
	runStageCommand = func(_ string, _ ...string) *exec.Cmd { return exec.Command("sh", "-c", "exit 0") }
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() { runStageCommand = origCmd })

	_, out, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID: f.runID.String(), Workflow: "feature_change", Stage: "acceptance",
		GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
	})
	if err != nil {
		t.Fatalf("dispatchStage: %v", err)
	}
	if out.NeedsTarget == nil {
		t.Fatal("NeedsTarget = nil, want the structured refusal (test precondition)")
	}
	if out.NextStep != nil {
		t.Errorf("NextStep = %+v, want nil on the needs_target refusal", out.NextStep)
	}
}

// TestDispatchStage_AcceptanceNeedsTargetVerified_SpawnsAsToday (#1953): a
// needs_target admission whose target is VERIFIED proceeds to record + spawn.
func TestDispatchStage_AcceptanceNeedsTargetVerified_SpawnsAsToday(t *testing.T) {
	target := healthzServer(t, http.StatusOK, `{"git_sha":"abc1234def"}`)

	f := &dispatchAutoDriveFake{
		runID: uuid.New(), stageID: uuid.New(), stageType: "acceptance",
		admissionNeedsTarget:     true,
		admissionTargetHosts:     []string{hostOf(target.URL)},
		admissionExpectedHeadSHA: probeExpectedSHA,
	}
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
		RunID: f.runID.String(), Workflow: "feature_change", Stage: "acceptance",
		GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
	})
	if err != nil {
		t.Fatalf("dispatchStage: %v", err)
	}
	if out.NeedsTarget != nil {
		t.Errorf("a verified target must proceed, got needs_target refusal: %+v", out.NeedsTarget)
	}
	if spawned != 1 {
		t.Errorf("a verified target must spawn exactly one runner, got %d", spawned)
	}
	f.mu.Lock()
	acts, marks := f.actCalledN, f.hostDispatchCalledN
	f.mu.Unlock()
	if acts != 1 {
		t.Errorf("a verified target must record the spawn-evidence act, got %d", acts)
	}
	if marks != 1 {
		t.Errorf("a verified target must stamp the host-dispatch marker once, got %d", marks)
	}
}

// TestDispatchStage_AcceptanceAdmissionFalse_SpawnsAsToday pins the no-op mode:
// short_circuited:false records + spawns exactly as today, with NO short-circuit
// warning appended (the reconciliation binding condition).
func TestDispatchStage_AcceptanceAdmissionFalse_SpawnsAsToday(t *testing.T) {
	f := &dispatchAutoDriveFake{runID: uuid.New(), stageID: uuid.New(), stageType: "acceptance", admissionShortCircuit: false}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	r := &runResolver{
		api:    newAPIClient(config{backendURL: srv.URL, apiToken: "tok"}),
		getenv: func(string) string { return "" },
	}

	spawned := 0
	var argv []string
	origCmd := runStageCommand
	runStageCommand = func(name string, args ...string) *exec.Cmd {
		spawned++
		argv = append([]string{name}, args...)
		return exec.Command("sh", "-c", "exit 0")
	}
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() { runStageCommand = origCmd })

	_, out, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID: f.runID.String(), Workflow: "feature_change", Stage: "acceptance",
		GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
	})
	if err != nil {
		t.Fatalf("dispatchStage: %v", err)
	}
	if spawned != 1 {
		t.Fatalf("short_circuited:false must spawn exactly one runner, got %d", spawned)
	}
	if joined := strings.Join(argv, " "); !strings.Contains(joined, "--stage acceptance") {
		t.Errorf("spawned argv missing --stage acceptance: %s", joined)
	}
	f.mu.Lock()
	acts := f.actCalledN
	f.mu.Unlock()
	if acts != 1 {
		t.Errorf("short_circuited:false must record exactly one spawn-evidence act, got %d", acts)
	}
	for _, w := range out.Warnings {
		if strings.Contains(w, "short-circuited") || strings.Contains(w, "fail-open") {
			t.Errorf("no short-circuit / fail-open warning must appear on the normal no-op path; got %q", w)
		}
	}
}

// TestDispatchStage_AcceptanceAdmissionError_FailsOpen pins the fail-open mode:
// an admission-call error (500) appends a warning and the dispatch proceeds to
// record + spawn as today.
func TestDispatchStage_AcceptanceAdmissionError_FailsOpen(t *testing.T) {
	f := &dispatchAutoDriveFake{runID: uuid.New(), stageID: uuid.New(), stageType: "acceptance", admissionStatus: http.StatusInternalServerError}
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
		RunID: f.runID.String(), Workflow: "feature_change", Stage: "acceptance",
		GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
	})
	if err != nil {
		t.Fatalf("dispatchStage must fail open on an admission error, got: %v", err)
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

// TestDispatchStage_AcceptanceAdmissionAuthzRejection_FailsClosed pins the #1928
// authz concern: a 4xx admission rejection (403 cross_run_admission) is NOT a
// fail-open condition — the dispatch HALTS with a tool error and spawns NO runner
// rather than proceed after the run-subject authorization boundary rejected it.
func TestDispatchStage_AcceptanceAdmissionAuthzRejection_FailsClosed(t *testing.T) {
	f := &dispatchAutoDriveFake{runID: uuid.New(), stageID: uuid.New(), stageType: "acceptance", admissionStatus: http.StatusForbidden}
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

	_, _, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID: f.runID.String(), Workflow: "feature_change", Stage: "acceptance",
		GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
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
	f.mu.Lock()
	acts := f.actCalledN
	f.mu.Unlock()
	if acts != 0 {
		t.Errorf("a fail-closed admission rejection must record NO spawn-evidence act, got %d", acts)
	}
}

// TestDispatchStage_AcceptanceAdmissionError_StageLeftRunning_FailsClosed pins the
// #1928 mid-walk concern: when admission 500s AND the failed short-circuit walk
// left the target stage 'running', the fail-open re-check observes the
// non-dispatchable state and HALTS rather than spawning a second runner against a
// partially-settled acceptance stage.
func TestDispatchStage_AcceptanceAdmissionError_StageLeftRunning_FailsClosed(t *testing.T) {
	f := &dispatchAutoDriveFake{
		runID: uuid.New(), stageID: uuid.New(), stageType: "acceptance",
		admissionStatus: http.StatusInternalServerError, stageStateAfterAdmission: "running",
	}
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

	_, _, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID: f.runID.String(), Workflow: "feature_change", Stage: "acceptance",
		GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
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

// TestDispatchStage_AcceptanceShortCircuit_PostFetchFailure pins the #1928
// untested-error-path concern: when the short-circuit fires but the
// post-short-circuit stage fetch fails, the dispatch still returns success with NO
// spawn — the degraded output omits stage_wait_status and carries the warning.
func TestDispatchStage_AcceptanceShortCircuit_PostFetchFailure(t *testing.T) {
	f := &dispatchAutoDriveFake{
		runID: uuid.New(), stageID: uuid.New(), stageType: "acceptance",
		admissionShortCircuit: true, stagesFailAfterAdmission: true,
	}
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
		RunID: f.runID.String(), Workflow: "feature_change", Stage: "acceptance",
		GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
	})
	if err != nil {
		t.Fatalf("a post-short-circuit fetch failure must still return success, got: %v", err)
	}
	if spawned != 0 {
		t.Errorf("a short-circuited acceptance dispatch must spawn NO runner, got %d", spawned)
	}
	if out.StageWaitStatus != nil {
		t.Errorf("StageWaitStatus = %+v, want nil (degraded fetch)", out.StageWaitStatus)
	}
	if out.LogPath != "" {
		t.Errorf("LogPath = %q, want empty (no runner spawned)", out.LogPath)
	}
	var fetchWarn, scNote bool
	for _, w := range out.Warnings {
		if strings.Contains(w, "post-short-circuit stage fetch failed") {
			fetchWarn = true
		}
		if strings.Contains(w, plan.AcceptanceVerdictNotValidated) {
			scNote = true
		}
		if passedWordRe.MatchString(w) {
			t.Errorf("short-circuit warning must not name a passed verdict (#2458): %q", w)
		}
	}
	if !fetchWarn {
		t.Errorf("missing the degraded-fetch warning; warnings: %v", out.Warnings)
	}
	if !scNote {
		t.Errorf("missing the short-circuit note naming %q; warnings: %v", plan.AcceptanceVerdictNotValidated, out.Warnings)
	}
}

// --- #2479: transport-conditional working_dir resolution ---

// TestDispatchStage_HTTPTransportRefusesOmittedWorkingDir drives the handler on
// an httpTransport resolver with working_dir OMITTED and asserts the OUTCOME,
// not the schema text: the call errors naming working_dir, no runner is spawned,
// and NO state is committed (the fakeBackend recorded no host-dispatch marker and
// the output carries no log_path). The no-state assertion is required by the
// counterfactual rule's committed-state clause — a refusal that fired then
// rolled back would return a byte-identical error, so error identity alone
// cannot distinguish it; the assertion must land on state (#2479).
func TestDispatchStage_HTTPTransportRefusesOmittedWorkingDir(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	r.httpTransport = true
	calls := captureAllArgv(t)

	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "implement", "awaiting_host_dispatch")

	_, out, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID: runID.String(), Workflow: "feature_change", Stage: "implement",
		GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
		// working_dir intentionally omitted.
	})
	if err == nil {
		t.Fatal("expected a refusal error when working_dir is omitted over HTTP")
	}
	if !strings.Contains(err.Error(), "working_dir") {
		t.Errorf("error should name working_dir; got %v", err)
	}
	// No runner spawned.
	if len(*calls) != 0 {
		t.Errorf("runner spawned despite the refusal: %v", *calls)
	}
	// No committed state: no host-dispatch marker, no log_path.
	if n := fb.hostDispatchCalledByID[stageID]; n != 0 {
		t.Errorf("host-dispatch marker called %d times, want 0 (refusal must commit no state)", n)
	}
	if out.LogPath != "" {
		t.Errorf("log_path = %q, want empty (no spawn)", out.LogPath)
	}
}

// TestDispatchStage_StdioTransportOmittedResolvesToAbsoluteCwd asserts the
// shipped stdio default: an omitted working_dir resolves to the ABSOLUTE process
// cwd, and the spawned argv carries `--working-dir <cwd>`, never the literal "."
// (#2479).
func TestDispatchStage_StdioTransportOmittedResolvesToAbsoluteCwd(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil) // httpTransport:false (stdio)
	calls := captureAllArgv(t)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "implement", "pending")

	_, out, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID: runID.String(), Workflow: "feature_change", Stage: "implement",
		GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
		// working_dir intentionally omitted.
	})
	if err != nil {
		t.Fatalf("dispatchStage: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected 1 spawn, got %d", len(*calls))
	}
	joined := strings.Join((*calls)[0], " ")
	if !strings.Contains(joined, "--working-dir "+cwd) {
		t.Errorf("argv missing --working-dir %q: %v", cwd, (*calls)[0])
	}
	if strings.Contains(joined, "--working-dir .") {
		t.Errorf("argv carries the literal \".\" working dir: %v", (*calls)[0])
	}
	if out.ResolvedWorkingDir != cwd {
		t.Errorf("resolved_working_dir = %q, want %q", out.ResolvedWorkingDir, cwd)
	}
}

// TestDispatchStage_ExplicitWorkingDirEchoedAndUnchanged passes an explicit
// absolute working_dir and asserts resolved_working_dir echoes it and the
// spawned argv still carries `--working-dir <that path>` — the
// existing-callers-unaffected criterion (#2479).
func TestDispatchStage_ExplicitWorkingDirEchoedAndUnchanged(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	r.httpTransport = true // explicit absolute path is accepted on EITHER transport
	calls := captureAllArgv(t)

	dir := t.TempDir() // absolute
	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "implement", "pending")

	_, out, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID: runID.String(), Workflow: "feature_change", Stage: "implement",
		WorkingDir: dir, GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
	})
	if err != nil {
		t.Fatalf("dispatchStage: %v", err)
	}
	if out.ResolvedWorkingDir != dir {
		t.Errorf("resolved_working_dir = %q, want %q", out.ResolvedWorkingDir, dir)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected 1 spawn, got %d", len(*calls))
	}
	if !strings.Contains(strings.Join((*calls)[0], " "), "--working-dir "+dir) {
		t.Errorf("argv missing --working-dir %q: %v", dir, (*calls)[0])
	}
}

// --- E66.42 / #2482: working_dir bound at start_run, inherited here ---

// TestDispatchStage_InheritsBoundWorkingDir is the full end-to-end inheritance
// vehicle: an httptest backend serving a run whose working_dir is a bound
// absolute path, an HTTP-transport resolver, dispatch_stage called with
// working_dir OMITTED, asserting the spawn recorder received --working-dir
// <bound> and the tool output's resolved_working_dir is <bound>.
func TestDispatchStage_InheritsBoundWorkingDir(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	r.httpTransport = true
	calls := captureAllArgv(t)

	bound := t.TempDir() // absolute
	runID := uuid.New()
	stageID := uuid.New()
	seedRunWorkingDir(fb, runID, bound)
	seedStageOfType(fb, runID, stageID, "implement", "pending")

	_, out, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID: runID.String(), Workflow: "feature_change", Stage: "implement",
		GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
		// working_dir OMITTED — inherits the run's binding.
	})
	if err != nil {
		t.Fatalf("dispatchStage: %v", err)
	}
	if out.ResolvedWorkingDir != bound {
		t.Errorf("resolved_working_dir = %q, want the inherited binding %q", out.ResolvedWorkingDir, bound)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected 1 spawn, got %d", len(*calls))
	}
	if !strings.Contains(strings.Join((*calls)[0], " "), "--working-dir "+bound) {
		t.Errorf("argv missing inherited --working-dir %q: %v", bound, (*calls)[0])
	}
}

// TestDispatchStage_UnboundRunOverHTTPStillRefuses (C1): a run whose working_dir
// is "" (the legacy pre-change row), called with the parameter omitted over
// HTTP, is refused naming working_dir AND the spawn seam is NEVER called — the
// counterfactual proving inheritance did not reintroduce the daemon-cwd
// fallback. Deleting the ladder's fall-through-to-resolveWorkingDir("") refusal
// turns it red.
func TestDispatchStage_UnboundRunOverHTTPStillRefuses(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	r.httpTransport = true
	calls := captureAllArgv(t)

	runID := uuid.New()
	stageID := uuid.New()
	seedRunWorkingDir(fb, runID, "") // explicit empty binding — the unbound legacy row
	seedStageOfType(fb, runID, stageID, "implement", "awaiting_host_dispatch")

	_, out, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID: runID.String(), Workflow: "feature_change", Stage: "implement",
		GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
		// working_dir omitted AND the run is unbound.
	})
	if err == nil {
		t.Fatal("expected a refusal for an omitted working_dir on an unbound run over HTTP")
	}
	if !strings.Contains(err.Error(), "working_dir") {
		t.Errorf("error should name working_dir; got %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("spawn seam called despite the refusal: %v", *calls)
	}
	if n := fb.hostDispatchCalledByID[stageID]; n != 0 {
		t.Errorf("host-dispatch marker called %d times, want 0 (refusal commits no state)", n)
	}
	if out.LogPath != "" {
		t.Errorf("log_path = %q, want empty (no spawn)", out.LogPath)
	}
}

// TestDispatchStage_PollIntervalStaysAtFloor pins the deliberate call-site
// decision behind E48.62 / #2489: the spawn path does NOT hold the run row, so
// it passes a zero prediction and the derivation takes its elapsed branch. A
// freshly dispatched stage has ~0 elapsed, so the advertised cadence is the
// floor — byte-identical to pre-#2489 — even when the run itself carries a
// large prediction. The operator's NEXT get_run_status poll (which does hold
// the run row) is what carries the derived value.
//
// Seeded BY CONSTRUCTION: the run row is stamped with 115 predicted minutes, so
// a call site that wrongly reached for it would advertise the 900s ceiling here
// and fail.
func TestDispatchStage_PollIntervalStaysAtFloor(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeRunner(t, "true")

	runID := uuid.New()
	stageID := uuid.New()
	fb.getRunByID[runID] = Run{
		ID: runID.String(), Repo: "x/y", State: "running",
		PredictedRuntimeMinutes: 115,
	}
	seedStageOfType(fb, runID, stageID, "implement", "pending")

	_, out, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID:      runID.String(),
		Workflow:   "feature_change",
		Stage:      "implement",
		GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("dispatchStage: %v", err)
	}
	if out.StageWaitStatus == nil {
		t.Fatal("StageWaitStatus is nil")
	}
	if out.StageWaitStatus.PollIntervalSeconds != suggestedStageWaitPollIntervalSeconds {
		t.Errorf("PollIntervalSeconds = %d, want %d (the spawn path derives from a zero prediction by design)",
			out.StageWaitStatus.PollIntervalSeconds, suggestedStageWaitPollIntervalSeconds)
	}
}

// noPRGuardBackend is the wire-level fixture for the #2691 call-site tests. It
// serves a run (decomposed or standalone, per decomposedFrom) plus one pending
// implement stage, and COUNTS every mutating POST it receives — the
// auto-drive-attribution row and the host-dispatch marker are the two state
// commits the guard must precede, so counting POSTs at the wire proves "no
// state committed" directly rather than by inspecting a fake's fields.
func noPRGuardBackend(t *testing.T, runID, stageID uuid.UUID, decomposedFrom *string) (*httptest.Server, *int) {
	t.Helper()
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost:
			posts++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/stages"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []Stage{{ID: stageID.String(), RunID: runID.String(), Type: "implement", State: "pending"}},
			})
		default:
			// GET /v0/runs/{id}: read by guardHostDispatch, the working_dir
			// resolver and guardNoPRImplement.
			_ = json.NewEncoder(w).Encode(Run{
				ID: runID.String(), Repo: "x/y", State: "running", DecomposedFrom: decomposedFrom,
			})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &posts
}

// countingSpawnSeam swaps dispatchSpawnDetached for a counting stub (and stubs
// the binary lookup that precedes it), so a test can assert the refusal spawned
// NOTHING through the seam itself rather than by observing processes.
func countingSpawnSeam(t *testing.T) *int {
	t.Helper()
	var spawns int
	savedSpawn := dispatchSpawnDetached
	dispatchSpawnDetached = func(_ string, _, _ []string, _, _ string, _ detachedFailureReporter, _ detachedStageStateProbe) (string, error) {
		spawns++
		return "/dev/null", nil
	}
	savedLook := runStageLookPath
	runStageLookPath = func(_ string) (string, error) { return "/fake/fishhawk-runner", nil }
	t.Cleanup(func() {
		dispatchSpawnDetached = savedSpawn
		runStageLookPath = savedLook
	})
	return &spawns
}

// TestDispatchStage_RefusesNoPROnDecomposedChild is the #2691 done-means at the
// dispatch verb: a decomposition child dispatched with push_and_open_pr=false
// is refused, NO runner is spawned (proven through the dispatchSpawnDetached
// seam, not by observing processes), and the refusal commits NO state — neither
// the auto-drive attribution row nor the host-dispatch marker POSTs.
func TestDispatchStage_RefusesNoPROnDecomposedChild(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	parent := uuid.New().String()
	srv, posts := noPRGuardBackend(t, runID, stageID, &parent)
	spawns := countingSpawnSeam(t)

	r := &runResolver{
		api:    newAPIClient(config{backendURL: srv.URL, apiToken: "tok-test"}),
		getenv: func(string) string { return "" },
	}

	_, _, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID: runID.String(), Workflow: "feature_change", Stage: "implement",
		GitHubRepo: "x/y", WorkingDir: t.TempDir(), PushAndOpenPR: boolPtr(false),
	})
	// t.Error, NOT t.Fatal: the load-bearing assertions here are the STATE ones
	// below (no spawn, no committed state), and a Fatal would short-circuit them
	// out of the counterfactual RED — leaving the deletion evidenced only by the
	// error identity, the trap the counterfactual discipline warns about.
	if err == nil {
		t.Error("expected a refusal for a decomposition child dispatched with push_and_open_pr=false")
	} else if !strings.Contains(err.Error(), "fishhawk_run_children") {
		t.Errorf("refusal must name the remedy verb: %v", err)
	}
	if *spawns != 0 {
		t.Errorf("detached spawns = %d, want 0 — the refusal must spawn no runner at all", *spawns)
	}
	if *posts != 0 {
		t.Errorf("mutating POSTs = %d, want 0 — the refusal must commit no state (no auto-drive act, no host-dispatch marker)", *posts)
	}
}

// TestDispatchStage_AdmitsNoPROnStandaloneRun is the E22.8/#406 pin at the same
// call site: the guard is scoped to decomposition children, so a STANDALONE run
// dispatched with push_and_open_pr=false still spawns.
func TestDispatchStage_AdmitsNoPROnStandaloneRun(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	srv, _ := noPRGuardBackend(t, runID, stageID, nil)
	spawns := countingSpawnSeam(t)

	r := &runResolver{
		api:    newAPIClient(config{backendURL: srv.URL, apiToken: "tok-test"}),
		getenv: func(string) string { return "" },
	}

	if _, _, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID: runID.String(), Workflow: "feature_change", Stage: "implement",
		GitHubRepo: "x/y", WorkingDir: t.TempDir(), PushAndOpenPR: boolPtr(false),
	}); err != nil {
		t.Fatalf("a standalone --no-pr dispatch must be admitted: %v", err)
	}
	if *spawns != 1 {
		t.Errorf("detached spawns = %d, want 1 — the commit-yourself flow must keep working", *spawns)
	}
}

// TestDispatchStage_NoPRRunnerRefusal_SettlesCategoryC is the #2691 approval
// condition 4 evidence: the runner's L1 refusal SETTLES the stage rather than
// stranding it.
//
// The two halves of that claim are proven by two tests joined on the reason
// string. The runner half
// (TestRun_ImplementStage_NoPR_RefusesSharedBranchPushPaths) proves the real
// runner emits exactly {"event":"runner_failed","reason":
// "no_pr_unsupported_push_path",...} and exits non-zero before invoking the
// agent. THIS test proves that exact line, produced by a real detached child
// through the real spawn path, drives the #1747 reaper to a TERMINAL category-C
// failure report — so the refusal lands the stage `failed`, not `running`.
//
// The fix-up case is the one modelled here deliberately: the MCP guard cannot
// see fix-up status, so a fix-up --no-pr dispatch is ADMITTED at L2 and refused
// at L1 — the exact path this settle claim has to cover.
func TestDispatchStage_NoPRRunnerRefusal_SettlesCategoryC(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()

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
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []Stage{{ID: stageID.String(), RunID: runID.String(), Type: "implement", State: "pending"}},
			})
		default:
			// A STANDALONE run: the L2 guard admits, exactly as it does for the
			// fix-up dispatch this models (fix-up status is invisible pre-spawn).
			_ = json.NewEncoder(w).Encode(Run{ID: runID.String(), Repo: "x/y", State: "running"})
		}
	}))
	defer srv.Close()

	r := &runResolver{
		api:    newAPIClient(config{backendURL: srv.URL, apiToken: "tok-test"}),
		getenv: func(string) string { return "" },
	}

	// The runner's L1 refusal, byte-for-byte as main.go emits it.
	withFakeRunner(t, `echo '{"event":"runner_failed","reason":"no_pr_unsupported_push_path","detail":"stage is a fix-up pass"}'; exit 1`)

	if _, _, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID: runID.String(), Workflow: "feature_change", Stage: "implement",
		GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
	}); err != nil {
		t.Fatalf("dispatchStage: %v", err)
	}

	select {
	case b := <-gotCh:
		if b.Category != "C" {
			t.Errorf("category = %q, want C (a terminal, named failure — not a strand)", b.Category)
		}
		if b.Reason != "no_pr_unsupported_push_path" {
			t.Errorf("reason = %q, want the runner's named refusal reason", b.Reason)
		}
		if b.ExitCode != 1 {
			t.Errorf("exit_code = %d, want 1", b.ExitCode)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the reaper never reported the L1 refusal — the stage would be STRANDED, not settled")
	}
}

// TestDispatchStage_PushAndOpenPRDescription_DescribesPerCaseBehavior is the
// done-means behavioral test for the user-facing text: the advertised
// push_and_open_pr description — the SAME jsonschema.For inference AddTool uses
// — must describe the REAL per-case behavior. The weight is on the POSITIVE
// assertions (each per-case claim the field now makes); the negative check is a
// backstop only, since byte-level phrasing checks are brittle to benign
// rewording. Also pins the base_branch correction: the flag applies regardless
// of push_and_open_pr.
func TestDispatchStage_PushAndOpenPRDescription_DescribesPerCaseBehavior(t *testing.T) {
	schema, err := jsonschema.For[DispatchStageInput](nil)
	if err != nil {
		t.Fatalf("infer DispatchStageInput schema: %v", err)
	}
	assertPushAndOpenPRDescription(t, "fishhawk_dispatch_stage", schema)
}

// assertPushAndOpenPRDescription is shared by the dispatch_stage and run_stage
// description pins: both verbs expose the identical flag and reach the identical
// runner path, so both descriptions must make the same per-case claims.
func assertPushAndOpenPRDescription(t *testing.T, tool string, schema *jsonschema.Schema) {
	t.Helper()
	prop, ok := schema.Properties["push_and_open_pr"]
	if !ok {
		t.Fatalf("%s schema has no push_and_open_pr property", tool)
	}
	desc := prop.Description
	// POSITIVE assertions — one per per-case claim the field guards.
	for _, want := range []struct{ claim, substr string }{
		{"the standalone flow is still supported", "STANDALONE"},
		{"the standalone flow is still supported", "supported"},
		{"the standalone flow leaves the work in the tree", "working tree"},
		{"a decomposition child is refused", "DECOMPOSITION CHILD"},
		{"a fix-up is refused", "FIX-UP"},
		{"the refusal is named as such", "REFUSED"},
		{"the child remedy verb is named", "fishhawk_run_children"},
		{"the fix-up remedy is named", "re-dispatch without push_and_open_pr=false"},
	} {
		if !strings.Contains(desc, want.substr) {
			t.Errorf("%s push_and_open_pr description must state %s (missing %q):\n%s", tool, want.claim, want.substr, desc)
		}
	}
	// NEGATIVE backstop: the description must never claim the flag discards an
	// implement result — it does not, on any path.
	for _, forbidden := range []string{"discard", "throw away"} {
		if strings.Contains(strings.ToLower(desc), forbidden) {
			t.Errorf("%s push_and_open_pr description must not claim the flag %qs an implement result:\n%s", tool, forbidden, desc)
		}
	}
	// base_branch correction: it is NOT a no-op under push_and_open_pr=false.
	base, ok := schema.Properties["base_branch"]
	if !ok {
		t.Fatalf("%s schema has no base_branch property", tool)
	}
	if strings.Contains(base.Description, "no effect when push_and_open_pr is false") {
		t.Errorf("%s base_branch description still claims no effect under push_and_open_pr=false, which is wrong (--base-branch and --check-base-ref are passed unconditionally):\n%s", tool, base.Description)
	}
}

// --- runner self-host bootstrap advisory (E64.5 / #3086), verb boundary ---

// TestDispatchStage_RunnerScope_SurfacesBootstrapWarning drives the real
// dispatchStage handler end to end (fake backend -> guard -> DispatchStageOutput)
// with the detached spawn stubbed, asserting BOTH that the advisory string
// reaches DispatchStageOutput.Warnings AND that the dispatch still succeeds (a
// resolved stage_id returned, the spawn seam invoked) — so the advisory is
// proven non-blocking at the verb boundary. scope.files spans the guard, this
// verb, and the operator-visible tool output, so the per-layer guard unit is not
// sufficient.
func TestDispatchStage_RunnerScope_SurfacesBootstrapWarning(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withStubbedDispatchSpawn(t)

	runID := uuid.New()
	planScopeForRun(fb, runID, "runner/internal/agent/claudecode/claudecode.go")

	_, out, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
		RunID:      runID.String(),
		Workflow:   "feature_change",
		Stage:      "implement",
		GitHubRepo: "x/y",
	})
	if err != nil {
		t.Fatalf("the self-host advisory must NOT block a dispatch: %v", err)
	}
	if out.StageID == "" {
		t.Error("dispatch must still succeed with a resolved stage_id")
	}
	if out.LogPath != "/dev/null" {
		t.Errorf("the spawn seam must have been invoked (LogPath=%q, want /dev/null)", out.LogPath)
	}
	found := false
	for _, w := range out.Warnings {
		if strings.Contains(w, "runner/README.md") && strings.Contains(w, "fix-up budget") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("advisory not surfaced in DispatchStageOutput.Warnings: %v", out.Warnings)
	}
}
