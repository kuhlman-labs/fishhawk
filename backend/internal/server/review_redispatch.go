package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
)

// Boot re-dispatch of restart-orphaned review rounds (E72.59 / #4077).
//
// A fishhawkd restart kills the detached goroutine running an in-flight plan or
// implement review round, so no terminal entry ever lands. Since #1781/#2712
// the boot sweep closed such a round by synthesizing *_review_failed. For an
// ADVISORY round that closure throws away a review the operator still wants,
// and it made every reload during a campaign destructive. The boot sweep now
// RE-DISPATCHES an eligible orphaned round against the SAME plan artifact or
// reviewed head, and keeps the synthesize-failed closure for every round it
// cannot safely re-dispatch, with a reason naming why.
//
// The on-demand POST /v0/runs/{run_id}/reviews/reconcile stays TERMINATE-ONLY
// (review_reconcile_http.go): only the boot sweep re-dispatches.

// categoryReviewRoundRedispatched is the audit row the boot sweep appends,
// synchronously and before any dispatch, for each orphaned round it
// re-dispatches. Registered in audit/categories.go.
const categoryReviewRoundRedispatched = "review_round_redispatched"

// maxReviewRoundRedispatches caps how many re-dispatches deep a round's lineage
// may go. One re-dispatch per round per boot holds structurally (the
// re-dispatched round's started entry post-dates the boot marker, so the same
// boot never sees it orphaned), but that alone does not stop a crash loop:
// each boot would re-dispatch the previous boot's round. Past the cap the round
// closes failed instead.
const maxReviewRoundRedispatches = 3

// Re-dispatch reason slugs. The INELIGIBILITY slugs are returned by
// redispatchEligibility and stamped on the synthesized *_review_failed as
// "...; not re-dispatched: <slug>" (gating_authority through
// advanced_past_review, plus eligibility_check_failed and
// redispatch_audit_failed, which the boot sweep stamps itself). The FAILURE
// slugs name why a re-dispatch that was audited as review_round_redispatched
// started no new round; the fallback stamps them as "...; re-dispatch did not
// start a round: <slug>".
const (
	redispatchSlugGatingAuthority        = "gating_authority"
	redispatchSlugReviewerUnwired        = "reviewer_unwired"
	redispatchSlugAlreadyRedispatched    = "already_redispatched"
	redispatchSlugDepthCapReached        = "depth_cap_reached"
	redispatchSlugUnknownRoundSource     = "unknown_round_source"
	redispatchSlugPlanStageNotAwaiting   = "plan_stage_not_awaiting_approval"
	redispatchSlugSupersededByNewPlan    = "superseded_by_new_plan"
	redispatchSlugSupersededByFixup      = "superseded_by_fixup"
	redispatchSlugImplementStageTerminal = "implement_stage_terminal"
	redispatchSlugAdvancedPastReview     = "advanced_past_review"
	redispatchSlugPlanArtifactUnavail    = "plan_artifact_unavailable"
	redispatchSlugTraceBundleUnavail     = "trace_bundle_unavailable"
	redispatchSlugForgeCompareUnavail    = "forge_compare_unavailable"
	redispatchSlugCompareFailed          = "compare_failed"
	redispatchSlugRoundNotStarted        = "round_not_started"
	redispatchSlugEligibilityCheckFailed = "eligibility_check_failed"
	redispatchSlugRedispatchAuditFailed  = "redispatch_audit_failed"
)

// orphanedReviewNotRedispatchedReason is the synthesized *_review_failed reason
// for an orphaned round the boot sweep declined to re-dispatch.
func orphanedReviewNotRedispatchedReason(slug string) string {
	return orphanedReviewRestartReason + "; not re-dispatched: " + slug
}

// orphanedReviewRedispatchFallbackReason is the synthesized *_review_failed
// reason for an orphaned round whose re-dispatch started no new round.
func orphanedReviewRedispatchFallbackReason(slug string) string {
	return "reviewer orphaned by daemon restart; re-dispatch did not start a round: " + slug
}

