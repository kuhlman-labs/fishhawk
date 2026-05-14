// Package httpclient is the CLI's typed wrapper around the
// Fishhawk backend HTTP API. It handles auth, request building,
// JSON decoding, and error envelope translation so subcommands
// can call typed methods without touching net/http.
//
// Errors come back as *APIError when the server returned an
// error envelope per docs/api/v0.openapi.yaml's `Error` schema;
// callers can errors.As to inspect the code/message/details.
package httpclient

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

// Client is the typed Fishhawk API client. Construct via New;
// callers can override HTTP for tests.
type Client struct {
	BaseURL string
	Token   string // optional; sent as Authorization: Bearer <token> when set
	HTTP    *http.Client
}

// New returns a Client with a 60s default timeout. baseURL must
// not have a trailing slash.
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: baseURL,
		Token:   token,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

// APIError is the typed form of the OpenAPI error envelope. The
// Code field is what callers switch on; Message is human and may
// drift between versions.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
	Details    map[string]any
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("fishhawk: HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("fishhawk: HTTP %d (%s): %s", e.StatusCode, e.Code, e.Message)
}

// errorEnvelope mirrors the wire shape the backend always emits on
// non-2xx responses.
type errorEnvelope struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

// Run is the CLI-side projection of the OpenAPI Run schema. Field
// names + types match the wire shape verbatim.
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

// CreateRunInput is what StartRun marshals into the request body.
type CreateRunInput struct {
	Repo          string  `json:"repo"`
	WorkflowID    string  `json:"workflow_id"`
	WorkflowSHA   string  `json:"workflow_sha"`
	TriggerSource string  `json:"trigger_source"`
	TriggerRef    *string `json:"trigger_ref,omitempty"`
}

