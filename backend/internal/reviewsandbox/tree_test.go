package reviewsandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
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
// / CLAUDE.md / CLAUDE.local.md at any depth and everything under .claude/,
// .codex/ and .agents/, matched case-insensitively (the re-cased entries). The
// .agents/ entries (root and nested) pin the codex
// Agent Skills discovery path: codex loads .agents/skills/*/SKILL.md from every
// directory level, so a tracked skill would otherwise be auto-discovered from
// the grounded tree. The AGENTS.override.md entries (root and nested) pin the #2486
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
		reg(".agents/skills/x/SKILL.md", "---\nname: x\ndescription: planted\n---\n"),
		reg("backend/.agents/skills/y/SKILL.md", "nested planted skill"),
		// Re-cased variants: on a case-insensitive host filesystem a CLI
		// opening `.agents/skills` or `AGENTS.md` resolves to these.
		reg(".Agents/skills/z/SKILL.md", "re-cased planted skill"),
		reg(".CLAUDE/settings.json", "{}"),
		reg("docs/agents.md", "re-cased codex instructions"),
		reg("cli/.Codex/config.toml", "x=1"),
		reg("backend/agents.OVERRIDE.md", "re-cased codex override"),
		reg("docs/claude.md", "re-cased claude instructions"),
		reg("web/claude.LOCAL.md", "re-cased claude local override"),
		reg("main.go", "package main"), // an ordinary file that MUST land
	})
	stats, err := extractTar(bytes.NewReader(data), dest, DefaultLimits())
	if err != nil {
		t.Fatalf("extractTar: %v", err)
	}
	if stats.Instructions != 19 {
		t.Errorf("stats.Instructions = %d, want 19 agent-instruction skips", stats.Instructions)
	}
	if stats.Files != 1 {
		t.Errorf("stats.Files = %d, want 1 (only main.go lands)", stats.Files)
	}
	for _, skipped := range []string{"AGENTS.md", "AGENTS.override.md", "CLAUDE.md", "CLAUDE.local.md", "backend/AGENTS.md", "backend/AGENTS.override.md", "docs/CLAUDE.md", ".claude/settings.json", ".codex/config.toml", ".agents/skills/x/SKILL.md", "backend/.agents/skills/y/SKILL.md", ".Agents/skills/z/SKILL.md", ".CLAUDE/settings.json", "docs/agents.md", "cli/.Codex/config.toml", "backend/agents.OVERRIDE.md", "docs/claude.md", "web/claude.LOCAL.md"} {
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
	gitOut(t, dir, args...)
}

// gitOut runs git in dir and returns its trimmed combined output, failing the
// test on a non-zero exit.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
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
	if !errors.Is(err, ErrRefUnavailable) {
		t.Errorf("err = %v, want it to wrap ErrRefUnavailable (#4066)", err)
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
	_, _, _, _, err := ExportTree(context.Background(), repo, "HEAD", DefaultLimits())
	if err == nil {
		t.Fatal("expected an error when git is absent from PATH, got nil")
	}
	// A start failure is not a ref miss: no fetch, and not ErrRefUnavailable
	// (the server reports it as export_failed, #4066).
	if errors.Is(err, ErrRefUnavailable) {
		t.Errorf("git-absent err = %v, must NOT wrap ErrRefUnavailable", err)
	}
	// Same with a full object id, which would otherwise be fetch-eligible.
	_, _, _, _, err = ExportTree(context.Background(), repo, strings.Repeat("a", 40), DefaultLimits())
	if err == nil || errors.Is(err, ErrRefUnavailable) {
		t.Errorf("git-absent err for a full SHA = %v, want a non-ErrRefUnavailable error", err)
	}
}

// --- fetch-on-miss (#4066) ---

// originFixture is a local clone whose bare origin holds a commit (c2) the
// clone does NOT have — the consolidated-review shape, where the server pushed
// the head to origin and the operator's checkout never fetched it.
type originFixture struct {
	root, src, origin, local string
	c1, c2                   string
	c2Tree                   string
	branch                   string // the origin-only branch naming c2
}

