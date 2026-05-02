package claudecode

import (
	"context"
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
}
