package gateiso

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// cloneFixture is a real primary repository with main, an origin/main ref
// pinned to an OLDER commit than main (as a real checkout with unpushed
// commits looks), and a LINKED worktree carrying a further detached commit
// — the shape the runner clones from.
type cloneFixture struct {
	primary    string
	linked     string
	mainSHA    string
	originMain string
	headSHA    string
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func gitFails(t *testing.T, dir string, args ...string) bool {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	return cmd.Run() != nil
}

func newCloneFixture(t *testing.T) cloneFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	primary := filepath.Join(root, "primary")
	if err := os.Mkdir(primary, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, primary, "init", "-q", "-b", "main")
	gitT(t, primary, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(primary, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, primary, "add", "a.txt")
	gitT(t, primary, "commit", "-q", "-m", "one")
	originMain := gitT(t, primary, "rev-parse", "HEAD")
	gitT(t, primary, "update-ref", "refs/remotes/origin/main", originMain)
	if err := os.WriteFile(filepath.Join(primary, "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, primary, "commit", "-q", "-am", "two")
	mainSHA := gitT(t, primary, "rev-parse", "HEAD")
	linked := filepath.Join(root, "linked")
	gitT(t, primary, "worktree", "add", "-q", "--detach", linked, mainSHA)
	if err := os.WriteFile(filepath.Join(linked, "b.txt"), []byte("three\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, linked, "add", "b.txt")
	gitT(t, linked, "commit", "-q", "-m", "three")
	headSHA := gitT(t, linked, "rev-parse", "HEAD")
	return cloneFixture{primary: primary, linked: linked, mainSHA: mainSHA, originMain: originMain, headSHA: headSHA}
}

func TestMaterializeClone_FromLinkedWorktree(t *testing.T) {
	fx := newCloneFixture(t)
	dest := filepath.Join(t.TempDir(), "clone")
	rep, err := MaterializeClone(context.Background(), "", fx.linked, fx.headSHA, dest)
	if err != nil {
		t.Fatalf("MaterializeClone: %v", err)
	}
	if rep.HeadSHA != fx.headSHA {
		t.Fatalf("report HeadSHA = %s, want %s", rep.HeadSHA, fx.headSHA)
	}
	if got := gitT(t, dest, "rev-parse", "HEAD"); got != fx.headSHA {
		t.Fatalf("clone HEAD = %s, want %s (the linked worktree's commit)", got, fx.headSHA)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "b.txt")); err != nil || string(b) != "three\n" {
		t.Fatalf("clone work tree not checked out at head: %v %q", err, b)
	}

	// Independent .git: a real directory that is also the common dir.
	indep, err := IsIndependentGitDir(context.Background(), "", dest)
	if err != nil || !indep {
		t.Fatalf("IsIndependentGitDir(clone) = %v, %v; want true", indep, err)
	}
	if wtIndep, err := IsIndependentGitDir(context.Background(), "", fx.linked); err != nil || wtIndep {
		t.Fatalf("IsIndependentGitDir(linked worktree) = %v, %v; want false (control)", wtIndep, err)
	}
	if strings.Contains(gitT(t, fx.primary, "worktree", "list"), dest) {
		t.Fatalf("clone %s is registered as a worktree of the primary", dest)
	}

	// --no-hardlinks: every loose object is a COPY (Nlink == 1), never a
	// hardlink into the primary's object store.
	assertNoHardlinkedObjects(t, filepath.Join(dest, ".git", "objects"))

	// The source's origin/main (the OLDER commit) is mirrored — not the
	// source's main, which a plain local clone would map to origin/main.
	if got := gitT(t, dest, "rev-parse", "--verify", "refs/remotes/origin/main"); got != fx.originMain {
		t.Fatalf("clone origin/main = %s, want the primary's origin/main %s (main is %s)", got, fx.originMain, fx.mainSHA)
	}
	if !rep.RemoteRefsMirrored {
		t.Fatalf("report RemoteRefsMirrored = false: %s", rep.RemoteRefsError)
	}

	// origin url unset: nothing inside the clone can reach the primary by
	// its default remote.
	if !gitFails(t, dest, "config", "--get", "remote.origin.url") {
		t.Fatalf("remote.origin.url still set in clone")
	}
}

func assertNoHardlinkedObjects(t *testing.T, objects string) {
	t.Helper()
	checked := 0
	err := filepath.Walk(objects, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Skip("no Stat_t on this platform")
		}
		checked++
		if st.Nlink != 1 {
			t.Errorf("%s has Nlink=%d: object hardlinked into the primary (missing --no-hardlinks)", path, st.Nlink)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatalf("no object files under %s to check", objects)
	}
}

// TestMaterializeClone_PlantedRefAndHookNeverReachPrimary is the isolation
// property: a gate command that writes a hook or a ref inside the clone
// leaves the primary untouched. Its sibling below proves the pre-ADR-063
// `git worktree add --detach` materialization does NOT have this property,
// so the pair discriminates the clone from the worktree.
func TestMaterializeClone_PlantedRefAndHookNeverReachPrimary(t *testing.T) {
	fx := newCloneFixture(t)
	dest := filepath.Join(t.TempDir(), "clone")
	if _, err := MaterializeClone(context.Background(), "", fx.linked, fx.headSHA, dest); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(dest, ".git", "hooks", "pre-commit")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho planted\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, dest, "update-ref", "refs/heads/planted", "HEAD")
	if gitFails(t, dest, "rev-parse", "--verify", "--quiet", "refs/heads/planted") {
		t.Fatalf("planted ref missing from the clone itself")
	}

	if !gitFails(t, fx.primary, "rev-parse", "--verify", "--quiet", "refs/heads/planted") {
		t.Fatalf("planted ref reached the primary")
	}
	if _, err := os.Stat(filepath.Join(fx.primary, ".git", "hooks", "pre-commit")); err == nil {
		t.Fatalf("planted hook reached the primary")
	}
}

// TestWorktreeAdd_PlantedRefDoesReachPrimary is the discrimination sibling:
// under the OLD materialization the planted ref lands in the primary.
func TestWorktreeAdd_PlantedRefDoesReachPrimary(t *testing.T) {
	fx := newCloneFixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	gitT(t, fx.primary, "worktree", "add", "-q", "--detach", wt, fx.headSHA)
	t.Cleanup(func() { gitT(t, fx.primary, "worktree", "remove", "--force", wt) })
	gitT(t, wt, "update-ref", "refs/heads/planted-wt", "HEAD")
	if gitFails(t, fx.primary, "rev-parse", "--verify", "--quiet", "refs/heads/planted-wt") {
		t.Fatalf("expected the worktree-planted ref to reach the primary (shared refs); it did not — the discrimination is lost")
	}
}

func TestMaterializeClone_UnknownSHAIsNamedError(t *testing.T) {
	fx := newCloneFixture(t)
	dest := filepath.Join(t.TempDir(), "clone")
	_, err := MaterializeClone(context.Background(), "", fx.linked, "0123456789abcdef0123456789abcdef01234567", dest)
	if !errors.Is(err, ErrCommitNotFound) {
		t.Fatalf("err = %v, want ErrCommitNotFound", err)
	}
	if !strings.Contains(err.Error(), "0123456789abcdef") {
		t.Fatalf("error does not name the sha: %v", err)
	}
}

func TestMaterializeClone_ArgumentErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := MaterializeClone(ctx, "", "", "abc", t.TempDir()+"/x"); err == nil {
		t.Fatal("empty repoDir accepted")
	}
	if _, err := MaterializeClone(ctx, "", t.TempDir(), "abc", ""); err == nil {
		t.Fatal("empty dest accepted")
	}
	if _, err := MaterializeClone(ctx, "", t.TempDir(), " ", t.TempDir()+"/x"); err == nil {
		t.Fatal("blank head accepted")
	}
	if _, err := MaterializeClone(ctx, "", filepath.Join(t.TempDir(), "nope"), "abc", t.TempDir()+"/x"); err == nil {
		t.Fatal("missing source accepted")
	}
}

func TestIsIndependentGitDir_NonRepo(t *testing.T) {
	if _, err := IsIndependentGitDir(context.Background(), "", t.TempDir()); err == nil {
		t.Fatal("expected an error for a dir without .git")
	}
}

func TestIsIndependentGitDir_CorruptDotGitDirIsNamedError(t *testing.T) {
	dir := t.TempDir()
	// A .git DIRECTORY that is not a repository: Lstat passes, rev-parse fails.
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := IsIndependentGitDir(context.Background(), "", dir)
	if err == nil || !strings.Contains(err.Error(), "git-common-dir of") {
		t.Fatalf("err = %v, want the named rev-parse failure", err)
	}
}
