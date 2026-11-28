package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// deregGit runs git in dir for a test fixture and fails the test on error.
func deregGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// deregTestRepo creates a throwaway repository with one commit.
func deregTestRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	deregGit(t, repo, "init", "-q", "--initial-branch=main")
	deregGit(t, repo, "commit", "-q", "--allow-empty", "-m", "base")
	return repo
}

// hiddenForeignWorktree registers a FOREIGN worktree of repo under its own
// temp dir, outside repo, and returns its path plus a hide/restore pair. Hiding
// moves the tree aside, which is how a partial filesystem view looks to git: a
// repo-wide `git worktree prune` sees <ext>/.git missing and deletes
// .git/worktrees/<id>, so git then fails inside ext after the restore (#4281).
// restore is idempotent and also runs in t.Cleanup.
func hiddenForeignWorktree(t *testing.T, repo string) (ext string, hide, restore func()) {
	t.Helper()
	ext = filepath.Join(t.TempDir(), "ext")
	deregGit(t, repo, "worktree", "add", "-q", "--detach", ext, "HEAD")
	hidden := ext + ".hidden"
	hide = func() {
		t.Helper()
		if err := os.Rename(ext, hidden); err != nil {
			t.Fatalf("hide foreign worktree: %v", err)
		}
	}
	restore = func() {
		if _, err := os.Stat(hidden); err == nil {
			_ = os.Rename(hidden, ext)
		}
	}
	t.Cleanup(restore)
	return ext, hide, restore
}

// assertForeignWorktreeSurvives reads COMMITTED state after the call under
// test: git still works inside ext, and repo still lists it.
func assertForeignWorktreeSurvives(t *testing.T, repo, ext string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", ext, "rev-parse", "--git-dir").CombinedOutput(); err != nil {
		t.Errorf("foreign worktree %s lost its admin dir (a repo-wide prune ran): %v\n%s", ext, err, out)
	}
	if !worktreeRegistered(t, repo, ext) {
		t.Errorf("foreign worktree %s is no longer registered in %s", ext, repo)
	}
}

// TestDeregisterWorktree_SparesHiddenForeignWorktree: deregistering one missing
// worktree leaves a hidden foreign worktree's registration intact.
func TestDeregisterWorktree_SparesHiddenForeignWorktree(t *testing.T) {
	repo := deregTestRepo(t)
	ext, hide, restore := hiddenForeignWorktree(t, repo)
	target := filepath.Join(t.TempDir(), "target")
	deregGit(t, repo, "worktree", "add", "-q", "--detach", target, "HEAD")
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}

	hide()
	err := deregisterWorktree(context.Background(), acceptanceTreeGit(repo), target)
	restore()

	if err != nil {
		t.Fatalf("deregisterWorktree: %v", err)
	}
	if worktreeRegistered(t, repo, target) {
		t.Error("target registration must be gone")
	}
	assertForeignWorktreeSurvives(t, repo, ext)
}

// TestDeregisterWorktree_LockedMissingDeregistered: double --force clears a
// LOCKED entry whose tree is already gone.
func TestDeregisterWorktree_LockedMissingDeregistered(t *testing.T) {
	repo := deregTestRepo(t)
	target := filepath.Join(t.TempDir(), "target")
	deregGit(t, repo, "worktree", "add", "-q", "--detach", target, "HEAD")
	deregGit(t, repo, "worktree", "lock", target)
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}

	if err := deregisterWorktree(context.Background(), acceptanceTreeGit(repo), target); err != nil {
		t.Fatalf("deregisterWorktree: %v", err)
	}
	if worktreeRegistered(t, repo, target) {
		t.Error("locked, missing target must be deregistered")
	}
}

// symlinkAlias returns a real directory and a symlink alias that points at it.
func symlinkAlias(t *testing.T) (realDir, alias string) {
	t.Helper()
	realDir = filepath.Join(t.TempDir(), "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	alias = filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	return realDir, alias
}

// TestDeregisterWorktree_AliasPathMissingTree: a missing tree named through a
// symlinked parent (the macOS /tmp -> /private/tmp shape) is deregistered.
func TestDeregisterWorktree_AliasPathMissingTree(t *testing.T) {
	repo := deregTestRepo(t)
	_, alias := symlinkAlias(t)
	target := filepath.Join(alias, "target")
	deregGit(t, repo, "worktree", "add", "-q", "--detach", target, "HEAD")
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}

	if err := deregisterWorktree(context.Background(), acceptanceTreeGit(repo), target); err != nil {
		t.Fatalf("deregisterWorktree: %v", err)
	}
	if worktreeRegistered(t, repo, target) {
		t.Error("alias-named missing target must be deregistered")
	}
}

