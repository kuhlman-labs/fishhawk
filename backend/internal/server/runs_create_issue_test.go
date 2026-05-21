package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCreateRun_IssueContext_PersistedOnRun is the #415
// happy-path: the CLI ships an inline issue_context payload on
// an issue-triggered run, the API persists it on the row, and
// the response echoes the same shape back.
func TestCreateRun_IssueContext_PersistedOnRun(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "trivial",
		"workflow_sha":   "abc",
		"trigger_source": "github_issue",
		"trigger_ref":    "issue:42",
		"workflow_spec":  minimalSpecYAML,
		"issue_context": map[string]any{
			"title":  "Add foo",
			"body":   "We need foo helpers.",
			"url":    "https://github.com/x/y/issues/42",
			"number": 42,
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var got runResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.IssueContext == nil {
		t.Fatal("response missing IssueContext")
	}
	if got.IssueContext.Title != "Add foo" || got.IssueContext.Body != "We need foo helpers." {
		t.Errorf("IssueContext mismatch in response: %+v", got.IssueContext)
	}
	if got.IssueContext.Number != 42 {
		t.Errorf("IssueContext.Number = %d, want 42", got.IssueContext.Number)
	}
	// CreateRunParams was given the same shape, so prompt builder
	// will read from the row.
	if repo.lastCreateRunParams.IssueContext == nil {
		t.Fatal("IssueContext not forwarded to repo CreateRun")
	}
	if repo.lastCreateRunParams.IssueContext.Body != "We need foo helpers." {
		t.Errorf("repo IssueContext.Body = %q", repo.lastCreateRunParams.IssueContext.Body)
	}
}

// TestCreateRun_IssueContext_RejectedOnNonIssueTrigger documents
// the narrow shape: shipping issue_context with a non-issue
// trigger_source returns 400 rather than silently dropping it,
// so a CLI typo surfaces fast.
func TestCreateRun_IssueContext_RejectedOnNonIssueTrigger(t *testing.T) {
	s := newServer(t, newFakeRepo())
	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "trivial",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"issue_context": map[string]any{
			"title": "Should not be here",
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "issue_context") {
		t.Errorf("body should reference issue_context: %s", w.Body.String())
	}
}
