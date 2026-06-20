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
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// DefaultBinary is the executable name resolved against PATH when
// Invoker.Binary is empty.
const DefaultBinary = "claude"

// allowedExtraDirs is the single source of truth for write roots the
// agent is permitted outside the repo working tree. It seeds BOTH the
// `--add-dir` invocation flags AND the out-of-tree write detector's
// allowlist, so the flag and the detector can never drift. /tmp is
// required for the plan artifact (/tmp/fishhawk-plan.json, matched by
// backend/internal/prompt.PlanArtifactPath). The full allowlist at
// runtime is inv.WorkingDir plus these.
var allowedExtraDirs = []string{"/tmp"}

// fileWritingTools maps Claude Code stream-json tool_use names that
// write to the filesystem to the `input` field carrying the target
// path. A tool_use for any other tool (Bash, Read, Grep, …) is not a
// direct filesystem write through the tool layer and is ignored — note
// the residual gap this leaves: Bash-mediated writes (shell `>`
// redirects) are NOT visible here, only Write/Edit-TOOL writes (the
// #601 class). Full confinement of Bash-mediated writes requires an
// OS-level sandbox; see the flag-rationale block in invokeOnce and the
// deferred agent-filesystem-confinement ADR.
var fileWritingTools = map[string]string{
	"Write":        "file_path",
	"Edit":         "file_path",
	"MultiEdit":    "file_path",
	"NotebookEdit": "notebook_path",
}

// defaultHeartbeatInterval is the cadence of stage_progress liveness
// heartbeats written to Invocation.ProgressSink during an invocation
// (#580). Used when Invoker.HeartbeatInterval is zero.
const defaultHeartbeatInterval = 15 * time.Second

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

	// MaxThinkingBlockRetries bounds the in-driver retry for the
	// transient interleaved-thinking API 400 (see
	// agent.ErrAgentThinkingBlock). It counts RETRIES, not attempts:
	// the loop runs MaxThinkingBlockRetries+1 attempts total. The
	// default (1) is set at construction in New(); a zero value means
	// "no retry" so tests and operators can disable it deterministically.
	MaxThinkingBlockRetries int

	// HeartbeatInterval is the cadence of stage_progress liveness
	// heartbeats written to Invocation.ProgressSink during an
	// invocation (#580). Zero means defaultHeartbeatInterval (15s).
	// A per-Invoker field rather than a package-level global so
	// parallel tests can shorten it without racing on shared state.
	HeartbeatInterval time.Duration

	// LoopThreshold is the number of identical CONSECUTIVE tool-call
	// signatures that trips the no-progress / duplicate-action loop
	// detector and aborts the stage with agent.ErrLoopDetected. Zero
	// means agent.DefaultLoopThreshold — a deliberately conservative
	// value so legitimate repeated calls (re-reading a file, retrying a
	// flaky command a couple of times) never false-abort real work. A
	// per-Invoker field so tests can lower it deterministically.
	LoopThreshold int
}

// New returns an Invoker configured to use the system `claude`
// binary with the given API key. The thinking-block retry budget
// defaults to 1 retry here (rather than via a zero-value sentinel) so
// that an explicit MaxThinkingBlockRetries=0 on a struct literal
// unambiguously disables retry.
func New(apiKey string) *Invoker {
	return &Invoker{APIKey: apiKey, MaxThinkingBlockRetries: 1}
}

