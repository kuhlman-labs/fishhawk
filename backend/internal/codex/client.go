// Package codex provides a local-mode PlanReviewer adapter that spawns the
// Codex CLI as a subprocess for inference-only plan/implement review. It is the
// Codex sibling of backend/internal/claudecode (#575): instead of provisioning
// an Anthropic API key, a dogfood operator reuses their existing Codex setup
// (an OPENAI_API_KEY in the environment, or a ChatGPT login on the host), which
// the subprocess inherits via os.Environ().
//
// The adapter shells out to `codex exec --json --skip-git-repo-check <prompt>`
// for inference-only review and reads one JSON event per line from stdout. The
// flag shape and JSONL event schema below were pinned against the installed
// Codex CLI (codex-cli 0.137.0), the same version the runner executor adapter
// pinned (runner/internal/agent/codex). Unlike that streaming executor, this is
// inference-only: the review prompt forbids tool use, so there is no heartbeat,
// no process-group kill, and no --dangerously-bypass-approvals-and-sandbox — a
// real `codex exec --json --skip-git-repo-check` of a review prompt from a
// non-repo dir returns cleanly without blocking on approvals or sandbox setup.
// cmd.Output() captures the whole response in one buffer, sidestepping the
// StdoutPipe read race the streaming adapter must handle.
package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
)

// DefaultBinary is the executable name resolved against PATH when
// Config.Binary is empty.
const DefaultBinary = "codex"

// Config holds the settings needed to spawn the `codex` CLI for inference.
type Config struct {
	// Binary is the executable name or absolute path. Empty means
	// DefaultBinary.
	Binary string
	// APIKey is forwarded as OPENAI_API_KEY to the child, layered on top of the
	// inherited os.Environ(). Empty is NOT an error: the operator may have Codex
	// authenticated via a ChatGPT login on the host instead, in which case the
	// forwarded var is simply unused — the same posture as the runner executor
	// adapter (runner/internal/agent/codex). Any resulting auth failure surfaces
	// as a normal non-zero-exit error.
	APIKey string
	// Model is passed to the CLI as `--model <model>` AND returned verbatim as
	// the model identifier from Inference, so the recorded label matches the
	// model that actually ran. Codex's JSONL does not carry a model field today,
	// so the deterministic config value keeps the server's self-review guard
	// honest. Empty means inherit the host ~/.codex config default (no --model
	// flag is passed).
	Model string
	// ReasoningEffort is passed to the CLI as a `-c
	// model_reasoning_effort=<effort>` config override (e.g. low/medium/high).
	// Empty means inherit the host ~/.codex config (no override is passed).
	ReasoningEffort string
	// MaxTokens caps the response length. Reserved for parity with the SDK and
	// claudecode adapters; the `codex` CLI has no stable per-call max-tokens
	// flag, so it is currently advisory only.
	MaxTokens int
	// Timeout bounds a single inference call via context.WithTimeout. It is the
	// no-deadline fallback only: when the caller's ctx already carries a
	// deadline (the server's #747 size-aware review budget), that wins.
	Timeout time.Duration
	// MaxRetries bounds the in-adapter retry for a transient subprocess launch
	// crash (an external/OOM SIGKILL surfacing as *exec.ExitError, #620). It
	// counts RETRIES, not attempts: the loop runs MaxRetries+1 attempts total.
	// NewClient normalises a zero value to 1 — Go cannot distinguish an
	// explicit 0 from an unset field, so the constructor always defaults zero
	// to 1. To run a single attempt (retry disabled), set cfg.MaxRetries to 0
	// on the Client AFTER NewClient (as the tests do) rather than passing 0 to
	// NewClient. A per-attempt timeout and deterministic faults (binary-missing,
	// no-agent-message, bad-verdict) are never retried.
	MaxRetries int
}

// Client wraps the `codex` CLI for one-shot inference calls.
type Client struct {
	cfg Config
	// Cmd builds the *exec.Cmd. Defaults to exec.CommandContext; overridable
	// by tests to redirect to a fake binary.
	Cmd func(ctx context.Context, name string, args ...string) *exec.Cmd
}

