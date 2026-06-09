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

// Make sure `errors` is used so a refactor that drops the import
// stays caught by go vet/imports tooling.
var _ = errors.New
