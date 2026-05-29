package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// runResolver bundles the API client + an env getter so the tool
// handlers can read FISHHAWK_RUN_ID / GITHUB_REPOSITORY without
// reaching into os.Getenv directly. Tests substitute an envFunc
// backed by a literal map; production passes os.Getenv.
type runResolver struct {
	api    *apiClient
	getenv func(string) string
}

// registerTools wires every MCP tool onto srv. Called once at
// server startup; the SDK keeps the handlers alive for the
// lifetime of the stdio session. Tools register in alphabetical
// order so the protocol's tool-listing endpoint returns a stable
// ordering for clients that index on position.
func registerTools(srv *mcp.Server, resolver *runResolver) {
	registerGetActiveRun(srv, resolver)
	registerGetPlan(srv, resolver)
	registerGetRunStatus(srv, resolver)
	registerListAudit(srv, resolver)
	registerStartRun(srv, resolver)
	registerCancelRun(srv, resolver)
	registerRetryStage(srv, resolver)
	registerApprovePlan(srv, resolver)
	registerRejectPlan(srv, resolver)
	registerListRuns(srv, resolver)
	registerRunStage(srv, resolver)
	registerRuntimeCalibration(srv, resolver)
	registerVerifyRun(srv, resolver)
}

// GetActiveRunInput is the tool's input schema (E19.3 / #343). All
// fields optional — every field is a hint the resolver may use to
// pick the right run.
type GetActiveRunInput struct {
	Repo       string `json:"repo,omitempty" jsonschema:"GitHub repo as owner/name; falls back to GITHUB_REPOSITORY env when omitted"`
	PRNumber   int    `json:"pr_number,omitempty" jsonschema:"GitHub pull request number; resolves to the most-recent run whose pull_request_url matches"`
	TriggerRef string `json:"trigger_ref,omitempty" jsonschema:"explicit trigger reference (e.g. 'issue:42'); resolves to the most-recent run on that ref"`
}

// GetActiveRunOutput mirrors the OpenAPI Run schema. Keeping the
// fields flat (not nested under a `run` key) lets the MCP client's
// natural-language reasoning index directly on field names without
// schema-walking.
type GetActiveRunOutput struct {
	Run Run `json:"run"`
}

// registerGetActiveRun wires the fishhawk_get_active_run tool. The
// resolver order matches the issue spec: pr_number > trigger_ref >
// env-based detection. Each path queries the backend with a
// different filter; the runRow-most-recent winner returns to the
// caller. When no resolution path produces a hit, the handler
// returns a structured error explaining what input the caller
// could provide — agents reading the response have enough context
// to ask the human for the missing piece.
func registerGetActiveRun(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_get_active_run",
		Description: strings.TrimSpace(`
Resolve the Fishhawk run for the current context.

Resolution order:
  1. If pr_number is set, returns the most-recent run on that PR.
  2. Else if trigger_ref is set (e.g. "issue:42"), returns the
     most-recent run on that ref.
  3. Else if FISHHAWK_RUN_ID is set in the env (the runner case),
     fetches that run directly.
  4. Otherwise returns an error asking for pr_number or trigger_ref.

repo defaults to GITHUB_REPOSITORY env when omitted. Returns the
full Run shape with id, state, workflow_id, trigger info, and
pull-request URL.
`),
	}, resolver.getActiveRun)
}

// getActiveRun is the tool handler. Pure-ish on its inputs; only
// side effects are the HTTP call and reading env.
func (r *runResolver) getActiveRun(ctx context.Context, _ *mcp.CallToolRequest, in GetActiveRunInput) (*mcp.CallToolResult, GetActiveRunOutput, error) {
	repo := in.Repo
	if repo == "" {
		repo = r.getenv("GITHUB_REPOSITORY")
	}

	// Path 1: pr_number → query by pull_request_url. Requires
	// repo to construct the canonical URL the backend stores.
	if in.PRNumber > 0 {
		if repo == "" {
			return nil, GetActiveRunOutput{}, fmt.Errorf("repo required when pr_number is set; pass repo or set GITHUB_REPOSITORY")
		}
		prURL := fmt.Sprintf("https://github.com/%s/pull/%d", repo, in.PRNumber)
		runRow, err := r.findMostRecent(ctx, listRunsFilter{
			Repo:           repo,
			PullRequestURL: prURL,
			Limit:          5,
		})
		if err != nil {
			return nil, GetActiveRunOutput{}, fmt.Errorf("list runs by pr_number: %w", err)
		}
		if runRow == nil {
			return nil, GetActiveRunOutput{}, fmt.Errorf("no Fishhawk run found for %s pull/%d", repo, in.PRNumber)
		}
		return nil, GetActiveRunOutput{Run: *runRow}, nil
	}

	// Path 2: trigger_ref → query by trigger_ref. Repo is
	// optional but recommended (scopes the search across
	// installations that share an issue-number namespace).
	if in.TriggerRef != "" {
		runRow, err := r.findMostRecent(ctx, listRunsFilter{
			Repo:       repo,
			TriggerRef: in.TriggerRef,
			Limit:      5,
		})
		if err != nil {
			return nil, GetActiveRunOutput{}, fmt.Errorf("list runs by trigger_ref: %w", err)
		}
		if runRow == nil {
			return nil, GetActiveRunOutput{}, fmt.Errorf("no Fishhawk run found for trigger_ref=%q", in.TriggerRef)
		}
		return nil, GetActiveRunOutput{Run: *runRow}, nil
	}

	// Path 3: FISHHAWK_RUN_ID in env. The in-runner agent (E19.8
	// / future) gets this stamped at stage-start; the operator
	// can also export it manually.
	if runID := r.getenv("FISHHAWK_RUN_ID"); runID != "" {
		id, parseErr := uuid.Parse(runID)
		if parseErr != nil {
			return nil, GetActiveRunOutput{}, fmt.Errorf("FISHHAWK_RUN_ID=%q is not a valid UUID: %w", runID, parseErr)
		}
		runRow, err := r.api.GetRun(ctx, id)
		if err != nil {
			return nil, GetActiveRunOutput{}, fmt.Errorf("get run by FISHHAWK_RUN_ID: %w", err)
		}
		return nil, GetActiveRunOutput{Run: *runRow}, nil
	}

	// Path 4: nothing to resolve from. Tell the caller exactly
	// what they could pass.
	return nil, GetActiveRunOutput{}, errors.New("no active run resolvable from context; pass pr_number or trigger_ref, or set FISHHAWK_RUN_ID in the environment")
}

// retryPlanChainDepth caps the parent-walk so a corrupt
// parent_run_id cycle can't loop forever. Mirrors the constant of
// the same name in `backend/internal/server/prompt.go` — keep them
// aligned so the MCP tool returns the same plan the backend's
// prompt builder would resolve for the same run.
const retryPlanChainDepth = 8

// GetPlanInput is the get_plan tool's input schema. run_id is the
// only field — the resolver walks the parent chain itself; agents
// don't need to know about the retry-chain mechanism.
type GetPlanInput struct {
	RunID string `json:"run_id" jsonschema:"the Fishhawk run UUID; the tool walks parent_run_id internally for CI-retry chains"`
}

// PlanContent mirrors the standard_v1 plan shape. Fields the agent
// reads to reason about scope, approach, and verification expectations.
// Kept flat for jsonschema friendliness; the SDK turns the struct
// into an output schema the MCP client can introspect.
type PlanContent struct {
	PlanVersion                string             `json:"plan_version"`
	TicketReference            PlanTicketRef      `json:"ticket_reference"`
	GeneratedBy                PlanGeneratedBy    `json:"generated_by"`
	Summary                    string             `json:"summary"`
	Scope                      PlanScope          `json:"scope"`
	Approach                   []PlanApproachStep `json:"approach"`
	Verification               PlanVerification   `json:"verification"`
	RisksAndAssumptions        []string           `json:"risks_and_assumptions,omitempty"`
	PredictedRuntimeMinutes    int                `json:"predicted_runtime_minutes"`
	PredictedRuntimeConfidence string             `json:"predicted_runtime_confidence"`
	Decomposition              *PlanDecomposition `json:"decomposition,omitempty"`
}

