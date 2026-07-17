// Package gitlabclient is the backend's minimal typed wrapper around the
// GitLab REST API v4 for the small set of operations the Fishhawk
// work-management gitlab provider (ADR-058, #1856) needs: resolving a
// namespaced project path to its numeric id, creating an issue with
// labels applied at create time, and best-effort linking that issue to a
// parent via the Free-tier issue-links API.
//
// Auth is the PRIVATE-TOKEN header carrying a personal, project, or group
// access token (the documented GitLab server-to-server scheme — see
// https://docs.gitlab.com/ee/api/rest/authentication.html). The instance
// base URL and the token are supplied at construction; in production they
// come from FISHHAWKD_GITLAB_* server env (secrets cannot live in a
// checked-in repo config), wired in a sibling slice. A configurable base
// URL is what lets one client cover both GitLab.com SaaS and self-managed
// instances. The token is unexported and never logged.
//
// What's NOT in scope: a comprehensive GitLab SDK. We cover exactly the
// three calls the provider maps a resolved work item onto. The HTTP
// transport is injectable (Doer) so tests drive every method against a
// stub without touching the network.
package gitlabclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Doer is the minimal HTTP capability the client needs. *http.Client
// satisfies it; tests inject a stub that asserts the request shape and
// returns canned responses.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// APIError is returned when GitLab answers a request with a non-2xx
// status. It carries the operation, the HTTP status code, and a brief
// excerpt of the response body for observability. Callers can switch on
// StatusCode (e.g. 404 → project/issue gone, 401/403 → bad token).
type APIError struct {
	Op         string
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("gitlabclient: %s: status %d", e.Op, e.StatusCode)
	}
	return fmt.Sprintf("gitlabclient: %s: status %d: %s", e.Op, e.StatusCode, e.Body)
}

// Client is a minimal GitLab REST v4 client. Concurrent use is safe
// provided the injected Doer is. Construct via New.
type Client struct {
	baseURL string
	token   string
	http    Doer
}

// Option customises a Client at construction.
type Option func(*Client)

// WithHTTPClient injects the HTTP transport. Without it the client uses
// http.DefaultClient. Tests pass a stub Doer here.
func WithHTTPClient(d Doer) Option {
	return func(c *Client) { c.http = d }
}

// New returns a Client for the GitLab instance at baseURL authenticating
// with token (PRIVATE-TOKEN header). baseURL is normalised by trimming
// any trailing slash so path joining is unambiguous; the same
// construction covers GitLab.com (https://gitlab.com) and a self-managed
// host. The token is stored unexported and never appears in logs or
// errors.
func New(baseURL, token string, opts ...Option) *Client {
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    http.DefaultClient,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Project is the subset of a GitLab project Fishhawk reads back from a
// lookup: the numeric id used to address the project in subsequent calls,
// and the human-facing web URL.
type Project struct {
	// ID is GitLab's numeric project id.
	ID int
	// WebURL is the browse URL for the project.
	WebURL string
}

// projectResponse is the subset of GET /api/v4/projects/:path's success
// body Fishhawk reads.
type projectResponse struct {
	ID     int    `json:"id"`
	WebURL string `json:"web_url"`
}

// GetProject resolves a namespaced project path (e.g. "group/subgroup/name")
// to its numeric id and web URL.
//
//	GET /api/v4/projects/:url-encoded-path
//
// GitLab accepts a URL-encoded namespaced path in place of the numeric id
// (https://docs.gitlab.com/ee/api/rest/#namespaced-paths), so the slashes
// in path are percent-encoded into a single path segment.
func (c *Client) GetProject(ctx context.Context, path string) (*Project, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("gitlabclient: project path required")
	}

	// PathEscape encodes the whole namespaced path as one segment,
	// turning the group/project slashes into %2F as GitLab requires.
	encoded := url.PathEscape(path)
	resp, err := c.do(ctx, http.MethodGet, "/api/v4/projects/"+encoded, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if err := errForStatus("get project", resp); err != nil {
		return nil, err
	}

	var out projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("gitlabclient: decode project: %w", err)
	}
	return &Project{ID: out.ID, WebURL: out.WebURL}, nil
}

