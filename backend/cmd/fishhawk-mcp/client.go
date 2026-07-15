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
	// http is the 30s short client for every read/decide/file/direct-edit
	// arm — bodies return in well under a second.
	http *http.Client
	// httpLong is the minutes-long client for the two agent-backed refinement
	// arms (create-session, brief-amendment). A drafting-agent call is a
	// multi-minute LLM inference, so the 30s short client aborts it mid-flight
	// (aborting the request context and, server-side, killing the drafter).
	httpLong *http.Client
}

// refinementDraftClientTimeout bounds the MCP client's wait on the two
// agent-backed refinement arms (open + brief_amendment). It is set a couple of
// minutes ABOVE the server-side drafting budget (refinementDraftBudget, 20m in
// backend/internal/server/refinement.go) so the client waits for the server's
// own bounded response/error instead of aborting first. The 20m server budget
// is anchored to planreview's review-budget Cap (1200s /
// backend/internal/planreview/budget.go).
const refinementDraftClientTimeout = 22 * time.Minute

func newAPIClient(cfg config) *apiClient {
	return &apiClient{
		baseURL:  cfg.backendURL,
		token:    cfg.apiToken,
		http:     &http.Client{Timeout: 30 * time.Second},
		httpLong: &http.Client{Timeout: refinementDraftClientTimeout},
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
	var base string
	if e.Code == "" {
		base = fmt.Sprintf("fishhawk: HTTP %d", e.StatusCode)
	} else {
		base = fmt.Sprintf("fishhawk: HTTP %d (%s): %s", e.StatusCode, e.Code, e.Message)
	}
	// Append the parsed Details map when present so callers that render the
	// error via %v (e.g. run_children's between-wave integrate-wave transport
	// warning) surface details.error — the real cause — instead of an opaque
	// HTTP-status stop. encoding/json marshals map keys in sorted order, so
	// the suffix is deterministic for tests. A nil/empty map appends nothing.
	if len(e.Details) > 0 {
		if b, err := json.Marshal(e.Details); err == nil {
			base += "; details: " + string(b)
		}
	}
	return base
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
	ID                 string  `json:"id"`
	Repo               string  `json:"repo"`
	WorkflowID         string  `json:"workflow_id"`
	WorkflowSHA        string  `json:"workflow_sha"`
	TriggerSource      string  `json:"trigger_source"`
	TriggerRef         *string `json:"trigger_ref"`
	State              string  `json:"state"`
	ParentRunID        *string `json:"parent_run_id"`
	UpstreamRunID      *string `json:"upstream_run_id,omitempty"`
	PullRequestURL     *string `json:"pull_request_url"`
	RetryAttempt       int     `json:"retry_attempt"`
	MaxRetriesSnapshot int     `json:"max_retries_snapshot"`
	RunnerKind         string  `json:"runner_kind,omitempty"`
	// RunnerKindResolved mirrors GET /v0/runs/{id}'s lock flag (#1355):
	// true once the run's first signed runner self-report LOCKED runner_kind
	// (#1346/#1348). The host-dispatch guard (guardHostDispatch) reads it to
	// reject a local host dispatch against a github_actions-locked run.
	RunnerKindResolved bool          `json:"runner_kind_resolved,omitempty"`
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
	// AckRequired mirrors the backend's escalation boolean (#1371): true
	// once period spend reaches the configured ack multiple (the
	// ack_required or page tier), signalling that a plan-approval gate now
	// requires an explicit --ack-budget acknowledgment. omitempty so the
	// no-budget / sub-ack path stays byte-identical to today.
	AckRequired bool   `json:"ack_required,omitempty"`
	Enforcement string `json:"enforcement,omitempty"`
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

// CacheEfficiency mirrors the backend's GET
// /v0/runs/{run_id}/cache-efficiency body
// (`backend/internal/server/cache_efficiency.go::cacheEfficiencyResponse`):
// the per-run prompt-cache efficiency metric derived from the run's
// cost_recorded ledger (ADR-044 slice 3 / #1352). DISPLAY-ONLY — surfaced
// in fishhawk_get_run_status so the operator sees cache-hit usage and the
// net dollar effect; it never gates a run.
//
// Repeated here rather than imported because the MCP server's apiClient is
// a thin local copy (the import direction is `cli → backend`, not the
// reverse). The scalar fields are omitempty so the no-data path — which
// GetRunCacheEfficiency collapses to a nil pointer — never marshals a
// half-empty block.
type CacheEfficiency struct {
	FreshInputTokens    int                    `json:"fresh_input_tokens,omitempty"`
	CacheReadTokens     int                    `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens    int                    `json:"cache_write_tokens,omitempty"`
	OutputTokens        int                    `json:"output_tokens,omitempty"`
	CacheReadRatio      float64                `json:"cache_read_ratio,omitempty"`
	ReuseFactor         float64                `json:"reuse_factor,omitempty"`
	GrossReadSavingsUSD float64                `json:"gross_read_savings_usd,omitempty"`
	WritePenaltyUSD     float64                `json:"write_penalty_usd,omitempty"`
	NetSavingsUSD       float64                `json:"net_savings_usd,omitempty"`
	Stages              []CacheEfficiencyStage `json:"stages,omitempty"`
}

// CacheEfficiencyStage is the per-source breakdown row (plan_review /
// implement_review / agent).
type CacheEfficiencyStage struct {
	Source              string  `json:"source"`
	FreshInputTokens    int     `json:"fresh_input_tokens,omitempty"`
	CacheReadTokens     int     `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens    int     `json:"cache_write_tokens,omitempty"`
	OutputTokens        int     `json:"output_tokens,omitempty"`
	CacheReadRatio      float64 `json:"cache_read_ratio,omitempty"`
	ReuseFactor         float64 `json:"reuse_factor,omitempty"`
	GrossReadSavingsUSD float64 `json:"gross_read_savings_usd,omitempty"`
	WritePenaltyUSD     float64 `json:"write_penalty_usd,omitempty"`
	NetSavingsUSD       float64 `json:"net_savings_usd,omitempty"`
}

// GetRunCacheEfficiency fetches the run's cache-efficiency metric. The
// backend returns 200 with an empty object when the run has no cost data;
// GetRunCacheEfficiency collapses that to (nil, nil) so every caller treats
// "no data" uniformly by checking for a nil pointer. The presence sentinel
// is "all token buckets zero AND no stages" — analogous to budget's
// Period=="" check; a real run always reports output tokens, so the empty
// object never false-collapses a real metric.
func (c *apiClient) GetRunCacheEfficiency(ctx context.Context, runID uuid.UUID) (*CacheEfficiency, error) {
	var ce CacheEfficiency
	if err := c.do(ctx, http.MethodGet, "/v0/runs/"+runID.String()+"/cache-efficiency", nil, &ce); err != nil {
		return nil, err
	}
	if ce.FreshInputTokens == 0 && ce.CacheReadTokens == 0 && ce.CacheWriteTokens == 0 &&
		ce.OutputTokens == 0 && len(ce.Stages) == 0 {
		return nil, nil
	}
	return &ce, nil
}

// RunCost mirrors the backend's GET /v0/runs/{run_id}/cost body
// (`backend/internal/server/cost.go::costSummaryResponse`): the per-run
// estimated cost derived from the run's cost_recorded ledger, a per-stage
// (agent / plan_review / implement_review) breakdown, and — when the run
// resolved to a merged PR — the cost-per-merged-PR rollup (#1372).
// DISPLAY-ONLY — surfaced in fishhawk_get_run_status so the operator sees the
// cost to land work; it never gates a run.
//
// Repeated here rather than imported because the MCP server's apiClient is a
// thin local copy (the import direction is `cli → backend`, not the reverse).
// The scalar fields are omitempty so the no-data path — which GetRunCost
// collapses to a nil pointer — never marshals a half-empty block.
type RunCost struct {
	TotalCostUSD float64          `json:"total_cost_usd,omitempty"`
	Stages       []RunCostStage   `json:"stages,omitempty"`
	MergedPR     *RunMergedPRCost `json:"merged_pr,omitempty"`
}

// RunCostStage is the per-source cost breakdown row (agent / plan_review /
// implement_review).
type RunCostStage struct {
	Source  string  `json:"source"`
	CostUSD float64 `json:"cost_usd"`
}

// RunMergedPRCost is the cost-per-merged-PR rollup: the summed CostUSDTotal
// across every run sharing the PR URL, present only when the run resolved to a
// merged PR.
type RunMergedPRCost struct {
	PullRequestURL     string  `json:"pull_request_url"`
	CostPerMergedPRUSD float64 `json:"cost_per_merged_pr_usd"`
	RunCount           int     `json:"run_count"`
}

// GetRunCost fetches the run's cost summary. The backend returns 200 with an
// empty object when the run has no cost data; GetRunCost collapses that to
// (nil, nil) so every caller treats "no data" uniformly by checking for a nil
// pointer. The presence sentinel is "no per-stage rows AND no merged-PR
// rollup" — the empty object has neither, while any costed run reports at
// least one stage, so the empty object never false-collapses a real metric.
func (c *apiClient) GetRunCost(ctx context.Context, runID uuid.UUID) (*RunCost, error) {
	var rc RunCost
	if err := c.do(ctx, http.MethodGet, "/v0/runs/"+runID.String()+"/cost", nil, &rc); err != nil {
		return nil, err
	}
	if len(rc.Stages) == 0 && rc.MergedPR == nil {
		return nil, nil
	}
	return &rc, nil
}

// RunLatency mirrors the backend's GET /v0/runs/{run_id}/latency body
// (`backend/internal/server/latency.go::latencySummaryResponse`): the per-run
// gate-latency (wait-on-human) rollup derived from the run's audit-chain
// timestamps (#1702) — the time parked at each human gate (plan approval,
// implement-review → next dispatch, checks-green → merge), the total wait on
// human decisions, and the run's end-to-end wall clock. DISPLAY-ONLY —
// surfaced in fishhawk_get_run_status so the operator sees human-gate latency;
// it never gates a run.
//
// Repeated here rather than imported because the MCP server's apiClient is a
// thin local copy (the import direction is `cli → backend`, not the reverse).
// The scalar fields are omitempty so the no-data path — which GetRunLatency
// collapses to a nil pointer — never marshals a half-empty block.
type RunLatency struct {
	Gates                   []LatencyGate `json:"gates,omitempty"`
	TotalWaitOnHumanSeconds float64       `json:"total_wait_on_human_seconds,omitempty"`
	WallClockSeconds        float64       `json:"wall_clock_seconds,omitempty"`
}

// LatencyGate is one measured human gate: the interval between its opening and
// closing audit markers, with the wait in seconds.
type LatencyGate struct {
	Gate        string    `json:"gate"`
	OpenedAt    time.Time `json:"opened_at"`
	ClosedAt    time.Time `json:"closed_at"`
	WaitSeconds float64   `json:"wait_seconds"`
}

// GetRunLatency fetches the run's gate-latency rollup. The backend returns 200
// with an empty object when no gate interval resolves; GetRunLatency collapses
// that to (nil, nil) so every caller treats "no data" uniformly by checking for
// a nil pointer. The presence sentinel is "no gate rows" — the empty object has
// none, while any gated run reports at least one gate, so the empty object never
// false-collapses a real rollup.
func (c *apiClient) GetRunLatency(ctx context.Context, runID uuid.UUID) (*RunLatency, error) {
	var rl RunLatency
	if err := c.do(ctx, http.MethodGet, "/v0/runs/"+runID.String()+"/latency", nil, &rl); err != nil {
		return nil, err
	}
	if len(rl.Gates) == 0 {
		return nil, nil
	}
	return &rl, nil
}

// OnboardingReadinessReport mirrors the backend's GET
// /v0/onboarding/readiness body
// (`backend/internal/server/onboarding.go::onboardingReadinessResponse`, E29.4 /
// #1511): the four server-side-only readiness checks a repo's first run needs —
// GitHub App installation, the committed workflow spec's parse/validate state,
// per-reviewer availability on this deployment, and the caller token's scope
// adequacy. Repeated here rather than imported because the MCP server's
// apiClient is a thin local copy (the import direction is `cli → backend`, not
// the reverse). Every field is a scalar/string/slice — no UUID/raw-JSON field,
// so the #371 reflection trap does not apply. MUST stay byte-identical with the
// backend response's json tags.
type OnboardingReadinessReport struct {
	Repo      string               `json:"repo" jsonschema:"the target repo as owner/name that was probed"`
	App       OnboardingApp        `json:"app" jsonschema:"GitHub App installation readiness"`
	Spec      OnboardingSpec       `json:"spec" jsonschema:"committed workflow spec fetch/parse/validate readiness"`
	Reviewers []OnboardingReviewer `json:"reviewers" jsonschema:"per spec-declared reviewer availability on this deployment; empty when the spec is unavailable or invalid"`
	Scopes    OnboardingScopes     `json:"scopes" jsonschema:"caller-token run-driving scope adequacy"`
}

// OnboardingApp mirrors the backend appInstallReadiness sub-object: whether the
// GitHub App is installed on the target repo, with reason set when it is not.
type OnboardingApp struct {
	Installed      bool   `json:"installed" jsonschema:"true when the GitHub App is installed on the target repo"`
	InstallationID int64  `json:"installation_id,omitempty" jsonschema:"the resolved installation id when installed"`
	Reason         string `json:"reason,omitempty" jsonschema:"why the app is not installed / could not be resolved"`
}

// OnboardingSpec mirrors the backend specReadiness sub-object: the committed
// workflow spec's fetch + parse + validate state. Valid is only meaningful when
// Source == "fetched".
type OnboardingSpec struct {
	Source string `json:"source" jsonschema:"'fetched' when the spec was read from the repo, else 'unavailable'"`
	Valid  bool   `json:"valid" jsonschema:"true when the fetched spec parsed and validated; meaningful only when source is 'fetched'"`
	Error  string `json:"error,omitempty" jsonschema:"the parse or validation failure when the spec is invalid"`
	Note   string `json:"note,omitempty" jsonschema:"why the spec is unavailable (app not installed, spec not found, fetch error)"`
}

// OnboardingReviewer mirrors the backend reviewerReadiness sub-object: one
// spec-declared reviewer's availability on this deployment, with the adapter's
// missing-env-var hint when the provider cannot be resolved.
type OnboardingReviewer struct {
	Provider        string `json:"provider" jsonschema:"the reviewer provider (e.g. claudecode, codex)"`
	Model           string `json:"model,omitempty" jsonschema:"the reviewer model"`
	ReasoningEffort string `json:"reasoning_effort,omitempty" jsonschema:"the reviewer reasoning-effort tier when set"`
	Available       bool   `json:"available" jsonschema:"true when this reviewer can be resolved on this deployment"`
	MissingHint     string `json:"missing_hint,omitempty" jsonschema:"the adapter's missing-env-var hint when the provider is unavailable"`
}

// OnboardingScopes mirrors the backend scopeReadiness sub-object: whether the
// caller token holds the run-driving scope subset, listing any missing scopes.
type OnboardingScopes struct {
	Adequate bool     `json:"adequate" jsonschema:"true when the caller token holds every run-driving scope"`
	Required []string `json:"required" jsonschema:"the run-driving scope subset the check requires"`
	Missing  []string `json:"missing" jsonschema:"the required scopes the caller lacks; empty when adequate"`
	Note     string   `json:"note,omitempty" jsonschema:"context, e.g. a cookie-session caller bypasses scope enforcement"`
}

// OnboardingReadiness fetches a repo's first-run readiness report via
// `GET /v0/onboarding/readiness?repo=owner/name` (E29.4 / #1511). The endpoint
// gates on AUTHENTICATION only (401 for anonymous) — scope adequacy is itself a
// reported field, not a gate — so a token with a run-driving scope gap still
// gets a 200 report naming its gap. 4xx surfaces as *apiError; the tool layer
// maps authentication_required (401) and validation_failed (400, malformed
// repo) onto clean tool errors.
func (c *apiClient) OnboardingReadiness(ctx context.Context, repo string) (*OnboardingReadinessReport, error) {
	var out OnboardingReadinessReport
	if err := c.do(ctx, http.MethodGet, "/v0/onboarding/readiness?repo="+url.QueryEscape(repo), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
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
	UpstreamRunID  string        `json:"upstream_run_id,omitempty"`
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
	UpstreamRunID  string
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
	// RemoveScopeFiles is the structured scope removal (#1726): repo-relative
	// paths to subtract from the implement stage's scope.files on approve — the
	// inverse of AddScopeFiles. Combined with it in the same approve call it
	// expresses a scope REPLACE. The DisallowUnknownFields decoder on the
	// backend requires the field be declared here too; reject and removal-less
	// approve callers pass nil (omitempty).
	RemoveScopeFiles []string `json:"remove_scope_files,omitempty"`
	// BindingAssertions is the operator-declared binding-assertion list (#1171)
	// the backend validates pre-Submit and records on the approval audit
	// payload. The DisallowUnknownFields decoder requires the field be declared
	// here too; reject and assertion-less approve callers pass nil (omitempty).
	BindingAssertions []BindingAssertion `json:"binding_assertions,omitempty"`
	// ImplementModel is the optional operator override for the implement-stage
	// model (#1013) — the highest rung of the resolution ladder. The backend
	// resolves the full ladder at the plan gate, validates the resolved value
	// against the allow-list (422 plan_invalid_model on an unknown model), and
	// records it as the model_resolved audit. The DisallowUnknownFields decoder
	// requires the field be declared here too; reject and override-less approve
	// callers pass "" (omitempty) and stay byte-identical to today.
	ImplementModel string `json:"implement_model,omitempty"`
}

// approvalResult is the decoded 200 body of POST /v0/stages/{id}/
// approvals (#986). On a first submission the duplicate fields are
// absent (zero values). On a duplicate — the same subject already
// decided this stage — DuplicateSubmission is true, the prior decision
// stands, the stage state is unchanged, and the backend ran NO gates
// and emitted NO audit entries; PriorDecision/PriorSubmittedAt carry
// the EXISTING approval row's provenance.
type approvalResult struct {
	Stage
	DuplicateSubmission bool   `json:"duplicate_submission"`
	PriorDecision       string `json:"prior_decision"`
	PriorSubmittedAt    string `json:"prior_submitted_at"`
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
// approve; nil on reject and conditionless approve. `removeScopeFiles` is the
// inverse structured scope removal (#1726) subtracted from the implement
// stage's scope.files on approve (a scope REPLACE = addScopeFiles +
// removeScopeFiles in one call); nil on reject and removal-less approve.
// `bindingAssertions` is the
// operator-declared binding-assertion list (#1171) the backend validates
// pre-Submit and records on the approval audit payload; nil on reject and
// assertion-less approve. Returns the updated Stage. 4xx
// surfaces:
//   - 400 validation_failed (decision other than approve/reject; a malformed
//     binding_assertions declaration — unknown type, empty literal, a
//     test_asserts path not ending in _test.go)
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
//     implement-stage budget; decompose or --override-budget. #986:
//     refused PRE-insert — no approval row is recorded, so the same
//     subject's retry with the override flows normally)
//   - 422 plan_violates_scope_cap (#983: effective scope.files — plan
//     scope plus add_scope_files — exceeds the implement stage's
//     max_files_changed; re-scope the plan or include
//     --override-scope-cap in the comment. Also pre-insert and
//     override-retryable, same as plan_violates_budget)
//   - 422 plan_invalid_model (#1013: the RESOLVED implement model — the
//     ladder of deployment default < spec executor.model < plan
//     model_recommendation < implement_model override — is not in the
//     deployment's per-adapter allow-list; details carry model,
//     model_source, and adapter. Pre-insert: retry with an allowed
//     implement_model, or widen the allow-list)
func (c *apiClient) SubmitApproval(ctx context.Context, stageID uuid.UUID, decision, comment, approverGithubLogin string, addScopeFiles, removeScopeFiles []string, bindingAssertions []BindingAssertion, implementModel string) (*approvalResult, error) {
	body, err := json.Marshal(approvalRequest{
		Decision:            decision,
		Comment:             comment,
		ApproverGithubLogin: approverGithubLogin,
		AddScopeFiles:       addScopeFiles,
		RemoveScopeFiles:    removeScopeFiles,
		BindingAssertions:   bindingAssertions,
		ImplementModel:      implementModel,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal approval: %w", err)
	}
	var res approvalResult
	if err := c.do(ctx, http.MethodPost, "/v0/stages/"+stageID.String()+"/approvals", body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// ClarificationAnswer is one operator answer to a parked clarification
// question, matched back to the question by id. Exported (like
// RecoverScopePath) so the MCP tool's input schema can reuse the same
// shape the wire body carries.
type ClarificationAnswer struct {
	ID     string `json:"id" jsonschema:"the parked question's id, from the run's clarification_requested audit entry (read it via fishhawk_get_run_status / fishhawk_list_audit)"`
	Answer string `json:"answer" jsonschema:"the operator's answer to that question"`
}

// clarificationAnswerRequest mirrors the backend's
// `POST /v0/stages/{stage_id}/clarification` body
// (`backend/internal/server/clarification_answer.go::clarificationAnswerRequest`).
type clarificationAnswerRequest struct {
	Answers []ClarificationAnswer `json:"answers"`
	Comment string                `json:"comment,omitempty"`
}

// AnswerClarification posts the operator's answers to a plan stage parked
// at awaiting_input by a clarification_request, re-opening it
// (AwaitingInput → Pending), via
// `POST /v0/stages/{stage_id}/clarification` (#1088, the #1057
// answer-and-resume seam). The answers are persisted as a dedicated
// clarification_answered audit entry — NOT an approval — and injected into
// the resumed plan prompt's binding conditions. Returns the re-opened
// Stage. 4xx surfaces:
//   - 400 validation_failed (empty answers / unknown fields)
//   - 400 clarification_answer_invalid (unknown / missing / duplicate
//     answer id relative to the parked questions)
//   - 404 stage_not_found
//   - 409 invalid_state_transition (the stage is not a plan stage parked
//     at awaiting_input)
func (c *apiClient) AnswerClarification(ctx context.Context, stageID uuid.UUID, answers []ClarificationAnswer, comment string) (*Stage, error) {
	body, err := json.Marshal(clarificationAnswerRequest{Answers: answers, Comment: comment})
	if err != nil {
		return nil, fmt.Errorf("marshal clarification answer: %w", err)
	}
	var s Stage
	if err := c.do(ctx, http.MethodPost, "/v0/stages/"+stageID.String()+"/clarification", body, &s); err != nil {
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

// AcceptanceAdmissionResult mirrors the backend's acceptance-admission 200 body
// (#1928). ShortCircuited is the only always-present field: true means the
// backend settled the acceptance stage server-side (a passed verdict / skip
// marker recorded, NO runner needed) and Kind/Basis/CriteriaTotal/Stage are
// populated; false is the normal no-op path (proceed to spawn as today).
type AcceptanceAdmissionResult struct {
	ShortCircuited bool   `json:"short_circuited"`
	Kind           string `json:"kind"`
	Basis          string `json:"basis"`
	CriteriaTotal  int    `json:"criteria_total"`
	Stage          *Stage `json:"stage"`
}

// AcceptanceDispatchAdmission POSTs the pre-spawn acceptance-admission check via
// `POST /v0/stages/{stage_id}/acceptance-admission` (#1928): the backend
// evaluates the approved plan's three short-circuit predicates and, on a hit,
// settles the acceptance stage to a passed verdict WITHOUT a runner. The dispatch
// verbs call it for an acceptance stage before recording spawn evidence or
// spawning; a short_circuited:true result means skip the spawn. Follows
// RetryStage's shape — a non-2xx surfaces as *apiError so the caller can fail
// OPEN (spawn as today) on any admission-call error.
func (c *apiClient) AcceptanceDispatchAdmission(ctx context.Context, stageID uuid.UUID) (*AcceptanceAdmissionResult, error) {
	var res AcceptanceAdmissionResult
	if err := c.do(ctx, http.MethodPost, "/v0/stages/"+stageID.String()+"/acceptance-admission", nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// HostDispatchResult mirrors the backend's host-dispatch 200 body (#1912):
// whether this call drove the stage pending|awaiting_host_dispatch → dispatched
// (the spawn marker) and the resulting stage state. Transitioned:true is the
// common case (a parked stage marked as a spawn attempt); Transitioned:false is
// the idempotent no-op — the stage was already 'dispatched', a legal manual
// re-dispatch of a stage whose spawned runner died, which the caller proceeds on.
type HostDispatchResult struct {
	Transitioned bool   `json:"transitioned"`
	StageState   string `json:"stage_state"`
}

// HostDispatchStage marks a host spawn against a runner_kind-locked-local stage
// via POST /v0/runs/{run_id}/stages/{stage_id}/host-dispatch (#1912): the
// backend cannot spawn the host-local runner (ADR-024), so it parks an agent
// stage at 'awaiting_host_dispatch' rather than 'dispatched'. The MCP host-spawn
// verbs (fishhawk_run_stage, fishhawk_dispatch_stage, fishhawk_drive_run) call
// this IMMEDIATELY BEFORE spawning the runner and FAIL CLOSED on any error, so
// post-#1912 'dispatched' unambiguously means "a spawn attempt exists". The
// endpoint CAS-transitions {pending, awaiting_host_dispatch} → dispatched.
// Callers fail closed on a non-nil error (transport / 4xx). 4xx surfaces:
//   - 401 authentication_required / 403 insufficient_scope (needs write:runs)
//   - 404 stage_not_found (unknown stage, or the stage's run_id disagrees)
//   - 409 dispatch_not_admissible (a running/terminal/awaiting_* gate state — a
//     live or settled stage can never be re-marked as a fresh spawn)
func (c *apiClient) HostDispatchStage(ctx context.Context, runID, stageID uuid.UUID) (*HostDispatchResult, error) {
	path := "/v0/runs/" + runID.String() + "/stages/" + stageID.String() + "/host-dispatch"
	var res HostDispatchResult
	if err := c.do(ctx, http.MethodPost, path, nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Reap-failure body caps (#1791). The reap-failure endpoint caps the request
// body at 32*1024 bytes (backend/internal/server/reap_failure.go
// maxReapFailureBodyBytes) and rejects an oversized body 413 body_too_large.
// The detached reaper re-POSTs the runner_failed line's detail here, so a
// category-B failure whose detail embeds the whole multi-module verify output
// would 413 the backstop too — the exact #1791 double-failure. reason is a short
// classification and detail is the large diagnostic, so detail gets the bulk of
// the budget; their sum plus the JSON envelope stays under 32*1024.
// aggressiveReapFailureBytes is the far smaller cap the bounded post-4xx retry
// re-marshals both fields with. Mirrors upload.MaxFailureReportReasonBytes /
// AggressiveFailureReportReasonBytes (a separate Go module — the helper cannot
// be shared, so each module defines its own with the same head+tail contract).
const (
	maxReapFailureReasonBytes  = 4 * 1024
	maxReapFailureDetailBytes  = 26 * 1024
	aggressiveReapFailureBytes = 2 * 1024
)

// truncateReason bounds s to at most max bytes for a reap-failure field (#1791).
// When s already fits it is returned byte-identical. Otherwise the middle is
// elided — a head + a "\n… [truncated N bytes] …\n" marker + a tail — so BOTH
// the leading classification and the trailing summary survive, and the result
// never exceeds max. Same contract as the runner's upload.TruncateReason; kept
// package-local because backend → runner is not an allowed import direction.
func truncateReason(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	const markerFmt = "\n… [truncated %d bytes] …\n"
	// Size the head/tail budget against the marker rendered with the LARGEST
	// possible elided count (len(s)); the real marker cannot have more digits,
	// so the final head+marker+tail can only be <= max.
	upper := fmt.Sprintf(markerFmt, len(s))
	keep := max - len(upper)
	if keep <= 0 {
		return s[:max]
	}
	head := keep / 2
	tail := keep - head
	elided := len(s) - head - tail
	marker := fmt.Sprintf(markerFmt, elided)
	return s[:head] + marker + s[len(s)-tail:]
}

// reapFailureRequest mirrors the backend's
// `POST /v0/runs/{run_id}/stages/{stage_id}/reap-failure` body
// (`backend/internal/server/reap_failure.go::reapFailureRequest`). Both
// category and reason are required; detail and exit_code are optional
// diagnostics.
type reapFailureRequest struct {
	Category string `json:"category"`
	Reason   string `json:"reason"`
	Detail   string `json:"detail,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
}

// ReapFailureResult mirrors the backend's reap-failure 200 body: whether this
// report drove the stage to failed (false on the idempotent already-terminal
// no-op) and the resulting stage state.
type ReapFailureResult struct {
	Transitioned bool   `json:"transitioned"`
	StageState   string `json:"stage_state"`
}

// ReportStageFailure reports a spawn-phase runner failure — a detached runner
// that exited non-zero BEFORE reporting a terminal stage state — to the backend
// via `POST /v0/runs/{run_id}/stages/{stage_id}/reap-failure` (#1747). The
// backend fails the stage (category C is the retryable infrastructure class),
// writes a dispatch_reaper_failed audit entry, and advances the run, so the
// stage lands in failed/category-C instead of stuck 'dispatched'. Idempotent: a
// double-report against an already-terminal stage returns
// {transitioned:false}. Mirrors VouchCommit/RetryStage. 4xx surfaces:
//   - 400 validation_failed (category other than B/C, empty reason, malformed
//     UUIDs/body)
//   - 401 authentication_required / 403 insufficient_scope (needs write:runs)
//   - 404 stage_not_found (unknown stage, or the stage's run_id disagrees)
func (c *apiClient) ReportStageFailure(ctx context.Context, runID, stageID uuid.UUID, category, reason, detail string, exitCode int) (*ReapFailureResult, error) {
	// Truncate both fields so the marshalled body fits the endpoint's 32*1024
	// cap (#1791) — otherwise this backstop 413s for the very oversized detail
	// that stranded the stage in the first place.
	reason = truncateReason(reason, maxReapFailureReasonBytes)
	detail = truncateReason(detail, maxReapFailureDetailBytes)
	body, err := json.Marshal(reapFailureRequest{Category: category, Reason: reason, Detail: detail, ExitCode: exitCode})
	if err != nil {
		return nil, fmt.Errorf("marshal reap-failure: %w", err)
	}
	path := "/v0/runs/" + runID.String() + "/stages/" + stageID.String() + "/reap-failure"
	var res ReapFailureResult
	err = c.do(ctx, http.MethodPost, path, body, &res)
	if err == nil {
		return &res, nil
	}
	// A 4xx (esp. 413 body_too_large) means even the normal-cap body was
	// rejected. Re-marshal both fields with the aggressive cap and re-POST
	// exactly once (#1791). A 5xx, a network error, or a second 4xx surfaces
	// unchanged — no loop.
	var ae *apiError
	if errors.As(err, &ae) && ae.StatusCode >= 400 && ae.StatusCode < 500 {
		aggBody, mErr := json.Marshal(reapFailureRequest{
			Category: category,
			Reason:   truncateReason(reason, aggressiveReapFailureBytes),
			Detail:   truncateReason(detail, aggressiveReapFailureBytes),
			ExitCode: exitCode,
		})
		if mErr != nil {
			return nil, fmt.Errorf("marshal aggressive reap-failure: %w", mErr)
		}
		var aggRes ReapFailureResult
		if aggErr := c.do(ctx, http.MethodPost, path, aggBody, &aggRes); aggErr != nil {
			return nil, aggErr
		}
		return &aggRes, nil
	}
	return nil, err
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
	// ImplementModel is the optional operator/driver model override for this
	// fix-up pass (#1164). omitempty: the common fix-up omits it and inherits
	// the run's already-resolved implement model (byte-identical default).
	ImplementModel string `json:"implement_model,omitempty"`
	// OperatorConcern is the optional free-text operator instruction routed
	// back to the agent with NO pre-existing review concern (#1311). omitempty:
	// the common fix-up omits it and addresses recorded concerns instead.
	OperatorConcern string `json:"operator_concern,omitempty"`
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
//   - 422 fixup_invalid_model (the resolved implement_model override is not in
//     the deployment's per-adapter allow-list; details carry the resolved
//     model, source, and adapter)
//
// allowCreate declares net-new files this pass will create (#823), folded
// into the effective scope.files for that dispatch only; an invalid entry
// (absolute / containing "..") surfaces 400 validation_failed.
// forceAdditionalPass is the bounded operator override (#860): grant ONE
// pass beyond the normal budget, hard-capped at 3 total passes.
// implementModel is the optional operator/driver model override for this pass
// (#1164), validated server-side against the deployment allow-list (422
// fixup_invalid_model on reject); empty inherits the run's implement model.
// operatorConcern is the optional free-text operator instruction routed back
// to the agent with NO pre-existing review concern (#1311); empty addresses
// recorded concerns instead.
func (c *apiClient) FixupStage(ctx context.Context, id uuid.UUID, concernIDs []string, concerns []int, reason string, allowCreate []string, forceAdditionalPass bool, implementModel, operatorConcern string) (*Stage, error) {
	body, err := json.Marshal(fixupRequest{ConcernIDs: concernIDs, Concerns: concerns, Reason: reason, AllowCreate: allowCreate, ForceAdditionalPass: forceAdditionalPass, ImplementModel: implementModel, OperatorConcern: operatorConcern})
	if err != nil {
		return nil, fmt.Errorf("marshal fixup: %w", err)
	}
	var s Stage
	if err := c.do(ctx, http.MethodPost, "/v0/stages/"+id.String()+"/fixup", body, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// reviseRequest mirrors the backend's
// `POST /v0/stages/{stage_id}/revise` body
// (`backend/internal/server/revise.go::reviseRequest`). Constraint is the
// operator's binding design constraint the planner must revise the prior
// plan to satisfy — REQUIRED. ForceAdditionalPass is the bounded operator
// override: grant ONE revise pass beyond the normal budget, hard-capped
// at 3 total passes per stage.
type reviseRequest struct {
	Constraint          string `json:"constraint"`
	ForceAdditionalPass bool   `json:"force_additional_pass,omitempty"`
}

// SubmitRevise re-opens a plan stage parked at its approval gate to
// re-plan IN PLACE against a binding operator design constraint via
// `POST /v0/stages/{stage_id}/revise` (#1099). The third plan-gate
// verdict alongside approve/reject: the constraint is injected into the
// re-dispatched plan prompt (the #558 binding channel, a dedicated
// "Revision constraint" section) with the prior plan as the revision
// base, and the stage re-enters the review → approve gate. Distinct from
// RetryStage: revise re-opens a HEALTHY plan gate and is bounded
// (default one pass). Returns the re-opened Stage row (pending, or
// dispatched once the orchestrator advances it before the response
// returns). 4xx surfaces:
//   - 400 validation_failed (empty constraint / malformed UUID)
//   - 403 cross_run_revise (a run-bound token reaching another run's
//     stage) or insufficient_scope
//   - 404 stage_not_found
//   - 409 revise_not_applicable (the stage is not a plan stage parked at
//     awaiting_approval)
//   - 409 revise_budget_exhausted (the NORMAL bounded pass count is spent;
//     one more pass is available via forceAdditionalPass)
//   - 409 revise_ceiling_reached (the hard ceiling of 3 total passes is
//     reached; the override cannot push past it — reject → fresh-run replan)
func (c *apiClient) SubmitRevise(ctx context.Context, stageID uuid.UUID, constraint string, forceAdditionalPass bool) (*Stage, error) {
	body, err := json.Marshal(reviseRequest{Constraint: constraint, ForceAdditionalPass: forceAdditionalPass})
	if err != nil {
		return nil, fmt.Errorf("marshal revise: %w", err)
	}
	var s Stage
	if err := c.do(ctx, http.MethodPost, "/v0/stages/"+stageID.String()+"/revise", body, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// waiveConcernRequest mirrors the backend's
// `POST /v0/concerns/{concern_id}/waive` body
// (`backend/internal/server/waive.go::waiveConcernRequest`). Reason is
// REQUIRED — the backend refuses an empty reason with 400.
type waiveConcernRequest struct {
	Reason string `json:"reason"`
}

// WaivedConcern mirrors the backend's waive 200 body: the updated
// concern row, now in state waived with the operator's reason as
// state_reason.
type WaivedConcern struct {
	ID          string `json:"id"`
	RunID       string `json:"run_id"`
	StageID     string `json:"stage_id"`
	StageKind   string `json:"stage_kind"`
	Severity    string `json:"severity"`
	Category    string `json:"category"`
	Note        string `json:"note"`
	State       string `json:"state"`
	StateReason string `json:"state_reason"`
}

// WaiveConcern transitions one open review concern to the terminal
// waived state with a required, audited reason via
// `POST /v0/concerns/{concern_id}/waive` (E22.X / #984). 4xx surfaces:
//   - 400 validation_failed (empty reason)
//   - 403 cross_run_waive (a run-bound token reaching another run's
//     concern) or insufficient_scope
//   - 404 concern_not_found
//   - 422 concern_waive_conflict (the concern is not in an open state —
//     already waived/superseded/addressed; details carry the from/to pair)
//   - 503 concern_store_unconfigured
func (c *apiClient) WaiveConcern(ctx context.Context, id uuid.UUID, reason string) (*WaivedConcern, error) {
	body, err := json.Marshal(waiveConcernRequest{Reason: reason})
	if err != nil {
		return nil, fmt.Errorf("marshal waive: %w", err)
	}
	var out WaivedConcern
	if err := c.do(ctx, http.MethodPost, "/v0/concerns/"+id.String()+"/waive", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// deferConcernRequest mirrors the backend's defer 200 request body
// (`backend/internal/server/defer_concern.go::deferConcernRequest`). The
// follow-up body is auto-drafted server-side; the operator supplies only
// the title coordinates + optional overrides.
type deferConcernRequest struct {
	ParentEpic string   `json:"parent_epic,omitempty"`
	N          string   `json:"n,omitempty"`
	Type       string   `json:"type,omitempty"`
	Labels     []string `json:"labels,omitempty"`
	Note       string   `json:"note,omitempty"`
}

// DeferConcernParams bundles the caller-supplied defer inputs the tool
// layer collects, so DeferConcern's signature stays readable.
type DeferConcernParams struct {
	ParentEpic string
	N          string
	Type       string
	Labels     []string
	Note       string
}

// DeferredConcern mirrors the backend defer 200 body's `concern` block:
// the updated concern row, now in state deferred with state_reason
// naming the filed follow-up issue.
type DeferredConcern struct {
	ID          string `json:"id"`
	RunID       string `json:"run_id"`
	StageID     string `json:"stage_id"`
	StageKind   string `json:"stage_kind"`
	Severity    string `json:"severity"`
	Category    string `json:"category"`
	Note        string `json:"note"`
	State       string `json:"state"`
	StateReason string `json:"state_reason"`
}

// DeferFiledIssue mirrors the backend defer 200 body's `issue` block: the
// filed follow-up work item.
type DeferFiledIssue struct {
	Type          string   `json:"type"`
	Title         string   `json:"title"`
	Number        int      `json:"number"`
	URL           string   `json:"url"`
	Provider      string   `json:"provider"`
	AppliedLabels []string `json:"applied_labels,omitempty"`
	// DefaultedLabels / MissingLabelNamespaces mirror the work-item filing's
	// LOUD label-completeness report (#1616) on the deferred follow-up.
	DefaultedLabels        []string `json:"defaulted_labels,omitempty"`
	MissingLabelNamespaces []string `json:"missing_label_namespaces,omitempty"`
}

// DeferredConcernResult mirrors the backend defer 200 body: the filed
// follow-up work item plus the now-deferred concern row.
type DeferredConcernResult struct {
	Concern DeferredConcern `json:"concern"`
	Issue   DeferFiledIssue `json:"issue"`
}

// DeferConcern converts one open review concern into a follow-up work
// item and transitions the concern to the terminal deferred state via
// `POST /v0/concerns/{concern_id}/defer` (E22.X / #1202). 4xx/5xx
// surfaces:
//   - 403 cross_run_defer (a run-bound token reaching another run's
//     concern) or insufficient_scope
//   - 404 concern_not_found
//   - 422 concern_defer_conflict (the concern is not open, or a
//     post-filing transition race — details may carry the filed issue url)
//   - 422 work_item_invalid (the follow-up violates the type's conventions)
//   - 501 provider_unimplemented / 502 work_item_filing_failed (the
//     provider could not file — the concern stays OPEN, no transition)
//   - 503 concern_store_unconfigured
func (c *apiClient) DeferConcern(ctx context.Context, id uuid.UUID, p DeferConcernParams) (*DeferredConcernResult, error) {
	body, err := json.Marshal(deferConcernRequest(p))
	if err != nil {
		return nil, fmt.Errorf("marshal defer: %w", err)
	}
	var out DeferredConcernResult
	if err := c.do(ctx, http.MethodPost, "/v0/concerns/"+id.String()+"/defer", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
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

// ReviveRestoredStage mirrors the backend's reviveRestoredStage wire shape
// (`backend/internal/server/revive.go`): one re-parked stage in a revive's
// batch. StageID is typed `string` (not `uuid.UUID`) per the #371 reflection
// rule so the MCP SDK's response-schema reflection sees a string — the JSON
// payload IS a string, and a `uuid.UUID` (a 16-byte array) would surface as
// `type: array` and reject at the wire boundary.
type ReviveRestoredStage struct {
	StageID       string `json:"stage_id" jsonschema:"the re-parked stage's UUID"`
	Type          string `json:"type" jsonschema:"the stage kind (plan/implement/review/…)"`
	PriorCategory string `json:"prior_category" jsonschema:"the stage's failure category before the revive (A/C, or a retryable D)"`
	PriorReason   string `json:"prior_reason" jsonschema:"the stage's failure_reason from before the revive"`
	RestoredState string `json:"restored_state" jsonschema:"the pre-dispatch state the stage was re-parked to (pending for A/C, awaiting_approval for a D SLA-timeout gate, awaiting_children for a decomposed-parent implement)"`
}

// ReviveRunResult mirrors the backend's revive 200 body (`reviveResponse`):
// the re-opened run (now running) plus the per-stage re-park summary. The
// nested Run reuses the client's Run type, which already decodes the backend's
// runResponse (it is the GET /v0/runs/{id} shape).
type ReviveRunResult struct {
	Run            Run                   `json:"run"`
	RestoredStages []ReviveRestoredStage `json:"restored_stages"`
}

// ReviveRun re-admits a terminal-FAILED run for another operator turn via
// `POST /v0/runs/{run_id}/revive` (#1915): the backend pre-validates that
// EVERY failed stage is retryable, then re-parks each failed stage in its
// correct gate-ordered pre-dispatch state (A/C → pending, D SLA-timeout →
// awaiting_approval, decomposed-parent implement → awaiting_children) and flips
// the run failed → running. CRUCIALLY revive performs NO orchestrator handoff
// and never dispatches — it re-parks only, so the #1700 wrong-order
// re-dispatch corruption is structurally impossible; dispatch happens later at
// each stage's proper gate turn via the existing verbs. Operator-token-only,
// modeled on ResetRunBranch/VouchCommit. 4xx/5xx surfaces:
//   - 403 agent_token_forbidden (a run-bound agent/mcp token attempted revive)
//   - 403 insufficient_scope (token lacks write:stages or write:retries)
//   - 404 run_not_found
//   - 409 invalid_state_transition (a concurrent transition raced the reopen)
//   - 422 revive_not_applicable (the run is not failed, has no failed stage,
//     or a failed stage is non-retryable — category-B, D-rejected, or no
//     recorded category; the message names the blocking stage. No partial
//     mutation: the whole revive is refused pre-transition)
//   - 503 revive_unconfigured (run/audit repositories not wired)
func (c *apiClient) ReviveRun(ctx context.Context, runID uuid.UUID) (*ReviveRunResult, error) {
	var res ReviveRunResult
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+runID.String()+"/revive", nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// vouchCommitRequest mirrors the backend's
// `POST /v0/runs/{run_id}/vouch-commit` body
// (`backend/internal/server/vouch.go::vouchCommitRequest`). Both fields
// are required — the vouch is an audited operator declaration.
type vouchCommitRequest struct {
	SHA    string `json:"sha"`
	Reason string `json:"reason"`
}

// VouchCommitResult mirrors the backend's vouch-commit 200 body: the
// recorded declaration, surfaced back to the operator.
type VouchCommitResult struct {
	RunID      string `json:"run_id"`
	VouchedSHA string `json:"vouched_sha"`
	Reason     string `json:"reason"`
}

// VouchCommit declares a foreign commit on a run branch to be run-authored
// lineage (ADR-035 remediation, #1044), via
// `POST /v0/runs/{run_id}/vouch-commit`. The vouched SHA is unioned into
// the reported-head ledger, un-wedging the merge reconciler for an
// operator's mechanical remediation commit. Operator-token-only
// (write:stages); distinct from ResetRunBranch (which DROPS an on-top
// foreign commit) — vouch KEEPS the operator commit and attributes it.
// 4xx/5xx surfaces:
//   - 400 validation_failed (empty sha or reason)
//   - 403 run_token_forbidden (a run-bound agent token attempted the vouch)
//   - 403 insufficient_scope (token lacks write:stages)
//   - 404 run_not_found
//   - 503 vouch_unconfigured (run/audit repositories not wired)
func (c *apiClient) VouchCommit(ctx context.Context, runID uuid.UUID, sha, reason string) (*VouchCommitResult, error) {
	body, err := json.Marshal(vouchCommitRequest{SHA: sha, Reason: reason})
	if err != nil {
		return nil, fmt.Errorf("marshal vouch-commit: %w", err)
	}
	var res VouchCommitResult
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+runID.String()+"/vouch-commit", body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// AutoDriveOutcome mirrors the backend's POST /v0/runs/{run_id}/auto-drive
// 200 body (#1700): the AutoDriveRunGate result the local drive verb switches
// on. Exactly one of Acted / Paged is true on a non-observe-only outcome; an
// observe-only outcome has both false. Repeated (not imported) per the thin
// local-copy rule — import direction is cli → backend, not the reverse.
type AutoDriveOutcome struct {
	Acted     bool   `json:"acted"`
	Action    string `json:"action,omitempty"`
	Paged     bool   `json:"paged"`
	PageEvent string `json:"page_event,omitempty"`
	Note      string `json:"note"`
}

// AutoDriveRunGate calls POST /v0/runs/{run_id}/auto-drive (#1700): it drives
// the run's ONE parked gate under ADR-040 delegation, returning the outcome.
// The delegated action's own audit row is the authoritative record; the
// endpoint ALSO lands a supplementary run_auto_driven act:gate attribution
// row on an ACTED outcome. FAIL-LOUD: a supplementary-append failure surfaces
// as a 500 apiError (auto_drive_record_failed), and a genuine gate-dispatch
// failure as auto_drive_dispatch_failed — the drive loop stops acting on
// either rather than continuing on a silent success. 4xx/5xx surfaces:
//   - 401 authentication_required / 403 insufficient_scope (needs write:approvals)
//   - 404 run_not_found
//   - 500 auto_drive_dispatch_failed / auto_drive_record_failed
func (c *apiClient) AutoDriveRunGate(ctx context.Context, runID uuid.UUID) (*AutoDriveOutcome, error) {
	var res AutoDriveOutcome
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+runID.String()+"/auto-drive", []byte("{}"), &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// RecordAutoDriveAct is one record-before-dispatch call the drive verb makes
// before host-spawning a stage. Action is always "dispatch_stage"; Stage is
// one of plan|implement|acceptance|fixup_redispatch; Source is the driver
// tag ("fishhawk_drive_run").
type RecordAutoDriveAct struct {
	Action string `json:"action"`
	Stage  string `json:"stage"`
	Source string `json:"source"`
	Note   string `json:"note,omitempty"`
}

// RecordAutoDriveActResult mirrors the backend's POST
// /v0/runs/{run_id}/auto-drive/acts 200 body (#1700): the appended
// run_auto_driven act:dispatch attribution row's identifying fields.
type RecordAutoDriveActResult struct {
	RunID    string `json:"run_id"`
	Category string `json:"category"`
	Act      string `json:"act"`
	Action   string `json:"action"`
	Stage    string `json:"stage"`
	Source   string `json:"source"`
	Sequence int64  `json:"sequence"`
}

// RecordAutoDriveAct calls POST /v0/runs/{run_id}/auto-drive/acts (#1700): the
// server-owned write path the drive verb uses to record a stage dispatch
// BEFORE it host-spawns the runner. The audit chain stays server-owned; the
// MCP host never writes a chain entry itself. Validation fails CLOSED — an
// unknown run 404s and every missing/bad field 400s, appending nothing.
// FAIL-LOUD: a record-append failure surfaces as 500 auto_drive_record_failed
// so the caller does NOT dispatch. 4xx/5xx surfaces:
//   - 400 validation_failed (missing/bad action, stage, or source)
//   - 401 authentication_required / 403 insufficient_scope (needs write:approvals)
//   - 404 run_not_found
//   - 500 auto_drive_record_failed
func (c *apiClient) RecordAutoDriveAct(ctx context.Context, runID uuid.UUID, act RecordAutoDriveAct) (*RecordAutoDriveActResult, error) {
	body, err := json.Marshal(act)
	if err != nil {
		return nil, fmt.Errorf("marshal record auto-drive act: %w", err)
	}
	var res RecordAutoDriveActResult
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+runID.String()+"/auto-drive/acts", body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// ConsolidateResult mirrors the backend's consolidate 200 body (E24.2 /
// #1238): the outcome of running the decomposed-parent fan-in on demand.
// Outcome is "integrated" (every slice merged, parent implement succeeded,
// consolidated PR opened) or "slice_conflict" (a slice failed to merge, parent
// implement failed recoverable category-B). The conflict fields are set only
// on the slice_conflict outcome.
type ConsolidateResult struct {
	RunID                 string `json:"run_id"`
	Outcome               string `json:"outcome"`
	ResolvedToState       string `json:"resolved_to_state"`
	ConsolidatedBranch    string `json:"consolidated_branch,omitempty"`
	PullRequestURL        string `json:"pull_request_url,omitempty"`
	ConflictingSliceIndex *int   `json:"conflicting_slice_index,omitempty"`
	ConflictingChildRunID string `json:"conflicting_child_run_id,omitempty"`
	Detail                string `json:"detail,omitempty"`
}

// ConsolidateRun runs the E24.2 fan-in for a decomposed parent on demand via
// `POST /v0/runs/{run_id}/consolidate` (#1238) — the operator path to
// complete a local decomposition where the 60s child-completion sweeper
// backstop is off. It returns the integrated/conflict outcome on 200, and
// SURFACES a fan-in failure the event-driven path would WARN-swallow. 4xx/5xx
// surfaces:
//   - 400 not_a_decomposed_parent (the run is a child, or has no children)
//   - 403 agent_token_forbidden (a run-bound agent token attempted it)
//   - 403 insufficient_scope (token lacks write:runs)
//   - 404 run_not_found
//   - 409 not_awaiting_children (already resolved, or not a decomposition)
//   - 409 children_in_flight (a child is still non-terminal)
//   - 409 children_failed (a child failed; resolve it before consolidating)
//   - 502 slice_integration_error (the fan-in failed; the error is surfaced)
func (c *apiClient) ConsolidateRun(ctx context.Context, id uuid.UUID) (*ConsolidateResult, error) {
	var res ConsolidateResult
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+id.String()+"/consolidate", nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// IntegrateWaveResult mirrors the backend's integrate-wave 200 body (#1278
// slice B): the outcome of the NON-settling per-wave fan-in the topological-
// wave run_children dispatch runs BETWEEN waves. Outcome is "integrated" (the
// succeeded slices so far merged onto the consolidated branch) or
// "slice_conflict" (a slice failed to merge). UNLIKE ConsolidateResult there
// is NO resolved_to_state — integrate-wave does not transition the parent
// stage. The conflict fields are set only on the slice_conflict outcome.
type IntegrateWaveResult struct {
	RunID                 string `json:"run_id"`
	Outcome               string `json:"outcome"`
	ConsolidatedBranch    string `json:"consolidated_branch,omitempty"`
	ConflictingSliceIndex *int   `json:"conflicting_slice_index,omitempty"`
	ConflictingChildRunID string `json:"conflicting_child_run_id,omitempty"`
	Detail                string `json:"detail,omitempty"`
}

// IntegrateWave runs the NON-settling per-wave fan-in for a decomposed parent
// via `POST /v0/runs/{run_id}/integrate-wave` (#1278 slice B) — the run_children
// wave loop calls it BETWEEN waves to merge the slices succeeded so far onto the
// consolidated branch so the next wave's dependent slices cut their branch from
// a tree carrying the predecessors' merged symbols. It does NOT require all
// children terminal, does NOT transition the parent stage, and does NOT
// advance/open the PR. 4xx/5xx surfaces:
//   - 400 not_a_decomposed_parent (the run is a child, or has no children)
//   - 403 agent_token_forbidden (a run-bound agent token attempted it)
//   - 403 insufficient_scope (token lacks write:runs)
//   - 404 run_not_found
//   - 502 slice_integration_error (the fan-in failed; the error is surfaced)
func (c *apiClient) IntegrateWave(ctx context.Context, id uuid.UUID) (*IntegrateWaveResult, error) {
	var res IntegrateWaveResult
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+id.String()+"/integrate-wave", nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// ScopeCompletenessDecisionResult mirrors the backend's scope-completeness
// decision 200 body (`backend/internal/server/scope_completeness.go` — SLICE
// 1, #1231): the resolved park record. State is the implement stage's
// resulting state (running/succeeded on exempt as the held commit's PR opens,
// failed on a category-B fail). HeldCommitSHA is the exact gate-verified
// commit the runner pushed to the run branch at park time; PullRequestURL is
// set only on exempt, when the backend opens the PR from that held commit
// with NO agent re-invocation. MissingPaths echoes the declared scope paths
// the #1151 shortfall gate flagged. Repeated here rather than imported — the
// MCP server's apiClient is deliberately a thin local copy (import direction
// is `cli → backend`, not the reverse). MUST stay byte-identical with the
// backend handler's response shape.
type ScopeCompletenessDecisionResult struct {
	RunID          string   `json:"run_id"`
	StageID        string   `json:"stage_id"`
	Decision       string   `json:"decision"`
	State          string   `json:"state"`
	HeldCommitSHA  string   `json:"held_commit_sha"`
	RunBranch      string   `json:"run_branch,omitempty"`
	MissingPaths   []string `json:"missing_paths,omitempty"`
	PullRequestURL string   `json:"pull_request_url,omitempty"`
}

// scopeCompletenessDecisionRequest mirrors the backend's decision body
// (`backend/internal/server/scope_completeness.go::scopeCompletenessDecisionRequest`
// — SLICE 1, #1231). Both fields are required: the backend rejects a decision
// other than exempt/fail and an empty reason with 400.
type scopeCompletenessDecisionRequest struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// DecideScopeCompleteness resolves an implement stage parked in
// awaiting_scope_decision via
// `POST /v0/runs/{run_id}/scope-completeness/decision` (#1231). decision is
// "exempt" (open the PR from the held commit with NO agent re-run) or "fail"
// (fall through to category-B); reason is required. Operator-token-only
// (write:stages); the backend rejects run-bound agent tokens
// (run_token_forbidden). 4xx surfaces:
//   - 400 validation_failed (decision not exempt/fail, empty reason)
//   - 403 run_token_forbidden (a run-bound agent token attempted the decision)
//   - 403 insufficient_scope (token lacks write:stages)
//   - 404 run_not_found
//   - 409 scope_completeness_not_parked (the stage is not parked in
//     awaiting_scope_decision)
func (c *apiClient) DecideScopeCompleteness(ctx context.Context, runID uuid.UUID, decision, reason string) (*ScopeCompletenessDecisionResult, error) {
	body, err := json.Marshal(scopeCompletenessDecisionRequest{Decision: decision, Reason: reason})
	if err != nil {
		return nil, fmt.Errorf("marshal scope-completeness decision: %w", err)
	}
	var res ScopeCompletenessDecisionResult
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+runID.String()+"/scope-completeness/decision", body, &res); err != nil {
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
		UpstreamRunID:  p.UpstreamRunID,
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

// RecoverScopePath is one operator-named path on a recovery request
// (#978). Operation is "modify" or "create"; the backend defaults an
// empty value to modify.
type RecoverScopePath struct {
	Path      string `json:"path"`
	Operation string `json:"operation,omitempty"`
}

// RecoverExemptPath is one operator-justified-unchanged path on a recovery
// request (#1229): a DECLARED scope.files path the runner's #1151 shortfall
// gate subtracts. The inverse of RecoverScopePath — it carries a required
// {path, reason} and subtracts from the gate rather than widening scope.
// Exported (like RecoverScopePath) so the MCP tool's input schema can reuse
// the same shape the wire body carries.
type RecoverExemptPath struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// recoverRunRequest mirrors `server/recover.go::recoverRunRequest`.
type recoverRunRequest struct {
	AddScopeFiles    []RecoverScopePath  `json:"add_scope_files,omitempty"`
	ExemptScopeFiles []RecoverExemptPath `json:"exempt_scope_files,omitempty"`
	Reason           string              `json:"reason,omitempty"`
	BudgetOverride   bool                `json:"budget_override,omitempty"`
}

// RecoverRunParams bundles the inputs to RecoverRun. IdempotencyKey
// travels in the HTTP header per the backend's E8.2 contract, same
// keyspace as StartRun.
type RecoverRunParams struct {
	ParentRunID      uuid.UUID
	AddScopeFiles    []RecoverScopePath
	ExemptScopeFiles []RecoverExemptPath
	Reason           string
	BudgetOverride   bool
	IdempotencyKey   string
}

// RecoverRun mints a category-B recovery run via
// `POST /v0/runs/{run_id}/recover` (#978). Returns the created (or
// replayed) child run plus an `idempotent` flag mirroring StartRun.
// 4xx surfaces as *apiError; the tool layer maps the codes:
//   - 404 run_not_found
//   - 409 recovery_not_eligible (plan not succeeded / implement not
//     failed category-B)
//   - 422 recovery_unsupported (no cached workflow spec)
func (c *apiClient) RecoverRun(ctx context.Context, p RecoverRunParams) (*Run, bool, error) {
	body, err := json.Marshal(recoverRunRequest{
		AddScopeFiles:    p.AddScopeFiles,
		ExemptScopeFiles: p.ExemptScopeFiles,
		Reason:           p.Reason,
		BudgetOverride:   p.BudgetOverride,
	})
	if err != nil {
		return nil, false, fmt.Errorf("marshal recover_run: %w", err)
	}
	headers := map[string]string{}
	if p.IdempotencyKey != "" {
		headers["Idempotency-Key"] = p.IdempotencyKey
	}
	var run Run
	status, err := c.doWithStatus(ctx, http.MethodPost,
		"/v0/runs/"+p.ParentRunID.String()+"/recover", body, headers, &run)
	if err != nil {
		return nil, false, err
	}
	// 200 = idempotent replay; 201 = newly created. Both are success.
	return &run, status == http.StatusOK, nil
}

// Campaign mirrors the backend's `Campaign` wire schema
// (`backend/internal/server/campaigns.go::campaignResponse`): the campaign
// row POST /v0/campaigns and GET /v0/campaigns/{id}/status return. As with
// the Run struct above, IDs are typed `string` (not uuid.UUID) so the MCP
// SDK's reflection-built schema sees a string rather than a 16-byte array
// (which would surface as type:array and fail the SDK's response validation);
// tool handlers parse with uuid.Parse locally.
type Campaign struct {
	ID      string `json:"id"`
	Repo    string `json:"repo"`
	EpicRef string `json:"epic_ref"`
	State   string `json:"state"`
	// PausePolicy is the operator-chosen pause behavior on a gate hand-off
	// (E25.7): pause_campaign (block the whole campaign, the default) or
	// pause_item (continue-others). Always normalized on a persisted campaign.
	PausePolicy string `json:"pause_policy"`
	// OperatorAgent is the OPTIONAL campaign-level operator_agent delegation
	// override (E25.12 / #1451). When present it is the effective delegation
	// contract for EVERY issue-run the campaign drives — it wins WHOLESALE over
	// the per-run workflow operator_agent (campaign > gate > workflow, never
	// merged). Typed map[string]any (not json.RawMessage) so the MCP SDK's
	// reflection-built output schema sees an unconstrained object rather than a
	// []byte array (which would surface as type:array and fail response
	// validation) — the same reason the CalibrationResult.ConfidenceBandAccuracy
	// field documents. Omitted on a campaign with no override (the byte-identical
	// default — each issue-run inherits its workflow contract).
	OperatorAgent map[string]any `json:"operator_agent,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

// CampaignPauseReason mirrors the backend's campaign.PauseReason: why a paused
// item was handed off to a human (the page event + run/stage/gate). The
// run_id/stage_id are typed `string` here (the backend carries *uuid.UUID, which
// JSON-marshals to a string) for the same MCP-SDK reflection reason Campaign
// documents.
type CampaignPauseReason struct {
	PageEvent string `json:"page_event,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	StageID   string `json:"stage_id,omitempty"`
	Gate      string `json:"gate,omitempty"`
}

// CampaignItem mirrors the backend's `CampaignItem` wire schema
// (`campaignItemResponse`): one node in the campaign DAG. RunID is omitempty —
// an unlinked (pre-dispatch) item carries no run_id.
type CampaignItem struct {
	ID          string               `json:"id"`
	IssueRef    string               `json:"issue_ref"`
	DependsOn   []string             `json:"depends_on"`
	RunID       string               `json:"run_id,omitempty"`
	State       string               `json:"state"`
	PauseReason *CampaignPauseReason `json:"pause_reason,omitempty"`
	CreatedAt   time.Time            `json:"created_at"`
	UpdatedAt   time.Time            `json:"updated_at"`
}

// CampaignRollup mirrors the backend's `CampaignRollup` wire schema
// (`campaignRollupPayload`): the engine's readiness partition over a campaign's
// items. Every slice holds issue refs and an item appears in exactly one slice.
type CampaignRollup struct {
	Eligible []string `json:"eligible"`
	// HumanLed holds deps-satisfied autonomy:low items diverted out of Eligible
	// (human-led work the auto-driver must never dispatch).
	HumanLed  []string `json:"human_led"`
	Blocked   []string `json:"blocked"`
	Running   []string `json:"running"`
	Done      []string `json:"done"`
	Failed    []string `json:"failed"`
	Cancelled []string `json:"cancelled"`
	Paused    []string `json:"paused"`
}

// CampaignNextAction mirrors the backend's `campaignNextActionPayload`: the
// single server-computed next step for the operator-agent, distilled from the
// rollup partition. Action is drawn from the closed set
// attention|resume|start_run|attend_human_led|wait|complete
// (computeCampaignNextAction).
type CampaignNextAction struct {
	Action   string `json:"action"`
	IssueRef string `json:"issue_ref,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// CampaignStatus mirrors the GET /v0/campaigns/{id}/status response body: the
// campaign + its items + the engine's readiness rollup + the distilled
// next_action. This is the surface the operator-agent polls to drive a campaign.
type CampaignStatus struct {
	Campaign   Campaign           `json:"campaign"`
	Items      []CampaignItem     `json:"items"`
	Rollup     CampaignRollup     `json:"rollup"`
	NextAction CampaignNextAction `json:"next_action"`
}

// campaignCreateRequest mirrors the backend's POST /v0/campaigns body
// (`backend/internal/server/campaigns.go::createCampaignRequest`). Repeated
// here for the same thin-local-copy reason as createRunRequest.
type campaignCreateRequest struct {
	Repo        string `json:"repo"`
	EpicRef     string `json:"epic_ref"`
	PausePolicy string `json:"pause_policy,omitempty"`
	// OperatorAgent is the OPTIONAL campaign-level operator_agent override
	// (E25.12 / #1451), carried as opaque JSON the backend validates against
	// spec.OperatorAgent (unknown fields rejected -> 400 validation_failed).
	// json.RawMessage (not map[string]any) here because this is an HTTP request
	// body, not an MCP tool schema — omitempty drops a nil/empty value so a
	// campaign without an override sends no operator_agent key.
	OperatorAgent json.RawMessage `json:"operator_agent,omitempty"`
}

// CreateCampaign assembles a campaign from an epic ref via
// `POST /v0/campaigns` (E25.4) and returns the created campaign (201 fresh).
// pausePolicy is optional — empty normalizes to pause_campaign server-side.
// operatorAgent is the OPTIONAL campaign-level operator_agent override (E25.12 /
// #1451) carried as opaque JSON; empty/nil omits the field so the campaign
// inherits each issue-run's workflow contract. A write tool: requires an
// operator token with write:campaigns scope. 4xx/5xx surfaces as *apiError; the
// tool layer reads the code:
//   - 400 validation_failed (repo not owner/name, empty epic_ref, bad
//     pause_policy, a malformed/unknown-field operator_agent, or a dependency
//     cycle)
//   - 403 insufficient_scope (token lacks write:campaigns)
//   - 422 repo_not_installed (the GitHub App is not on the target repo)
//   - 422 campaign_dangling_dependency (a depends_on target is not a fellow child)
//   - 503 campaign_repo_unconfigured (no campaign repository wired on the deploy)
func (c *apiClient) CreateCampaign(ctx context.Context, repo, epicRef, pausePolicy string, operatorAgent json.RawMessage) (*Campaign, error) {
	body, err := json.Marshal(campaignCreateRequest{Repo: repo, EpicRef: epicRef, PausePolicy: pausePolicy, OperatorAgent: operatorAgent})
	if err != nil {
		return nil, fmt.Errorf("marshal create campaign: %w", err)
	}
	var camp Campaign
	if _, err := c.doWithStatus(ctx, http.MethodPost, "/v0/campaigns", body, nil, &camp); err != nil {
		return nil, err
	}
	return &camp, nil
}

// GetCampaignStatus reads the campaign rollup + distilled next_action via
// `GET /v0/campaigns/{id}/status` (E25.4) — the surface the operator-agent
// polls to drive a campaign. Read-only. 4xx/5xx surfaces:
//   - 400 validation_failed (campaign_id not a UUID)
//   - 404 campaign_not_found
//   - 503 campaign_repo_unconfigured
func (c *apiClient) GetCampaignStatus(ctx context.Context, id uuid.UUID) (*CampaignStatus, error) {
	var st CampaignStatus
	if err := c.do(ctx, http.MethodGet, "/v0/campaigns/"+id.String()+"/status", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// ResumeCampaign hands a paused campaign back to the auto-driver via
// `POST /v0/campaigns/{id}/resume` (E25.7) — the operator's hand-back after the
// driver paged a human at a run gate. It flips a paused campaign (and every
// paused item) back to running. A write tool: requires write:campaigns. 4xx/5xx
// surfaces:
//   - 400 validation_failed (campaign_id not a UUID)
//   - 403 insufficient_scope (token lacks write:campaigns)
//   - 404 campaign_not_found
//   - 409 campaign_not_paused (nothing is paused on either axis — no item and
//     not the campaign — so there is nothing to resume)
//   - 503 campaign_repo_unconfigured
func (c *apiClient) ResumeCampaign(ctx context.Context, id uuid.UUID) (*Campaign, error) {
	var camp Campaign
	if err := c.do(ctx, http.MethodPost, "/v0/campaigns/"+id.String()+"/resume", nil, &camp); err != nil {
		return nil, err
	}
	return &camp, nil
}

// startCampaignItemRunRequest mirrors the backend's POST
// /v0/campaigns/{campaign_id}/runs body
// (`backend/internal/server/campaigns.go::startCampaignItemRunRequest`).
// Repeated here for the same thin-local-copy reason as createRunRequest. There
// is deliberately NO idempotency_key field — the backend does not dedup this
// create-link-transition sequence, so the request shape advertises none (#1443
// honesty).
type startCampaignItemRunRequest struct {
	IssueRef    string `json:"issue_ref"`
	WorkflowID  string `json:"workflow_id"`
	WorkflowRef string `json:"workflow_ref,omitempty"`
	RunnerKind  string `json:"runner_kind,omitempty"`
}

// StartCampaignItemRunResult mirrors the backend's POST
// /v0/campaigns/{campaign_id}/runs 201 body: the minted run plus the linked
// campaign item (now running, with run_id set).
type StartCampaignItemRunResult struct {
	Run  Run          `json:"run"`
	Item CampaignItem `json:"item"`
}

// StartCampaignItemRun starts a run for an eligible campaign item via
// `POST /v0/campaigns/{campaign_id}/runs` (E26.2 / #1481) — the operator-driven,
// campaign-aware run start that DAG-gates each run and links it to the campaign
// so the rollup advances as the operator drives the loop. workflowRef empty =
// the repo's default branch; runnerKind empty = github_actions ("local" for the
// local dogfood loop). A write tool: requires write:campaigns. 4xx/5xx surfaces:
//   - 400 validation_failed (campaign_id not a UUID, empty issue_ref/workflow_id,
//     bad runner_kind, unknown fields)
//   - 403 insufficient_scope (token lacks write:campaigns)
//   - 404 campaign_not_found / campaign_item_not_found
//   - 409 campaign_not_startable (the campaign is paused or terminal)
//   - 409 item_not_eligible (the item is blocked on a dependency, already
//     running, or terminal — the detail names the blocker)
//   - 502 campaign_run_start_failed (the installation/spec could not be resolved)
//   - 503 campaign_repo_unconfigured
func (c *apiClient) StartCampaignItemRun(ctx context.Context, campaignID uuid.UUID, issueRef, workflowID, workflowRef, runnerKind string) (*StartCampaignItemRunResult, error) {
	body, err := json.Marshal(startCampaignItemRunRequest{
		IssueRef:    issueRef,
		WorkflowID:  workflowID,
		WorkflowRef: workflowRef,
		RunnerKind:  runnerKind,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal start campaign item run: %w", err)
	}
	var res StartCampaignItemRunResult
	if err := c.do(ctx, http.MethodPost, "/v0/campaigns/"+campaignID.String()+"/runs", body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// FileWorkItemRequest mirrors the backend's POST /v0/work-items body
// (`backend/internal/server/workitems.go::workItemRequest`). The
// conventions layer turns this provider-neutral filing into a created
// item; only Repo, Type, and Summary are required. Repeated here rather
// than imported because the MCP server's apiClient is deliberately a thin
// local copy — the import-direction rule is `cli → backend`, not the
// reverse.
type FileWorkItemRequest struct {
	Repo            string             `json:"repo"`
	Type            string             `json:"type"`
	Summary         string             `json:"summary"`
	Body            string             `json:"body,omitempty"`
	Sections        map[string]string  `json:"sections,omitempty"`
	TitleVars       map[string]string  `json:"title_vars,omitempty"`
	Labels          []string           `json:"labels,omitempty"`
	Complexity      string             `json:"complexity,omitempty"`
	Status          string             `json:"status,omitempty"`
	Relations       *WorkItemRelations `json:"relations,omitempty"`
	ExistingNumbers []int              `json:"existing_numbers,omitempty"`
	RunID           string             `json:"run_id,omitempty"`
}

// WorkItemRelations mirrors the wire `relations` sub-object: the
// provider-neutral links the conventions layer resolves into provider
// link operations.
type WorkItemRelations struct {
	ParentEpic   string   `json:"parent_epic,omitempty"`
	Supersedes   []string `json:"supersedes,omitempty"`
	CompanionTo  []string `json:"companion_to,omitempty"`
	EvidenceRuns []string `json:"evidence_runs,omitempty"`
	DependsOn    []string `json:"depends_on,omitempty"`
}

// FiledWorkItem mirrors the backend's WorkItemResponse: the created item,
// echoing the conventions-resolved placement so the caller renders the
// result without a second fetch. Audited is true only when a
// work_item_filed audit entry was written (a run was in flight).
type FiledWorkItem struct {
	Type          string   `json:"type"`
	Title         string   `json:"title"`
	Number        int      `json:"number"`
	URL           string   `json:"url"`
	Provider      string   `json:"provider"`
	AppliedLabels []string `json:"applied_labels,omitempty"`
	Complexity    string   `json:"complexity,omitempty"`
	Status        string   `json:"status,omitempty"`
	BoardColumn   string   `json:"board_column,omitempty"`
	// Boarded / EpicLinked report whether the best-effort post-create
	// enrichment landed (#1107). Board placement and epic linking are no
	// longer fatal: the issue is the durable result, so a placement/link
	// failure files the issue (boarded/epic_linked false) and carries the
	// cause in BoardingError / EpicLinkError rather than a 502.
	Boarded       bool   `json:"boarded"`
	EpicLinked    bool   `json:"epic_linked"`
	BoardingError string `json:"boarding_error,omitempty"`
	EpicLinkError string `json:"epic_link_error,omitempty"`
	Audited       bool   `json:"audited"`
	// DefaultedLabels / MissingLabelNamespaces surface the backend's LOUD
	// label-completeness report (#1616): every label the system added that the
	// caller did not supply (namespace defaults + handler-derived area), and
	// any required namespace still absent after merge/derivation/defaulting. A
	// missing namespace is reported, never a rejection.
	DefaultedLabels        []string `json:"defaulted_labels,omitempty"`
	MissingLabelNamespaces []string `json:"missing_label_namespaces,omitempty"`
}

// FileWorkItem files a provider-agnostic work item via
// `POST /v0/work-items` (#1005). The backend loads the repo's
// work-management conventions, applies them, dispatches to the registered
// provider, and (when run_id names an in-flight run) writes a best-effort
// work_item_filed audit entry. 4xx/5xx surface as *apiError; the tool
// layer reads the code:
//   - 400 validation_failed (repo not owner/name, missing type/summary,
//     unknown fields)
//   - 401 authentication_required (anonymous caller)
//   - 422 work_item_invalid (the request violates the type's conventions)
//   - 501 provider_unimplemented (the configured provider id — e.g. the
//     interface-only jira — is not registered; details name it)
//   - 502 work_item_filing_failed (the provider rejected the filing)
func (c *apiClient) FileWorkItem(ctx context.Context, req FileWorkItemRequest) (*FiledWorkItem, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal file-work-item: %w", err)
	}
	var out FiledWorkItem
	if err := c.do(ctx, http.MethodPost, "/v0/work-items", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RefinementDecision mirrors the backend's RefinementDecision schema: one
// append-only approve/reject verdict pinning a draft revision + its content
// hash. DraftID is a `string` (not uuid.UUID) so the MCP SDK's reflection-built
// output schema sees a string, not a 16-byte array (the #371 trap).
type RefinementDecision struct {
	Decision         string    `json:"decision" jsonschema:"approved or rejected"`
	Reason           string    `json:"reason"`
	DraftID          string    `json:"draft_id" jsonschema:"the decided revision's id"`
	DraftContentHash string    `json:"draft_content_hash" jsonschema:"sha256 of the decoded EpicDraft the decision pinned"`
	DecidedBy        string    `json:"decided_by,omitempty" jsonschema:"the deciding identity's subject; absent when unknown"`
	CreatedAt        time.Time `json:"created_at"`
}

// RefinementAcceptanceFinding mirrors one deterministic acceptance-criteria
// defect the intake pre-check flagged (backend plan.AcceptanceFinding): the
// machine-readable rule name, the offending criterion id (the criterion text at
// intake), and a human-readable detail.
type RefinementAcceptanceFinding struct {
	Rule        string `json:"rule" jsonschema:"the machine-readable rule: no_blocking_criterion, missing_source_ref, missing_rationale, empty_id, or duplicate_id"`
	CriterionID string `json:"criterion_id,omitempty" jsonschema:"the offending criterion (its text at intake); absent for the presence-level no_blocking_criterion finding"`
	Detail      string `json:"detail" jsonschema:"a short human-readable explanation of the defect"`
}

// ChildCriteriaCheck mirrors the backend's per-child intake acceptance-criteria
// pre-check: the 1-based child ordinal, its needs_attention marker, and its
// findings ([] when checked-and-clean).
type ChildCriteriaCheck struct {
	Ordinal        int                           `json:"ordinal" jsonschema:"the 1-based child ordinal this check is for"`
	NeedsAttention bool                          `json:"needs_attention,omitempty" jsonschema:"true when this child has an unjustified no_blocking_criterion finding (advisory — approval remains legal)"`
	Findings       []RefinementAcceptanceFinding `json:"findings" jsonschema:"the child's acceptance-criteria findings; [] when checked and clean"`
}

// CriteriaPrecheck mirrors the backend's E34.5 advisory acceptance-criteria
// pre-check over the latest draft (#1596): per-child findings plus a
// draft-level needs_attention marker. Advisory only — a flagged draft can still
// be approved; the guidance names the flagged child ordinals so the operator
// sees the defect before deciding.
type CriteriaPrecheck struct {
	NeedsAttention bool                 `json:"needs_attention" jsonschema:"true when any child has an unjustified no_blocking_criterion finding (advisory — approval remains legal)"`
	Children       []ChildCriteriaCheck `json:"children" jsonschema:"the per-child acceptance-criteria checks (one per draft child)"`
}

// RefinementSession mirrors the backend's RefinementSession schema
// (docs/api/v0.openapi.yaml): the refinement gate's session view — the DERIVED
// approval state, the revision count, the latest EpicDraft, the full filing
// preview, the wave DAG, the advisory acceptance-criteria pre-check, and the
// decision history. State is derived, never stored: a decision counts only when
// it targets the latest revision and its pinned hash still matches, so an edit
// after approval re-gates the session.
//
// SessionID is a `string` (not uuid.UUID) so the MCP SDK's reflection-built
// output schema sees a string (the #371 trap). Preview is []map[string]any —
// each item is an opaque work-item render (the backend serializes
// []workmgmt.WorkItem), typed as map[string]any (not json.RawMessage) so the
// SDK's schema reflection sees an object, not a base64 string.
type RefinementSession struct {
	SessionID        string               `json:"session_id"`
	State            string               `json:"state" jsonschema:"awaiting_approval, approved, or rejected (derived)"`
	Drifted          bool                 `json:"drifted,omitempty" jsonschema:"true when the latest revision's decision pins a content hash that no longer matches (fail-closed to awaiting_approval)"`
	RevisionCount    int                  `json:"revision_count" jsonschema:"number of draft revisions in the session"`
	LatestOrigin     string               `json:"latest_origin" jsonschema:"how the latest revision came to exist: brief, amendment, or edit"`
	LatestDraft      EpicDraft            `json:"latest_draft" jsonschema:"the latest structured epic/children draft"`
	Preview          []map[string]any     `json:"preview" jsonschema:"the full filing preview — the epic then each child, rendered exactly as it would file"`
	Waves            [][]int              `json:"waves" jsonschema:"the topological dispatch order as waves of 1-based child ordinals"`
	CriteriaPrecheck CriteriaPrecheck     `json:"criteria_precheck" jsonschema:"the advisory acceptance-criteria pre-check over the latest draft's children; needs_attention flags an unjustified missing blocking criterion (approval remains legal)"`
	Decisions        []RefinementDecision `json:"decisions" jsonschema:"the append-only decision history"`
}

// RefinementFilingEpic mirrors the file response's `epic` sub-object.
type RefinementFilingEpic struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

// RefinementFilingChild mirrors one filed child in the file response.
type RefinementFilingChild struct {
	Ordinal int    `json:"ordinal" jsonschema:"1-based draft child ordinal"`
	Number  int    `json:"number"`
	URL     string `json:"url"`
}

// RefinementFilingResult mirrors the backend's POST .../file 200 body: the
// outcome of filing an approved draft into tracker items (fresh, resumed, or an
// already-completed replay). SessionID / DraftID are strings (the #371 trap).
type RefinementFilingResult struct {
	SessionID        string                  `json:"session_id"`
	DraftID          string                  `json:"draft_id"`
	Repo             string                  `json:"repo"`
	Epic             RefinementFilingEpic    `json:"epic"`
	Children         []RefinementFilingChild `json:"children"`
	Resumed          bool                    `json:"resumed" jsonschema:"true when this invocation resumed a partially-filed session"`
	AlreadyCompleted bool                    `json:"already_completed" jsonschema:"true when replaying a fully-completed session (no writes performed)"`
	Verified         bool                    `json:"verified" jsonschema:"true when the filed epic passed the epic-children + campaign-assembly round-trip"`
}

// createRefinementSessionRequest mirrors the backend's POST
// /v0/refinement/sessions body.
type createRefinementSessionRequest struct {
	Brief string `json:"brief"`
}

// editRefinementDraftRequest mirrors the backend's PATCH
// /v0/refinement/sessions/{id}/draft body. Exactly one arm is serialized (both are
// omitempty): brief_amendment (agent re-draft) XOR draft (direct edit). The
// caller (the tool handler) guarantees the XOR before this is built.
type editRefinementDraftRequest struct {
	BriefAmendment string     `json:"brief_amendment,omitempty"`
	Draft          *EpicDraft `json:"draft,omitempty"`
}

// decideRefinementSessionRequest mirrors the backend's POST .../decision body.
type decideRefinementSessionRequest struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// fileRefinementSessionRequest mirrors the backend's POST .../file body.
type fileRefinementSessionRequest struct {
	Repo string `json:"repo"`
}

// CreateRefinementSession opens a refinement session over a natural-language
// brief via `POST /v0/refinement/sessions` (E34.2, ADR-052 option A): it drafts
// the initial epic/children revision and returns the session view. Nothing
// files here. Requires write:approvals. 4xx/5xx surface as *apiError:
//   - 400 validation_failed (malformed JSON / unknown fields)
//   - 403 insufficient_scope (token lacks write:approvals)
//   - 422 validation_failed (brief is empty)
//   - 502 refinement_drafting_failed (the drafting agent produced no valid draft)
//   - 503 refinement_repo_unconfigured / refinement_drafting_unavailable
func (c *apiClient) CreateRefinementSession(ctx context.Context, brief string) (*RefinementSession, error) {
	body, err := json.Marshal(createRefinementSessionRequest{Brief: brief})
	if err != nil {
		return nil, fmt.Errorf("marshal create-refinement-session: %w", err)
	}
	var out RefinementSession
	// Agent-backed open arm: the drafter runs for minutes, so route through the
	// long client (refinementDraftClientTimeout) — the 30s short client would
	// abort mid-inference and cancel the request context.
	if err := c.doLong(ctx, http.MethodPost, "/v0/refinement/sessions", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetRefinementSession reads a session's preview + derived approval state via
// `GET /v0/refinement/sessions/{id}` (E34.2). Requires write:approvals. 4xx/5xx
// surface as *apiError: 400 validation_failed (non-UUID id), 403
// insufficient_scope, 404 refinement_session_not_found, 503
// refinement_repo_unconfigured.
func (c *apiClient) GetRefinementSession(ctx context.Context, sessionID uuid.UUID) (*RefinementSession, error) {
	var out RefinementSession
	if err := c.do(ctx, http.MethodGet, "/v0/refinement/sessions/"+sessionID.String(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// EditRefinementDraft appends a new draft revision via `PATCH
// /v0/refinement/sessions/{id}/draft` (E34.2) — which is precisely what
// invalidates a prior approval. Exactly one arm: briefAmendment (non-empty -> agent re-draft,
// origin=amendment, bounded by a per-session budget of 3) XOR draft (non-nil ->
// a direct strict-decoded EpicDraft edit, origin=edit, no agent call). The
// caller guarantees the XOR. Requires write:approvals. 4xx/5xx surface as
// *apiError:
//   - 400 validation_failed (malformed JSON / unknown fields / non-UUID id)
//   - 403 insufficient_scope
//   - 404 refinement_session_not_found
//   - 409 amendment_budget_exhausted (the brief-amendment budget is spent)
//   - 422 validation_failed (neither/both arms, or a draft that fails strict
//     decode/validation — an empty field, a dangling or cyclic depends_on edge)
//   - 500 audit_append_failed (the edit's audit entry could not be recorded)
//   - 502 refinement_drafting_failed (brief-amendment arm)
//   - 503 refinement_repo_unconfigured / refinement_drafting_unavailable
func (c *apiClient) EditRefinementDraft(ctx context.Context, sessionID uuid.UUID, briefAmendment string, draft *EpicDraft) (*RefinementSession, error) {
	body, err := json.Marshal(editRefinementDraftRequest{BriefAmendment: briefAmendment, Draft: draft})
	if err != nil {
		return nil, fmt.Errorf("marshal edit-refinement-draft: %w", err)
	}
	var out RefinementSession
	path := "/v0/refinement/sessions/" + sessionID.String() + "/draft"
	// The brief-amendment arm re-runs the drafting agent (minutes), so route it
	// through the long client; the direct `draft` edit is a fast strict-decode
	// with no agent call and stays on the 30s short client.
	doFn := c.do
	if briefAmendment != "" {
		doFn = c.doLong
	}
	if err := doFn(ctx, http.MethodPatch, path, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DecideRefinementSession records an append-only approve/reject verdict pinning
// the latest revision's draft_id + content hash via `POST .../decision`
// (E34.2). reason is REQUIRED. A second decision on the same revision is 409
// (re-gate by editing, not by deciding twice). Requires write:approvals.
// 4xx/5xx surface as *apiError:
//   - 400 validation_failed (malformed JSON / unknown fields / non-UUID id)
//   - 403 insufficient_scope
//   - 404 refinement_session_not_found
//   - 409 decision_already_recorded (the latest revision already carries a decision)
//   - 422 validation_failed (decision not approved/rejected, or a blank reason)
//   - 500 audit_append_failed
//   - 503 refinement_repo_unconfigured
func (c *apiClient) DecideRefinementSession(ctx context.Context, sessionID uuid.UUID, decision, reason string) (*RefinementSession, error) {
	body, err := json.Marshal(decideRefinementSessionRequest{Decision: decision, Reason: reason})
	if err != nil {
		return nil, fmt.Errorf("marshal decide-refinement-session: %w", err)
	}
	var out RefinementSession
	if err := c.do(ctx, http.MethodPost, "/v0/refinement/sessions/"+sessionID.String()+"/decision", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FileRefinementSession files an approved, un-drifted draft into tracker items
// (the epic then children in wave order) via `POST .../file` (E34.3). It is
// IDEMPOTENT: the target repo is pinned at first invoke (a re-invoke naming a
// different repo is 409 refinement_filing_repo_mismatch); a mid-sequence
// provider failure is 502 refinement_filing_failed with the filed-so-far items
// + failing ordinal in details, and re-invoking resumes at the first unfiled
// ordinal; a fully completed session replays as 200 with already_completed.
// Requires write:approvals (no new scope — the E34.2 precedent). 4xx/5xx
// surface as *apiError:
//   - 400 validation_failed (malformed JSON / unknown fields / non-UUID id /
//     repo not owner/name)
//   - 403 insufficient_scope
//   - 404 refinement_session_not_found
//   - 409 refinement_not_approved / refinement_draft_drifted /
//     refinement_filing_repo_mismatch
//   - 500 internal_error (audit_append_failed on the completion close)
//   - 502 refinement_filing_failed (resumable) /
//     refinement_filing_verification_failed
//   - 503 refinement_repo_unconfigured
func (c *apiClient) FileRefinementSession(ctx context.Context, sessionID uuid.UUID, repo string) (*RefinementFilingResult, error) {
	body, err := json.Marshal(fileRefinementSessionRequest{Repo: repo})
	if err != nil {
		return nil, fmt.Errorf("marshal file-refinement-session: %w", err)
	}
	var out RefinementFilingResult
	if err := c.do(ctx, http.MethodPost, "/v0/refinement/sessions/"+sessionID.String()+"/file", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DiagnosticBundle mirrors the backend's product-facts-only diagnostic
// bundle (GET /v0/runs/{run_id}/diagnostics, #1006). Thin local copy —
// same import-direction rule as the other mirrored shapes here. Every
// field is a structured product fact safe to surface verbatim; the
// bundle carries NO diffs, paths, prompts, or free text by construction,
// so the report tool can echo it as a transparency preview of what its
// egress attached.
type DiagnosticBundle struct {
	RunID              string                  `json:"run_id"`
	WorkflowID         string                  `json:"workflow_id"`
	WorkflowSpecHash   string                  `json:"workflow_spec_hash"`
	RunnerKind         string                  `json:"runner_kind"`
	RunState           string                  `json:"run_state"`
	Stages             []DiagnosticStageFact   `json:"stages,omitempty"`
	FailingStage       *DiagnosticFailingStage `json:"failing_stage,omitempty"`
	AuditSequenceRange *DiagnosticSeqRange     `json:"audit_sequence_range,omitempty"`
	Versions           DiagnosticVersions      `json:"versions"`
}

// DiagnosticStageFact is one stage's position + state in the bundle.
type DiagnosticStageFact struct {
	Sequence int    `json:"sequence"`
	Type     string `json:"type"`
	State    string `json:"state"`
}

// DiagnosticFailingStage names which stage failed and how (structured
// facts only — category + audit-surface enum, never the free-text
// failure reason).
type DiagnosticFailingStage struct {
	Sequence        int    `json:"sequence"`
	Type            string `json:"type"`
	FailureCategory string `json:"failure_category"`
	FailureSurface  string `json:"failure_surface,omitempty"`
}

// DiagnosticSeqRange is the [min,max] of the run's audit sequence numbers.
type DiagnosticSeqRange struct {
	Min int64 `json:"min"`
	Max int64 `json:"max"`
}

// DiagnosticVersions carries the backend's build identity.
type DiagnosticVersions struct {
	Fishhawkd        DiagnosticComponent `json:"fishhawkd"`
	MinRunnerVersion string              `json:"min_runner_version"`
}

// DiagnosticComponent is a single build's version + git SHA.
type DiagnosticComponent struct {
	Version string `json:"version"`
	GitSHA  string `json:"git_sha"`
}

// GetDiagnostics fetches a run's product-facts-only diagnostic bundle via
// `GET /v0/runs/{run_id}/diagnostics` (#1006, slice 1). Read-only; the
// report tool uses it to surface a transparency preview of exactly which
// structured facts its egress attached. 4xx surfaces as *apiError.
func (c *apiClient) GetDiagnostics(ctx context.Context, runID uuid.UUID) (*DiagnosticBundle, error) {
	var b DiagnosticBundle
	if err := c.do(ctx, http.MethodGet, "/v0/runs/"+runID.String()+"/diagnostics", nil, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// productReportBody mirrors the backend's
// `POST /v0/runs/{run_id}/product-reports` request body
// (`backend/internal/server/product_report.go::productReportRequest`).
// Kind selects the report flavor (bug default; feature). Description is
// operator free text that crosses the boundary ONLY when IncludeFreeText
// is true, and is run through the shared redaction module server-side
// first (#1006, slice 3 consent boundary).
type productReportBody struct {
	Kind            string `json:"kind,omitempty"`
	Description     string `json:"description,omitempty"`
	IncludeFreeText bool   `json:"include_free_text,omitempty"`
}

// ProductReport mirrors the backend's product-report response: what left
// the boundary, echoed so the caller renders the outcome without a second
// fetch. Action is "created" on a dedup miss or "occurrence" on a hit.
type ProductReport struct {
	Fingerprint string `json:"fingerprint"`
	Action      string `json:"action"`
	Number      int    `json:"number"`
	URL         string `json:"url"`
	Destination string `json:"destination"`
}

// ReportProductIssue files a deduped, audited upstream product report for
// a run via `POST /v0/runs/{run_id}/product-reports` (#1006). The backend
// collects the run's product-facts bundle, fingerprints the failure,
// dedup-searches the fixed product repo, and either files a new
// fingerprint-marked report or appends an occurrence comment — then writes
// a source-side product_report_filed audit entry. Free text crosses only
// when includeFreeText is true (server-side redacted first). 4xx/5xx
// surface as *apiError; the tool layer reads the code:
//   - 400 validation_failed (bad run_id / kind, unknown fields)
//   - 401 authentication_required (anonymous caller)
//   - 403 run_not_entitled (not the run's own run-bound token)
//   - 403 product_feedback_disabled (per-repo kill-switch)
//   - 404 run_not_found
//   - 501 provider_unimplemented (the configured feedback provider id is
//     not registered)
//   - 502 product_report_failed (the dedup search / file / comment failed)
func (c *apiClient) ReportProductIssue(ctx context.Context, runID uuid.UUID, kind, description string, includeFreeText bool) (*ProductReport, error) {
	body, err := json.Marshal(productReportBody{
		Kind:            kind,
		Description:     description,
		IncludeFreeText: includeFreeText,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal product-report: %w", err)
	}
	var out ProductReport
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+runID.String()+"/product-reports", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
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

// releaseNotesPersistRequest mirrors the backend's POST /v0/releases/notes
// request body (`backend/internal/server/release_notes.go`, E33.2 / #1587).
// stage_id keys the persisted release_notes artifact — the persist endpoint is
// stage-scoped because no first-class release stage type exists yet. Repeated
// here (not imported) per the thin-local-copy rule: the import direction is
// cli → backend, not the reverse.
type releaseNotesPersistRequest struct {
	Repo    string `json:"repo"`
	From    string `json:"from"`
	To      string `json:"to"`
	StageID string `json:"stage_id"`
}

// ReleaseNotesPersistResult mirrors the backend's POST /v0/releases/notes 201
// body: the persisted artifact id, the coordinates, the content hash, and the
// rendered markdown (which carries the advisory semver bump hint after E33.4).
// IDs are typed string per the #371 reflection rule so the MCP SDK's schema
// reflection sees a string, not a uuid byte array.
type ReleaseNotesPersistResult struct {
	ArtifactID  string `json:"artifact_id"`
	StageID     string `json:"stage_id"`
	Repo        string `json:"repo"`
	From        string `json:"from"`
	To          string `json:"to"`
	ContentHash string `json:"content_hash"`
	Markdown    string `json:"markdown"`
}

// PreviewReleaseNotes renders the release-notes markdown for the ref range via
// GET /v0/releases/notes/preview?repo=&from=&to= (E33.2 / #1587) WITHOUT
// persisting anything. The endpoint responds with a text/markdown body (NOT a
// JSON envelope), so this reads the raw body via getText rather than decoding
// into a struct. 4xx surfaces as *apiError:
//   - 400 validation_failed (missing repo/from/to)
//   - 401 authentication_required (anonymous)
//   - 503 release_notes_unconfigured (a required repository is not wired)
func (c *apiClient) PreviewReleaseNotes(ctx context.Context, repo, from, to string) (string, error) {
	q := url.Values{}
	q.Set("repo", repo)
	q.Set("from", from)
	q.Set("to", to)
	return c.getText(ctx, "/v0/releases/notes/preview?"+q.Encode())
}

// PersistReleaseNotes renders exactly as the preview endpoint and persists the
// notes as a release_notes artifact keyed to stageID, via
// POST /v0/releases/notes (E33.2 / #1587). Returns the persisted artifact id +
// coordinates + rendered markdown. 4xx/5xx surfaces as *apiError:
//   - 400 validation_failed (missing repo/from/to/stage_id, malformed stage_id)
//   - 401 authentication_required (anonymous) / 403 insufficient_scope (needs
//     write:runs)
//   - 404 stage_not_found (stage_id references no stages row)
//   - 503 release_notes_unconfigured (a required repository is not wired)
func (c *apiClient) PersistReleaseNotes(ctx context.Context, repo, from, to, stageID string) (*ReleaseNotesPersistResult, error) {
	body, err := json.Marshal(releaseNotesPersistRequest{Repo: repo, From: from, To: to, StageID: stageID})
	if err != nil {
		return nil, fmt.Errorf("marshal release-notes persist: %w", err)
	}
	var res ReleaseNotesPersistResult
	if err := c.do(ctx, http.MethodPost, "/v0/releases/notes", body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// getText performs a GET and returns the raw response body as a string. Unlike
// do, it does NOT json-decode the body — used for the text/markdown
// release-notes preview (E33.2), whose body is rendered markdown, not a JSON
// envelope. On a non-2xx response the body IS parsed as the OpenAPI error
// envelope and returned as *apiError, so callers get the same typed error
// surface as the JSON methods. Routed through the 30s short client.
func (c *apiClient) getText(ctx context.Context, path string) (string, error) {
	if c.baseURL == "" {
		return "", errors.New("apiClient: baseURL not set")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "text/markdown")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
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
		return "", ae
	}
	return string(raw), nil
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
	// EntryHash carries omitempty so the compact get_run_status projection can
	// blank it to drop the verifier-only hash-chain field from the wire (#1749).
	// Safe for fishhawk_list_audit: a real audit entry always has a non-empty
	// entry_hash there, so omitempty never elides it on the verifier surface.
	EntryHash string `json:"entry_hash,omitempty"`
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
	// AllowUnknown sets allow_unknown=true on the request (#1764), telling
	// the endpoint to skip its known-category validation. The MCP tool sets
	// it when the operator has opted into an unknown category via
	// fishhawk_await_audit's allow_unknown flag, so the tool's own polling
	// calls are not re-rejected by the endpoint. False omits the param and
	// stays byte-identical to the prior request.
	AllowUnknown bool
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
	if f.AllowUnknown {
		q.Set("allow_unknown", "true")
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

// PlanDecomposed is the decoded plan_decomposed audit payload (E24.6 /
// #1146) the orchestrator emits when it mints a decomposed parent's
// children: the minted child run ids and the orchestrator-resolved
// effective concurrency cap (0 == unlimited). The fishhawk_run_children
// tool reads it to discover which children to dispatch and at what
// concurrency — the MCP cannot reach the workflow spec or the
// FISHHAWK_MAX_PARALLEL_CHILDREN default, so the cap is read from here.
type PlanDecomposed struct {
	ChildRunIDs          []string `json:"child_run_ids"`
	EffectiveMaxParallel int      `json:"effective_max_parallel"`
	// Waves carries the topological dispatch order (#1258 slice B) as ordered
	// waves of slice indices into ChildRunIDs (ChildRunIDs[i] is the child
	// minted for slice i). The run_children wave loop dispatches each wave
	// concurrently and integrates between waves. omitempty + nil-decodes
	// back-compat: an old plan_decomposed entry (or a no-depends_on
	// decomposition) carries no waves, which the loop collapses to a single
	// all-indices wave.
	Waves [][]int `json:"waves,omitempty"`
}

// LatestPlanDecomposed returns the decoded payload of the run's most-recent
// plan_decomposed audit entry, or (nil, nil) when the run has none (it is
// not a decomposed parent). The per-run audit endpoint returns entries
// sequence-ascending, so the authoritative entry is the last one. A corrupt
// payload surfaces as a decode error — unlike the best-effort plan-gate
// advisory reads (loadScopePrecheck et al.), run_children cannot proceed
// without the child ids, so a malformed entry must fail loud rather than
// silently dispatch nothing.
func (c *apiClient) LatestPlanDecomposed(ctx context.Context, runID uuid.UUID) (*PlanDecomposed, error) {
	entries, _, err := c.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: "plan_decomposed",
		Limit:    reviewAuditQueryLimit,
	})
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	newest := entries[len(entries)-1]
	if newest.Payload == nil {
		return nil, fmt.Errorf("plan_decomposed entry %s has no payload", newest.ID)
	}
	raw, err := json.Marshal(newest.Payload)
	if err != nil {
		return nil, fmt.Errorf("re-encode plan_decomposed payload: %w", err)
	}
	var pd PlanDecomposed
	if err := json.Unmarshal(raw, &pd); err != nil {
		return nil, fmt.Errorf("decode plan_decomposed payload: %w", err)
	}
	return &pd, nil
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

// doLong is the long-client analogue of do: it routes the request through
// c.httpLong (refinementDraftClientTimeout) instead of the 30s short client,
// for the two agent-backed refinement arms whose bodies take minutes.
func (c *apiClient) doLong(ctx context.Context, method, path string, body []byte, out any) error {
	_, err := c.doWithStatusUsing(c.httpLong, ctx, method, path, body, nil, out)
	return err
}

// doWithStatus performs the request on the 30s short client. See
// doWithStatusUsing for the mechanics.
func (c *apiClient) doWithStatus(ctx context.Context, method, path string, body []byte, extraHeaders map[string]string, out any) (int, error) {
	return c.doWithStatusUsing(c.http, ctx, method, path, body, extraHeaders, out)
}

// doWithStatusUsing performs the request on the supplied client and decodes the
// JSON body into out. On non-2xx the body is parsed as the OpenAPI error
// envelope and returned as *apiError. `extraHeaders` is merged into the
// request — used for E8.2's Idempotency-Key on POST /v0/runs. Same posture as
// the CLI's httpclient.do. The client parameter selects the per-arm timeout
// (short vs refinementDraftClientTimeout).
func (c *apiClient) doWithStatusUsing(client *http.Client, ctx context.Context, method, path string, body []byte, extraHeaders map[string]string, out any) (int, error) {
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
	resp, err := client.Do(req)
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
