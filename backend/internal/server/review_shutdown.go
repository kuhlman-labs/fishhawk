package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// The reviewer shutdown clock and the supplemental pass's finite lifetime
// (ADR-140 (#5047) D8). They are package vars only so tests can lower them.
var (
	// reviewerShutdownDrain is how long into Server.Shutdown the reviewer
	// cancellation fires, while HTTP drain is still running: inside scripts/dev's
	// 5s SIGKILL window, so process groups die before the daemon can be killed.
	reviewerShutdownDrain = 3 * time.Second
	// reviewerShutdownGrace bounds Shutdown's wait for cancelled reviewers. It
	// must exceed the review adapters' 5s killGrace so a group kill lands and its
	// cmd.Wait returns before the process exits.
	reviewerShutdownGrace = 6 * time.Second
	// supplementalPassOverhead is the fixed non-review allowance in a
	// supplemental pass's ownership lifetime.
	supplementalPassOverhead = 2 * time.Minute
	// supplementalPassPollInterval is the longest Retry-After a
	// review_round_in_flight response advertises.
	supplementalPassPollInterval = 15 * time.Second
)

// reviewPassRemainingHeader carries the live pass owner's remaining lifetime in
// seconds on a review_round_in_flight response. It is a header because
// writeError's 5xx detail allow-list drops new detail keys.
const reviewPassRemainingHeader = "X-Fishhawk-Review-Pass-Remaining-Seconds"

var (
	// errReviewerStoppedForShutdown is the interrupted outcome of one reviewer
	// invocation: daemon shutdown stopped it, so no terminal row may be written
	// for it and the next process's boot sweep owns the round.
	errReviewerStoppedForShutdown = errors.New("reviewer stopped for daemon shutdown")
	// errSupplementalAttemptUnknown refuses an attempt-pinned edge with no attempt
	// token: TransitionStageFromAttempt treats an empty token as "no comparison".
	errSupplementalAttemptUnknown = errors.New("supplemental review: stage has no attempt identity")
	// errSupplementalAttemptCASAbsent refuses an attempt-pinned or guarded edge on
	// a run repository without the capability; there is no unpinned fallback.
	errSupplementalAttemptCASAbsent = errors.New("supplemental review: run repository lacks the attempt-pinned transition")
)

// reviewerLifetime owns every in-process reviewer invocation and every claimed
// supplemental pass handler so shutdown can cancel them on a fixed clock
// (ADR-140 (#5047) D8). Reviewers otherwise run under context.WithoutCancel,
// which http.Server.Shutdown can never reach. The zero value is ready to use;
// it is held by value as Server.reviewers.
type reviewerLifetime struct {
	mu      sync.Mutex
	closed  bool
	atClose int
	live    map[*liveReviewer]struct{}
	wg      sync.WaitGroup
}

// liveReviewer is one entered invocation's cancellation handle.
type liveReviewer struct {
	cancel context.CancelFunc
}

// enter registers one unit of work and derives its cancellable context. It
// refuses once the lifetime is closed, returning parent, a no-op release and
// false. release is idempotent, cancels the derived context and leaves the
// lifetime; the caller must call it on every exit.
func (l *reviewerLifetime) enter(parent context.Context) (context.Context, func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return parent, func() {}, false
	}
	ctx, cancel := context.WithCancel(parent)
	entered := &liveReviewer{cancel: cancel}
	if l.live == nil {
		l.live = make(map[*liveReviewer]struct{})
	}
	l.live[entered] = struct{}{}
	l.wg.Add(1)
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			l.mu.Lock()
			delete(l.live, entered)
			l.mu.Unlock()
			cancel()
			l.wg.Done()
		})
	}, true
}

// closeAndCancel closes the lifetime to new entrants and cancels every live
// one. It never blocks, so it is safe on Shutdown's timer goroutine.
func (l *reviewerLifetime) closeAndCancel() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		l.atClose = len(l.live)
	}
	for entry := range l.live {
		entry.cancel()
	}
}

