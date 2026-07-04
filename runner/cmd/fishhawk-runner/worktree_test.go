package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
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
	// Solo run keys on its own id (parallel-isolate off — the default).
	if got, want := lineageRoot(runID, "", false), shortID(runID); got != want {
		t.Errorf("solo lineageRoot = %q, want %q", got, want)
	}
	// Decomposed child keys on the parent id — so siblings share a tree.
	if got, want := lineageRoot(runID, parent, false), shortID(parent); got != want {
		t.Errorf("child lineageRoot = %q, want %q", got, want)
	}
}

// TestLineageRoot_ParallelIsolate asserts the E24.4 / #1144 keying flip: a
// decomposed child under parallel-isolate keys on its OWN id (so concurrent
// siblings get distinct worktrees), two siblings of one parent resolve to
// DISTINCT roots, and the solo path is unchanged regardless of the flag.
func TestLineageRoot_ParallelIsolate(t *testing.T) {
	const (
		parent = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		child1 = "11111111-2222-3333-4444-555555555555"
		child2 = "22222222-3333-4444-5555-666666666666"
		solo   = "99999999-8888-7777-6666-555555555555"
	)
	// A decomposed child under parallel-isolate keys on its OWN id.
	if got, want := lineageRoot(child1, parent, true), shortID(child1); got != want {
		t.Errorf("parallel-isolate child lineageRoot = %q, want own id %q", got, want)
	}
	// Two siblings of one parent get DISTINCT roots (no shared-tree race).
	if a, b := lineageRoot(child1, parent, true), lineageRoot(child2, parent, true); a == b {
		t.Errorf("parallel-isolate siblings shared a root: %q", a)
	}
	// And distinct from the shared-tree root the off path would pick.
	if got, shared := lineageRoot(child1, parent, true), lineageRoot(child1, parent, false); got == shared {
		t.Errorf("parallel-isolate child collided with the shared parent root: %q", got)
	}
	// Solo runs are unaffected by the flag (own id either way).
	if got, want := lineageRoot(solo, "", true), shortID(solo); got != want {
		t.Errorf("parallel-isolate solo lineageRoot = %q, want %q", got, want)
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

func TestAcquireWorktreeAdminLock(t *testing.T) {
	ctx := context.Background()

	// (a) acquire → release round-trip leaves no lockfile.
	t.Run("RoundTripLeavesNoFile", func(t *testing.T) {
		repo := initRepo(t)
		release, err := acquireWorktreeAdminLock(ctx, repo, io.Discard)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		wtDir, err := worktreesDir(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		lockPath := filepath.Join(wtDir, worktreeAdminLockName)
		if _, err := os.Stat(lockPath); err != nil {
			t.Fatalf("lockfile missing while held: %v", err)
		}
		release()
		if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
			t.Errorf("lockfile present after release: stat err = %v", err)
		}
	})

	// (b) a second acquire BLOCKS while the first is held and SUCCEEDS once
	// released — cross-lineage contention must wait, not fail loud.
	t.Run("BlocksThenSucceedsAfterRelease", func(t *testing.T) {
		repo := initRepo(t)
		release1, err := acquireWorktreeAdminLock(ctx, repo, io.Discard)
		if err != nil {
			t.Fatalf("first acquire: %v", err)
		}
		acquired := make(chan error, 1)
		go func() {
			r2, err := acquireWorktreeAdminLock(ctx, repo, io.Discard)
			if err == nil {
				r2()
			}
			acquired <- err
		}()
		// While the first lock is held the second acquire must still be
		// blocked (not yet sent on the channel).
		select {
		case err := <-acquired:
			t.Fatalf("second acquire returned (%v) while first lock held; want block", err)
		case <-time.After(150 * time.Millisecond):
		}
		release1()
		// After release the blocked acquire proceeds within a few backoffs.
		select {
		case err := <-acquired:
			if err != nil {
				t.Fatalf("second acquire after release: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("second acquire did not succeed after release")
		}
	})

	// (c) a stale lock whose recorded pid is dead is reclaimed.
	t.Run("ReclaimsStale", func(t *testing.T) {
		repo := initRepo(t)
		wtDir, err := worktreesDir(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(wtDir, 0o755); err != nil {
			t.Fatal(err)
		}
		lockPath := filepath.Join(wtDir, worktreeAdminLockName)
		// PID 0x7FFFFFFF is far above any real pid on these platforms.
		if err := os.WriteFile(lockPath, []byte("2147483647\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		release, err := acquireWorktreeAdminLock(ctx, repo, io.Discard)
		if err != nil {
			t.Fatalf("acquire over stale lock: %v", err)
		}
		defer release()
		if pid := readLockPID(lockPath); pid != os.Getpid() {
			t.Errorf("lock pid after reclaim = %d, want %d", pid, os.Getpid())
		}
	})

	// (d) a lock held by a LIVE pid times out within the bounded deadline and
	// returns an error rather than hanging the stage forever.
	t.Run("TimesOutOnLiveHolder", func(t *testing.T) {
		repo := initRepo(t)
		release, err := acquireWorktreeAdminLock(ctx, repo, io.Discard)
		if err != nil {
			t.Fatalf("first acquire: %v", err)
		}
		defer release()
		// The first lock records THIS (live) process's pid, so the reclaim
		// path can't fire; a bounded context makes the contended acquire
		// return an error instead of blocking forever.
		bounded, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
		defer cancel()
		start := time.Now()
		if _, err := acquireWorktreeAdminLock(bounded, repo, io.Discard); err == nil {
			t.Fatal("contended acquire succeeded against a live holder; want timeout error")
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("contended acquire took %s; want bounded by the deadline", elapsed)
		}
	})
}

func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Error("processAlive(self) = false, want true")
	}
	if processAlive(2147483647) {
		t.Error("processAlive(impossible pid) = true, want false")
	}
}

func TestLineageRootFull(t *testing.T) {
	const (
		runID  = "11111111-2222-3333-4444-555555555555"
		parent = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	)
	if got := lineageRootFull(runID, "", false); got != runID {
		t.Errorf("solo lineageRootFull = %q, want %q", got, runID)
	}
	if got := lineageRootFull(runID, parent, false); got != parent {
		t.Errorf("child lineageRootFull = %q, want %q", got, parent)
	}
	// Under parallel-isolate a decomposed child records its OWN full id so the
	// sidecar names the same run the per-child worktree dir is keyed on.
	if got := lineageRootFull(runID, parent, true); got != runID {
		t.Errorf("parallel-isolate child lineageRootFull = %q, want own id %q", got, runID)
	}
}

func TestWriteReadLineageRunID_RoundTrip(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()
	const (
		root  = "abcd1234"
		runID = "abcd1234-5678-90ab-cdef-1234567890ab"
	)
	writeLineageRunID(ctx, repo, root, runID, io.Discard)
	wtDir, err := worktreesDir(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := readLineageRunID(wtDir, root); got != runID {
		t.Errorf("readLineageRunID = %q, want %q", got, runID)
	}
	// An absent sidecar reads back empty (the sweep then skips it).
	if got := readLineageRunID(wtDir, "nopesuch"); got != "" {
		t.Errorf("readLineageRunID(absent) = %q, want empty", got)
	}
}

// fakeLineageClient reports lineage completion from a per-run-id map and
// records every queried id.
type fakeLineageClient struct {
	complete map[string]bool
	err      error
	queried  []string
}

func (f *fakeLineageClient) RunLineageComplete(_ context.Context, runID string) (bool, error) {
	f.queried = append(f.queried, runID)
	if f.err != nil {
		return false, f.err
	}
	return f.complete[runID], nil
}

func TestSweepTerminalWorktrees_RemovesTerminalKeepsLive(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	// Two lineages: "done" (terminal) and "live" (still running).
	// The sidecar values are canonical-UUID-shaped (hex only) so the sweep's
	// looksLikeRunID gate passes them through to the backend query; the short
	// root directory names need not be hex.
	const (
		doneRoot = "done0000"
		doneID   = "d09e0000-0000-0000-0000-000000000000"
		liveRoot = "live0000"
		liveID   = "11e70000-0000-0000-0000-000000000000"
	)
	donePath, err := provisionLineageWorktree(ctx, repo, doneRoot, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	writeLineageRunID(ctx, repo, doneRoot, doneID, io.Discard)
	livePath, err := provisionLineageWorktree(ctx, repo, liveRoot, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	writeLineageRunID(ctx, repo, liveRoot, liveID, io.Discard)

	client := &fakeLineageClient{complete: map[string]bool{doneID: true, liveID: false}}
	sweepTerminalWorktrees(ctx, repo, client, io.Discard)

	// The terminal lineage's worktree + sidecar are gone; the live one stays.
	if _, err := os.Stat(donePath); !os.IsNotExist(err) {
		t.Errorf("terminal worktree still present: stat err = %v", err)
	}
	wtDir, _ := worktreesDir(ctx, repo)
	if got := readLineageRunID(wtDir, doneRoot); got != "" {
		t.Errorf("terminal lineage sidecar not removed: %q", got)
	}
	if st, err := os.Stat(livePath); err != nil || !st.IsDir() {
		t.Errorf("live worktree was removed: %v", err)
	}
	if got := readLineageRunID(wtDir, liveRoot); got != liveID {
		t.Errorf("live lineage sidecar removed: %q", got)
	}
	// Both lineages were queried by their FULL run id.
	if len(client.queried) != 2 {
		t.Errorf("queried = %v, want both lineage ids", client.queried)
	}
	// The operator's tracked tree stays clean throughout.
	if status := gitPorcelain(t, repo); status != "" {
		t.Errorf("operator git status not clean after sweep:\n%s", status)
	}
}

func TestSweepTerminalWorktrees_SkipsWhenNoSidecar(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()
	const root = "nosc0000"
	path, err := provisionLineageWorktree(ctx, repo, root, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	// No writeLineageRunID — the sweep can't resolve the short name to a
	// run id, so it must leave the worktree and never query the backend.
	client := &fakeLineageClient{complete: map[string]bool{}}
	sweepTerminalWorktrees(ctx, repo, client, io.Discard)
	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		t.Errorf("worktree without a sidecar was removed: %v", err)
	}
	if len(client.queried) != 0 {
		t.Errorf("queried backend without a sidecar: %v", client.queried)
	}
}

func TestSweepTerminalWorktrees_BackendErrorIsBestEffort(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()
	const (
		root  = "errr0000"
		runID = "e4440000-0000-0000-0000-000000000000"
	)
	path, err := provisionLineageWorktree(ctx, repo, root, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	writeLineageRunID(ctx, repo, root, runID, io.Discard)
	client := &fakeLineageClient{err: errSweepProbe}
	// Must not panic and must not remove the worktree on a backend error.
	sweepTerminalWorktrees(ctx, repo, client, io.Discard)
	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		t.Errorf("worktree removed despite backend error: %v", err)
	}
}

func TestLooksLikeRunID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"11111111-2222-3333-4444-555555555555", true},
		{"abcd1234-5678-90ab-cdef-1234567890ab", true},
		{"rid", false},
		{"", false},
		{"11111111", false},
		{"11111111-2222-3333-4444-55555555555", false},   // 11 hex in last group
		{"11111111-2222-3333-4444-5555555555555", false}, // 13 hex in last group
		{"g1111111-2222-3333-4444-555555555555", false},  // non-hex
	}
	for _, c := range cases {
		if got := looksLikeRunID(c.in); got != c.want {
			t.Errorf("looksLikeRunID(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestSweepTerminalWorktrees_PrunesNonRunRoot asserts a worktree whose
// sidecar records a NON-UUID value (a leftover test fixture like "rid") is
// pruned locally, the backend is NEVER queried (so no 400), and exactly one
// lineage_worktree_pruned line with reason non_run_root is emitted.
func TestSweepTerminalWorktrees_PrunesNonRunRoot(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()
	const root = "rid00000"
	path, err := provisionLineageWorktree(ctx, repo, root, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	// A non-UUID sidecar value — the runner recorded a fixture "run id".
	writeLineageRunID(ctx, repo, root, "rid", io.Discard)

	var log bytes.Buffer
	client := &fakeLineageClient{complete: map[string]bool{}}
	sweepTerminalWorktrees(ctx, repo, client, &log)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("non-run worktree not pruned: stat err = %v", err)
	}
	wtDir, _ := worktreesDir(ctx, repo)
	if got := readLineageRunID(wtDir, root); got != "" {
		t.Errorf("non-run sidecar not removed: %q", got)
	}
	if len(client.queried) != 0 {
		t.Errorf("backend queried for a non-UUID root: %v", client.queried)
	}
	if !strings.Contains(log.String(), `"event":"lineage_worktree_pruned"`) ||
		!strings.Contains(log.String(), `"reason":"non_run_root"`) {
		t.Errorf("missing lineage_worktree_pruned/non_run_root line:\n%s", log.String())
	}
}

// TestSweepTerminalWorktrees_PrunesUnknownRun asserts that when the backend
// definitively reports the run absent (upload.ErrNotFound / 404), the
// orphaned worktree is pruned after exactly one query and a single
// lineage_worktree_pruned line with reason run_not_found is emitted.
func TestSweepTerminalWorktrees_PrunesUnknownRun(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()
	const (
		root  = "gone0000"
		runID = "9c9e0000-0000-0000-0000-000000000000"
	)
	path, err := provisionLineageWorktree(ctx, repo, root, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	writeLineageRunID(ctx, repo, root, runID, io.Discard)

	var log bytes.Buffer
	client := &fakeLineageClient{err: upload.ErrNotFound}
	sweepTerminalWorktrees(ctx, repo, client, &log)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("unknown-run worktree not pruned: stat err = %v", err)
	}
	if len(client.queried) != 1 {
		t.Errorf("queried = %v, want exactly one query", client.queried)
	}
	if got := strings.Count(log.String(), `"event":"lineage_worktree_pruned"`); got != 1 {
		t.Errorf("lineage_worktree_pruned lines = %d, want 1:\n%s", got, log.String())
	}
	if !strings.Contains(log.String(), `"reason":"run_not_found"`) {
		t.Errorf("missing run_not_found reason:\n%s", log.String())
	}
}

// TestSweepTerminalWorktrees_KeepsHealthyRun asserts a worktree whose run
// exists but is not lineage-complete is KEPT after exactly one query.
func TestSweepTerminalWorktrees_KeepsHealthyRun(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()
	const (
		root  = "helt0000"
		runID = "be170000-0000-0000-0000-000000000000"
	)
	path, err := provisionLineageWorktree(ctx, repo, root, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	writeLineageRunID(ctx, repo, root, runID, io.Discard)

	client := &fakeLineageClient{complete: map[string]bool{runID: false}}
	sweepTerminalWorktrees(ctx, repo, client, io.Discard)

	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		t.Errorf("healthy (not-complete) worktree was removed: %v", err)
	}
	if len(client.queried) != 1 {
		t.Errorf("queried = %v, want exactly one query", client.queried)
	}
}

var errSweepProbe = errProbe("backend down")

type errProbe string

func (e errProbe) Error() string { return string(e) }

// gitPorcelain returns `git status --porcelain` output for dir.
func gitPorcelain(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "status", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	return strings.TrimSpace(string(out))
}
