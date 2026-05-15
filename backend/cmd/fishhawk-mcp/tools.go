package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

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
	// Subsequent tools register here:
	//   E19.4 / #344 — fishhawk_get_plan
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
