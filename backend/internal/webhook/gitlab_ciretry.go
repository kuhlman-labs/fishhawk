package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/runnerbackend"
)

// GitLab CI-failure retry — E45.22 / #2043, HIGH 2 and HIGH 3.
//
// The GitHub path correlates a failing check to its run through
// runs.pull_request_url, which the check_run payload hands it directly. A
// GitLab Pipeline Hook carries no such handle: it carries a project, a ref, a
// sha, a pipeline id and (for an MR pipeline) a merge-request iid. So this
// path correlates DETERMINISTICALLY on (ref, sha):
//
//   - ref must be the candidate run's ADR-035 sole-writer run branch, and
//   - sha must equal the head SHA that run recorded on its implement-stage
//     pull_request artifact.
//
// The merge-request iid only NARROWS the candidate set. It cannot select a
// run: a retry lineage produces MULTIPLE runs per merge request by
// construction, which is exactly the ambiguity HIGH 2 names.
//
// FIRST-STAGE RETRY IS DEFERRED (BC1). Issue #2043 offers two resolutions for
// the first-stage case — "correlate by pipeline/project+SHA, or defer
// first-stage retry until the run branch exists" — and this contract
// deliberately takes the SECOND. A run's first pipeline runs on the default
// ref (`main`), because the run branch does not exist until the runner creates
// it, so its ref can never equal a run branch and it correlates to nothing.
// The alternative — correlating a default-ref pipeline by project+SHA alone —
// would match any run whose recorded head happened to be that commit,
// including runs on other branches and other lineages, and there is no second
// signal to disambiguate them. A deferred retry costs one manual re-run; a
// mis-correlated one retries the WRONG run.
//
// A deferral is not a silent nothing. Every no-correlation outcome writes a
// `ci_retry_skipped` audit entry naming WHICH reason applied, so a failure that
// was correctly ignored is distinguishable in the audit from one that should
// have retried and did not (#2860). The entry is chained to a run ONLY when
// that run actually owns the pipeline's ref; otherwise it is global-chained.
// An entry hung off "the newest gitlab_ci run for the project" would land on an
// arbitrary run with no relationship to the failing pipeline — every failed
// default-branch build in the project would misattribute itself onto whichever
// run happened to be newest.
//
// PRE-FLIGHT REFUSAL, AND HOW IT DIFFERS FROM THE GITHUB PATH (E45.25 /
// #2876). runnerbackend.GitLabCI.TriggerStage returns nil on BOTH of its
// fail-closed warn-skip branches — a nil Trigger (GitLab unconfigured) and a
// zero p.Scope — which is indistinguishable from a created pipeline. So this
// path checks both preconditions BEFORE minting the retry child and refuses
// with a named `ci_retry_skipped` reason instead.
//
// The GitHub path is shaped differently ON PURPOSE. It mints the child and
// CONVERTS each warn-skip into a dispatchErr (errCIRetryGitHubUnconfigured /
// errCIRetryNoInstallation), leaving a persisted child with an untransitioned
// stage and a `dispatch_failed` audit row. The GitLab path refuses BEFORE
// minting, leaving no child at all and a `ci_retry_skipped` row. This
// divergence in committed state is deliberate and operator-ratified: a minted
// child consumes the single retry slot enforced by runs_retry_child_once_idx
// (retry_attempt = parent.RetryAttempt + 1), so a child that never dispatched
// permanently occupies the attempt a corrected deployment would otherwise use —
// a worse artifact than no row at all, on a path whose entire subject is the
// record telling the truth.
//
// AUTHORIZATION. This path runs the SAME GitLabProjectAuthorizer gate the
// create path runs, before candidate lookup, child creation or any pipeline
// trigger. A Pipeline Hook is authenticated only by the deployment-shared
// X-Gitlab-Token with no HMAC over the body, so ev.Repo / ev.CredentialRef are
// untrusted: without the gate, a token holder who can name a managed project
// path plus an observable run branch and head SHA could drive credentialed
// pipeline dispatches — agent code execution and spend — on someone else's
// run. Correlating on (ref, sha) bounds the blast radius but is NOT an
// authorization binding: both values are readable from a public branch
// listing.

