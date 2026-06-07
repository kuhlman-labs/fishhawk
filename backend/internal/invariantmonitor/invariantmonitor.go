// Package invariantmonitor runs the background ticker that asserts
// the loop's self-consistency invariants and surfaces violations
// truthfully (#764). It generalizes the one-off startup recovery
// orchestrator.ReconcileStuckRuns (#727) into a periodic sweep over
// two classes of loop-state inconsistency:
//
//   - Invariant 1 — {all stages terminal, run non-terminal}. The
//     SAFE, auto-reconcilable class: a run whose every stage has
//     reached a terminal state but whose run row is still running was
//     left behind by a transition that didn't complete the run. The
//     ticker delegates to Reconcile (wired to ReconcileStuckRuns),
//     which advances such runs through completeRun. Idempotent.
//
//   - Invariant 2 — {review stage awaiting_approval, null/empty
//     pull_request_url} on a run that INTENDED to open a PR. The
//     surface-only class: a push-and-open-pr run parked at its review
//     gate with no PR URL can never auto-resolve (the missing PR is
//     the unrecoverable fact — #742), so the ticker detects, audits,
//     and logs it for operator action and mutates nothing.
//
// Invariant 2 fires ONLY for runs whose workflow actually opens a PR.
// A null PR is the LEGITIMATE normal state for a workflow whose review
// stage has no PR step (commit-yourself / non-PR workflows); flagging
// those would emit false-positive violations for every such run and
// undermine the monitor's whole purpose. The PR intent is read from
// the run's cached WorkflowSpec (a stage that produces a pull_request
// artifact); a run whose intent can't be determined is NOT flagged.
//
// Mirrors the dispatchwatchdog.Ticker shape: Run blocks on ctx, fires
// Tick immediately then on Interval, and Tick is exported so
// deterministic-clock tests can drive it step-by-step. Per-run errors
// are best-effort/logged and never abort the sweep.
package invariantmonitor

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// CategoryInvariantViolation is the audit-log category the monitor
// writes when it detects a loop-state inconsistency. Stable so log
// scrapers and the compliance export can index on it. It is an
// internal, system-actor audit kind — NOT an issue-comment surface
// (see docs/issue-comment-surfaces.md).
const CategoryInvariantViolation = "invariant_violation"

// KindReviewAwaitingApprovalNullPR is the invariant-2 violation kind
// recorded in the audit payload's `kind` field: a review stage parked
// in awaiting_approval on a push-and-open-pr run with no PR URL.
const KindReviewAwaitingApprovalNullPR = "review_awaiting_approval_null_pr"

// listRunsPageSize bounds each ListRuns page the sweep walks. Matches
// the constant ReconcileStuckRuns uses — at v0 scale the running-run
// set is tiny; this only bounds memory if it ever grows.
const listRunsPageSize = 100

// Ticker periodically asserts the loop's self-consistency invariants.
// Run() blocks until ctx is done; for production wiring start it on
// its own goroutine via the server config.
type Ticker struct {
	// Runs pages running runs and lists their stages. Required.
	Runs run.Repository

	// Audit appends the invariant_violation entry per detected
	// inconsistency. Required.
	Audit audit.Repository

	// Reconcile auto-resolves the safe invariant-1 class. Wired to
	// orchestrator.ReconcileStuckRuns. Optional; nil skips invariant 1
	// (invariant 2 still runs).
	Reconcile func(ctx context.Context) (int, error)

	// Logger receives the WARN lines for detected violations and the
	// best-effort per-run error logs. nil → slog.Default().
	Logger *slog.Logger

	// Interval is the tick period. Defaults to 60s when zero. The
	// ticker fires immediately on Run() start; the interval gates
	// subsequent ticks.
	Interval time.Duration

	// Now is the clock used for the audit timestamp. Tests inject a
	// fake clock; production leaves it nil for time.Now.
	Now func() time.Time
}

// Run drives the ticker until ctx is cancelled.
func (t *Ticker) Run(ctx context.Context) error {
	if t.Runs == nil {
		return errors.New("invariantmonitor: ticker requires Runs")
	}
	if t.Audit == nil {
		return errors.New("invariantmonitor: ticker requires Audit")
	}
	interval := t.Interval
	if interval <= 0 {
		interval = 60 * time.Second
	}

	t.Tick(ctx)

	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			t.Tick(ctx)
		}
	}
}

