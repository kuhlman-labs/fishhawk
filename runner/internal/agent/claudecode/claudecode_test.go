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
	case "cache_split":
		// A transcript whose terminal result event carries the prompt-cache
		// split alongside the (already cache-exclusive) input_tokens, so the
		// adapter surfaces Result.CacheRead/CacheWrite while InputTokens stays
		// unchanged (#1349).
		fmt.Println(`{"type":"system","subtype":"init"}`)
		fmt.Println(`{"type":"result","model":"claude-opus-4-8","usage":{"input_tokens":200,"output_tokens":80,"cache_read_input_tokens":400,"cache_creation_input_tokens":150}}`)
	case "cache_only":
		// A terminal result event with ZERO fresh input/output but non-zero
		// cache read/write — the cache-only spend edge (#1349). The scan loop
		// must still capture the cache buckets onto Result rather than dropping
		// the line because its fresh input+output sum is 0.
		fmt.Println(`{"type":"system","subtype":"init"}`)
		fmt.Println(`{"type":"result","model":"claude-opus-4-8","usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":500,"cache_creation_input_tokens":120}}`)
	case "model_split":
		// A realistic transcript: the assistant event carries the
		// model id + usage nested under `message` (the real
		// stream-json shape), and the terminal result event reports
		// the cumulative split. Notably, NO temperature is emitted —
		// claude --print stream-json does not expose it (G6).
		fmt.Println(`{"type":"system","subtype":"init"}`)
		fmt.Println(`{"type":"assistant","message":{"model":"claude-opus-4-8","usage":{"input_tokens":120,"output_tokens":30}}}`)
		fmt.Println(`{"type":"result","model":"claude-opus-4-8","usage":{"input_tokens":200,"output_tokens":80}}`)
	case "out_of_tree_write":
		// Emit an assistant tool_use writing to an out-of-tree absolute
		// path supplied via OOT_PATH, driving the full scan->detector->
		// event path through Invoke. A clean result line follows so the
		// stage still succeeds (surfacing must not fail the stage).
		fmt.Println(`{"type":"system","subtype":"init"}`)
		fmt.Printf(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":%q}}]}}`+"\n",
			os.Getenv("OOT_PATH"))
		fmt.Println(`{"type":"result","usage":{"input_tokens":1,"output_tokens":1}}`)
	case "loop":
		// Emit many identical Bash tool_use lines so the loop detector
		// trips on an unbroken run of the same signature. A trailing sleep
		// lets the harness kill us after it trips; if we somehow exit
		// cleanly first the test still asserts the trip on the events read.
		fmt.Println(`{"type":"system","subtype":"init"}`)
		for n := 0; n < 30; n++ {
			fmt.Println(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}`)
			os.Stdout.Sync()
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(500 * time.Millisecond)
	case "varied_tools":
		// Distinct tool calls (different files / commands) interleaved with
		// a couple of legitimate repeats — must NOT trip the detector. Ends
		// with a clean result so the stage succeeds.
		fmt.Println(`{"type":"system","subtype":"init"}`)
		fmt.Println(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"a.go"}}]}}`)
		fmt.Println(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"a.go"}}]}}`)
		fmt.Println(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"a.go"}}]}}`)
		fmt.Println(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test"}}]}}`)
		fmt.Println(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"b.go"}}]}}`)
		fmt.Println(`{"type":"result","usage":{"input_tokens":5,"output_tokens":5}}`)
	case "wait_poll_loop":
		// Emit many identical SANCTIONED scope-amendment wait-poll Bash
		// calls (well above the lowered threshold). The detector exempts
		// them (#1273), so this must NOT trip; a clean result follows so
		// the stage succeeds.
		fmt.Println(`{"type":"system","subtype":"init"}`)
		for n := 0; n < 30; n++ {
			fmt.Println(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"curl -s $FISHHAWK_BACKEND_URL/v0/runs/rid/scope-amendments?wait=30"}}]}}`)
			os.Stdout.Sync()
			time.Sleep(2 * time.Millisecond)
		}
		fmt.Println(`{"type":"result","usage":{"input_tokens":5,"output_tokens":5}}`)
	case "loop_with_wait_polls":
		// A real identical-call loop (go test ./...) interleaved with
		// sanctioned wait-polls. Because the wait-poll is a no-op (skipped,
		// not reset), the real loop's streak still accumulates and trips —
		// proving the skip does not reset an in-progress real loop (#1273).
		fmt.Println(`{"type":"system","subtype":"init"}`)
		for n := 0; n < 30; n++ {
			fmt.Println(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}`)
			fmt.Println(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"curl -s $FISHHAWK_BACKEND_URL/v0/runs/rid/scope-amendments?wait=30"}}]}}`)
			os.Stdout.Sync()
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(500 * time.Millisecond)
	case "structured_output":
		// A captured-shape stream-json transcript whose terminal result event
		// carries a top-level structured_output object conforming to the passed
		// --json-schema (#1325). The harness must capture those exact bytes onto
		// Result.StructuredOutput.
		fmt.Println(`{"type":"system","subtype":"init"}`)
		fmt.Println(`{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]}}`)
		fmt.Println(`{"type":"result","usage":{"input_tokens":3,"output_tokens":4},"structured_output":{"plan_version":"standard_v1","summary":"hi"}}`)
	case "echo_env":
		// Echo a single env var so we can assert the harness
		// wired API key forwarding correctly.
		fmt.Printf(`{"type":"env","key":"ANTHROPIC_API_KEY","value":%q}`+"\n",
			os.Getenv("ANTHROPIC_API_KEY"))
		fmt.Println(`{"type":"result","usage":{"input_tokens":1,"output_tokens":1}}`)
	case "echo_env_multi":
		// Echo both the API-key var (from the APIKey field) and a
		// FISHHAWK_* var (from Invocation.Env) so the override test can
		// assert each configured value wins over a conflicting inherited
		// one (#899). A separate mode from echo_env so the single-var
		// TestInvoke_ForwardsAPIKey assertion stays valid.
		for _, k := range []string{"ANTHROPIC_API_KEY", "FISHHAWK_API_TOKEN"} {
			fmt.Printf(`{"type":"env","key":%q,"value":%q}`+"\n", k, os.Getenv(k))
		}
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