// NewClient constructs a Client from cfg, defaulting Binary to DefaultBinary
// and Cmd to exec.CommandContext.
func NewClient(cfg Config) *Client {
	if cfg.Binary == "" {
		cfg.Binary = DefaultBinary
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 1
	}
	return &Client{cfg: cfg, Cmd: exec.CommandContext}
}

// codexEvent is one JSON event line from `codex exec --json` (pinned against
// codex-cli 0.137.0). The verdict body arrives on an `item.completed` line
// whose nested item has type `agent_message`; the per-turn token usage arrives
// on a `turn.completed` line. Both Item and Usage are pointers so a line that
// carries neither (e.g. `thread.started`, `turn.started`) decodes with them nil.
type codexEvent struct {
	Type  string      `json:"type"`
	Item  *codexItem  `json:"item"`
	Usage *usageBlock `json:"usage"`
}

// codexItem is the nested `item` object on an `item.completed` line. The
// review verdict body is the Text of the item whose Type is `agent_message`.
type codexItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// usageBlock is the shape of Codex's `usage` object on a `turn.completed`
// line (pinned against codex-cli 0.137.0):
//
//	{"type":"turn.completed","usage":{"input_tokens":N,"cached_input_tokens":N,
//	 "output_tokens":N,"reasoning_output_tokens":N}}
//
// cached_input_tokens is a (cheaper) SUBSET of input_tokens, so it is not added
// in — input_tokens already counts it. reasoning_output_tokens is a SEPARATE
// completion-side count (the hidden reasoning tokens, distinct from
// output_tokens which counts the visible message), so it IS added to the output
// side to avoid undercounting billable output. Identical accounting to the
// runner executor adapter (runner/internal/agent/codex/codex.go).
type usageBlock struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
}

// Inference runs `codex exec --json --skip-git-repo-check <prompt>`, scans the
// JSONL stream for the final agent-message text (the verdict body) and the
// summed per-turn token usage, and returns the verdict text as responseText,
// the configured model as the model identifier, and the token usage (#681). The
// child inherits os.Environ() so the operator's existing OPENAI_API_KEY or
// ChatGPT-login auth is used with zero new plumbing.
func (c *Client) Inference(ctx context.Context, prompt string) (responseText, model string, usage planreview.Usage, err error) {
	maxAttempts := c.cfg.MaxRetries + 1

	for attempt := 1; ; attempt++ {
		text, mdl, u, retryable, ierr := c.invokeOnce(ctx, prompt)
		if ierr == nil {
			return text, mdl, u, nil
		}
		// Stop on a non-retryable fault (binary-missing, a per-attempt
		// timeout, a no-agent-message/decode fault, or any non-*exec.ExitError
		// invocation failure), once the retry budget is spent, or when the
		// PARENT ctx is already done. The last diagnostic error is returned
		// verbatim so the plan-review WARN keeps its cause + elapsed + stderr
		// detail.
		if !retryable || attempt >= maxAttempts || ctx.Err() != nil {
			return "", "", planreview.Usage{}, ierr
		}
	}
}

