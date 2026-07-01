package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/securityscan"
)

// runResolver bundles the API client + an env getter so the tool
// handlers can read FISHHAWK_RUN_ID / GITHUB_REPOSITORY without
// reaching into os.Getenv directly. Tests substitute an envFunc
// backed by a literal map; production passes os.Getenv.
type runResolver struct {
	api    *apiClient
	getenv func(string) string

	// reviewPollInterval is the poll cadence fishhawk_await_review uses
	// while a review is pending. Zero falls back to
	// defaultReviewPollInterval; tests inject a sub-millisecond value so
	// the poll loop runs without wall-clock sleeps.
	reviewPollInterval time.Duration
}

// registerTools wires every MCP tool onto srv. Called once at
// server startup; the SDK keeps the handlers alive for the
// lifetime of the stdio session. Tools register in alphabetical
// order so the protocol's tool-listing endpoint returns a stable
// ordering for clients that index on position.
func registerTools(srv *mcp.Server, resolver *runResolver) {
	registerAnswerClarification(srv, resolver)
	registerGetActiveRun(srv, resolver)
	registerGetPlan(srv, resolver)
	registerGetRunStatus(srv, resolver)
	registerAwaitAudit(srv, resolver)
	registerAwaitReview(srv, resolver)
	registerListAudit(srv, resolver)
	registerStartRun(srv, resolver)
	registerResumeRun(srv, resolver)
	registerStartCampaign(srv, resolver)
	registerStartCampaignItemRun(srv, resolver)
	registerGetCampaignStatus(srv, resolver)
	registerResumeCampaign(srv, resolver)
	registerCancelRun(srv, resolver)
	registerConsolidateSlices(srv, resolver)
	registerResetRunBranch(srv, resolver)
	registerRetryStage(srv, resolver)
	registerFileIssue(srv, resolver)
	registerFixupStage(srv, resolver)
	registerWaiveConcern(srv, resolver)
	registerDeferConcern(srv, resolver)
	registerListScopeAmendments(srv, resolver)
	registerDecideScopeAmendment(srv, resolver)
	registerDecideScopeCompleteness(srv, resolver)
	registerApprovePlan(srv, resolver)
	registerRejectPlan(srv, resolver)
	registerApproveDeploy(srv, resolver)
	registerRejectDeploy(srv, resolver)
	registerRevisePlan(srv, resolver)
	registerListRuns(srv, resolver)
	registerRunStage(srv, resolver)
	registerDispatchStage(srv, resolver)
	registerRunChildren(srv, resolver)
	registerRuntimeCalibration(srv, resolver)
	registerVerifyRun(srv, resolver)
	registerVouchCommit(srv, resolver)
	registerReportProductIssue(srv, resolver)
	registerDoctor(srv, resolver)
	registerInit(srv, resolver)
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
Resolve the Fishhawk run UUID for the current context when you do not
already have it. Reach for this first when you hold a PR number, a
trigger ref (e.g. "issue:42"), or the runner's FISHHAWK_RUN_ID env but
need the run id that fishhawk_get_run_status / fishhawk_get_plan take —
the "which run" resolver, as distinct from fishhawk_list_runs (browse
many runs).

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
	// PlanReviewStatus is the review lifecycle summary for the plan stage
	// (#600): none|pending|complete|skipped|failed derived from the audit
	// trail. Re-polling fishhawk_get_run_status is the authoritative path to
	// a terminal status (#879); on 'pending' the ReviewStatus carries a
	// server-suggested poll_interval_seconds. 'pending' (a review was
	// dispatched but no verdict landed) is the state Reviews[] alone cannot
	// express — fishhawk_await_review is an optional convenience block over
	// the poll.
	PlanReviewStatus *ReviewStatus `json:"plan_review_status,omitempty" jsonschema:"review lifecycle for the plan stage: status is one of none, pending, complete, skipped, failed. Re-polling fishhawk_get_run_status is the authoritative way to reach a terminal status. 'complete' means ALL configured agent reviewers have landed a terminal verdict (reviews[] carries one row per configured reviewer); 'pending' means a review was dispatched but the configured reviewers have not all landed yet — re-poll on the advertised poll_interval_seconds (this now also covers the partial-landing window where some but not all heterogeneous reviewers have returned; fishhawk_await_review is an optional convenience block); 'failed' means the reviewer errored or timed out (terminal)"`
	// ScopePrecheck surfaces the plan-gate scope/constraint pre-check
	// (#658): scope.files evaluated against the implement stage's
	// forbidden_paths/allowed_paths/max_files_changed before approval.
	ScopePrecheck *ScopePrecheck `json:"scope_precheck,omitempty" jsonschema:"plan-gate scope pre-check (#658): flags scope.files that would violate the implement stage's forbidden_paths/allowed_paths/max_files_changed constraints, using the same matcher as the post-implement gate. Present (possibly with empty violations) when the plan stage ran the pre-check; absent on older runs predating it. A non-empty violations[] means this plan's scope would fail the implement stage's path constraints before any code is written — most often a sign the run is on the wrong workflow"`
	// SurfaceSweep surfaces the plan-gate surface-sweep advisory (#763):
	// scope.files evaluated against the static surface registry for
	// sibling surfaces a plan must move in lockstep with.
	SurfaceSweep *SurfaceSweep `json:"surface_sweep,omitempty" jsonschema:"plan-gate surface sweep (#763): flags sibling surfaces a plan must move together with. When scope.files touches one surface of a known multi-surface pattern (an @-mention render surface, or an audit-kind emitter that mandates a docs/issue-comment-surfaces.md entry) but omits a required sibling, that sibling is reported. Present (possibly with empty findings) when the plan stage ran the sweep; absent on older runs predating it. A non-empty findings[] means the plan likely forgot a surface that must change in lockstep"`
	// TestSweep surfaces the plan-gate test-sweep advisory (#942):
	// scope.files evaluated against the repository's existing *_test.go
	// files via the Contents API.
	TestSweep *TestSweep `json:"test_sweep,omitempty" jsonschema:"plan-gate test sweep (#942): heuristic advisory flagging EXISTING test files the plan omitted — a stem-sibling test of a scoped production .go file, existing tests in a package where the plan creates a new test file, or a path-trigger rule's pinned test (migration_walk: a scoped migrations/*.sql requires the postgres_test.go that pins the latest migration). Judge whether the changed behavior's tests or shared harness live in the flagged files; if so the plan must scope them or the runner will scope_drift-exclude the agent's edits to them. Present (possibly with empty findings) when the plan stage ran the sweep; absent on older runs, non-GitHub triggers, and fail-open paths. listed_dirs below scanned directories means some listings failed and findings may be incomplete"`
}

// TestSweepFinding is one test-sweep result decoded from a plan_test_sweep
// audit entry (#942): the plan touches TriggerPath but omits the existing
// test files MissingTests the named Rule (stem_sibling |
// new_test_in_tested_package | migration_walk) associates with it.
// OmittedCount carries the number of additional existing test files
// truncated from MissingTests. Mirrors the server-side TestSweepFinding
// shape exactly.
type TestSweepFinding struct {
	Rule         string   `json:"rule"`
	TriggerPath  string   `json:"trigger_path"`
	MissingTests []string `json:"missing_tests"`
	OmittedCount int      `json:"omitted_count,omitempty"`
}

// TestSweep is the plan-gate test-sweep result decoded from the newest
// plan_test_sweep audit entry (#942). Findings is empty when no existing
// test file adjacent to the scoped change was left out of scope;
// ScannedFiles is the number of scope.files evaluated; ListedDirs counts
// the directories successfully listed via the Contents API.
type TestSweep struct {
	Findings     []TestSweepFinding `json:"findings,omitempty" jsonschema:"existing test files the plan omitted; empty when the scoped directories carried no missing adjacent tests"`
	ScannedFiles int                `json:"scanned_files" jsonschema:"number of scope.files the sweep evaluated"`
	ListedDirs   int                `json:"listed_dirs" jsonschema:"directories successfully listed via the Contents API; lower than the scoped-directory count means some listings failed open and findings may be incomplete"`
}

// SurfaceSweepFinding is one missing-sibling result decoded from a
// plan_surface_sweep audit entry (#763): the plan touched TriggerPath
// (a surface in a known multi-surface pattern named Pattern) but omitted
// the MissingSiblings the pattern requires move together. Mirrors the
// server-side SurfaceSweepFinding shape exactly.
type SurfaceSweepFinding struct {
	Pattern         string   `json:"pattern"`
	TriggerPath     string   `json:"trigger_path"`
	MissingSiblings []string `json:"missing_siblings"`
}

// CrossSliceClaim is one decomposition slice's ownership of a lockstep
// pattern's member files in a cross-slice coupling finding (#1102). Mirrors
// the server-side CrossSliceClaim shape exactly.
type CrossSliceClaim struct {
	SliceTitle string   `json:"slice_title"`
	Files      []string `json:"files"`
}

// CrossSliceCouplingFinding is one cross-slice coupling result decoded from
// a plan_surface_sweep audit entry (#1102): a lockstep pattern's member
// files are split across 2+ distinct decomposition slices, so completing the
// seam would otherwise need a runtime scope amendment (which can time out,
// #1035). Mirrors the server-side CrossSliceCouplingFinding shape exactly.
type CrossSliceCouplingFinding struct {
	Pattern string            `json:"pattern"`
	Slices  []CrossSliceClaim `json:"slices"`
}

// SurfaceSweep is the plan-gate surface-sweep result decoded from the
// newest plan_surface_sweep audit entry (#763). Findings is empty when the
// plan's scope.files touched no incomplete multi-surface pattern;
// ScannedFiles is the number of scope.files the sweep evaluated.
// CrossSliceFindings carries the cross-slice coupling pass (#1102).
type SurfaceSweep struct {
	Findings           []SurfaceSweepFinding       `json:"findings,omitempty" jsonschema:"sibling surfaces the plan omitted; empty when scope.files touched no incomplete multi-surface pattern"`
	ScannedFiles       int                         `json:"scanned_files" jsonschema:"number of scope.files the sweep evaluated"`
	CrossSliceFindings []CrossSliceCouplingFinding `json:"cross_slice_findings,omitempty" jsonschema:"plan-gate cross-slice coupling (#1102): lockstep-pattern member files split across 2+ distinct decomposition slices, so completing the seam would otherwise need a runtime scope amendment that can time out (#1035). Each finding names the pattern and which slice owns which member files. The inverse of the same-file-in-two-slices gate (#1062): the fix is consolidating the seam into one slice, not declaring the shared file twice. Empty/absent when no lockstep pattern is split across slices or on older runs predating the pass"`
}

// ScopePrecheckViolation is one path-constraint mismatch decoded from a
// plan_scope_precheck audit entry (#658). Constraint names the
// implement-stage path constraint the plan's scope.files would violate
// (forbidden_paths, allowed_paths, or max_files_changed); Files lists the
// offending scope.files paths when relevant. Mirrors the server-side
// policy.Violation shape exactly.
type ScopePrecheckViolation struct {
	Constraint string   `json:"constraint"`
	Detail     string   `json:"detail"`
	Files      []string `json:"files,omitempty"`
}