// reviewRoundRedispatchedPayload is the review_round_redispatched audit
// payload. OrphanedRoundSequence is the crash-loop guard's key: a round named
// here is never re-dispatched again (redispatchEligibility). RedispatchDepth is
// the NEW round's depth (the orphaned round's depth + 1).
type reviewRoundRedispatchedPayload struct {
	Stage                 string                   `json:"stage"`
	StageID               string                   `json:"stage_id"`
	OrphanedRoundSequence int64                    `json:"orphaned_round_sequence"`
	ConfiguredAgents      int                      `json:"configured_agents"`
	LandedBefore          int                      `json:"landed_before"`
	Authority             planreview.AuthorityMode `json:"authority"`
	RoundOrigin           string                   `json:"round_origin,omitempty"`
	HeadSHA               string                   `json:"head_sha,omitempty"`
	RedispatchDepth       int                      `json:"redispatch_depth"`
}

// pendingRedispatchKey names one orphaned round handed to a re-dispatch
// goroutine: run, stage label ("plan" / "implement") and the orphaned round's
// *_review_started sequence.
type pendingRedispatchKey struct {
	runID uuid.UUID
	stage string
	seq   int64
}

// pendingRedispatches is the process-local set of orphaned rounds a boot
// re-dispatch has taken over but not yet settled (package-level, the
// reconcileEmitMu precedent). Between the boot sweep's handoff and the new
// round's *_review_started, the orphaned round is still the stage's latest
// round and still predates the boot marker, so without this set the on-demand
// reconcile verb would synthesize its failure mid-handoff. Entries are removed
// when the re-dispatch goroutine returns.
var (
	pendingRedispatchMu sync.Mutex
	pendingRedispatches = map[pendingRedispatchKey]struct{}{}
)

func markRedispatchPending(k pendingRedispatchKey) {
	pendingRedispatchMu.Lock()
	defer pendingRedispatchMu.Unlock()
	pendingRedispatches[k] = struct{}{}
}

func clearRedispatchPending(k pendingRedispatchKey) {
	pendingRedispatchMu.Lock()
	defer pendingRedispatchMu.Unlock()
	delete(pendingRedispatches, k)
}

// reviewRedispatchPending reports whether the round anchored at orphanedSeq is
// mid-handoff to a boot re-dispatch in THIS process. The on-demand reconcile
// verb reports such a round as review_dispatched_by_this_process.
func reviewRedispatchPending(runID uuid.UUID, stage string, orphanedSeq int64) bool {
	pendingRedispatchMu.Lock()
	defer pendingRedispatchMu.Unlock()
	_, ok := pendingRedispatches[pendingRedispatchKey{runID: runID, stage: stage, seq: orphanedSeq}]
	return ok
}