// TestDeregisterWorktree_UnregisteredPathIsNoop: a path that was never
// registered returns nil (the remove's "is not a working tree" error is not
// propagated) and leaves the foreign registration intact.
func TestDeregisterWorktree_UnregisteredPathIsNoop(t *testing.T) {
	repo := deregTestRepo(t)
	ext, _, _ := hiddenForeignWorktree(t, repo)
	never := filepath.Join(t.TempDir(), "never-registered")

	if err := deregisterWorktree(context.Background(), acceptanceTreeGit(repo), never); err != nil {
		t.Fatalf("unregistered path must be a no-op, got %v", err)
	}
	assertForeignWorktreeSurvives(t, repo, ext)
}

// stubDeregGit is a gitAdminRunner whose `worktree remove` always fails and
// whose `worktree list --porcelain` returns list / listErr.
func stubDeregGit(list string, listErr error) gitAdminRunner {
	return func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case len(args) >= 2 && args[0] == "worktree" && args[1] == "remove":
			return []byte("fatal: stub refuses"), errors.New("exit status 128")
		case len(args) >= 2 && args[0] == "worktree" && args[1] == "list":
			return []byte(list), listErr
		}
		return nil, errors.New("unexpected git call: " + strings.Join(args, " "))
	}
}

// TestDeregisterWorktree_FailedRemoveStillRegisteredReturnsError (DD1c): a
// remove that fails while the listing still names the path is an error.
func TestDeregisterWorktree_FailedRemoveStillRegisteredReturnsError(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	list := "worktree /repo\nHEAD abc\n\nworktree " + target + "\nHEAD abc\ndetached\n"
	err := deregisterWorktree(context.Background(), stubDeregGit(list, nil), target)
	if err == nil || !strings.Contains(err.Error(), "stub refuses") {
		t.Fatalf("err = %v, want the failed remove reported", err)
	}
}

// TestDeregisterWorktree_MissingLeafAliasStillRegisteredReturnsError (DD1b/c):
// the listing names the symlink-RESOLVED path, the caller names a missing leaf
// through the alias. The parent-resolving canonicalization must still match,
// so the stranded registration is reported, not read as "not registered".
func TestDeregisterWorktree_MissingLeafAliasStillRegisteredReturnsError(t *testing.T) {
	realDir, alias := symlinkAlias(t)
	resolvedReal, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatal(err)
	}
	list := "worktree /repo\nHEAD abc\n\nworktree " + filepath.Join(resolvedReal, "target") + "\nHEAD abc\ndetached\n"
	err = deregisterWorktree(context.Background(), stubDeregGit(list, nil), filepath.Join(alias, "target"))
	if err == nil {
		t.Fatal("a registration listed under the resolved path must not read as unregistered")
	}
}

// TestCanonWorktreePath_MissingAncestorsUnderAlias: with the leaf AND its
// parent gone, the nearest existing ancestor is resolved, so an alias path
// still canonicalizes to the realpath git registered.
func TestCanonWorktreePath_MissingAncestorsUnderAlias(t *testing.T) {
	realDir, alias := symlinkAlias(t)
	resolvedReal, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatal(err)
	}
	got := canonWorktreePath(filepath.Join(alias, "gone-parent", "tree"))
	if want := filepath.Join(resolvedReal, "gone-parent", "tree"); got != want {
		t.Fatalf("canonWorktreePath = %q, want %q", got, want)
	}
}

// TestDeregisterWorktree_ListFailureReturnsError (DD1a): a failed listing never
// reads as "not registered".
func TestDeregisterWorktree_ListFailureReturnsError(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	err := deregisterWorktree(context.Background(), stubDeregGit("", errors.New("exit status 128")), target)
	if err == nil || !strings.Contains(err.Error(), "git worktree list") {
		t.Fatalf("err = %v, want the failed listing reported", err)
	}
}

// TestDeregisterWorktree_EmptyListReturnsError (DD1a): a listing with no
// worktree records (a working one always names the main worktree) never reads
// as "not registered".
func TestDeregisterWorktree_EmptyListReturnsError(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	err := deregisterWorktree(context.Background(), stubDeregGit("", nil), target)
	if err == nil || !strings.Contains(err.Error(), "no worktree records") {
		t.Fatalf("err = %v, want the empty listing reported", err)
	}
}

// TestNoRepoWideWorktreePrune fences every runner call site, including the
// conflict-resolution add-failure branch that no in-package fixture can drive
// with a foreign worktree registered: no non-test source in this package may
// pass a `"worktree", "prune"` literal to git (#4281).
func TestNoRepoWideWorktreePrune(t *testing.T) {
	re := regexp.MustCompile(`"worktree",\s*"prune"`)
	files, err := filepath.Glob(filepath.Join(pkgSrcDir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		if loc := re.FindIndex(src); loc != nil {
			line := 1 + strings.Count(string(src[:loc[0]]), "\n")
			t.Errorf("%s:%d runs a repo-wide `git worktree prune`; use deregisterWorktree on the owned path instead (#4281)",
				filepath.Base(f), line)
		}
	}
	if scanned == 0 {
		t.Fatalf("no non-test sources found under %s", pkgSrcDir)
	}
}
