package main

import (
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// stripGhFromPath ensures the gh lookup inside ghcomment.Post
// fails fast, so maybePostLocalComment can't accidentally invoke
// a real gh during the test (which would either spam an issue or
// take seconds to time out). Tests assert behavior via stderr
// rather than a recording fake.
func stripGhFromPath(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("PATH", tmp)
	// Restore via t.Setenv's cleanup; nothing else needed.
	_ = os.Getenv("PATH") // touch so the env var is read
}

func TestMaybePostLocalComment_NilRun(t *testing.T) {
	stripGhFromPath(t)
	var stderr strings.Builder
	maybePostLocalComment(&stderr, nil, "hello")
	if stderr.Len() != 0 {
		t.Errorf("nil run should be a no-op; stderr: %s", stderr.String())
	}
}

func TestMaybePostLocalComment_NonLocalRunnerKind(t *testing.T) {
	stripGhFromPath(t)
	var stderr strings.Builder
	r := &httpclient.Run{
		ID:           uuid.New(),
		Repo:         "x/y",
		RunnerKind:   "github_actions",
		IssueContext: &httpclient.IssueContext{Number: 42},
	}
	maybePostLocalComment(&stderr, r, "hello")
	if stderr.Len() != 0 {
		t.Errorf("github_actions runs handled by backend notifier; CLI should not post: %s",
			stderr.String())
	}
}

func TestMaybePostLocalComment_NoIssueContext(t *testing.T) {
	stripGhFromPath(t)
	var stderr strings.Builder
	r := &httpclient.Run{
		ID:         uuid.New(),
		Repo:       "x/y",
		RunnerKind: "local",
		// No IssueContext — manual local run, no issue thread.
	}
	maybePostLocalComment(&stderr, r, "hello")
	if stderr.Len() != 0 {
		t.Errorf("non-issue local run should not post; stderr: %s", stderr.String())
	}
}

func TestMaybePostLocalComment_EmptyBody(t *testing.T) {
	stripGhFromPath(t)
	var stderr strings.Builder
	r := &httpclient.Run{
		ID:           uuid.New(),
		Repo:         "x/y",
		RunnerKind:   "local",
		IssueContext: &httpclient.IssueContext{Number: 42},
	}
	maybePostLocalComment(&stderr, r, "")
	if stderr.Len() != 0 {
		t.Errorf("empty body should be a no-op; stderr: %s", stderr.String())
	}
}

func TestMaybePostLocalComment_GhMissing_QuietDegradation(t *testing.T) {
	// Local-issue run, gh missing. ghcomment.Post returns
	// ErrGhNotInstalled; maybePostLocalComment swallows it
	// silently (the kickoff path already warned the operator
	// once).
	stripGhFromPath(t)
	var stderr strings.Builder
	r := &httpclient.Run{
		ID:           uuid.New(),
		Repo:         "x/y",
		RunnerKind:   "local",
		IssueContext: &httpclient.IssueContext{Number: 42},
	}
	maybePostLocalComment(&stderr, r, "hello")
	if stderr.Len() != 0 {
		t.Errorf("gh-missing should not double-warn here; stderr: %s",
			stderr.String())
	}
}

func TestToGhCommentRun_ThreadsPullRequestURL(t *testing.T) {
	pr := "https://github.com/x/y/pull/77"
	r := &httpclient.Run{
		ID:             uuid.New(),
		WorkflowID:     "feature_change",
		State:          "succeeded",
		RunnerKind:     "local",
		PullRequestURL: &pr,
	}
	gcr := toGhCommentRun(r, "http://localhost:8080")
	if gcr.PullRequestURL != pr {
		t.Errorf("PullRequestURL not threaded: %q", gcr.PullRequestURL)
	}
	if gcr.ExternalURL != "http://localhost:8080" {
		t.Errorf("ExternalURL not threaded: %q", gcr.ExternalURL)
	}
	if gcr.WorkflowID != "feature_change" {
		t.Errorf("WorkflowID not threaded: %q", gcr.WorkflowID)
	}
}

func TestToGhCommentRun_NilPullRequestURL(t *testing.T) {
	r := &httpclient.Run{
		ID:         uuid.New(),
		WorkflowID: "trivial",
		State:      "pending",
	}
	gcr := toGhCommentRun(r, "http://localhost:8080")
	if gcr.PullRequestURL != "" {
		t.Errorf("nil PR should map to empty string, got %q", gcr.PullRequestURL)
	}
}