// BUDGET ADMISSION IS NOT RUN ON THIS PATH (E45.27 / #2878). Like the GitHub
// retry handler, this one mints its child without calling
// refusedByBlockingBudget. The blocking periodic-budget seams gate whether NEW
// work starts; a CI-failure retry continues a lineage that already passed
// admission, and ADR-030's position is that in-flight work finishes. Gating it
// would strand a red merge request with no in-product path forward (a webhook
// trigger cannot carry budget_override), and the fail-open sum would make that
// stranding nondeterministic. The exemption is bounded by construction rather
// than by trust: retry_attempt is parent-derived and runs_retry_child_once_idx
// caps a lineage at on_ci_failure.max_retries children however many deliveries
// arrive, while opening a NEW lineage still passes the gated create seam. This
// complements the AUTHORIZATION paragraph above — the population able to reach
// this path is bound to the installation row's EXACT recorded project path
// (E45.26 / #2877), not a namespace. Parity with the GitHub path is deliberate
// here, unlike the pre-flight divergence above. Contract:
// backend/internal/webhook/README.md ("Why the CI-retry paths are exempt");
// pinned by
// TestGitLabCIRetry_BlockingBudgetExhausted_StillRetries_ADR030Exemption.

// gitLabRunBranchNamespace is the prefix of every ADR-035 sole-writer run
// branch. A pipeline ref that does not start with it cannot belong to any run
// — the first-stage default-ref case.
const gitLabRunBranchNamespace = "fishhawk/run-"

// gitLabMergeRequestRefPattern matches the two ref shapes a merge-request
// pipeline is documented to present OUTSIDE the Pipeline Hook payload
// (E45.30 / #2881):
//
//   - refs/merge-requests/<iid>/head — the DETACHED merge-request pipeline
//     ref, the value of the CI_MERGE_REQUEST_REF_PATH predefined variable
//     (https://docs.gitlab.com/ci/variables/predefined_variables/), and
//   - refs/merge-requests/<iid>/merge — the MERGED-RESULTS pipeline ref
//     (https://docs.gitlab.com/ci/pipelines/merged_results_pipelines/).
//
// BOTH shapes are INFERRED from that CI-variable / merged-results
// documentation, NOT transcribed from a documented webhook example. The
// documented Pipeline Hook payload carries object_attributes.ref as the MR's
// TARGET BRANCH with source = "merge_request_event", so this ref arm may never
// fire on a real Pipeline Hook. It is kept as defensive breadth — the cost is
// one anchored regexp branch on a delivery that is already being skipped, and
// the only effect is which reason string that skip carries. The SOURCE
// discriminator below is the primary, documented signal.
//
// Anchored at both ends deliberately: an unanchored pattern would classify
// refs/merge-requests/7/head/extra or a leading-garbage ref, widening a
// classification whose whole purpose is to be more precise than the one it
// replaces.
var gitLabMergeRequestRefPattern = regexp.MustCompile(`^refs/merge-requests/[0-9]+/(head|merge)$`)

// gitLabPipelineSourceMergeRequest is the object_attributes.source value
// GitLab's documented Pipeline Hook carries for a merge-request pipeline
// (https://docs.gitlab.com/user/project/integrations/webhook_events/).
const gitLabPipelineSourceMergeRequest = "merge_request_event"