// isClosed reports whether shutdown has closed this lifetime.
func (l *reviewerLifetime) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// stop closes the lifetime, then waits at most grace for live work to return. It
// reports how many of the entrants live at close have ended and how many are
// still live; a reviewer ignoring its context cannot hold shutdown past grace.
func (l *reviewerLifetime) stop(grace time.Duration) (ended, stillLive int) {
	l.closeAndCancel()
	done := make(chan struct{})
	go func() {
		l.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.atClose - len(l.live), len(l.live)
}

// reviewLoopOutcome is what a shared review loop returns. rejected keeps its
// verdict-only meaning. interrupted means shutdown stopped a reviewer before the
// loop finished; evidenceWriteFailed means a known terminal write was lost. Both
// return before every post-loop pass and no stage decision exists.
type reviewLoopOutcome struct {
	rejected            bool
	interrupted         bool
	evidenceWriteFailed bool
}

// reviewDispatchOutcome is what a review dispatcher returns to the caller that
// owns the stage edge. A caller reads interrupted, undecided, then
// evidenceWriteFailed: when any is true blockReason is empty and no stage
// decision exists. undecided means a required pass could not be proved or built,
// so nothing may advance.
type reviewDispatchOutcome struct {
	blockReason         string
	interrupted         bool
	undecided           bool
	evidenceWriteFailed bool
}

// blocked reports a recorded gating block: the only outcome that decides the
// stage. The no-decision outcomes are deliberately not blocked.
func (o reviewDispatchOutcome) blocked() bool { return o.blockReason != "" }

// writeReviewRoundInterrupted answers a gating upload whose review round was
// stopped or lost. A closing daemon answers 503: this process's round was
// stopped and the upload may be retried on the next process. An open daemon
// answers 409: a prior daemon lost the round and nothing here will run it.
func (s *Server) writeReviewRoundInterrupted(w http.ResponseWriter, r *http.Request, runID, stageID uuid.UUID, stageLabel string) {
	status := http.StatusConflict
	message := "the gating review round for this upload was lost and will not resume; retry the stage with fishhawk_retry_stage"
	if s.reviewers.isClosed() {
		status = http.StatusServiceUnavailable
		message = "the daemon is shutting down and stopped the gating review round for this upload; nothing advanced"
	}
	s.writeError(w, r, status, "review_round_interrupted", message, map[string]any{
		"run_id":   runID.String(),
		"stage_id": stageID.String(),
		"stage":    stageLabel,
	})
}

// gatingRoundLostByRestart reports whether stageID's newest review round of this
// kind is a gating round that can no longer finish. Only a round this open
// process dispatched is still its own to finish; a prior process's round, or any
// round once this process is closing, goes on to count its terminals. The boot
// sweep's named closure is not a verdict, so it never counts as settling.
func (s *Server) gatingRoundLostByRestart(ctx context.Context, runID uuid.UUID, stage orphanedReviewStageKind, stageID uuid.UUID, headSHA string) (bool, error) {
	latest, payload, ok, err := s.latestReviewStarted(ctx, runID, stage)
	if err != nil {
		return false, err
	}
	if !ok || latest.StageID == nil || *latest.StageID != stageID || payload.Authority != planreview.AuthorityGating {
		return false, nil
	}
	if stage.isImplement && payload.HeadSHA != headSHA {
		return false, nil
	}
	if !latest.Timestamp.Before(s.processStart) && !s.reviewers.isClosed() {
		return false, nil
	}
	landed := 0
	for _, category := range stage.terminals {
		entries, lerr := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, category)
		if lerr != nil {
			return false, lerr
		}
		for _, entry := range entries {
			if entry.Sequence <= latest.Sequence || entry.StageID == nil || *entry.StageID != stageID {
				continue
			}
			if category == stage.failed {
				var failed planreview.ReviewFailedPayload
				if derr := json.Unmarshal(entry.Payload, &failed); derr != nil {
					return false, derr
				}
				if strings.HasPrefix(failed.Reason, orphanedReviewRestartReason) {
					continue
				}
			}
			landed++
		}
	}
	return landed < payload.ConfiguredAgents, nil
}

