package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// TestIsRunAgentMatchesAcceptanceTreePath pins scripts/is-run-agent (the
// operator-only agent skills' run-agent guard, #3938) to the runner's REAL
// acceptance-tree name: it renders acceptanceTreePath with UUID ids, creates
// a directory of that name, and requires the script to classify it as a run
// agent. A rename of the Go format string that the script's hand-written
// pattern does not follow fails here instead of silently turning the guard
// into `operator` inside real acceptance trees. The conflict-resolution and
// acceptance-workdir shapes are inline os.MkdirTemp patterns with no function
// to render; the dedicated runner-stamped marker (#3945) supersedes the path
// list.
func TestIsRunAgentMatchesAcceptanceTreePath(t *testing.T) {
	// Resolve from the init-captured package source dir, not the working
	// directory: this package's TestMain moves the process cwd.
	script := filepath.Join(pkgSrcDir, "..", "..", "..", "scripts", "is-run-agent")
	const runID, stageID = "3a8cc5f5-85cd-4664-8b7c-6974739db24e", "123bc991-6f01-4850-8d83-8cc8ac19a3e6"
	dir := filepath.Join(t.TempDir(), filepath.Base(acceptanceTreePath(runID, stageID)))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cmd := exec.Command(script, dir)
	// Scrub the env signals so only the path can produce the verdict. That
	// includes the runner-stamped marker FISHHAWK_RUN_AGENT (#3945): inside a
	// run agent's own shell it is set, and scripts/is-run-agent reads it
	// first (#3947), so the script would print a marker reason and fail the
	// "acceptance tree" prefix check below.
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "FISHHAWK_RUN_ID=") || strings.HasPrefix(kv, "FISHHAWK_FORGE_WRITES=") ||
			strings.HasPrefix(kv, agent.RunAgentEnvVar+"=") {
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 10 {
		t.Fatalf("is-run-agent on %s: err=%v output=%q, want exit 10", dir, err, out)
	}
	if !strings.HasPrefix(string(out), "run-agent: acceptance tree") {
		t.Fatalf("is-run-agent on %s: output=%q, want run-agent: acceptance tree", dir, out)
	}
}