// isGitLabMergeRequestPipeline reports whether a pipeline that is NOT on a run
// branch is identifiably a merge-request pipeline (E45.30 / #2881).
//
// The OR is the point. Neither signal may be load-bearing alone: a delivery
// carrying the source discriminator presents a plain target-branch ref, and a
// delivery presenting a merge-request-shaped ref may carry no source at all.
// Requiring both would leave each shape misclassified whenever the other
// signal is absent.
//
// A non-zero MergeRequestIID is DELIBERATELY NOT a third signal. GitLab
// attaches a merge_request block to a BRANCH pipeline that merely has an
// associated open merge request, so treating the iid as sufficient would
// reclassify ordinary default-ref first-stage pipelines as MR pipelines — the
// exact mislabelling this predicate exists to fix, inverted. Pinned by the
// "no source, non-zero iid -> false" row in TestIsGitLabMergeRequestPipeline.
func isGitLabMergeRequestPipeline(pr *PipelineRef) bool {
	if pr == nil {
		return false
	}
	return pr.Source == gitLabPipelineSourceMergeRequest ||
		gitLabMergeRequestRefPattern.MatchString(pr.Ref)
}

// ci_retry_skipped reasons. Each names a DISTINCT non-correlation so an
// operator reading the audit can tell which one happened (BC1). They are
// payload values, not categories: one category keeps the stream awaitable.
const (
	// gitLabRetrySkipFirstStageRef is the DELIBERATE deferral: the pipeline
	// ran on a ref outside the run-branch namespace — the default ref a
	// first-stage pipeline necessarily runs on.
	gitLabRetrySkipFirstStageRef = "first_stage_pipeline_on_non_run_branch"
	// gitLabRetrySkipMergeRequestRef is the same DEFERRAL as
	// gitLabRetrySkipFirstStageRef, told truthfully for a merge-request
	// pipeline (E45.30 / #2881). An MR pipeline's ref is the target branch (or,
	// on other GitLab surfaces, refs/merge-requests/<iid>/{head,merge}), never a
	// run branch, so it fell into the first-stage arm and drew a reason that
	// asserts something affirmatively FALSE about it: it is neither the run's
	// first pipeline nor a default-ref one. Under the #2860 principle the reason
	// set exists to serve, a wrong reason is worse than a generic one — an
	// operator debugging a missing retry reads "first stage on non-run branch",
	// concludes the deferral was correct, and stops looking.
	gitLabRetrySkipMergeRequestRef = "merge_request_pipeline_ref_not_a_run_branch"
	// gitLabRetrySkipNoRunForRef means the ref IS a run branch but no
	// candidate run in the narrowed set owns it (a run from another project
	// view, or one aged out of the candidate window).
	gitLabRetrySkipNoRunForRef = "no_candidate_run_owns_pipeline_ref"
	// gitLabRetrySkipSHAMismatch means a run owns the ref but recorded a
	// DIFFERENT head SHA — a stale pipeline for a superseded commit.
	gitLabRetrySkipSHAMismatch = "pipeline_sha_does_not_match_run_head_sha"
	// gitLabRetrySkipHeadSHAUnreadable means a run owns the ref but its
	// recorded head SHA could not be READ — a repository fault, not a
	// judgement about the pipeline. It is deliberately distinct from
	// gitLabRetrySkipSHAMismatch: reporting a lookup failure as a mismatch
	// tells an operator the pipeline was stale when in fact Fishhawk never
	// learned what the run's head was, and the retry that should have
	// happened would be indistinguishable from one correctly declined
	// (BC1 / #2860). A mismatch is terminal; this one is worth re-driving.
	gitLabRetrySkipHeadSHAUnreadable = "run_head_sha_lookup_failed"
	// gitLabRetrySkipCancelledLineage means the correlated run's lineage was
	// manually stopped; the GitHub path refuses the same case.
	gitLabRetrySkipCancelledLineage = "run_lineage_cancelled"
	// gitLabRetrySkipNoRetryPolicy means the correlated run's cached spec
	// could not be read, so the retry cap is unknown and a retry would risk a
	// runaway loop.
	gitLabRetrySkipNoRetryPolicy = "retry_policy_unresolvable_from_cached_spec"
	// gitLabRetrySkipGitLabUnconfigured means the deployment has no GitLab
	// pipeline trigger wired: the dispatcher's injected trigger is nil and
	// forge.Get("gitlab") resolved nothing (or the gitlab_ci registry entry is
	// absent / not a *runnerbackend.GitLabCI). It pre-empts
	// GitLabCI.TriggerStage's FIRST warn-skip branch (E45.25 / #2876).
	gitLabRetrySkipGitLabUnconfigured = "gitlab_pipeline_trigger_unconfigured"
	// gitLabRetrySkipNoCredentialScope means the correlated parent carries a
	// nil or empty installation_ref, so gitLabRunScope returns the zero scope
	// and no pipeline can be created under it. It pre-empts
	// GitLabCI.TriggerStage's SECOND warn-skip branch (p.Scope.IsZero()).
	//
	// Deliberately DISTINCT from gitLabRetrySkipGitLabUnconfigured: that one is
	// a deployment misconfiguration an operator fixes once (wire the GitLab
	// forge credentials); this one is a per-run legacy-row property fixed by
	// backfilling THAT row's installation_ref — the shape a row minted by the
	// dormant #1861 plumbing before migration 0076 and missed by its backfill
	// carries. Collapsing them would send an operator to the wrong remedy.
	gitLabRetrySkipNoCredentialScope = "run_has_no_credential_scope"
)