// redispatchEligibility is the ONE predicate deciding whether an orphaned
// review round can be re-dispatched on boot. The boot sweep uses it to choose
// re-dispatch over synthesis, and GET /v0/restart-blockers uses it to decide
// whether a restart would lose a current-process round, so the two surfaces
// cannot disagree.
//
// latest/payload are the round's *_review_started entry and decoded payload
// (latestReviewStarted). Returns (true, "", nil) when eligible, or
// (false, slug, nil) naming the first failed condition. err is non-nil only
// when a read needed to decide failed; the caller decides how to degrade.
//
// Check order per round kind, after the shared authority / wiring / depth /
// already-redispatched checks: superseded, then stage state, then input-source
// wiring. A plan round: superseded_by_new_plan, plan_stage_not_awaiting_approval,
// plan_artifact_unavailable. An implement round: unknown_round_source,
// superseded_by_fixup, implement_stage_terminal / advanced_past_review
// (implementRoundStageSlug, #4174), then trace_bundle_unavailable or
// forge_compare_unavailable.
//
// The checks are CHEAP (audit and stage reads plus wiring), never input
// loading: a round eligible here can still fail to rebuild its inputs at
// dispatch time, which the fallback closes.
func (s *Server) redispatchEligibility(ctx context.Context, runID uuid.UUID, stage orphanedReviewStageKind, latest *audit.Entry, payload planreview.ReviewStartedPayload) (bool, string, error) {
	// Gating rounds are synchronous: a gating plan round's reject transitions
	// the stage, so re-running one on boot would act on a stage the operator
	// has not seen. Only an advisory round, whose human gate stands behind it,
	// is re-dispatched.
	if payload.Authority != planreview.AuthorityAdvisory {
		return false, redispatchSlugGatingAuthority, nil
	}
	if s.defaultPlanReviewer() == nil {
		return false, redispatchSlugReviewerUnwired, nil
	}
	if payload.RedispatchDepth >= maxReviewRoundRedispatches {
		return false, redispatchSlugDepthCapReached, nil
	}
	if s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil || latest == nil || latest.StageID == nil {
		return false, redispatchSlugEligibilityCheckFailed, nil
	}
	// Crash-loop guard: a round already named by a review_round_redispatched
	// entry was handed to a re-dispatch that died before starting its new
	// round (or the new round would be latest). Never retry it.
	already, err := s.roundAlreadyRedispatched(ctx, runID, stage.label, latest.Sequence)
	if err != nil {
		return false, "", err
	}
	if already {
		return false, redispatchSlugAlreadyRedispatched, nil
	}
	stageID := *latest.StageID

	if !stage.isImplement {
		newer, err := s.stageEntryAfter(ctx, runID, "plan_generated", stageID, latest.Sequence)
		if err != nil {
			return false, "", err
		}
		if newer {
			return false, redispatchSlugSupersededByNewPlan, nil
		}
		st, err := s.cfg.RunRepo.GetStage(ctx, stageID)
		if err != nil {
			return false, "", fmt.Errorf("get plan stage %s: %w", stageID, err)
		}
		if st.State != run.StageStateAwaitingApproval {
			return false, redispatchSlugPlanStageNotAwaiting, nil
		}
		if s.cfg.ArtifactRepo == nil {
			return false, redispatchSlugPlanArtifactUnavail, nil
		}
		return true, "", nil
	}

	switch payload.RoundOrigin {
	case reviewRoundOriginTrace:
	case reviewRoundOriginFixupPush, reviewRoundOriginConsolidated:
		// A compare-sourced round is rebuilt as ComparePatch(base, head); a
		// payload missing either cannot name its input.
		if payload.RoundBaseSHA == "" || payload.HeadSHA == "" {
			return false, redispatchSlugUnknownRoundSource, nil
		}
	default:
		// Includes every implement round written before #4077 (no origin):
		// inferring the source from audit context was rejected as fragile.
		return false, redispatchSlugUnknownRoundSource, nil
	}
	newer, err := s.stageEntryAfter(ctx, runID, CategoryStageFixupTriggered, stageID, latest.Sequence)
	if err != nil {
		return false, "", err
	}
	if newer {
		return false, redispatchSlugSupersededByFixup, nil
	}
	if slug, err := s.implementRoundStageSlug(ctx, runID, stageID); err != nil || slug != "" {
		return false, slug, err
	}
	if payload.RoundOrigin == reviewRoundOriginTrace {
		if s.cfg.TraceStore == nil {
			return false, redispatchSlugTraceBundleUnavail, nil
		}
		return true, "", nil
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		return false, "", fmt.Errorf("get run %s: %w", runID, err)
	}
	if _, _, _, reason := s.forgeCompareFor(runRow); reason != "" {
		return false, redispatchSlugForgeCompareUnavail, nil
	}
	return true, "", nil
}

// implementRoundStageSlug is redispatchEligibility's stage-state check for an
// implement round (#4174). It returns implement_stage_terminal when the round's
// implement stage is failed (a reap-failure lands here), cancelled or
// superseded, and advanced_past_review when a review-type stage sequenced after
// it is already terminal: the merge gate the advisory verdict feeds has been
// decided. "" means the stage state does not refuse the round.
//
// SUCCEEDED is deliberately not refused, so StageState.IsTerminal is not used
// here: a gateless implement stage settles succeeded on PR upload
// (advanceImplementStageAfterPR) while the advisory round dispatched at trace
// upload is still in flight, which is the main case boot re-dispatch exists for.
//
// The round's stage is read with GetStage rather than found in the
// ListStagesForRun result, the same read the plan branch uses. A read error,
// including a missing stage row (run.ErrNotFound), is returned so the caller
// degrades: the boot sweep closes the round eligibility_check_failed and
// GET /v0/restart-blockers reports check_failed.
func (s *Server) implementRoundStageSlug(ctx context.Context, runID, stageID uuid.UUID) (string, error) {
	st, err := s.cfg.RunRepo.GetStage(ctx, stageID)
	if err != nil {
		return "", fmt.Errorf("get implement stage %s: %w", stageID, err)
	}
	switch st.State {
	case run.StageStateFailed, run.StageStateCancelled, run.StageStateSuperseded:
		return redispatchSlugImplementStageTerminal, nil
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		return "", fmt.Errorf("list stages for run %s: %w", runID, err)
	}
	for _, other := range stages {
		if other.Type == run.StageTypeReview && other.Sequence > st.Sequence && other.State.IsTerminal() {
			return redispatchSlugAdvancedPastReview, nil
		}
	}
	return "", nil
}

