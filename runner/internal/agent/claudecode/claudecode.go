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

// quotaUnavailableMaxWall bounds the "instant post-init exit" window that
// fingerprints a model-quota exhaustion (a usage / rate cap): a non-zero
// exit that reached no model call within this long of spawn is the
// "cannot obtain model quota" shape from run a75b0765 (#2085). It is a
// conservative heuristic guard, not a precise marker — a slow zero-token
// HANG is NOT a cap — so it is set generously (well past the few-second
// real-world observation) rather than tight. See isQuotaUnavailable.
const quotaUnavailableMaxWall = 30 * time.Second

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

	// WaitPollThreshold is the much higher threshold applied to a streak
	// of WAIT-POLL tool calls — an agent awaiting a long-running
	// backgrounded command via BashOutput or a repeated read-only log
	// inspection (see isWaitPoll, #2758). Zero means
	// agent.DefaultWaitPollThreshold. Setting it equal to LoopThreshold
	// collapses the two tiers back into the pre-#2758 single threshold.
	// A per-Invoker field so tests can lower it deterministically.
	WaitPollThreshold int
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
		agg.CacheReadInputTokens += res.CacheReadInputTokens
		agg.CacheWriteInputTokens += res.CacheWriteInputTokens
		if res.Model != "" {
			agg.Model = res.Model
		}
		// Structured output (#1325): latest non-empty attempt wins, like Model.
		if len(res.StructuredOutput) > 0 {
			agg.StructuredOutput = res.StructuredOutput
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
			// Carry the terminal external-API status (0 on every other path)
			// through the aggregate so the runner_completed event and the
			// operator next_actions hint can name it (#1548).
			agg.APIErrorStatus = res.APIErrorStatus
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
	// --json-schema: constrain the agent's structured_output to the given
	// schema (#1325). The CLI takes the schema as an INLINE JSON string
	// argument (NOT a file path — verified against claude 2.1.186). When set,
	// the terminal result event carries a top-level structured_output object
	// conforming to the schema, captured below onto Result.StructuredOutput.
	// An empty inv.JSONSchema appends NO flag, so the spawn is byte-identical
	// to today and structured_output stays nil — the feature is gated entirely
	// on this field being non-empty.
	if inv.JSONSchema != "" {
		args = append(args, "--json-schema", inv.JSONSchema)
	}
	args = append(args, "-p", inv.Prompt)
	cmd := cmdFn(ctx, binary, args...)
	cmd.Dir = inv.WorkingDir
	// Compose env so a Cmd builder (e.g. tests) can pre-set
	// vars on cmd.Env and we layer the API key on top. nil means
	// "child will inherit our env", so seed with os.Environ() in
	// that case to keep PATH, HOME, etc. for the agent process —
	// UNLESS the invocation carries a BaseEnv (ADR-050 / #1535),
	// which replaces the os.Environ() seed with a minimized set so
	// the child never sees runner-env secrets. A nil BaseEnv is
	// byte-identical to the pre-BaseEnv behavior; a non-nil empty
	// one seeds an empty env (default-deny). Copied so the caller's
	// slice is never aliased by the overlay appends below.
	//
	// A subprocess resolves a variable to the FIRST matching entry, so a
	// plain append would be shadowed by any inherited ANTHROPIC_API_KEY —
	// strip existing entries from the seed before appending the configured
	// one so i.APIKey actually wins (#899). An empty i.APIKey is skipped,
	// leaving the inherited env untouched.
	if cmd.Env == nil {
		if inv.BaseEnv != nil {
			// append to a non-nil empty slice, NOT []string(nil): an empty
			// BaseEnv must yield a non-nil cmd.Env, because os/exec treats
			// nil as inherit-parent-env — the opposite of default-deny.
			cmd.Env = append([]string{}, inv.BaseEnv...)
		} else {
			cmd.Env = os.Environ()
		}
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
	cacheReadInputTokens := 0
	cacheWriteInputTokens := 0
	model := ""
	budgetHit := false
	// Loop / duplicate-action detection (#653). The detector watches the
	// tool-call signature stream and trips on an unbroken run of identical
	// signatures; on trip we kill the agent and fail the stage with
	// agent.ErrLoopDetected. loopSig/loopCount carry the figures into the
	// audit reason. The detector is per-invokeOnce (not shared across
	// thinking-block retries) because a fresh re-spawn starts a fresh
	// action stream.
	loopDetector := agent.NewLoopDetector(i.LoopThreshold, i.WaitPollThreshold)
	loopHit := false
	loopSig := ""
	loopCount := 0
	// loopWait/loopThreshold record which TIER tripped (#2758) so the audit
	// payload and failure reason can name it.
	loopWait := false
	loopThreshold := 0
	// resultPayload retains the terminal type=="result" event so a
	// post-mortem can inspect is_error / api_error_status for
	// thinking-block detection (see isThinkingBlock400).
	var resultPayload []byte
	// structuredOutput captures the schema-guaranteed object from the
	// terminal result event's top-level `structured_output` field when the
	// invocation carried a --json-schema (#1325). nil when absent — the
	// documented fallback trigger.
	var structuredOutput []byte

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
		// Capture the schema-guaranteed structured_output object whenever a
		// line surfaces one (it rides on the terminal result event). The
		// latest non-empty wins.
		if len(info.StructuredOutput) > 0 {
			structuredOutput = info.StructuredOutput
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
			// The sanctioned scope-amendments wait-poll (#1273) is an
			// intentionally-repeated long-poll the prompt instructs the
			// agent to issue (backend/internal/prompt/prompt.go:1020).
			// Skip feeding it to the detector entirely: like an empty
			// signature it is a no-op — it neither accumulates a streak
			// nor resets a real one — so a bounded amendment wait reaches
			// the agent's own decision/EXPIRY branch instead of being
			// killed early as a category-A loop_detected.
			if isSanctionedWaitPoll(sig) {
				continue
			}
			// Classify the signature so an agent legitimately AWAITING a
			// long-running backgrounded command is held to the much higher
			// wait-poll threshold rather than killed at ~8 polls (#2758).
			// Unlike the #1273 skip above this is not an exemption: a
			// wait-classed streak still accumulates and still trips.
			if loopDetector.Observe(sig, isWaitPoll(sig)) {
				loopHit = true
				loopSig = sig
				loopCount = loopDetector.Streak()
				loopWait = loopDetector.WaitPoll()
				loopThreshold = loopDetector.EffectiveThreshold()
				res.Events = append(res.Events, agent.Event{
					Kind:      "loop_detected",
					Timestamp: now(),
					Payload: agent.MakePayload(map[string]any{
						"signature": loopSig,
						"count":     loopCount,
						"wait_poll": loopWait,
						"threshold": loopThreshold,
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
		if ok {
			// Usage lines report cumulative counts, so the latest line
			// carrying usage wins (not a running sum within an attempt). The
			// gate is hasUsage (ok), not a non-zero fresh-token sum: a
			// cache-only line (zero fresh input/output but non-zero cache
			// read/write) still carries spend and its buckets must not be
			// dropped (#1349). TokensUsed stays the fresh input+output total
			// (the cache buckets are priced separately on Result); the buckets
			// ride the same winning line.
			tokensUsed = used
			inputTokens = info.InputTokens
			outputTokens = info.OutputTokens
			cacheReadInputTokens = info.CacheReadInputTokens
			cacheWriteInputTokens = info.CacheWriteInputTokens
		}
		progMu.Unlock()
		if ok {
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
	res.CacheReadInputTokens = cacheReadInputTokens
	res.CacheWriteInputTokens = cacheWriteInputTokens
	res.Model = model
	res.StructuredOutput = structuredOutput

	// A non-zero exit whose result payload or stderr carries the
	// durable thinking-block marker is the one fault Invoke retries.
	thinkingBlock := waitErr != nil && isThinkingBlock400(resultPayload, stderrBuf.String())
	// apiStatus is the terminal result event's api_error_status (0 when
	// absent). A 5xx value denotes a terminal external-API incident (the
	// agent's in-run retries were exhausted), lifted below onto the Result.
	apiStatus := terminalAPIErrorStatus(resultPayload)
	// elapsed is the wall-clock span from spawn to the terminal switch,
	// derived from the same fake-clock seam (now/start) the heartbeat uses,
	// so the quota-unavailable wall-clock bound is deterministic in tests.
	elapsed := now().Sub(start)

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
		// The reason names the TIER that tripped (#2758) so an operator can
		// tell a base-threshold no-progress loop from a frozen wait-poll,
		// keeping the "loop detected: " prefix and the count so existing
		// reason-matching stays valid.
		kind := "tool calls"
		if loopWait {
			kind = "wait-poll tool calls"
		}
		return failureResult(res, now(), "A",
			fmt.Sprintf("loop detected: %d identical consecutive %s: %s",
				loopCount, kind, truncateSignature(loopSig)),
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

	case waitErr != nil && apiStatus >= 500:
		// A terminal 5xx external-API error (overloaded/unavailable, e.g.
		// 529 — the agent's in-run retries were exhausted). Still an
		// agent-surface failure (category "A") but the ErrExternalAPI
		// sentinel plus the stable "terminal external API error <N>" reason
		// phrase carry the real upstream cause so the operator surface can
		// name it without trace archaeology. Checked AFTER the 400
		// thinking-block arm (status-disjoint) and BEFORE the generic
		// agent_error arm; a non-5xx failure falls through with
		// APIErrorStatus==0.
		res.APIErrorStatus = apiStatus
		return failureResult(res, now(), "A",
			fmt.Sprintf("terminal external API error %d (retries exhausted): %v", apiStatus, waitErr),
			"external_api",
		), false, fmt.Errorf("%w: %v", agent.ErrExternalAPI, waitErr)

	case isQuotaUnavailable(waitErr, tokensUsed, model, elapsed):
		// Model-quota exhaustion (a usage / rate cap): the agent exited
		// non-zero having reached no model call (0 tokens, no model id) within
		// a short wall-clock bound of spawn, after runner-side init already
		// succeeded — the "cannot obtain model quota" fingerprint (#2085 / run
		// a75b0765). Category "A" (agent-surface) but the
		// ErrAgentQuotaUnavailable sentinel plus the stable "could not obtain
		// model quota" reason phrase let the operator surface tell it apart
		// from a transient crash. NOT retried in-driver (false): re-running the
		// same prompt against an unreset cap would fail identically. Checked
		// AFTER the thinking-block and external-API arms so a fast zero-token
		// exit that ALSO carried a thinking-block marker or a 5xx
		// api_error_status still classifies as those; a failure with tokens>0,
		// a model id, or elapsed over the bound falls through unchanged to the
		// generic agent_error arm.
		return failureResult(res, now(), "A",
			fmt.Sprintf("could not obtain model quota (likely a usage/rate cap): agent exited with %v after %s having made no model call (0 tokens)",
				waitErr, elapsed.Round(time.Second)),
			"agent_quota_unavailable",
		), false, fmt.Errorf("%w: %v", agent.ErrAgentQuotaUnavailable, waitErr)

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

// terminalAPIErrorStatus extracts the terminal result event's
// api_error_status integer from the retained result payload, returning 0
// when the field is absent or the payload is unparseable. It reads the
// SAME api_error_status field isThinkingBlock400 already inspects; here a
// 5xx value classifies the failure as a terminal external-API incident
// (see the apiStatus>=500 arm in invokeOnce). Fail-soft: any unmarshal
// error or a missing field yields 0, so a non-5xx / no-status failure
// falls through to the unchanged generic agent_error path.
func terminalAPIErrorStatus(resultPayload []byte) int {
	var meta struct {
		APIErrorStatus *int `json:"api_error_status"`
	}
	if err := json.Unmarshal(resultPayload, &meta); err == nil && meta.APIErrorStatus != nil {
		return *meta.APIErrorStatus
	}
	return 0
}

// isQuotaUnavailable reports whether a failed attempt matches the
// model-quota-exhaustion fingerprint (#2085): the agent exited non-zero
// (waitErr != nil) having reached NO model call — zero reported tokens
// (tokensUsed == 0) AND no model id seen (model == "") — within a short
// wall-clock bound of spawn (elapsed <= quotaUnavailableMaxWall). model=="" is
// the load-bearing "no model call was reached" signal: only assistant/result
// stream-json events carry a model id, and system.init does not (see the
// pin comment near the model-id capture in the scan loop), so an empty model
// means no model turn ever started. tokensUsed==0 discriminates a
// cap-exhausted session (0 tokens) from a genuine transient crash (which
// reports non-zero usage — run a75b0765's ~52000-token crash). The wall-clock
// bound is a conservative guard so a slow zero-token HANG is not mislabeled a
// cap. Mirrors isThinkingBlock400 as a pure, side-effect-free helper the
// terminal switch keys an arm on.
func isQuotaUnavailable(waitErr error, tokensUsed int, model string, elapsed time.Duration) bool {
	return waitErr != nil && tokensUsed == 0 && model == "" && elapsed <= quotaUnavailableMaxWall
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
	// CacheReadInputTokens / CacheWriteInputTokens are the prompt-cache
	// split (ADR-044 / #1349): Anthropic usage reports input_tokens
	// EXCLUSIVE of cache_read_input_tokens and cache_creation_input_tokens
	// (three separate, additive line items), so InputTokens needs no
	// normalization — these carry the cache-served read and the
	// cache-creation write portions independently.
	CacheReadInputTokens  int
	CacheWriteInputTokens int
	Model                 string
	// StructuredOutput is the raw JSON bytes of the line's top-level
	// `structured_output` field, present on the terminal result event when
	// the invocation carried a --json-schema (#1325). nil otherwise.
	StructuredOutput []byte
}

// usageBlock is the shape of Claude Code's `usage` object, present
// either at the top level (the convention in the recorded test
// fixtures) or nested under `message` on a real assistant event.
type usageBlock struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// CacheReadInputTokens / CacheCreationInputTokens are the prompt-cache
	// portions of the input side (ADR-044 / #1349). Anthropic reports them as
	// separate, additive line items alongside input_tokens (which is already
	// cache-exclusive). Absent on older streams / non-cache lines → decode 0.
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
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
		Type             string          `json:"type"`
		Subtype          string          `json:"subtype"`
		Model            string          `json:"model"`
		Usage            *usageBlock     `json:"usage"`
		StructuredOutput json.RawMessage `json:"structured_output"`
		Message          *struct {
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
	// Capture structured_output (#1325) — copy out of the trimmed backing
	// slice; skip a literal JSON null so it reads as absent.
	if len(meta.StructuredOutput) > 0 && string(bytes.TrimSpace(meta.StructuredOutput)) != "null" {
		info.StructuredOutput = append([]byte(nil), meta.StructuredOutput...)
	}
	hasUsage := false
	if usage != nil {
		info.InputTokens = usage.InputTokens
		info.OutputTokens = usage.OutputTokens
		// input_tokens is already cache-exclusive in Anthropic usage, so
		// InputTokens needs no normalization; the cache portions ride
		// alongside as their own additive buckets (#1349).
		info.CacheReadInputTokens = usage.CacheReadInputTokens
		info.CacheWriteInputTokens = usage.CacheCreationInputTokens
		// A line carries usage if ANY token bucket is non-zero — including a
		// cache-only line (zero fresh input/output but a non-zero cache read or
		// cache-creation count), e.g. a turn whose entire prompt was served from
		// the prompt cache. Gating on the fresh input+output sum alone would
		// drop that cache spend before it reaches agent.Result (#1349).
		hasUsage = info.InputTokens+info.OutputTokens+info.CacheReadInputTokens+info.CacheWriteInputTokens > 0
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

// isSanctionedWaitPoll reports whether a tool-call signature is the
// operator-gated scope-amendment wait-poll the prompt instructs the agent
// to issue while awaiting a decision: GET .../scope-amendments?wait=30
// (backend/internal/prompt/prompt.go:1020). That bounded long-poll is a
// deliberately-repeated identical action, so the loop detector (#653) would
// otherwise count it as a no-progress loop and kill the stage category-A
// (#1273) before the documented ~15-minute wait-poll window elapses. (That cap
// is an EXPIRY, not a denial, since #2601 — the prompt's step-5 branch — so the
// older "proceed-as-denied window" name for it is retired.)
// The feed loop skips this signature so it is a no-op for the detector.
//
// The match is deliberately NARROW to the documented prompt form: it requires
// the `Bash ` tool-name prefix AND the `scope-amendments` path AND `wait=` as
// a QUERY parameter of that path (introduced by `?` immediately after the path
// and present as the first parameter or after a `&`). A bare `wait=` elsewhere
// in an arbitrary Bash command, a non-waiting GET (no `wait=`), or any non-Bash
// tool does NOT match and is counted by the detector as normal.
func isSanctionedWaitPoll(sig string) bool {
	const bashPrefix = "Bash "
	if !strings.HasPrefix(sig, bashPrefix) {
		return false
	}
	const pathMarker = "scope-amendments"
	idx := strings.Index(sig[len(bashPrefix):], pathMarker)
	if idx < 0 {
		return false
	}
	rest := sig[len(bashPrefix)+idx+len(pathMarker):]
	// The query must be introduced by '?' immediately after the path.
	if len(rest) == 0 || rest[0] != '?' {
		return false
	}
	query := rest[1:]
	// Bound the query to the URL token: stop at the first character that
	// ends a URL inside a JSON-escaped shell command. '&' is excluded — it
	// separates query parameters, so a wait= introduced by '&' stays in
	// scope.
	const urlEnders = " \t\n\"'\\<>}|;" + "`"
	if end := strings.IndexAny(query, urlEnders); end >= 0 {
		query = query[:end]
	}
	// wait= must be a query parameter of the scope-amendments path: the
	// first parameter, or one introduced by '&'.
	return strings.HasPrefix(query, "wait=") || strings.Contains(query, "&wait=")
}

// waitPollCommands is the allow-list of command names admitted by
// isWaitPoll's read-only chain test (#2758).
//
// ADMISSION RULE — read this before adding an entry. A command earns a
// place here only if it has NO DOCUMENTED MODE that executes another
// command or mutates system state. Check that property against the
// command's own documentation and record the check; the list is only the
// OUTPUT of that check, and copying the list without redoing the check is
// how the class of bug below gets re-introduced.
//
// The rule exists because an argv[0] allow-list cannot express "this
// command in a read-only mode" — an allow-listed NAME carries every mode
// that name has, including its mutating and delegating ones. Two names
// that read as obviously-inert were rejected on exactly that ground:
//
//   - `jobs`: `jobs -x command [args]` substitutes jobspecs into its
//     arguments and then EXECUTES the command. `jobs -x rm -rf x` contains
//     no newline, redirect, backtick, `$(` or bare `&`, and its argv[0] is
//     `jobs` — so an allow-list carrying `jobs` would wait-class a call
//     that deletes a tree. Verified by execution, not by reading a man
//     page. Special-casing the `-x` flag was deliberately NOT the fix: an
//     agent awaiting a backgrounded command uses BashOutput or reads the
//     log, so `jobs` buys nothing that justifies parsing its flag grammar.
//   - `date`: GNU `date -s`/`--set` and BSD `date`'s bare time argument SET
//     THE SYSTEM CLOCK. Polling never needs `date`.
//
// Both are pinned as NOT-wait-classed by TestIsWaitPoll, which must stay
// green if anyone ever re-adds them.
var waitPollCommands = map[string]bool{
	"sleep": true, // suspends; no exec or mutate mode
	"wait":  true, // shell builtin, awaits jobs; no exec or mutate mode
	"ps":    true, // reports processes; no signal/exec mode (that is `kill`)
	"pgrep": true, // matches processes; the signalling variant is `pkill`
	"tail":  true, // reads
	"head":  true, // reads
	"cat":   true, // reads (writing needs a shell redirect, rejected above)
	"wc":    true, // reads
	"grep":  true, // reads; no -exec analogue (that is `find`, excluded)
	"ls":    true, // reads
	"stat":  true, // reads
	"test":  true, // evaluates a condition; no exec or mutate mode
	"[":     true, // `test` under its bracket name
	"echo":  true, // writes to stdout only
}

// isWaitPoll reports whether a tool-call signature is a WAIT-POLL: an
// identical-by-construction call an agent issues while AWAITING a
// long-running backgrounded command (#2758). A wait-classed streak is held
// to agent.DefaultWaitPollThreshold instead of agent.DefaultLoopThreshold,
// because the poll is the only instrument the agent has for the wait and
// its output is legitimately byte-identical for minutes at a time — the
// observed kill in run 3490416b was the signature
//
//	Bash {"command":"tail -2 /tmp/verify-2591.log","description":"Check verify progress"}
//
// repeated 8 times while `scripts/test verify` ran lint in silence.
//
// Two forms qualify:
//
//   - the BashOutput tool, which is definitionally an await of a
//     backgrounded command; and
//   - a Bash command that is a chain of READ-ONLY inspection commands, per
//     isReadOnlyPollChain below.
//
// This is distinct from isSanctionedWaitPoll above, which grants its one
// prompt-mandated long-poll a TOTAL exemption; a wait-poll here is still
// counted and still trips, just at the higher threshold.
//
// It is FAIL-CLOSED throughout: anything it does not fully understand
// yields false, i.e. the pre-#2758 base-threshold behaviour. A parse gap
// can therefore only under-classify, never let a mutating command reach
// the higher limit.
func isWaitPoll(sig string) bool {
	if strings.HasPrefix(sig, "BashOutput ") {
		return true
	}
	const bashPrefix = "Bash "
	if !strings.HasPrefix(sig, bashPrefix) {
		return false
	}
	var input struct {
		Command string `json:"command"`
	}
	// Fail-open in the same sense as toolCallSignatures: an unparseable
	// remainder yields false, i.e. today's base-threshold behaviour.
	if err := json.Unmarshal([]byte(sig[len(bashPrefix):]), &input); err != nil {
		return false
	}
	return isReadOnlyPollChain(input.Command)
}

// isReadOnlyPollChain reports whether a shell command is a chain of
// allow-listed read-only inspection commands. It is a deliberately NARROW
// shell-lite parse and does NOT attempt to be a shell parser: it rejects
// outright anything carrying a construct whose semantics it cannot fully
// account for, and requires EVERY segment — not merely the first — to lead
// with an allow-listed bare command name.
//
// Rejected constructs, each because it can introduce execution or mutation
// the segment scan would not see: a newline (a second command line),
// redirection (`>`/`<` — `cat x > y` mutates), command substitution (`$(`
// or a backtick), parameter expansion (`${`, whose value is unknown at
// classification time), and a `&` that is not part of `&&` (backgrounding).
// Because the allow-list holds bare command names, a segment leading with
// a `VAR=x` env-assignment prefix or a path-qualified binary (`/bin/rm`,
// and equally `/usr/bin/tail`) is rejected by the lookup itself.
func isReadOnlyPollChain(cmd string) bool {
	if strings.ContainsAny(cmd, "\n\r><`") {
		return false
	}
	if strings.Contains(cmd, "$(") || strings.Contains(cmd, "${") {
		return false
	}
	// A '&' is admissible only as the '&&' operator; a lone '&' backgrounds
	// the preceding command and is rejected.
	for i := 0; i < len(cmd); i++ {
		if cmd[i] != '&' {
			continue
		}
		if i+1 < len(cmd) && cmd[i+1] == '&' {
			i++ // consume the pair
			continue
		}
		return false
	}
	// Split on the chain separators. '&&' and '||' are replaced before the
	// single-character forms so the two-character operators win.
	const sep = "\x00"
	segments := strings.Split(
		strings.NewReplacer("&&", sep, "||", sep, ";", sep, "|", sep).Replace(cmd),
		sep,
	)
	saw := false
	for _, seg := range segments {
		fields := strings.Fields(seg)
		if len(fields) == 0 {
			continue // a trailing separator leaves an empty segment
		}
		// The allow-list holds BARE command names only, so this lookup is
		// also what rejects a `VAR=x cmd` env-assignment prefix (whose
		// first token is `VAR=x`, hiding the real argv[0]) and a
		// path-qualified binary such as `/bin/rm` or even `/usr/bin/tail`
		// (a path need not resolve to the command its basename implies).
		if !waitPollCommands[fields[0]] {
			return false
		}
		saw = true
	}
	// saw is the empty-chain control: a command with no allow-listed
	// segment at all — the empty string, or one made only of separators
	// such as ";;;" — must NOT be wait-classed just because the segment
	// loop found nothing to reject.
	return saw
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