// ScopePrecheck is the plan-gate scope pre-check result decoded from the
// newest plan_scope_precheck audit entry (#658). Violations is empty when
// the plan's scope satisfies every implement-stage path constraint;
// ScannedFiles is the number of scope.files the pre-check evaluated.
type ScopePrecheck struct {
	Violations   []ScopePrecheckViolation `json:"violations,omitempty" jsonschema:"path-constraint mismatches; empty when the plan's scope.files satisfy the implement stage's forbidden_paths/allowed_paths/max_files_changed"`
	ScannedFiles int                      `json:"scanned_files" jsonschema:"number of scope.files the pre-check evaluated"`
	// MaxFilesChanged is the resolved implement-stage cap (#983).
	// Omitted (0) on payloads from older backends and when no cap is
	// configured.
	MaxFilesChanged int `json:"max_files_changed,omitempty" jsonschema:"the implement stage's resolved max_files_changed cap; read headroom as max_files_changed - scanned_files before approving (add_scope_files and mid-stage amendments consume it). A non-empty violations[] containing a max_files_changed entry means this plan cannot pass the implement stage it configures as scoped — re-scope, decompose, or approve with --override-scope-cap. Absent when no cap is configured or the backend predates #983"`
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
Read a run's approved plan artifact. Use this after run_stage(plan) and
before fishhawk_approve_plan / fishhawk_reject_plan to inspect what the
agent proposed — the plan-artifact read, as distinct from
fishhawk_get_run_status (the lifecycle snapshot).

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
			reviewStatus, err := r.reviewStatusFor(ctx, current, "plan")
			if err != nil {
				return nil, GetPlanOutput{}, fmt.Errorf("plan review status: %w", err)
			}
			scopePrecheck, err := r.loadScopePrecheck(ctx, current)
			if err != nil {
				return nil, GetPlanOutput{}, fmt.Errorf("load scope precheck: %w", err)
			}
			surfaceSweep, err := r.loadSurfaceSweep(ctx, current)
			if err != nil {
				return nil, GetPlanOutput{}, fmt.Errorf("load surface sweep: %w", err)
			}
			testSweep, err := r.loadTestSweep(ctx, current)
			if err != nil {
				return nil, GetPlanOutput{}, fmt.Errorf("load test sweep: %w", err)
			}
			return nil, GetPlanOutput{
				Status:           "available",
				Plan:             p,
				ResolvedVia:      resolvedVia,
				Reviews:          reviews,
				PlanReviewStatus: reviewStatus,
				ScopePrecheck:    scopePrecheck,
				SurfaceSweep:     surfaceSweep,
				TestSweep:        testSweep,
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
//
// Floored to the latest plan_revised boundary (#1201): the get_plan Reviews[]
// field reads the CURRENT revision's verdicts only, identically to
// plan_review_status (reviewStatusFor). A fishhawk_revise_plan re-opens the
// plan gate and writes a plan_revised entry; flooring past its sequence drops
// the stale pre-revision round so a fresh re-review is not masked by old
// verdicts. When no plan_revised entry exists the floor is 0 (a no-op since
// sequences are >= 1), so the no-revise plan path is byte-for-byte unchanged.
func (r *runResolver) loadPlanReviews(ctx context.Context, runID uuid.UUID) ([]PlanReview, error) {
	sinceSeq, err := r.latestPlanRevisedSeq(ctx, runID)
	if err != nil {
		return nil, err
	}

	reviews, err := r.decodeReviewVerdicts(ctx, runID, "plan_reviewed", sinceSeq)
	if err != nil {
		return nil, err
	}

	// Second pass: plan_review_skipped entries (#574). These mark a
	// configured agent layer that was not wired on the backend. Each
	// surfaces as a synthesized PlanReview with verdict "skipped" so
	// an agent reading the response can tell a degraded gate from a
	// real verdict without a separate audit query. Floored to the same
	// plan_revised boundary as the verdict read above.
	skipped, err := r.decodeSkippedReviews(ctx, runID, "plan_review_skipped", sinceSeq)
	if err != nil {
		return nil, err
	}
	return append(reviews, skipped...), nil
}

// loadScopePrecheck fetches the NEWEST plan_scope_precheck audit entry
// (#658) for the run and decodes its payload into a ScopePrecheck. The
// backend's per-run audit endpoint returns entries sequence-ascending, so
// the authoritative entry is the last one: a schema-retry run (#646)
// re-opens the plan stage and writes a second precheck on the re-upload,
// and the latest reflects the plan the human actually approves. Returns
// nil when no entry exists (an older run predating the pre-check, or a
// fail-open no-op) so the field is omitted from the response. A corrupt
// payload is treated as "not checked" rather than failing the whole plan
// fetch, mirroring decodeReviewVerdicts' degradation contract.
func (r *runResolver) loadScopePrecheck(ctx context.Context, runID uuid.UUID) (*ScopePrecheck, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: "plan_scope_precheck",
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
		return nil, nil
	}
	raw, merr := json.Marshal(newest.Payload)
	if merr != nil {
		return nil, nil
	}
	var sp ScopePrecheck
	if uerr := json.Unmarshal(raw, &sp); uerr != nil {
		return nil, nil
	}
	return &sp, nil
}

// loadSurfaceSweep fetches the NEWEST plan_surface_sweep audit entry (#763)
// for the run and decodes its payload into a SurfaceSweep. As with
// loadScopePrecheck the backend's per-run audit endpoint returns entries
// sequence-ascending, so the authoritative entry is the last one: a
// schema-retry run re-uploads the plan and writes a second sweep, and the
// latest reflects the plan the human actually approves. Returns nil when no
// entry exists (an older run predating the sweep, or a fail-open no-op) so
// the field is omitted. A corrupt payload is treated as "not checked"
// rather than failing the whole plan fetch.
func (r *runResolver) loadSurfaceSweep(ctx context.Context, runID uuid.UUID) (*SurfaceSweep, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: "plan_surface_sweep",
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
		return nil, nil
	}
	raw, merr := json.Marshal(newest.Payload)
	if merr != nil {
		return nil, nil
	}
	var ss SurfaceSweep
	if uerr := json.Unmarshal(raw, &ss); uerr != nil {
		return nil, nil
	}
	return &ss, nil
}

// loadTestSweep fetches the NEWEST plan_test_sweep audit entry (#942) for
// the run and decodes its payload into a TestSweep. As with
// loadScopePrecheck and loadSurfaceSweep the backend's per-run audit
// endpoint returns entries sequence-ascending, so the authoritative entry
// is the last one: a schema-retry run re-uploads the plan and writes a
// second sweep, and the latest reflects the plan the human actually
// approves. Returns nil when no entry exists (an older run predating the
// sweep, or a fail-open no-op) so the field is omitted. A corrupt payload
// is treated as "not checked" rather than failing the whole plan fetch.
func (r *runResolver) loadTestSweep(ctx context.Context, runID uuid.UUID) (*TestSweep, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: "plan_test_sweep",
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
		return nil, nil
	}
	raw, merr := json.Marshal(newest.Payload)
	if merr != nil {
		return nil, nil
	}
	var ts TestSweep
	if uerr := json.Unmarshal(raw, &ts); uerr != nil {
		return nil, nil
	}
	return &ts, nil
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

// RunNextAction mirrors GET /v0/runs/{run_id}'s next_action object
// (#1023): the distilled operator next step from the run's most recent
// run_auto_advanced audit entry.
type RunNextAction struct {
	Action string `json:"action" jsonschema:"the distilled operator next step, e.g. run_implement_stage (dispatch the implement stage from the operator host) or merge_pr (review and merge the PR)"`
	Detail string `json:"detail,omitempty" jsonschema:"one-line elaboration of the action"`
	PRURL  string `json:"pr_url,omitempty" jsonschema:"the pull request the action refers to, when relevant"`
}

// RunAutoAdvance mirrors one entry of GET /v0/runs/{run_id}'s
// auto_advanced list (#1023): a transition the drive engine
// auto-advanced (or parked with a next action), distilled from the
// run's run_auto_advanced audit trail.
type RunAutoAdvance struct {
	Rule      string    `json:"rule" jsonschema:"the named drive rule that fired: plan_approved_dispatch, reviews_settled_gate, fixup_rereview_repark, checks_green_awaiting_merge, or ci_failed (its negative mirror: a required PR check concluded red)"`
	From      string    `json:"from" jsonschema:"the transition's from edge"`
	To        string    `json:"to" jsonschema:"the transition's to edge"`
	Parked    bool      `json:"parked,omitempty" jsonschema:"true when the mechanical rule could not be backend-executed (runner_kind local dispatch, ADR-024) and recorded a park-with-next-action instead of an executed advance"`
	Timestamp time.Time `json:"ts" jsonschema:"when the transition was recorded"`
}

// DriveStatus is the drive-mode read view (#1023) the get_run_status
// tool surfaces for drive-enabled runs: which transitions advanced
// themselves and what (if anything) the run is waiting on the operator
// for. Omitted entirely for non-drive runs.
type DriveStatus struct {
	Drive         bool             `json:"drive" jsonschema:"always true — the block is omitted entirely for non-drive runs"`
	DerivedStatus string           `json:"derived_status,omitempty" jsonschema:"presentation-only status: awaiting_merge when every gate is resolved and required PR checks are green on an open PR, or ci_failed when a required PR check concluded red (its negative mirror). Never a persisted run state — run.state stays running while parked here"`
	NextAction    *RunNextAction   `json:"next_action,omitempty" jsonschema:"the distilled operator next step from the most recent auto-advance; omitted on terminal runs and when nothing waits on the operator"`
	AutoAdvanced  []RunAutoAdvance `json:"auto_advanced,omitempty" jsonschema:"the run's auto-advanced (or parked-with-next-action) transitions, oldest first"`
}

// runDriveView decodes GET /v0/runs/{run_id} into the thin Run mirror
// plus the drive read surfaces (#1023) the client.go Run shape does
// not carry. Local to the get_run_status tool — its only consumer.
type runDriveView struct {
	Run
	Drive         bool             `json:"drive"`
	DerivedStatus string           `json:"derived_status"`
	NextAction    *RunNextAction   `json:"next_action"`
	AutoAdvanced  []RunAutoAdvance `json:"auto_advanced"`
}

// driveStatus distills the view into the tool's drive_status block.
// nil (field omitted) for non-drive runs, so the block never claims
// drive semantics on a legacy run.
func (v *runDriveView) driveStatus() *DriveStatus {
	if !v.Drive {
		return nil
	}
	return &DriveStatus{
		Drive:         true,
		DerivedStatus: v.DerivedStatus,
		NextAction:    v.NextAction,
		AutoAdvanced:  v.AutoAdvanced,
	}
}

