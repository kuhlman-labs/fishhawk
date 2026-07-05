package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

func TestParsePRDescriptionFile_AgentAuthored(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "pr-*.md")
	if err != nil {
		t.Fatal(err)
	}
	content := "Add widget support\n\n## Summary\n- adds widgets\n\n## Test plan\n- [ ] run tests\n"
	if _, werr := f.WriteString(content); werr != nil {
		t.Fatal(werr)
	}
	f.Close()

	runID := "11111111-2222-3333-4444-555555555555"
	title, body, err := parsePRDescriptionFile(f.Name(), runID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if title != "Add widget support" {
		t.Errorf("title = %q, want %q", title, "Add widget support")
	}
	if !strings.Contains(body, "## Summary") {
		t.Errorf("body should contain ## Summary:\n%s", body)
	}
	if !strings.Contains(body, autoPRFooter) {
		t.Errorf("body should contain attribution footer:\n%s", body)
	}
}

func TestParsePRDescriptionFile_MissingFallback(t *testing.T) {
	path := t.TempDir() + "/no-such-file.md"
	runID := "aabbccdd-1122-3344-5566-778899aabbcc"

	title, body, err := parsePRDescriptionFile(path, runID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantTitle := "chore: fishhawk implement stage aabbccdd"
	if title != wantTitle {
		t.Errorf("title = %q, want %q", title, wantTitle)
	}
	if !strings.Contains(body, autoPRFooter) {
		t.Errorf("body should contain attribution footer:\n%s", body)
	}
}

func TestParsePRDescriptionFile_BlankFirstLineFallback(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "pr-*.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, werr := f.WriteString("\nsome content below"); werr != nil {
		t.Fatal(werr)
	}
	f.Close()

	runID := "11111111-2222-3333-4444-555555555555"
	title, _, err := parsePRDescriptionFile(f.Name(), runID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantTitle := "chore: fishhawk implement stage 11111111"
	if title != wantTitle {
		t.Errorf("title = %q, want %q", title, wantTitle)
	}
}

// captureStderr redirects os.Stderr to a pipe for the duration of fn and
// returns everything written to it.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()
	fn()
	if cerr := w.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	out, rerr := io.ReadAll(r)
	if rerr != nil {
		t.Fatal(rerr)
	}
	return string(out)
}

// TestParsePRDescriptionFile_NonConventionalTitleWarns (#1572): a non-conventional
// agent title emits pr_template_warning AND is used verbatim.
func TestParsePRDescriptionFile_NonConventionalTitleWarns(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "pr-*.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, werr := f.WriteString("Add widget support\n\n## Summary\n- adds widgets\n"); werr != nil {
		t.Fatal(werr)
	}
	f.Close()

	var title string
	stderr := captureStderr(t, func() {
		var perr error
		title, _, perr = parsePRDescriptionFile(f.Name(), "11111111-2222-3333-4444-555555555555")
		if perr != nil {
			t.Fatalf("unexpected error: %v", perr)
		}
	})
	if title != "Add widget support" {
		t.Errorf("non-conventional title must be used VERBATIM, got %q", title)
	}
	if !strings.Contains(stderr, `"event":"pr_template_warning"`) ||
		!strings.Contains(stderr, "title is not a conventional-commit header") {
		t.Errorf("expected conventional-header pr_template_warning, got %q", stderr)
	}
}

// TestParsePRDescriptionFile_ConventionalTitleNoWarn (#1572): a conventional
// agent title emits NO pr_template_warning.
func TestParsePRDescriptionFile_ConventionalTitleNoWarn(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "pr-*.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, werr := f.WriteString("feat(widget): add widget support\n\n## Summary\n- adds widgets\n"); werr != nil {
		t.Fatal(werr)
	}
	f.Close()

	var title string
	stderr := captureStderr(t, func() {
		var perr error
		title, _, perr = parsePRDescriptionFile(f.Name(), "11111111-2222-3333-4444-555555555555")
		if perr != nil {
			t.Fatalf("unexpected error: %v", perr)
		}
	})
	if title != "feat(widget): add widget support" {
		t.Errorf("conventional title = %q", title)
	}
	if strings.Contains(stderr, "pr_template_warning") {
		t.Errorf("conventional title must not warn, got %q", stderr)
	}
}

