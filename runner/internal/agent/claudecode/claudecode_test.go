package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// TestHelperProcess is the test-helper-process pattern from the Go
// stdlib: when invoked with GO_HELPER_PROCESS=1 set in env, this
// test pretends to be a `claude` binary and emits a canned
// stream-json transcript driven by additional env vars. The real
// tests then run the test binary itself with these env vars in
// place of the missing claude binary.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_HELPER_PROCESS") != "1" {
		return
	}
	defer os.Exit(0)

	switch os.Getenv("HELPER_MODE") {
	case "happy":
		// Three event lines, last one carries usage so the
		// harness can populate Result.TokensUsed.
		fmt.Println(`{"type":"system","subtype":"init"}`)
		fmt.Println(`{"type":"assistant","content":"hello"}`)
		fmt.Println(`{"type":"result","usage":{"input_tokens":42,"output_tokens":58}}`)
	case "budget":
		// Each line reports cumulative usage; the third trips a
		// caller-set 100-token budget.
		fmt.Println(`{"type":"system","subtype":"init"}`)
		fmt.Println(`{"type":"assistant","usage":{"input_tokens":40,"output_tokens":40}}`)
		fmt.Println(`{"type":"assistant","usage":{"input_tokens":80,"output_tokens":80}}`)
		// Sleep to allow the harness time to kill us; if we exit
		// cleanly the test still validates that budget was hit.
		time.Sleep(200 * time.Millisecond)
	case "error":
		fmt.Println(`{"type":"system","subtype":"init"}`)
		fmt.Fprintln(os.Stderr, "agent: model rate-limited")
		os.Exit(1)
	case "raw_line":
		// Non-JSON output should not crash the harness; it must
		// still appear in the trace as kind=raw.
		fmt.Println(`not even close to JSON`)
		fmt.Println(`{"type":"result","usage":{"input_tokens":1,"output_tokens":1}}`)
	case "timeout":
		// Sleep longer than the test's configured timeout.
		time.Sleep(2 * time.Second)
	case "thinking_block":
		// Emit a terminal result event carrying the durable
		// interleaved-thinking 400 marker, then exit non-zero. This is
		// the fault Invoke retries.
		fmt.Println(`{"type":"system","subtype":"init"}`)
		fmt.Println(`{"type":"result","subtype":"error","is_error":true,"api_error_status":400,"result":"messages.1.content.0.thinking: thinking or redacted_thinking blocks in the latest assistant message cannot be modified"}`)
		fmt.Fprintln(os.Stderr, "API Error: 400 thinking blocks cannot be modified")
		os.Exit(1)
	case "thinking_block_then_ok":
		// Stateful across re-execs via a marker file: the first run
		// fails with the thinking-block 400, the second succeeds. Lets
		// the retry-then-success path run end to end.
		marker := os.Getenv("MARKER_FILE")
		if _, err := os.Stat(marker); err != nil {
			// First attempt: record that we ran, then fail.
			_ = os.WriteFile(marker, []byte("1"), 0o600)
			fmt.Println(`{"type":"system","subtype":"init"}`)
			fmt.Println(`{"type":"result","subtype":"error","is_error":true,"api_error_status":400,"result":"thinking blocks in the latest assistant message cannot be modified"}`)
			os.Exit(1)
		}
		// Second attempt: clean run with usage.
		fmt.Println(`{"type":"system","subtype":"init"}`)
		fmt.Println(`{"type":"assistant","content":"recovered"}`)
		fmt.Println(`{"type":"result","usage":{"input_tokens":10,"output_tokens":20}}`)
	case "spaced":
		// Emit several events spaced apart so a shortened heartbeat
		// interval fires multiple times across the invocation, with
		// usage advancing so the heartbeat counters move (#580).
		fmt.Println(`{"type":"system","subtype":"init"}`)
		os.Stdout.Sync()
		time.Sleep(60 * time.Millisecond)
		fmt.Println(`{"type":"assistant","usage":{"input_tokens":10,"output_tokens":5}}`)
		os.Stdout.Sync()
		time.Sleep(60 * time.Millisecond)
		fmt.Println(`{"type":"assistant","usage":{"input_tokens":30,"output_tokens":10}}`)
		os.Stdout.Sync()
		time.Sleep(60 * time.Millisecond)
		fmt.Println(`{"type":"result","usage":{"input_tokens":50,"output_tokens":20}}`)
	case "out_of_tree_write":
		// Emit an assistant tool_use writing to an out-of-tree absolute
		// path supplied via OOT_PATH, driving the full scan->detector->
		// event path through Invoke. A clean result line follows so the
		// stage still succeeds (surfacing must not fail the stage).
		fmt.Println(`{"type":"system","subtype":"init"}`)
		fmt.Printf(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":%q}}]}}`+"\n",
			os.Getenv("OOT_PATH"))
		fmt.Println(`{"type":"result","usage":{"input_tokens":1,"output_tokens":1}}`)
	case "echo_env":
		// Echo a single env var so we can assert the harness
		// wired API key forwarding correctly.
		fmt.Printf(`{"type":"env","key":"ANTHROPIC_API_KEY","value":%q}`+"\n",
			os.Getenv("ANTHROPIC_API_KEY"))
		fmt.Println(`{"type":"result","usage":{"input_tokens":1,"output_tokens":1}}`)
	default:
		fmt.Fprintln(os.Stderr, "unknown HELPER_MODE")
		os.Exit(2)
	}
}