// StartRun calls POST /v0/runs.
func (c *Client) StartRun(ctx context.Context, in CreateRunInput) (*Run, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	var run Run
	if err := c.do(ctx, http.MethodPost, "/v0/runs", body, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// GetRun calls GET /v0/runs/{id}.
func (c *Client) GetRun(ctx context.Context, id uuid.UUID) (*Run, error) {
	var run Run
	if err := c.do(ctx, http.MethodGet, "/v0/runs/"+id.String(), nil, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// ListRunsFilter scopes a ListRuns call. Empty values are dropped
// from the query string.
type ListRunsFilter struct {
	Repo       string
	WorkflowID string
	State      string
	Limit      int
	Cursor     string
}

// ListRunsResult is the paginated response.
type ListRunsResult struct {
	Items      []Run  `json:"items"`
	NextCursor string `json:"next_cursor"`
}

// ListRuns calls GET /v0/runs with optional filters and cursor.
func (c *Client) ListRuns(ctx context.Context, f ListRunsFilter) (*ListRunsResult, error) {
	q := url.Values{}
	if f.Repo != "" {
		q.Set("repo", f.Repo)
	}
	if f.WorkflowID != "" {
		q.Set("workflow_id", f.WorkflowID)
	}
	if f.State != "" {
		q.Set("state", f.State)
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	if f.Cursor != "" {
		q.Set("cursor", f.Cursor)
	}
	path := "/v0/runs"
	if encoded := q.Encode(); encoded != "" {
		path = path + "?" + encoded
	}
	var res ListRunsResult
	if err := c.do(ctx, http.MethodGet, path, nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// CancelRun calls POST /v0/runs/{id}/cancel.
func (c *Client) CancelRun(ctx context.Context, id uuid.UUID) (*Run, error) {
	var run Run
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+id.String()+"/cancel", nil, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// Stage is the CLI-side projection of the OpenAPI Stage schema.
// Field names + types match the wire shape verbatim. The pointer
// fields mirror the OpenAPI `[string, "null"]` shape.
type Stage struct {
	ID              uuid.UUID     `json:"id"`
	RunID           uuid.UUID     `json:"run_id"`
	Sequence        int           `json:"sequence"`
	Type            string        `json:"type"`
	Executor        StageExecutor `json:"executor"`
	State           string        `json:"state"`
	StartedAt       *time.Time    `json:"started_at"`
	EndedAt         *time.Time    `json:"ended_at"`
	FailureCategory *string       `json:"failure_category"`
	FailureReason   *string       `json:"failure_reason"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

// StageExecutor mirrors the OpenAPI executor sub-schema.
type StageExecutor struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// ListStagesResult is the response envelope for GET /v0/runs/{id}/stages.
type ListStagesResult struct {
	Items []Stage `json:"items"`
}

// ListRunStages calls GET /v0/runs/{run_id}/stages. Stages come back
// ordered by sequence ascending.
func (c *Client) ListRunStages(ctx context.Context, runID uuid.UUID) (*ListStagesResult, error) {
	var res ListStagesResult
	if err := c.do(ctx, http.MethodGet, "/v0/runs/"+runID.String()+"/stages", nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// ApprovalDecision is the typed enum the approvals endpoint accepts.
type ApprovalDecision string

// Approval decisions.
const (
	ApprovalApprove ApprovalDecision = "approve"
	ApprovalReject  ApprovalDecision = "reject"
)

// SubmitApprovalInput is the request body for POST /v0/stages/{id}/approvals.
type SubmitApprovalInput struct {
	Decision ApprovalDecision `json:"decision"`
	Comment  string           `json:"comment,omitempty"`
}

// SubmitApproval calls POST /v0/stages/{stage_id}/approvals.
// The response is the updated Stage with state transitioned to
// succeeded (approve) or failed (reject).
func (c *Client) SubmitApproval(ctx context.Context, stageID uuid.UUID, in SubmitApprovalInput) (*Stage, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	var stage Stage
	if err := c.do(ctx, http.MethodPost, "/v0/stages/"+stageID.String()+"/approvals", body, &stage); err != nil {
		return nil, err
	}
	return &stage, nil
}

// AuditEntry is the CLI-side projection of the OpenAPI AuditEntry
// schema. Payload is left as raw JSON so the CLI can render or pass
// through whatever shape a given category emits — categories grow
// over time and the CLI shouldn't need to track each one.
type AuditEntry struct {
	ID           uuid.UUID       `json:"id"`
	Sequence     int64           `json:"sequence"`
	RunID        uuid.UUID       `json:"run_id"`
	StageID      *uuid.UUID      `json:"stage_id"`
	Timestamp    time.Time       `json:"ts"`
	Category     string          `json:"category"`
	ActorKind    *string         `json:"actor_kind"`
	ActorSubject *string         `json:"actor_subject"`
	Payload      json.RawMessage `json:"payload"`
	PrevHash     *string         `json:"prev_hash"`
	EntryHash    string          `json:"entry_hash"`
}

// ListRunAuditFilter scopes a ListRunAudit call. Empty values are
// dropped from the query string; zero Limit lets the server pick its
// default (50, per the OpenAPI default; 500 max).
type ListRunAuditFilter struct {
	Category string
	StageID  string
	Limit    int
	Cursor   string
}

// ListRunAuditResult is the paginated response envelope.
type ListRunAuditResult struct {
	Items      []AuditEntry `json:"items"`
	NextCursor string       `json:"next_cursor"`
}

// ListRunAudit calls GET /v0/runs/{run_id}/audit with optional
// category / stage / pagination filters. Entries come back
// sequence-ascending; the cursor stays opaque to the CLI — the
// server defines its encoding.
func (c *Client) ListRunAudit(ctx context.Context, runID uuid.UUID, f ListRunAuditFilter) (*ListRunAuditResult, error) {
	q := url.Values{}
	if f.Category != "" {
		q.Set("category", f.Category)
	}
	if f.StageID != "" {
		q.Set("stage_id", f.StageID)
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	if f.Cursor != "" {
		q.Set("cursor", f.Cursor)
	}
	path := "/v0/runs/" + runID.String() + "/audit"
	if encoded := q.Encode(); encoded != "" {
		path = path + "?" + encoded
	}
	var res ListRunAuditResult
	if err := c.do(ctx, http.MethodGet, path, nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// RetryStage calls POST /v0/stages/{stage_id}/retry. The response is
// the post-retry Stage — typically `dispatched` after orchestrator
// handoff for category A/C, or `awaiting_approval` for the
// D-timeout path. Server-side rejections (non-failed stage,
// non-retryable failure category) surface as *APIError with the
// envelope's code (e.g. `retry_not_applicable`).
func (c *Client) RetryStage(ctx context.Context, stageID uuid.UUID) (*Stage, error) {
	var stage Stage
	if err := c.do(ctx, http.MethodPost, "/v0/stages/"+stageID.String()+"/retry", nil, &stage); err != nil {
		return nil, err
	}
	return &stage, nil
}

// do performs the request and decodes the JSON body into out (or
// reads the error envelope on non-2xx and returns *APIError).
func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	if c.BaseURL == "" {
		return errors.New("httpclient: BaseURL not set")
	}

	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode >= 400 {
		apiErr := &APIError{StatusCode: resp.StatusCode}
		var env errorEnvelope
		if json.Unmarshal(raw, &env) == nil {
			apiErr.Code = env.Error.Code
			apiErr.Message = env.Error.Message
			apiErr.Details = env.Error.Details
		}
		return apiErr
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
