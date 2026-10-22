package githubclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// snapshotFake serves POST /graphql with a scripted sequence of page bodies,
// recording each request's variables and Authorization header.
type snapshotFake struct {
	mu      sync.Mutex
	pages   []string
	status  int
	calls   int
	auth    []string
	cursors []any
	queries []string
}

func snapshotClient(t *testing.T) (*Client, *snapshotFake, *stubTokens) {
	t.Helper()
	f := &snapshotFake{status: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &req)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.cursors = append(f.cursors, req.Variables["cursor"])
		f.queries = append(f.queries, req.Query)
		idx := f.calls
		f.calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		if idx < len(f.pages) {
			_, _ = io.WriteString(w, f.pages[idx])
		}
	}))
	t.Cleanup(srv.Close)
	tok := &stubTokens{token: "ghs_run_install"}
	return &Client{BaseURL: srv.URL, Tokens: tok, HTTP: &http.Client{Timeout: 5 * time.Second}}, f, tok
}

// recorded-shape fixtures per the GitHub GraphQL schema (PullRequest,
// latestOpinionatedReviews, Actor union with User/Bot, nullable author).
const snapshotPageFull = `{"data":{"repository":{"isPrivate":true,"pullRequest":{
 "merged":false,"state":"OPEN","headRefOid":"h2","reviewDecision":"REVIEW_REQUIRED","mergeStateStatus":"BLOCKED",
 "author":{"__typename":"User","login":"opener","databaseId":11},
 "latestOpinionatedReviews":{"pageInfo":{"hasNextPage":false,"endCursor":"c1"},"nodes":[
  {"databaseId":101,"state":"APPROVED","submittedAt":"2026-10-20T10:00:00Z","commit":{"oid":"h2"},"author":{"__typename":"User","login":"alice","databaseId":21}},
  {"databaseId":102,"state":"CHANGES_REQUESTED","submittedAt":"2026-10-20T11:00:00Z","commit":{"oid":"h1"},"author":{"__typename":"Bot","login":"ci[bot]"}},
  {"databaseId":103,"state":"DISMISSED","submittedAt":"2026-10-20T12:00:00Z","commit":null,"author":null}
 ]}}}}}`

const snapshotPageNulls = `{"data":{"repository":{"isPrivate":false,"pullRequest":{
 "merged":true,"state":"MERGED","headRefOid":"h9","reviewDecision":null,"mergeStateStatus":null,
 "author":{"__typename":"Bot","login":"dependabot[bot]"},
 "latestOpinionatedReviews":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[
  {"databaseId":201,"state":"APPROVED","submittedAt":"2026-10-21T10:00:00Z","commit":{"oid":"h9"},"author":{"__typename":"User","login":"bob","databaseId":31}}
 ]}}}}}`

func TestPullRequestReviewSnapshot_DecodesFullShape(t *testing.T) {
	c, f, tok := snapshotClient(t)
	f.pages = []string{snapshotPageFull}
	snap, err := c.PullRequestReviewSnapshot(context.Background(), forge.FromGitHubInstallationID(55), RepoRef{Owner: "x", Name: "y"}, 7)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !snap.IsPrivate || snap.Merged || snap.State != "OPEN" || snap.HeadOID != "h2" {
		t.Errorf("pr fields = %+v", snap)
	}
	if snap.ReviewDecision != "REVIEW_REQUIRED" || snap.MergeStateStatus != "BLOCKED" {
		t.Errorf("decision/state = %q/%q", snap.ReviewDecision, snap.MergeStateStatus)
	}
	if want := (ReviewActor{Present: true, Typename: "User", Login: "opener", DatabaseID: 11}); snap.Author != want {
		t.Errorf("author = %+v, want %+v", snap.Author, want)
	}
	if snap.Truncated {
		t.Error("Truncated = true on a single complete page")
	}
	if len(snap.Reviews) != 3 {
		t.Fatalf("reviews = %d, want 3", len(snap.Reviews))
	}
	r0 := snap.Reviews[0]
	if r0.DatabaseID != 101 || r0.State != "APPROVED" || r0.CommitOID != "h2" ||
		!r0.SubmittedAt.Equal(time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)) ||
		r0.Author != (ReviewActor{Present: true, Typename: "User", Login: "alice", DatabaseID: 21}) {
		t.Errorf("review[0] = %+v", r0)
	}
	r1 := snap.Reviews[1]
	if r1.State != "CHANGES_REQUESTED" || r1.Author.Typename != "Bot" || r1.Author.DatabaseID != 0 || r1.Author.Login != "ci[bot]" {
		t.Errorf("review[1] (bot) = %+v", r1)
	}
	r2 := snap.Reviews[2]
	if r2.State != "DISMISSED" || r2.Author.Present || r2.CommitOID != "" {
		t.Errorf("review[2] (DISMISSED, null author, null commit) = %+v", r2)
	}
	if tok.installationCalled != 55 || !strings.Contains(f.auth[0], "ghs_run_install") {
		t.Errorf("token: installation=%d auth=%q, want the run's installation token", tok.installationCalled, f.auth[0])
	}
	if !strings.Contains(f.queries[0], "latestOpinionatedReviews") || !strings.Contains(f.queries[0], "writersOnly: true") {
		t.Errorf("query does not read latestOpinionatedReviews(writersOnly: true): %s", f.queries[0])
	}
}

