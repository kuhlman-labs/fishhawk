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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/budget"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
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
	// GetBranchSHA resolves a branch ref to its tip SHA, reporting
	// absence as (_, false, nil). Used by the fan-in step (ADR-041 /
	// #1142) to read the base ref and probe the consolidated branch.
	GetBranchSHA(ctx context.Context, installationID int64,
		repo githubclient.RepoRef, branch string) (string, bool, error)
	// CreateRef creates a branch ref at sha (idempotent on a 422
	// "already exists"). The fan-in step creates the consolidated branch
	// from the run's base ref when it does not yet exist (ADR-041 /
	// #1142 — under E24.1 nobody else creates it).
	CreateRef(ctx context.Context, installationID int64,
		repo githubclient.RepoRef, branch, sha string) error
	// MergeBranch performs a server-side merge of head into base,
	// returning the resulting merge commit SHA (empty on a 204 nothing-to-
	// merge) and ErrMergeConflict on a 409. The fan-in step merges each
	// succeeded slice branch onto the consolidated branch in slice order
	// (ADR-041 / #1142) and records each merge SHA in slices_integrated so
	// the ADR-035 lineage guard attributes the integration merges (#1459).
	MergeBranch(ctx context.Context, installationID int64,
		repo githubclient.RepoRef, base, head, commitMessage string) (string, error)
}

