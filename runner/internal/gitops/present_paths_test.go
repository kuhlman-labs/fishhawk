package gitops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// presentPathsRepo builds the PresentPaths fixture BY CONSTRUCTION: a commit
// tracking gone_tracked.go and star*.go (both then deleted on disk, so they
// exist only in the commit), plus untracked on-disk files untracked.go and
// glob[ab].txt (so they exist only on disk). It returns the repo and the
// commit SHA.
func presentPathsRepo(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	for _, name := range []string{"gone_tracked.go", "star*.go"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte("package p\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "tracked files")
	head := mustGitOut(t, repo, "rev-parse", "HEAD")
	for _, name := range []string{"gone_tracked.go", "star*.go"} {
		if err := os.Remove(filepath.Join(repo, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"untracked.go", "glob[ab].txt"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return repo, head
}

func mustPresentPaths(t *testing.T, repo, head string, paths []string) []string {
	t.Helper()
	got, err := PresentPaths(context.Background(), repo, head, paths)
	if err != nil {
		t.Fatalf("PresentPaths: %v", err)
	}
	return got
}

// TestPresentPaths_KeepsOnDiskPath: an untracked drift file still on disk is
// kept although the commit does not carry it.
func TestPresentPaths_KeepsOnDiskPath(t *testing.T) {
	repo, head := presentPathsRepo(t)
	if got := mustPresentPaths(t, repo, head, []string{"untracked.go"}); !reflect.DeepEqual(got, []string{"untracked.go"}) {
		t.Errorf("PresentPaths = %v, want [untracked.go] (on disk, not in the commit)", got)
	}
}

// TestPresentPaths_KeepsCommitOnlyPath: a tracked file the agent deleted on
// disk is still in the held commit, so it is kept.
func TestPresentPaths_KeepsCommitOnlyPath(t *testing.T) {
	repo, head := presentPathsRepo(t)
	if got := mustPresentPaths(t, repo, head, []string{"gone_tracked.go"}); !reflect.DeepEqual(got, []string{"gone_tracked.go"}) {
		t.Errorf("PresentPaths = %v, want [gone_tracked.go] (deleted on disk, still in the commit)", got)
	}
}

// TestPresentPaths_DropsPathPresentNowhere: a drift path absent from the
// commit AND the working tree is dropped, and the surviving order holds.
func TestPresentPaths_DropsPathPresentNowhere(t *testing.T) {
	repo, head := presentPathsRepo(t)
	in := []string{"untracked.go", "junk 2.go", "gone_tracked.go", "pkg/vanished.go"}
	want := []string{"untracked.go", "gone_tracked.go"}
	if got := mustPresentPaths(t, repo, head, in); !reflect.DeepEqual(got, want) {
		t.Errorf("PresentPaths(%v) = %v, want %v (absent-everywhere paths dropped, order preserved)", in, got, want)
	}
}

// TestPresentPaths_GlobNamedPathsAreLiteral: glob metacharacters are matched
// literally in both lookups. A file named glob[ab].txt on disk and a commit
// file named star*.go are kept; *.txt and star?.go, which would match them as
// patterns, are dropped.
func TestPresentPaths_GlobNamedPathsAreLiteral(t *testing.T) {
	repo, head := presentPathsRepo(t)
	in := []string{"glob[ab].txt", "*.txt", "star*.go", "star?.go"}
	want := []string{"glob[ab].txt", "star*.go"}
	if got := mustPresentPaths(t, repo, head, in); !reflect.DeepEqual(got, want) {
		t.Errorf("PresentPaths(%v) = %v, want %v", in, got, want)
	}
}

// TestPresentPaths_KeepsPorcelainEscapedPath pins approval condition G2: a
// path that still carries a porcelain C-escape is kept unexamined. The
// escaped path is taken from real `git status --porcelain` output for an
// on-disk non-ASCII file name, so neither lookup can resolve the escaped
// string and only the escape guard keeps it. A quote-wrapped string a caller
// did not trim is kept the same way.
func TestPresentPaths_KeepsPorcelainEscapedPath(t *testing.T) {
	repo, head := presentPathsRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "café.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var escaped string
	for _, line := range strings.Split(mustGitOut(t, repo, "-c", "core.quotePath=true", "status", "--porcelain", "-uall"), "\n") {
		if p := porcelainPath(line); strings.HasPrefix(p, "caf") {
			escaped = p
		}
	}
	if !strings.ContainsRune(escaped, '\\') {
		t.Fatalf("fixture: porcelain path for café.txt = %q, want a C-escaped form carrying a backslash", escaped)
	}
	in := []string{escaped, `"wrapped.txt"`}
	if got := mustPresentPaths(t, repo, head, in); !reflect.DeepEqual(got, in) {
		t.Errorf("PresentPaths(%q) = %q, want both kept (an undecoded C-escape fails open)", in, got)
	}
}

// TestPresentPaths_UnresolvableCommitErrors: `git cat-file -e` exits non-zero
// for a bad object and for a missing path alike, so an unresolvable commit
// must be an error (the caller fails open), never "every path absent".
func TestPresentPaths_UnresolvableCommitErrors(t *testing.T) {
	repo, _ := presentPathsRepo(t)
	got, err := PresentPaths(context.Background(), repo, "0123456789abcdef0123456789abcdef01234567", []string{"junk 2.go"})
	if err == nil {
		t.Fatalf("PresentPaths with an unresolvable commit = %v, nil; want an error", got)
	}
	if !strings.Contains(err.Error(), "resolve commit") {
		t.Errorf("error %q, want it to name the commit resolution", err)
	}
}
