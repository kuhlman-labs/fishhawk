package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent/claudecode"
	"github.com/kuhlman-labs/fishhawk/runner/internal/agent/codex"
)

// TestAgentBinaryEnvVar pins the per-provider override env var: codex
// reads FISHHAWK_CODEX_BIN, everything else (the claude-code default and
// any unknown id) reads FISHHAWK_AGENT_BIN.
func TestAgentBinaryEnvVar(t *testing.T) {
	cases := map[string]string{
		"codex":       "FISHHAWK_CODEX_BIN",
		"claude-code": "FISHHAWK_AGENT_BIN",
		"unknown":     "FISHHAWK_AGENT_BIN",
	}
	for id, want := range cases {
		if got := agentBinaryEnvVar(id); got != want {
			t.Errorf("agentBinaryEnvVar(%q) = %q, want %q", id, got, want)
		}
	}
}

// TestAgentBinaryOverride covers the override resolution: a set value is
// trimmed and returned; an unset var yields ""; a whitespace-only value
// is treated as unset so an empty export never shadows the default.
func TestAgentBinaryOverride(t *testing.T) {
	t.Run("claude-code set is trimmed", func(t *testing.T) {
		t.Setenv("FISHHAWK_AGENT_BIN", "  /opt/claude-good  ")
		if got := agentBinaryOverride("claude-code", os.Getenv); got != "/opt/claude-good" {
			t.Errorf("agentBinaryOverride(claude-code) = %q, want /opt/claude-good", got)
		}
	})
	t.Run("codex set is trimmed", func(t *testing.T) {
		t.Setenv("FISHHAWK_CODEX_BIN", "/opt/codex-good")
		if got := agentBinaryOverride("codex", os.Getenv); got != "/opt/codex-good" {
			t.Errorf("agentBinaryOverride(codex) = %q, want /opt/codex-good", got)
		}
	})
	t.Run("unset yields empty", func(t *testing.T) {
		if got := agentBinaryOverride("claude-code", func(string) string { return "" }); got != "" {
			t.Errorf("agentBinaryOverride(unset) = %q, want empty", got)
		}
	})
	t.Run("whitespace-only is treated as unset", func(t *testing.T) {
		if got := agentBinaryOverride("claude-code", func(string) string { return "   \t " }); got != "" {
			t.Errorf("agentBinaryOverride(whitespace) = %q, want empty", got)
		}
	})
}

// TestEffectiveAgentBinary asserts the override-or-default resolution for
// both providers: a set override wins; an unset override falls back to
// the adapter DefaultBinary, proving the historical PATH-resolution path
// is preserved.
func TestEffectiveAgentBinary(t *testing.T) {
	t.Run("claude-code override wins", func(t *testing.T) {
		getenv := func(k string) string {
			if k == "FISHHAWK_AGENT_BIN" {
				return "/opt/claude-good"
			}
			return ""
		}
		if got := effectiveAgentBinary("claude-code", getenv); got != "/opt/claude-good" {
			t.Errorf("effectiveAgentBinary(claude-code, override) = %q, want /opt/claude-good", got)
		}
	})
	t.Run("claude-code default when unset", func(t *testing.T) {
		if got := effectiveAgentBinary("claude-code", func(string) string { return "" }); got != claudecode.DefaultBinary {
			t.Errorf("effectiveAgentBinary(claude-code, unset) = %q, want %q", got, claudecode.DefaultBinary)
		}
	})
	t.Run("codex override wins", func(t *testing.T) {
		getenv := func(k string) string {
			if k == "FISHHAWK_CODEX_BIN" {
				return "/opt/codex-good"
			}
			return ""
		}
		if got := effectiveAgentBinary("codex", getenv); got != "/opt/codex-good" {
			t.Errorf("effectiveAgentBinary(codex, override) = %q, want /opt/codex-good", got)
		}
	})
	t.Run("codex default when unset", func(t *testing.T) {
		if got := effectiveAgentBinary("codex", func(string) string { return "" }); got != codex.DefaultBinary {
			t.Errorf("effectiveAgentBinary(codex, unset) = %q, want %q", got, codex.DefaultBinary)
		}
	})
}