// invokeOnce runs a single `codex` subprocess: it builds the command with a
// fresh per-attempt deadline (only when the incoming ctx carries none), captures
// stdout and stderr, scans the JSONL stream, and extracts the verdict text and
// usage. It returns retryable=true only for the transient crash class — an
// *exec.ExitError that is NOT a per-attempt timeout (an external/OOM SIGKILL). A
// timeout-kill and every deterministic fault return retryable=false so the loop
// fails fast. Mirrors claudecode.Client.invokeOnce.
func (c *Client) invokeOnce(ctx context.Context, prompt string) (responseText, model string, usage planreview.Usage, retryable bool, err error) {
	// Honour a caller-supplied deadline. The server computes a size-aware
	// per-invocation budget (#747) and applies it as a ctx deadline at the
	// review call site; capping it again with c.cfg.Timeout would defeat the
	// budget for large diffs. So only impose c.cfg.Timeout when the incoming
	// ctx carries NO deadline. The timeout-kill detection below keys off
	// ctx.Err()==DeadlineExceeded regardless of which deadline fired.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline && c.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.cfg.Timeout)
		defer cancel()
	}

	cmdFn := c.Cmd
	if cmdFn == nil {
		cmdFn = exec.CommandContext
	}

	// Workspace bound (#995): run the child from a fresh EMPTY scratch
	// directory. `codex exec` in non-interactive mode runs read-only shell
	// commands without approval, so a reviewer that decides to explore its cwd
	// reads whatever fishhawkd's inherited cwd holds (the repo checkout, in the
	// dogfood loop) across many turns — each turn re-sending the full growing
	// conversation (~400k input tokens per review). The prompt forbids tool use
	// but cannot prevent it; an empty cwd makes exploration fruitless. Review
	// invocations review the provided artifact and need no repo access —
	// --skip-git-repo-check already pins the non-repo-cwd execution posture.
	//
	// FAIL CLOSED on MkdirTemp error: a review that silently regains an
	// unbounded workspace defeats the bound, so the invocation errors instead
	// (the #955 per-invocation failure path degrades the advisory review
	// gracefully — terminal *_review_failed entry, loop continues).
	scratchDir, err := os.MkdirTemp("", "fishhawk-codex-review-")
	if err != nil {
		return "", "", planreview.Usage{}, false, fmt.Errorf("codex: create scratch workspace dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratchDir) }()

	// Flag rationale (pinned against codex-cli 0.137.0):
	//   exec                  — the non-interactive subcommand; reads the
	//                           prompt from the positional argument.
	//   --json                — emit one JSON event per line to stdout (the
	//                           stream this adapter parses).
	//   --skip-git-repo-check — Codex refuses to run outside a git repo by
	//                           default; cmd.Dir is a fresh non-repo scratch
	//                           dir (above), so this is required. A real run
	//                           from a non-repo dir returns cleanly without
	//                           the executor's --dangerously-bypass flags.
	//   --model <model>       — overrides the host ~/.codex config's model;
	//                           appended only when cfg.Model is set, so an empty
	//                           config inherits the host default.
	//   -c model_reasoning_effort=<effort>
	//                         — generic config override selecting the reasoning
	//                           effort; appended only when cfg.ReasoningEffort is
	//                           set. A bare value like `medium` that fails to
	//                           parse as TOML is treated as a string literal.
	// Both optional flags are placed BEFORE the prompt positional, matching the
	// existing argv shape.
	args := []string{
		"exec", "--json",
		"--skip-git-repo-check",
	}
	if c.cfg.Model != "" {
		args = append(args, "--model", c.cfg.Model)
	}
	if c.cfg.ReasoningEffort != "" {
		args = append(args, "-c", "model_reasoning_effort="+c.cfg.ReasoningEffort)
	}
	args = append(args, prompt)
	cmd := cmdFn(ctx, c.cfg.Binary, args...)
	cmd.Dir = scratchDir
	// Seed with os.Environ() when the Cmd builder left env nil (production), so
	// the operator's existing OPENAI_API_KEY / ChatGPT-login auth and PATH are
	// inherited. Then layer the configured API key on top. A subprocess's
	// os.Getenv returns the FIRST matching entry, so a plain append would be
	// shadowed by any inherited OPENAI_API_KEY — strip existing entries from the
	// seed before appending the configured one so cfg.APIKey actually wins. An
	// empty cfg.APIKey is skipped, leaving the inherited env untouched.
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	if c.cfg.APIKey != "" {
		cmd.Env = appendEnvOverride(cmd.Env, "OPENAI_API_KEY", c.cfg.APIKey)
	}
	// Capture stderr into our own buffer. Because cmd.Stderr is non-nil,
	// cmd.Output() no longer populates exitErr.Stderr — diagnostics are read
	// from this buffer, which survives even when a SIGKILLed child flushed
	// nothing to its own ExitError capture (the empty "signal: killed" in #620).
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	start := time.Now()
	out, runErr := cmd.Output()
	elapsed := time.Since(start)
	if runErr != nil {
		if isBinaryMissing(runErr) {
			return "", "", planreview.Usage{}, false, fmt.Errorf("codex: binary not found: %s", c.cfg.Binary)
		}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			stderrText := strings.TrimSpace(stderr.String())
			// A per-attempt context deadline kills the child and leaves
			// ctx.Err()==DeadlineExceeded: a slow review, not a launch crash.
			// Label it and do NOT retry — retrying a timeout would compound the
			// wait (#606). External/OOM kills (ctx.Err()==nil) are the transient
			// #620 class and retry.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return "", "", planreview.Usage{}, false, fmt.Errorf("codex: codex killed after %s (timeout): %v%s", elapsed, runErr, stderrSuffix(stderrText))
			}
			return "", "", planreview.Usage{}, true, fmt.Errorf("codex: codex killed after %s (external/OOM): %v%s", elapsed, runErr, stderrSuffix(stderrText))
		}
		return "", "", planreview.Usage{}, false, fmt.Errorf("codex: codex invocation failed: %w", runErr)
	}

	verdictText, usageOut, parseErr := parseStream(out)
	if parseErr != nil {
		return "", "", planreview.Usage{}, false, parseErr
	}

	// cfg.Model is truthful as the model identifier: when set it was passed via
	// --model above, and when empty the host default ran (reported as ""). If
	// codex's JSONL ever surfaces a model field, prefer it here — the 0.137.0
	// stream has no such field, so the config value is the only label source.
	return verdictText, c.cfg.Model, usageOut, false, nil
}