// PlanDecomposition carries the agent's proposal to split the plan
// into parallel sub-plans (standard_v1 D2 field, ADR-025).
type PlanDecomposition struct {
	Rationale string        `json:"rationale"`
	SubPlans  []PlanSubPlan `json:"sub_plans"`
}

// PlanSubPlan describes one sub-plan within a decomposed plan.
type PlanSubPlan struct {
	Title                      string `json:"title"`
	ScopeHint                  string `json:"scope_hint"`
	PredictedRuntimeMinutes    int    `json:"predicted_runtime_minutes"`
	PredictedRuntimeConfidence string `json:"predicted_runtime_confidence"`
}

// PlanTicketRef identifies the ticket that originated the run.
type PlanTicketRef struct {
	Type string `json:"type"`
	URL  string `json:"url"`
	ID   string `json:"id"`
}

// PlanGeneratedBy identifies the agent + model + timestamp that
// produced the plan artifact.
type PlanGeneratedBy struct {
	Agent     string    `json:"agent"`
	Model     string    `json:"model"`
	Version   string    `json:"version,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// PlanScope lists the files the agent intends to touch + estimate.
type PlanScope struct {
	Files                 []PlanScopeFile `json:"files"`
	EstimatedLinesChanged int             `json:"estimated_lines_changed,omitempty"`
}

// PlanScopeFile is one path the plan covers.
type PlanScopeFile struct {
	Path      string `json:"path"`
	Operation string `json:"operation"`
}

// PlanApproachStep is one numbered step in the approach list.
type PlanApproachStep struct {
	Step        int    `json:"step"`
	Description string `json:"description"`
}

// PlanVerification carries the test_strategy and rollback_plan.
type PlanVerification struct {
	TestStrategy string `json:"test_strategy"`
	RollbackPlan string `json:"rollback_plan"`
}

// PlanReviewConcern is one flagged issue within a review verdict,
// decoded from a plan_reviewed audit entry (ADR-027).
type PlanReviewConcern struct {
	Severity string `json:"severity"`
	Category string `json:"category"`
	Note     string `json:"note"`
}

// PlanReview is one review-agent verdict decoded from a plan_reviewed
// audit entry (ADR-027). Each entry represents one agent invocation.
// ReviewerKind is always "agent" for agent reviews. Authority is one
// of gating, advisory, or gateless per the stage's reviewers config.
// Verdict is one of approve, approve_with_concerns, or reject.
type PlanReview struct {
	ReviewerKind  string              `json:"reviewer_kind"`
	ReviewerModel string              `json:"reviewer_model,omitempty"`
	Authority     string              `json:"authority"`
	Verdict       string              `json:"verdict"`
	Concerns      []PlanReviewConcern `json:"concerns,omitempty"`
	FreeForm      string              `json:"free_form,omitempty"`
	// Reason is populated only on a "skipped" verdict (#574): it
	// names why the configured agent layer was not run (e.g.
	// "reviewer_not_configured" when reviewers.agent>0 but no
	// PlanReviewer is wired on the backend).
	Reason string `json:"reason,omitempty"`
}

// GetPlanOutput is the response shape. Status is `available` or
// `no_plan_yet`; on `no_plan_yet` Plan is nil and Message explains
// why so an agent reading the response can branch on the state
// without parsing prose.
type GetPlanOutput struct {
	Status      string       `json:"status" jsonschema:"either 'available' or 'no_plan_yet'"`
	Message     string       `json:"message,omitempty" jsonschema:"human-readable explanation when status=no_plan_yet"`
	Plan        *PlanContent `json:"plan,omitempty"`
	ResolvedVia string       `json:"resolved_via,omitempty" jsonschema:"'self' when the plan came from the requested run; 'parent:<run_id>' when the parent-walk resolved it for a CI-retry chain"`
	Reviews     []PlanReview `json:"reviews,omitempty" jsonschema:"plan-review agent verdicts; populated when reviewers.agent>0 is configured on the stage (ADR-027). A verdict of 'skipped' with a reason marks an agent layer that was configured but not wired on the backend"`
}

// registerGetPlan wires the fishhawk_get_plan tool. The handler
// mirrors `backend/internal/server/prompt.go::loadApprovedPlanForRun`
// — find the plan stage on the run, fall back to parent_run_id when
// no plan stage exists (the CI-retry case per #279 / E16). Read-only
// per ADR-021; never modifies state.
func registerGetPlan(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_get_plan",
		Description: strings.TrimSpace(`
Fetch the approved plan for a Fishhawk run.

Walks parent_run_id up to 8 levels so CI-retry runs (which skip the
plan stage and re-execute against the parent's plan) resolve to the
canonical plan. Returns the parsed standard_v1 plan shape: summary,
scope.files, approach steps, verification (test_strategy +
rollback_plan), risks_and_assumptions when present,
predicted_runtime_minutes + predicted_runtime_confidence (every plan),
decomposition (when the agent proposed sub-plans), and reviews[]
(when plan-review agents were configured on the stage — each entry
has reviewer_kind, authority, verdict, concerns[], and free_form). A
verdict of "skipped" with a reason marks an agent layer that was
configured (reviewers.agent>0) but not wired on the backend.

Response status:
  - "available"     — Plan is populated; ResolvedVia tells you whether
                      it came from the requested run ("self") or a
                      parent in the retry chain ("parent:<run_id>").
                      Reviews[] is populated from plan_reviewed audit
                      entries on the resolved run (empty when no
                      review agents ran).
  - "no_plan_yet"   — The run (and its parents) have no terminal plan
                      artifact yet. Message names the chain depth
                      searched.
`),
	}, resolver.getPlan)
}

// getPlan is the tool handler.
func (r *runResolver) getPlan(ctx context.Context, _ *mcp.CallToolRequest, in GetPlanInput) (*mcp.CallToolResult, GetPlanOutput, error) {
	runID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, GetPlanOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}

	// Walk the chain. Cap at retryPlanChainDepth so a corrupt
	// parent cycle can't loop the agent.
	current := runID
	for depth := 0; depth < retryPlanChainDepth; depth++ {
		p, found, err := r.tryGetPlanForRun(ctx, current)
		if err != nil {
			return nil, GetPlanOutput{}, err
		}
		if found {
			resolvedVia := "self"
			if current != runID {
				resolvedVia = "parent:" + current.String()
			}
			reviews, err := r.loadPlanReviews(ctx, current)
			if err != nil {
				return nil, GetPlanOutput{}, fmt.Errorf("load plan reviews: %w", err)
			}
			return nil, GetPlanOutput{
				Status:      "available",
				Plan:        p,
				ResolvedVia: resolvedVia,
				Reviews:     reviews,
			}, nil
		}
		runRow, err := r.api.GetRun(ctx, current)
		if err != nil {
			return nil, GetPlanOutput{}, fmt.Errorf("get run for parent walk: %w", err)
		}
		if runRow.ParentRunID == nil || *runRow.ParentRunID == "" {
			return nil, GetPlanOutput{
				Status:  "no_plan_yet",
				Message: fmt.Sprintf("no terminal plan artifact on run %s (chain root reached at depth %d)", runID, depth),
			}, nil
		}
		// Parse the parent id back to uuid.UUID for the next GetRun
		// call. The Run shape carries IDs as strings so the MCP SDK
		// can infer a string schema; the API client signatures still
		// take uuid.UUID for the path segment.
		parent, parseErr := uuid.Parse(*runRow.ParentRunID)
		if parseErr != nil {
			return nil, GetPlanOutput{}, fmt.Errorf("parse parent_run_id %q: %w", *runRow.ParentRunID, parseErr)
		}
		current = parent
	}
	return nil, GetPlanOutput{
		Status:  "no_plan_yet",
		Message: fmt.Sprintf("no terminal plan artifact on run %s after walking %d parent levels (chain depth cap)", runID, retryPlanChainDepth),
	}, nil
}

// tryGetPlanForRun mirrors prompt.go's tryLoadPlanForRun. Returns
// (plan, true, nil) on a hit; (nil, false, nil) when the run has no
// plan stage or no usable plan artifact (caller walks to parent);
// (nil, false, err) on transport / decode failure.
func (r *runResolver) tryGetPlanForRun(ctx context.Context, runID uuid.UUID) (*PlanContent, bool, error) {
	stages, err := r.api.ListRunStages(ctx, runID)
	if err != nil {
		return nil, false, fmt.Errorf("list stages: %w", err)
	}
	var planStageIDStr string
	for _, st := range stages {
		if st.Type == "plan" {
			planStageIDStr = st.ID
			break
		}
	}
	if planStageIDStr == "" {
		return nil, false, nil
	}
	planStageID, parseErr := uuid.Parse(planStageIDStr)
	if parseErr != nil {
		return nil, false, fmt.Errorf("parse plan stage id %q: %w", planStageIDStr, parseErr)
	}
	arts, err := r.api.ListStageArtifacts(ctx, planStageID)
	if err != nil {
		return nil, false, fmt.Errorf("list plan stage artifacts: %w", err)
	}
	var picked *Artifact
	for i := range arts {
		a := &arts[i]
		if a.Kind != "plan" {
			continue
		}
		if a.SchemaVersion == nil || *a.SchemaVersion != "standard_v1" {
			continue
		}
		if picked == nil || a.CreatedAt.After(picked.CreatedAt) {
			picked = a
		}
	}
	if picked == nil {
		return nil, false, nil
	}
	if picked.Content == nil {
		// Backend invariant: ListStageArtifacts returns content
		// inline. An absent content suggests a partial response we
		// shouldn't try to parse — surface as not-yet rather than
		// a confusing JSON parse error.
		return nil, false, nil
	}
	// Content is typed `any` so the MCP SDK's output schema infers
	// an unconstrained shape (rather than RawMessage's []byte =
	// array). Re-marshal to bytes here so we can decode into the
	// typed PlanContent. The extra round-trip is cheap and lives
	// only on the plan-fetch path.
	raw, err := json.Marshal(picked.Content)
	if err != nil {
		return nil, false, fmt.Errorf("re-encode plan artifact content: %w", err)
	}
	var p PlanContent
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, false, fmt.Errorf("decode plan artifact: %w", err)
	}
	return &p, true, nil
}

// loadPlanReviews queries plan_reviewed audit entries for the given
// run and decodes each payload into a PlanReview. Entries whose
// payload is absent or malformed are silently skipped — a review
// with a corrupt payload is not a reason to fail the whole plan
// fetch. Returns nil when no plan_reviewed entries exist.
func (r *runResolver) loadPlanReviews(ctx context.Context, runID uuid.UUID) ([]PlanReview, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: "plan_reviewed",
		Limit:    50,
	})
	if err != nil {
		return nil, err
	}
	var reviews []PlanReview
	for _, e := range entries {
		if e.Payload == nil {
			continue
		}
		raw, merr := json.Marshal(e.Payload)
		if merr != nil {
			continue
		}
		var review PlanReview
		if uerr := json.Unmarshal(raw, &review); uerr != nil {
			continue
		}
		reviews = append(reviews, review)
	}

	// Second pass: plan_review_skipped entries (#574). These mark a
	// configured agent layer that was not wired on the backend. Each
	// surfaces as a synthesized PlanReview with verdict "skipped" so
	// an agent reading the response can tell a degraded gate from a
	// real verdict without a separate audit query.
	skipped, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: "plan_review_skipped",
		Limit:    50,
	})
	if err != nil {
		return nil, err
	}
	for _, e := range skipped {
		if e.Payload == nil {
			continue
		}
		raw, merr := json.Marshal(e.Payload)
		if merr != nil {
			continue
		}
		var p struct {
			Reason    string `json:"reason"`
			Authority string `json:"authority"`
		}
		if uerr := json.Unmarshal(raw, &p); uerr != nil {
			continue
		}
		reviews = append(reviews, PlanReview{
			ReviewerKind: "agent",
			Authority:    p.Authority,
			Verdict:      "skipped",
			Reason:       p.Reason,
		})
	}
	return reviews, nil
}

// auditLimitDefault is the default value for the get_run_status
// tool's audit_limit input. Five is enough for "what's happening
// right now" without overwhelming the agent's reasoning window.
const auditLimitDefault = 5

// auditLimitMax caps the audit_limit input. The backend's /v0/audit
// endpoint accepts up to 500; we cap lower because the get_run_
// status tool's job is to surface recent activity, not to paginate
// the full chain. Agents wanting more rows can use the dedicated
// fishhawk_list_audit tool (E19.6 / #346).
const auditLimitMax = 50

// GetRunStatusInput is the tool's input schema.
type GetRunStatusInput struct {
	RunID      string `json:"run_id" jsonschema:"the Fishhawk run UUID"`
	AuditLimit int    `json:"audit_limit,omitempty" jsonschema:"how many recent audit entries to include (default 5, capped at 50)"`
}

// GetRunStatusOutput bundles the three /v0 reads into one
// agent-friendly response. Stages come back sequence-ascending so
// the agent reads them in the order the pipeline executes; audit
// rows come back time-descending so "most recent" is item 0.
type GetRunStatusOutput struct {
	Run         Run          `json:"run"`
	Stages      []Stage      `json:"stages" jsonschema:"ordered by sequence ascending"`
	RecentAudit []AuditEntry `json:"recent_audit" jsonschema:"time-descending; item 0 is the most recent"`
	// ImplementReviews surfaces implement-review agent verdicts (ADR-027
	// impl 2/2) so the human reviewer sees the diff-review outcome before
	// approving the implement stage. Each entry has reviewer_kind,
	// authority, verdict, concerns[] (a {category:"scope"} concern flags
	// scope.files drift), and free_form. A verdict of "skipped" with a
	// reason marks a configured agent layer that was not wired.
	ImplementReviews []PlanReview `json:"implement_reviews,omitempty" jsonschema:"implement-review agent verdicts; populated when reviewers.agent>0 is configured on the implement stage (ADR-027). A {category:'scope'} concern flags scope.files drift (flag-only, never an auto-reject). A verdict of 'skipped' with a reason marks an agent layer that was configured but not wired on the backend"`
}

// registerGetRunStatus wires the fishhawk_get_run_status tool. The
// handler aggregates three backend calls (GetRun + ListRunStages +
// ListRecentRunAudit) into one MCP tool call so the agent saves
// the round-trip-back latency that a sequential chain of
// individual tool calls would impose. Read-only per ADR-021.
func registerGetRunStatus(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_get_run_status",
		Description: strings.TrimSpace(`
Snapshot a Fishhawk run's current state in one call.

Returns the Run row (state, workflow, trigger, PR URL when stamped),
the full ordered stage list (each stage's id / type / state /
executor / timing / failure category if any), and the N most-recent
audit entries time-descending (default 5; capped at 50).

Also returns implement_reviews[]: implement-review agent verdicts
(ADR-027) when reviewers.agent>0 is configured on the implement stage.
Each entry carries reviewer_kind, authority, verdict, concerns[], and
free_form; a {category:"scope"} concern flags scope.files drift
(flag-only, never an auto-reject). Read these before approving the
implement stage. A verdict of "skipped" with a reason marks an agent
layer that was configured but not wired on the backend.

Use this as the agent's "where are we" query — replaces a sequential
chain of GetRun / ListStages / ListAudit calls with a single
round-trip. For deeper audit pagination, use fishhawk_list_audit.
`),
	}, resolver.getRunStatus)
}

// getRunStatus is the tool handler.
func (r *runResolver) getRunStatus(ctx context.Context, _ *mcp.CallToolRequest, in GetRunStatusInput) (*mcp.CallToolResult, GetRunStatusOutput, error) {
	runID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, GetRunStatusOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}

	limit := clampAuditLimit(in.AuditLimit)

	runRow, err := r.api.GetRun(ctx, runID)
	if err != nil {
		return nil, GetRunStatusOutput{}, fmt.Errorf("get run: %w", err)
	}

	stages, err := r.api.ListRunStages(ctx, runID)
	if err != nil {
		return nil, GetRunStatusOutput{}, fmt.Errorf("list stages: %w", err)
	}
	// Defensive sort. The backend returns sequence-ascending, but
	// re-sorting locally insulates the agent from any future
	// ordering change and costs nothing on a list of < 10 stages.
	sort.SliceStable(stages, func(i, j int) bool {
		return stages[i].Sequence < stages[j].Sequence
	})

	recent, err := r.api.ListRecentRunAudit(ctx, runID, limit)
	if err != nil {
		return nil, GetRunStatusOutput{}, fmt.Errorf("list recent audit: %w", err)
	}

	implementReviews, err := r.loadImplementReviews(ctx, runID)
	if err != nil {
		return nil, GetRunStatusOutput{}, fmt.Errorf("load implement reviews: %w", err)
	}

	return nil, GetRunStatusOutput{
		Run:              *runRow,
		Stages:           stages,
		RecentAudit:      recent,
		ImplementReviews: implementReviews,
	}, nil
}

// loadImplementReviews queries implement_reviewed audit entries for the
// given run and decodes each into a PlanReview (the verdict shape is
// identical across plan and implement review, ADR-027). It mirrors
// loadPlanReviews: corrupt payloads are skipped, and a second pass over
// implement_review_skipped entries synthesizes a "skipped" verdict so a
// degraded gate is distinguishable from a real verdict. Returns nil when
// no implement-review entries exist.
func (r *runResolver) loadImplementReviews(ctx context.Context, runID uuid.UUID) ([]PlanReview, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: "implement_reviewed",
		Limit:    50,
	})
	if err != nil {
		return nil, err
	}
	var reviews []PlanReview
	for _, e := range entries {
		if e.Payload == nil {
			continue
		}
		raw, merr := json.Marshal(e.Payload)
		if merr != nil {
			continue
		}
		var review PlanReview
		if uerr := json.Unmarshal(raw, &review); uerr != nil {
			continue
		}
		reviews = append(reviews, review)
	}

	skipped, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: "implement_review_skipped",
		Limit:    50,
	})
	if err != nil {
		return nil, err
	}
	for _, e := range skipped {
		if e.Payload == nil {
			continue
		}
		raw, merr := json.Marshal(e.Payload)
		if merr != nil {
			continue
		}
		var p struct {
			Reason    string `json:"reason"`
			Authority string `json:"authority"`
		}
		if uerr := json.Unmarshal(raw, &p); uerr != nil {
			continue
		}
		reviews = append(reviews, PlanReview{
			ReviewerKind: "agent",
			Authority:    p.Authority,
			Verdict:      "skipped",
			Reason:       p.Reason,
		})
	}
	return reviews, nil
}

// clampAuditLimit applies the default + cap. Negative or zero
// falls back to auditLimitDefault; values over auditLimitMax clamp
// to the cap. The backend would reject too-large values with a
// 400; we clamp client-side so the agent's bad input doesn't
// surface as a confusing API error.
func clampAuditLimit(n int) int {
	if n <= 0 {
		return auditLimitDefault
	}
	if n > auditLimitMax {
		return auditLimitMax
	}
	return n
}

// listAuditLimitDefault / listAuditLimitMax cap the
// fishhawk_list_audit tool's limit input. The backend's
// /v0/runs/{id}/audit endpoint accepts up to 500; the MCP tool
// caps lower (200) because the agent's reasoning window has a
// practical limit on how many entries it can process per call.
// Pagination via cursor covers the > 200 case.
const (
	listAuditLimitDefault = 50
	listAuditLimitMax     = 200
)

// ListAuditInput is the tool's input schema. category / stage_id
// are optional filters; limit / cursor drive pagination through
// the per-run audit endpoint.
type ListAuditInput struct {
	RunID    string `json:"run_id" jsonschema:"the Fishhawk run UUID"`
	Category string `json:"category,omitempty" jsonschema:"single category filter (e.g. 'approval_submitted', 'plan_generated', 'ci_failure_retry_dispatched')"`
	StageID  string `json:"stage_id,omitempty" jsonschema:"scope entries to a specific stage UUID"`
	Limit    int    `json:"limit,omitempty" jsonschema:"max items per page (default 50, capped at 200)"`
	Cursor   string `json:"cursor,omitempty" jsonschema:"pagination cursor returned by a prior list call as next_cursor"`
}

// ListAuditOutput mirrors the OpenAPI paginated list envelope.
// NextCursor is the opaque token the agent feeds back into the
// next call to walk past the current page; empty when the page
// reached the end of the chain.
type ListAuditOutput struct {
	Items      []AuditEntry `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty" jsonschema:"opaque pagination cursor; empty when no more pages remain"`
}

// registerListAudit wires the fishhawk_list_audit tool. Forwards
// filters verbatim to GET /v0/runs/{id}/audit — the same endpoint
// the CLI's `fishhawk audit list` uses (E18.4 / #335). Read-only
// per ADR-021.
func registerListAudit(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_list_audit",
		Description: strings.TrimSpace(`
List audit entries for a Fishhawk run with optional filters.

Returns rows sequence-ascending (matches the per-run scope the
backend exposes for the run-detail UI + verifier path). For "most-
recent N" queries, use fishhawk_get_run_status which uses the
cross-chain endpoint for time-descending order.

Inputs:
  - run_id    (required) — Fishhawk run UUID.
  - category  — single category filter (e.g. 'approval_submitted').
  - stage_id  — scope to a stage's entries.
  - limit     — default 50, capped at 200. For deeper paging use
                the returned next_cursor.
  - cursor    — opaque pagination token from a prior call.

Response: items[] (AuditEntry shape) + next_cursor (empty when the
chain is exhausted).
`),
	}, resolver.listAudit)
}