// ConsolidatedReviewDispatcher dispatches the gating agent implement
// review against a decomposed parent's consolidated PR diff (#1060) after
// Advance dispatches the parent's review stage. It is implemented
// server-side (*server.Server): the review machinery depends on the
// orchestrator, so calling it from here directly would close an import
// cycle. The dispatch is best-effort and idempotent — the implementation
// dedups on its own started-key and no-ops for non-decomposed runs — so
// the orchestrator fires it fire-and-forget.
type ConsolidatedReviewDispatcher interface {
	DispatchConsolidatedReview(ctx context.Context, parentRunID uuid.UUID, base, head string)
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

	// ConsolidatedReview, when wired, dispatches the gating consolidated
	// implement review for a decomposed parent run after Advance
	// dispatches the parent's review stage with the consolidated PR
	// present (#1060). Nil disables the dispatch — ordinary (non-
	// decomposed) runs and the CLI/dev posture are unaffected.
	ConsolidatedReview ConsolidatedReviewDispatcher

	// MaxParallelChildren is the global default cap on how many decomposed
	// child runs may dispatch concurrently (E24.6 / #1146), wired from
	// server.Config.MaxParallelChildren (FISHHAWKD_MAX_PARALLEL_CHILDREN).
	// fanoutIfDecomposed resolves the effective cap from the run's cached
	// workflow spec via spec.EffectiveMaxParallel(MaxParallelChildren) and
	// surfaces it (log + plan_decomposed payload). 0 = unlimited. This is
	// the cap RESOLUTION seam only; concurrency throttling that consumes
	// the resolved value lands in E24.3 (#1143) — all children are still
	// minted here.
	MaxParallelChildren int

	// Drive, when wired, emits the run_auto_advanced audit trail for the
	// decomposed-child dispatch (RuleChildrenDispatch, E24.3 / #1143) so
	// each concurrent child dispatch is attributable to a named rule.
	// Nil-safe: Engine.Record/Recorded guard a nil receiver, so an
	// unwired Drive disables the audit while DispatchDecomposedChildren
	// still dispatches the children (the dispatch is the shipped
	// behavior; the audit is pure observability).
	Drive *drive.Engine

	// ExternalURL is the operator-facing base URL used to render the
	// consolidated PR body's audit-log footer (#1774), mirroring the
	// runner's cfg.backendURL for the single-run implement PR. Wired from
	// server.Config.ExternalURL in newStageOrchestrator. Empty (the CLI/dev
	// posture) degrades the audit-log URL to a relative `/v0/runs/<id>/audit`,
	// matching the defensive nil-URL posture elsewhere in this file.
	ExternalURL string
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
	var gated *run.Stage
	for _, s := range stages {
		if s.State == run.StageStateFailed || s.State == run.StageStateCancelled {
			return o.completeRun(ctx, r, stages)
		}
		if s.State == run.StageStatePending {
			next = s
			break
		}
		if !s.State.IsTerminal() && gated == nil {
			// Non-terminal but not pending: dispatched, running,
			// awaiting_approval, or awaiting_children — an open gate or
			// in-flight stage.
			gated = s
		}
	}

	if next == nil {
		// #968: a run must never roll up succeeded while a gate is still
		// open or a stage is in flight. With nothing pending but a
		// non-terminal stage present (e.g. a review re-parked at
		// awaiting_approval), there is nothing to dispatch AND nothing to
		// complete — stay running at the gate.
		if gated != nil {
			o.logger().LogAttrs(ctx, slog.LevelInfo, "orchestrator advance no-op: stage non-terminal, nothing pending",
				slog.String("run_id", r.ID.String()),
				slog.String("stage_id", gated.ID.String()),
				slog.String("stage_state", string(gated.State)),
			)
			return OutcomeNoOp, nil
		}
		// Every stage has terminated successfully. completeRun
		// transitions the run to succeeded.
		return o.completeRun(ctx, r, stages)
	}

	// ADR-038 (#1384): a deploy stage's effect IS the side effect, so its
	// gate is PRE-execution. Park the pending deploy stage at its
	// pre-execution approval gate (pending → awaiting_deploy_approval) and
	// return WITHOUT dispatching — nothing ships until an operator approves
	// the deploy INTENT (checkDeployPreflight at the approval gate). This is
	// the inverse of the plan/implement/review stages, which dispatch first
	// and gate after. Return OutcomeDispatched (advanced to the gate,
	// consistent with the human-stage gate path). Idempotent: a re-entrant
	// Advance hits the same-state no-op (awaiting_deploy_approval is settled,
	// so the next Advance finds it as `gated`, not pending). Placed BEFORE
	// the decomposition / consolidated-PR / dispatch block so no
	// fireDispatch / workflow_dispatch can fire for a deploy stage.
	if next.Type == run.StageTypeDeploy {
		if _, err := o.Runs.TransitionStage(ctx, next.ID, run.StageStateAwaitingDeployApproval, nil); err != nil {
			return OutcomeNoOp, fmt.Errorf("orchestrator: park deploy stage at pre-execution gate: %w", err)
		}
		o.logger().LogAttrs(ctx, slog.LevelInfo, "orchestrator parked deploy stage at pre-execution approval gate",
			slog.String("run_id", r.ID.String()),
			slog.String("stage_id", next.ID.String()),
			slog.Int("sequence", next.Sequence),
		)
		return OutcomeDispatched, nil
	}

	// E38.3 (#1657): auto-terminate a degenerate acceptance stage. When the
	// approved plan declares verification.out_of_scope with ZERO
	// acceptance_criteria, the acceptance stage has no observable criterion to
	// validate — dispatching it would only stall the run waiting for an operator
	// to run a no-op stage (the operator deadlock this closes, the #1612
	// companion). Walk it straight to succeeded exactly like dispatchAutoMergeStage
	// does for auto-merge (the state machine forbids pending → succeeded directly),
	// emit an acceptance_skipped_out_of_scope marker so auditcomplete exempts it
	// from the trace-required rule, then re-enter Advance so the now-terminal
	// acceptance stage lets the run reach completeRun → succeeded in the SAME call.
	// Guarded by o.Artifacts != nil (a harness without the artifact repo takes the
	// unchanged dispatch path — decomposition children have no plan stage anyway,
	// so loadApprovedPlan returns nil for them and this never fires). Placed BEFORE
	// the decomposition / consolidated-PR / dispatch block so no workflow_dispatch
	// can fire for the skipped stage. A load error surfaces (do NOT silently
	// dispatch); a nil plan or a false predicate falls through to the unchanged
	// operator-dispatched acceptance path.
	//
	// #1728 (E41.5) rides the SAME approved-plan load with a second, disjoint
	// predicate: when the approved plan declares ZERO acceptance_criteria AND
	// ZERO out_of_scope, there is no observable criterion to validate and no
	// out_of_scope justification, so short-circuit the acceptance stage straight
	// to succeeded — but record a REAL passed verdict
	// (acceptance_outcome_recorded, verdict=passed, basis=empty-criteria) rather
	// than an acceptance_skipped_out_of_scope marker, so downstream next_actions
	// surfaces acceptance_passed and auto-close/merge proceed identically. The two
	// predicates are disjoint by construction (E38.3 requires out_of_scope>0; #1728
	// requires out_of_scope==0), so at most one fires.
	if next.Type == run.StageTypeAcceptance && o.Artifacts != nil {
		sc, _, err := o.tryShortCircuitAcceptanceCore(ctx, r, stages, next)
		if err != nil {
			return OutcomeNoOp, err
		}
		if sc != nil {
			// The stage settled to succeeded and the matching audit was
			// emitted; re-enter Advance so the now-terminal acceptance stage
			// lets the run reach completeRun → succeeded (or the next stage)
			// in the SAME call.
			return o.Advance(ctx, runID)
		}
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
	isParentReviewGate := next.Type == run.StageTypeReview && r.DecomposedFrom == nil
	if isParentReviewGate && (r.PullRequestURL == nil || *r.PullRequestURL == "") {
		updated, err := o.maybeOpenConsolidatedPR(ctx, r, next)
		if err != nil {
			return OutcomeNoOp, fmt.Errorf("orchestrator: open consolidated pr: %w", err)
		}
		r = updated
	}

	out, err := o.dispatchStage(ctx, r, next)

	// Acceptance dispatch audit (E31.6 / #1534, ADR-049): when an
	// acceptance-typed stage was successfully advanced (the agent fireDispatch
	// path OR the human awaiting_approval walk, both via dispatchStage), emit an
	// acceptance_dispatched entry so the living anchor renders the dispatch line
	// (E31.3 already registered the category + renderer). Unlike deploy,
	// acceptance rides the ordinary pending→dispatched agent path — no
	// pre-execution park branch — so this is placed AFTER a successful dispatch,
	// not before. Best-effort: emitAcceptanceDispatched never unwinds the
	// dispatch.
	if err == nil && next.Type == run.StageTypeAcceptance {
		o.emitAcceptanceDispatched(ctx, r.ID, next.ID, next.Sequence, string(next.ExecutorKind))
	}

	// ADR-032 / #1060: once the decomposed parent's review stage is
	// dispatched WITH the consolidated PR present, dispatch the gating
	// implement review against the whole consolidated diff (the diff that
	// actually merges) so a child-raised high gates the parent merge. The
	// trigger lives here — where the decomposed-parent + consolidated-PR
	// condition is already computed — but the dispatch itself is
	// server-side (import-cycle avoidance). Fire-and-forget after a
	// successful dispatch; the dispatcher no-ops for ordinary runs (a
	// DecomposedFrom==nil run with a PR but no children, e.g. a normal
	// feature run) and dedups its own re-fire. base/head match
	// maybeOpenConsolidatedPR exactly.
	if err == nil && isParentReviewGate && o.ConsolidatedReview != nil &&
		r.PullRequestURL != nil && *r.PullRequestURL != "" {
		base := o.DefaultRef
		if base == "" {
			base = "main"
		}
		o.ConsolidatedReview.DispatchConsolidatedReview(ctx, r.ID, base, consolidatedBranch(r.ID))
	}

	return out, err
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

	// Gather the inputs the aligned PR body (#1774) needs. Every load is
	// defensive: a failure logs at WARN and proceeds with a nil plan / empty
	// child ids so a missing artifact or child-list error NEVER fails the open.
	stages, serr := o.Runs.ListStagesForRun(ctx, r.ID)
	if serr != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: list stages for consolidated PR body failed; proceeding without implement stage id / plan",
			slog.String("run_id", r.ID.String()),
			slog.String("error", serr.Error()))
		stages = nil
	}
	implStageID := firstImplementStageID(stages)
	var approvedPlan *plan.Plan
	if o.Artifacts != nil {
		if p, _, perr := o.loadApprovedPlan(ctx, stages); perr != nil {
			o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: load approved plan for consolidated PR body failed; proceeding with nil plan",
				slog.String("run_id", r.ID.String()),
				slog.String("error", perr.Error()))
		} else {
			approvedPlan = p
		}
	}
	childIDs, cerr := o.listAllDecomposedChildren(ctx, r.ID)
	if cerr != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: list decomposed children for consolidated PR body failed; proceeding with empty child ids",
			slog.String("run_id", r.ID.String()),
			slog.String("error", cerr.Error()))
		childIDs = nil
	}
	// listAllDecomposedChildren returns created_at DESC (repository contract);
	// reverse to created_at ASC to approximate the children's mint order so the
	// per-slice bullet pairing lines up with the plan's sub_plans ordering.
	childRunIDs := childRunIDsInMintOrder(childIDs)

	title, body := consolidatedPRTitleBody(r, approvedPlan, implStageID, head, o.ExternalURL, childRunIDs)

	// (e) Open the PR; recover the existing one on a lost race.
	var prURL string
	var prNumber int
	pr, err := o.GitHub.CreatePullRequest(ctx, *r.InstallationID, repo, head, base, title, body)
	switch {
	case err == nil:
		prURL = pr.HTMLURL
		prNumber = pr.Number
	case errors.Is(err, githubclient.ErrPullRequestExists):
		existing, lerr := o.GitHub.ListOpenPullRequestsByHead(ctx, *r.InstallationID, repo, head, base)
		if lerr != nil {
			return r, fmt.Errorf("recover existing pr for head %q: %w", head, lerr)
		}
		if len(existing) == 0 {
			return r, fmt.Errorf("pr already exists for head %q but none returned by list", head)
		}
		prURL = existing[0].HTMLURL
		prNumber = existing[0].Number
	default:
		return r, fmt.Errorf("create consolidated pr: %w", err)
	}
	if prURL == "" {
		return r, fmt.Errorf("consolidated pr opened but URL is empty")
	}

	// (#1732) Record the kind=pull_request artifact on the parent implement
	// stage BEFORE stamping pull_request_url. The single-run flow records
	// that artifact in the runner-driven PR-upload handler, but the decomposed
	// parent's PR is opened here — so without this, audit_complete's Rule 3
	// (hasPullRequest over the implement stage) reports pr_missing on every
	// decomposed parent. Ordering matters: recording BEFORE the URL stamp means
	// the (b) "URL already set -> return early" short-circuit above can never
	// observe a URL-stamped-but-artifact-missing state. On an error, return it
	// (unwind) WITHOUT stamping the URL so the next Advance re-enters this gate
	// with the URL still empty, re-opens the PR idempotently via the
	// ErrPullRequestExists recovery, and records the artifact on retry.
	if err := o.recordConsolidatedPRArtifact(ctx, r, prURL, prNumber, head, base); err != nil {
		return r, fmt.Errorf("record consolidated pull_request artifact: %w", err)
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

// recordConsolidatedPRArtifact records the kind=pull_request artifact on the
// decomposed parent's IMPLEMENT stage when the orchestrator opens the
// consolidated PR (#1732). The single-run flow records this artifact in the
// runner-driven PR-upload handler (server/pullrequest.go), so audit_complete's
// Rule 3 (hasPullRequest over the implement stage) passes there. A decomposed
// parent's PR is opened by the orchestrator, which never recorded the artifact
// — leaving audit_complete permanently red (pr_missing) on an otherwise-healthy
// run. This helper closes that gap on every fan-in settle path, since both the
// event-driven sweeper and the on-demand /consolidate endpoint funnel through
// Advance -> maybeOpenConsolidatedPR.
//
// The recorded content mirrors the field shape the runner's pull_request
// artifact ships and the three downstream consumers decode: head_sha +
// pr_number (auditcheckpublisher.decodeHeadSHA readiness,
// webhook.decodeArtifactHeadSHA synchronize matching, auditcomplete.decodePRArtifact
// foreign-commit gather). An empty head_sha makes the publisher treat the run
// as "not ready to publish", so the consolidated branch tip is resolved via
// GetBranchSHA to carry a real SHA.
//
// Graceful skip (nil error) when o.Artifacts == nil, mirroring the GitHub/
// installation nil-skip posture in maybeOpenConsolidatedPR. IDEMPOTENT: there
// is NO unique (stage_id, kind) DB constraint, so a naive Create on a race or
// retry would duplicate — the helper ListForStage-guards and skips Create when
// a kind=pull_request artifact already exists. The ListForStage-then-Create
// guard is non-atomic; per-run Advance serialization makes that an accepted
// documented limitation.
func (o *Orchestrator) recordConsolidatedPRArtifact(ctx context.Context, r *run.Run, prURL string, prNumber int, head, base string) error {
	// Graceful skip: a harness without the artifact repo can't record the
	// artifact — same posture as maybeOpenConsolidatedPR's GitHub/installation
	// nil-skip. The parent stays artifact-less; narrowing, not regressing.
	if o.Artifacts == nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: Artifacts not configured; skipping consolidated pull_request artifact",
			slog.String("run_id", r.ID.String()))
		return nil
	}

	// Locate the parent IMPLEMENT stage — the same stage auditcomplete's Rule 3
	// checks (NOT the review stage maybeOpenConsolidatedPR receives).
	stages, err := o.Runs.ListStagesForRun(ctx, r.ID)
	if err != nil {
		return fmt.Errorf("list stages for consolidated pull_request artifact: %w", err)
	}
	implID := firstImplementStageID(stages)
	if implID == uuid.Nil {
		return fmt.Errorf("no implement stage on run %s to record consolidated pull_request artifact", r.ID)
	}

	// Idempotency: skip Create when a pull_request artifact already exists on
	// the implement stage (no unique (stage_id, kind) constraint exists, so a
	// naive Create on a race/retry would duplicate).
	existing, err := o.Artifacts.ListForStage(ctx, implID)
	if err != nil {
		return fmt.Errorf("list artifacts for implement stage %s: %w", implID, err)
	}
	for _, a := range existing {
		if a.Kind == artifact.KindPullRequest {
			return nil
		}
	}

	// Resolve the consolidated branch tip so the artifact carries a real
	// head_sha (empty head_sha makes the publisher treat the run as not ready
	// to publish). base_sha is resolved best-effort for a complete artifact;
	// only head_sha and pr_number are load-bearing for the consumers.
	repo, err := parseRepo(r.Repo)
	if err != nil {
		return fmt.Errorf("parse repo %q: %w", r.Repo, err)
	}
	headSHA, _, err := o.GitHub.GetBranchSHA(ctx, *r.InstallationID, repo, head)
	if err != nil {
		return fmt.Errorf("resolve consolidated head %q: %w", head, err)
	}
	// base_sha is best-effort per the doc comment: only head_sha and pr_number
	// are load-bearing for the consumers. A transient failure resolving the base
	// branch tip must NOT unwind the artifact record (and, per the ordering, the
	// URL stamp) — log it and leave base_sha empty rather than forcing a retry.
	baseSHA, _, berr := o.GitHub.GetBranchSHA(ctx, *r.InstallationID, repo, base)
	if berr != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: best-effort consolidated base_sha resolution failed; recording artifact without it",
			slog.String("run_id", r.ID.String()),
			slog.String("base", base),
			slog.Any("error", berr))
		baseSHA = ""
	}

	content, err := json.Marshal(map[string]any{
		"pr_number": prNumber,
		"pr_url":    prURL,
		"branch":    head,
		"head_sha":  headSHA,
		"base_sha":  baseSHA,
		"origin":    "orchestrator_consolidated",
	})
	if err != nil {
		return fmt.Errorf("marshal consolidated pull_request artifact: %w", err)
	}
	sum := sha256.Sum256(content)
	contentHash := hex.EncodeToString(sum[:])

	if _, err := o.Artifacts.Create(ctx, artifact.CreateParams{
		StageID:     implID,
		Kind:        artifact.KindPullRequest,
		Content:     content,
		ContentHash: contentHash,
		// SchemaVersion intentionally nil for v0 — mirrors the runner-driven
		// PR-upload handler.
	}); err != nil {
		return fmt.Errorf("create consolidated pull_request artifact: %w", err)
	}
	o.logger().LogAttrs(ctx, slog.LevelInfo, "orchestrator recorded consolidated pull_request artifact",
		slog.String("run_id", r.ID.String()),
		slog.String("stage_id", implID.String()),
		slog.String("head", head),
		slog.String("head_sha", headSHA),
		slog.Int("pr_number", prNumber),
	)
	return nil
}

// shortRunID returns the first 8 characters of a run UUID's string
// form. It MUST stay in sync with the runner's shortID helper
// (runner/cmd/fishhawk-runner/main.go): the runner names each decomposed
// child's sole-writer slice branch under
// "fishhawk/run-<shortID(parentRunID)>/slice-<n>", and the orchestrator's
// sliceBranch must produce the byte-identical name. A divergence would
// orphan the children's commits from the fan-in merge. The branch-name
// unit test asserts the exact strings for a known UUID so a drift fails
// the unit suite, not only the Docker e2e.
func shortRunID(id uuid.UUID) string {
	s := id.String()
	if len(s) < 8 {
		return s
	}
	return s[:8]
}

// runBranchPrefix is the stable per-run branch namespace
// "fishhawk/run-<short>". The slice branches nest UNDER it
// (runBranchPrefix+"/slice-<n>") while the consolidated branch is a
// NON-NESTING sibling (runBranchPrefix+"-consolidated"). These two MUST
// NOT nest: git stores refs as a filesystem-like hierarchy under
// .git/refs/heads, so a ref whose full path is a strict prefix of an
// existing ref's path (refs/heads/<prefix> vs refs/heads/<prefix>/slice-0)
// cannot be created — the directory/file (D/F) conflict that 422'd fan-in
// 100% in production (#1243). Keeping the consolidated name a sibling of,
// rather than a parent directory of, the slice refs eliminates it.
func runBranchPrefix(parentID uuid.UUID) string {
	return "fishhawk/run-" + shortRunID(parentID)
}

