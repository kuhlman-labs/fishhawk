package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// apiClient is the MCP server's typed wrapper around the Fishhawk
// backend HTTP API. Intentionally a thin local copy rather than an
// import of `cli/internal/httpclient` so the dependency direction
// stays clean (backend → cli would invert the module hierarchy and
// force a published cli version on every backend release).
//
// Only the slice of endpoints the MCP tools actually need lives
// here. Subsequent tool tickets (E19.4 / #344, E19.5 / #345,
// E19.6 / #346) extend this surface as they land.
type apiClient struct {
	baseURL string
	token   string
	http    *http.Client
}

func newAPIClient(cfg config) *apiClient {
	return &apiClient{
		baseURL: cfg.backendURL,
		token:   cfg.apiToken,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// apiError is the typed form of the OpenAPI error envelope. Mirrors
// the CLI's *APIError so the wire shape stays consistent across
// surfaces; callers errors.As into this to switch on Code.
type apiError struct {
	StatusCode int
	Code       string
	Message    string
	Details    map[string]any
}

func (e *apiError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("fishhawk: HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("fishhawk: HTTP %d (%s): %s", e.StatusCode, e.Code, e.Message)
}

// Run mirrors the OpenAPI Run schema's wire shape. Subset: the MCP
// tools surface every operator-relevant field, but skip internal
// bookkeeping (workflow_sha etc.) the agent has no use for. JSON
// tags match the backend exactly so the renderer in tools.go can
// pass the decoded struct straight back to the MCP client without
// re-mapping.
type Run struct {
	ID                 uuid.UUID  `json:"id"`
	Repo               string     `json:"repo"`
	WorkflowID         string     `json:"workflow_id"`
	WorkflowSHA        string     `json:"workflow_sha"`
	TriggerSource      string     `json:"trigger_source"`
	TriggerRef         *string    `json:"trigger_ref"`
	State              string     `json:"state"`
	ParentRunID        *uuid.UUID `json:"parent_run_id"`
	PullRequestURL     *string    `json:"pull_request_url"`
	RetryAttempt       int        `json:"retry_attempt"`
	MaxRetriesSnapshot int        `json:"max_retries_snapshot"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

type listRunsResult struct {
	Items      []Run  `json:"items"`
	NextCursor string `json:"next_cursor"`
}

// listRunsFilter scopes a runs query. Empty values drop from the
// query string. The MCP server only uses three of the backend's
// supported filters (repo, pull_request_url, trigger_ref) — the
// resolution logic in tools.go picks the right one per input
// shape.
type listRunsFilter struct {
	Repo           string
	PullRequestURL string
	TriggerRef     string
	Limit          int
}

func (c *apiClient) GetRun(ctx context.Context, id uuid.UUID) (*Run, error) {
	var r Run
	if err := c.do(ctx, http.MethodGet, "/v0/runs/"+id.String(), nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *apiClient) ListRuns(ctx context.Context, f listRunsFilter) (*listRunsResult, error) {
	q := url.Values{}
	if f.Repo != "" {
		q.Set("repo", f.Repo)
	}
	if f.PullRequestURL != "" {
		q.Set("pull_request_url", f.PullRequestURL)
	}
	if f.TriggerRef != "" {
		q.Set("trigger_ref", f.TriggerRef)
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	path := "/v0/runs"
	if encoded := q.Encode(); encoded != "" {
		path = path + "?" + encoded
	}
	var res listRunsResult
	if err := c.do(ctx, http.MethodGet, path, nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// do performs the request and decodes the JSON body into out. On
// non-2xx the body is parsed as the OpenAPI error envelope and
// returned as *apiError. Same posture as the CLI's httpclient.do.
func (c *apiClient) do(ctx context.Context, method, path string, body []byte, out any) error {
	if c.baseURL == "" {
		return errors.New("apiClient: baseURL not set")
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		ae := &apiError{StatusCode: resp.StatusCode}
		var env struct {
			Error struct {
				Code    string         `json:"code"`
				Message string         `json:"message"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &env) == nil {
			ae.Code = env.Error.Code
			ae.Message = env.Error.Message
			ae.Details = env.Error.Details
		}
		return ae
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