// --- #1686 initial-implement commit message ------------------------------

// writeImplementCommitMsgSidecar redirects implementCommitMessageDir to a temp
// dir and writes raw text to the keyed initial-implement commit-message path.
func writeImplementCommitMsgSidecar(t *testing.T, runID, stageID, raw string) string {
	t.Helper()
	dir := t.TempDir()
	orig := implementCommitMessageDir
	implementCommitMessageDir = dir
	t.Cleanup(func() { implementCommitMessageDir = orig })
	path := implementCommitMessagePath(runID, stageID)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestImplementCommitMessagePath_Format (#1686, binding condition 1) asserts the
// LITERAL CLI-side path for KNOWN ids, byte-identical to the backend prompt-render
// test and the runner load test, so a one-sided edit to any of the three hardcoded
// format strings fails a test (the pragmatic cross-module lock).
func TestImplementCommitMessagePath_Format(t *testing.T) {
	orig := implementCommitMessageDir
	implementCommitMessageDir = "/tmp"
	t.Cleanup(func() { implementCommitMessageDir = orig })
	const runID = "11112222333344445555666677778888"
	const stageID = "99990000aaaabbbbccccddddeeeeffff"
	got := implementCommitMessagePath(runID, stageID)
	want := "/tmp/fishhawk-implement-commitmsg-" + runID + "-" + stageID + ".txt"
	if got != want {
		t.Errorf("implementCommitMessagePath = %q, want %q", got, want)
	}
}

// TestLoadImplementCommitMessage_Present (#1686, mode 1): a present sidecar yields
// (subject, body) split on the first newline AND is deleted after read.
func TestLoadImplementCommitMessage_Present(t *testing.T) {
	const runID, stageID = "run-cccc", "stage-dddd"
	path := writeImplementCommitMsgSidecar(t, runID, stageID,
		"feat(cli): add a flag\n\nAdds a --thing flag to autopr.\n")
	subject, body, ok := loadImplementCommitMessage(runID, stageID)
	if !ok {
		t.Fatalf("present sidecar must yield ok=true")
	}
	if subject != "feat(cli): add a flag" {
		t.Errorf("subject = %q", subject)
	}
	if body != "Adds a --thing flag to autopr." {
		t.Errorf("body = %q", body)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("consumed sidecar must be removed, stat err = %v", err)
	}
}

// TestLoadImplementCommitMessage_Absent (#1686): no sidecar → ok=false.
func TestLoadImplementCommitMessage_Absent(t *testing.T) {
	dir := t.TempDir()
	orig := implementCommitMessageDir
	implementCommitMessageDir = dir
	t.Cleanup(func() { implementCommitMessageDir = orig })
	if _, _, ok := loadImplementCommitMessage("run-cccc", "stage-dddd"); ok {
		t.Errorf("absent sidecar must yield ok=false")
	}
}

// TestLoadImplementCommitMessage_EmptyWhitespace (#1686, mode 4): an empty or
// whitespace-only sidecar is treated as missing (ok=false) and is removed.
func TestLoadImplementCommitMessage_EmptyWhitespace(t *testing.T) {
	for _, raw := range []string{"", "   \n\t\n"} {
		path := writeImplementCommitMsgSidecar(t, "run-cccc", "stage-dddd", raw)
		subject, body, ok := loadImplementCommitMessage("run-cccc", "stage-dddd")
		if ok || subject != "" || body != "" {
			t.Errorf("empty sidecar %q must yield ok=false, got (%q,%q,%v)", raw, subject, body, ok)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("empty sidecar must be removed for %q, stat err = %v", raw, err)
		}
	}
}

// TestImplementCommitMessage_FallbackMissing (#1686, mode 3, binding condition 2):
// with no sidecar the message is EXACTLY today's title + "\n\n" + body — no
// synthetic subject, no behavior change for an older agent.
func TestImplementCommitMessage_FallbackMissing(t *testing.T) {
	dir := t.TempDir()
	orig := implementCommitMessageDir
	implementCommitMessageDir = dir
	t.Cleanup(func() { implementCommitMessageDir = orig })
	got := implementCommitMessage("run-cccc", "stage-dddd", "feat(x): do a thing", "## Summary\n\nbody\n")
	want := "feat(x): do a thing" + "\n\n" + "## Summary\n\nbody\n"
	if got != want {
		t.Errorf("fallback = %q, want exactly title + \\n\\n + body %q", got, want)
	}
}

// TestImplementCommitMessage_SidecarPresent (#1686, mode 1): the resolver returns
// the sidecar content (subject + blank line + body), NOT the PR title/body.
func TestImplementCommitMessage_SidecarPresent(t *testing.T) {
	writeImplementCommitMsgSidecar(t, "run-cccc", "stage-dddd", "feat(cli): add a flag\n\nDetail.\n")
	got := implementCommitMessage("run-cccc", "stage-dddd", "feat(cli): PR TITLE", "PR BODY\n\n## Summary\n\nx")
	if got != "feat(cli): add a flag\n\nDetail." {
		t.Errorf("implementCommitMessage = %q, want sidecar content", got)
	}
}

// TestAutoOpenPR_ImplementCommitMessageSidecar (#1686) is the end-to-end mirror:
// with a sidecar present the pushed commit's message is the SIDECAR content (not
// the PR title/body), and the sidecar file is deleted after read.
func TestAutoOpenPR_ImplementCommitMessageSidecar(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, _ := autoOpenPRTestRepo(t)

	prFile := filepath.Join(t.TempDir(), "pr.md")
	if err := os.WriteFile(prFile, []byte("feat(cli): PR TITLE\n\n## Summary\n\nRich PR body.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	origPath := prDescriptionPath
	prDescriptionPath = prFile
	t.Cleanup(func() { prDescriptionPath = origPath })

	runID := uuid.New()
	stageID := uuid.New()
	sidecarPath := writeImplementCommitMsgSidecar(t, runID.String(), stageID.String(),
		"feat(cli): add minio-init target\n\nConcise commit body.\n")

	origGh := autoGhCommand
	autoGhCommand = func(_ string, _ ...string) *exec.Cmd {
		return exec.Command("sh", "-c", "echo https://github.com/owner/repo/pull/7")
	}
	t.Cleanup(func() { autoGhCommand = origGh })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(httpclient.ShipLocalPullRequestResult{
			PRNumber: 7, PRURL: "https://github.com/owner/repo/pull/7",
		})
	}))
	defer srv.Close()

	if _, err := autoOpenPR(context.Background(),
		httpclient.New(srv.URL, "test-token"),
		autoOpenPRArgs{
			WorkingDir: repo,
			RunID:      runID,
			StageID:    stageID,
			GitHubRepo: "owner/repo",
			BaseBranch: "main",
		}); err != nil {
		t.Fatalf("autoOpenPR: %v", err)
	}

	msg := mustGitOutCLI(t, repo, "show", "--format=%B", "--no-patch", "HEAD")
	if !strings.Contains(msg, "add minio-init target") || !strings.Contains(msg, "Concise commit body.") {
		t.Errorf("commit message must be the sidecar content, got %q", msg)
	}
	if strings.Contains(msg, "Rich PR body.") {
		t.Errorf("commit message must NOT contain the PR body, got %q", msg)
	}
	if _, err := os.Stat(sidecarPath); !os.IsNotExist(err) {
		t.Errorf("sidecar must be deleted after read, stat err = %v", err)
	}
}

// TestAutoOpenPR_ImplementCommitMessageFallback (#1686, binding condition 2) is
// the absent-sidecar end-to-end mirror: with no sidecar the pushed commit's
// message is EXACTLY the PR title + "\n\n" + body (older-agent no-behavior-change).
func TestAutoOpenPR_ImplementCommitMessageFallback(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, _ := autoOpenPRTestRepo(t)

	// Point implementCommitMessageDir at an empty temp dir so no sidecar exists.
	origDir := implementCommitMessageDir
	implementCommitMessageDir = t.TempDir()
	t.Cleanup(func() { implementCommitMessageDir = origDir })

	prFile := filepath.Join(t.TempDir(), "pr.md")
	if err := os.WriteFile(prFile, []byte("feat(cli): PR TITLE\n\n## Summary\n\nRich PR body.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	origPath := prDescriptionPath
	prDescriptionPath = prFile
	t.Cleanup(func() { prDescriptionPath = origPath })

	origGh := autoGhCommand
	autoGhCommand = func(_ string, _ ...string) *exec.Cmd {
		return exec.Command("sh", "-c", "echo https://github.com/owner/repo/pull/7")
	}
	t.Cleanup(func() { autoGhCommand = origGh })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(httpclient.ShipLocalPullRequestResult{
			PRNumber: 7, PRURL: "https://github.com/owner/repo/pull/7",
		})
	}))
	defer srv.Close()

	if _, err := autoOpenPR(context.Background(),
		httpclient.New(srv.URL, "test-token"),
		autoOpenPRArgs{
			WorkingDir: repo,
			RunID:      uuid.New(),
			StageID:    uuid.New(),
			GitHubRepo: "owner/repo",
			BaseBranch: "main",
		}); err != nil {
		t.Fatalf("autoOpenPR: %v", err)
	}

	// git --signoff appends a Signed-off-by trailer, so assert the message BEGINS
	// with exactly title + "\n\n" + body (the fallback), not equality.
	msg := mustGitOutCLI(t, repo, "show", "--format=%B", "--no-patch", "HEAD")
	want := "feat(cli): PR TITLE\n\n## Summary\n\nRich PR body." + "\n\n" + autoPRFooter
	if !strings.HasPrefix(msg, want) {
		t.Errorf("fallback commit message = %q, want prefix title + \\n\\n + body %q", msg, want)
	}
}