// consolidatedBranch is the consolidated branch and the consolidated PR's
// head: the branch each slice merges onto during fan-in. It is a
// non-nesting sibling of the slice branches (see runBranchPrefix for the
// D/F-conflict rationale) — the children push to their slice branches, NOT
// to this one.
func consolidatedBranch(parentID uuid.UUID) string {
	return runBranchPrefix(parentID) + "-consolidated"
}

// ConsolidatedBranch is the canonical, exported derivation of the
// consolidated PR head / decomposed-parent fix-up branch. It delegates to
// the single unexported consolidatedBranch formula so there is exactly one
// source of truth for the name. Out-of-package consumers (e.g.
// server.fixupBranchForRun) MUST call this rather than re-hardcoding the
// "fishhawk/run-<short>-consolidated" literal — a duplicated reconstruction
// silently diverged on the #1243 rename and orphaned parent fix-up commits
// (#1245).
func ConsolidatedBranch(parentID uuid.UUID) string {
	return consolidatedBranch(parentID)
}

// conventionalCommitHeaderRe matches a Conventional Commits v1.0.0 header
// (#1572): a lowercase type from the allowed set, an optional lowercase scope
// in parens, an optional breaking-change `!`, then `: ` and a non-empty
// description. Used by consolidatedPRTitleBody (#1774) to decide whether the
// issue title is already a conventional header (used verbatim) or needs the
// `chore: ` prefix.
//
// BYTE-IDENTICAL MIRROR of runner/cmd/fishhawk-runner/main.go's
// conventionalCommitHeaderRe. The runner is a separate Go module, so this
// cannot be imported and MUST stay in sync — the runner side carries a
// cross-reference comment pointing back here. The consolidatedPRTitleBody unit
// test asserts the shipped title shape, so a drift that changed the
// verbatim-vs-prefix decision fails the suite.
var conventionalCommitHeaderRe = regexp.MustCompile(`^(feat|fix|docs|refactor|test|chore|perf|build)(\([a-z0-9/._-]+\))?!?: .+$`)

// consolidatedPRTitleBody renders the decomposed parent's consolidated PR
// title + body (#1774) aligned with the single-run implement PR the runner
// produces in prTitleAndBody. The title is a Conventional Commits header (the
// issue title verbatim when it already matches, else `chore: <title>`, else a
// deterministic run-id-stamped fallback); the body carries `## Summary` (the
// approved-plan summary plus a per-slice bullet list from decomposition
// sub_plans), a `Closes #N` line for issue-triggered parents, and the SAME
// attribution footer literal the runner appends.
//
// Defensive by construction: nil/empty IssueContext, a nil plan, and a plan
// lacking a summary or sub_plan titles each still yield a valid, run-id-stamped
// PR — the open never fails on missing context. childIDs pairs positionally
// with p.Decomposition.SubPlans; the pairing is descriptive metadata only, so a
// length mismatch degrades to titles-only bullets rather than mispairing.
func consolidatedPRTitleBody(r *run.Run, p *plan.Plan, implStageID uuid.UUID, head, baseURL string, childIDs []uuid.UUID) (title, body string) {
	// Title: a Conventional Commits header. An issue title that already
	// matches the header shape is used verbatim (no double prefix); a
	// non-conventional title gets the `chore: ` prefix; no issue title falls
	// back to a deterministic run-id-stamped chore subject.
	switch {
	case r.IssueContext != nil && r.IssueContext.Title != "":
		if conventionalCommitHeaderRe.MatchString(r.IssueContext.Title) {
			title = r.IssueContext.Title
		} else {
			title = "chore: " + r.IssueContext.Title
		}
	default:
		title = fmt.Sprintf("chore: fishhawk consolidated run %s", shortRunID(r.ID))
	}

	var b strings.Builder
	b.WriteString("## Summary\n\n")
	if p != nil && p.Summary != "" {
		b.WriteString(p.Summary)
	} else {
		fmt.Fprintf(&b, "Consolidated changes for decomposed run %s.", r.ID)
	}

	// Per-slice bullet list from the approved plan's sub_plans. Pair each
	// bullet with its child run's short id ONLY when the counts line up
	// (positional pairing relies on ListRuns mint order — descriptive, not
	// load-bearing); a mismatch degrades to titles-only bullets.
	if p != nil && p.Decomposition != nil && len(p.Decomposition.SubPlans) > 0 {
		pairIDs := len(childIDs) == len(p.Decomposition.SubPlans)
		b.WriteString("\n")
		for i, sub := range p.Decomposition.SubPlans {
			b.WriteString("\n- ")
			b.WriteString(sub.Title)
			if pairIDs {
				fmt.Fprintf(&b, " (run %s)", shortRunID(childIDs[i]))
			}
		}
	}

	if r.IssueContext != nil && r.IssueContext.Number > 0 {
		fmt.Fprintf(&b, "\n\nCloses #%d", r.IssueContext.Number)
	}

	b.WriteString(consolidatedPRFooter(r.ID, implStageID, head, baseURL))
	return title, b.String()
}

// consolidatedPRFooter renders the Fishhawk attribution footer appended to the
// consolidated PR body (#1774).
//
// BYTE-IDENTICAL MIRROR of the agent-path footer literal in
// runner/cmd/fishhawk-runner/main.go's prTitleAndBody. The runner is a separate
// Go module, so this cannot be imported and MUST stay in sync — the runner side
// carries a cross-reference comment pointing back here, and the
// consolidatedPRTitleBody unit test asserts the exact footer shape so a drift
// fails the suite. baseURL is right-trimmed of a trailing slash exactly as the
// runner does; an empty baseURL renders a relative `/v0/runs/<id>/audit`.
func consolidatedPRFooter(runID, implStageID uuid.UUID, head, baseURL string) string {
	return fmt.Sprintf(
		"\n\n---\n_Opened by [Fishhawk](https://github.com/kuhlman-labs/fishhawk) for run `%s`, stage `%s`._\n_Branch: `%s` · Audit log: `%s/v0/runs/%s/audit`._\n",
		runID, implStageID, head,
		strings.TrimRight(baseURL, "/"), runID,
	)
}

// firstImplementStageID returns the id of the first implement-typed stage in
// the run's stage list, or uuid.Nil when there is none. It is the single
// derivation reused by both the consolidated-PR footer (#1774) and
// recordConsolidatedPRArtifact (#1732) so the implement stage they reference is
// always the same one.
func firstImplementStageID(stages []*run.Stage) uuid.UUID {
	for _, s := range stages {
		if s.Type == run.StageTypeImplement {
			return s.ID
		}
	}
	return uuid.Nil
}

// childRunIDsInMintOrder reverses the created_at-DESC child list
// (listAllDecomposedChildren's repository ordering) into created_at-ASC order,
// approximating the children's mint order so the consolidated PR body's
// per-slice bullets pair positionally with the plan's sub_plans (#1774). The
// pairing is descriptive metadata only — consolidatedPRTitleBody degrades to
// titles-only bullets on any count mismatch — so an imperfect approximation
// never mispairs load-bearing state.
func childRunIDsInMintOrder(children []*run.Run) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(children))
	for i := len(children) - 1; i >= 0; i-- {
		ids = append(ids, children[i].ID)
	}
	return ids
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

// emitAcceptanceDispatched writes an acceptance_dispatched audit entry
// (system actor) when the orchestrator dispatches an acceptance stage (E31.6 /
// #1534, ADR-049). Best-effort, mirroring emitConsolidatedPROpened: nil-Audit
// guard, WARN-on-error, never unwinds the dispatch. The rendered payload
// carries stage_id/sequence/executor so a downstream surface can correlate the
// dispatch with the later acceptance_outcome_recorded entry.
func (o *Orchestrator) emitAcceptanceDispatched(ctx context.Context, runID, stageID uuid.UUID, sequence int, executor string) {
	if o.Audit == nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: Audit not configured; skipping acceptance_dispatched entry",
			slog.String("run_id", runID.String()))
		return
	}
	payload, err := json.Marshal(map[string]any{
		"stage_id": stageID.String(),
		"sequence": sequence,
		"executor": executor,
	})
	if err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: marshal acceptance_dispatched payload failed",
			slog.String("error", err.Error()))
		return
	}
	systemKind := audit.ActorSystem
	if _, err := o.Audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  "acceptance_dispatched",
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: append acceptance_dispatched failed",
			slog.String("error", err.Error()))
	}
}

// emitAcceptanceSkippedOutOfScope writes an acceptance_skipped_out_of_scope
// audit entry (system actor) when the orchestrator auto-terminates a degenerate
// acceptance stage — an approved plan that declares verification.out_of_scope
// with zero acceptance_criteria (E38.3 / #1657). Best-effort, mirroring
// emitAcceptanceDispatched: nil-Audit guard, WARN-on-error, never unwinds the
// walk. The category literal is byte-identical to
// server.CategoryAcceptanceSkippedOutOfScope — auditcomplete reads it to exempt
// the skipped stage from the trace-required rule and the MCP next_actions surface
// reads it to label the succeeded_acceptance_skipped_out_of_scope state. Appended
// with AppendChained so Rule 4 chain-verify stays intact. The payload carries
// stage_id/sequence/out_of_scope_count and the skip reason.
func (o *Orchestrator) emitAcceptanceSkippedOutOfScope(ctx context.Context, runID, stageID uuid.UUID, sequence, outOfScopeCount int) {
	if o.Audit == nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: Audit not configured; skipping acceptance_skipped_out_of_scope entry",
			slog.String("run_id", runID.String()))
		return
	}
	payload, err := json.Marshal(map[string]any{
		"stage_id":           stageID.String(),
		"sequence":           sequence,
		"out_of_scope_count": outOfScopeCount,
		"reason":             "approved plan declares verification.out_of_scope with zero acceptance_criteria; acceptance stage auto-terminated with no observable-change validation",
	})
	if err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: marshal acceptance_skipped_out_of_scope payload failed",
			slog.String("error", err.Error()))
		return
	}
	systemKind := audit.ActorSystem
	if _, err := o.Audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  "acceptance_skipped_out_of_scope",
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: append acceptance_skipped_out_of_scope failed",
			slog.String("error", err.Error()))
	}
}

