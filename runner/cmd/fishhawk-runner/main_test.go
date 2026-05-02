package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// fakeInvoker lets tests drive run() without spawning a child
// process. Returning (canned, returnErr) keeps the seam tiny.
type fakeInvoker struct {
	canned    agent.Result
	returnErr error
	gotAPIKey string
}

func (f *fakeInvoker) Invoke(ctx context.Context, inv agent.Invocation) (agent.Result, error) {
	return f.canned, f.returnErr
}

// withFakeInvoker swaps the package's newInvoker for one that
// records the API key and returns canned results. Cleanup restores
// the original constructor.
func withFakeInvoker(t *testing.T, fake *fakeInvoker) {
	t.Helper()
	orig := newInvoker
	newInvoker = func(apiKey string) agent.Invoker {
		fake.gotAPIKey = apiKey
		return fake
	}
	t.Cleanup(func() { newInvoker = orig })
}

// TestRun_HappyPath exercises the no-op success path: every
// required flag set, run() returns 0 and writes a startup log line.
func TestRun_HappyPath(t *testing.T) {
	var out strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change",
		"--stage", "plan",
	}, &out)
	if got != exitOK {
		t.Errorf("run = %d, want %d", got, exitOK)
	}
	for _, want := range []string{
		`"event":"runner_started"`,
		`"run_id":"11111111-2222-3333-4444-555555555555"`,
		`"workflow":"feature_change"`,
		`"stage":"plan"`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("startup log missing %s:\n%s", want, out.String())
		}
	}
}

func TestRun_MissingRunID(t *testing.T) {
	var out strings.Builder
	got := run([]string{
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change",
		"--stage", "plan",
	}, &out)
	if got != exitUsage {
		t.Errorf("run = %d, want %d", got, exitUsage)
	}
}

func TestRun_BadFlag(t *testing.T) {
	got := run([]string{"--no-such-flag"}, io.Discard)
	if got != exitUsage {
		t.Errorf("run = %d, want %d", got, exitUsage)
	}
}

func TestRun_HelpExitsUsage(t *testing.T) {
	// flag.ContinueOnError + --help surfaces ErrHelp. We treat that
	// as a usage exit, same as a malformed flag.
	got := run([]string{"--help"}, io.Discard)
	if got != exitUsage {
		t.Errorf("run = %d, want %d", got, exitUsage)
	}
}

func TestRunnerVersion_NonEmpty(t *testing.T) {
	if runnerVersion() == "" {
		t.Fatal("runnerVersion() should never be empty")
	}
}

func TestRun_PromptFileMissing(t *testing.T) {
	var out strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change",
		"--stage", "plan",
		"--prompt-file", "/no/such/path/anywhere.txt",
	}, &out)
	if got != exitUsage {
		t.Errorf("run = %d, want %d", got, exitUsage)
	}
	if !strings.Contains(out.String(), `"event":"runner_failed"`) {
		t.Errorf("missing runner_failed log line: %s", out.String())
	}
	if !strings.Contains(out.String(), `"reason":"read_prompt"`) {
		t.Errorf("missing read_prompt reason: %s", out.String())
	}
}