// roundAlreadyRedispatched reports whether a review_round_redispatched entry
// already names the stage's round anchored at orphanedSeq.
func (s *Server) roundAlreadyRedispatched(ctx context.Context, runID uuid.UUID, stage string, orphanedSeq int64) (bool, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, categoryReviewRoundRedispatched)
	if err != nil {
		return false, fmt.Errorf("list %s for run %s: %w", categoryReviewRoundRedispatched, runID, err)
	}
	for _, e := range entries {
		var p reviewRoundRedispatchedPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		if p.Stage == stage && p.OrphanedRoundSequence == orphanedSeq {
			return true, nil
		}
	}
	return false, nil
}

// stageEntryAfter reports whether the run carries a category entry for stageID
// sequenced strictly after afterSeq — the "superseded" checks.
func (s *Server) stageEntryAfter(ctx context.Context, runID uuid.UUID, category string, stageID uuid.UUID, afterSeq int64) (bool, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, category)
	if err != nil {
		return false, fmt.Errorf("list %s for run %s: %w", category, runID, err)
	}
	for _, e := range entries {
		if e.StageID != nil && *e.StageID == stageID && e.Sequence > afterSeq {
			return true, nil
		}
	}
	return false, nil
}

// redispatchOrphanedRound takes over an ELIGIBLE orphaned round. It MUST be
// called under the run's reconcileEmitLockFor stripe lock (the boot sweep's
// per-run pass holds it).
//
// It appends review_round_redispatched synchronously, so a crash at any later
// point leaves the crash-loop guard's record, marks the round pending, and
// hands the input rebuild and dispatch to an s.bgReviews goroutine so the boot
// sweep (which runs before the listener starts) is not delayed by forge reads
// or a git export. Shutdown drains bgReviews.
//
// Returns false, having done nothing, when the audit append fails: without the
// record a crash could re-dispatch the round on every boot, so the caller
// closes it failed instead.
func (s *Server) redispatchOrphanedRound(ctx context.Context, runID uuid.UUID, stage orphanedReviewStageKind, latest *audit.Entry, payload planreview.ReviewStartedPayload, landed int) bool {
	stageID := *latest.StageID
	body, _ := json.Marshal(reviewRoundRedispatchedPayload{
		Stage:                 stage.label,
		StageID:               stageID.String(),
		OrphanedRoundSequence: latest.Sequence,
		ConfiguredAgents:      payload.ConfiguredAgents,
		LandedBefore:          landed,
		Authority:             payload.Authority,
		RoundOrigin:           payload.RoundOrigin,
		HeadSHA:               payload.HeadSHA,
		RedispatchDepth:       payload.RedispatchDepth + 1,
	})
	systemKind := audit.ActorKind("system")
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  categoryReviewRoundRedispatched,
		ActorKind: &systemKind,
		Payload:   body,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "review reconcile: append review_round_redispatched failed — closing the round failed instead",
			slog.String("run_id", runID.String()),
			slog.String("stage", stage.label),
			slog.String("error", err.Error()),
		)
		return false
	}

	key := pendingRedispatchKey{runID: runID, stage: stage.label, seq: latest.Sequence}
	markRedispatchPending(key)
	bgCtx := context.WithoutCancel(ctx)
	s.bgReviews.Add(1)
	go func() {
		defer s.bgReviews.Done()
		defer clearRedispatchPending(key)
		s.runOrphanedRoundRedispatch(bgCtx, runID, stage, latest.Sequence, payload)
	}()
	return true
}