// sameHeadGatingRoundLost is gatingRoundLostByRestart for an implement round at
// headSHA. A read error fails closed: the retried upload is refused, not advanced.
func (s *Server) sameHeadGatingRoundLost(ctx context.Context, runID, stageID uuid.UUID, headSHA string) bool {
	return s.roundLostFailClosed(ctx, runID, implementReviewKind(), stageID, headSHA)
}

// planRoundLost is gatingRoundLostByRestart for a plan round, failing closed.
func (s *Server) planRoundLost(ctx context.Context, runID, stageID uuid.UUID) bool {
	return s.roundLostFailClosed(ctx, runID, orphanedReviewStages[0], stageID, "")
}

// roundLostFailClosed WARNs on a read error and reports the round lost.
func (s *Server) roundLostFailClosed(ctx context.Context, runID uuid.UUID, stage orphanedReviewStageKind, stageID uuid.UUID, headSHA string) bool {
	lost, err := s.gatingRoundLostByRestart(ctx, runID, stage, stageID, headSHA)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "review round lost check: read failed; treating the round as lost",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("stage", stage.label),
			slog.String("error", err.Error()))
		return true
	}
	return lost
}

// supplementalPassRequirementState is the ONE decision whether a supplemental
// base-rebase pass is required (ADR-140 (#5047) D8, D9).
type supplementalPassRequirementState int

const (
	passNotRequired supplementalPassRequirementState = iota
	passRequiredAdvisory
	passRequiredGating
	passRequirementUnknown
)

// supplementalPassRequirement is resolveSupplementalPassRequirement's result.
// err carries the cause only when state is passRequirementUnknown.
type supplementalPassRequirement struct {
	state      supplementalPassRequirementState
	agentCount int
	authority  planreview.AuthorityMode
	reviewers  *spec.ReviewersConfig
	runRow     *run.Run
	plan       *plan.Plan
	err        error
}

// resolveSupplementalPassRequirement is the ONE decision the dispatcher, the
// claim, the lost-pass guard and prompt admission read. Its order matches the
// dispatcher's: a read fault is unknown rather than proof that no pass is due,
// while a run with no reviewers, no backend or no plan has nothing to judge.
func (s *Server) resolveSupplementalPassRequirement(ctx context.Context, runID uuid.UUID) supplementalPassRequirement {
	if s.cfg.RunRepo == nil {
		return supplementalPassRequirement{state: passNotRequired}
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		return supplementalPassRequirement{state: passRequirementUnknown, err: err}
	}
	if runRow.WorkflowSpec != nil {
		if _, perr := spec.ParseBytes(runRow.WorkflowSpec); perr != nil {
			return supplementalPassRequirement{state: passRequirementUnknown, runRow: runRow, err: perr}
		}
	}
	reviewers := s.resolveStageReviewers(ctx, runRow, spec.StageTypeImplement)
	if reviewers == nil || reviewers.AgentCount() == 0 {
		return supplementalPassRequirement{state: passNotRequired, runRow: runRow}
	}
	if s.defaultPlanReviewer() == nil {
		return supplementalPassRequirement{state: passNotRequired, runRow: runRow}
	}
	approvedPlan, err := s.loadApprovedPlanForRun(ctx, runID)
	if err != nil {
		return supplementalPassRequirement{state: passRequirementUnknown, runRow: runRow, err: err}
	}
	if approvedPlan == nil {
		return supplementalPassRequirement{state: passNotRequired, runRow: runRow}
	}
	authority := planreview.ResolveAuthority(*reviewers)
	state := passRequiredAdvisory
	if authority == planreview.AuthorityGating {
		state = passRequiredGating
	}
	return supplementalPassRequirement{
		state:      state,
		agentCount: reviewers.AgentCount(),
		authority:  authority,
		reviewers:  reviewers,
		runRow:     runRow,
		plan:       approvedPlan,
	}
}

// supplementalPassLifetime is a live pass's finite ownership lifetime: the
// configured reviewer count times the larger of the deployment review budget
// ceiling and the stage's review timeout, plus a fixed non-review allowance.
func (s *Server) supplementalPassLifetime(req supplementalPassRequirement) time.Duration {
	n := max(req.agentCount, 1)
	capDur := s.cfg.ReviewBudget.Cap
	if capDur <= 0 {
		capDur = planreview.DefaultReviewBudget.Cap
	}
	floor := spec.ResolveReviewTimeout(req.reviewers, s.cfg.ReviewBudget.Floor)
	return time.Duration(n)*max(capDur, floor) + supplementalPassOverhead
}

