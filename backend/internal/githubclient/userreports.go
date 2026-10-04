package githubclient

// This file carries the two repository-wide activity listings the E81
// user-report source reader (#3771) collects through: every issue and every
// issue comment updated since a bound. Both walk OLDEST-FIRST under a
// KEYSET bound (see keysetWalk), never newest-first offset pages, so a
// deletion or transfer between page requests cannot shift an unread item
// onto an already-read page offset and skip it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// userReportPerPage is the page size both activity listings request.
const userReportPerPage = 100

// userReportMaxPages bounds the requests ONE activity listing may make
// (100 x 100 = 10 000 nodes). Reaching it with nodes remaining is NOT an
// error: the listing returns what it read with ListingMeta.Truncated and a
// ResumeAt bound, so the caller's cursor still makes progress. The one
// fail-closed case is ErrEqualTimestampRunExceedsCap.
const userReportMaxPages = 100

// ErrEqualTimestampRunExceedsCap reports a keyset walk that exhausted its
// page cap INSIDE one equal-timestamp run sitting at the walk's own since
// bound. Moving the bound cannot step past such a run (every node shares
// the bound's updated_at) and the cap stops the offset pages under it, so
// no resume point beyond since exists: the listing fails closed with NO
// items rather than return a truncation a caller would re-read forever.
// Match with errors.Is; it is distinct from a transient transport error.
var ErrEqualTimestampRunExceedsCap = errors.New("githubclient: page cap exhausted inside one equal-timestamp run at the since bound")

// ListingMeta is what an activity listing reports about its walk besides
// the items.
type ListingMeta struct {
	// Date is the parsed Date response header (RFC 9110 §6.6.1) of the
	// walk's FIRST response — the forge's own clock at the earliest request
	// of the walk. Zero when the header is absent or unparseable; a caller
	// anchoring a cursor on it must then hold the cursor.
	Date time.Time
	// Truncated reports that the page cap stopped the walk with nodes
	// remaining. The returned items are everything read up to that point.
	Truncated bool
	// ResumeAt is set only when Truncated: the updated_at of the last node
	// the walk read. Every node updated STRICTLY before it was returned, so
	// a later walk passing it as since re-reads the boundary timestamp and
	// continues without a gap. It is always strictly after the walk's since.
	ResumeAt time.Time
}

// ReactionRollup is GitHub's per-item reactions summary (the `reactions`
// object on an issue or comment payload). It is a SNAPSHOT: adding a
// reaction does not bump the item's updated_at, so counts reflect the item
// as of the last update a walk observed, not reactions added since.
type ReactionRollup struct {
	TotalCount int `json:"total_count"`
	PlusOne    int `json:"+1"`
	MinusOne   int `json:"-1"`
	Laugh      int `json:"laugh"`
	Hooray     int `json:"hooray"`
	Confused   int `json:"confused"`
	Heart      int `json:"heart"`
	Rocket     int `json:"rocket"`
	Eyes       int `json:"eyes"`
}

