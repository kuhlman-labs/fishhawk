package github

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// fakeFeedbackAPI records the calls the provider makes and returns canned
// results. createdBodies captures every created issue body so the
// create-then-search marker round-trip can be asserted.
type fakeFeedbackAPI struct {
	searchResults []MatchedIssue
	searchQuery   string
	searchErr     error

	createdBody  string
	createdTitle string
	createErr    error

	commentNumber int
	commentBody   string
	commentErr    error
}

func (f *fakeFeedbackAPI) SearchOpenIssues(_ context.Context, _ int64, _ githubclient.RepoRef, query string) ([]MatchedIssue, error) {
	f.searchQuery = query
	return f.searchResults, f.searchErr
}

func (f *fakeFeedbackAPI) CreateIssue(_ context.Context, _ int64, _ githubclient.RepoRef, p githubclient.CreateIssueParams) (*githubclient.CreatedIssue, error) {
	f.createdBody = p.Body
	f.createdTitle = p.Title
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &githubclient.CreatedIssue{Number: 7, HTMLURL: "https://github.com/kuhlman-labs/fishhawk/issues/7"}, nil
}

func (f *fakeFeedbackAPI) CreateIssueComment(_ context.Context, _ int64, _ githubclient.RepoRef, number int, body string) (*githubclient.IssueComment, error) {
	f.commentNumber = number
	f.commentBody = body
	if f.commentErr != nil {
		return nil, f.commentErr
	}
	return &githubclient.IssueComment{ID: 99, Body: body, HTMLURL: "https://github.com/x/y/issues/7#c99"}, nil
}

func testTarget() workmgmt.Target {
	return workmgmt.Target{InstallationID: 42, Repo: workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"}}
}

func TestFeedback_FileEmbedsMarker_SearchFindsIt(t *testing.T) {
	api := &fakeFeedbackAPI{}
	p := NewFeedback(api)
	const fp = "abc123def456"

	created, err := p.File(context.Background(), testTarget(), workmgmt.FeedbackReport{
		Title:       "Diagnostic report",
		Body:        "product facts only",
		Labels:      []string{"type:bug"},
		Fingerprint: fp,
	})
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if created.Number != 7 || created.Provider != FeedbackProviderName {
		t.Errorf("created = %+v", created)
	}
	// The body must carry the exact marker the search will look for.
	if !strings.Contains(api.createdBody, marker(fp)) {
		t.Fatalf("created body missing marker %q: %q", marker(fp), api.createdBody)
	}

	// Feed the created body back as a search result: the provider must
	// re-verify the marker and report a hit. This pins writer==reader.
	api.searchResults = []MatchedIssue{{Number: 7, URL: created.URL, Body: api.createdBody}}
	existing, err := p.SearchOpenByFingerprint(context.Background(), testTarget(), fp)
	if err != nil {
		t.Fatalf("SearchOpenByFingerprint: %v", err)
	}
	if existing == nil || existing.Number != 7 {
		t.Fatalf("dedup hit = %+v, want #7", existing)
	}
	if !strings.Contains(api.searchQuery, marker(fp)) {
		t.Errorf("search query did not filter on the marker: %q", api.searchQuery)
	}
}

func TestFeedback_SearchMiss_ReturnsNil(t *testing.T) {
	api := &fakeFeedbackAPI{searchResults: []MatchedIssue{
		{Number: 3, Body: "unrelated issue without the marker"},
	}}
	p := NewFeedback(api)
	existing, err := p.SearchOpenByFingerprint(context.Background(), testTarget(), "deadbeef0000")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if existing != nil {
		t.Errorf("dedup hit = %+v, want nil (no marker in any body)", existing)
	}
}

func TestFeedback_AppendOccurrence(t *testing.T) {
	api := &fakeFeedbackAPI{}
	p := NewFeedback(api)
	if err := p.AppendOccurrence(context.Background(), testTarget(), 7, "another occurrence"); err != nil {
		t.Fatalf("AppendOccurrence: %v", err)
	}
	if api.commentNumber != 7 || api.commentBody != "another occurrence" {
		t.Errorf("comment = #%d %q", api.commentNumber, api.commentBody)
	}
}

func TestFeedback_FailsClosedWithoutInstallation(t *testing.T) {
	p := NewFeedback(&fakeFeedbackAPI{})
	tgt := workmgmt.Target{Repo: workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"}}
	if _, err := p.File(context.Background(), tgt, workmgmt.FeedbackReport{}); err == nil {
		t.Error("File with no installation id should fail closed")
	}
	if _, err := p.SearchOpenByFingerprint(context.Background(), tgt, "x"); err == nil {
		t.Error("Search with no installation id should fail closed")
	}
}

func TestFeedback_PropagatesSearchError(t *testing.T) {
	api := &fakeFeedbackAPI{searchErr: errors.New("boom")}
	p := NewFeedback(api)
	if _, err := p.SearchOpenByFingerprint(context.Background(), testTarget(), "x"); err == nil {
		t.Error("search error should propagate")
	}
}
