package gitlab

// This file implements the optional workmgmt.UserReportReader capability
// (E81.1 / #3771) on the GitLab provider. GitLab has no project-wide notes
// listing, so notes are found through their issue: every issue updated since
// the cursor is listed (gitlabclient's keyset walk), then its notes are listed
// and only those updated since the cursor are kept. Every gap this shape
// leaves is NAMED on the page as a degradation, never silent.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/gitlabclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// userReportLister is the OPTIONAL activity-read primitive set, declared as an
// extension of API rather than a member of it so the filing and campaign fakes
// do not grow stubs for a capability they never reach (the github provider's
// issueParentReader pattern). An API lacking it is refused with a typed
// ReasonNotImplemented. GetProject comes from API.
type userReportLister interface {
	ListIssuesUpdatedAfter(ctx context.Context, projectID int, after time.Time) ([]gitlabclient.UpdatedIssue, gitlabclient.ListingMeta, error)
	ListIssueNotes(ctx context.Context, projectID, iid int) ([]gitlabclient.Note, error)
	GetProjectMemberAccessLevel(ctx context.Context, projectID int, userID int64) (int, bool, error)
}

// Compile-time assertions: the production client satisfies the extension and
// the provider implements the capability.
var (
	_ userReportLister          = (*gitlabclient.Client)(nil)
	_ workmgmt.UserReportReader = (*Provider)(nil)
)

// developerAccessLevel is GitLab's Developer role (30), the lowest role that
// can push. Developer and above are internal; Reporter, Planner, Guest and
// Minimal Access are not, which roughly matches GitHub's write-capable trust.
const developerAccessLevel = 30

// accessTokenBotUsername is GitLab's project/group access-token bot username
// shape (project_<id>_bot_<suffix>, group_<id>_bot_<suffix>,
// https://docs.gitlab.com/user/project/settings/project_access_tokens/#bot-users-for-projects),
// the same pattern backend/internal/webhook/dispatcher.go's gitLabBotUsername
// matches. It is a HEURISTIC: any user can register such a username, so a
// match counts as a bot only when the member lookup corroborates it.
var accessTokenBotUsername = regexp.MustCompile(`^(project|group)_\d+_bot`)

// memberLookup is one author's resolved project membership.
type memberLookup struct {
	level    int
	member   bool
	resolved bool
}

