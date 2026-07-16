package claudecode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
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
		// A success envelope whose result field is the verdict JSON. No
		// `usage` object — models the pre-usage / degraded envelope.
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"{\"verdict\":\"approve\"}"}`)
	case "happy_usage":
		// A success envelope carrying a top-level `usage` object (#681).
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"{\"verdict\":\"approve\"}","usage":{"input_tokens":1234,"output_tokens":567}}`)
	case "happy_usage_cache":
		// A success envelope whose usage carries the cache members (#995). The
		// field names are pinned against a live `claude --print --output-format
		// json` envelope: input_tokens EXCLUDES the cache counts, which arrive
		// as cache_read_input_tokens / cache_creation_input_tokens.
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"{\"verdict\":\"approve\"}","usage":{"input_tokens":10,"output_tokens":41,"cache_read_input_tokens":11944,"cache_creation_input_tokens":7319}}`)
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
	case "flaky_decode_bad":
		// A success envelope whose result text is structurally-malformed verdict
		// JSON — a missing comma between members (`"approve" "concerns"`), the
		// #901 class strict-then-repair DecodeVerdict cannot rescue. The envelope
		// itself is valid JSON; only the nested verdict body is malformed.
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"{\"verdict\":\"approve\" \"concerns\":[]}"}`)
	case "prose_prefix":
		// The #1576 bug end-to-end: a success envelope whose result text is
		// PROSE followed by the JSON verdict — the E32.9 prose-prefix class that
		// produced `decode verdict JSON: invalid character 'T'`. The envelope
		// itself is valid JSON; after the envelope decode responseText carries
		// the prose prefix, which the shared DecodeVerdict must now extract past
		// via firstJSONObject.
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"The plan looks solid. Here is my verdict:\n{\"verdict\":\"approve\"}"}`)
	case "invalid_escape_regex":
		// The #739 bug end-to-end: a success envelope whose result text is a
		// verdict JSON that quotes a regex containing a lone `\-`. The envelope
		// itself is valid JSON; after the envelope decode, responseText carries
		// the lone backslash escape, which a strict json.Unmarshal rejects and
		// DecodeVerdict must repair.
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"{\"verdict\":\"reject\",\"free_form\":\"redact ghs_[A-Za-z0-9_.\\-]{36,}\"}"}`)
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
	case "pipe_leak_group":
		// The #1805 latent hang, group-kill arm: fork a grandchild that
		// inherits this stdout pipe and holds it open, staying in this process
		// GROUP; then this fake claude sleeps past the review deadline. A naive
		// single-child kill would leave the grandchild holding the pipe and
		// wedge cmd.Output(); procgroup.Harden's whole-group SIGKILL reaps both.
		spawnGrandchild(false)
		time.Sleep(timescale.D(30 * time.Second))
	case "pipe_leak_escape":
		// The #1805 latent hang, WaitDelay arm: fork a grandchild that ESCAPES
		// this process group (self-setpgid) and holds the inherited stdout pipe,
		// then exit immediately. The group kill cannot reach the escaped
		// grandchild, so only cmd.WaitDelay can force-close the parent-side pipe
		// fd — the branch whose forced return is a non-ExitError the timeout
		// hoist must still classify.
		spawnGrandchild(true)
		// exit immediately (deferred os.Exit(0)), leaving the pipe held.
	case "pipe_grandchild":
		// The stdout-inheriting grandchild forked by the pipe_leak_* modes:
		// record our pid so the driving test can assert reap/survival, then hold
		// the inherited stdout open well past any deadline+grace.
		if pf := os.Getenv("HELPER_GC_PIDFILE"); pf != "" {
			_ = os.WriteFile(pf, []byte(strconv.Itoa(os.Getpid())), 0o600)
		}
		time.Sleep(timescale.D(30 * time.Second))
	case "slow_brief":
		// Sleep longer than a short cfg.Timeout but shorter than an incoming
		// ctx deadline, then succeed. Used to prove invokeOnce honours the
		// incoming deadline and does NOT cap it at cfg.Timeout (#747): if the
		// short cfg.Timeout were applied the child would be killed before this
		// sleep finishes.
		time.Sleep(300 * time.Millisecond)
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"{\"verdict\":\"approve\"}"}`)
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

// spawnGrandchild re-execs the test binary as a stdout-inheriting grandchild of
// the fake claude process (the pipe_grandchild mode). escape=true makes it its
// own process-group leader so procgroup.Harden's kill(-pgid) misses it (the
// WaitDelay path); escape=false leaves it in the fake claude's group so the
// group kill reaps it. Used only inside TestHelperProcess (the fake-claude
// re-exec).
func spawnGrandchild(escape bool) {
	gc := exec.Command(os.Args[0], "-test.run=TestHelperProcess") //nolint:gosec // re-exec of the test binary itself
	env := append(os.Environ(), "GO_HELPER_PROCESS=1", "HELPER_MODE=pipe_grandchild")
	if pf := os.Getenv("HELPER_GC_PIDFILE"); pf != "" {
		env = append(env, "HELPER_GC_PIDFILE="+pf)
	}
	gc.Env = env
	gc.Stdout = os.Stdout // inherit the pipe write-end so it stays open after the fake claude dies
	if escape {
		gc.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	_ = gc.Start()
}

// countingPipeLeakHelper returns a Cmd-builder for a pipe_leak_* mode that
// threads HELPER_GC_PIDFILE through to the fake claude (so the forked grandchild
// records its pid) and counts subprocess attempts.
func countingPipeLeakHelper(mode, pidfile string, attempts *int) func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		*attempts++
		c := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcess")
		c.Env = append(os.Environ(),
			"GO_HELPER_PROCESS=1",
			"HELPER_MODE="+mode,
			"HELPER_GC_PIDFILE="+pidfile,
		)
		return c
	}
}

// setKillGrace overrides the package killGrace (the procgroup.Harden WaitDelay)
// for a timing-sensitive test and returns a restore func.
func setKillGrace(d time.Duration) func() {
	prev := killGrace
	killGrace = d
	return func() { killGrace = prev }
}

// killPidFromFile best-effort SIGKILLs the pid recorded in pidfile — cleanup for
// an escaped grandchild the group kill cannot reap.
func killPidFromFile(pidfile string) {
	if b, err := os.ReadFile(pidfile); err == nil {
		if pid, perr := strconv.Atoi(string(b)); perr == nil && pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// pidAliveFromFile reports whether the pid recorded in pidfile is still live.
func pidAliveFromFile(t *testing.T, pidfile string) bool {
	t.Helper()
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("read grandchild pidfile %s: %v", pidfile, err)
	}
	pid, err := strconv.Atoi(string(b))
	if err != nil {
		t.Fatalf("parse grandchild pid %q: %v", b, err)
	}
	return syscall.Kill(pid, 0) == nil
}

// grandchildLivenessWait bounds how long a loaded CI runner may take to START
// (fork/exec the grandchild and flush its pid file) or REAP (deliver the
// group-kill signal and have the kernel remove the process) a helper process
// in waitPidGone. This is spawn/reap liveness, NOT the kill-latency behavior
// under test — the #1805 regression bounds (elapsed > D(3s) at
// TestReviewer_PipeLeakGroupKillTimeout, elapsed > D(5s) at
// TestReviewer_PipeLeakEscapedGroupWaitDelayTimeout) preserve their ratio to
// the deadline under the same factor. It is a function (not a const) so the
// factor is read at call time, after any t.Setenv.
func grandchildLivenessWait() time.Duration { return timescale.D(30 * time.Second) }

// waitPidGone polls until the pid recorded in pidfile is gone, or fails the test.
func waitPidGone(t *testing.T, pidfile string) {
	t.Helper()
	var pid int
	for deadline := time.Now().Add(grandchildLivenessWait()); time.Now().Before(deadline); {
		if b, err := os.ReadFile(pidfile); err == nil {
			if p, perr := strconv.Atoi(string(b)); perr == nil && p > 0 {
				pid = p
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatalf("grandchild never wrote its pid to %s", pidfile)
	}
	for deadline := time.Now().Add(grandchildLivenessWait()); time.Now().Before(deadline); {
		if syscall.Kill(pid, 0) != nil {
			return // gone
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL) // best-effort cleanup on failure
	t.Errorf("grandchild pid %d still alive — the group kill did not reap it", pid)
}

// TestReviewer_PipeLeakGroupKillTimeout is the #1805 group-kill arm through the
// full Review()/Inference() path: a wedged fake claude holds an in-group
// grandchild that inherited stdout, run under a genuine incoming context
// DEADLINE. procgroup.Harden's whole-group SIGKILL reaps the grandchild so
// cmd.Output() returns AT the deadline (not minutes later), the error is the
// (timeout) classification, the deadline (not a bare cancel) is the trigger, the
// attempt is not retried, and the grandchild is reaped.
func TestReviewer_PipeLeakGroupKillTimeout(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "gc.pid")
	// Every deadline-competing duration derives from timescale.D (base × the
	// shared factor) so the discrimination ratios (bound/deadline,
	// long-grace/bound, wedge/bound) hold at any factor while CI gains headroom.
	defer setKillGrace(timescale.D(10 * time.Second))() // a long grace: group-kill must return well before it

	var attempts int
	r := NewReviewer(testConfig())
	r.client.Cmd = countingPipeLeakHelper("pipe_leak_group", pidfile, &attempts)

	ctx, cancel := context.WithTimeout(context.Background(), timescale.D(300*time.Millisecond))
	defer cancel()

	start := time.Now()
	_, _, err := r.Review(ctx, "review this plan")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error from the wedged reviewer, got nil")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("ctx.Err() = %v, want context.DeadlineExceeded (the deadline, not a bare cancel, must be the trigger)", ctx.Err())
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error = %q, want it labelled a timeout", err)
	}
	if elapsed > timescale.D(3*time.Second) {
		t.Errorf("Review took %s — the group kill should return at the deadline, not wait the 10s grace (the #1805 hang)", elapsed)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (a timeout must not be retried)", attempts)
	}
	waitPidGone(t, pidfile)
}

// TestReviewer_PipeLeakEscapedGroupWaitDelayTimeout is the #1805 WaitDelay arm
// and the step-6 hoist guard: the fake claude forks a grandchild that ESCAPES
// its group and holds the inherited stdout pipe, then exits. The group kill
// cannot reach the escaped grandchild, so cmd.WaitDelay force-closes the parent
// pipe fd and cmd.Output() returns a NON-ExitError. The HOISTED ctx.Err() check
// must still classify this as a (timeout); the pre-hoist in-ExitError-branch code
// would have mislabelled it a generic invocation failure. The trigger is
// asserted to be a genuine deadline.
func TestReviewer_PipeLeakEscapedGroupWaitDelayTimeout(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "gc.pid")
	// Grace and deadline both derive from timescale.D so the WaitDelay path
	// stays fast and the elapsed bound below preserves its ratio at any factor.
	defer setKillGrace(timescale.D(300 * time.Millisecond))() // short grace so the WaitDelay path is fast
	t.Cleanup(func() { killPidFromFile(pidfile) })

	var attempts int
	r := NewReviewer(testConfig())
	r.client.Cmd = countingPipeLeakHelper("pipe_leak_escape", pidfile, &attempts)

	ctx, cancel := context.WithTimeout(context.Background(), timescale.D(200*time.Millisecond))
	defer cancel()

	start := time.Now()
	_, _, err := r.Review(ctx, "review this plan")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error from the WaitDelay-forced return, got nil")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("ctx.Err() = %v, want context.DeadlineExceeded (the deadline must be the trigger, not a bare cancel)", ctx.Err())
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error = %q, want the WaitDelay-forced non-ExitError return still classified as a timeout (the step-6 hoist)", err)
	}
	if elapsed > timescale.D(5*time.Second) {
		t.Errorf("Review took %s — WaitDelay should force-close near deadline+grace, not hang on the escaped grandchild", elapsed)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (a timeout must not be retried)", attempts)
	}
	if !pidAliveFromFile(t, pidfile) {
		t.Error("grandchild was reaped — it should have escaped the group and been force-closed via WaitDelay")
	}
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

// flakyDecodeHelperCommand returns a builder whose first attempt emits a
// structurally-malformed verdict body (`flaky_decode_bad`) and whose every later
// attempt emits a valid approve verdict (`happy`). It drives the #901
// decode-retry: a malformed roll must re-roll the reviewer for fresh sampling.
func flakyDecodeHelperCommand(attempts *int) func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		*attempts++
		mode := "happy"
		if *attempts == 1 {
			mode = "flaky_decode_bad"
		}
		return helperCommand(mode)(ctx, name, args...)
	}
}

// TestReviewer_FlakyDecodeRetries asserts a first-roll structurally-malformed
// verdict body re-rolls the reviewer and the second roll's valid approve verdict
// is returned (#901), in exactly two attempts.
func TestReviewer_FlakyDecodeRetries(t *testing.T) {
	var attempts int
	r := NewReviewer(testConfig())
	r.client.Cmd = flakyDecodeHelperCommand(&attempts)

	verdict, _, err := r.Review(context.Background(), "review this plan")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if verdict.Verdict != planreview.VerdictApprove {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, planreview.VerdictApprove)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (one malformed roll + one recovery)", attempts)
	}
}

// TestReviewer_PersistentBadJSONExhausts asserts a reviewer that emits a
// structurally-malformed verdict on every roll terminates as a "decode verdict
// JSON" error after the bounded budget — SetMaxRetries(1) => exactly 2 attempts
// (the ADR-036 backstop: no unbounded re-roll).
func TestReviewer_PersistentBadJSONExhausts(t *testing.T) {
	var attempts int
	r := NewReviewer(testConfig())
	r.SetMaxRetries(1)
	r.client.Cmd = countingHelperCommand("flaky_decode_bad", &attempts)

	_, _, err := r.Review(context.Background(), "review this plan")
	if err == nil {
		t.Fatal("expected a terminal decode error from a persistently-malformed reviewer, got nil")
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (SetMaxRetries(1) => 2 rolls)", attempts)
	}
	if !strings.Contains(err.Error(), "decode verdict JSON") {
		t.Errorf("error = %q, want a 'decode verdict JSON' terminal error", err)
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

// TestReviewer_InvalidEscapeRegexDecodes drives the #739 bug through the full
// envelope-decode -> verdict-decode -> validVerdicts seam: a verdict body
// quoting a regex with a lone `\-` must yield a decoded verdict, not a "decode
// verdict JSON" error, with the regex preserved verbatim in FreeForm.
func TestReviewer_InvalidEscapeRegexDecodes(t *testing.T) {
	verdict, _, err := reviewerWithMode("invalid_escape_regex").Review(context.Background(), "review this plan")
	if err != nil {
		t.Fatalf("Review: got error for a verdict carrying a regex escape, want a decoded verdict: %v", err)
	}
	if verdict.Verdict != planreview.VerdictReject {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, planreview.VerdictReject)
	}
	if !strings.Contains(verdict.FreeForm, `ghs_[A-Za-z0-9_.\-]{36,}`) {
		t.Errorf("FreeForm = %q, want it to contain the regex verbatim", verdict.FreeForm)
	}
}

// TestReviewer_ProsePrefixVerdictDecodes drives the #1576 bug through the full
// envelope-decode -> verdict-decode -> validVerdicts seam: a verdict emitted as
// prose followed by the JSON object must yield a decoded verdict, not a "decode
// verdict JSON" error — proving the shared firstJSONObject extraction reaches
// the claudecode adapter (whose CLI has no response-schema flag).
func TestReviewer_ProsePrefixVerdictDecodes(t *testing.T) {
	verdict, _, err := reviewerWithMode("prose_prefix").Review(context.Background(), "review this plan")
	if err != nil {
		t.Fatalf("Review: got error for a prose-prefixed verdict, want a decoded verdict: %v", err)
	}
	if verdict.Verdict != planreview.VerdictApprove {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, planreview.VerdictApprove)
	}
}

// TestReviewer_PersistentBadJSONCarriesRawSnippet asserts the terminal decode
// error a persistently-undecodable reviewer surfaces now carries the raw-output
// snippet (#1576): the operator-visible *_review_failed reason is diagnosable
// (it names both 'decode verdict JSON' AND the offending raw output) without
// trace archaeology.
func TestReviewer_PersistentBadJSONCarriesRawSnippet(t *testing.T) {
	var attempts int
	r := NewReviewer(testConfig())
	r.SetMaxRetries(1)
	r.client.Cmd = countingHelperCommand("flaky_decode_bad", &attempts)

	_, _, err := r.Review(context.Background(), "review this plan")
	if err == nil {
		t.Fatal("expected a terminal decode error from a persistently-malformed reviewer, got nil")
	}
	if !strings.Contains(err.Error(), "decode verdict JSON") {
		t.Errorf("error = %q, want it to name the 'decode verdict JSON' failure", err)
	}
	if !strings.Contains(err.Error(), "raw output:") {
		t.Errorf("error = %q, want it to carry a 'raw output:' snippet of the offending body", err)
	}
	// The flaky_decode_bad body is the missing-comma verdict; its distinctive
	// text must appear in the quoted snippet.
	if !strings.Contains(err.Error(), "concerns") {
		t.Errorf("error = %q, want it to quote the offending raw verdict body", err)
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

// TestReviewer_IncomingDeadlineHonoredNotCapped asserts invokeOnce respects a
// caller-supplied ctx deadline and does NOT cap it at cfg.Timeout (#747). The
// helper sleeps 300ms — longer than the 50ms cfg.Timeout but well under the 5s
// incoming deadline — and then succeeds. If cfg.Timeout were still applied, the
// child would be killed at 50ms and the review would error; success proves the
// server-computed size-aware deadline is the effective one for large diffs.
func TestReviewer_IncomingDeadlineHonoredNotCapped(t *testing.T) {
	cfg := testConfig()
	cfg.Timeout = 50 * time.Millisecond
	r := NewReviewer(cfg)
	r.client.Cmd = helperCommand("slow_brief")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	verdict, _, err := r.Review(ctx, "review this plan")
	if err != nil {
		t.Fatalf("Review with a generous incoming deadline must not be capped by cfg.Timeout: %v", err)
	}
	if verdict.Verdict != planreview.VerdictApprove {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, planreview.VerdictApprove)
	}
}

// TestReviewer_ExpiredIncomingDeadline asserts that an already-expired incoming
// ctx deadline yields the non-retryable (timeout) error (#747) — the budget
// deadline applied at the server call site surfaces here exactly as the
// internal per-attempt deadline does.
func TestReviewer_ExpiredIncomingDeadline(t *testing.T) {
	var attempts int
	cfg := testConfig()
	r := NewReviewer(cfg)
	r.client.Cmd = countingHelperCommand("happy", &attempts)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	_, _, err := r.Review(ctx, "review this plan")
	if err == nil {
		t.Fatal("expected error from an expired incoming deadline, got nil")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (an expired deadline must not be retried)", attempts)
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

// TestReviewer_PopulatesUsageFromEnvelope asserts a CLI envelope carrying a
// top-level `usage` object surfaces token usage on the verdict with
// Known=true (#681).
func TestReviewer_PopulatesUsageFromEnvelope(t *testing.T) {
	verdict, _, err := reviewerWithMode("happy_usage").Review(context.Background(), "review this plan")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if !verdict.Usage.Known {
		t.Error("Usage.Known = false, want true for an envelope carrying a usage object")
	}
	if verdict.Usage.InputTokens != 1234 || verdict.Usage.OutputTokens != 567 {
		t.Errorf("Usage = %+v, want {InputTokens:1234 OutputTokens:567 Known:true}", verdict.Usage)
	}
	if verdict.Usage.Turns != 1 {
		t.Errorf("Usage.Turns = %d, want 1 (single-shot --print)", verdict.Usage.Turns)
	}
	if verdict.Usage.CacheReadInputTokens != 0 || verdict.Usage.CacheWriteInputTokens != 0 {
		t.Errorf("Usage cache split = read %d / write %d, want 0/0 for an envelope without cache members", verdict.Usage.CacheReadInputTokens, verdict.Usage.CacheWriteInputTokens)
	}
}

// TestReviewer_PopulatesCachedUsageFromEnvelope is the claudecode contract
// pin for the normalized Usage accounting (#1010) and the read/write cache
// split (#1343): InputTokens stays the envelope's cache-EXCLUSIVE fresh count
// (10 — NOT inflated by the ~19k cache tokens), cache_read lands in the READ
// bucket and cache_creation in the WRITE bucket SEPARATELY (not summed), and
// the CachedInputTokens() accessor returns read+write, with Turns=1 (#995). A
// cache-inclusive envelope (or an adapter that started subtracting or summing)
// would break the 10/11944/7319 expectations loudly.
func TestReviewer_PopulatesCachedUsageFromEnvelope(t *testing.T) {
	verdict, _, err := reviewerWithMode("happy_usage_cache").Review(context.Background(), "review this plan")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if !verdict.Usage.Known {
		t.Error("Usage.Known = false, want true for an envelope carrying a usage object")
	}
	if verdict.Usage.InputTokens != 10 || verdict.Usage.OutputTokens != 41 {
		t.Errorf("Usage = %+v, want {InputTokens:10 OutputTokens:41} (fresh, cache-exclusive)", verdict.Usage)
	}
	// cache_read 11944 → CacheReadInputTokens (cheaper); cache_creation 7319 →
	// CacheWriteInputTokens (premium) — kept SEPARATE, not summed.
	if verdict.Usage.CacheReadInputTokens != 11944 || verdict.Usage.CacheWriteInputTokens != 7319 {
		t.Errorf("Usage cache split = read %d / write %d, want 11944/7319 (cache_read vs cache_creation, not summed)", verdict.Usage.CacheReadInputTokens, verdict.Usage.CacheWriteInputTokens)
	}
	// The accessor returns the summed total = 11944 + 7319 = 19263 (back-compat).
	if got := verdict.Usage.CachedInputTokens(); got != 19263 {
		t.Errorf("CachedInputTokens() = %d, want 19263 (cache_read + cache_creation)", got)
	}
	if verdict.Usage.Turns != 1 {
		t.Errorf("Usage.Turns = %d, want 1 (single-shot --print)", verdict.Usage.Turns)
	}
}

// TestReviewer_UsageAbsentDegrades asserts a pre-usage envelope (no `usage`
// object) decodes with Known=false rather than erroring — the graceful
// degradation path the server records at usd=0 (#681).
func TestReviewer_UsageAbsentDegrades(t *testing.T) {
	verdict, _, err := reviewerWithMode("happy").Review(context.Background(), "review this plan")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if verdict.Usage.Known {
		t.Errorf("Usage.Known = true, want false when the envelope carried no usage object; got %+v", verdict.Usage)
	}
}