// Tick performs one pass: reconcile invariant 1, then sweep running
// runs for invariant-2 violations. Exposed for deterministic tests.
func (t *Ticker) Tick(ctx context.Context) {
	logger := t.logger()

	// Invariant 1: auto-reconcile the safe {all stages terminal, run
	// non-terminal} class. A reconcile failure is systemic and logged
	// but never blocks the invariant-2 sweep.
	if t.Reconcile != nil {
		if n, err := t.Reconcile(ctx); err != nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "invariantmonitor: reconcile failed",
				slog.String("error", err.Error()))
		} else if n > 0 {
			logger.LogAttrs(ctx, slog.LevelInfo, "invariantmonitor: reconciled stuck runs",
				slog.Int("advanced", n))
		}
	}

	// Invariant 2: surface-only detection over running runs.
	offset := 0
	for {
		runs, err := t.Runs.ListRuns(ctx, run.ListRunsFilter{
			State:  string(run.StateRunning),
			Limit:  listRunsPageSize,
			Offset: offset,
		})
		if err != nil {
			// A paging failure is systemic (not specific to one run),
			// so abort this sweep — best-effort applies per-run, not to
			// the listing itself.
			logger.LogAttrs(ctx, slog.LevelWarn, "invariantmonitor: list runs failed",
				slog.String("error", err.Error()))
			return
		}
		if len(runs) == 0 {
			break
		}
		for _, r := range runs {
			t.checkReviewPRInvariant(ctx, logger, r)
		}
		if len(runs) < listRunsPageSize {
			break
		}
		offset += len(runs)
	}
}

// checkReviewPRInvariant flags a single run if it is parked at a review
// gate with no PR URL despite intending to open one. Best-effort: a
// stage-list error is logged and skipped, never aborting the sweep.
func (t *Ticker) checkReviewPRInvariant(ctx context.Context, logger *slog.Logger, r *run.Run) {
	// A null/empty PR is only a violation when the run actually
	// intended to open one. A non-push-and-open-pr workflow parks its
	// review stage with a null PR as its LEGITIMATE normal state, so
	// flagging it would cry wolf (#764, condition 1). Skip early.
	if !runIntendsPR(r) {
		return
	}
	if r.PullRequestURL != nil && *r.PullRequestURL != "" {
		return
	}

	stages, err := t.Runs.ListStagesForRun(ctx, r.ID)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "invariantmonitor: skipped run on stage-list error",
			slog.String("run_id", r.ID.String()),
			slog.String("error", err.Error()))
		return
	}

	hasParkedReview := false
	for _, s := range stages {
		if s.Type == run.StageTypeReview && s.State == run.StageStateAwaitingApproval {
			hasParkedReview = true
			break
		}
	}
	if !hasParkedReview {
		return
	}

	now := time.Now().UTC()
	if t.Now != nil {
		now = t.Now().UTC()
	}

	logger.LogAttrs(ctx, slog.LevelWarn, "invariantmonitor: invariant violation",
		slog.String("kind", KindReviewAwaitingApprovalNullPR),
		slog.String("run_id", r.ID.String()),
		slog.Bool("reconciled", false),
	)

	payload, _ := json.Marshal(map[string]any{
		"kind":       KindReviewAwaitingApprovalNullPR,
		"run_id":     r.ID.String(),
		"reconciled": false,
	})
	systemKind := audit.ActorSystem
	if _, err := t.Audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     r.ID,
		Timestamp: now,
		Category:  CategoryInvariantViolation,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "invariantmonitor: audit append failed",
			slog.String("run_id", r.ID.String()),
			slog.String("error", err.Error()))
	}
}

// runIntendsPR reports whether the run's workflow opens a pull request
// — i.e. some stage in the workflow keyed by r.WorkflowID produces a
// `pull_request` artifact. This is the authoritative push-and-open-pr
// signal. A run whose WorkflowSpec is absent (legacy rows) or fails to
// parse returns false: without proof the run meant to open a PR, the
// monitor stays silent rather than emitting a false-positive (#764).
func runIntendsPR(r *run.Run) bool {
	if len(r.WorkflowSpec) == 0 {
		return false
	}
	s, err := spec.ParseBytes(r.WorkflowSpec)
	if err != nil {
		return false
	}
	wf, ok := s.Workflows[r.WorkflowID]
	if !ok {
		return false
	}
	for _, st := range wf.Stages {
		for _, p := range st.Produces {
			if p.Artifact == spec.ArtifactPullRequest {
				return true
			}
		}
	}
	return false
}

func (t *Ticker) logger() *slog.Logger {
	if t.Logger != nil {
		return t.Logger
	}
	return slog.Default()
}
