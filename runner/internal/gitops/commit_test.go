package gitops

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runReal initializes a git repo on disk and exercises CommitAndPush
// end-to-end, with origin pointed at a bare local repo so push
// actually completes. HTTPS auth is the calling environment's
// concern (actions/checkout's extraheader in production); these
// tests only exercise the local-bare-repo path.
func TestCommitAndPush_RealRepo_HappyPath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	bare := filepath.Join(dir, "origin.git")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}

	mustGit(t, repo, "init", "--initial-branch=main")
	mustGit(t, repo, "config", "user.name", "init")
	mustGit(t, repo, "config", "user.email", "init@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "initial")

	// Bare remote so `push` works without network.
	mustGit(t, repo, "init", "--bare", bare)
	mustGit(t, repo, "remote", "add", "origin", bare)

	// Agent-style modification.
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Pusher{}
	res, err := p.CommitAndPush(context.Background(), CommitAndPushArgs{
		RepoDir:       repo,
		Branch:        "fishhawk/test/branch",
		CommitMessage: "Test commit\n\nMulti-line body.",
		RemoteURL:     bare,
	})
	if err != nil {
		t.Fatalf("CommitAndPush: %v", err)
	}
	if res.NoChanges {
		t.Error("expected NoChanges=false on dirty tree")
	}
	if res.HeadSHA == "" {
		t.Error("HeadSHA empty")
	}
	if res.BaseSHA == "" {
		t.Error("BaseSHA empty")
	}
	if res.HeadSHA == res.BaseSHA {
		t.Error("HeadSHA should differ from BaseSHA after a commit")
	}
	// TreeSHA is the pushed commit's tree object hash (#960).
	if res.TreeSHA == "" {
		t.Error("TreeSHA empty")
	}
	if got := mustGitOut(t, repo, "rev-parse", res.HeadSHA+"^{tree}"); got != res.TreeSHA {
		t.Errorf("TreeSHA = %q, want rev-parse %s^{tree} = %q", res.TreeSHA, res.HeadSHA, got)
	}

	// Verify the branch landed in the bare remote.
	out, err := exec.Command("git", "--git-dir="+bare, "rev-parse", "fishhawk/test/branch").Output()
	if err != nil {
		t.Fatalf("verify branch in bare: %v", err)
	}
	if strings.TrimSpace(string(out)) != res.HeadSHA {
		t.Errorf("bare branch sha = %q, want %q", strings.TrimSpace(string(out)), res.HeadSHA)
	}
}

// TestCommitAndPush_AppBotAuthorIdentity is the consumer→git boundary of
// the #722 seam: when the backend-resolved App bot identity is threaded
// through AuthorName/AuthorEmail, the produced commit must carry exactly
// that author name and the `<id>+<slug>[bot]@users.noreply.github.com`
// email, so App-backed commits attribute to the App's bot account.
func TestCommitAndPush_AppBotAuthorIdentity(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	bare := filepath.Join(dir, "origin.git")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "--initial-branch=main")
	mustGit(t, repo, "config", "user.name", "init")
	mustGit(t, repo, "config", "user.email", "init@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "initial")
	mustGit(t, repo, "init", "--bare", bare)

	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	const (
		wantName  = "fishhawk[bot]"
		wantEmail = "41898282+fishhawk[bot]@users.noreply.github.com"
	)
	p := &Pusher{}
	if _, err := p.CommitAndPush(context.Background(), CommitAndPushArgs{
		RepoDir:       repo,
		Branch:        "fishhawk/test/identity",
		CommitMessage: "Identity commit",
		RemoteURL:     bare,
		AuthorName:    wantName,
		AuthorEmail:   wantEmail,
	}); err != nil {
		t.Fatalf("CommitAndPush: %v", err)
	}

	gotName := mustGitOut(t, repo, "log", "-1", "--format=%an")
	gotEmail := mustGitOut(t, repo, "log", "-1", "--format=%ae")
	if gotName != wantName {
		t.Errorf("commit author name = %q, want %q", gotName, wantName)
	}
	if gotEmail != wantEmail {
		t.Errorf("commit author email = %q, want %q", gotEmail, wantEmail)
	}
}

func TestCommitAndPush_NoChangesShortCircuits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "--initial-branch=main")
	mustGit(t, repo, "config", "user.name", "init")
	mustGit(t, repo, "config", "user.email", "init@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "initial")

	p := &Pusher{}
	// Working tree is clean; CommitAndPush should short-circuit
	// without trying to push (which would fail without a remote).
	res, err := p.CommitAndPush(context.Background(), CommitAndPushArgs{
		RepoDir:       repo,
		Branch:        "fishhawk/should-not-be-created",
		CommitMessage: "x",
		RemoteURL:     "https://example.com/x/y",
	})
	if err != nil {
		t.Fatalf("CommitAndPush: %v", err)
	}
	if !res.NoChanges {
		t.Error("expected NoChanges=true on clean tree")
	}
	if res.HeadSHA != "" {
		t.Errorf("HeadSHA = %q, want empty when no changes", res.HeadSHA)
	}
	if res.TreeSHA != "" {
		t.Errorf("TreeSHA = %q, want empty when no changes", res.TreeSHA)
	}
	if res.BaseSHA == "" {
		t.Error("BaseSHA should still be populated even on no-changes")
	}

	// Branch should NOT exist in the local repo (we short-circuited
	// before checkout).
	out, _ := exec.Command("git", "-C", repo, "branch", "--list", "fishhawk/should-not-be-created").Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("branch was created on no-changes path: %q", out)
	}
}

// TestTreeSHA_MetadataIndependent guards the verified-SHA invariant's core
// git assumption (#960): `git rev-parse <rev>^{tree}` peels a commit to its
// tree object hash (gitrevisions(7)), and that hash is content-addressed —
// it depends only on the snapshot (content + modes + paths), never on commit
// message, author, or timestamp. Two commits with identical content but
// different metadata must yield EQUAL ^{tree} hashes (so the gates' verdict
// transfers from the throwaway commit to the differently-authored real
// commit), and a content change must yield a DIFFERENT hash.
func TestTreeSHA_MetadataIndependent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	mustGit(t, repo, "init", "--initial-branch=main")
	mustGit(t, repo, "config", "user.name", "one")
	mustGit(t, repo, "config", "user.email", "one@example.com")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("same content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "first message")
	treeA := mustGitOut(t, repo, "rev-parse", "HEAD^{tree}")

	// Same snapshot, entirely different commit metadata.
	mustGit(t, repo, "checkout", "--orphan", "other")
	mustGit(t, repo, "config", "user.name", "two")
	mustGit(t, repo, "config", "user.email", "two@example.com")
	mustGit(t, repo, "add", "-A")
	cmd := exec.Command("git", "-C", repo, "commit", "-m", "completely different message")
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_DATE=2005-04-07T22:13:13Z",
		"GIT_COMMITTER_DATE=2005-04-07T22:13:13Z")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("orphan commit: %v\n%s", err, out)
	}
	treeB := mustGitOut(t, repo, "rev-parse", "HEAD^{tree}")
	if treeA != treeB {
		t.Errorf("identical content must yield equal tree hashes: %q vs %q", treeA, treeB)
	}

	// A content change must move the tree hash.
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("different content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "content change")
	if treeC := mustGitOut(t, repo, "rev-parse", "HEAD^{tree}"); treeC == treeA {
		t.Errorf("changed content must yield a different tree hash, both %q", treeC)
	}
}

func TestCommitAndPush_RejectsBadInputs(t *testing.T) {
	cases := map[string]CommitAndPushArgs{
		"missing repo dir":   {Branch: "b", RemoteURL: "https://x/y/z"},
		"missing branch":     {RepoDir: ".", RemoteURL: "https://x/y/z"},
		"missing remote URL": {RepoDir: ".", Branch: "b"},
	}
	p := &Pusher{}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := p.CommitAndPush(context.Background(), args)
			if err == nil {
				t.Error("expected error")
			}
		})
	}
}

// TestStageScoped_StagesDeclaredExcludesStray is the #581 gating test:
// given one declared file plus one undeclared stray file, scope-bounded
// staging stages exactly the declared path and reports the stray as
// drift without staging it. Fails if per-path staging pulled in the
// stray or if drift exclusion regressed.
func TestStageScoped_StagesDeclaredExcludesStray(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)

	// Declared modification + an undeclared untracked stray.
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "stray.pid"), []byte("12345\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Pusher{}
	drift, err := p.StageScoped(context.Background(), repo, []string{"README.md"})
	if err != nil {
		t.Fatalf("StageScoped: %v", err)
	}

	staged := mustGitOut(t, repo, "diff", "--cached", "--name-only")
	if staged != "README.md" {
		t.Errorf("staged files = %q, want only README.md", staged)
	}
	if len(drift) != 1 || drift[0] != "stray.pid" {
		t.Errorf("drift = %v, want [stray.pid]", drift)
	}
}

// TestStageScoped_StagesFileInBrandNewDir is the #691 gating test: when a
// declared file lives inside an entirely-untracked new directory, plain
// `git status --porcelain` collapses the directory to one entry that
// matches no file-level scope path, so the declared file never stages and
// the stage fails as a false category-B. The -uall flag enumerates the
// untracked files individually so the declared one matches and stages
// while its undeclared sibling is still surfaced as drift.
func TestStageScoped_StagesFileInBrandNewDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)

	// A brand-new, entirely-untracked directory with two files: one
	// declared in scope, one undeclared sibling.
	newDir := filepath.Join(repo, "pkg", "budget")
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newDir, "budget.go"), []byte("package budget\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newDir, "extra.go"), []byte("package budget\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Pusher{}
	drift, err := p.StageScoped(context.Background(), repo, []string{"pkg/budget/budget.go"})
	if err != nil {
		t.Fatalf("StageScoped: %v", err)
	}

	staged := mustGitOut(t, repo, "diff", "--cached", "--name-only")
	if staged != "pkg/budget/budget.go" {
		t.Errorf("staged files = %q, want only pkg/budget/budget.go", staged)
	}
	if len(drift) != 1 || drift[0] != "pkg/budget/extra.go" {
		t.Errorf("drift = %v, want [pkg/budget/extra.go]", drift)
	}
}

// TestStageScoped_DirPrefixStagesFolderContents is the #824 gating test: a
// trailing-slash scope entry is a folded DIRECTORY whose created files should
// all stage, so a directory the operator names via add_scope_files actually
// reaches the commit. A file created OUTSIDE the prefix is still drift, and
// the exact-match entries keep their precise behavior.
func TestStageScoped_DirPrefixStagesFolderContents(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)

	// Two created files under a folded directory plus an out-of-prefix stray.
	newDir := filepath.Join(repo, "testdata", "corpus", "newcase")
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newDir, "input.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newDir, "expected.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "stray.pid"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Pusher{}
	drift, err := p.StageScoped(context.Background(), repo, []string{"testdata/corpus/newcase/"})
	if err != nil {
		t.Fatalf("StageScoped: %v", err)
	}

	staged := mustGitOut(t, repo, "diff", "--cached", "--name-only")
	want := "testdata/corpus/newcase/expected.json\ntestdata/corpus/newcase/input.json"
	if staged != want {
		t.Errorf("staged files = %q, want %q", staged, want)
	}
	if len(drift) != 1 || drift[0] != "stray.pid" {
		t.Errorf("drift = %v, want [stray.pid]", drift)
	}
}

// TestStageScoped_ExactEntryDoesNotPrefixMatch pins the #824 condition that the
// trailing-slash directory matching must NOT bleed into exact-path entries: a
// plain (non-slash) scope entry stays exact-match, so a declared file must not
// prefix-match a sibling that shares its name as a prefix (foo/bar.go must not
// stage foo/bar.go.bak).
func TestStageScoped_ExactEntryDoesNotPrefixMatch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)

	dir := filepath.Join(repo, "foo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bar.go"), []byte("package foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bar.go.bak"), []byte("backup\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Pusher{}
	drift, err := p.StageScoped(context.Background(), repo, []string{"foo/bar.go"})
	if err != nil {
		t.Fatalf("StageScoped: %v", err)
	}

	staged := mustGitOut(t, repo, "diff", "--cached", "--name-only")
	if staged != "foo/bar.go" {
		t.Errorf("staged files = %q, want only foo/bar.go", staged)
	}
	if len(drift) != 1 || drift[0] != "foo/bar.go.bak" {
		t.Errorf("drift = %v, want [foo/bar.go.bak] (sibling must not prefix-match)", drift)
	}
}