// emitAcceptanceOutcomeShortCircuit writes the acceptance_outcome_recorded
// verdict the orchestrator records when it short-circuits a no-surface
// acceptance stage — either an approved plan with ZERO acceptance_criteria AND
// ZERO out_of_scope (basis empty-criteria, #1728 / E41.5) or a plan whose every
// criterion is skip_expected-with-basis (basis all-skip-with-basis, #1748 /
// E41.6). The basis and the criteria total are parametrized: empty-criteria
// passes total=0 (no criterion existed); all-skip-with-basis passes total=N and
// records all N as skipped. Unlike emitAcceptanceSkippedOutOfScope (which
// records a skip MARKER), this records a real passed verdict so downstream
// next_actions surfaces acceptance_passed and auto-close/merge proceed
// identically to a validator-shipped pass. Best-effort, mirroring the other
// emit helpers: nil-Audit guard with a WARN log, WARN-on-error, never unwinds
// the walk.
//
// The category literal is byte-identical to server.CategoryAcceptanceOutcomeRecorded
// so the same audit shape the validator ships is what auditcomplete and the MCP
// next_actions classifier read. The payload mirrors the server's
// buildOutcomePayload verdict/outcome/criteria_* render contract (no criterion
// was actually validated — passed/failed stay zero) and additionally carries the
// basis field (plan.AcceptanceBasisKey → the passed basis). A normal
// server-recorded verdict never sets basis, so its presence cleanly marks this
// entry as the pre-spawn short-circuit — the discriminator auditcomplete reads
// to exempt the no-trace acceptance stage from the trace-required rule. Appended
// with AppendChained (ActorKind system) so Rule 4 chain-verify stays intact.
func (o *Orchestrator) emitAcceptanceOutcomeShortCircuit(ctx context.Context, runID, stageID uuid.UUID, sequence int, basis string, criteriaTotal int) {
	if o.Audit == nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: Audit not configured; skipping acceptance_outcome_recorded short-circuit entry",
			slog.String("run_id", runID.String()))
		return
	}
	reason := "approved plan declares zero acceptance_criteria and zero verification.out_of_scope; acceptance stage short-circuited to a passed verdict with no runner spawn and no preview"
	if basis == plan.AcceptanceBasisAllSkipWithBasis {
		reason = "approved plan's acceptance criteria are all skip_expected with an expectation_basis; acceptance stage short-circuited to a passed verdict with no runner spawn and no preview"
	}
	payload, err := json.Marshal(map[string]any{
		"stage_id":              stageID.String(),
		"sequence":              sequence,
		"verdict":               "passed",
		"outcome":               "accepted",
		"criteria_passed":       0,
		"criteria_failed":       0,
		"criteria_skipped":      criteriaTotal,
		"criteria_total":        criteriaTotal,
		plan.AcceptanceBasisKey: basis,
		"reason":                reason,
	})
	if err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: marshal acceptance_outcome_recorded short-circuit payload failed",
			slog.String("error", err.Error()))
		return
	}
	systemKind := audit.ActorSystem
	if _, err := o.Audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  "acceptance_outcome_recorded",
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: append acceptance_outcome_recorded short-circuit failed",
			slog.String("error", err.Error()))
	}
}

// AcceptanceShortCircuit describes a fired pre-spawn acceptance short-circuit
// (#1928). It is the return value of TryShortCircuitAcceptance and the inline
// Advance arm's shared core: a non-nil value means the acceptance stage was
// walked straight to succeeded with the matching audit emitted and NO runner
// dispatched. Kind names which of the three disjoint predicates fired; Basis is
// the acceptance_outcome_recorded basis value for the two verdict-recording
// predicates (empty for the out-of-scope skip, which records a marker rather
// than a verdict); CriteriaTotal is the acceptance-criteria count carried into
// the verdict payload.
type AcceptanceShortCircuit struct {
	Kind          string
	Basis         string
	CriteriaTotal int
}

// Acceptance short-circuit Kind values (#1928). Distinct from the plan-package
// Basis constants: Kind labels the predicate that fired, Basis is the audit
// payload discriminator.
const (
	AcceptanceShortCircuitOutOfScope       = "out_of_scope_skip"
	AcceptanceShortCircuitEmptyCriteria    = "empty_criteria"
	AcceptanceShortCircuitAllSkipWithBasis = "all_skip_with_basis"
)

// acceptanceStateWalkOrder is the ordered pending → succeeded state walk the
// short-circuit follows. The state machine forbids skipping straight to
// succeeded, and it also forbids re-issuing an already-passed transition, so
// walkAcceptanceToSucceededFrom starts from the stage's CURRENT state: Advance
// always finds the stage pending, but a local operator host dispatch may find it
// parked at 'awaiting_host_dispatch' (#1912, was the conflated 'dispatched'
// park pre-#1928) or, for a migration-missed legacy row, at 'dispatched'.
// awaiting_host_dispatch is inserted between pending and dispatched so a park
// walks awaiting_host_dispatch → dispatched → running → succeeded.
var acceptanceStateWalkOrder = []run.StageState{
	run.StageStatePending,
	run.StageStateAwaitingHostDispatch,
	run.StageStateDispatched,
	run.StageStateRunning,
	run.StageStateSucceeded,
}

// walkAcceptanceToSucceededFrom transitions the acceptance stage from its
// current state through the remaining suffix of acceptanceStateWalkOrder up to
// succeeded. Starting from pending walks dispatched → running → succeeded
// (byte-identical to the pre-#1928 inline walk); starting from dispatched walks
// running → succeeded. An unknown current state falls back to walking the full
// suffix after pending.
func (o *Orchestrator) walkAcceptanceToSucceededFrom(ctx context.Context, stageID uuid.UUID, current run.StageState, kind string) error {
	startIdx := 0
	for i, s := range acceptanceStateWalkOrder {
		if s == current {
			startIdx = i
			break
		}
	}
	for _, to := range acceptanceStateWalkOrder[startIdx+1:] {
		if _, err := o.Runs.TransitionStage(ctx, stageID, to, nil); err != nil {
			return fmt.Errorf("orchestrator: walk acceptance stage to %s for %s: %w", to, kind, err)
		}
	}
	return nil
}

// tryShortCircuitAcceptanceCore is the shared acceptance short-circuit arm
// (#1928): it evaluates the three disjoint approved-plan predicates
// (out-of-scope skip, empty-criteria, all-skip-with-basis), and on a hit walks
// the target acceptance stage straight to succeeded and emits the matching audit
// (the skip marker for out-of-scope, a passed verdict for the other two). It
// does NOT re-enter Advance — the caller owns that so the retry-path inline arm
// stays byte-identical. Returns a non-nil *AcceptanceShortCircuit on a hit, and
// (nil, _, nil) for a nil approved plan, a false predicate, a non-acceptance
// stage, or an un-wired Artifacts repo (no state change). Only o.Artifacts != nil
// and a non-nil approved plan reach the predicates, mirroring the pre-refactor
// guard.
//
// liveValidationRequired (E48.6 / #1953) is true ONLY when the target is an
// admissible acceptance stage, the approved plan loaded, and NONE of the three
// short-circuit predicates matched — i.e. at least one acceptance criterion
// needs a LIVE target the runner would validate against. It is false on every
// (nil, false, nil) no-op path (nil plan, non-acceptance stage, un-wired
// Artifacts) and false on every short-circuit hit (the stage settled server-
// side; no live target is needed). The admission endpoint reads it to report
// needs_target so a dispatch verb probes the target BEFORE spawning a runner
// doomed to fail category-C acceptance_target_unreachable.
func (o *Orchestrator) tryShortCircuitAcceptanceCore(ctx context.Context, r *run.Run, stages []*run.Stage, target *run.Stage) (*AcceptanceShortCircuit, bool, error) {
	if target.Type != run.StageTypeAcceptance || o.Artifacts == nil {
		return nil, false, nil
	}
	approvedPlan, _, err := o.loadApprovedPlan(ctx, stages)
	if err != nil {
		return nil, false, fmt.Errorf("orchestrator: load approved plan for acceptance skip: %w", err)
	}
	if approvedPlan == nil {
		return nil, false, nil
	}
	switch {
	case plan.AcceptanceSkippableOutOfScope(approvedPlan.Verification):
		if err := o.walkAcceptanceToSucceededFrom(ctx, target.ID, target.State, "out-of-scope skip"); err != nil {
			return nil, false, err
		}
		o.emitAcceptanceSkippedOutOfScope(ctx, r.ID, target.ID, target.Sequence, len(approvedPlan.Verification.OutOfScope))
		o.logger().LogAttrs(ctx, slog.LevelInfo, "orchestrator auto-terminated acceptance stage (out_of_scope, zero acceptance_criteria)",
			slog.String("run_id", r.ID.String()),
			slog.String("stage_id", target.ID.String()),
			slog.Int("sequence", target.Sequence),
			slog.Int("out_of_scope_count", len(approvedPlan.Verification.OutOfScope)),
		)
		return &AcceptanceShortCircuit{Kind: AcceptanceShortCircuitOutOfScope, CriteriaTotal: 0}, false, nil
	case plan.AcceptanceSkippableEmptyCriteria(approvedPlan.Verification):
		if err := o.walkAcceptanceToSucceededFrom(ctx, target.ID, target.State, "empty-criteria short-circuit"); err != nil {
			return nil, false, err
		}
		o.emitAcceptanceOutcomeShortCircuit(ctx, r.ID, target.ID, target.Sequence, plan.AcceptanceBasisEmptyCriteria, 0)
		o.logger().LogAttrs(ctx, slog.LevelInfo, "orchestrator short-circuited acceptance stage (zero acceptance_criteria, zero out_of_scope) to a passed verdict",
			slog.String("run_id", r.ID.String()),
			slog.String("stage_id", target.ID.String()),
			slog.Int("sequence", target.Sequence),
			slog.String("basis", plan.AcceptanceBasisEmptyCriteria),
		)
		return &AcceptanceShortCircuit{Kind: AcceptanceShortCircuitEmptyCriteria, Basis: plan.AcceptanceBasisEmptyCriteria, CriteriaTotal: 0}, false, nil
	case plan.AcceptanceSkippableAllSkipWithBasis(approvedPlan.Verification):
		if err := o.walkAcceptanceToSucceededFrom(ctx, target.ID, target.State, "all-skip-with-basis short-circuit"); err != nil {
			return nil, false, err
		}
		total := len(approvedPlan.Verification.AcceptanceCriteria)
		o.emitAcceptanceOutcomeShortCircuit(ctx, r.ID, target.ID, target.Sequence, plan.AcceptanceBasisAllSkipWithBasis, total)
		o.logger().LogAttrs(ctx, slog.LevelInfo, "orchestrator short-circuited acceptance stage (every acceptance criterion skip_expected with basis) to a passed verdict",
			slog.String("run_id", r.ID.String()),
			slog.String("stage_id", target.ID.String()),
			slog.Int("sequence", target.Sequence),
			slog.Int("criteria_total", total),
			slog.String("basis", plan.AcceptanceBasisAllSkipWithBasis),
		)
		return &AcceptanceShortCircuit{Kind: AcceptanceShortCircuitAllSkipWithBasis, Basis: plan.AcceptanceBasisAllSkipWithBasis, CriteriaTotal: total}, false, nil
	}
	// The plan loaded and no short-circuit predicate matched: at least one
	// acceptance criterion needs a live target. No state change here — the
	// admission endpoint reports needs_target and the dispatch verb probes.
	return nil, true, nil
}