// Invoke runs Claude Code under the given Invocation and returns the
// captured trace. The returned error is non-nil only on agent
// failure — Result.OK is the canonical success signal so callers can
// treat the Result as the source of truth even on error.
//
// Invoke wraps a bounded in-driver retry around invokeOnce for the
// transient interleaved-thinking API 400 (agent.ErrAgentThinkingBlock):
// a single transient harness fault re-spawns the agent fresh from the
// same prompt rather than wasting the whole stage attempt. Every other
// failure (timeout, budget, generic non-zero exit) is returned on the
// first attempt with no retry. The aggregate Result carries every
// attempt's events in order — with an agent_retry marker between them —
// and the cumulative token total across all attempts, so cost stays
// honest even when a retry doubles spend.
func (i *Invoker) Invoke(ctx context.Context, inv agent.Invocation) (agent.Result, error) {
	maxAttempts := i.MaxThinkingBlockRetries + 1

	var agg agent.Result
	for attempt := 1; ; attempt++ {
		res, thinkingBlock, err := i.invokeOnce(ctx, inv)

		// Aggregate this attempt's events and tokens. TokensUsed is
		// cumulative across attempts on purpose: a retry really does
		// spend the tokens twice and the trace must say so. The
		// input/output split accumulates the same way so the cost
		// rollup matches TokensUsed. Model is the latest non-empty id
		// reported across attempts.
		agg.Events = append(agg.Events, res.Events...)
		agg.TokensUsed += res.TokensUsed
		agg.InputTokens += res.InputTokens
		agg.OutputTokens += res.OutputTokens
		if res.Model != "" {
			agg.Model = res.Model
		}

		retriesLeft := attempt < maxAttempts
		overBudget := inv.Budget.MaxTokens > 0 && agg.TokensUsed >= inv.Budget.MaxTokens
		if !thinkingBlock || !retriesLeft || overBudget {
			// Adopt this attempt's outcome verbatim — on the
			// retry-exhausted thinking-block path res is already a
			// failureResult carrying outcome=agent_api_thinking_block,
			// FailureCategory=="A", and a wrapped ErrAgentThinkingBlock.
			agg.OK = res.OK
			agg.FailureCategory = res.FailureCategory
			agg.FailureReason = res.FailureReason
			return agg, err
		}

		// Transient thinking-block fault with retries remaining: mark
		// the boundary and re-spawn a fresh `claude` process from the
		// same prompt. We deliberately do NOT git-reset/clean the
		// working tree between attempts: in local --no-pr mode the tree
		// is the operator's own repo, so a reset would be destructive,
		// and a fresh `claude --print` exec carries no conversation
		// state anyway (no --continue/--resume), so the partial edits
		// the killed attempt left are a safe, intended starting point.
		// This mirrors fishhawk_retry_stage semantics. Do not "fix"
		// this into a reset.
		agg.Events = append(agg.Events, agent.Event{
			Kind:      "agent_retry",
			Timestamp: i.now(),
			Payload: agent.MakePayload(map[string]any{
				"attempt":       attempt,
				"reason":        "agent_api_thinking_block",
				"tokens_so_far": agg.TokensUsed,
			}),
		})
	}
}

