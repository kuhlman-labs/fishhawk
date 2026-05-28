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

	out, err := cmd.Output()
	if err != nil {
		if isBinaryMissing(err) {
			return "", "", fmt.Errorf("claudecode: binary not found: %s", c.cfg.Binary)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", "", fmt.Errorf("claudecode: claude exited non-zero: %v: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", "", fmt.Errorf("claudecode: claude invocation failed: %w", err)
	}

	var env cliEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		return "", "", fmt.Errorf("claudecode: decode CLI envelope: %w", err)
	}
	if env.IsError {
		return "", "", fmt.Errorf("claudecode: claude reported error envelope (subtype=%q)", env.Subtype)
	}

	return env.Result, c.cfg.Model, nil
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
