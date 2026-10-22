package githubclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// reviewSnapshotPerPage is the latestOpinionatedReviews page size: GitHub's
// GraphQL connection ceiling.
const reviewSnapshotPerPage = 100

// reviewSnapshotMaxPages bounds the snapshot walk (ADR-112): three pages of
// 100 reviewer-latest opinions is far past any real pull request, and a
// listing still advertising a next page at the cap is reported Truncated,
// never silently cut.
const reviewSnapshotMaxPages = 3

// reviewSnapshotQuery is the ONE GraphQL read the ADR-112 forge-approval fold
// takes its approvals and vetoes from. latestOpinionatedReviews already folds
// each reviewer to their latest APPROVED / CHANGES_REQUESTED (and, on some
// GitHub versions, DISMISSED) review; writersOnly asks GitHub to drop
// reviewers without write access, which the fold does NOT rely on — it
// re-reads every candidate's permission itself (R1(c)).
const reviewSnapshotQuery = `query($owner: String!, $name: String!, $number: Int!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    isPrivate
    pullRequest(number: $number) {
      merged
      state
      headRefOid
      reviewDecision
      mergeStateStatus
      author { __typename login ... on User { databaseId } }
      latestOpinionatedReviews(first: 100, writersOnly: true, after: $cursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          databaseId
          state
          submittedAt
          commit { oid }
          author { __typename login ... on User { databaseId } }
        }
      }
    }
  }
}`

// ReviewActor is a GraphQL Actor as the snapshot reports it. Typename is the
// concrete GraphQL type ("User", "Bot", "Mannequin", ...); DatabaseID is set
// only for a User (0 otherwise). A null actor (a deleted account) decodes to
// the zero value with Present false.
type ReviewActor struct {
	Present    bool
	Typename   string
	Login      string
	DatabaseID int64
}

// SnapshotReview is one reviewer-latest opinionated review.
type SnapshotReview struct {
	// DatabaseID is the review's own database id (the fold's tie-breaker).
	DatabaseID int64
	// State is the GraphQL PullRequestReviewState verbatim: APPROVED,
	// CHANGES_REQUESTED, DISMISSED, COMMENTED or PENDING.
	State string
	// SubmittedAt is the zero time when GitHub reports it null.
	SubmittedAt time.Time
	// CommitOID is the head commit the review was submitted against; "" when
	// GitHub reports the commit null (a force-pushed-away commit).
	CommitOID string
	Author    ReviewActor
}

// ReviewSnapshot is the typed result of PullRequestReviewSnapshot.
type ReviewSnapshot struct {
	IsPrivate bool
	Merged    bool
	// State is the GraphQL PullRequestState verbatim (OPEN, CLOSED, MERGED).
	State string
	// HeadOID is the pull request's live headRefOid.
	HeadOID string
	// ReviewDecision is the GraphQL PullRequestReviewDecision verbatim
	// (APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED), "" when GitHub reports
	// null — which it does when the base branch requires no review, so ""
	// is NOT "not approved".
	ReviewDecision string
	// MergeStateStatus is the GraphQL MergeStateStatus verbatim (BEHIND,
	// BLOCKED, CLEAN, DIRTY, DRAFT, HAS_HOOKS, UNKNOWN, UNSTABLE), "" for null.
	MergeStateStatus string
	// Author is the pull request's opener.
	Author  ReviewActor
	Reviews []SnapshotReview
	// Truncated is true when the walk stopped with reviews remaining: the
	// reviewSnapshotMaxPages cap was reached with a next page still
	// advertised. A caller must treat a truncated snapshot as incomplete.
	Truncated bool
}

type graphQLActor struct {
	Typename   string `json:"__typename"`
	Login      string `json:"login"`
	DatabaseID int64  `json:"databaseId"`
}

func (a *graphQLActor) toActor() ReviewActor {
	if a == nil {
		return ReviewActor{}
	}
	return ReviewActor{Present: true, Typename: a.Typename, Login: a.Login, DatabaseID: a.DatabaseID}
}

