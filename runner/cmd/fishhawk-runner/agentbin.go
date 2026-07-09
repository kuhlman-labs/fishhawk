package main

import (
	"context"
	"os"
	"os/exec"
	"strings"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent/claudecode"
	"github.com/kuhlman-labs/fishhawk/runner/internal/agent/codex"
)

// agentBinaryEnvVar returns the operator env var that overrides the
// agent CLI binary for the given provider id. The codex executor reads
// FISHHAWK_CODEX_BIN; every other provider (the claude-code default)
// reads FISHHAWK_AGENT_BIN. These let an operator pin a known-good CLI
// build without touching a global PATH symlink (#1741).
func agentBinaryEnvVar(agentID string) string {
	if agentID == "codex" {
		return "FISHHAWK_CODEX_BIN"
	}
	return "FISHHAWK_AGENT_BIN"
}

// agentBinaryOverride returns the trimmed operator binary override for
// the provider, or "" when unset. A whitespace-only value is treated as
// unset so an empty export never shadows the adapter DefaultBinary. The
// call site threads the same "" into the invoker, whose empty .Binary
// resolves to the adapter DefaultBinary — so an unset override preserves
// the historical PATH-resolution path exactly.
func agentBinaryOverride(agentID string, getenv func(string) string) string {
	return strings.TrimSpace(getenv(agentBinaryEnvVar(agentID)))
}

// effectiveAgentBinary resolves the executable the runner will invoke
// for the provider: the operator override when set, else the adapter
// DefaultBinary (claudecode.DefaultBinary / codex.DefaultBinary). This
// is the value probed for a version and recorded on the runner_started
// line, so the log names the exact binary that was probed and invoked.
// An unknown id (rejected by selectInvoker before invocation) falls back
// to the claude-code default, matching apiKeyForAgent's default arm.
func effectiveAgentBinary(agentID string, getenv func(string) string) string {
	if override := agentBinaryOverride(agentID, getenv); override != "" {
		return override
	}
	if agentID == "codex" {
		return codex.DefaultBinary
	}
	return claudecode.DefaultBinary
}

// probeVersionCmd is the exec seam for the version-probe subprocess.
// Defaults to exec.CommandContext; tests swap it to redirect to a canned
// stub without standing up the real CLI.
var probeVersionCmd = exec.CommandContext

// probeAgentVersion is the swappable seam that reports the agent CLI's
// self-declared version by running `<binary> --version`, returning the
// trimmed first non-empty stdout line. It returns "unknown" on any error
// (empty binary, missing binary, non-zero exit, or no output) so a CLI
// without a --version flag degrades to "unknown" rather than failing the
// run. It is best-effort observability stamped once into runner_started
// at process start, never on a per-agent-turn hot path (#1741).
//
// The subprocess ALWAYS runs with its working directory set to a
// private, freshly created temp dir (removed on return), NEVER the
// runner's cwd or the working tree: a PATH-resolved binary that writes a
// relative file on invocation (e.g. an e2e fake `claude` script) must
// not drop that file into the checkout, where the pre-push scope gate
// would flag it as an out-of-scope creation.
var probeAgentVersion = func(ctx context.Context, binary string) string {
	if strings.TrimSpace(binary) == "" {
		return "unknown"
	}
	dir, err := os.MkdirTemp("", "fishhawk-agentprobe-")
	if err != nil {
		return "unknown"
	}
	defer func() { _ = os.RemoveAll(dir) }()

	cmd := probeVersionCmd(ctx, binary, "--version")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	for _, line := range strings.Split(string(out), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return "unknown"
}