// TryShortCircuitAcceptance is the exported entry point (#1928) the acceptance-
// admission endpoint calls BEFORE a local host dispatch spawns a runner. It
// evaluates the same three short-circuit predicates the Advance arm does, but
// keyed on an explicit (runID, stageID) rather than the earliest pending stage,
// so a run whose every acceptance criterion is skip_expected-with-basis (or
// whose plan is out-of-scope / empty-criteria) settles server-side WITHOUT
// spawning a runner that would fail category-C acceptance_target_unreachable.
//
// The target must be an acceptance stage in a dispatch-admissible state (pending,
// the local 'awaiting_host_dispatch' park, or a legacy 'dispatched' park); every
// other state — already-settled,
// non-acceptance, an unknown stage id — returns (nil, false, nil) as a no-op, which
// the endpoint renders short_circuited:false and the caller's own spawn path handles
// as today. On a hit it walks the stage to succeeded, emits the matching audit,
// then re-enters Advance so the run rolls forward (completeRun or later stages)
// exactly as the inline arm does, and returns the fired short-circuit.
//
// The second return value, liveValidationRequired (E48.6 / #1953), is true ONLY on
// the admissible-acceptance, plan-loaded, no-predicate-matched path (the short
// circuit did NOT fire and the stage still needs a live target). It is false on
// every no-op path — a non-admissible state, an unknown/non-acceptance stage, a nil
// plan, an un-wired orchestrator — and false alongside a fired short-circuit. The
// admission endpoint reads it to add needs_target + the target hosts / expected head
// SHA to its short_circuited:false response.
func (o *Orchestrator) TryShortCircuitAcceptance(ctx context.Context, runID, stageID uuid.UUID) (*AcceptanceShortCircuit, bool, error) {
	if o.Runs == nil {
		return nil, false, errors.New("orchestrator: Runs is nil")
	}
	r, err := o.Runs.GetRun(ctx, runID)
	if err != nil {
		return nil, false, fmt.Errorf("orchestrator: get run for acceptance admission: %w", err)
	}
	stages, err := o.Runs.ListStagesForRun(ctx, runID)
	if err != nil {
		return nil, false, fmt.Errorf("orchestrator: list stages for acceptance admission: %w", err)
	}
	var target *run.Stage
	for _, s := range stages {
		if s.ID == stageID {
			target = s
			break
		}
	}
	if target == nil {
		return nil, false, nil
	}
	// Dispatch-admissible only: pending, the local 'awaiting_host_dispatch' park
	// (#1912) an operator host dispatch finds, or a legacy 'dispatched' park a
	// migration missed (the state walk skips the already-passed transitions). Any
	// other state — already succeeded/failed, a run in flight — is a
	// non-admissible no-op (short_circuited:false), never a state change.
	if target.State != run.StageStatePending &&
		target.State != run.StageStateAwaitingHostDispatch &&
		target.State != run.StageStateDispatched {
		return nil, false, nil
	}
	sc, liveValidationRequired, err := o.tryShortCircuitAcceptanceCore(ctx, r, stages, target)
	if err != nil {
		return nil, false, err
	}
	if sc == nil {
		return nil, liveValidationRequired, nil
	}
	// Re-enter Advance so the now-terminal acceptance stage rolls the run
	// forward (completeRun / later stages) in the same request — identical to
	// the inline Advance arm's re-entry.
	if _, err := o.Advance(ctx, runID); err != nil {
		return nil, false, fmt.Errorf("orchestrator: advance after acceptance short-circuit: %w", err)
	}
	return sc, false, nil
}

// SliceConflict carries the structured provenance of a slice-branch
// merge conflict during fan-in (ADR-041 / #1142). integrateSlices
// returns it (non-nil) instead of string-parsing the free-form failure
// reason: the conflicting slice's index AND its owning child run id are
// the machine resume target the next_actions arm reads back from the
// slice_integration_conflict audit payload. Detail is the human-display
// message (stable "slice integration conflict: ..." prefix); the resume
// target is the structured fields, never parsed from Detail.
type SliceConflict struct {
	SliceIndex int
	ChildRunID uuid.UUID
	Detail     string
}

// integrateSlicesPageSize bounds each ListRuns page the fan-in
// children-listing walk fetches. Decompositions are small (a handful of
// slices), so this is far above any realistic child count — but the walk
// PAGINATES TO COMPLETION (#1142 partial-integration safety) so a future
// large fan-out can never silently integrate only the first page. A var
// (not a const) only so the pagination test can shrink it to exercise the
// multi-page walk without seeding 100+ child rows.
var integrateSlicesPageSize = 100

// IntegrateSlices is the exported wrapper the child-completion sweeper's
// adapter calls: it loads the parent run then delegates to integrateSlices
// (ADR-041 / #1142). A non-nil *SliceConflict means a slice branch failed
// to merge (the parent must fail recoverable); a nil conflict + nil error
// means a clean integration (or a graceful skip).
func (o *Orchestrator) IntegrateSlices(ctx context.Context, parentRunID uuid.UUID) (*SliceConflict, error) {
	if o.Runs == nil {
		return nil, errors.New("orchestrator: Runs is nil")
	}
	r, err := o.Runs.GetRun(ctx, parentRunID)
	if err != nil {
		return nil, fmt.Errorf("get parent run: %w", err)
	}
	return o.integrateSlices(ctx, r)
}

// integrateSlices is the fan-in step (ADR-041 / E24.2 / #1142): once every
// decomposed child has succeeded, it sequentially merges each succeeded
// slice branch fishhawk/run-<parent>/slice-<n> onto the consolidated
// branch fishhawk/run-<parent> in ascending slice-index order via
// server-side git merges, creating the consolidated branch from the run's
// base ref first (under E24.1/#1141 nobody else creates it). A merge
// conflict returns a non-nil *SliceConflict (the caller fails the parent
// implement stage category-B recoverable); a clean run emits a
// slices_integrated audit and returns (nil, nil).
//
// Graceful-skip (nil, nil — same posture as maybeOpenConsolidatedPR /
// fireDispatch) when GitHub/installation isn't wired or there are zero
// succeeded children: the CLI/dev posture must never regress.
func (o *Orchestrator) integrateSlices(ctx context.Context, parent *run.Run) (*SliceConflict, error) {
	// Graceful-skip when GitHub can't be reached (no client / no
	// installation) — the consolidated branch is simply not produced, the
	// same posture maybeOpenConsolidatedPR takes.
	if o.GitHub == nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: GitHub not configured; skipping slice integration",
			slog.String("run_id", parent.ID.String()))
		return nil, nil
	}
	if parent.InstallationID == nil || *parent.InstallationID == 0 {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: run has no installation_id; skipping slice integration",
			slog.String("run_id", parent.ID.String()))
		return nil, nil
	}

	children, err := o.listAllDecomposedChildren(ctx, parent.ID)
	if err != nil {
		return nil, fmt.Errorf("list decomposed children: %w", err)
	}

	// Keep succeeded children with a slice index, ascending by index. A
	// succeeded child missing SliceIndex is a defensive skip (it has no
	// derivable slice branch) — WARN rather than guess a branch name.
	succeeded := make([]*run.Run, 0, len(children))
	for _, c := range children {
		if c.State != run.StateSucceeded {
			continue
		}
		if c.SliceIndex == nil {
			o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: succeeded decomposed child missing slice_index; skipping integration of it",
				slog.String("parent_run_id", parent.ID.String()),
				slog.String("child_run_id", c.ID.String()))
			continue
		}
		succeeded = append(succeeded, c)
	}
	if len(succeeded) == 0 {
		// Zero children to integrate — an ordinary non-decomposed run, or a
		// decomposition whose children all lack a slice branch. Same skip
		// posture as maybeOpenConsolidatedPR's zero-children branch.
		return nil, nil
	}
	sort.SliceStable(succeeded, func(i, j int) bool {
		return *succeeded[i].SliceIndex < *succeeded[j].SliceIndex
	})

	repo, err := parseRepo(parent.Repo)
	if err != nil {
		return nil, fmt.Errorf("parse repo %q: %w", parent.Repo, err)
	}

	base := o.DefaultRef
	if base == "" {
		base = "main"
	}
	baseSHA, exists, err := o.GitHub.GetBranchSHA(ctx, *parent.InstallationID, repo, base)
	if err != nil {
		return nil, fmt.Errorf("resolve base ref %q: %w", base, err)
	}
	if !exists {
		return nil, fmt.Errorf("base ref %q does not exist on %s", base, repo)
	}

	// Ensure the consolidated branch exists, creating it from the base sha
	// when absent. CreateRef's 422 "already exists" no-op makes a
	// re-entrant settle (sweeper + event-driven race) safe.
	consolidated := consolidatedBranch(parent.ID)
	if _, cexists, err := o.GitHub.GetBranchSHA(ctx, *parent.InstallationID, repo, consolidated); err != nil {
		return nil, fmt.Errorf("resolve consolidated branch %q: %w", consolidated, err)
	} else if !cexists {
		if err := o.GitHub.CreateRef(ctx, *parent.InstallationID, repo, consolidated, baseSHA); err != nil {
			return nil, fmt.Errorf("create consolidated branch %q: %w", consolidated, err)
		}
	}

	// Merge each succeeded slice in ascending order. A 204 (already merged)
	// is an idempotent no-op so a resumed pass is clean. Each 201 returns the
	// integration merge commit SHA, accumulated so slices_integrated records
	// them for the ADR-035 lineage guard (#1459); a 204 returns "" and is
	// skipped — the SHA was recorded on the original 201 pass.
	childIDs := make([]string, 0, len(succeeded))
	integrationCommitSHAs := make([]string, 0, len(succeeded))
	for _, c := range succeeded {
		head := sliceBranch(parent.ID, *c.SliceIndex)
		msg := fmt.Sprintf("Integrate slice %d (run %s) into %s", *c.SliceIndex, shortRunID(c.ID), consolidated)
		mergeSHA, err := o.GitHub.MergeBranch(ctx, *parent.InstallationID, repo, consolidated, head, msg)
		switch {
		case err == nil:
			childIDs = append(childIDs, c.ID.String())
			if mergeSHA != "" {
				integrationCommitSHAs = append(integrationCommitSHAs, mergeSHA)
				// Record the merge SHA on the parent chain the INSTANT the
				// commit is created — durable across a later slice's
				// conflict/error early return and a re-entrant 204 no-op pass,
				// both of which never reach the terminal emitSlicesIntegrated
				// (#1806). buildReportedHeadLedger unions this alongside
				// slices_integrated.integration_commit_shas.
				o.emitIntegrationCommitRecorded(ctx, parent.ID, mergeSHA, *c.SliceIndex, c.ID.String(), consolidated)
			}
		case errors.Is(err, githubclient.ErrMergeConflict):
			detail := fmt.Sprintf("slice integration conflict: slice %d (child run %s) could not merge onto %s",
				*c.SliceIndex, c.ID, consolidated)
			o.logger().LogAttrs(ctx, slog.LevelInfo, "orchestrator: slice integration conflict",
				slog.String("parent_run_id", parent.ID.String()),
				slog.String("conflicting_child_run_id", c.ID.String()),
				slog.Int("conflicting_slice_index", *c.SliceIndex))
			return &SliceConflict{SliceIndex: *c.SliceIndex, ChildRunID: c.ID, Detail: detail}, nil
		default:
			return nil, fmt.Errorf("merge slice %d (child %s) onto %s: %w", *c.SliceIndex, c.ID, consolidated, err)
		}
	}

	o.emitSlicesIntegrated(ctx, parent.ID, childIDs, consolidated, integrationCommitSHAs)
	o.logger().LogAttrs(ctx, slog.LevelInfo, "orchestrator integrated decomposed slices",
		slog.String("parent_run_id", parent.ID.String()),
		slog.String("consolidated_branch", consolidated),
		slog.Int("slice_count", len(childIDs)))
	return nil, nil
}

