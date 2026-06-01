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
// Unlike the runner's streaming claudecode adapter, this is inference-only:
// the review prompt forbids tool use, so there is no --verbose/stream-json,
// no --dangerously-skip-permissions, and no --add-dir. cmd.Output() captures
// the whole response in one buffer, sidestepping the StdoutPipe read race the
// streaming adapter must handle.
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
)

// DefaultBinary is the executable name resolved against PATH when
// Config.Binary is empty.
const DefaultBinary = "claude"

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
	// attempts total. The default (1) is set at construction in NewClient;
	// an explicit 0 disables retry deterministically so tests can pin a
	// single attempt. A per-attempt timeout (a slow review, #606) and
	// deterministic faults (binary-missing, envelope-decode, bad verdict)
	// are never retried.
	MaxRetries int
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
// field.
type cliEnvelope struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
}

// Inference runs `claude --print --output-format json --model <model> -p
// <prompt>`, decodes the CLI envelope, and returns the envelope's `result`
// field as responseText plus the configured model as the model identifier.
// The child inherits os.Environ() so the operator's existing ANTHROPIC_API_KEY
// or subscription auth is used with zero new plumbing.
func (c *Client) Inference(ctx context.Context, prompt string) (responseText, model string, err error) {
	maxAttempts := c.cfg.MaxRetries + 1

	for attempt := 1; ; attempt++ {
		text, mdl, retryable, ierr := c.invokeOnce(ctx, prompt)
		if ierr == nil {
			return text, mdl, nil
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
			return "", "", ierr
		}
	}
}

// invokeOnce runs a single `claude` subprocess: it builds the command with a
// fresh per-attempt deadline, captures stdout and stderr, decodes the CLI
// envelope, and validates it. It returns retryable=true only for the transient
// crash class — an *exec.ExitError that is NOT a per-attempt timeout (an
// external/OOM SIGKILL). A timeout-kill (a slow review, #606) and every
// deterministic fault return retryable=false so the loop fails fast.
func (c *Client) invokeOnce(ctx context.Context, prompt string) (responseText, model string, retryable bool, err error) {
	if c.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.cfg.Timeout)
		defer cancel()
	}

	cmdFn := c.Cmd
	if cmdFn == nil {
		cmdFn = exec.CommandContext
	}

	args := []string{
		"--print",
		"--output-format", "json",
		"--model", c.cfg.Model,
		"-p", prompt,
	}
	cmd := cmdFn(ctx, c.cfg.Binary, args...)
	if cmd.Env == nil {
		cmd.Env = os.Environ()
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
			return "", "", false, fmt.Errorf("claudecode: binary not found: %s", c.cfg.Binary)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderrText := strings.TrimSpace(stderr.String())
			// A per-attempt context deadline kills the child and leaves
			// ctx.Err()==DeadlineExceeded: a slow review, not a launch
			// crash. Label it and do NOT retry — retrying a 300s timeout
			// would compound into a 600s wait (#606). External/OOM kills
			// (ctx.Err()==nil) are the transient #620 class and retry.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return "", "", false, fmt.Errorf("claudecode: claude killed after %s (timeout): %v%s", elapsed, err, stderrSuffix(stderrText))
			}
			return "", "", true, fmt.Errorf("claudecode: claude killed after %s (external/OOM): %v%s", elapsed, err, stderrSuffix(stderrText))
		}
		return "", "", false, fmt.Errorf("claudecode: claude invocation failed: %w", err)
	}

	var env cliEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		return "", "", false, fmt.Errorf("claudecode: decode CLI envelope: %w", err)
	}
	if env.IsError {
		return "", "", false, fmt.Errorf("claudecode: claude reported error envelope (subtype=%q)", env.Subtype)
	}

	return env.Result, c.cfg.Model, false, nil
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
