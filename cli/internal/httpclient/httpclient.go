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
	ID                 uuid.UUID     `json:"id"`
	Repo               string        `json:"repo"`
	WorkflowID         string        `json:"workflow_id"`
	WorkflowSHA        string        `json:"workflow_sha"`
	TriggerSource      string        `json:"trigger_source"`
	TriggerRef         *string       `json:"trigger_ref"`
	State              string        `json:"state"`
	ParentRunID        *uuid.UUID    `json:"parent_run_id"`
	DecomposedFrom     *uuid.UUID    `json:"decomposed_from,omitempty"`
	PullRequestURL     *string       `json:"pull_request_url"`
	RetryAttempt       int           `json:"retry_attempt"`
	MaxRetriesSnapshot int           `json:"max_retries_snapshot"`
	RunnerKind         string        `json:"runner_kind"`
	IssueContext       *IssueContext `json:"issue_context,omitempty"`
	CreatedAt          time.Time     `json:"created_at"`
	UpdatedAt          time.Time     `json:"updated_at"`
}

// IssueContext mirrors the OpenAPI shape: the GitHub issue payload
// the CLI fetches via `gh issue view` and ships inline at
// run-create (#415). Stored on the run row so the prompt builder
// reads it without needing a backend-side GitHub fetch.
type IssueContext struct {
	Title    string         `json:"title"`
	Body     string         `json:"body"`
	URL      string         `json:"url"`
	Number   int            `json:"number"`
	Comments []IssueComment `json:"comments,omitempty"`
}