// supplementalPassKey identifies one attempt's gating pass at one head.
type supplementalPassKey struct {
	stageID uuid.UUID
	attempt string
	headSHA string
}

// supplementalPassOwners is the process-local set of live gating passes, held by
// value as Server.supplementalPasses. It proves liveness in THIS process only;
// the durable start row decides between processes. The zero value is ready.
type supplementalPassOwners struct {
	mu   sync.Mutex
	live map[supplementalPassKey]time.Time
}

// tryAcquire takes key's ownership until release. A busy key returns the holder's
// deadline and false, keeping the holder's entry untouched. release is idempotent.
func (o *supplementalPassOwners) tryAcquire(key supplementalPassKey, deadline time.Time) (release func(), holderDeadline time.Time, ok bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if holder, busy := o.live[key]; busy {
		return func() {}, holder, false
	}
	if o.live == nil {
		o.live = make(map[supplementalPassKey]time.Time)
	}
	o.live[key] = deadline
	var once sync.Once
	return func() {
		once.Do(func() {
			o.mu.Lock()
			defer o.mu.Unlock()
			delete(o.live, key)
		})
	}, deadline, true
}

// supplementalPassClaim is claimSupplementalGatingPass's result. release is never
// nil and the handler must defer it. unknown means the pass cannot be proved or
// fenced: the handler answers 500 before persisting anything.
type supplementalPassClaim struct {
	release        func()
	claimed        bool
	busy           bool
	unknown        bool
	key            supplementalPassKey
	lifetime       time.Duration
	deadline       time.Time
	holderDeadline time.Time
}

// supplementalStartAppender resolves the atomic start allocation's capability by
// plain assertion, with no fallback. Only the test binary may replace it.
var supplementalStartAppender = func(repo audit.Repository) (audit.DedupedChainAppender, bool) {
	d, ok := repo.(audit.DedupedChainAppender)
	return d, ok
}

// supplementalGuardedEdge resolves the guarded terminal edge's capability by plain
// assertion, with no fallback. Only the test binary may replace it.
var supplementalGuardedEdge = func(s *Server) (run.GuardedStageTransitioner, bool) {
	g, ok := s.cfg.RunRepo.(run.GuardedStageTransitioner)
	return g, ok
}

// claimSupplementalGatingPass takes current-attempt, process-local ownership of a
// required gating pass before anything is persisted, so a retry can tell a live
// pass from a lost one without relying on partial verdict counts. Only a running
// implement stage whose upload carries exemptions (or a discharged omission,
// forceClaim) under a gating requirement claims; everything else lands untouched.
func (s *Server) claimSupplementalGatingPass(ctx context.Context, runID uuid.UUID, stage *run.Stage, pr *pullRequestBody, forceClaim bool) supplementalPassClaim {
	claim := supplementalPassClaim{release: func() {}}
	if stage.Type != run.StageTypeImplement || stage.State != run.StageStateRunning {
		return claim
	}
	if len(pr.SupplementalScopeExemptions) == 0 && !forceClaim {
		return claim
	}
	req := s.resolveSupplementalPassRequirement(ctx, runID)
	switch req.state {
	case passRequiredGating:
	case passRequirementUnknown:
		claim.unknown = true
		return claim
	default:
		return claim
	}
	key := supplementalPassKey{stageID: stage.ID, attempt: run.StageAttemptToken(stage.DispatchedAt), headSHA: pr.HeadSHA}
	claim.key = key
	if key.attempt == "" {
		claim.unknown = true
		return claim
	}
	if _, ok := supplementalStartAppender(s.cfg.AuditRepo); !ok {
		claim.unknown = true
		return claim
	}
	if _, ok := s.cfg.RunRepo.(run.StageAttemptCASTransitioner); !ok {
		claim.unknown = true
		return claim
	}
	if _, ok := supplementalGuardedEdge(s); !ok {
		claim.unknown = true
		return claim
	}
	claim.lifetime = s.supplementalPassLifetime(req)
	deadline := time.Now().Add(claim.lifetime)
	release, holderDeadline, ok := s.supplementalPasses.tryAcquire(key, deadline)
	claim.holderDeadline = holderDeadline
	if !ok {
		claim.busy = true
		return claim
	}
	claim.release = release
	claim.claimed = true
	claim.deadline = deadline
	return claim
}

