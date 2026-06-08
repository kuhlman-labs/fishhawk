// Package codex adapts OpenAI's Codex CLI to the runner's
// agent.Invoker interface, mirroring the Claude Code adapter in
// runner/internal/agent/claudecode.
//
// In v0 the customer supplies the API key via GitHub Secrets
// (MVP_SPEC §5.3); the runner forwards it as the OPENAI_API_KEY env
// var on the child. (When Codex is authenticated via a ChatGPT login
// instead of an API key the forwarded var is simply unused — an empty
// APIKey is not an error here, the same posture as claudecode.)
//
// The adapter spawns `codex exec --json
// --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check
// <prompt>` and reads one JSON event per line from stdout. The flag
// shape and the JSONL event schema below were pinned against the
// installed Codex CLI (codex-cli 0.137.0) — see invoke() for the
// rationale on each flag and parseLine for the event schema. Each
// stdout line becomes an agent.Event; a `turn.completed` line carries
// the per-turn usage block which we sum into the running token total
// and enforce the budget against. A non-zero exit, a context
// cancellation, or a budget breach all map to MVP_SPEC §6 category-A
// failures — never silent successes.
//
// Not ported from claudecode (out of this issue's scope, see #840):
// the interleaved-thinking 400 retry (Anthropic-specific), loop
// detection (#653, stream-json-specific), and out-of-tree-write
// surfacing (#601, stream-json-specific). Codex surfaces no `model`
// field in its JSONL today, so Result.Model is left empty and the
// backend records the bundle's cost from the token split alone.
package codex

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
	"sync"
	"syscall"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// DefaultBinary is the executable name resolved against PATH when
// Invoker.Binary is empty.
const DefaultBinary = "codex"

// defaultHeartbeatInterval is the cadence of stage_progress liveness
// heartbeats written to Invocation.ProgressSink during an invocation
// (#580). Used when Invoker.HeartbeatInterval is zero. Matches the
// claudecode default so the two adapters present an identical liveness
// cadence to the runner.
const defaultHeartbeatInterval = 15 * time.Second

// Invoker is the agent.Invoker implementation for the Codex CLI.
type Invoker struct {
	// Binary is the executable name or absolute path. Empty means
	// DefaultBinary.
	Binary string

	// APIKey is forwarded as OPENAI_API_KEY to the child. Empty means
	// the runner did not receive a key (e.g. Codex is authenticated via
	// a ChatGPT login on the host instead); that's not an error here —
	// any resulting auth failure surfaces as a category-A agent failure
	// like any other non-zero exit, rather than crashing the runner.
	APIKey string

	// Cmd builds the *exec.Cmd. Defaults to exec.CommandContext;
	// overridable by tests to redirect to a fake binary.
	Cmd func(ctx context.Context, name string, args ...string) *exec.Cmd

	// Now returns the current time. Defaults to time.Now;
	// overridable for deterministic event timestamps in tests.
	Now func() time.Time

	// HeartbeatInterval is the cadence of stage_progress liveness
	// heartbeats written to Invocation.ProgressSink during an
	// invocation (#580). Zero means defaultHeartbeatInterval (15s).
	HeartbeatInterval time.Duration
}

// New returns an Invoker configured to use the system `codex` binary
// with the given API key.
func New(apiKey string) *Invoker {
	return &Invoker{APIKey: apiKey}
}

