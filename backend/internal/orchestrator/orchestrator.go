// Package orchestrator owns the "after stage N succeeds, dispatch
// stage N+1" loop. The webhook dispatcher (PR #121) creates the
// Run + every Stage row up front but only fires workflow_dispatch
// for the first stage. Subsequent stages stay in `pending` until
// this orchestrator advances them — typically called from the
// approval handler after a gate passes.
//
// Advance is idempotent at every step: if the next pending stage
// has already been transitioned to dispatched (by a redelivered
// approval, by a parallel orchestrator call), the underlying
// state-machine accepts the same-state re-application as a no-op.
//
// Today the orchestrator only fires workflow_dispatch for agent
// stages. Human stages transition to awaiting_approval directly so
// the next approval can come in. v0.x adds notification fan-out
// for human stages (Slack ping, GitHub assignment, etc.).
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// GitHubAPI is the slice of githubclient.Client the orchestrator
// uses. Extracting an interface lets tests substitute a stub.
type GitHubAPI interface {
	DispatchWorkflow(ctx context.Context, installationID int64,
		repo githubclient.RepoRef, workflowFile, ref string,
		inputs githubclient.DispatchInputs) error
	// EnableAutoMerge queues a PR for auto-merge once branch
	// protection clears (#255 / ADR-017). Used by routine_change-
	// style workflows whose review stage is a check-only gate.
	EnableAutoMerge(ctx context.Context, installationID int64,
		repo githubclient.RepoRef, prNumber int,
		method githubclient.MergeMethod) error
	// CreatePullRequest opens the single consolidated PR for a
	// decomposed parent run (#714 / ADR-032) once all children have
	// pushed to the shared branch. Returns ErrPullRequestExists when a
	// PR already exists for head/base (lost double-open race).
	CreatePullRequest(ctx context.Context, installationID int64,
		repo githubclient.RepoRef, head, base, title, body string) (*githubclient.PullRequest, error)
	// ListOpenPullRequestsByHead recovers the existing open PR for a
	// head branch — used to resolve the URL after CreatePullRequest
	// returns ErrPullRequestExists (#714).
	ListOpenPullRequestsByHead(ctx context.Context, installationID int64,
		repo githubclient.RepoRef, headBranch, base string) ([]githubclient.PullRequest, error)
}

// Orchestrator wires the run repository to a GitHub client to
// advance a run's stages. Construct directly via the public fields;
// every dependency is required (the orchestrator no-ops if any is
// nil, but production callers should always wire all four).
type Orchestrator struct {
	Runs   run.Repository
	GitHub GitHubAPI
	Logger *slog.Logger

	// Artifacts is the artifact repository the fanout helper reads to
	// load the approved plan. When nil, decomposition detection is
	// disabled and Advance falls through to the legacy single-implement
	// path even on plans that declare sub_plans.
	Artifacts artifact.Repository

	// Audit emits plan_decomposed entries when the orchestrator mints
	// child runs. Nil-safe; a nil Audit means the fanout still happens
	// but the audit is dropped (logged at warn).
	Audit audit.Repository

	// DefaultRef is the git ref to dispatch against. Matches the
	// webhook dispatcher's default; v0.x persists the ref on the
	// run row so subsequent dispatches can target the same ref.
	DefaultRef string

	// ActionsWorkflowFile is the customer-side .github/workflows/
	// file. Defaults to "fishhawk.yml" at use time.
	ActionsWorkflowFile string
}

// Outcome describes what Advance did. Useful for telemetry and
// for callers (the approval handler) that want to react to
// "run completed" vs "next stage dispatched."
type Outcome string

// Outcome values.
const (
	OutcomeDispatched   Outcome = "dispatched"    // a next stage was dispatched
	OutcomeRunCompleted Outcome = "run_completed" // no more stages; run transitioned to succeeded
	OutcomeDecomposed   Outcome = "decomposed"    // parent's implement stage parked in awaiting_children; N child runs minted
	OutcomeNoOp         Outcome = "noop"          // run not in a state that accepts advancement
)

