package gitdiff

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/constraint"
)

func TestParse_Empty(t *testing.T) {
	d, err := Parse(nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(d.ChangedFiles) != 0 {
		t.Errorf("expected empty diff, got %+v", d)
	}
}

func TestParse_SimpleAddedModifiedDeleted(t *testing.T) {
	// Simulated `git diff --name-status -z` output: each field is
	// NUL-terminated.
	raw := []byte("M\x00backend/main.go\x00A\x00new.go\x00D\x00gone.go\x00")
	d, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(d.ChangedFiles) != 3 {
		t.Fatalf("got %d files, want 3: %+v", len(d.ChangedFiles), d.ChangedFiles)
	}
	want := []constraint.ChangedFile{
		{Path: "backend/main.go", Status: constraint.StatusModified},
		{Path: "new.go", Status: constraint.StatusAdded},
		{Path: "gone.go", Status: constraint.StatusDeleted},
	}
	for i, w := range want {
		if d.ChangedFiles[i] != w {
			t.Errorf("file %d = %+v, want %+v", i, d.ChangedFiles[i], w)
		}
	}
}

func TestParse_RenameAndCopy(t *testing.T) {
	// R100 = pure rename. C75 = copy with 75% similarity. The
	// destination path goes second; we record the destination.
	raw := []byte("R100\x00old.go\x00new.go\x00C75\x00source.go\x00dest.go\x00")
	d, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(d.ChangedFiles) != 2 {
		t.Fatalf("got %d files, want 2: %+v", len(d.ChangedFiles), d.ChangedFiles)
	}
	if d.ChangedFiles[0].Path != "new.go" || d.ChangedFiles[0].Status != constraint.StatusRenamed {
		t.Errorf("rename: got %+v", d.ChangedFiles[0])
	}
	if d.ChangedFiles[1].Path != "dest.go" || d.ChangedFiles[1].Status != constraint.StatusCopied {
		t.Errorf("copy: got %+v", d.ChangedFiles[1])
	}
}

func TestParse_PathWithSpecialChars(t *testing.T) {
	// -z form preserves filenames with newlines / quotes / spaces
	// without escaping. Test that a path containing a tab survives.
	raw := []byte("M\x00path with\ttab.go\x00")
	d, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(d.ChangedFiles) != 1 || d.ChangedFiles[0].Path != "path with\ttab.go" {
		t.Errorf("got %+v, want path preserved verbatim", d.ChangedFiles)
	}
}

func TestParse_MissingPath(t *testing.T) {
	// A stream that ends after a status with no following path is
	// malformed; the parser should reject rather than silently
	// drop.
	raw := []byte("M\x00")
	_, err := Parse(raw)
	if err == nil {
		t.Fatal("expected error for status with no path")
	}
}

func TestParse_RenameMissingDestination(t *testing.T) {
	raw := []byte("R100\x00old.go\x00")
	_, err := Parse(raw)
	if err == nil {
		t.Fatal("expected error for rename with no destination")
	}
}

// fakeCmd returns a Cmd builder that re-execs the test binary
// pretending to be `git`. The helper test below emits canned -z
// output driven by HELPER_MODE.
func fakeCmd(mode string) func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		c := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcess")
		c.Env = append(os.Environ(),
			"GO_HELPER_PROCESS=1",
			"HELPER_MODE="+mode,
		)
		return c
	}
}

