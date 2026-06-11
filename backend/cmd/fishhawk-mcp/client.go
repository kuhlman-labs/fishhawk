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
	ID                 string        `json:"id"`
	Repo               string        `json:"repo"`
	WorkflowID         string        `json:"workflow_id"`
	WorkflowSHA        string        `json:"workflow_sha"`
	TriggerSource      string        `json:"trigger_source"`
	TriggerRef         *string       `json:"trigger_ref"`
	State              string        `json:"state"`
	ParentRunID        *string       `json:"parent_run_id"`
	PullRequestURL     *string       `json:"pull_request_url"`
	RetryAttempt       int           `json:"retry_attempt"`
	MaxRetriesSnapshot int           `json:"max_retries_snapshot"`
	RunnerKind         string        `json:"runner_kind,omitempty"`
	IssueContext       *IssueContext `json:"issue_context,omitempty"`
	// Concerns is the run's OPEN review-concern summary (#964), mirrored
	// from GET /v0/runs/{run_id}: count, per-state breakdown, and the
	// stable concern IDs fishhawk_fixup_stage's concern_ids addressing
	// needs. The backend emits it on the single-run read only; omitted
	// when the run has no open concerns.
	Concerns  *RunConcerns `json:"concerns,omitempty" jsonschema:"OPEN review concerns for the run: open count, by_state breakdown, and items carrying the stable concern IDs fishhawk_fixup_stage's concern_ids parameter addresses. Omitted when nothing is open"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
}

// RunConcerns mirrors the backend's run-status concerns block (#964):
// the run's OPEN review concerns (states raised, addressed_pending,
// reopened). Note text is intentionally elided — read the originating
// *_reviewed audit entry for the full note.
type RunConcerns struct {
	Open    int              `json:"open" jsonschema:"number of open review concerns on the run"`
	ByState map[string]int   `json:"by_state,omitempty" jsonschema:"open-concern count per lifecycle state (raised, addressed_pending, reopened)"`
	Items   []RunConcernItem `json:"items,omitempty" jsonschema:"the open concerns; each carries the stable id to pass to fishhawk_fixup_stage concern_ids"`
}

// RunConcernItem is one open concern. ID is the stable server-minted
// UUID — the primary fix-up addressing scheme (positional indices are
// deprecated).
type RunConcernItem struct {
	ID        string `json:"id" jsonschema:"stable concern UUID — pass these to fishhawk_fixup_stage concern_ids"`
	StageKind string `json:"stage_kind" jsonschema:"plan or implement; only implement-stage concerns can be routed into an implement fix-up"`
	Severity  string `json:"severity"`
	Category  string `json:"category"`
	State     string `json:"state" jsonschema:"raised, addressed_pending, or reopened"`
}

// IssueContext mirrors the OpenAPI shape: the GitHub issue payload
// fetched at run-create and cached on the run row (#415). The MCP
// server populates this from `gh issue view` when an agent passes
// the `issue` input — same pattern the CLI uses.
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

type listRunsResult struct {
	Items      []Run  `json:"items"`
	NextCursor string `json:"next_cursor"`
}

// listRunsFilter scopes a runs query. Empty values drop from the
// query string. The MCP server consumes this for two surfaces:
// `get_active_run`'s resolver (which uses repo, pull_request_url,
// trigger_ref) and `list_runs`'s broader enumeration (which adds
// workflow_id, state, and cursor pagination). Same struct, both
// callers, no separate types.
type listRunsFilter struct {
	Repo           string
	PullRequestURL string
	TriggerRef     string
	WorkflowID     string
	State          string
	Limit          int
	Cursor         string
}

func (c *apiClient) GetRun(ctx context.Context, id uuid.UUID) (*Run, error) {
	var r Run
	if err := c.do(ctx, http.MethodGet, "/v0/runs/"+id.String(), nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// BudgetStatus mirrors the backend's GET /v0/runs/{run_id}/budget body
// (`backend/internal/server/budget_status.go::budgetStatusResponse`):
// the current calendar-period status of the run's workflow periodic
// budget (#693 / ADR-030). DISPLAY-ONLY — surfaced in the tool outputs
// so the operator sees spend-vs-limit every stage; it never gates a run.
//
// Repeated here rather than imported because the MCP server's apiClient
// is a thin local copy (the import direction is `cli → backend`, not the
// reverse). The scalar fields are omitempty so the no-budget path — which
// GetRunBudget collapses to a nil pointer — never marshals a half-empty
// block.
type BudgetStatus struct {
	Period      string   `json:"period"`
	PeriodStart string   `json:"period_start,omitempty"`
	LimitUSD    float64  `json:"limit_usd,omitempty"`
	SpentUSD    float64  `json:"spent_usd,omitempty"`
	Fraction    float64  `json:"fraction,omitempty"`
	WarnAt      *float64 `json:"warn_at,omitempty"`
	Tier        string   `json:"tier,omitempty"`
	Enforcement string   `json:"enforcement,omitempty"`
}

// GetRunBudget fetches the run's workflow periodic-budget status. The
// backend returns 200 with an empty object when no budget is configured;
// GetRunBudget collapses that to (nil, nil) so every caller treats "no
// budget" uniformly by checking for a nil pointer.
func (c *apiClient) GetRunBudget(ctx context.Context, runID uuid.UUID) (*BudgetStatus, error) {
	var b BudgetStatus
	if err := c.do(ctx, http.MethodGet, "/v0/runs/"+runID.String()+"/budget", nil, &b); err != nil {
		return nil, err
	}
	if b.Period == "" {
		return nil, nil
	}
	return &b, nil
}

// createRunRequest mirrors the backend's `POST /v0/runs` request body
// (`backend/internal/server/runs.go::createRunRequest`). Repeated here
// rather than imported because the MCP server's apiClient is
// deliberately a thin local copy — the import-direction rule is
// `cli → backend`, not the other way around, and the same applies
// to this binary.
type createRunRequest struct {
	Repo           string        `json:"repo"`
	WorkflowID     string        `json:"workflow_id"`
	WorkflowSHA    string        `json:"workflow_sha"`
	TriggerSource  string        `json:"trigger_source"`
	TriggerRef     *string       `json:"trigger_ref,omitempty"`
	RunnerKind     string        `json:"runner_kind,omitempty"`
	WorkflowSpec   string        `json:"workflow_spec,omitempty"`
	IssueContext   *IssueContext `json:"issue_context,omitempty"`
	BudgetOverride bool          `json:"budget_override,omitempty"`
}

// StartRunParams is the typed input the apiClient takes for run
// creation. `IdempotencyKey` is optional and travels in the HTTP
// header per the backend's E8.2 contract — when set, a previously-
// created run with the same `(repo, key)` returns 200 instead of a
// fresh 201.
//
// RunnerKind / WorkflowSpec / IssueContext mirror the CLI's
// CreateRunInput surface (#411, #415, ADR-022) so an agent calling
// fishhawk_start_run via MCP has the same composition reach the
// CLI's `fishhawk run start` does.
type StartRunParams struct {
	Repo           string
	WorkflowID     string
	WorkflowSHA    string
	TriggerSource  string
	TriggerRef     string
	IdempotencyKey string
	RunnerKind     string
	WorkflowSpec   string
	IssueContext   *IssueContext
	BudgetOverride bool
}

// approvalRequest mirrors the backend's
// `POST /v0/stages/{stage_id}/approvals` body
// (`backend/internal/server/approvals.go::approvalRequest`).
type approvalRequest struct {
	Decision string `json:"decision"`
	Comment  string `json:"comment,omitempty"`
	// ApproverGithubLogin is the resolved GitHub login of the acting
	// operator (#751), threaded through so the issue-thread status
	// footer `@`-mentions the real login rather than the raw token
	// subject. Omitempty: SPA/CLI callers omit it and stay unaffected.
	ApproverGithubLogin string `json:"approver_github_login,omitempty"`
	// AddScopeFiles is the structured, authoritative scope amendment (#824):
	// repo-relative paths to fold into the implement stage's scope.files on
	// approve. A trailing slash marks a directory. The DisallowUnknownFields
	// decoder on the backend requires the field be declared here too; reject
	// and conditionless approve callers pass nil (omitempty).
	AddScopeFiles []string `json:"add_scope_files,omitempty"`
}

// SubmitApproval posts an approve or reject decision against the
// given stage. `decision` must be "approve" or "reject"; `comment`
// is optional but recommended on rejects (the CLI emits a warning
// when missing). `approverGithubLogin` is the resolved GitHub login
// of the acting operator (#751) — empty when gh resolution was
// unavailable; the backend records it in the approval audit payload
// for issue-thread `@`-mention rendering while keeping the token
// subject as the provenance identity. `addScopeFiles` is the structured
// scope amendment (#824) folded into the implement stage's scope.files on
// approve; nil on reject and conditionless approve. Returns the updated Stage. 4xx
// surfaces:
//   - 400 validation_failed (decision other than approve/reject)
//   - 404 stage_not_found
//   - 409 review_stage_managed_by_github (review-stage approvals
//     live on GitHub per ADR-018; not relevant for the MCP plan-
//     approval tools but the wrapper surfaces the code if a future
//     caller reaches this method with a review-stage id)
//   - 409 agent_review_pending (ADR-036: a configured agent plan
//     review is still in-flight; retryable once the review reaches
//     any terminal state — plan_reviewed / plan_review_failed /
//     plan_review_skipped. details carry configured_agents +
//     landed_terminal)
//   - 422 plan_violates_budget (plan predicted runtime exceeds the
//     implement-stage budget; decompose or --override-budget)
//   - 422 plan_violates_scope_cap (#983: effective scope.files — plan
//     scope plus add_scope_files — exceeds the implement stage's
//     max_files_changed; re-scope the plan or include
//     --override-scope-cap in the comment)
func (c *apiClient) SubmitApproval(ctx context.Context, stageID uuid.UUID, decision, comment, approverGithubLogin string, addScopeFiles []string) (*Stage, error) {
	body, err := json.Marshal(approvalRequest{
		Decision:            decision,
		Comment:             comment,
		ApproverGithubLogin: approverGithubLogin,
		AddScopeFiles:       addScopeFiles,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal approval: %w", err)
	}
	var s Stage
	if err := c.do(ctx, http.MethodPost, "/v0/stages/"+stageID.String()+"/approvals", body, &s); err != nil {
		return nil, err
	}
	return &s, nil
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

// fixupRequest mirrors the backend's
// `POST /v0/stages/{stage_id}/fixup` body
// (`backend/internal/server/fixup.go::fixupRequest`). ConcernIDs is the
// PRIMARY addressing scheme (#964): stable concern UUIDs from the run's
// concerns block. Concerns (positional indices into the stage's
// flattened resolved concern set) is DEPRECATED and only valid when
// ConcernIDs is absent — the backend rejects supplying both. Reason is
// an optional operator note recorded on the audit entry.
type fixupRequest struct {
	ConcernIDs []string `json:"concern_ids,omitempty"`
	Concerns   []int    `json:"concerns,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	// AllowCreate declares net-new files the fix-up will create (#823),
	// folded into the effective scope.files for that pass only. omitempty:
	// the common fix-up omits it and stays unaffected.
	AllowCreate []string `json:"allow_create,omitempty"`
	// ForceAdditionalPass is the bounded operator override (#860): grant ONE
	// fix-up pass beyond the normal budget, hard-capped at 3 total passes.
	// omitempty: the common fix-up omits it and stays on the default budget.
	ForceAdditionalPass bool `json:"force_additional_pass,omitempty"`
}