// capturingHelperCommand wraps the "happy" helper process but records the
// argv it was built with into *captured, so a test can assert the presence or
// absence of the --model flag (#1013).
func capturingHelperCommand(captured *[]string) func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		*captured = append([]string(nil), args...)
		c := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcess")
		c.Env = append(os.Environ(),
			"GO_HELPER_PROCESS=1",
			"HELPER_MODE=happy",
		)
		return c
	}
}

func argsHaveFlagValue(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func argsHaveFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// TestInvoke_ModelFlag asserts the implement-model routing (#1013): a non-empty
// Invocation.Model appends `--model <m>`; an empty Model appends NO --model flag
// (byte-identical to today's spawn).
func TestInvoke_ModelFlag(t *testing.T) {
	t.Run("non-empty model appends --model", func(t *testing.T) {
		var captured []string
		inv := &Invoker{Cmd: capturingHelperCommand(&captured), Now: frozenNow()}
		if _, err := inv.Invoke(context.Background(), agent.Invocation{Prompt: "p", Model: "claude-opus-4-8"}); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		if !argsHaveFlagValue(captured, "--model", "claude-opus-4-8") {
			t.Fatalf("expected --model claude-opus-4-8 in args, got %v", captured)
		}
	})
	t.Run("empty model omits --model (byte-identical spawn)", func(t *testing.T) {
		var captured []string
		inv := &Invoker{Cmd: capturingHelperCommand(&captured), Now: frozenNow()}
		if _, err := inv.Invoke(context.Background(), agent.Invocation{Prompt: "p"}); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		if argsHaveFlag(captured, "--model") {
			t.Fatalf("expected NO --model flag for empty model, got %v", captured)
		}
	})
}

// TestInvoke_JSONSchemaFlag asserts the structured-output gate (#1325): a
// non-empty Invocation.JSONSchema appends `--json-schema <schema>`; an empty
// JSONSchema appends NO such flag (byte-identical to today's spawn).
func TestInvoke_JSONSchemaFlag(t *testing.T) {
	t.Run("non-empty schema appends --json-schema with the schema text", func(t *testing.T) {
		var captured []string
		inv := &Invoker{Cmd: capturingHelperCommand(&captured), Now: frozenNow()}
		schema := `{"type":"object","required":["plan_version"]}`
		if _, err := inv.Invoke(context.Background(), agent.Invocation{Prompt: "p", JSONSchema: schema}); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		if !argsHaveFlagValue(captured, "--json-schema", schema) {
			t.Fatalf("expected --json-schema %q in args, got %v", schema, captured)
		}
	})
	t.Run("empty schema omits --json-schema (byte-identical spawn)", func(t *testing.T) {
		var captured []string
		inv := &Invoker{Cmd: capturingHelperCommand(&captured), Now: frozenNow()}
		if _, err := inv.Invoke(context.Background(), agent.Invocation{Prompt: "p"}); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		if argsHaveFlag(captured, "--json-schema") {
			t.Fatalf("expected NO --json-schema flag for empty JSONSchema, got %v", captured)
		}
	})
}

// TestInvoke_CapturesStructuredOutput pins the capture path (#1325): the terminal
// result event's top-level structured_output object is surfaced verbatim on
// Result.StructuredOutput. If the CLI ever stops emitting it, this fixture-driven
// test fails.
func TestInvoke_CapturesStructuredOutput(t *testing.T) {
	inv := &Invoker{Cmd: helperCommand("structured_output"), Now: frozenNow()}
	res, err := inv.Invoke(context.Background(), agent.Invocation{Prompt: "p", JSONSchema: `{"type":"object"}`})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false; FailureReason = %q", res.FailureReason)
	}
	want := `{"plan_version":"standard_v1","summary":"hi"}`
	if string(res.StructuredOutput) != want {
		t.Errorf("StructuredOutput = %q, want %q", res.StructuredOutput, want)
	}
}

