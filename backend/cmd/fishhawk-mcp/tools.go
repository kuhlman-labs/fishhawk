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
	// Subsequent tools register here:
	//   E19.5 / #345 — fishhawk_get_run_status
	//   E19.6 / #346 — fishhawk_list_audit
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
