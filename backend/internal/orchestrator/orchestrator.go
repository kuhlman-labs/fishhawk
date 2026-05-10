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
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
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
}

// Orchestrator wires the run repository to a GitHub client to
// advance a run's stages. Construct directly via the public fields;
// every dependency is required (the orchestrator no-ops if any is
// nil, but production callers should always wire all four).
type Orchestrator struct {
	Runs   run.Repository
	GitHub GitHubAPI
	Logger *slog.Logger

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

	return o.dispatchStage(ctx, r, next)
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
	return OutcomeRunCompleted, nil
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
