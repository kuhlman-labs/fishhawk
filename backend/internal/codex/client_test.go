package codex

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// secretAPIKey is the sentinel forwarded as OPENAI_API_KEY in the forwarding /
// redaction tests; it must never appear in a surfaced error string.
const secretAPIKey = "sk-test-codex-secret-DO-NOT-LEAK"

// TestHelperProcess is the Go stdlib test-helper-process pattern: when
// GO_HELPER_PROCESS=1 is set, this test pretends to be the `codex` binary and
// emits a canned `codex exec --json` JSONL transcript driven by HELPER_MODE. The
// real tests re-exec the test binary itself in place of the missing `codex`. The
// JSONL event shapes here are pinned against the installed Codex CLI
// (codex-cli 0.137.0) so a future CLI drift fails a test rather than silently
// dropping the reviewer.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_HELPER_PROCESS") != "1" {
		return
	}
	defer os.Exit(0)

	// A pinned, well-formed turn.completed usage line: 1234 input (incl. 100
	// cached) + 567 output + 33 reasoning → InputTokens=1234, OutputTokens=600.
	const usageLine = `{"type":"turn.completed","usage":{"input_tokens":1234,"cached_input_tokens":100,"output_tokens":567,"reasoning_output_tokens":33}}`

	switch os.Getenv("HELPER_MODE") {
	case "happy":
		// A full single-turn transcript whose agent_message text is the verdict
		// JSON, plus a turn.completed usage line.
		fmt.Println(`{"type":"thread.started","thread_id":"t-1"}`)
		fmt.Println(`{"type":"turn.started"}`)
		fmt.Println(`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"{\"verdict\":\"approve\"}"}}`)
		fmt.Println(usageLine)
	case "happy_no_usage":
		// agent_message but no turn.completed usage line → Known=false.
		fmt.Println(`{"type":"thread.started","thread_id":"t-1"}`)
		fmt.Println(`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"{\"verdict\":\"approve\"}"}}`)
	case "multi_message":
		// Two agent_message items: the FINAL one is the model's conclusion and
		// must win. The first says reject, the last approve.
		fmt.Println(`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"{\"verdict\":\"reject\"}"}}`)
		fmt.Println(`{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"{\"verdict\":\"approve\"}"}}`)
		fmt.Println(usageLine)
	case "no_message":
		// Only usage, never an agent_message → hard error (no verdict).
		fmt.Println(`{"type":"thread.started","thread_id":"t-1"}`)
		fmt.Println(usageLine)
	case "raw_line":
		// A non-JSON log line (Codex interleaves these on stdout) must be
		// skipped fail-open, with the real verdict still decoded.
		fmt.Println(`2026-06-08T21:46:01Z ERROR codex_core_skills::loader: warning`)
		fmt.Println(`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"{\"verdict\":\"approve\"}"}}`)
		fmt.Println(usageLine)
	case "non_json_verdict":
		// Valid transcript but the agent_message text is not JSON.
		fmt.Println(`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"this is not json"}}`)
	case "bad_verdict":
		// Valid agent_message JSON, but verdict outside the closed set.
		fmt.Println(`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"{\"verdict\":\"maybe\"}"}}`)
	case "flaky_decode_bad":
		// An agent_message whose text is structurally-malformed verdict JSON — a
		// missing comma between members (`"approve" "concerns"`), the #901 class
		// strict-then-repair planreview.DecodeVerdict cannot rescue. The JSONL
		// line itself is valid; only the nested verdict body is malformed.
		fmt.Println(`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"{\"verdict\":\"approve\" \"concerns\":[]}"}}`)
		fmt.Println(usageLine)
	case "fenced_escape_regex":
		// The #739/#889 path: the agent_message text is a fenced verdict whose
		// free_form quotes a regex with a lone `\-`. The fence-strip + escape
		// repair in planreview.DecodeVerdict must still decode it. The fence and
		// backslash are double-escaped here because the text is itself a JSON
		// string value inside the JSONL line.
		fmt.Println(`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"` + "```json\\n{\\\"verdict\\\":\\\"reject\\\",\\\"free_form\\\":\\\"redact ghs_[A-Za-z0-9_.\\\\-]{36,}\\\"}\\n```" + `"}}`)
		fmt.Println(usageLine)
	case "echo_env":
		// Approve only if the adapter forwarded the expected OPENAI_API_KEY;
		// otherwise reject. Proves explicit key forwarding without ever printing
		// the key value.
		verdict := "reject"
		if os.Getenv("OPENAI_API_KEY") == secretAPIKey {
			verdict = "approve"
		}
		fmt.Printf(`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"{\"verdict\":\"%s\"}"}}`+"\n", verdict)
		fmt.Println(usageLine)
	case "error":
		// Non-zero exit stands in for a subprocess failure; a fixed,
		// non-secret stderr line is folded into the diagnostic.
		fmt.Fprintln(os.Stderr, "codex: model rate-limited")
		os.Exit(1)
	case "killed":
		// Reproduce the #620 SIGKILL with empty stderr: surfaces as an
		// *exec.ExitError with ctx.Err()==nil (the retryable external/OOM class).
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {}
	case "slow":
		// Sleep past a short Timeout so the per-attempt deadline fires and the
		// child is killed with ctx.Err()==DeadlineExceeded (the timeout class,
		// which must NOT be retried).
		time.Sleep(5 * time.Second)
	default:
		fmt.Fprintln(os.Stderr, "unknown HELPER_MODE")
		os.Exit(2)
	}
}

