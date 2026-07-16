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
// The tick also self-heals a dropped work-item board move (#1012): for
// every parked review stage whose PR is confirmed open, the run's
// authoritative lifecycle state is in_review — the state the pr_opened
// board transition targets — so the optional BoardTransitionHealer
// re-asserts pr_opened, healing the in_review board move a missed
// pull_request.opened webhook would have dropped. The provider's
// expected-source check makes the re-assert idempotent at the board level
// (a card already in_review, or one a human parked in Blocked, is left
// untouched). Because the board hook audits every attempt — both moves and
// never-fight-the-human skips — the Ticker dedups the heal to at most once
// per run per process so a long-parked run doesn't append a skip audit
// every tick. The run_merged board move needs no heal here: it already
// rides the shared ResolveReviewFromPollState merge-resolve path.
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

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// DefaultInterval is the tick period fishhawkd applies when the caller
// leaves Interval zero.
const DefaultInterval = 60 * time.Second

// boardHealEvent is the run-lifecycle event the board-state self-heal
// re-asserts for every parked-open review stage (#1012). A parked review
// stage whose PR is open has an authoritative lifecycle state of in_review —
// the state pr_opened targets — so re-asserting pr_opened heals an in_review
// board move dropped by a missed pull_request.opened webhook. The string MUST
// match the server's lifecyclePROpened transition-event key (boardsync.go);
// the server maps it to a canonical state via the repo conventions, and the
// provider's expected-source check makes the re-assert idempotent.
const boardHealEvent = "pr_opened"