// listAudit is the tool handler.
func (r *runResolver) listAudit(ctx context.Context, _ *mcp.CallToolRequest, in ListAuditInput) (*mcp.CallToolResult, ListAuditOutput, error) {
	runID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, ListAuditOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	// Validate stage_id locally before the API round-trip so a
	// malformed input surfaces as a clean tool error rather than
	// a generic backend 400. Mirrors the CLI's E18.4 posture.
	if in.StageID != "" {
		if _, err := uuid.Parse(in.StageID); err != nil {
			return nil, ListAuditOutput{}, fmt.Errorf("stage_id %q is not a valid UUID: %w", in.StageID, err)
		}
	}
	limit := clampListAuditLimit(in.Limit)
	items, nextCursor, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: in.Category,
		StageID:  in.StageID,
		Limit:    limit,
		Cursor:   in.Cursor,
	})
	if err != nil {
		return nil, ListAuditOutput{}, fmt.Errorf("list audit: %w", err)
	}
	return nil, ListAuditOutput{Items: items, NextCursor: nextCursor}, nil
}

// clampListAuditLimit applies the default + cap. Centralized so
// the test surface can exercise the clamp directly without driving
// the full tool flow.
func clampListAuditLimit(n int) int {
	if n <= 0 {
		return listAuditLimitDefault
	}
	if n > listAuditLimitMax {
		return listAuditLimitMax
	}
	return n
}