// CreateIssueParams describes a single issue to create. Title is
// required; Description and Labels are optional. Labels ride the create
// request, so an issue lands with its full label set (including a
// board-status label) in one call.
type CreateIssueParams struct {
	Title       string
	Description string
	Labels      []string
}

// CreatedIssue is the result of CreateIssue.
type CreatedIssue struct {
	// IID is the project-scoped issue number (the "#N" users see).
	IID int
	// WebURL is the browse URL for the issue.
	WebURL string
}

// createIssueResponse is the subset of POST .../issues's success body
// Fishhawk reads back.
type createIssueResponse struct {
	IID    int    `json:"iid"`
	WebURL string `json:"web_url"`
}

// CreateIssue creates an issue in the given project.
//
//	POST /api/v4/projects/:id/issues
//
// Labels are sent as a single comma-separated string, the shape the v4
// issues API expects (https://docs.gitlab.com/ee/api/issues.html#new-issue).
// An empty Description or Labels omits that field.
func (c *Client) CreateIssue(ctx context.Context, projectID int, p CreateIssueParams) (*CreatedIssue, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("gitlabclient: project id required")
	}
	if strings.TrimSpace(p.Title) == "" {
		return nil, fmt.Errorf("gitlabclient: issue title required")
	}

	body := map[string]any{"title": p.Title}
	if p.Description != "" {
		body["description"] = p.Description
	}
	if len(p.Labels) > 0 {
		body["labels"] = strings.Join(p.Labels, ",")
	}

	resp, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/api/v4/projects/%d/issues", projectID), body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if err := errForStatus("create issue", resp); err != nil {
		return nil, err
	}

	var out createIssueResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("gitlabclient: decode create issue: %w", err)
	}
	return &CreatedIssue{IID: out.IID, WebURL: out.WebURL}, nil
}

// LinkIssues links the source issue (iid, in projectID) to a target issue
// (targetIID, in the same project) with a "relates_to" issue link.
//
//	POST /api/v4/projects/:id/issues/:iid/links
//
// The Free-tier issue-links API is used deliberately: GitLab group epics
// are a Premium feature, so a parent link in v0 is a relates_to issue
// link, not an epic membership (https://docs.gitlab.com/ee/api/issue_links.html).
// The target lives in the same project, so target_project_id echoes
// projectID. The gitlab provider treats a link failure as best-effort
// (#1107), recording it without failing the filing.
func (c *Client) LinkIssues(ctx context.Context, projectID, iid, targetIID int) error {
	if projectID <= 0 {
		return fmt.Errorf("gitlabclient: project id required")
	}
	if iid <= 0 {
		return fmt.Errorf("gitlabclient: source issue iid required")
	}
	if targetIID <= 0 {
		return fmt.Errorf("gitlabclient: target issue iid required")
	}

	resp, err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/api/v4/projects/%d/issues/%d/links", projectID, iid),
		map[string]any{
			"target_project_id": projectID,
			"target_issue_iid":  targetIID,
		})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return errForStatus("link issues", resp)
}

// do builds and sends a PRIVATE-TOKEN-authed request. A non-nil body is
// JSON-encoded; a nil body sends no payload (GET). The caller owns
// closing the returned response body.
func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("gitlabclient: marshal request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("gitlabclient: build request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitlabclient: %s %s: %w", method, path, err)
	}
	return resp, nil
}

// errForStatus returns an *APIError for any non-2xx response and nil
// otherwise. The body excerpt is bounded so a large error payload can't
// bloat logs.
func errForStatus(op string, resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return &APIError{
		Op:         op,
		StatusCode: resp.StatusCode,
		Body:       readBriefBody(resp.Body),
	}
}

// readBriefBody returns up to 512 bytes of the response body for
// inclusion in error messages. The caller closes the body.
func readBriefBody(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 512))
	return strings.TrimSpace(string(b))
}
