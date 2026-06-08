package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// TestHelperProcess is the test-helper-process pattern from the Go
// stdlib: when invoked with GO_HELPER_PROCESS=1 set in env, this test
// pretends to be a `codex` binary and emits a canned `codex exec --json`
// transcript driven by HELPER_MODE. The real tests then run the test
// binary itself with these env vars in place of the missing codex binary.
//
// NOTE: the fake binary is a direct child only — it cannot model Codex
// grandchildren (a shell command-execution subprocess or an MCP server)
// that inherit the stdout pipe. The process-GROUP kill on the
// budget/timeout paths exists precisely for those grandchildren; this
// seam validates the kill+drain ORDERING (the killed direct child's pipe
// is drained before Wait), not the grandchild-reaping itself.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_HELPER_PROCESS") != "1" {
		return
	}
	defer os.Exit(0)

	switch os.Getenv("HELPER_MODE") {
	case "happy":
		// A full single-turn transcript; the turn.completed line carries
		// per-turn usage (42+58 = 100 tokens).
		fmt.Println(`{"type":"thread.started","thread_id":"t-1"}`)
		fmt.Println(`{"type":"turn.started"}`)
		fmt.Println(`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"hello"}}`)
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":42,"cached_input_tokens":10,"output_tokens":50,"reasoning_output_tokens":8}}`)
	case "no_usage":
		// A transcript that never emits a usage block — Result tokens stay
		// zero and Model stays empty (known_usage=false downstream, #682).
		fmt.Println(`{"type":"thread.started","thread_id":"t-1"}`)
		fmt.Println(`{"type":"turn.started"}`)
		fmt.Println(`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"done"}}`)
	case "multi_turn":
		// Two turn.completed lines: Codex reports usage PER TURN, so the
		// adapter must SUM them (not last-wins). 100+10 then 100+10 → 220.
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":8,"reasoning_output_tokens":2}}`)
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":8,"reasoning_output_tokens":2}}`)
	case "budget":
		// Per-turn usage that SUMS past a caller-set 100-token budget on the
		// second line (80 then 160). Sleep so the harness has time to kill
		// the process group; if we exit cleanly first the test still asserts
		// the budget trip on the events read.
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":40,"cached_input_tokens":0,"output_tokens":40,"reasoning_output_tokens":0}}`)
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":40,"cached_input_tokens":0,"output_tokens":40,"reasoning_output_tokens":0}}`)
		time.Sleep(2 * time.Second)
	case "error":
		fmt.Println(`{"type":"thread.started","thread_id":"t-1"}`)
		fmt.Fprintln(os.Stderr, "codex: model rate-limited")
		os.Exit(1)
	case "raw_line":
		// A non-JSON log line (Codex interleaves these on stdout) must not
		// crash the harness; it must surface as kind=raw.
		fmt.Println(`2026-06-08T21:46:01Z ERROR codex_core_skills::loader: warning`)
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1,"reasoning_output_tokens":0}}`)
	case "timeout":
		// Sleep longer than the test's configured timeout.
		time.Sleep(5 * time.Second)
	case "model":
		// Defensive: if a future Codex version surfaces a top-level model
		// id on an event, parseLine must pin it onto Result.Model.
		fmt.Println(`{"type":"turn.started","model":"gpt-5-codex"}`)
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":5,"cached_input_tokens":0,"output_tokens":5,"reasoning_output_tokens":0}}`)
	case "echo_env":
		// Echo the env vars the runner forwards so the test can assert
		// OPENAI_API_KEY + the FISHHAWK_* MCP vars all reach the child.
		for _, k := range []string{"OPENAI_API_KEY", "FISHHAWK_API_TOKEN", "FISHHAWK_BACKEND_URL"} {
			fmt.Printf(`{"type":"env","key":%q,"value":%q}`+"\n", k, os.Getenv(k))
		}
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1,"reasoning_output_tokens":0}}`)
	case "spaced":
		// Emit several events spaced apart so a shortened heartbeat interval
		// fires multiple times, with usage advancing across turns so the
		// heartbeat counters move (#580).
		fmt.Println(`{"type":"turn.started"}`)
		os.Stdout.Sync()
		time.Sleep(60 * time.Millisecond)
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":5,"reasoning_output_tokens":0}}`)
		os.Stdout.Sync()
		time.Sleep(60 * time.Millisecond)
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":30,"cached_input_tokens":0,"output_tokens":10,"reasoning_output_tokens":0}}`)
	default:
		fmt.Fprintln(os.Stderr, "unknown HELPER_MODE")
		os.Exit(2)
	}
}

// helperCommand returns a Cmd-builder that re-execs the test binary as
// the `codex` stand-in, passing through HELPER_MODE.
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