// findMostRecent returns the most-recent run from a filter-scoped
// list query, or nil when the page is empty. The backend's
// /v0/runs list orders descending by created_at (per #213); we
// re-sort defensively in case the ordering ever changes.
func (r *runResolver) findMostRecent(ctx context.Context, f listRunsFilter) (*Run, error) {
	page, err := r.api.ListRuns(ctx, f)
	if err != nil {
		return nil, err
	}
	if len(page.Items) == 0 {
		return nil, nil
	}
	items := page.Items
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
	return &items[0], nil
}

// validStartRunTriggerSources mirrors `server/runs.go::validTriggerSources`.
// Kept narrow because the MCP tool sets a sensible default; agents
// passing a bad value get a clean tool error before the HTTP call.
var validStartRunTriggerSources = map[string]struct{}{
	"github_issue": {},
	"cli":          {},
	"ui":           {},
}

// StartRunInput is the fishhawk_start_run tool's input schema
// (E22.1 / #390, extended in #426). Mirrors `POST /v0/runs`'s body
// shape so an agent running inside Claude Code can mint a run
// without dropping to the CLI. trigger_source defaults to "cli"
// when omitted because that's the dominant case for an operator-
// driven MCP call.
//
// Three of the fields below — workflow_spec, issue_context, and
// the convenience wrappers (issue, working_dir) — exist for the
// local-runner flow (E22.7-E22.9 / #406, #411, #415). Without
// them, an MCP-minted run is stage-less and prompt-degraded, which
// is useless for the local loop.
type StartRunInput struct {
	Repo           string `json:"repo" jsonschema:"GitHub repo as owner/name; the workflow spec must live at .fishhawk/workflows.yaml in this repo"`
	WorkflowID     string `json:"workflow_id" jsonschema:"workflow key in .fishhawk/workflows.yaml (e.g. 'feature_change')"`
	WorkflowSHA    string `json:"workflow_sha,omitempty" jsonschema:"blob SHA of the spec file; auto-computed from the discovered spec when omitted and working_dir resolves a checkout"`
	TriggerSource  string `json:"trigger_source,omitempty" jsonschema:"one of 'cli', 'github_issue', 'ui'; defaults to 'cli' when omitted, auto-flips to 'github_issue' when issue or issue_context is set"`
	TriggerRef     string `json:"trigger_ref,omitempty" jsonschema:"optional reference (e.g. 'issue:42') threading the run to its trigger; auto-derived from issue when omitted"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"E8.2 idempotency token; a second call with the same (repo, key) returns the existing run with Idempotent=true instead of fresh-creating"`

	// RunnerKind tags the execution backend (ADR-022 / #388).
	// Empty defaults to github_actions at the backend; the local-
	// runner flow passes "local" so the dispatcher skips the
	// workflow_dispatch hop and waits for the operator's
	// `fishhawk runner start` to drive the stage.
	RunnerKind string `json:"runner_kind,omitempty" jsonschema:"execution backend tag: 'github_actions' (default) or 'local'"`

	// WorkflowSpec is the inline YAML of the workflow file (#411).
	// When non-empty the backend creates one Stage row per stage
	// definition; without it, an API-minted run sits stage-less
	// and can't progress on the local-runner path. Most callers
	// leave this empty and pass WorkingDir instead — the MCP
	// server auto-discovers the file and fills this for them.
	WorkflowSpec string `json:"workflow_spec,omitempty" jsonschema:"inline YAML body of .fishhawk/workflows.yaml; auto-discovered from working_dir when empty"`

	// WorkingDir is the directory the MCP server walks (up to the
	// .git boundary) looking for `.fishhawk/workflows.yaml`. The
	// agent passes the checkout it's working in; the resolved
	// spec's bytes + computed SHA ride along on the create call.
	// Skipped when WorkflowSpec is already set or when the agent
	// passes WorkflowSHA explicitly (legacy "no checkout" path).
	WorkingDir string `json:"working_dir,omitempty" jsonschema:"checkout directory to search for .fishhawk/workflows.yaml; auto-discovery only runs when set"`

	// SpecFile overrides the walk-up auto-discovery with an
	// explicit path. Used when the spec lives outside the
	// canonical location (rare; mostly for test scenarios).
	SpecFile string `json:"spec_file,omitempty" jsonschema:"explicit workflow spec path; overrides working_dir auto-discovery"`

	// IssueContext is the cached GitHub issue payload (#415).
	// Only valid with trigger_source=github_issue (or auto-flip
	// from Issue). Agents that want the MCP server to fetch this
	// themselves pass Issue instead.
	IssueContext *IssueContext `json:"issue_context,omitempty" jsonschema:"pre-fetched issue payload; valid only with trigger_source=github_issue. Most callers pass issue instead and let the MCP server fetch via gh."`

	// Issue is a convenience alternative to IssueContext: the MCP
	// server shells to `gh issue view` and fills the
	// IssueContext from the result. Accepts the same forms as
	// the CLI's --issue (a bare number, #N, or full URL).
	Issue string `json:"issue,omitempty" jsonschema:"GitHub issue number, #N, or .../issues/N URL; the MCP server fetches via gh and ships inline"`
}