// TestPullRequestReviewSnapshot_NullDecisionAndStateNoDismissed pins the
// null reviewDecision / mergeStateStatus decode ("" — a base with no review
// requirement, which is NOT "not approved") and a fixture WITHOUT a
// DISMISSED node; the fold must answer fail-closed on both shapes.
func TestPullRequestReviewSnapshot_NullDecisionAndStateNoDismissed(t *testing.T) {
	c, f, _ := snapshotClient(t)
	f.pages = []string{snapshotPageNulls}
	snap, err := c.PullRequestReviewSnapshot(context.Background(), forge.FromGitHubInstallationID(55), RepoRef{Owner: "x", Name: "y"}, 7)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.ReviewDecision != "" || snap.MergeStateStatus != "" {
		t.Errorf("null decision/state = %q/%q, want empty", snap.ReviewDecision, snap.MergeStateStatus)
	}
	if !snap.Merged || snap.IsPrivate || snap.State != "MERGED" {
		t.Errorf("merged/private/state = %v/%v/%q", snap.Merged, snap.IsPrivate, snap.State)
	}
	if snap.Author.Typename != "Bot" || snap.Author.DatabaseID != 0 {
		t.Errorf("bot opener = %+v", snap.Author)
	}
	for _, r := range snap.Reviews {
		if r.State == "DISMISSED" {
			t.Errorf("unexpected DISMISSED node: %+v", r)
		}
	}
}

