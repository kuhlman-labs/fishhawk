package gitlabclient

// This file carries the two reads the E81 user-report source reader
// (#3771) needs from GitLab: every project issue updated since a bound
// (ListIssuesUpdatedAfter) and one author's effective project access level
// (GetProjectMemberAccessLevel). The issue listing walks OLDEST-FIRST under
// a KEYSET bound (see keysetWalk), never newest-first offset pages, so a
// deletion or transfer between page requests cannot shift an unread issue
// onto an already-read page offset and skip it; the one place offset pages
// remain, an equal-timestamp run, is re-walked until two consecutive passes
// agree. Notes are still listed per issue through ListIssueNotes
// (issue_ops.go): GitLab has no project-wide notes listing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// userReportPerPage is the page size ListIssuesUpdatedAfter requests.
const userReportPerPage = 100

// ErrEqualTimestampRunExceedsCap reports a keyset walk that exhausted
// maxListPages INSIDE one equal-timestamp run sitting at the walk's own
// updated_after bound. Moving the bound cannot step past such a run and the
// cap stops the offset pages under it, so no resume point beyond the bound
// exists: the listing fails closed with NO items rather than return a
// truncation a caller would re-read forever. Match with errors.Is; it is
// distinct from an *APIError or a transport failure.
var ErrEqualTimestampRunExceedsCap = errors.New("gitlabclient: page cap exhausted inside one equal-timestamp run at the updated_after bound")

// ListingMeta is what ListIssuesUpdatedAfter reports about its walk
// besides the issues.
type ListingMeta struct {
	// Date is the parsed Date response header (RFC 9110 §6.6.1) of the
	// walk's FIRST response — the forge's own clock at the earliest request.
	// Zero when absent or unparseable; a caller anchoring a cursor on it
	// must then hold the cursor.
	Date time.Time
	// Truncated reports that maxListPages stopped the walk with issues
	// remaining. The returned issues are everything read up to that point.
	Truncated bool
	// ResumeAt is set only when Truncated: the updated_at of the last issue
	// the walk read, or — when the cap stopped the walk inside an
	// equal-timestamp run it had not yet proven complete — that run's
	// updated_at, so the cursor never passes an unconfirmed run. Every issue
	// updated STRICTLY before it was returned, so a later walk passing it as
	// updated_after re-reads the boundary and continues without a gap. It is
	// always strictly after the walk's bound.
	ResumeAt time.Time
}