// StartRunOutput is the response shape. Run is the canonical Run
// row. Idempotent is true when the backend returned 200 against an
// existing run for the same (repo, idempotency_key) — clients that
// react to "fresh run" (e.g. notify a Slack channel) can branch on
// the flag.
type StartRunOutput struct {
	Run        Run  `json:"run"`
	Idempotent bool `json:"idempotent" jsonschema:"true when this call replayed against an existing run via Idempotency-Key; false on fresh create"`
}

// registerStartRun wires the fishhawk_start_run tool (E22.1 / #390;
// field-parity extension #426). Mirrors the CLI's `fishhawk run start`.
//
// Two operator-side flows the tool supports:
//
//  1. **Stage-less seed** (legacy / integration tests). Agent passes
//     repo + workflow_id + workflow_sha; backend creates a row, no
//     stages. Useful only for tests; can't drive a real run.
//  2. **Full local-runner mint** (#411 + #415 + ADR-022). Agent
//     passes working_dir (or workflow_spec inline) + optionally
//     issue + runner_kind=local. The MCP server walks for
//     `.fishhawk/workflows.yaml`, shells to `gh issue view`, and
//     ships everything inline so the backend creates Stage rows
//     and primes the prompt cache.
//
// Auth: this is a write tool. Operator-side fhk_* tokens with
// scope `write:runs` will succeed; runner-side fhm_* tokens (per
// the bearer middleware's prefix routing) will surface a 403 as a
// tool error.
func registerStartRun(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_start_run",
		Description: strings.TrimSpace(`
Create a new Fishhawk run.

Mirrors the CLI's "fishhawk run start" verb. The new run is created
in pending state and dispatched immediately via the existing
workflow_dispatch path or, for runner_kind=local, waits for the
operator's "fishhawk runner start" to drive each stage.

For the local-runner flow, pass working_dir (the checkout the agent
is in) — the MCP server walks for .fishhawk/workflows.yaml and
ships the bytes inline so the backend creates stages. Pass issue
(a number, #N, or full URL) and the server shells to gh and ships
the title/body inline so the prompt builder uses the cached payload.

Idempotency: when idempotency_key is set, a previously-created run
with the same (repo, key) returns 200 with Idempotent=true instead
of fresh-creating a duplicate. Re-running this call after a network
hiccup is safe.

Returns the canonical Run row + an Idempotent flag.
`),
	}, resolver.startRun)
}