// frozenNow returns a Now() that ticks deterministically so tests can
// assert event ordering without fighting wall-clock jitter.
func frozenNow() func() time.Time {
	t := time.Date(2026, 6, 8, 9, 30, 0, 0, time.UTC)
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
		Stage:  "implement",
		Prompt: "do the thing",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.OK {
		t.Errorf("OK = false; FailureReason = %q", res.FailureReason)
	}
	// input 42, output 50+8 reasoning = 58 → 100.
	if res.TokensUsed != 100 {
		t.Errorf("TokensUsed = %d, want 100", res.TokensUsed)
	}
	if res.InputTokens != 42 || res.OutputTokens != 58 {
		t.Errorf("split = (%d,%d), want (42,58)", res.InputTokens, res.OutputTokens)
	}
	// invocation_start, thread.started, turn.started, item.completed,
	// turn.completed, invocation_end → 6.
	if got, want := len(res.Events), 6; got != want {
		t.Fatalf("Events = %d, want %d:\n%+v", got, want, res.Events)
	}
	if res.Events[0].Kind != "invocation_start" {
		t.Errorf("Events[0].Kind = %q, want invocation_start", res.Events[0].Kind)
	}
	if res.Events[1].Kind != "thread.started" {
		t.Errorf("Events[1].Kind = %q, want thread.started", res.Events[1].Kind)
	}
	if res.Events[len(res.Events)-1].Kind != "invocation_end" {
		t.Errorf("last event kind = %q, want invocation_end", res.Events[len(res.Events)-1].Kind)
	}
}

