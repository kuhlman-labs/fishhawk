package gitlabclient

// This file carries the four issue-thread operations the forge/gitlab
// adapter's forge.IssueOperations implementation needs (E50.17 / #2900):
// read an issue, list its notes, post a note, and edit its state. Before
// this file the client spoke only CreateIssue and LinkIssues on the
// issues surface. Each method reuses the c.do + errForStatus helpers and
// the same argument-validation posture (project id > 0, iid > 0, a
// non-empty note body) the existing methods hold.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Issue is the subset of a GitLab issue object Fishhawk reads back
// (https://docs.gitlab.com/ee/api/issues.html#single-project-issue).
// State is GitLab's NATIVE vocabulary ("opened" | "closed"); the forge
// adapter normalizes it onto the forge-neutral words. There is NO
// state_reason field because GitLab's issue object carries none.
type Issue struct {
	// IID is the project-scoped issue number (the "#N" users see).
	IID int `json:"iid"`
	// Title is the issue title.
	Title string `json:"title"`
	// Description is the issue body (GitLab's `description`).
	Description string `json:"description"`
	// State is "opened" or "closed" as GitLab reports it.
	State string `json:"state"`
	// Labels is the issue's label names. The v4 issues API returns labels
	// as an array of strings unless `with_labels_details=true` is
	// requested, which these calls never set.
	Labels []string `json:"labels"`
	// WebURL is the browse URL for the issue.
	WebURL string `json:"web_url"`
}

// Note is the subset of a GitLab issue note (comment) Fishhawk reads back
// (https://docs.gitlab.com/ee/api/notes.html#issues). Body is the raw
// markdown byte-intact, so an HTML-comment idempotency marker round-trips.
type Note struct {
	// ID is the note's global id.
	ID int64
	// Body is the note's raw markdown.
	Body string
	// System reports a GitLab-generated activity note (label change,
	// milestone, …) rather than a user comment. Surfaced, not filtered:
	// a system note never carries a Fishhawk marker, so a consumer that
	// scans bodies for one is unaffected either way.
	System bool
	// CreatedAt is the RFC 3339 creation timestamp as GitLab returned it.
	CreatedAt string
	// Author is the note author's username, lifted out of the nested
	// `author` object GitLab sends (see noteResponse).
	Author string
}

// noteResponse is the wire shape of one note: Note's flat fields plus the
// nested author object GitLab sends; note() flattens it.
type noteResponse struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	System    bool   `json:"system"`
	CreatedAt string `json:"created_at"`
	Author    struct {
		Username string `json:"username"`
	} `json:"author"`
}

func (n noteResponse) note() Note {
	return Note{ID: n.ID, Body: n.Body, System: n.System, CreatedAt: n.CreatedAt, Author: n.Author.Username}
}

// GetIssue reads a single issue by its project-scoped iid.
//
//	GET /api/v4/projects/:id/issues/:iid
func (c *Client) GetIssue(ctx context.Context, projectID, iid int) (*Issue, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("gitlabclient: project id required")
	}
	if iid <= 0 {
		return nil, fmt.Errorf("gitlabclient: issue iid required")
	}

	resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v4/projects/%d/issues/%d", projectID, iid), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := errForStatus("get issue", resp); err != nil {
		return nil, err
	}

	var out Issue
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("gitlabclient: decode issue: %w", err)
	}
	return &out, nil
}

// ListIssueNotes lists every note on an issue, oldest first.
//
//	GET /api/v4/projects/:id/issues/:iid/notes?per_page=100&sort=asc&order_by=created_at
//
// It PAGES TO EXHAUSTION via the rel="next" Link header GitLab sends on a
// paginated collection (https://docs.gitlab.com/ee/api/rest/#pagination-link-header)
// — the same mechanism the GitHub client's ListIssueComments uses. A
// single-page read would miss an idempotency marker that scrolled onto
// page 2 and re-post the marked note on every redelivery.
//
// A next link is followed only when it targets the client's OWN base host
// and scheme; any other host is refused rather than followed, because the
// request carries the PRIVATE-TOKEN header and a forge-supplied absolute
// URL must never redirect that credential off-instance. The SAME boundary is
// enforced across HTTP 3xx redirects by doNoOffOriginRedirect (client.go),
// which every gitlabclient request now dispatches through: a same-origin
// endpoint that redirects off-instance is refused BEFORE the redirected
// request is sent, so the header never follows.
func (c *Client) ListIssueNotes(ctx context.Context, projectID, iid int) ([]Note, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("gitlabclient: project id required")
	}
	if iid <= 0 {
		return nil, fmt.Errorf("gitlabclient: issue iid required")
	}

	next := c.baseURL + fmt.Sprintf("/api/v4/projects/%d/issues/%d/notes?per_page=100&sort=asc&order_by=created_at", projectID, iid)
	var out []Note
	for next != "" {
		if err := c.sameOrigin(next, "next-page link"); err != nil {
			return nil, err
		}
		page, link, err := c.getNotesPage(ctx, next)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		next = nextPageURL(link)
	}
	return out, nil
}