// TestInvoke_NoStructuredOutputWhenAbsent asserts the fallback trigger: a normal
// transcript with no structured_output field leaves Result.StructuredOutput nil,
// so the runner falls through to the agent-written-file path.
func TestInvoke_NoStructuredOutputWhenAbsent(t *testing.T) {
	inv := &Invoker{Cmd: helperCommand("happy"), Now: frozenNow()}
	res, err := inv.Invoke(context.Background(), agent.Invocation{Prompt: "p"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.StructuredOutput != nil {
		t.Errorf("StructuredOutput = %q, want nil when the result event carries none", res.StructuredOutput)
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

// fixtureStream is a recorded claude --print stream-json transcript
// used to pin parseLine's model-id + input/output split extraction.
// It deliberately contains no `temperature` field, backing the G6
// reproducibility claim that claude does not surface sampling params:
// TestParseLine_ModelSplit asserts the substring is absent so a
// future claude version that starts emitting it trips this test
// rather than being silently ignored.
const fixtureStream = `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"model":"claude-opus-4-8","usage":{"input_tokens":120,"output_tokens":30}}}
{"type":"result","model":"claude-opus-4-8","usage":{"input_tokens":200,"output_tokens":80}}`

func TestParseLine_ModelSplit(t *testing.T) {
	if strings.Contains(fixtureStream, "temperature") {
		t.Fatal("fixture unexpectedly contains a temperature field; the G6 best-effort assumption no longer holds — update otelemit to capture it")
	}

	lines := strings.Split(fixtureStream, "\n")
	ts := time.Now()

	// assistant line: model + usage nested under `message`.
	_, info, ok := parseLine([]byte(lines[1]), ts)
	if !ok {
		t.Fatal("assistant line: hasUsage = false")
	}
	if info.Model != "claude-opus-4-8" {
		t.Errorf("assistant Model = %q, want claude-opus-4-8", info.Model)
	}
	if info.InputTokens != 120 || info.OutputTokens != 30 {
		t.Errorf("assistant split = (%d,%d), want (120,30)", info.InputTokens, info.OutputTokens)
	}

	// result line: top-level model + usage.
	_, info, ok = parseLine([]byte(lines[2]), ts)
	if !ok {
		t.Fatal("result line: hasUsage = false")
	}
	if info.Model != "claude-opus-4-8" {
		t.Errorf("result Model = %q", info.Model)
	}
	if info.InputTokens != 200 || info.OutputTokens != 80 {
		t.Errorf("result split = (%d,%d), want (200,80)", info.InputTokens, info.OutputTokens)
	}
}

func TestInvoke_ModelAndSplitSurfaced(t *testing.T) {
	inv := &Invoker{
		Cmd: helperCommand("model_split"),
		Now: frozenNow(),
	}
	res, err := inv.Invoke(context.Background(), agent.Invocation{
		RunID: "r", Stage: "implement", Prompt: "go",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Model != "claude-opus-4-8" {
		t.Errorf("Result.Model = %q, want claude-opus-4-8", res.Model)
	}
	// Latest cumulative usage line wins: result reported (200,80).
	if res.InputTokens != 200 || res.OutputTokens != 80 {
		t.Errorf("Result split = (%d,%d), want (200,80)", res.InputTokens, res.OutputTokens)
	}
	if res.TokensUsed != 280 {
		t.Errorf("Result.TokensUsed = %d, want 280", res.TokensUsed)
	}
}

// TestParseLine_CacheSplit pins the #1349 cache capture: Anthropic reports
// input_tokens EXCLUSIVE of the cache buckets, so parseLine lands
// cache_read_input_tokens / cache_creation_input_tokens in CacheRead/CacheWrite
// while leaving InputTokens (the fresh portion) unchanged.
func TestParseLine_CacheSplit(t *testing.T) {
	ts := time.Now()
	line := `{"type":"result","model":"claude-opus-4-8","usage":{"input_tokens":200,"output_tokens":80,"cache_read_input_tokens":400,"cache_creation_input_tokens":150}}`
	_, info, ok := parseLine([]byte(line), ts)
	if !ok {
		t.Fatal("hasUsage = false")
	}
	if info.InputTokens != 200 || info.OutputTokens != 80 {
		t.Errorf("split = (%d,%d), want (200,80) — input is already cache-exclusive", info.InputTokens, info.OutputTokens)
	}
	if info.CacheReadInputTokens != 400 {
		t.Errorf("CacheReadInputTokens = %d, want 400", info.CacheReadInputTokens)
	}
	if info.CacheWriteInputTokens != 150 {
		t.Errorf("CacheWriteInputTokens = %d, want 150 (from cache_creation_input_tokens)", info.CacheWriteInputTokens)
	}
}

// TestInvoke_CacheSplitSurfaced drives the full scan-loop capture: the cache
// buckets from the winning usage line must land on the aggregated Result with
// InputTokens unchanged (#1349).
func TestInvoke_CacheSplitSurfaced(t *testing.T) {
	inv := &Invoker{
		Cmd: helperCommand("cache_split"),
		Now: frozenNow(),
	}
	res, err := inv.Invoke(context.Background(), agent.Invocation{
		RunID: "r", Stage: "implement", Prompt: "go",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.InputTokens != 200 || res.OutputTokens != 80 {
		t.Errorf("Result split = (%d,%d), want (200,80)", res.InputTokens, res.OutputTokens)
	}
	if res.CacheReadInputTokens != 400 || res.CacheWriteInputTokens != 150 {
		t.Errorf("Result cache split = (read %d, write %d), want (400, 150)",
			res.CacheReadInputTokens, res.CacheWriteInputTokens)
	}
}

// TestParseLine_CacheOnly pins the cache-only edge (#1349): a usage line with
// zero fresh input/output but non-zero cache read/write still reports
// hasUsage=true, so the scan loop captures it instead of dropping the cache
// spend. A regression that gated hasUsage on the fresh input+output sum alone
// would fail here.
func TestParseLine_CacheOnly(t *testing.T) {
	ts := time.Now()
	line := `{"type":"result","model":"claude-opus-4-8","usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":500,"cache_creation_input_tokens":120}}`
	_, info, ok := parseLine([]byte(line), ts)
	if !ok {
		t.Fatal("hasUsage = false on a cache-only line; cache spend would be dropped")
	}
	if info.InputTokens != 0 || info.OutputTokens != 0 {
		t.Errorf("fresh split = (%d,%d), want (0,0)", info.InputTokens, info.OutputTokens)
	}
	if info.CacheReadInputTokens != 500 || info.CacheWriteInputTokens != 120 {
		t.Errorf("cache split = (read %d, write %d), want (500, 120)",
			info.CacheReadInputTokens, info.CacheWriteInputTokens)
	}
}

// TestInvoke_CacheOnlySurfaced drives the full scan-loop capture for the
// cache-only edge: a result line with zero fresh input/output but non-zero
// cache buckets must still land its cache read/write on the aggregated Result
// (#1349). Before the hasUsage fix the scan loop dropped this line entirely.
func TestInvoke_CacheOnlySurfaced(t *testing.T) {
	inv := &Invoker{
		Cmd: helperCommand("cache_only"),
		Now: frozenNow(),
	}
	res, err := inv.Invoke(context.Background(), agent.Invocation{
		RunID: "r", Stage: "implement", Prompt: "go",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false; FailureReason = %q", res.FailureReason)
	}
	if res.InputTokens != 0 || res.OutputTokens != 0 {
		t.Errorf("Result fresh split = (%d,%d), want (0,0)", res.InputTokens, res.OutputTokens)
	}
	if res.CacheReadInputTokens != 500 || res.CacheWriteInputTokens != 120 {
		t.Errorf("Result cache split = (read %d, write %d), want (500, 120) — cache-only spend must not be dropped",
			res.CacheReadInputTokens, res.CacheWriteInputTokens)
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

// TestInvoke_ForwardedEnvOverridesInherited proves the env-override fix
// (#899) end to end: the Cmd builder pre-seeds CONFLICTING inherited
// values for both ANTHROPIC_API_KEY (the APIKey field) and
// FISHHAWK_API_TOKEN (an Invocation.Env key) onto cmd.Env, and the
// echo_env_multi child must still observe the CONFIGURED values — a
// subprocess resolves a variable to the FIRST matching entry, so without
// the strip-then-append both inherited entries would shadow the override.
func TestInvoke_ForwardedEnvOverridesInherited(t *testing.T) {
	inv := &Invoker{
		APIKey: "sk-configured-wins",
		Cmd: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			c := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcess")
			// Pre-seed cmd.Env (non-nil so the adapter skips its os.Environ
			// seed) with conflicting inherited values placed BEFORE the
			// adapter layers the configured ones on top.
			c.Env = append(os.Environ(),
				"GO_HELPER_PROCESS=1",
				"HELPER_MODE=echo_env_multi",
				"ANTHROPIC_API_KEY=inherited-wrong-value",
				"FISHHAWK_API_TOKEN=inherited-wrong-token",
			)
			return c
		},
		Now: frozenNow(),
	}
	res, err := inv.Invoke(context.Background(), agent.Invocation{
		Env: map[string]string{"FISHHAWK_API_TOKEN": "fhm_configured_wins"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false: %s", res.FailureReason)
	}
	got := map[string]string{}
	for _, ev := range res.Events {
		if ev.Kind != "env" {
			continue
		}
		var e struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		if err := json.Unmarshal(ev.Payload, &e); err != nil {
			t.Fatalf("env event not JSON: %v: %s", err, ev.Payload)
		}
		got[e.Key] = e.Value
	}
	if got["ANTHROPIC_API_KEY"] != "sk-configured-wins" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want sk-configured-wins (configured must override inherited)", got["ANTHROPIC_API_KEY"])
	}
	if got["FISHHAWK_API_TOKEN"] != "fhm_configured_wins" {
		t.Errorf("FISHHAWK_API_TOKEN = %q, want fhm_configured_wins (per-run Env must override inherited)", got["FISHHAWK_API_TOKEN"])
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

func TestToolCallSignatures(t *testing.T) {
	cases := []struct {
		name string
		line string
		want []string
	}{
		{
			name: "single_tool_use",
			line: `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"a.go"}}]}}`,
			want: []string{`Read {"file_path":"a.go"}`},
		},
		{
			name: "multiple_tool_uses_one_line",
			line: `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"a.go"}},{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`,
			want: []string{`Read {"file_path":"a.go"}`, `Bash {"command":"ls"}`},
		},
		{
			// Key order in the input must not change the signature — the
			// canonicaliser sorts object keys.
			name: "key_order_canonicalised",
			line: `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"new":"y","file_path":"a.go"}}]}}`,
			want: []string{`Edit {"file_path":"a.go","new":"y"}`},
		},
		{
			name: "non_assistant_line",
			line: `{"type":"result","usage":{"input_tokens":1,"output_tokens":1}}`,
			want: nil,
		},
		{
			name: "text_block_no_tool",
			line: `{"type":"assistant","message":{"content":[{"type":"text","text":"thinking"}]}}`,
			want: nil,
		},
		{
			name: "malformed_line",
			line: `not json`,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toolCallSignatures([]byte(tc.line))
			if len(got) != len(tc.want) {
				t.Fatalf("signatures = %#v, want %#v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("signature[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestInvoke_LoopDetected drives the full scan->detector->abort path: a
// helper that emits a run of identical tool_use lines trips the detector
// (LoopThreshold lowered for determinism) and the stage fails with
// agent.ErrLoopDetected, category A, a loop_detected event, and a reason
// naming the count.
func TestInvoke_LoopDetected(t *testing.T) {
	inv := &Invoker{
		Cmd:           helperCommand("loop"),
		Now:           frozenNow(),
		LoopThreshold: 4,
	}
	res, err := inv.Invoke(context.Background(), agent.Invocation{
		RunID: "rid-loop", Stage: "implement", Prompt: "go",
	})
	if !errors.Is(err, agent.ErrLoopDetected) {
		t.Fatalf("err = %v, want wrapping ErrLoopDetected", err)
	}
	if errors.Is(err, agent.ErrAgentFailed) {
		t.Error("ErrLoopDetected must not wrap ErrAgentFailed")
	}
	if res.OK {
		t.Error("OK = true on loop detected")
	}
	if res.FailureCategory != "A" {
		t.Errorf("FailureCategory = %q, want A", res.FailureCategory)
	}
	if !strings.Contains(res.FailureReason, "loop detected") {
		t.Errorf("FailureReason = %q, want it to mention 'loop detected'", res.FailureReason)
	}
	if !strings.Contains(res.FailureReason, "4 identical") {
		t.Errorf("FailureReason = %q, want it to name the count (4)", res.FailureReason)
	}
	var loopEvents int
	for _, ev := range res.Events {
		if ev.Kind == "loop_detected" {
			loopEvents++
			if !strings.Contains(string(ev.Payload), `"count":4`) {
				t.Errorf("loop_detected payload missing count: %s", ev.Payload)
			}
			if !strings.Contains(string(ev.Payload), `"run_id":"rid-loop"`) {
				t.Errorf("loop_detected payload missing run_id: %s", ev.Payload)
			}
		}
	}
	if loopEvents != 1 {
		t.Errorf("loop_detected event count = %d, want 1", loopEvents)
	}
}

// TestInvoke_NoLoopOnVariedTools confirms the detector does not false-abort
// a legitimate trace of varied tool calls with a couple of benign repeats.
func TestInvoke_NoLoopOnVariedTools(t *testing.T) {
	inv := &Invoker{
		Cmd:           helperCommand("varied_tools"),
		Now:           frozenNow(),
		LoopThreshold: 3,
	}
	res, err := inv.Invoke(context.Background(), agent.Invocation{
		RunID: "rid-varied", Stage: "implement", Prompt: "go",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false; varied tools must not trip the loop detector: %q", res.FailureReason)
	}
	for _, ev := range res.Events {
		if ev.Kind == "loop_detected" {
			t.Errorf("loop_detected event emitted on a varied-tool trace: %s", ev.Payload)
		}
	}
}

// TestIsSanctionedWaitPoll is the pure-predicate table for the #1273
// loop-detector exemption. It covers the EXEMPT form (the documented
// scope-amendments wait-poll) and the false-positive cases that MUST still
// be counted by the detector (binding condition 1): a wait= that is not a
// query param of the scope-amendments path, a non-curl Bash command that
// merely contains both substrings, the non-waiting amendments GET, and a
// non-Bash tool whose input contains the substrings.
func TestIsSanctionedWaitPoll(t *testing.T) {
	cases := []struct {
		name string
		sig  string
		want bool
	}{
		{
			// EXEMPT: wait= is the sole query parameter.
			name: "exempt_wait_query_only",
			sig:  `Bash {"command":"curl -s $FISHHAWK_BACKEND_URL/v0/runs/r/scope-amendments?wait=30"}`,
			want: true,
		},
		{
			// EXEMPT: wait= introduced by '&' after another query param.
			name: "exempt_wait_after_other_param",
			sig:  `Bash {"command":"curl -s $FISHHAWK_BACKEND_URL/v0/runs/r/scope-amendments?status=pending&wait=30"}`,
			want: true,
		},
		{
			// (c) Non-waiting amendments GET (no wait=) — still counts.
			name: "non_waiting_get",
			sig:  `Bash {"command":"curl -s $FISHHAWK_BACKEND_URL/v0/runs/r/scope-amendments"}`,
			want: false,
		},
		{
			// (a) scope-amendments present + a wait= that is NOT a query
			// param of that path — still counts.
			name: "wait_not_query_param",
			sig:  `Bash {"command":"curl $URL/scope-amendments && echo wait=now"}`,
			want: false,
		},
		{
			// (a) variant: a non-wait query param, with the unrelated wait=
			// after the URL token ends — still counts.
			name: "other_query_param_unrelated_wait",
			sig:  `Bash {"command":"curl \"$URL/scope-amendments?status=pending\" ; sleep wait=5"}`,
			want: false,
		},
		{
			// (b) Non-curl Bash command that merely contains both strings —
			// still counts.
			name: "non_curl_both_substrings",
			sig:  `Bash {"command":"echo scope-amendments wait=30"}`,
			want: false,
		},
		{
			// (d) Non-Bash tool whose input contains the substrings —
			// still counts.
			name: "non_bash_tool",
			sig:  `Read {"file_path":"scope-amendments?wait=30"}`,
			want: false,
		},
		{
			// Unrelated Bash command, neither substring — still counts.
			name: "unrelated_bash",
			sig:  `Bash {"command":"go test ./..."}`,
			want: false,
		},
		{
			// Empty signature is never a wait-poll; the existing empty-sig
			// no-op contract in loopdetect.go is unaffected.
			name: "empty",
			sig:  "",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSanctionedWaitPoll(tc.sig); got != tc.want {
				t.Errorf("isSanctionedWaitPoll(%q) = %v, want %v", tc.sig, got, tc.want)
			}
		})
	}
}

// TestInvoke_NoLoopOnSanctionedWaitPoll drives the full scan->detector path
// with a run of identical SANCTIONED wait-poll Bash calls well above the
// threshold: the exemption (#1273) means the detector never trips and the
// stage succeeds (no loop_detected event, no agent.ErrLoopDetected).
func TestInvoke_NoLoopOnSanctionedWaitPoll(t *testing.T) {
	inv := &Invoker{
		Cmd:           helperCommand("wait_poll_loop"),
		Now:           frozenNow(),
		LoopThreshold: 4,
	}
	res, err := inv.Invoke(context.Background(), agent.Invocation{
		RunID: "rid-wait", Stage: "implement", Prompt: "go",
	})
	if errors.Is(err, agent.ErrLoopDetected) {
		t.Fatalf("sanctioned wait-poll tripped the loop detector: %v", err)
	}
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false; sanctioned wait-poll must not trip the loop detector: %q", res.FailureReason)
	}
	for _, ev := range res.Events {
		if ev.Kind == "loop_detected" {
			t.Errorf("loop_detected event emitted on a sanctioned wait-poll trace: %s", ev.Payload)
		}
	}
}

// TestInvoke_LoopDetectedNotResetByWaitPolls proves the skip is a true no-op
// rather than a reset: a real identical-call loop interleaved with sanctioned
// wait-polls still trips (the wait-polls neither count toward nor reset the
// real streak), and the tripping signature is the real repeated call.
func TestInvoke_LoopDetectedNotResetByWaitPolls(t *testing.T) {
	inv := &Invoker{
		Cmd:           helperCommand("loop_with_wait_polls"),
		Now:           frozenNow(),
		LoopThreshold: 4,
	}
	res, err := inv.Invoke(context.Background(), agent.Invocation{
		RunID: "rid-loop-wait", Stage: "implement", Prompt: "go",
	})
	if !errors.Is(err, agent.ErrLoopDetected) {
		t.Fatalf("err = %v, want wrapping ErrLoopDetected (interleaved wait-polls must not reset a real loop)", err)
	}
	if res.OK {
		t.Error("OK = true on a real loop interleaved with wait-polls")
	}
	var loopEvents int
	for _, ev := range res.Events {
		if ev.Kind == "loop_detected" {
			loopEvents++
			if !strings.Contains(string(ev.Payload), "go test") {
				t.Errorf("loop signature should be the real repeated call, not the wait-poll: %s", ev.Payload)
			}
			if strings.Contains(string(ev.Payload), "scope-amendments") {
				t.Errorf("loop signature must not be the exempt wait-poll: %s", ev.Payload)
			}
		}
	}
	if loopEvents != 1 {
		t.Errorf("loop_detected event count = %d, want 1", loopEvents)
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