// TestUntrackedPaths_IsolatesCreatedFromModified is the #818 gate seam:
// UntrackedPaths must return only the brand-new (untracked) candidates, not
// a modified-but-tracked one — that distinction is what lets the fix-up gate
// hard-fail on a created out-of-scope file while leaving modified-out-of-scope
// drift flag-only (ADR-027).
func TestUntrackedPaths_IsolatesCreatedFromModified(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)

	// Modify the tracked README (out-of-scope, but tracked → modified) and
	// create a brand-new untracked out-of-scope file.
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "newfile.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	created, err := UntrackedPaths(context.Background(), repo, []string{"README.md", "newfile.go"})
	if err != nil {
		t.Fatalf("UntrackedPaths: %v", err)
	}
	if len(created) != 1 || created[0] != "newfile.go" {
		t.Errorf("created = %v, want [newfile.go] (modified-tracked README excluded)", created)
	}

	// No candidate untracked → empty.
	none, err := UntrackedPaths(context.Background(), repo, []string{"README.md"})
	if err != nil {
		t.Fatalf("UntrackedPaths (none): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("created = %v, want empty when no candidate is untracked", none)
	}

	// Empty candidates → empty, no git invocation needed.
	if got, err := UntrackedPaths(context.Background(), repo, nil); err != nil || len(got) != 0 {
		t.Errorf("UntrackedPaths(nil) = %v, %v; want empty, nil", got, err)
	}
}

// TestErrFixupCreatedOutOfScope_WrapsGeneralSentinel pins the error-wrapping
// relationship the runner's category-B classification relies on (#825):
// ErrFixupCreatedOutOfScope is a specialization that wraps ErrCreatedOutOfScope,
// so a single errors.Is(err, ErrCreatedOutOfScope) check matches both the
// open-PR and fix-up wrapped errors. The reverse must NOT hold — the general
// sentinel is not the fix-up specialization.
func TestErrFixupCreatedOutOfScope_WrapsGeneralSentinel(t *testing.T) {
	if !errors.Is(ErrFixupCreatedOutOfScope, ErrCreatedOutOfScope) {
		t.Error("ErrFixupCreatedOutOfScope must wrap ErrCreatedOutOfScope")
	}
	if errors.Is(ErrCreatedOutOfScope, ErrFixupCreatedOutOfScope) {
		t.Error("ErrCreatedOutOfScope must NOT match the fix-up specialization")
	}
}

// TestCommitAndPush_ScopeBounded_CommitsOnlyDeclared exercises the full
// commit boundary: the stray file is excluded from the commit and
// surfaced as ScopeDrift while still left dirty in the working tree.
func TestCommitAndPush_ScopeBounded_CommitsOnlyDeclared(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	bare := filepath.Join(dir, "origin.git")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "--initial-branch=main")
	mustGit(t, repo, "config", "user.name", "init")
	mustGit(t, repo, "config", "user.email", "init@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "initial")
	mustGit(t, repo, "init", "--bare", bare)
	mustGit(t, repo, "remote", "add", "origin", bare)

	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "stray.pid"), []byte("12345\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Pusher{}
	res, err := p.CommitAndPush(context.Background(), CommitAndPushArgs{
		RepoDir:       repo,
		Branch:        "fishhawk/scope/branch",
		CommitMessage: "Scoped commit",
		RemoteURL:     bare,
		ScopeFiles:    []string{"README.md"},
	})
	if err != nil {
		t.Fatalf("CommitAndPush: %v", err)
	}
	if res.NoChanges {
		t.Error("expected NoChanges=false (README.md is in scope and dirty)")
	}
	if len(res.ScopeDrift) != 1 || res.ScopeDrift[0] != "stray.pid" {
		t.Errorf("ScopeDrift = %v, want [stray.pid]", res.ScopeDrift)
	}
	// The commit touched exactly the declared path.
	committed := mustGitOut(t, repo, "diff", "--name-only", res.BaseSHA, res.HeadSHA)
	if committed != "README.md" {
		t.Errorf("committed files = %q, want only README.md", committed)
	}
	// The stray file stays dirty in the working tree (excluded, not lost).
	status := mustGitOut(t, repo, "status", "--porcelain")
	if !strings.Contains(status, "stray.pid") {
		t.Errorf("stray.pid should still be dirty in the working tree; status = %q", status)
	}
}

// TestCommitAndPush_ScopeBounded_AllStrayIsNoChanges covers the case
// where every dirty file is out of scope: nothing is staged, so the
// commit is short-circuited as NoChanges rather than failing, and the
// strays are reported as drift.
func TestCommitAndPush_ScopeBounded_AllStrayIsNoChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "stray.pid"), []byte("12345\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Pusher{}
	res, err := p.CommitAndPush(context.Background(), CommitAndPushArgs{
		RepoDir:       repo,
		Branch:        "fishhawk/scope/none",
		CommitMessage: "should not commit",
		RemoteURL:     "https://example.com/x/y",
		ScopeFiles:    []string{"README.md"},
	})
	if err != nil {
		t.Fatalf("CommitAndPush: %v", err)
	}
	if !res.NoChanges {
		t.Error("expected NoChanges=true when all dirty files are out of scope")
	}
	if len(res.ScopeDrift) != 1 || res.ScopeDrift[0] != "stray.pid" {
		t.Errorf("ScopeDrift = %v, want [stray.pid]", res.ScopeDrift)
	}
}

// TestCommitAndPush_PreStagedOutOfScopeBinaryExcluded is the #980 regression
// test reproducing the incident shape (run 4c2c6374): an in-scope edit plus an
// out-of-scope untracked binary that the agent PRE-STAGED with its own
// `git add`. Before the StageScoped mixed reset, the binary stayed in the
// index — `git commit` commits the index — so it landed in the commit while
// ScopeDrift reported it as excluded. The commit's diff-tree path set must
// exactly equal the in-scope paths, the drift must name the binary, and the
// two must agree (report == commit content). Fails on pre-#980 code.
func TestCommitAndPush_PreStagedOutOfScopeBinaryExcluded(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	bare := filepath.Join(dir, "origin.git")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "--initial-branch=main")
	mustGit(t, repo, "config", "user.name", "init")
	mustGit(t, repo, "config", "user.email", "init@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "initial")
	mustGit(t, repo, "init", "--bare", bare)
	mustGit(t, repo, "remote", "add", "origin", bare)

	// In-scope edit + an out-of-scope "binary" the agent built AND staged.
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	binPath := "cmd/tool/tool"
	if err := os.MkdirAll(filepath.Join(repo, "cmd", "tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, binPath), []byte("\x7fELF fake binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", binPath)

	p := &Pusher{}
	res, err := p.CommitAndPush(context.Background(), CommitAndPushArgs{
		RepoDir:       repo,
		Branch:        "fishhawk/scope/prestaged",
		CommitMessage: "Scoped commit",
		RemoteURL:     bare,
		ScopeFiles:    []string{"README.md"},
	})
	if err != nil {
		t.Fatalf("CommitAndPush: %v", err)
	}

	// The commit's content is exactly the in-scope set — the pre-staged
	// binary must NOT have ridden into it.
	committed := mustGitOut(t, repo, "diff-tree", "-r", "--no-commit-id", "--name-only", res.HeadSHA)
	if committed != "README.md" {
		t.Errorf("committed paths = %q, want exactly README.md (pre-staged binary excluded)", committed)
	}
	// The exclusion report names the binary…
	if len(res.ScopeDrift) != 1 || res.ScopeDrift[0] != binPath {
		t.Errorf("ScopeDrift = %v, want [%s]", res.ScopeDrift, binPath)
	}
	// …and report and commit content agree: nothing reported excluded is in
	// the commit (the #980 disagreement).
	for _, d := range res.ScopeDrift {
		if strings.Contains(committed, d) {
			t.Errorf("drift path %q reported excluded but present in commit content %q", d, committed)
		}
	}
	// The binary survives in the working tree as untracked again — excluded,
	// not lost, and visible to the #818 gate's `git ls-files --others`.
	status := mustGitOut(t, repo, "status", "--porcelain", "-uall")
	if !strings.Contains(status, "?? "+binPath) {
		t.Errorf("binary should be untracked after the commit; status = %q", status)
	}
}

// TestStageScoped_PreStagedUndeclaredIsUnstaged pins the StageScoped half of
// #980 in isolation: a file the agent pre-staged with its own `git add` but
// did not declare must be absent from the index after StageScoped (mixed
// reset → index == HEAD + declared set) and present in the drift report.
func TestStageScoped_PreStagedUndeclaredIsUnstaged(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)

	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "artifact.bin"), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "artifact.bin")

	p := &Pusher{}
	drift, err := p.StageScoped(context.Background(), repo, []string{"README.md"})
	if err != nil {
		t.Fatalf("StageScoped: %v", err)
	}

	staged := mustGitOut(t, repo, "diff", "--cached", "--name-only")
	if staged != "README.md" {
		t.Errorf("staged files = %q, want only README.md (pre-staged artifact.bin unstaged)", staged)
	}
	if len(drift) != 1 || drift[0] != "artifact.bin" {
		t.Errorf("drift = %v, want [artifact.bin]", drift)
	}
}

// TestStageScoped_PostSoftResetStagedEntry exercises the verify-fix-loop
// entry condition (#980 approval condition 1): runVerifyFixLoop's iterations
// call StageScoped, make a THROWAWAY commit, and undo it with `git reset
// --soft HEAD~1` — which leaves the scope files STAGED. The next StageScoped
// call (next iteration, or the real CommitAndPush) therefore enters with a
// staged index, not a clean one; the mixed reset must be idempotent there:
// re-partition and re-stage the declared set, keep classifying the stray as
// drift, and produce a commit containing exactly the declared paths.
func TestStageScoped_PostSoftResetStagedEntry(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)

	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "stray.pid"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Pusher{}
	// Iteration 1: stage scope-only, throwaway commit, soft reset — the
	// verify-fix loop's exact sequence (StageScoped → commitVerifyWIP →
	// gitResetSoftHEAD1).
	if _, err := p.StageScoped(context.Background(), repo, []string{"README.md"}); err != nil {
		t.Fatalf("StageScoped (iteration 1): %v", err)
	}
	mustGit(t, repo, "commit", "--no-verify", "-m", "WIP verify throwaway")
	mustGit(t, repo, "reset", "--soft", "HEAD~1")

	// Entry state for iteration 2: README.md is STAGED (soft reset preserves
	// the index), stray.pid untracked.
	if staged := mustGitOut(t, repo, "diff", "--cached", "--name-only"); staged != "README.md" {
		t.Fatalf("precondition: staged = %q, want README.md staged after reset --soft", staged)
	}

	// Iteration 2 entry: StageScoped from the STAGED state must be idempotent.
	drift, err := p.StageScoped(context.Background(), repo, []string{"README.md"})
	if err != nil {
		t.Fatalf("StageScoped (post-soft-reset entry): %v", err)
	}
	staged := mustGitOut(t, repo, "diff", "--cached", "--name-only")
	if staged != "README.md" {
		t.Errorf("staged files = %q, want only README.md after re-entry", staged)
	}
	if len(drift) != 1 || drift[0] != "stray.pid" {
		t.Errorf("drift = %v, want [stray.pid] on re-entry", drift)
	}

	// The re-staged index commits exactly the declared path.
	mustGit(t, repo, "commit", "--no-verify", "-m", "real commit")
	committed := mustGitOut(t, repo, "diff-tree", "-r", "--no-commit-id", "--name-only", "HEAD")
	if committed != "README.md" {
		t.Errorf("committed paths = %q, want exactly README.md", committed)
	}
}