// fetchRunDriveView reads the single-run endpoint once, decoding both
// the Run mirror and the drive surfaces from the same response body.
func (r *runResolver) fetchRunDriveView(ctx context.Context, runID uuid.UUID) (*runDriveView, error) {
	var v runDriveView
	if err := r.api.do(ctx, http.MethodGet, "/v0/runs/"+runID.String(), nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

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
	// PlanReviewStatus / ImplementReviewStatus summarize each stage's review
	// lifecycle (#600): none|pending|complete|skipped|failed derived from the
	// audit trail. Re-polling this tool is the AUTHORITATIVE way to reach a
	// terminal review status (#879); on 'pending' each ReviewStatus carries a
	// server-suggested poll_interval_seconds cadence. Since #1127 'complete'
	// means ALL configured agent reviewers have landed a terminal verdict;
	// 'pending' (a review was dispatched but the configured reviewers have not
	// all landed yet — including the heterogeneous partial-landing window) is
	// the state the Reviews slices alone cannot express; fishhawk_await_review
	// is an optional convenience block over the same poll.
	PlanReviewStatus      *ReviewStatus `json:"plan_review_status,omitempty" jsonschema:"review lifecycle for the plan stage: status is one of none, pending, complete, skipped, failed. Re-polling fishhawk_get_run_status is the AUTHORITATIVE way to reach a terminal review status. 'complete' means ALL configured agent reviewers have landed a terminal verdict (reviews[] carries one row per configured reviewer); 'pending' means a review was dispatched but the configured reviewers have not all landed yet — re-poll on the advertised poll_interval_seconds (this now also covers the partial-landing window where some but not all heterogeneous reviewers have returned; fishhawk_await_review is an optional convenience block over the same poll); 'failed' means the reviewer errored or timed out (terminal)"`
	ImplementReviewStatus *ReviewStatus `json:"implement_review_status,omitempty" jsonschema:"review lifecycle for the implement stage: status is one of none, pending, complete, skipped, failed. Re-polling fishhawk_get_run_status is the AUTHORITATIVE way to reach a terminal review status. 'complete' means ALL configured agent reviewers have landed a terminal verdict (reviews[] carries one row per configured reviewer); 'pending' means a review was dispatched but the configured reviewers have not all landed yet — re-poll on the advertised poll_interval_seconds (this now also covers the partial-landing window where some but not all heterogeneous reviewers have returned; fishhawk_await_review is an optional convenience block over the same poll); 'failed' means the reviewer errored or timed out (terminal)"`
	// PlanStageWaitStatus / ImplementStageWaitStatus summarize each stage's
	// EXECUTION lifecycle (#879/#880, ADR-037): pending|running|succeeded|
	// failed|cancelled derived from the stage row. Re-polling this tool is the
	// AUTHORITATIVE way to await a stage's terminal status; while the status is
	// non-terminal each StageWaitStatus carries a server-suggested
	// poll_interval_seconds cadence (30s, dropped once the run itself is
	// terminal per the ADR-036 #874 backstop). Distinct from the *ReviewStatus
	// pair above, which tracks a stage's REVIEW rather than its execution.
	// Omitted (nil) when no stage of that type exists in the run.
	PlanStageWaitStatus      *StageWaitStatus `json:"plan_stage_wait_status,omitempty" jsonschema:"execution lifecycle for the plan stage: status is one of pending, running, succeeded, failed, cancelled. Re-polling fishhawk_get_run_status is the AUTHORITATIVE way to await a stage's terminal status; while non-terminal it carries a server-suggested poll_interval_seconds cadence. Omitted when no plan stage exists"`
	ImplementStageWaitStatus *StageWaitStatus `json:"implement_stage_wait_status,omitempty" jsonschema:"execution lifecycle for the implement stage: status is one of pending, running, succeeded, failed, cancelled. Re-polling fishhawk_get_run_status is the AUTHORITATIVE way to await a stage's terminal status; while non-terminal it carries a server-suggested poll_interval_seconds cadence. Omitted when no implement stage exists"`
	// Budget is the workflow's current periodic-budget status (#693 /
	// ADR-030), fetched best-effort. Omitted when the workflow declares
	// no budget or the fetch failed — DISPLAY-ONLY, never gates a run.
	Budget *BudgetStatus `json:"budget,omitempty" jsonschema:"workflow periodic-budget status for the current calendar period (spend vs limit, tier ok|warn|over); omitted when no budget is configured. Display-only — never blocks the run"`
	// CacheEfficiency is the run's prompt-cache efficiency metric (ADR-044
	// slice 3 / #1352), fetched best-effort and derived from the run's
	// cost_recorded ledger. Omitted when the run has no cost data or the
	// fetch failed — DISPLAY-ONLY, never gates a run.
	CacheEfficiency *CacheEfficiency `json:"cache_efficiency,omitempty" jsonschema:"per-run prompt-cache efficiency derived from the cost ledger (ADR-044): cache_read_ratio (share of input served from cache), reuse_factor (re-reads per cache-write token), and gross/penalty/net USD savings, with a per-stage (plan_review|implement_review|agent) breakdown. Omitted when the run has no cost data. Display-only — never blocks the run"`
	// Cost is the run's estimated cost surface (#1372), fetched best-effort
	// and derived from the run's cost_recorded ledger. Omitted when the run has
	// no cost data or the fetch failed — DISPLAY-ONLY, never gates a run.
	Cost *RunCost `json:"cost,omitempty" jsonschema:"per-run estimated cost derived from the cost ledger (#1372): total_cost_usd, a per-stage (agent|plan_review|implement_review) breakdown, and — when the run resolved to a merged PR — a cost-per-merged-PR rollup (cost_per_merged_pr_usd summed across every run on that PR plus run_count). Omitted when the run has no cost data. Display-only — never blocks the run"`
	// ReviewActionHint is a display-only next-action pointer (#777) surfaced
	// when the implement review has landed with unresolved approve_with_concerns
	// concerns and the bounded fix-up budget is not yet spent. It points at
	// fishhawk_fixup_stage (route the concerns back to the agent) vs approving
	// to merge, plus the concern count and remaining fix-up budget. Omitted
	// when there is no actionable concern or the budget is exhausted — never
	// gates the run (mirrors the periodic-budget block). Not surfaced on
	// fishhawk_start_run: no implement review exists at run start.
	ReviewActionHint *ReviewActionHint `json:"review_action_hint,omitempty" jsonschema:"display-only next-action pointer when an implement review returned unresolved approve_with_concerns concerns and the fix-up budget is not spent; points at fishhawk_fixup_stage vs approving to merge. Omitted when there is nothing to act on. Never gates the run"`
	// ImplementReviewMergeHint is a display-only merge-readiness warning (#947
	// local-loop parity) surfaced while the implement-stage agent review is
	// 'pending' (dispatched, no terminal verdict). It mirrors the backend's
	// review-pending presence gate: the required fishhawk_audit_complete check
	// is held pending on the same condition, so the PR is not safe to merge or
	// resolve yet — the check flips green automatically once the verdict lands.
	// Omitted once the implement review reaches a terminal status. Display-only,
	// never gates the run (no MCP merge tool; the operator merges on GitHub).
	ImplementReviewMergeHint string `json:"implement_review_merge_hint,omitempty" jsonschema:"display-only merge-readiness warning while the implement-stage agent review is pending (dispatched but no verdict yet): the PR is NOT safe to merge/resolve because the required fishhawk_audit_complete check is held pending on this review (it flips green automatically once the verdict lands). Omitted once the implement review is terminal. Never gates the run"`
	// DriveStatus is the drive-mode read view (#1023): drive flag,
	// the auto_advanced transition list, the distilled next_action,
	// and the derived awaiting_merge presentation status — so the
	// operator sees which transitions advanced themselves and what
	// the run waits on them for. Omitted for non-drive runs.
	DriveStatus *DriveStatus `json:"drive_status,omitempty" jsonschema:"drive-mode read view (#1023): which transitions auto-advanced (auto_advanced, oldest first), the distilled operator next step (next_action), and the derived awaiting_merge presentation status when every gate is resolved and required checks are green. Omitted entirely for non-drive runs"`
	// NextActions is the server-suggested next-action block (#1024): the
	// classified run lifecycle state plus at least one legal next action
	// for every non-terminal run, generalizing review_action_hint across
	// the whole lifecycle. Computed entirely from the data fetched above
	// (pure function — never fails the snapshot). Display-only, never
	// gates the run. For drive-enabled runs the drive next_action is
	// folded in as the first entry so the two surfaces agree.
	NextActions *NextActions `json:"next_actions,omitempty" jsonschema:"server-suggested next actions (#1024): the classified run lifecycle state plus the legal next moves — each entry names the tool to call (with key params), its precondition, what it consumes (none, fixup_budget, retry_budget, approval_slot, new_run), and a one-line reason. Every non-terminal run carries at least one action; terminal runs carry the state with no actions. Display-only — never gates the run"`
	// ChildrenStatus is the decomposed-parent per-child + integration-phase
	// view (#1147): each child's live lifecycle state in slice-index order
	// plus the fan-in phase classified from the slices_integrated /
	// slice_integration_conflict audit kinds. Best-effort — a per-child read
	// failure degrades that child to state="unknown" rather than failing the
	// snapshot. Cost-gated: fetched only for a decomposed parent (no
	// parent_run_id, plus an awaiting_children implement stage or a
	// decomposition audit marker in the recent window), so ordinary runs pay
	// nothing. Omitted for non-decomposed runs.
	ChildrenStatus *ChildrenStatus `json:"children_status,omitempty" jsonschema:"decomposed-parent per-child status + fan-in phase (#1147): children[] lists each child's live state (pending/running/succeeded/failed/unknown) in slice-index order; integration_phase is running_children, ready_to_integrate, integrated, or integration_conflict; consolidated_branch / conflicting_child_run_id surface the fan-in outcome. Best-effort (a child read failure yields state=unknown, never fails the snapshot). Omitted for non-decomposed runs"`
	// SecurityFindings surfaces the run's unresolved high-severity
	// code-scanning (CodeQL/SAST) findings on the implement diff (#1096),
	// distilled from the newest implement_security_findings audit entry. A
	// SEPARATE signal from the implement-review concerns: a finding here is
	// held by its own merge gate (security_findings_unresolved) and routed
	// to its own fix-up pass, so it never consumes a design-concern budget.
	// Best-effort (a read/decode error or no scan leaves it empty — never
	// fails the snapshot). Omitted when the run has no findings (no scan
	// yet, a clean scan, or a clean re-scan after a fix-up cleared them).
	SecurityFindings []SecurityFinding `json:"security_findings,omitempty" jsonschema:"unresolved high-severity code-scanning (CodeQL/SAST) findings on the implement diff (#1096), from the newest scan. A SEPARATE signal from implement-review concerns — held by its own merge gate and routed to its own fix-up pass, never consuming a design-concern budget. Omitted when the run has no findings (no scan, a clean scan, or a clean re-scan after a fix-up)"`
}

// SecurityFinding is one high-severity code-scanning finding on the MCP
// run-status surface (#1096), mirroring the REST security_findings shape so
// the agent locates and opens the alert (severity, rule, path:line, link).
type SecurityFinding struct {
	Number      int    `json:"number" jsonschema:"the alert's per-repo identifier"`
	RuleID      string `json:"rule_id" jsonschema:"the analysis rule that fired (e.g. go/sql-injection)"`
	Description string `json:"description,omitempty" jsonschema:"human-facing rule description"`
	Severity    string `json:"severity" jsonschema:"normalized security-severity: critical or high (the gating levels)"`
	State       string `json:"state,omitempty" jsonschema:"alert state: open, fixed, or dismissed"`
	Path        string `json:"path" jsonschema:"repo-relative file the finding points at"`
	StartLine   int    `json:"start_line,omitempty" jsonschema:"1-based line of the finding; 0 when GitHub omits a location"`
	HTMLURL     string `json:"html_url,omitempty" jsonschema:"link to the alert on GitHub"`
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
Snapshot a run's current state in one call — the agent's "where are we"
query. Use this when you need to know what stage a run is on, what just
happened, or whether a review has landed; it replaces a sequential
GetRun / ListStages / ListAudit chain. Distinct from fishhawk_get_plan
(reads the plan artifact) and fishhawk_get_active_run (resolves which
run); for deeper audit pagination use fishhawk_list_audit.

Returns the Run row (state, workflow, trigger, PR URL when stamped),
the full ordered stage list (each stage's id / type / state /
executor / timing / failure category if any), and the N most-recent
audit entries time-descending (default 5; capped at 50).

Also returns plan_stage_wait_status + implement_stage_wait_status — each a
StageWaitStatus whose status is one of
pending/running/succeeded/failed/cancelled, derived from the durable
(run_id, stage_id) handle. Re-polling this tool is the AUTHORITATIVE way to
await a stage's terminal status: while the status is non-terminal
(pending/running) the StageWaitStatus carries a server-suggested
poll_interval_seconds (30s) — re-call get_run_status on that cadence until
the status goes terminal. (The interval is dropped once the run itself is
terminal, so the wait never strands.) fishhawk_run_stage's
synchronous-with-progress call is the negotiated fallback for clients that
prefer to block; a future native MCP Tasks (invocationMode:async) mode is
deferred (ADR-033 transport + MCP Tasks GA).

Also returns plan_review_status + implement_review_status — each a
ReviewStatus whose status is one of none/pending/complete/skipped/failed.
Re-polling this tool is the AUTHORITATIVE way to reach a terminal review
status: on "pending" the ReviewStatus carries a server-suggested
poll_interval_seconds — re-call get_run_status on that cadence until the
status goes terminal. fishhawk_await_review is an OPTIONAL convenience that
blocks that poll for you; it is not the primary mechanism.

Also returns implement_reviews[]: implement-review agent verdicts
(ADR-027) when reviewers.agent>0 is configured on the implement stage.
Each entry carries reviewer_kind, authority, verdict, concerns[], and
free_form; a {category:"scope"} concern flags scope.files drift
(flag-only, never an auto-reject). Read these before approving the
implement stage. A verdict of "skipped" with a reason marks an agent
layer that was configured but not wired on the backend.

The run row also carries run.concerns when the run has OPEN review
concerns (#964): the open count, a by_state breakdown (raised /
addressed_pending / reopened), and items[] with each concern's STABLE id,
stage_kind, severity, category, and state. Those ids are the primary
addressing scheme for fishhawk_fixup_stage's concern_ids parameter
(positional indices are deprecated). Note text is elided — read the
originating plan_reviewed / implement_reviewed audit entry for the full
note.

Also returns review_action_hint when the implement review landed with
unresolved approve_with_concerns concerns and the bounded fix-up budget
is not yet spent: a one-line pointer at fishhawk_fixup_stage (route the
concerns back to the agent) vs approving to merge, with the concern
count and remaining fix-up budget. Display-only — never gates the run;
omitted when there is nothing to act on or the budget is exhausted.

Also returns implement_review_merge_hint while the implement-stage agent
review is still pending (dispatched, no verdict yet): a display-only
warning that the PR is NOT safe to merge/resolve because the required
fishhawk_audit_complete check is held pending on that review (#947). It
flips green automatically once the verdict lands; omitted once the
implement review is terminal. Never gates the run.

Also returns drive_status for drive-enabled runs (#1023): auto_advanced
lists the transitions the backend advanced itself (rule + from/to +
timestamp, oldest first; parked marks a runner_kind-local dispatch that
recorded a ready-to-run next action instead), next_action is the
distilled operator next step from the most recent auto-advance, and
derived_status is "awaiting_merge" when every gate is resolved and the
required PR checks are green, or "ci_failed" when a required PR check
concluded red (its negative mirror, #1045) — presentation-only, the run
row's state stays running. Omitted entirely for non-drive runs.

Also returns next_actions (#1024): the classified run lifecycle state
plus at least one LEGAL next action for every non-terminal run — each
entry names the tool to call (with key params), its precondition, what
it consumes (none | fixup_budget | retry_budget | approval_slot |
new_run), and a one-line reason. It generalizes review_action_hint
across the lifecycle (plan dispatch/review/gate, implement failures by
category, review pending, open concerns, the merge ritual, the
#968-class wedge) and embeds the same hint computation for the
concern state, so the two surfaces cannot disagree. On drive-enabled
runs the drive next_action folds in as the first entry. Display-only —
never gates the run.

Also returns children_status for a DECOMPOSED PARENT (#1147): children[]
lists each child's live lifecycle state (pending/running/succeeded/failed,
or unknown when a per-child read failed) in slice-index order, and
integration_phase classifies the fan-in — running_children (a child is
still in flight), ready_to_integrate (all children succeeded, no fan-in
yet), integrated (a slices_integrated audit recorded a clean fan-in, with
consolidated_branch), or integration_conflict (a slice_integration_conflict
audit recorded a merge conflict, with conflicting_child_run_id). Cost-gated:
fetched only for a decomposed parent (an awaiting_children implement stage
or a decomposition audit marker), so ordinary runs make zero extra calls.
Best-effort — never gates the run; omitted for non-decomposed runs.
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

	// One read serves both the Run mirror and the drive surfaces
	// (#1023) — they come off the same GET /v0/runs/{run_id} body.
	view, err := r.fetchRunDriveView(ctx, runID)
	if err != nil {
		return nil, GetRunStatusOutput{}, fmt.Errorf("get run: %w", err)
	}
	runRow := &view.Run

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

	planReviewStatus, err := r.reviewStatusFor(ctx, runID, "plan")
	if err != nil {
		return nil, GetRunStatusOutput{}, fmt.Errorf("plan review status: %w", err)
	}
	implementReviewStatus, err := r.reviewStatusFor(ctx, runID, "implement")
	if err != nil {
		return nil, GetRunStatusOutput{}, fmt.Errorf("implement review status: %w", err)
	}

	// Best-effort periodic-budget status (#693). On a fetch error the
	// field stays nil — never fails the snapshot.
	budgetStatus, _ := r.fetchBudgetStatus(ctx, runID)
	cacheEfficiency, _ := r.fetchCacheEfficiency(ctx, runID)
	runCost, _ := r.fetchRunCost(ctx, runID)

	// Best-effort review-action hint (#777). Derived from the SAME
	// implementReviewStatus computed above (single audit read — the hint
	// and ImplementReviewStatus cannot disagree) plus the implement stage's
	// fix-up-pass count. On any error or when no implement stage exists the
	// field stays nil — never fails the snapshot.
	var reviewActionHint *ReviewActionHint
	if implementStageID, ok := stageIDOfType(stages, "implement"); ok {
		reviewActionHint, _ = r.reviewActionHintFor(ctx, runID, implementStageID, runRow.State, implementReviewStatus)
	}

	// Stage-execution wait status (#879/#880, ADR-037), derived from the
	// stages slice already fetched above and the run row's state — no extra
	// round-trip. nil when no stage of that type exists in the run.
	planStageWaitStatus := stageWaitStatusFor(stages, "plan", runRow.State)
	implementStageWaitStatus := stageWaitStatusFor(stages, "implement", runRow.State)

	// Server-suggested next actions (#1024): a pure function over the
	// run/stage/review/hint/drive data fetched above — no extra
	// round-trip, never fails the snapshot. mergeObserved (#1370) is read
	// off the same `recent` slice and gates the succeeded_merged state.
	mergeObserved := mergeObservedIn(recent)
	nextActions := nextActionsFor(runRow, stages, planReviewStatus, implementReviewStatus, reviewActionHint, view.driveStatus(), mergeObserved)

	// Best-effort decomposed-parent children status (#1147). Cost-gated so an
	// ordinary run pays nothing: only a decomposed parent (no parent_run_id,
	// plus an awaiting_children implement stage OR a decomposition marker in
	// the recent-audit window) triggers the bounded per-child fetch. On a
	// fetch error the field stays nil — never fails the snapshot.
	var childrenStatus *ChildrenStatus
	if shouldFetchChildrenStatus(runRow, stages, recent) {
		childrenStatus, _ = r.childrenStatusFor(ctx, runID, recent)
	}

	// Best-effort security-findings surface (#1096). A dedicated read of the
	// implement_security_findings audit category — a SEPARATE signal from the
	// review concerns. On any error the slice stays nil — never fails the
	// snapshot.
	securityFindings := r.securityFindingsFor(ctx, runID)

	return nil, GetRunStatusOutput{
		Run:                      *runRow,
		Stages:                   stages,
		RecentAudit:              recent,
		ImplementReviews:         implementReviews,
		PlanReviewStatus:         planReviewStatus,
		ImplementReviewStatus:    implementReviewStatus,
		PlanStageWaitStatus:      planStageWaitStatus,
		ImplementStageWaitStatus: implementStageWaitStatus,
		Budget:                   budgetStatus,
		CacheEfficiency:          cacheEfficiency,
		Cost:                     runCost,
		ReviewActionHint:         reviewActionHint,
		ImplementReviewMergeHint: implementReviewMergeHint(implementReviewStatus),
		DriveStatus:              view.driveStatus(),
		NextActions:              nextActions,
		ChildrenStatus:           childrenStatus,
		SecurityFindings:         securityFindings,
	}, nil
}

// securityFindingsFor distills the run's unresolved high-severity
// code-scanning findings (#1096) from the newest implement_security_findings
// audit entry recorded ABOVE the latest stage_fixup_triggered floor — the same
// floor the merge gate (auditcomplete.securityFindingsRule) applies. The floor
// is load-bearing: the webhook writer records no clean marker entry when a
// post-fixup re-scan comes back clean, so without it "newest overall" would
// keep surfacing the pre-fixup dirty entry after a clean re-scan cleared the
// gate. Returns nil — the field is omitted — on any read/decode error
// (best-effort, never fails the snapshot), when the run has had no scan, when
// every scan predates the latest fix-up, or when the newest in-window entry
// carries no findings (a clean scan or a clean re-scan after a fix-up).
func (r *runResolver) securityFindingsFor(ctx context.Context, runID uuid.UUID) []SecurityFinding {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: securityscan.AuditCategorySecurityFindings,
	})
	if err != nil || len(entries) == 0 {
		return nil
	}
	// Floor on the latest fix-up, mirroring the merge gate; a floor-read
	// failure degrades to an omitted block (best-effort).
	floorSeq, ferr := r.latestFixupSequenceFor(ctx, runID)
	if ferr != nil {
		return nil
	}
	newestIdx := -1
	for i := range entries {
		if entries[i].Sequence > floorSeq {
			newestIdx = i
		}
	}
	if newestIdx == -1 {
		return nil
	}
	newest := entries[newestIdx]
	if newest.Payload == nil {
		return nil
	}
	// Payload is decoded JSON (any); re-marshal then unmarshal into the
	// cross-slice {findings:[...]} shape, mirroring decodeReviewVerdicts.
	raw, merr := json.Marshal(newest.Payload)
	if merr != nil {
		return nil
	}
	var payload struct {
		Findings []securityscan.Finding `json:"findings"`
	}
	if uerr := json.Unmarshal(raw, &payload); uerr != nil {
		return nil
	}
	if len(payload.Findings) == 0 {
		return nil
	}
	out := make([]SecurityFinding, 0, len(payload.Findings))
	for _, f := range payload.Findings {
		out = append(out, SecurityFinding{
			Number:      f.Number,
			RuleID:      f.RuleID,
			Description: f.Description,
			Severity:    f.Severity,
			State:       f.State,
			Path:        f.Path,
			StartLine:   f.StartLine,
			HTMLURL:     f.HTMLURL,
		})
	}
	return out
}

// latestFixupSequenceFor returns the audit sequence of the most-recent
// stage_fixup_triggered entry for the run, or 0 when none has been recorded.
// Run-scoped (any stage's fix-up, no stage filter), mirroring the merge gate's
// floor (auditcomplete.latestFixupSequence) so the MCP run-status surface and
// the gate floor the securityscan signal identically (#1096). Distinct from
// fixupPassesAndLatestSeq, which is stage-scoped for the per-stage pass budget.
func (r *runResolver) latestFixupSequenceFor(ctx context.Context, runID uuid.UUID) (int64, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: categoryStageFixupTriggered,
	})
	if err != nil {
		return 0, err
	}
	var latest int64
	for _, e := range entries {
		if e.Sequence > latest {
			latest = e.Sequence
		}
	}
	return latest, nil
}

// decompositionAuditCategories are the recent-audit markers that, present on a
// top-level run, signal it is a decomposed parent whose children-status block
// is worth the bounded per-child fetch (#1147).
var decompositionAuditCategories = map[string]struct{}{
	"plan_decomposed":            {},
	"slices_integrated":          {},
	"slice_integration_conflict": {},
}

// shouldFetchChildrenStatus is the cost gate for the decomposed-parent
// children-status block (#1147). It returns true only for a top-level run
// (no parent_run_id) whose implement stage is awaiting_children OR whose
// recent-audit window carries a decomposition marker. A non-decomposed run
// returns false, so it makes zero extra calls (no LatestPlanDecomposed read).
func shouldFetchChildrenStatus(run *Run, stages []Stage, recent []AuditEntry) bool {
	if run == nil || (run.ParentRunID != nil && *run.ParentRunID != "") {
		return false
	}
	if impl := stageByType(stages, "implement"); impl != nil && impl.State == "awaiting_children" {
		return true
	}
	for i := range recent {
		if _, ok := decompositionAuditCategories[recent[i].Category]; ok {
			return true
		}
	}
	return false
}

// stageIDOfType returns the UUID of the first stage of the given type in the
// run's stage list, or (uuid.Nil, false) when none matches or the id does not
// parse. Used to resolve the implement stage for the review-action hint (#777).
func stageIDOfType(stages []Stage, stageType string) (uuid.UUID, bool) {
	for _, s := range stages {
		if s.Type == stageType {
			id, err := uuid.Parse(s.ID)
			if err != nil {
				return uuid.Nil, false
			}
			return id, true
		}
	}
	return uuid.Nil, false
}

// loadImplementReviews queries implement_reviewed audit entries for the
// given run and decodes each into a PlanReview (the verdict shape is
// identical across plan and implement review, ADR-027). It mirrors
// loadPlanReviews: corrupt payloads are skipped, and a second pass over
// implement_review_skipped entries synthesizes a "skipped" verdict so a
// degraded gate is distinguishable from a real verdict. Returns nil when
// no implement-review entries exist.
func (r *runResolver) loadImplementReviews(ctx context.Context, runID uuid.UUID) ([]PlanReview, error) {
	// sinceSeq=0: this listing surface returns every recorded verdict
	// across all fix-up rounds; only reviewStatusFor floors to the latest
	// fix-up (#894).
	reviews, err := r.decodeReviewVerdicts(ctx, runID, "implement_reviewed", 0)
	if err != nil {
		return nil, err
	}
	skipped, err := r.decodeSkippedReviews(ctx, runID, "implement_review_skipped", 0)
	if err != nil {
		return nil, err
	}
	return append(reviews, skipped...), nil
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
List a run's audit entries with optional filters and cursor pagination.
Use this when you need the filtered or paginated audit trail rather than
the recent slice — e.g. to read the implement_reviewed concern indices
that fishhawk_fixup_stage takes, or to walk a single category.

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

	// BudgetOverride forces the run past a blocking periodic cost
	// budget that is over its limit for the current period (#688 /
	// ADR-030). Ignored unless a blocking budget would otherwise
	// refuse the run with 402 budget_exhausted.
	BudgetOverride bool `json:"budget_override,omitempty" jsonschema:"force the run past a blocking periodic cost budget that is over its limit for the current period; ignored when no blocking budget is over"`

	// UpstreamRunID names the upstream feature_change run whose
	// ci_green / review_merged a standalone deploy-only release
	// run's required_upstream pre-flight gate evaluates (E23.11 /
	// #1417). Distinct from parent_run_id — a deploy-gate safety
	// reference, not a follow-up/lineage link. Optional; omit for
	// non-deploy-gate runs.
	UpstreamRunID string `json:"upstream_run_id,omitempty" jsonschema:"optional UUID of the upstream feature_change run whose ci_green/review_merged a deploy-only release run's required_upstream pre-flight gate evaluates (E23.11/#1417); distinct from parent_run_id — a deploy-gate safety reference, not a lineage link"`
}

// StartRunOutput is the response shape. Run is the canonical Run
// row. Idempotent is true when the backend returned 200 against an
// existing run for the same (repo, idempotency_key) — clients that
// react to "fresh run" (e.g. notify a Slack channel) can branch on
// the flag.
type StartRunOutput struct {
	Run        Run  `json:"run"`
	Idempotent bool `json:"idempotent" jsonschema:"true when this call replayed against an existing run via Idempotency-Key; false on fresh create"`
	// Budget is the workflow's current periodic-budget status (#693 /
	// ADR-030), fetched best-effort. Omitted when the workflow declares
	// no budget or the fetch failed — DISPLAY-ONLY, never gates a run.
	Budget *BudgetStatus `json:"budget,omitempty" jsonschema:"workflow periodic-budget status for the current calendar period (spend vs limit, tier ok|warn|over); omitted when no budget is configured. Display-only — never blocks the run"`
}

// fetchBudgetStatus retrieves the run's periodic-budget status
// best-effort. It NEVER fails the caller: on any error it returns
// (nil, <warning>); on a no-budget response it returns (nil, ""). The
// warning string is for callers that surface a Warnings slice
// (run_stage); start_run / get_run_status simply discard it and omit
// the field.
func (r *runResolver) fetchBudgetStatus(ctx context.Context, runID uuid.UUID) (*BudgetStatus, string) {
	bs, err := r.api.GetRunBudget(ctx, runID)
	if err != nil {
		return nil, fmt.Sprintf("budget status unavailable: %v", err)
	}
	return bs, ""
}

// fetchCacheEfficiency retrieves the run's cache-efficiency metric
// best-effort, mirroring fetchBudgetStatus. It NEVER fails the caller: on
// any error it returns (nil, <warning>); on a no-data response it returns
// (nil, ""). get_run_status discards the warning and omits the field.
func (r *runResolver) fetchCacheEfficiency(ctx context.Context, runID uuid.UUID) (*CacheEfficiency, string) {
	ce, err := r.api.GetRunCacheEfficiency(ctx, runID)
	if err != nil {
		return nil, fmt.Sprintf("cache efficiency unavailable: %v", err)
	}
	return ce, ""
}

// fetchRunCost retrieves the run's estimated-cost surface best-effort,
// mirroring fetchCacheEfficiency. It NEVER fails the caller: on any error it
// returns (nil, <warning>); on a no-data response it returns (nil, "").
// get_run_status discards the warning and omits the field.
func (r *runResolver) fetchRunCost(ctx context.Context, runID uuid.UUID) (*RunCost, string) {
	rc, err := r.api.GetRunCost(ctx, runID)
	if err != nil {
		return nil, fmt.Sprintf("run cost unavailable: %v", err)
	}
	return rc, ""
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
Create a new Fishhawk run — step 1 of the agent-driven local loop. Use
this to mint a run before fishhawk_run_stage drives its stages; the
sequence is fishhawk_run_stage (plan) → fishhawk_approve_plan →
fishhawk_run_stage (implement).

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
				return nil, StartRunOutput{}, fmt.Errorf("%s: %w", found.Path, annotateStaleSpecError(perr))
			}
		}
	} else if len(specBytes) > 0 {
		// Caller supplied workflow_spec inline. Compute the SHA so
		// the backend's content-hash gate still has a value to bind
		// against, and validate so a bad inline body fails locally.
		computedSHA = gitBlobSHA(specBytes)
		if perr := specValidate(specBytes); perr != nil {
			return nil, StartRunOutput{}, fmt.Errorf("workflow_spec: %w", annotateStaleSpecError(perr))
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

	// Validate upstream_run_id locally so a malformed value surfaces a
	// clean error rather than an opaque backend 400.
	if in.UpstreamRunID != "" {
		if _, perr := uuid.Parse(in.UpstreamRunID); perr != nil {
			return nil, StartRunOutput{}, fmt.Errorf("upstream_run_id %q is not a valid UUID: %w", in.UpstreamRunID, perr)
		}
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
		BudgetOverride: in.BudgetOverride,
		UpstreamRunID:  in.UpstreamRunID,
	})
	if err != nil {
		return nil, StartRunOutput{}, fmt.Errorf("start run: %w", err)
	}

	out := StartRunOutput{Run: *created, Idempotent: idempotent}
	// Best-effort periodic-budget status (#693). A parse failure on the
	// run id or a fetch error simply leaves the field nil — never fails
	// the create.
	if runUUID, perr := uuid.Parse(created.ID); perr == nil {
		out.Budget, _ = r.fetchBudgetStatus(ctx, runUUID)
	}

	var meta *mcp.CallToolResult
	if len(warnings) > 0 {
		meta = &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: strings.Join(warnings, "\n")}},
		}
	}
	return meta, out, nil
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
Cancel a whole run. Use this when you want to abandon an in-flight or
stuck run outright — distinct from fishhawk_retry_stage (re-run one
failed stage) and fishhawk_reject_plan (fail just the plan gate).

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

