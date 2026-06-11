// Package mergereconciler runs the background ticker that resolves a
// run's review gate on a VERIFIED PR merge state (ADR-031 Phase 1).
//
// The pull_request.closed webhook is the primary signal that advances a
// review-gated run to its terminal state (merged -> succeeded,
// closed-unmerged -> cancelled per ADR-018). But webhook delivery is
// best-effort: a missed or dropped delivery leaves the run parked at
// review awaiting_approval indefinitely with no operator-visible
// recovery. This ticker is the catch-net — it walks the review stages
// parked in awaiting_approval, reads each run's live PR state from the
// GitHub REST API, and resolves the gate through the SAME path the
// webhook uses (server.ResolveReviewFromPollState). Because both
// surfaces share one resolution method and TransitionStage is a no-op
// on an already-terminal stage, the poll is idempotent against the
// webhook: whichever fires first wins, the other is absorbed.
//
// The reconciler resolves ONLY on a terminal PR state — pr.Merged
// (succeeded) or state=="closed" && !merged (cancelled). An open PR is
// left parked. There is no force-succeed path: ADR-018's "the merge
// event is what advances the stage" invariant is honored, the poll just
// supplies the merge signal the webhook would have.
//
// Mirrors the dispatchwatchdog / reactionpoller ticker shape: Run()
// blocks until ctx is cancelled; for production wiring start it on its
// own goroutine via the server config. OFF BY DEFAULT in fishhawkd
// (--enable-merge-reconciler) so a v0 deployment that doesn't need it
// doesn't pay the poll cost.
//
// The tick doubles as the self-heal for the fishhawk_audit_complete
// Check Run publish (#973): each parked review stage also gets a
// recompute+republish via the optional AuditCheckRepublisher. The
// publisher's dedup cache records only on a SUCCESSFUL publish, so the
// sweep retries exactly the publishes a transient GitHub failure
// dropped, and an already-published state dedups to a no-op.
//
// Rate-limit note (ADR-031 Phase 1, low severity): each tick makes one
// synchronous GetPullRequest call per parked review stage with NO
// per-stage cooldown — unlike reactionpoller's adaptive fast/slow
// cadence — plus, when AuditCheckRepublisher is wired, up to one MORE
// GetPullRequest inside the audit-complete recompute (the auditcomplete
// PRHead foreign-commit rule), roughly doubling the per-tick REST cost.
// This is acceptable at v0 scale (a handful of concurrently parked
// review stages), but an operator enabling this at scale should tune
// --merge-reconciler-interval upward: N parked stages cost up to 2N
// REST calls every interval, against GitHub's 5,000/hour
// per-installation budget. A future phase may add adaptive cadence + a
// per-stage last-polled gate (cf. reactionpoller) if the call volume
// warrants it.
package mergereconciler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// DefaultInterval is the tick period fishhawkd applies when the caller
// leaves Interval zero.
const DefaultInterval = 60 * time.Second

// PRGetter reads a single pull request's live state from GitHub.
// Satisfied by *githubclient.Client. Tests inject a stub returning
// canned PR states.
type PRGetter interface {
	GetPullRequest(ctx context.Context, installationID int64, repo githubclient.RepoRef, number int) (*githubclient.PullRequest, error)
}

// Resolver resolves a run's review stage from the poll's terminal PR
// state, routing through the same path the pull_request.closed webhook
// uses. Satisfied by *server.Server (ResolveReviewFromPollState).
type Resolver interface {
	ResolveReviewFromPollState(ctx context.Context, runID uuid.UUID, merged bool, prURL string) error
}

// LineageReverifier re-checks a merged run branch's lineage at merge
// resolution (ADR-035 second line of defense, #862) before the gate is
// resolved succeeded. clean=false means a foreign commit rode the merged
// branch — the run is left parked/flagged rather than landing as
// succeeded. Satisfied by *server.Server (ReverifyBranchLineage).
//
// OPTIONAL — unlike Runs/PRGetter/Resolver this is not required by Run().
// A nil reverifier preserves the pre-#862 behavior (resolve every verified
// merge with no re-check).
type LineageReverifier interface {
	ReverifyBranchLineage(ctx context.Context, runID uuid.UUID, prNumber int) (clean bool)
}

// AuditCheckRepublisher re-derives and republishes the
// fishhawk_audit_complete Check Run for a run (#973). Satisfied by
// *server.Server (RepublishAuditCheck). The implementation is
// best-effort (failures log, never propagate) and dedups
// already-published states, so calling it every tick for every parked
// stage is cheap and idempotent.
//
// OPTIONAL — like LineageReverifier, not required by Run(). A nil
// republisher preserves the pre-#973 behavior (no publish heal; the
// Check Run is only published when an HTTP/webhook surface recomputes
// it).
type AuditCheckRepublisher interface {
	RepublishAuditCheck(ctx context.Context, runID uuid.UUID)
}