// TestAssertCommitInScope_NamesViolatingPath drives the post-commit
// out-of-scope assertion (#980) directly against a raw-git crafted commit
// that bypasses StageScoped — the only way to produce the violation the
// assertion guards against. The crafted commit is PARENTED (initRepo's
// initial commit is the parent), matching the commit shape the staging path
// produces, so the diff-tree invocation is confirmed against the real shape.
// The returned error must wrap ErrCommitOutOfScope and name the violating
// path; a fully-declared commit must pass.
func TestAssertCommitInScope_NamesViolatingPath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)

	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "undeclared.bin"), []byte("oops"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "crafted out-of-scope commit")
	head := mustGitOut(t, repo, "rev-parse", "HEAD")

	p := &Pusher{}
	err := p.assertCommitInScope(context.Background(), repo, head, []string{"README.md"})
	if !errors.Is(err, ErrCommitOutOfScope) {
		t.Fatalf("assertCommitInScope = %v, want ErrCommitOutOfScope", err)
	}
	if !strings.Contains(err.Error(), "undeclared.bin") {
		t.Errorf("error %q must name the violating path undeclared.bin", err.Error())
	}
	if strings.Contains(err.Error(), "README.md") {
		t.Errorf("error %q must not name the declared in-scope path", err.Error())
	}

	// A commit whose every path is declared passes.
	if err := p.assertCommitInScope(context.Background(), repo, head, []string{"README.md", "undeclared.bin"}); err != nil {
		t.Errorf("assertCommitInScope (all declared) = %v, want nil", err)
	}
}

// TestUntrackedPaths_SeesPreStagedFileAfterStageScoped pins the #818-reach
// half of #980: `git ls-files --others` excludes index-resident files, so a
// net-new out-of-scope file the agent pre-staged was invisible to the
// created-out-of-scope gate (the run died later on the #960 tree-mismatch
// path instead of ErrCreatedOutOfScope naming the file). After StageScoped's
// mixed reset the file is untracked again and the gate sees it. Passes
// trivially without StageScoped's reset only if the file was never staged —
// the pre-staging here is what makes it the regression.
func TestUntrackedPaths_SeesPreStagedFileAfterStageScoped(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)

	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "newfile.bin"), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "newfile.bin")

	// Pre-staged → in the index → invisible to ls-files --others (the gap).
	before, err := UntrackedPaths(context.Background(), repo, []string{"newfile.bin"})
	if err != nil {
		t.Fatalf("UntrackedPaths (before): %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("precondition: pre-staged file should be invisible to UntrackedPaths, got %v", before)
	}

	p := &Pusher{}
	drift, err := p.StageScoped(context.Background(), repo, []string{"README.md"})
	if err != nil {
		t.Fatalf("StageScoped: %v", err)
	}
	if len(drift) != 1 || drift[0] != "newfile.bin" {
		t.Errorf("drift = %v, want [newfile.bin]", drift)
	}

	after, err := UntrackedPaths(context.Background(), repo, []string{"newfile.bin"})
	if err != nil {
		t.Fatalf("UntrackedPaths (after): %v", err)
	}
	if len(after) != 1 || after[0] != "newfile.bin" {
		t.Errorf("UntrackedPaths after StageScoped = %v, want [newfile.bin] (gate reach restored)", after)
	}
}