// IssueComment is one issue comment carried alongside the body in
// IssueContext (#618). Captured at run-create from `gh issue view
// --json comments` so the plan agent sees refinements/decisions
// posted as comments, not just the title+body snapshot.
type IssueComment struct {
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// CreateRunInput is what StartRun marshals into the request body.
// RunnerKind (ADR-022 / #388) is optional; empty omits the field
// from the wire body, letting the backend apply its `github_actions`
// default. The local-runner CLI flow (Phase C / E22) passes "local"
// to mint a run that runs on the operator's workstation.
type CreateRunInput struct {
	Repo          string  `json:"repo"`
	WorkflowID    string  `json:"workflow_id"`
	WorkflowSHA   string  `json:"workflow_sha"`
	TriggerSource string  `json:"trigger_source"`
	TriggerRef    *string `json:"trigger_ref,omitempty"`
	RunnerKind    string  `json:"runner_kind,omitempty"`
	// WorkflowSpec is the YAML bytes of `.fishhawk/workflows.yaml`
	// at the requested workflow_sha, sent inline so the backend
	// can create stages for API-minted runs (#411). The CLI
	// discovers the file locally (auto-walk from --working-dir or
	// explicit --spec-file). Empty falls back to the legacy
	// no-stages create path; useful for integration tests that
	// just want to seed a run row.
	WorkflowSpec string `json:"workflow_spec,omitempty"`
	// IssueContext, when set, carries the title/body/url/number
	// for an issue-triggered run that was minted outside the
	// webhook flow (#415). The CLI shells to `gh issue view` at
	// run-create time and ships the payload inline so the
	// backend's prompt builder doesn't need an installation_id
	// to look up the issue.
	IssueContext *IssueContext `json:"issue_context,omitempty"`
	// BudgetOverride forces the run past a blocking periodic cost
	// budget that is over its limit for the current period (#688 /
	// ADR-030). Ignored when no blocking budget is over. Set by the
	// CLI's `--override-budget` flag.
	BudgetOverride bool `json:"budget_override,omitempty"`
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

// GetStage calls GET /v0/stages/{id}.
func (c *Client) GetStage(ctx context.Context, id uuid.UUID) (*Stage, error) {
	var stage Stage
	if err := c.do(ctx, http.MethodGet, "/v0/stages/"+id.String(), nil, &stage); err != nil {
		return nil, err
	}
	return &stage, nil
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

// ApprovalResult is the decoded 200 body of POST /v0/stages/{id}/
// approvals (#986). On a first submission the duplicate fields are
// absent (zero values) and Stage reflects the post-transition state.
// On a duplicate — the same subject already decided this stage —
// DuplicateSubmission is true, the prior decision stands, the stage
// state is unchanged, and the backend ran no gates and emitted no
// audit entries; PriorDecision/PriorSubmittedAt carry the EXISTING
// approval row's provenance.
// omitempty so `--output json` re-encodes a first submission without
// the keys, matching the wire's additive-only contract.
type ApprovalResult struct {
	Stage
	DuplicateSubmission bool   `json:"duplicate_submission,omitempty"`
	PriorDecision       string `json:"prior_decision,omitempty"`
	PriorSubmittedAt    string `json:"prior_submitted_at,omitempty"`
}

// SubmitApproval calls POST /v0/stages/{stage_id}/approvals.
// The response is the updated Stage with state transitioned to
// succeeded (approve) or failed (reject), plus the #986 duplicate
// labeling when the submission was a no-op re-submission.
func (c *Client) SubmitApproval(ctx context.Context, stageID uuid.UUID, in SubmitApprovalInput) (*ApprovalResult, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	var res ApprovalResult
	if err := c.do(ctx, http.MethodPost, "/v0/stages/"+stageID.String()+"/approvals", body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// SubmitReviseInput is the request body for POST /v0/stages/{id}/revise
// (#1099). Constraint is the operator's binding design constraint the
// planner must revise the prior plan to satisfy — REQUIRED.
// ForceAdditionalPass is the bounded operator override (grant ONE revise
// pass beyond the normal budget, hard-capped at 3 total passes).
type SubmitReviseInput struct {
	Constraint          string `json:"constraint"`
	ForceAdditionalPass bool   `json:"force_additional_pass,omitempty"`
}

// SubmitRevise calls POST /v0/stages/{stage_id}/revise (#1099) — the
// plan-gate revise verdict. It re-opens the plan stage parked at
// awaiting_approval, re-planning in place against the operator's binding
// constraint (injected into the re-dispatched plan prompt with the prior
// plan as the revision base), and returns the re-opened Stage (pending,
// or dispatched once the orchestrator advances it).
func (c *Client) SubmitRevise(ctx context.Context, stageID uuid.UUID, in SubmitReviseInput) (*Stage, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	var s Stage
	if err := c.do(ctx, http.MethodPost, "/v0/stages/"+stageID.String()+"/revise", body, &s); err != nil {
		return nil, err
	}
	return &s, nil
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

// ShipLocalPullRequestInput is the request body for POST /v0/runs/{run_id}/pull-request.
// Field names match the backend's pullRequestBody wire shape. BaseSHA is omitempty
// per the spec but the backend validates it as non-empty at runtime; callers must supply it.
type ShipLocalPullRequestInput struct {
	PRNumber          int    `json:"pr_number"`
	PRURL             string `json:"pr_url"`
	Branch            string `json:"branch"`
	HeadSHA           string `json:"head_sha"`
	BaseSHA           string `json:"base_sha,omitempty"`
	Title             string `json:"title"`
	Body              string `json:"body"`
	FilesChangedCount int    `json:"files_changed_count,omitempty"`
}

// ShipLocalPullRequestResult mirrors the backend's pullRequestResponse.
type ShipLocalPullRequestResult struct {
	ID          uuid.UUID `json:"id"`
	StageID     uuid.UUID `json:"stage_id"`
	ContentHash string    `json:"content_hash"`
	PRNumber    int       `json:"pr_number"`
	PRURL       string    `json:"pr_url"`
	HeadSHA     string    `json:"head_sha"`
	Idempotent  bool      `json:"idempotent"`
}

// ShipLocalPullRequest calls POST /v0/runs/{run_id}/pull-request?stage_id={stage_id}.
// Auth is bearer-token only; no X-Fishhawk-Signature is sent.
func (c *Client) ShipLocalPullRequest(ctx context.Context, runID, stageID uuid.UUID, in ShipLocalPullRequestInput) (*ShipLocalPullRequestResult, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	path := "/v0/runs/" + runID.String() + "/pull-request?stage_id=" + stageID.String()
	var res ShipLocalPullRequestResult
	if err := c.do(ctx, http.MethodPost, path, body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// ScopeAmendmentPath is one path entry of a scope amendment: the
// repo-relative path plus its operation (create | modify | delete).
// Mirrors the backend scopeamendment.PathEntry wire shape.
type ScopeAmendmentPath struct {
	Path      string `json:"path"`
	Operation string `json:"operation"`
}

// ScopeAmendment is the CLI-side projection of the backend
// scopeAmendmentResponse wire shape (one mid-stage scope-amendment
// request). Field names + types match the wire shape verbatim. Only
// the fields the auto-decider reads are projected; the #983 headroom
// fields are intentionally omitted (additive, ignored on decode).
type ScopeAmendment struct {
	ID             uuid.UUID            `json:"id"`
	RunID          uuid.UUID            `json:"run_id"`
	StageID        uuid.UUID            `json:"stage_id"`
	Paths          []ScopeAmendmentPath `json:"paths"`
	Reason         string               `json:"reason"`
	Status         string               `json:"status"`
	DecisionReason *string              `json:"decision_reason,omitempty"`
	DecidedBy      *string              `json:"decided_by,omitempty"`
	RequestedAt    time.Time            `json:"requested_at"`
	DecidedAt      *time.Time           `json:"decided_at,omitempty"`
}

// scopeAmendmentListResult is the GET list envelope.
type scopeAmendmentListResult struct {
	Items []ScopeAmendment `json:"items"`
}

// ListScopeAmendments calls GET /v0/runs/{run_id}/scope-amendments,
// decoding the {items:[...]} envelope. waitSeconds>0 adds the opt-in
// bounded server-side long-poll (?wait=<n>, #1035) so a newly-decided
// amendment surfaces promptly; <=0 omits the query for the unchanged
// single-list behavior. The backend clamps ?wait above its own cap,
// so any positive value is safe.
func (c *Client) ListScopeAmendments(ctx context.Context, runID uuid.UUID, waitSeconds int) ([]ScopeAmendment, error) {
	path := "/v0/runs/" + runID.String() + "/scope-amendments"
	if waitSeconds > 0 {
		path += "?wait=" + strconv.Itoa(waitSeconds)
	}
	var res scopeAmendmentListResult
	if err := c.do(ctx, http.MethodGet, path, nil, &res); err != nil {
		return nil, err
	}
	return res.Items, nil
}

// scopeAmendmentDecisionBody is the POST decision request body,
// matching the backend scopeAmendmentDecisionRequest wire shape.
type scopeAmendmentDecisionBody struct {
	Decision string `json:"decision"` // approve | deny
	Reason   string `json:"reason"`
}

// DecideScopeAmendment calls POST
// /v0/runs/{run_id}/scope-amendments/{amendment_id}/decision with
// {decision, reason}. An already-decided amendment surfaces as
// *APIError (409, code amendment_already_decided) so callers can
// distinguish a benign race from a real failure.
func (c *Client) DecideScopeAmendment(ctx context.Context, runID, amendmentID uuid.UUID, decision, reason string) (*ScopeAmendment, error) {
	body, err := json.Marshal(scopeAmendmentDecisionBody{Decision: decision, Reason: reason})
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	path := "/v0/runs/" + runID.String() + "/scope-amendments/" + amendmentID.String() + "/decision"
	var res ScopeAmendment
	if err := c.do(ctx, http.MethodPost, path, body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Artifact is the CLI-side projection of the OpenAPI Artifact schema.
// Content is left as raw JSON so the caller decodes only the shape it
// needs (the auto-decider decodes the standard_v1 plan's scope.files).
type Artifact struct {
	ID            uuid.UUID       `json:"id"`
	StageID       uuid.UUID       `json:"stage_id"`
	Kind          string          `json:"kind"`
	SchemaVersion *string         `json:"schema_version"`
	ContentHash   string          `json:"content_hash"`
	Content       json.RawMessage `json:"content,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

// listArtifactsResult is the GET artifacts list envelope.
type listArtifactsResult struct {
	Items []Artifact `json:"items"`
}

// ListStageArtifacts calls GET /v0/stages/{stage_id}/artifacts.
// Artifacts come back ordered by created_at ascending.
func (c *Client) ListStageArtifacts(ctx context.Context, stageID uuid.UUID) ([]Artifact, error) {
	var res listArtifactsResult
	if err := c.do(ctx, http.MethodGet, "/v0/stages/"+stageID.String()+"/artifacts", nil, &res); err != nil {
		return nil, err
	}
	return res.Items, nil
}

// RollbackDeploymentResult is the decoded 202 body of POST
// /v0/runs/{run_id}/deployment/rollback. Field names + types match the
// backend rollbackResponse wire shape (backend/internal/server/deploy_rollback.go)
// verbatim: the run + deploy stage the rollback targets, the resolved external
// rollback handle (GHARunID/ExternalRunURL, omitempty when the dispatch endpoint
// returns no run id yet, e.g. a github_actions dispatch whose run isn't visible),
// and the human Message explaining when the rolled_back outcome is recorded.
type RollbackDeploymentResult struct {
	RunID          uuid.UUID `json:"run_id"`
	StageID        uuid.UUID `json:"stage_id"`
	Target         string    `json:"target"`
	GHARunID       int64     `json:"gha_run_id,omitempty"`
	ExternalRunURL string    `json:"external_run_url,omitempty"`
	Message        string    `json:"message"`
}

// RollbackDeployment calls POST /v0/runs/{run_id}/deployment/rollback (no
// request body) — the operator-triggered rollback sub-action for a delegating
// deploy (ADR-038 / #1386). The endpoint re-dispatches the same external
// pipeline down its rollback path and returns the rollback run handle (202).
// Server-side preconditions surface as *APIError verbatim: a deploy stage that
// has not settled (409 deploy_not_settled), a run whose cached spec carries no
// delegating deploy stage (422 rollback_unconfigured), or a token missing the
// write:deploy scope (403 insufficient_scope).
func (c *Client) RollbackDeployment(ctx context.Context, runID uuid.UUID) (*RollbackDeploymentResult, error) {
	var res RollbackDeploymentResult
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+runID.String()+"/deployment/rollback", nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Campaign is the CLI-side projection of the OpenAPI Campaign schema
// (ADR-047 / #1437): the parent record of an epic-driven multi-issue
// run. Field names + types match the wire shape verbatim.
type Campaign struct {
	ID          uuid.UUID `json:"id"`
	Repo        string    `json:"repo"`
	EpicRef     string    `json:"epic_ref"`
	State       string    `json:"state"`
	PausePolicy string    `json:"pause_policy"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// CampaignPauseReason explains why a paused item was handed off to a
// human by the auto-driver (E25.7). Present only while an item is — or
// was — paused. Mirrors the OpenAPI CampaignItem.pause_reason sub-shape.
type CampaignPauseReason struct {
	PageEvent string     `json:"page_event,omitempty"`
	RunID     *uuid.UUID `json:"run_id,omitempty"`
	StageID   *uuid.UUID `json:"stage_id,omitempty"`
	Gate      string     `json:"gate,omitempty"`
}

// CampaignItem is the CLI-side projection of the OpenAPI CampaignItem
// schema: one issue within a campaign. RunID is nil until a run is
// assigned; PauseReason is nil unless the item is/was paused (E25.7).
type CampaignItem struct {
	ID          uuid.UUID            `json:"id"`
	IssueRef    string               `json:"issue_ref"`
	DependsOn   []string             `json:"depends_on"`
	RunID       *uuid.UUID           `json:"run_id,omitempty"`
	State       string               `json:"state"`
	PauseReason *CampaignPauseReason `json:"pause_reason,omitempty"`
	CreatedAt   time.Time            `json:"created_at"`
	UpdatedAt   time.Time            `json:"updated_at"`
}

// CampaignRollup is the engine's readiness partition over a campaign's
// items. Every slice holds issue refs; an item appears in exactly one
// slice. Each field is always an array (never null) on the wire.
type CampaignRollup struct {
	Eligible  []string `json:"eligible"`
	Blocked   []string `json:"blocked"`
	Running   []string `json:"running"`
	Done      []string `json:"done"`
	Failed    []string `json:"failed"`
	Cancelled []string `json:"cancelled"`
	Paused    []string `json:"paused"`
}

// CampaignNextAction is the single next step for the operator-agent,
// distilled from the rollup. IssueRef + Detail are omitted for the
// wait/complete actions per the OpenAPI shape.
type CampaignNextAction struct {
	Action   string `json:"action"`
	IssueRef string `json:"issue_ref,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// CampaignStatus is the campaign rollup surface: the campaign, its
// items, the engine's readiness partition, and the distilled next
// action. The surface the operator-agent polls to drive a campaign.
type CampaignStatus struct {
	Campaign   Campaign           `json:"campaign"`
	Items      []CampaignItem     `json:"items"`
	Rollup     CampaignRollup     `json:"rollup"`
	NextAction CampaignNextAction `json:"next_action"`
}

// CreateCampaignInput is what CreateCampaign marshals into the POST
// /v0/campaigns body. PausePolicy is optional — empty omits the field
// so the backend applies its `pause_campaign` default.
//
// OperatorAgent is the OPTIONAL campaign-level operator_agent override
// (E25.12 / #1451), mirroring the backend campaignCreateRequest and the
// MCP apiClient request struct. It is carried as raw JSON bytes: a nil
// field omits the key (each issue-run inherits its workflow default),
// while an explicit `[]byte("{}")` is a valid wholesale override that
// must reach the wire (page on every action). json.RawMessage's
// omitempty drops only a nil/zero-length value, so this preserves the
// explicit-empty-`{}` vs omitted distinction the MCP path carries after
// #1470/#1475.
type CreateCampaignInput struct {
	Repo          string          `json:"repo"`
	EpicRef       string          `json:"epic_ref"`
	PausePolicy   string          `json:"pause_policy,omitempty"`
	OperatorAgent json.RawMessage `json:"operator_agent,omitempty"`
}

// CreateCampaign calls POST /v0/campaigns. The 201 body is the created
// Campaign. Server-side rejections (validation_failed 400,
// insufficient_scope 403, repo_not_installed 422, …) surface as
// *APIError with the envelope's code.
func (c *Client) CreateCampaign(ctx context.Context, in CreateCampaignInput) (*Campaign, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	var camp Campaign
	if err := c.do(ctx, http.MethodPost, "/v0/campaigns", body, &camp); err != nil {
		return nil, err
	}
	return &camp, nil
}

// GetCampaignStatus calls GET /v0/campaigns/{id}/status, decoding the
// campaign + items + rollup + next_action surface.
func (c *Client) GetCampaignStatus(ctx context.Context, id uuid.UUID) (*CampaignStatus, error) {
	var st CampaignStatus
	if err := c.do(ctx, http.MethodGet, "/v0/campaigns/"+id.String()+"/status", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// ListCampaignsFilter scopes a ListCampaigns call. Empty values are
// dropped from the query string; the repo and state filters AND
// together server-side.
type ListCampaignsFilter struct {
	Repo   string
	State  string
	Limit  int
	Cursor string
}

// ListCampaignsResult is the paginated response envelope (the same
// {items, next_cursor} cursor-pagination shape as ListRuns).
type ListCampaignsResult struct {
	Items      []Campaign `json:"items"`
	NextCursor string     `json:"next_cursor"`
}

// ListCampaigns calls GET /v0/campaigns with optional filters and
// cursor. Campaigns come back ordered by created_at descending.
func (c *Client) ListCampaigns(ctx context.Context, f ListCampaignsFilter) (*ListCampaignsResult, error) {
	q := url.Values{}
	if f.Repo != "" {
		q.Set("repo", f.Repo)
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
	path := "/v0/campaigns"
	if encoded := q.Encode(); encoded != "" {
		path = path + "?" + encoded
	}
	var res ListCampaignsResult
	if err := c.do(ctx, http.MethodGet, path, nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// ResumeCampaign calls POST /v0/campaigns/{id}/resume (no request
// body) — the operator's hand-back after the auto-driver paged a human
// at a run gate (E25.7). The 200 body is the resumed Campaign (now
// running). "Nothing to resume" surfaces as *APIError (409
// campaign_not_paused); a token missing write:campaigns surfaces as 403
// insufficient_scope.
func (c *Client) ResumeCampaign(ctx context.Context, id uuid.UUID) (*Campaign, error) {
	var camp Campaign
	if err := c.do(ctx, http.MethodPost, "/v0/campaigns/"+id.String()+"/resume", nil, &camp); err != nil {
		return nil, err
	}
	return &camp, nil
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
