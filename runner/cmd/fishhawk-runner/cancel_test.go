package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// blockingInvoker waits for ctx.Done() and returns a partial Result.
// Used to exercise the runner's cancellation path: the agent is
// "running" when SIGTERM lands. Returns the canned result with the
// context's error so the caller can verify the cooperative half of
// the cancel chain works end-to-end.
type blockingInvoker struct {
	events []agent.Event
}

func (b *blockingInvoker) Invoke(ctx context.Context, _ agent.Invocation) (agent.Result, error) {
	<-ctx.Done()
	return agent.Result{
		OK:              false,
		Events:          b.events,
		FailureCategory: "A",
		FailureReason:   "cancelled mid-stage",
	}, ctx.Err()
}

// withCancelableRunnerContext swaps newRunnerContext with one whose
// cancel func is returned to the test. Standalone signal.NotifyContext
// would force the test to raise a signal at the test process — that
// works but is heavy and racy across parallel tests. The seam pattern
// here is the same one #436's TestRunStage_ContextCancelSendsSIGTERM
// uses on the MCP side.
func withCancelableRunnerContext(t *testing.T) context.CancelFunc {
	t.Helper()
	orig := newRunnerContext
	ctx, cancel := context.WithCancel(context.Background())
	newRunnerContext = func() (context.Context, context.CancelFunc) {
		return ctx, cancel
	}
	t.Cleanup(func() {
		cancel()
		newRunnerContext = orig
	})
	return cancel
}

// TestRun_ContextCancel_EmitsCancelledAndExits130 is the headline
// #435 test: when newRunnerContext is cancelled while the agent is
// running, the runner emits a `runner_cancelled` log line and
// exits with code 130, regardless of where in the body the cancel
// landed.
func TestRun_ContextCancel_EmitsCancelledAndExits130(t *testing.T) {
	prompt := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(prompt, []byte("do work"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Block forever in the agent until ctx is cancelled.
	origInvoker := newInvoker
	newInvoker = func(_ string) agent.Invoker {
		return &blockingInvoker{events: []agent.Event{{Payload: []byte(`{"kind":"runner_started"}`)}}}
	}
	t.Cleanup(func() { newInvoker = origInvoker })

	cancel := withCancelableRunnerContext(t)

	// Cancel after a brief delay so the invoker is definitely
	// blocking before the signal lands.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	var out strings.Builder
	start := time.Now()
	exitCode := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change",
		"--stage", "plan",
		"--prompt-file", prompt,
	}, &out)
	elapsed := time.Since(start)

	if exitCode != exitCancelled {
		t.Errorf("exit code = %d, want %d (exitCancelled)", exitCode, exitCancelled)
	}
	if elapsed > 3*time.Second {
		t.Errorf("run() took %v after cancel; should exit promptly", elapsed)
	}
	if !strings.Contains(out.String(), `"event":"runner_cancelled"`) {
		t.Errorf("expected runner_cancelled log line, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `"run_id":"11111111-2222-3333-4444-555555555555"`) {
		t.Errorf("runner_cancelled missing run_id, got:\n%s", out.String())
	}
}

// TestRun_UncancelledRun_PreservesOriginalExitCode is the
// regression guard: the cancellation defer should ONLY override
// exit code when ctx was actually cancelled. A normal happy-path
// run preserves the existing exit code.
func TestRun_UncancelledRun_PreservesOriginalExitCode(t *testing.T) {
	// Don't swap newRunnerContext — let it set up the real
	// signal handler. As long as no signal lands during the
	// test, ctx.Err() will be nil at defer time.
	var out strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change",
		"--stage", "plan",
	}, &out)
	if got != exitOK {
		t.Errorf("happy-path run = %d, want %d", got, exitOK)
	}
	if strings.Contains(out.String(), `"event":"runner_cancelled"`) {
		t.Errorf("uncancelled run should NOT emit runner_cancelled, got:\n%s", out.String())
	}
}

// TestRun_ContextCancel_BeforeAgentStart_StillEmitsCancelled
// exercises the early-cancel branch: even when ctx is already
// cancelled before the agent invocation, the defer still fires
// with the right event + exit code. This mirrors the case where
// the MCP server's SIGTERM arrives milliseconds after spawn.
func TestRun_ContextCancel_BeforeAgentStart_StillEmitsCancelled(t *testing.T) {
	prompt := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(prompt, []byte("do work"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Pre-cancel: ctx is already done before run() reads it.
	origInvoker := newInvoker
	newInvoker = func(_ string) agent.Invoker {
		return &blockingInvoker{}
	}
	t.Cleanup(func() { newInvoker = origInvoker })

	cancel := withCancelableRunnerContext(t)
	cancel()

	var out strings.Builder
	exitCode := run([]string{
		"--run-id", "rid",
		"--stage-id", "sid",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "w",
		"--stage", "plan",
		"--prompt-file", prompt,
	}, &out)

	if exitCode != exitCancelled {
		t.Errorf("exit code = %d, want %d", exitCode, exitCancelled)
	}
	if !strings.Contains(out.String(), `"event":"runner_cancelled"`) {
		t.Errorf("missing runner_cancelled:\n%s", out.String())
	}
}