// runOrphanedRoundRedispatch is the re-dispatch goroutine body: re-check, then
// rebuild the inputs and dispatch, then the fallback.
//
// Read-error exits (#4174): when the re-check (orphanedRoundStillOpen) or the
// fallback (closeUnstartedRedispatch) cannot read the round's audit state, the
// goroutine WARN-logs and returns without dispatching or synthesizing. This is
// deliberate best-effort, the per-run posture ReconcileOrphanedReviews takes:
// the round keeps its review_round_redispatched record, the pending entry is
// cleared on return, the on-demand reconcile verb can close it, and the next
// boot closes it failed as already_redispatched — the cross-boot bound.
func (s *Server) runOrphanedRoundRedispatch(ctx context.Context, runID uuid.UUID, stage orphanedReviewStageKind, orphanedSeq int64, payload planreview.ReviewStartedPayload) {
	if !s.orphanedRoundStillOpen(ctx, runID, stage, orphanedSeq, payload.ConfiguredAgents) {
		return
	}

	rctx := withReviewRedispatch(ctx, reviewRedispatch{Of: orphanedSeq, Depth: payload.RedispatchDepth + 1})
	var slug string
	if stage.isImplement {
		slug = s.redispatchImplementRound(rctx, runID, orphanedSeq, payload)
	} else {
		slug = s.redispatchPlanRound(rctx, runID, orphanedSeq)
	}
	if slug == "" {
		slug = redispatchSlugRoundNotStarted
	}
	s.closeUnstartedRedispatch(ctx, runID, stage, orphanedSeq, payload, slug)
}

// orphanedRoundStillOpen re-checks, under the run's stripe lock, that the
// orphaned round is still the stage's latest round and still unsettled. A
// newer round (or a settle) since the handoff means there is nothing to do.
//
// The lock is released BEFORE the dispatch, so this check does not serialize
// two concurrent re-dispatch goroutines for the SAME round: both would pass it
// and both would dispatch. One goroutine per round is guaranteed upstream
// instead — redispatchOrphanedRound runs only under the boot sweep's per-run
// stripe lock, after the already_redispatched check, and records the round in
// the pending set — and TestReviewRedispatchD6Guard_RealPackage pins
// redispatchOrphanedRound and runOrphanedRoundRedispatch to their single
// callers. A second hand-off path must therefore add its own dedup (or hold the
// lock across the dispatch); it must not lean on this check.
//
// A read error also returns false, with a WARN naming it, so it is not
// mistaken for "superseded": the round is left for the next boot, which closes
// it failed as already_redispatched (see runOrphanedRoundRedispatch).
func (s *Server) orphanedRoundStillOpen(ctx context.Context, runID uuid.UUID, stage orphanedReviewStageKind, orphanedSeq int64, configured int) bool {
	lock := reconcileEmitLockFor(runID)
	lock.Lock()
	defer lock.Unlock()
	readFailed := func(err error) bool {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "review reconcile: re-dispatch re-check read failed — leaving the orphaned round for the next boot",
			slog.String("run_id", runID.String()),
			slog.String("stage", stage.label),
			slog.Int64("orphaned_round_sequence", orphanedSeq),
			slog.String("error", err.Error()),
		)
		return false
	}
	latest, _, ok, err := s.latestReviewStarted(ctx, runID, stage)
	if err != nil {
		return readFailed(err)
	}
	if !ok || latest.Sequence != orphanedSeq {
		return false
	}
	landed, err := s.countLandedReviewTerminals(ctx, runID, stage, orphanedSeq)
	if err != nil {
		return readFailed(err)
	}
	return landed < configured
}