// gitLabRetryCandidateLimit bounds the candidate window. A retry lineage is a
// handful of runs; 25 matches the GitHub path's ListRuns limit.
const gitLabRetryCandidateLimit = 25

// gitLabRunBranch derives the ADR-035 sole-writer branch a run's stages
// execute on. It MUST stay byte-identical to orchestrator.runBranchRef, which
// is what the gitlab_ci backend actually targets — a divergence would make
// every correlation miss. Duplicated rather than imported because the
// orchestrator imports this direction, not the reverse; the drift is pinned by
// TestGitLabRunBranch_MatchesOrchestratorDerivation.
func gitLabRunBranch(r *run.Run) string {
	if r.DecomposedFrom != nil && r.SliceIndex != nil {
		return fmt.Sprintf("%s%s/slice-%d", gitLabRunBranchNamespace,
			shortRunID(*r.DecomposedFrom), *r.SliceIndex)
	}
	return gitLabRunBranchNamespace + shortRunID(r.ID)
}

// shortRunID is the first 8 characters of a run UUID's string form — the
// branch-name shortener the orchestrator and the runner both use.
func shortRunID(id uuid.UUID) string {
	s := id.String()
	if len(s) < 8 {
		return s
	}
	return s[:8]
}

// handleGitLabCIRetry creates a follow-up implement run when a GitLab pipeline
// fails on a Fishhawk-managed run branch. See the package-level contract above
// for the correlation and deferral rules.
func (d *Dispatcher) handleGitLabCIRetry(ctx context.Context, ev Event, m Match) error {
	now := d.now()

	// Step 0: AUTHORIZATION. First statement in the function, exactly as on
	// the create path: everything below reads Fishhawk state keyed on the
	// payload's project and can end in a credentialed pipeline trigger, so an
	// unauthorized delivery must fall out here having created no child, no
	// stage and no forge call. See the package contract above for why (ref,
	// sha) correlation is a blast-radius bound and not a substitute.
	if !d.authorizeGitLabProject(ctx, ev, m, gitLabAuthzPathCIRetry, now) {
		return nil
	}

	if m.PipelineRef == nil {
		d.logger().LogAttrs(ctx, slog.LevelWarn,
			"gitlab_ci_retry: missing PipelineRef",
			slog.String("delivery_id", ev.DeliveryID))
		return nil
	}
	pr := m.PipelineRef

	candidates, err := d.gitLabRetryCandidates(ctx, ev, pr)
	if err != nil {
		d.logger().LogAttrs(ctx, slog.LevelWarn,
			"gitlab_ci_retry: candidate lookup failed",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("error", err.Error()))
		return nil
	}
	if len(candidates) == 0 {
		// Nothing to chain an audit entry to; the structured log is the
		// whole record. This is the "GitLab project Fishhawk does not manage"
		// case, not a Fishhawk failure.
		d.logger().LogAttrs(ctx, slog.LevelDebug,
			"gitlab_ci_retry: no gitlab_ci runs for this project",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("repo", ev.Repo))
		return nil
	}

	// THE DEFERRAL. Checked before the per-candidate walk so the reason names
	// the first-stage case specifically rather than degrading into the
	// generic "no run owns this ref". The entry is GLOBAL-chained: a
	// default-ref pipeline belongs to no run by definition, so there is no
	// run it could honestly be attributed to.
	if !strings.HasPrefix(pr.Ref, gitLabRunBranchNamespace) {
		// The DEFERRAL is unchanged; only the reason is selected. A pipeline
		// identifiable as a merge-request pipeline is not the first-stage case,
		// and saying so would be affirmatively wrong (E45.30 / #2881).
		reason := gitLabRetrySkipFirstStageRef
		if isGitLabMergeRequestPipeline(pr) {
			reason = gitLabRetrySkipMergeRequestRef
		}
		d.writeGitLabRetrySkippedAudit(ctx, ev, nil, pr, reason, now)
		return nil
	}

	// anchor is the run the pipeline's ref belongs to, or nil when none does.
	// It is the ONLY run a skip entry may be chained to.
	parent, anchor, reason := d.correlateGitLabPipeline(ctx, candidates, pr)
	if parent == nil {
		d.writeGitLabRetrySkippedAudit(ctx, ev, anchor, pr, reason, now)
		return nil
	}
	if parent.State == run.StateCancelled {
		d.writeGitLabRetrySkippedAudit(ctx, ev, parent, pr, gitLabRetrySkipCancelledLineage, now)
		return nil
	}

	workflow, maxRetries, ok := d.resolveRetryPolicy(ctx, parent)
	if !ok {
		d.writeGitLabRetrySkippedAudit(ctx, ev, parent, pr, gitLabRetrySkipNoRetryPolicy, now)
		return nil
	}
	if parent.RetryAttempt >= maxRetries {
		d.writeCIRetryExhaustedAudit(ctx, ev, parent, &CheckRunRef{
			CheckName:  gitLabPipelineCheckName(pr),
			Conclusion: pr.Status,
			HeadSHA:    pr.SHA,
		}, maxRetries, now)
		return nil
	}

	// PRE-FLIGHT DISPATCHABILITY (E45.25 / #2876). Both of
	// runnerbackend.GitLabCI.TriggerStage's fail-closed warn-skip branches
	// return nil, which the code below cannot distinguish from a created
	// pipeline — so without this guard a nil trigger or a zero credential scope
	// transitioned the first retry stage to `dispatched` and audited outcome
	// `dispatched` with nothing behind it. Refused HERE, before
	// run.ChildParamsFrom and therefore before ANY mutation: no child run, no
	// stage rows, no retry-index slot consumed.
	//
	// ONE RESOLUTION, ONE INSTANCE. The registry is built once and the guard
	// reads the resolved instance's OWN Trigger field — the very field
	// TriggerStage will consult on the very instance dispatched below. Calling
	// d.gitLabTrigger() separately would work today (backends() constructs the
	// gitlab_ci entry from it) but each call independently re-enters the
	// process-global forge.Get("gitlab") when d.GitLabTrigger is nil, so
	// agreement would rest on registry stability rather than construction.
	// A nil `gl` (absent, or a non-*GitLabCI registry entry) folds into the
	// unconfigured reason: neither state can demonstrate a wired trigger, and
	// falling through would call TriggerStage on a nil interface.
	glBackend, _ := d.backends().Backend(run.RunnerKindGitLabCI)
	gl, _ := glBackend.(*runnerbackend.GitLabCI)
	// Resolved once and reused for the dispatch below, so the value this guard
	// cleared is byte-identically the value that is dispatched. Checked in the
	// SAME order TriggerStage checks them, so each reason names the branch it
	// pre-empts.
	scope := gitLabRunScope(parent)
	switch {
	case gl == nil || gl.Trigger == nil:
		d.writeGitLabRetrySkippedAudit(ctx, ev, parent, pr, gitLabRetrySkipGitLabUnconfigured, now)
		return nil
	case scope.IsZero():
		d.writeGitLabRetrySkippedAudit(ctx, ev, parent, pr, gitLabRetrySkipNoCredentialScope, now)
		return nil
	}

	params := run.ChildParamsFrom(parent)
	// BC3: retry_attempt is derived from the PARENT row, never from the
	// latest existing child. Two concurrent deliveries for one parent
	// therefore compute the SAME value and collide on
	// runs_retry_child_once_idx — a child-derived value would make them
	// disagree and the index would silently never fire.
	params.RetryAttempt = parent.RetryAttempt + 1
	child, err := d.Runs.CreateRun(ctx, params)
	if run.IsRetryChildDuplicate(err) {
		d.logger().LogAttrs(ctx, slog.LevelInfo,
			"gitlab_ci_retry: concurrent delivery already created this retry child",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("parent_run_id", parent.ID.String()),
			slog.Int("retry_attempt", params.RetryAttempt))
		return nil
	}
	if err != nil {
		return fmt.Errorf("dispatcher: create gitlab retry run: %w", err)
	}

	retryStages := FilterOutPlanStages(workflow.Stages)
	if len(retryStages) == 0 {
		d.logger().LogAttrs(ctx, slog.LevelWarn,
			"gitlab_ci_retry: no non-plan stages to retry against",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("run_id", child.ID.String()))
		return nil
	}
	stages, err := CreateStagesFromSpec(ctx, d.Runs, child.ID, retryStages)
	if err != nil {
		return fmt.Errorf("dispatcher: create gitlab retry stages: %w", err)
	}

	firstStage := stages[0]
	// glBackend / scope come from the pre-flight guard above: one registry
	// build, one trigger resolution, one instance. With both warn-skip
	// preconditions refused upstream, a nil return here genuinely means a
	// pipeline was created — which is what makes the `dispatched` transition
	// and audit below truthful.
	dispatchErr := glBackend.TriggerStage(ctx, runnerbackend.TriggerParams{
		RunID:            child.ID,
		StageID:          firstStage.ID,
		WorkflowID:       parent.WorkflowID,
		StageExecutorRef: firstStage.ExecutorRef,
		Repo:             ev.Repo,
		Scope:            scope,
		Ref:              gitLabRunBranch(child),
	})
	if dispatchErr == nil {
		if _, err := d.Runs.TransitionStage(ctx, firstStage.ID,
			run.StageStateDispatched, nil); err != nil {
			d.logger().LogAttrs(ctx, slog.LevelWarn,
				"gitlab_ci_retry: transition stage to dispatched failed",
				slog.String("delivery_id", ev.DeliveryID),
				slog.String("stage_id", firstStage.ID.String()),
				slog.String("error", err.Error()))
		}
	}

	d.writeGitLabCIRetryDispatchedAudit(ctx, ev, parent, child, pr, maxRetries, dispatchErr, now)
	d.logger().LogAttrs(ctx, slog.LevelInfo, "gitlab_ci_retry: dispatched retry run",
		slog.String("delivery_id", ev.DeliveryID),
		slog.String("parent_run_id", parent.ID.String()),
		slog.String("run_id", child.ID.String()),
		slog.Int("retry_attempt", child.RetryAttempt),
		slog.Int("pipeline_id", pr.PipelineID),
		slog.String("pipeline_ref", pr.Ref),
	)
	return nil
}