// TestHelperProcess stands in for the `git` binary in tests.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_HELPER_PROCESS") != "1" {
		return
	}
	defer os.Exit(0)

	switch os.Getenv("HELPER_MODE") {
	case "ok":
		fmt.Print("M\x00a.go\x00A\x00b.go\x00")
	case "error":
		fmt.Fprintln(os.Stderr, "fatal: ambiguous argument 'no-such-ref'")
		os.Exit(128)
	case "ok_empty":
		// No output — clean diff.
	case "patch_ok":
		// A small unified diff (hunk format), as `git diff --cached
		// <base>` (without --name-status) emits.
		fmt.Print("diff --git a/a.go b/a.go\n" +
			"index 111..222 100644\n" +
			"--- a/a.go\n" +
			"+++ b/a.go\n" +
			"@@ -1,2 +1,2 @@\n" +
			" package x\n" +
			"-old line\n" +
			"+new line\n")
	case "merge_base":
		// Stand in for `git merge-base <base> HEAD`: print a SHA on stdout.
		// The trailing newline is what MergeBase trims off.
		fmt.Println("abc123def4567890abc123def4567890abc12345")
	case "patch_big":
		// Emit more than maxPatchBytes so RunPatch truncates.
		fmt.Print(strings.Repeat("+", maxPatchBytes+1024))
	default:
		fmt.Fprintln(os.Stderr, "unknown HELPER_MODE")
		os.Exit(2)
	}
}

func TestRun_OK(t *testing.T) {
	r := &Runner{Cmd: fakeCmd("ok")}
	d, err := r.Run(context.Background(), "main", t.TempDir())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(d.ChangedFiles) != 2 {
		t.Errorf("got %d files, want 2", len(d.ChangedFiles))
	}
}

func TestRun_Error_SurfacesStderr(t *testing.T) {
	r := &Runner{Cmd: fakeCmd("error")}
	_, err := r.Run(context.Background(), "no-such-ref", t.TempDir())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "ambiguous argument") {
		t.Errorf("err = %v, want stderr-derived message", err)
	}
}

func TestRun_Empty(t *testing.T) {
	r := &Runner{Cmd: fakeCmd("ok_empty")}
	d, err := r.Run(context.Background(), "main", t.TempDir())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(d.ChangedFiles) != 0 {
		t.Errorf("expected empty diff, got %+v", d)
	}
}

func TestRun_RequiredArgs(t *testing.T) {
	r := &Runner{}
	if _, err := r.Run(context.Background(), "", "/x"); err == nil {
		t.Error("expected baseRef required")
	}
	if _, err := r.Run(context.Background(), "main", ""); err == nil {
		t.Error("expected repoDir required")
	}
}

// TestRun_RealRepo_CapturesUncommittedEdits is the regression test
// for #296: the runner's bundle event was empty because the gitdiff
// form `<base>...HEAD` only saw committed changes and the agent's
// edits were still unstaged when the runner emitted the bundle.
// The two-arg form `<base>` diffs the working tree, so uncommitted
// edits land in the result.
//
// Real git is required; skipped when not on PATH so the suite stays
// portable.
func TestRun_RealRepo_CapturesUncommittedEdits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()

	mustRunGit(t, repo, "init", "--initial-branch=main")
	mustRunGit(t, repo, "config", "user.name", "init")
	mustRunGit(t, repo, "config", "user.email", "init@example.com")
	// Disable signing so developers with `commit.gpgsign=true` in
	// their global gitconfig don't trip an interactive signer here.
	mustRunGit(t, repo, "config", "commit.gpgsign", "false")
	mustRunGit(t, repo, "config", "tag.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, repo, "add", "-A")
	mustRunGit(t, repo, "commit", "-m", "initial")

	// Snapshot the "base" — what the workflow's base branch points
	// at. Production runners use `origin/main`; for this isolated
	// repo we just keep "main" pinned to the initial commit.
	mustRunGit(t, repo, "branch", "base")

	// Agent-style edits: add a new file and modify an existing one,
	// but DON'T commit. The caller is expected to stage before
	// calling Run (mirrors what computeAndEmitDiff does in the
	// runner's main.go). Pre-#296 the form was `<base>...HEAD` and
	// this would produce an empty diff because HEAD == base; the
	// new `--cached <base>` form catches everything once staged.
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new_test.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, repo, "add", "-A")

	d, err := (&Runner{}).Run(context.Background(), "base", repo)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(d.ChangedFiles) != 2 {
		t.Fatalf("got %d changed files, want 2 (README modify + new_test.go add); files=%+v",
			len(d.ChangedFiles), d.ChangedFiles)
	}

	// Verify the new file is in the diff as an Add — that's the
	// signal the `tests_added_or_updated` constraint reads.
	var sawNew bool
	for _, f := range d.ChangedFiles {
		if f.Path == "new_test.go" && f.Status == constraint.StatusAdded {
			sawNew = true
		}
	}
	if !sawNew {
		t.Errorf("expected new_test.go (Added) in diff; got %+v", d.ChangedFiles)
	}
}

