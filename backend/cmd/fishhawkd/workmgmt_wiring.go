package main

import (
	"context"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/jiraclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
	workmgmtgithub "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt/github"
	workmgmtjira "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt/jira"
)

// registerWorkmgmtProviders wires the work-management providers into the
// global workmgmt registries at startup. Without this, the registries are
// empty in production — the only registrations happen in tests against
// fakes — so fishhawk_file_issue and fishhawk_report_product_issue return
// 501 provider_unimplemented even though the providers exist (#1104; same
// tests-green/production-broken class as #735).
//
// Each provider is gated on its own client being configured, independently:
// an unconfigured GitHub client (no App id/key) leaves the github_projects
// + feedback registrations off, and an unconfigured Jira client (no
// FISHHAWKD_JIRA_* env) leaves the jira registration off — in either case
// the affected endpoint/provider continues to return 501, the intended v0
// not-yet-wired posture.
func registerWorkmgmtProviders(gh *githubclient.Client, jira *jiraclient.Client) {
	if gh != nil {
		// *githubclient.Client satisfies the work-item API directly (all six
		// methods exist with matching signatures), so no adapter is needed.
		workmgmt.Register(workmgmtgithub.New(gh))
		// The feedback provider needs an adapter: FeedbackAPI.SearchOpenIssues
		// returns []workmgmtgithub.MatchedIssue, a workmgmt/github type the
		// client cannot return without an import cycle.
		workmgmt.RegisterFeedback(workmgmtgithub.NewFeedback(feedbackAPIAdapter{gh}))
	}
	if jira != nil {
		// *jiraclient.Client satisfies the jira work-item API directly
		// (CreateIssue + Transition), so no adapter is needed.
		workmgmt.Register(workmgmtjira.New(jira))
	}
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

func (a feedbackAPIAdapter) SearchOpenIssuesScoped(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, query string) ([]workmgmtgithub.MatchedIssue, error) {
	// The provider embeds repo:owner/name into the query string, so the
	// client search method needs only the composed query — repo is unused
	// here, kept to satisfy the consumer-side interface.
	_ = repo
	res, err := a.client.SearchOpenIssuesScoped(ctx, scope, query)
	if err != nil {
		return nil, err
	}
	matches := make([]workmgmtgithub.MatchedIssue, 0, len(res))
	for _, r := range res {
		matches = append(matches, workmgmtgithub.MatchedIssue{Number: r.Number, URL: r.HTMLURL, Body: r.Body})
	}
	return matches, nil
}

func (a feedbackAPIAdapter) CreateIssueScoped(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, p githubclient.CreateIssueParams) (*githubclient.CreatedIssue, error) {
	return a.client.CreateIssueScoped(ctx, scope, repo, p)
}

func (a feedbackAPIAdapter) CreateIssueCommentScoped(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, issueNumber int, body string) (*githubclient.IssueComment, error) {
	return a.client.CreateIssueCommentScoped(ctx, scope, repo, issueNumber, body)
}
