package github

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
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

	// Board-placement recording (#1737). meta defaults to a Backlog-bearing
	// project in newBoardingFeedbackAPI; the *Err fields drive the
	// placement-failure arm.
	meta          *githubclient.ProjectMeta
	fieldsCoord   githubclient.ProjectCoord
	fieldsErr     error
	addItemCalled bool
	addItemErr    error
	setOptionID   string
	setCalled     bool
	setErr        error
}

func (f *fakeFeedbackAPI) ProjectFields(_ context.Context, _ forge.CredentialScope, coord githubclient.ProjectCoord, _ string) (*githubclient.ProjectMeta, error) {
	f.fieldsCoord = coord
	if f.fieldsErr != nil {
		return nil, f.fieldsErr
	}
	return f.meta, nil
}

func (f *fakeFeedbackAPI) AddProjectItem(_ context.Context, _ forge.CredentialScope, _, _ string) (string, error) {
	f.addItemCalled = true
	if f.addItemErr != nil {
		return "", f.addItemErr
	}
	return "item-1", nil
}

func (f *fakeFeedbackAPI) SetProjectItemSingleSelect(_ context.Context, _ forge.CredentialScope, _, _, _, optionID string) error {
	f.setCalled, f.setOptionID = true, optionID
	return f.setErr
}

// newBoardingFeedbackAPI returns a fake whose project carries a Backlog
// Status option, so a placement request for Backlog can succeed.
func newBoardingFeedbackAPI() *fakeFeedbackAPI {
	return &fakeFeedbackAPI{meta: &githubclient.ProjectMeta{
		ProjectID:     "proj-1",
		FieldID:       "field-1",
		StatusOptions: map[string]string{"Backlog": "opt-backlog"},
	}}
}

// boardingTarget is testTarget plus a configured project board.
func boardingTarget() workmgmt.Target {
	tgt := testTarget()
	tgt.Project = &workmgmt.Project{Owner: "kuhlman-labs", OwnerType: "user", Number: 7}
	return tgt
}

func (f *fakeFeedbackAPI) SearchOpenIssues(_ context.Context, _ forge.CredentialScope, _ githubclient.RepoRef, query string) ([]MatchedIssue, error) {
	f.searchQuery = query
	return f.searchResults, f.searchErr
}

func (f *fakeFeedbackAPI) CreateIssue(_ context.Context, _ forge.CredentialScope, _ githubclient.RepoRef, p githubclient.CreateIssueParams) (*githubclient.CreatedIssue, error) {
	f.createdBody = p.Body
	f.createdTitle = p.Title
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &githubclient.CreatedIssue{Number: 7, HTMLURL: "https://github.com/kuhlman-labs/fishhawk/issues/7"}, nil
}

func (f *fakeFeedbackAPI) CreateIssueComment(_ context.Context, _ forge.CredentialScope, _ githubclient.RepoRef, number int, body string) (*githubclient.IssueComment, error) {
	f.commentNumber = number
	f.commentBody = body
	if f.commentErr != nil {
		return nil, f.commentErr
	}
	return &githubclient.IssueComment{ID: 99, Body: body, HTMLURL: "https://github.com/x/y/issues/7#c99"}, nil
}

