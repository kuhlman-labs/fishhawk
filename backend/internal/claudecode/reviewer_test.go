package claudecode

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
)

// TestHelperProcess is the Go stdlib test-helper-process pattern: when
// GO_HELPER_PROCESS=1 is set, this test pretends to be the `claude` binary and
// emits a canned --output-format json envelope driven by HELPER_MODE. The real
// tests re-exec the test binary itself in place of the missing `claude`.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_HELPER_PROCESS") != "1" {
		return
	}
	defer os.Exit(0)

	switch os.Getenv("HELPER_MODE") {
	case "happy":
		// A success envelope whose result field is the verdict JSON.
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"{\"verdict\":\"approve\"}"}`)
	case "error":
		// Non-zero exit stands in for a subprocess failure.
		fmt.Fprintln(os.Stderr, "claude: model rate-limited")
		os.Exit(1)
	case "non_json_result":
		// Valid envelope but the result text is not JSON.
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"this is not json"}`)
	case "bad_verdict":
		// Valid envelope, valid JSON, but verdict outside the closed set.
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"{\"verdict\":\"maybe\"}"}`)
	case "killed":
		// Reproduce the #620 SIGKILL with empty stderr: the child kills
		// itself, surfacing as an *exec.ExitError ("signal: killed") with
		// ctx.Err()==nil (an external/OOM kill, the retryable class).
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		// SIGKILL delivery is asynchronous; block so the deferred
		// os.Exit(0) cannot win the race and exit 0 with empty output
		// before the signal lands (which would surface as a non-retryable
		// decode error and flake the retry-class tests).
		select {}
	case "slow":
		// Sleep past a short Timeout so the per-attempt deadline fires and
		// the child is killed with ctx.Err()==DeadlineExceeded (the
		// timeout class, which must NOT be retried).
		time.Sleep(5 * time.Second)
	default:
		fmt.Fprintln(os.Stderr, "unknown HELPER_MODE")
		os.Exit(2)
	}
}

// helperCommand returns a Cmd-builder that re-execs the test binary as the
// `claude` stand-in, passing through HELPER_MODE.
func helperCommand(mode string) func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		c := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcess")
		c.Env = append(os.Environ(),
			"GO_HELPER_PROCESS=1",
			"HELPER_MODE="+mode,
		)
		return c
	}
}

func testConfig() Config {
	return Config{
		Binary:    "claude",
		Model:     "claude-sonnet-4-6",
		MaxTokens: 4096,
		Timeout:   5 * time.Second,
	}
}

func reviewerWithMode(mode string) *Reviewer {
	r := NewReviewer(testConfig())
	r.client.Cmd = helperCommand(mode)
	return r
}

// countingHelperCommand wraps helperCommand to count how many times the Cmd
// builder is invoked — i.e. how many subprocess attempts the retry loop made.
// The builder runs once per attempt, so the counter pins attempt count.
func countingHelperCommand(mode string, attempts *int) func(ctx context.Context, name string, args ...string) *exec.Cmd {
	build := helperCommand(mode)
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		*attempts++
		return build(ctx, name, args...)
	}
}

// flakyHelperCommand returns a builder whose first attempt runs the `killed`
// mode (a transient SIGKILL crash) and whose every later attempt runs `happy`.
// State lives in the closure, not the helper process, because each attempt is
// a fresh exec that cannot share memory.
func flakyHelperCommand(attempts *int) func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		*attempts++
		mode := "happy"
		if *attempts == 1 {
			mode = "killed"
		}
		return helperCommand(mode)(ctx, name, args...)
	}
}

// TestReviewer_HappyPath asserts a success envelope decodes to an approve
// verdict and the returned model is the configured model.
func TestReviewer_HappyPath(t *testing.T) {
	verdict, model, err := reviewerWithMode("happy").Review(context.Background(), "review this plan")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if verdict.Verdict != planreview.VerdictApprove {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, planreview.VerdictApprove)
	}
	if model != "claude-sonnet-4-6" {
		t.Errorf("model = %q, want %q", model, "claude-sonnet-4-6")
	}
}

// TestReviewer_SubprocessError asserts a non-zero exit surfaces as an error.
func TestReviewer_SubprocessError(t *testing.T) {
	_, _, err := reviewerWithMode("error").Review(context.Background(), "review this plan")
	if err == nil {
		t.Fatal("expected error from non-zero subprocess exit, got nil")
	}
}

// TestReviewer_NonJSONResult asserts a result field that is not JSON surfaces
// as an error.
func TestReviewer_NonJSONResult(t *testing.T) {
	_, _, err := reviewerWithMode("non_json_result").Review(context.Background(), "review this plan")
	if err == nil {
		t.Fatal("expected error from non-JSON result text, got nil")
	}
}

// TestReviewer_UnknownVerdict asserts a valid envelope carrying a verdict
// outside the closed set surfaces as an error.
func TestReviewer_UnknownVerdict(t *testing.T) {
	_, _, err := reviewerWithMode("bad_verdict").Review(context.Background(), "review this plan")
	if err == nil {
		t.Fatal("expected error from unknown verdict value, got nil")
	}
}

// TestClient_NewClientDefaultsMaxRetries asserts NewClient defaults the retry
// budget to 1 so production always retries a transient crash once, while a
// struct/cfg-level explicit 0 (set directly) disables it.
func TestClient_NewClientDefaultsMaxRetries(t *testing.T) {
	c := NewClient(Config{Model: "m"})
	if c.cfg.MaxRetries != 1 {
		t.Errorf("MaxRetries = %d, want 1", c.cfg.MaxRetries)
	}
}

// TestReviewer_RetryRecovers asserts a first-attempt transient crash is
// retried and the second attempt's approve verdict is returned (MaxRetries=1).
func TestReviewer_RetryRecovers(t *testing.T) {
	var attempts int
	r := NewReviewer(testConfig())
	r.client.Cmd = flakyHelperCommand(&attempts)

	verdict, _, err := r.Review(context.Background(), "review this plan")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if verdict.Verdict != planreview.VerdictApprove {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, planreview.VerdictApprove)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (one crash + one recovery)", attempts)
	}
}

// TestReviewer_RetryExhausted asserts a persistently crashing subprocess is
// retried up to the budget and the final diagnostic names the external/OOM
// cause and an elapsed wall-clock substring (MaxRetries=1 → exactly 2 tries).
func TestReviewer_RetryExhausted(t *testing.T) {
	var attempts int
	r := NewReviewer(testConfig())
	r.client.Cmd = countingHelperCommand("killed", &attempts)

	_, _, err := r.Review(context.Background(), "review this plan")
	if err == nil {
		t.Fatal("expected error from exhausted retries, got nil")
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (MaxRetries=1)", attempts)
	}
	if !strings.Contains(err.Error(), "external/OOM") {
		t.Errorf("error = %q, want it to name the external/OOM cause", err)
	}
	if !strings.Contains(err.Error(), "after ") {
		t.Errorf("error = %q, want it to carry an elapsed substring", err)
	}
}

// TestReviewer_TimeoutNotRetried asserts a per-attempt timeout (a slow review)
// fails fast: it is labelled `timeout` and attempted EXACTLY ONCE even with
// MaxRetries=1, so a slow review never compounds into a doubled wait (#606).
func TestReviewer_TimeoutNotRetried(t *testing.T) {
	var attempts int
	cfg := testConfig()
	cfg.Timeout = 50 * time.Millisecond
	r := NewReviewer(cfg)
	r.client.Cmd = countingHelperCommand("slow", &attempts)

	_, _, err := r.Review(context.Background(), "review this plan")
	if err == nil {
		t.Fatal("expected error from per-attempt timeout, got nil")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (a timeout must not be retried)", attempts)
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error = %q, want it to be labelled a timeout", err)
	}
}

// TestReviewer_RetryDisabled asserts MaxRetries=0 attempts exactly once even
// for the retryable crash class, so retry can be deterministically disabled.
func TestReviewer_RetryDisabled(t *testing.T) {
	var attempts int
	r := NewReviewer(testConfig())
	r.client.cfg.MaxRetries = 0
	r.client.Cmd = countingHelperCommand("killed", &attempts)

	_, _, err := r.Review(context.Background(), "review this plan")
	if err == nil {
		t.Fatal("expected error from crashing subprocess, got nil")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (MaxRetries=0 disables retry)", attempts)
	}
}

// TestReviewer_SetMaxRetriesDisablesRetry asserts SetMaxRetries(0) yields a
// single attempt even on the retryable crash class — proving the explicit-0
// override bypasses NewClient's zero->1 normalisation (the env disable path).
func TestReviewer_SetMaxRetriesDisablesRetry(t *testing.T) {
	var attempts int
	r := NewReviewer(testConfig())
	r.SetMaxRetries(0)
	r.client.Cmd = countingHelperCommand("killed", &attempts)

	_, _, err := r.Review(context.Background(), "review this plan")
	if err == nil {
		t.Fatal("expected error from crashing subprocess, got nil")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (SetMaxRetries(0) disables retry)", attempts)
	}
}

// TestReviewer_SetMaxRetriesBudget asserts SetMaxRetries(3) yields 4 attempts
// (N retries => N+1 attempts) on a persistently retryable crash.
func TestReviewer_SetMaxRetriesBudget(t *testing.T) {
	var attempts int
	r := NewReviewer(testConfig())
	r.SetMaxRetries(3)
	r.client.Cmd = countingHelperCommand("killed", &attempts)

	_, _, err := r.Review(context.Background(), "review this plan")
	if err == nil {
		t.Fatal("expected error from exhausted retries, got nil")
	}
	if attempts != 4 {
		t.Errorf("attempts = %d, want 4 (SetMaxRetries(3) => 3+1 attempts)", attempts)
	}
}

// TestReviewer_SetMaxRetriesClampsNegative asserts a negative budget is clamped
// to 0, yielding a single attempt rather than panicking or looping unbounded.
func TestReviewer_SetMaxRetriesClampsNegative(t *testing.T) {
	var attempts int
	r := NewReviewer(testConfig())
	r.SetMaxRetries(-1)
	r.client.Cmd = countingHelperCommand("killed", &attempts)

	_, _, err := r.Review(context.Background(), "review this plan")
	if err == nil {
		t.Fatal("expected error from crashing subprocess, got nil")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (negative budget clamps to 0)", attempts)
	}
}

// TestReviewer_StderrSurfaced asserts captured child stderr is folded into the
// diagnostic error so a non-zero exit is not silently undiagnosable.
func TestReviewer_StderrSurfaced(t *testing.T) {
	// MaxRetries=0 keeps this to a single attempt; `error` mode writes
	// "claude: model rate-limited" to stderr then exits non-zero.
	r := NewReviewer(testConfig())
	r.client.cfg.MaxRetries = 0
	r.client.Cmd = helperCommand("error")

	_, _, err := r.Review(context.Background(), "review this plan")
	if err == nil {
		t.Fatal("expected error from non-zero subprocess exit, got nil")
	}
	if !strings.Contains(err.Error(), "model rate-limited") {
		t.Errorf("error = %q, want it to surface the child stderr", err)
	}
}
