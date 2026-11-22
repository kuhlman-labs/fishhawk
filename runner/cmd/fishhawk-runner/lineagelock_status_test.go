package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// lineagelock_status_test.go pins the read-only `lineage-lock-status`
// subcommand (#5485) over real `git init` checkouts.

const (
	llsRunID    = "aaaaaaaa-1111-2222-3333-444444444444"
	llsParentID = "bbbbbbbb-5555-6666-7777-888888888888"
)

// llsRun calls the subcommand with buffers and returns the exit code, stdout
// and the log sink.
func llsRun(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, logSink bytes.Buffer
	code := runLineageLockStatus(args, &stdout, &logSink)
	return code, stdout.String(), logSink.String()
}

// llsDecode decodes exactly one strict JSON verdict line from stdout.
func llsDecode(t *testing.T, stdout string) lineageLockStatus {
	t.Helper()
	if strings.Count(stdout, "\n") != 1 || !strings.HasSuffix(stdout, "\n") {
		t.Fatalf("stdout = %q; want exactly one JSON line", stdout)
	}
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.DisallowUnknownFields()
	var got lineageLockStatus
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("decode stdout %q: %v", stdout, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"root", "lock_path", "present", "held", "holder_pid", "holder_run_id"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("stdout %q lacks the %q key", stdout, k)
		}
	}
	return got
}

// llsWorktreesDir is the checkout's resolved <git-common-dir>/fishhawk-worktrees.
func llsWorktreesDir(t *testing.T, repo string) string {
	t.Helper()
	common, err := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(common, "fishhawk-worktrees")
}

// llsDeadPID is the pid of a child that has exited and been reaped.
func llsDeadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skipf("cannot run true: %v", err)
	}
	return cmd.ProcessState.Pid()
}

// llsPlantLock writes a lockfile in the acquirer's format (pid, run id).
func llsPlantLock(t *testing.T, repo, root, body string) string {
	t.Helper()
	dir := llsWorktreesDir(t, repo)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "run-"+root+".lock")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLineageLockStatus_NoLock(t *testing.T) {
	repo := initRepo(t)
	code, stdout, logs := llsRun(t, "--working-dir", repo, "--run-id", llsRunID)
	if code != exitOK {
		t.Fatalf("exit = %d; want %d (log: %s)", code, exitOK, logs)
	}
	got := llsDecode(t, stdout)
	want := filepath.Join(llsWorktreesDir(t, repo), "run-"+shortID(llsRunID)+".lock")
	if got.Present || got.Held || got.HolderPID != 0 || got.HolderRunID != "" {
		t.Fatalf("verdict = %+v; want absent, not held, no holder", got)
	}
	if got.LockPath != want || got.Root != shortID(llsRunID) {
		t.Fatalf("lock_path = %q root = %q; want %q and %q", got.LockPath, got.Root, want, shortID(llsRunID))
	}
}

func TestLineageLockStatus_ReportsLockAcquiredByAcquireLineageLock(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()
	release, err := acquireLineageLock(ctx, repo, lineageRoot(llsRunID, "", false), llsRunID, nil, io.Discard)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	code, stdout, logs := llsRun(t, "--working-dir", repo, "--run-id", llsRunID)
	if code != exitOK {
		release()
		t.Fatalf("exit = %d; want %d (log: %s)", code, exitOK, logs)
	}
	got := llsDecode(t, stdout)
	if !got.Present || !got.Held || got.HolderPID != os.Getpid() || got.HolderRunID != llsRunID {
		release()
		t.Fatalf("verdict = %+v; want held by pid %d run %s", got, os.Getpid(), llsRunID)
	}
	if _, err := os.Stat(got.LockPath); err != nil {
		release()
		t.Fatalf("reported lock_path %q is not the file acquireLineageLock created: %v", got.LockPath, err)
	}
	release()
	code, stdout, _ = llsRun(t, "--working-dir", repo, "--run-id", llsRunID)
	if got := llsDecode(t, stdout); code != exitOK || got.Present || got.Held {
		t.Fatalf("after release: exit %d verdict %+v; want absent, not held", code, got)
	}
}

func TestLineageLockStatus_StaleLockNotHeld(t *testing.T) {
	repo := initRepo(t)
	dead := llsDeadPID(t)
	llsPlantLock(t, repo, shortID(llsRunID), fmt.Sprintf("%d\n%s\n", dead, llsRunID))
	code, stdout, logs := llsRun(t, "--working-dir", repo, "--run-id", llsRunID)
	if code != exitOK {
		t.Fatalf("exit = %d; want %d (log: %s)", code, exitOK, logs)
	}
	got := llsDecode(t, stdout)
	if !got.Present || got.Held || got.HolderPID != dead || got.HolderRunID != llsRunID {
		t.Fatalf("verdict = %+v; want present, NOT held, holder_pid %d", got, dead)
	}
}

func TestLineageLockStatus_UnparseablePIDNotHeld(t *testing.T) {
	repo := initRepo(t)
	llsPlantLock(t, repo, shortID(llsRunID), "")
	code, stdout, _ := llsRun(t, "--working-dir", repo, "--run-id", llsRunID)
	got := llsDecode(t, stdout)
	if code != exitOK || !got.Present || got.Held || got.HolderPID != 0 {
		t.Fatalf("exit %d verdict %+v; want present, not held, holder_pid 0", code, got)
	}
}