// helperCommand returns a Cmd-builder that re-execs the test binary as the
// `codex` stand-in, passing through HELPER_MODE.
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
		Binary:    "codex",
		Model:     "gpt-5-codex",
		MaxTokens: 4096,
		Timeout:   5 * time.Second,
	}
}

// countingHelperCommand wraps helperCommand to count Cmd-builder invocations —
// i.e. how many subprocess attempts the retry loop made.
func countingHelperCommand(mode string, attempts *int) func(ctx context.Context, name string, args ...string) *exec.Cmd {
	build := helperCommand(mode)
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		*attempts++
		return build(ctx, name, args...)
	}
}

// flakyHelperCommand runs the `killed` mode on the first attempt (a transient
// crash) and `happy` on every later attempt.
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

// flakyDecodeHelperCommand runs the `flaky_decode_bad` mode (a structurally-
// malformed verdict body) on the first attempt and `happy` on every later
// attempt. It drives the #901 decode-retry: a malformed roll must re-roll the
// reviewer for fresh sampling. It lives here (the file owning the TestHelperProcess
// fake-binary harness) so reviewer_test.go can reference it without a scope-drift
// compile break.
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

func clientWithMode(mode string) *Client {
	c := NewClient(testConfig())
	c.Cmd = helperCommand(mode)
	return c
}

// TestClient_NewClientDefaults asserts NewClient defaults Binary and the retry
// budget so production always retries a transient crash once.
func TestClient_NewClientDefaults(t *testing.T) {
	c := NewClient(Config{Model: "m"})
	if c.cfg.Binary != DefaultBinary {
		t.Errorf("Binary = %q, want %q", c.cfg.Binary, DefaultBinary)
	}
	if c.cfg.MaxRetries != 1 {
		t.Errorf("MaxRetries = %d, want 1", c.cfg.MaxRetries)
	}
}

// TestInference_HappyUsage asserts a well-formed transcript yields the verdict
// text, the configured model, and summed usage with Known=true.
func TestInference_HappyUsage(t *testing.T) {
	text, model, usage, err := clientWithMode("happy").Inference(context.Background(), "review")
	if err != nil {
		t.Fatalf("Inference: %v", err)
	}
	if text != `{"verdict":"approve"}` {
		t.Errorf("text = %q, want the agent_message verdict body", text)
	}
	if model != "gpt-5-codex" {
		t.Errorf("model = %q, want gpt-5-codex", model)
	}
	if !usage.Known {
		t.Error("Usage.Known = false, want true for a transcript with a turn.completed usage line")
	}
	// 1234 input; 567 output + 33 reasoning = 600 output.
	if usage.InputTokens != 1234 || usage.OutputTokens != 600 {
		t.Errorf("Usage = %+v, want {InputTokens:1234 OutputTokens:600 Known:true}", usage)
	}
}

// TestInference_UsageAbsentDegrades asserts a transcript with no turn.completed
// usage line decodes with Known=false rather than erroring (#681/#682).
func TestInference_UsageAbsentDegrades(t *testing.T) {
	_, _, usage, err := clientWithMode("happy_no_usage").Inference(context.Background(), "review")
	if err != nil {
		t.Fatalf("Inference: %v", err)
	}
	if usage.Known {
		t.Errorf("Usage.Known = true, want false when no usage line appeared; got %+v", usage)
	}
}

// TestInference_FinalMessageWins asserts that with multiple agent_message items
// the LAST one is returned as the verdict body.
func TestInference_FinalMessageWins(t *testing.T) {
	text, _, _, err := clientWithMode("multi_message").Inference(context.Background(), "review")
	if err != nil {
		t.Fatalf("Inference: %v", err)
	}
	if text != `{"verdict":"approve"}` {
		t.Errorf("text = %q, want the FINAL agent_message body (approve)", text)
	}
}