// ConsolidateSlicesInput is the fishhawk_consolidate_slices tool's input
// schema (E24.2 / ADR-041 / #1238). Mirrors
// `POST /v0/runs/{run_id}/consolidate`.
type ConsolidateSlicesInput struct {
	RunID string `json:"run_id" jsonschema:"the Fishhawk run UUID of the decomposed PARENT whose children's slice branches should be fanned in; resolved like the other run-keyed verbs"`
}

// ConsolidateSlicesOutput surfaces the fan-in outcome: integrated (with the
// consolidated branch + PR URL) or a recoverable slice conflict (with the
// conflicting slice index + child run id).
type ConsolidateSlicesOutput struct {
	Result ConsolidateResult `json:"result"`
}

// registerConsolidateSlices wires the fishhawk_consolidate_slices tool (E24.2
// / ADR-041 / #1238).
//
// Auth: write tool. Operator-side fhk_* tokens with `write:runs` scope
// succeed; a run-bound MCP token is rejected (consolidation is an operator
// action, not one the implement agent self-drives).
func registerConsolidateSlices(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_consolidate_slices",
		Description: strings.TrimSpace(`
Use this when a decomposed parent run is stuck in awaiting_children after all
its children have finished on the LOCAL runner — run the E24.2 fan-in on
demand to merge the slice branches into the consolidated branch and open the
consolidated PR. The 60s child-completion sweeper is the automatic backstop
that normally does this, but it is OFF by default in the local dev stack, so
locally a settled parent stays parked until you call this verb.

Distinct from fishhawk_cancel_run (abandon a run) and fishhawk_retry_stage
(re-run one failed stage): this resolves a HEALTHY parent whose children all
succeeded.

Preconditions (else a clean tool error):
  - the run is a decomposed parent (not a child, and it has children);
  - its implement stage is parked in awaiting_children;
  - every child reached a terminal state AND every one succeeded.

Outcomes:
  - integrated      : every slice merged cleanly; the parent implement stage
                      resolved succeeded and the consolidated PR opened. The
                      result carries consolidated_branch + pull_request_url.
  - slice_conflict  : a slice branch failed to merge; the parent implement
                      stage failed recoverable (category-B), preserving the
                      E24.2 contract. The result carries
                      conflicting_slice_index + conflicting_child_run_id.

Unlike the event-driven path, which silently WARN-swallows a non-conflict
integration error, this SURFACES it (slice_integration_error) so you can
diagnose a stuck local fan-in. Returns a tool error on:
  - invalid UUID (caught before the HTTP hop)
  - not_a_decomposed_parent (400)
  - not_awaiting_children (409 — already resolved or not a decomposition)
  - children_in_flight (409 — a child is still non-terminal)
  - children_failed (409 — a child failed; resolve it first)
  - slice_integration_error (502 — the fan-in failed; the cause is surfaced)
`),
	}, resolver.consolidateSlices)
}