func testTarget() workmgmt.Target {
	return workmgmt.Target{Scope: forge.FromGitHubInstallationID(42), Repo: workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"}}
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

// --- board placement on a filed product report (#1737) ---

// TestFeedback_FileBoardsAtBacklog is the done-means behavioral pin: the
// filed report is placed on the configured project and its Status is set
// to the requested Backlog option.
func TestFeedback_FileBoardsAtBacklog(t *testing.T) {
	api := newBoardingFeedbackAPI()
	p := NewFeedback(api)

	created, err := p.File(context.Background(), boardingTarget(), workmgmt.FeedbackReport{
		Title:          "Diagnostic report",
		Body:           "facts",
		Labels:         []string{"type:bug", "autonomy:medium"},
		Fingerprint:    "fp1",
		BoardPlacement: workmgmt.BoardPlacement{Status: "Backlog"},
	})
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if !created.Boarded || created.BoardingError != "" {
		t.Errorf("Boarded=%v BoardingError=%q, want true/empty", created.Boarded, created.BoardingError)
	}
	if !api.addItemCalled || !api.setCalled {
		t.Errorf("addItem=%v set=%v, want both true", api.addItemCalled, api.setCalled)
	}
	if api.setOptionID != "opt-backlog" {
		t.Errorf("status option = %q, want opt-backlog", api.setOptionID)
	}
	if created.Status != "Backlog" {
		t.Errorf("created.Status = %q, want Backlog", created.Status)
	}
	if got := workmgmt.BoardingStatusOf(boardingTarget(), created); got != workmgmt.BoardingStatusBoarded {
		t.Errorf("BoardingStatusOf = %q, want boarded", got)
	}
	// The report itself is unaffected by boarding.
	if created.Number != 7 {
		t.Errorf("Number = %d, want 7", created.Number)
	}
}

// TestFeedback_FileBoardingFailureStillFiles is failure mode m7 and the
// counterfactual vehicle for the best-effort recover: placement errors,
// the report is still returned with number/URL intact, and the cause is
// recorded rather than returned as an error.
func TestFeedback_FileBoardingFailureStillFiles(t *testing.T) {
	api := newBoardingFeedbackAPI()
	api.addItemErr = errors.New("projects API 403")
	p := NewFeedback(api)

	created, err := p.File(context.Background(), boardingTarget(), workmgmt.FeedbackReport{
		Title: "t", Body: "b", Fingerprint: "fp2",
		BoardPlacement: workmgmt.BoardPlacement{Status: "Backlog"},
	})
	if err != nil {
		t.Fatalf("File returned an error on a placement failure: %v", err)
	}
	if created == nil || created.Number != 7 || created.URL == "" {
		t.Fatalf("created = %+v, want the filed report preserved", created)
	}
	if created.Boarded {
		t.Error("Boarded = true, want false after a placement failure")
	}
	if !strings.Contains(created.BoardingError, "projects API 403") {
		t.Errorf("BoardingError = %q, want it to name the cause", created.BoardingError)
	}
	if got := workmgmt.BoardingStatusOf(boardingTarget(), created); got != workmgmt.BoardingStatusFailed {
		t.Errorf("BoardingStatusOf = %q, want failed", got)
	}
}

// TestFeedback_FileNoProjectNotAttempted is failure mode m6 and the
// Condition-1 contract: no project configured is a CONFIGURATION STATE,
// so boarding is not attempted, no cause is recorded, and the distinct
// not_attempted_no_project status is what tells the operator apart from
// a real failure.
func TestFeedback_FileNoProjectNotAttempted(t *testing.T) {
	api := newBoardingFeedbackAPI()
	p := NewFeedback(api)

	created, err := p.File(context.Background(), testTarget(), workmgmt.FeedbackReport{
		Title: "t", Body: "b", Fingerprint: "fp3",
		BoardPlacement: workmgmt.BoardPlacement{Status: "Backlog"},
	})
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if created.Boarded || created.BoardingError != "" {
		t.Errorf("Boarded=%v BoardingError=%q, want false/empty (nothing to board)",
			created.Boarded, created.BoardingError)
	}
	if api.addItemCalled || api.setCalled {
		t.Error("board API called with no project configured")
	}
	if got := workmgmt.BoardingStatusOf(testTarget(), created); got != workmgmt.BoardingStatusNotAttemptedNoProject {
		t.Errorf("BoardingStatusOf = %q, want not_attempted_no_project", got)
	}
}

// TestFeedback_FileBoardingUnknownStatusOption pins the second placement
// failure branch: the project has no matching Status option, so placement
// fails with a naming cause while the report still lands.
func TestFeedback_FileBoardingUnknownStatusOption(t *testing.T) {
	api := &fakeFeedbackAPI{meta: &githubclient.ProjectMeta{
		ProjectID: "proj-1", FieldID: "field-1",
		StatusOptions: map[string]string{"Todo": "opt-todo"},
	}}
	p := NewFeedback(api)

	created, err := p.File(context.Background(), boardingTarget(), workmgmt.FeedbackReport{
		Title: "t", Body: "b", Fingerprint: "fp4",
		BoardPlacement: workmgmt.BoardPlacement{Status: "Backlog"},
	})
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if created.Boarded || !strings.Contains(created.BoardingError, "Backlog") {
		t.Errorf("Boarded=%v BoardingError=%q, want false and a Backlog-naming cause",
			created.Boarded, created.BoardingError)
	}
}
