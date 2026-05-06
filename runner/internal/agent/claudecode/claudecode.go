// Package claudecode adapts Anthropic's Claude Code CLI to the
// runner's agent.Invoker interface.
//
// In v0 the customer supplies the API key via GitHub Secrets
// (MVP_SPEC §5.3); the runner forwards it as the
// ANTHROPIC_API_KEY env var on the child. Centralized issuance
// (Fishhawk-managed ephemeral keys) is a v0.x story, not v0.
//
// The adapter spawns `claude --print --verbose --output-format
// stream-json --dangerously-skip-permissions --add-dir /tmp -p
// <prompt>` and reads one JSON event per line from stdout. Each
// line becomes an agent.Event; if the line carries a `usage` block
// we update the running token total and enforce the budget. A
// non-zero exit, a context cancellation, or a budget breach all map
// to MVP_SPEC §6 category-A failures — never silent successes.
package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// DefaultBinary is the executable name resolved against PATH when
// Invoker.Binary is empty.
const DefaultBinary = "claude"

// Invoker is the agent.Invoker implementation for Claude Code.
type Invoker struct {
	// Binary is the executable name or absolute path. Empty means
	// DefaultBinary.
	Binary string

	// APIKey is forwarded as ANTHROPIC_API_KEY to the child. Empty
	// means the runner did not receive a key and the child is
	// expected to fail; that's reported as a category-A failure
	// like any other agent error rather than crashing the runner.
	APIKey string

	// Cmd builds the *exec.Cmd. Defaults to exec.CommandContext;
	// overridable by tests to redirect to a fake binary.
	Cmd func(ctx context.Context, name string, args ...string) *exec.Cmd

	// Now returns the current time. Defaults to time.Now;
	// overridable for deterministic event timestamps in tests.
	Now func() time.Time
}

// New returns an Invoker configured to use the system `claude`
// binary with the given API key.
func New(apiKey string) *Invoker {
	return &Invoker{APIKey: apiKey}
}

// Invoke runs Claude Code under the given Invocation and returns
// the captured trace. The returned error is non-nil only on agent
// failure — Result.OK is the canonical success signal so callers
// can treat the Result as the source of truth even on error.
func (i *Invoker) Invoke(ctx context.Context, inv agent.Invocation) (agent.Result, error) {
	binary := i.Binary
	if binary == "" {
		binary = DefaultBinary
	}
	cmdFn := i.Cmd
	if cmdFn == nil {
		cmdFn = exec.CommandContext
	}
	now := i.now

	if inv.Budget.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, inv.Budget.Timeout)
		defer cancel()
	}

	res := agent.Result{
		Events: []agent.Event{
			{
				Kind:      "invocation_start",
				Timestamp: now(),
				Payload: agent.MakePayload(map[string]string{
					"agent":  "claude-code",
					"run_id": inv.RunID,
					"stage":  inv.Stage,
				}),
			},
		},
	}

	// Claude Code requires --verbose when --print is combined with
	// --output-format=stream-json (validated by `claude` itself with
	// "Error: When using --print, --output-format=stream-json requires
	// --verbose"). --verbose forces emission of intermediate events
	// alongside the final result, which is exactly what the trace
	// bundle wants anyway.
	//
	// --dangerously-skip-permissions: --print is a non-interactive
	// invocation, so Claude's "may I read / write / run X?" prompts
	// have no human to answer them and every tool call returns
	// "permissions not granted". The whole point of running under
	// the Fishhawk runner is that the audit log captures every tool
	// call after-the-fact; an interactive permission gate is not an
	// additional safety boundary in that model. The trace bundle is
	// the authoritative record.
	//
	// --add-dir /tmp: Claude restricts writes to the working
	// directory tree by default. The runner needs the agent to write
	// its plan artifact to /tmp/fishhawk-plan.json (matched by
	// backend/internal/prompt.PlanArtifactPath); /tmp is outside the
	// customer's repo checkout so we explicitly expand the allowlist.
	args := []string{
		"--print", "--verbose",
		"--output-format", "stream-json",
		"--dangerously-skip-permissions",
		"--add-dir", "/tmp",
		"-p", inv.Prompt,
	}
	cmd := cmdFn(ctx, binary, args...)
	cmd.Dir = inv.WorkingDir
	// Compose env so a Cmd builder (e.g. tests) can pre-set
	// vars on cmd.Env and we layer the API key on top. nil means
	// "child will inherit our env", so seed with os.Environ() in
	// that case to keep PATH, HOME, etc. for the agent process.
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	if i.APIKey != "" {
		cmd.Env = append(cmd.Env, "ANTHROPIC_API_KEY="+i.APIKey)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return res, fmt.Errorf("claudecode: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return res, fmt.Errorf("claudecode: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		// Distinguish "binary missing" from other start errors so
		// callers can surface a precise error to the operator.
		if isBinaryMissing(err) {
			return failureResult(res, now(), "A",
				fmt.Sprintf("agent binary not found: %s", binary),
				"binary_not_found",
			), agent.ErrBinaryNotFound
		}
		return res, fmt.Errorf("claudecode: start: %w", err)
	}

	// Drain stderr concurrently to avoid deadlock if the child
	// writes more than the pipe buffer can hold.
	var stderrBuf bytes.Buffer
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		_, _ = io.Copy(&stderrBuf, stderr)
	}()

	tokensUsed := 0
	budgetHit := false

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		ev, used, ok := parseLine(line, now())
		res.Events = append(res.Events, ev)
		if ok && used > 0 {
			tokensUsed = used
			if inv.Budget.MaxTokens > 0 && tokensUsed > inv.Budget.MaxTokens {
				budgetHit = true
				_ = cmd.Process.Kill()
				break
			}
		}
	}
	scanErr := scanner.Err()

	// Drain remaining stdout if we killed mid-stream.
	_, _ = io.Copy(io.Discard, stdout)
	<-stderrDone

	if stderrBuf.Len() > 0 {
		res.Events = append(res.Events, agent.Event{
			Kind:      "stderr",
			Timestamp: now(),
			Payload: agent.MakePayload(map[string]string{
				"text": stderrBuf.String(),
			}),
		})
	}

	waitErr := cmd.Wait()
	res.TokensUsed = tokensUsed

	switch {
	case budgetHit:
		return failureResult(res, now(), "A",
			fmt.Sprintf("token budget exceeded: used %d, max %d", tokensUsed, inv.Budget.MaxTokens),
			"budget_exceeded",
		), agent.ErrBudgetExceeded

	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return failureResult(res, now(), "A",
			fmt.Sprintf("agent timeout after %s", inv.Budget.Timeout),
			"timeout",
		), agent.ErrTimeout

	case waitErr != nil:
		return failureResult(res, now(), "A",
			fmt.Sprintf("agent exited with error: %v", waitErr),
			"agent_error",
		), fmt.Errorf("%w: %v", agent.ErrAgentFailed, waitErr)

	case scanErr != nil:
		return failureResult(res, now(), "A",
			fmt.Sprintf("trace stream read error: %v", scanErr),
			"stream_error",
		), fmt.Errorf("%w: %v", agent.ErrAgentFailed, scanErr)
	}

	res.OK = true
	res.Events = append(res.Events, agent.Event{
		Kind:      "invocation_end",
		Timestamp: now(),
		Payload:   agent.MakePayload(map[string]any{"outcome": "ok", "tokens_used": tokensUsed}),
	})
	return res, nil
}