// consolidateSlices is the tool handler. All precondition checks, the fan-in
// composition, and the E24.2 contract live server-side in
// server/consolidate.go.
func (r *runResolver) consolidateSlices(ctx context.Context, _ *mcp.CallToolRequest, in ConsolidateSlicesInput) (*mcp.CallToolResult, ConsolidateSlicesOutput, error) {
	runID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, ConsolidateSlicesOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	res, err := r.api.ConsolidateRun(ctx, runID)
	if err != nil {
		return nil, ConsolidateSlicesOutput{}, fmt.Errorf("consolidate slices: %w", err)
	}
	return nil, ConsolidateSlicesOutput{Result: *res}, nil
}

// ResetRunBranchInput is the fishhawk_reset_run_branch tool's input
// schema (ADR-035 remediation, #867). Mirrors
// `POST /v0/runs/{run_id}/reset-branch`. Confirm MUST be true — the reset
// is destructive (it force-rewinds the PR head ref), so it is never
// silent/auto.
type ResetRunBranchInput struct {
	RunID   string `json:"run_id" jsonschema:"the Fishhawk run UUID whose branch is being reset; resolved like the other run-keyed verbs"`
	Reason  string `json:"reason,omitempty" jsonschema:"optional operator rationale, recorded on the branch_reset audit entry"`
	Confirm bool   `json:"confirm" jsonschema:"MUST be true to proceed — this is a destructive force-rewind of the PR head ref; a missing/false confirm is refused"`
}

