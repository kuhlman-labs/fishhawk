package workmgmt

// This file carries the USER-REPORT read capability (E81.1 / #3771): the
// optional UserReportReader a forge provider implements to list every issue
// and issue comment created or updated since a cursor, the provider-neutral
// item / page / degradation vocabulary it answers in, the UserReportReaderFor
// chokepoint, and the two rules both providers share — the cursor rule
// (NextUserReportCursor) and the dedupe-and-sort rule (DedupeUserReportItems).
//
// It follows the ADR-064 WorkItemReader pattern (reader.go): a SEPARATE
// capability interface, resolved through a chokepoint that returns a typed
// *UnavailableError for a provider lacking it, and a NIL page on every
// degradation that prevents an honest answer. Gaps that still allow an honest
// answer (a forge that cannot report comment reactions, a member lookup that
// failed for one author) are not errors: they are NAMED on the page as
// UserReportDegradation entries, never silent.
//
// NOTHING IN PRODUCTION CALLS THIS YET. backend/internal/userreport's Scan is
// the one consumer, and E81.5 (#3775) wires Scan into the comms stage.

import (
	"context"
	"errors"
	"sort"
	"time"
)

// UserReportReader is the optional user-report READ capability (#3771): list
// every issue and issue comment in the target repository created or updated at
// or after req.Since.
//
// It FAILS CLOSED like WorkItemReader: a provider that cannot read the
// activity — no installation scope, a forge permission refusal, a page cap hit
// inside one equal-timestamp run — returns a NIL page and an error, never an
// empty page a caller would read as "nobody reported anything". Gaps that leave
// the page honest are named in UserReportPage.Degradations instead.
type UserReportReader interface {
	ListUserReports(ctx context.Context, req ListUserReportsRequest) (*UserReportPage, error)
}

// ListUserReportsRequest is the resolved input to ListUserReports: the Target
// (repo + credential scope + optional gitlab connection) and the inclusive
// lower bound. A zero Since lists the repository's whole history; callers
// (userreport.Scan) always pass a persisted cursor.
type ListUserReportsRequest struct {
	Target Target
	Since  time.Time
}

// UserReportKind is the closed set of item kinds a user-report page carries.
type UserReportKind string

const (
	// UserReportKindIssue is an issue (never a pull/merge request).
	UserReportKindIssue UserReportKind = "issue"
	// UserReportKindComment is a comment on an issue (a GitHub issue comment
	// on an issue conversation, or a GitLab issue note).
	UserReportKindComment UserReportKind = "comment"
)