// FixupStage routes one or more advisory implement-review concerns back
// to the implement agent for a bounded, operator-gated fix-up pass via
// `POST /v0/stages/{stage_id}/fixup`. Distinct from RetryStage: fix-up
// re-opens a HEALTHY review gate, commits onto the SAME PR branch, and
// is bounded (default one pass). It applies in either flow: the implement
// stage parked at its own gate (awaiting_approval → pending), or a
// succeeded implement stage whose run still holds a separate review stage
// at awaiting_approval (succeeded → pending, the review stage re-parked
// alongside — the push_and_open_pr flow, #780). Returns the re-opened
// Stage row (pending, or dispatched once the orchestrator advances it
// before the response returns). 4xx surfaces:
//   - 400 validation_failed (no concern selection / both concern_ids and
//     indices supplied / out-of-range index / unknown, foreign,
//     plan-stage, or non-open concern_id)
//   - 403 cross_run_fixup (a run-bound token reaching another run's stage)
//   - 404 stage_not_found
//   - 422 fixup_not_applicable (no recorded approve_with_concerns verdict,
//     or the stage is not at the gate / its review gate already resolved)
//   - 422 fixup_budget_exhausted (the NORMAL bounded pass count is spent;
//     details carry max_passes + used — one more pass is still available
//     via forceAdditionalPass below)
//   - 422 fixup_ceiling_reached (the hard ceiling of 3 total passes is
//     reached; the override cannot push past it — merge-with-follow-up or a
//     fresh run; details carry ceiling + used)
//
// allowCreate declares net-new files this pass will create (#823), folded
// into the effective scope.files for that dispatch only; an invalid entry
// (absolute / containing "..") surfaces 400 validation_failed.
// forceAdditionalPass is the bounded operator override (#860): grant ONE
// pass beyond the normal budget, hard-capped at 3 total passes.
func (c *apiClient) FixupStage(ctx context.Context, id uuid.UUID, concernIDs []string, concerns []int, reason string, allowCreate []string, forceAdditionalPass bool) (*Stage, error) {
	body, err := json.Marshal(fixupRequest{ConcernIDs: concernIDs, Concerns: concerns, Reason: reason, AllowCreate: allowCreate, ForceAdditionalPass: forceAdditionalPass})
	if err != nil {
		return nil, fmt.Errorf("marshal fixup: %w", err)
	}
	var s Stage
	if err := c.do(ctx, http.MethodPost, "/v0/stages/"+id.String()+"/fixup", body, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// resetBranchRequest mirrors the backend's
// `POST /v0/runs/{run_id}/reset-branch` body
// (`backend/internal/server/reset_branch.go::resetBranchRequest`).
// Confirm MUST be true — the reset is destructive (force-rewinds the PR
// head ref), so the backend refuses a missing/false confirm with 400.
type resetBranchRequest struct {
	Reason  string `json:"reason,omitempty"`
	Confirm bool   `json:"confirm"`
}

// ResetBranchResult mirrors the backend's reset-branch 200 body: the
// summary of a successful rewind. Surfaced back to the operator so the
// dropped commit + recovery path are visible.
type ResetBranchResult struct {
	RunID                 string `json:"run_id"`
	PRNumber              int    `json:"pr_number"`
	Branch                string `json:"branch"`
	DroppedOffendingSHA   string `json:"dropped_offending_sha"`
	ResetToSHA            string `json:"reset_to_sha"`
	PriorHeadSHA          string `json:"prior_head_sha"`
	ReparkedReviewStageID string `json:"reparked_review_stage_id,omitempty"`
	RecoveryNote          string `json:"recovery_note"`
}

// ResetRunBranch force-rewinds a run/PR branch back to its last
// run-authored HEAD, dropping a foreign commit pushed ON TOP of the run's
// commits (ADR-035 remediation, #867), via
// `POST /v0/runs/{run_id}/reset-branch`. Destructive + operator-gated:
// confirm is always sent true (the tool layer requires the operator's
// confirm). 4xx/5xx surfaces:
//   - 400 confirmation_required (confirm not true)
//   - 403 cross_run_reset (a run-bound token reaching another run's branch)
//   - 404 run_not_found
//   - 422 reset_out_of_scope (the foreign commit is an ancestor, not on
//     top — owned by prevention #861/#865)
//   - 422 reset_not_applicable (the tip is already the last run-authored
//     HEAD; nothing on top to drop)
//   - 422 reset_not_determinable (fail-closed: the lineage could not be
//     classified with certainty, or the lease re-check failed)
func (c *apiClient) ResetRunBranch(ctx context.Context, runID uuid.UUID, reason string) (*ResetBranchResult, error) {
	body, err := json.Marshal(resetBranchRequest{Reason: reason, Confirm: true})
	if err != nil {
		return nil, fmt.Errorf("marshal reset-branch: %w", err)
	}
	var res ResetBranchResult
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+runID.String()+"/reset-branch", body, &res); err != nil {
		return nil, err
	}
	return &res, nil
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
		Repo:           p.Repo,
		WorkflowID:     p.WorkflowID,
		WorkflowSHA:    p.WorkflowSHA,
		TriggerSource:  p.TriggerSource,
		RunnerKind:     p.RunnerKind,
		WorkflowSpec:   p.WorkflowSpec,
		IssueContext:   p.IssueContext,
		BudgetOverride: p.BudgetOverride,
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
	// SinceSequence narrows the response to entries with sequence
	// strictly greater than this value (#962) — the anchor the
	// fishhawk_await_audit primitive polls from. Zero drops from the
	// query string (the server treats 0 as a no-op anyway).
	SinceSequence int64
	Limit         int
	Cursor        string
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
	if f.SinceSequence > 0 {
		q.Set("since_sequence", strconv.FormatInt(f.SinceSequence, 10))
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

// CalibrationParams scopes a GET /v0/calibration request. Empty
// fields drop from the query string; StageType defaults to "implement"
// server-side when omitted.
type CalibrationParams struct {
	WorkflowID string
	StageType  string
	Since      string
}

// CalibrationResult mirrors the /v0/calibration response body.
// ConfidenceBandAccuracy is typed as map[string]any so the MCP
// SDK's schema reflection sees an unconstrained object — the
// per-level bucket shape (samples + within_1.5x) is stable but
// the confidence keys ('low', 'medium', 'high') are a variable set.
type CalibrationResult struct {
	WorkflowID             string         `json:"workflow_id,omitempty"`
	StageType              string         `json:"stage_type"`
	Samples                int            `json:"samples"`
	PredictedP50Minutes    float64        `json:"predicted_p50_minutes"`
	ActualP50Minutes       float64        `json:"actual_p50_minutes"`
	ActualP95Minutes       float64        `json:"actual_p95_minutes"`
	CalibrationRatio       float64        `json:"calibration_ratio"`
	ConfidenceBandAccuracy map[string]any `json:"confidence_band_accuracy"`
}

// GetCalibration calls GET /v0/calibration. Returns aggregate runtime
// statistics across all runtime_observed audit entries that match the
// supplied filters. An empty CalibrationParams returns stats across all
// implement stages.
func (c *apiClient) GetCalibration(ctx context.Context, p CalibrationParams) (*CalibrationResult, error) {
	q := url.Values{}
	if p.WorkflowID != "" {
		q.Set("workflow_id", p.WorkflowID)
	}
	if p.StageType != "" {
		q.Set("stage_type", p.StageType)
	}
	if p.Since != "" {
		q.Set("since", p.Since)
	}
	path := "/v0/calibration"
	if encoded := q.Encode(); encoded != "" {
		path = path + "?" + encoded
	}
	var res CalibrationResult
	if err := c.do(ctx, http.MethodGet, path, nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
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