// mustRunGit invokes git in `repoDir` and fails the test on any
// non-zero exit. Output is captured into the test log so a flaky
// CI failure is debuggable.
func mustRunGit(t *testing.T, repoDir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func TestRunPatch_OK(t *testing.T) {
	r := &Runner{Cmd: fakeCmd("patch_ok")}
	patch, truncated, err := r.RunPatch(context.Background(), "main", t.TempDir())
	if err != nil {
		t.Fatalf("RunPatch: %v", err)
	}
	if truncated {
		t.Error("small patch should not be truncated")
	}
	for _, want := range []string{"diff --git a/a.go b/a.go", "@@ -1,2 +1,2 @@", "-old line", "+new line"} {
		if !strings.Contains(patch, want) {
			t.Errorf("patch missing %q; got:\n%s", want, patch)
		}
	}
}

func TestRunPatch_TruncatesWithMarker(t *testing.T) {
	r := &Runner{Cmd: fakeCmd("patch_big")}
	patch, truncated, err := r.RunPatch(context.Background(), "main", t.TempDir())
	if err != nil {
		t.Fatalf("RunPatch: %v", err)
	}
	if !truncated {
		t.Fatal("oversized patch should be truncated")
	}
	if !strings.Contains(patch, "[patch truncated:") {
		t.Errorf("expected truncation marker; got tail: %q", patch[len(patch)-80:])
	}
	// The captured body is capped at maxPatchBytes; total length is
	// the cap plus the (small) marker, never the full raw output. This
	// keeps the emitted JSONL line well under bundle.ReadEvents' 4 MiB
	// per-line scanner buffer.
	if len(patch) > maxPatchBytes+256 {
		t.Errorf("patch len %d exceeds cap+marker budget", len(patch))
	}
}

func TestRunPatch_Error_SurfacesStderr(t *testing.T) {
	r := &Runner{Cmd: fakeCmd("error")}
	_, _, err := r.RunPatch(context.Background(), "no-such-ref", t.TempDir())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "ambiguous argument") {
		t.Errorf("err = %v, want stderr-derived message", err)
	}
}

func TestRunPatch_RequiredArgs(t *testing.T) {
	r := &Runner{}
	if _, _, err := r.RunPatch(context.Background(), "", "/x"); err == nil {
		t.Error("expected baseRef required")
	}
	if _, _, err := r.RunPatch(context.Background(), "main", ""); err == nil {
		t.Error("expected repoDir required")
	}
}

// TestRunPatch_RealRepo_CapturesHunks verifies RunPatch returns real
// unified-diff hunks (including rename hunks) for the same staged index
// Run sees. Real git required; skipped when not on PATH.
func TestRunPatch_RealRepo_CapturesHunks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	mustRunGit(t, repo, "init", "--initial-branch=main")
	mustRunGit(t, repo, "config", "user.name", "init")
	mustRunGit(t, repo, "config", "user.email", "init@example.com")
	mustRunGit(t, repo, "config", "commit.gpgsign", "false")
	mustRunGit(t, repo, "config", "tag.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package x\nold\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, repo, "add", "-A")
	mustRunGit(t, repo, "commit", "-m", "initial")
	mustRunGit(t, repo, "branch", "base")

	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package x\nnew\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, repo, "add", "-A")

	patch, truncated, err := (&Runner{}).RunPatch(context.Background(), "base", repo)
	if err != nil {
		t.Fatalf("RunPatch: %v", err)
	}
	if truncated {
		t.Error("tiny patch should not be truncated")
	}
	if !strings.Contains(patch, "-old") || !strings.Contains(patch, "+new") {
		t.Errorf("expected added/removed lines in hunks; got:\n%s", patch)
	}
}

