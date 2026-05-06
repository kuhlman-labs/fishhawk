package gitops

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runReal initializes a git repo on disk and exercises CommitAndPush
// end-to-end, with origin pointed at a bare local repo so push
// actually completes. Production code targets HTTPS origin URLs;
// the pushExtraHeader branch that handles auth for HTTPS remotes is
// unit-tested separately below.
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
		Token:         "x",
		// Direct file-path remote: tokenization is a no-op (not
		// HTTPS), so the push targets the bare repo directly.
		RemoteURL: bare,
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
		Token:         "x",
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
		"missing repo dir":   {Branch: "b", Token: "t", RemoteURL: "https://x/y/z"},
		"missing branch":     {RepoDir: ".", Token: "t", RemoteURL: "https://x/y/z"},
		"missing token":      {RepoDir: ".", Branch: "b", RemoteURL: "https://x/y/z"},
		"missing remote URL": {RepoDir: ".", Branch: "b", Token: "t"},
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

func TestPushExtraHeader(t *testing.T) {
	// "AUTHORIZATION: basic " + base64("x-access-token:secret")
	const wantAuth = "AUTHORIZATION: basic eC1hY2Nlc3MtdG9rZW46c2VjcmV0"

	cases := []struct {
		name       string
		in         string
		wantHeader string
		wantHost   string
	}{
		{
			name:       "github https",
			in:         "https://github.com/owner/repo",
			wantHeader: wantAuth,
			wantHost:   "https://github.com/",
		},
		{
			name:       "trailing .git preserved",
			in:         "https://github.com/owner/repo.git",
			wantHeader: wantAuth,
			wantHost:   "https://github.com/",
		},
		{
			name:       "GHES host",
			in:         "https://ghe.example.com/owner/repo",
			wantHeader: wantAuth,
			wantHost:   "https://ghe.example.com/",
		},
		{
			name:       "ssh remote skips auth",
			in:         "git@github.com:owner/repo.git",
			wantHeader: "",
			wantHost:   "",
		},
		{
			name:       "local path skips auth",
			in:         "/tmp/origin.git",
			wantHeader: "",
			wantHost:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotHeader, gotHost, err := pushExtraHeader(tc.in, "secret")
			if err != nil {
				t.Fatal(err)
			}
			if gotHeader != tc.wantHeader {
				t.Errorf("header = %q, want %q", gotHeader, tc.wantHeader)
			}
			if gotHost != tc.wantHost {
				t.Errorf("host = %q, want %q", gotHost, tc.wantHost)
			}
		})
	}
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

// Make sure `errors` is used so a refactor that drops the import
// stays caught by go vet/imports tooling.
var _ = errors.New

// TestCommitAndPush_PushPassesExtraHeader replays the production
// command path through a fake exec so we can assert the push goes
// out with `-c http.<host>.extraheader=…`. This is the regression
// guard for the issue where actions/checkout's extraheader was
// winning over our URL-embedded credential and the runner was
// pushing as github-actions[bot] instead of the App.
func TestCommitAndPush_PushPassesExtraHeader(t *testing.T) {
	// Use a fake exec hook so we capture every `git ...` invocation
	// and can synthesize a succeeding HEAD-rev / status / etc.
	// without touching disk.
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
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "init")
	mustGit(t, repo, "init", "--bare", bare)

	// Capture every command's args without changing behavior.
	var captured [][]string
	p := &Pusher{
		Cmd: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			capturedArgs := append([]string{name}, args...)
			captured = append(captured, capturedArgs)
			return exec.CommandContext(ctx, name, args...)
		},
	}

	// Modify the working tree so there's something to commit + push.
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	httpsRemote := "https://github.com/owner/repo"
	_, err := p.CommitAndPush(context.Background(), CommitAndPushArgs{
		RepoDir:       repo,
		Branch:        "fishhawk/test",
		CommitMessage: "test",
		Token:         "ghs_xyz",
		RemoteURL:     httpsRemote, // not actually pushed; bare-repo test above covers real push
	})
	// We expect an error from the push (network call to github.com
	// from a unit test would actually try to authenticate), but the
	// args MUST have included our extraheader.
	if err == nil {
		t.Logf("CommitAndPush succeeded unexpectedly (no network?): captured=%v", captured)
	}

	// Find the push invocation.
	var pushCmd []string
	for _, c := range captured {
		// `git -c ... push ...` — find the one with "push" in it.
		for _, a := range c {
			if a == "push" {
				pushCmd = c
				break
			}
		}
	}
	if pushCmd == nil {
		t.Fatalf("push command not captured among %d invocations", len(captured))
	}

	// Expect: git -c http.https://github.com/.extraheader=AUTHORIZATION: basic <b64> push https://github.com/owner/repo HEAD:fishhawk/test
	wantHeaderPrefix := "http.https://github.com/.extraheader=AUTHORIZATION: basic "
	gotHeaderArg := false
	for i, a := range pushCmd {
		if a == "-c" && i+1 < len(pushCmd) && strings.HasPrefix(pushCmd[i+1], wantHeaderPrefix) {
			gotHeaderArg = true
			break
		}
	}
	if !gotHeaderArg {
		t.Errorf("push args did not include `-c %s…`: %v", wantHeaderPrefix, pushCmd)
	}
	// Remote URL must NOT have credentials embedded in it (we moved
	// off the x-access-token-in-URL approach).
	for _, a := range pushCmd {
		if strings.Contains(a, "x-access-token:") {
			t.Errorf("push command leaked credentials in URL: %q", a)
		}
	}
}