// ResetRunBranchOutput surfaces the rewind summary: the dropped on-top
// commit, the reset target (last run-authored HEAD), the prior head, and
// the recovery note (the dropped commit stays recoverable via the remote
// reflog).
type ResetRunBranchOutput struct {
	Result ResetBranchResult `json:"result"`
}

// registerResetRunBranch wires the fishhawk_reset_run_branch tool
// (ADR-035 remediation, #867).
//
// Auth: write tool. Operator-side fhk_* tokens with `write:runs` scope
// succeed; a run-bound MCP token may reset ONLY its own run's branch.
func registerResetRunBranch(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_reset_run_branch",
		Description: strings.TrimSpace(`
DESTRUCTIVE, operator-gated remediation: force-rewind a run/PR branch back
to its last run-authored HEAD, dropping a foreign commit pushed ON TOP of
the run's own commits (the ADR-035 post-prevention residual vector).

Use this only when a foreign writer pushed a commit on top of the run's
commits on the open PR branch and you want to drop it. The reset:

  - force-updates the PR head ref back to the newest commit attributable
    to this run (the last run-authored HEAD);
  - re-parks the review gate so CI + the merge reconciler re-evaluate the
    rewound head;
  - records a branch_reset audit entry. The dropped commit stays
    recoverable from the remote reflog / the foreign pusher's own branch.

Safety: the reset is REFUSED unless the foreign commit sits STRICTLY ON
TOP. A foreign ancestor/interleaved commit is out of scope (a reset can't
drop it — that is prevention's job) and returns reset_out_of_scope. The
classification is FAIL-CLOSED: any uncertainty returns reset_not_
determinable rather than force-updating.

Inputs:
  - run_id  : the run whose branch to reset.
  - reason  : optional operator note, recorded on the audit entry.
  - confirm : MUST be true. The force-rewind is destructive, so a
    missing/false confirm is refused (confirmation_required).

Returns the rewind summary (dropped_offending_sha, reset_to_sha,
prior_head_sha, recovery_note) on success. Returns a tool error on:
  - invalid UUID (caught before the HTTP hop)
  - confirmation_required (confirm not true, 400)
  - cross_run_reset (a run-bound token reaching another run's branch, 403)
  - run_not_found (404)
  - reset_out_of_scope (the foreign commit is an ancestor, not on top, 422)
  - reset_not_applicable (no foreign commit on top to drop, 422)
  - reset_not_determinable (fail-closed: lineage not classifiable / lease
    re-check failed, 422)
`),
	}, resolver.resetRunBranch)
}

// resetRunBranch is the tool handler. The on-top/ancestor classification,
// fail-closed posture, lease re-check, and subject-binding all live
// server-side in server/reset_branch.go.
func (r *runResolver) resetRunBranch(ctx context.Context, _ *mcp.CallToolRequest, in ResetRunBranchInput) (*mcp.CallToolResult, ResetRunBranchOutput, error) {
	runID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, ResetRunBranchOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	if !in.Confirm {
		return nil, ResetRunBranchOutput{}, fmt.Errorf("confirm must be true: fishhawk_reset_run_branch force-rewinds the PR head ref and is destructive")
	}
	res, err := r.api.ResetRunBranch(ctx, runID, in.Reason)
	if err != nil {
		return nil, ResetRunBranchOutput{}, fmt.Errorf("reset run branch: %w", err)
	}
	return nil, ResetRunBranchOutput{Result: *res}, nil
}

// RetryStageInput is the fishhawk_retry_stage tool's input schema
// (E22.3 / #392). Mirrors `POST /v0/stages/{stage_id}/retry`.
type RetryStageInput struct {
	StageID string `json:"stage_id" jsonschema:"the Fishhawk stage UUID to retry; must be a stage in a failed state"`
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
Retry a FAILED stage. Use this when a stage has failed and you want the
orchestrator to re-run it fresh — distinct from fishhawk_fixup_stage,
which re-opens a HEALTHY implement-review gate on the same PR branch.
Precondition: the stage must be in a failed state.

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
	Reason string `json:"reason,omitempty" jsonschema:"optional reviewer rationale; injected to the implement agent as binding approval conditions (#558), so use it to amend the plan — also recorded on the approval row as 'comment'"`
	// AddScopeFiles is the structured, authoritative way to add files to the
	// implement stage's scope at approval time (#824) — preferred over naming
	// paths in the free-text reason, which is regex-scraped (#730) and silently
	// misses directories, extensionless/repo-root files, and described-not-spelled
	// paths.
	AddScopeFiles []string `json:"add_scope_files,omitempty" jsonschema:"optional authoritative list of repo-relative paths to fold into the implement stage's scope.files; preferred over naming paths in 'reason'. A trailing slash marks a directory (e.g. 'pkg/testdata/corpus/') whose created files all stage; handles extensionless and repo-root files (e.g. 'go.work') the prose fallback misses"`
	// BindingAssertions is the OPTIONAL list of operator-declared,
	// deterministic binding-assertion checks (#1171) — the machine-checkable
	// half of an approval condition. Each is evaluated by the runner
	// post-implement against the committed scope-only tree; an unsatisfied
	// assertion fails the implement stage category-B (park for re-scope/
	// re-plan). Deterministic substring matching only — never parses prose.
	BindingAssertions []BindingAssertion `json:"binding_assertions,omitempty" jsonschema:"optional list of deterministic binding-assertion checks the operator declares so an explicit approval condition becomes machine-checkable post-implement. Each check has type ('file_contains' or 'test_asserts'), path (repo-relative; must end in _test.go for test_asserts), and literal (a substring that must appear in the committed file). Evaluated by the runner against the committed scope-only tree; any unsatisfied assertion fails the implement stage category-B. Substring matching only — choose a literal specific enough to be meaningful"`
	// ImplementModel is the optional operator override for the implement-stage
	// model (#1013) — the top rung of the resolution ladder. The backend
	// resolves the full ladder at the gate, validates the resolved value
	// against the deployment allow-list (422 plan_invalid_model on an unknown
	// model), and records the model_resolved audit the runner spawn routes
	// through. Omit to leave the model to the lower rungs (spec / plan
	// recommendation / deployment default).
	ImplementModel string `json:"implement_model,omitempty" jsonschema:"optional operator override for the implement-stage model (#1013): the top rung of the resolution ladder (deployment default < spec executor.model < plan model_recommendation < this override). The backend validates the resolved model against the deployment's per-adapter allow-list and rejects an unknown one 422 plan_invalid_model. Omit to ratify the plan's model_recommendation or fall through to the spec/deployment default"`
}

// BindingAssertion is one operator-declared binding-assertion check passed to
// fishhawk_approve_plan (#1171). The wire tags (type/path/literal) are
// byte-identical to the backend's bindingAssertion and the runner's
// upload.BindingAssertion so the declaration round-trips unchanged.
type BindingAssertion struct {
	Type    string `json:"type" jsonschema:"the assertion type: 'file_contains' (literal must appear in the committed file at path) or 'test_asserts' (same substring check, but path must name a Go test file)"`
	Path    string `json:"path" jsonschema:"repo-relative path of the committed file the literal must appear in; for type 'test_asserts' it must end in _test.go"`
	Literal string `json:"literal" jsonschema:"the substring that must be present (deterministic match) in the committed content of path"`
}

// ApprovePlanOutput surfaces the post-approve Stage row plus the
// resolved plan-stage id (the caller passed a run id, not a stage
// id, so the response makes the resolution visible for audit
// clarity).
type ApprovePlanOutput struct {
	Stage   Stage  `json:"stage"`
	StageID string `json:"stage_id" jsonschema:"the resolved plan-stage UUID the approval was posted to"`
	// Duplicate labeling (#986): set when this call was a no-op because the
	// same subject already submitted a decision for this stage.
	DuplicateSubmission bool   `json:"duplicate_submission,omitempty" jsonschema:"true when this submission was a no-op duplicate: the same subject already decided this stage, the prior decision stands, the stage state is unchanged, and NO gates re-ran and NO audit entries were emitted"`
	PriorDecision       string `json:"prior_decision,omitempty" jsonschema:"on a duplicate submission, the existing approval row's decision (approve|reject) — the decision that actually stands"`
}

// RejectPlanInput mirrors `fishhawk plan reject <run-id> [--reason
// …]`. Reason is recommended; the CLI emits a warning when missing
// because reject without a rationale is poor practice.
type RejectPlanInput struct {
	RunID  string `json:"run_id" jsonschema:"the Fishhawk run UUID whose plan stage is being rejected"`
	Reason string `json:"reason,omitempty" jsonschema:"reviewer rationale; recommended on rejects (the CLI warns when missing). Propagates to a fresh run's plan as prior-rejection feedback"`
}

// RejectPlanOutput mirrors ApprovePlanOutput.
type RejectPlanOutput struct {
	Stage   Stage  `json:"stage"`
	StageID string `json:"stage_id" jsonschema:"the resolved plan-stage UUID the rejection was posted to"`
	// Duplicate labeling (#986): see ApprovePlanOutput.
	DuplicateSubmission bool   `json:"duplicate_submission,omitempty" jsonschema:"true when this submission was a no-op duplicate: the same subject already decided this stage, the prior decision stands, the stage state is unchanged, and NO gates re-ran and NO audit entries were emitted"`
	PriorDecision       string `json:"prior_decision,omitempty" jsonschema:"on a duplicate submission, the existing approval row's decision (approve|reject) — the decision that actually stands"`
}

