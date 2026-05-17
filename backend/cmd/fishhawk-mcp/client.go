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
//
// IDs are typed as `string` rather than `uuid.UUID` so the MCP
// SDK's auto-generated JSON schema (which uses reflection over the
// Go type) sees a string. `uuid.UUID` is a 16-byte array under the
// hood, which would surface in the schema as `type: array` and
// fail the SDK's response validation at the wire boundary — even
// though the JSON payload itself is a string. Tools that need a
// typed UUID parse the string locally (e.g. `uuid.Parse(in.RunID)`).
type Run struct {
	ID                 string    `json:"id"`
	Repo               string    `json:"repo"`
	WorkflowID         string    `json:"workflow_id"`
	WorkflowSHA        string    `json:"workflow_sha"`
	TriggerSource      string    `json:"trigger_source"`
	TriggerRef         *string   `json:"trigger_ref"`
	State              string    `json:"state"`
	ParentRunID        *string   `json:"parent_run_id"`
	PullRequestURL     *string   `json:"pull_request_url"`
	RetryAttempt       int       `json:"retry_attempt"`
	MaxRetriesSnapshot int       `json:"max_retries_snapshot"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
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

// createRunRequest mirrors the backend's `POST /v0/runs` request body
// (`backend/internal/server/runs.go::createRunRequest`). Repeated here
// rather than imported because the MCP server's apiClient is
// deliberately a thin local copy — the import-direction rule is
// `cli → backend`, not the other way around, and the same applies
// to this binary.
type createRunRequest struct {
	Repo          string  `json:"repo"`
	WorkflowID    string  `json:"workflow_id"`
	WorkflowSHA   string  `json:"workflow_sha"`
	TriggerSource string  `json:"trigger_source"`
	TriggerRef    *string `json:"trigger_ref,omitempty"`
}

// StartRunParams is the typed input the apiClient takes for run
// creation. `IdempotencyKey` is optional and travels in the HTTP
// header per the backend's E8.2 contract — when set, a previously-
// created run with the same `(repo, key)` returns 200 instead of a
// fresh 201.
type StartRunParams struct {
	Repo           string
	WorkflowID     string
	WorkflowSHA    string
	TriggerSource  string
	TriggerRef     string
	IdempotencyKey string
}

// RetryStage re-fires a failed stage via
// `POST /v0/stages/{stage_id}/retry`. Returns the updated Stage row
// (failed → pending → dispatched for category A/C; failed →
// awaiting_approval for category-D SLA-timeout). 4xx surfaces:
//   - 404 stage_not_found
//   - 422 retry_not_applicable (category B / gate-rejected D — the
//     workflow or spec needs to change first; a fresh run is the
//     right next step)
func (c *apiClient) RetryStage(ctx context.Context, id uuid.UUID) (*Stage, error) {
	var s Stage
	if err := c.do(ctx, http.MethodPost, "/v0/stages/"+id.String()+"/retry", nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// CancelRun transitions a run to the cancelled state via
// `POST /v0/runs/{run_id}/cancel`. Idempotent: cancelling an already-
// cancelled run returns 200 with the same body. 4xx surfaces:
//   - 404 run_not_found
//   - 409 invalid_state_transition (the run is already terminal in a
//     non-cancelled state, e.g. succeeded / failed)
func (c *apiClient) CancelRun(ctx context.Context, id uuid.UUID) (*Run, error) {
	var r Run
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+id.String()+"/cancel", nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// StartRun creates a new run. Returns the created (or replayed) run
// plus an `idempotent` flag indicating whether the backend served
// 200 (replay against an existing run) versus 201 (fresh). 4xx
// surfaces as *apiError; the MCP tool layer reads the code field to
// translate validation errors into clean tool errors.
func (c *apiClient) StartRun(ctx context.Context, p StartRunParams) (*Run, bool, error) {
	req := createRunRequest{
		Repo:          p.Repo,
		WorkflowID:    p.WorkflowID,
		WorkflowSHA:   p.WorkflowSHA,
		TriggerSource: p.TriggerSource,
	}
	if p.TriggerRef != "" {
		ref := p.TriggerRef
		req.TriggerRef = &ref
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, false, fmt.Errorf("marshal start_run: %w", err)
	}
	headers := map[string]string{}
	if p.IdempotencyKey != "" {
		headers["Idempotency-Key"] = p.IdempotencyKey
	}
	var run Run
	status, err := c.doWithStatus(ctx, http.MethodPost, "/v0/runs", body, headers, &run)
	if err != nil {
		return nil, false, err
	}
	// 200 = idempotent replay; 201 = newly created. Both are success.
	return &run, status == http.StatusOK, nil
}

// Stage mirrors the wire shape. The fields cover both get_plan's
// "find the plan stage" use case and get_run_status's "tell me
// what's happening" view: type/state for the lifecycle, sequence
// for ordering, executor + timestamps + failure fields for the
// agent's context.
type Stage struct {
	ID              string        `json:"id"`
	RunID           string        `json:"run_id"`
	Sequence        int           `json:"sequence"`
	Type            string        `json:"type"`
	Executor        StageExecutor `json:"executor"`
	State           string        `json:"state"`
	StartedAt       *time.Time    `json:"started_at,omitempty"`
	EndedAt         *time.Time    `json:"ended_at,omitempty"`
	FailureCategory *string       `json:"failure_category,omitempty"`
	FailureReason   *string       `json:"failure_reason,omitempty"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

// StageExecutor mirrors the OpenAPI sub-schema. The closed-set
// kind field (`agent` | `human`) is what an agent reads to know
// whether a downstream stage will be self-driven or wait for a
// human.
type StageExecutor struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

type listStagesResult struct {
	Items []Stage `json:"items"`
}

// ListRunStages calls GET /v0/runs/{run_id}/stages. Stages come back
// ordered by sequence ascending; the tool layer picks the plan
// stage from the list.
func (c *apiClient) ListRunStages(ctx context.Context, runID uuid.UUID) ([]Stage, error) {
	var res listStagesResult
	if err := c.do(ctx, http.MethodGet, "/v0/runs/"+runID.String()+"/stages", nil, &res); err != nil {
		return nil, err
	}
	return res.Items, nil
}

// Artifact is the wire shape with content inline. The backend
// returns content directly on the listStageArtifacts endpoint (per
// the OpenAPI Artifact schema), so the MCP tool doesn't need a
// separate /v0/artifacts/{id} fetch.
// Content is typed as `any` rather than `json.RawMessage` so the MCP
// SDK's schema reflection sees an unconstrained value. `RawMessage`
// is `[]byte` under the hood, which would surface as `type: array`
// and reject the object/scalar payloads each artifact kind carries.
// The decode side (tryGetPlanForRun) re-marshals + unmarshals into
// the typed PlanContent shape; the cost is one extra round-trip
// through json.Marshal per plan fetch, which is negligible.
type Artifact struct {
	ID            string    `json:"id"`
	StageID       string    `json:"stage_id"`
	Kind          string    `json:"kind"`
	SchemaVersion *string   `json:"schema_version,omitempty"`
	ContentHash   string    `json:"content_hash"`
	Content       any       `json:"content,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

type listArtifactsResult struct {
	Items []Artifact `json:"items"`
}

// ListStageArtifacts calls GET /v0/stages/{stage_id}/artifacts.
// Artifacts come back ordered by created_at ascending; callers
// pick the most-recent (the SPA pre-trace does the same — see
// `frontend/src/routes/stage-detail.tsx`).
func (c *apiClient) ListStageArtifacts(ctx context.Context, stageID uuid.UUID) ([]Artifact, error) {
	var res listArtifactsResult
	if err := c.do(ctx, http.MethodGet, "/v0/stages/"+stageID.String()+"/artifacts", nil, &res); err != nil {
		return nil, err
	}
	return res.Items, nil
}

// AuditEntry mirrors the OpenAPI AuditEntry schema. Payload is
// left as json.RawMessage so the MCP tool can pass the typed shape
// directly through to the client without re-encoding category-
// specific payloads — the agent introspects them as JSON.
// Payload is typed `any` for the same reason Artifact.Content is —
// the SDK's schema reflection treats `json.RawMessage` as an array,
// but per-category payloads are arbitrary JSON objects. Agents
// reading the response introspect each category's shape directly.
type AuditEntry struct {
	ID           string    `json:"id"`
	Sequence     int64     `json:"sequence"`
	RunID        string    `json:"run_id"`
	StageID      *string   `json:"stage_id,omitempty"`
	Timestamp    time.Time `json:"ts"`
	Category     string    `json:"category"`
	ActorKind    *string   `json:"actor_kind,omitempty"`
	ActorSubject *string   `json:"actor_subject,omitempty"`
	Payload      any       `json:"payload,omitempty"`
	PrevHash     *string   `json:"prev_hash,omitempty"`
	EntryHash    string    `json:"entry_hash"`
}

type listAuditResult struct {
	Items      []AuditEntry `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

// ListRunAuditFilter scopes a per-run audit query. Empty values
// drop from the query string; zero Limit lets the server pick its
// default (100, per the OpenAPI; 500 max). The MCP tool layer
// clamps to a lower cap before calling.
type ListRunAuditFilter struct {
	Category string
	StageID  string
	Limit    int
	Cursor   string
}

// ListRunAudit calls GET /v0/runs/{run_id}/audit with optional
// category / stage_id / limit / cursor filters. Returns entries
// sequence-ascending (matches the API surface: per-run scope for
// the run-detail UI + verifier path). For "most-recent-first"
// queries use ListRecentRunAudit which hits the cross-chain
// endpoint with time-descending order.
func (c *apiClient) ListRunAudit(ctx context.Context, runID uuid.UUID, f ListRunAuditFilter) ([]AuditEntry, string, error) {
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
	var res listAuditResult
	if err := c.do(ctx, http.MethodGet, path, nil, &res); err != nil {
		return nil, "", err
	}
	return res.Items, res.NextCursor, nil
}

// ListRecentRunAudit calls GET /v0/audit?run_id=<id>&limit=<N>.
// Returns rows time-descending — exactly the order an agent wants
// when surfacing "what's happened recently" in the get_run_status
// view. The cross-chain endpoint is the only way to get
// descending order without a paginate-to-end walk; per-run rows
// for the queried run are the only thing returned because global
// rows have run_id IS NULL and don't match the filter.
//
// The MCP tool layer is responsible for clamping limit to the
// server's range before calling this; the client passes it
// through verbatim.
func (c *apiClient) ListRecentRunAudit(ctx context.Context, runID uuid.UUID, limit int) ([]AuditEntry, error) {
	q := url.Values{}
	q.Set("run_id", runID.String())
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var res listAuditResult
	if err := c.do(ctx, http.MethodGet, "/v0/audit?"+q.Encode(), nil, &res); err != nil {
		return nil, err
	}
	return res.Items, nil
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

// do is the no-extra-headers wrapper around doWithStatus that
// discards the response status code. Most readers only need to
// know that the call succeeded; the StartRun path needs the 200
// vs 201 distinction (idempotent replay vs fresh create) so it
// reaches for doWithStatus directly.
func (c *apiClient) do(ctx context.Context, method, path string, body []byte, out any) error {
	_, err := c.doWithStatus(ctx, method, path, body, nil, out)
	return err
}

// doWithStatus performs the request and decodes the JSON body into
// out. On non-2xx the body is parsed as the OpenAPI error envelope
// and returned as *apiError. `extraHeaders` is merged into the
// request — used for E8.2's Idempotency-Key on POST /v0/runs. Same
// posture as the CLI's httpclient.do.
func (c *apiClient) doWithStatus(ctx context.Context, method, path string, body []byte, extraHeaders map[string]string, out any) (int, error) {
	if c.baseURL == "" {
		return 0, errors.New("apiClient: baseURL not set")
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read body: %w", err)
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
		return resp.StatusCode, ae
	}
	if out == nil {
		return resp.StatusCode, nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return resp.StatusCode, fmt.Errorf("decode response: %w", err)
	}
	return resp.StatusCode, nil
}
