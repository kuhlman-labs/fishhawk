package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fileIssueServer is a self-contained backend stub for the file-issue verb:
// it serves only POST /v0/work-items, captures the last request body, and
// echoes a created item (or a configured error envelope).
type fileIssueServer struct {
	mu       sync.Mutex
	lastBody fileIssueRequest
	calls    int
	status   int
	errBody  string
}

func newFileIssueServer(t *testing.T) (*fileIssueServer, *httptest.Server) {
	fs := &fileIssueServer{status: http.StatusCreated}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/work-items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body fileIssueRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		fs.mu.Lock()
		fs.calls++
		fs.lastBody = body
		status := fs.status
		errBody := fs.errBody
		fs.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		_ = json.NewEncoder(w).Encode(filedWorkItem{
			Type:          body.Type,
			Title:         "Add the widget endpoint",
			Number:        1207,
			URL:           "https://github.com/" + body.Repo + "/issues/1207",
			Provider:      "github_projects",
			AppliedLabels: []string{"type:feature"},
			Complexity:    body.Complexity,
			Status:        "Backlog",
			BoardColumn:   "Backlog",
			Audited:       body.RunID != "",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fs, srv
}

func TestFileIssue_HappyPath_Text(t *testing.T) {
	fs, srv := newFileIssueServer(t)
	withBackend(t, srv)

	var stdout strings.Builder
	got := run([]string{
		"file-issue",
		"--repo", "kuhlman-labs/fishhawk",
		"--type", "feature",
		"--summary", "Add the widget endpoint",
		"--complexity", "low",
		"--label", "area:backend",
		"--parent-epic", "#1005",
	}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	out := stdout.String()
	if !strings.Contains(out, "filed feature #1207") {
		t.Errorf("text output missing filed line:\n%s", out)
	}
	if !strings.Contains(out, "github_projects") {
		t.Errorf("text output missing provider:\n%s", out)
	}
	if fs.calls != 1 {
		t.Errorf("backend called %d times, want 1", fs.calls)
	}
	if fs.lastBody.Type != "feature" || fs.lastBody.Summary != "Add the widget endpoint" {
		t.Errorf("body type/summary = %q/%q", fs.lastBody.Type, fs.lastBody.Summary)
	}
	if fs.lastBody.Complexity != "low" {
		t.Errorf("body complexity = %q, want low", fs.lastBody.Complexity)
	}
	if len(fs.lastBody.Labels) != 1 || fs.lastBody.Labels[0] != "area:backend" {
		t.Errorf("body labels = %v", fs.lastBody.Labels)
	}
	if fs.lastBody.Relations == nil || fs.lastBody.Relations.ParentEpic != "#1005" {
		t.Errorf("body relations = %+v", fs.lastBody.Relations)
	}
}

func TestFileIssue_JSONOutput(t *testing.T) {
	_, srv := newFileIssueServer(t)
	withBackend(t, srv)

	var stdout strings.Builder
	got := run([]string{
		"file-issue",
		"--repo", "o/n", "--type", "bug", "--summary", "x",
		"--output", "json",
	}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	var item filedWorkItem
	if err := json.Unmarshal([]byte(stdout.String()), &item); err != nil {
		t.Fatalf("json output not decodable: %v\n%s", err, stdout.String())
	}
	if item.Number != 1207 {
		t.Errorf("Number = %d, want 1207", item.Number)
	}
}

func TestFileIssue_RepoFromEnv(t *testing.T) {
	fs, srv := newFileIssueServer(t)
	withBackend(t, srv)
	t.Setenv("GITHUB_REPOSITORY", "kuhlman-labs/fishhawk")

	got := run([]string{
		"file-issue", "--type", "chore", "--summary", "tidy",
		"--run-id", "11111111-1111-1111-1111-111111111111",
	}, io.Discard, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	if fs.lastBody.Repo != "kuhlman-labs/fishhawk" {
		t.Errorf("body repo = %q, want env fallback", fs.lastBody.Repo)
	}
	if fs.lastBody.RunID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("body run_id = %q", fs.lastBody.RunID)
	}
}

func TestFileIssue_MissingType_Usage(t *testing.T) {
	fs, srv := newFileIssueServer(t)
	withBackend(t, srv)

	var stderr strings.Builder
	got := run([]string{"file-issue", "--repo", "o/n", "--summary", "x"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Fatalf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "--type is required") {
		t.Errorf("stderr missing --type message: %s", stderr.String())
	}
	if fs.calls != 0 {
		t.Errorf("backend called %d times, want 0", fs.calls)
	}
}

func TestFileIssue_MissingSummary_Usage(t *testing.T) {
	_, srv := newFileIssueServer(t)
	withBackend(t, srv)

	var stderr strings.Builder
	got := run([]string{"file-issue", "--repo", "o/n", "--type", "feature"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Fatalf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "--summary is required") {
		t.Errorf("stderr missing --summary message: %s", stderr.String())
	}
}

func TestFileIssue_MissingRepoNoEnv_Usage(t *testing.T) {
	_, srv := newFileIssueServer(t)
	withBackend(t, srv)
	t.Setenv("GITHUB_REPOSITORY", "")

	var stderr strings.Builder
	got := run([]string{"file-issue", "--type", "feature", "--summary", "x"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Fatalf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "--repo is required") {
		t.Errorf("stderr missing --repo message: %s", stderr.String())
	}
}

func TestFileIssue_ProviderUnimplemented_Failure(t *testing.T) {
	fs, srv := newFileIssueServer(t)
	withBackend(t, srv)
	fs.status = http.StatusNotImplemented
	fs.errBody = `{"error":{"code":"provider_unimplemented","message":"work-item provider \"jira\" is not registered"}}`

	var stderr strings.Builder
	got := run([]string{
		"file-issue", "--repo", "o/n", "--type", "feature", "--summary", "x",
	}, io.Discard, &stderr)
	if got != exitFailure {
		t.Fatalf("status = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "provider_unimplemented") {
		t.Errorf("stderr missing provider_unimplemented: %s", stderr.String())
	}
}