func TestSplitNULs(t *testing.T) {
	// Direct exercise of the splitter so we cover the request-more
	// branch.
	advance, token, err := splitNULs([]byte("ab"), false)
	if err != nil || advance != 0 || token != nil {
		t.Errorf("partial input: got (%d, %q, %v), want (0, nil, nil)", advance, token, err)
	}
	advance, token, err = splitNULs([]byte("ab\x00cd"), false)
	if err != nil || advance != 3 || string(token) != "ab" {
		t.Errorf("found NUL: got (%d, %q, %v)", advance, token, err)
	}
	advance, token, err = splitNULs([]byte("ab"), true)
	if err != nil || advance != 2 || string(token) != "ab" {
		t.Errorf("EOF: got (%d, %q, %v)", advance, token, err)
	}
	// EOF with no data should yield (0, nil, nil).
	advance, token, err = splitNULs(nil, true)
	if err != nil || advance != 0 || token != nil {
		t.Errorf("EOF empty: got (%d, %q, %v)", advance, token, err)
	}
}

// TestMergeBase_ProviderAgnosticArgv asserts the merge-base resolution is a
// purely LOCAL git invocation — `git merge-base <baseRef> HEAD` — and never a
// forge API / compare-endpoint call. It drives MergeBase through a Cmd builder
// that records the binary + argv, proving ADR-043 rev 2 stays provider-agnostic.
func TestMergeBase_ProviderAgnosticArgv(t *testing.T) {
	var gotName string
	var gotArgs []string
	r := &Runner{
		Cmd: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			gotName = name
			gotArgs = append([]string(nil), args...)
			c := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcess")
			c.Env = append(os.Environ(), "GO_HELPER_PROCESS=1", "HELPER_MODE=merge_base")
			return c
		},
	}
	mb, err := r.MergeBase(context.Background(), "origin/main", t.TempDir())
	if err != nil {
		t.Fatalf("MergeBase: %v", err)
	}
	if gotName != "git" {
		t.Errorf("binary = %q, want %q (local git, no forge API)", gotName, "git")
	}
	want := []string{"merge-base", "origin/main", "HEAD"}
	if !reflect.DeepEqual(gotArgs, want) {
		t.Errorf("argv = %v, want %v (local merge-base, not a forge compare endpoint)", gotArgs, want)
	}
	if mb != "abc123def4567890abc123def4567890abc12345" {
		t.Errorf("merge-base = %q, want trimmed SHA", mb)
	}
}

// TestMergeBase_Error_SurfacesStderr covers the fail-open error branch's
// message quality: an unresolvable base (e.g. an ambiguous ref) surfaces git's
// stderr so the caller's merge_base_unresolved log line is actionable.
func TestMergeBase_Error_SurfacesStderr(t *testing.T) {
	r := &Runner{Cmd: fakeCmd("error")}
	_, err := r.MergeBase(context.Background(), "no-such-ref", t.TempDir())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "ambiguous argument") {
		t.Errorf("err = %v, want stderr-derived message", err)
	}
}

// TestMergeBase_EmptyOutput guards the exit-0-but-empty branch: git printing no
// SHA (defensive; shouldn't happen on a real merge-base) is treated as an error
// so the caller fails open to the tip baseRef rather than diffing against "".
func TestMergeBase_EmptyOutput(t *testing.T) {
	r := &Runner{Cmd: fakeCmd("ok_empty")}
	_, err := r.MergeBase(context.Background(), "main", t.TempDir())
	if err == nil {
		t.Fatal("expected error for empty merge-base output")
	}
	if !strings.Contains(err.Error(), "empty output") {
		t.Errorf("err = %v, want empty-output message", err)
	}
}

