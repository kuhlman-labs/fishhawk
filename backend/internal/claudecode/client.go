// Package claudecode provides a local-mode PlanReviewer adapter that spawns
// the `claude` CLI as a subprocess for inference-only plan review. It is the
// local-mode sibling of backend/internal/anthropic (the production SDK
// adapter, #572): instead of provisioning FISHHAWKD_ANTHROPIC_API_KEY, a
// dogfood operator reuses their existing Claude Code setup (subscription auth
// or an ANTHROPIC_API_KEY already in the environment), which the subprocess
// inherits via os.Environ().
//
// The adapter shells out to `claude --print --output-format json --model
// <model> -p <prompt>` and decodes the single JSON envelope the CLI emits.
// cmd.Output() captures the whole response in one buffer, sidestepping the
// StdoutPipe read race the streaming adapter must handle.
//
// The adapter runs one of two postures per invocation (#2486):
//
// BOTH postures pin an EMPTY MCP server set (--strict-mcp-config plus
// --mcp-config {"mcpServers":{}}), posture-independent (#2524): --tools bounds
// only the BUILT-IN toolset, so operator-configured MCP tools (browser, Gmail,
// GitHub) load from the operator's config regardless of grounding. Pinning them
// off in both postures means the child's MCP exposure does not depend on the
// CLI's default-deny permission behavior staying the default.
//
//   - UNGROUNDED (treeDir == ""): the diff-only posture. The child runs from a
//     fresh EMPTY scratch directory — NOT the process working directory. This
//     adapter used to leave cmd.Dir unset, so the reviewer inherited fishhawkd's
//     cwd (the operator's live checkout, including uncommitted changes, .env, and
//     .git); read-only built-in tools need no permission prompt, so an ungrounded
//     reviewer was accidentally grounded against whatever directory the daemon
//     sat in. Pinning an empty scratch cwd makes ungrounded genuinely diff-only.
//     The empty-MCP pin applies here too (#2524).
//   - GROUNDED (treeDir != ""): the child runs from the caller-supplied EXPORTED
//     read-only tree (reviewsandbox.ExportTree — tracked files at one commit, no
//     .git) with --add-dir <tree>, and the toolset restricted to Read,Grep,Glob
//     via --tools plus --allowed-tools so print mode never hits an unanswerable
//     permission prompt. --add-dir/--tools/--allowed-tools are the grounded
//     tree-read grant; the empty-MCP pin above is shared with the ungrounded
//     posture, not grounded-specific. Neither posture passes
//     --dangerously-skip-permissions or --permission-mode bypassPermissions.
//
// Either posture seeds the child environment from reviewsandbox.Env — the
// enumerated ClaudeAllow list plus the operator passthrough — never a wholesale
// os.Environ(), so a tool-enabled reviewer never holds the daemon's secrets.
package claudecode

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
	"github.com/kuhlman-labs/fishhawk/backend/internal/procgroup"
	"github.com/kuhlman-labs/fishhawk/backend/internal/reviewsandbox"
)

// DefaultBinary is the executable name resolved against PATH when
// Config.Binary is empty.
const DefaultBinary = "claude"

// killGrace is the procgroup.Harden WaitDelay applied to every claude
// subprocess: after the review deadline fires and the whole-group SIGKILL runs,
// os/exec waits at most this long for the process and its inherited pipe fds to
// close before force-closing the parent-side descriptors so cmd.Output()
// returns. It bounds the residual hang from a group member that escaped the
// kill (#1805). A var, not a const, so timing-sensitive tests can shorten it.
var killGrace = 5 * time.Second

// Config holds the settings needed to spawn the `claude` CLI for inference.
type Config struct {
	// Binary is the executable name or absolute path. Empty means
	// DefaultBinary.
	Binary string
	// Model is the model identifier passed to `claude --model`. It is also
	// returned verbatim as the model identifier from Inference because the
	// CLI's JSON envelope does not reliably echo the model, and a
	// deterministic model string keeps the server's self-review guard honest.
	Model string
	// MaxTokens caps the response length. Reserved for parity with the SDK
	// adapter; the `claude` CLI has no stable per-call max-tokens flag, so it
	// is currently advisory only.
	MaxTokens int
	// Timeout bounds a single inference call via context.WithTimeout.
	Timeout time.Duration
	// MaxRetries bounds the in-adapter retry for a transient subprocess
	// launch crash (an external/OOM SIGKILL surfacing as *exec.ExitError,
	// #620). It counts RETRIES, not attempts: the loop runs MaxRetries+1
	// attempts total. NewClient normalises a zero value to 1 — Go cannot
	// distinguish an explicit 0 from an unset field, so the constructor
	// always defaults zero to 1. To run a single attempt (retry disabled),
	// set cfg.MaxRetries to 0 on the Client AFTER NewClient (as the tests
	// do) rather than passing 0 to NewClient. A per-attempt timeout (a slow
	// review, #606) and deterministic faults (binary-missing,
	// envelope-decode, bad verdict) are never retried.
	MaxRetries int
	// EnvPassthrough is the operator-configured list of EXACT environment
	// variable names appended to reviewsandbox.ClaudeAllow when scrubbing the
	// child environment (#2486) — the documented escape hatch for a deployment
	// whose auth needs a variable the minimal allow-list omits (Bedrock/Vertex
	// routing, an unusual proxy/CA-bundle var), named explicitly rather than
	// admitted by a prefix. Empty is the default — only the allow-list survives.
	EnvPassthrough []string
}