// startRun is the tool handler.
//
// Composition order (mirrors cli/cmd/fishhawk/run.go::runStart):
//  1. validate the obvious inputs (repo, workflow_id).
//  2. resolve issue number (explicit or trigger_ref-derived).
//  3. walk for the workflow spec (when working_dir/spec_file are
//     set and workflow_spec wasn't supplied directly).
//  4. compose the effective workflow_sha (explicit > computed).
//  5. compose trigger_source (auto-flip to github_issue when an
//     issue resolves).
//  6. fetch the issue via gh (when an issue resolved and IssueContext
//     wasn't already passed inline).
//  7. validate the trigger_source / issue_context pairing.
//  8. hand off to apiClient.StartRun.
func (r *runResolver) startRun(ctx context.Context, _ *mcp.CallToolRequest, in StartRunInput) (*mcp.CallToolResult, StartRunOutput, error) {
	if in.Repo == "" {
		return nil, StartRunOutput{}, errors.New("repo is required (owner/name)")
	}
	if in.WorkflowID == "" {
		return nil, StartRunOutput{}, errors.New("workflow_id is required")
	}

	// (2) Parse the explicit issue argument up front so a typo
	// surfaces before any disk walk or backend round-trip.
	issueNumber, err := resolveIssueRef(in.Issue)
	if err != nil {
		return nil, StartRunOutput{}, err
	}
	if issueNumber == 0 {
		issueNumber = inferIssueNumberFromTriggerRef(in.TriggerRef)
	}

	// (3) Resolve the workflow spec. Skipped entirely when the
	// caller supplied workflow_spec inline (the "I already know
	// what I want to ship" path). Skipped when neither working_dir
	// nor spec_file was set (the legacy stage-less seed path —
	// only useful for tests).
	specBytes := []byte(in.WorkflowSpec)
	computedSHA := ""
	if len(specBytes) == 0 && (in.WorkingDir != "" || in.SpecFile != "") {
		startDir := in.WorkingDir
		if startDir == "" {
			startDir = "."
		}
		found, derr := discoverSpec(startDir, in.SpecFile)
		if derr != nil {
			return nil, StartRunOutput{}, fmt.Errorf("spec discovery: %w", derr)
		}
		if found != nil {
			specBytes = found.Contents
			computedSHA = found.BlobSHA
			// Pre-parse so a YAML/schema typo is a fast local
			// failure instead of a backend round-trip.
			if perr := specValidate(specBytes); perr != nil {
				return nil, StartRunOutput{}, fmt.Errorf("%s: %w", found.Path, perr)
			}
		}
	} else if len(specBytes) > 0 {
		// Caller supplied workflow_spec inline. Compute the SHA so
		// the backend's content-hash gate still has a value to bind
		// against, and validate so a bad inline body fails locally.
		computedSHA = gitBlobSHA(specBytes)
		if perr := specValidate(specBytes); perr != nil {
			return nil, StartRunOutput{}, fmt.Errorf("workflow_spec: %w", perr)
		}
	}

	// (4) Compose the effective SHA. Explicit input wins (an
	// override hook for minting historic runs); otherwise the
	// discovered file's blob SHA travels with the bytes.
	effectiveSHA := in.WorkflowSHA
	if effectiveSHA == "" {
		effectiveSHA = computedSHA
	}
	if effectiveSHA == "" {
		return nil, StartRunOutput{}, errors.New(
			"workflow_sha is required (or pass working_dir / workflow_spec so the MCP server can compute it)")
	}

	// (5) Compose trigger_source. When the caller omitted the
	// field, default to cli unless an issue resolves — then flip
	// to github_issue (mirrors the CLI's behavior). An explicit
	// trigger_source is left alone; if it conflicts with the
	// issue payload, step (7) catches it.
	triggerSource := in.TriggerSource
	if triggerSource == "" {
		triggerSource = "cli"
		if issueNumber > 0 || in.IssueContext != nil {
			triggerSource = "github_issue"
		}
	}
	if _, ok := validStartRunTriggerSources[triggerSource]; !ok {
		return nil, StartRunOutput{}, fmt.Errorf("trigger_source %q is not one of cli, github_issue, ui", triggerSource)
	}

	// Normalize trigger_ref to the canonical issue:N form when
	// the operator only passed issue, so threading + audit
	// surfaces (#216) keep working.
	triggerRef := in.TriggerRef
	if issueNumber > 0 && triggerRef == "" {
		triggerRef = fmt.Sprintf("issue:%d", issueNumber)
	}

	// (6) Fetch the issue locally via gh and bundle the payload.
	// Best-effort: a missing or unauthed gh emits a warning on the
	// tool result and the run proceeds without the cache
	// (degraded prompt = pre-#415 shape). When the caller already
	// supplied IssueContext inline, skip the fetch and use what
	// they sent.
	issueContext := in.IssueContext
	var warnings []string
	if issueNumber > 0 && issueContext == nil {
		ic, ferr := fetchIssueViaGh(in.Repo, issueNumber)
		switch {
		case ferr == nil:
			issueContext = ic
		case errors.Is(ferr, ErrGhNotInstalled):
			warnings = append(warnings,
				"gh CLI not on PATH; proceeding without inline issue context. Install https://cli.github.com for the full prompt.")
		default:
			warnings = append(warnings,
				fmt.Sprintf("issue fetch warning: %v — proceeding without inline issue context", ferr))
		}
	}

	// (7) Validate the trigger_source / issue_context pairing.
	// Mirrors the backend handler's check — better to fail here
	// with a clear tool error than round-trip to a 422.
	if issueContext != nil && triggerSource != "github_issue" {
		return nil, StartRunOutput{}, fmt.Errorf(
			"issue_context is only valid with trigger_source=github_issue (got %q)", triggerSource)
	}

	// (8) Hand off to the backend.
	created, idempotent, err := r.api.StartRun(ctx, StartRunParams{
		Repo:           in.Repo,
		WorkflowID:     in.WorkflowID,
		WorkflowSHA:    effectiveSHA,
		TriggerSource:  triggerSource,
		TriggerRef:     triggerRef,
		IdempotencyKey: in.IdempotencyKey,
		RunnerKind:     in.RunnerKind,
		WorkflowSpec:   string(specBytes),
		IssueContext:   issueContext,
	})
	if err != nil {
		return nil, StartRunOutput{}, fmt.Errorf("start run: %w", err)
	}

	var meta *mcp.CallToolResult
	if len(warnings) > 0 {
		meta = &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: strings.Join(warnings, "\n")}},
		}
	}
	return meta, StartRunOutput{Run: *created, Idempotent: idempotent}, nil
}

// CancelRunInput is the fishhawk_cancel_run tool's input schema
// (E22.2 / #391). Mirrors `POST /v0/runs/{run_id}/cancel`.
type CancelRunInput struct {
	RunID string `json:"run_id" jsonschema:"the Fishhawk run UUID to cancel"`
}

// CancelRunOutput surfaces the post-cancel Run row. State should be
// `cancelled` on success; the rest of the row is unchanged.
type CancelRunOutput struct {
	Run Run `json:"run"`
}

// registerCancelRun wires the fishhawk_cancel_run tool (E22.2 /
// #391). Idempotent: cancelling an already-cancelled run succeeds.
// Cancelling a terminally-succeeded / failed run surfaces a clean
// `invalid_state_transition` tool error from the backend.
//
// Auth: write tool. Operator-side fhk_* tokens with `write:runs`
// scope succeed; runner-side fhm_* tokens surface 403 as a tool
// error.
func registerCancelRun(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_cancel_run",
		Description: strings.TrimSpace(`
Cancel a Fishhawk run.

Mirrors the CLI's "fishhawk run cancel" verb. Transitions the run to
the cancelled state via the existing state-machine rules.

Idempotent on re-cancel (200 with the cancelled run). Returns a
clean tool error on:
  - invalid UUID (caught before the HTTP hop)
  - run_not_found (404)
  - invalid_state_transition (409 — the run is already terminal in
    a non-cancelled state like succeeded / failed)
`),
	}, resolver.cancelRun)
}

// cancelRun is the tool handler.
func (r *runResolver) cancelRun(ctx context.Context, _ *mcp.CallToolRequest, in CancelRunInput) (*mcp.CallToolResult, CancelRunOutput, error) {
	runID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, CancelRunOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	cancelled, err := r.api.CancelRun(ctx, runID)
	if err != nil {
		return nil, CancelRunOutput{}, fmt.Errorf("cancel run: %w", err)
	}
	return nil, CancelRunOutput{Run: *cancelled}, nil
}

// RetryStageInput is the fishhawk_retry_stage tool's input schema
// (E22.3 / #392). Mirrors `POST /v0/stages/{stage_id}/retry`.
type RetryStageInput struct {
	StageID string `json:"stage_id" jsonschema:"the Fishhawk stage UUID to retry"`
}

// RetryStageOutput surfaces the post-retry Stage row. Category-A/C
// retries land in `pending` (orchestrator advances to dispatched
// before the response returns); category-D SLA-timeout retries land
// in `awaiting_approval`. Category-B / gate-rejected don't reach
// this output — they surface as a tool error from the backend's
// 422.
type RetryStageOutput struct {
	Stage Stage `json:"stage"`
}