// ListUserReports implements workmgmt.UserReportReader.
//
// It fails with a NIL page on: a nil API, a missing gitlab connection or
// project path (plain errors, File's posture); an API without the listings
// (ReasonNotImplemented); a 401/403 from the project resolve, the issue
// listing or a notes listing (ReasonForbidden, cause retained); a page cap hit
// inside one equal-timestamp run (workmgmt.ErrUserReportUnresumable); any other
// listing error (wrapped). A failed MEMBER lookup does not fail the page: that
// author is unresolved, not internal, and named association_unresolved.
//
// Classification evidence, per item:
//
//   - Bot: a system note (the note's own system flag), or an access-token-bot
//     username CORROBORATED by a member lookup that resolved the author as a
//     project member. A non-member, or a lookup error, with that username is
//     NOT a bot — the username alone is attacker-choosable.
//   - Internal: member access level >= Developer (30). Looked up ONCE per
//     distinct author id per scan; a 404 is a resolved non-member.
//
// Confidential issues (and with them their notes) and internal notes are
// EXCLUDED and counted under confidential_excluded. Every page ALWAYS names
// reactions_partial, comment_reactions_unavailable, comments_via_issue_activity
// and bot_detection_heuristic.
func (p *Provider) ListUserReports(ctx context.Context, req workmgmt.ListUserReportsRequest) (*workmgmt.UserReportPage, error) {
	if p.api == nil {
		return nil, errors.New("workmgmt/gitlab: provider missing API client")
	}
	conn := req.Target.GitLab
	if conn == nil {
		return nil, errors.New("workmgmt/gitlab: target gitlab connection required; the conventions must declare a gitlab block")
	}
	projectPath := resolveProjectPath(conn, req.Target.Repo)
	if projectPath == "" {
		return nil, errors.New("workmgmt/gitlab: no target project; set the gitlab.project override or supply a repo")
	}
	lister, ok := p.api.(userReportLister)
	if !ok {
		return nil, userReportUnavailable(workmgmt.ReasonNotImplemented,
			"the api client does not implement the issue-activity, notes and member-access reads", nil)
	}
	project, err := p.api.GetProject(ctx, projectPath)
	if err != nil {
		return nil, gitlabReadError(fmt.Sprintf("resolve project %q", projectPath), err)
	}
	issues, meta, err := lister.ListIssuesUpdatedAfter(ctx, project.ID, req.Since)
	if err != nil {
		return nil, gitlabReadError("list issues updated after", err)
	}

	members := map[int64]memberLookup{}
	author := func(id int64, username string) workmgmt.ReportAuthor {
		m, seen := members[id]
		if !seen {
			if id > 0 {
				level, member, lerr := lister.GetProjectMemberAccessLevel(ctx, project.ID, id)
				m = memberLookup{level: level, member: member, resolved: lerr == nil}
			}
			members[id] = m
		}
		a := workmgmt.ReportAuthor{Login: username}
		if !m.resolved {
			return a
		}
		a.AssociationResolved = true
		if m.member {
			a.Association = "access_level:" + strconv.Itoa(m.level)
			a.Internal = m.level >= developerAccessLevel
			a.Bot = accessTokenBotUsername.MatchString(username)
		} else {
			a.Association = "non_member"
		}
		return a
	}

	var items []workmgmt.UserReportItem
	// excluded is keyed by item identity so a boundary re-read of a
	// confidential issue counts once; notesListedAt skips re-listing the notes
	// of an issue re-read at the SAME updated_at (a keyset boundary re-read),
	// while an issue updated mid-scan, re-read at a NEWER updated_at, has its
	// notes listed again.
	excluded := map[string]bool{}
	notesListedAt := map[int]time.Time{}
	for _, is := range issues {
		if is.Confidential {
			excluded["issue/"+strconv.Itoa(is.IID)] = true
			continue
		}
		items = append(items, workmgmt.UserReportItem{
			Kind: workmgmt.UserReportKindIssue, IssueNumber: is.IID,
			Title: is.Title, Body: is.Description, URL: is.WebURL,
			Author: author(is.AuthorID, is.AuthorUsername),
			// GitLab's issue listing carries only thumbs up/down award
			// counts: a PARTIAL rollup, so Resolved stays false.
			Reactions: workmgmt.ReactionCounts{Total: is.Upvotes + is.Downvotes, PlusOne: is.Upvotes, MinusOne: is.Downvotes},
			CreatedAt: is.CreatedAt, UpdatedAt: is.UpdatedAt,
		})
		if at, listed := notesListedAt[is.IID]; listed && !is.UpdatedAt.After(at) {
			continue
		}
		notesListedAt[is.IID] = is.UpdatedAt
		notes, err := lister.ListIssueNotes(ctx, project.ID, is.IID)
		if err != nil {
			return nil, gitlabReadError(fmt.Sprintf("list notes of issue #%d", is.IID), err)
		}
		for _, n := range notes {
			if n.Internal {
				excluded["note/"+strconv.FormatInt(n.ID, 10)] = true
				continue
			}
			created, updated, err := noteTimes(n)
			if err != nil {
				return nil, fmt.Errorf("workmgmt/gitlab: issue #%d: %w", is.IID, err)
			}
			if updated.Before(req.Since) {
				continue
			}
			a := author(n.AuthorID, n.Author)
			if n.System {
				a.Bot = true
			}
			items = append(items, workmgmt.UserReportItem{
				Kind: workmgmt.UserReportKindComment, IssueNumber: is.IID, CommentID: n.ID,
				Body: n.Body, URL: fmt.Sprintf("%s#note_%d", is.WebURL, n.ID),
				Author:    a,
				CreatedAt: created, UpdatedAt: updated,
			})
		}
	}
	items = workmgmt.DedupeUserReportItems(items)

	next, degs := workmgmt.NextUserReportCursor(req.Since, meta.Date,
		workmgmt.UserReportListingEnd{Truncated: meta.Truncated, ResumeAt: meta.ResumeAt})
	var unresolved, issueItems, noteItems int
	for _, it := range items {
		if !it.Author.AssociationResolved {
			unresolved++
		}
		if it.Kind == workmgmt.UserReportKindIssue {
			issueItems++
		} else {
			noteItems++
		}
	}
	if unresolved > 0 {
		degs = append(degs, workmgmt.UserReportDegradation{
			Code:   workmgmt.UserReportAssociationUnresolved,
			Detail: "the project-member lookup failed (or the author id was absent) for these items' authors; they are treated as neither internal nor bot",
			Count:  unresolved,
		})
	}
	if len(excluded) > 0 {
		degs = append(degs, workmgmt.UserReportDegradation{
			Code:   workmgmt.UserReportConfidentialExcluded,
			Detail: "confidential issues (with their notes) and internal notes were excluded from the page; they are not user reports to act on in the open",
			Count:  len(excluded),
		})
	}
	degs = append(degs,
		workmgmt.UserReportDegradation{
			Code:   workmgmt.UserReportReactionsPartial,
			Detail: "GitLab's issue listing carries only thumbs up/down award counts; other award emoji are not counted",
			Count:  issueItems,
		},
		workmgmt.UserReportDegradation{
			Code:   workmgmt.UserReportCommentReactionsUnavailable,
			Detail: "GitLab's notes listing carries no award emoji; comment reaction counts are unknown",
			Count:  noteItems,
		},
		workmgmt.UserReportDegradation{
			Code:   workmgmt.UserReportCommentsViaIssueActivity,
			Detail: "GitLab has no project-wide notes listing; notes are found through their issue's updated_at, relying on GitLab touching the issue when a note is saved",
		},
		workmgmt.UserReportDegradation{
			Code:   workmgmt.UserReportBotDetectionHeuristic,
			Detail: "GitLab bots are detected from system notes and from access-token-bot usernames corroborated as project members; other service accounts classify by membership",
		},
	)
	return &workmgmt.UserReportPage{
		Forge:        workmgmt.UserReportForgeGitLab,
		Items:        items,
		Since:        req.Since,
		NextCursor:   next,
		Degradations: degs,
	}, nil
}