func (i *Invoker) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now().UTC()
}

// failureResult appends an invocation_end with the failure metadata
// and stamps the top-level failure fields. Centralized so every
// failure path produces the same shape.
func failureResult(res agent.Result, ts time.Time, category, reason, outcome string) agent.Result {
	res.OK = false
	res.FailureCategory = category
	res.FailureReason = reason
	res.Events = append(res.Events, agent.Event{
		Kind:      "invocation_end",
		Timestamp: ts,
		Payload: agent.MakePayload(map[string]string{
			"outcome": outcome,
			"reason":  reason,
		}),
	})
	return res
}

// parseLine turns one JSON line from Claude Code's stream-json
// output into an agent.Event. The kind is taken from the line's
// `type` field when present; unknown / non-JSON lines become
// kind=raw so we never silently drop trace bytes.
//
// Returns (event, tokensUsed, hasUsage). hasUsage is true when the
// line carried a `usage.input_tokens` + `usage.output_tokens`
// block we could parse; tokensUsed is the SUM of those two.
func parseLine(line []byte, ts time.Time) (agent.Event, int, bool) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return agent.Event{Kind: "raw", Timestamp: ts}, 0, false
	}

	// Probe the kind without unmarshaling the whole payload.
	var meta struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Usage   *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(trimmed, &meta); err != nil {
		// Non-JSON line — capture verbatim so the trace is still
		// honest about what the child wrote.
		return agent.Event{
			Kind:      "raw",
			Timestamp: ts,
			Payload:   agent.MakePayload(map[string]string{"text": string(trimmed)}),
		}, 0, false
	}

	kind := meta.Type
	if kind == "" {
		kind = "raw"
	} else if meta.Subtype != "" {
		kind = kind + "." + meta.Subtype
	}

	used := 0
	hasUsage := false
	if meta.Usage != nil {
		used = meta.Usage.InputTokens + meta.Usage.OutputTokens
		hasUsage = used > 0
	}

	return agent.Event{
		Kind:      kind,
		Timestamp: ts,
		Payload:   json.RawMessage(trimmed),
	}, used, hasUsage
}

// isBinaryMissing reports whether err means the binary itself is
// not on disk / not on PATH, as opposed to a runtime failure.
// exec.ErrNotFound is the canonical case but the underlying syscall
// error message varies by platform; matching the substring is a
// pragmatic fallback.
func isBinaryMissing(err error) bool {
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	return strings.Contains(err.Error(), "executable file not found") ||
		strings.Contains(err.Error(), "no such file or directory")
}