// TestInference_RawLineSkipped asserts a non-JSON log line interleaved on stdout
// is skipped fail-open and the real verdict still decodes.
func TestInference_RawLineSkipped(t *testing.T) {
	text, _, usage, err := clientWithMode("raw_line").Inference(context.Background(), "review")
	if err != nil {
		t.Fatalf("Inference: %v", err)
	}
	if text != `{"verdict":"approve"}` {
		t.Errorf("text = %q, want the verdict body despite the interleaved raw line", text)
	}
	if !usage.Known {
		t.Error("Usage.Known = false, want true; the raw line must not drop the usage line")
	}
}

// TestInference_NoAgentMessage asserts a transcript that never emits an
// agent_message is a hard error (no verdict to decode), distinct from the
// graceful Known=false usage degradation.
func TestInference_NoAgentMessage(t *testing.T) {
	_, _, _, err := clientWithMode("no_message").Inference(context.Background(), "review")
	if err == nil {
		t.Fatal("expected error when no agent_message is present, got nil")
	}
	if !strings.Contains(err.Error(), "no agent_message") {
		t.Errorf("error = %q, want it to name the missing agent_message", err)
	}
}

// TestInference_BinaryMissing asserts a missing binary maps to a precise,
// non-retryable error naming the binary.
func TestInference_BinaryMissing(t *testing.T) {
	var attempts int
	c := NewClient(testConfig())
	c.cfg.Binary = "definitely-not-a-real-binary-xyz"
	c.Cmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		attempts++
		return exec.CommandContext(ctx, name, args...)
	}
	_, _, _, err := c.Inference(context.Background(), "review")
	if err == nil {
		t.Fatal("expected error for a missing binary, got nil")
	}
	if !strings.Contains(err.Error(), "binary not found") {
		t.Errorf("error = %q, want it to name the missing binary", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (binary-missing must not be retried)", attempts)
	}
}

// TestInference_TimeoutNotRetried asserts a per-attempt timeout fails fast: it
// is labelled `timeout` and attempted EXACTLY ONCE even with MaxRetries=1 (#606).
func TestInference_TimeoutNotRetried(t *testing.T) {
	var attempts int
	cfg := testConfig()
	cfg.Timeout = 50 * time.Millisecond
	c := NewClient(cfg)
	c.Cmd = countingHelperCommand("slow", &attempts)

	_, _, _, err := c.Inference(context.Background(), "review")
	if err == nil {
		t.Fatal("expected error from per-attempt timeout, got nil")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (a timeout must not be retried)", attempts)
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error = %q, want it labelled a timeout", err)
	}
}

// TestInference_RetryRecovers asserts a first-attempt transient crash is retried
// and the second attempt's verdict is returned (MaxRetries=1).
func TestInference_RetryRecovers(t *testing.T) {
	var attempts int
	c := NewClient(testConfig())
	c.Cmd = flakyHelperCommand(&attempts)

	text, _, _, err := c.Inference(context.Background(), "review")
	if err != nil {
		t.Fatalf("Inference: %v", err)
	}
	if text != `{"verdict":"approve"}` {
		t.Errorf("text = %q, want the recovered verdict body", text)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (one crash + one recovery)", attempts)
	}
}