// ApproveDeployInput is the fishhawk_approve_deploy tool's input
// schema (E23.15 / #1432). Takes a run id; the resolver finds the
// type=deploy stage internally, mirroring fishhawk_approve_plan.
//
// The deploy target environment is conveyed to the backend ONLY
// through the approval comment (the backend's parseEnvironmentFlag
// scans whitespace-delimited tokens for --environment=<env>; there is
// no structured environment field on the approval request body), so
// the handler composes --environment=<env> into the comment.
type ApproveDeployInput struct {
	RunID string `json:"run_id" jsonschema:"the Fishhawk run UUID whose deploy stage is being approved"`
	// Environment is REQUIRED: the backend deploy pre-flight reads the
	// target environment from a --environment=<env> token in the approval
	// comment and 422s deploy_environment_not_allowed when it is absent or
	// not one of the deploy stage's allowed_environments. The handler
	// composes --environment=<environment> into the comment.
	Environment string `json:"environment" jsonschema:"REQUIRED target deploy environment; must be one of the deploy stage's allowed_environments (read them from the stage spec). Composed into the approval comment as --environment=<env>, which the backend deploy pre-flight parses; an absent or disallowed value is rejected 422 deploy_environment_not_allowed"`
	// OverrideFreeze appends --override-freeze to the comment. The backend
	// only consults it when the deploy stage declares change_freeze (#1384);
	// the token is a standalone, whitespace-delimited flag the backend's
	// commentHasFlag matches exactly.
	OverrideFreeze bool   `json:"override_freeze,omitempty" jsonschema:"when true, appends --override-freeze to the approval comment so the backend permits a deploy during an active change freeze. Only meaningful when the deploy stage declares change_freeze; absent it the backend 422s deploy_change_freeze_active"`
	Reason         string `json:"reason,omitempty" jsonschema:"optional operator rationale appended to the approval comment after the --environment / --override-freeze flags; recorded on the approval row"`
}

// ApproveDeployOutput surfaces the post-approve Stage row plus the
// resolved deploy-stage id (the caller passed a run id). Duplicate
// labeling (#986) mirrors ApprovePlanOutput.
type ApproveDeployOutput struct {
	Stage   Stage  `json:"stage"`
	StageID string `json:"stage_id" jsonschema:"the resolved deploy-stage UUID the approval was posted to"`
	// Duplicate labeling (#986): set when this call was a no-op because the
	// same subject already submitted a decision for this stage.
	DuplicateSubmission bool   `json:"duplicate_submission,omitempty" jsonschema:"true when this submission was a no-op duplicate: the same subject already decided this stage, the prior decision stands, the stage state is unchanged, and NO gates re-ran and NO audit entries were emitted"`
	PriorDecision       string `json:"prior_decision,omitempty" jsonschema:"on a duplicate submission, the existing approval row's decision (approve|reject) — the decision that actually stands"`
}

// RejectDeployInput mirrors RejectPlanInput for the deploy gate. A
// deploy reject routes through the backend advanceStage path (NOT the
// approve-only deploy pre-flight), so it needs neither write:deploy
// scope nor an environment.
type RejectDeployInput struct {
	RunID  string `json:"run_id" jsonschema:"the Fishhawk run UUID whose deploy stage is being rejected"`
	Reason string `json:"reason,omitempty" jsonschema:"reviewer rationale; recommended on rejects. Recorded on the approval row as 'comment'"`
}

// RejectDeployOutput mirrors RejectPlanOutput.
type RejectDeployOutput struct {
	Stage   Stage  `json:"stage"`
	StageID string `json:"stage_id" jsonschema:"the resolved deploy-stage UUID the rejection was posted to"`
	// Duplicate labeling (#986): see ApproveDeployOutput.
	DuplicateSubmission bool   `json:"duplicate_submission,omitempty" jsonschema:"true when this submission was a no-op duplicate: the same subject already decided this stage, the prior decision stands, the stage state is unchanged, and NO gates re-ran and NO audit entries were emitted"`
	PriorDecision       string `json:"prior_decision,omitempty" jsonschema:"on a duplicate submission, the existing approval row's decision (approve|reject) — the decision that actually stands"`
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
Approve a run's plan so it can proceed to the implement stage. Use this
after reading fishhawk_get_plan, once the plan is sound and its plan
stage is parked awaiting approval — the approve counterpart to
fishhawk_reject_plan.

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

Duplicate labeling (#986, NOT an error — a 200): when the same
subject already submitted a decision for this stage, the call is a
no-op. The output carries duplicate_submission=true and
prior_decision, and the result text says so explicitly: the prior
decision stands, the stage state is unchanged, and the budget/scope
gates did NOT re-run. Never read a duplicate as an effective
approval. The 422 budget/scope-cap refusals fire BEFORE any approval
row is recorded, so retrying with --override-budget /
--override-scope-cap in the reason flows normally.

binding_assertions (#1171, optional): declare deterministic,
machine-checkable conditions alongside (or instead of) free-text in
'reason'. Each is a typed substring check — type 'file_contains' or
'test_asserts', a repo-relative path, and a literal that must appear
in the committed file. The runner evaluates them post-implement
against the committed scope-only tree; any unsatisfied assertion
fails the implement stage category-B (park for re-scope/re-plan).
Use this when an approval condition can be expressed as "file X must
contain literal Y" so the condition is enforced rather than merely
restated. A malformed declaration (unknown type, empty literal, a
test_asserts path not ending in _test.go) is rejected 400
validation_failed before any approval row is recorded.

implement_model (#1013, optional): override the implement-stage model.
The backend resolves the ladder deployment-default < spec executor.model
< plan model_recommendation < this override, validates the resolved
value against the deployment's per-adapter allow-list, and records the
choice as the model_resolved audit the runner spawn routes through. An
unknown resolved model is rejected 422 plan_invalid_model (pre-insert —
retry with an allowed model). Omit to ratify the plan's
model_recommendation or fall through to the spec/deployment default.
`),
	}, resolver.approvePlan)
}

// registerRejectPlan wires the fishhawk_reject_plan tool.
func registerRejectPlan(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_reject_plan",
		Description: strings.TrimSpace(`
Reject a run's plan, failing the plan gate. Use this after reading
fishhawk_get_plan, once the plan is wrong and its plan stage is parked
awaiting approval — the reject counterpart to fishhawk_approve_plan.

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

// registerApproveDeploy wires the fishhawk_approve_deploy tool (E23.15
// / #1432). The deploy-gate counterpart to fishhawk_approve_plan:
// resolves the run's type=deploy stage and posts an approve decision
// via the existing /v0/stages/{id}/approvals endpoint. A deploy stage's
// gate is PRE-execution (ADR-038: its effect IS the side effect), so an
// approve here triggers the external pipeline.
//
// Auth: write tool requiring an operator token with write:deploy
// (ADR-038/#1390) — a runner-side fhm_* token surfaces 403.
func registerApproveDeploy(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_approve_deploy",
		Description: strings.TrimSpace(`
Approve a run's deploy stage at its pre-execution approval gate, triggering
the external deploy pipeline. Use this when a release run's deploy stage is
parked at awaiting_deploy_approval (the next_actions deploy arm points here) —
the deploy-gate counterpart to fishhawk_approve_plan, which fails on a
plan-less release run.

Takes a run id; the tool resolves the type=deploy stage internally by
listing the run's stages. The deploy gate is PRE-execution (ADR-038: a
deploy stage's effect is the side effect), so approving triggers the
external pipeline — a production deploy pages the human regardless of
runner kind.

Inputs:
  - run_id        — the run whose deploy stage to approve
  - environment   — REQUIRED; must be one of the deploy stage's
                    allowed_environments. Composed into the approval
                    comment as --environment=<env>, which the backend
                    deploy pre-flight parses
  - override_freeze — optional; appends --override-freeze so a deploy
                    during a spec-declared change_freeze is permitted
  - reason        — optional operator rationale

Auth: a write tool requiring an operator token with write:deploy
(ADR-038/#1390); a runner-side fhm_* token surfaces 403.

Common error shapes (surfaced as tool errors):
  - local validation — environment is required; an empty value fails
    before the HTTP hop
  - "no deploy stage" — the run has no deploy stage (a feature_change
    workflow, or a malformed run)
  - 422 deploy_environment_not_allowed — the environment is absent or
    not in allowed_environments
  - 422 deploy_change_freeze_active — the stage declares change_freeze
    and override_freeze was not set
  - 422 deploy_upstream_not_satisfied — a required_upstream pre-flight
    signal (ci_green / review_merged) is not satisfied
  - 403 — the caller's token lacks write:deploy

Duplicate labeling (#986, NOT an error — a 200): a re-submission by the
same subject is a no-op carrying duplicate_submission=true and
prior_decision; never read a duplicate as an effective approval.
`),
	}, resolver.approveDeploy)
}

// registerRejectDeploy wires the fishhawk_reject_deploy tool (E23.15 /
// #1432). The reject counterpart to fishhawk_approve_deploy. A deploy
// reject routes through the backend advanceStage path, NOT the
// approve-only deploy pre-flight, so it needs neither write:deploy nor
// an environment.
func registerRejectDeploy(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_reject_deploy",
		Description: strings.TrimSpace(`
Reject a run's deploy stage, failing the deploy gate. Use this when a
release run's deploy stage is parked at awaiting_deploy_approval and the
deploy should NOT proceed — the reject counterpart to
fishhawk_approve_deploy.

Takes a run id; the tool resolves the type=deploy stage internally.
Unlike approve, a deploy reject routes through the backend advanceStage
path (not the pre-execution deploy pre-flight block), so it needs neither
write:deploy scope nor an environment.

The reason is recorded on the approval row as 'comment'. Reason is
optional but recommended. Same resolver + "no deploy stage" error shape
as fishhawk_approve_deploy.
`),
	}, resolver.rejectDeploy)
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
	// Resolve the operator's real GitHub login best-effort (#751) so
	// the issue-thread status footer `@`-mentions the right user
	// instead of the raw token subject. A missing/unauthed gh yields a
	// warning on the tool result and an empty login — never a blocked
	// approval.
	login, warn := resolveApproverGithubLogin()
	updated, err := r.api.SubmitApproval(ctx, stageID, "approve", in.Reason, login, in.AddScopeFiles, in.BindingAssertions, in.ImplementModel)
	if err != nil {
		// ADR-036 (#875): the backend refuses the approve while a
		// configured agent plan review is still in-flight. Surface this
		// as a typed, operator-actionable poll-until-landed message
		// rather than a generic wrap. Do NOT auto-retry here — the
		// operator loop polls fishhawk_get_plan / fishhawk_await_review
		// and re-invokes this tool once the review reaches a terminal
		// state.
		var ae *apiError
		if errors.As(err, &ae) && ae.Code == "agent_review_pending" {
			landed, _ := ae.Details["landed_terminal"].(float64)
			configured, configuredOK := ae.Details["configured_agents"].(float64)
			// Lead with the landed/configured counts when the backend
			// supplied them; degrade to a count-free message when the
			// details are absent or malformed so we never print a
			// misleading "0 of 0 landed". The poll-until-landed guidance
			// is identical in both branches.
			countPhrase := "the agent plan review is still in-flight"
			if configuredOK && configured > 0 {
				countPhrase = fmt.Sprintf("%d of %d configured plan reviews have landed; the agent plan review is still in-flight",
					int(landed), int(configured))
			}
			return nil, ApprovePlanOutput{}, fmt.Errorf(
				"agent_review_pending: %s. Poll fishhawk_get_plan or fishhawk_await_review until the plan review reaches a terminal state (complete/skipped/failed), then retry fishhawk_approve_plan",
				countPhrase)
		}
		return nil, ApprovePlanOutput{}, fmt.Errorf("submit approval: %w", err)
	}
	return approvalSubmitResult(updated, warn), ApprovePlanOutput{
		Stage:               updated.Stage,
		StageID:             updated.ID,
		DuplicateSubmission: updated.DuplicateSubmission,
		PriorDecision:       updated.PriorDecision,
	}, nil
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
	// Resolve the operator's real GitHub login best-effort (#751); see
	// approvePlan for the rationale. Empty on gh failure, never fatal.
	login, warn := resolveApproverGithubLogin()
	updated, err := r.api.SubmitApproval(ctx, stageID, "reject", in.Reason, login, nil, nil, "")
	if err != nil {
		return nil, RejectPlanOutput{}, fmt.Errorf("submit approval: %w", err)
	}
	return approvalSubmitResult(updated, warn), RejectPlanOutput{
		Stage:               updated.Stage,
		StageID:             updated.ID,
		DuplicateSubmission: updated.DuplicateSubmission,
		PriorDecision:       updated.PriorDecision,
	}, nil
}

