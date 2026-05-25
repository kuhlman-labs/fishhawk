package ghcomment

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func mkRun(t *testing.T) Run {
	t.Helper()
	return Run{
		ID:          uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		WorkflowID:  "feature_change",
		State:       "pending",
		RunnerKind:  "local",
		ExternalURL: "http://localhost:8080",
	}
}

func TestRenderKickoff(t *testing.T) {
	got := RenderKickoff(mkRun(t))
	for _, want := range []string{
		"Fishhawk picked this up",
		"`11111111`",
		"http://localhost:8080/runs/11111111-2222-3333-4444-555555555555",
		"feature_change",
		"plan stage queued",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("kickoff missing %q\n---\n%s", want, got)
		}
	}
}

func TestRenderPlanApproved_WithApprover(t *testing.T) {
	got := RenderPlanApproved(mkRun(t), "brettkuhlman")
	if !strings.Contains(got, "by @brettkuhlman") {
		t.Errorf("missing approver clause: %s", got)
	}
	if !strings.Contains(got, "implement stage queued") {
		t.Errorf("missing implement clause: %s", got)
	}
}

func TestRenderPlanApproved_AnonymousApprover(t *testing.T) {
	got := RenderPlanApproved(mkRun(t), "")
	if strings.Contains(got, " by @") {
		t.Errorf("anonymous approver should not produce 'by @' clause: %s", got)
	}
	if !strings.Contains(got, "Plan approved.") {
		t.Errorf("anonymous approver missing 'Plan approved.': %s", got)
	}
}

func TestRenderPlanRejected_WithReason(t *testing.T) {
	got := RenderPlanRejected(mkRun(t), "brett", "scope is too broad; split into two PRs")
	if !strings.Contains(got, "Plan rejected by @brett") {
		t.Errorf("missing rejection lead: %s", got)
	}
	if !strings.Contains(got, "> scope is too broad") {
		t.Errorf("reason should render as blockquote: %s", got)
	}
}

func TestRenderPlanRejected_NoReason(t *testing.T) {
	got := RenderPlanRejected(mkRun(t), "brett", "")
	if strings.Contains(got, ">") {
		t.Errorf("empty reason should not render a blockquote: %s", got)
	}
}

func TestRenderRunCancelled(t *testing.T) {
	got := RenderRunCancelled(mkRun(t), "brett")
	if !strings.Contains(got, "cancelled by @brett") {
		t.Errorf("missing canceller: %s", got)
	}
}

func TestRenderImplementPROpened(t *testing.T) {
	r := mkRun(t)
	prURL := "https://github.com/kuhlman-labs/fishhawk/pull/99"
	got := RenderImplementPROpened(r, prURL, 99)
	for _, want := range []string{
		"implement stage opened",
		"PR #99",
		"11111111",
		prURL,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("RenderImplementPROpened missing %q\n---\n%s", want, got)
		}
	}
}

func TestRenderStageComplete_WithPR(t *testing.T) {
	r := mkRun(t)
	r.PullRequestURL = "https://github.com/x/y/pull/77"
	got := RenderStageComplete(r, "implement", "succeeded")
	for _, want := range []string{
		"`implement` stage complete",
		"succeeded",
		"PR: https://github.com/x/y/pull/77",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q: %s", want, got)
		}
	}
}

func TestRenderStageComplete_NoPR(t *testing.T) {
	got := RenderStageComplete(mkRun(t), "plan", "awaiting_approval")
	if strings.Contains(got, "PR:") {
		t.Errorf("plan stage should not surface PR line: %s", got)
	}
	if !strings.Contains(got, "awaiting_approval") {
		t.Errorf("missing state-after: %s", got)
	}
}

// withFakeGh swaps both the lookup and the command so the unit
// tests don't depend on a real gh binary being installed.
func withFakeGh(t *testing.T, body string, exitNonZero bool) {
	t.Helper()
	origLook := ghLookPath
	ghLookPath = func(_ string) (string, error) { return "/usr/local/bin/gh", nil }
	origCmd := ghCommentCommand
	if exitNonZero {
		ghCommentCommand = func(_ string, _ ...string) *exec.Cmd {
			return exec.Command("sh", "-c", "echo '"+body+"' >&2; exit 1")
		}
	} else {
		ghCommentCommand = func(_ string, _ ...string) *exec.Cmd {
			return exec.Command("/usr/bin/true")
		}
	}
	t.Cleanup(func() {
		ghLookPath = origLook
		ghCommentCommand = origCmd
	})
}

func withGhMissing(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	orig := os.Getenv("PATH")
	t.Setenv("PATH", tmp)
	origLook := ghLookPath
	ghLookPath = exec.LookPath // force the real lookup against the empty PATH
	t.Cleanup(func() {
		_ = os.Setenv("PATH", orig)
		ghLookPath = origLook
	})
}

func TestPost_HappyPath(t *testing.T) {
	withFakeGh(t, "", false)
	if err := Post("x/y", 42, "hello"); err != nil {
		t.Errorf("Post err = %v, want nil", err)
	}
}

func TestPost_GhMissing(t *testing.T) {
	withGhMissing(t)
	err := Post("x/y", 42, "hello")
	if !errors.Is(err, ErrGhNotInstalled) {
		t.Errorf("err = %v, want ErrGhNotInstalled", err)
	}
}

func TestPost_SubprocessFails_SurfacesStderr(t *testing.T) {
	withFakeGh(t, "HTTP 404: Not Found", true)
	err := Post("x/y", 42, "hello")
	if err == nil {
		t.Fatal("expected error from non-zero subprocess")
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("err should surface gh stderr: %v", err)
	}
}