// noteTimes parses a note's created_at and its updated_at, falling back to
// created_at when updated_at is absent. A malformed timestamp is an error:
// the note cannot be placed against the cursor.
func noteTimes(n gitlabclient.Note) (time.Time, time.Time, error) {
	created, err := time.Parse(time.RFC3339, n.CreatedAt)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("note %d created_at %q: %w", n.ID, n.CreatedAt, err)
	}
	if n.UpdatedAt == "" {
		return created, created, nil
	}
	updated, err := time.Parse(time.RFC3339, n.UpdatedAt)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("note %d updated_at %q: %w", n.ID, n.UpdatedAt, err)
	}
	return created, updated, nil
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

// gitlabReadError maps one read's failure: a 401/403 *APIError is a typed
// ReasonForbidden, a cap hit inside one equal-timestamp run is the
// errors.Is-matchable workmgmt.ErrUserReportUnresumable (the client sentinel
// stays matchable too), and anything else is wrapped.
func gitlabReadError(what string, err error) error {
	var apiErr *gitlabclient.APIError
	switch {
	case errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusForbidden || apiErr.StatusCode == http.StatusUnauthorized):
		return userReportUnavailable(workmgmt.ReasonForbidden,
			"GitLab refused the read ("+what+"); the token needs read access to the project's issues and members", err)
	case errors.Is(err, gitlabclient.ErrEqualTimestampRunExceedsCap):
		return fmt.Errorf("workmgmt/gitlab: %s: %w: %w", what, workmgmt.ErrUserReportUnresumable, err)
	default:
		return fmt.Errorf("workmgmt/gitlab: %s: %w", what, err)
	}
}