// Advance looks at the run's stages and transitions the next
// pending one to dispatched, firing workflow_dispatch when the
// stage is agent-driven. If no pending stage remains, the run
// itself transitions to succeeded.
//
// Errors from the underlying repos surface as wrapped errors;
// callers usually log + acknowledge rather than retry. The
// orchestrator is intended to be called from a request handler,
// not from a background loop.
func (o *Orchestrator) Advance(ctx context.Context, runID uuid.UUID) (Outcome, error) {
	if o.Runs == nil {
		return OutcomeNoOp, errors.New("orchestrator: Runs is nil")
	}

	r, err := o.Runs.GetRun(ctx, runID)
	if err != nil {
		return OutcomeNoOp, fmt.Errorf("orchestrator: get run: %w", err)
	}
	if r.State.IsTerminal() {
		// Already done — nothing to advance.
		return OutcomeNoOp, nil
	}

	stages, err := o.Runs.ListStagesForRun(ctx, runID)
	if err != nil {
		return OutcomeNoOp, fmt.Errorf("orchestrator: list stages: %w", err)
	}

	// Walk pending → running before any stage transitions so the
	// terminal target (succeeded / failed) is reachable. The state
	// machine rejects pending → terminal directly; without this
	// step every run that completes its stages stays stuck in
	// pending. Idempotent on subsequent Advance calls (same-state
	// transitions are no-ops at the repo layer).
	if r.State == run.StatePending {
		updated, err := o.Runs.TransitionRun(ctx, r.ID, run.StateRunning)
		if err != nil {
			return OutcomeNoOp, fmt.Errorf("orchestrator: transition run to running: %w", err)
		}
		r = updated
	}

	// Find the next pending stage in sequence order. The
	// repository returns stages ordered by sequence ascending, so
	// we walk and pick the first non-terminal pending one.
	//
	// If we hit a failed or cancelled stage before finding a
	// pending one, the run is over — completing as failed (or
	// cancelled) is correct, and dispatching downstream stages
	// would be wrong because the upstream output they depended on
	// never landed.
	var next *run.Stage
	for _, s := range stages {
		if s.State == run.StageStateFailed || s.State == run.StageStateCancelled {
			return o.completeRun(ctx, r, stages)
		}
		if s.State == run.StageStatePending {
			next = s
			break
		}
	}

	if next == nil {
		// Every stage has terminated successfully. completeRun
		// transitions the run to succeeded.
		return o.completeRun(ctx, r, stages)
	}

	// ADR-025 D4: when the approved plan declares decomposition,
	// fan the parent's implement stage out into child runs rather
	// than dispatching it. Only checked when the next pending stage
	// is the implement type and Artifacts is wired — the legacy
	// path runs unchanged otherwise. Children themselves never
	// fanout (their plans are scoped narrow enough that they fit
	// the budget) — we skip the check when this run is a child.
	if next.Type == run.StageTypeImplement && o.Artifacts != nil && r.DecomposedFrom == nil {
		decomposed, err := o.fanoutIfDecomposed(ctx, r, stages, next)
		if err != nil {
			return OutcomeNoOp, fmt.Errorf("orchestrator: fanout: %w", err)
		}
		if decomposed {
			return OutcomeDecomposed, nil
		}
	}

	// ADR-032 (#714): a decomposed parent reaching its review gate opens
	// ONE consolidated PR for the whole decomposition. The children
	// pushed their commits onto the shared branch and opened no PR of
	// their own, so the parent run carries no pull_request_url and the
	// merge reconciler would never resolve its review. Stamp the URL here
	// — BEFORE dispatchStage — so the review dispatches with the PR
	// present and reconciles on the consolidated PR's merge. This gate
	// covers BOTH settle paths because the sweeper's resolveParent and
	// maybeAdvanceDecomposedParent both finish by calling Advance.
	if next.Type == run.StageTypeReview && r.DecomposedFrom == nil &&
		(r.PullRequestURL == nil || *r.PullRequestURL == "") {
		updated, err := o.maybeOpenConsolidatedPR(ctx, r, next)
		if err != nil {
			return OutcomeNoOp, fmt.Errorf("orchestrator: open consolidated pr: %w", err)
		}
		r = updated
	}

	return o.dispatchStage(ctx, r, next)
}

// reconcileStuckRunsPageSize bounds each ListRuns page the startup
// recovery sweep walks. Small constant — at v0 scale the running-run
// count is tiny; this only bounds memory if it ever grows.
const reconcileStuckRunsPageSize = 100

// ReconcileStuckRuns is the one-time startup recovery for runs stuck in
// the {all stages terminal, run non-terminal} class (#727): the
// merge-resolution path used to transition the review stage without
// completing the run, leaving runs like 0c50834a / e3316c14 in
// {review succeeded, run running} forever. It pages every running run,
// and for any whose stages are ALL terminal calls Advance — which routes
// through completeRun and resolves the run to succeeded/failed/cancelled
// per the existing stage scan.
//
// Skips any run with a non-terminal stage so a genuinely in-flight run is
// never force-completed. Idempotent: an already-terminal run is a
// completeRun no-op, and a second pass finds nothing to advance. Returns
// the count advanced. Reuses existing repo methods only (no new query).
//
// Best-effort PER RUN: a failure on one run (stage-scan or Advance error)
// is logged and skipped so it cannot block recovery of the others — a
// single unresolvable run never wedges the whole boot sweep. Only a
// systemic ListRuns paging failure aborts (and is returned).
func (o *Orchestrator) ReconcileStuckRuns(ctx context.Context) (int, error) {
	if o.Runs == nil {
		return 0, errors.New("orchestrator: Runs is nil")
	}

	advanced := 0
	failed := 0
	offset := 0
	for {
		runs, err := o.Runs.ListRuns(ctx, run.ListRunsFilter{
			State:  string(run.StateRunning),
			Limit:  reconcileStuckRunsPageSize,
			Offset: offset,
		})
		if err != nil {
			// A paging failure is systemic (not specific to one run), so
			// abort the sweep — best-effort applies per-run, not to the
			// listing itself.
			return advanced, fmt.Errorf("orchestrator: reconcile list runs: %w", err)
		}
		if len(runs) == 0 {
			break
		}
		for _, r := range runs {
			// Best-effort PER RUN: a failure on one run (e.g. its record
			// was partially cleaned up and Advance hits ErrNotFound) must
			// not block recovery of the others. Log and continue rather
			// than aborting the whole boot sweep (#727).
			stuck, err := o.runStagesAllTerminal(ctx, r.ID)
			if err != nil {
				failed++
				o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator reconcile: skipped run on stage-scan error",
					slog.String("run_id", r.ID.String()),
					slog.String("error", err.Error()),
				)
				continue
			}
			if !stuck {
				continue
			}
			if _, err := o.Advance(ctx, r.ID); err != nil {
				failed++
				o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator reconcile: skipped run on advance error",
					slog.String("run_id", r.ID.String()),
					slog.String("error", err.Error()),
				)
				continue
			}
			advanced++
			o.logger().LogAttrs(ctx, slog.LevelInfo, "orchestrator reconciled stuck run",
				slog.String("run_id", r.ID.String()),
			)
		}
		if len(runs) < reconcileStuckRunsPageSize {
			break
		}
		offset += len(runs)
	}

	o.logger().LogAttrs(ctx, slog.LevelInfo, "orchestrator stuck-run reconciliation complete",
		slog.Int("advanced", advanced),
		slog.Int("failed", failed),
	)
	return advanced, nil
}