// boundSupplementalPassContext detaches the rest of a claimed handler from its
// client and bounds it at the claim's deadline, inside the reviewer lifetime so
// shutdown cancels it. A client disconnect does not revoke the pass. On a closed
// lifetime the deadline still holds and the reviewer invocation refuses.
func (s *Server) boundSupplementalPassContext(parent context.Context, deadline time.Time) (context.Context, func()) {
	ctx, cancel := context.WithDeadline(context.WithoutCancel(parent), deadline)
	handlerCtx, releaseHandler, entered := s.reviewers.enter(ctx)
	if !entered {
		return ctx, cancel
	}
	return handlerCtx, func() {
		releaseHandler()
		cancel()
	}
}

// writeSupplementalPassBusy answers an upload that found its pass owned. A
// stopped round keeps review_round_interrupted, as does an owner past its captured
// deadline, which is no longer proven live: its ownership stays held and no second
// reviewer is authorized. Otherwise the retry is told the pass is live, with
// delay and remaining seconds in headers; the response promises no wait.
func (s *Server) writeSupplementalPassBusy(w http.ResponseWriter, r *http.Request, runID, stageID uuid.UUID, holderDeadline time.Time) {
	const stageLabel = "implement"
	closing := s.reviewers.isClosed()
	if closing {
		s.writeReviewRoundInterrupted(w, r, runID, stageID, stageLabel)
		return
	}
	if !time.Now().Before(holderDeadline) {
		s.writeReviewRoundInterrupted(w, r, runID, stageID, stageLabel)
		return
	}
	remainingSeconds := max(int64(math.Ceil(time.Until(holderDeadline).Seconds())), 1)
	retryAfter := min(int64(supplementalPassPollInterval/time.Second), remainingSeconds)
	w.Header().Set(reviewPassRemainingHeader, strconv.FormatInt(remainingSeconds, 10))
	w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter)))
	s.writeError(w, r, http.StatusServiceUnavailable, "review_round_in_flight",
		"a required supplemental review pass is still running for this upload; retry after the advertised delay", map[string]any{
			"run_id":   runID.String(),
			"stage_id": stageID.String(),
			"stage":    stageLabel,
		})
}

// transitionStageAtAttempt is the attempt-pinned edge for a claimed pass's
// failure: it moves the stage out of running only on the attempt the claim
// captured, so a late pass can never fail a newer attempt.
func (s *Server) transitionStageAtAttempt(ctx context.Context, stageID uuid.UUID, attempt string, to run.StageState, completion *run.StageCompletion) (*run.Stage, error) {
	if attempt == "" {
		return nil, errSupplementalAttemptUnknown
	}
	cas, ok := s.cfg.RunRepo.(run.StageAttemptCASTransitioner)
	if !ok {
		return nil, errSupplementalAttemptCASAbsent
	}
	return cas.TransitionStageFromAttempt(ctx, stageID, run.StageStateRunning, to, attempt, completion)
}

// transitionStageAtAttemptGuarded is the attempt-pinned success edge: guard
// re-reads recorded evidence after the run lock and decides inside the edge's
// transaction, so evidence committed after the caller's last read still binds.
func (s *Server) transitionStageAtAttemptGuarded(ctx context.Context, stageID uuid.UUID, attempt string, to run.StageState, guard run.StageEdgeGuard) error {
	g, ok := supplementalGuardedEdge(s)
	if !ok {
		return errSupplementalAttemptCASAbsent
	}
	_, err := g.TransitionStageFromAttemptGuarded(ctx, stageID, run.StageStateRunning, to, attempt, nil, guard)
	return err
}
