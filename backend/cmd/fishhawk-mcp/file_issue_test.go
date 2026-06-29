package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// --- fishhawk_file_issue (#1005) ---

// fileIssueFakeBackend is a self-contained backend stub for the file-issue
// tool: it serves only POST /v0/work-items. lastBody captures the last
// decoded request so tests assert field threading; status drives the HTTP
// status (default 201); errBody, when set, is written verbatim for the
// error-path tests. resp overrides the default echoed item.
type fileIssueFakeBackend struct {
	mu       sync.Mutex
	lastBody FileWorkItemRequest
	calls    int
	status   int
	errBody  string
	resp     *FiledWorkItem
}

func newFileIssueFakeBackend(t *testing.T) (*fileIssueFakeBackend, *httptest.Server) {
	fb := &fileIssueFakeBackend{status: http.StatusCreated}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/work-items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body FileWorkItemRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.calls++
		fb.lastBody = body
		status := fb.status
		errBody := fb.errBody
		resp := fb.resp
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if resp == nil {
			resp = &FiledWorkItem{
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
			}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fb, srv
}

func TestFileIssue_HappyPath_ThreadsFieldsAndRelations(t *testing.T) {
	fb, srv := newFileIssueFakeBackend(t)
	r := newResolver(srv, nil)

	_, out, err := r.fileIssue(context.Background(), nil, FileIssueInput{
		Repo:       "kuhlman-labs/fishhawk",
		Type:       "feature",
		Summary:    "Add the widget endpoint",
		Complexity: "low",
		Labels:     []string{"area:backend"},
		Relations: &FileIssueRelations{
			ParentEpic:   "#1005",
			EvidenceRuns: []string{"run-abc"},
			DependsOn:    []string{"#41", "42"},
		},
	})
	if err != nil {
		t.Fatalf("fileIssue: %v", err)
	}
	if out.Item.Number != 1207 {
		t.Errorf("Number = %d, want 1207", out.Item.Number)
	}
	if out.Item.Provider != "github_projects" {
		t.Errorf("Provider = %q", out.Item.Provider)
	}
	if fb.calls != 1 {
		t.Errorf("backend called %d times, want 1", fb.calls)
	}
	if fb.lastBody.Type != "feature" || fb.lastBody.Summary != "Add the widget endpoint" {
		t.Errorf("body type/summary = %q/%q", fb.lastBody.Type, fb.lastBody.Summary)
	}
	if fb.lastBody.Complexity != "low" {
		t.Errorf("body complexity = %q, want low", fb.lastBody.Complexity)
	}
	if fb.lastBody.Relations == nil || fb.lastBody.Relations.ParentEpic != "#1005" {
		t.Errorf("body relations = %+v", fb.lastBody.Relations)
	}
	if got := fb.lastBody.Relations.DependsOn; len(got) != 2 || got[0] != "#41" || got[1] != "42" {
		t.Errorf("body relations depends_on = %v, want [#41 42]", got)
	}
}

func TestFileIssue_RepoAndRunFromEnv(t *testing.T) {
	fb, srv := newFileIssueFakeBackend(t)
	r := newResolver(srv, map[string]string{
		"GITHUB_REPOSITORY": "kuhlman-labs/fishhawk",
		"FISHHAWK_RUN_ID":   "11111111-1111-1111-1111-111111111111",
	})

	_, out, err := r.fileIssue(context.Background(), nil, FileIssueInput{
		Type:    "bug",
		Summary: "Widget 500s on empty body",
	})
	if err != nil {
		t.Fatalf("fileIssue: %v", err)
	}
	if fb.lastBody.Repo != "kuhlman-labs/fishhawk" {
		t.Errorf("body repo = %q, want env fallback", fb.lastBody.Repo)
	}
	if fb.lastBody.RunID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("body run_id = %q, want env fallback", fb.lastBody.RunID)
	}
	if !out.Item.Audited {
		t.Errorf("Audited = false, want true (run in flight)")
	}
}