// runStagesAllTerminal reports whether a run has at least one stage and
// EVERY stage is terminal. A run with no stages, or any non-terminal
// stage, returns false — the gate ReconcileStuckRuns uses to avoid
// force-completing a genuinely in-flight run.
func (o *Orchestrator) runStagesAllTerminal(ctx context.Context, runID uuid.UUID) (bool, error) {
	stages, err := o.Runs.ListStagesForRun(ctx, runID)
	if err != nil {
		return false, fmt.Errorf("orchestrator: reconcile list stages for run %s: %w", runID, err)
	}
	if len(stages) == 0 {
		return false, nil
	}
	for _, s := range stages {
		if !s.State.IsTerminal() {
			return false, nil
		}
	}
	return true, nil
}

// maybeOpenConsolidatedPR opens the single consolidated PR for a
// decomposed parent run reaching its review gate (#714 / ADR-032) and
// stamps pull_request_url on the run, returning the (reloaded) run so
// the in-flight Advance dispatches the review with the URL present.
//
// Idempotency is load-bearing: the periodic child-completion sweeper and
// the event-driven maybeAdvanceDecomposedParent can both call Advance on
// the parent near-simultaneously, and create-PR is not idempotent. The
// empty-URL re-read (b) shrinks the window; the ErrPullRequestExists
// recovery (e) makes a lost race benign by resolving the already-open PR.
//
// Graceful-skip (returns the run unchanged, nil error) when: the run has
// no decomposed children (an ordinary PR-less run, never a parent), the
// URL is already set, or GitHub/installation isn't wired (CLI/dev
// posture — same as fireDispatch/enableAutoMerge; the parent stays
// PR-less, narrowing rather than regressing prior behavior). A genuine
// GitHub error surfaces so the next Advance retries (the awaiting_children
// stage is already succeeded, so the retry re-enters this gate).
func (o *Orchestrator) maybeOpenConsolidatedPR(ctx context.Context, r *run.Run, reviewStage *run.Stage) (*run.Run, error) {
	// (a) Only decomposed parents. A run with zero children is an
	// ordinary PR-less / commit-yourself run — never open a PR for it.
	children, err := o.Runs.ListRuns(ctx, run.ListRunsFilter{
		DecomposedFrom: &r.ID,
		Limit:          1,
	})
	if err != nil {
		return r, fmt.Errorf("list decomposed children: %w", err)
	}
	if len(children) == 0 {
		return r, nil
	}

	// (b) Re-read immediately before the create to shrink the double-
	// open window between the two settle paths.
	fresh, err := o.Runs.GetRun(ctx, r.ID)
	if err != nil {
		return r, fmt.Errorf("reload run: %w", err)
	}
	if fresh.PullRequestURL != nil && *fresh.PullRequestURL != "" {
		return fresh, nil
	}
	r = fresh

	// (d) Graceful-skip when GitHub can't be reached (no client / no
	// installation). The parent stays PR-less — same posture as
	// fireDispatch.
	if o.GitHub == nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: GitHub not configured; skipping consolidated PR",
			slog.String("run_id", r.ID.String()))
		return r, nil
	}
	if r.InstallationID == nil || *r.InstallationID == 0 {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: run has no installation_id; skipping consolidated PR",
			slog.String("run_id", r.ID.String()))
		return r, nil
	}

	repo, err := parseRepo(r.Repo)
	if err != nil {
		return r, fmt.Errorf("parse repo %q: %w", r.Repo, err)
	}

	head := consolidatedBranch(r.ID)
	base := o.DefaultRef
	if base == "" {
		base = "main"
	}
	title, body := consolidatedPRTitleBody(r)

	// (e) Open the PR; recover the existing one on a lost race.
	var prURL string
	pr, err := o.GitHub.CreatePullRequest(ctx, *r.InstallationID, repo, head, base, title, body)
	switch {
	case err == nil:
		prURL = pr.HTMLURL
	case errors.Is(err, githubclient.ErrPullRequestExists):
		existing, lerr := o.GitHub.ListOpenPullRequestsByHead(ctx, *r.InstallationID, repo, head, base)
		if lerr != nil {
			return r, fmt.Errorf("recover existing pr for head %q: %w", head, lerr)
		}
		if len(existing) == 0 {
			return r, fmt.Errorf("pr already exists for head %q but none returned by list", head)
		}
		prURL = existing[0].HTMLURL
	default:
		return r, fmt.Errorf("create consolidated pr: %w", err)
	}
	if prURL == "" {
		return r, fmt.Errorf("consolidated pr opened but URL is empty")
	}

	// (f) Stamp the URL, reload so the in-flight Advance dispatches the
	// review with it present, and emit a best-effort audit entry.
	updated, err := o.Runs.SetRunPullRequestURL(ctx, r.ID, prURL)
	if err != nil {
		return r, fmt.Errorf("set pull_request_url: %w", err)
	}
	o.emitConsolidatedPROpened(ctx, r.ID, reviewStage.ID, prURL)
	o.logger().LogAttrs(ctx, slog.LevelInfo, "orchestrator opened consolidated PR",
		slog.String("run_id", r.ID.String()),
		slog.String("head", head),
		slog.String("base", base),
		slog.String("pull_request_url", prURL),
	)
	return updated, nil
}

