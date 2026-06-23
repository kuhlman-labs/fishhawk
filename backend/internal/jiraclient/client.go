// Package jiraclient is the backend's minimal typed wrapper around the
// Jira Cloud REST API v3 for the small set of operations the Fishhawk
// work-management jira provider (#1094, deferred from #1005) needs:
// creating an issue, linking it to a parent/epic via a best-effort
// post-create edit, and moving an issue through a workflow transition.
//
// Auth is HTTP Basic with `email:api_token` (the documented Jira Cloud
// server-to-server scheme — see
// https://developer.atlassian.com/cloud/jira/platform/basic-auth-for-rest-apis/).
// The instance base URL and both credentials are supplied at
// construction; in production they come from FISHHAWKD_JIRA_* server
// env (secrets cannot live in a checked-in repo config), wired in
// slice 3. The credentials are unexported and never logged.
//
// What's NOT in scope: a comprehensive Jira SDK. We cover exactly the
// two calls the provider maps a resolved work item onto. The HTTP
// transport is injectable (Doer) so tests drive every method against a
// stub without touching the network.
package jiraclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Doer is the minimal HTTP capability the client needs. *http.Client
// satisfies it; tests inject a stub that asserts the request shape and
// returns canned responses.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Errors callers may want to switch on.
var (
	// ErrTransitionNotFound means Transition could not find a workflow
	// transition reaching the requested target status from the issue's
	// current state. The jira provider treats this as a best-effort
	// enrichment failure (#1107), not a fatal error.
	ErrTransitionNotFound = errors.New("jiraclient: no transition to target status")
)

// APIError is returned when Jira answers a request with a non-2xx
// status. It carries the operation, the HTTP status code, and a brief
// excerpt of the response body for observability. Callers can switch on
// StatusCode (e.g. 404 → issue/project gone, 401/403 → bad creds).
type APIError struct {
	Op         string
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("jiraclient: %s: status %d", e.Op, e.StatusCode)
	}
	return fmt.Sprintf("jiraclient: %s: status %d: %s", e.Op, e.StatusCode, e.Body)
}

// Client is a minimal Jira Cloud REST v3 client. Concurrent use is safe
// provided the injected Doer is. Construct via New.
type Client struct {
	baseURL  string
	email    string
	apiToken string
	http     Doer
}

// Option customises a Client at construction.
type Option func(*Client)

// WithHTTPClient injects the HTTP transport. Without it the client uses
// http.DefaultClient. Tests pass a stub Doer here.
func WithHTTPClient(d Doer) Option {
	return func(c *Client) { c.http = d }
}

// New returns a Client for the Jira instance at baseURL authenticating
// as email with apiToken (HTTP Basic). baseURL is normalised by
// trimming any trailing slash so path joining is unambiguous. The
// credentials are stored unexported and never appear in logs or errors.
func New(baseURL, email, apiToken string, opts ...Option) *Client {
	c := &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		email:    email,
		apiToken: apiToken,
		http:     http.DefaultClient,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// CreateIssueParams describes a single issue to create. ProjectKey,
// IssueType, and Summary are required; the rest are optional. Parent/epic
// linking is NOT done at create time — it is a separate best-effort
// post-create LinkParent PUT, so the per-project field-shape distinction
// (team-managed `parent` vs a classic epic-link custom field) lives there.
type CreateIssueParams struct {
	ProjectKey  string
	IssueType   string
	Summary     string
	Description string
	Labels      []string
}

// CreatedIssue is the result of CreateIssue.
type CreatedIssue struct {
	// Key is the human-facing issue key, e.g. "ENG-1234".
	Key string
	// ID is Jira's numeric issue id as a string.
	ID string
	// URL is the browse URL for the issue (baseURL + /browse/{key}).
	URL string
}

// createIssueResponse is the subset of POST /rest/api/3/issue's success
// body Fishhawk reads back.
type createIssueResponse struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

// CreateIssue creates an issue in the configured Jira instance.
//
//	POST /rest/api/3/issue
//
// REST API v3 requires the description in Atlassian Document Format
// (ADF), so a plain Description string is wrapped into a minimal ADF
// document here. An empty Description omits the field entirely.
func (c *Client) CreateIssue(ctx context.Context, p CreateIssueParams) (*CreatedIssue, error) {
	if p.ProjectKey == "" {
		return nil, errors.New("jiraclient: project key required")
	}
	if p.IssueType == "" {
		return nil, errors.New("jiraclient: issue type required")
	}
	if p.Summary == "" {
		return nil, errors.New("jiraclient: summary required")
	}

	fields := map[string]any{
		"project":   map[string]string{"key": p.ProjectKey},
		"issuetype": map[string]string{"name": p.IssueType},
		"summary":   p.Summary,
	}
	if p.Description != "" {
		fields["description"] = adfDoc(p.Description)
	}
	if len(p.Labels) > 0 {
		fields["labels"] = p.Labels
	}

	resp, err := c.do(ctx, http.MethodPost, "/rest/api/3/issue", map[string]any{"fields": fields})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if err := errForStatus("create issue", resp); err != nil {
		return nil, err
	}

	var out createIssueResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("jiraclient: decode create issue: %w", err)
	}
	return &CreatedIssue{
		Key: out.Key,
		ID:  out.ID,
		URL: c.baseURL + "/browse/" + out.Key,
	}, nil
}