// UpdatedIssue is one issue from ListIssuesUpdatedAfter.
type UpdatedIssue struct {
	IID         int
	Title       string
	Description string
	// State is GitLab's native "opened" | "closed".
	State  string
	Labels []string
	WebURL string
	// AuthorID and AuthorUsername are the nested author object's id and
	// username; the id keys GetProjectMemberAccessLevel.
	AuthorID       int64
	AuthorUsername string
	// Upvotes and Downvotes are GitLab's award-emoji thumbs counts — the
	// only reaction counts the issues listing carries. Like GitHub's rollup
	// they are a SNAPSHOT as of the issue's last observed update.
	Upvotes   int
	Downvotes int
	// Confidential is the issue's confidential flag. The client surfaces it
	// and never filters on it; excluding confidential issues is the
	// consumer's decision.
	Confidential bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// updatedIssueResponse is the wire shape of one listed issue.
type updatedIssueResponse struct {
	IID         int      `json:"iid"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	State       string   `json:"state"`
	Labels      []string `json:"labels"`
	WebURL      string   `json:"web_url"`
	Author      struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	} `json:"author"`
	Upvotes      int    `json:"upvotes"`
	Downvotes    int    `json:"downvotes"`
	Confidential bool   `json:"confidential"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// ListIssuesUpdatedAfter lists every issue in the project updated at or
// after the bound, oldest-first.
//
//	GET /api/v4/projects/:id/issues?scope=all&order_by=updated_at&sort=asc&per_page=100[&updated_after=RFC3339][&page=N]
//
// GitLab documents updated_after as "on or after"
// (https://docs.gitlab.com/ee/api/issues.html#list-project-issues), so the
// keyset bound re-reads its own boundary; the caller dedupes. after is
// omitted when zero. Every page URL is built from the client's own base —
// a forge-supplied Link target is consulted only for whether a next page
// exists and is never requested, so PRIVATE-TOKEN cannot follow it — and
// each request dispatches through doNoOffOriginRedirect for the 3xx half of
// that boundary.
func (c *Client) ListIssuesUpdatedAfter(ctx context.Context, projectID int, after time.Time) ([]UpdatedIssue, ListingMeta, error) {
	if projectID <= 0 {
		return nil, ListingMeta{}, fmt.Errorf("gitlabclient: project id required")
	}
	endpoint := c.baseURL + fmt.Sprintf("/api/v4/projects/%d/issues", projectID)
	return keysetWalk(after, maxListPages, func(bound time.Time, page int) (keysetPage[UpdatedIssue], error) {
		q := url.Values{
			"scope":    {"all"},
			"order_by": {"updated_at"},
			"sort":     {"asc"},
			"per_page": {strconv.Itoa(userReportPerPage)},
		}
		if !bound.IsZero() {
			q.Set("updated_after", bound.UTC().Format(time.RFC3339Nano))
		}
		if page > 1 {
			q.Set("page", strconv.Itoa(page))
		}
		var out keysetPage[UpdatedIssue]
		var raw []updatedIssueResponse
		if err := c.getActivityPage(ctx, endpoint+"?"+q.Encode(), &raw, &out.more, &out.date); err != nil {
			return out, err
		}
		for _, n := range raw {
			updated, err := time.Parse(time.RFC3339, n.UpdatedAt)
			if err != nil {
				return out, fmt.Errorf("gitlabclient: list issues updated after: issue #%d updated_at %q: %w", n.IID, n.UpdatedAt, err)
			}
			created, err := time.Parse(time.RFC3339, n.CreatedAt)
			if err != nil {
				return out, fmt.Errorf("gitlabclient: list issues updated after: issue #%d created_at %q: %w", n.IID, n.CreatedAt, err)
			}
			out.observe(int64(n.IID), updated)
			out.kept = append(out.kept, UpdatedIssue{
				IID: n.IID, Title: n.Title, Description: n.Description, State: n.State,
				Labels: n.Labels, WebURL: n.WebURL,
				AuthorID: n.Author.ID, AuthorUsername: n.Author.Username,
				Upvotes: n.Upvotes, Downvotes: n.Downvotes, Confidential: n.Confidential,
				CreatedAt: created, UpdatedAt: updated,
			})
		}
		return out, nil
	})
}

// getActivityPage fetches one page at absURL (built from the client's base),
// decodes it into out, and reports whether Link advertises a rel="next"
// page and the parsed Date header.
func (c *Client) getActivityPage(ctx context.Context, absURL string, out any, more *bool, date *time.Time) error {
	const op = "list issues updated after"
	if err := c.sameOrigin(absURL, "page url"); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, absURL, nil)
	if err != nil {
		return fmt.Errorf("gitlabclient: build request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.doNoOffOriginRedirect(req)
	if err != nil {
		return fmt.Errorf("gitlabclient: GET %s: %w", req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := errForStatus(op, resp); err != nil {
		return err
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("gitlabclient: decode %s: %w", op, err)
	}
	*more = nextPageURL(resp.Header.Get("Link")) != ""
	if t, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
		*date = t
	}
	return nil
}

// keysetPage is one page as keysetWalk sees it: the items kept, the stamp
// (identity + updated_at) of every item on the page, their updated_at range,
// whether the forge advertises more, and the page's Date header.
type keysetPage[T any] struct {
	kept     []T
	stamps   []nodeStamp
	min, max time.Time
	more     bool
	date     time.Time
}

// nodeStamp is one listed item's identity at the updated_at it was read at.
type nodeStamp struct {
	id int64
	at int64 // UnixNano
}

func (p *keysetPage[T]) observe(id int64, updatedAt time.Time) {
	if len(p.stamps) == 0 || updatedAt.Before(p.min) {
		p.min = updatedAt
	}
	if len(p.stamps) == 0 || updatedAt.After(p.max) {
		p.max = updatedAt
	}
	p.stamps = append(p.stamps, nodeStamp{id: id, at: updatedAt.UnixNano()})
}

// keysetWalk drives the oldest-first keyset walk. The first request
// carries since. After each page with items remaining, the next request
// MOVES THE BOUND to the page's latest updated_at (page 1), so no page
// offset exists that a deletion between requests could shift; it requests
// the next OFFSET page under the SAME bound only when the whole page shares
// one updated_at (an equal-timestamp run, which moving the bound cannot
// step past). The bound is re-read inclusively, so boundary items come back
// twice: the caller dedupes.
//
// AN OFFSET-PAGED PASS IS CONFIRMED BEFORE THE BOUND MOVES PAST IT. Inside a
// run, removing an already-read item (a deletion, a transfer, or an update
// that moves it to the tail) between two offset requests shifts every later
// item one offset left, so the item at the page boundary is never served.
// A pass — pages 1..k under one bound, ending at the first page that is not
// one full equal-timestamp page — is therefore re-walked from page 1 until
// two CONSECUTIVE passes observe the identical stamp set. That is a proof,
// not a heuristic: the set under a past bound only shrinks, so a survivor
// the later pass skipped was also skipped by the earlier one, which needs a
// removal during the earlier pass of an item the later pass still read
// unchanged — impossible. A one-page pass is a single request and needs no
// confirmation. (An item updated DURING the walk lands at or after the
// forge Date the cursor anchor is taken from, so the next scan's overlap
// re-reads it.)
//
// At maxPages with items remaining the walk returns what it read with
// Truncated and ResumeAt = the last page's latest updated_at — or, when the
// cap lands inside an unconfirmed run, that run's updated_at, so the cursor
// cannot pass it. When that resume point is not after since — the capped
// walk never confirmed a run sitting at since — it fails closed with
// ErrEqualTimestampRunExceedsCap.
//
// githubclient carries the same walk for its activity listings; the two
// client packages share no code by design (neither imports the other).
func keysetWalk[T any](since time.Time, maxPages int, fetch func(bound time.Time, page int) (keysetPage[T], error)) ([]T, ListingMeta, error) {
	var out []T
	var meta ListingMeta
	bound, page := since, 1
	// pass collects the stamps of the current multi-page pass, prev those of
	// the previous complete pass under the same bound (nil when none), and
	// runAt the timestamp of the run that put the pass onto offset pages.
	var pass, prev map[nodeStamp]bool
	var runAt time.Time
	truncate := func(resume time.Time, requests int) ([]T, ListingMeta, error) {
		if !resume.After(since) {
			return nil, ListingMeta{}, fmt.Errorf("%w: %d pages of issues all updated at %s", ErrEqualTimestampRunExceedsCap, requests, since.UTC().Format(time.RFC3339Nano))
		}
		meta.Truncated, meta.ResumeAt = true, resume
		return out, meta, nil
	}
	for requests := 1; ; requests++ {
		p, err := fetch(bound, page)
		if err != nil {
			return nil, ListingMeta{}, err
		}
		if requests == 1 {
			meta.Date = p.date
		}
		out = append(out, p.kept...)
		inRun := p.more && len(p.stamps) > 0 && p.min.Equal(p.max)
		if page == 1 {
			pass = nil
			if inRun {
				runAt = p.min
			}
		}
		if inRun || page > 1 {
			if pass == nil {
				pass = make(map[nodeStamp]bool, len(p.stamps))
			}
			for _, st := range p.stamps {
				pass[st] = true
			}
		}
		if inRun {
			if requests >= maxPages {
				return truncate(runAt, requests)
			}
			page++
			continue
		}
		if page > 1 && !maps.Equal(pass, prev) {
			if requests >= maxPages {
				return truncate(runAt, requests)
			}
			prev, page = pass, 1
			continue
		}
		prev = nil
		if !p.more || len(p.stamps) == 0 {
			return out, meta, nil
		}
		if requests >= maxPages {
			return truncate(p.max, requests)
		}
		bound, page = p.max, 1
	}
}

// GetProjectMemberAccessLevel reads one user's effective access level on
// the project, inherited group membership included.
//
//	GET /api/v4/projects/:id/members/all/:user_id
//
// (https://docs.gitlab.com/ee/api/members.html#get-a-member-of-a-group-or-project-including-inherited-and-invited-members).
// A 200 returns (access_level, true, nil). A 404 is a RESOLVED non-member:
// (0, false, nil) — GitLab also answers 404 for a project the token cannot
// see, and reading that as non-member errs toward NOT trusting the author,
// the safe direction. Any other status is an *APIError, so a caller can tell
// "not a member" from "could not tell".
func (c *Client) GetProjectMemberAccessLevel(ctx context.Context, projectID int, userID int64) (int, bool, error) {
	if projectID <= 0 {
		return 0, false, fmt.Errorf("gitlabclient: project id required")
	}
	if userID <= 0 {
		return 0, false, fmt.Errorf("gitlabclient: user id required")
	}
	resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v4/projects/%d/members/all/%d", projectID, userID), nil)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return 0, false, nil
	}
	if err := errForStatus("get project member", resp); err != nil {
		return 0, false, err
	}
	var body struct {
		AccessLevel int `json:"access_level"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, false, fmt.Errorf("gitlabclient: decode project member: %w", err)
	}
	return body.AccessLevel, true, nil
}