func TestFileIssue_MissingType_FailsLocally(t *testing.T) {
	fb, srv := newFileIssueFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.fileIssue(context.Background(), nil, FileIssueInput{
		Repo: "o/n", Summary: "x",
	})
	if err == nil || !strings.Contains(err.Error(), "type is required") {
		t.Fatalf("err = %v, want type-required error", err)
	}
	if fb.calls != 0 {
		t.Errorf("backend called %d times, want 0", fb.calls)
	}
}

func TestFileIssue_MissingSummary_FailsLocally(t *testing.T) {
	fb, srv := newFileIssueFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.fileIssue(context.Background(), nil, FileIssueInput{
		Repo: "o/n", Type: "feature", Summary: "  ",
	})
	if err == nil || !strings.Contains(err.Error(), "summary is required") {
		t.Fatalf("err = %v, want summary-required error", err)
	}
	if fb.calls != 0 {
		t.Errorf("backend called %d times, want 0", fb.calls)
	}
}

func TestFileIssue_MissingRepoNoEnv_FailsLocally(t *testing.T) {
	fb, srv := newFileIssueFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.fileIssue(context.Background(), nil, FileIssueInput{
		Type: "feature", Summary: "x",
	})
	if err == nil || !strings.Contains(err.Error(), "repo is required") {
		t.Fatalf("err = %v, want repo-required error", err)
	}
	if fb.calls != 0 {
		t.Errorf("backend called %d times, want 0", fb.calls)
	}
}

func TestFileIssue_BoardingBestEffort_DecodesThroughMirror(t *testing.T) {
	fb, srv := newFileIssueFakeBackend(t)
	fb.resp = &FiledWorkItem{
		Type:          "feature",
		Title:         "Best-effort boarding",
		Number:        1300,
		URL:           "https://github.com/kuhlman-labs/fishhawk/issues/1300",
		Provider:      "github_projects",
		Boarded:       false,
		EpicLinked:    false,
		BoardingError: "workmgmt/github: status \"Backlog\" is not a Status option on the project",
	}
	r := newResolver(srv, nil)

	_, out, err := r.fileIssue(context.Background(), nil, FileIssueInput{
		Repo: "kuhlman-labs/fishhawk", Type: "feature", Summary: "x",
	})
	if err != nil {
		t.Fatalf("fileIssue: %v", err)
	}
	// The created issue lands; boarded/epic_linked decode through the mirror
	// so the tool renders exactly what landed (#1107).
	if out.Item.Number != 1300 {
		t.Errorf("Number = %d, want 1300", out.Item.Number)
	}
	if out.Item.Boarded {
		t.Errorf("Boarded = true, want false")
	}
	if !strings.Contains(out.Item.BoardingError, "is not a Status option") {
		t.Errorf("BoardingError = %q, want the cause", out.Item.BoardingError)
	}
}

func TestFileIssue_FilingFailed_SurfacesDetailsError(t *testing.T) {
	fb, srv := newFileIssueFakeBackend(t)
	fb.status = http.StatusBadGateway
	fb.errBody = `{"error":{"code":"work_item_filing_failed","message":"provider could not file the work item","details":{"error":"workmgmt/github: create issue: 403 Resource not accessible by integration"}}}`
	r := newResolver(srv, nil)

	_, _, err := r.fileIssue(context.Background(), nil, FileIssueInput{
		Repo: "kuhlman-labs/fishhawk", Type: "feature", Summary: "x",
	})
	if err == nil {
		t.Fatal("want a tool error on a 502")
	}
	// The operator must see the provider cause, not a bare HTTP 502.
	if !strings.Contains(err.Error(), "create issue: 403 Resource not accessible by integration") {
		t.Errorf("err = %v, want the surfaced details.error cause", err)
	}
}

func TestFileIssue_ProviderUnimplemented_PropagatesError(t *testing.T) {
	fb, srv := newFileIssueFakeBackend(t)
	fb.status = http.StatusNotImplemented
	fb.errBody = `{"error":{"code":"provider_unimplemented","message":"work-item provider \"jira\" is not registered"}}`
	r := newResolver(srv, nil)

	_, _, err := r.fileIssue(context.Background(), nil, FileIssueInput{
		Repo: "o/n", Type: "feature", Summary: "x",
	})
	if err == nil || !strings.Contains(err.Error(), "provider_unimplemented") {
		t.Fatalf("err = %v, want provider_unimplemented", err)
	}
}