// parseStream scans the `codex exec --json` stdout and returns the final
// agent-message text (the verdict body) and the summed token usage. Non-JSON
// lines (Codex interleaves the occasional human-readable log line on stdout) are
// skipped fail-open rather than failing the whole review. An absence of any
// agent_message line is a hard error: there is no verdict to decode, distinct
// from the graceful Known=false degradation when only the usage line is missing.
func parseStream(out []byte) (verdictText string, usage planreview.Usage, err error) {
	var (
		haveMessage bool
		haveUsage   bool
		inputTokens int
		cachedTok   int
		outputTok   int
		turns       int
	)
	for _, line := range bytes.Split(out, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		var ev codexEvent
		if jerr := json.Unmarshal(trimmed, &ev); jerr != nil {
			// Non-JSON log line — skip it rather than failing the review.
			continue
		}
		switch ev.Type {
		case "item.completed":
			// The verdict body is the LAST agent_message text: a multi-turn run
			// may emit several, and the final one is the model's conclusion.
			if ev.Item != nil && ev.Item.Type == "agent_message" {
				verdictText = ev.Item.Text
				haveMessage = true
			}
		case "turn.completed":
			// Codex reports usage PER TURN, so SUM across turns rather than
			// last-wins. input_tokens already includes cached_input_tokens
			// (which is summed separately so the cached split stays visible,
			// #995); reasoning_output_tokens is added to the output side. The
			// turn count makes a multi-turn agentic blowup observable.
			turns++
			if ev.Usage != nil {
				inputTokens += ev.Usage.InputTokens
				cachedTok += ev.Usage.CachedInputTokens
				outputTok += ev.Usage.OutputTokens + ev.Usage.ReasoningOutputTokens
				haveUsage = true
			}
		}
	}

	if !haveMessage {
		return "", planreview.Usage{}, fmt.Errorf("codex: no agent_message found in codex output")
	}

	// Surface token usage from the summed turn.completed lines. No usage line
	// leaves Known=false → the server degrades to a usd=0 record rather than
	// guessing (#681/#682).
	if haveUsage {
		usage = planreview.Usage{
			InputTokens:       inputTokens,
			CachedInputTokens: cachedTok,
			OutputTokens:      outputTok,
			Turns:             turns,
			Known:             true,
		}
	}
	return verdictText, usage, nil
}

// stderrSuffix formats captured child stderr as a trailing ": <text>" clause
// for a diagnostic error, or the empty string when nothing was captured.
// appendEnvOverride returns env with every existing "key=" entry removed and a
// single "key=value" appended. A subprocess resolves a variable to the FIRST
// matching entry, so a plain append would be shadowed by an inherited value;
// stripping first guarantees the override actually takes effect.
func appendEnvOverride(env []string, key, value string) []string {
	prefix := key + "="
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, prefix+value)
}

func stderrSuffix(stderrText string) string {
	if stderrText == "" {
		return ""
	}
	return ": " + stderrText
}

// isBinaryMissing reports whether err means the binary itself is not on disk /
// not on PATH, as opposed to a runtime failure. Mirrors
// claudecode.isBinaryMissing.
func isBinaryMissing(err error) bool {
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	return strings.Contains(err.Error(), "executable file not found") ||
		strings.Contains(err.Error(), "no such file or directory")
}
