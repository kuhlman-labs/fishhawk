package main

import (
	"context"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
	workmgmtgithub "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt/github"
)

// registerWorkmgmtProviders wires the GitHub Projects work-item and
// product-feedback providers into the global workmgmt registries at
// startup. Without this, both registries are empty in production — the
// only registrations happen in tests against fakes — so fishhawk_file_issue
// and fishhawk_report_product_issue return 501 provider_unimplemented even
// though the providers exist (#1104; same tests-green/production-broken
// class as #735).
//
// gated on gh != nil: an unconfigured GitHub client (no App id/key) leaves
// both registries unregistered and the two endpoints continue to return
// 501 — the intended v0 not-yet-wired posture, matching the surrounding
// cfg.GitHub-gated wiring (role resolver, webhook dispatcher).
func registerWorkmgmtProviders(gh *githubclient.Client) {
	if gh == nil {
		return
	}
	// *githubclient.Client satisfies the work-item API directly (all six
	// methods exist with matching signatures), so no adapter is needed.
	workmgmt.Register(workmgmtgithub.New(gh))
	// The feedback provider needs an adapter: FeedbackAPI.SearchOpenIssues
	// returns []workmgmtgithub.MatchedIssue, a workmgmt/github type the
	// client cannot return without an import cycle.
	workmgmt.RegisterFeedback(workmgmtgithub.NewFeedback(feedbackAPIAdapter{gh}))
}

// feedbackAPIAdapter adapts *githubclient.Client to the feedback
// provider's FeedbackAPI: CreateIssue and CreateIssueComment delegate
// straight through, and SearchOpenIssues maps the client's
// []githubclient.IssueSearchResult to the []workmgmtgithub.MatchedIssue the
// provider consumes.
type feedbackAPIAdapter struct {
	client *githubclient.Client
}

var _ workmgmtgithub.FeedbackAPI = feedbackAPIAdapter{}

func (a feedbackAPIAdapter) SearchOpenIssues(ctx context.Context, installationID int64, repo githubclient.RepoRef, query string) ([]workmgmtgithub.MatchedIssue, error) {
	// The provider embeds repo:owner/name into the query string, so the
	// client search method needs only the composed query — repo is unused
	// here, kept to satisfy the consumer-side interface.
	_ = repo
	res, err := a.client.SearchOpenIssues(ctx, installationID, query)
	if err != nil {
		return nil, err
	}
	matches := make([]workmgmtgithub.MatchedIssue, 0, len(res))
	for _, r := range res {
		matches = append(matches, workmgmtgithub.MatchedIssue{Number: r.Number, URL: r.HTMLURL, Body: r.Body})
	}
	return matches, nil
}

func (a feedbackAPIAdapter) CreateIssue(ctx context.Context, installationID int64, repo githubclient.RepoRef, p githubclient.CreateIssueParams) (*githubclient.CreatedIssue, error) {
	return a.client.CreateIssue(ctx, installationID, repo, p)
}

func (a feedbackAPIAdapter) CreateIssueComment(ctx context.Context, installationID int64, repo githubclient.RepoRef, issueNumber int, body string) (*githubclient.IssueComment, error) {
	return a.client.CreateIssueComment(ctx, installationID, repo, issueNumber, body)
}