// TestProbeAgentVersion_Success swaps the probeVersionCmd seam for a
// canned command that prints a multi-line version banner, and asserts
// probeAgentVersion returns the trimmed FIRST non-empty line.
func TestProbeAgentVersion_Success(t *testing.T) {
	orig := probeVersionCmd
	probeVersionCmd = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		// A leading blank line proves the "first NON-EMPTY line" contract.
		return exec.CommandContext(ctx, "printf", "\n  claude 2.1.205 \nextra\n")
	}
	t.Cleanup(func() { probeVersionCmd = orig })

	if got := probeAgentVersion(context.Background(), "claude"); got != "claude 2.1.205" {
		t.Errorf("probeAgentVersion = %q, want %q", got, "claude 2.1.205")
	}
}

// TestProbeAgentVersion_RunsInPrivateTempDir is the binding recovery
// assertion: the probe subprocess MUST run with cmd.Dir set to a private
// directory outside the working tree, so a PATH-resolved binary that
// writes a relative file (e.g. an e2e fake `claude` script writing
// added.txt) cannot drop it into the checkout where the pre-push scope
// gate would flag it as an out-of-scope creation.
func TestProbeAgentVersion_RunsInPrivateTempDir(t *testing.T) {
	var captured *exec.Cmd
	orig := probeVersionCmd
	probeVersionCmd = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		captured = exec.CommandContext(ctx, "printf", "1.2.3\n")
		return captured
	}
	t.Cleanup(func() { probeVersionCmd = orig })

	if got := probeAgentVersion(context.Background(), "claude"); got != "1.2.3" {
		t.Fatalf("probeAgentVersion = %q, want 1.2.3", got)
	}
	if captured == nil {
		t.Fatal("probeVersionCmd seam was never invoked")
	}
	if strings.TrimSpace(captured.Dir) == "" {
		t.Error("probe command Dir is empty; a PATH-resolved binary's relative writes would land in the runner cwd")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if captured.Dir == cwd || strings.HasPrefix(captured.Dir, cwd+string(os.PathSeparator)) {
		t.Errorf("probe command Dir %q is inside the working tree %q; relative writes could escape into the checkout", captured.Dir, cwd)
	}
}

// TestProbeAgentVersion_EmptyBinary asserts the empty-binary guard: no
// subprocess is spawned and the probe degrades to "unknown".
func TestProbeAgentVersion_EmptyBinary(t *testing.T) {
	orig := probeVersionCmd
	spawned := false
	probeVersionCmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		spawned = true
		return exec.CommandContext(ctx, name, args...)
	}
	t.Cleanup(func() { probeVersionCmd = orig })

	if got := probeAgentVersion(context.Background(), "   "); got != "unknown" {
		t.Errorf("probeAgentVersion(whitespace) = %q, want unknown", got)
	}
	if spawned {
		t.Error("probeAgentVersion spawned a subprocess for an empty binary")
	}
}

// TestProbeAgentVersion_MissingBinary exercises the real exec failure
// path (no seam swap): a binary that does not resolve on PATH returns
// "unknown" rather than failing the run. This is the degradation a CLI
// without a working `--version` invocation takes.
func TestProbeAgentVersion_MissingBinary(t *testing.T) {
	if got := probeAgentVersion(context.Background(), "fishhawk-no-such-binary-xyz"); got != "unknown" {
		t.Errorf("probeAgentVersion(missing) = %q, want unknown", got)
	}
}

// TestProbeAgentVersion_EmptyOutput asserts a binary that exits 0 with no
// output (or only blank lines) degrades to "unknown".
func TestProbeAgentVersion_EmptyOutput(t *testing.T) {
	orig := probeVersionCmd
	probeVersionCmd = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "printf", "\n   \n")
	}
	t.Cleanup(func() { probeVersionCmd = orig })

	if got := probeAgentVersion(context.Background(), "claude"); got != "unknown" {
		t.Errorf("probeAgentVersion(blank output) = %q, want unknown", got)
	}
}