// listAllDecomposedChildren pages ListRuns(DecomposedFrom=parent) to
// COMPLETION (#1142 partial-integration safety): a full page is NOT
// silently treated as the whole set — the walk advances the offset until
// a short page proves the listing is exhausted. Never integrating only
// the first page is the fail-closed requirement; without it a fan-out
// exceeding one page would consolidate a PR silently missing later slices.
func (o *Orchestrator) listAllDecomposedChildren(ctx context.Context, parentID uuid.UUID) ([]*run.Run, error) {
	var out []*run.Run
	offset := 0
	for {
		page, err := o.Runs.ListRuns(ctx, run.ListRunsFilter{
			DecomposedFrom: &parentID,
			Limit:          integrateSlicesPageSize,
			Offset:         offset,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if len(page) < integrateSlicesPageSize {
			break
		}
		offset += len(page)
	}
	return out, nil
}

// sliceBranch is the sole-writer slice branch the decomposed child at
// sliceIndex pushed to (E24.1 / #1141 / ADR-041):
// fishhawk/run-<shortParent>/slice-<n>. It derives from runBranchPrefix
// (NOT consolidatedBranch — the consolidated branch is the non-nesting
// "-consolidated" sibling, see runBranchPrefix's D/F-conflict note) and
// MUST stay byte-identical to the runner's childSliceBranch
// (runner/cmd/fishhawk-runner/main.go), which derives the same name; a
// divergence orphans a slice's commits from the fan-in merge (surfaces as
// a 404 ErrNotFound on MergeBranch).
func sliceBranch(parentID uuid.UUID, sliceIndex int) string {
	return runBranchPrefix(parentID) + "/slice-" + strconv.Itoa(sliceIndex)
}

// SliceBranch is the canonical, exported derivation of a decomposed child's
// per-slice sole-writer branch (ADR-041 / E24.1 / #1141):
// fishhawk/run-<short-parent>/slice-<n>. It delegates to the single
// unexported sliceBranch formula so there is exactly one source of truth for
// the name. Out-of-package consumers (e.g. server.fixupBranchFor, #1246) MUST
// call this rather than re-hardcoding the slice-branch literal — the same
// duplicated-reconstruction drift that orphaned parent fix-up commits on the
// #1243 consolidated rename (#1245). It MUST stay byte-identical to the
// runner's childSliceBranch (runner/cmd/fishhawk-runner/main.go), which
// derives the same name in the separate runner module that cannot import this
// package.
func SliceBranch(parentID uuid.UUID, sliceIndex int) string {
	return sliceBranch(parentID, sliceIndex)
}

// emitSlicesIntegrated writes a slices_integrated audit entry (system
// actor) once every succeeded slice merged cleanly onto the consolidated
// branch (#1142). Consumed by E24.7. Best-effort, mirroring
// emitChildrenSettled: nil-Audit guard, WARN-on-error, never unwinds the
// settle.
//
// integrationCommitSHAs are the merge commit SHAs the fan-in created on the
// consolidated branch (one per 201 merge). Recorded additively as
// integration_commit_shas so the server-side ADR-035 lineage guard can
// attribute the "Integrate slice N" merges instead of flagging them foreign
// at a later report boundary (#1459). The existing child_run_ids /
// consolidated_branch / slice_count fields are unchanged so downstream
// consumers (children_status, consolidatedBranchFromAudit) are unaffected.
func (o *Orchestrator) emitSlicesIntegrated(ctx context.Context, parentRunID uuid.UUID, childIDs []string, consolidatedBranch string, integrationCommitSHAs []string) {
	if o.Audit == nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: Audit not configured; skipping slices_integrated entry",
			slog.String("parent_run_id", parentRunID.String()))
		return
	}
	payload, err := json.Marshal(map[string]any{
		"child_run_ids":           childIDs,
		"consolidated_branch":     consolidatedBranch,
		"slice_count":             len(childIDs),
		"integration_commit_shas": integrationCommitSHAs,
	})
	if err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: marshal slices_integrated payload failed",
			slog.String("error", err.Error()))
		return
	}
	systemKind := audit.ActorSystem
	if _, err := o.Audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     parentRunID,
		Timestamp: time.Now().UTC(),
		Category:  "slices_integrated",
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: append slices_integrated failed",
			slog.String("error", err.Error()))
	}
}

// emitIntegrationCommitRecorded writes an integration_commit_recorded audit
// entry (system actor) immediately after each successful 201 slice merge, so
// the merge SHA is durable the instant the commit is created — BEFORE the
// merge loop reaches a slice that may conflict or error (#1806). The terminal
// emitSlicesIntegrated fires only on a fully-clean pass, so a partial pass
// that merges some slices then bails early, or a re-entrant pass that sees
// those merges as 204 no-ops, would otherwise lose the SHA from the reported-
// head ledger forever and flag it foreign at the parent fix-up boundary.
// buildReportedHeadLedger unions merge_sha in alongside
// slices_integrated.integration_commit_shas. Best-effort, mirroring
// emitSlicesIntegrated: nil-Audit guard, WARN-on-error, never unwinds the
// settle.
func (o *Orchestrator) emitIntegrationCommitRecorded(ctx context.Context, parentRunID uuid.UUID, mergeSHA string, sliceIndex int, childRunID, consolidatedBranch string) {
	if o.Audit == nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: Audit not configured; skipping integration_commit_recorded entry",
			slog.String("parent_run_id", parentRunID.String()))
		return
	}
	payload, err := json.Marshal(map[string]any{
		"merge_sha":           mergeSHA,
		"slice_index":         sliceIndex,
		"child_run_id":        childRunID,
		"consolidated_branch": consolidatedBranch,
	})
	if err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: marshal integration_commit_recorded payload failed",
			slog.String("error", err.Error()))
		return
	}
	systemKind := audit.ActorSystem
	if _, err := o.Audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     parentRunID,
		Timestamp: time.Now().UTC(),
		Category:  "integration_commit_recorded",
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: append integration_commit_recorded failed",
			slog.String("error", err.Error()))
	}
}

// emitSliceIntegrationConflict writes a slice_integration_conflict audit
// entry (system actor) when a slice branch fails to merge during fan-in
// (#1142). The payload carries the STRUCTURED conflict provenance —
// conflicting_slice_index + conflicting_child_run_id — so the next_actions
// arm sources the resume target from this entry rather than parsing the
// stage's free-form failure reason. Best-effort.
func (o *Orchestrator) emitSliceIntegrationConflict(ctx context.Context, parentRunID, stageID uuid.UUID, conflict *SliceConflict) {
	if o.Audit == nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: Audit not configured; skipping slice_integration_conflict entry",
			slog.String("parent_run_id", parentRunID.String()))
		return
	}
	payload, err := json.Marshal(map[string]any{
		"parent_stage_id":          stageID.String(),
		"conflicting_slice_index":  conflict.SliceIndex,
		"conflicting_child_run_id": conflict.ChildRunID.String(),
	})
	if err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: marshal slice_integration_conflict payload failed",
			slog.String("error", err.Error()))
		return
	}
	systemKind := audit.ActorSystem
	if _, err := o.Audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     parentRunID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  "slice_integration_conflict",
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: append slice_integration_conflict failed",
			slog.String("error", err.Error()))
	}
}