// UpdatedIssue is one issue (never a pull request) from
// ListIssuesUpdatedSince.
type UpdatedIssue struct {
	Number  int
	Title   string
	Body    string
	State   string
	Labels  []string
	HTMLURL string
	// AuthorLogin and AuthorType are user.login and user.type ("User",
	// "Bot", "Organization").
	AuthorLogin string
	AuthorType  string
	// AuthorAssociation is GitHub's author_association (OWNER, MEMBER,
	// COLLABORATOR, CONTRIBUTOR, FIRST_TIME_CONTRIBUTOR, FIRST_TIMER,
	// MANNEQUIN, NONE). AssociationPresent is false when the key was
	// absent, null or empty — a THIRD state, never read as NONE.
	AuthorAssociation  string
	AssociationPresent bool
	// Reactions is the rollup; ReactionsPresent is false when the
	// `reactions` key was absent or null, so zero counts are never
	// mistaken for a known-zero rollup.
	Reactions        ReactionRollup
	ReactionsPresent bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// UpdatedIssueComment is one issue comment from
// ListIssueCommentsUpdatedSince. The repo-wide listing also returns
// comments on PULL REQUEST conversations (a PR is an issue); OnPullRequest
// marks them so a consumer can drop them.
type UpdatedIssueComment struct {
	ID int64
	// IssueNumber is the last path segment of issue_url; 0 when that
	// segment is not a positive integer.
	IssueNumber        int
	Body               string
	HTMLURL            string
	AuthorLogin        string
	AuthorType         string
	AuthorAssociation  string
	AssociationPresent bool
	Reactions          ReactionRollup
	ReactionsPresent   bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
	// OnPullRequest is true when html_url's path carries a /pull/ segment.
	// An unrecognised URL shape reads false, so a PR comment is INCLUDED
	// rather than dropped — over-inclusion is the safe direction.
	OnPullRequest bool
}

// activityAuthor, activityCommon and the two node shapes below are the wire
// subset both listings decode.
type activityAuthor struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type activityCommon struct {
	User              activityAuthor  `json:"user"`
	AuthorAssociation *string         `json:"author_association"`
	Reactions         *ReactionRollup `json:"reactions"`
	CreatedAt         string          `json:"created_at"`
	UpdatedAt         string          `json:"updated_at"`
	HTMLURL           string          `json:"html_url"`
	Body              string          `json:"body"`
}

// decoded is activityCommon with its presence flags and timestamps resolved.
type decoded struct {
	association        string
	associationPresent bool
	reactions          ReactionRollup
	reactionsPresent   bool
	createdAt          time.Time
	updatedAt          time.Time
}

func (a activityCommon) decode(op string) (decoded, error) {
	var d decoded
	if a.AuthorAssociation != nil && *a.AuthorAssociation != "" {
		d.association, d.associationPresent = *a.AuthorAssociation, true
	}
	if a.Reactions != nil {
		d.reactions, d.reactionsPresent = *a.Reactions, true
	}
	var err error
	if d.updatedAt, err = time.Parse(time.RFC3339, a.UpdatedAt); err != nil {
		return decoded{}, fmt.Errorf("githubclient: %s: node updated_at %q: %w", op, a.UpdatedAt, err)
	}
	if d.createdAt, err = time.Parse(time.RFC3339, a.CreatedAt); err != nil {
		return decoded{}, fmt.Errorf("githubclient: %s: node created_at %q: %w", op, a.CreatedAt, err)
	}
	return d, nil
}

// ListIssuesUpdatedSince lists every issue in repo updated at or after
// since, oldest-first.
//
//	GET /repos/{owner}/{repo}/issues?state=all&sort=updated&direction=asc&per_page=100[&since=RFC3339][&page=N]
//
// Pull requests (nodes carrying a pull_request key) are skipped; they
// still count toward the keyset walk's page bookkeeping. since is omitted
// when zero. Errors classify through classifyStatus (ErrForbidden,
// ErrNotFound unchanged).
func (c *Client) ListIssuesUpdatedSince(ctx context.Context, scope forge.CredentialScope, repo RepoRef, since time.Time) ([]UpdatedIssue, ListingMeta, error) {
	const op = "list issues updated since"
	base, installationID, err := c.activityPreflight(scope, repo, "/issues", url.Values{"state": {"all"}})
	if err != nil {
		return nil, ListingMeta{}, err
	}
	return keysetWalk(since, userReportMaxPages, func(bound time.Time, page int) (keysetPage[UpdatedIssue], error) {
		var nodes []struct {
			activityCommon
			Number      int               `json:"number"`
			Title       string            `json:"title"`
			State       string            `json:"state"`
			Labels      []json.RawMessage `json:"labels"`
			PullRequest json.RawMessage   `json:"pull_request"`
		}
		var out keysetPage[UpdatedIssue]
		if err := c.getActivityPage(ctx, op, base, bound, page, installationID, &nodes, &out.more, &out.date); err != nil {
			return out, err
		}
		for _, n := range nodes {
			d, err := n.decode(op)
			if err != nil {
				return out, err
			}
			out.observe(d.updatedAt)
			if isPullRequestNode(n.PullRequest) {
				continue
			}
			out.kept = append(out.kept, UpdatedIssue{
				Number: n.Number, Title: n.Title, Body: n.Body, State: n.State,
				Labels: decodeLabelNames(n.Labels), HTMLURL: n.HTMLURL,
				AuthorLogin: n.User.Login, AuthorType: n.User.Type,
				AuthorAssociation: d.association, AssociationPresent: d.associationPresent,
				Reactions: d.reactions, ReactionsPresent: d.reactionsPresent,
				CreatedAt: d.createdAt, UpdatedAt: d.updatedAt,
			})
		}
		return out, nil
	})
}

// ListIssueCommentsUpdatedSince lists every issue comment in repo updated
// at or after since, oldest-first, including comments on pull-request
// conversations (marked OnPullRequest).
//
//	GET /repos/{owner}/{repo}/issues/comments?sort=updated&direction=asc&per_page=100[&since=RFC3339][&page=N]
func (c *Client) ListIssueCommentsUpdatedSince(ctx context.Context, scope forge.CredentialScope, repo RepoRef, since time.Time) ([]UpdatedIssueComment, ListingMeta, error) {
	const op = "list issue comments updated since"
	base, installationID, err := c.activityPreflight(scope, repo, "/issues/comments", url.Values{})
	if err != nil {
		return nil, ListingMeta{}, err
	}
	return keysetWalk(since, userReportMaxPages, func(bound time.Time, page int) (keysetPage[UpdatedIssueComment], error) {
		var nodes []struct {
			activityCommon
			ID       int64  `json:"id"`
			IssueURL string `json:"issue_url"`
		}
		var out keysetPage[UpdatedIssueComment]
		if err := c.getActivityPage(ctx, op, base, bound, page, installationID, &nodes, &out.more, &out.date); err != nil {
			return out, err
		}
		for _, n := range nodes {
			d, err := n.decode(op)
			if err != nil {
				return out, err
			}
			out.observe(d.updatedAt)
			out.kept = append(out.kept, UpdatedIssueComment{
				ID: n.ID, IssueNumber: lastPathInt(n.IssueURL), Body: n.Body, HTMLURL: n.HTMLURL,
				AuthorLogin: n.User.Login, AuthorType: n.User.Type,
				AuthorAssociation: d.association, AssociationPresent: d.associationPresent,
				Reactions: d.reactions, ReactionsPresent: d.reactionsPresent,
				CreatedAt: d.createdAt, UpdatedAt: d.updatedAt,
				OnPullRequest: isPullRequestURL(n.HTMLURL),
			})
		}
		return out, nil
	})
}

// activityBase is one listing's fixed request shape: the endpoint path and
// the query parameters every page carries.
type activityBase struct {
	endpoint string
	query    url.Values
}

func (c *Client) activityPreflight(scope forge.CredentialScope, repo RepoRef, suffix string, extra url.Values) (activityBase, int64, error) {
	installationID, err := installationIDForScope(scope)
	if err != nil {
		return activityBase{}, 0, err
	}
	if c.Tokens == nil {
		return activityBase{}, 0, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return activityBase{}, 0, errors.New("githubclient: repo owner and name required")
	}
	extra.Set("sort", "updated")
	extra.Set("direction", "asc")
	extra.Set("per_page", strconv.Itoa(userReportPerPage))
	return activityBase{
		endpoint: c.endpoint("/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + suffix),
		query:    extra,
	}, installationID, nil
}

// getActivityPage requests one keyset page — the URL is built from the
// client's own base, never a forge-supplied Link target — decodes it into
// nodes, and reports whether Link advertises a rel="next" page and the
// parsed Date header.
func (c *Client) getActivityPage(ctx context.Context, op string, base activityBase, bound time.Time, page int, installationID int64, nodes any, more *bool, date *time.Time) error {
	q := url.Values{}
	for k, v := range base.query {
		q[k] = v
	}
	if !bound.IsZero() {
		q.Set("since", bound.UTC().Format(time.RFC3339Nano))
	}
	if page > 1 {
		q.Set("page", strconv.Itoa(page))
	}
	req, err := c.buildRequest(ctx, http.MethodGet, base.endpoint+"?"+q.Encode(), nil, installationID)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("githubclient: %s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := classifyStatus(op, resp); err != nil {
		return err
	}
	if err := json.NewDecoder(resp.Body).Decode(nodes); err != nil {
		return fmt.Errorf("githubclient: decode %s: %w", op, err)
	}
	*more = nextPageURL(resp.Header.Get("Link")) != ""
	if t, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
		*date = t
	}
	return nil
}

// keysetPage is one page as keysetWalk sees it: the items kept, the
// updated_at range over EVERY node on the page (kept or skipped), whether
// the forge advertises more, and the page's Date header.
type keysetPage[T any] struct {
	kept     []T
	n        int
	min, max time.Time
	more     bool
	date     time.Time
}

func (p *keysetPage[T]) observe(updatedAt time.Time) {
	if p.n == 0 || updatedAt.Before(p.min) {
		p.min = updatedAt
	}
	if p.n == 0 || updatedAt.After(p.max) {
		p.max = updatedAt
	}
	p.n++
}

// keysetWalk drives the oldest-first keyset walk both listings share.
// The first request carries since. After each page with nodes remaining,
// the next request MOVES THE BOUND to the page's latest updated_at (page
// 1), so no page offset exists that a deletion between requests could
// shift; it requests the next OFFSET page under the SAME bound only when
// the whole page shares one updated_at (an equal-timestamp run, which
// moving the bound cannot step past). The bound is re-read inclusively
// (GitHub's since and GitLab's updated_after both return items updated AT
// the bound), so boundary nodes come back twice: the caller dedupes.
//
// At maxPages with nodes remaining the walk returns what it read with
// Truncated and ResumeAt = the last page's latest updated_at, unless that
// is not after since — the whole capped walk sat in one equal-timestamp run
// at since — which fails closed with ErrEqualTimestampRunExceedsCap.
func keysetWalk[T any](since time.Time, maxPages int, fetch func(bound time.Time, page int) (keysetPage[T], error)) ([]T, ListingMeta, error) {
	var out []T
	var meta ListingMeta
	bound, page := since, 1
	for requests := 1; ; requests++ {
		p, err := fetch(bound, page)
		if err != nil {
			return nil, ListingMeta{}, err
		}
		if requests == 1 {
			meta.Date = p.date
		}
		out = append(out, p.kept...)
		if !p.more || p.n == 0 {
			return out, meta, nil
		}
		if requests >= maxPages {
			if !p.max.After(since) {
				return nil, ListingMeta{}, fmt.Errorf("%w: %d pages of nodes all updated at %s", ErrEqualTimestampRunExceedsCap, requests, since.UTC().Format(time.RFC3339Nano))
			}
			meta.Truncated, meta.ResumeAt = true, p.max
			return out, meta, nil
		}
		if p.min.Equal(p.max) {
			page++
		} else {
			bound, page = p.max, 1
		}
	}
}

// isPullRequestNode reports whether an issues-listing node carried a
// non-null pull_request key.
func isPullRequestNode(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

// isPullRequestURL reports whether a comment's html_url path carries a
// /pull/ segment (https://github.com/{o}/{r}/pull/{n}#issuecomment-{id}).
func isPullRequestURL(htmlURL string) bool {
	u, err := url.Parse(htmlURL)
	if err != nil {
		return false
	}
	return strings.Contains(u.Path, "/pull/")
}

// lastPathInt returns the last path segment of rawURL as a positive int, or
// 0 when it is not one.
func lastPathInt(rawURL string) int {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0
	}
	seg := u.Path[strings.LastIndex(u.Path, "/")+1:]
	n, err := strconv.Atoi(seg)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
