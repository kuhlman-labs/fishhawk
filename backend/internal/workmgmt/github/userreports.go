package github

// This file implements the optional workmgmt.UserReportReader capability
// (E81.1 / #3771) on the GitHub provider: list every issue and issue comment
// updated since a cursor through githubclient's two keyset activity listings,
// and map them into provider-neutral items with author, association and
// reactions. Pull requests are excluded (the issues listing skips PR nodes;
// PR-conversation comments are dropped here).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// userReportLister is the OPTIONAL activity-listing primitive pair, declared
// as an extension of API rather than a member of it — the issueParentReader
// pattern (reader.go), for its stated reason: promoting the two listings into
// API would force every filing and board-sync fake in sibling packages to grow
// stubs for a capability they never reach. An API lacking it is refused with a
// typed ReasonNotImplemented.
type userReportLister interface {
	ListIssuesUpdatedSince(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, since time.Time) ([]githubclient.UpdatedIssue, githubclient.ListingMeta, error)
	ListIssueCommentsUpdatedSince(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, since time.Time) ([]githubclient.UpdatedIssueComment, githubclient.ListingMeta, error)
}

// Compile-time assertions: the production client satisfies the extension (a
// signature drift fails the BUILD rather than silently falling through to
// ReasonNotImplemented), and the provider implements the capability.
var (
	_ userReportLister          = (*githubclient.Client)(nil)
	_ workmgmt.UserReportReader = (*Provider)(nil)
)

// internalAssociations is GitHub's write-capable-trust set: the repository
// owner, an organization member, and an invited collaborator. CONTRIBUTOR,
// FIRST_TIME_CONTRIBUTOR, FIRST_TIMER, MANNEQUIN and NONE are external.
var internalAssociations = map[string]bool{
	"OWNER":        true,
	"MEMBER":       true,
	"COLLABORATOR": true,
}

// isGitHubBot is the forge-evidenced bot rule: user.type "Bot" (GitHub Apps
// and their [bot] accounts) OR a "[bot]" login suffix — the same suffix rule
// the prompt's bot filter applies, kept as an independent arm so an account
// whose type is reported as "User" but whose login carries the suffix still
// classifies as a bot.
func isGitHubBot(userType, login string) bool {
	return strings.EqualFold(userType, "Bot") || strings.HasSuffix(strings.ToLower(login), "[bot]")
}

// userReportUnavailable builds the typed capability-unavailable error for the
// user-report capability.
func userReportUnavailable(reason workmgmt.UnavailableReason, detail string, cause error) *workmgmt.UnavailableError {
	return &workmgmt.UnavailableError{
		Provider:   ProviderName,
		Capability: workmgmt.UserReportCapability,
		Reason:     reason,
		Detail:     detail,
		Cause:      cause,
	}
}