// autoOpenPRTestRepo builds a real git repo with a bare origin and one pending
// tracked change, returning the working dir and the bare origin path. Shared by
// the #1686 end-to-end commit-message tests.
func autoOpenPRTestRepo(t *testing.T) (repo, bare string) {
	t.Helper()
	dir := t.TempDir()
	repo = filepath.Join(dir, "src")
	bare = filepath.Join(dir, "origin.git")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGitCLI(t, repo, "init", "--initial-branch=main")
	mustGitCLI(t, repo, "config", "user.name", "init")
	mustGitCLI(t, repo, "config", "user.email", "init@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGitCLI(t, repo, "add", "-A")
	mustGitCLI(t, repo, "commit", "-m", "initial")
	mustGitCLI(t, dir, "init", "--bare", "origin.git")
	mustGitCLI(t, repo, "remote", "add", "origin", bare)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Point scopeFilePath at a nonexistent path so staging falls back to
	// `git add -A` (the default /tmp/fishhawk-scope.json may exist on the host).
	origScope := scopeFilePath
	scopeFilePath = filepath.Join(dir, "no-such-scope.json")
	t.Cleanup(func() { scopeFilePath = origScope })
	return repo, bare
}

func TestShortID_8HexChars(t *testing.T) {
	id := uuid.MustParse("abcdef12-3456-7890-abcd-ef1234567890")
	s := shortID(id)

	if len(s) != 8 {
		t.Errorf("shortID len = %d, want 8", len(s))
	}
	if strings.Contains(s, "-") {
		t.Errorf("shortID contains hyphen: %q", s)
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Errorf("shortID %q contains non-hex character %q", s, string(c))
		}
	}
	if s != "abcdef12" {
		t.Errorf("shortID = %q, want %q", s, "abcdef12")
	}
}