func TestClassifyErr(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{agent.ErrTimeout, "timeout"},
		{fmt.Errorf("wrapped: %w", agent.ErrTimeout), "timeout"},
		{agent.ErrBudgetExceeded, "budget_exceeded"},
		{agent.ErrBinaryNotFound, "binary_not_found"},
		{agent.ErrAgentFailed, "agent_failed"},
		{fmt.Errorf("wrapped: %w", agent.ErrAgentFailed), "agent_failed"},
		{errors.New("anything else"), "other"},
	}
	for _, tc := range cases {
		var name string
		if tc.err == nil {
			name = "nil"
		} else {
			name = tc.err.Error()
		}
		t.Run(name, func(t *testing.T) {
			if got := classifyErr(tc.err); got != tc.want {
				t.Errorf("classifyErr(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestLogCompletion_OK(t *testing.T) {
	var w strings.Builder
	logCompletion(&w, agent.Result{OK: true, TokensUsed: 250}, nil)
	out := w.String()
	if !strings.Contains(out, `"outcome":"ok"`) {
		t.Errorf("missing outcome ok: %s", out)
	}
	if !strings.Contains(out, `"tokens_used":250`) {
		t.Errorf("missing tokens_used: %s", out)
	}
}

func TestLogCompletion_Failure(t *testing.T) {
	var w strings.Builder
	logCompletion(&w, agent.Result{
		OK:              false,
		FailureCategory: "A",
		FailureReason:   "agent timeout after 100ms",
		TokensUsed:      0,
	}, agent.ErrTimeout)
	out := w.String()
	for _, want := range []string{
		`"outcome":"failed"`,
		`"category":"A"`,
		`"reason":"agent timeout after 100ms"`,
		`"err_class":"timeout"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in: %s", want, out)
		}
	}
}

func TestRun_PromptInvokesAgentAndEmitsEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(path, []byte("do the thing"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ANTHROPIC_API_KEY", "sk-test-1234")

	fake := &fakeInvoker{
		canned: agent.Result{
			OK:         true,
			TokensUsed: 250,
			Events: []agent.Event{
				{Kind: "invocation_start", Payload: agent.MakePayload(map[string]string{"a": "b"})},
				{Kind: "result", Payload: agent.MakePayload(map[string]int{"n": 1})},
				{Kind: "invocation_end"},
			},
		},
	}
	withFakeInvoker(t, fake)

	// Capture stdout — emitEvents writes there.
	stdoutR, stdoutW, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = stdoutW
	t.Cleanup(func() { os.Stdout = origStdout })

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change",
		"--stage", "plan",
		"--prompt-file", path,
		"--max-tokens", "1000",
		"--timeout", "30s",
	}, &stderr)

	_ = stdoutW.Close()
	stdoutBytes, _ := io.ReadAll(stdoutR)

	if got != exitOK {
		t.Errorf("run = %d, want %d", got, exitOK)
	}
	if fake.gotAPIKey != "sk-test-1234" {
		t.Errorf("invoker gotAPIKey = %q, want sk-test-1234", fake.gotAPIKey)
	}

	// Three events should have been emitted as JSON Lines on stdout.
	lines := bytes.Split(bytes.TrimRight(stdoutBytes, "\n"), []byte("\n"))
	if len(lines) != 3 {
		t.Fatalf("emitted %d JSONL lines, want 3:\n%s", len(lines), stdoutBytes)
	}
	for i, line := range lines {
		var ev agent.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Errorf("line %d not JSON: %v: %s", i, err, line)
		}
	}

	// The completion log line should report ok and the token count.
	if !strings.Contains(stderr.String(), `"outcome":"ok"`) {
		t.Errorf("missing ok outcome in stderr: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"tokens_used":250`) {
		t.Errorf("missing tokens_used in stderr: %s", stderr.String())
	}
}

func TestRun_AgentFailureMapsToExit1(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(path, []byte("p"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "")

	fake := &fakeInvoker{
		canned: agent.Result{
			OK:              false,
			FailureCategory: "A",
			FailureReason:   "agent timeout after 30s",
			Events: []agent.Event{
				{Kind: "invocation_start"},
				{Kind: "invocation_end"},
			},
		},
		returnErr: agent.ErrTimeout,
	}
	withFakeInvoker(t, fake)

	// Discard stdout to keep test output clean.
	stdoutR, stdoutW, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = stdoutW
	t.Cleanup(func() { os.Stdout = origStdout })

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change",
		"--stage", "plan",
		"--prompt-file", path,
	}, &stderr)

	_ = stdoutW.Close()
	_, _ = io.ReadAll(stdoutR)

	if got != exitFailure {
		t.Errorf("run = %d, want %d", got, exitFailure)
	}
	out := stderr.String()
	if !strings.Contains(out, `"outcome":"failed"`) {
		t.Errorf("missing failed outcome: %s", out)
	}
	if !strings.Contains(out, `"category":"A"`) {
		t.Errorf("missing category A: %s", out)
	}
	if !strings.Contains(out, `"err_class":"timeout"`) {
		t.Errorf("missing err_class timeout: %s", out)
	}
}

func TestEmitEvents_OneJSONPerLine(t *testing.T) {
	var w bytes.Buffer
	emitEvents(&w, []agent.Event{
		{Kind: "a"},
		{Kind: "b", Payload: agent.MakePayload(map[string]int{"n": 1})},
	})
	lines := bytes.Split(bytes.TrimRight(w.Bytes(), "\n"), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), w.String())
	}
	for _, line := range lines {
		var ev agent.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Errorf("line not JSON: %v: %s", err, line)
		}
	}
}

func TestNewInvoker_DefaultIsClaudeCode(t *testing.T) {
	// Sanity check: production wiring constructs a non-nil invoker.
	// Regression guard for someone removing the default assignment.
	inv := newInvoker("k")
	if inv == nil {
		t.Fatal("newInvoker returned nil")
	}
}

func TestLogCompletion_FailureFallsBackToErrText(t *testing.T) {
	// FailureReason empty → reason should fall back to err.Error().
	var w strings.Builder
	logCompletion(&w, agent.Result{OK: false}, errors.New("boom"))
	out := w.String()
	if !strings.Contains(out, `"reason":"boom"`) {
		t.Errorf("missing reason fallback: %s", out)
	}
	// FailureCategory empty → should default to "A".
	if !strings.Contains(out, `"category":"A"`) {
		t.Errorf("missing default category A: %s", out)
	}
}