// sliceIntegrationConflictReasonPrefix is the STABLE prefix the fan-in
// conflict stamps on the parent implement stage's failure reason for human
// display. The next_actions arm keys on it to recognize the conflict
// state, but the machine resume target is sourced from the structured
// slice_integration_conflict audit payload, never parsed from this string.
const sliceIntegrationConflictReasonPrefix = "slice integration conflict"

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

	// #1063: existing-children idempotency guard. A fix-up on a decomposed
	// parent's implement stage re-opens that stage to pending; without this
	// guard the re-entry through Advance would re-mint a fresh fan-out from
	// the same approved plan instead of routing the consolidated-review
	// concern back onto the shared branch. When the parent already has minted
	// children (DecomposedFrom == parent.ID), this is a fix-up re-open (or a
	// sweeper double-advance), NOT a fresh decomposition: skip the fanout and
	// return (false, nil) so Advance falls through to dispatchStage and re-
	// invokes the parent's implement stage against the existing shared branch.
	// A ListRuns error returns a wrapped err so a transient store failure does
	// not silently re-mint a second fan-out. Only the first fanout (zero
	// children) proceeds to mint.
	existing, err := o.Runs.ListRuns(ctx, run.ListRunsFilter{
		DecomposedFrom: &parent.ID,
		Limit:          1,
	})
	if err != nil {
		return false, fmt.Errorf("list existing children: %w", err)
	}
	if len(existing) > 0 {
		o.logger().LogAttrs(ctx, slog.LevelInfo, "fanout skipped: parent already has children",
			slog.String("parent_run_id", parent.ID.String()),
		)
		return false, nil
	}

	childIDs := make([]string, 0, len(approvedPlan.Decomposition.SubPlans))
	for i, sub := range approvedPlan.Decomposition.SubPlans {
		parentID := parent.ID
		// Capture the loop index into a local: each child is routed by
		// the runner onto its own sole-writer slice branch
		// fishhawk/run-<parent>/slice-<idx> (E24.1 / #1141 / ADR-041).
		// The index is the sub_plan's position in dependency order.
		idx := i
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
			SliceIndex:     &idx,
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
			slog.Int("slice_index", idx),
		)
	}

	// Park the parent's implement stage in awaiting_children. The
	// child-completion sweeper transitions it to succeeded/failed
	// once every child has reached a terminal run state.
	if _, err := o.Runs.TransitionStage(ctx, parentImplement.ID, run.StageStateAwaitingChildren, nil); err != nil {
		return false, fmt.Errorf("transition parent implement to awaiting_children: %w", err)
	}

	// Resolve the effective concurrency cap from the run's cached workflow
	// spec (E24.6 / #1146): the per-workflow decomposition.max_parallel knob
	// wins, else the global FISHHAWKD_MAX_PARALLEL_CHILDREN default (0 =
	// unlimited). We RESOLVE and SURFACE it here (log + plan_decomposed
	// payload) so E24.3 (#1143) can consume it; this does NOT throttle
	// minting — every child above was already created.
	effectiveMaxParallel := o.resolveEffectiveMaxParallel(ctx, parent)

	// Topological dispatch order (#1258 slice B): plan.Waves derives the
	// dependency-ordered waves of sub-plan indices from the depends_on edges.
	// The indices are POSITIONAL into childIDs (childIDs[i] is the child minted
	// for sub_plan i — both are built in sub_plan order), so the MCP can map a
	// wave's indices back to child run ids. Waves() is pure and was wired into
	// the plan semantic check in slice A, so an error here is should-be-
	// impossible post-validation — fall back to a single all-indices wave
	// (back-compat: one concurrent wave) with a WARN rather than dropping the
	// payload.
	waves, werr := plan.Waves(approvedPlan.Decomposition)
	if werr != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: plan.Waves failed post-validation; falling back to a single all-indices wave",
			slog.String("parent_run_id", parent.ID.String()),
			slog.String("error", werr.Error()),
		)
		waves = singleAllIndicesWave(len(childIDs))
	}

	o.emitPlanDecomposed(ctx, parent.ID, planStageID, childIDs, approvedPlan.Decomposition.Rationale, effectiveMaxParallel, waves)
	o.logger().LogAttrs(ctx, slog.LevelInfo, "orchestrator parent parked awaiting children",
		slog.String("parent_run_id", parent.ID.String()),
		slog.String("parent_stage_id", parentImplement.ID.String()),
		slog.Int("child_count", len(childIDs)),
		slog.Int("effective_max_parallel", effectiveMaxParallel),
	)

	// Initial concurrent dispatch (E24.3 / #1143): dispatch the freshly
	// minted children up to the resolved cap rather than leaving them
	// pending for serial operator drive. Best-effort — a dispatch error
	// does NOT unwind the fanout (the children are already minted and the
	// parent is parked; the event-driven refill and the sweeper backstop
	// will retry the undispatched ones).
	if _, err := o.DispatchDecomposedChildren(ctx, parent.ID); err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: initial decomposed-child dispatch failed; sweeper backstop will retry",
			slog.String("parent_run_id", parent.ID.String()),
			slog.String("error", err.Error()),
		)
	}
	return true, nil
}

// resolveEffectiveMaxParallel computes the decomposition concurrency cap
// for the parent run (E24.6 / #1146) by parsing the run's cached workflow
// spec, looking up the run's workflow, and resolving the per-workflow
// decomposition.max_parallel knob against the global
// MaxParallelChildren default (0 = unlimited). Best-effort: an absent
// spec, a parse failure, or a workflow not found in the spec degrades to
// the global default with a WARN — never blocking the fanout that has
// already minted the children.
func (o *Orchestrator) resolveEffectiveMaxParallel(ctx context.Context, parent *run.Run) int {
	if len(parent.WorkflowSpec) == 0 {
		return o.MaxParallelChildren
	}
	parsed, err := spec.ParseBytes(parent.WorkflowSpec)
	if err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn,
			"orchestrator: parse cached workflow spec for max_parallel failed — using global default",
			slog.String("parent_run_id", parent.ID.String()),
			slog.String("error", err.Error()))
		return o.MaxParallelChildren
	}
	wf, ok := parsed.Workflows[parent.WorkflowID]
	if !ok {
		o.logger().LogAttrs(ctx, slog.LevelWarn,
			"orchestrator: workflow not in cached spec for max_parallel — using global default",
			slog.String("parent_run_id", parent.ID.String()),
			slog.String("workflow_id", parent.WorkflowID))
		return o.MaxParallelChildren
	}
	return wf.EffectiveMaxParallel(o.MaxParallelChildren)
}

// DispatchDecomposedChildren dispatches a decomposed parent's pending
// child runs concurrently, up to the resolved concurrency cap (E24.3 /
// ADR-041 / #1143). It is the backend-agnostic orchestration seam: the
// per-backend dispatch mechanics (local host-spawn vs Actions
// workflow_dispatch) stay owned by the existing runner-kind-aware
// Advance/fireDispatch path (E24.4 / E24.5).
//
// It lists ALL children, partitions them into pending / in-flight /
// terminal, resolves the cap, and consumes budget.ParallelDecision with
// requested = the active (pending+in-flight) fan-out width. Headroom is
// Allowed - in-flight, so as in-flight children settle the next pending
// children dispatch to hold the active count at the cap. Pending
// children dispatch in ascending SliceIndex order via o.Advance (the
// same edge plan-approval dispatch uses). Returns the count dispatched.
//
// Best-effort + idempotent: in-flight children are counted from current
// run state, so re-entrant/concurrent calls (fanout + the event-driven
// refill + the sweeper backstop can overlap) bound to the cap, and
// Advance same-state transitions no-op. The cap is a soft target — a
// benign one-slot overshoot in a tight race is acceptable and never
// strands or double-runs a child. A per-child Advance error is
// WARN-logged and skipped so one undispatchable child cannot block the
// others; only a parent-load or child-listing failure is returned.
func (o *Orchestrator) DispatchDecomposedChildren(ctx context.Context, parentRunID uuid.UUID) (int, error) {
	if o.Runs == nil {
		return 0, errors.New("orchestrator: Runs is nil")
	}
	parent, err := o.Runs.GetRun(ctx, parentRunID)
	if err != nil {
		return 0, fmt.Errorf("orchestrator: get parent run: %w", err)
	}
	children, err := o.listAllDecomposedChildren(ctx, parentRunID)
	if err != nil {
		return 0, fmt.Errorf("orchestrator: list decomposed children: %w", err)
	}

	var pending, inFlight []*run.Run
	for _, c := range children {
		switch {
		case c.State == run.StatePending:
			pending = append(pending, c)
		case !c.State.IsTerminal():
			inFlight = append(inFlight, c)
		}
	}
	if len(pending) == 0 {
		return 0, nil
	}

	cap := o.resolveEffectiveMaxParallel(ctx, parent)
	decision := budget.ParallelDecision(len(pending)+len(inFlight), cap)
	headroom := decision.Allowed - len(inFlight)
	if headroom <= 0 {
		o.logger().LogAttrs(ctx, slog.LevelInfo, "orchestrator: decomposed children at concurrency cap; no dispatch headroom",
			slog.String("parent_run_id", parentRunID.String()),
			slog.Int("pending", len(pending)),
			slog.Int("in_flight", len(inFlight)),
			slog.Int("allowed", decision.Allowed),
			slog.Int("cap", cap),
		)
		return 0, nil
	}

	// Dispatch pending children in ascending slice-index order so the
	// cap admits the earliest slices first (a nil SliceIndex sorts last).
	sort.SliceStable(pending, func(i, j int) bool {
		si, sj := pending[i].SliceIndex, pending[j].SliceIndex
		switch {
		case si == nil && sj == nil:
			return pending[i].ID.String() < pending[j].ID.String()
		case si == nil:
			return false
		case sj == nil:
			return true
		default:
			return *si < *sj
		}
	})

	dispatched := 0
	for _, child := range pending {
		if dispatched >= headroom {
			break
		}
		if _, err := o.Advance(ctx, child.ID); err != nil {
			o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: dispatch decomposed child failed",
				slog.String("parent_run_id", parentRunID.String()),
				slog.String("child_run_id", child.ID.String()),
				slog.String("error", err.Error()),
			)
			continue
		}
		dispatched++
		o.recordChildDispatch(ctx, child)
	}

	o.logger().LogAttrs(ctx, slog.LevelInfo, "orchestrator dispatched decomposed children",
		slog.String("parent_run_id", parentRunID.String()),
		slog.Int("dispatched", dispatched),
		slog.Int("pending_before", len(pending)),
		slog.Int("in_flight_before", len(inFlight)),
		slog.Int("allowed", decision.Allowed),
		slog.Bool("capped", decision.Capped),
		slog.Int("cap", cap),
	)
	return dispatched, nil
}