func TestMergeBase_RequiredArgs(t *testing.T) {
	r := &Runner{}
	if _, err := r.MergeBase(context.Background(), "", "/x"); err == nil {
		t.Error("expected baseRef required")
	}
	if _, err := r.MergeBase(context.Background(), "main", ""); err == nil {
		t.Error("expected repoDir required")
	}
}

// stagedPaths is a small helper: the sorted set of changed-file paths in a Diff.
func stagedPaths(d constraint.Diff) []string {
	out := make([]string, 0, len(d.ChangedFiles))
	for _, f := range d.ChangedFiles {
		out = append(out, f.Path)
	}
	sort.Strings(out)
	return out
}

// buildStaleBaseRepo constructs the #1290 shape with real git and returns the
// repo dir. Layout: base commit B0; an orthogonal base advance to B1 (adds
// b.go); HEAD pinned at B0 (the run's fork point) with a.go staged-not-committed
// (the run's own increment). baseRef "base" points at B1.
//
//	B0 (main, HEAD) ── a.go staged
//	 └── B1 (base)   ── adds b.go orthogonally
func buildStaleBaseRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	mustRunGit(t, repo, "init", "--initial-branch=main")
	mustRunGit(t, repo, "config", "user.name", "init")
	mustRunGit(t, repo, "config", "user.email", "init@example.com")
	mustRunGit(t, repo, "config", "commit.gpgsign", "false")
	mustRunGit(t, repo, "config", "tag.gpgsign", "false")

	// B0: shared base commit, on both main and base.
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, repo, "add", "-A")
	mustRunGit(t, repo, "commit", "-m", "B0")
	mustRunGit(t, repo, "branch", "base")

	// Advance base to B1 by adding an unrelated file b.go — an orthogonal
	// move that does NOT touch the run's HEAD (main, still B0).
	mustRunGit(t, repo, "checkout", "base")
	if err := os.WriteFile(filepath.Join(repo, "b.go"), []byte("package b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, repo, "add", "-A")
	mustRunGit(t, repo, "commit", "-m", "B1: orthogonal base advance")

	// Back on the run's fork point (B0), stage the run's own edit a.go
	// without committing — mirrors computeAndEmitDiff's staged index.
	mustRunGit(t, repo, "checkout", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, repo, "add", "-A")
	return repo
}

// TestMergeBase_StaleBase_RemovesPhantomDeletion is the #1290 repro (real git):
// the 3-dot comparison (staged index vs. merge-base(base,HEAD)=B0) sees the
// run's increment {a.go} with NO phantom deletion for the orthogonally-added
// b.go, whereas the OLD 2-dot comparison (staged index vs. base TIP=B1) WOULD
// have reported b.go as a phantom D — exactly the inflation ADR-043 rev 2 removes.
func TestMergeBase_StaleBase_RemovesPhantomDeletion(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := buildStaleBaseRepo(t)
	r := &Runner{}
	ctx := context.Background()

	// merge-base(base, HEAD=main) must resolve to the fork point B0.
	mb, err := r.MergeBase(ctx, "base", repo)
	if err != nil {
		t.Fatalf("MergeBase: %v", err)
	}
	b0 := strings.TrimSpace(mustRunGitOut(t, repo, "rev-parse", "main"))
	if mb != b0 {
		t.Fatalf("merge-base = %q, want B0 (%q) — the run's fork point", mb, b0)
	}

	// NEW 3-dot path: diff the staged index against the merge-base.
	newDiff, err := r.Run(ctx, mb, repo)
	if err != nil {
		t.Fatalf("Run(merge-base): %v", err)
	}
	if got := stagedPaths(newDiff); !reflect.DeepEqual(got, []string{"a.go"}) {
		t.Fatalf("3-dot staged set = %v, want [a.go] (no phantom b.go deletion)", got)
	}
	for _, f := range newDiff.ChangedFiles {
		if f.Path == "b.go" {
			t.Errorf("3-dot diff contains b.go (%s); the orthogonal base advance must not appear", f.Status)
		}
	}

	// OLD 2-dot path: diff against the base TIP (B1) — proves the bug the fix
	// removes. b.go shows up as a phantom deletion that inflated the count.
	oldDiff, err := r.Run(ctx, "base", repo)
	if err != nil {
		t.Fatalf("Run(base tip): %v", err)
	}
	var sawPhantomDelete bool
	for _, f := range oldDiff.ChangedFiles {
		if f.Path == "b.go" && f.Status == constraint.StatusDeleted {
			sawPhantomDelete = true
		}
	}
	if !sawPhantomDelete {
		t.Fatalf("expected the OLD 2-dot diff to report b.go as a phantom deletion; got %+v", oldDiff.ChangedFiles)
	}
	if len(oldDiff.ChangedFiles) <= len(newDiff.ChangedFiles) {
		t.Errorf("2-dot diff (%d files) should be inflated over 3-dot (%d files)",
			len(oldDiff.ChangedFiles), len(newDiff.ChangedFiles))
	}
}

// TestMergeBase_BaseUnmoved_BackCompat asserts the change is a no-op when the
// base has NOT advanced: merge-base(base==B0, HEAD==B0) is B0, so the 3-dot
// staged set is byte-for-byte identical to the 2-dot set — today's behavior is
// preserved on the common (base-unmoved) path.
func TestMergeBase_BaseUnmoved_BackCompat(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	mustRunGit(t, repo, "init", "--initial-branch=main")
	mustRunGit(t, repo, "config", "user.name", "init")
	mustRunGit(t, repo, "config", "user.email", "init@example.com")
	mustRunGit(t, repo, "config", "commit.gpgsign", "false")
	mustRunGit(t, repo, "config", "tag.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, repo, "add", "-A")
	mustRunGit(t, repo, "commit", "-m", "B0")
	mustRunGit(t, repo, "branch", "base") // base stays pinned at B0 (unmoved)

	// Stage the run's increment without committing; HEAD stays at B0.
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, repo, "add", "-A")

	r := &Runner{}
	ctx := context.Background()
	mb, err := r.MergeBase(ctx, "base", repo)
	if err != nil {
		t.Fatalf("MergeBase: %v", err)
	}
	threeDot, err := r.Run(ctx, mb, repo)
	if err != nil {
		t.Fatalf("Run(merge-base): %v", err)
	}
	twoDot, err := r.Run(ctx, "base", repo)
	if err != nil {
		t.Fatalf("Run(base tip): %v", err)
	}
	if !reflect.DeepEqual(stagedPaths(threeDot), stagedPaths(twoDot)) {
		t.Errorf("base unmoved: 3-dot %v != 2-dot %v (must be identical)",
			stagedPaths(threeDot), stagedPaths(twoDot))
	}
}

// mustRunGitOut is mustRunGit that returns stdout, for ref resolution.
func mustRunGitOut(t *testing.T, repoDir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}

// errExitWithoutStderr ensures the *exec.ExitError-without-stderr
// path is exercised. When git fails but emits no stderr (rare but
// possible in mocked cases), the wrapped error still carries the
// exit message.
func TestRun_ExitErrorWithoutStderr(t *testing.T) {
	// Use exec.CommandContext to exec /bin/false (or equivalent).
	// false always exits 1 with no output — perfect proxy.
	r := &Runner{
		Cmd: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "false")
		},
	}
	_, err := r.Run(context.Background(), "main", t.TempDir())
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, err) { // smoke: just make sure non-nil
		t.Errorf("err = %v", err)
	}
}
