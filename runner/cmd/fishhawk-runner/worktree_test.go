package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initRepo creates a throwaway git repo with one commit so HEAD exists
// (git worktree add --detach HEAD requires a commit). Skips the test when
// git is unavailable.
func initRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "seed")
	return dir
}

func TestLineageRoot(t *testing.T) {
	const (
		runID  = "11111111-2222-3333-4444-555555555555"
		parent = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	)
	// Solo run keys on its own id.
	if got, want := lineageRoot(runID, ""), shortID(runID); got != want {
		t.Errorf("solo lineageRoot = %q, want %q", got, want)
	}
	// Decomposed child keys on the parent id — so siblings share a tree.
	if got, want := lineageRoot(runID, parent), shortID(parent); got != want {
		t.Errorf("child lineageRoot = %q, want %q", got, want)
	}
}

func TestWorktreesDir_StableAcrossLinkedWorktree(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	fromRepo, err := worktreesDir(ctx, repo)
	if err != nil {
		t.Fatalf("worktreesDir(repo): %v", err)
	}
	if !strings.HasSuffix(fromRepo, filepath.Join("fishhawk-worktrees")) {
		t.Errorf("worktreesDir = %q, want a .../fishhawk-worktrees path", fromRepo)
	}

	// Create a linked worktree of the repo and resolve worktreesDir FROM
	// it: --git-common-dir must point both invocations at the one shared
	// gitdir, so the fishhawk-worktrees root is identical.
	linked := filepath.Join(t.TempDir(), "linked")
	if out, err := exec.CommandContext(ctx, "git", "-C", repo,
		"worktree", "add", "--detach", linked, "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("git worktree add linked: %v\n%s", err, out)
	}
	fromLinked, err := worktreesDir(ctx, linked)
	if err != nil {
		t.Fatalf("worktreesDir(linked): %v", err)
	}
	if canonPath(fromRepo) != canonPath(fromLinked) {
		t.Errorf("worktreesDir differs: repo=%q linked=%q", fromRepo, fromLinked)
	}
}

func TestProvisionLineageWorktree_CreateThenReuse(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()
	const root = "abcdef12"

	first, err := provisionLineageWorktree(ctx, repo, root, io.Discard)
	if err != nil {
		t.Fatalf("first provision: %v", err)
	}
	if st, err := os.Stat(first); err != nil || !st.IsDir() {
		t.Fatalf("worktree path not a dir: %v", err)
	}
	// The worktree lives under the shared gitdir, invisible to the
	// operator's tracked tree.
	if status := gitPorcelain(t, repo); status != "" {
		t.Errorf("operator git status not clean after provision:\n%s", status)
	}

	// Second provision of the SAME root reuses the existing worktree
	// (decomposed-child sharing) rather than failing on a populated path.
	second, err := provisionLineageWorktree(ctx, repo, root, io.Discard)
	if err != nil {
		t.Fatalf("reuse provision: %v", err)
	}
	if canonPath(first) != canonPath(second) {
		t.Errorf("reuse returned a different path: %q vs %q", first, second)
	}
}

func TestProvisionLineageWorktree_SoloDistinct(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	a, err := provisionLineageWorktree(ctx, repo, "aaaaaaaa", io.Discard)
	if err != nil {
		t.Fatalf("provision a: %v", err)
	}
	b, err := provisionLineageWorktree(ctx, repo, "bbbbbbbb", io.Discard)
	if err != nil {
		t.Fatalf("provision b: %v", err)
	}
	if canonPath(a) == canonPath(b) {
		t.Errorf("distinct roots shared a worktree: %q", a)
	}
}

func TestAcquireLineageLock_HeldThenReleased(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	release, err := acquireLineageLock(ctx, repo, "root1234", "run-a", io.Discard)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	// A second acquire while the lock is held by THIS (live) process must
	// fail loud — a concurrent same-lineage stage is the corruption hazard
	// the lock exists to catch.
	if _, err := acquireLineageLock(ctx, repo, "root1234", "run-b", io.Discard); err == nil {
		t.Fatal("second acquire succeeded while lock held; want loud failure")
	}
	release()
	// After release the lock is reacquirable.
	release2, err := acquireLineageLock(ctx, repo, "root1234", "run-c", io.Discard)
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	release2()
}

func TestAcquireLineageLock_ReclaimsStale(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()
	wtDir, err := worktreesDir(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Plant a stale lock recording a pid that cannot be alive. PID
	// 0x7FFFFFFF is far above any real pid on these platforms.
	lockPath := filepath.Join(wtDir, "run-stale123.lock")
	if err := os.WriteFile(lockPath, []byte("2147483647\nold-run\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	release, err := acquireLineageLock(ctx, repo, "stale123", "run-new", io.Discard)
	if err != nil {
		t.Fatalf("acquire over stale lock: %v", err)
	}
	defer release()
	// The lock now records OUR pid.
	if pid := readLockPID(lockPath); pid != os.Getpid() {
		t.Errorf("lock pid after reclaim = %d, want %d", pid, os.Getpid())
	}
}

func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Error("processAlive(self) = false, want true")
	}
	if processAlive(2147483647) {
		t.Error("processAlive(impossible pid) = true, want false")
	}
}

// gitPorcelain returns `git status --porcelain` output for dir.
func gitPorcelain(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "status", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	return strings.TrimSpace(string(out))
}