// commentHasStandaloneToken reports whether tok appears as a
// whitespace-delimited standalone token in s. It mirrors the backend's
// commentHasFlag parse (server/approvals.go) so the MCP-side smuggling
// guard treats a token exactly as the deploy pre-flight will — an
// embedded substring ("see --override-freeze-docs") is NOT a match.
func commentHasStandaloneToken(s, tok string) bool {
	for _, f := range strings.Fields(s) {
		if f == tok {
			return true
		}
	}
	return false
}

// approveDeploy is the fishhawk_approve_deploy tool handler (E23.15 /
// #1432). Resolves the deploy stage, composes the deploy environment
// (and optional freeze override) into the approval comment the backend
// pre-flight parses, and posts an approve decision.
func (r *runResolver) approveDeploy(ctx context.Context, _ *mcp.CallToolRequest, in ApproveDeployInput) (*mcp.CallToolResult, ApproveDeployOutput, error) {
	// Fail fast locally on the missing-environment case so the operator
	// gets a clean tool error rather than a backend 422 round-trip. The
	// backend re-validates (the comment may omit --environment for any
	// reason), so this is a convenience, not the authority.
	env := strings.TrimSpace(in.Environment)
	if env == "" {
		return nil, ApproveDeployOutput{}, fmt.Errorf("environment is required for a deploy approval — pass one of the deploy stage's allowed_environments (it is composed into the approval comment as --environment=<env>, which the backend deploy pre-flight parses)")
	}
	// Guard against flag smuggling (#1432 review). The backend deploy
	// pre-flight parses whitespace-delimited tokens from the WHOLE comment
	// (parseEnvironmentFlag / commentHasFlag), so an untrusted Environment
	// or Reason carrying a standalone --override-freeze token would bypass
	// the explicit OverrideFreeze control. An environment name is a single
	// token: reject embedded whitespace outright (this also stops e.g.
	// "production --override-freeze"). Reason is free-form but must not
	// introduce the freeze-override flag the operator did not request, so
	// --override-freeze appears in the comment ONLY when OverrideFreeze set.
	if len(strings.Fields(env)) != 1 {
		return nil, ApproveDeployOutput{}, fmt.Errorf("environment %q must be a single whitespace-free token — it is composed verbatim into the approval comment as --environment=<env>, and embedded whitespace could smuggle a flag token (e.g. --override-freeze) past the deploy pre-flight", in.Environment)
	}
	reason := strings.TrimSpace(in.Reason)
	if !in.OverrideFreeze && commentHasStandaloneToken(reason, "--override-freeze") {
		return nil, ApproveDeployOutput{}, fmt.Errorf("reason must not contain a standalone --override-freeze token unless override_freeze is set — pass override_freeze:true to override an active change freeze; the backend treats it as an explicit flag wherever it appears in the comment")
	}
	deployStage, err := r.resolveDeployStage(ctx, in.RunID)
	if err != nil {
		return nil, ApproveDeployOutput{}, err
	}
	stageID, err := uuid.Parse(deployStage.ID)
	if err != nil {
		return nil, ApproveDeployOutput{}, fmt.Errorf("resolved deploy stage has invalid id %q: %w", deployStage.ID, err)
	}
	// Compose the comment the backend deploy pre-flight parses: the
	// --environment=<env> token (parseEnvironmentFlag) plus an optional
	// standalone --override-freeze token (commentHasFlag), then the
	// trimmed operator rationale. Order is deterministic so the flag
	// tokens are always whitespace-delimited at the head.
	comment := "--environment=" + env
	if in.OverrideFreeze {
		comment += " --override-freeze"
	}
	if reason != "" {
		comment += " " + reason
	}
	// Resolve the operator's real GitHub login best-effort (#751); see
	// approvePlan. Empty on gh failure, never fatal.
	login, warn := resolveApproverGithubLogin()
	updated, err := r.api.SubmitApproval(ctx, stageID, "approve", comment, login, nil, nil, "")
	if err != nil {
		// The deploy pre-flight 422s (deploy_environment_not_allowed,
		// deploy_change_freeze_active, deploy_upstream_not_satisfied) and the
		// write:deploy 403 already carry operator-readable code+message via
		// apiError.Error(); surface them with a stable wrap so the operator
		// loop can match on the code.
		return nil, ApproveDeployOutput{}, fmt.Errorf("submit deploy approval: %w", err)
	}
	return approvalSubmitResult(updated, warn), ApproveDeployOutput{
		Stage:               updated.Stage,
		StageID:             updated.ID,
		DuplicateSubmission: updated.DuplicateSubmission,
		PriorDecision:       updated.PriorDecision,
	}, nil
}

// rejectDeploy is the fishhawk_reject_deploy tool handler (E23.15 /
// #1432). Mirrors rejectPlan: a deploy reject routes through the
// backend advanceStage path, so it needs no environment and no
// write:deploy scope.
func (r *runResolver) rejectDeploy(ctx context.Context, _ *mcp.CallToolRequest, in RejectDeployInput) (*mcp.CallToolResult, RejectDeployOutput, error) {
	deployStage, err := r.resolveDeployStage(ctx, in.RunID)
	if err != nil {
		return nil, RejectDeployOutput{}, err
	}
	stageID, err := uuid.Parse(deployStage.ID)
	if err != nil {
		return nil, RejectDeployOutput{}, fmt.Errorf("resolved deploy stage has invalid id %q: %w", deployStage.ID, err)
	}
	login, warn := resolveApproverGithubLogin()
	updated, err := r.api.SubmitApproval(ctx, stageID, "reject", in.Reason, login, nil, nil, "")
	if err != nil {
		return nil, RejectDeployOutput{}, fmt.Errorf("submit deploy rejection: %w", err)
	}
	return approvalSubmitResult(updated, warn), RejectDeployOutput{
		Stage:               updated.Stage,
		StageID:             updated.ID,
		DuplicateSubmission: updated.DuplicateSubmission,
		PriorDecision:       updated.PriorDecision,
	}, nil
}

// resolveApproverGithubLogin wraps resolveGitHubLoginViaGh for the
// approve/reject tools (#751). It is strictly best-effort: any gh
// failure (binary missing, unauthed, error) yields an empty login and
// a human-readable warning string for the tool result, never an error
// that would block the approval. The empty-login case degrades to the
// notifier's "an approver" rendering rather than an incorrect mention.
func resolveApproverGithubLogin() (login, warning string) {
	resolved, err := resolveGitHubLoginViaGh()
	switch {
	case err == nil:
		return resolved, ""
	case errors.Is(err, ErrGhNotInstalled):
		return "", "gh CLI not on PATH; recording the approval without a resolved GitHub login. The issue-thread status will read \"an approver\" instead of an @-mention."
	default:
		return "", fmt.Sprintf("could not resolve GitHub login via gh (%v); recording the approval without one — the issue-thread status will read \"an approver\".", err)
	}
}

// approvalSubmitResult composes the tool result for an approve/reject
// submission. A duplicate submission (#986) LEADS with an explicit
// no-op banner — the operator loop must never mistake it for an
// effective approval — followed by the approver-login warning when one
// applies (mirroring startRun's warnings-on-the-result pattern so the
// operator sees the degradation without it failing the call). Nil when
// there is nothing to report.
func approvalSubmitResult(res *approvalResult, warning string) *mcp.CallToolResult {
	var content []mcp.Content
	if res.DuplicateSubmission {
		content = append(content, &mcp.TextContent{Text: fmt.Sprintf(
			"duplicate submission — your prior %s decision (submitted %s) stands; stage state unchanged; budget/scope gates were NOT re-run and no transition or audit entry occurred",
			res.PriorDecision, res.PriorSubmittedAt)})
	}
	if warning != "" {
		content = append(content, &mcp.TextContent{Text: warning})
	}
	if len(content) == 0 {
		return nil
	}
	return &mcp.CallToolResult{Content: content}
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

// resolveDeployStage walks the run's stages and returns the one with
// type=deploy. The deploy analogue of resolvePlanStage, shared by
// fishhawk_approve_deploy and fishhawk_reject_deploy (E23.15 / #1432)
// so both surface a run id (not a stage id) and discover the deploy
// stage server-side.
//
// Returns a typed error for the missing-deploy-stage case so a
// plan-only run (a feature_change with no deploy stage) surfaces an
// operator-readable message rather than a generic not-found. Local
// UUID parse on the input is a fast-path that catches obvious typos
// before the HTTP hop.
func (r *runResolver) resolveDeployStage(ctx context.Context, runIDStr string) (*Stage, error) {
	runID, err := uuid.Parse(runIDStr)
	if err != nil {
		return nil, fmt.Errorf("run_id %q is not a valid UUID: %w", runIDStr, err)
	}
	stages, err := r.api.ListRunStages(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list run stages: %w", err)
	}
	for i := range stages {
		if stages[i].Type == "deploy" {
			return &stages[i], nil
		}
	}
	return nil, fmt.Errorf("no deploy stage on run %s; this run's workflow may not have a deploy stage", runIDStr)
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
	// IncludeIssueContext opts each returned run's full issue_context
	// (issue body + every comment) back into the response. Omitted by
	// default because it can overflow the tool-result token cap on
	// issues with large bodies/comment threads — a single list_runs over
	// such issues errors and forces a curl fallback to enumerate run IDs.
	IncludeIssueContext bool `json:"include_issue_context,omitempty" jsonschema:"include each run's full issue_context (issue body + all comments) in the response; omitted by default because it can overflow the tool-result token cap on issues with large bodies/comment threads. Set true only when the issue payload is actually needed"`
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
List runs with optional filters — the "what runs do I have" enumeration.
Use this to find runs by repo / workflow / state when you don't already
have a specific run in context; to resolve a single run from a PR or
trigger ref instead, use fishhawk_get_active_run.

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
  - include_issue_context — include each run's full issue_context
                  (issue body + all comments) in the response;
                  omitted by default. Set true only when the issue
                  payload is actually needed

Response: items[] (Run shape) + next_cursor (empty when the page
exhausts the result set). Each run's issue_context (issue body + all
comments) is omitted by default to keep the enumeration within the
tool-result token budget — a single list over issues with large
bodies/comment threads would otherwise overflow the cap. Set
include_issue_context=true to re-include it when the payload is needed.
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
	// Compact by default: strip each run's heavy issue_context (issue
	// body + every comment) unless the caller opted in. Run.IssueContext
	// carries json:"issue_context,omitempty" (client.go), so a nil
	// pointer drops the field from the marshalled output entirely rather
	// than emitting "issue_context":null — keeping the enumeration within
	// the tool-result token budget on fan-out driving (#1098).
	if !in.IncludeIssueContext {
		for i := range page.Items {
			page.Items[i].IssueContext = nil
		}
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
Fetch runtime calibration statistics for Fishhawk implement stages. Use
this BEFORE writing a plan (the run_stage plan step) to self-correct
predicted_runtime_minutes using historical actual vs. predicted data.
The key fields:

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
