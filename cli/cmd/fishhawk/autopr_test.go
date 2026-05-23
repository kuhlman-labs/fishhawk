package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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
	wantTitle := "Fishhawk: implement stage aabbccdd"
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
	wantTitle := "Fishhawk: implement stage 11111111"
	if title != wantTitle {
		t.Errorf("title = %q, want %q", title, wantTitle)
	}
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