// shortRunID returns the first 8 characters of a run UUID's string
// form. It MUST stay in sync with the runner's shortID helper
// (runner/cmd/fishhawk-runner/main.go): the runner names the shared
// branch the decomposed children push to as
// "fishhawk/run-<shortID(parentRunID)>", and the orchestrator names that
// same ref as the consolidated PR's head. A divergence would orphan the
// children's commits from the PR. TestConsolidatedBranch_MatchesRunner
// asserts the exact string for a known UUID so a drift fails the unit
// suite, not only the Docker e2e.
func shortRunID(id uuid.UUID) string {
	s := id.String()
	if len(s) < 8 {
		return s
	}
	return s[:8]
}

// consolidatedBranch is the shared-branch name the decomposed children
// pushed to and the consolidated PR's head. See shortRunID for the
// runner-side cross-reference.
func consolidatedBranch(parentID uuid.UUID) string {
	return "fishhawk/run-" + shortRunID(parentID)
}

// consolidatedPRTitleBody derives the PR title + body from the parent
// run's cached issue context. Falls back to a run-id-stamped title when
// no issue context is present (webhook runs that left it nil are fetched
// by the runner; this is the defensive default).
func consolidatedPRTitleBody(r *run.Run) (string, string) {
	if r.IssueContext != nil && r.IssueContext.Title != "" {
		body := fmt.Sprintf("Consolidated changes for decomposed run %s.", r.ID)
		if r.IssueContext.Number > 0 {
			body += fmt.Sprintf("\n\nCloses #%d", r.IssueContext.Number)
		}
		return r.IssueContext.Title, body
	}
	return fmt.Sprintf("Fishhawk decomposition %s", shortRunID(r.ID)),
		fmt.Sprintf("Consolidated changes for decomposed run %s.", r.ID)
}

// emitConsolidatedPROpened writes a consolidated_pr_opened audit entry
// (system actor) when the orchestrator opens the parent's PR (#714).
// Best-effort, mirroring emitChildrenSettled: nil-Audit guard,
// WARN-on-error, never unwinds the settle.
func (o *Orchestrator) emitConsolidatedPROpened(ctx context.Context, runID, stageID uuid.UUID, prURL string) {
	if o.Audit == nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: Audit not configured; skipping consolidated_pr_opened entry",
			slog.String("run_id", runID.String()))
		return
	}
	payload, err := json.Marshal(map[string]any{"pull_request_url": prURL})
	if err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: marshal consolidated_pr_opened payload failed",
			slog.String("error", err.Error()))
		return
	}
	systemKind := audit.ActorSystem
	if _, err := o.Audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  "consolidated_pr_opened",
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: append consolidated_pr_opened failed",
			slog.String("error", err.Error()))
	}
}

// fanoutIfDecomposed inspects the run's approved plan for a
// decomposition.sub_plans block. When present, it mints one child
// run per sub_plan (inheriting parent's workflow + trigger +
// installation context) and parks the parent's implement stage in
// awaiting_children. Returns true when fanout happened, false when
// the plan had no decomposition (caller falls through to dispatch).
//
// Each child carries:
//   - parent_run_id = parent.ID (CI-retry-chain semantic; preserved
//     for compatibility with existing retry walkers).
//   - decomposed_from = parent.ID (NEW; disambiguates "decomposition
//     child" from "CI-retry follow-up").
//   - issue_context derived from the parent's issue title + the
//     sub_plan's scope_hint as the body, so the child's plan stage
//     gets the narrowed context.
func (o *Orchestrator) fanoutIfDecomposed(ctx context.Context, parent *run.Run, stages []*run.Stage, parentImplement *run.Stage) (bool, error) {
	approvedPlan, planStageID, err := o.loadApprovedPlan(ctx, stages)
	if err != nil {
		return false, fmt.Errorf("load approved plan: %w", err)
	}
	if approvedPlan == nil || approvedPlan.Decomposition == nil || len(approvedPlan.Decomposition.SubPlans) == 0 {
		return false, nil
	}

	childIDs := make([]string, 0, len(approvedPlan.Decomposition.SubPlans))
	for _, sub := range approvedPlan.Decomposition.SubPlans {
		parentID := parent.ID
		childCtx := childIssueContextFromSubPlan(parent, sub)
		child, err := o.Runs.CreateRun(ctx, run.CreateRunParams{
			Repo:           parent.Repo,
			WorkflowID:     parent.WorkflowID,
			WorkflowSHA:    parent.WorkflowSHA,
			TriggerSource:  parent.TriggerSource,
			TriggerRef:     parent.TriggerRef,
			InstallationID: parent.InstallationID,
			ParentRunID:    &parentID,
			DecomposedFrom: &parentID,
			RunnerKind:     parent.RunnerKind,
			IssueContext:   childCtx,
			// Inherit the parent's cached workflow spec so the child's
			// implement-stage prompt resolves the policy max_stage_runtime
			// (30m for feature_change) instead of the runner's 15m default.
			// Without this the decomposition budget is defeated: oversized
			// plans split to fit 30m, then each child times out at 15m.
			WorkflowSpec: parent.WorkflowSpec,
		})
		if err != nil {
			return false, fmt.Errorf("create child run for sub_plan %q: %w", sub.Title, err)
		}
		// Each child gets a single implement stage. We skip plan +
		// review because the parent's plan is the contract (the
		// child's prompt builder walks parent_run_id to load it),
		// and review on a sub-PR is the parent's review concern.
		// Mirror the parent implement stage's executor so the child
		// dispatches to the same agent runtime.
		childImpl, err := o.Runs.CreateStage(ctx, run.CreateStageParams{
			RunID:        child.ID,
			Sequence:     0,
			Type:         run.StageTypeImplement,
			ExecutorKind: parentImplement.ExecutorKind,
			ExecutorRef:  parentImplement.ExecutorRef,
		})
		if err != nil {
			return false, fmt.Errorf("create child implement stage for sub_plan %q: %w", sub.Title, err)
		}
		childIDs = append(childIDs, child.ID.String())
		o.logger().LogAttrs(ctx, slog.LevelInfo, "orchestrator minted decomposed child run",
			slog.String("parent_run_id", parent.ID.String()),
			slog.String("child_run_id", child.ID.String()),
			slog.String("child_implement_stage_id", childImpl.ID.String()),
			slog.String("sub_plan_title", sub.Title),
		)
	}

	// Park the parent's implement stage in awaiting_children. The
	// child-completion sweeper transitions it to succeeded/failed
	// once every child has reached a terminal run state.
	if _, err := o.Runs.TransitionStage(ctx, parentImplement.ID, run.StageStateAwaitingChildren, nil); err != nil {
		return false, fmt.Errorf("transition parent implement to awaiting_children: %w", err)
	}

	o.emitPlanDecomposed(ctx, parent.ID, planStageID, childIDs, approvedPlan.Decomposition.Rationale)
	o.logger().LogAttrs(ctx, slog.LevelInfo, "orchestrator parent parked awaiting children",
		slog.String("parent_run_id", parent.ID.String()),
		slog.String("parent_stage_id", parentImplement.ID.String()),
		slog.Int("child_count", len(childIDs)),
	)
	return true, nil
}