// ListUserReports implements workmgmt.UserReportReader.
//
// It FAILS CLOSED with a NIL page on:
//
//   - a zero credential scope        -> ReasonNoInstallation (the shared preflight)
//   - an API without the listings    -> ReasonNotImplemented
//   - a forge permission refusal     -> ReasonForbidden (cause retained)
//   - a page cap hit inside one equal-timestamp run at Since
//     -> workmgmt.ErrUserReportUnresumable (wrapping the client sentinel)
//   - any other listing error        -> wrapped
//
// Otherwise it maps issues then comments (dropping PR-conversation comments),
// dedupes and sorts through workmgmt.DedupeUserReportItems, names
// association_unresolved / reactions_partial with counts, and anchors
// NextCursor on the ISSUES listing's first-page Date — the earliest request of
// the scan — pulled back to any truncated listing's resume point.
func (p *Provider) ListUserReports(ctx context.Context, req workmgmt.ListUserReportsRequest) (*workmgmt.UserReportPage, error) {
	repo, err := p.preflight(req.Target)
	if err != nil {
		var ue *workmgmt.UnavailableError
		if errors.As(err, &ue) {
			ue.Capability = workmgmt.UserReportCapability
		}
		return nil, err
	}
	lister, ok := p.api.(userReportLister)
	if !ok {
		return nil, userReportUnavailable(workmgmt.ReasonNotImplemented,
			"the api client does not implement the issue and issue-comment activity listings", nil)
	}
	issues, issuesMeta, err := lister.ListIssuesUpdatedSince(ctx, req.Target.Scope, repo, req.Since)
	if err != nil {
		return nil, listingError("issues", err)
	}
	comments, commentsMeta, err := lister.ListIssueCommentsUpdatedSince(ctx, req.Target.Scope, repo, req.Since)
	if err != nil {
		return nil, listingError("issue comments", err)
	}

	items := make([]workmgmt.UserReportItem, 0, len(issues)+len(comments))
	for _, is := range issues {
		items = append(items, workmgmt.UserReportItem{
			Kind: workmgmt.UserReportKindIssue, IssueNumber: is.Number,
			Title: is.Title, Body: is.Body, URL: is.HTMLURL,
			Author:    githubAuthor(is.AuthorLogin, is.AuthorType, is.AuthorAssociation, is.AssociationPresent),
			Reactions: githubReactions(is.Reactions, is.ReactionsPresent),
			CreatedAt: is.CreatedAt, UpdatedAt: is.UpdatedAt,
		})
	}
	for _, c := range comments {
		if c.OnPullRequest {
			continue
		}
		items = append(items, workmgmt.UserReportItem{
			Kind: workmgmt.UserReportKindComment, IssueNumber: c.IssueNumber, CommentID: c.ID,
			Body: c.Body, URL: c.HTMLURL,
			Author:    githubAuthor(c.AuthorLogin, c.AuthorType, c.AuthorAssociation, c.AssociationPresent),
			Reactions: githubReactions(c.Reactions, c.ReactionsPresent),
			CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
		})
	}
	items = workmgmt.DedupeUserReportItems(items)

	next, degs := workmgmt.NextUserReportCursor(req.Since, issuesMeta.Date,
		workmgmt.UserReportListingEnd{Truncated: issuesMeta.Truncated, ResumeAt: issuesMeta.ResumeAt},
		workmgmt.UserReportListingEnd{Truncated: commentsMeta.Truncated, ResumeAt: commentsMeta.ResumeAt},
	)
	var unresolvedAssoc, partialReactions int
	for _, it := range items {
		if !it.Author.AssociationResolved {
			unresolvedAssoc++
		}
		if !it.Reactions.Resolved {
			partialReactions++
		}
	}
	if unresolvedAssoc > 0 {
		degs = append(degs, workmgmt.UserReportDegradation{
			Code:   workmgmt.UserReportAssociationUnresolved,
			Detail: "GitHub sent no author_association for these items; their authors are treated as not internal",
			Count:  unresolvedAssoc,
		})
	}
	if partialReactions > 0 {
		degs = append(degs, workmgmt.UserReportDegradation{
			Code:   workmgmt.UserReportReactionsPartial,
			Detail: "GitHub sent no reactions rollup for these items; their reaction counts are unknown, not zero",
			Count:  partialReactions,
		})
	}
	return &workmgmt.UserReportPage{
		Forge:        workmgmt.UserReportForgeGitHub,
		Items:        items,
		Since:        req.Since,
		NextCursor:   next,
		Degradations: degs,
	}, nil
}

// listingError maps one listing's failure: a permission refusal is a typed
// ReasonForbidden, a cap hit inside one equal-timestamp run is the
// errors.Is-matchable workmgmt.ErrUserReportUnresumable (the client sentinel
// stays matchable too), and anything else is wrapped.
func listingError(what string, err error) error {
	switch {
	case errors.Is(err, forge.ErrForbidden):
		return userReportUnavailable(workmgmt.ReasonForbidden,
			"the forge refused the "+what+" activity listing; the token needs read access to the repository's issues", err)
	case errors.Is(err, githubclient.ErrEqualTimestampRunExceedsCap):
		return fmt.Errorf("workmgmt/github: list %s: %w: %w", what, workmgmt.ErrUserReportUnresumable, err)
	default:
		return fmt.Errorf("workmgmt/github: list %s: %w", what, err)
	}
}

func githubAuthor(login, userType, association string, present bool) workmgmt.ReportAuthor {
	a := workmgmt.ReportAuthor{Login: login, Bot: isGitHubBot(userType, login)}
	if present {
		a.Association, a.AssociationResolved = association, true
		a.Internal = internalAssociations[association]
	}
	return a
}

func githubReactions(r githubclient.ReactionRollup, present bool) workmgmt.ReactionCounts {
	if !present {
		return workmgmt.ReactionCounts{}
	}
	return workmgmt.ReactionCounts{
		Total: r.TotalCount, PlusOne: r.PlusOne, MinusOne: r.MinusOne, Laugh: r.Laugh,
		Hooray: r.Hooray, Confused: r.Confused, Heart: r.Heart, Rocket: r.Rocket, Eyes: r.Eyes,
		Resolved: true,
	}
}