// closeUnstartedRedispatch is the fallback. When the dispatcher returned
// without starting a newer round, the orphaned round would stay pending
// forever, so its missing terminals are synthesized with a reason naming slug.
//
// Approval condition C2: the landed count is RE-RUN here, under the stripe
// lock, against the orphaned round. A dispatcher that failed before its own
// *_review_started (the document_injection_failed branch) still appends a
// *_review_failed, sequenced after the orphaned round, which therefore counts
// toward it; synthesizing from the handoff-time count would close the round
// twice.
//
// Either read failing (the round anchor or the landed count) WARN-logs and
// synthesizes nothing: the round is left for the next boot, which closes it
// failed as already_redispatched (see runOrphanedRoundRedispatch).
func (s *Server) closeUnstartedRedispatch(ctx context.Context, runID uuid.UUID, stage orphanedReviewStageKind, orphanedSeq int64, payload planreview.ReviewStartedPayload, slug string) {
	lock := reconcileEmitLockFor(runID)
	lock.Lock()
	latest, _, ok, err := s.latestReviewStarted(ctx, runID, stage)
	if err != nil {
		lock.Unlock()
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "review reconcile: re-dispatch fallback could not read the round anchor — leaving the orphaned round for the next boot",
			slog.String("run_id", runID.String()),
			slog.String("stage", stage.label),
			slog.Int64("orphaned_round_sequence", orphanedSeq),
			slog.String("error", err.Error()),
		)
		return
	}
	if !ok || latest.Sequence != orphanedSeq || latest.StageID == nil {
		// A newer round started (the re-dispatch succeeded) or the anchor
		// carries no stage: nothing to close.
		lock.Unlock()
		return
	}
	landed, err := s.countLandedReviewTerminals(ctx, runID, stage, orphanedSeq)
	if err != nil {
		lock.Unlock()
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "review reconcile: re-dispatch fallback could not count landed terminals",
			slog.String("run_id", runID.String()),
			slog.String("stage", stage.label),
			slog.String("error", err.Error()),
		)
		return
	}
	missing := payload.ConfiguredAgents - landed
	reason := orphanedReviewRedispatchFallbackReason(slug)
	for i := 0; i < missing; i++ {
		s.emitReviewFailed(ctx, runID, *latest.StageID, stage.failed, payload.Authority, "", reason, false)
	}
	lock.Unlock()
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "review reconcile: re-dispatch started no round — closed the orphaned round failed",
		slog.String("run_id", runID.String()),
		slog.String("stage", stage.label),
		slog.String("reason", slug),
		slog.Int("synthesized", max(missing, 0)),
	)
	if missing > 0 && stage.isImplement {
		if s.reconcileRecomputeAuditComplete != nil {
			s.reconcileRecomputeAuditComplete(ctx, runID)
		} else {
			s.recomputeAndPublishAuditComplete(ctx, runID)
		}
	}
}

// redispatchPlanRound rebuilds a plan round's inputs from the persisted record
// and re-runs runPlanReviews under ctx (which carries the re-dispatch marker).
// The plan is the artifact whose content_hash the latest plan_generated before
// the round names; the gate payloads are the latest of each kind recorded
// between that plan_generated and the round. Returns "" when it dispatched,
// else the failure slug.
func (s *Server) redispatchPlanRound(ctx context.Context, runID uuid.UUID, orphanedSeq int64) string {
	if s.cfg.ArtifactRepo == nil {
		return redispatchSlugPlanArtifactUnavail
	}
	orphaned, err := s.auditEntryAt(ctx, runID, "plan_review_started", orphanedSeq)
	if err != nil || orphaned == nil || orphaned.StageID == nil {
		return redispatchSlugPlanArtifactUnavail
	}
	stageID := *orphaned.StageID
	generated, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "plan_generated")
	if err != nil {
		return redispatchSlugPlanArtifactUnavail
	}
	gen := newestStageEntryBefore(generated, stageID, 0, orphanedSeq)
	if gen == nil {
		return redispatchSlugPlanArtifactUnavail
	}
	var genPayload struct {
		ContentHash string `json:"content_hash"`
	}
	if json.Unmarshal(gen.Payload, &genPayload) != nil || genPayload.ContentHash == "" {
		return redispatchSlugPlanArtifactUnavail
	}
	art, err := s.cfg.ArtifactRepo.GetByHash(ctx, stageID, genPayload.ContentHash)
	if err != nil || art == nil || len(art.Content) == 0 {
		return redispatchSlugPlanArtifactUnavail
	}

	var (
		precheck   *ScopePrecheckPayload
		sweep      *SurfaceSweepPayload
		testSweep  *TestSweepPayload
		regression *ScopeRegressionPayload
		acceptance *AcceptancePrecheckPayload
	)
	loadPlanGatePayload(ctx, s, runID, categoryPlanScopePrecheck, stageID, gen.Sequence, orphanedSeq, &precheck)
	loadPlanGatePayload(ctx, s, runID, categoryPlanSurfaceSweep, stageID, gen.Sequence, orphanedSeq, &sweep)
	loadPlanGatePayload(ctx, s, runID, categoryPlanTestSweep, stageID, gen.Sequence, orphanedSeq, &testSweep)
	loadPlanGatePayload(ctx, s, runID, categoryPlanScopeRegression, stageID, gen.Sequence, orphanedSeq, &regression)
	loadPlanGatePayload(ctx, s, runID, categoryPlanAcceptancePrecheck, stageID, gen.Sequence, orphanedSeq, &acceptance)

	s.runPlanReviews(ctx, runID, stageID, []byte(art.Content), precheck, sweep, testSweep, regression, acceptance)
	return ""
}

