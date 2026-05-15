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
	PlanVersion         string             `json:"plan_version"`
	TicketReference     PlanTicketRef      `json:"ticket_reference"`
	GeneratedBy         PlanGeneratedBy    `json:"generated_by"`
	Summary             string             `json:"summary"`
	Scope               PlanScope          `json:"scope"`
	Approach            []PlanApproachStep `json:"approach"`
	Verification        PlanVerification   `json:"verification"`
	RisksAndAssumptions []string           `json:"risks_and_assumptions,omitempty"`
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

// GetPlanOutput is the response shape. Status is `available` or
// `no_plan_yet`; on `no_plan_yet` Plan is nil and Message explains
// why so an agent reading the response can branch on the state
// without parsing prose.
type GetPlanOutput struct {
	Status      string       `json:"status" jsonschema:"either 'available' or 'no_plan_yet'"`
	Message     string       `json:"message,omitempty" jsonschema:"human-readable explanation when status=no_plan_yet"`
	Plan        *PlanContent `json:"plan,omitempty"`
	ResolvedVia string       `json:"resolved_via,omitempty" jsonschema:"'self' when the plan came from the requested run; 'parent:<run_id>' when the parent-walk resolved it for a CI-retry chain"`
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
rollback_plan), and risks_and_assumptions when present.

Response status:
  - "available"     — Plan is populated; ResolvedVia tells you whether
                      it came from the requested run ("self") or a
                      parent in the retry chain ("parent:<run_id>").
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
			return nil, GetPlanOutput{
				Status:      "available",
				Plan:        p,
				ResolvedVia: resolvedVia,
			}, nil
		}
		runRow, err := r.api.GetRun(ctx, current)
		if err != nil {
			return nil, GetPlanOutput{}, fmt.Errorf("get run for parent walk: %w", err)
		}
		if runRow.ParentRunID == nil {
			return nil, GetPlanOutput{
				Status:  "no_plan_yet",
				Message: fmt.Sprintf("no terminal plan artifact on run %s (chain root reached at depth %d)", runID, depth),
			}, nil
		}
		current = *runRow.ParentRunID
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
	var planStageID uuid.UUID
	for _, st := range stages {
		if st.Type == "plan" {
			planStageID = st.ID
			break
		}
	}
	if planStageID == uuid.Nil {
		return nil, false, nil
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
	if len(picked.Content) == 0 {
		// Backend invariant: ListStageArtifacts returns content
		// inline. An empty content suggests a partial response we
		// shouldn't try to parse — surface as not-yet rather than
		// a confusing JSON parse error.
		return nil, false, nil
	}
	var p PlanContent
	if err := json.Unmarshal(picked.Content, &p); err != nil {
		return nil, false, fmt.Errorf("decode plan artifact: %w", err)
	}
	return &p, true, nil
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

	return nil, GetRunStatusOutput{
		Run:         *runRow,
		Stages:      stages,
		RecentAudit: recent,
	}, nil
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