// registerRetryStage wires the fishhawk_retry_stage tool (E22.3 /
// #392). Mirrors the CLI's `fishhawk run retry <stage-id>`.
//
// Per-category retry semantics live in `server/retry.go` /
// `run.RetryStage`. The MCP tool is a thin wrapper; failures of
// the form "this category isn't retryable" surface as the
// backend's `retry_not_applicable` 422 propagated as a tool error.
//
// Auth: write tool. Operator-side fhk_* tokens with `write:stages`
// scope succeed; runner-side fhm_* tokens surface 403.
func registerRetryStage(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_retry_stage",
		Description: strings.TrimSpace(`
Retry a failed Fishhawk stage.

Mirrors the CLI's "fishhawk run retry <stage-id>" verb. The backend's
state machine decides whether the stage is retryable per its failure
category:

  - A (agent failure)  : retried — flips failed → pending →
                         dispatched (orchestrator fires fresh
                         workflow_dispatch).
  - B (constraint)     : NOT retryable — the workflow or spec
                         needs to change first. Surfaces as a
                         retry_not_applicable tool error.
  - C (infrastructure) : retried — same flow as A.
  - D (gate-related)   : depends — SLA timeout retries (flip back
                         to awaiting_approval), gate-rejected does
                         not (file a fresh run instead).

Returns the updated Stage row on retry. Returns a tool error on:
  - invalid UUID (caught before the HTTP hop)
  - stage_not_found (404)
  - retry_not_applicable (422)
`),
	}, resolver.retryStage)
}

// retryStage is the tool handler.
func (r *runResolver) retryStage(ctx context.Context, _ *mcp.CallToolRequest, in RetryStageInput) (*mcp.CallToolResult, RetryStageOutput, error) {
	stageID, err := uuid.Parse(in.StageID)
	if err != nil {
		return nil, RetryStageOutput{}, fmt.Errorf("stage_id %q is not a valid UUID: %w", in.StageID, err)
	}
	retried, err := r.api.RetryStage(ctx, stageID)
	if err != nil {
		return nil, RetryStageOutput{}, fmt.Errorf("retry stage: %w", err)
	}
	return nil, RetryStageOutput{Stage: *retried}, nil
}

// ApprovePlanInput is the fishhawk_approve_plan tool's input
// schema (E22.4 / #393). Mirrors the CLI's `fishhawk plan approve
// <run-id> [--reason …]` — takes a run id, the resolver finds the
// plan stage internally.
type ApprovePlanInput struct {
	RunID  string `json:"run_id" jsonschema:"the Fishhawk run UUID whose plan stage is being approved"`
	Reason string `json:"reason,omitempty" jsonschema:"optional reviewer rationale, recorded on the approval row as 'comment'"`
}

// ApprovePlanOutput surfaces the post-approve Stage row plus the
// resolved plan-stage id (the caller passed a run id, not a stage
// id, so the response makes the resolution visible for audit
// clarity).
type ApprovePlanOutput struct {
	Stage   Stage  `json:"stage"`
	StageID string `json:"stage_id" jsonschema:"the resolved plan-stage UUID the approval was posted to"`
}

// RejectPlanInput mirrors `fishhawk plan reject <run-id> [--reason
// …]`. Reason is recommended; the CLI emits a warning when missing
// because reject without a rationale is poor practice.
type RejectPlanInput struct {
	RunID  string `json:"run_id" jsonschema:"the Fishhawk run UUID whose plan stage is being rejected"`
	Reason string `json:"reason,omitempty" jsonschema:"reviewer rationale; recommended on rejects (the CLI warns when missing)"`
}

// RejectPlanOutput mirrors ApprovePlanOutput.
type RejectPlanOutput struct {
	Stage   Stage  `json:"stage"`
	StageID string `json:"stage_id" jsonschema:"the resolved plan-stage UUID the rejection was posted to"`
}

// registerApprovePlan wires the fishhawk_approve_plan tool (E22.4
// / #393). Resolves the plan stage from the run id, then posts an
// approve decision via the existing /v0/stages/{id}/approvals
// endpoint. Idempotent at the backend's existing approval-
// idempotency layer (same authenticated subject re-submitting
// returns the existing row).
//
// Auth: write tool. Operator-side fhk_* tokens with `write:
// approvals` scope succeed; runner-side fhm_* tokens surface 403
// (their `mcp:read` scope can't authorize an approval).
func registerApprovePlan(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_approve_plan",
		Description: strings.TrimSpace(`
Approve a Fishhawk plan.

Mirrors the CLI's "fishhawk plan approve <run-id> [--reason …]" verb.
Takes a run id; the tool resolves the plan stage internally by
listing the run's stages and finding the one with type=plan.

Returns the updated Stage row (typically State=succeeded after
approve) and the resolved plan-stage UUID so the response makes
the resolution visible.

Common error shapes (surfaced as tool errors):
  - "no plan stage" — the run has no plan stage (a routine_change
    workflow, or a malformed run)
  - "plan stage not awaiting approval" — the stage is already
    terminal or in a non-approvable state
  - role-based 403 from the backend when the caller's subject
    isn't in the gate's approver list
`),
	}, resolver.approvePlan)
}

// registerRejectPlan wires the fishhawk_reject_plan tool.
func registerRejectPlan(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_reject_plan",
		Description: strings.TrimSpace(`
Reject a Fishhawk plan.

Mirrors the CLI's "fishhawk plan reject <run-id> [--reason …]" verb.
Takes a run id; the tool resolves the plan stage internally.

Rejection fails the plan stage as category D (gate didn't pass).
The reason is stored on the approval row as 'comment' and surfaces
in the audit log + the plan-on-issue comment's status footer
(#377). Reason is optional but recommended — the CLI warns when
missing.

Same resolver + error shapes as fishhawk_approve_plan.
`),
	}, resolver.rejectPlan)
}

// approvePlan is the tool handler.
func (r *runResolver) approvePlan(ctx context.Context, _ *mcp.CallToolRequest, in ApprovePlanInput) (*mcp.CallToolResult, ApprovePlanOutput, error) {
	planStage, err := r.resolvePlanStage(ctx, in.RunID)
	if err != nil {
		return nil, ApprovePlanOutput{}, err
	}
	stageID, err := uuid.Parse(planStage.ID)
	if err != nil {
		return nil, ApprovePlanOutput{}, fmt.Errorf("resolved plan stage has invalid id %q: %w", planStage.ID, err)
	}
	updated, err := r.api.SubmitApproval(ctx, stageID, "approve", in.Reason)
	if err != nil {
		return nil, ApprovePlanOutput{}, fmt.Errorf("submit approval: %w", err)
	}
	return nil, ApprovePlanOutput{Stage: *updated, StageID: updated.ID}, nil
}

// rejectPlan is the tool handler.
func (r *runResolver) rejectPlan(ctx context.Context, _ *mcp.CallToolRequest, in RejectPlanInput) (*mcp.CallToolResult, RejectPlanOutput, error) {
	planStage, err := r.resolvePlanStage(ctx, in.RunID)
	if err != nil {
		return nil, RejectPlanOutput{}, err
	}
	stageID, err := uuid.Parse(planStage.ID)
	if err != nil {
		return nil, RejectPlanOutput{}, fmt.Errorf("resolved plan stage has invalid id %q: %w", planStage.ID, err)
	}
	updated, err := r.api.SubmitApproval(ctx, stageID, "reject", in.Reason)
	if err != nil {
		return nil, RejectPlanOutput{}, fmt.Errorf("submit approval: %w", err)
	}
	return nil, RejectPlanOutput{Stage: *updated, StageID: updated.ID}, nil
}

// resolvePlanStage walks the run's stages and returns the one with
// type=plan. Shared by fishhawk_approve_plan and fishhawk_reject_
// plan because both surface the same input shape (run id, not
// stage id) — pushing stage-id-from-run-id discovery server-side
// keeps the agent's reasoning simple.
//
// Returns a typed error for the missing-plan-stage case so the
// tool error message is operator-readable rather than a generic
// "not found." Local UUID parse on the input is a fast-path that
// catches obvious typos before the HTTP hop.
func (r *runResolver) resolvePlanStage(ctx context.Context, runIDStr string) (*Stage, error) {
	runID, err := uuid.Parse(runIDStr)
	if err != nil {
		return nil, fmt.Errorf("run_id %q is not a valid UUID: %w", runIDStr, err)
	}
	stages, err := r.api.ListRunStages(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list run stages: %w", err)
	}
	for i := range stages {
		if stages[i].Type == "plan" {
			return &stages[i], nil
		}
	}
	return nil, fmt.Errorf("no plan stage on run %s; this run's workflow may not have a plan stage (e.g. routine_change)", runIDStr)
}