// Ticker scans review stages parked in awaiting_approval and resolves
// any whose PR has reached a terminal merge state. Run() blocks until
// ctx is done.
type Ticker struct {
	// Runs lists awaiting-approval review stages (via the dedicated,
	// SLA-independent ListReviewStagesAwaitingApproval — NOT the SLA
	// ticker's gate_sla-filtered query, which hides SLA-less review
	// gates; #725) and reads the run row (for installation_id +
	// pull_request_url). Required.
	Runs run.Repository

	// PRGetter reads live PR state from GitHub. Required.
	PRGetter PRGetter

	// Resolver resolves the review stage through the shared
	// webhook+poll path. Required.
	Resolver Resolver

	// LineageReverifier re-checks a verified merge's run-branch lineage
	// before the succeeded-resolve (ADR-035, #862). OPTIONAL: nil
	// preserves today's behavior (no re-check). A non-clean verdict on a
	// merged PR leaves the run parked/flagged for #867 instead of
	// resolving it succeeded.
	LineageReverifier LineageReverifier

	// AuditCheckRepublisher heals dropped fishhawk_audit_complete Check
	// Run publishes (#973): invoked once per parked review stage per
	// tick, BEFORE the PR poll so a GitHub poll failure cannot skip the
	// heal. OPTIONAL: nil preserves the pre-#973 ticker behavior.
	AuditCheckRepublisher AuditCheckRepublisher

	// Logger receives structured warnings about transient errors.
	// nil → slog.Default().
	Logger *slog.Logger

	// Interval is the tick period. Defaults to DefaultInterval when
	// zero. The ticker fires immediately on Run() start; the interval
	// gates subsequent ticks.
	Interval time.Duration

	// Now is reserved for future cadence accounting; unused today.
	// Tests may set it; production leaves it nil.
	Now func() time.Time
}

// Run drives the ticker until ctx is cancelled. Each tick lists
// awaiting-approval review stages and reconciles any whose PR has
// reached a terminal merge state. Per-stage errors log but don't
// abort the loop.
func (t *Ticker) Run(ctx context.Context) error {
	if t.Runs == nil {
		return errors.New("mergereconciler: ticker requires Runs")
	}
	if t.PRGetter == nil {
		return errors.New("mergereconciler: ticker requires PRGetter")
	}
	if t.Resolver == nil {
		return errors.New("mergereconciler: ticker requires Resolver")
	}
	interval := t.Interval
	if interval <= 0 {
		interval = DefaultInterval
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

// Tick performs one pass over awaiting-approval review stages. Exposed
// for tests so the deterministic scenarios can drive the ticker
// step-by-step without spinning real timers.
func (t *Ticker) Tick(ctx context.Context) {
	logger := t.Logger
	if logger == nil {
		logger = slog.Default()
	}

	stages, err := t.Runs.ListReviewStagesAwaitingApproval(ctx)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "mergereconciler: list awaiting stages failed",
			slog.String("error", err.Error()))
		return
	}

	for _, s := range stages {
		if s.Type != run.StageTypeReview {
			// Defense-in-depth: ListReviewStagesAwaitingApproval already
			// filters to stage_type = 'review', so this is dead under the
			// real query. Kept so a fake or future listing that returns a
			// non-review stage can't reach reconcileStage.
			continue
		}
		t.reconcileStage(ctx, logger, s)
	}
}