// loadApprovedPlan returns the parsed standard_v1 plan from the
// run's most recent plan-stage artifact, plus the plan stage's ID
// (used as the audit anchor when fanout fires). (nil, _, nil) means
// no plan artifact is available — caller falls through to dispatch.
func (o *Orchestrator) loadApprovedPlan(ctx context.Context, stages []*run.Stage) (*plan.Plan, uuid.UUID, error) {
	var planStageID uuid.UUID
	for _, s := range stages {
		if s.Type == run.StageTypePlan {
			planStageID = s.ID
			break
		}
	}
	if planStageID == uuid.Nil {
		return nil, uuid.Nil, nil
	}

	arts, err := o.Artifacts.ListForStage(ctx, planStageID)
	if err != nil {
		return nil, planStageID, fmt.Errorf("list plan artifacts: %w", err)
	}
	var picked *artifact.Artifact
	for _, a := range arts {
		if a.Kind != artifact.KindPlan {
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
		return nil, planStageID, nil
	}
	var p plan.Plan
	if err := json.Unmarshal(picked.Content, &p); err != nil {
		return nil, planStageID, fmt.Errorf("decode plan artifact %s: %w", picked.ID, err)
	}
	return &p, planStageID, nil
}

// childIssueContextFromSubPlan derives a child run's issue context
// from the parent run + sub_plan. The parent's title is reused (so
// the child's plan stage knows what feature it belongs to); the body
// is replaced with the sub_plan's title + scope_hint so the agent's
// narrowed context surfaces in the planning prompt.
func childIssueContextFromSubPlan(parent *run.Run, sub plan.SubPlanSummary) *run.IssueContext {
	if parent.IssueContext == nil {
		return &run.IssueContext{
			Title: sub.Title,
			Body:  sub.ScopeHint,
		}
	}
	out := *parent.IssueContext
	out.Body = fmt.Sprintf("## %s\n\n%s\n\n---\n*Decomposed sub-plan of [%s](%s).*",
		sub.Title, sub.ScopeHint, parent.IssueContext.Title, parent.IssueContext.URL)
	return &out
}

// emitPlanDecomposed writes a plan_decomposed audit entry naming
// the child run IDs and the rationale string. Best-effort: a failure
// here logs and returns; the fanout has already taken effect at the
// data layer.
func (o *Orchestrator) emitPlanDecomposed(ctx context.Context, parentRunID, parentStageID uuid.UUID, childIDs []string, rationale string) {
	if o.Audit == nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: Audit not configured; skipping plan_decomposed entry",
			slog.String("parent_run_id", parentRunID.String()))
		return
	}
	payload, err := json.Marshal(map[string]any{
		"child_run_ids":   childIDs,
		"rationale":       rationale,
		"parent_stage_id": parentStageID.String(),
	})
	if err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: marshal plan_decomposed payload failed",
			slog.String("error", err.Error()))
		return
	}
	systemKind := audit.ActorSystem
	if _, err := o.Audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     parentRunID,
		StageID:   &parentStageID,
		Timestamp: time.Now().UTC(),
		Category:  "plan_decomposed",
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: append plan_decomposed failed",
			slog.String("error", err.Error()))
	}
}

// completeRun transitions the run to a terminal state when every
// stage has terminated. Same-state re-application is fine —
// state-machine treats it as idempotent.
func (o *Orchestrator) completeRun(ctx context.Context, r *run.Run, stages []*run.Stage) (Outcome, error) {
	target := run.StateSucceeded
	for _, s := range stages {
		if s.State == run.StageStateFailed {
			target = run.StateFailed
			break
		}
		if s.State == run.StageStateCancelled {
			target = run.StateCancelled
		}
	}
	if _, err := o.Runs.TransitionRun(ctx, r.ID, target); err != nil {
		return OutcomeNoOp, fmt.Errorf("orchestrator: transition run to %s: %w", target, err)
	}
	o.logger().LogAttrs(ctx, slog.LevelInfo, "orchestrator run completed",
		slog.String("run_id", r.ID.String()),
		slog.String("state", string(target)),
	)
	if r.DecomposedFrom != nil {
		o.maybeAdvanceDecomposedParent(ctx, *r.DecomposedFrom)
	}
	return OutcomeRunCompleted, nil
}