// LinkParent links the issue identified by issueKey to its parent epic
// (epicKey) by editing the issue's fields.
//
//	PUT /rest/api/3/issue/{issueKey}
//
// fieldName selects the wire shape, which differs by project style:
//   - "parent" (or an empty fieldName, normalised to "parent") is the
//     team-managed (next-gen) reference and emits {"parent":{"key":epicKey}}.
//   - any other fieldName is a company-managed (classic) epic-link custom
//     field id (e.g. customfield_10014) and emits the bare-string form
//     {<fieldName>: epicKey}.
//
// A non-2xx response becomes an *APIError via the shared errForStatus
// helper; the jira provider treats a link failure as best-effort (#1107),
// recording it without failing the filing.
func (c *Client) LinkParent(ctx context.Context, issueKey, fieldName, epicKey string) error {
	if issueKey == "" {
		return errors.New("jiraclient: issue key required")
	}
	if epicKey == "" {
		return errors.New("jiraclient: epic key required")
	}
	if fieldName == "" {
		fieldName = "parent"
	}

	var value any
	if fieldName == "parent" {
		value = map[string]string{"key": epicKey}
	} else {
		value = epicKey
	}

	resp, err := c.do(ctx, http.MethodPut, "/rest/api/3/issue/"+issueKey, map[string]any{
		"fields": map[string]any{fieldName: value},
	})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return errForStatus("link parent", resp)
}

// transitionsResponse is the subset of GET .../transitions Fishhawk
// reads: each available transition's id, its own name, and the status it
// moves the issue to.
type transitionsResponse struct {
	Transitions []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		To   struct {
			Name string `json:"name"`
		} `json:"to"`
	} `json:"transitions"`
}

// Transition moves the issue identified by key into targetStatusName by
// finding and executing the matching workflow transition.
//
//	GET  /rest/api/3/issue/{key}/transitions   (discover available transitions)
//	POST /rest/api/3/issue/{key}/transitions   (execute by transition id)
//
// A created issue lands in the project's default status; reaching a
// target state requires this separate transition call — Jira does not
// let the status be set directly at create time. The match is
// case-insensitive against the transition's target status name first,
// then the transition's own name. Returns ErrTransitionNotFound when no
// available transition reaches targetStatusName from the issue's current
// state.
func (c *Client) Transition(ctx context.Context, key, targetStatusName string) error {
	if key == "" {
		return errors.New("jiraclient: issue key required")
	}
	if targetStatusName == "" {
		return errors.New("jiraclient: target status name required")
	}

	path := "/rest/api/3/issue/" + key + "/transitions"

	getResp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer func() { _ = getResp.Body.Close() }()
	if err := errForStatus("list transitions", getResp); err != nil {
		return err
	}

	var avail transitionsResponse
	if err := json.NewDecoder(getResp.Body).Decode(&avail); err != nil {
		return fmt.Errorf("jiraclient: decode transitions: %w", err)
	}

	target := strings.ToLower(strings.TrimSpace(targetStatusName))
	transitionID := ""
	for _, t := range avail.Transitions {
		if strings.ToLower(t.To.Name) == target || strings.ToLower(t.Name) == target {
			transitionID = t.ID
			break
		}
	}
	if transitionID == "" {
		return fmt.Errorf("%w: %q on %s", ErrTransitionNotFound, targetStatusName, key)
	}

	postResp, err := c.do(ctx, http.MethodPost, path, map[string]any{
		"transition": map[string]string{"id": transitionID},
	})
	if err != nil {
		return err
	}
	defer func() { _ = postResp.Body.Close() }()
	return errForStatus("execute transition", postResp)
}

// do builds and sends a Basic-authed request. A non-nil body is
// JSON-encoded; a nil body sends no payload (GET). The caller owns
// closing the returned response body.
func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("jiraclient: marshal request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("jiraclient: build request: %w", err)
	}
	req.SetBasicAuth(c.email, c.apiToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jiraclient: %s %s: %w", method, path, err)
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

// adfDoc wraps plain text into a minimal Atlassian Document Format
// document, which REST API v3 requires for rich-text fields like
// description. Each input line becomes its own paragraph; an empty line
// becomes an empty paragraph, preserving blank-line spacing. ADF text
// nodes cannot carry literal newlines, so per-line paragraphs are the
// faithful representation.
func adfDoc(text string) map[string]any {
	lines := strings.Split(text, "\n")
	content := make([]any, 0, len(lines))
	for _, line := range lines {
		para := map[string]any{"type": "paragraph"}
		if line != "" {
			para["content"] = []any{
				map[string]any{"type": "text", "text": line},
			}
		}
		content = append(content, para)
	}
	return map[string]any{
		"type":    "doc",
		"version": 1,
		"content": content,
	}
}