// Client wraps the `claude` CLI for one-shot inference calls.
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

// cliEnvelope is the JSON document `claude --print --output-format json`
// emits: a single object whose response text lives in the top-level `result`
// field and whose token usage lives in the top-level `usage` object (#681).
type cliEnvelope struct {
	Type    string    `json:"type"`
	Subtype string    `json:"subtype"`
	IsError bool      `json:"is_error"`
	Result  string    `json:"result"`
	Usage   *cliUsage `json:"usage"`
}

// cliUsage is the token usage object the CLI envelope carries. It is a
// pointer on cliEnvelope so a pre-usage / malformed envelope (no `usage`
// key) decodes as nil and the adapter reports Known=false rather than a
// spurious zero-token figure.
// The cache fields were verified against a live `claude --print
// --output-format json` envelope (#995): `input_tokens` EXCLUDES cache reads
// and writes, which arrive as the separate `cache_read_input_tokens` /
// `cache_creation_input_tokens` members — so the envelope already satisfies
// the normalized cache-EXCLUSIVE planreview.Usage contract (#1010) with no
// boundary arithmetic. An envelope that omits them decodes to 0 — harmless.
type cliUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// Inference runs `claude --print --output-format json --model <model> -p
// <prompt>`, decodes the CLI envelope, and returns the envelope's `result`
// field as responseText, the configured model as the model identifier, and
// the envelope's token usage (#681). The child inherits os.Environ() so the
// operator's existing ANTHROPIC_API_KEY or subscription auth is used with
// zero new plumbing.
func (c *Client) Inference(ctx context.Context, prompt string) (responseText, model string, usage planreview.Usage, err error) {
	return c.InferenceInTree(ctx, prompt, "")
}

// InferenceInTree is Inference with an optional grounding tree (#2486). When
// treeDir is non-empty the child runs from that exported read-only tree with the
// toolset restricted to Read,Grep,Glob (the GROUNDED posture); when empty it runs
// from a fresh empty scratch dir with no tool grant (the UNGROUNDED, genuinely
// diff-only posture). Inference delegates here with an empty treeDir so the two
// postures share one code path.
func (c *Client) InferenceInTree(ctx context.Context, prompt, treeDir string) (responseText, model string, usage planreview.Usage, err error) {
	maxAttempts := c.cfg.MaxRetries + 1

	for attempt := 1; ; attempt++ {
		text, mdl, u, retryable, ierr := c.invokeOnce(ctx, prompt, treeDir)
		if ierr == nil {
			return text, mdl, u, nil
		}
		// Stop on a non-retryable fault (binary-missing, a per-attempt
		// timeout, an envelope-decode/bad-verdict fault, or any non-
		// *exec.ExitError invocation failure), once the retry budget is
		// spent, or when the PARENT ctx is already done — an outer
		// cancellation or deadline (ctx.Err() != nil), distinct from the
		// per-attempt timeout invokeOnce derives internally. The last
		// diagnostic error is returned verbatim so the plan-review WARN
		// keeps its cause + elapsed + stderr detail.
		if !retryable || attempt >= maxAttempts || ctx.Err() != nil {
			return "", "", planreview.Usage{}, ierr
		}
	}
}