// loadPlanGatePayload decodes the newest category entry for stageID sequenced
// strictly between afterSeq and beforeSeq into *dst. Fail-open like the gates
// that wrote them: a read or decode failure leaves *dst nil, which omits that
// gate's evidence from the prompt.
func loadPlanGatePayload[T any](ctx context.Context, s *Server, runID uuid.UUID, category string, stageID uuid.UUID, afterSeq, beforeSeq int64, dst **T) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, category)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "review reconcile: re-dispatch could not read plan gate evidence — omitting it",
			slog.String("run_id", runID.String()),
			slog.String("category", category),
			slog.String("error", err.Error()),
		)
		return
	}
	e := newestStageEntryBefore(entries, stageID, afterSeq, beforeSeq)
	if e == nil {
		return
	}
	var v T
	if json.Unmarshal(e.Payload, &v) != nil {
		return
	}
	*dst = &v
}

// newestStageEntryBefore returns the highest-sequence entry for stageID with
// afterSeq < sequence < beforeSeq, or nil.
func newestStageEntryBefore(entries []*audit.Entry, stageID uuid.UUID, afterSeq, beforeSeq int64) *audit.Entry {
	var best *audit.Entry
	for _, e := range entries {
		if e.StageID == nil || *e.StageID != stageID || e.Sequence <= afterSeq || e.Sequence >= beforeSeq {
			continue
		}
		if best == nil || e.Sequence > best.Sequence {
			best = e
		}
	}
	return best
}

// auditEntryAt returns the run's category entry at exactly seq, or nil.
func (s *Server) auditEntryAt(ctx context.Context, runID uuid.UUID, category string, seq int64) (*audit.Entry, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, category)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Sequence == seq {
			return e, nil
		}
	}
	return nil, nil
}

// redispatchImplementRound rebuilds an implement round's diff from its
// recorded source and re-runs the implement review under ctx (which carries
// the re-dispatch marker, so the #797 same-head dedup and the diff secrets
// check are bypassed — both already ran for the orphaned round). The round
// source is re-stamped so the new round records the same origin and base.
// Returns "" when it dispatched, else the failure slug.
func (s *Server) redispatchImplementRound(ctx context.Context, runID uuid.UUID, orphanedSeq int64, payload planreview.ReviewStartedPayload) string {
	orphaned, err := s.auditEntryAt(ctx, runID, "implement_review_started", orphanedSeq)
	if err != nil || orphaned == nil || orphaned.StageID == nil {
		return redispatchSlugRoundNotStarted
	}
	stageID := *orphaned.StageID
	ctx = withReviewRoundSource(ctx, reviewRoundSource{Origin: payload.RoundOrigin, BaseSHA: payload.RoundBaseSHA})

	if payload.RoundOrigin == reviewRoundOriginTrace {
		bundleBytes, ok := s.redactedBundleForHead(ctx, runID, stageID, payload.HeadSHA)
		if !ok {
			return redispatchSlugTraceBundleUnavail
		}
		in, ok := implementReviewInputsFromBundle(bundleBytes)
		if !ok {
			return redispatchSlugTraceBundleUnavail
		}
		s.runImplementReviewsForTree(ctx, runID, stageID, in.diff, in.scopeDrift, in.headSHA, in.treeSHA, in.changeID, in.gateEvidence)
		return ""
	}

	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		return redispatchSlugForgeCompareUnavail
	}
	comparer, scope, repo, reason := s.forgeCompareFor(runRow)
	if reason != "" {
		return redispatchSlugForgeCompareUnavail
	}
	cmp, err := comparer.ComparePatch(ctx, scope, repo, payload.RoundBaseSHA, payload.HeadSHA)
	if err != nil || cmp == nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "review reconcile: re-dispatch compare patch failed",
			slog.String("run_id", runID.String()),
			slog.String("base", payload.RoundBaseSHA),
			slog.String("head", payload.HeadSHA),
			slog.Any("error", err),
		)
		return redispatchSlugCompareFailed
	}
	diff := consolidatedReviewDiff(cmp)
	var gateEvidence *prompt.GateEvidence
	if payload.RoundOrigin == reviewRoundOriginFixupPush {
		// The same evidence resolution maybeBackstopFixupReReview applies.
		ev, why := s.resolveStageGateEvidence(ctx, runID, stageID)
		if ev == nil && why != "" {
			ev = &prompt.GateEvidence{VerifyEvidenceUnavailableReason: why}
		}
		gateEvidence = ev
	}
	// The recorded head: for a fixup_push round the pushed head the backstop
	// keyed on, for a consolidated round the compare result's head SHA the
	// dispatcher stamped.
	s.runImplementReviews(ctx, runID, stageID, diff, nil, payload.HeadSHA, gateEvidence)
	return ""
}