// TestCommitAndPush_UnsetsStaleExtraHeaderBeforePush replays the
// failure mode from production: actions/checkout sets a --local
// http.<host>.extraheader pointing at the workflow's GITHUB_TOKEN,
// our `-c` override stacks rather than replaces, and GitHub
// rejects with "Duplicate header: Authorization". The fix unsets
// the existing local entry first; this test asserts the unset
// call happens BEFORE the push.
func TestCommitAndPush_UnsetsStaleExtraHeaderBeforePush(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "init")

	// Simulate actions/checkout setting the local extraheader.
	mustGit(t, repo, "config", "--local",
		"http.https://github.com/.extraheader",
		"AUTHORIZATION: basic stale-token-value")

	// Capture every command's args without changing behavior.
	var captured [][]string
	p := &Pusher{
		Cmd: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			capturedArgs := append([]string{name}, args...)
			captured = append(captured, capturedArgs)
			return exec.CommandContext(ctx, name, args...)
		},
	}

	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Push will fail (no real remote); we only care about the
	// invocation order before the push.
	_, _ = p.CommitAndPush(context.Background(), CommitAndPushArgs{
		RepoDir:       repo,
		Branch:        "fishhawk/test",
		CommitMessage: "test",
		Token:         "ghs_xyz",
		RemoteURL:     "https://github.com/owner/repo",
	})

	// Find the first push invocation and the unset invocation.
	pushIdx, unsetIdx := -1, -1
	for i, c := range captured {
		// Skip past `git`, look at the rest.
		args := c[1:]
		// Push has "push" as a top-level (post `-c` flags) arg.
		for j, a := range args {
			if a == "push" {
				pushIdx = i
				_ = j
				break
			}
		}
		// Unset matches `config --local --unset-all
		// http.<host>.extraheader`.
		if len(args) >= 4 && args[0] == "config" && args[1] == "--local" &&
			args[2] == "--unset-all" && strings.HasPrefix(args[3], "http.") &&
			strings.HasSuffix(args[3], ".extraheader") {
			unsetIdx = i
		}
	}

	if unsetIdx < 0 {
		t.Errorf("expected an `unset-all extraheader` call before push, none found:\n%v", captured)
	}
	if pushIdx < 0 {
		t.Fatalf("push command not captured:\n%v", captured)
	}
	if unsetIdx >= pushIdx {
		t.Errorf("unset must precede push: unsetIdx=%d, pushIdx=%d", unsetIdx, pushIdx)
	}

	// Final repo state: actions/checkout's stale extraheader should
	// be gone (the unset took effect on disk).
	out, _ := exec.Command("git", "-C", repo, "config", "--local", "--get-all",
		"http.https://github.com/.extraheader").Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("stale extraheader still present after CommitAndPush: %q", out)
	}
}