// gitLabRetryCandidates lists the gitlab_ci runs on this project that a
// failing pipeline could belong to, NARROWED by merge-request iid when the
// pipeline carries one and a candidate records a matching merge-request URL.
// Narrowing is best-effort by design: a run that has not opened its MR yet
// carries no URL and stays in the set, because dropping it would make the
// correlation depend on artifact timing.
func (d *Dispatcher) gitLabRetryCandidates(ctx context.Context, ev Event, pr *PipelineRef) ([]*run.Run, error) {
	if d.Runs == nil || ev.Repo == "" {
		return nil, nil
	}
	kind := run.RunnerKindGitLabCI
	runs, err := d.Runs.ListRuns(ctx, run.ListRunsFilter{
		Repo:       ev.Repo,
		RunnerKind: &kind,
		Limit:      gitLabRetryCandidateLimit,
	})
	if err != nil {
		return nil, err
	}
	if pr.MergeRequestIID == 0 {
		return runs, nil
	}
	suffix := fmt.Sprintf("/merge_requests/%d", pr.MergeRequestIID)
	narrowed := make([]*run.Run, 0, len(runs))
	for _, r := range runs {
		if r.PullRequestURL == nil || strings.HasSuffix(*r.PullRequestURL, suffix) {
			narrowed = append(narrowed, r)
		}
	}
	if len(narrowed) == 0 {
		return runs, nil
	}
	return narrowed, nil
}