// redactedBundleForHead loads the stage's newest REDACTED trace bundle whose
// verify head_sha equals head, or the newest one when head is empty. The
// redacted variant is the one every later reader of a stage's bundle uses
// (ADR-029; resolveStageGateEvidence, resolveFixupPriorDiff), and the review
// prompt redacts the patch again, so the reviewer-visible diff matches the
// orphaned round's.
//
// The lookup is NOT bounded to bundles sequenced before the round: the runner
// uploads the raw variant (which dispatches the round) before the redacted one,
// so the redacted bundle of the round's own pack is sequenced AFTER its
// started entry. head_sha is what ties a bundle to the round.
func (s *Server) redactedBundleForHead(ctx context.Context, runID, stageID uuid.UUID, head string) ([]byte, bool) {
	if s.cfg.TraceStore == nil {
		return nil, false
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "trace_uploaded")
	if err != nil {
		return nil, false
	}
	ordered := append([]*audit.Entry(nil), entries...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Sequence < ordered[j].Sequence })
	for i := len(ordered) - 1; i >= 0; i-- {
		e := ordered[i]
		if e.StageID == nil || *e.StageID != stageID {
			continue
		}
		var p struct {
			Variant     string `json:"variant"`
			ContentHash string `json:"content_hash"`
		}
		if json.Unmarshal(e.Payload, &p) != nil || p.Variant != string(tracestore.VariantRedacted) || len(p.ContentHash) != 64 {
			continue
		}
		body, err := s.cfg.TraceStore.Get(ctx, tracestore.BundleRef{RunID: runID, Variant: tracestore.VariantRedacted, ContentHash: p.ContentHash})
		if err != nil {
			continue
		}
		b, err := io.ReadAll(body)
		_ = body.Close()
		if err != nil {
			continue
		}
		if head != "" {
			got, herr := bundle.ExtractHeadSHA(b)
			if herr != nil || got != head {
				continue
			}
		}
		return b, true
	}
	return nil, false
}

// implementReviewInputs is what the trace-time review hook extracts from a
// stage bundle.
type implementReviewInputs struct {
	diff         policy.Diff
	scopeDrift   []string
	headSHA      string
	treeSHA      string
	changeID     string
	gateEvidence *prompt.GateEvidence
}

// implementReviewInputsFromBundle re-extracts the trace hook's review inputs
// from a stored bundle, with the hook's own degrade contract
// (advanceStageAfterTrace): only the diff is required; scope drift (minus the
// folded paths), head, verify identity and gate evidence each degrade to empty.
func implementReviewInputsFromBundle(b []byte) (implementReviewInputs, bool) {
	var in implementReviewInputs
	diff, err := bundle.ExtractDiff(b)
	if err != nil {
		return in, false
	}
	in.diff = diff
	drift, _ := bundle.ExtractScopeDrift(b)
	folded, _ := bundle.ExtractScopeAmendmentsFolded(b)
	in.scopeDrift = subtractPaths(drift, folded)
	if head, herr := bundle.ExtractHeadSHA(b); herr == nil {
		in.headSHA = head
	}
	if tree, change, terr := bundle.ExtractVerifyIdentity(b); terr == nil {
		in.treeSHA, in.changeID = tree, change
	}
	if ev, gerr := bundle.ExtractGateEvidence(b); gerr == nil {
		in.gateEvidence = gateEvidenceForReview(ev, folded)
	}
	return in, true
}