// TestMissingScopeFiles covers the pre-push scope-completeness (shortfall)
// gate (#1151): every declared concrete path the commit did NOT touch is
// returned, the committed set is returned alongside, trailing-slash directory
// prefixes are never reported, and a declared delete the commit performs counts
// as touched.
func TestMissingScopeFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	t.Run("all declared paths committed -> empty", func(t *testing.T) {
		repo := initRepo(t)
		// Modify the pre-existing README.md and create a net-new file.
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# changed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(repo, "pkg"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "pkg/new.go"), []byte("package pkg\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mustGit(t, repo, "add", "-A")
		mustGit(t, repo, "commit", "-m", "touch both declared paths")
		head := mustGitOut(t, repo, "rev-parse", "HEAD")

		missing, committed, err := MissingScopeFiles(context.Background(), repo, head, []string{"README.md", "pkg/new.go"})
		if err != nil {
			t.Fatalf("MissingScopeFiles: %v", err)
		}
		if len(missing) != 0 {
			t.Errorf("missing = %v, want empty (every declared path committed)", missing)
		}
		if !contains(committed, "README.md") || !contains(committed, "pkg/new.go") {
			t.Errorf("committed = %v, want both README.md and pkg/new.go", committed)
		}
	})

	t.Run("declared modify path not committed -> missing", func(t *testing.T) {
		repo := initRepo(t)
		// Only README.md is touched; the declared docs/extra.md is dropped (the
		// #1148 subset-PR class).
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# changed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mustGit(t, repo, "add", "-A")
		mustGit(t, repo, "commit", "-m", "touch only one declared path")
		head := mustGitOut(t, repo, "rev-parse", "HEAD")

		missing, committed, err := MissingScopeFiles(context.Background(), repo, head, []string{"README.md", "docs/extra.md"})
		if err != nil {
			t.Fatalf("MissingScopeFiles: %v", err)
		}
		if len(missing) != 1 || missing[0] != "docs/extra.md" {
			t.Errorf("missing = %v, want [docs/extra.md]", missing)
		}
		if !contains(committed, "README.md") {
			t.Errorf("committed = %v, want it to contain README.md", committed)
		}
		if contains(committed, "docs/extra.md") {
			t.Errorf("committed = %v must not contain the untouched declared path", committed)
		}
	})

	t.Run("trailing-slash dir prefix never reported missing", func(t *testing.T) {
		repo := initRepo(t)
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# changed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mustGit(t, repo, "add", "-A")
		mustGit(t, repo, "commit", "-m", "touch README only")
		head := mustGitOut(t, repo, "rev-parse", "HEAD")

		// corpus/ is a folded directory prefix; no file beneath it changed, but
		// a folded dir cannot require any specific touched path -> not missing.
		missing, _, err := MissingScopeFiles(context.Background(), repo, head, []string{"README.md", "corpus/"})
		if err != nil {
			t.Fatalf("MissingScopeFiles: %v", err)
		}
		if len(missing) != 0 {
			t.Errorf("missing = %v, want empty (dir prefix is never required)", missing)
		}
	})

	t.Run("declared delete the commit performs counts as touched", func(t *testing.T) {
		repo := initRepo(t)
		// Create a second file in the base so the run can delete it.
		if err := os.WriteFile(filepath.Join(repo, "gone.txt"), []byte("temp\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mustGit(t, repo, "add", "-A")
		mustGit(t, repo, "commit", "-m", "add gone.txt to base")

		// The run deletes the declared file and modifies README.md.
		if err := os.Remove(filepath.Join(repo, "gone.txt")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# changed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mustGit(t, repo, "add", "-A")
		mustGit(t, repo, "commit", "-m", "delete gone.txt, modify README")
		head := mustGitOut(t, repo, "rev-parse", "HEAD")

		missing, committed, err := MissingScopeFiles(context.Background(), repo, head, []string{"README.md", "gone.txt"})
		if err != nil {
			t.Fatalf("MissingScopeFiles: %v", err)
		}
		if len(missing) != 0 {
			t.Errorf("missing = %v, want empty (a declared delete is a touched path)", missing)
		}
		if !contains(committed, "gone.txt") {
			t.Errorf("committed = %v, want it to list the deleted path gone.txt", committed)
		}
	})
}

func contains(s []string, v string) bool {
	for _, e := range s {
		if e == v {
			return true
		}
	}
	return false
}

func TestPorcelainPath(t *testing.T) {
	cases := map[string]string{
		" M README.md":        "README.md",
		"?? stray.pid":        "stray.pid",
		"A  pkg/new.go":       "pkg/new.go",
		"R  old.go -> new.go": "new.go",
		`?? "with space"`:     "with space",
		"":                    "",
		"X":                   "",
	}
	for line, want := range cases {
		if got := porcelainPath(line); got != want {
			t.Errorf("porcelainPath(%q) = %q, want %q", line, got, want)
		}
	}
}

// initRepo creates a git repo with one committed README.md and returns
// its path. No remote — callers that push add their own bare repo.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "--initial-branch=main")
	mustGit(t, repo, "config", "user.name", "init")
	mustGit(t, repo, "config", "user.email", "init@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "initial")
	return repo
}

// TestCheckoutRemoteBranch_EstablishesFixupBase is the #967 fix-up
// base-establishment seam against a real repo pair: the local working
// tree starts on a DIFFERENT branch (main, the operator's incidental
// checkout) with no local copy of the PR branch, and CheckoutRemoteBranch
// must fetch the remote branch tip, update the remote-tracking ref via
// the explicit refspec, check the tree out onto it, and return the tip
// SHA. This is the concrete test for the explicit-refspec fetch
// semantics the helper relies on — it fails if the tracking ref is not
// deterministically updated.
func TestCheckoutRemoteBranch_EstablishesFixupBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	bare := filepath.Join(dir, "origin.git")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}

	mustGit(t, repo, "init", "--initial-branch=main")
	mustGit(t, repo, "config", "user.name", "init")
	mustGit(t, repo, "config", "user.email", "init@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "initial")
	mustGit(t, repo, "init", "--bare", bare)
	mustGit(t, repo, "remote", "add", "origin", bare)
	mustGit(t, repo, "push", "origin", "main")

	// Create the PR branch with a commit and push it, then return the local
	// tree to main and ERASE every local trace of the branch (local ref +
	// tracking ref) — the operator's checkout knows nothing about the run
	// branch, the shape #967 fixes.
	const branch = "fishhawk/run-12345678/stage-87654321"
	mustGit(t, repo, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(repo, "fix.txt"), []byte("fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "fix-up target")
	mustGit(t, repo, "push", "origin", branch)
	remoteTip := mustGitOut(t, repo, "rev-parse", "HEAD")
	mustGit(t, repo, "checkout", "main")
	mustGit(t, repo, "branch", "-D", branch)
	mustGit(t, repo, "update-ref", "-d", "refs/remotes/origin/"+branch)

	tip, err := CheckoutRemoteBranch(context.Background(), repo, "origin", branch)
	if err != nil {
		t.Fatalf("CheckoutRemoteBranch: %v", err)
	}
	if tip != remoteTip {
		t.Errorf("returned tip = %q, want the remote branch tip %q", tip, remoteTip)
	}
	// The working tree is now ON the branch at the fetched tip.
	if got := mustGitOut(t, repo, "symbolic-ref", "--short", "HEAD"); got != branch {
		t.Errorf("HEAD = %q, want %q", got, branch)
	}
	if got := mustGitOut(t, repo, "rev-parse", "HEAD"); got != remoteTip {
		t.Errorf("HEAD sha = %q, want %q", got, remoteTip)
	}
	// The explicit refspec updated the remote-tracking ref.
	if got := mustGitOut(t, repo, "rev-parse", "refs/remotes/origin/"+branch); got != remoteTip {
		t.Errorf("tracking ref = %q, want %q", got, remoteTip)
	}
	// The agent's edits land on the run branch from here.
	if err := os.WriteFile(filepath.Join(repo, "fix.txt"), []byte("fix v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCheckoutRemoteBranch_RemoteNameLockstep proves the fetch source and
// the tracking/checkout ref are derived from the SAME remote name — never
// a hard-coded "origin" — by running against a remote named "upstream"
// and asserting refs/remotes/upstream/<branch> is what gets updated and
// checked out. It also covers the stale-local-branch shape: a local
// branch of the same name pointing at an OLD commit is reset to the
// fetched tip by checkout -B.
func TestCheckoutRemoteBranch_RemoteNameLockstep(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	bare := filepath.Join(dir, "upstream.git")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}

	mustGit(t, repo, "init", "--initial-branch=main")
	mustGit(t, repo, "config", "user.name", "init")
	mustGit(t, repo, "config", "user.email", "init@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "initial")
	mustGit(t, repo, "init", "--bare", bare)
	mustGit(t, repo, "remote", "add", "upstream", bare)

	// Local PR branch with one commit — the STALE local state.
	const branch = "fishhawk/run-aaaabbbb/stage-ccccdddd"
	mustGit(t, repo, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(repo, "fix.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "v1")
	staleTip := mustGitOut(t, repo, "rev-parse", "HEAD")
	mustGit(t, repo, "push", "upstream", branch)

	// Advance the branch in the remote only (a fixup_pushed head the local
	// clone never fetched), then rewind the local branch to the stale tip.
	if err := os.WriteFile(filepath.Join(repo, "fix.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "v2")
	remoteTip := mustGitOut(t, repo, "rev-parse", "HEAD")
	mustGit(t, repo, "push", "upstream", branch)
	mustGit(t, repo, "reset", "--hard", staleTip)
	mustGit(t, repo, "update-ref", "-d", "refs/remotes/upstream/"+branch)
	mustGit(t, repo, "checkout", "main")

	tip, err := CheckoutRemoteBranch(context.Background(), repo, "upstream", branch)
	if err != nil {
		t.Fatalf("CheckoutRemoteBranch: %v", err)
	}
	if tip != remoteTip {
		t.Errorf("returned tip = %q, want remote tip %q (not the stale local %q)", tip, remoteTip, staleTip)
	}
	// Lockstep invariant: the UPSTREAM tracking ref was updated; no origin
	// ref was invented.
	if got := mustGitOut(t, repo, "rev-parse", "refs/remotes/upstream/"+branch); got != remoteTip {
		t.Errorf("refs/remotes/upstream/%s = %q, want %q", branch, got, remoteTip)
	}
	if out, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch).Output(); err == nil {
		t.Errorf("refs/remotes/origin/%s unexpectedly exists (= %q): tracking ref must derive from the fetch remote", branch, strings.TrimSpace(string(out)))
	}
	// The stale local branch was reset to the fetched tip.
	if got := mustGitOut(t, repo, "rev-parse", "HEAD"); got != remoteTip {
		t.Errorf("HEAD sha = %q, want fetched tip %q", got, remoteTip)
	}
	if got := mustGitOut(t, repo, "symbolic-ref", "--short", "HEAD"); got != branch {
		t.Errorf("HEAD = %q, want %q", got, branch)
	}
}

// TestCheckoutRemoteBranch_RejectsEmptyBranch pins the input contract.
func TestCheckoutRemoteBranch_RejectsEmptyBranch(t *testing.T) {
	if _, err := CheckoutRemoteBranch(context.Background(), t.TempDir(), "origin", ""); err == nil {
		t.Fatal("expected error for empty branch")
	}
}

func mustGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// TestCommitAndPush_VerifyCommit_AbortsBeforePush is the #728 gate seam:
// the VerifyCommit hook runs AFTER the scope-only commit exists but
// BEFORE the push, receives the committed HEAD SHA and the scope drift,
// and a non-nil error aborts the push — so the bare remote never
// receives the ref and the error surfaces via errors.Is.
func TestCommitAndPush_VerifyCommit_AbortsBeforePush(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	bare := filepath.Join(dir, "origin.git")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "--initial-branch=main")
	mustGit(t, repo, "config", "user.name", "init")
	mustGit(t, repo, "config", "user.email", "init@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "initial")
	mustGit(t, repo, "init", "--bare", bare)
	mustGit(t, repo, "remote", "add", "origin", bare)

	// README.md is in scope; stray.pid is drift (excluded from the commit).
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "stray.pid"), []byte("12345\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var gotSHA string
	var gotDrift []string
	called := false
	p := &Pusher{}
	_, err := p.CommitAndPush(context.Background(), CommitAndPushArgs{
		RepoDir:       repo,
		Branch:        "fishhawk/verify/branch",
		CommitMessage: "Scoped commit",
		RemoteURL:     bare,
		ScopeFiles:    []string{"README.md"},
		VerifyCommit: func(_ context.Context, headSHA string, drift []string) error {
			called = true
			gotSHA = headSHA
			gotDrift = drift
			// The commit must already exist when the hook runs.
			if headSHA == "" {
				t.Error("VerifyCommit got empty headSHA")
			}
			return ErrCommitWouldNotCompile
		},
	})
	if !called {
		t.Fatal("VerifyCommit was never invoked")
	}
	if !errors.Is(err, ErrCommitWouldNotCompile) {
		t.Fatalf("CommitAndPush error = %v, want errors.Is ErrCommitWouldNotCompile", err)
	}
	// The hook saw the real committed HEAD SHA.
	localHead := mustGitOut(t, repo, "rev-parse", "HEAD")
	if gotSHA != localHead {
		t.Errorf("VerifyCommit headSHA = %q, want committed HEAD %q", gotSHA, localHead)
	}
	// The hook saw the scope drift.
	if len(gotDrift) != 1 || gotDrift[0] != "stray.pid" {
		t.Errorf("VerifyCommit drift = %v, want [stray.pid]", gotDrift)
	}
	// The push was aborted: the bare remote has no such branch.
	check := exec.Command("git", "--git-dir="+bare, "rev-parse", "fishhawk/verify/branch")
	if out, err := check.CombinedOutput(); err == nil {
		t.Errorf("bare remote unexpectedly has the branch (push not aborted): %s", out)
	}
}

// TestCommitAndPush_VerifyCommit_PassThroughPushes confirms the happy
// path: a VerifyCommit that returns nil does not block the push.
func TestCommitAndPush_VerifyCommit_PassThroughPushes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	bare := filepath.Join(dir, "origin.git")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "--initial-branch=main")
	mustGit(t, repo, "config", "user.name", "init")
	mustGit(t, repo, "config", "user.email", "init@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "initial")
	mustGit(t, repo, "init", "--bare", bare)
	mustGit(t, repo, "remote", "add", "origin", bare)

	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Pusher{}
	res, err := p.CommitAndPush(context.Background(), CommitAndPushArgs{
		RepoDir:       repo,
		Branch:        "fishhawk/verify/ok",
		CommitMessage: "Verified commit",
		RemoteURL:     bare,
		VerifyCommit: func(_ context.Context, _ string, _ []string) error {
			return nil
		},
	})
	if err != nil {
		t.Fatalf("CommitAndPush: %v", err)
	}
	// The branch landed in the bare remote.
	out, err := exec.Command("git", "--git-dir="+bare, "rev-parse", "fishhawk/verify/ok").Output()
	if err != nil {
		t.Fatalf("verify branch in bare: %v", err)
	}
	if strings.TrimSpace(string(out)) != res.HeadSHA {
		t.Errorf("bare branch sha = %q, want %q", strings.TrimSpace(string(out)), res.HeadSHA)
	}
}

// TestCommitAndPush_UpdateTrackingRef_MaterializesRemoteRef pins #770: a
// URL push (git push <url> HEAD:<branch>) never updates the local
// remote-tracking ref refs/remotes/origin/<branch>, so the decomposition
// fan-out's remoteBranchExists read sees the shared branch as absent and
// mis-routes the next child. With UpdateTrackingRef:true the ref is
// materialized to the pushed HEAD; with it false the ref stays absent
// (the exact bug condition).
func TestCommitAndPush_UpdateTrackingRef_MaterializesRemoteRef(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	const branch = "fishhawk/run-abc123"

	// setup builds a real repo + bare remote with one initial commit and a
	// pending agent-style modification, returning the repo dir.
	setup := func(t *testing.T) (repo, bare string) {
		t.Helper()
		dir := t.TempDir()
		repo = filepath.Join(dir, "src")
		bare = filepath.Join(dir, "origin.git")
		if err := os.Mkdir(repo, 0o755); err != nil {
			t.Fatal(err)
		}
		mustGit(t, repo, "init", "--initial-branch=main")
		mustGit(t, repo, "config", "user.name", "init")
		mustGit(t, repo, "config", "user.email", "init@example.com")
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# initial\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mustGit(t, repo, "add", "-A")
		mustGit(t, repo, "commit", "-m", "initial")
		mustGit(t, repo, "init", "--bare", bare)
		mustGit(t, repo, "remote", "add", "origin", bare)
		if err := os.WriteFile(filepath.Join(repo, "child.txt"), []byte("child\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return repo, bare
	}

	t.Run("true materializes the tracking ref", func(t *testing.T) {
		repo, bare := setup(t)
		p := &Pusher{}
		res, err := p.CommitAndPush(context.Background(), CommitAndPushArgs{
			RepoDir:           repo,
			Branch:            branch,
			CommitMessage:     "child commit",
			RemoteURL:         bare,
			UpdateTrackingRef: true,
		})
		if err != nil {
			t.Fatalf("CommitAndPush: %v", err)
		}
		// The tracking ref resolves and equals the pushed HEAD.
		got := mustGitOut(t, repo, "show-ref", "--verify", "-s", "refs/remotes/origin/"+branch)
		if got != res.HeadSHA {
			t.Errorf("tracking ref = %q, want HeadSHA %q", got, res.HeadSHA)
		}
	})

	t.Run("false leaves the tracking ref absent (the #770 bug condition)", func(t *testing.T) {
		repo, bare := setup(t)
		p := &Pusher{}
		if _, err := p.CommitAndPush(context.Background(), CommitAndPushArgs{
			RepoDir:           repo,
			Branch:            branch,
			CommitMessage:     "child commit",
			RemoteURL:         bare,
			UpdateTrackingRef: false,
		}); err != nil {
			t.Fatalf("CommitAndPush: %v", err)
		}
		// show-ref --verify exits non-zero when the ref is absent.
		cmd := exec.Command("git", "show-ref", "--verify", "refs/remotes/origin/"+branch)
		cmd.Dir = repo
		if err := cmd.Run(); err == nil {
			t.Error("tracking ref unexpectedly present after URL push with UpdateTrackingRef:false")
		}
	})
}

// TestCommitAndPush_UpdateTrackingRef_KeepsForceWithLeaseFresh pins #767:
// in a local fan-out, child A pushes the shared branch via a URL push with
// UpdateTrackingRef:true; child B then commits through the SAME real Pusher
// against the SAME bare remote with RebaseFromRemote+ForceWithLease. Because
// the maintained tracking ref lets CommitAndPush pass an *explicit*
// --force-with-lease=<branch>:<sha> (a bare lease can't be associated with a
// URL push and always rejects with `(stale info)`), child B's push SUCCEEDS
// instead of rejecting. The only residual stale-lease path is an out-of-band
// branch advance (an operator fold-in mid-fan-out), which is operator
// discipline, not a runner bug. Same git-state-staleness family as #770.
func TestCommitAndPush_UpdateTrackingRef_KeepsForceWithLeaseFresh(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	const branch = "fishhawk/run-shared01"

	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	bare := filepath.Join(dir, "origin.git")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "--initial-branch=main")
	mustGit(t, repo, "config", "user.name", "init")
	mustGit(t, repo, "config", "user.email", "init@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "initial")
	mustGit(t, repo, "init", "--bare", bare)
	mustGit(t, repo, "remote", "add", "origin", bare)

	p := &Pusher{}

	// Child A: first child of the fan-out. Not subsequent → checkout -b
	// path. ForceWithLease + UpdateTrackingRef as the runner sets for
	// decomposed children.
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CommitAndPush(context.Background(), CommitAndPushArgs{
		RepoDir:           repo,
		Branch:            branch,
		CommitMessage:     "child A",
		RemoteURL:         bare,
		ForceWithLease:    true,
		UpdateTrackingRef: true,
	}); err != nil {
		t.Fatalf("child A CommitAndPush: %v", err)
	}

	// Child B: subsequent child. RebaseFromRemote + ForceWithLease. Without
	// child A having materialized refs/remotes/origin/<branch>, CommitAndPush
	// would fall back to a bare --force-with-lease, which a URL push rejects
	// with `(stale info)`. The maintained ref lets it pass an explicit lease,
	// so this push succeeds.
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resB, err := p.CommitAndPush(context.Background(), CommitAndPushArgs{
		RepoDir:           repo,
		Branch:            branch,
		CommitMessage:     "child B",
		RemoteURL:         bare,
		RebaseFromRemote:  true,
		ForceWithLease:    true,
		UpdateTrackingRef: true,
	})
	if err != nil {
		t.Fatalf("child B CommitAndPush (stale-lease regression #767): %v", err)
	}

	// The bare remote's shared branch now points at child B's HEAD.
	got := mustGitOut(t, repo, "--git-dir="+bare, "rev-parse", branch)
	if got != resB.HeadSHA {
		t.Errorf("bare branch sha = %q, want child B HeadSHA %q", got, resB.HeadSHA)
	}
}

// TestCommitAndPush_RebaseFromRemote_FetchesViaRemoteURL pins #772: the
// RebaseFromRemote path must fetch the shared branch over args.RemoteURL (the
// run's authenticated HTTPS URL), NOT the named `origin` remote, which in the
// operator's checkout is typically an SSH URL whose auth depends on an SSH
// agent that may be unavailable. The test wires `origin` to a deliberately
// unreachable SSH-style URL while passing a good bare repo as RemoteURL; the
// rebase path must still succeed and advance the bare branch to the new HEAD.
// On the old code (fetch+pull against `origin`) this fails exit 128.
func TestCommitAndPush_RebaseFromRemote_FetchesViaRemoteURL(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	const branch = "fishhawk/run-shared772"

	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	bare := filepath.Join(dir, "origin.git")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "--initial-branch=main")
	mustGit(t, repo, "config", "user.name", "init")
	mustGit(t, repo, "config", "user.email", "init@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "initial")
	mustGit(t, repo, "init", "--bare", bare)

	p := &Pusher{}

	// Child A: first child establishes the shared branch on the bare remote
	// via the checkout -b path (origin still points at the good bare repo).
	mustGit(t, repo, "remote", "add", "origin", bare)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CommitAndPush(context.Background(), CommitAndPushArgs{
		RepoDir:           repo,
		Branch:            branch,
		CommitMessage:     "child A",
		RemoteURL:         bare,
		UpdateTrackingRef: true,
	}); err != nil {
		t.Fatalf("child A CommitAndPush: %v", err)
	}

	// Now break `origin`: repoint it at an unreachable SSH URL, reproducing
	// the operator checkout whose SSH agent is unavailable. The fixed code
	// fetches args.RemoteURL (the good bare repo), so this must not matter.
	mustGit(t, repo, "remote", "set-url", "origin", "git@example.invalid:kuhlman-labs/fishhawk.git")

	// Child B: subsequent child takes the RebaseFromRemote path. The fetch
	// targets RemoteURL=bare, sidestepping the broken origin.
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resB, err := p.CommitAndPush(context.Background(), CommitAndPushArgs{
		RepoDir:           repo,
		Branch:            branch,
		CommitMessage:     "child B",
		RemoteURL:         bare,
		RebaseFromRemote:  true,
		UpdateTrackingRef: true,
	})
	if err != nil {
		t.Fatalf("child B CommitAndPush (#772 SSH-origin regression): %v", err)
	}

	// The bare remote's shared branch advanced to child B's HEAD, and the
	// reapplied stash (b.txt) is part of that commit.
	got := mustGitOut(t, repo, "--git-dir="+bare, "rev-parse", branch)
	if got != resB.HeadSHA {
		t.Errorf("bare branch sha = %q, want child B HeadSHA %q", got, resB.HeadSHA)
	}
	tree := mustGitOut(t, repo, "--git-dir="+bare, "ls-tree", "--name-only", branch)
	if !strings.Contains(tree, "b.txt") {
		t.Errorf("bare branch tree missing reapplied edit b.txt; got %q", tree)
	}
}

// TestCommitAndPush_FreshFetchBase_CutsFromFetchedTipNotAmbientHEAD pins #861
// (ADR-035 prevention): a standalone run branch must be cut from the freshly-
// FETCHED authoritative base (origin/main), NOT the ambient local HEAD, so a
// foreign commit another writer made in the same shared checkout (the #797
// shape) cannot ride in as the run branch base. The test advances the local
// ambient HEAD with a foreign commit that is NEVER pushed to the bare remote,
// writes an agent working-tree edit, then calls CommitAndPush with
// FreshFetchBase="main". It asserts: (a) the foreign commit is NOT an ancestor
// of the run-branch HEAD; (b) the run branch's parent and result.BaseSHA both
// equal the fetched origin/main tip (not the foreign ambient HEAD); (c) the
// agent edit is present in the committed tree (stash/pop preserved it across
// the base reset).
func TestCommitAndPush_FreshFetchBase_CutsFromFetchedTipNotAmbientHEAD(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	const branch = "fishhawk/run-861/stage-x"

	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	bare := filepath.Join(dir, "origin.git")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "--initial-branch=main")
	mustGit(t, repo, "config", "user.name", "init")
	mustGit(t, repo, "config", "user.email", "init@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "initial")

	// Establish the bare remote with main at the authoritative tip and push it.
	mustGit(t, repo, "init", "--bare", bare)
	mustGit(t, repo, "remote", "add", "origin", bare)
	mustGit(t, repo, "push", "origin", "main")
	authoritativeTip := mustGitOut(t, repo, "rev-parse", "HEAD")

	// A FOREIGN writer advances the local ambient HEAD with a commit that is
	// NEVER pushed to the bare remote — the 509a62c analogue from #797. On the
	// old code (checkout -b from ambient HEAD) this commit would become the run
	// branch base.
	if err := os.WriteFile(filepath.Join(repo, "foreign.txt"), []byte("foreign\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "foreign writer commit (never pushed)")
	foreignSHA := mustGitOut(t, repo, "rev-parse", "HEAD")

	// The agent's uncommitted working-tree edit, which must survive the base
	// reset via stash/pop.
	if err := os.WriteFile(filepath.Join(repo, "agent.txt"), []byte("agent edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Pusher{}
	res, err := p.CommitAndPush(context.Background(), CommitAndPushArgs{
		RepoDir:        repo,
		Branch:         branch,
		CommitMessage:  "agent stage commit",
		RemoteURL:      bare,
		FreshFetchBase: "main",
	})
	if err != nil {
		t.Fatalf("CommitAndPush (FreshFetchBase): %v", err)
	}

	// (a) The foreign ambient-HEAD commit must NOT be an ancestor of the run
	// branch HEAD — it was laundered out by cutting from the fetched base.
	if isAncestor(t, repo, foreignSHA, "HEAD") {
		t.Errorf("foreign commit %s is an ancestor of run-branch HEAD; it leaked into the branch base", foreignSHA)
	}

	// (b) The run branch's first parent and result.BaseSHA both equal the
	// fetched origin/main tip, not the foreign ambient HEAD.
	parent := mustGitOut(t, repo, "rev-parse", "HEAD^")
	if parent != authoritativeTip {
		t.Errorf("run branch parent = %q, want fetched origin/main tip %q", parent, authoritativeTip)
	}
	if res.BaseSHA != authoritativeTip {
		t.Errorf("result.BaseSHA = %q, want fetched origin/main tip %q", res.BaseSHA, authoritativeTip)
	}
	if res.BaseSHA == foreignSHA {
		t.Errorf("result.BaseSHA = foreign ambient HEAD %q; must be the fetched tip", foreignSHA)
	}

	// (c) The agent edit is present in the committed tree (stash/pop preserved
	// it across the checkout -B FETCH_HEAD base reset).
	tree := mustGitOut(t, repo, "ls-tree", "--name-only", "-r", "HEAD")
	if !strings.Contains(tree, "agent.txt") {
		t.Errorf("committed tree missing agent edit agent.txt; got %q", tree)
	}
	// And the foreign file is NOT in the tree (it was never on the fetched base
	// and the agent didn't create it).
	if strings.Contains(tree, "foreign.txt") {
		t.Errorf("committed tree unexpectedly contains foreign.txt; got %q", tree)
	}
}

// TestCommitAndPush_FreshFetchBase_StashPopConflict drives the #866 hardened
// pop-conflict path end-to-end against a real bare remote. origin/main advances
// a line that the agent's uncommitted working-tree edit ALSO changes
// (divergent edit to the same line), so reapplying the stashed edit onto the
// freshly-fetched base conflicts. It asserts: (a) the error is
// ErrBaseRebaseConflict; (b) the stash entry survives; (c) the working tree is
// clean with no unmerged paths after the reset --hard abort; (d) nothing was
// pushed to the bare remote.
func TestCommitAndPush_FreshFetchBase_StashPopConflict(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	const branch = "fishhawk/run-866/stage-x"

	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	bare := filepath.Join(dir, "origin.git")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "--initial-branch=main")
	mustGit(t, repo, "config", "user.name", "init")
	mustGit(t, repo, "config", "user.email", "init@example.com")
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("base line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "initial")

	// Bare remote with main at the initial tip.
	mustGit(t, repo, "init", "--bare", bare)
	mustGit(t, repo, "remote", "add", "origin", bare)
	mustGit(t, repo, "push", "origin", "main")

	// origin/main advances the shared line, then the local checkout is reset
	// back to the initial commit so the advance only exists on the remote tip
	// (the fresh fetch will pull it in). This mirrors the authoritative base
	// moving ahead of the ambient checkout.
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("remote-advanced line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "remote advances shared line")
	mustGit(t, repo, "push", "origin", "main")
	mustGit(t, repo, "reset", "--hard", "HEAD~1")

	// The agent's uncommitted edit to the SAME line — divergent from the remote
	// advance, so reapplying it onto the fetched tip conflicts.
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("agent-edited line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Pusher{}
	_, err := p.CommitAndPush(context.Background(), CommitAndPushArgs{
		RepoDir:        repo,
		Branch:         branch,
		CommitMessage:  "agent stage commit",
		RemoteURL:      bare,
		FreshFetchBase: "main",
	})

	// (a) The error is the dedicated rebase-conflict sentinel.
	if !errors.Is(err, ErrBaseRebaseConflict) {
		t.Fatalf("CommitAndPush error = %v, want ErrBaseRebaseConflict", err)
	}

	// (a2) The error is the typed *BaseRebaseConflictError carrying the
	// conflict context for the runner's bounded re-invoke (#989): the
	// conflicted path, the conflict-marker hunks captured before the abort,
	// and the agent's stashed patch captured after it.
	var bre *BaseRebaseConflictError
	if !errors.As(err, &bre) {
		t.Fatalf("CommitAndPush error = %v, want *BaseRebaseConflictError via errors.As", err)
	}
	if len(bre.ConflictPaths) != 1 || bre.ConflictPaths[0] != "shared.txt" {
		t.Errorf("ConflictPaths = %v, want [shared.txt]", bre.ConflictPaths)
	}
	if !strings.Contains(bre.ConflictHunks, "<<<<<<<") {
		t.Errorf("ConflictHunks must contain conflict markers, got: %q", bre.ConflictHunks)
	}
	if !strings.Contains(bre.StashPatch, "agent-edited line") {
		t.Errorf("StashPatch must contain the agent's stashed edit, got: %q", bre.StashPatch)
	}

	// (b) The stash entry survives the conflicted pop — recoverable.
	if list := mustGitOut(t, repo, "stash", "list"); strings.TrimSpace(list) == "" {
		t.Error("stash list is empty; the conflicted pop must leave the stash entry recoverable")
	}

	// (c) The working tree is clean with no unmerged paths after reset --hard.
	if unmerged := mustGitOut(t, repo, "ls-files", "--unmerged"); strings.TrimSpace(unmerged) != "" {
		t.Errorf("ls-files --unmerged not empty after abort: %q", unmerged)
	}
	if status := mustGitOut(t, repo, "status", "--porcelain"); strings.Contains(status, "UU") {
		t.Errorf("working tree has conflict markers (UU) after abort: %q", status)
	}

	// (d) Nothing was pushed — the run branch does not exist on the bare remote.
	out, lsErr := exec.Command("git", "--git-dir="+bare, "ls-remote", bare, branch).Output()
	if lsErr != nil {
		t.Fatalf("ls-remote: %v", lsErr)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("run branch %q was pushed to the bare remote on a conflicted pop: %q", branch, string(out))
	}
}

// TestCommitAndPush_RebaseFromRemote_SiblingAppendConflict_CapturesContext pins
// the exact #989 regression shape (run 8342436e / child 4e595927): a shared
// decomposition branch carries an earlier sibling's commit that APPENDED at an
// anchor line, and the current child's uncommitted working tree appends a
// different line at the SAME anchor — a trivially-resolvable keep-both
// conflict. The stash-reapply onto the fetched shared-branch tip conflicts,
// and the typed error must carry BOTH sides of the conflict (the sibling's
// committed addition in the hunks, the child's own addition in the stash
// patch) so the re-invoke prompt can instruct a keep-both re-land.
func TestCommitAndPush_RebaseFromRemote_SiblingAppendConflict_CapturesContext(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	const branch = "fishhawk/run-989shared"

	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	bare := filepath.Join(dir, "origin.git")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "--initial-branch=main")
	mustGit(t, repo, "config", "user.name", "init")
	mustGit(t, repo, "config", "user.email", "init@example.com")
	if err := os.WriteFile(filepath.Join(repo, "registry.txt"), []byte("header\nanchor\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "initial")
	mustGit(t, repo, "init", "--bare", bare)
	mustGit(t, repo, "remote", "add", "origin", bare)

	// Child 1's commit lands on the shared branch on the remote: an append at
	// the anchor. The local checkout is reset back so the addition only exists
	// on the remote tip (the RebaseFromRemote fetch pulls it in).
	mustGit(t, repo, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(repo, "registry.txt"), []byte("header\nanchor\nchild-one addition\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "child 1 appends at anchor")
	mustGit(t, repo, "push", "origin", branch)
	remoteTip := mustGitOut(t, repo, "rev-parse", "HEAD")
	mustGit(t, repo, "reset", "--hard", "HEAD~1")

	// Child 2's uncommitted append at the SAME anchor.
	if err := os.WriteFile(filepath.Join(repo, "registry.txt"), []byte("header\nanchor\nchild-two addition\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Pusher{}
	_, err := p.CommitAndPush(context.Background(), CommitAndPushArgs{
		RepoDir:          repo,
		Branch:           branch,
		CommitMessage:    "child 2",
		RemoteURL:        bare,
		RebaseFromRemote: true,
	})

	if !errors.Is(err, ErrBaseRebaseConflict) {
		t.Fatalf("CommitAndPush error = %v, want ErrBaseRebaseConflict", err)
	}
	var bre *BaseRebaseConflictError
	if !errors.As(err, &bre) {
		t.Fatalf("CommitAndPush error = %v, want *BaseRebaseConflictError via errors.As", err)
	}
	if len(bre.ConflictPaths) != 1 || bre.ConflictPaths[0] != "registry.txt" {
		t.Errorf("ConflictPaths = %v, want [registry.txt]", bre.ConflictPaths)
	}
	// Both-sides context: the hunks carry the conflict markers plus the
	// sibling's committed addition AND the child's own addition (the
	// half-applied working tree holds both between the markers); the stash
	// patch carries the child's un-landed slice.
	if !strings.Contains(bre.ConflictHunks, "<<<<<<<") {
		t.Errorf("ConflictHunks must contain conflict markers, got: %q", bre.ConflictHunks)
	}
	if !strings.Contains(bre.ConflictHunks, "child-one addition") ||
		!strings.Contains(bre.ConflictHunks, "child-two addition") {
		t.Errorf("ConflictHunks must carry both sides of the append conflict, got: %q", bre.ConflictHunks)
	}
	if !strings.Contains(bre.StashPatch, "child-two addition") {
		t.Errorf("StashPatch must contain the child's own stashed addition, got: %q", bre.StashPatch)
	}

	// Clean-abort invariants hold unchanged (#866): stash preserved, tree
	// clean, remote tip untouched.
	if list := mustGitOut(t, repo, "stash", "list"); strings.TrimSpace(list) == "" {
		t.Error("stash list is empty; the conflicted pop must leave the stash entry recoverable")
	}
	if unmerged := mustGitOut(t, repo, "ls-files", "--unmerged"); strings.TrimSpace(unmerged) != "" {
		t.Errorf("ls-files --unmerged not empty after abort: %q", unmerged)
	}
	if got := mustGitOut(t, repo, "--git-dir="+bare, "rev-parse", branch); got != remoteTip {
		t.Errorf("remote branch tip = %q, want unchanged %q (no push on conflicted pop)", got, remoteTip)
	}
}

// TestCommitAndPush_FreshFetchBase_ConflictCaptureFailure pins the #989
// degradation contract: when the CONFIRMED-conflict context captures fail (the
// `git diff` hunk read and the `git stash show -p` patch read both error), the
// error keeps full ErrBaseRebaseConflict semantics — typed, unwrapping to the
// sentinel, conflicted paths intact from the already-read unmerged listing —
// with the failed captures degraded to empty fields, and the #866 clean-abort
// invariants (stash preserved, tree reset, no push) still hold.
func TestCommitAndPush_FreshFetchBase_ConflictCaptureFailure(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	const branch = "fishhawk/run-989degraded"

	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	bare := filepath.Join(dir, "origin.git")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "--initial-branch=main")
	mustGit(t, repo, "config", "user.name", "init")
	mustGit(t, repo, "config", "user.email", "init@example.com")
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("base line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "initial")
	mustGit(t, repo, "init", "--bare", bare)
	mustGit(t, repo, "remote", "add", "origin", bare)
	mustGit(t, repo, "push", "origin", "main")

	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("remote-advanced line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "remote advances shared line")
	mustGit(t, repo, "push", "origin", "main")
	mustGit(t, repo, "reset", "--hard", "HEAD~1")

	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("agent-edited line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Cmd override: delegate to real git EXCEPT the two best-effort context
	// captures — the bare `git diff` hunk read and the `git stash show`
	// patch read — which are rewritten to an unknown flag so they exit
	// non-zero, while the conflict probe, the abort, and everything else run
	// normally against the real repo.
	p := &Pusher{
		Cmd: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			if len(args) == 1 && args[0] == "diff" {
				return exec.CommandContext(ctx, name, "diff", "--fishhawk-no-such-flag")
			}
			if len(args) >= 2 && args[0] == "stash" && args[1] == "show" {
				return exec.CommandContext(ctx, name, "stash", "show", "--fishhawk-no-such-flag")
			}
			return exec.CommandContext(ctx, name, args...)
		},
	}
	_, err := p.CommitAndPush(context.Background(), CommitAndPushArgs{
		RepoDir:        repo,
		Branch:         branch,
		CommitMessage:  "agent stage commit",
		RemoteURL:      bare,
		FreshFetchBase: "main",
	})

	if !errors.Is(err, ErrBaseRebaseConflict) {
		t.Fatalf("CommitAndPush error = %v, want ErrBaseRebaseConflict despite capture failures", err)
	}
	var bre *BaseRebaseConflictError
	if !errors.As(err, &bre) {
		t.Fatalf("CommitAndPush error = %v, want *BaseRebaseConflictError via errors.As", err)
	}
	if len(bre.ConflictPaths) != 1 || bre.ConflictPaths[0] != "shared.txt" {
		t.Errorf("ConflictPaths = %v, want [shared.txt] (from the already-read unmerged listing)", bre.ConflictPaths)
	}
	if bre.ConflictHunks != "" {
		t.Errorf("ConflictHunks must degrade to empty on a failed capture, got: %q", bre.ConflictHunks)
	}
	if bre.StashPatch != "" {
		t.Errorf("StashPatch must degrade to empty on a failed capture, got: %q", bre.StashPatch)
	}

	// Clean-abort invariants hold unchanged.
	if list := mustGitOut(t, repo, "stash", "list"); strings.TrimSpace(list) == "" {
		t.Error("stash list is empty; the conflicted pop must leave the stash entry recoverable")
	}
	if unmerged := mustGitOut(t, repo, "ls-files", "--unmerged"); strings.TrimSpace(unmerged) != "" {
		t.Errorf("ls-files --unmerged not empty after abort: %q", unmerged)
	}
	out, lsErr := exec.Command("git", "--git-dir="+bare, "ls-remote", bare, branch).Output()
	if lsErr != nil {
		t.Fatalf("ls-remote: %v", lsErr)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("run branch %q was pushed to the bare remote on a conflicted pop: %q", branch, string(out))
	}
}

// TestCommitAndPush_FreshFetchBase_LsFilesProbeFailure drives the #893 double-
// failure branch of popStash: `git stash pop` fails AND the subsequent
// `git ls-files --unmerged` conflict-detection probe also fails. The pop is made
// to conflict by the same divergent-edit setup as the conflict test, but a
// Pusher.Cmd override rewrites ONLY the `ls-files --unmerged` invocation to an
// unknown flag so it exits non-zero (lsErr != nil) while every other git call
// runs normally against the real repo. It asserts: (a) the error is non-nil and
// surfaces both the pop failure and the ls-files detection failure, but is NOT
// ErrBaseRebaseConflict (the conflict was never confirmed); (b) the best-effort
// reset --hard ran — no unmerged entries and no UU markers remain; (c) nothing
// was pushed to the bare remote.
func TestCommitAndPush_FreshFetchBase_LsFilesProbeFailure(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	const branch = "fishhawk/run-893/stage-x"

	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	bare := filepath.Join(dir, "origin.git")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "--initial-branch=main")
	mustGit(t, repo, "config", "user.name", "init")
	mustGit(t, repo, "config", "user.email", "init@example.com")
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("base line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "initial")

	mustGit(t, repo, "init", "--bare", bare)
	mustGit(t, repo, "remote", "add", "origin", bare)
	mustGit(t, repo, "push", "origin", "main")

	// origin/main advances the shared line; the local checkout is reset back so
	// the advance only exists on the remote tip (the fresh fetch pulls it in).
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("remote-advanced line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "remote advances shared line")
	mustGit(t, repo, "push", "origin", "main")
	mustGit(t, repo, "reset", "--hard", "HEAD~1")

	// The agent's uncommitted divergent edit to the SAME line, so reapplying the
	// stash onto the fetched tip conflicts.
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("agent-edited line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Cmd override: delegate to real git for everything EXCEPT the
	// `ls-files --unmerged` conflict-detection probe, which is rewritten to an
	// unknown flag so it exits non-zero (lsErr != nil) — forcing the
	// double-failure branch while reset --hard still runs against the real repo.
	p := &Pusher{
		Cmd: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			if len(args) >= 2 && args[0] == "ls-files" && args[1] == "--unmerged" {
				return exec.CommandContext(ctx, name, "ls-files", "--fishhawk-no-such-flag")
			}
			return exec.CommandContext(ctx, name, args...)
		},
	}
	_, err := p.CommitAndPush(context.Background(), CommitAndPushArgs{
		RepoDir:        repo,
		Branch:         branch,
		CommitMessage:  "agent stage commit",
		RemoteURL:      bare,
		FreshFetchBase: "main",
	})

	// (a) Non-nil error that surfaces the pop failure and the ls-files detection
	// failure, but NOT ErrBaseRebaseConflict (conflict never confirmed).
	if err == nil {
		t.Fatal("CommitAndPush error = nil, want non-nil double-failure error")
	}
	if errors.Is(err, ErrBaseRebaseConflict) {
		t.Errorf("CommitAndPush error = %v, must NOT be ErrBaseRebaseConflict (conflict unconfirmed)", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "ls-files") || !strings.Contains(msg, "stash pop") {
		t.Errorf("error %q must surface both the stash pop failure and the ls-files detection failure", msg)
	}

	// (b) The best-effort reset --hard ran — the tree is clean.
	if unmerged := mustGitOut(t, repo, "ls-files", "--unmerged"); strings.TrimSpace(unmerged) != "" {
		t.Errorf("ls-files --unmerged not empty after reset: %q", unmerged)
	}
	if status := mustGitOut(t, repo, "status", "--porcelain"); strings.Contains(status, "UU") {
		t.Errorf("working tree has conflict markers (UU) after reset: %q", status)
	}

	// (c) Nothing was pushed — the run branch does not exist on the bare remote.
	out, lsErr := exec.Command("git", "--git-dir="+bare, "ls-remote", bare, branch).Output()
	if lsErr != nil {
		t.Fatalf("ls-remote: %v", lsErr)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("run branch %q was pushed to the bare remote on a double-failure pop: %q", branch, string(out))
	}
}

// TestCommitAndPush_RebaseFromRemote_StashPopConflict pins the #866 hardened
// pop-conflict path for the decomposed-child shared-branch route, which routes
// through the SAME popStash helper as FreshFetchBase. The shared branch advances
// on the remote (another writer changes a line) while the agent's uncommitted
// edit changes the same line divergently, so reapplying the stash onto the
// freshly-fetched branch tip conflicts. It asserts the same contract: (a) the
// error is ErrBaseRebaseConflict; (b) the stash entry survives; (c) the working
// tree is clean with no unmerged paths after the reset --hard abort; (d) the
// remote branch was NOT advanced by a child-B push (no push on a conflicted pop).
func TestCommitAndPush_RebaseFromRemote_StashPopConflict(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	const branch = "fishhawk/run-shared866"

	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	bare := filepath.Join(dir, "origin.git")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "--initial-branch=main")
	mustGit(t, repo, "config", "user.name", "init")
	mustGit(t, repo, "config", "user.email", "init@example.com")
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("base line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "initial")
	mustGit(t, repo, "init", "--bare", bare)
	mustGit(t, repo, "remote", "add", "origin", bare)

	p := &Pusher{}

	// Child A establishes the shared branch on the bare remote with shared.txt
	// still at "base line" (the dirty change is an unrelated file).
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CommitAndPush(context.Background(), CommitAndPushArgs{
		RepoDir:           repo,
		Branch:            branch,
		CommitMessage:     "child A",
		RemoteURL:         bare,
		UpdateTrackingRef: true,
	}); err != nil {
		t.Fatalf("child A CommitAndPush: %v", err)
	}

	// Another writer advances the shared branch on the remote, changing the
	// shared line. The local checkout is reset back so the advance only exists
	// on the remote tip (the RebaseFromRemote fetch pulls it in).
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("remote-advanced line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "remote advances shared line")
	mustGit(t, repo, "push", "origin", branch)
	remoteTip := mustGitOut(t, repo, "rev-parse", "HEAD")
	mustGit(t, repo, "reset", "--hard", "HEAD~1")

	// The agent's uncommitted divergent edit to the SAME line.
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("agent-edited line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := p.CommitAndPush(context.Background(), CommitAndPushArgs{
		RepoDir:          repo,
		Branch:           branch,
		CommitMessage:    "child B",
		RemoteURL:        bare,
		RebaseFromRemote: true,
	})

	// (a) The error is the dedicated rebase-conflict sentinel.
	if !errors.Is(err, ErrBaseRebaseConflict) {
		t.Fatalf("CommitAndPush error = %v, want ErrBaseRebaseConflict", err)
	}

	// (b) The stash entry survives the conflicted pop — recoverable.
	if list := mustGitOut(t, repo, "stash", "list"); strings.TrimSpace(list) == "" {
		t.Error("stash list is empty; the conflicted pop must leave the stash entry recoverable")
	}

	// (c) The working tree is clean with no unmerged paths after reset --hard.
	if unmerged := mustGitOut(t, repo, "ls-files", "--unmerged"); strings.TrimSpace(unmerged) != "" {
		t.Errorf("ls-files --unmerged not empty after abort: %q", unmerged)
	}
	if status := mustGitOut(t, repo, "status", "--porcelain"); strings.Contains(status, "UU") {
		t.Errorf("working tree has conflict markers (UU) after abort: %q", status)
	}

	// (d) The remote shared branch was NOT advanced by a child-B push — its tip
	// is unchanged from the other writer's commit.
	if got := mustGitOut(t, repo, "--git-dir="+bare, "rev-parse", branch); got != remoteTip {
		t.Errorf("remote branch tip = %q, want unchanged %q (no push on conflicted pop)", got, remoteTip)
	}
}

// TestCaptureHead_AttachedReturnsBranchName asserts that on a normal attached
// HEAD, CaptureHead returns the short branch name and detached=false (#911) —
// the restore target the runner force-checks-out after the run-branch switch.
func TestCaptureHead_AttachedReturnsBranchName(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)

	ref, detached, err := CaptureHead(context.Background(), repo)
	if err != nil {
		t.Fatalf("CaptureHead: %v", err)
	}
	if detached {
		t.Errorf("detached = true, want false on an attached HEAD")
	}
	if ref != "main" {
		t.Errorf("ref = %q, want %q", ref, "main")
	}
}

// TestCaptureHead_DetachedReturnsSHA asserts the hosted actions/checkout shape:
// on a detached HEAD, `git symbolic-ref` exits non-zero and CaptureHead falls
// back to the commit SHA with detached=true (#911).
func TestCaptureHead_DetachedReturnsSHA(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	sha := mustGitOut(t, repo, "rev-parse", "HEAD")
	// Detach HEAD at the current commit (the actions/checkout shape).
	mustGit(t, repo, "checkout", "--detach", sha)

	ref, detached, err := CaptureHead(context.Background(), repo)
	if err != nil {
		t.Fatalf("CaptureHead: %v", err)
	}
	if !detached {
		t.Errorf("detached = false, want true on a detached HEAD")
	}
	if ref != sha {
		t.Errorf("ref = %q, want SHA %q", ref, sha)
	}
}

// TestRestoreHead_SwitchesOffDirtyRunBranch is the load-bearing #911 seam:
// from a run branch carrying staged-modified tracked files, RestoreHead
// force-switches back to the original branch, leaves the tree CLEAN, and
// leaves the run-branch commit reachable via its branch ref (no committed work
// lost — HEAD just moved off the branch).
func TestRestoreHead_SwitchesOffDirtyRunBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	const runBranch = "fishhawk/run-x/stage-y"

	// Simulate a run: switch to the run branch, commit an edit (the pushed
	// run-branch commit), then leave a staged-modified tracked file dirty (the
	// failed-pass tree shape).
	mustGit(t, repo, "checkout", "-b", runBranch)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# run-branch commit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "config", "user.name", "run")
	mustGit(t, repo, "config", "user.email", "run@example.com")
	mustGit(t, repo, "commit", "-am", "run-branch work")
	runCommit := mustGitOut(t, repo, "rev-parse", "HEAD")
	// Now dirty the tree with a staged-modified tracked file — checkout would
	// refuse this without --force.
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "README.md")

	if err := RestoreHead(context.Background(), repo, "main"); err != nil {
		t.Fatalf("RestoreHead: %v", err)
	}

	// Back on the original branch.
	if got := mustGitOut(t, repo, "symbolic-ref", "--short", "HEAD"); got != "main" {
		t.Errorf("HEAD branch = %q, want %q", got, "main")
	}
	// Tree is clean — the staged-modified tracked file was discarded by --force.
	if got := mustGitOut(t, repo, "status", "--porcelain"); got != "" {
		t.Errorf("status --porcelain = %q, want clean", got)
	}
	// The run-branch commit survives: its branch ref still points at it.
	if got := mustGitOut(t, repo, "rev-parse", runBranch); got != runCommit {
		t.Errorf("run branch tip = %q, want preserved %q", got, runCommit)
	}
}

// isAncestor reports whether maybeAncestor is an ancestor of ref via
// `git merge-base --is-ancestor` (exit 0 = ancestor, exit 1 = not).
func isAncestor(t *testing.T, dir, maybeAncestor, ref string) bool {
	t.Helper()
	cmd := exec.Command("git", "merge-base", "--is-ancestor", maybeAncestor, ref)
	cmd.Dir = dir
	return cmd.Run() == nil
}

// TestConfigureExtraheader_SetsCredentialForHTTPS covers the credential-
// configuration path the #772 fetch test cannot reach: that test passes a bare
// filesystem path as RemoteURL, so configureExtraheader no-ops on the
// not-HTTPS branch in both the rebase and push call sites. Here we exercise the
// helper directly against a real repo to assert (1) an HTTPS RemoteURL with a
// non-empty PushToken writes the host-scoped `http.<host>.extraheader` to the
// Basic auth header derived from the token, (2) an empty token is a no-op
// (ambient-auth path), and (3) a non-HTTPS RemoteURL is a no-op. This is the
// branch coverage the helper's straight extraction shares with the already-
// tested push-side block; pinning it here makes the credential path explicit.
func TestConfigureExtraheader_SetsCredentialForHTTPS(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	p := &Pusher{}

	const (
		httpsURL = "https://github.com/kuhlman-labs/fishhawk.git"
		token    = "ghs-test-token"
		key      = "http.https://github.com/.extraheader"
	)
	wantHeader := "AUTHORIZATION: basic " +
		base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))

	// HTTPS + token: the host-scoped extraheader is written with the token's
	// Basic auth header. The token lives in the config value, never on argv.
	if err := p.configureExtraheader(context.Background(), repo, httpsURL, token); err != nil {
		t.Fatalf("configureExtraheader (https+token): %v", err)
	}
	if got := mustGitOut(t, repo, "config", "--local", "--get", key); got != wantHeader {
		t.Errorf("extraheader = %q, want %q", got, wantHeader)
	}

	// Empty token is a no-op (ambient-auth path) — no second value appended,
	// and a fresh repo would have none at all.
	repoEmpty := initRepo(t)
	if err := p.configureExtraheader(context.Background(), repoEmpty, httpsURL, ""); err != nil {
		t.Fatalf("configureExtraheader (empty token): %v", err)
	}
	if gitConfigPresent(t, repoEmpty, key) {
		t.Error("empty token should not write an extraheader")
	}

	// Non-HTTPS RemoteURL (the bare-repo / SSH path) is a no-op even with a
	// token — the same branch the #772 fetch test hits.
	repoSSH := initRepo(t)
	if err := p.configureExtraheader(context.Background(), repoSSH, "/tmp/origin.git", token); err != nil {
		t.Fatalf("configureExtraheader (non-https): %v", err)
	}
	if gitConfigPresent(t, repoSSH, key) {
		t.Error("non-HTTPS RemoteURL should not write an extraheader")
	}
}

// gitConfigPresent reports whether a local git config key is set. `git config
// --get` exits non-zero (code 1) when the key is absent, which is the "no-op
// happened" signal we assert on rather than a test failure.
func gitConfigPresent(t *testing.T, dir, key string) bool {
	t.Helper()
	cmd := exec.Command("git", "config", "--local", "--get", key)
	cmd.Dir = dir
	return cmd.Run() == nil
}

// TestDirtyPaths_EnumeratesModifiedAndUntracked: DirtyPaths reports tracked
// modifications, tracked deletions, and untracked files — including files
// inside a brand-new directory listed individually (-uall, the #691
// rationale) — and an empty list on a clean tree.
func TestDirtyPaths_EnumeratesModifiedAndUntracked(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)

	clean, err := DirtyPaths(context.Background(), repo)
	if err != nil {
		t.Fatalf("DirtyPaths (clean): %v", err)
	}
	if len(clean) != 0 {
		t.Fatalf("DirtyPaths on a clean tree = %v, want empty", clean)
	}

	if err := os.WriteFile(filepath.Join(repo, "tracked-del.txt"), []byte("doomed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "add tracked-del")

	// Tracked modification, tracked deletion, untracked file, and an
	// untracked file inside a brand-new directory.
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# modified\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, "tracked-del.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "stray.txt"), []byte("stray\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "newdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "newdir", "inner.txt"), []byte("inner\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dirty, err := DirtyPaths(context.Background(), repo)
	if err != nil {
		t.Fatalf("DirtyPaths: %v", err)
	}
	got := make(map[string]bool, len(dirty))
	for _, p := range dirty {
		got[p] = true
	}
	for _, want := range []string{"README.md", "tracked-del.txt", "stray.txt", "newdir/inner.txt"} {
		if !got[want] {
			t.Errorf("DirtyPaths missing %q; got %v", want, dirty)
		}
	}
	if len(dirty) != 4 {
		t.Errorf("DirtyPaths = %v, want exactly 4 paths", dirty)
	}
}

// TestCleanDriftPaths_RevertsNamedLeavesUndeclared: one CleanDriftPaths call
// reverts a tracked modification, a tracked deletion, and an untracked file
// while an undeclared dirty path is untouched — the concrete test for the
// pathspec-scoped stash semantics the helper relies on (git-stash(1),
// pathspec after `--`, --include-untracked). The stash entry is dropped, so
// the stash stack ends empty.
func TestCleanDriftPaths_RevertsNamedLeavesUndeclared(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "tracked-del.txt"), []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "add tracked-del")

	// Drift: tracked modification, tracked deletion, untracked creation.
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# drift\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, "tracked-del.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "drift-new.txt"), []byte("net new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Undeclared dirty path that must survive untouched.
	if err := os.WriteFile(filepath.Join(repo, "operator.txt"), []byte("operator edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := CleanDriftPaths(context.Background(), repo,
		[]string{"README.md", "tracked-del.txt", "drift-new.txt"})
	if err != nil {
		t.Fatalf("CleanDriftPaths: %v", err)
	}

	if got, _ := os.ReadFile(filepath.Join(repo, "README.md")); string(got) != "# initial\n" {
		t.Errorf("README.md = %q, want reverted to HEAD content", got)
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "tracked-del.txt")); string(got) != "keep me\n" {
		t.Errorf("tracked-del.txt = %q, want the deletion reverted", got)
	}
	if _, err := os.Stat(filepath.Join(repo, "drift-new.txt")); !os.IsNotExist(err) {
		t.Errorf("drift-new.txt still exists; untracked drift must be removed")
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "operator.txt")); string(got) != "operator edit\n" {
		t.Errorf("operator.txt = %q, want the undeclared path left alone", got)
	}
	if list := mustGitOut(t, repo, "stash", "list"); list != "" {
		t.Errorf("stash list = %q, want empty (drift entry dropped)", list)
	}
}

// TestCleanDriftPaths_NoOpKeepsOperatorStash pins the entry-created guard: a
// CleanDriftPaths call whose named paths are already clean is a "No local
// changes" stash no-op (exit 0, no entry created), and the follow-up drop
// must NOT fire — a pre-existing operator stash entry survives. Without the
// refs/stash before/after probe, the blind drop would destroy it.
func TestCleanDriftPaths_NoOpKeepsOperatorStash(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)

	// The operator's own stash entry, created before the runner's cleanup.
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# operator stash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "stash", "push", "-m", "operator wip")

	// Named paths are clean (README.md was just stashed) → no-op success.
	if err := CleanDriftPaths(context.Background(), repo, []string{"README.md"}); err != nil {
		t.Fatalf("CleanDriftPaths (clean paths): %v", err)
	}
	list := mustGitOut(t, repo, "stash", "list")
	if !strings.Contains(list, "operator wip") {
		t.Errorf("stash list = %q, want the operator's pre-existing entry preserved", list)
	}

	// Empty path list short-circuits without touching git at all.
	if err := CleanDriftPaths(context.Background(), repo, nil); err != nil {
		t.Fatalf("CleanDriftPaths (empty): %v", err)
	}
}

// TestRestoreHeadPreserving_RoundTripsOperatorEdit: an operator's uncommitted
// edit named in preserve survives the forced branch switch byte-identical —
// the carve-out RestoreHead's `checkout --force` would otherwise discard.
func TestRestoreHeadPreserving_RoundTripsOperatorEdit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	mustGit(t, repo, "checkout", "-b", "fishhawk/run-943/stage-x")

	// The operator's pre-existing edit, carried onto the run branch.
	const operatorEdit = "# operator pre-existing edit\n"
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte(operatorEdit), 0o644); err != nil {
		t.Fatal(err)
	}
	// Agent leftovers a plain RestoreHead would discard too — NOT preserved,
	// so the forced checkout must drop this one.
	if err := os.WriteFile(filepath.Join(repo, "agent-junk.txt"), []byte("junk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "agent-junk.txt")
	mustGit(t, repo, "commit", "-m", "track agent junk")
	if err := os.WriteFile(filepath.Join(repo, "agent-junk.txt"), []byte("dirty junk\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := RestoreHeadPreserving(context.Background(), repo, "main", []string{"README.md"})
	if err != nil {
		t.Fatalf("RestoreHeadPreserving: %v", err)
	}

	if branch := mustGitOut(t, repo, "branch", "--show-current"); branch != "main" {
		t.Errorf("branch = %q, want %q", branch, "main")
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "README.md")); string(got) != operatorEdit {
		t.Errorf("README.md = %q, want the operator edit byte-identical across the restore", got)
	}
	if list := mustGitOut(t, repo, "stash", "list"); list != "" {
		t.Errorf("stash list = %q, want empty after a clean pop", list)
	}
}

// TestRestoreHeadPreserving_EmptyPreserveDelegates: an empty preserve set is
// exactly RestoreHead — the forced checkout discards tracked modifications
// and moves HEAD, with no stash machinery involved.
func TestRestoreHeadPreserving_EmptyPreserveDelegates(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	mustGit(t, repo, "checkout", "-b", "fishhawk/run-943/stage-y")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# discarded\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := RestoreHeadPreserving(context.Background(), repo, "main", nil); err != nil {
		t.Fatalf("RestoreHeadPreserving (empty preserve): %v", err)
	}
	if branch := mustGitOut(t, repo, "branch", "--show-current"); branch != "main" {
		t.Errorf("branch = %q, want %q", branch, "main")
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "README.md")); string(got) != "# initial\n" {
		t.Errorf("README.md = %q, want tracked modification discarded (plain RestoreHead semantics)", got)
	}
	if list := mustGitOut(t, repo, "stash", "list"); list != "" {
		t.Errorf("stash list = %q, want empty (no stash on the delegate path)", list)
	}
}

// TestRestoreHeadPreserving_PopConflict_LeavesStashRecoverable: when
// reapplying the preserved edit onto the restored ref conflicts, the popStash
// (#989) machinery aborts cleanly — the checkout stands, the working tree is
// clean, and the operator's content stays recoverable via `git stash list`
// (git-stash(1): a conflicted pop does not drop the entry). Never silently
// destroyed.
func TestRestoreHeadPreserving_PopConflict_LeavesStashRecoverable(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)

	// A branch whose README diverges from main's, so a stash taken on it
	// cannot reapply onto main without a conflict.
	mustGit(t, repo, "checkout", "-b", "diverged")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# diverged committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "diverge README")
	// The operator's uncommitted edit on the diverged branch.
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# operator edit on diverged\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := RestoreHeadPreserving(context.Background(), repo, "main", []string{"README.md"})
	if err == nil {
		t.Fatal("RestoreHeadPreserving = nil, want a pop-conflict error")
	}
	if !errors.Is(err, ErrBaseRebaseConflict) {
		t.Errorf("err = %v, want errors.Is ErrBaseRebaseConflict (popStash's typed conflict)", err)
	}
	if branch := mustGitOut(t, repo, "branch", "--show-current"); branch != "main" {
		t.Errorf("branch = %q, want %q (the checkout stands; only the reapply failed)", branch, "main")
	}
	if status := mustGitOut(t, repo, "status", "--porcelain"); status != "" {
		t.Errorf("status = %q, want a clean tree after the conflict abort", status)
	}
	if list := mustGitOut(t, repo, "stash", "list"); list == "" {
		t.Error("stash list empty — the operator's preserved edit must stay recoverable after a pop conflict")
	}
}

// TestDriftCleanup_EndToEnd_PreservesOperatorEdit is the #943 cross-layer
// integration test (#618 rule: the change spans the gitops primitives and the
// runner's partition orchestration, so per-layer units are not sufficient).
// Against a real repo + bare remote: the operator has a pre-existing
// uncommitted edit, the agent then modifies an in-scope file, modifies an
// out-of-scope tracked file, and creates an out-of-scope untracked file.
// CommitAndPush (ScopeFiles + FreshFetchBase, the standalone implement shape)
// pushes the scoped commit and reports the other three paths as drift; the
// runner-side sequence — partition against the pre-agent DirtyPaths snapshot,
// CleanDriftPaths the agent-introduced subset, RestoreHeadPreserving the rest
// — must end with: scoped commit on the remote, agent drift (tracked and
// untracked) gone from the working tree, and the operator's pre-existing edit
// byte-identical on the restored original ref.
func TestDriftCleanup_EndToEnd_PreservesOperatorEdit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	bare := filepath.Join(dir, "origin.git")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "--initial-branch=main")
	mustGit(t, repo, "config", "user.name", "init")
	mustGit(t, repo, "config", "user.email", "init@example.com")
	for name, content := range map[string]string{
		"scoped.txt":        "scoped base\n",
		"operator.txt":      "operator base\n",
		"drift-tracked.txt": "drift base\n",
	} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "initial")
	mustGit(t, repo, "init", "--bare", bare)
	mustGit(t, repo, "remote", "add", "origin", bare)
	mustGit(t, repo, "push", "origin", "main")

	// The operator's pre-existing uncommitted edit, present BEFORE the agent.
	const operatorEdit = "operator pre-existing edit\n"
	if err := os.WriteFile(filepath.Join(repo, "operator.txt"), []byte(operatorEdit), 0o644); err != nil {
		t.Fatal(err)
	}

	// The pre-agent snapshot run() captures (#943).
	preDirty, err := DirtyPaths(context.Background(), repo)
	if err != nil {
		t.Fatalf("DirtyPaths (pre-agent): %v", err)
	}
	if len(preDirty) != 1 || preDirty[0] != "operator.txt" {
		t.Fatalf("pre-agent dirty = %v, want exactly [operator.txt]", preDirty)
	}

	// The agent's edits: in-scope, out-of-scope tracked, out-of-scope untracked.
	if err := os.WriteFile(filepath.Join(repo, "scoped.txt"), []byte("agent scoped edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "drift-tracked.txt"), []byte("agent drift edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "drift-new.txt"), []byte("agent net-new drift\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	const branch = "fishhawk/run-943/stage-e2e"
	res, err := (&Pusher{}).CommitAndPush(context.Background(), CommitAndPushArgs{
		RepoDir:        repo,
		Branch:         branch,
		CommitMessage:  "scoped implement commit",
		RemoteURL:      bare,
		FreshFetchBase: "main",
		ScopeFiles:     []string{"scoped.txt"},
	})
	if err != nil {
		t.Fatalf("CommitAndPush: %v", err)
	}
	driftSet := make(map[string]bool, len(res.ScopeDrift))
	for _, p := range res.ScopeDrift {
		driftSet[p] = true
	}
	for _, want := range []string{"operator.txt", "drift-tracked.txt", "drift-new.txt"} {
		if !driftSet[want] {
			t.Fatalf("ScopeDrift = %v, want it to contain %q (path identity stable across the #866 stash cycle)", res.ScopeDrift, want)
		}
	}

	// The runner's partition (openPRAndShipArtifact): pre-agent-dirty paths
	// are preserved, the rest of the drift is agent-introduced and cleaned.
	preSet := make(map[string]bool, len(preDirty))
	for _, p := range preDirty {
		preSet[p] = true
	}
	var agentDrift, preserved []string
	for _, p := range res.ScopeDrift {
		if preSet[p] {
			preserved = append(preserved, p)
		} else {
			agentDrift = append(agentDrift, p)
		}
	}
	if err := CleanDriftPaths(context.Background(), repo, agentDrift); err != nil {
		t.Fatalf("CleanDriftPaths: %v", err)
	}
	if err := RestoreHeadPreserving(context.Background(), repo, "main", preserved); err != nil {
		t.Fatalf("RestoreHeadPreserving: %v", err)
	}

	// Final state: the scoped commit is on the remote with ONLY scoped.txt.
	if got := mustGitOut(t, bare, "show", branch+":scoped.txt"); got != "agent scoped edit" {
		t.Errorf("remote scoped.txt = %q, want the agent's in-scope edit pushed", got)
	}
	committed := mustGitOut(t, bare, "diff-tree", "-r", "--no-commit-id", "--name-only", branch)
	if committed != "scoped.txt" {
		t.Errorf("committed paths = %q, want only scoped.txt", committed)
	}
	// The operator is back on main with the pre-existing edit byte-identical.
	if got := mustGitOut(t, repo, "branch", "--show-current"); got != "main" {
		t.Errorf("branch = %q, want main", got)
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "operator.txt")); string(got) != operatorEdit {
		t.Errorf("operator.txt = %q, want the pre-existing edit byte-identical", got)
	}
	// Agent drift — tracked and untracked — is gone from the working tree.
	if got, _ := os.ReadFile(filepath.Join(repo, "drift-tracked.txt")); string(got) != "drift base\n" {
		t.Errorf("drift-tracked.txt = %q, want the agent's tracked drift reverted", got)
	}
	if _, err := os.Stat(filepath.Join(repo, "drift-new.txt")); !os.IsNotExist(err) {
		t.Error("drift-new.txt still exists; agent untracked drift must not accumulate")
	}
	// Nothing left dangling on the stash stack.
	if list := mustGitOut(t, repo, "stash", "list"); list != "" {
		t.Errorf("stash list = %q, want empty", list)
	}
	// The only dirt left is the operator's own edit.
	status := strings.TrimSpace(mustGitOut(t, repo, "status", "--porcelain", "-uall"))
	if !strings.Contains(status, "operator.txt") || strings.Contains(status, "\n") {
		t.Errorf("status = %q, want exactly one dirty path: the operator's edit", status)
	}
}

// Make sure `errors` is used so a refactor that drops the import
// stays caught by go vet/imports tooling.
var _ = errors.New