// stubGitCommand returns an *exec.Cmd that behaves differently based on
// the git subcommand (arg[2] when called as "git -C <dir> <subcommand> ...").
// rev-parse HEAD returns a fake HEAD SHA; rev-parse origin/... returns a
// fake base SHA; diff returns two filenames; everything else exits 0.
func stubGitCommand(name string, arg ...string) *exec.Cmd {
	if len(arg) >= 3 {
		switch arg[2] {
		case "rev-parse":
			if len(arg) > 3 && strings.HasPrefix(arg[3], "origin/") {
				return exec.Command("sh", "-c",
					"echo bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
			}
			return exec.Command("sh", "-c",
				"echo aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		case "diff":
			return exec.Command("sh", "-c", "echo foo.go; echo bar.go")
		}
	}
	return exec.Command("/usr/bin/true")
}

func TestAutoOpenPR_SuccessPath(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "pr-*.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, werr := f.WriteString("Add feature X\n\nThis adds feature X.\n"); werr != nil {
		t.Fatal(werr)
	}
	f.Close()

	origPath := prDescriptionPath
	prDescriptionPath = f.Name()
	t.Cleanup(func() { prDescriptionPath = origPath })

	origGit := autoGitCommand
	autoGitCommand = stubGitCommand
	t.Cleanup(func() { autoGitCommand = origGit })

	origGh := autoGhCommand
	autoGhCommand = func(_ string, _ ...string) *exec.Cmd {
		return exec.Command("sh", "-c",
			"echo https://github.com/owner/repo/pull/42")
	}
	t.Cleanup(func() { autoGhCommand = origGh })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(httpclient.ShipLocalPullRequestResult{
			PRNumber: 42,
			PRURL:    "https://github.com/owner/repo/pull/42",
			HeadSHA:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		})
	}))
	defer srv.Close()

	runID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	stageID := uuid.MustParse("22222222-3333-4444-5555-666666666666")

	result, err := autoOpenPR(context.Background(),
		httpclient.New(srv.URL, "test-token"),
		autoOpenPRArgs{
			WorkingDir: t.TempDir(),
			RunID:      runID,
			StageID:    stageID,
			GitHubRepo: "owner/repo",
			BaseBranch: "main",
		})
	if err != nil {
		t.Fatalf("autoOpenPR: %v", err)
	}
	if result.PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42", result.PRNumber)
	}
	if result.PRURL != "https://github.com/owner/repo/pull/42" {
		t.Errorf("PRURL = %q", result.PRURL)
	}
	if result.Branch != "fishhawk/run-11111111/stage-22222222" {
		t.Errorf("Branch = %q, want fishhawk/run-11111111/stage-22222222", result.Branch)
	}
	if result.HeadSHA != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("HeadSHA = %q", result.HeadSHA)
	}
}