// PRGetter reads a single pull request's live state from GitHub.
// Satisfied by *githubclient.Client. Tests inject a stub returning
// canned PR states.
type PRGetter interface {
	GetPullRequestScoped(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, number int) (*githubclient.PullRequest, error)
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

// DriveObserver evaluates one parked review stage of a drive-enabled
// run against the poll-driven mechanical drive rules (#1023):
// reviews_settled_gate and the derived checks_green_awaiting_merge.
// Satisfied by *server.Server (ObserveParkedReviewForDrive). The
// implementation is best-effort and idempotent (each rule is stamped
// at most once per stage), so calling it every tick is cheap.
//
// OPTIONAL — like LineageReverifier, not required by Run(). When the
// field is nil the ticker upgrades the Resolver via a type assertion,
// so the production wiring (Resolver: *server.Server) gets the
// observer for free; a Resolver that doesn't implement it preserves
// the pre-#1023 behavior (open PRs are left parked silently).
type DriveObserver interface {
	ObserveParkedReviewForDrive(ctx context.Context, stage *run.Stage, prURL string)
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

// BoardTransitionHealer re-asserts the work-item board transition implied by
// a parked review stage's authoritative lifecycle state (#1012), healing a
// board move dropped by a missed webhook. Satisfied by *server.Server
// (NotifyBoardTransition): the server resolves the run, maps the lifecycle
// event to a canonical state through the repo conventions, and dispatches the
// provider transition. The implementation is best-effort (errors log, never
// propagate) and the provider's expected-source check makes the move
// idempotent at the board level, so the Ticker only needs to avoid re-auditing
// an unchanged card every tick — it does so with a once-per-run dedup.
//
// OPTIONAL — like LineageReverifier and DriveObserver, not required by Run().
// When the field is nil the ticker upgrades the Resolver via a type assertion
// (production wires *server.Server as Resolver, which implements it), so the
// fishhawkd wiring needs no new field; a Resolver that doesn't implement it
// preserves the pre-#1012 behavior (no board heal).
type BoardTransitionHealer interface {
	NotifyBoardTransition(ctx context.Context, runID uuid.UUID, event string)
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

	// DriveObserver evaluates parked review stages of drive-enabled
	// runs on the open-PR branch of each tick (#1023). OPTIONAL: when
	// nil, Tick upgrades the Resolver via a type assertion (production
	// wires *server.Server as Resolver, which implements it); tests can
	// inject a stub explicitly.
	DriveObserver DriveObserver

	// BoardTransitionHealer re-asserts the in_review board move for a
	// parked-open review stage (#1012), healing a transition dropped by a
	// missed pull_request.opened webhook. OPTIONAL: when nil, Tick upgrades
	// the Resolver via a type assertion (production wires *server.Server as
	// Resolver, which implements NotifyBoardTransition); a Resolver that
	// doesn't implement it preserves the pre-#1012 behavior.
	BoardTransitionHealer BoardTransitionHealer

	// boardHealed dedups the board-state heal to at most once per run per
	// process. The provider's expected-source check already makes the move
	// idempotent at the board level, but the hook audits every attempt (move
	// AND never-fight-the-human skip), so re-asserting every tick would
	// append a skip audit to a healthy long-parked run's chained log on each
	// pass. Recorded after the heal fires; a process restart re-arms it (cost:
	// one more skip). Touched only from the single-goroutine tick loop, so it
	// needs no lock — same posture as the resolved-stage bookkeeping.
	boardHealed map[uuid.UUID]bool

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

	// Resolver-upgrade default for the drive observer (#1023): the
	// production Resolver is *server.Server, which implements
	// DriveObserver — asserting here means fishhawkd's ticker wiring
	// needs no new field while tests keep explicit injection.
	if t.DriveObserver == nil {
		if obs, ok := t.Resolver.(DriveObserver); ok {
			t.DriveObserver = obs
		}
	}

	// Same Resolver-upgrade default for the board-transition healer (#1012):
	// *server.Server implements NotifyBoardTransition, so the production
	// wiring picks up the board heal with no new fishhawkd field.
	if t.BoardTransitionHealer == nil {
		if h, ok := t.Resolver.(BoardTransitionHealer); ok {
			t.BoardTransitionHealer = h
		}
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

	pr, err := t.PRGetter.GetPullRequestScoped(ctx, forge.FromGitHubInstallationID(*runRow.InstallationID), repo, number)
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
		// Board-state self-heal (#1012): the PR is confirmed open, so the
		// run's authoritative lifecycle state is in_review — re-assert the
		// pr_opened board move a dropped pull_request.opened webhook would
		// have left undone. Best-effort, deduped to once per run, and
		// idempotent at the board level via the provider's expected-source
		// check (a card already in_review or human-parked in Blocked is left
		// untouched). Fires for every issue-triggered run regardless of Drive.
		t.healBoardTransition(ctx, s.RunID)
		// Drive (#1023): a drive-enabled run's parked-open tick is where
		// the poll-driven mechanical rules are evaluated —
		// reviews_settled_gate when every configured implement review is
		// terminal, and the derived awaiting_merge stamp when the review
		// evidence is complete AND required checks are green. The observer
		// emits audit entries only; the stage stays parked and the merge
		// remains a judgment point.
		if t.DriveObserver != nil && runRow.Drive {
			t.DriveObserver.ObserveParkedReviewForDrive(ctx, s, prURL)
		}
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

// healBoardTransition re-asserts the pr_opened board move for a parked-open
// review stage (#1012), at most once per run per process. The server-side hook
// is best-effort and the provider's expected-source check makes the move
// idempotent at the board level; the dedup here keeps a healthy long-parked run
// from appending a never-fight-the-human skip audit on every tick. A nil healer
// (Resolver doesn't implement NotifyBoardTransition) is a clean no-op.
func (t *Ticker) healBoardTransition(ctx context.Context, runID uuid.UUID) {
	if t.BoardTransitionHealer == nil || t.boardHealed[runID] {
		return
	}
	t.BoardTransitionHealer.NotifyBoardTransition(ctx, runID, boardHealEvent)
	if t.boardHealed == nil {
		t.boardHealed = map[uuid.UUID]bool{}
	}
	t.boardHealed[runID] = true
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