// getNotesPage fetches one page of notes at the absolute URL and returns
// the page plus the raw Link header for the caller to walk.
func (c *Client) getNotesPage(ctx context.Context, absURL string) ([]Note, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, absURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("gitlabclient: build request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.doNoOffOriginRedirect(req)
	if err != nil {
		return nil, "", fmt.Errorf("gitlabclient: GET %s: %w", req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := errForStatus("list issue notes", resp); err != nil {
		return nil, "", err
	}

	var raw []noteResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, "", fmt.Errorf("gitlabclient: decode issue notes: %w", err)
	}
	page := make([]Note, 0, len(raw))
	for _, n := range raw {
		page = append(page, n.note())
	}
	return page, resp.Header.Get("Link"), nil
}

// nextPageURL extracts the rel="next" target from an RFC 8288 Link header,
// or "" when the header carries none (the last page).
func nextPageURL(link string) string {
	for _, part := range strings.Split(link, ",") {
		segs := strings.Split(strings.TrimSpace(part), ";")
		if len(segs) < 2 {
			continue
		}
		urlPart := strings.TrimSpace(segs[0])
		if !strings.HasPrefix(urlPart, "<") || !strings.HasSuffix(urlPart, ">") {
			continue
		}
		for _, attr := range segs[1:] {
			if strings.TrimSpace(attr) == `rel="next"` {
				return urlPart[1 : len(urlPart)-1]
			}
		}
	}
	return ""
}

// CreateIssueNote posts body as a new note on the issue.
//
//	POST /api/v4/projects/:id/issues/:iid/notes
func (c *Client) CreateIssueNote(ctx context.Context, projectID, iid int, body string) (*Note, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("gitlabclient: project id required")
	}
	if iid <= 0 {
		return nil, fmt.Errorf("gitlabclient: issue iid required")
	}
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("gitlabclient: note body required")
	}

	resp, err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/api/v4/projects/%d/issues/%d/notes", projectID, iid),
		map[string]any{"body": body})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := errForStatus("create issue note", resp); err != nil {
		return nil, err
	}

	var out noteResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("gitlabclient: decode create issue note: %w", err)
	}
	n := out.note()
	return &n, nil
}

// UpdateIssueParams describes an issue edit. A nil Description leaves the
// body unchanged; a non-nil one sends the field (empty string clears it).
// A nil Labels leaves the label set unchanged; a non-nil one REPLACES it
// wholesale. StateEvent is GitLab's lifecycle verb — "close" or "reopen"
// — which is how the v4 API changes an issue's state
// (https://docs.gitlab.com/ee/api/issues.html#edit-issue): writing
// `state: "closed"` directly is NOT accepted.
type UpdateIssueParams struct {
	Description *string
	Labels      *[]string
	StateEvent  string
}

// UpdateIssue edits an issue's description, label set and/or state.
//
//	PUT /api/v4/projects/:id/issues/:iid
//
// Only the fields the caller set are transmitted; a params set with no
// field at all is refused locally rather than sent as a no-op PUT.
func (c *Client) UpdateIssue(ctx context.Context, projectID, iid int, p UpdateIssueParams) (*Issue, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("gitlabclient: project id required")
	}
	if iid <= 0 {
		return nil, fmt.Errorf("gitlabclient: issue iid required")
	}

	body := map[string]any{}
	if p.Description != nil {
		body["description"] = *p.Description
	}
	if p.Labels != nil {
		body["labels"] = strings.Join(*p.Labels, ",")
	}
	if p.StateEvent != "" {
		body["state_event"] = p.StateEvent
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("gitlabclient: update issue: no fields to update")
	}

	resp, err := c.do(ctx, http.MethodPut, fmt.Sprintf("/api/v4/projects/%d/issues/%d", projectID, iid), body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := errForStatus("update issue", resp); err != nil {
		return nil, err
	}

	var out Issue
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("gitlabclient: decode update issue: %w", err)
	}
	return &out, nil
}