// correlateGitLabPipeline selects the ONE run a failing pipeline belongs to.
// It returns (match, anchor, "") on a hit and (nil, anchor, reason) naming why
// none matched. Both halves of the predicate are required: the ref identifies
// the run, the SHA proves the pipeline is for the commit that run actually
// produced.
//
// anchor is the ref-owning run — the only run a skip entry may honestly be
// chained to — and is nil when no candidate owns the ref at all.
func (d *Dispatcher) correlateGitLabPipeline(ctx context.Context, candidates []*run.Run,
	pr *PipelineRef) (*run.Run, *run.Run, string) {
	var refOwner *run.Run
	// headUnreadable records that a ref-owner's head SHA could not be READ.
	// Without it the caller would report a repository fault as a SHA
	// MISMATCH — a terminal-sounding verdict about a comparison that never
	// actually happened.
	headUnreadable := false
	for _, r := range candidates {
		if gitLabRunBranch(r) != pr.Ref {
			continue
		}
		if refOwner == nil {
			refOwner = r
		}
		headSHA, err := d.runHeadSHA(ctx, r.ID)
		if err != nil {
			headUnreadable = true
			d.logger().LogAttrs(ctx, slog.LevelWarn,
				"gitlab_ci_retry: head sha lookup failed",
				slog.String("run_id", r.ID.String()),
				slog.String("error", err.Error()))
			continue
		}
		if headSHA != "" && headSHA == pr.SHA {
			return r, r, ""
		}
	}
	switch {
	case refOwner == nil:
		return nil, nil, gitLabRetrySkipNoRunForRef
	case headUnreadable:
		return nil, refOwner, gitLabRetrySkipHeadSHAUnreadable
	default:
		return nil, refOwner, gitLabRetrySkipSHAMismatch
	}
}