func TestAutoOpenPR_GhMissing(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "pr-*.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, werr := f.WriteString("Add feature\n\nBody.\n"); werr != nil {
		t.Fatal(werr)
	}
	f.Close()

	origPath := prDescriptionPath
	prDescriptionPath = f.Name()
	t.Cleanup(func() { prDescriptionPath = origPath })

	origGit := autoGitCommand
	autoGitCommand = stubGitCommand
	t.Cleanup(func() { autoGitCommand = origGit })

	origGh := autoGhCommand
	autoGhCommand = func(_ string, _ ...string) *exec.Cmd {
		return exec.Command("sh", "-c", "exit 1")
	}
	t.Cleanup(func() { autoGhCommand = origGh })

	shipCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		shipCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, err = autoOpenPR(context.Background(),
		httpclient.New(srv.URL, ""),
		autoOpenPRArgs{
			WorkingDir: t.TempDir(),
			RunID:      uuid.New(),
			StageID:    uuid.New(),
			GitHubRepo: "owner/repo",
			BaseBranch: "main",
		})
	if err == nil {
		t.Fatal("expected error when gh fails, got nil")
	}
	if shipCalled {
		t.Error("ShipLocalPullRequest should not be called when gh pr create fails")
	}
}

func TestAutoOpenPR_PushFails(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "pr-*.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, werr := f.WriteString("Title\n\nBody.\n"); werr != nil {
		t.Fatal(werr)
	}
	f.Close()

	origPath := prDescriptionPath
	prDescriptionPath = f.Name()
	t.Cleanup(func() { prDescriptionPath = origPath })

	origGit := autoGitCommand
	autoGitCommand = func(name string, arg ...string) *exec.Cmd {
		if len(arg) >= 3 && arg[2] == "push" {
			return exec.Command("sh", "-c", "exit 1")
		}
		return stubGitCommand(name, arg...)
	}
	t.Cleanup(func() { autoGitCommand = origGit })

	origGh := autoGhCommand
	autoGhCommand = func(_ string, _ ...string) *exec.Cmd {
		return exec.Command("sh", "-c",
			"echo https://github.com/owner/repo/pull/99")
	}
	t.Cleanup(func() { autoGhCommand = origGh })

	shipCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		shipCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, err = autoOpenPR(context.Background(),
		httpclient.New(srv.URL, ""),
		autoOpenPRArgs{
			WorkingDir: t.TempDir(),
			RunID:      uuid.New(),
			StageID:    uuid.New(),
			GitHubRepo: "owner/repo",
			BaseBranch: "main",
		})
	if err == nil {
		t.Fatal("expected error when push fails, got nil")
	}
	if !strings.Contains(err.Error(), "git push") {
		t.Errorf("expected push error message, got: %v", err)
	}
	if shipCalled {
		t.Error("ShipLocalPullRequest should not be called when push fails")
	}
}

