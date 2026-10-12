package basedelta

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/reviewsandbox"
)

// gitRepo is a real, remote-less fixture repository. Nothing is pushed, so the
// #3503 maintenance-child trap does not apply; auto maintenance is disabled
// anyway because `git merge` runs it.
type gitRepo struct {
	t   *testing.T
	dir string
}

func newGitRepo(t *testing.T) *gitRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git not on PATH: %v", err)
	}
	r := &gitRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *gitRepo) git(args ...string) string {
	r.t.Helper()
	full := append([]string{"-C", r.dir,
		"-c", "user.name=fixture", "-c", "user.email=fixture@example.com",
		"-c", "commit.gpgsign=false", "-c", "maintenance.auto=false", "-c", "gc.auto=0",
		"-c", "core.symlinks=true"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes files (a "" body deletes the path) and commits them.
func (r *gitRepo) commit(msg string, files map[string]string) string {
	r.t.Helper()
	for rel, body := range files {
		p := filepath.Join(r.dir, filepath.FromSlash(rel))
		if body == "" {
			r.git("rm", "-q", rel)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", msg)
	return r.head()
}

// commitIndex commits the index as staged (no `add -A`), keeping update-index
// edits such as a gitlink or a mode change.
func (r *gitRepo) commitIndex(msg string) string {
	r.t.Helper()
	r.git("commit", "-q", "-m", msg)
	return r.head()
}

func (r *gitRepo) head() string { return r.git("rev-parse", "HEAD") }

// merge merges `from` into the checked-out branch with a merge commit.
func (r *gitRepo) merge(from string) string {
	r.t.Helper()
	r.git("merge", "-q", "--no-ff", "--no-edit", from)
	return r.head()
}

// advance builds the canonical fixture: base B (a/a.go, b/b.go), the PR head H
// on branch pr editing a/a.go, main advanced to B' by mainFiles, and the merge
// candidate H' = merge of main into pr (parents H, B').
type advance struct{ base, head, bPrime, candidate string }

func (r *gitRepo) advance(mainFiles map[string]string) advance {
	r.t.Helper()
	var a advance
	a.base = r.commit("base", map[string]string{"a/a.go": "package a\n", "b/b.go": "package b\n"})
	r.git("checkout", "-q", "-b", "pr")
	a.head = r.commit("pr", map[string]string{"a/a.go": "package a\n\nconst X = 1\n"})
	r.git("checkout", "-q", "main")
	a.bPrime = r.commit("advance", mainFiles)
	r.git("checkout", "-q", "pr")
	a.candidate = r.merge("main")
	return a
}

func TestGitDelta_CleanAdvance(t *testing.T) {
	t.Parallel()
	r := newGitRepo(t)
	a := r.advance(map[string]string{"b/b.go": "package b\n\nconst Y = 2\n"})
	got, err := GitDelta(context.Background(), r.dir, a.candidate, a.head, a.head)
	if err != nil {
		t.Fatalf("GitDelta: %v", err)
	}
	want := DeltaInputs{FromBase: a.base, ToBase: a.bPrime, DeltaPaths: []string{"b/b.go"}, PRPaths: []string{"a/a.go"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GitDelta = %+v, want %+v", got, want)
	}
}

// A chain H -> H1' -> H2': classified against the ROOT head H, the delta is
// cumulative from the base the real pass verified, not H1'..B2'.
func TestGitDelta_TransferChainUsesRootBase(t *testing.T) {
	t.Parallel()
	r := newGitRepo(t)
	a := r.advance(map[string]string{"b/b.go": "package b\n\nconst Y = 2\n"})
	r.git("checkout", "-q", "main")
	b2 := r.commit("advance 2", map[string]string{"docs.txt": "x\n"})
	r.git("checkout", "-q", "pr")
	c2 := r.merge("main")

	got, err := GitDelta(context.Background(), r.dir, c2, a.candidate, a.head)
	if err != nil {
		t.Fatalf("GitDelta: %v", err)
	}
	want := DeltaInputs{FromBase: a.base, ToBase: b2, DeltaPaths: []string{"b/b.go", "docs.txt"}, PRPaths: []string{"a/a.go"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GitDelta(root=H) = %+v, want %+v", got, want)
	}
	// Anchored on the intermediate head instead, only the second advance shows.
	mid, err := GitDelta(context.Background(), r.dir, c2, a.candidate, a.candidate)
	if err != nil {
		t.Fatalf("GitDelta(root=H1'): %v", err)
	}
	if mid.FromBase != a.bPrime || !reflect.DeepEqual(mid.DeltaPaths, []string{"docs.txt"}) {
		t.Fatalf("GitDelta(root=H1') = %+v, want from %s delta [docs.txt]", mid, a.bPrime)
	}
}

// Counterfactual (9): without --no-renames git reports only b/c.go.
func TestGitDelta_RenameReportsBothSides(t *testing.T) {
	t.Parallel()
	r := newGitRepo(t)
	body := "package b\n\n// a body long enough that the rename is detected as similar\nconst Y = 2\n"
	r.commit("base", map[string]string{"a/a.go": "package a\n", "b/b.go": body})
	r.git("checkout", "-q", "-b", "pr")
	head := r.commit("pr", map[string]string{"a/a.go": "package a\n\nconst X = 1\n"})
	r.git("checkout", "-q", "main")
	r.git("mv", "b/b.go", "b/c.go")
	r.commit("rename", nil)
	r.git("checkout", "-q", "pr")
	cand := r.merge("main")

	got, err := GitDelta(context.Background(), r.dir, cand, head, head)
	if err != nil {
		t.Fatalf("GitDelta: %v", err)
	}
	if want := []string{"b/b.go", "b/c.go"}; !reflect.DeepEqual(got.DeltaPaths, want) {
		t.Fatalf("DeltaPaths = %v, want %v (both sides of the rename)", got.DeltaPaths, want)
	}
}

func TestGitDelta_UnexpectedParents(t *testing.T) {
	t.Parallel()
	t.Run("single-parent", func(t *testing.T) {
		t.Parallel()
		r := newGitRepo(t)
		a := r.advance(map[string]string{"b/b.go": "package b\n\nconst Y = 2\n"})
		plain := r.commit("plain", map[string]string{"a/z.go": "package a\n"})
		_, err := GitDelta(context.Background(), r.dir, plain, a.candidate, a.head)
		if !errors.Is(err, ErrUnexpectedParents) {
			t.Fatalf("err = %v, want ErrUnexpectedParents", err)
		}
	})
	// Counterfactual (7): the merge was made from main's side, so parent 1 is
	// B' and parent 2 is H. Without the parent-1 check H would be taken as B'.
	t.Run("first-parent", func(t *testing.T) {
		t.Parallel()
		r := newGitRepo(t)
		a := r.advance(map[string]string{"b/b.go": "package b\n\nconst Y = 2\n"})
		r.git("checkout", "-q", "main")
		r.git("reset", "-q", "--hard", a.bPrime)
		reversed := r.merge(a.head)
		_, err := GitDelta(context.Background(), r.dir, reversed, a.head, a.head)
		if !errors.Is(err, ErrUnexpectedParents) || !strings.Contains(err.Error(), "first parent") {
			t.Fatalf("err = %v, want ErrUnexpectedParents naming the first parent", err)
		}
	})
	t.Run("octopus", func(t *testing.T) {
		t.Parallel()
		r := newGitRepo(t)
		r.commit("base", map[string]string{"a/a.go": "package a\n"})
		r.git("checkout", "-q", "-b", "x")
		r.commit("x", map[string]string{"x.txt": "x\n"})
		r.git("checkout", "-q", "-b", "y", "main")
		r.commit("y", map[string]string{"y.txt": "y\n"})
		r.git("checkout", "-q", "-b", "pr", "main")
		head := r.commit("pr", map[string]string{"a/a.go": "package a\n\nconst X = 1\n"})
		r.git("merge", "-q", "--no-ff", "--no-edit", "x", "y")
		_, err := GitDelta(context.Background(), r.dir, r.head(), head, head)
		if !errors.Is(err, ErrUnexpectedParents) {
			t.Fatalf("err = %v, want ErrUnexpectedParents", err)
		}
	})
}

func TestGitDelta_RootNotAncestor(t *testing.T) {
	t.Parallel()
	r := newGitRepo(t)
	a := r.advance(map[string]string{"b/b.go": "package b\n\nconst Y = 2\n"})
	_, err := GitDelta(context.Background(), r.dir, a.candidate, a.head, a.bPrime)
	if !errors.Is(err, ErrNotAncestor) {
		t.Fatalf("err = %v, want ErrNotAncestor", err)
	}
}

// Counterfactual (8): criss-cross history gives root and B' exactly two merge
// bases; taking the first would silently pick one.
func TestGitDelta_AmbiguousMergeBase(t *testing.T) {
	t.Parallel()
	r := newGitRepo(t)
	r.commit("base", map[string]string{"a/a.go": "package a\n"})
	r.git("checkout", "-q", "-b", "pr")
	x1 := r.commit("x1", map[string]string{"x.txt": "x\n"})
	r.git("checkout", "-q", "main")
	y1 := r.commit("y1", map[string]string{"y.txt": "y\n"})
	r.merge(x1) // main: Y2 = merge(Y1, X1)
	r.git("checkout", "-q", "pr")
	head := r.merge(y1) // pr: X2 = merge(X1, Y1)
	cand := r.merge("main")
	if bases := strings.Fields(r.git("merge-base", "--all", head, "main")); len(bases) != 2 {
		t.Fatalf("fixture: merge bases = %v, want 2 (criss-cross)", bases)
	}
	_, err := GitDelta(context.Background(), r.dir, cand, head, head)
	if !errors.Is(err, ErrAmbiguousMergeBase) {
		t.Fatalf("err = %v, want ErrAmbiguousMergeBase", err)
	}
}

func TestGitDelta_NoMergeBase(t *testing.T) {
	t.Parallel()
	r := newGitRepo(t)
	head := r.commit("pr", map[string]string{"a/a.go": "package a\n"})
	r.git("checkout", "-q", "--orphan", "other")
	r.git("rm", "-rq", "--cached", ".")
	other := r.commit("unrelated", map[string]string{"b/b.go": "package b\n"})
	r.git("checkout", "-q", "-f", "main")
	r.git("merge", "-q", "--no-ff", "--no-edit", "--allow-unrelated-histories", other)
	_, err := GitDelta(context.Background(), r.dir, r.head(), head, head)
	if !errors.Is(err, ErrNoMergeBase) {
		t.Fatalf("err = %v, want ErrNoMergeBase", err)
	}
}

func TestGitDelta_NonSHAInputsRefused(t *testing.T) {
	t.Parallel()
	full := strings.Repeat("a", 40)
	bad := []string{"HEAD", "abc1234", "-x", "", strings.Repeat("A", 40), strings.Repeat("a", 41), "--output=/tmp/x" + strings.Repeat("0", 25)}
	for _, b := range bad {
		for pos := 0; pos < 3; pos++ {
			ids := []string{full, full, full}
			ids[pos] = b
			var runs atomic.Int64
			counting := func(ctx context.Context, dir string, args ...string) ([]byte, error) {
				runs.Add(1)
				return runGit(ctx, dir, args...)
			}
			_, err := gitDelta(context.Background(), counting, t.TempDir(), ids[0], ids[1], ids[2])
			if !errors.Is(err, ErrInvalidObjectID) {
				t.Fatalf("input %q at %d: err = %v, want ErrInvalidObjectID", b, pos, err)
			}
			if n := runs.Load(); n != 0 {
				t.Fatalf("input %q at %d: %d git children ran, want 0", b, pos, n)
			}
		}
	}
	if err := MaterializeTree(context.Background(), t.TempDir(), "HEAD", t.TempDir()); !errors.Is(err, ErrInvalidObjectID) {
		t.Fatalf("MaterializeTree(HEAD) err = %v, want ErrInvalidObjectID", err)
	}
	if _, err := GitDelta(context.Background(), "", full, full, full); !errors.Is(err, ErrGit) {
		t.Fatalf("empty repo dir err = %v, want ErrGit", err)
	}
}

// A long stderr is cut to its last 256 bytes, on one line.
func TestGitDelta_GitFailureNamesBoundedStderr(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for i := 0; i < 6; i++ {
		dir = filepath.Join(dir, strings.Repeat("d", 200))
	}
	full := strings.Repeat("a", 40)
	_, err := GitDelta(context.Background(), dir, full, full, full)
	if !errors.Is(err, ErrGit) || InputsErrorCode(err) != "git" {
		t.Fatalf("err = %v, want ErrGit", err)
	}
	msg := err.Error()
	_, tail, ok := strings.Cut(msg, "exit status 128: ")
	if !ok {
		t.Fatalf("err %q carries no exit status and stderr tail", msg)
	}
	if len(tail) > gitStderrTail || len(tail) < gitStderrTail-8 || strings.Contains(tail, "\n") {
		t.Fatalf("stderr tail is %d bytes (want a single line of <= %d, cut from a longer stderr): %q", len(tail), gitStderrTail, tail)
	}
}

func TestGitDelta_ContextDeadline(t *testing.T) {
	t.Parallel()
	r := newGitRepo(t)
	a := r.advance(map[string]string{"b/b.go": "package b\n\nconst Y = 2\n"})
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := GitDelta(ctx, r.dir, a.candidate, a.head, a.head)
	if got := InputsErrorCode(err); got != "deadline_exceeded" {
		t.Fatalf("InputsErrorCode(%v) = %q, want deadline_exceeded", err, got)
	}
}

func TestInputsErrorCode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, ""},
		{ErrInvalidObjectID, "invalid_object_id"},
		{ErrUnexpectedParents, "unexpected_parents"},
		{ErrNotAncestor, "not_ancestor"},
		{ErrAmbiguousMergeBase, "ambiguous_merge_base"},
		{ErrNoMergeBase, "no_merge_base"},
		{ErrIncompleteTree, "incomplete_tree"},
		{ErrUnsafeTreePath, "unsafe_tree_path"},
		{ErrGit, "git"},
		{errors.New("other"), "unknown"},
	} {
		if got := InputsErrorCode(tc.err); got != tc.want {
			t.Errorf("InputsErrorCode(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestMaterializeTree_KeepsExportIgnoredAndSymlinks(t *testing.T) {
	t.Parallel()
	r := newGitRepo(t)
	r.commit("base", map[string]string{
		".gitattributes": "c/** export-ignore\n",
		"a/a.go":         "package a\n",
		"c/c.go":         "package c\n",
		"run.sh":         "#!/bin/sh\n",
	})
	if err := os.Symlink("a/a.go", filepath.Join(r.dir, "link.go")); err != nil {
		t.Fatal(err)
	}
	r.git("add", "link.go")
	r.git("update-index", "--chmod=+x", "run.sh")
	sha := r.commitIndex("link")
	dest := t.TempDir()
	if err := MaterializeTree(context.Background(), r.dir, sha, dest); err != nil {
		t.Fatalf("MaterializeTree: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "c", "c.go")); err != nil || string(b) != "package c\n" {
		t.Fatalf("export-ignored c/c.go = %q, %v; want kept", b, err)
	}
	if target, err := os.Readlink(filepath.Join(dest, "link.go")); err != nil || target != "a/a.go" {
		t.Fatalf("link.go -> %q, %v; want symlink to a/a.go", target, err)
	}
	if fi, err := os.Stat(filepath.Join(dest, "run.sh")); err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("run.sh mode = %v, %v; want executable", fi, err)
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); !os.IsNotExist(err) {
		t.Fatalf("dest/.git exists (err %v); MaterializeTree writes only the tree", err)
	}
}

// Approval condition S0/S2: a dependent package excluded by export-ignore that
// imports BOTH changed packages makes them dependent. `git archive`
// (reviewsandbox.ExportTree) drops it and the classifier answers independent;
// MaterializeTree keeps it and the answer is reach_overlap:c.
func TestMaterializeTree_ExportIgnoredDependentNeverIndependent(t *testing.T) {
	t.Parallel()
	requireGo(t)
	r := newGitRepo(t)
	base := r.commit("base", map[string]string{
		".gitattributes": "c/** export-ignore\n",
		"go.mod":         "module example.com/m\n\ngo 1.25.0\n",
		"a/a.go":         "package a\n",
		"b/b.go":         "package b\n",
		"c/c.go":         "package c\n\nimport (\n\t_ \"example.com/m/a\"\n\t_ \"example.com/m/b\"\n)\n",
	})
	r.git("checkout", "-q", "-b", "pr")
	head := r.commit("pr", map[string]string{"a/a.go": "package a\n\nconst X = 1\n"})
	r.git("checkout", "-q", "main")
	bPrime := r.commit("advance", map[string]string{"b/b.go": "package b\n\nconst Y = 2\n"})
	r.git("checkout", "-q", "pr")
	cand := r.merge("main")

	in, err := GitDelta(context.Background(), r.dir, cand, head, head)
	if err != nil {
		t.Fatalf("GitDelta: %v", err)
	}
	if in.FromBase != base {
		t.Fatalf("FromBase = %s, want %s", in.FromBase, base)
	}
	cls := &Classifier{Loader: &GoListLoader{Env: emptyModCache(t)}}

	// Precondition: the fixture discriminates — an archive export drops c.
	var archived []string
	for _, sha := range []string{bPrime, cand} {
		dir, _, _, cleanup, err := reviewsandbox.ExportTree(context.Background(), r.dir, sha, reviewsandbox.DefaultLimits())
		if err != nil {
			t.Fatalf("ExportTree(%s): %v", sha, err)
		}
		t.Cleanup(cleanup)
		archived = append(archived, dir)
	}
	if res := cls.Classify(context.Background(), Input{DeltaPaths: in.DeltaPaths, PRPaths: in.PRPaths, Workspaces: archived}); !res.Independent {
		t.Fatalf("fixture: archive-export classification = %+v, want independent (c dropped)", res)
	}

	var trees []string
	for _, sha := range []string{bPrime, cand} {
		dest := t.TempDir()
		if err := MaterializeTree(context.Background(), r.dir, sha, dest); err != nil {
			t.Fatalf("MaterializeTree(%s): %v", sha, err)
		}
		trees = append(trees, dest)
	}
	res := cls.Classify(context.Background(), Input{DeltaPaths: in.DeltaPaths, PRPaths: in.PRPaths, Workspaces: trees})
	if res.Independent || res.Reason != "reach_overlap:c" {
		t.Fatalf("Classify over materialized trees = %+v, want reach_overlap:c", res)
	}
}

func TestMaterializeTree_SubmoduleIncomplete(t *testing.T) {
	t.Parallel()
	r := newGitRepo(t)
	first := r.commit("base", map[string]string{"a/a.go": "package a\n"})
	r.git("update-index", "--add", "--cacheinfo", "160000,"+first+",sub")
	sha := r.commitIndex("gitlink")
	err := MaterializeTree(context.Background(), r.dir, sha, t.TempDir())
	if !errors.Is(err, ErrIncompleteTree) {
		t.Fatalf("err = %v, want ErrIncompleteTree", err)
	}
}

func TestMaterializeTree_GitFailure(t *testing.T) {
	t.Parallel()
	r := newGitRepo(t)
	r.commit("base", map[string]string{"a/a.go": "package a\n"})
	err := MaterializeTree(context.Background(), r.dir, strings.Repeat("0", 40), t.TempDir())
	if !errors.Is(err, ErrGit) {
		t.Fatalf("err = %v, want ErrGit", err)
	}
}

func TestCheckTreePath(t *testing.T) {
	t.Parallel()
	for _, p := range []string{"a/a.go", ".gitattributes", "x/.github/y", "..a/b"} {
		if err := checkTreePath(p); err != nil {
			t.Errorf("checkTreePath(%q) = %v, want nil", p, err)
		}
	}
	for _, p := range []string{"", "/etc/passwd", "../x", "a/../b", "a//b", "./a", "a/.", ".git/config", "x/.GIT/hooks/post", "a\\b", "a\x00b"} {
		if err := checkTreePath(p); !errors.Is(err, ErrUnsafeTreePath) {
			t.Errorf("checkTreePath(%q) = %v, want ErrUnsafeTreePath", p, err)
		}
	}
}

func TestParseLsTree_Refusals(t *testing.T) {
	t.Parallel()
	oid := strings.Repeat("a", 40)
	for name, tc := range map[string]struct {
		rec  string
		want error
	}{
		"malformed":  {"garbage", ErrGit},
		"tree mode":  {"040000 tree " + oid + "\tdir", ErrIncompleteTree},
		"short oid":  {"100644 blob abc\tf", ErrGit},
		"unsafe":     {"100644 blob " + oid + "\t../f", ErrUnsafeTreePath},
		"gitlink":    {"160000 commit " + oid + "\tsub", ErrIncompleteTree},
		"other mode": {"100664 blob " + oid + "\tf", ErrIncompleteTree},
	} {
		if _, err := parseLsTree([]byte(tc.rec + "\x00")); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

// Symlinks are created after every regular file and files are O_EXCL: on a
// case-folding filesystem a tree holding symlink L -> outside and file l/x.go
// must fail, never write through L. On a case-sensitive one both entries land
// inside dest. Either way nothing is written outside dest.
func TestMaterializeTree_CaseFoldSymlinkNeverEscapes(t *testing.T) {
	t.Parallel()
	r := newGitRepo(t)
	outside := t.TempDir()
	cmd := exec.Command("git", "-C", r.dir, "hash-object", "-w", "--stdin")
	cmd.Stdin = strings.NewReader(outside)
	link, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "-C", r.dir, "hash-object", "-w", "--stdin")
	cmd.Stdin = strings.NewReader("package l\n")
	file, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	r.git("update-index", "--add", "--cacheinfo", "120000,"+strings.TrimSpace(string(link))+",L")
	r.git("update-index", "--add", "--cacheinfo", "100644,"+strings.TrimSpace(string(file))+",l/x.go")
	sha := r.commitIndex("case fold")
	dest := t.TempDir()
	err = MaterializeTree(context.Background(), r.dir, sha, dest)
	if _, serr := os.Stat(filepath.Join(outside, "x.go")); !os.IsNotExist(serr) {
		t.Fatalf("x.go was written outside dest through symlink L (stat err %v, MaterializeTree err %v)", serr, err)
	}
	if err != nil && !errors.Is(err, ErrIncompleteTree) {
		t.Fatalf("err = %v, want nil or ErrIncompleteTree", err)
	}
}

// O_EXCL: on a case-folding filesystem A.go and a.go collide and the second
// must fail rather than silently overwrite the first; on a case-sensitive one
// both keep their own bytes.
func TestMaterializeTree_CaseFoldFileCollisionFails(t *testing.T) {
	t.Parallel()
	r := newGitRepo(t)
	for name, body := range map[string]string{"A.go": "package upper\n", "a.go": "package lower\n"} {
		cmd := exec.Command("git", "-C", r.dir, "hash-object", "-w", "--stdin")
		cmd.Stdin = strings.NewReader(body)
		oid, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		r.git("update-index", "--add", "--cacheinfo", "100644,"+strings.TrimSpace(string(oid))+","+name)
	}
	sha := r.commitIndex("case fold files")
	dest := t.TempDir()
	err := MaterializeTree(context.Background(), r.dir, sha, dest)
	if err != nil {
		if !errors.Is(err, ErrIncompleteTree) {
			t.Fatalf("err = %v, want nil or ErrIncompleteTree", err)
		}
		return
	}
	for name, want := range map[string]string{"A.go": "package upper\n", "a.go": "package lower\n"} {
		if b, rerr := os.ReadFile(filepath.Join(dest, name)); rerr != nil || string(b) != want {
			t.Fatalf("%s = %q, %v; want %q (a case-fold collision overwrote a file)", name, b, rerr, want)
		}
	}
}

// fakeGit answers each git child from a queue keyed by its subcommand
// ("rev-list", "merge-base --is-ancestor", "merge-base --all", "diff").
type fakeAnswer struct {
	out string
	err error
}

func fakeGit(t *testing.T, answers map[string][]fakeAnswer) gitRunner {
	return func(_ context.Context, _ string, args ...string) ([]byte, error) {
		key := args[0]
		if key == "merge-base" {
			key += " " + args[1]
		}
		q := answers[key]
		if len(q) == 0 {
			t.Fatalf("unexpected git child %v", args)
		}
		answers[key] = q[1:]
		return []byte(q[0].out), q[0].err
	}
}

// The failure arms gitDelta reaches only on malformed or failing git output.
func TestGitDelta_SeamFailures(t *testing.T) {
	t.Parallel()
	cand, prior, toBase, fromBase := strings.Repeat("1", 40), strings.Repeat("2", 40), strings.Repeat("4", 40), strings.Repeat("5", 40)
	parents := fakeAnswer{out: cand + " " + prior + " " + toBase + "\n"}
	ok := fakeAnswer{}
	base := fakeAnswer{out: fromBase + "\n"}
	gitFail := fakeAnswer{err: fmt.Errorf("%w: git x: %w", ErrGit, &gitExitError{code: 128, msg: "exit status 128: fatal"})}
	for name, tc := range map[string]struct {
		answers  map[string][]fakeAnswer
		want     error
		contains string
	}{
		"malformed parent": {
			answers:  map[string][]fakeAnswer{"rev-list": {{out: cand + " " + prior + " " + strings.Repeat("z", 80) + "\n"}}},
			want:     ErrGit,
			contains: "…",
		},
		"is-ancestor non-exit failure": {
			answers: map[string][]fakeAnswer{"rev-list": {parents}, "merge-base --is-ancestor": {{err: fmt.Errorf("%w: start: boom", ErrGit)}}},
			want:    ErrGit,
		},
		"merge-base exit 128": {
			answers: map[string][]fakeAnswer{"rev-list": {parents}, "merge-base --is-ancestor": {ok}, "merge-base --all": {gitFail}},
			want:    ErrGit,
		},
		"merge-base empty output": {
			answers: map[string][]fakeAnswer{"rev-list": {parents}, "merge-base --is-ancestor": {ok}, "merge-base --all": {ok}},
			want:    ErrNoMergeBase,
		},
		"merge-base malformed base": {
			answers: map[string][]fakeAnswer{"rev-list": {parents}, "merge-base --is-ancestor": {ok}, "merge-base --all": {{out: "xyz\n"}}},
			want:    ErrGit,
		},
		"delta diff failure": {
			answers: map[string][]fakeAnswer{"rev-list": {parents}, "merge-base --is-ancestor": {ok}, "merge-base --all": {base}, "diff": {gitFail}},
			want:    ErrGit,
		},
		"pr diff failure": {
			answers: map[string][]fakeAnswer{"rev-list": {parents}, "merge-base --is-ancestor": {ok}, "merge-base --all": {base}, "diff": {{out: "b/b.go\x00"}, gitFail}},
			want:    ErrGit,
		},
	} {
		_, err := gitDelta(context.Background(), fakeGit(t, tc.answers), t.TempDir(), cand, prior, prior)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
			continue
		}
		if errors.Is(err, ErrNotAncestor) || (tc.want != ErrNoMergeBase && errors.Is(err, ErrNoMergeBase)) {
			t.Errorf("%s: err = %v, misclassified as a history verdict", name, err)
		}
		if tc.contains != "" && !strings.Contains(err.Error(), tc.contains) {
			t.Errorf("%s: err = %v, want it to contain %q", name, err, tc.contains)
		}
	}
}

func TestWriteBatch_Refusals(t *testing.T) {
	t.Parallel()
	oid := strings.Repeat("a", 40)
	file := treeEntry{mode: "100644", oid: oid, path: "f.go"}
	link := treeEntry{mode: "120000", oid: oid, path: "L"}
	for name, tc := range map[string]struct {
		entry treeEntry
		out   string
		want  error
	}{
		"truncated":          {file, "", ErrGit},
		"oid mismatch":       {file, strings.Repeat("b", 40) + " blob 3\nabc\n", ErrGit},
		"not a blob":         {file, oid + " tree 3\nabc\n", ErrGit},
		"non-numeric size":   {file, oid + " blob x\nabc\n", ErrGit},
		"negative size":      {file, oid + " blob -1\n", ErrGit},
		"short symlink body": {link, oid + " blob 10\nabc", ErrGit},
		"short file body":    {file, oid + " blob 10\nabc", io.EOF},
		"missing terminator": {file, oid + " blob 3\nabcX", ErrGit},
		"missing final byte": {link, oid + " blob 3\nabc", ErrGit},
	} {
		err := writeBatch(bufio.NewReader(strings.NewReader(tc.out)), []treeEntry{tc.entry}, t.TempDir())
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

// A parent directory that is a regular file fails the write, for a file and a
// symlink alike, and is never classified as a git failure.
func TestWriteBatch_ParentIsAFile(t *testing.T) {
	t.Parallel()
	oid := strings.Repeat("a", 40)
	for _, mode := range []string{"100644", "120000"} {
		dest := t.TempDir()
		if err := os.WriteFile(filepath.Join(dest, "f"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := writeBatch(bufio.NewReader(strings.NewReader(oid+" blob 3\nabc\n")), []treeEntry{{mode: mode, oid: oid, path: "f/x"}}, dest)
		if err == nil || errors.Is(err, ErrGit) || !strings.Contains(err.Error(), "materialize") {
			t.Errorf("mode %s: err = %v, want a materialize error", mode, err)
		}
	}
}

func TestMaterializeTree_EmptyTreeAndRepoDir(t *testing.T) {
	t.Parallel()
	r := newGitRepo(t)
	sha := r.commit("empty", nil)
	dest := t.TempDir()
	if err := MaterializeTree(context.Background(), r.dir, sha, dest); err != nil {
		t.Fatalf("MaterializeTree(empty tree): %v", err)
	}
	if ents, err := os.ReadDir(dest); err != nil || len(ents) != 0 {
		t.Fatalf("dest = %v, %v; want empty", ents, err)
	}
	if err := MaterializeTree(context.Background(), "", sha, dest); !errors.Is(err, ErrGit) {
		t.Fatalf("empty repo dir err = %v, want ErrGit", err)
	}
}