// gitLabRunScope resolves the credential scope a retry child dispatches under,
// preferring the parent's forge-neutral installation_ref (migration 0076).
// Mirrors orchestrator.runCredentialScope's ref-first ladder.
func gitLabRunScope(parent *run.Run) forge.CredentialScope {
	if parent.InstallationRef != nil && *parent.InstallationRef != "" {
		return forge.FromRef(*parent.InstallationRef)
	}
	return forge.CredentialScope{}
}

// gitLabPipelineCheckName renders a stable, human-readable identifier for the
// failing pipeline, for audit payloads shaped like the GitHub path's
// check_name.
func gitLabPipelineCheckName(pr *PipelineRef) string {
	return fmt.Sprintf("gitlab-pipeline-%d", pr.PipelineID)
}

// writeGitLabCIRetryDispatchedAudit records the dispatch, carrying the
// pipeline id as correlation provenance so an operator can join the Fishhawk
// retry back to the GitLab pipeline that caused it.
func (d *Dispatcher) writeGitLabCIRetryDispatchedAudit(ctx context.Context, ev Event,
	parent, child *run.Run, pr *PipelineRef, maxRetries int, dispatchErr error, now time.Time) {
	systemKind := audit.ActorKind("system")
	outcome := "dispatched"
	if dispatchErr != nil {
		outcome = "dispatch_failed"
	}
	payload := map[string]any{
		"event":             ev.Type,
		"delivery_id":       ev.DeliveryID,
		"repo":              ev.Repo,
		"parent_run_id":     parent.ID.String(),
		"child_run_id":      child.ID.String(),
		"pipeline_id":       pr.PipelineID,
		"pipeline_ref":      pr.Ref,
		"head_sha":          pr.SHA,
		"merge_request_iid": pr.MergeRequestIID,
		"retry_attempt":     child.RetryAttempt,
		"max_retries":       maxRetries,
		"outcome":           outcome,
		"runner_kind":       parent.RunnerKind,
	}
	if dispatchErr != nil {
		payload["error"] = dispatchErr.Error()
	}
	body, _ := json.Marshal(payload)
	if _, err := d.Audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        child.ID,
		Timestamp:    now,
		Category:     "ci_failure_retry_dispatched",
		ActorKind:    &systemKind,
		ActorSubject: stringPtr("gitlab-webhook"),
		Payload:      body,
	}); err != nil {
		d.logger().LogAttrs(ctx, slog.LevelError, "gitlab_ci_retry: audit append failed",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("error", err.Error()))
	}
}