// maybeAdvanceDecomposedParent is called after a child run reaches a
// terminal state. When all siblings are also terminal, it transitions
// the parent's awaiting_children stage (to succeeded or failed-C) and
// calls Advance so the next parent stage — typically the review gate —
// is dispatched inline rather than waiting for the periodic sweeper.
// Best-effort: failures are logged at WARN and never surface to callers.
func (o *Orchestrator) maybeAdvanceDecomposedParent(ctx context.Context, parentRunID uuid.UUID) {
	children, err := o.Runs.ListRuns(ctx, run.ListRunsFilter{
		DecomposedFrom: &parentRunID,
		Limit:          100,
	})
	if err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: list decomposed children failed",
			slog.String("parent_run_id", parentRunID.String()),
			slog.String("error", err.Error()),
		)
		return
	}
	if len(children) == 0 {
		return
	}
	var failedChildren []*run.Run
	for _, c := range children {
		if !c.State.IsTerminal() {
			return
		}
		if c.State == run.StateFailed {
			failedChildren = append(failedChildren, c)
		}
	}

	stages, err := o.Runs.ListStagesForRun(ctx, parentRunID)
	if err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: list parent stages for children_settled failed",
			slog.String("parent_run_id", parentRunID.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	var awaitingStage *run.Stage
	for _, s := range stages {
		if s.State == run.StageStateAwaitingChildren {
			awaitingStage = s
			break
		}
	}
	if awaitingStage == nil {
		return
	}

	// #698: when children failed but EVERY failed child's implement
	// failure is retryable (A/C/D-timeout), park the parent in
	// awaiting_children rather than resolving it to failed-C. This
	// closes the race where a near-instant event-driven resolution
	// would terminate the parent before an operator can re-drive the
	// recoverable child. The parent stays parked until a re-drive
	// re-runs the child and this path fires again on its next
	// terminal transition. Only a non-retryable failed child (genuine
	// category B) resolves the parent to failed-C.
	if len(failedChildren) > 0 && o.failedChildrenAllRetryable(ctx, failedChildren) {
		o.emitParentAwaitingRedrive(ctx, parentRunID, awaitingStage.ID, failedChildren)
		o.logger().LogAttrs(ctx, slog.LevelInfo, "orchestrator parked parent awaiting re-drive",
			slog.String("parent_run_id", parentRunID.String()),
			slog.String("parent_stage_id", awaitingStage.ID.String()),
			slog.Int("failed_child_count", len(failedChildren)),
		)
		return
	}

	anyFailed := len(failedChildren) > 0
	target := run.StageStateSucceeded
	var completion *run.StageCompletion
	if anyFailed {
		target = run.StageStateFailed
		cat := run.FailureC
		reason := "one or more decomposed child runs failed"
		completion = &run.StageCompletion{
			FailureCategory: &cat,
			FailureReason:   &reason,
		}
	}

	if _, err := o.Runs.TransitionStage(ctx, awaitingStage.ID, target, completion); err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: transition awaiting_children stage failed",
			slog.String("parent_run_id", parentRunID.String()),
			slog.String("stage_id", awaitingStage.ID.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	o.emitChildrenSettled(ctx, parentRunID, awaitingStage.ID, anyFailed)

	if _, err := o.Advance(ctx, parentRunID); err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: advance parent after children settled failed",
			slog.String("parent_run_id", parentRunID.String()),
			slog.String("error", err.Error()),
		)
	}
}

// failedChildrenAllRetryable reports whether every failed child run's
// implement-stage failure is in a retryable category (A/C/D-timeout).
// Used by maybeAdvanceDecomposedParent to decide whether to park the
// parent awaiting re-drive. A failed child whose stages can't be
// listed, or whose implement stage carries no failure category, is
// treated as NOT retryable — parking is only safe when every failure
// is positively confirmed recoverable, so an unclassifiable child
// resolves the parent rather than parking it indefinitely.
func (o *Orchestrator) failedChildrenAllRetryable(ctx context.Context, failed []*run.Run) bool {
	for _, c := range failed {
		stages, err := o.Runs.ListStagesForRun(ctx, c.ID)
		if err != nil {
			o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: list child stages for retryability check failed",
				slog.String("child_run_id", c.ID.String()),
				slog.String("error", err.Error()),
			)
			return false
		}
		if !run.ImplementFailureRetryable(stages) {
			return false
		}
	}
	return true
}

// emitParentAwaitingRedrive writes a parent_awaiting_redrive audit
// entry (system actor) when a parent is parked because every failed
// child is retryable. It is the one-time, operator-discoverable
// signal that the parent needs a re-drive; the parked state is
// otherwise silent. Best-effort: a failure here logs and returns.
func (o *Orchestrator) emitParentAwaitingRedrive(ctx context.Context, parentRunID, stageID uuid.UUID, failed []*run.Run) {
	if o.Audit == nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: Audit not configured; skipping parent_awaiting_redrive entry",
			slog.String("parent_run_id", parentRunID.String()))
		return
	}
	ids := make([]string, 0, len(failed))
	for _, c := range failed {
		ids = append(ids, c.ID.String())
	}
	payload, err := json.Marshal(map[string]any{
		"parent_stage_id":         stageID.String(),
		"retryable_child_run_ids": ids,
	})
	if err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: marshal parent_awaiting_redrive payload failed",
			slog.String("error", err.Error()))
		return
	}
	systemKind := audit.ActorSystem
	if _, err := o.Audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     parentRunID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  "parent_awaiting_redrive",
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: append parent_awaiting_redrive failed",
			slog.String("error", err.Error()))
	}
}