// invokeOnce runs a single `claude` invocation and returns its
// per-attempt Result, whether the failure was a transient
// thinking-block 400 (the retry signal), and the wrapped error. Each
// attempt gets its own wall-clock budget derived from the parent ctx.
func (i *Invoker) invokeOnce(ctx context.Context, inv agent.Invocation) (agent.Result, bool, error) {
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
	// Why this flag is RETAINED rather than swapped for a confining
	// --permission-mode (empirical matrix, claude 2.1.156, 2026-06-01):
	//
	//   mode                          | Bash (go test, lint, …) | out-of-tree write
	//   ------------------------------|-------------------------|------------------
	//   acceptEdits / dontAsk         | DENIED ("requires       | Write/Edit tool
	//                                 |  approval") — regresses |  confined, but the
	//                                 |  the non-interactive    |  loop can't build
	//                                 |  implement loop         |  or test
	//   acceptEdits + allowedTools    | allowed                 | reopened via shell
	//     Bash  /  auto               |                         |  `>` redirect
	//   dangerously-skip-permissions  | allowed                 | unconfined (today)
	//
	// No claude-native mode gives BOTH non-interactive Bash AND full
	// out-of-tree write confinement: every mode that allows the Bash
	// the implement stage needs (go build/test, golangci-lint,
	// scripts/test) also leaves a shell-redirect escape hatch. True
	// confinement therefore requires an OS-level sandbox, deferred to
	// the agent-filesystem-confinement ADR (see ADR-024 for agent
	// execution). This PR does NOT change the flag; instead out-of-tree
	// writes through the Write/Edit TOOLS (the #601 class) are now
	// SURFACED as out_of_tree_write trace events (see the scan loop
	// below). Bash-mediated writes remain invisible to that detector —
	// that residual gap is the ADR's domain.
	//
	// --add-dir: Claude restricts writes to the working directory tree
	// by default. The runner needs the agent to write its plan artifact
	// to /tmp/fishhawk-plan.json (matched by
	// backend/internal/prompt.PlanArtifactPath); /tmp is outside the
	// customer's repo checkout so we explicitly expand the allowlist.
	// allowedExtraDirs is the single source of truth shared with the
	// out_of_tree_write detector so the flag and the detector can't drift.
	args := []string{
		"--print", "--verbose",
		"--output-format", "stream-json",
		"--dangerously-skip-permissions",
	}
	for _, dir := range allowedExtraDirs {
		args = append(args, "--add-dir", dir)
	}
	// --model: pin the agent to the backend-resolved implement model (#1013)
	// when one was resolved. An empty inv.Model (no plan recommendation, no
	// spec executor.model, no operator override, no deployment default) appends
	// NO flag, so the spawn is byte-identical to today and Claude Code uses its
	// built-in default model.
	if inv.Model != "" {
		args = append(args, "--model", inv.Model)
	}
	args = append(args, "-p", inv.Prompt)
	cmd := cmdFn(ctx, binary, args...)
	cmd.Dir = inv.WorkingDir
	// Compose env so a Cmd builder (e.g. tests) can pre-set
	// vars on cmd.Env and we layer the API key on top. nil means
	// "child will inherit our env", so seed with os.Environ() in
	// that case to keep PATH, HOME, etc. for the agent process.
	//
	// A subprocess resolves a variable to the FIRST matching entry, so a
	// plain append would be shadowed by any inherited ANTHROPIC_API_KEY —
	// strip existing entries from the seed before appending the configured
	// one so i.APIKey actually wins (#899). An empty i.APIKey is skipped,
	// leaving the inherited env untouched.
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	if i.APIKey != "" {
		cmd.Env = agent.AppendEnvOverride(cmd.Env, "ANTHROPIC_API_KEY", i.APIKey)
	}
	// Layer Invocation.Env on top so per-run secrets (FISHHAWK_API_TOKEN,
	// FISHHAWK_BACKEND_URL, etc. set by the runner per E19.8 / #348)
	// reach the agent process. The agent's MCP server reads these to
	// authenticate against the Fishhawk backend; missing them is
	// fine — MCP awareness is best-effort per ADR-021. Route each through
	// AppendEnvOverride too so a per-run value deterministically overrides
	// any inherited same-named host var (making Invocation.Env's "later
	// keys win" contract true, not first-match-wins).
	for k, v := range inv.Env {
		if k == "" {
			continue
		}
		cmd.Env = agent.AppendEnvOverride(cmd.Env, k, v)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return res, false, fmt.Errorf("claudecode: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return res, false, fmt.Errorf("claudecode: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		// Distinguish "binary missing" from other start errors so
		// callers can surface a precise error to the operator.
		if isBinaryMissing(err) {
			return failureResult(res, now(), "A",
				fmt.Sprintf("agent binary not found: %s", binary),
				"binary_not_found",
			), false, agent.ErrBinaryNotFound
		}
		return res, false, fmt.Errorf("claudecode: start: %w", err)
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
	inputTokens := 0
	outputTokens := 0
	model := ""
	budgetHit := false
	// Loop / duplicate-action detection (#653). The detector watches the
	// tool-call signature stream and trips on an unbroken run of identical
	// signatures; on trip we kill the agent and fail the stage with
	// agent.ErrLoopDetected. loopSig/loopCount carry the figures into the
	// audit reason. The detector is per-invokeOnce (not shared across
	// thinking-block retries) because a fresh re-spawn starts a fresh
	// action stream.
	loopDetector := agent.NewLoopDetector(i.LoopThreshold)
	loopHit := false
	loopSig := ""
	loopCount := 0
	// resultPayload retains the terminal type=="result" event so a
	// post-mortem can inspect is_error / api_error_status for
	// thinking-block detection (see isThinkingBlock400).
	var resultPayload []byte

	// Progress heartbeat state (#580). The scan loop below writes
	// turns / tokensUsed / lastKind; the heartbeat goroutine reads
	// them on each tick. Both accesses go through progMu so the race
	// detector stays quiet (Go memory model: concurrent access from
	// multiple goroutines needs explicit synchronization).
	var (
		progMu   sync.Mutex
		turns    int
		lastKind string
	)
	start := now()

	// Heartbeat goroutine. It is the SOLE writer to inv.ProgressSink
	// during Invoke, so single whole-line Fprintf writes never
	// interleave with another writer's partial line. Proof by
	// inspection of every ProgressSink (== runner logSink) writer:
	//   - This goroutine — the only writer inside invokeOnce.
	//   - The scan loop (same invokeOnce) — touches res.Events and the
	//     progMu-guarded counters only; never writes ProgressSink.
	//   - main.go run() lifecycle lines (runner_started, prompt_fetched,
	//     mcp_token_issued, etc.) — all on run()'s main goroutine, which
	//     is blocked inside invoker.Invoke for the whole invocation, so
	//     they are strictly before/after, never concurrent.
	//   - main.go's deferred runner_cancelled line — runs only when
	//     run() returns, i.e. after Invoke has already returned; a
	//     SIGTERM/cancel during Invoke propagates via ctx (cooperative
	//     shutdown) and the line is emitted post-Invoke, not concurrently.
	// Hence no second goroutine ever writes ProgressSink while this one
	// is running, and JSONL line integrity is guaranteed. A nil
	// ProgressSink starts no goroutine and emits zero heartbeats.
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
					// Time-driven, not event-driven: a stalled stage
					// still emits heartbeats with non-advancing counters,
					// which is exactly how the driver tells "alive and
					// progressing" from "stuck".
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

	// allowedRoots is the working tree plus the explicitly allowlisted
	// extra dirs (shared with --add-dir via allowedExtraDirs). The
	// detector flags any Write/Edit-tool target outside all of these.
	allowedRoots := append([]string{inv.WorkingDir}, allowedExtraDirs...)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		ev, info, ok := parseLine(line, now())
		used := info.InputTokens + info.OutputTokens
		// Model id is pinned from the latest line that surfaced one
		// (assistant/result events carry it; system.init does not).
		if info.Model != "" {
			model = info.Model
		}
		res.Events = append(res.Events, ev)
		if ev.Kind == "result" || strings.HasPrefix(ev.Kind, "result.") {
			resultPayload = append([]byte(nil), ev.Payload...)
		}
		// Surface (never block) any agent write targeting a path outside
		// the working tree + allowlist. Purely additive: a detection
		// appends a warning event and does NOT flip res.OK or fail the
		// stage. The detector is fail-open — an unparseable / unknown
		// shape line yields no writes, never a panic.
		for _, w := range outOfTreeWrites(line, allowedRoots) {
			res.Events = append(res.Events, agent.Event{
				Kind:      "out_of_tree_write",
				Timestamp: now(),
				Payload: agent.MakePayload(map[string]string{
					"path":   w.path,
					"tool":   w.tool,
					"run_id": inv.RunID,
					"stage":  inv.Stage,
				}),
			})
		}
		// Feed each tool-call signature on this line to the loop detector.
		// On trip, mark a loop_detected trace event, kill the agent, and
		// break out of the scan loop — the terminal switch maps loopHit to
		// agent.ErrLoopDetected. Signatures are extracted fail-open, so an
		// unparseable line contributes none.
		for _, sig := range toolCallSignatures(line) {
			if loopDetector.Observe(sig) {
				loopHit = true
				loopSig = sig
				loopCount = loopDetector.Streak()
				res.Events = append(res.Events, agent.Event{
					Kind:      "loop_detected",
					Timestamp: now(),
					Payload: agent.MakePayload(map[string]any{
						"signature": loopSig,
						"count":     loopCount,
						"run_id":    inv.RunID,
						"stage":     inv.Stage,
					}),
				})
				_ = cmd.Process.Kill()
				break
			}
		}
		if loopHit {
			break
		}
		progMu.Lock()
		turns++
		lastKind = ev.Kind
		if ok && used > 0 {
			// Usage lines report cumulative counts, so the latest
			// non-zero line wins (not a running sum within an attempt).
			tokensUsed = used
			inputTokens = info.InputTokens
			outputTokens = info.OutputTokens
		}
		progMu.Unlock()
		if ok && used > 0 {
			if inv.Budget.MaxTokens > 0 && tokensUsed > inv.Budget.MaxTokens {
				budgetHit = true
				_ = cmd.Process.Kill()
				break
			}
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
	res.InputTokens = inputTokens
	res.OutputTokens = outputTokens
	res.Model = model

	// A non-zero exit whose result payload or stderr carries the
	// durable thinking-block marker is the one fault Invoke retries.
	thinkingBlock := waitErr != nil && isThinkingBlock400(resultPayload, stderrBuf.String())

	switch {
	case budgetHit:
		return failureResult(res, now(), "A",
			fmt.Sprintf("token budget exceeded: used %d, max %d", tokensUsed, inv.Budget.MaxTokens),
			"budget_exceeded",
		), false, agent.ErrBudgetExceeded

	case loopHit:
		// A loop is terminal and NOT retried (false): re-running the same
		// prompt would just loop again. Classified category-A so stage-level
		// handling treats it like any other agent failure, but the sentinel
		// is ErrLoopDetected so callers can switch on it.
		return failureResult(res, now(), "A",
			fmt.Sprintf("loop detected: %d identical consecutive tool calls: %s",
				loopCount, truncateSignature(loopSig)),
			"loop_detected",
		), false, agent.ErrLoopDetected

	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return failureResult(res, now(), "A",
			fmt.Sprintf("agent timeout after %s", inv.Budget.Timeout),
			"timeout",
		), false, agent.ErrTimeout

	case thinkingBlock:
		return failureResult(res, now(), "A",
			fmt.Sprintf("transient thinking-block API 400: %v", waitErr),
			"agent_api_thinking_block",
		), true, fmt.Errorf("%w: %v", agent.ErrAgentThinkingBlock, waitErr)

	case waitErr != nil:
		return failureResult(res, now(), "A",
			fmt.Sprintf("agent exited with error: %v", waitErr),
			"agent_error",
		), false, fmt.Errorf("%w: %v", agent.ErrAgentFailed, waitErr)

	case scanErr != nil:
		return failureResult(res, now(), "A",
			fmt.Sprintf("trace stream read error: %v", scanErr),
			"stream_error",
		), false, fmt.Errorf("%w: %v", agent.ErrAgentFailed, scanErr)
	}

	res.OK = true
	res.Events = append(res.Events, agent.Event{
		Kind:      "invocation_end",
		Timestamp: now(),
		Payload:   agent.MakePayload(map[string]any{"outcome": "ok", "tokens_used": tokensUsed}),
	})
	return res, false, nil
}

// isThinkingBlock400 reports whether a failed attempt was the
// transient interleaved-thinking API 400 — the one fault Invoke
// retries. Anthropic returns this when a prior assistant message's
// thinking/redacted_thinking blocks were modified before being passed
// back (extended-thinking guide: blocks must be preserved verbatim).
// On a long agent run the Claude Code harness can trip this at high
// turn counts; a fresh re-spawn clears the corrupted history.
//
// Detection matches the DURABLE fragments "thinking" + "cannot be
// modified" (case-insensitive) in the result payload or stderr, rather
// than the full sentence, so minor wording drift doesn't silently
// regress to no-retry. When the result payload carries an explicit
// api_error_status it must corroborate (== 400): a 400 whose message
// is unrelated is NOT a thinking-block fault, and a payload without the
// marker is never one regardless of status.
func isThinkingBlock400(resultPayload []byte, stderr string) bool {
	hay := strings.ToLower(string(resultPayload) + "\n" + stderr)
	if !strings.Contains(hay, "thinking") || !strings.Contains(hay, "cannot be modified") {
		return false
	}
	var meta struct {
		APIErrorStatus *int `json:"api_error_status"`
	}
	if err := json.Unmarshal(resultPayload, &meta); err == nil && meta.APIErrorStatus != nil {
		return *meta.APIErrorStatus == 400
	}
	return true
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

// lineInfo carries the structured usage + model metadata parseLine
// extracted from one stream-json line, beyond the kind already on
// the event. InputTokens/OutputTokens are the split counts (their
// sum is the legacy total); Model is the resolved model id when the
// line surfaced one.
type lineInfo struct {
	InputTokens  int
	OutputTokens int
	Model        string
}

// usageBlock is the shape of Claude Code's `usage` object, present
// either at the top level (the convention in the recorded test
// fixtures) or nested under `message` on a real assistant event.
type usageBlock struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// parseLine turns one JSON line from Claude Code's stream-json
// output into an agent.Event. The kind is taken from the line's
// `type` field when present; unknown / non-JSON lines become
// kind=raw so we never silently drop trace bytes.
//
// Returns (event, info, hasUsage). hasUsage is true when the line
// carried a usage block (top-level or message-nested) whose token
// sum is > 0. info.Model is the resolved model id (top-level `model`
// or `message.model`) when present, "" otherwise — surfaced even on
// lines without usage so the assistant/result event's model pins
// cost + reproducibility (G6).
func parseLine(line []byte, ts time.Time) (agent.Event, lineInfo, bool) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return agent.Event{Kind: "raw", Timestamp: ts}, lineInfo{}, false
	}

	// Probe the kind without unmarshaling the whole payload. usage +
	// model appear top-level on the synthesized fixtures and the
	// result event, but nested under `message` on a real assistant
	// event — accept both shapes, top-level winning.
	var meta struct {
		Type    string      `json:"type"`
		Subtype string      `json:"subtype"`
		Model   string      `json:"model"`
		Usage   *usageBlock `json:"usage"`
		Message *struct {
			Model string      `json:"model"`
			Usage *usageBlock `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(trimmed, &meta); err != nil {
		// Non-JSON line — capture verbatim so the trace is still
		// honest about what the child wrote.
		return agent.Event{
			Kind:      "raw",
			Timestamp: ts,
			Payload:   agent.MakePayload(map[string]string{"text": string(trimmed)}),
		}, lineInfo{}, false
	}

	kind := meta.Type
	if kind == "" {
		kind = "raw"
	} else if meta.Subtype != "" {
		kind = kind + "." + meta.Subtype
	}

	usage := meta.Usage
	if usage == nil && meta.Message != nil {
		usage = meta.Message.Usage
	}
	model := meta.Model
	if model == "" && meta.Message != nil {
		model = meta.Message.Model
	}

	var info lineInfo
	info.Model = model
	hasUsage := false
	if usage != nil {
		info.InputTokens = usage.InputTokens
		info.OutputTokens = usage.OutputTokens
		hasUsage = info.InputTokens+info.OutputTokens > 0
	}

	return agent.Event{
		Kind:      kind,
		Timestamp: ts,
		Payload:   json.RawMessage(trimmed),
	}, info, hasUsage
}

// toolCallSignatures extracts a stable signature for every tool_use block
// in one Claude Code assistant stream-json line, for the loop detector
// (#653). A signature is the tool name plus its canonicalised input
// arguments, so two identical "Read file X" calls collide while "Read
// file X" and "Read file Y" stay distinct — only a genuinely repeated
// ACTION accumulates toward a loop, not merely a repeated tool.
//
// Like outOfTreeWrites it is fail-open: a non-assistant line, a non-tool
// block, an unparseable line, or unparseable input all yield no signatures
// rather than a panic, so stream-json schema drift degrades to no-signal.
func toolCallSignatures(line []byte) []string {
	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &msg); err != nil || msg.Type != "assistant" {
		return nil
	}
	var sigs []string
	for _, block := range msg.Message.Content {
		if block.Type != "tool_use" {
			continue
		}
		sigs = append(sigs, block.Name+" "+canonicalInput(block.Input))
	}
	return sigs
}

// canonicalInput renders a tool_use input to a stable string so equal
// arguments compare equal regardless of key order. json.Marshal sorts map
// keys, so round-tripping through an interface{} canonicalises object key
// ordering. Fail-open: on any parse/marshal failure the raw bytes are used
// verbatim (still deterministic for byte-identical inputs).
func canonicalInput(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// truncateSignature bounds a signature embedded in an audit/failure reason
// so a large tool input (e.g. a full file body in a Write call) cannot
// bloat the reason string. The detection figures (count) are the
// load-bearing part; the signature is a hint.
func truncateSignature(sig string) string {
	const max = 160
	if len(sig) <= max {
		return sig
	}
	return sig[:max] + "…"
}

// outOfTreeWrite is one detected file-writing tool_use whose target
// escapes the allowed roots.
type outOfTreeWrite struct {
	tool string
	path string
}

// outOfTreeWrites inspects one Claude Code stream-json line and returns
// every file-writing tool_use whose target path is NOT contained within
// an allowed root. allowedRoots is inv.WorkingDir followed by
// allowedExtraDirs; relative target paths resolve against allowedRoots[0]
// (the working dir).
//
// It is a SURFACING signal, not a gate: the caller appends a warning
// event and never fails the stage. Accordingly the function is
// fail-open — any parse failure, a non-assistant line, an unknown
// payload shape, or a missing path yields no writes and never panics, so
// a stream-json schema drift across claude versions degrades to
// no-signal rather than a crash.
func outOfTreeWrites(line []byte, allowedRoots []string) []outOfTreeWrite {
	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &msg); err != nil || msg.Type != "assistant" {
		return nil
	}

	base := ""
	if len(allowedRoots) > 0 {
		base = allowedRoots[0]
	}

	var out []outOfTreeWrite
	for _, block := range msg.Message.Content {
		if block.Type != "tool_use" {
			continue
		}
		field, ok := fileWritingTools[block.Name]
		if !ok {
			continue
		}
		var input map[string]json.RawMessage
		if err := json.Unmarshal(block.Input, &input); err != nil {
			continue
		}
		raw, ok := input[field]
		if !ok {
			continue
		}
		var target string
		if err := json.Unmarshal(raw, &target); err != nil || target == "" {
			continue
		}
		if !containedInAny(target, base, allowedRoots) {
			out = append(out, outOfTreeWrite{tool: block.Name, path: target})
		}
	}
	return out
}

// containedInAny reports whether target (resolved against base if it is
// relative) lies within any of allowedRoots. Comparison is on cleaned,
// symlink-resolved absolute paths: a target is inside a root iff
// filepath.Rel succeeds and the result neither escapes upward ("..",
// "../…") nor is absolute.
func containedInAny(target, base string, allowedRoots []string) bool {
	abs := target
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(base, abs)
	}
	abs = resolveSymlinks(abs)
	for _, root := range allowedRoots {
		r := resolveSymlinks(root)
		rel, err := filepath.Rel(r, abs)
		if err != nil {
			continue
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			continue
		}
		return true
	}
	return false
}

// resolveSymlinks canonicalises path as far as the filesystem allows.
// filepath.EvalSymlinks fails on a path that does not exist yet — which
// is the COMMON case here, since the agent typically writes NEW files —
// so we resolve the deepest EXISTING ancestor and re-append the
// not-yet-created tail. This still canonicalises e.g. macOS's
// /tmp -> /private/tmp symlink (the agent emits the resolved
// /private/tmp form) on the existing parent dirs, so a new in-tree file
// is correctly judged contained while a new out-of-tree file is flagged.
// Fail-open: if no ancestor resolves, the cleaned input is returned.
func resolveSymlinks(path string) string {
	path = filepath.Clean(path)
	tail := ""
	cur := path
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			if tail == "" {
				return resolved
			}
			return filepath.Join(resolved, tail)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the filesystem root with nothing resolvable.
			return path
		}
		if tail == "" {
			tail = filepath.Base(cur)
		} else {
			tail = filepath.Join(filepath.Base(cur), tail)
		}
		cur = parent
	}
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
