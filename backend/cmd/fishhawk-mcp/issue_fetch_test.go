package main

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestResolveIssueRef_Forms(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"", 0, false}, // empty = "agent passed nothing"
		{"1247", 1247, false},
		{"#1247", 1247, false},
		{"  1247  ", 1247, false},
		{"https://github.com/owner/repo/issues/1247", 1247, false},
		{"https://github.com/owner/repo/issues/1247/", 1247, false},
		{"https://github.com/owner/repo/issues/1247#issuecomment-99", 1247, false},
		{"not-a-number", 0, true},
		{"-3", 0, true},
		{"0", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := resolveIssueRef(tc.in)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr=%v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestInferIssueNumberFromTriggerRef(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"issue:42", 42},
		{"issue:0", 0},
		{"issue:abc", 0},
		{"pr:42", 0},
		{"42", 0},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := inferIssueNumberFromTriggerRef(tc.in); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// withFakeGh swaps in a fake gh subprocess that returns the
// supplied JSON body verbatim. Also ensures ghLookPath says the
// binary is present (test seam — production uses exec.LookPath).
func withFakeGh(t *testing.T, jsonBody string) {
	t.Helper()
	origCmd := ghIssueCommand
	origLook := ghLookPath
	ghIssueCommand = func(_ string, _ ...string) *exec.Cmd {
		return exec.Command("sh", "-c", "cat <<'BODY'\n"+jsonBody+"\nBODY")
	}
	ghLookPath = func(_ string) (string, error) { return "/fake/gh", nil }
	t.Cleanup(func() {
		ghIssueCommand = origCmd
		ghLookPath = origLook
	})
}

// withFakeGhMissing makes the gh binary appear absent. Swaps
// ghLookPath rather than mutating PATH so the test doesn't race
// with other tests.
func withFakeGhMissing(t *testing.T) {
	t.Helper()
	orig := ghLookPath
	ghLookPath = func(_ string) (string, error) {
		return "", exec.ErrNotFound
	}
	t.Cleanup(func() { ghLookPath = orig })
}

// withFakeGhBroken returns a non-zero exit so the failure branch
// is exercised. Pair with a successful ghLookPath.
func withFakeGhBroken(t *testing.T) {
	t.Helper()
	origCmd := ghIssueCommand
	origLook := ghLookPath
	ghIssueCommand = func(_ string, _ ...string) *exec.Cmd {
		return exec.Command("/usr/bin/false")
	}
	ghLookPath = func(_ string) (string, error) { return "/fake/gh", nil }
	t.Cleanup(func() {
		ghIssueCommand = origCmd
		ghLookPath = origLook
	})
}

func TestFetchIssueViaGh_Success(t *testing.T) {
	withFakeGh(t, `{"title":"Add foo","body":"We need foo.","url":"https://github.com/x/y/issues/42","number":42}`)
	got, err := fetchIssueViaGh("x/y", 42)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Title != "Add foo" || got.Body != "We need foo." || got.Number != 42 {
		t.Errorf("decoded payload mismatch: %+v", got)
	}
	if got.URL != "https://github.com/x/y/issues/42" {
		t.Errorf("URL = %q", got.URL)
	}
}

func TestFetchIssueViaGh_DecodesComments(t *testing.T) {
	withFakeGh(t, `{"title":"Add foo","body":"We need foo.","url":"https://github.com/x/y/issues/42","number":42,"comments":[{"author":{"login":"alice"},"body":"Refinement here.","createdAt":"2026-05-01T10:00:00Z"},{"author":{"login":"bob"},"body":"Second comment.","createdAt":"2026-05-02T11:00:00Z"}]}`)
	got, err := fetchIssueViaGh("x/y", 42)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got.Comments) != 2 {
		t.Fatalf("want 2 comments, got %d: %+v", len(got.Comments), got.Comments)
	}
	if got.Comments[0].Author != "alice" || got.Comments[0].Body != "Refinement here." ||
		got.Comments[0].CreatedAt != "2026-05-01T10:00:00Z" {
		t.Errorf("comment[0] mismatch: %+v", got.Comments[0])
	}
	if got.Comments[1].Author != "bob" {
		t.Errorf("comment[1] author = %q, want bob", got.Comments[1].Author)
	}
}

func TestFetchIssueViaGh_NotInstalled(t *testing.T) {
	withFakeGhMissing(t)
	_, err := fetchIssueViaGh("x/y", 42)
	if !errors.Is(err, ErrGhNotInstalled) {
		t.Errorf("err = %v, want ErrGhNotInstalled", err)
	}
}

func TestFetchIssueViaGh_CommandFails(t *testing.T) {
	withFakeGhBroken(t)
	_, err := fetchIssueViaGh("x/y", 42)
	if err == nil {
		t.Fatal("expected error from broken gh subprocess")
	}
	if errors.Is(err, ErrGhNotInstalled) {
		t.Errorf("err should NOT be ErrGhNotInstalled: %v", err)
	}
}

// --- resolveGitHubLoginViaGh (#751) ---

func TestResolveGitHubLoginViaGh_Success(t *testing.T) {
	// Record the args so we lock the exact `gh api user --jq .login`
	// invocation, and return a trailing-newline-terminated login so the
	// trim behavior is exercised (gh emits one line + \n).
	var gotArgs []string
	origCmd := ghIssueCommand
	origLook := ghLookPath
	ghIssueCommand = func(name string, args ...string) *exec.Cmd {
		gotArgs = append([]string{name}, args...)
		return exec.Command("sh", "-c", "printf 'kuhlman-labs\\n'")
	}
	ghLookPath = func(string) (string, error) { return "/fake/gh", nil }
	t.Cleanup(func() {
		ghIssueCommand = origCmd
		ghLookPath = origLook
	})

	got, err := resolveGitHubLoginViaGh()
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "kuhlman-labs" {
		t.Errorf("login = %q, want kuhlman-labs (trimmed)", got)
	}
	want := []string{"gh", "api", "user", "--jq", ".login"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("gh invoked with %v, want %v", gotArgs, want)
	}
}

func TestResolveGitHubLoginViaGh_NotInstalled(t *testing.T) {
	withFakeGhMissing(t)
	_, err := resolveGitHubLoginViaGh()
	if !errors.Is(err, ErrGhNotInstalled) {
		t.Errorf("err = %v, want ErrGhNotInstalled", err)
	}
}

func TestResolveGitHubLoginViaGh_CommandFails(t *testing.T) {
	withFakeGhBroken(t)
	_, err := resolveGitHubLoginViaGh()
	if err == nil {
		t.Fatal("expected error from broken gh subprocess")
	}
	if errors.Is(err, ErrGhNotInstalled) {
		t.Errorf("err should NOT be ErrGhNotInstalled: %v", err)
	}
}