// TestInvoke_NoUsage covers the known_usage=false path: a Codex run that
// never surfaces usage leaves the token split and model zero/empty so the
// backend records the bundle as known_usage=false (#682) rather than a
// fabricated value.
func TestInvoke_NoUsage(t *testing.T) {
	inv := &Invoker{
		Cmd: helperCommand("no_usage"),
		Now: frozenNow(),
	}
	res, err := inv.Invoke(context.Background(), agent.Invocation{Stage: "implement"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false; FailureReason = %q", res.FailureReason)
	}
	if res.TokensUsed != 0 || res.InputTokens != 0 || res.OutputTokens != 0 {
		t.Errorf("tokens = (%d,%d,%d), want all 0", res.TokensUsed, res.InputTokens, res.OutputTokens)
	}
	if res.Model != "" {
		t.Errorf("Model = %q, want empty", res.Model)
	}
}

// TestInvoke_MultiTurnSumsUsage pins the per-turn accumulation: two
// turn.completed lines must SUM (220), not last-win (110).
func TestInvoke_MultiTurnSumsUsage(t *testing.T) {
	inv := &Invoker{
		Cmd: helperCommand("multi_turn"),
		Now: frozenNow(),
	}
	res, err := inv.Invoke(context.Background(), agent.Invocation{})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.TokensUsed != 220 {
		t.Errorf("TokensUsed = %d, want 220 (per-turn sum)", res.TokensUsed)
	}
	if res.InputTokens != 200 || res.OutputTokens != 20 {
		t.Errorf("split = (%d,%d), want (200,20)", res.InputTokens, res.OutputTokens)
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
			if !strings.Contains(string(ev.Payload), "codex_core_skills") {
				t.Errorf("raw event payload missing text: %s", ev.Payload)
			}
		}
	}
	if !sawRaw {
		t.Error("no kind=raw event captured")
	}
	// The trailing usage line after the raw line must still be counted.
	if res.TokensUsed != 2 {
		t.Errorf("TokensUsed = %d, want 2 (usage after raw line still parsed)", res.TokensUsed)
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

func TestInvoke_ModelSurfaced(t *testing.T) {
	inv := &Invoker{
		Cmd: helperCommand("model"),
		Now: frozenNow(),
	}
	res, err := inv.Invoke(context.Background(), agent.Invocation{})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Model != "gpt-5-codex" {
		t.Errorf("Model = %q, want gpt-5-codex", res.Model)
	}
}

// TestInvoke_ForwardsEnv asserts the runner-forwarded env reaches the
// child: OPENAI_API_KEY (from the APIKey field) plus the FISHHAWK_* MCP
// vars (from Invocation.Env) — the cross-boundary env-composition the
// agent's MCP awareness depends on.
func TestInvoke_ForwardsEnv(t *testing.T) {
	inv := &Invoker{
		APIKey: "sk-test-codex-key",
		Cmd:    helperCommand("echo_env"),
		Now:    frozenNow(),
	}
	res, err := inv.Invoke(context.Background(), agent.Invocation{
		Env: map[string]string{
			"FISHHAWK_API_TOKEN":   "fhm_token123",
			"FISHHAWK_BACKEND_URL": "https://api.fishhawk.test",
		},
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
	if got["OPENAI_API_KEY"] != "sk-test-codex-key" {
		t.Errorf("OPENAI_API_KEY = %q, want sk-test-codex-key", got["OPENAI_API_KEY"])
	}
	if got["FISHHAWK_API_TOKEN"] != "fhm_token123" {
		t.Errorf("FISHHAWK_API_TOKEN = %q, want fhm_token123", got["FISHHAWK_API_TOKEN"])
	}
	if got["FISHHAWK_BACKEND_URL"] != "https://api.fishhawk.test" {
		t.Errorf("FISHHAWK_BACKEND_URL = %q, want https://api.fishhawk.test", got["FISHHAWK_BACKEND_URL"])
	}
}

func TestInvoke_StartsWithInvocationStart(t *testing.T) {
	inv := &Invoker{Cmd: helperCommand("happy"), Now: frozenNow()}
	res, _ := inv.Invoke(context.Background(), agent.Invocation{
		RunID: "rid", Stage: "implement",
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
	if !strings.Contains(string(first.Payload), `"agent":"codex"`) {
		t.Errorf("invocation_start payload missing agent=codex: %s", first.Payload)
	}
}

func TestNew_DefaultsBinary(t *testing.T) {
	inv := New("k")
	if inv.Binary != "" {
		t.Errorf("Binary = %q, want empty (resolved at Invoke time)", inv.Binary)
	}
	if inv.APIKey != "k" {
		t.Errorf("APIKey = %q, want k", inv.APIKey)
	}
}

func TestParseLine_UsageAndModel(t *testing.T) {
	ts := time.Now()

	// turn.completed with usage: cached is a subset of input (not added),
	// reasoning is added to output.
	ev, info := parseLine([]byte(`{"type":"turn.completed","usage":{"input_tokens":120,"cached_input_tokens":40,"output_tokens":30,"reasoning_output_tokens":15}}`), ts)
	if ev.Kind != "turn.completed" {
		t.Errorf("kind = %q, want turn.completed", ev.Kind)
	}
	if !info.HasUsage {
		t.Fatal("HasUsage = false")
	}
	if info.InputTokens != 120 {
		t.Errorf("InputTokens = %d, want 120 (cached not added)", info.InputTokens)
	}
	if info.OutputTokens != 45 {
		t.Errorf("OutputTokens = %d, want 45 (30 + 15 reasoning)", info.OutputTokens)
	}

	// Non-JSON line → raw, no usage.
	ev, info = parseLine([]byte(`not json at all`), ts)
	if ev.Kind != "raw" {
		t.Errorf("kind = %q, want raw", ev.Kind)
	}
	if info.HasUsage {
		t.Error("HasUsage = true on non-JSON line")
	}

	// Empty line → raw, no usage.
	ev, _ = parseLine([]byte(`   `), ts)
	if ev.Kind != "raw" {
		t.Errorf("empty line kind = %q, want raw", ev.Kind)
	}
}

// heartbeat mirrors the claudecode ProgressSink schema so the assertions
// below validate the identical liveness contract (#580).
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

func TestInvoke_ProgressHeartbeats(t *testing.T) {
	var sink bytes.Buffer
	inv := &Invoker{
		Cmd:               helperCommand("spaced"),
		HeartbeatInterval: 20 * time.Millisecond,
		// Real Now (default): frozenNow mutates shared state unsynchronized
		// and would race the heartbeat goroutine under -race.
	}
	res, err := inv.Invoke(context.Background(), agent.Invocation{
		ProgressSink: &sink,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false; FailureReason = %q", res.FailureReason)
	}
	hbs := parseHeartbeats(t, sink.Bytes())
	if len(hbs) == 0 {
		t.Fatal("no stage_progress heartbeats written to ProgressSink")
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

// TestInvoke_NilProgressSinkNoHeartbeats confirms a nil ProgressSink
// emits zero heartbeats and returns promptly (no goroutine leak).
func TestInvoke_NilProgressSinkNoHeartbeats(t *testing.T) {
	inv := &Invoker{
		Cmd:               helperCommand("happy"),
		Now:               frozenNow(),
		HeartbeatInterval: time.Millisecond, // would fire constantly if a sink existed
	}
	res, err := inv.Invoke(context.Background(), agent.Invocation{})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false; FailureReason = %q", res.FailureReason)
	}
	for _, ev := range res.Events {
		if ev.Kind == "stage_progress" {
			t.Error("stage_progress event present with nil ProgressSink")
		}
	}
}
