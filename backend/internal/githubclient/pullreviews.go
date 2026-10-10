package githubclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// pullReviewsPerPage is GitHub's per_page ceiling for the reviews listing.
const pullReviewsPerPage = 100

// pullReviewsMaxPages bounds the reviews walk (#4086): 10 pages of 100 is far
// past any real pull request's review count, and a listing that still
// advertises a next page at the cap is reported truncated, not silently cut.
const pullReviewsMaxPages = 10

// PullRequestReview is one review as the pull request reviews listing reports
// it, reduced to what the merge-readiness approval check reads (#4086).
type PullRequestReview struct {
	ID int64
	// UserLogin is the reviewer's login; "" when GitHub reports a null user
	// (a deleted account).
	UserLogin string
	// State is GitHub's value verbatim: APPROVED, CHANGES_REQUESTED,
	// COMMENTED, DISMISSED or PENDING. A dismissed review reports DISMISSED
	// and no longer carries the state it had before the dismissal.
	State string
	// CommitID is the head commit the review was submitted against.
	CommitID string
	// SubmittedAt is the zero time when GitHub reports submitted_at null or
	// absent (a PENDING review).
	SubmittedAt time.Time
}

// ListPullRequestReviews lists a pull request's reviews in the order GitHub
// returns them (chronological) — the read under the merge-readiness
// approval_dismissed check (#4086).
//
//	GET /repos/{owner}/{repo}/pulls/{number}/reviews?per_page=100&page=N
//
// Page URLs are built from the client's own base; the Link header is read
// only for whether a rel="next" page exists (the openpulls.go posture), so a
// response header never redirects the installation token. truncated is true
// when the walk stops with reviews remaining — the pullReviewsMaxPages cap was
// reached with a next page still advertised, or a page came back empty while
// advertising a next page — and a caller must then treat the listing as
// incomplete. A zero scope, a missing TokenProvider, a blank owner/name or a
// number <= 0 is refused before any request. Returns ErrNotFound when the repo
// or pull request isn't visible, ErrForbidden on auth issues, ErrValidation
// on a 422; any other non-2xx is an untyped status error via classifyStatus.
func (c *Client) ListPullRequestReviews(ctx context.Context, scope forge.CredentialScope, repo RepoRef, number int) ([]PullRequestReview, bool, error) {
	installationID, err := installationIDForScope(scope)
	if err != nil {
		return nil, false, err
	}
	if c.Tokens == nil {
		return nil, false, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return nil, false, errors.New("githubclient: repo owner and name required")
	}
	if number <= 0 {
		return nil, false, errors.New("githubclient: pr number must be > 0")
	}

	base := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/pulls/" + url.PathEscape(strconv.Itoa(number)) + "/reviews")
	out := []PullRequestReview{}
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("per_page", strconv.Itoa(pullReviewsPerPage))
		q.Set("page", strconv.Itoa(page))
		req, err := c.buildRequest(ctx, http.MethodGet, base+"?"+q.Encode(), nil, installationID)
		if err != nil {
			return nil, false, err
		}
		reviews, hasNext, err := c.doPullReviewsPage(req)
		if err != nil {
			return nil, false, err
		}
		out = append(out, reviews...)
		if !hasNext {
			return out, false, nil
		}
		if page == pullReviewsMaxPages {
			return out, true, nil
		}
		if len(reviews) == 0 {
			// A next page advertised after an empty one: the walk cannot
			// make progress, so stop rather than loop.
			return out, true, nil
		}
	}
}

// doPullReviewsPage executes one reviews page request and reports whether the
// Link header advertises a next page.
func (c *Client) doPullReviewsPage(req *http.Request) ([]PullRequestReview, bool, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("githubclient: list pr reviews: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := classifyStatus("list pr reviews", resp); err != nil {
		return nil, false, err
	}
	var body []struct {
		ID   int64 `json:"id"`
		User *struct {
			Login string `json:"login"`
		} `json:"user"`
		State       string     `json:"state"`
		CommitID    string     `json:"commit_id"`
		SubmittedAt *time.Time `json:"submitted_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, false, fmt.Errorf("githubclient: decode pr reviews: %w", err)
	}
	reviews := make([]PullRequestReview, 0, len(body))
	for _, r := range body {
		rv := PullRequestReview{
			ID:       r.ID,
			State:    r.State,
			CommitID: r.CommitID,
		}
		if r.User != nil {
			rv.UserLogin = r.User.Login
		}
		if r.SubmittedAt != nil {
			rv.SubmittedAt = *r.SubmittedAt
		}
		reviews = append(reviews, rv)
	}
	return reviews, nextPageURL(resp.Header.Get("Link")) != "", nil
}