// UserReportItem is one created-or-updated issue or comment in provider-neutral
// vocabulary.
//
// IssueNumber is the issue's number (GitHub) or iid (GitLab) for both kinds;
// CommentID is the comment's forge id and is 0 for an issue. Title is empty
// for a comment. URL is the browse URL of the item itself.
//
// Reactions is a SNAPSHOT: adding a reaction does not bump the item's
// updated_at on either forge, so an item is only re-listed when its text (or,
// for an issue, its state, labels or a note) changes. The counts reflect the
// item as of the last update a scan observed, never reactions added since —
// a consumer ranking by reactions must read them as a lower bound.
type UserReportItem struct {
	Kind        UserReportKind
	IssueNumber int
	CommentID   int64
	Title       string
	Body        string
	URL         string
	Author      ReportAuthor
	Reactions   ReactionCounts
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ReportAuthor is an item author as the forge evidences them.
//
// Bot is FORGE-EVIDENCED only (GitHub: user.type "Bot" or a "[bot]" login
// suffix; GitLab: a system note, or an access-token-bot username CORROBORATED
// by a project-member lookup). Association is the forge's raw relationship
// string (GitHub author_association such as "OWNER"; GitLab "access_level:30"
// or "non_member"), empty when unresolved. AssociationResolved is false when
// the forge did not say (an absent author_association, a failed member
// lookup), and an unresolved association is NEVER Internal — the safe
// direction for a classifier that trusts internal authors. Internal is the
// provider's own write-capable-trust rule (GitHub OWNER/MEMBER/COLLABORATOR,
// GitLab access level Developer (30) and above).
type ReportAuthor struct {
	Login               string
	Bot                 bool
	Association         string
	AssociationResolved bool
	Internal            bool
}

// ReactionCounts is an item's reaction counts. Resolved is false when the
// forge did not report a full rollup (GitHub omitted the reactions object;
// GitLab carries only thumbs up/down on issues and nothing on notes), so zero
// counts under Resolved == false mean "unknown", never "none".
type ReactionCounts struct {
	Total    int
	PlusOne  int
	MinusOne int
	Laugh    int
	Hooray   int
	Confused int
	Heart    int
	Rocket   int
	Eyes     int
	Resolved bool
}

// UserReportPage is the ListUserReports result.
//
// Forge names the forge the items came from ("github" | "gitlab") so a
// classifier can match a provider-qualified captain subject against it.
// Since echoes the request bound. NextCursor is the bound the caller should
// persist once it has durably recorded the page (see NextUserReportCursor); it
// never falls below Since. Items are deduplicated and deterministically sorted
// (DedupeUserReportItems). Degradations names every gap in the page.
type UserReportPage struct {
	Forge        string
	Items        []UserReportItem
	Since        time.Time
	NextCursor   time.Time
	Degradations []UserReportDegradation
}

// Forge identifiers UserReportPage.Forge carries.
const (
	UserReportForgeGitHub = "github"
	UserReportForgeGitLab = "gitlab"
)

// UserReportDegradationCode is the CLOSED set of page-level gap codes. A
// report-level code (userreport's captain_unavailable) is a separate set owned
// by that package; the two never share a value.
type UserReportDegradationCode string

const (
	// UserReportReactionsPartial means some items carry no full reaction rollup
	// (GitHub omitted the reactions object; GitLab issues carry only thumbs
	// up/down). Count is the affected item count.
	UserReportReactionsPartial UserReportDegradationCode = "reactions_partial"
	// UserReportCommentReactionsUnavailable means the forge's listing carries no
	// reactions for comments at all (GitLab notes). Count is the comment count.
	UserReportCommentReactionsUnavailable UserReportDegradationCode = "comment_reactions_unavailable"
	// UserReportCommentsViaIssueActivity means comments are found through their
	// issue's updated_at because the forge has no repository-wide comment
	// listing (GitLab), relying on the forge touching the issue when a note
	// is saved.
	UserReportCommentsViaIssueActivity UserReportDegradationCode = "comments_via_issue_activity"
	// UserReportBotDetectionHeuristic means bot detection rests on a naming
	// heuristic plus corroboration rather than a forge-asserted account type
	// (GitLab access-token bots); other service accounts classify by
	// membership.
	UserReportBotDetectionHeuristic UserReportDegradationCode = "bot_detection_heuristic"
	// UserReportAssociationUnresolved means some authors' association could not be
	// resolved (absent from the payload, or a member lookup failed). Those
	// authors are never Internal. Count is the affected item count.
	UserReportAssociationUnresolved UserReportDegradationCode = "association_unresolved"
	// UserReportCursorAnchorUnavailable means the forge sent no parseable Date
	// header on the scan's first response, so NextCursor HOLDS at Since.
	UserReportCursorAnchorUnavailable UserReportDegradationCode = "cursor_anchor_unavailable"
	// UserReportScanTruncated means a listing hit its page cap with items
	// remaining. The page holds what was read, and NextCursor stops at the
	// last fully-read updated_at so the next scan continues from there.
	// Count is the number of truncated listings.
	UserReportScanTruncated UserReportDegradationCode = "scan_truncated"
	// UserReportConfidentialExcluded means confidential issues and internal notes
	// were EXCLUDED from the page (GitLab). Count is the excluded item count.
	UserReportConfidentialExcluded UserReportDegradationCode = "confidential_excluded"
)

// UserReportDegradationCodes returns the closed page-level code set, sorted.
func UserReportDegradationCodes() []UserReportDegradationCode {
	out := []UserReportDegradationCode{
		UserReportReactionsPartial,
		UserReportCommentReactionsUnavailable,
		UserReportCommentsViaIssueActivity,
		UserReportBotDetectionHeuristic,
		UserReportAssociationUnresolved,
		UserReportCursorAnchorUnavailable,
		UserReportScanTruncated,
		UserReportConfidentialExcluded,
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// UserReportDegradation is one named gap on a page: the Code, an
// operator-facing Detail, and the affected Count (0 for a page-wide gap).
type UserReportDegradation struct {
	Code   UserReportDegradationCode
	Detail string
	Count  int
}

// UserReportCapability is the capability name UnavailableError carries for
// the user-report read capability.
const UserReportCapability = "user-report read"

// ErrUserReportUnresumable reports a listing that exhausted its page cap
// INSIDE one equal-timestamp run at the scan's own Since: no resume point
// beyond Since exists, so the provider returns a nil page with this error
// (wrapping the client's own sentinel) instead of a truncation the next scan
// would re-read forever. Match with errors.Is; a transient forge failure never
// matches it. The caller's cursor stays where it was.
var ErrUserReportUnresumable = errors.New("workmgmt: user-report listing exhausted its page cap inside one equal-timestamp run at the since bound")

// UserReportReaderFor resolves the registered provider for id and
// type-asserts the optional UserReportReader capability. It is the SINGLE
// chokepoint every consumer resolves through: a provider lacking the
// capability (jira) yields a typed *UnavailableError{Reason:
// ReasonNotImplemented} and a nil reader, never a nil interface a caller could
// dispatch against. An unregistered id returns Get's *UnknownProviderError.
func UserReportReaderFor(id string) (UserReportReader, error) {
	p, err := Get(id)
	if err != nil {
		return nil, err
	}
	r, ok := p.(UserReportReader)
	if !ok {
		return nil, &UnavailableError{
			Provider:   p.Name(),
			Capability: UserReportCapability,
			Reason:     ReasonNotImplemented,
			Detail:     "this provider does not list issue and comment activity; user reports are read from GitHub or GitLab",
		}
	}
	return r, nil
}

// UserReportCursorOverlap is subtracted from the forge's Date when anchoring
// the next cursor, so the next scan re-reads a boundary window whether the
// forge's since filter is inclusive or exclusive and whatever its own
// clock-to-index lag. Re-reads are deduplicated downstream; skips are not
// recoverable.
const UserReportCursorOverlap = 2 * time.Minute

// UserReportListingEnd is how one listing of a scan ended: exhausted
// (Truncated false) or stopped by its page cap with every item updated
// STRICTLY before ResumeAt returned.
type UserReportListingEnd struct {
	Truncated bool
	ResumeAt  time.Time
}

// NextUserReportCursor is the single cursor rule both providers apply.
//
// The anchor is the forge's own Date header from the scan's FIRST response,
// minus UserReportCursorOverlap — the forge's clock domain, never fishhawkd's
// (the AGENTS.md cross-clock-domain trap, #3048). Every item updated before
// the anchor was listed, because the listings started at or after it.
//
//   - A zero forgeDate HOLDS the cursor at since and names
//     cursor_anchor_unavailable: an unanchorable scan never advances.
//   - A truncated listing pulls the cursor back to its ResumeAt when that is
//     earlier (NextCursor = min(anchor, every truncated ResumeAt)) and names
//     scan_truncated, so the next scan continues where the cap stopped. A
//     truncated listing with no ResumeAt holds the cursor at since.
//   - The result is clamped to never fall below since.
func NextUserReportCursor(since, forgeDate time.Time, listings ...UserReportListingEnd) (time.Time, []UserReportDegradation) {
	var degs []UserReportDegradation
	truncated := 0
	next := forgeDate.Add(-UserReportCursorOverlap)
	for _, l := range listings {
		if !l.Truncated {
			continue
		}
		truncated++
		if l.ResumeAt.IsZero() {
			next = since
		} else if l.ResumeAt.Before(next) {
			next = l.ResumeAt
		}
	}
	if truncated > 0 {
		degs = append(degs, UserReportDegradation{
			Code:   UserReportScanTruncated,
			Detail: "a listing hit its page cap with items remaining; the page holds what was read and the cursor stops at the last fully-read updated_at so the next scan continues from there",
			Count:  truncated,
		})
	}
	if forgeDate.IsZero() {
		degs = append(degs, UserReportDegradation{
			Code:   UserReportCursorAnchorUnavailable,
			Detail: "the forge sent no parseable Date header on the scan's first response; the cursor is held at the request bound so the next scan re-reads instead of guessing with fishhawkd's clock",
		})
		return since, degs
	}
	if next.Before(since) {
		next = since
	}
	return next, degs
}

// userReportKey identifies one item across listings: an issue by its number,
// a comment by its forge id.
type userReportKey struct {
	kind UserReportKind
	id   int64
}

func keyOf(it UserReportItem) userReportKey {
	if it.Kind == UserReportKindComment {
		return userReportKey{kind: it.Kind, id: it.CommentID}
	}
	return userReportKey{kind: it.Kind, id: int64(it.IssueNumber)}
}

// kindRank orders issues before comments at an equal updated_at.
func kindRank(k UserReportKind) int {
	if k == UserReportKindIssue {
		return 0
	}
	return 1
}

// DedupeUserReportItems is the shared dedupe-and-sort rule both providers
// call. A keyset walk re-reads its own boundary inclusively and an item
// updated mid-scan reappears at the walk's tail, so one (kind, id) can occur
// more than once: the occurrence with the LATEST UpdatedAt is kept (the first
// such on a tie), so the page carries the newest state the scan observed.
// The result is sorted by (UpdatedAt, kind — issue first, IssueNumber,
// CommentID), so two scans over the same activity yield the same order. The
// input is not mutated.
func DedupeUserReportItems(items []UserReportItem) []UserReportItem {
	idx := make(map[userReportKey]int, len(items))
	out := make([]UserReportItem, 0, len(items))
	for _, it := range items {
		k := keyOf(it)
		if i, seen := idx[k]; seen {
			if it.UpdatedAt.After(out[i].UpdatedAt) {
				out[i] = it
			}
			continue
		}
		idx[k] = len(out)
		out = append(out, it)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			return a.UpdatedAt.Before(b.UpdatedAt)
		}
		if ra, rb := kindRank(a.Kind), kindRank(b.Kind); ra != rb {
			return ra < rb
		}
		if a.IssueNumber != b.IssueNumber {
			return a.IssueNumber < b.IssueNumber
		}
		return a.CommentID < b.CommentID
	})
	return out
}