// emitChildrenSettled writes a children_settled audit entry once all
// decomposed children have reached terminal states. Best-effort: a
// failure here logs and returns; the stage transition has already landed.
func (o *Orchestrator) emitChildrenSettled(ctx context.Context, parentRunID, stageID uuid.UUID, anyFailed bool) {
	if o.Audit == nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: Audit not configured; skipping children_settled entry",
			slog.String("parent_run_id", parentRunID.String()))
		return
	}
	outcome := "succeeded"
	if anyFailed {
		outcome = "failed"
	}
	payload, err := json.Marshal(map[string]any{"outcome": outcome})
	if err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: marshal children_settled payload failed",
			slog.String("error", err.Error()))
		return
	}
	systemKind := audit.ActorSystem
	if _, err := o.Audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     parentRunID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  "children_settled",
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: append children_settled failed",
			slog.String("error", err.Error()))
	}
}

// dispatchStage transitions the next stage to dispatched and (for
// agent stages) fires workflow_dispatch. Human stages transition
// to awaiting_approval directly — there's no runner to wake up.
// Auto-merge stages (review with a check-only gate, ADR-017 / #255)
// take a third path: queue gh pr merge --auto and transition
// straight to succeeded — there's no runner work to do, and GitHub
// owns the merge gate.
func (o *Orchestrator) dispatchStage(ctx context.Context, r *run.Run, next *run.Stage) (Outcome, error) {
	if isAutoMergeStage(next) {
		return o.dispatchAutoMergeStage(ctx, r, next)
	}
	if next.ExecutorKind == run.ExecutorHuman {
		// Human stages don't need workflow_dispatch — they go to
		// awaiting_approval and wait for someone to click. Walk
		// pending → dispatched → running → awaiting_approval
		// because the state machine doesn't allow a direct
		// pending → awaiting_approval transition. Each step is
		// idempotent at the state machine, so a redelivered
		// approval that lands here twice doesn't fault.
		for _, to := range []run.StageState{
			run.StageStateDispatched,
			run.StageStateRunning,
			run.StageStateAwaitingApproval,
		} {
			if _, err := o.Runs.TransitionStage(ctx, next.ID, to, nil); err != nil {
				return OutcomeNoOp, fmt.Errorf("orchestrator: transition human stage to %s: %w", to, err)
			}
		}
		o.logger().LogAttrs(ctx, slog.LevelInfo, "orchestrator advanced to human stage",
			slog.String("run_id", r.ID.String()),
			slog.String("stage_id", next.ID.String()),
			slog.Int("sequence", next.Sequence),
		)
		return OutcomeDispatched, nil
	}

	// Agent stage: transition to dispatched, then fire workflow_dispatch.
	if _, err := o.Runs.TransitionStage(ctx, next.ID, run.StageStateDispatched, nil); err != nil {
		return OutcomeNoOp, fmt.Errorf("orchestrator: transition stage to dispatched: %w", err)
	}

	if err := o.fireDispatch(ctx, r, next); err != nil {
		// We've already moved the stage forward; failing here
		// leaves it in dispatched without an actual GitHub
		// trigger. Surface the error so the caller can audit it,
		// but don't roll back — the runner CAN be triggered
		// manually if needed, and a fresh Advance call will hit
		// the same-state idempotency path.
		return OutcomeDispatched, err
	}

	o.logger().LogAttrs(ctx, slog.LevelInfo, "orchestrator dispatched next stage",
		slog.String("run_id", r.ID.String()),
		slog.String("stage_id", next.ID.String()),
		slog.Int("sequence", next.Sequence),
		slog.String("executor", string(next.ExecutorKind)),
	)
	return OutcomeDispatched, nil
}

// isAutoMergeStage returns true when a stage's role is "queue auto-
// merge and step out of the way" — review stages with a check-only
// gate (#255 / ADR-017). The routine_change workflow is the
// canonical case: agent implements, GitHub branch protection +
// auto-merge handle the rest. The dispatcher persists the gate's
// kind on the stage row at create time (#213); we read that here
// rather than re-parsing the spec.
//
// Returns false for stages with no gate (implement) and for
// approval-typed gates (feature_change review). Falls open for
// pre-#213 rows that don't have a persisted gate — they fall
// through to the standard agent / human dispatch paths.
func isAutoMergeStage(s *run.Stage) bool {
	return s.Type == run.StageTypeReview && s.Gate != nil && s.Gate.Kind == run.GateKindCheck
}