// TestInference_RetryExhausted asserts a persistently crashing subprocess is
// retried up to the budget and the final diagnostic names the external/OOM cause
// and an elapsed substring (MaxRetries=1 → exactly 2 tries).
func TestInference_RetryExhausted(t *testing.T) {
	var attempts int
	c := NewClient(testConfig())
	c.Cmd = countingHelperCommand("killed", &attempts)

	_, _, _, err := c.Inference(context.Background(), "review")
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

// TestInference_StderrSurfaced asserts captured child stderr is folded into the
// diagnostic error so a non-zero exit is not silently undiagnosable.
func TestInference_StderrSurfaced(t *testing.T) {
	c := NewClient(testConfig())
	c.cfg.MaxRetries = 0
	c.Cmd = helperCommand("error")

	_, _, _, err := c.Inference(context.Background(), "review")
	if err == nil {
		t.Fatal("expected error from non-zero subprocess exit, got nil")
	}
	if !strings.Contains(err.Error(), "model rate-limited") {
		t.Errorf("error = %q, want it to surface the child stderr", err)
	}
}

// TestInference_ForwardsAPIKey asserts the configured APIKey reaches the child
// as OPENAI_API_KEY: the fake binary approves only when it sees the sentinel.
func TestInference_ForwardsAPIKey(t *testing.T) {
	cfg := testConfig()
	cfg.APIKey = secretAPIKey
	c := NewClient(cfg)
	c.Cmd = helperCommand("echo_env")

	text, _, _, err := c.Inference(context.Background(), "review")
	if err != nil {
		t.Fatalf("Inference: %v", err)
	}
	if text != `{"verdict":"approve"}` {
		t.Errorf("text = %q, want approve (the child must have seen the forwarded OPENAI_API_KEY)", text)
	}
}

// TestInference_APIKeyNotLeakedInError asserts the forwarded OPENAI_API_KEY
// value never appears in a surfaced error string, even on the stderr-folding
// failure path.
func TestInference_APIKeyNotLeakedInError(t *testing.T) {
	cfg := testConfig()
	cfg.APIKey = secretAPIKey
	cfg.MaxRetries = 0
	c := NewClient(cfg)
	c.Cmd = helperCommand("error")

	_, _, _, err := c.Inference(context.Background(), "review")
	if err == nil {
		t.Fatal("expected error from non-zero subprocess exit, got nil")
	}
	if strings.Contains(err.Error(), secretAPIKey) {
		t.Errorf("error string leaked the API key: %q", err)
	}
}

// TestInference_EmptyAPIKeyNotAnError asserts an empty APIKey is not an error:
// the child inherits the host env and succeeds.
func TestInference_EmptyAPIKeyNotAnError(t *testing.T) {
	cfg := testConfig()
	cfg.APIKey = ""
	c := NewClient(cfg)
	c.Cmd = helperCommand("happy")

	_, _, _, err := c.Inference(context.Background(), "review")
	if err != nil {
		t.Fatalf("Inference with empty APIKey must not error: %v", err)
	}
}

// TestInference_ArgvModelAndEffort asserts the config→argv boundary: Model and
// ReasoningEffort are appended as `--model <m>` / `-c model_reasoning_effort=<e>`
// only when set, both placed BEFORE the prompt positional (pinned by the exact
// argv comparison), and an all-empty config yields exactly the base argv — the
// inherit-host-default regression guard. The returned model label must equal
// the configured (argv) model, so label and reality match.
func TestInference_ArgvModelAndEffort(t *testing.T) {
	const prompt = "review"
	tests := []struct {
		name     string
		model    string
		effort   string
		wantArgv []string
	}{
		{
			name:   "model and effort set",
			model:  "gpt-5.5",
			effort: "medium",
			wantArgv: []string{
				"exec", "--json", "--skip-git-repo-check",
				"--model", "gpt-5.5",
				"-c", "model_reasoning_effort=medium",
				prompt,
			},
		},
		{
			name:  "model set only",
			model: "gpt-5.5",
			wantArgv: []string{
				"exec", "--json", "--skip-git-repo-check",
				"--model", "gpt-5.5",
				prompt,
			},
		},
		{
			name:   "effort set only",
			effort: "high",
			wantArgv: []string{
				"exec", "--json", "--skip-git-repo-check",
				"-c", "model_reasoning_effort=high",
				prompt,
			},
		},
		{
			name:     "both empty inherits host default",
			wantArgv: []string{"exec", "--json", "--skip-git-repo-check", prompt},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Model = tt.model
			cfg.ReasoningEffort = tt.effort
			c := NewClient(cfg)
			var captured []string
			build := helperCommand("happy")
			c.Cmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
				captured = append([]string(nil), args...)
				return build(ctx, name, args...)
			}

			_, model, _, err := c.Inference(context.Background(), prompt)
			if err != nil {
				t.Fatalf("Inference: %v", err)
			}
			if !slices.Equal(captured, tt.wantArgv) {
				t.Errorf("argv = %q, want %q", captured, tt.wantArgv)
			}
			if model != tt.model {
				t.Errorf("model label = %q, want the configured model %q", model, tt.model)
			}
		})
	}
}

// TestParseStream_SumsMultipleTurns asserts usage is SUMMED across multiple
// turn.completed lines rather than last-wins.
func TestParseStream_SumsMultipleTurns(t *testing.T) {
	out := []byte(`{"type":"item.completed","item":{"type":"agent_message","text":"{\"verdict\":\"approve\"}"}}
{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":8,"reasoning_output_tokens":2}}
{"type":"turn.completed","usage":{"input_tokens":50,"cached_input_tokens":0,"output_tokens":4,"reasoning_output_tokens":1}}`)
	text, usage, err := parseStream(out)
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if text != `{"verdict":"approve"}` {
		t.Errorf("text = %q, want the verdict body", text)
	}
	if usage.InputTokens != 150 || usage.OutputTokens != 15 || !usage.Known {
		t.Errorf("Usage = %+v, want {InputTokens:150 OutputTokens:15 Known:true}", usage)
	}
}
