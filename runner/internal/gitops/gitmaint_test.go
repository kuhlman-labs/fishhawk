package gitops

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitMaintenancePinPath is the GIT_CONFIG_GLOBAL file runTestMain writes,
// recorded here so gitNoPromptEnv (commit_test.go) can repoint its own
// per-test override at it instead of /dev/null (#3507).
var gitMaintenancePinPath string

// TestMain pins git's auto-maintenance off for every git child this test
// process spawns, mirroring runner/cmd/fishhawk-runner/main_test.go's
// runTestMain (#3503). This package's TestCommitAndPush_VerifyCommit_*
// tests and the #3443 refreshWireFixture tests push into t.TempDir() bare
// origins, so without this pin a `git push` here can spawn a detached
// `git maintenance run --auto --detach` child that races a fixture's
// t.TempDir() cleanup ("directory not empty", #3503).
func TestMain(m *testing.M) {
	os.Exit(runTestMain(m))
}

// runTestMain does TestMain's work in a func so its cleanup defer runs
// before os.Exit (os.Exit skips deferred funcs).
func runTestMain(m *testing.M) int {
	if _, err := exec.LookPath("git"); err != nil {
		return m.Run() // git unavailable — nothing to pin.
	}

	// GIT_CONFIG_GLOBAL is the only channel that reaches a spawned
	// receive-pack over a file-path push: git's connect.c scrubs
	// repo-local config env (GIT_CONFIG_COUNT / GIT_CONFIG_PARAMETERS)
	// for the file transport. This file lives in its own temp dir (not
	// a fixture repo, where an untracked file would show up in `git
	// status`) and carries exactly one key so the #912 commit.gpgsign
	// posture (an ambient GIT_CONFIG_GLOBAL=/dev/null) is preserved
	// rather than widened.
	if gitConfigDir, err := os.MkdirTemp("", "fishhawk-gitops-gitconfig-*"); err == nil {
		defer func() { _ = os.RemoveAll(gitConfigDir) }()
		gitConfigPath := filepath.Join(gitConfigDir, "gitconfig")
		if err := os.WriteFile(gitConfigPath, []byte("[maintenance]\n\tauto = false\n"), 0o644); err == nil {
			gitMaintenancePinPath = gitConfigPath
			_ = os.Setenv("GIT_CONFIG_GLOBAL", gitConfigPath)
		}
	}

	return m.Run()
}

// trace2Event is the subset of a GIT_TRACE2_EVENT JSONL line this test
// cares about: the "start" event for the top-level process and any
// "child_start" event it spawns each carry an "argv". Git's local-transport
// receive-pack spawn is logged as a SINGLE joined command-line string
// (e.g. `git-receive-pack '../origin.git'`) rather than split tokens, while
// a re-exec'd "git" child (including a spawned "maintenance" run) is logged
// with split argv — so callers must substring-match, not element-match.
type trace2Event struct {
	Event string   `json:"event"`
	Argv  []string `json:"argv"`
}