// Invoke runs the Codex CLI under the given Invocation and returns the
// captured trace. The returned error is non-nil only on agent
// failure — Result.OK is the canonical success signal so callers can
// treat the Result as the source of truth even on error.
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
					"agent":  "codex",
					"run_id": inv.RunID,
					"stage":  inv.Stage,
				}),
			},
		},
	}

	// Flag rationale (pinned against codex-cli 0.137.0 `codex exec --help`):
	//
	//   exec        — the non-interactive subcommand; reads the prompt from
	//                 the positional argument and streams the run to stdout.
	//   --json      — emit one JSON event per line to stdout (the trace
	//                 stream this adapter parses). Without it Codex prints
	//                 human-formatted text we couldn't turn into events.
	//   --dangerously-bypass-approvals-and-sandbox
	//               — a non-interactive run has no human to answer Codex's
	//                 "may I run X?" approval prompts, and the runner already
	//                 relies on the signed trace bundle as the authoritative
	//                 after-the-fact record (the same model claudecode uses
	//                 with --dangerously-skip-permissions). This skips the
	//                 approval prompts AND the OS sandbox so the implement
	//                 loop can run build/test/lint and write its plan artifact
	//                 to /tmp. True confinement is an OS-sandbox concern,
	//                 deferred to the agent-filesystem-confinement ADR — same
	//                 residual gap claudecode documents.
	//   --skip-git-repo-check
	//               — Codex refuses to run outside a git repo by default; the
	//                 customer checkout is a repo so this is a no-op there, but
	//                 it keeps the adapter robust when WorkingDir is not a repo.
	//
	// The prompt is the trailing positional argument. cmd.Stdin is left nil,
	// which os/exec wires to /dev/null: Codex notes "Reading additional input
	// from stdin..." and gets immediate EOF, so the prompt arg is the sole
	// instruction and the child never blocks waiting on a terminal stdin.
	args := []string{
		"exec", "--json",
		"--dangerously-bypass-approvals-and-sandbox",
		"--skip-git-repo-check",
		inv.Prompt,
	}
	cmd := cmdFn(ctx, binary, args...)
	cmd.Dir = inv.WorkingDir

	// Put the child in its own process group so a budget/timeout kill can
	// reap the WHOLE tree, not just the direct child. Codex spawns
	// grandchildren (shell command executions, MCP servers) that inherit
	// the stdout pipe; killing only the direct child can leave a
	// grandchild holding the pipe open, hanging the io.Discard drain and
	// cmd.Wait below. Setpgid makes cmd.Process the group leader (pgid ==
	// its pid), so killProcessGroup(-pid) signals every descendant.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Override exec.CommandContext's default ctx-cancel behavior (which
	// kills only the direct child) with a process-GROUP kill, so the
	// timeout path reaps grandchildren too. Runs when ctx is Done (the
	// Budget.Timeout deadline above, or a parent cancel).
	cmd.Cancel = func() error {
		return killProcessGroup(cmd.Process)
	}

	// Compose env so a Cmd builder (e.g. tests) can pre-set vars on
	// cmd.Env and we layer the API key on top. nil means "child will
	// inherit our env", so seed with os.Environ() in that case to keep
	// PATH, HOME, etc. for the agent process. Identical ordering to the
	// claudecode adapter so the Fishhawk MCP server stays reachable.
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	if i.APIKey != "" {
		cmd.Env = append(cmd.Env, "OPENAI_API_KEY="+i.APIKey)
	}
	// Layer Invocation.Env on top so per-run secrets (FISHHAWK_API_TOKEN,
	// FISHHAWK_BACKEND_URL, etc. set by the runner per E19.8 / #348) reach
	// the agent process. The agent's MCP server reads these to authenticate
	// against the Fishhawk backend; missing them is fine — MCP awareness is
	// best-effort per ADR-021.
	for k, v := range inv.Env {
		if k == "" {
			continue
		}
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return res, fmt.Errorf("codex: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return res, fmt.Errorf("codex: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		// Distinguish "binary missing" from other start errors so callers
		// can surface a precise error to the operator.
		if isBinaryMissing(err) {
			return failureResult(res, now(), "A",
				fmt.Sprintf("agent binary not found: %s", binary),
				"binary_not_found",
			), agent.ErrBinaryNotFound
		}
		return res, fmt.Errorf("codex: start: %w", err)
	}

	// Drain stderr concurrently to avoid deadlock if the child writes
	// more than the pipe buffer can hold.
	var stderrBuf bytes.Buffer
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		_, _ = io.Copy(&stderrBuf, stderr)
	}()

	tokensUsed := 0
	inputTokens := 0
	outputTokens := 0
	model := ""
	budgetHit := false

	// Progress heartbeat state (#580). The scan loop writes turns /
	// tokensUsed / lastKind; the heartbeat goroutine reads them on each
	// tick. Both accesses go through progMu so the race detector stays
	// quiet. See the claudecode adapter for the full proof that this
	// goroutine is the SOLE writer to inv.ProgressSink during Invoke.
	var (
		progMu   sync.Mutex
		turns    int
		lastKind string
	)
	start := now()

	var (
		hbDone    chan struct{}
		hbStopped chan struct{}
	)
	if inv.ProgressSink != nil {
		interval := i.HeartbeatInterval
		if interval <= 0 {
			interval = defaultHeartbeatInterval
		}
		hbDone = make(chan struct{})
		hbStopped = make(chan struct{})
		go func() {
			defer close(hbStopped)
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-hbDone:
					return
				case <-ticker.C:
					// Time-driven, not event-driven: a stalled stage still
					// emits heartbeats with non-advancing counters, which is
					// how the driver tells "alive and progressing" from "stuck".
					progMu.Lock()
					t, tok, lk := turns, tokensUsed, lastKind
					progMu.Unlock()
					_, _ = fmt.Fprintf(inv.ProgressSink,
						`{"event":"stage_progress","elapsed_seconds":%d,"turns":%d,"tokens_so_far":%d,"last_event_kind":%q}`+"\n",
						int(now().Sub(start).Seconds()), t, tok, lk)
				}
			}
		}()
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		ev, info := parseLine(line, now())
		if info.Model != "" {
			model = info.Model
		}
		res.Events = append(res.Events, ev)

		progMu.Lock()
		turns++
		lastKind = ev.Kind
		if info.HasUsage {
			// Codex reports usage PER TURN on each `turn.completed` line
			// (not a cumulative running total like Claude Code), so we SUM
			// across turns rather than last-wins. A single `codex exec`
			// invocation is one turn today, so for the common case this is
			// just that turn's figures; summing is the correct accumulation
			// if a future Codex version emits multiple turns per exec, and
			// can never silently undercount.
			tokensUsed += info.InputTokens + info.OutputTokens
			inputTokens += info.InputTokens
			outputTokens += info.OutputTokens
		}
		curTokens := tokensUsed
		progMu.Unlock()

		if info.HasUsage && inv.Budget.MaxTokens > 0 && curTokens > inv.Budget.MaxTokens {
			budgetHit = true
			_ = killProcessGroup(cmd.Process)
			break
		}
	}
	scanErr := scanner.Err()

	// Stop the heartbeat goroutine now the scan loop has finished —
	// covers both the EOF path and the budget-hit early break, so the
	// goroutine never outlives the invocation (no ticker/timer leak).
	if hbDone != nil {
		close(hbDone)
		<-hbStopped
	}

	// Drain remaining stdout if we killed mid-stream, THEN wait for the
	// stderr drain. Ordering matters: a process-group kill above frees any
	// grandchild holding the pipe, so this io.Discard read reaches EOF
	// rather than blocking forever — the reason the kill is a group kill.
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
	res.InputTokens = inputTokens
	res.OutputTokens = outputTokens
	res.Model = model

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