func snapshotPage(hasNext bool, cursor string, reviewID int) string {
	next := "false"
	if hasNext {
		next = "true"
	}
	return `{"data":{"repository":{"isPrivate":false,"pullRequest":{"merged":false,"state":"OPEN","headRefOid":"h","reviewDecision":null,"mergeStateStatus":"CLEAN",
 "author":{"__typename":"User","login":"o","databaseId":1},
 "latestOpinionatedReviews":{"pageInfo":{"hasNextPage":` + next + `,"endCursor":"` + cursor + `"},"nodes":[
  {"databaseId":` + itoa(reviewID) + `,"state":"APPROVED","submittedAt":"2026-10-20T10:00:00Z","commit":{"oid":"h"},"author":{"__typename":"User","login":"u","databaseId":2}}]}}}}}`
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestPullRequestReviewSnapshot_PaginatesWithCursor(t *testing.T) {
	c, f, _ := snapshotClient(t)
	f.pages = []string{snapshotPage(true, "c1", 1), snapshotPage(false, "c2", 2)}
	snap, err := c.PullRequestReviewSnapshot(context.Background(), forge.FromGitHubInstallationID(1), RepoRef{Owner: "x", Name: "y"}, 3)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Truncated || len(snap.Reviews) != 2 {
		t.Errorf("truncated=%v reviews=%d, want false/2", snap.Truncated, len(snap.Reviews))
	}
	if f.calls != 2 || f.cursors[0] != nil || f.cursors[1] != "c1" {
		t.Errorf("calls=%d cursors=%v, want 2 / [nil c1]", f.calls, f.cursors)
	}
}

func TestPullRequestReviewSnapshot_TruncatedAtPageCap(t *testing.T) {
	c, f, _ := snapshotClient(t)
	f.pages = []string{snapshotPage(true, "c1", 1), snapshotPage(true, "c2", 2), snapshotPage(true, "c3", 3), snapshotPage(false, "c4", 4)}
	snap, err := c.PullRequestReviewSnapshot(context.Background(), forge.FromGitHubInstallationID(1), RepoRef{Owner: "x", Name: "y"}, 3)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !snap.Truncated {
		t.Error("Truncated = false with a next page still advertised at the cap")
	}
	if f.calls != reviewSnapshotMaxPages || len(snap.Reviews) != reviewSnapshotMaxPages {
		t.Errorf("calls=%d reviews=%d, want %d each", f.calls, len(snap.Reviews), reviewSnapshotMaxPages)
	}
}

func TestPullRequestReviewSnapshot_EmptyCursorWithNextPageIsTruncated(t *testing.T) {
	c, f, _ := snapshotClient(t)
	f.pages = []string{snapshotPage(true, "", 1)}
	snap, err := c.PullRequestReviewSnapshot(context.Background(), forge.FromGitHubInstallationID(1), RepoRef{Owner: "x", Name: "y"}, 3)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !snap.Truncated || f.calls != 1 {
		t.Errorf("truncated=%v calls=%d, want true/1", snap.Truncated, f.calls)
	}
}

func TestPullRequestReviewSnapshot_ErrorsEnvelopeIsValidation(t *testing.T) {
	c, f, _ := snapshotClient(t)
	f.pages = []string{`{"data":null,"errors":[{"message":"Could not resolve to a PullRequest"}]}`}
	_, err := c.PullRequestReviewSnapshot(context.Background(), forge.FromGitHubInstallationID(1), RepoRef{Owner: "x", Name: "y"}, 3)
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
}

func TestPullRequestReviewSnapshot_MissingPullRequestIsNotFound(t *testing.T) {
	c, f, _ := snapshotClient(t)
	f.pages = []string{`{"data":{"repository":{"isPrivate":false,"pullRequest":null}}}`}
	_, err := c.PullRequestReviewSnapshot(context.Background(), forge.FromGitHubInstallationID(1), RepoRef{Owner: "x", Name: "y"}, 3)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestPullRequestReviewSnapshot_HTTPStatusClassified(t *testing.T) {
	c, f, _ := snapshotClient(t)
	f.status = http.StatusForbidden
	f.pages = []string{`{"message":"nope"}`}
	_, err := c.PullRequestReviewSnapshot(context.Background(), forge.FromGitHubInstallationID(1), RepoRef{Owner: "x", Name: "y"}, 3)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
}

func TestPullRequestReviewSnapshot_RefusedBeforeRequest(t *testing.T) {
	cases := []struct {
		name   string
		scope  forge.CredentialScope
		repo   RepoRef
		number int
	}{
		{"zero scope", forge.CredentialScope{}, RepoRef{Owner: "x", Name: "y"}, 1},
		{"blank owner", forge.FromGitHubInstallationID(1), RepoRef{Name: "y"}, 1},
		{"blank name", forge.FromGitHubInstallationID(1), RepoRef{Owner: "x"}, 1},
		{"number 0", forge.FromGitHubInstallationID(1), RepoRef{Owner: "x", Name: "y"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, f, _ := snapshotClient(t)
			if _, err := c.PullRequestReviewSnapshot(context.Background(), tc.scope, tc.repo, tc.number); err == nil {
				t.Fatal("want refusal, got nil")
			}
			if f.calls != 0 {
				t.Errorf("requests = %d, want 0", f.calls)
			}
		})
	}
	c := &Client{}
	if _, err := c.PullRequestReviewSnapshot(context.Background(), forge.FromGitHubInstallationID(1), RepoRef{Owner: "x", Name: "y"}, 1); err == nil ||
		!strings.Contains(err.Error(), "TokenProvider") {
		t.Errorf("missing tokens: %v", err)
	}
}