// invokeOnce runs a single `claude` subprocess: it builds the command with a
// fresh per-attempt deadline, captures stdout and stderr, decodes the CLI
// envelope, and validates it. It returns retryable=true only for the transient
// crash class — an *exec.ExitError that is NOT a per-attempt timeout (an
// external/OOM SIGKILL). A timeout-kill (a slow review, #606) and every
// deterministic fault return retryable=false so the loop fails fast.
func (c *Client) invokeOnce(ctx context.Context, prompt, treeDir string) (responseText, model string, usage planreview.Usage, retryable bool, err error) {
	// Honour a caller-supplied deadline. The server now computes a size-aware
	// per-invocation budget (#747) and applies it as a ctx deadline at the
	// review call site; capping it again with c.cfg.Timeout would defeat the
	// budget for large diffs. So only impose c.cfg.Timeout when the incoming
	// ctx carries NO deadline — preserving today's fixed-timeout fallback for
	// no-deadline callers while letting the server's deadline win when set.
	// The timeout-kill detection below keys off ctx.Err()==DeadlineExceeded
	// regardless of which deadline fired, so it stays correct either way.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline && c.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.cfg.Timeout)
		defer cancel()
	}

	cmdFn := c.Cmd
	if cmdFn == nil {
		cmdFn = exec.CommandContext
	}

	// Workspace posture (#2486). In BOTH postures cmd.Dir is set to a directory
	// we control — NEVER left unset (which would inherit fishhawkd's cwd, the
	// operator's live checkout, and accidentally ground a diff-only review). The
	// GROUNDED case runs from the caller-owned exported tree (removed by the
	// caller when the loop returns — this path creates/removes NOTHING). The
	// UNGROUNDED case runs from a fresh EMPTY scratch dir created and removed
	// here. FAIL CLOSED on MkdirTemp error in the ungrounded case rather than
	// silently falling back to the process cwd.
	workDir := treeDir
	if workDir == "" {
		scratchDir, serr := os.MkdirTemp("", "fishhawk-claude-review-")
		if serr != nil {
			return "", "", planreview.Usage{}, false, fmt.Errorf("claudecode: create scratch workspace dir: %w", serr)
		}
		defer func() { _ = os.RemoveAll(scratchDir) }()
		workDir = scratchDir
	}

	args := []string{
		"--print",
		"--output-format", "json",
		"--model", c.cfg.Model,
	}
	// Empty-MCP pin, applied in BOTH postures (#2524, refining #2486). --tools
	// (below) bounds only the BUILT-IN toolset; MCP tools are not built-ins, so
	// they load from the operator's config regardless of posture (verified live:
	// a grounded child enumerated GitHub/Gmail/browser MCP tools despite --tools
	// Read,Grep,Glob). --strict-mcp-config makes --mcp-config the sole source of
	// MCP config (ignoring the operator's ~/.claude and project .mcp.json), and
	// the empty {"mcpServers":{}} document loads zero MCP servers.
	//
	// This was previously applied only inside the grounded (treeDir != "") block,
	// so the UNGROUNDED (diff-only) posture — the shipping posture, since grounding
	// ships dormant (#2522) — omitted it and the child still loaded the operator's
	// MCP servers. In ungrounded print mode with no --allowed-tools, MCP tool
	// INVOCATION was already permission-denied (measured on #2524), so this is
	// defense-in-depth: it removes MCP tools from the enumeration the child sees,
	// and removes the dependence on the CLI's default-deny permission behavior
	// staying the default — a CLI change that auto-approves MCP tools, or a
	// deployment that pre-approves them in settings, would otherwise turn a listing
	// into an egress channel with no change on our side. Pinned against Claude
	// Code 2.1.224.
	args = append(args,
		"--strict-mcp-config",
		"--mcp-config", `{"mcpServers":{}}`,
	)
	// Grounded posture (#2486): grant the reviewer read+search of the exported
	// tree, restrict the AVAILABLE built-in toolset to Read,Grep,Glob (no Bash,
	// no Write/Edit, no WebFetch/WebSearch) via --tools, and pre-approve exactly
	// those via --allowed-tools so print mode never reaches an unanswerable
	// permission prompt. Never --dangerously-skip-permissions and never
	// --permission-mode bypassPermissions. --add-dir/--tools/--allowed-tools are
	// the grounded-only tree-read grant; the MCP pin above is posture-independent.
	if treeDir != "" {
		args = append(args,
			"--add-dir", treeDir,
			"--tools", "Read,Grep,Glob",
			"--allowed-tools", "Read", "Grep", "Glob",
		)
	}
	args = append(args, "-p", prompt)
	cmd := cmdFn(ctx, c.cfg.Binary, args...)
	// Harden the subprocess so the review deadline actually terminates a wedged
	// reviewer (#1805): the child leads its own process group, a deadline-fired
	// cancel SIGKILLs the whole group (reaping a grandchild that inherited the
	// stdout pipe), and WaitDelay force-closes the parent-side pipe fd if a group
	// member escaped the kill. Must be applied after the cmd is built and before
	// cmd.Output().
	procgroup.Harden(cmd, killGrace)
	cmd.Dir = workDir
	// Seed the child environment from the SCRUBBED allow-list, not a wholesale
	// os.Environ() (#2486): a tool-enabled reviewer processing untrusted diff/issue
	// text must never hold FISHHAWKD_DATABASE_URL, GITHUB_TOKEN, or unrelated API
	// keys. reviewsandbox.Env keeps only ClaudeAllow ∪ the operator passthrough
	// (model reachability, host auth-config discovery, corporate egress). The
	// `if cmd.Env == nil` seam lets tests inject env.
	if cmd.Env == nil {
		cmd.Env = reviewsandbox.Env(os.Environ(), reviewsandbox.ClaudeAllow, c.cfg.EnvPassthrough)
	}
	// Capture stderr into our own buffer. Because cmd.Stderr is now non-nil,
	// cmd.Output() no longer populates exitErr.Stderr — so diagnostics must
	// be read from this buffer, which survives even when a SIGKILLed child
	// flushed nothing to its own ExitError capture (the empty "signal:
	// killed:" string in #620).
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	start := time.Now()
	out, err := cmd.Output()
	elapsed := time.Since(start)
	if err != nil {
		if isBinaryMissing(err) {
			return "", "", planreview.Usage{}, false, fmt.Errorf("claudecode: binary not found: %s", c.cfg.Binary)
		}
		stderrText := strings.TrimSpace(stderr.String())
		// A context deadline (the #747 size-aware review budget applied at the
		// server call site, or the internal per-attempt fallback) fired and
		// killed the review: a slow/wedged review, not a launch crash. This
		// check is HOISTED above the *exec.ExitError type gate because the
		// procgroup.Harden group-kill / WaitDelay termination (#1805) can force
		// cmd.Output() to return a NON-ExitError (e.g. context.DeadlineExceeded
		// when the direct child already exited but an escaped grandchild held
		// the pipe) — the old in-branch check dropped the timeout label on that
		// path. Keying off ctx.Err() keeps the label correct on every return
		// type. Do NOT retry — retrying a 300s timeout would compound into a
		// 600s wait (#606).
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", "", planreview.Usage{}, false, fmt.Errorf("claudecode: claude killed after %s (timeout): %v%s", elapsed, err, stderrSuffix(stderrText))
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// An external/OOM SIGKILL (ctx.Err()==nil) is the transient #620
			// class — retry.
			return "", "", planreview.Usage{}, true, fmt.Errorf("claudecode: claude killed after %s (external/OOM): %v%s", elapsed, err, stderrSuffix(stderrText))
		}
		return "", "", planreview.Usage{}, false, fmt.Errorf("claudecode: claude invocation failed: %w", err)
	}

	var env cliEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		return "", "", planreview.Usage{}, false, fmt.Errorf("claudecode: decode CLI envelope: %w", err)
	}
	if env.IsError {
		return "", "", planreview.Usage{}, false, fmt.Errorf("claudecode: claude reported error envelope (subtype=%q)", env.Subtype)
	}

	// Surface token usage from the envelope's `usage` object. A pre-usage or
	// malformed envelope (no `usage` key) leaves env.Usage nil → Known=false,
	// so the server degrades to a usd=0 record rather than guessing (#681).
	var usageOut planreview.Usage
	if env.Usage != nil {
		usageOut = planreview.Usage{
			// The envelope's input_tokens is already cache-exclusive
			// (Anthropic accounting), so it passes through unchanged as the
			// normalized fresh count (#1010). The cache members are carried
			// SEPARATELY into the read/write split (#1343) rather than summed:
			// cache_read_input_tokens → CacheReadInputTokens (cheaper),
			// cache_creation_input_tokens → CacheWriteInputTokens (premium).
			InputTokens:           env.Usage.InputTokens,
			CacheReadInputTokens:  env.Usage.CacheReadInputTokens,
			CacheWriteInputTokens: env.Usage.CacheCreationInputTokens,
			OutputTokens:          env.Usage.OutputTokens,
			Turns:                 1, // single-shot --print: exactly one turn
			Known:                 true,
		}
	}

	return env.Result, c.cfg.Model, usageOut, false, nil
}

// stderrSuffix formats captured child stderr as a trailing ": <text>" clause
// for a diagnostic error, or the empty string when nothing was captured.
func stderrSuffix(stderrText string) string {
	if stderrText == "" {
		return ""
	}
	return ": " + stderrText
}

// isBinaryMissing reports whether err means the binary itself is not on disk /
// not on PATH, as opposed to a runtime failure. Cribbed from the runner's
// claudecode adapter: exec.ErrNotFound is the canonical case but the
// underlying syscall message varies by platform, so the substring match is a
// pragmatic fallback.
func isBinaryMissing(err error) bool {
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	return strings.Contains(err.Error(), "executable file not found") ||
		strings.Contains(err.Error(), "no such file or directory")
}