// newOriginFixture builds the fixture WITHOUT a `git push` into the bare
// origin (AGENTS.md #3503: a push into a local bare repo forks a detached
// maintenance child that can race t.TempDir cleanup). The bare origin instead
// FETCHES c2 from src with --no-auto-maintenance. It precondition-asserts c2 is
// absent from the local clone.
func newOriginFixture(t *testing.T) originFixture {
	t.Helper()
	gitOrSkip(t)
	root := t.TempDir()
	f := originFixture{
		root:   root,
		src:    filepath.Join(root, "src"),
		origin: filepath.Join(root, "origin.git"),
		local:  filepath.Join(root, "local"),
		branch: "fishhawk/run-x-consolidated",
	}
	runGit(t, root, "init", "-q", f.src)
	if err := os.WriteFile(filepath.Join(f.src, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.src, "add", "base.txt")
	runGit(t, f.src, "commit", "-q", "-m", "c1")
	f.c1 = gitOut(t, f.src, "rev-parse", "HEAD")
	runGit(t, root, "clone", "-q", "--bare", f.src, f.origin)
	runGit(t, root, "clone", "-q", f.origin, f.local)

	if err := os.WriteFile(filepath.Join(f.src, "consolidated.txt"), []byte("only in c2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.src, "add", "consolidated.txt")
	runGit(t, f.src, "commit", "-q", "-m", "c2")
	f.c2 = gitOut(t, f.src, "rev-parse", "HEAD")
	f.c2Tree = gitOut(t, f.src, "rev-parse", "HEAD^{tree}")
	runGit(t, f.origin, "fetch", "-q", "--no-auto-maintenance", f.src, "HEAD:refs/heads/"+f.branch)

	f.requireAbsentLocally(t, f.c2)
	f.requireAbsentLocally(t, f.c2Tree)
	return f
}

// hasLocally reports whether the local clone has object oid.
func (f originFixture) hasLocally(oid string) bool {
	cmd := exec.Command("git", "-C", f.local, "cat-file", "-e", oid)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	return cmd.Run() == nil
}

func (f originFixture) requireAbsentLocally(t *testing.T, oid string) {
	t.Helper()
	if f.hasLocally(oid) {
		t.Fatalf("fixture precondition: %s must be absent from the local clone", oid)
	}
}

func (f originFixture) refs(t *testing.T) string {
	t.Helper()
	return gitOut(t, f.local, "for-each-ref")
}

// TestExportTree_FetchesOriginOnlyCommit is the #4066 done-means: a ref that
// exists only in origin still exports. The commit is precondition-asserted
// ABSENT locally, so without the fetch rev-parse cannot resolve it. The fetch
// must move no ref (for-each-ref byte-identical) and write no FETCH_HEAD.
func TestExportTree_FetchesOriginOnlyCommit(t *testing.T) {
	f := newOriginFixture(t)
	before := f.refs(t)

	dir, commit, _, cleanup, err := ExportTree(context.Background(), f.local, f.c2, DefaultLimits())
	if err != nil {
		t.Fatalf("ExportTree(origin-only %s): %v", f.c2, err)
	}
	defer cleanup()
	if dir == "" || commit != f.c2 {
		t.Fatalf("dir=%q commit=%q, want a tree at %s", dir, commit, f.c2)
	}
	if got, rerr := os.ReadFile(filepath.Join(dir, "consolidated.txt")); rerr != nil || string(got) != "only in c2\n" {
		t.Errorf("c2-only file = %q, %v; want the origin-only commit's content", got, rerr)
	}
	if after := f.refs(t); after != before {
		t.Errorf("fetch moved a ref:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if _, serr := os.Stat(filepath.Join(f.local, ".git", "FETCH_HEAD")); !os.IsNotExist(serr) {
		t.Errorf("fetch wrote FETCH_HEAD (stat err = %v), want --no-write-fetch-head", serr)
	}
}

// TestExportTree_NonSHARefNeverFetches pins the isFullObjectID guard. A: the
// origin-only BRANCH NAME degrades with ErrRefUnavailable and its commit is
// STILL absent locally (without the guard `git fetch origin <branch>`
// downloads the objects even with --refmap= and no FETCH_HEAD). B: a token
// starting with '-' is never handed to fetch (without the guard git parses it
// as --upload-pack and the local transport executes it, creating the marker).
func TestExportTree_NonSHARefNeverFetches(t *testing.T) {
	t.Run("branch name", func(t *testing.T) {
		f := newOriginFixture(t)
		_, _, _, _, err := ExportTree(context.Background(), f.local, f.branch, DefaultLimits())
		if !errors.Is(err, ErrRefUnavailable) {
			t.Fatalf("err = %v, want ErrRefUnavailable for a non-SHA miss", err)
		}
		if f.hasLocally(f.c2) {
			t.Error("a non-SHA ref was fetched: the branch's commit is now present locally")
		}
	})
	t.Run("option-shaped ref", func(t *testing.T) {
		f := newOriginFixture(t)
		marker := filepath.Join(f.root, "upload-pack-ran")
		_, _, _, _, err := ExportTree(context.Background(), f.local, "--upload-pack=touch "+marker, DefaultLimits())
		if !errors.Is(err, ErrRefUnavailable) {
			t.Fatalf("err = %v, want ErrRefUnavailable for an option-shaped ref", err)
		}
		if _, serr := os.Stat(marker); !os.IsNotExist(serr) {
			t.Errorf("option-shaped ref reached the fetch argv: marker exists (stat err = %v)", serr)
		}
	})
}

// TestExportTree_FetchMissDegradesWithNamedCause: a well-formed SHA that exists
// nowhere degrades with ErrRefUnavailable naming the fetch and its exit, with
// no export dir created — both against a real origin and in a repo with no
// origin remote at all.
func TestExportTree_FetchMissDegradesWithNamedCause(t *testing.T) {
	const nowhere = "0123456789abcdef0123456789abcdef01234567"
	noOrigin := newFixtureRepo(t)
	for name, repo := range map[string]string{
		"absent from origin": newOriginFixture(t).local,
		"no origin remote":   noOrigin,
	} {
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			dir, _, _, _, err := ExportTree(context.Background(), repo, nowhere, DefaultLimits())
			if !errors.Is(err, ErrRefUnavailable) {
				t.Fatalf("err = %v, want ErrRefUnavailable", err)
			}
			for _, w := range []string{"could not be fetched from origin", "fetch from origin", "exit status"} {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err = %q, want it to name %q", err, w)
				}
			}
			if dir != "" {
				t.Errorf("dir = %q, want empty", dir)
			}
			if left, _ := filepath.Glob(filepath.Join(tmp, "fishhawk-review-tree-*")); len(left) != 0 {
				t.Errorf("partial export dirs left behind: %v", left)
			}
		})
	}
}

// TestExportTree_FetchedObjectNotACommitDegrades: the fetch succeeds but the
// object is a TREE, so the post-fetch rev-parse ^{commit} peel still fails —
// ErrRefUnavailable naming that the object is not a commit after the fetch.
func TestExportTree_FetchedObjectNotACommitDegrades(t *testing.T) {
	f := newOriginFixture(t)
	_, _, _, _, err := ExportTree(context.Background(), f.local, f.c2Tree, DefaultLimits())
	if !errors.Is(err, ErrRefUnavailable) {
		t.Fatalf("err = %v, want ErrRefUnavailable", err)
	}
	if !strings.Contains(err.Error(), "still not a commit after fetching") {
		t.Errorf("err = %q, want it to name the post-fetch miss", err)
	}
	if !f.hasLocally(f.c2Tree) {
		t.Error("fixture: the tree object was not fetched, so the post-fetch branch was not reached")
	}
}

// hangingUploadPack points the LOCAL clone's remote.origin.uploadpack (read on
// the fetching side, C2) at a command that sleeps for d and ignores git's
// trailing repository-path argument (`: "$@"` absorbs it), so the fetch hangs
// until something kills it. The sleep is a GRANDCHILD of git holding the
// stderr pipe, which is what makes the procgroup.Harden half observable.
func (f originFixture) hangingUploadPack(t *testing.T, d time.Duration) {
	t.Helper()
	secs := int(d / time.Second)
	if secs < 1 {
		secs = 1
	}
	runGit(t, f.local, "config", "remote.origin.uploadpack", "/bin/sleep "+itoa(secs)+"; : \"$@\"")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestExportTree_FetchTimeoutDegrades pins the fetch bound and the whole-group
// kill. The upload-pack hangs for timescale.D(20s); Limits.FetchTimeout is
// timescale.D(300ms). ExportTree must return within timescale.D(5s), wrapping
// ErrRefUnavailable and naming the timeout. Without the deadline the fetch
// blocks for the full sleep; without procgroup.Harden the killed git's
// sleeping grandchild keeps the stderr pipe open so Wait blocks for the full
// sleep. Both breach the elapsed bound. The elapsed LOWER bound proves the
// arm actually hung until the bound fired (the expected verdict IS the
// timeout, so the cheap budget is right per the #3587 rule).
func TestExportTree_FetchTimeoutDegrades(t *testing.T) {
	f := newOriginFixture(t)
	f.hangingUploadPack(t, timescale.D(20*time.Second))
	fetchTimeout := timescale.D(300 * time.Millisecond)

	start := time.Now()
	_, _, _, _, err := ExportTree(context.Background(), f.local, f.c2, Limits{FetchTimeout: fetchTimeout})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrRefUnavailable) {
		t.Fatalf("err = %v, want ErrRefUnavailable", err)
	}
	if !strings.Contains(err.Error(), "timed out after") {
		t.Errorf("err = %q, want it to name the fetch timeout", err)
	}
	if bound := timescale.D(5 * time.Second); elapsed > bound {
		t.Errorf("ExportTree took %s, want <= %s (the fetch bound or the group kill did not fire)", elapsed, bound)
	}
	if elapsed < fetchTimeout {
		t.Errorf("ExportTree returned after %s, before the %s bound — the upload-pack did not hang", elapsed, fetchTimeout)
	}
}

// TestExportTree_ParentCancelDuringFetchIsCtxError (C1, fetch half): when the
// PARENT context's deadline fires mid-fetch (the fetch's own bound is far
// longer), the error names the context error — not "timed out after", and not
// ErrRefUnavailable.
func TestExportTree_ParentCancelDuringFetchIsCtxError(t *testing.T) {
	f := newOriginFixture(t)
	f.hangingUploadPack(t, timescale.D(20*time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), timescale.D(300*time.Millisecond))
	defer cancel()

	_, _, _, _, err := ExportTree(ctx, f.local, f.c2, Limits{FetchTimeout: timescale.D(20 * time.Second)})
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrRefUnavailable) {
		t.Errorf("err = %v, a parent-context cancel must NOT wrap ErrRefUnavailable", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap the parent's context.DeadlineExceeded", err)
	}
	if strings.Contains(err.Error(), "timed out after") {
		t.Errorf("err = %q, a parent cancel must not be reported as the fetch's own timeout", err)
	}
}

// fakeGit installs a `git` shell script on PATH (non-parallel: t.Setenv) and
// returns the path of a marker the script creates when it is invoked as
// `fetch`. body runs after the marker handling; $state is a scratch file.
func fakeGit(t *testing.T, body string) (marker string) {
	t.Helper()
	bin := t.TempDir()
	marker = filepath.Join(bin, "fetch-ran")
	state := filepath.Join(bin, "state")
	script := "#!/bin/sh\nmarker='" + marker + "'\nstate='" + state + "'\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	return marker
}

// TestExportTree_CancelledRevParseIsNotAMiss is the C1 pin: a rev-parse killed
// by the context deadline surfaces as an *exec.ExitError (signal: killed), and
// must NOT be classified as a ref miss even for a fetch-eligible 40-hex ref —
// the error is the context's, not ErrRefUnavailable, and no fetch is
// attempted. The fake git HANGS in rev-parse so the kill lands mid-run (a
// pre-cancelled context would fail cmd.Start with a non-ExitError and mask the
// check). exec.Cmd.Start refuses a done context, so with the check deleted the
// fetch never spawns either; the discriminating observable is the error naming
// a fetch attempt.
func TestExportTree_CancelledRevParseIsNotAMiss(t *testing.T) {
	marker := fakeGit(t, `for a in "$@"; do
  if [ "$a" = fetch ]; then : > "$marker"; exit 1; fi
  if [ "$a" = rev-parse ]; then exec /bin/sleep 30; fi
done
exit 1`)
	ctx, cancel := context.WithTimeout(context.Background(), timescale.D(300*time.Millisecond))
	defer cancel()

	_, _, _, _, err := ExportTree(ctx, t.TempDir(), strings.Repeat("ab", 20), DefaultLimits())
	if err == nil {
		t.Fatal("expected an error")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Errorf("err = %v, want the killed rev-parse's ExitError carried (fixture: the kill must land mid-run)", err)
	}
	if errors.Is(err, ErrRefUnavailable) {
		t.Errorf("err = %v, a ctx-killed rev-parse must NOT wrap ErrRefUnavailable", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to name context.DeadlineExceeded", err)
	}
	if strings.Contains(err.Error(), "fetch") {
		t.Errorf("err = %q, a ctx-killed rev-parse must not reach the fetch path", err)
	}
	if _, serr := os.Stat(marker); !os.IsNotExist(serr) {
		t.Errorf("a fetch child was spawned (marker stat err = %v)", serr)
	}
}

// TestExportTree_CancelledPostFetchRevParseIsCtxError pins the post-fetch ctx
// check: the first rev-parse misses (exit 1), the fetch succeeds, and the
// SECOND rev-parse hangs until the context deadline kills it. That is a ctx
// error, not "still not a commit after fetching" (ErrRefUnavailable).
func TestExportTree_CancelledPostFetchRevParseIsCtxError(t *testing.T) {
	fakeGit(t, `for a in "$@"; do
  if [ "$a" = fetch ]; then : > "$marker"; exit 0; fi
  if [ "$a" = rev-parse ]; then
    if [ -e "$marker" ]; then exec /bin/sleep 30; fi
    exit 1
  fi
done
exit 1`)
	ctx, cancel := context.WithTimeout(context.Background(), timescale.D(300*time.Millisecond))
	defer cancel()

	_, _, _, _, err := ExportTree(ctx, t.TempDir(), strings.Repeat("cd", 20), DefaultLimits())
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrRefUnavailable) {
		t.Errorf("err = %v, a ctx-killed post-fetch rev-parse must NOT wrap ErrRefUnavailable", err)
	}
	if strings.Contains(err.Error(), "still not a commit") {
		t.Errorf("err = %q, must not be reported as a post-fetch miss", err)
	}
}

func TestIsFullObjectID(t *testing.T) {
	for ref, want := range map[string]bool{
		strings.Repeat("a", 40):       true,
		strings.Repeat("F", 40):       true,
		strings.Repeat("0", 64):       true,
		strings.Repeat("a", 39):       false,
		strings.Repeat("a", 41):       false,
		strings.Repeat("a", 63):       false,
		strings.Repeat("g", 40):       false,
		"-" + strings.Repeat("a", 39): false,
		"HEAD":                        false,
		"fishhawk/run-x-consolidated": false,
		"":                            false,
	} {
		if got := isFullObjectID(ref); got != want {
			t.Errorf("isFullObjectID(%q) = %v, want %v", ref, got, want)
		}
	}
}

func TestStderrTail(t *testing.T) {
	if got := stderrTail(nil); got != "(no stderr)" {
		t.Errorf("stderrTail(nil) = %q, want (no stderr)", got)
	}
	if got := stderrTail([]byte("fatal: x\n\n  more\n")); got != "fatal: x more" {
		t.Errorf("stderrTail = %q, want single-line", got)
	}
	long := strings.Repeat("a", 1000) + "END"
	if got := stderrTail([]byte(long)); len(got) > fetchStderrTail || !strings.HasSuffix(got, "END") {
		t.Errorf("stderrTail(long) = %d bytes ending %q, want <= %d keeping the tail", len(got), got[len(got)-3:], fetchStderrTail)
	}
}

// TestExportTree_ZeroFetchTimeoutUsesDefault: a zero Limits.FetchTimeout falls
// back to the 30s default rather than a zero deadline that would fail every
// fetch-on-miss instantly.
func TestExportTree_ZeroFetchTimeoutUsesDefault(t *testing.T) {
	if got := (Limits{}).resolved().FetchTimeout; got != defaultFetchTimeout {
		t.Errorf("Limits{}.resolved().FetchTimeout = %s, want %s", got, defaultFetchTimeout)
	}
	if got := DefaultLimits().FetchTimeout; got != defaultFetchTimeout {
		t.Errorf("DefaultLimits().FetchTimeout = %s, want %s", got, defaultFetchTimeout)
	}
	f := newOriginFixture(t)
	_, commit, _, cleanup, err := ExportTree(context.Background(), f.local, f.c2, Limits{})
	if err != nil || commit != f.c2 {
		t.Fatalf("ExportTree with zero Limits: commit=%q err=%v, want the origin-only commit fetched", commit, err)
	}
	cleanup()
}