// killProcessGroup sends SIGKILL to the entire process group led by p.
// Setpgid at spawn makes p the group leader, so its pid is the pgid and
// the negative-pid signal reaches every descendant. A nil process (kill
// raced a not-yet-started child) is a no-op.
func killProcessGroup(p *os.Process) error {
	if p == nil {
		return nil
	}
	return syscall.Kill(-p.Pid, syscall.SIGKILL)
}

// failureResult appends an invocation_end with the failure metadata and
// stamps the top-level failure fields. Centralized so every failure path
// produces the same shape. Mirrors claudecode.failureResult.
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

// lineInfo carries the structured usage + model metadata parseLine
// extracted from one JSONL line, beyond the kind already on the event.
type lineInfo struct {
	InputTokens  int
	OutputTokens int
	Model        string
	HasUsage     bool
}

// usageBlock is the shape of Codex's `usage` object on a `turn.completed`
// line (pinned against codex-cli 0.137.0):
//
//	{"type":"turn.completed","usage":{"input_tokens":N,"cached_input_tokens":N,
//	 "output_tokens":N,"reasoning_output_tokens":N}}
//
// cached_input_tokens is a (cheaper) SUBSET of input_tokens, so it is not
// added in — input_tokens already counts it. reasoning_output_tokens is a
// SEPARATE completion-side count (the hidden reasoning tokens, distinct
// from output_tokens which counts the visible message), so it IS added to
// the output side to avoid undercounting billable output.
type usageBlock struct {
	InputTokens           int `json:"input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
}

// parseLine turns one JSON line from `codex exec --json` output into an
// agent.Event. The kind is taken from the line's `type` field when
// present; unknown / non-JSON lines become kind=raw so we never silently
// drop trace bytes. Fail-open: a malformed line yields a raw event with
// no usage rather than a panic.
//
// info.HasUsage is true when the line carried a usage block whose token
// sum is > 0 (a `turn.completed` line). info.Model is the resolved model
// id when a line surfaces one; Codex's JSONL does not carry a model field
// today, so this is generally "" and Result.Model is left empty.
func parseLine(line []byte, ts time.Time) (agent.Event, lineInfo) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return agent.Event{Kind: "raw", Timestamp: ts}, lineInfo{}
	}

	var meta struct {
		Type  string      `json:"type"`
		Model string      `json:"model"`
		Usage *usageBlock `json:"usage"`
	}
	if err := json.Unmarshal(trimmed, &meta); err != nil {
		// Non-JSON line — capture verbatim so the trace is still honest
		// about what the child wrote. Codex interleaves the occasional
		// human-readable log line (e.g. a skills-loader warning) on stdout.
		return agent.Event{
			Kind:      "raw",
			Timestamp: ts,
			Payload:   agent.MakePayload(map[string]string{"text": string(trimmed)}),
		}, lineInfo{}
	}

	kind := meta.Type
	if kind == "" {
		kind = "raw"
	}

	var info lineInfo
	info.Model = meta.Model
	if meta.Usage != nil {
		info.InputTokens = meta.Usage.InputTokens
		info.OutputTokens = meta.Usage.OutputTokens + meta.Usage.ReasoningOutputTokens
		info.HasUsage = info.InputTokens+info.OutputTokens > 0
	}

	return agent.Event{
		Kind:      kind,
		Timestamp: ts,
		Payload:   json.RawMessage(trimmed),
	}, info
}

// isBinaryMissing reports whether err means the binary itself is not on
// disk / not on PATH, as opposed to a runtime failure. Mirrors
// claudecode.isBinaryMissing.
func isBinaryMissing(err error) bool {
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	return strings.Contains(err.Error(), "executable file not found") ||
		strings.Contains(err.Error(), "no such file or directory")
}