// mustGitCLI runs git in dir and fails the test on a non-zero exit.
func mustGitCLI(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// mustGitOutCLI runs git in dir, fails on error, and returns trimmed stdout.
func mustGitOutCLI(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	out, err := exec.Command("git", full...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func TestReadScopeFiles(t *testing.T) {
	dir := t.TempDir()

	// Missing file → nil (fallback to git add -A).
	if got := readScopeFiles(filepath.Join(dir, "absent.json")); got != nil {
		t.Errorf("readScopeFiles(missing) = %v, want nil", got)
	}

	// Empty files list → nil.
	emptyPath := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(emptyPath, []byte(`{"files":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readScopeFiles(emptyPath); got != nil {
		t.Errorf("readScopeFiles(empty) = %v, want nil", got)
	}

	// Valid handoff → declared paths, dropping blank-path entries.
	validPath := filepath.Join(dir, "scope.json")
	body := `{"files":[{"path":"cli/cmd/fishhawk/autopr.go","operation":"modify"},` +
		`{"path":"","operation":"modify"},{"path":".gitignore","operation":"modify"}]}`
	if err := os.WriteFile(validPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got := readScopeFiles(validPath)
	want := []string{"cli/cmd/fishhawk/autopr.go", ".gitignore"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("readScopeFiles(valid) = %v, want %v", got, want)
	}
}

// TestStageScopedAuto_StagesOnlyDeclared is the gating test: a working
// tree with one declared file and one undeclared stray file must stage
// exactly the declared path and report the stray as drift. Uses a real
// git repo (autoGitCommand defaults to exec.Command).
func TestStageScopedAuto_StagesOnlyDeclared(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	mustGitCLI(t, repo, "init", "--initial-branch=main")
	mustGitCLI(t, repo, "config", "user.name", "init")
	mustGitCLI(t, repo, "config", "user.email", "init@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGitCLI(t, repo, "add", "-A")
	mustGitCLI(t, repo, "commit", "-m", "initial")

	// One declared edit plus one undeclared stray file.
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "stray.pid"), []byte("12345\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	drift, err := stageScopedAuto(repo, []string{"README.md"})
	if err != nil {
		t.Fatalf("stageScopedAuto: %v", err)
	}
	if len(drift) != 1 || drift[0] != "stray.pid" {
		t.Errorf("drift = %v, want [stray.pid]", drift)
	}
	staged := mustGitOutCLI(t, repo, "diff", "--cached", "--name-only")
	if staged != "README.md" {
		t.Errorf("staged files = %q, want only README.md", staged)
	}
}

// TestStageScopedAuto_DeclaredCleanNoStage confirms a declared path that
// is clean (not dirty) is a no-op: nothing staged, no drift, no error
// from `git add` matching an unchanged pathspec.
func TestStageScopedAuto_DeclaredCleanNoStage(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	mustGitCLI(t, repo, "init", "--initial-branch=main")
	mustGitCLI(t, repo, "config", "user.name", "init")
	mustGitCLI(t, repo, "config", "user.email", "init@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGitCLI(t, repo, "add", "-A")
	mustGitCLI(t, repo, "commit", "-m", "initial")

	drift, err := stageScopedAuto(repo, []string{"README.md"})
	if err != nil {
		t.Fatalf("stageScopedAuto: %v", err)
	}
	if len(drift) != 0 {
		t.Errorf("drift = %v, want empty", drift)
	}
	if staged := mustGitOutCLI(t, repo, "diff", "--cached", "--name-only"); staged != "" {
		t.Errorf("staged files = %q, want none", staged)
	}
}

// TestAutoOpenPR_ScopeBoundedStaging verifies autoOpenPR stages exactly
// the declared scope path and leaves the stray file dirty, going through
// the full PR flow with a real git working tree but stubbed gh/ship.
func TestAutoOpenPR_ScopeBoundedStaging(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	bare := filepath.Join(dir, "origin.git")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGitCLI(t, repo, "init", "--initial-branch=main")
	mustGitCLI(t, repo, "config", "user.name", "init")
	mustGitCLI(t, repo, "config", "user.email", "init@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGitCLI(t, repo, "add", "-A")
	mustGitCLI(t, repo, "commit", "-m", "initial")
	mustGitCLI(t, dir, "init", "--bare", "origin.git")
	mustGitCLI(t, repo, "remote", "add", "origin", bare)

	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "stray.pid"), []byte("999\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prFile := filepath.Join(dir, "pr.md")
	if err := os.WriteFile(prFile, []byte("Scoped change\n\nBody.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	origPath := prDescriptionPath
	prDescriptionPath = prFile
	t.Cleanup(func() { prDescriptionPath = origPath })

	scopeFile := filepath.Join(dir, "scope.json")
	if err := os.WriteFile(scopeFile,
		[]byte(`{"files":[{"path":"README.md","operation":"modify"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	origScope := scopeFilePath
	scopeFilePath = scopeFile
	t.Cleanup(func() { scopeFilePath = origScope })

	origGh := autoGhCommand
	autoGhCommand = func(_ string, _ ...string) *exec.Cmd {
		return exec.Command("sh", "-c", "echo https://github.com/owner/repo/pull/7")
	}
	t.Cleanup(func() { autoGhCommand = origGh })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(httpclient.ShipLocalPullRequestResult{
			PRNumber: 7, PRURL: "https://github.com/owner/repo/pull/7",
		})
	}))
	defer srv.Close()

	_, err := autoOpenPR(context.Background(),
		httpclient.New(srv.URL, "test-token"),
		autoOpenPRArgs{
			WorkingDir: repo,
			RunID:      uuid.New(),
			StageID:    uuid.New(),
			GitHubRepo: "owner/repo",
			BaseBranch: "main",
		})
	if err != nil {
		t.Fatalf("autoOpenPR: %v", err)
	}

	// The commit on the pushed branch touched only README.md.
	committed := mustGitOutCLI(t, repo, "show", "--name-only", "--format=", "HEAD")
	if committed != "README.md" {
		t.Errorf("committed files = %q, want only README.md", committed)
	}
	// stray.pid stays dirty (excluded, not committed or lost).
	if status := mustGitOutCLI(t, repo, "status", "--porcelain"); !strings.Contains(status, "stray.pid") {
		t.Errorf("stray.pid should remain dirty; status = %q", status)
	}
}

// stubGitCommandDecomposed extends stubGitCommand to handle show-ref and
// stash/fetch/checkout/pull commands used by the decomposed-child path.
// showRefExists controls whether show-ref reports the branch as present.
func stubGitCommandDecomposed(showRefExists bool) func(string, ...string) *exec.Cmd {
	return func(name string, arg ...string) *exec.Cmd {
		if len(arg) >= 3 {
			switch arg[2] {
			case "show-ref":
				if showRefExists {
					return exec.Command("sh", "-c", "exit 0")
				}
				return exec.Command("sh", "-c", "exit 1")
			case "stash", "fetch", "checkout", "pull":
				return exec.Command("/usr/bin/true")
			}
		}
		return stubGitCommand(name, arg...)
	}
}

func TestAutoOpenPR_DecomposedFirstChild(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "pr-*.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, werr := f.WriteString("Add feature X\n\nThis adds feature X.\n"); werr != nil {
		t.Fatal(werr)
	}
	f.Close()

	origPath := prDescriptionPath
	prDescriptionPath = f.Name()
	t.Cleanup(func() { prDescriptionPath = origPath })

	origGit := autoGitCommand
	// First child: show-ref exits non-zero (branch not yet on remote).
	autoGitCommand = stubGitCommandDecomposed(false)
	t.Cleanup(func() { autoGitCommand = origGit })

	ghCalled := false
	origGh := autoGhCommand
	autoGhCommand = func(_ string, _ ...string) *exec.Cmd {
		ghCalled = true
		return exec.Command("sh", "-c",
			"echo https://github.com/owner/repo/pull/42")
	}
	t.Cleanup(func() { autoGhCommand = origGh })

	shipCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		shipCalled = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(httpclient.ShipLocalPullRequestResult{
			PRNumber: 42, PRURL: "https://github.com/owner/repo/pull/42",
		})
	}))
	defer srv.Close()

	parentID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	runID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	stageID := uuid.MustParse("22222222-3333-4444-5555-666666666666")

	result, err := autoOpenPR(context.Background(),
		httpclient.New(srv.URL, "test-token"),
		autoOpenPRArgs{
			WorkingDir:     t.TempDir(),
			RunID:          runID,
			StageID:        stageID,
			GitHubRepo:     "owner/repo",
			BaseBranch:     "main",
			DecomposedFrom: &parentID,
		})
	if err != nil {
		t.Fatalf("autoOpenPR: %v", err)
	}
	// Shared branch: fishhawk/run-<shortParentID>
	wantBranch := "fishhawk/run-aaaaaaaa"
	if result.Branch != wantBranch {
		t.Errorf("Branch = %q, want %q", result.Branch, wantBranch)
	}
	if !ghCalled {
		t.Error("gh pr create not called for first decomposed child")
	}
	if !shipCalled {
		t.Error("ShipLocalPullRequest not called for first decomposed child")
	}
}

func TestAutoOpenPR_DecomposedSubsequentChild(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "pr-*.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, werr := f.WriteString("Add feature Y\n\nThis adds feature Y.\n"); werr != nil {
		t.Fatal(werr)
	}
	f.Close()

	origPath := prDescriptionPath
	prDescriptionPath = f.Name()
	t.Cleanup(func() { prDescriptionPath = origPath })

	origGit := autoGitCommand
	// Subsequent child: show-ref exits 0 (branch already on remote).
	autoGitCommand = stubGitCommandDecomposed(true)
	t.Cleanup(func() { autoGitCommand = origGit })

	ghCalled := false
	origGh := autoGhCommand
	autoGhCommand = func(_ string, _ ...string) *exec.Cmd {
		ghCalled = true
		return exec.Command("sh", "-c",
			"echo https://github.com/owner/repo/pull/42")
	}
	t.Cleanup(func() { autoGhCommand = origGh })

	shipCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		shipCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	parentID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	runID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	stageID := uuid.MustParse("22222222-3333-4444-5555-666666666666")

	result, err := autoOpenPR(context.Background(),
		httpclient.New(srv.URL, "test-token"),
		autoOpenPRArgs{
			WorkingDir:     t.TempDir(),
			RunID:          runID,
			StageID:        stageID,
			GitHubRepo:     "owner/repo",
			BaseBranch:     "main",
			DecomposedFrom: &parentID,
		})
	if err != nil {
		t.Fatalf("autoOpenPR: %v", err)
	}
	wantBranch := "fishhawk/run-aaaaaaaa"
	if result.Branch != wantBranch {
		t.Errorf("Branch = %q, want %q", result.Branch, wantBranch)
	}
	// Subsequent child: PR was already opened by first child.
	if ghCalled {
		t.Error("gh pr create called for subsequent decomposed child — should be skipped")
	}
	if shipCalled {
		t.Error("ShipLocalPullRequest called for subsequent decomposed child — should be skipped")
	}
}