// dispatchAutoMergeStage queues GitHub auto-merge and transitions
// the stage to succeeded (#255 / ADR-017). The merge happens later
// when branch protection clears; Fishhawk's run is logically done
// once auto-merge is enqueued. State machine walk: pending →
// dispatched → running → succeeded.
//
// Best-effort on the GitHub side: a failure to enable auto-merge
// (e.g., the customer hasn't enabled the feature on the repo,
// branch protection is misconfigured, the PR's already merged
// synchronously) leaves the stage in dispatched and surfaces the
// error to the caller. The stage doesn't fail — re-running Advance
// retries the auto-merge enable, and an operator can flip the PR
// manually.
func (o *Orchestrator) dispatchAutoMergeStage(ctx context.Context, r *run.Run, next *run.Stage) (Outcome, error) {
	if _, err := o.Runs.TransitionStage(ctx, next.ID, run.StageStateDispatched, nil); err != nil {
		return OutcomeNoOp, fmt.Errorf("orchestrator: transition auto-merge stage to dispatched: %w", err)
	}

	if err := o.enableAutoMerge(ctx, r); err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn,
			"orchestrator: enable auto-merge failed",
			slog.String("run_id", r.ID.String()),
			slog.String("stage_id", next.ID.String()),
			slog.String("error", err.Error()),
		)
		return OutcomeDispatched, err
	}

	for _, to := range []run.StageState{
		run.StageStateRunning,
		run.StageStateSucceeded,
	} {
		if _, err := o.Runs.TransitionStage(ctx, next.ID, to, nil); err != nil {
			return OutcomeNoOp, fmt.Errorf("orchestrator: transition auto-merge stage to %s: %w", to, err)
		}
	}

	o.logger().LogAttrs(ctx, slog.LevelInfo, "orchestrator queued auto-merge",
		slog.String("run_id", r.ID.String()),
		slog.String("stage_id", next.ID.String()),
		slog.Int("sequence", next.Sequence),
	)
	return OutcomeDispatched, nil
}

// enableAutoMerge calls GitHub's enablePullRequestAutoMerge
// mutation via the client. Skips silently when the GitHub client
// is unwired (CLI runs, dev posture). Requires the run's
// pull_request_url to be backfilled — that happens when the
// implement stage's PR artifact lands (#216), which is upstream of
// the review stage.
func (o *Orchestrator) enableAutoMerge(ctx context.Context, r *run.Run) error {
	if o.GitHub == nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: GitHub not configured; skipping auto-merge",
			slog.String("run_id", r.ID.String()),
		)
		return nil
	}
	if r.InstallationID == nil || *r.InstallationID == 0 {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: run has no installation_id; skipping auto-merge",
			slog.String("run_id", r.ID.String()),
		)
		return nil
	}
	if r.PullRequestURL == nil || *r.PullRequestURL == "" {
		return errors.New("orchestrator: run missing pull_request_url; cannot enable auto-merge")
	}

	repo, err := parseRepo(r.Repo)
	if err != nil {
		return fmt.Errorf("orchestrator: parse repo %q: %w", r.Repo, err)
	}
	prNumber, err := pullRequestNumberFromURL(*r.PullRequestURL)
	if err != nil {
		return fmt.Errorf("orchestrator: parse pr url %q: %w", *r.PullRequestURL, err)
	}

	// Default to SQUASH for v0 — matches the typical PR merge
	// conventions on GitHub repos and is what Fishhawk's own
	// CLAUDE.md prescribes. Spec-level merge_method is a v0.x
	// follow-up.
	return o.GitHub.EnableAutoMerge(ctx, *r.InstallationID, repo, prNumber, githubclient.MergeMethodSquash)
}

// pullRequestNumberFromURL parses the trailing /pull/<n> segment
// from a GitHub PR URL. The runner stores the canonical
// `https://github.com/<owner>/<repo>/pull/<n>` form when it opens
// the PR (#206); this helper round-trips it back to <n>.
func pullRequestNumberFromURL(u string) (int, error) {
	const segment = "/pull/"
	idx := strings.LastIndex(u, segment)
	if idx < 0 {
		return 0, fmt.Errorf("missing %q segment", segment)
	}
	tail := u[idx+len(segment):]
	if i := strings.IndexAny(tail, "/?#"); i >= 0 {
		tail = tail[:i]
	}
	n, err := strconv.Atoi(tail)
	if err != nil {
		return 0, fmt.Errorf("parse pr number from %q: %w", tail, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("pr number must be > 0, got %d", n)
	}
	return n, nil
}

// fireDispatch builds a RepoRef + dispatch inputs and calls the
// GitHub client. Skips silently when GitHub or InstallationID
// isn't configured (e.g., trigger_source=cli runs).
func (o *Orchestrator) fireDispatch(ctx context.Context, r *run.Run, next *run.Stage) error {
	if o.GitHub == nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: GitHub not configured; skipping workflow_dispatch",
			slog.String("run_id", r.ID.String()),
		)
		return nil
	}
	if r.InstallationID == nil || *r.InstallationID == 0 {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: run has no installation_id; skipping workflow_dispatch",
			slog.String("run_id", r.ID.String()),
		)
		return nil
	}

	repo, err := parseRepo(r.Repo)
	if err != nil {
		return fmt.Errorf("orchestrator: parse repo %q: %w", r.Repo, err)
	}

	ref := o.DefaultRef
	if ref == "" {
		ref = "main"
	}
	actionsFile := o.ActionsWorkflowFile
	if actionsFile == "" {
		actionsFile = "fishhawk.yml"
	}

	return o.GitHub.DispatchWorkflow(ctx, *r.InstallationID, repo,
		actionsFile, ref, githubclient.DispatchInputs{
			"run_id":      r.ID.String(),
			"stage_id":    next.ID.String(),
			"workflow_id": r.WorkflowID,
			"stage":       next.ExecutorRef,
		})
}

func (o *Orchestrator) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.Default()
}

// parseRepo splits "owner/name" — duplicated from
// internal/webhook/dispatcher.go to keep the orchestrator package
// dependency-light. A shared helper is a v0.x cleanup.
func parseRepo(s string) (githubclient.RepoRef, error) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			if i == 0 || i == len(s)-1 {
				return githubclient.RepoRef{}, fmt.Errorf("malformed repo %q", s)
			}
			return githubclient.RepoRef{Owner: s[:i], Name: s[i+1:]}, nil
		}
	}
	return githubclient.RepoRef{}, fmt.Errorf("malformed repo %q", s)
}