func TestLineageLockStatus_RootSelection(t *testing.T) {
	repo := initRepo(t)
	rows := []struct {
		name string
		args []string
		root string
	}{
		{"solo", nil, shortID(llsRunID)},
		{"decomposed child shares the parent's lineage", []string{"--decomposed-from", llsParentID}, shortID(llsParentID)},
		{"parallel-isolated child keys on its own id", []string{"--decomposed-from", llsParentID, "--parallel-isolate"}, shortID(llsRunID)},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--working-dir", repo, "--run-id", llsRunID}, tc.args...)
			code, stdout, logs := llsRun(t, args...)
			if code != exitOK {
				t.Fatalf("exit = %d (log: %s)", code, logs)
			}
			got := llsDecode(t, stdout)
			if got.Root != tc.root || !strings.HasSuffix(got.LockPath, string(filepath.Separator)+"run-"+tc.root+".lock") {
				t.Fatalf("root = %q lock_path = %q; want root %q", got.Root, got.LockPath, tc.root)
			}
		})
	}
}

func TestLineageLockStatus_UsageErrors(t *testing.T) {
	repo := initRepo(t)
	rows := map[string][]string{
		"missing --run-id":      {"--working-dir", repo},
		"missing --working-dir": {"--run-id", llsRunID},
		"stray positional":      {"--working-dir", repo, "--run-id", llsRunID, "extra"},
		"unknown flag":          {"--working-dir", repo, "--run-id", llsRunID, "--no-such-flag"},
	}
	for name, args := range rows {
		t.Run(name, func(t *testing.T) {
			code, stdout, _ := llsRun(t, args...)
			if code != exitUsage || stdout != "" {
				t.Fatalf("exit = %d stdout = %q; want %d and empty stdout", code, stdout, exitUsage)
			}
		})
	}
}

func TestLineageLockStatus_NotAGitCheckoutFails(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	code, stdout, logs := llsRun(t, "--working-dir", t.TempDir(), "--run-id", llsRunID)
	if code != exitFailure || stdout != "" {
		t.Fatalf("exit = %d stdout = %q; want %d and empty stdout", code, stdout, exitFailure)
	}
	if !strings.Contains(logs, `"event":"lineage_lock_status_failed"`) {
		t.Fatalf("log sink = %q; want a lineage_lock_status_failed line", logs)
	}
}

// A lockfile that cannot be stat'ed (an unsearchable worktrees dir) fails the
// call rather than reading absent.
func TestLineageLockStatus_UnstatableLockFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	repo := initRepo(t)
	dir := llsWorktreesDir(t, repo)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	code, stdout, logs := llsRun(t, "--working-dir", repo, "--run-id", llsRunID)
	if code != exitFailure || stdout != "" || !strings.Contains(logs, "lineage_lock_status_failed") {
		t.Fatalf("exit = %d stdout = %q log = %q; want %d, empty stdout, a lineage_lock_status_failed line", code, stdout, logs, exitFailure)
	}
}

func TestLineageLockStatus_ReadOnly(t *testing.T) {
	t.Run("no worktrees dir is created", func(t *testing.T) {
		repo := initRepo(t)
		if code, _, logs := llsRun(t, "--working-dir", repo, "--run-id", llsRunID); code != exitOK {
			t.Fatalf("exit = %d (log: %s)", code, logs)
		}
		if _, err := os.Stat(llsWorktreesDir(t, repo)); !os.IsNotExist(err) {
			t.Fatalf("fishhawk-worktrees stat err = %v; want it still absent", err)
		}
	})
	t.Run("a stale lock is left byte-identical", func(t *testing.T) {
		repo := initRepo(t)
		body := fmt.Sprintf("%d\n%s\n", llsDeadPID(t), llsRunID)
		p := llsPlantLock(t, repo, shortID(llsRunID), body)
		if code, _, logs := llsRun(t, "--working-dir", repo, "--run-id", llsRunID); code != exitOK {
			t.Fatalf("exit = %d (log: %s)", code, logs)
		}
		after, err := os.ReadFile(p)
		if err != nil || string(after) != body {
			t.Fatalf("stale lock after the call = %q, %v; want %q untouched", after, err, body)
		}
	})
}

// TestRun_RoutesLineageLockStatus drives run() itself (L2(f)): a dropped or
// misplaced route would reach parseFlags, which exits usage for want of the
// stage flags.
func TestRun_RoutesLineageLockStatus(t *testing.T) {
	repo := initRepo(t)
	swapInstallProcessGitConfig(t, func() error { return nil })
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	origStdout := os.Stdout
	os.Stdout = stdoutW
	t.Cleanup(func() { os.Stdout = origStdout })

	var sink bytes.Buffer
	code := run([]string{lineageLockStatusSubcommand, "--working-dir", repo, "--run-id", llsRunID}, &sink)
	os.Stdout = origStdout
	if err := stdoutW.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(stdoutR)
	if err != nil {
		t.Fatal(err)
	}
	if code != exitOK {
		t.Fatalf("run(lineage-lock-status) = %d; want exitOK (stdout %q, log %s)", code, out, sink.String())
	}
	if got := llsDecode(t, string(out)); got.Root != shortID(llsRunID) || got.Present {
		t.Fatalf("verdict = %+v; want root %s, absent", got, shortID(llsRunID))
	}
}