// PullRequestReviewSnapshot reads, in ONE GraphQL query per page, everything
// the ADR-112 forge-approval fold decides from: the reviewer-latest
// opinionated reviews (state, commit, submittedAt, author identity), the
// pull request's reviewDecision, mergeStateStatus, headRefOid, merged flag
// and opener, and the repository's visibility.
//
// It authenticates with the RUN's installation token (scope), never the
// boot-singleton identity. Pages are bounded by reviewSnapshotMaxPages; a
// listing still advertising a next page at the cap returns Truncated=true.
// The pull-request fields are taken from the first page. A zero scope, a
// missing TokenProvider, a blank owner/name or a number <= 0 is refused
// before any request. A GraphQL `errors` envelope is ErrValidation (via
// doGraphQL); a pull request or repository the installation cannot see is
// ErrNotFound; HTTP statuses classify as for every other read.
func (c *Client) PullRequestReviewSnapshot(ctx context.Context, scope forge.CredentialScope, repo RepoRef, number int) (*ReviewSnapshot, error) {
	installationID, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	if c.Tokens == nil {
		return nil, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return nil, errors.New("githubclient: repo owner and name required")
	}
	if number <= 0 {
		return nil, errors.New("githubclient: pr number must be > 0")
	}

	type reviewNode struct {
		DatabaseID  int64      `json:"databaseId"`
		State       string     `json:"state"`
		SubmittedAt *time.Time `json:"submittedAt"`
		Commit      *struct {
			OID string `json:"oid"`
		} `json:"commit"`
		Author *graphQLActor `json:"author"`
	}
	var data struct {
		Repository *struct {
			IsPrivate   bool `json:"isPrivate"`
			PullRequest *struct {
				Merged           bool          `json:"merged"`
				State            string        `json:"state"`
				HeadRefOid       string        `json:"headRefOid"`
				ReviewDecision   *string       `json:"reviewDecision"`
				MergeStateStatus *string       `json:"mergeStateStatus"`
				Author           *graphQLActor `json:"author"`
				Reviews          struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []reviewNode `json:"nodes"`
				} `json:"latestOpinionatedReviews"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}

	out := &ReviewSnapshot{Reviews: []SnapshotReview{}}
	var cursor any
	for page := 1; ; page++ {
		data.Repository = nil
		if err := c.doGraphQL(ctx, installationID, reviewSnapshotQuery, map[string]any{
			"owner":  repo.Owner,
			"name":   repo.Name,
			"number": number,
			"cursor": cursor,
		}, &data); err != nil {
			return nil, fmt.Errorf("githubclient: review snapshot: %w", err)
		}
		if data.Repository == nil || data.Repository.PullRequest == nil {
			return nil, fmt.Errorf("%w: review snapshot: pull request %s/%s#%d not visible", ErrNotFound, repo.Owner, repo.Name, number)
		}
		pr := data.Repository.PullRequest
		if page == 1 {
			out.IsPrivate = data.Repository.IsPrivate
			out.Merged = pr.Merged
			out.State = pr.State
			out.HeadOID = pr.HeadRefOid
			if pr.ReviewDecision != nil {
				out.ReviewDecision = *pr.ReviewDecision
			}
			if pr.MergeStateStatus != nil {
				out.MergeStateStatus = *pr.MergeStateStatus
			}
			out.Author = pr.Author.toActor()
		}
		for _, n := range pr.Reviews.Nodes {
			r := SnapshotReview{DatabaseID: n.DatabaseID, State: n.State, Author: n.Author.toActor()}
			if n.SubmittedAt != nil {
				r.SubmittedAt = *n.SubmittedAt
			}
			if n.Commit != nil {
				r.CommitOID = n.Commit.OID
			}
			out.Reviews = append(out.Reviews, r)
		}
		if !pr.Reviews.PageInfo.HasNextPage {
			return out, nil
		}
		if page >= reviewSnapshotMaxPages || pr.Reviews.PageInfo.EndCursor == "" {
			out.Truncated = true
			return out, nil
		}
		cursor = pr.Reviews.PageInfo.EndCursor
	}
}