// listRunsLimitDefault / listRunsLimitMax bound the
// fishhawk_list_runs tool's limit input. Matches the backend's
// own defaults (runsDefaultLimit=50, runsMaxLimit=200 per
// server/runs.go) — clamping client-side means a bad input
// surfaces as a clean tool error rather than a backend 400.
const (
	listRunsLimitDefault = 50
	listRunsLimitMax     = 200
)

// validRunStates mirrors `server/runs.go::validRunStates`. The
// MCP tool catches bad values before the HTTP hop so the agent
// gets a typed error instead of a generic 400.
var validRunStates = map[string]struct{}{
	"pending":   {},
	"running":   {},
	"succeeded": {},
	"failed":    {},
	"cancelled": {},
}

// ListRunsInput is the fishhawk_list_runs tool's input schema
// (E22.5 / #394). Mirrors the CLI's `fishhawk run list`.
type ListRunsInput struct {
	Repo       string `json:"repo,omitempty" jsonschema:"filter by GitHub repo as owner/name"`
	WorkflowID string `json:"workflow_id,omitempty" jsonschema:"filter by workflow key (e.g. 'feature_change')"`
	State      string `json:"state,omitempty" jsonschema:"filter by run state; one of pending, running, succeeded, failed, cancelled"`
	Limit      int    `json:"limit,omitempty" jsonschema:"max items per page (default 50, capped at 200)"`
	Cursor     string `json:"cursor,omitempty" jsonschema:"pagination cursor returned by a prior list call as next_cursor"`
}

// ListRunsOutput mirrors the OpenAPI paginated list envelope.
// NextCursor is empty when the page reached the end of the result
// set.
type ListRunsOutput struct {
	Items      []Run  `json:"items"`
	NextCursor string `json:"next_cursor,omitempty" jsonschema:"opaque pagination cursor; empty when no more pages remain"`
}

// registerListRuns wires the fishhawk_list_runs tool (E22.5 /
// #394). Mirrors the CLI's `fishhawk run list` — the operator's
// "what runs do I have" enumeration with optional filters.
//
// Read tool: works with both runner-side fhm_* tokens (the agent
// can list runs to give the operator context) and operator-side
// fhk_* tokens.
func registerListRuns(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_list_runs",
		Description: strings.TrimSpace(`
List Fishhawk runs with optional filters.

Mirrors the CLI's "fishhawk run list" verb. Returns runs ordered by
created_at descending; pagination via opaque cursor (feed back into
a subsequent call as cursor to walk past the first page).

Inputs:
  - repo        — filter by GitHub repo (owner/name)
  - workflow_id — filter by workflow key (e.g. 'feature_change')
  - state       — filter by run state; one of pending, running,
                  succeeded, failed, cancelled
  - limit       — default 50, capped at 200
  - cursor      — opaque pagination token from a prior call

Response: items[] (Run shape) + next_cursor (empty when the page
exhausts the result set).
`),
	}, resolver.listRuns)
}

// listRuns is the tool handler.
func (r *runResolver) listRuns(ctx context.Context, _ *mcp.CallToolRequest, in ListRunsInput) (*mcp.CallToolResult, ListRunsOutput, error) {
	if in.State != "" {
		if _, ok := validRunStates[in.State]; !ok {
			return nil, ListRunsOutput{}, fmt.Errorf("state %q is not one of pending, running, succeeded, failed, cancelled", in.State)
		}
	}
	limit := clampListRunsLimit(in.Limit)
	page, err := r.api.ListRuns(ctx, listRunsFilter{
		Repo:       in.Repo,
		WorkflowID: in.WorkflowID,
		State:      in.State,
		Limit:      limit,
		Cursor:     in.Cursor,
	})
	if err != nil {
		return nil, ListRunsOutput{}, fmt.Errorf("list runs: %w", err)
	}
	return nil, ListRunsOutput{Items: page.Items, NextCursor: page.NextCursor}, nil
}

// clampListRunsLimit applies the default + cap. Centralized so the
// test surface can exercise the clamp directly.
func clampListRunsLimit(n int) int {
	if n <= 0 {
		return listRunsLimitDefault
	}
	if n > listRunsLimitMax {
		return listRunsLimitMax
	}
	return n
}

// RuntimeCalibrationInput is the fishhawk_runtime_calibration tool's
// input schema. All fields are optional; omitting them returns stats
// across all implement stages in the audit log.
type RuntimeCalibrationInput struct {
	WorkflowID string `json:"workflow_id,omitempty" jsonschema:"filter to a specific workflow (e.g. 'feature_change'); omit for all workflows"`
	StageType  string `json:"stage_type,omitempty" jsonschema:"stage type to aggregate (default 'implement')"`
	Since      string `json:"since,omitempty" jsonschema:"RFC 3339 lower-bound on entry timestamp; omit for all time"`
}

// RuntimeCalibrationOutput mirrors the /v0/calibration response.
// ConfidenceBandAccuracy is keyed by confidence level (low/medium/high);
// each value is an object with 'samples' and 'within_1.5x' counts.
type RuntimeCalibrationOutput struct {
	WorkflowID             string         `json:"workflow_id,omitempty"`
	StageType              string         `json:"stage_type"`
	Samples                int            `json:"samples"`
	PredictedP50Minutes    float64        `json:"predicted_p50_minutes"`
	ActualP50Minutes       float64        `json:"actual_p50_minutes"`
	ActualP95Minutes       float64        `json:"actual_p95_minutes"`
	CalibrationRatio       float64        `json:"calibration_ratio"`
	ConfidenceBandAccuracy map[string]any `json:"confidence_band_accuracy"`
}

// registerRuntimeCalibration wires the fishhawk_runtime_calibration
// tool. Agents call this before writing a plan to self-correct
// runtime estimates using calibration_ratio and confidence band
// accuracy. The tool is read-only and works with both fhm_* and
// fhk_* tokens.
func registerRuntimeCalibration(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_runtime_calibration",
		Description: strings.TrimSpace(`
Fetch runtime calibration statistics for Fishhawk implement stages.

Call this BEFORE writing a plan to self-correct predicted_runtime_minutes
using historical actual vs. predicted data. The key fields:

  - calibration_ratio: actual_p50 / predicted_p50. Multiply your raw
    estimate by this ratio to get a historically calibrated value.
    A ratio > 1 means past predictions were too optimistic; < 1 means
    too pessimistic.
  - confidence_band_accuracy: per-confidence-level sample counts and
    'within_1.5x' hit counts. A low within_1.5x rate for 'high'
    confidence entries signals over-confidence in that category.
  - actual_p95_minutes: the tail; use this to set a conservative
    budget when the cost of overrun is high.

Inputs (all optional):
  - workflow_id — scope to a specific workflow (e.g. 'feature_change')
  - stage_type  — default 'implement'
  - since       — RFC 3339 lower bound; omit for all-time stats

Zero samples is normal on a fresh installation.
`),
	}, resolver.runtimeCalibration)
}

// runtimeCalibration is the tool handler.
func (r *runResolver) runtimeCalibration(ctx context.Context, _ *mcp.CallToolRequest, in RuntimeCalibrationInput) (*mcp.CallToolResult, RuntimeCalibrationOutput, error) {
	res, err := r.api.GetCalibration(ctx, CalibrationParams(in))
	if err != nil {
		return nil, RuntimeCalibrationOutput{}, fmt.Errorf("get calibration: %w", err)
	}
	return nil, RuntimeCalibrationOutput{
		WorkflowID:             res.WorkflowID,
		StageType:              res.StageType,
		Samples:                res.Samples,
		PredictedP50Minutes:    res.PredictedP50Minutes,
		ActualP50Minutes:       res.ActualP50Minutes,
		ActualP95Minutes:       res.ActualP95Minutes,
		CalibrationRatio:       res.CalibrationRatio,
		ConfidenceBandAccuracy: res.ConfidenceBandAccuracy,
	}, nil
}