// assertNoMaintenanceChildOnPush pushes repo's main into a fresh bare
// origin under GIT_TRACE2_EVENT and fails if the push spawned a
// "maintenance" child. It first REQUIRES the trace to carry both a push
// "start" record and a receive-pack "child_start" record — a trace that
// never recorded the push could satisfy the negative "no maintenance
// child" assertion for the wrong reason (an unexercised trace, not a
// working pin).
func assertNoMaintenanceChildOnPush(t *testing.T, repo string) {
	t.Helper()

	origin := filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", "--initial-branch=main", origin).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare %s: %v\n%s", origin, err, out)
	}
	mustGit(t, repo, "remote", "add", "origin", origin)

	tracePath := filepath.Join(t.TempDir(), "trace2.json")
	cmd := exec.Command("git", "push", "-q", "origin", "main:main")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GIT_TRACE2_EVENT="+tracePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git push: %v\n%s", err, out)
	}

	traceBytes, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read GIT_TRACE2_EVENT file %s: %v", tracePath, err)
	}

	var sawPushStart, sawReceivePackChild, sawMaintenanceChild bool
	for _, line := range strings.Split(strings.TrimSpace(string(traceBytes)), "\n") {
		if line == "" {
			continue
		}
		var evt trace2Event
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue // non-JSON or non-matching trace2 line (e.g. "version"); not a fatal condition.
		}
		for _, arg := range evt.Argv {
			switch {
			case evt.Event == "start" && arg == "push":
				sawPushStart = true
			case evt.Event == "child_start" && strings.Contains(arg, "receive-pack"):
				sawReceivePackChild = true
			case evt.Event == "child_start" && strings.Contains(arg, "maintenance"):
				sawMaintenanceChild = true
			}
		}
	}

	if !sawPushStart {
		t.Fatalf("trace2 %s carries no \"start\" record with argv \"push\" — trace unexercised, not a pin verification:\n%s", tracePath, traceBytes)
	}
	if !sawReceivePackChild {
		t.Fatalf("trace2 %s carries no \"child_start\" record naming receive-pack — trace unexercised, not a pin verification:\n%s", tracePath, traceBytes)
	}

	if sawMaintenanceChild {
		t.Errorf("git push spawned a \"maintenance\" child — the GIT_CONFIG_GLOBAL pin did not take effect (GIT_CONFIG_GLOBAL=%q):\n%s", os.Getenv("GIT_CONFIG_GLOBAL"), traceBytes)
	}
}

// TestHarnessDisablesAutoMaintenanceOnLocalPush pins the runTestMain
// GIT_CONFIG_GLOBAL maintenance.auto=false posture (#3503, extended to
// this package by #3507): a `git push` into a local bare origin must not
// spawn a detached `git maintenance run --auto --detach` child, because
// git >= 2.46 daemonizes that child (it forks and the parent returns from
// push while the child keeps running), and a later maintenance task
// recreating objects/pack/ can race a t.TempDir() bare-origin fixture's
// RemoveAll cleanup with "directory not empty".
//
// Deleting runTestMain's os.Setenv("GIT_CONFIG_GLOBAL", …) line (while
// keeping the config file write) turns the "default" subtest RED under
// `GIT_CONFIG_GLOBAL=/dev/null go test -run TestHarnessDisablesAutoMaintenanceOnLocalPush`
// (the scripts/test posture): the ambient pin never reaches this process,
// maintenance.auto defaults true, and receive-pack spawns the maintenance
// child.
//
// "after_gitNoPromptEnv" pins the coupling found in planning: gitNoPromptEnv
// (commit_test.go) sets GIT_CONFIG_GLOBAL per-test for the #3443 wire-fixture
// tests, and newRefreshWireFixture pushes into a TempDir bare — a TestMain
// pin alone would be defeated for the duration of any test calling
// gitNoPromptEnv unless it is repointed at gitMaintenancePinPath. Reverting
// that repoint back to "/dev/null" turns this subtest RED while "default"
// stays green.
func TestHarnessDisablesAutoMaintenanceOnLocalPush(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	newRepo := func(t *testing.T) string {
		t.Helper()
		repo := t.TempDir()
		mustGit(t, repo, "init", "-q", "--initial-branch=main")
		mustGit(t, repo, "config", "user.name", "init")
		mustGit(t, repo, "config", "user.email", "init@example.com")
		mustGit(t, repo, "config", "commit.gpgsign", "false")
		if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mustGit(t, repo, "add", "-A")
		mustGit(t, repo, "commit", "-q", "-m", "seed")
		return repo
	}

	t.Run("default", func(t *testing.T) {
		assertNoMaintenanceChildOnPush(t, newRepo(t))
	})

	t.Run("after_gitNoPromptEnv", func(t *testing.T) {
		gitNoPromptEnv(t)
		assertNoMaintenanceChildOnPush(t, newRepo(t))
	})
}
