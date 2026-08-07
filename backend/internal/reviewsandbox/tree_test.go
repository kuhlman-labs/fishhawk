package reviewsandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// tarEntry is one in-memory tar member for the extractTar guard tests.
type tarEntry struct {
	name     string
	typeflag byte
	linkname string
	body     string
	size     int64 // when > 0 and body empty, declare this size (byte-bound probe)
}

// buildTar serializes entries into a tar byte stream BY CONSTRUCTION so a guard
// test's RED lands on the guard, not on fixture setup.
func buildTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		size := e.size
		if size == 0 {
			size = int64(len(e.body))
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Linkname: e.linkname,
			Mode:     0o644,
			Size:     size,
		}
		if e.typeflag == tar.TypeDir {
			hdr.Mode = 0o755
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header %q: %v", e.name, err)
		}
		if hdr.Size > 0 && e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("write tar body %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

func reg(name, body string) tarEntry {
	return tarEntry{name: name, typeflag: tar.TypeReg, body: body}
}

// TestExtractTar_Happy pins a clean extraction: regular files land with their
// content, a nested dir is created, and skip counters stay zero.
func TestExtractTar_Happy(t *testing.T) {
	dest := t.TempDir()
	data := buildTar(t, []tarEntry{
		{name: "pkg/", typeflag: tar.TypeDir},
		reg("pkg/a.go", "package pkg\n"),
		reg("README.md", "# hi\n"),
	})
	stats, err := extractTar(bytes.NewReader(data), dest, DefaultLimits())
	if err != nil {
		t.Fatalf("extractTar: %v", err)
	}
	if stats.Files != 2 || stats.AnySkipped() {
		t.Errorf("stats = %+v, want 2 files and nothing skipped", stats)
	}
	got, err := os.ReadFile(filepath.Join(dest, "pkg/a.go"))
	if err != nil || string(got) != "package pkg\n" {
		t.Errorf("pkg/a.go = %q, %v; want the written content", got, err)
	}
}

// TestExtractTar_RejectsTraversal pins the path-traversal guard: a "../escape"
// entry returns an error AND writes NO file outside the export root. (The error
// alone is insufficient — a guard that fires then cleans up returns a
// similar-shaped error, so the state assertion is the real counterfactual.)
func TestExtractTar_RejectsTraversal(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "export")
	if err := os.Mkdir(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	data := buildTar(t, []tarEntry{reg("../escape.txt", "pwned")})
	_, err := extractTar(bytes.NewReader(data), dest, DefaultLimits())
	if err == nil {
		t.Fatal("expected an error for a traversal entry, got nil")
	}
	if _, serr := os.Stat(filepath.Join(root, "escape.txt")); !os.IsNotExist(serr) {
		t.Errorf("traversal wrote a file outside the export root: stat err = %v", serr)
	}
}

// TestExtractTar_RejectsAbsolutePath pins the absolute-path arm of the guard.
func TestExtractTar_RejectsAbsolutePath(t *testing.T) {
	dest := t.TempDir()
	data := buildTar(t, []tarEntry{reg("/etc/pwned.txt", "x")})
	if _, err := extractTar(bytes.NewReader(data), dest, DefaultLimits()); err == nil {
		t.Fatal("expected an error for an absolute-path entry, got nil")
	}
}

// TestExtractTar_SkipsSymlinks pins the symlink/hard-link skip: neither is
// materialized and both are counted.
func TestExtractTar_SkipsSymlinks(t *testing.T) {
	dest := t.TempDir()
	data := buildTar(t, []tarEntry{
		reg("real.txt", "ok"),
		{name: "link.txt", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
		{name: "hard.txt", typeflag: tar.TypeLink, linkname: "real.txt"},
	})
	stats, err := extractTar(bytes.NewReader(data), dest, DefaultLimits())
	if err != nil {
		t.Fatalf("extractTar: %v", err)
	}
	if stats.Symlinks != 2 {
		t.Errorf("stats.Symlinks = %d, want 2", stats.Symlinks)
	}
	if _, serr := os.Lstat(filepath.Join(dest, "link.txt")); !os.IsNotExist(serr) {
		t.Errorf("symlink was materialized: %v", serr)
	}
}

// TestExtractTar_SkipsAgentInstructionFiles is the C1 named guard (exact test
// name, machine-checked): agent-instruction files and config dirs are SKIPPED
// and counted, while ordinary files land. Covers AGENTS.md / AGENTS.override.md
// / CLAUDE.md / CLAUDE.local.md at any depth and everything under .claude/ and
// .codex/. The AGENTS.override.md entries (root and nested) pin the #2486
// fix-up: codex checks the .override. variant BEFORE AGENTS.md at each level, so
// a tracked AGENTS.override.md at any depth would otherwise survive extraction
// and reopen the approval-laundering channel C1 made blocking.
func TestExtractTar_SkipsAgentInstructionFiles(t *testing.T) {
	dest := t.TempDir()
	data := buildTar(t, []tarEntry{
		reg("AGENTS.md", "root codex instructions"),
		reg("AGENTS.override.md", "root codex override instructions"),
		reg("CLAUDE.md", "root claude instructions"),
		reg("CLAUDE.local.md", "local override"),
		reg("backend/AGENTS.md", "nested codex instructions"),
		reg("backend/AGENTS.override.md", "nested codex override instructions"),
		reg("docs/CLAUDE.md", "nested claude instructions"),
		reg(".claude/settings.json", "{}"),
		reg(".codex/config.toml", "x=1"),
		reg("backend/.claude/commands/foo.md", "cmd"),
		reg("main.go", "package main"), // an ordinary file that MUST land
	})
	stats, err := extractTar(bytes.NewReader(data), dest, DefaultLimits())
	if err != nil {
		t.Fatalf("extractTar: %v", err)
	}
	if stats.Instructions != 10 {
		t.Errorf("stats.Instructions = %d, want 10 agent-instruction skips", stats.Instructions)
	}
	if stats.Files != 1 {
		t.Errorf("stats.Files = %d, want 1 (only main.go lands)", stats.Files)
	}
	for _, skipped := range []string{"AGENTS.md", "AGENTS.override.md", "CLAUDE.md", "CLAUDE.local.md", "backend/AGENTS.md", "backend/AGENTS.override.md", "docs/CLAUDE.md", ".claude/settings.json", ".codex/config.toml"} {
		if _, serr := os.Stat(filepath.Join(dest, skipped)); !os.IsNotExist(serr) {
			t.Errorf("agent-instruction path %q was materialized: %v", skipped, serr)
		}
	}
	if _, serr := os.Stat(filepath.Join(dest, "main.go")); serr != nil {
		t.Errorf("ordinary file main.go was not materialized: %v", serr)
	}
}

// TestExtractTar_FileCountBound pins the file-count guard.
func TestExtractTar_FileCountBound(t *testing.T) {
	dest := t.TempDir()
	data := buildTar(t, []tarEntry{reg("a", "1"), reg("b", "2"), reg("c", "3")})
	_, err := extractTar(bytes.NewReader(data), dest, Limits{MaxFiles: 2, MaxBytes: 1 << 20})
	if err == nil || !strings.Contains(err.Error(), "file count") {
		t.Fatalf("err = %v, want a file-count-bound error", err)
	}
}

// TestExtractTar_ByteBound pins the byte-total guard.
func TestExtractTar_ByteBound(t *testing.T) {
	dest := t.TempDir()
	data := buildTar(t, []tarEntry{reg("big", strings.Repeat("x", 100))})
	_, err := extractTar(bytes.NewReader(data), dest, Limits{MaxFiles: 1000, MaxBytes: 50})
	if err == nil || !strings.Contains(err.Error(), "byte total") {
		t.Fatalf("err = %v, want a byte-total-bound error", err)
	}
}

// --- ExportTree integration tests over a real git repo ---

func gitOrSkip(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func newFixtureRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("tracked content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("ignored.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "tracked.txt", ".gitignore")
	runGit(t, repo, "commit", "-q", "-m", "init")
	// An untracked file and a gitignored file — neither must appear in the export.
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "ignored.txt"), []byte("nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

// TestExportTree_Happy pins a real export: tracked files present at the right
// content, .git absent, untracked + gitignored absent, and a resolved SHA
// returned (C4).
func TestExportTree_Happy(t *testing.T) {
	gitOrSkip(t)
	repo := newFixtureRepo(t)
	dir, commit, stats, cleanup, err := ExportTree(context.Background(), repo, "HEAD", DefaultLimits())
	if err != nil {
		t.Fatalf("ExportTree: %v", err)
	}
	defer cleanup()

	if len(commit) < 40 {
		t.Errorf("commit = %q, want a full resolved SHA", commit)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "tracked.txt")); err != nil || string(got) != "tracked content\n" {
		t.Errorf("tracked.txt = %q, %v", got, err)
	}
	for _, absent := range []string{".git", "untracked.txt", "ignored.txt"} {
		if _, serr := os.Stat(filepath.Join(dir, absent)); !os.IsNotExist(serr) {
			t.Errorf("%q should be absent from the export; stat err = %v", absent, serr)
		}
	}
	if stats.Files == 0 {
		t.Errorf("stats.Files = 0, want the tracked files counted")
	}
}

// TestExportTree_CleanupRemovesDir pins that the cleanup closure removes the dir.
func TestExportTree_CleanupRemovesDir(t *testing.T) {
	gitOrSkip(t)
	repo := newFixtureRepo(t)
	dir, _, _, cleanup, err := ExportTree(context.Background(), repo, "HEAD", DefaultLimits())
	if err != nil {
		t.Fatalf("ExportTree: %v", err)
	}
	cleanup()
	if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
		t.Errorf("export dir still exists after cleanup: %v", serr)
	}
}

// TestExportTree_BadRefDegrades pins the unresolvable-ref degrade (error, empty
// dir) — the server maps this to an ungrounded review.
func TestExportTree_BadRefDegrades(t *testing.T) {
	gitOrSkip(t)
	repo := newFixtureRepo(t)
	dir, _, _, _, err := ExportTree(context.Background(), repo, "does-not-exist-ref", DefaultLimits())
	if err == nil {
		t.Fatal("expected an error for an unresolvable ref, got nil")
	}
	if dir != "" {
		t.Errorf("dir = %q, want empty on degrade", dir)
	}
}

// TestExportTree_NotARepoDegrades pins the not-a-work-tree degrade.
func TestExportTree_NotARepoDegrades(t *testing.T) {
	gitOrSkip(t)
	plain := t.TempDir()
	if _, _, _, _, err := ExportTree(context.Background(), plain, "HEAD", DefaultLimits()); err == nil {
		t.Fatal("expected an error exporting from a non-repo dir, got nil")
	}
}

// TestExportTree_BoundExceededDegrades_LiveGitChild exercises archiveInto's
// early-abort branch (extractTar fails a bound mid-stream → cancel the git child
// blocked writing to the now-unread pipe → Wait reaps it) with a REAL git-archive
// subprocess, not an in-memory tar. The tracked file is large enough that its
// archive output overflows the OS pipe buffer, so the git child is still writing
// when extractTar aborts on the tiny byte bound — the exact pipe-abort/reap
// ordering the branch exists for. The prior coverage drove that branch only
// through extractTar directly, never against a live child (#2486 fix-up).
func TestExportTree_BoundExceededDegrades_LiveGitChild(t *testing.T) {
	gitOrSkip(t)
	repo := t.TempDir()
	runGit(t, repo, "init", "-q")
	// 1 MiB tracked file: git archive's output far exceeds the ~64 KiB pipe
	// buffer, guaranteeing the child is mid-write when extractTar aborts.
	big := strings.Repeat("x", 1<<20)
	if err := os.WriteFile(filepath.Join(repo, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "big.txt")
	runGit(t, repo, "commit", "-q", "-m", "big")

	dir, _, _, _, err := ExportTree(context.Background(), repo, "HEAD", Limits{MaxFiles: 1000, MaxBytes: 50})
	if err == nil {
		t.Fatal("expected a byte-bound error from the live git-archive child, got nil")
	}
	if !strings.Contains(err.Error(), "byte total") {
		t.Errorf("err = %v, want a byte-total-bound error surfaced from archiveInto", err)
	}
	if dir != "" {
		t.Errorf("dir = %q, want empty on a bound-exceeded degrade (partial export removed)", dir)
	}
}

// TestExportTree_GitAbsentDegrades pins the git-absent degrade by stripping PATH
// so the `git` binary cannot be resolved.
func TestExportTree_GitAbsentDegrades(t *testing.T) {
	repo := t.TempDir()
	t.Setenv("PATH", filepath.Join(repo, "no-such-bin"))
	if _, _, _, _, err := ExportTree(context.Background(), repo, "HEAD", DefaultLimits()); err == nil {
		t.Fatal("expected an error when git is absent from PATH, got nil")
	}
}