func TestPost_ValidationGuards(t *testing.T) {
	withFakeGh(t, "", false)
	cases := []struct {
		name  string
		repo  string
		num   int
		body  string
		wantS string
	}{
		{"empty repo", "", 1, "x", "empty repo"},
		{"zero issue", "x/y", 0, "x", "invalid issue"},
		{"negative issue", "x/y", -1, "x", "invalid issue"},
		{"empty body", "x/y", 1, "  ", "empty body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Post(tc.repo, tc.num, tc.body)
			if err == nil || !strings.Contains(err.Error(), tc.wantS) {
				t.Errorf("err = %v, want substring %q", err, tc.wantS)
			}
		})
	}
}

// withFakeGhOutput sets ghCommentCommand to echo the given string to stdout
// and exit 0. Used by EditOrCreate tests that need a numeric id in the output.
func withFakeGhOutput(t *testing.T, stdout string) {
	t.Helper()
	origLook := ghLookPath
	ghLookPath = func(_ string) (string, error) { return "/usr/local/bin/gh", nil }
	origCmd := ghCommentCommand
	ghCommentCommand = func(_ string, _ ...string) *exec.Cmd {
		return exec.Command("sh", "-c", "printf '%s' '"+stdout+"'")
	}
	t.Cleanup(func() {
		ghLookPath = origLook
		ghCommentCommand = origCmd
	})
}

// withFakeGhOutputOn404 sets ghCommentCommand to behave differently on first
// vs. subsequent calls: first call exits 1 with "HTTP 404" in stderr (simulating
// a deleted comment); subsequent calls echo the given id to stdout and exit 0.
func withFakeGhOutputOn404(t *testing.T, fallbackID string) {
	t.Helper()
	origLook := ghLookPath
	ghLookPath = func(_ string) (string, error) { return "/usr/local/bin/gh", nil }
	origCmd := ghCommentCommand
	call := 0
	ghCommentCommand = func(_ string, _ ...string) *exec.Cmd {
		call++
		if call == 1 {
			return exec.Command("sh", "-c", "echo 'HTTP 404: Not Found' >&2; exit 1")
		}
		return exec.Command("sh", "-c", "printf '%s' '"+fallbackID+"'")
	}
	t.Cleanup(func() {
		ghLookPath = origLook
		ghCommentCommand = origCmd
	})
}

func TestEditOrCreate_CreatePath(t *testing.T) {
	withFakeGhOutput(t, "99")
	id, err := EditOrCreate("x/y", 42, 0, "hello body")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if id != 99 {
		t.Errorf("id = %d, want 99", id)
	}
}

func TestEditOrCreate_EditPath(t *testing.T) {
	withFakeGhOutput(t, "55")
	id, err := EditOrCreate("x/y", 42, 11, "updated body")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if id != 55 {
		t.Errorf("id = %d, want 55", id)
	}
}

func TestEditOrCreate_404FallbackToCreate(t *testing.T) {
	withFakeGhOutputOn404(t, "77")
	id, err := EditOrCreate("x/y", 42, 11, "body")
	if err != nil {
		t.Fatalf("err = %v, want nil (404 should fall back to create)", err)
	}
	if id != 77 {
		t.Errorf("id = %d, want 77 (from create fallback)", id)
	}
}

func TestEditOrCreate_GhMissing(t *testing.T) {
	withGhMissing(t)
	_, err := EditOrCreate("x/y", 42, 0, "body")
	if !errors.Is(err, ErrGhNotInstalled) {
		t.Errorf("err = %v, want ErrGhNotInstalled", err)
	}
}

func TestEditOrCreate_ValidationGuards(t *testing.T) {
	withFakeGhOutput(t, "1")
	cases := []struct {
		name  string
		repo  string
		num   int
		body  string
		wantS string
	}{
		{"empty repo", "", 1, "x", "empty repo"},
		{"invalid repo format", "noslash", 1, "x", "owner/name"},
		{"zero issue", "x/y", 0, "x", "invalid issue"},
		{"empty body", "x/y", 1, "  ", "empty body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := EditOrCreate(tc.repo, tc.num, 0, tc.body)
			if err == nil || !strings.Contains(err.Error(), tc.wantS) {
				t.Errorf("err = %v, want substring %q", err, tc.wantS)
			}
		})
	}
}

func TestFetchStatus_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(StatusCommentResponse{
			Body:            "**Fishhawk run** ...",
			IssueNumber:     42,
			Repo:            "x/y",
			GithubCommentID: 99,
		})
	}))
	defer srv.Close()

	runID := uuid.New().String()
	got, err := FetchStatus(srv.URL, runID)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got.GithubCommentID != 99 {
		t.Errorf("GithubCommentID = %d, want 99", got.GithubCommentID)
	}
	if got.Repo != "x/y" {
		t.Errorf("Repo = %q, want x/y", got.Repo)
	}
}

func TestFetchStatus_BackendError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := FetchStatus(srv.URL, uuid.New().String())
	if err == nil {
		t.Fatal("expected error on 404 response")
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("err = %v, want HTTP 404 mention", err)
	}
}

func TestRecordCommentID_HappyPath(t *testing.T) {
	var gotPayload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotPayload)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	runID := uuid.New().String()
	if err := RecordCommentID(srv.URL, runID, 42); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if gotPayload["github_comment_id"].(float64) != 42 {
		t.Errorf("payload github_comment_id = %v, want 42", gotPayload["github_comment_id"])
	}
}

func TestRecordCommentID_BackendError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := RecordCommentID(srv.URL, uuid.New().String(), 1)
	if err == nil {
		t.Fatal("expected error on 500 response")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("err = %v, want HTTP 500 mention", err)
	}
}