// reconcileStage reads one parked review stage's live PR state and
// resolves the gate when the PR has reached a terminal merge state.
// Skips cleanly (no transition) when the run has no installation or no
// PR URL, when the PR URL is malformed, or when the PR is still open.
// Per-row errors log but don't propagate.
//
// Each call issues one synchronous GetPullRequest (two when the
// optional AuditCheckRepublisher recompute consults the auditcomplete
// PRHead rule) — see the package doc's rate-limit note before enabling
// at scale.
func (t *Ticker) reconcileStage(ctx context.Context, logger *slog.Logger, s *run.Stage) {
	runRow, err := t.Runs.GetRun(ctx, s.RunID)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "mergereconciler: get run failed",
			slog.String("run_id", s.RunID.String()),
			slog.String("error", err.Error()))
		return
	}
	if runRow.InstallationID == nil {
		// No installation_id → no GitHub creds to poll with. Pre-existing
		// no-PR parked runs are correctly untouched here. Same skip-clean
		// posture as the reactionpoller.
		return
	}
	if runRow.PullRequestURL == nil || *runRow.PullRequestURL == "" {
		// Run never reached a PR (no implement-stage PR artifact). Nothing
		// to reconcile; leave parked.
		return
	}
	prURL := *runRow.PullRequestURL
	repo, number, err := parsePRURL(prURL)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "mergereconciler: malformed pull_request_url",
			slog.String("run_id", s.RunID.String()),
			slog.String("pr_url", prURL),
			slog.String("error", err.Error()))
		return
	}

	// Heal a dropped fishhawk_audit_complete publish (#973) before the
	// merge poll: a GetPullRequest failure (the GitHub-outage shape that
	// dropped the publish in the first place) must not also skip the
	// retry. Idempotent — the publisher dedups an already-published
	// state, so the steady-state cost is one recompute per parked stage.
	if t.AuditCheckRepublisher != nil {
		t.AuditCheckRepublisher.RepublishAuditCheck(ctx, s.RunID)
	}

	pr, err := t.PRGetter.GetPullRequest(ctx, *runRow.InstallationID, repo, number)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "mergereconciler: get pull request failed",
			slog.String("run_id", s.RunID.String()),
			slog.String("pr_url", prURL),
			slog.String("error", err.Error()))
		return
	}

	switch {
	case pr.Merged:
		// ADR-035 second line of defense (#862): before marking the run
		// succeeded, re-check the merged branch's lineage. A foreign commit
		// riding the merged branch (the #797 shape that #858 guards at the
		// report boundary) must NOT land as a succeeded run. This observes a
		// merge GitHub already performed, so it refuses the resolve and
		// leaves the run parked/flagged for #867 rather than physically
		// blocking the merge. Optional/nil-safe: a nil reverifier or a clean
		// verdict falls through to the existing resolve.
		if t.LineageReverifier != nil &&
			!t.LineageReverifier.ReverifyBranchLineage(ctx, s.RunID, number) {
			logger.LogAttrs(ctx, slog.LevelWarn,
				"mergereconciler: merged run carries a foreign commit; left parked/flagged (not resolved succeeded)",
				slog.String("run_id", s.RunID.String()),
				slog.String("pr_url", prURL))
			return
		}
		t.resolve(ctx, logger, s.RunID, true, prURL)
	case pr.State == "closed" && !pr.Merged:
		t.resolve(ctx, logger, s.RunID, false, prURL)
	default:
		// Open PR (or any non-terminal state) — leave parked. No
		// force-succeed: the merge/close event is what advances the stage.
	}
}

// resolve hands off to the shared webhook+poll resolution path and logs
// the outcome. A resolver error logs but doesn't abort the tick — a
// later tick re-polls the (still-terminal) PR and retries idempotently.
func (t *Ticker) resolve(ctx context.Context, logger *slog.Logger, runID uuid.UUID, merged bool, prURL string) {
	if err := t.Resolver.ResolveReviewFromPollState(ctx, runID, merged, prURL); err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "mergereconciler: resolve review failed",
			slog.String("run_id", runID.String()),
			slog.String("pr_url", prURL),
			slog.Bool("merged", merged),
			slog.String("error", err.Error()))
		return
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "mergereconciler: resolved review from poll",
		slog.String("run_id", runID.String()),
		slog.String("pr_url", prURL),
		slog.Bool("merged", merged))
}

// parsePRURL extracts (repo, number) from a GitHub PR html_url of the
// form https://github.com/<owner>/<repo>/pull/<n> — exactly the value
// the runner stores in runs.pull_request_url and the webhook matches on.
// Returns an error for any other shape so the caller skips cleanly
// rather than panicking.
func parsePRURL(prURL string) (githubclient.RepoRef, int, error) {
	s := strings.TrimSpace(prURL)
	for _, prefix := range []string{"https://github.com/", "http://github.com/"} {
		s = strings.TrimPrefix(s, prefix)
	}
	parts := strings.Split(strings.Trim(s, "/"), "/")
	// Expect: <owner>/<repo>/pull/<n>
	if len(parts) != 4 || parts[2] != "pull" {
		return githubclient.RepoRef{}, 0, fmt.Errorf("not a github PR html_url: %q", prURL)
	}
	owner, name, num := parts[0], parts[1], parts[3]
	if owner == "" || name == "" {
		return githubclient.RepoRef{}, 0, fmt.Errorf("PR url missing owner/name: %q", prURL)
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return githubclient.RepoRef{}, 0, fmt.Errorf("PR url has non-numeric number %q: %q", num, prURL)
	}
	return githubclient.RepoRef{Owner: owner, Name: name}, n, nil
}