// writeGitLabRetrySkippedAudit records a DELIBERATE non-retry with its named
// reason (BC1). Without it, a first-stage deferral and a mis-correlation would
// both look like nothing happening, and nobody debugging a missing retry could
// tell which one they were looking at (#2860).
//
// ATTRIBUTION. anchor is the run that OWNS the pipeline's ref, or nil when no
// candidate does. A nil anchor writes a GLOBAL-chained entry rather than
// hanging the record off an unrelated run: the earlier shape chained to "the
// newest gitlab_ci run for the project", so every failed default-branch
// pipeline in a project — any push to main whose CI fails — deposited a
// ci_retry_skipped row into the audit stream of whichever run happened to be
// newest, an event that run had no part in. The entry stays on ONE category
// either way, so the stream remains awaitable; only its chain differs.
func (d *Dispatcher) writeGitLabRetrySkippedAudit(ctx context.Context, ev Event, anchor *run.Run,
	pr *PipelineRef, reason string, now time.Time) {
	systemKind := audit.ActorKind("system")
	fields := map[string]any{
		"event":             ev.Type,
		"delivery_id":       ev.DeliveryID,
		"repo":              ev.Repo,
		"reason":            reason,
		"pipeline_id":       pr.PipelineID,
		"pipeline_ref":      pr.Ref,
		"head_sha":          pr.SHA,
		"merge_request_iid": pr.MergeRequestIID,
	}
	runID := "" // "" renders as an absent run in the log line below.
	if anchor != nil {
		runID = anchor.ID.String()
		fields["run_id"] = runID
		fields["run_branch"] = gitLabRunBranch(anchor)
	}
	payload, _ := json.Marshal(fields)

	var err error
	if anchor != nil {
		_, err = d.Audit.AppendChained(ctx, audit.ChainAppendParams{
			RunID:        anchor.ID,
			Timestamp:    now,
			Category:     "ci_retry_skipped",
			ActorKind:    &systemKind,
			ActorSubject: stringPtr("gitlab-webhook"),
			Payload:      payload,
		})
	} else {
		_, err = d.Audit.AppendGlobalChained(ctx, audit.GlobalChainAppendParams{
			Timestamp:    now,
			Category:     "ci_retry_skipped",
			ActorKind:    &systemKind,
			ActorSubject: stringPtr("gitlab-webhook"),
			Payload:      payload,
		})
	}
	if err != nil {
		d.logger().LogAttrs(ctx, slog.LevelError, "gitlab_ci_retry: skip audit append failed",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("error", err.Error()))
	}
	d.logger().LogAttrs(ctx, slog.LevelInfo, "gitlab_ci_retry: not retrying",
		slog.String("delivery_id", ev.DeliveryID),
		slog.String("run_id", runID),
		slog.String("reason", reason),
		slog.String("pipeline_ref", pr.Ref),
		slog.Int("pipeline_id", pr.PipelineID),
	)
}