// helperCommand returns a Cmd-builder that re-execs the test
// binary as the `claude` stand-in, passing through HELPER_MODE.
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

// helperCommandWithEnv is like helperCommand but layers extra env
// vars (e.g. MARKER_FILE) onto every re-exec so stateful modes can
// coordinate across attempts.
func helperCommandWithEnv(mode string, extra ...string) func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		c := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcess")
		c.Env = append(os.Environ(),
			"GO_HELPER_PROCESS=1",
			"HELPER_MODE="+mode,
		)
		c.Env = append(c.Env, extra...)
		return c
	}
}

// frozenNow returns a Now() that ticks deterministically so tests
// can assert event ordering without fighting wall-clock jitter.
func frozenNow() func() time.Time {
	t := time.Date(2026, 5, 2, 9, 30, 0, 0, time.UTC)
	return func() time.Time {
		t = t.Add(time.Millisecond)
		return t
	}
}

func TestInvoke_HappyPath(t *testing.T) {
	inv := &Invoker{
		Cmd: helperCommand("happy"),
		Now: frozenNow(),
	}
	res, err := inv.Invoke(context.Background(), agent.Invocation{
		RunID:  "11111111-2222-3333-4444-555555555555",
		Stage:  "plan",
		Prompt: "do the thing",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.OK {
		t.Errorf("OK = false; FailureReason = %q", res.FailureReason)
	}
	if res.TokensUsed != 100 {
		t.Errorf("TokensUsed = %d, want 100", res.TokensUsed)
	}
	// invocation_start, system.init, assistant, result, invocation_end → 5
	if got, want := len(res.Events), 5; got != want {
		t.Fatalf("Events = %d, want %d:\n%+v", got, want, res.Events)
	}
	if res.Events[0].Kind != "invocation_start" {
		t.Errorf("Events[0].Kind = %q, want invocation_start", res.Events[0].Kind)
	}
	if res.Events[1].Kind != "system.init" {
		t.Errorf("Events[1].Kind = %q, want system.init", res.Events[1].Kind)
	}
	if res.Events[len(res.Events)-1].Kind != "invocation_end" {
		t.Errorf("last event kind = %q, want invocation_end", res.Events[len(res.Events)-1].Kind)
	}
}

func TestInvoke_BudgetExceeded(t *testing.T) {
	inv := &Invoker{
		Cmd: helperCommand("budget"),
		Now: frozenNow(),
	}
	res, err := inv.Invoke(context.Background(), agent.Invocation{
		Budget: agent.Budget{MaxTokens: 100},
	})
	if !errors.Is(err, agent.ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if res.OK {
		t.Error("OK = true on budget exceeded")
	}
	if res.FailureCategory != "A" {
		t.Errorf("FailureCategory = %q, want A", res.FailureCategory)
	}
	if !strings.Contains(res.FailureReason, "budget exceeded") {
		t.Errorf("FailureReason = %q", res.FailureReason)
	}
	if res.TokensUsed < 100 {
		t.Errorf("TokensUsed = %d, want >= 100", res.TokensUsed)
	}
}

func TestInvoke_AgentError(t *testing.T) {
	inv := &Invoker{
		Cmd: helperCommand("error"),
		Now: frozenNow(),
	}
	res, err := inv.Invoke(context.Background(), agent.Invocation{})
	if !errors.Is(err, agent.ErrAgentFailed) {
		t.Fatalf("err = %v, want wrapping ErrAgentFailed", err)
	}
	if res.OK {
		t.Error("OK = true on agent error")
	}
	if res.FailureCategory != "A" {
		t.Errorf("FailureCategory = %q, want A", res.FailureCategory)
	}
	// stderr from the helper must surface as a kind=stderr event.
	var sawStderr bool
	for _, ev := range res.Events {
		if ev.Kind == "stderr" {
			sawStderr = true
			if !strings.Contains(string(ev.Payload), "rate-limited") {
				t.Errorf("stderr event payload missing message: %s", ev.Payload)
			}
		}
	}
	if !sawStderr {
		t.Error("no kind=stderr event captured")
	}
}

func TestInvoke_RawLine(t *testing.T) {
	inv := &Invoker{
		Cmd: helperCommand("raw_line"),
		Now: frozenNow(),
	}
	res, err := inv.Invoke(context.Background(), agent.Invocation{})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var sawRaw bool
	for _, ev := range res.Events {
		if ev.Kind == "raw" {
			sawRaw = true
			if !strings.Contains(string(ev.Payload), "not even close") {
				t.Errorf("raw event payload missing text: %s", ev.Payload)
			}
		}
	}
	if !sawRaw {
		t.Error("no kind=raw event captured")
	}
}

func TestInvoke_Timeout(t *testing.T) {
	inv := &Invoker{
		Cmd: helperCommand("timeout"),
		Now: frozenNow(),
	}
	res, err := inv.Invoke(context.Background(), agent.Invocation{
		Budget: agent.Budget{Timeout: 100 * time.Millisecond},
	})
	if !errors.Is(err, agent.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if res.OK {
		t.Error("OK = true on timeout")
	}
	if res.FailureCategory != "A" {
		t.Errorf("FailureCategory = %q, want A", res.FailureCategory)
	}
}

func TestInvoke_BinaryNotFound(t *testing.T) {
	inv := &Invoker{
		Binary: "/no/such/binary/anywhere",
		Now:    frozenNow(),
	}
	res, err := inv.Invoke(context.Background(), agent.Invocation{})
	if !errors.Is(err, agent.ErrBinaryNotFound) {
		t.Fatalf("err = %v, want ErrBinaryNotFound", err)
	}
	if res.OK {
		t.Error("OK = true on missing binary")
	}
	if res.FailureCategory != "A" {
		t.Errorf("FailureCategory = %q, want A", res.FailureCategory)
	}
}

func TestInvoke_ForwardsAPIKey(t *testing.T) {
	inv := &Invoker{
		APIKey: "sk-test-1234",
		Cmd:    helperCommand("echo_env"),
		Now:    frozenNow(),
	}
	res, err := inv.Invoke(context.Background(), agent.Invocation{})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false: %s", res.FailureReason)
	}
	var found bool
	for _, ev := range res.Events {
		if ev.Kind != "env" {
			continue
		}
		found = true
		if !strings.Contains(string(ev.Payload), "sk-test-1234") {
			t.Errorf("env event missing API key value: %s", ev.Payload)
		}
	}
	if !found {
		t.Error("no kind=env event captured")
	}
}

func TestInvoke_StartsWithInvocationStart(t *testing.T) {
	// Run a no-op helper just to assert event-shape invariants the
	// other tests would also catch but make implicit.
	inv := &Invoker{Cmd: helperCommand("happy"), Now: frozenNow()}
	res, _ := inv.Invoke(context.Background(), agent.Invocation{
		RunID: "rid", Stage: "plan",
	})
	if len(res.Events) == 0 {
		t.Fatal("no events captured")
	}
	first := res.Events[0]
	if first.Kind != "invocation_start" {
		t.Errorf("first event kind = %q, want invocation_start", first.Kind)
	}
	if !strings.Contains(string(first.Payload), `"run_id":"rid"`) {
		t.Errorf("invocation_start payload missing run_id: %s", first.Payload)
	}
}

func TestNew_DefaultsBinary(t *testing.T) {
	// Ensures the public constructor doesn't accidentally hard-code
	// a different default than DefaultBinary.
	inv := New("k")
	if inv.Binary != "" {
		t.Errorf("Binary = %q, want empty (resolved at Invoke time)", inv.Binary)
	}
	if inv.APIKey != "k" {
		t.Errorf("APIKey = %q, want k", inv.APIKey)
	}
	// New defaults the thinking-block retry budget at construction so a
	// zero value on a struct literal unambiguously disables retry.
	if inv.MaxThinkingBlockRetries != 1 {
		t.Errorf("MaxThinkingBlockRetries = %d, want 1", inv.MaxThinkingBlockRetries)
	}
}

func TestIsThinkingBlock400_TruthTable(t *testing.T) {
	// The exact result string observed in run 657ade99 (issue #574
	// impl) — pins the detection so wording drift fails loudly.
	const realResult = `{"type":"result","subtype":"error","is_error":true,"api_error_status":400,"result":"messages.1.content.0.thinking: thinking or redacted_thinking blocks in the latest assistant message cannot be modified"}`
	cases := []struct {
		name    string
		payload string
		stderr  string
		want    bool
	}{
		{"real_trace_string", realResult, "", true},
		{"marker_in_stderr_only", "", "API Error 400: thinking blocks cannot be modified", true},
		{"generic_400_not_thinking", `{"type":"result","is_error":true,"api_error_status":400,"result":"context length exceeded"}`, "", false},
		{"status_400_but_no_modify_phrase", `{"type":"result","api_error_status":400,"result":"thinking budget exhausted"}`, "", false},
		{"marker_phrases_but_status_not_400", `{"type":"result","api_error_status":429,"result":"thinking blocks cannot be modified"}`, "", false},
		{"unrelated_error", `{"type":"result","is_error":true,"result":"file not found"}`, "boom", false},
		{"empty", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isThinkingBlock400([]byte(tc.payload), tc.stderr)
			if got != tc.want {
				t.Errorf("isThinkingBlock400(%q, %q) = %v, want %v", tc.payload, tc.stderr, got, tc.want)
			}
		})
	}
}

func TestInvoke_ThinkingBlockRetrySucceeds(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "attempt.marker")
	inv := &Invoker{
		Cmd:                     helperCommandWithEnv("thinking_block_then_ok", "MARKER_FILE="+marker),
		Now:                     frozenNow(),
		MaxThinkingBlockRetries: 1,
	}
	res, err := inv.Invoke(context.Background(), agent.Invocation{Stage: "implement"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false; FailureReason = %q", res.FailureReason)
	}
	// Two attempts ran: two invocation_start events with an agent_retry
	// marker between them.
	var starts, retries int
	for _, ev := range res.Events {
		switch ev.Kind {
		case "invocation_start":
			starts++
		case "agent_retry":
			retries++
			if !strings.Contains(string(ev.Payload), `"reason":"agent_api_thinking_block"`) {
				t.Errorf("agent_retry payload missing reason: %s", ev.Payload)
			}
		}
	}
	if starts != 2 {
		t.Errorf("invocation_start count = %d, want 2", starts)
	}
	if retries != 1 {
		t.Errorf("agent_retry count = %d, want 1", retries)
	}
	// Tokens are cumulative; only the successful attempt reports usage.
	if res.TokensUsed != 30 {
		t.Errorf("TokensUsed = %d, want 30 (cumulative)", res.TokensUsed)
	}
}

func TestInvoke_ThinkingBlockRetryExhausted(t *testing.T) {
	inv := &Invoker{
		Cmd:                     helperCommand("thinking_block"),
		Now:                     frozenNow(),
		MaxThinkingBlockRetries: 1,
	}
	res, err := inv.Invoke(context.Background(), agent.Invocation{Stage: "implement"})
	if !errors.Is(err, agent.ErrAgentThinkingBlock) {
		t.Fatalf("err = %v, want wrapping ErrAgentThinkingBlock", err)
	}
	if errors.Is(err, agent.ErrAgentFailed) {
		t.Error("ErrAgentThinkingBlock must not wrap ErrAgentFailed")
	}
	if res.OK {
		t.Error("OK = true on exhausted retry")
	}
	if res.FailureCategory != "A" {
		t.Errorf("FailureCategory = %q, want A (so stage-level retry still sees category A)", res.FailureCategory)
	}
	// Exactly maxAttempts = MaxThinkingBlockRetries+1 = 2 attempts.
	var starts, retries int
	var lastEnd string
	for _, ev := range res.Events {
		switch ev.Kind {
		case "invocation_start":
			starts++
		case "agent_retry":
			retries++
		case "invocation_end":
			lastEnd = string(ev.Payload)
		}
	}
	if starts != 2 {
		t.Errorf("invocation_start count = %d, want 2", starts)
	}
	if retries != 1 {
		t.Errorf("agent_retry count = %d, want 1", retries)
	}
	if !strings.Contains(lastEnd, `"outcome":"agent_api_thinking_block"`) {
		t.Errorf("final invocation_end outcome not agent_api_thinking_block: %s", lastEnd)
	}
}

func TestInvoke_ThinkingBlockNoRetryWhenDisabled(t *testing.T) {
	// MaxThinkingBlockRetries == 0 (struct literal default) means a
	// single attempt, no retry, even for a thinking-block fault.
	inv := &Invoker{
		Cmd: helperCommand("thinking_block"),
		Now: frozenNow(),
	}
	res, err := inv.Invoke(context.Background(), agent.Invocation{})
	if !errors.Is(err, agent.ErrAgentThinkingBlock) {
		t.Fatalf("err = %v, want ErrAgentThinkingBlock", err)
	}
	var starts int
	for _, ev := range res.Events {
		if ev.Kind == "invocation_start" {
			starts++
		}
	}
	if starts != 1 {
		t.Errorf("invocation_start count = %d, want 1 (retry disabled)", starts)
	}
}

func TestInvoke_RespawnIsStateless(t *testing.T) {
	// Each attempt must be a fresh, stateless exec: no --continue or
	// --resume can leak the corrupted thinking-block history into the
	// retry. Capture every attempt's argv and assert.
	marker := filepath.Join(t.TempDir(), "attempt.marker")
	var argvs [][]string
	inv := &Invoker{
		Now:                     frozenNow(),
		MaxThinkingBlockRetries: 1,
		Cmd: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			argvs = append(argvs, append([]string(nil), args...))
			c := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcess")
			c.Env = append(os.Environ(),
				"GO_HELPER_PROCESS=1",
				"HELPER_MODE=thinking_block_then_ok",
				"MARKER_FILE="+marker,
			)
			return c
		},
	}
	if _, err := inv.Invoke(context.Background(), agent.Invocation{Prompt: "p"}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(argvs) != 2 {
		t.Fatalf("attempts = %d, want 2", len(argvs))
	}
	for i, argv := range argvs {
		for _, a := range argv {
			if a == "--continue" || a == "--resume" {
				t.Errorf("attempt %d argv carries stateful flag %q: %v", i, a, argv)
			}
		}
	}
}

func TestInvoke_GenericExitNoRetry(t *testing.T) {
	// A plain non-zero exit with no thinking-block marker must map to
	// ErrAgentFailed with a single attempt — no false-positive retry.
	inv := &Invoker{
		Cmd:                     helperCommand("error"),
		Now:                     frozenNow(),
		MaxThinkingBlockRetries: 1,
	}
	res, err := inv.Invoke(context.Background(), agent.Invocation{})
	if !errors.Is(err, agent.ErrAgentFailed) {
		t.Fatalf("err = %v, want ErrAgentFailed", err)
	}
	if errors.Is(err, agent.ErrAgentThinkingBlock) {
		t.Error("generic exit must not be classified as thinking-block")
	}
	var starts int
	for _, ev := range res.Events {
		if ev.Kind == "invocation_start" {
			starts++
		}
	}
	if starts != 1 {
		t.Errorf("invocation_start count = %d, want 1 (no retry on generic failure)", starts)
	}
}

// assistantWrite builds an assistant stream-json line carrying a single
// file-writing tool_use targeting path.
func assistantWrite(tool, field, path string) []byte {
	return []byte(fmt.Sprintf(
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":%q,"input":{%q:%q}}]}}`,
		tool, field, path))
}

func TestOutOfTreeWrites(t *testing.T) {
	// workDir is a real, existing dir; its deepest-existing-ancestor is
	// itself, so containment of NEW files under it must resolve cleanly.
	// NOTE: the test sandbox often sets TMPDIR under /tmp, so t.TempDir()
	// lands inside the /tmp allowlist — to test genuine working-dir
	// containment independent of /tmp, the in/out-of-tree cases use
	// roots={workDir} only and a synthetic out-of-tree absolute path.
	workDir := t.TempDir()
	allowed := append([]string{workDir}, allowedExtraDirs...)
	const outside = "/opt/fishhawk-oot-test"

	// Portable symlink case: linkDir is a symlink to realRoot. A write
	// through the symlinked path must be judged inside realRoot.
	realRoot := t.TempDir()
	linkDir := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realRoot, linkDir); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	cases := []struct {
		name    string
		line    []byte
		roots   []string
		wantOOT bool
	}{
		{
			// Condition (1): a NEW (not-yet-created) in-tree file must
			// NOT be flagged — containment resolves against the deepest
			// existing ancestor (workDir), not the target itself.
			name:    "in_tree_new_file_not_flagged",
			line:    assistantWrite("Edit", "file_path", filepath.Join(workDir, "pkg", "new.go")),
			roots:   []string{workDir},
			wantOOT: false,
		},
		{
			// Condition (1): a NEW out-of-tree file IS flagged.
			name:    "out_of_tree_new_file_flagged",
			line:    assistantWrite("Write", "file_path", filepath.Join(outside, "sub", "new.txt")),
			roots:   []string{workDir},
			wantOOT: true,
		},
		{
			name:    "home_like_path_flagged",
			line:    assistantWrite("Edit", "file_path", "/some/operator/.claude/memory.md"),
			roots:   []string{workDir},
			wantOOT: true,
		},
		{
			name:    "under_allowlisted_tmp_not_flagged",
			line:    assistantWrite("Write", "file_path", "/tmp/fishhawk-plan.json"),
			roots:   allowed,
			wantOOT: false,
		},
		{
			// macOS: /tmp is a symlink to /private/tmp and the agent
			// emits the resolved /private/tmp form. Build that resolved
			// form explicitly and assert it's still inside the /tmp root.
			name:    "tmp_resolved_symlink_form_not_flagged",
			line:    assistantWrite("Write", "file_path", filepath.Join(resolveSymlinks("/tmp"), "fishhawk-plan.json")),
			roots:   []string{"/tmp"},
			wantOOT: false,
		},
		{
			name:    "symlinked_root_resolved_not_flagged",
			line:    assistantWrite("Write", "file_path", filepath.Join(linkDir, "out.txt")),
			roots:   []string{realRoot},
			wantOOT: false,
		},
		{
			name:    "notebook_edit_out_of_tree_flagged",
			line:    assistantWrite("NotebookEdit", "notebook_path", filepath.Join(outside, "nb.ipynb")),
			roots:   []string{workDir},
			wantOOT: true,
		},
		{
			// Bash is the unconfinable escape hatch — its tool_use is not
			// a direct filesystem write and must NOT be flagged here.
			name:    "bash_tool_use_not_flagged",
			line:    []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"echo x > /etc/passwd"}}]}}`),
			roots:   allowed,
			wantOOT: false,
		},
		{
			name:    "read_tool_use_not_flagged",
			line:    assistantWrite("Read", "file_path", "/etc/hosts"),
			roots:   allowed,
			wantOOT: false,
		},
		{
			name:    "non_assistant_line_not_flagged",
			line:    []byte(`{"type":"result","usage":{"input_tokens":1,"output_tokens":1}}`),
			roots:   allowed,
			wantOOT: false,
		},
		{
			name:    "malformed_line_not_flagged",
			line:    []byte(`not json at all`),
			roots:   allowed,
			wantOOT: false,
		},
		{
			name:    "empty_line_not_flagged",
			line:    []byte(``),
			roots:   allowed,
			wantOOT: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := outOfTreeWrites(tc.line, tc.roots)
			if tc.wantOOT && len(got) == 0 {
				t.Fatalf("expected an out-of-tree write, got none")
			}
			if !tc.wantOOT && len(got) != 0 {
				t.Fatalf("expected no out-of-tree write, got %+v", got)
			}
		})
	}
}

// TestInvoke_OutOfTreeWriteSurfaced drives the full scan->detector->
// event path through Invoke: a single out-of-tree Write tool_use lands
// exactly one out_of_tree_write event in Result.Events while Result.OK
// stays true (surfacing must never fail the stage).
func TestInvoke_OutOfTreeWriteSurfaced(t *testing.T) {
	workDir := t.TempDir()
	// Synthetic path outside both workDir and the /tmp allowlist. (The
	// test sandbox puts t.TempDir() under /tmp, so a temp path would be
	// in-allowlist; /opt/... is genuinely out of tree. The helper only
	// prints the path — nothing is written — so it need not exist.)
	const ootPath = "/opt/fishhawk-oot-test/escaped.txt"
	inv := &Invoker{
		Cmd: helperCommandWithEnv("out_of_tree_write", "OOT_PATH="+ootPath),
		Now: frozenNow(),
	}
	res, err := inv.Invoke(context.Background(), agent.Invocation{
		RunID:      "rid-oot",
		Stage:      "implement",
		WorkingDir: workDir,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false; surfacing must not fail the stage: %q", res.FailureReason)
	}
	var oot []agent.Event
	for _, ev := range res.Events {
		if ev.Kind == "out_of_tree_write" {
			oot = append(oot, ev)
		}
	}
	if len(oot) != 1 {
		t.Fatalf("out_of_tree_write events = %d, want 1:\n%+v", len(oot), res.Events)
	}
	payload := string(oot[0].Payload)
	if !strings.Contains(payload, ootPath) {
		t.Errorf("event missing path %q: %s", ootPath, payload)
	}
	if !strings.Contains(payload, `"tool":"Write"`) {
		t.Errorf("event missing tool=Write: %s", payload)
	}
	if !strings.Contains(payload, `"run_id":"rid-oot"`) {
		t.Errorf("event missing run_id: %s", payload)
	}
	if !strings.Contains(payload, `"stage":"implement"`) {
		t.Errorf("event missing stage: %s", payload)
	}
}

// progressHeartbeats parses every stage_progress JSONL line written to
// a ProgressSink buffer, asserting each carries the four expected
// fields. Returns the decoded heartbeats in arrival order.
type heartbeat struct {
	Event         string  `json:"event"`
	ElapsedSecs   *int    `json:"elapsed_seconds"`
	Turns         *int    `json:"turns"`
	TokensSoFar   *int    `json:"tokens_so_far"`
	LastEventKind *string `json:"last_event_kind"`
}

func parseHeartbeats(t *testing.T, raw []byte) []heartbeat {
	t.Helper()
	var hbs []heartbeat
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var hb heartbeat
		if err := json.Unmarshal([]byte(line), &hb); err != nil {
			t.Fatalf("ProgressSink line is not valid JSON: %q: %v", line, err)
		}
		if hb.Event != "stage_progress" {
			t.Fatalf("ProgressSink line has event=%q, want stage_progress: %q", hb.Event, line)
		}
		if hb.ElapsedSecs == nil || hb.Turns == nil || hb.TokensSoFar == nil || hb.LastEventKind == nil {
			t.Fatalf("stage_progress line missing one of the four fields: %q", line)
		}
		hbs = append(hbs, hb)
	}
	return hbs
}

// TestInvokeOnce_ProgressHeartbeats drives invokeOnce against a binary
// that emits several spaced events under a shortened heartbeat
// interval, and asserts heartbeats were written, each is a complete
// well-formed JSON line, and the counters advance across them (#580).
func TestInvokeOnce_ProgressHeartbeats(t *testing.T) {
	var sink bytes.Buffer
	inv := &Invoker{
		Cmd:               helperCommand("spaced"),
		HeartbeatInterval: 20 * time.Millisecond,
		// Note: real Now (default) — frozenNow mutates shared state
		// unsynchronized and would race the heartbeat goroutine under -race.
	}
	res, _, err := inv.invokeOnce(context.Background(), agent.Invocation{
		ProgressSink: &sink,
	})
	if err != nil {
		t.Fatalf("invokeOnce: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false; FailureReason = %q", res.FailureReason)
	}

	hbs := parseHeartbeats(t, sink.Bytes())
	if len(hbs) == 0 {
		t.Fatal("no stage_progress heartbeats written to ProgressSink")
	}
	// Counters must be monotonic non-decreasing and the last one must
	// reflect progress past the first event.
	last := hbs[len(hbs)-1]
	if *last.Turns < 1 {
		t.Errorf("final heartbeat turns = %d, want >= 1", *last.Turns)
	}
	for i := 1; i < len(hbs); i++ {
		if *hbs[i].Turns < *hbs[i-1].Turns {
			t.Errorf("turns went backwards: %d then %d", *hbs[i-1].Turns, *hbs[i].Turns)
		}
		if *hbs[i].TokensSoFar < *hbs[i-1].TokensSoFar {
			t.Errorf("tokens_so_far went backwards: %d then %d", *hbs[i-1].TokensSoFar, *hbs[i].TokensSoFar)
		}
	}
	// Heartbeats are ProgressSink-only: they must never enter res.Events.
	for _, ev := range res.Events {
		if ev.Kind == "stage_progress" {
			t.Error("stage_progress leaked into res.Events; must be ProgressSink-only")
		}
	}
}

// TestInvokeOnce_NilProgressSinkNoHeartbeats confirms a nil
// ProgressSink emits zero heartbeats and leaves res.Events as the
// pre-#580 happy path produced it.
func TestInvokeOnce_NilProgressSinkNoHeartbeats(t *testing.T) {
	inv := &Invoker{
		Cmd:               helperCommand("happy"),
		Now:               frozenNow(),
		HeartbeatInterval: time.Millisecond, // would fire constantly if a sink existed
	}
	res, _, err := inv.invokeOnce(context.Background(), agent.Invocation{
		// ProgressSink nil on purpose.
	})
	if err != nil {
		t.Fatalf("invokeOnce: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false; FailureReason = %q", res.FailureReason)
	}
	// invocation_start, system.init, assistant, result, invocation_end → 5
	if got, want := len(res.Events), 5; got != want {
		t.Fatalf("Events = %d, want %d (nil sink must not alter event stream)", got, want)
	}
	for _, ev := range res.Events {
		if ev.Kind == "stage_progress" {
			t.Error("stage_progress event present with nil ProgressSink")
		}
	}
}

// TestInvokeOnce_ProgressSinkReturnsPromptly asserts invokeOnce with a
// ProgressSink returns without hanging (no goroutine leak / ticker
// leak) on both the normal-completion and budget-hit early-break paths
// (#580 condition 3).
func TestInvokeOnce_ProgressSinkReturnsPromptly(t *testing.T) {
	cases := []struct {
		name   string
		mode   string
		budget agent.Budget
	}{
		{"normal_completion", "happy", agent.Budget{}},
		{"budget_hit_break", "budget", agent.Budget{MaxTokens: 100}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sink bytes.Buffer
			inv := &Invoker{
				Cmd:               helperCommand(tc.mode),
				HeartbeatInterval: 10 * time.Millisecond,
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _, _ = inv.invokeOnce(context.Background(), agent.Invocation{
					Budget:       tc.budget,
					ProgressSink: &sink,
				})
			}()
			select {
			case <-done:
				// Returned — the heartbeat goroutine was joined, no hang.
			case <-time.After(5 * time.Second):
				t.Fatal("invokeOnce did not return within 5s — goroutine leak / hang")
			}
		})
	}
}