// recordChildDispatch emits the RuleChildrenDispatch run_auto_advanced
// entry for one dispatched child (E24.3 / #1143), anchored to the
// child's implement stage. Best-effort + idempotent: a nil Drive engine
// no-ops, Engine.Recorded dedups a re-dispatch, and the entry's
// Parked/NextAction shape comes from drive.EvaluateChildrenDispatch
// (local parks for a host-side dispatch; github_actions advances).
func (o *Orchestrator) recordChildDispatch(ctx context.Context, child *run.Run) {
	if o.Drive == nil {
		return
	}
	var implStageID *uuid.UUID
	if stages, err := o.Runs.ListStagesForRun(ctx, child.ID); err == nil {
		for _, s := range stages {
			if s.Type == run.StageTypeImplement {
				id := s.ID
				implStageID = &id
				break
			}
		}
	}
	if o.Drive.Recorded(ctx, child.ID, implStageID, drive.RuleChildrenDispatch) {
		return
	}
	out := drive.EvaluateChildrenDispatch(child.RunnerKind)
	adv := drive.Advance{
		Rule: drive.RuleChildrenDispatch,
		From: "implement:awaiting_children_child",
	}
	if out.Advance {
		adv.To = "implement:dispatched"
		adv.Event = "decomposed parent dispatched child run via the runner-kind-aware Advance edge"
	} else {
		adv.To = "implement:ready"
		adv.Event = "decomposed parent: runner_kind local parks the child for a host-side dispatch"
		adv.Parked = true
		adv.NextAction = out.NextAction
	}
	o.Drive.Record(ctx, child.ID, implStageID, adv)
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

// singleAllIndicesWave returns a single wave [[0,1,...,n-1]] — the back-compat
// collapse a no-depends_on decomposition layers to, and the should-be-
// impossible plan.Waves error fallback. Returns nil for n<=0 so the
// plan_decomposed payload carries no waves rather than an empty wave.
func singleAllIndicesWave(n int) [][]int {
	if n <= 0 {
		return nil
	}
	wave := make([]int, n)
	for i := range wave {
		wave[i] = i
	}
	return [][]int{wave}
}

// emitPlanDecomposed writes a plan_decomposed audit entry naming the
// child run IDs, the rationale string, and the resolved
// effective_max_parallel concurrency cap (E24.6 / #1146 — 0 = unlimited).
// Best-effort: a failure here logs and returns; the fanout has already
// taken effect at the data layer.
func (o *Orchestrator) emitPlanDecomposed(ctx context.Context, parentRunID, parentStageID uuid.UUID, childIDs []string, rationale string, effectiveMaxParallel int, waves [][]int) {
	if o.Audit == nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: Audit not configured; skipping plan_decomposed entry",
			slog.String("parent_run_id", parentRunID.String()))
		return
	}
	payload, err := json.Marshal(map[string]any{
		"child_run_ids":          childIDs,
		"rationale":              rationale,
		"parent_stage_id":        parentStageID.String(),
		"effective_max_parallel": effectiveMaxParallel,
		// waves carries the topological dispatch order as ordered waves of
		// slice indices into child_run_ids (#1258 slice B). The MCP wave loop
		// maps each wave's indices back to child run ids and integrates between
		// waves. A nil/absent waves decodes back-compat as a single all-indices
		// wave on the consumer side.
		"waves": waves,
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
	// #968 belt-and-suspenders: refuse to stamp a run succeeded while ANY
	// stage is non-terminal (awaiting_approval, awaiting_children,
	// dispatched, running) — the single chokepoint covering every caller.
	// Applies ONLY to the succeeded target: a failed/cancelled run
	// legitimately completes with downstream stages still pending.
	if target == run.StateSucceeded {
		for _, s := range stages {
			if !s.State.IsTerminal() {
				o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator refused run completion: stage non-terminal",
					slog.String("run_id", r.ID.String()),
					slog.String("stage_id", s.ID.String()),
					slog.String("stage_state", string(s.State)),
				)
				return OutcomeNoOp, nil
			}
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
			// Event-driven refill (E24.3 / #1143): not all children are
			// terminal yet, so the parent stays parked — but a child just
			// settled, which may have freed a concurrency slot. Top up the
			// dispatch to the cap before returning so the next pending
			// children start as in-flight ones finish. Best-effort
			// WARN-on-error; the sweeper backstop covers a miss.
			if _, derr := o.DispatchDecomposedChildren(ctx, parentRunID); derr != nil {
				o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: event-driven decomposed-child refill failed",
					slog.String("parent_run_id", parentRunID.String()),
					slog.String("error", derr.Error()),
				)
			}
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

	// #698 / #1081: when children failed but EVERY failed child's
	// implement failure is recoverable in decomposition (A/C/D-timeout,
	// or category B via the in-place recover path), park the parent in
	// awaiting_children rather than resolving it to failed-C. This
	// closes the race where a near-instant event-driven resolution
	// would terminate the parent before an operator can re-drive the
	// recoverable child. The parent stays parked until a re-drive
	// re-runs the child and this path fires again on its next
	// terminal transition. Only a non-recoverable failed child
	// (D-rejection or an unclassifiable failure) resolves the parent to
	// failed-C; a genuine category-B child is now recoverable in place.
	if len(failedChildren) > 0 && o.failedChildrenAllRecoverable(ctx, failedChildren) {
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

	// Fan-in (ADR-041 / #1142): on the happy path (all children succeeded),
	// integrate each succeeded slice branch onto the consolidated branch
	// BEFORE stamping the awaiting_children stage succeeded, so a merge
	// conflict can fail the parent implement stage recoverable (category-B)
	// — the issue's requirement. A non-conflict error leaves the stage
	// parked (next tick/retry re-enters; merges are idempotent). On success
	// we fall through to the existing succeeded transition + Advance, which
	// opens the consolidated PR from the now-integrated branch.
	if !anyFailed {
		conflict, err := o.IntegrateSlices(ctx, parentRunID)
		switch {
		case err != nil:
			o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: slice integration error; leaving parent parked",
				slog.String("parent_run_id", parentRunID.String()),
				slog.String("error", err.Error()),
			)
			return
		case conflict != nil:
			cat := run.FailureB
			reason := conflict.Detail
			if _, terr := o.Runs.TransitionStage(ctx, awaitingStage.ID, run.StageStateFailed, &run.StageCompletion{
				FailureCategory: &cat,
				FailureReason:   &reason,
			}); terr != nil {
				o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: transition awaiting_children to failed-B on slice conflict failed",
					slog.String("parent_run_id", parentRunID.String()),
					slog.String("stage_id", awaitingStage.ID.String()),
					slog.String("error", terr.Error()),
				)
				return
			}
			o.emitSliceIntegrationConflict(ctx, parentRunID, awaitingStage.ID, conflict)
			return
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

// failedChildrenAllRecoverable reports whether every failed child run's
// implement-stage failure is recoverable in decomposition (A/C/D-timeout,
// or category B via the in-place recover path). Used by
// maybeAdvanceDecomposedParent to decide whether to park the parent
// awaiting re-drive. A failed child whose stages can't be listed, or
// whose implement stage carries no failure category, is treated as NOT
// recoverable — parking is only safe when every failure is positively
// confirmed recoverable, so an unclassifiable child resolves the parent
// rather than parking it indefinitely.
func (o *Orchestrator) failedChildrenAllRecoverable(ctx context.Context, failed []*run.Run) bool {
	for _, c := range failed {
		stages, err := o.Runs.ListStagesForRun(ctx, c.ID)
		if err != nil {
			o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: list child stages for recoverability check failed",
				slog.String("child_run_id", c.ID.String()),
				slog.String("error", err.Error()),
			)
			return false
		}
		if !run.ImplementFailureRecoverable(stages) {
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

	// #1912: a runner_kind-locked-local agent stage cannot be spawned by the
	// backend (the runner is host-spawned per ADR-024). Park it in
	// awaiting_host_dispatch instead of dispatched. Post-#1912 'dispatched'
	// unambiguously means a spawn attempt EXISTS; the host-dispatch marker
	// endpoint (or an MCP spawn verb calling it) flips awaiting_host_dispatch →
	// dispatched at the moment of the spawn. This is the SINGLE write site for
	// the park: every local park — plan-approved dispatch, retry_reopen, fixup
	// re-open, revise replan, children dispatch — flows through Advance →
	// dispatchStage, so this one branch covers every drive rule. Engages ONLY on
	// the LOCKED state (RunnerKindResolved == true); an un-resolved run falls
	// through to the normal dispatched + fireDispatch path, whose own
	// locked-local skip (see fireDispatch) remains the defensive backstop.
	if r.RunnerKindResolved && r.RunnerKind == run.RunnerKindLocal {
		if _, err := o.Runs.TransitionStage(ctx, next.ID, run.StageStateAwaitingHostDispatch, nil); err != nil {
			return OutcomeNoOp, fmt.Errorf("orchestrator: park stage awaiting host dispatch: %w", err)
		}
		o.logger().LogAttrs(ctx, slog.LevelInfo, "orchestrator parked agent stage awaiting host dispatch (runner_kind=local)",
			slog.String("run_id", r.ID.String()),
			slog.String("stage_id", next.ID.String()),
			slog.Int("sequence", next.Sequence),
			slog.String("executor", string(next.ExecutorKind)),
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
//
// This same runner-kind-aware path OWNS Actions decomposed-child dispatch
// (E24.5 / #1145). Each github_actions decomposed child auto-advances
// through here via DispatchDecomposedChildren -> Advance and fires its OWN
// workflow_dispatch carrying its own run_id/stage_id against the base ref
// (o.DefaultRef) — so the child runner checks out its own sole-writer slice
// branch fishhawk/run-<parent>/slice-<idx> and pushes a distinct branch
// name, never colliding with a sibling. The runner — NOT the dispatch —
// derives that slice branch by fetching decomposed_from + slice_index from
// the stage-details endpoint keyed by run_id; no NEW workflow_dispatch input
// is added because GitHub rejects inputs not declared in the customer-side
// .github/workflows/fishhawk.yml with a 422 "Unexpected inputs provided",
// and the existing run_id/stage_id inputs are already sufficient. For a
// decomposed child the dispatch is annotated with structured slice_index /
// decomposed_from log fields so the per-slice fan-out is observable.
func (o *Orchestrator) fireDispatch(ctx context.Context, r *run.Run, next *run.Stage) error {
	// Pre-dispatch runner_kind mismatch guardrail, Actions direction (#1355,
	// the ADR-045 guardrail variant #1346 deferred). A workflow_dispatch fires
	// a GitHub Actions runner; firing one against a run already LOCKED to
	// runner_kind=local is a guaranteed channel mismatch that #1348 would only
	// FLAG after execution. Skip the dispatch instead. Engages ONLY on the
	// LOCKED state (RunnerKindResolved == true) so an un-resolved run still
	// auto-resolves on its first dispatch (#1346 decision-1) and a
	// github_actions-locked run fires unchanged.
	if r.RunnerKindResolved && r.RunnerKind == run.RunnerKindLocal {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: run locked to runner_kind=local; skipping github_actions workflow_dispatch",
			slog.String("run_id", r.ID.String()),
			slog.String("runner_kind", r.RunnerKind),
		)
		return nil
	}
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

	// E24.5: annotate a decomposed child's workflow_dispatch with its slice
	// provenance so the per-slice Actions fan-out is observable. Each child
	// fires its OWN dispatch (own run_id/stage_id, base ref) and the runner
	// derives the sole-writer slice branch from the stage-details endpoint —
	// no new dispatch input is added (see the doc comment).
	if r.DecomposedFrom != nil && r.SliceIndex != nil {
		o.logger().LogAttrs(ctx, slog.LevelInfo, "orchestrator dispatching decomposed-child workflow_dispatch",
			slog.String("run_id", r.ID.String()),
			slog.String("stage_id", next.ID.String()),
			slog.Int("slice_index", *r.SliceIndex),
			slog.String("decomposed_from", r.DecomposedFrom.String()),
			slog.String("ref", ref),
		)
	}

	inputs := githubclient.DispatchInputs{
		"run_id":      r.ID.String(),
		"stage_id":    next.ID.String(),
		"workflow_id": r.WorkflowID,
		"stage":       next.ExecutorRef,
	}
	// #1227: a decomposed child carries its decomposition-parent id so the
	// customer workflow can key an Actions `concurrency:` group on the run
	// FAMILY and queue/serialize sibling child jobs as a runner-capacity guard
	// (complements the backend dispatch cap, FISHHAWKD_MAX_PARALLEL_CHILDREN).
	// DecomposedFrom (the fan-out parent), NOT ParentRunID (retry/related-run
	// threading), is the sibling family. A non-decomposed run omits the input,
	// so the workflow's group falls back to a per-run unique key (no
	// serialization). Empty when DecomposedFrom is nil.
	if r.DecomposedFrom != nil {
		inputs["parent_run_id"] = r.DecomposedFrom.String()
	}
	return o.GitHub.DispatchWorkflow(ctx, *r.InstallationID, repo, actionsFile, ref, inputs)
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
