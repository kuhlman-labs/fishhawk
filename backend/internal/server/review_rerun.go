package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/dberr"
	"github.com/kuhlman-labs/fishhawk/backend/internal/defaultbranch"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// Operator re-run of a failed implement-review round (#4219).
//
// An implement-review round whose reviewers ALL failed or were skipped settles
// with zero verdicts. On a decomposed parent that round covers nothing, so the
// review-coverage hold (reviewCoverageRefusal) refuses the merge, and before
// this verb nothing could dispatch a second review of the SAME head: every
// dispatcher keys on a new head and the #797 same-head dedup suppresses the
// rest. POST /v0/runs/{run_id}/reviews/rerun re-runs the run's latest
// implement round against its RECORDED source (round_origin / round_base_sha /
// head_sha, #4077) through the same rebuild the boot re-dispatch uses
// (redispatchImplementRound).
//
// It is a NEW same-head review path, distinct from ADR-091 D6's boot
// re-dispatch: it carries its own marker (reviewRerun), never the D6 marker,
// and the marker bypasses the dedup only after rerunBypassApplies re-verifies
// it inside reviewDispatchMu.
//
// FIRST ROUND (#4282). A decomposed parent whose consolidated dispatch never
// started a round (the forge compare failed, or was not wired when the
// orchestrator fired DispatchConsolidatedReview) is held by the coverage hold
// with nothing to re-run, and nothing re-fires that dispatch. On such a parent
// — once its consolidated PR is open — the verb starts the FIRST consolidated
// round instead: it runs the same round body (runConsolidatedReviewRound)
// synchronously. That round carries NO re-run marker: it is a first dispatch of
// its head, so the #797 same-head dedup under reviewDispatchMu is what keeps it
// single against a concurrent orchestrator dispatch.

// categoryReviewRoundRerunRequested is the audit row the re-run verb appends,
// synchronously and before any dispatch, for each round it re-runs.
// Registered in audit/categories.go.
const categoryReviewRoundRerunRequested = "review_round_rerun_requested"

// Re-run refusal codes. Every refusal is decided before the audit append, so a
// refused call writes nothing.
const (
	rerunCodeNoReviewRound     = "no_review_round"
	rerunCodeRoundInFlight     = "review_round_in_flight"
	rerunCodeRoundHasVerdict   = "review_round_has_verdict"
	rerunCodeIneligible        = "review_rerun_ineligible"
	rerunCodeNotStarted        = "review_rerun_not_started"
	rerunCodeCheckFailed       = "review_rerun_check_failed"
	rerunCodeAuditFailed       = "review_rerun_audit_failed"
	rerunCodeUnconfigured      = "review_rerun_unconfigured"
	rerunInFlightRerunPending  = "rerun_pending"
	rerunInFlightRedispatching = "boot_redispatch_pending"
	rerunInFlightUnsettled     = "round_unsettled"
)

// First-round refusal reasons (#4282), carried in details.reason of a 409
// review_rerun_ineligible. The rest of the first-round ladder reuses the
// re-dispatch slugs (reviewer_unwired, gating_authority,
// implement_stage_terminal, advanced_past_review, forge_compare_unavailable).
const (
	rerunSlugNoAgentReviewers = "no_agent_reviewers"
	rerunSlugConsolidatedDue  = "consolidated_review_not_due"
	// rerunSlugDefaultBranchUnresolved (#4660): the repository's forge
	// default_branch — the consolidated round's compare base — could not be
	// read; details.default_branch_unresolved_reason names why.
	rerunSlugDefaultBranchUnresolved = "default_branch_unresolved"
)

// rerunMergeCandidateUnreadable is details.merge_candidate_state on a
// fanin_verify_held 502 whose state could not be re-read.
const rerunMergeCandidateUnreadable = "unreadable"

// reviewRoundRerunRequestedPayload is the review_round_rerun_requested audit
// payload. RerunOfSequence names the re-run round's implement_review_started.
type reviewRoundRerunRequestedPayload struct {
	Stage            string                   `json:"stage"`
	StageID          string                   `json:"stage_id"`
	RerunOfSequence  int64                    `json:"rerun_of_sequence"`
	ConfiguredAgents int                      `json:"configured_agents"`
	LandedBefore     int                      `json:"landed_before"`
	Authority        planreview.AuthorityMode `json:"authority"`
	RoundOrigin      string                   `json:"round_origin,omitempty"`
	HeadSHA          string                   `json:"head_sha,omitempty"`
	// FirstRound marks a request that started a decomposed parent's FIRST
	// consolidated round (#4282): RerunOfSequence is then 0 and HeadSHA is
	// absent (the compare resolves it after the row is recorded).
	FirstRound bool `json:"first_round,omitempty"`
}

// rerunReviewResponse is the 200 body of POST /v0/runs/{run_id}/reviews/rerun.
type rerunReviewResponse struct {
	RunID            string `json:"run_id"`
	Stage            string `json:"stage"`
	StageID          string `json:"stage_id"`
	RerunOfSequence  int64  `json:"rerun_of_sequence"`
	NewRoundSequence int64  `json:"new_round_sequence"`
	ConfiguredAgents int    `json:"configured_agents"`
}

// pendingReruns is the process-local set of runs with a re-run between its
// refusal checks and its dispatch's return (the pendingRedispatches
// precedent). A second POST for the same run while one is pending is refused
// review_round_in_flight instead of racing the first.
var (
	pendingRerunMu sync.Mutex
	pendingReruns  = map[uuid.UUID]struct{}{}
)

// claimRerun marks runID's re-run pending; false when one already is.
func claimRerun(runID uuid.UUID) bool {
	pendingRerunMu.Lock()
	defer pendingRerunMu.Unlock()
	if _, ok := pendingReruns[runID]; ok {
		return false
	}
	pendingReruns[runID] = struct{}{}
	return true
}

func releaseRerun(runID uuid.UUID) {
	pendingRerunMu.Lock()
	defer pendingRerunMu.Unlock()
	delete(pendingReruns, runID)
}

// rerunBypassApplies is the SELF-VERIFICATION of the re-run marker (#4219): it
// reports whether ctx carries a reviewRerun marker that names THIS dispatch —
// stageID and headSHA equal the marker's — AND the run's newest
// implement_review_started is exactly the marker's round Of (so the newest for
// the (stage, head) pair is Of too). Only then may runImplementReviewsForTree
// skip the diff secrets check and the #797 same-head dedup.
//
// Fail-closed: no marker, a malformed one, a mismatched stage or head, a stale
// Of (a newer round has started — including the first of two dispatches that
// carried the same marker) or an audit read error all return false, and the
// dispatch then dedups exactly as an ordinary round would.
func (s *Server) rerunBypassApplies(ctx context.Context, runID, stageID uuid.UUID, headSHA string) bool {
	rr, ok := reviewRerunFrom(ctx)
	if !ok || rr.StageID != stageID || rr.HeadSHA != headSHA || s.cfg.AuditRepo == nil {
		return false
	}
	started, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "implement_review_started")
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "review rerun: marker re-verification read failed — not bypassing the dedup",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return false
	}
	var newest *audit.Entry
	for _, e := range started {
		if newest == nil || e.Sequence > newest.Sequence {
			newest = e
		}
	}
	if newest == nil || newest.Sequence != rr.Of || newest.StageID == nil || *newest.StageID != stageID {
		return false
	}
	var p planreview.ReviewStartedPayload
	if json.Unmarshal(newest.Payload, &p) != nil {
		return false
	}
	return p.HeadSHA == headSHA
}

// rerunIneligibleSlug decides whether the run's latest implement round may be
// re-run, after the in-flight and verdict refusals. Gating rounds are refused
// (a gating reject transitions the stage synchronously, behind the operator's
// back), as is a round no reviewer backend can run; the rest is the boot
// re-dispatch's implement-round check (implementRoundRebuildSlug), so the two
// same-head paths refuse a round for the same reasons. Unlike the boot path it
// does NOT apply the re-dispatch depth cap or the already_redispatched
// crash-loop guard: an operator names this round deliberately, once per call.
func (s *Server) rerunIneligibleSlug(ctx context.Context, runID uuid.UUID, latest *audit.Entry, payload planreview.ReviewStartedPayload) (string, error) {
	if payload.Authority != planreview.AuthorityAdvisory {
		return redispatchSlugGatingAuthority, nil
	}
	if s.defaultPlanReviewer() == nil {
		return redispatchSlugReviewerUnwired, nil
	}
	if latest.StageID == nil {
		return redispatchSlugUnknownRoundSource, nil
	}
	return s.implementRoundRebuildSlug(ctx, runID, *latest.StageID, latest.Sequence, payload)
}

// handleRerunReview re-runs the run's latest implement-review round when that
// round settled with ZERO verdicts (#4219), or starts a decomposed parent's
// FIRST consolidated round when it has none (#4282, rerunFirstConsolidatedRound,
// whose own ladder is documented there). Refusals, in order, each decided
// before anything is written:
//
//	409 no_review_round          — the run has no implement_review_started and
//	                               is not a decomposed parent.
//	409 review_round_in_flight   — a re-run of this run is pending in this
//	                               process, the round is mid-handoff to a boot
//	                               re-dispatch, or not every configured
//	                               reviewer has settled (an orphaned round is
//	                               closed by POST …/reviews/reconcile first).
//	409 review_round_has_verdict — the round carries an implement_reviewed.
//	409 review_rerun_ineligible  — details.reason names why (gating_authority,
//	                               reviewer_unwired, unknown_round_source,
//	                               superseded_by_fixup, implement_stage_terminal,
//	                               advanced_past_review,
//	                               trace_bundle_unavailable,
//	                               forge_compare_unavailable).
//
// Otherwise it appends review_round_rerun_requested, rebuilds the round's diff
// from its recorded source and dispatches it synchronously (an advisory
// round's reviewers then run detached), and answers 200 with the new round's
// sequence, or 502 review_rerun_not_started naming the rebuild failure. Read
// errors are 500 review_rerun_check_failed (fail-closed: nothing dispatched).
//
// Auth: OPERATOR-ONLY. adminWrite via the route wrapper (the sibling recovery
// verb reviews/reconcile's posture), then an authenticated identity, no
// run-bound agent token (an implement agent must not re-run its own review),
// no delegated operator-agent token, and write:runs.
func (s *Server) handleRerunReview(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, rerunCodeUnconfigured,
			"review re-run endpoint requires run and audit repositories", nil)
		return
	}
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	if _, runBound := runBoundTokenRunID(id); runBound {
		s.writeError(w, r, http.StatusForbidden, "run_token_forbidden",
			"a run-bound agent token may not re-run a review; re-running a failed review round is an operator action", nil)
		return
	}
	if operatorrole.IsTokenSubject(id.Subject) {
		s.writeError(w, r, http.StatusForbidden, "operator_agent_forbidden",
			"a delegated operator-agent token may not re-run a review; re-running a failed review round is an operator action",
			map[string]any{"subject": id.Subject})
		return
	}
	if id.TokenID != "" && !hasScope(id, "write:runs") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:runs",
			map[string]any{"required_scope": "write:runs"})
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}
	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), runID)
	if err != nil {
		if dberr.IsUnavailable(err) {
			s.writeDBUnavailable(w, r)
			return
		}
		s.writeError(w, r, http.StatusNotFound, "run_not_found",
			"no run with that id", map[string]any{"run_id": runID.String()})
		return
	}

	if !claimRerun(runID) {
		s.writeError(w, r, http.StatusConflict, rerunCodeRoundInFlight,
			"a review re-run for this run is already in progress",
			map[string]any{"run_id": runID.String(), "reason": rerunInFlightRerunPending})
		return
	}
	defer releaseRerun(runID)

	ctx := r.Context()
	stage := orphanedReviewStages[1]
	checkFailed := func(err error) {
		s.writeError(w, r, http.StatusInternalServerError, rerunCodeCheckFailed,
			"could not read this run's implement-review round; nothing was re-run",
			map[string]any{internalCauseKey: err.Error()})
	}
	latest, payload, ok, err := s.latestReviewStarted(ctx, runID, stage)
	if err != nil {
		checkFailed(err)
		return
	}
	if !ok {
		s.rerunFirstConsolidatedRound(w, r, id, runRow, stage)
		return
	}
	round := map[string]any{"run_id": runID.String(), "round_sequence": latest.Sequence}
	if reviewRedispatchPending(runID, stage.label, latest.Sequence) {
		round["reason"] = rerunInFlightRedispatching
		s.writeError(w, r, http.StatusConflict, rerunCodeRoundInFlight,
			"the latest implement-review round is being re-dispatched after a restart; await it instead", round)
		return
	}
	landed, err := s.countLandedReviewTerminals(ctx, runID, stage, latest.Sequence)
	if err != nil {
		checkFailed(err)
		return
	}
	// A payload with no configured count (pre-#600) cannot be in flight by
	// this test; its missing round source refuses it below instead.
	if payload.ConfiguredAgents > 0 && !planreview.Settled(payload.ConfiguredAgents, landed) {
		round["reason"] = rerunInFlightUnsettled
		round["configured_agents"] = payload.ConfiguredAgents
		round["landed"] = landed
		s.writeError(w, r, http.StatusConflict, rerunCodeRoundInFlight,
			"the latest implement-review round has not settled; await it (or close an orphaned round with POST /v0/runs/{run_id}/reviews/reconcile) before re-running", round)
		return
	}
	verdicts, err := s.countReviewVerdictsAfter(ctx, runID, latest.Sequence)
	if err != nil {
		checkFailed(err)
		return
	}
	if verdicts > 0 {
		round["verdicts"] = verdicts
		s.writeError(w, r, http.StatusConflict, rerunCodeRoundHasVerdict,
			"the latest implement-review round already carries a reviewer verdict; only a round whose reviewers all failed or were skipped is re-run", round)
		return
	}
	slug, err := s.rerunIneligibleSlug(ctx, runID, latest, payload)
	if err != nil {
		checkFailed(err)
		return
	}
	if slug != "" {
		round["reason"] = slug
		s.writeError(w, r, http.StatusConflict, rerunCodeIneligible,
			"the latest implement-review round cannot be re-run: "+slug, round)
		return
	}

	stageID := *latest.StageID
	body, _ := json.Marshal(reviewRoundRerunRequestedPayload{
		Stage:            stage.label,
		StageID:          stageID.String(),
		RerunOfSequence:  latest.Sequence,
		ConfiguredAgents: payload.ConfiguredAgents,
		LandedBefore:     landed,
		Authority:        payload.Authority,
		RoundOrigin:      payload.RoundOrigin,
		HeadSHA:          payload.HeadSHA,
	})
	actorKind := actorKindForSubject(id.Subject)
	subject := id.Subject
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &stageID,
		Timestamp:    time.Now().UTC(),
		Category:     categoryReviewRoundRerunRequested,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      body,
	}); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, rerunCodeAuditFailed,
			"could not record the review re-run request; nothing was re-run",
			map[string]any{internalCauseKey: err.Error()})
		return
	}

	// Detached from the request so a client disconnect mid-rebuild (a forge
	// compare) cannot cancel a round the audit row already announced.
	dctx := withReviewRerun(context.WithoutCancel(ctx), reviewRerun{Of: latest.Sequence, StageID: stageID, HeadSHA: payload.HeadSHA})
	failSlug := s.redispatchImplementRound(dctx, runID, latest.Sequence, payload)

	newSeq, err := s.rerunRoundStarted(ctx, runID, latest.Sequence)
	if err != nil {
		checkFailed(err)
		return
	}
	if newSeq == 0 {
		if failSlug == "" {
			failSlug = redispatchSlugRoundNotStarted
		}
		round["reason"] = failSlug
		s.writeError(w, r, http.StatusBadGateway, rerunCodeNotStarted,
			"the review re-run was recorded but started no round: "+failSlug, round)
		return
	}
	s.writeJSON(w, r, http.StatusOK, rerunReviewResponse{
		RunID:            runID.String(),
		Stage:            stage.label,
		StageID:          stageID.String(),
		RerunOfSequence:  latest.Sequence,
		NewRoundSequence: newSeq,
		ConfiguredAgents: payload.ConfiguredAgents,
	})
}

// firstRoundTarget is what the first-round ladder resolved for a decomposed
// parent that passed it: the parent implement stage the round attaches to, the
// configured agent count, and the forge compare to source the diff through.
type firstRoundTarget struct {
	implStage *run.Stage
	agents    int
	comparer  patchComparer
	scope     forge.CredentialScope
	repo      forge.RepoRef
}

// firstRoundIneligibleSlug is the first-round path's eligibility ladder (#4282)
// for a decomposed parent with no implement round. Each rung is decided before
// any write, in this order:
//
//   - reviewer_unwired: no reviewer backend is wired.
//   - no_agent_reviewers: the implement stage configures zero agent reviewers.
//     It precedes the authority rung because zero agents resolve to
//     AuthorityGateless, and that rung refuses only AuthorityGating.
//   - gating_authority: a gating round runs its reviewers synchronously and
//     transitions the stage on a reject, behind the operator's back (the same
//     reason rerunIneligibleSlug refuses a gating round). The remedy is a
//     fix-up on the parent implement stage, whose pushed head dispatches the
//     re-review.
//   - consolidated_review_not_due: the parent has no implement stage or its
//     consolidated PR is not open yet. The orchestrator dispatches the
//     consolidated round when the PR opens.
//   - implement_stage_terminal / advanced_past_review: implementRoundStageSlug
//     on the parent implement stage, the boot re-dispatch's stage check.
//   - forge_compare_unavailable: forgeCompareFor names a reason.
//
// "" means the round may start; err is non-nil only when a read failed.
func (s *Server) firstRoundIneligibleSlug(ctx context.Context, runRow *run.Run) (firstRoundTarget, string, error) {
	var t firstRoundTarget
	if s.defaultPlanReviewer() == nil {
		return t, redispatchSlugReviewerUnwired, nil
	}
	authority := planreview.AuthorityGateless
	if cfg := s.resolveStageReviewers(ctx, runRow, spec.StageTypeImplement); cfg != nil {
		t.agents = cfg.AgentCount()
		authority = planreview.ResolveAuthority(*cfg)
	}
	if t.agents == 0 {
		return t, rerunSlugNoAgentReviewers, nil
	}
	if authority == planreview.AuthorityGating {
		return t, redispatchSlugGatingAuthority, nil
	}
	if runRow.PullRequestURL == nil || *runRow.PullRequestURL == "" {
		return t, rerunSlugConsolidatedDue, nil
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runRow.ID)
	if err != nil {
		return t, "", fmt.Errorf("list stages for run %s: %w", runRow.ID, err)
	}
	t.implStage = consolidatedImplementStage(stages)
	if t.implStage == nil {
		return t, rerunSlugConsolidatedDue, nil
	}
	if slug, err := s.implementRoundStageSlug(ctx, runRow.ID, t.implStage.ID); err != nil || slug != "" {
		return t, slug, err
	}
	var reason string
	t.comparer, t.scope, t.repo, reason = s.forgeCompareFor(runRow)
	if reason != "" {
		return t, redispatchSlugForgeCompareUnavail, nil
	}
	return t, "", nil
}

// firstRoundRefusalHint is the sentence a first-round 409 appends after its
// reason.
func firstRoundRefusalHint(slug string) string {
	switch slug {
	case redispatchSlugGatingAuthority:
		return "the parent's implement reviewers are gating, and a gating round runs its reviewers synchronously and transitions the stage on a reject, so this verb does not start one. Route fishhawk_fixup_stage on the parent implement stage: the fix-up's pushed head dispatches the re-review"
	case rerunSlugConsolidatedDue:
		return "the consolidated PR is not open yet (or the parent has no implement stage); the orchestrator dispatches the consolidated round when the PR opens, so await it with fishhawk_await_review"
	case rerunSlugNoAgentReviewers:
		return "the implement stage configures no agent reviewers, so there is no round to start"
	case redispatchSlugReviewerUnwired:
		return "no reviewer backend is wired on this deployment"
	case redispatchSlugForgeCompareUnavail:
		return "the forge compare that sources the consolidated diff is not wired for this run"
	case rerunSlugDefaultBranchUnresolved:
		return "the forge default-branch read that supplies the compare base failed (details.default_branch_unresolved_reason names why); nothing was recorded, so retry once the read recovers"
	}
	return "the parent implement stage no longer wants a review round"
}

// rerunFirstConsolidatedRound is handleRerunReview's FIRST-ROUND path (#4282):
// the run has no implement_review_started. Not a decomposed parent (a child
// run, or a run with no decomposed children) → the unchanged 409
// no_review_round. Otherwise firstRoundIneligibleSlug's ladder answers 409
// review_rerun_ineligible before any write.
//
// An eligible parent then resolves its compare base — the repository's forge
// default branch, the same base the orchestrator's consolidated trigger uses
// (#4660) — BEFORE anything is written: an unresolved read answers 409
// review_rerun_ineligible with details.reason default_branch_unresolved and
// details.default_branch_unresolved_reason, write-free. A resolved base gets
// review_round_rerun_requested (rerun_of_sequence 0, first_round true,
// authority advisory, round_origin consolidated) appended chained, then
// runConsolidatedReviewRound runs SYNCHRONOUSLY on a context detached from the
// request, over that base and the consolidated branch. The permission-drift
// check re-runs with it, under the
// fan-in re-entry semantics every consolidated re-dispatch already has. The
// latest implement round is then re-read: one present → 200 with
// rerun_of_sequence 0 and its sequence (a concurrent orchestrator dispatch of
// the same head dedups on #797, so at most one exists); none → 502
// review_rerun_not_started with details.reason compare_failed,
// fanin_verify_held (plus details.merge_candidate_state; the hold may itself
// have STARTED a merge-candidate verify pass, so that 502 is not write-free) or
// round_not_started.
func (s *Server) rerunFirstConsolidatedRound(w http.ResponseWriter, r *http.Request, id Identity, runRow *run.Run, stage orphanedReviewStageKind) {
	ctx := r.Context()
	runID := runRow.ID
	details := map[string]any{"run_id": runID.String()}
	checkFailed := func(err error) {
		s.writeError(w, r, http.StatusInternalServerError, rerunCodeCheckFailed,
			"could not read this run's implement-review state; nothing was started",
			map[string]any{internalCauseKey: err.Error()})
	}
	noRound := func() {
		s.writeError(w, r, http.StatusConflict, rerunCodeNoReviewRound,
			"this run has no implement-review round to re-run", details)
	}
	if runRow.DecomposedFrom != nil {
		noRound()
		return
	}
	children, err := s.listAllDecomposedChildren(ctx, runID)
	if err != nil {
		checkFailed(err)
		return
	}
	if len(children) == 0 {
		noRound()
		return
	}
	target, slug, err := s.firstRoundIneligibleSlug(ctx, runRow)
	if err != nil {
		checkFailed(err)
		return
	}
	if slug != "" {
		details["reason"] = slug
		if target.implStage != nil {
			details["stage_id"] = target.implStage.ID.String()
		}
		s.writeError(w, r, http.StatusConflict, rerunCodeIneligible,
			"no implement-review round has been dispatched on this decomposed parent and its first consolidated round cannot be started: "+slug+" — "+firstRoundRefusalHint(slug), details)
		return
	}
	stageID := target.implStage.ID
	// Resolve the base AHEAD of the audit append (#4660): an unresolved
	// default branch refuses write-free, so no review_round_rerun_requested
	// announces a round that cannot start.
	base, berr := s.resolveRunDefaultBranch(ctx, runRow)
	if berr != nil {
		reason, _ := defaultbranch.ReasonOf(berr)
		details["reason"] = rerunSlugDefaultBranchUnresolved
		details["default_branch_unresolved_reason"] = string(reason)
		details["stage_id"] = stageID.String()
		s.writeError(w, r, http.StatusConflict, rerunCodeIneligible,
			"no implement-review round has been dispatched on this decomposed parent and its first consolidated round cannot be started: "+rerunSlugDefaultBranchUnresolved+" ("+string(reason)+") — "+firstRoundRefusalHint(rerunSlugDefaultBranchUnresolved), details)
		return
	}
	body, _ := json.Marshal(reviewRoundRerunRequestedPayload{
		Stage:            stage.label,
		StageID:          stageID.String(),
		RerunOfSequence:  0,
		ConfiguredAgents: target.agents,
		Authority:        planreview.AuthorityAdvisory,
		RoundOrigin:      reviewRoundOriginConsolidated,
		FirstRound:       true,
	})
	actorKind := actorKindForSubject(id.Subject)
	subject := id.Subject
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &stageID,
		Timestamp:    time.Now().UTC(),
		Category:     categoryReviewRoundRerunRequested,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      body,
	}); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, rerunCodeAuditFailed,
			"could not record the review re-run request; nothing was started",
			map[string]any{internalCauseKey: err.Error()})
		return
	}

	// Detached from the request so a client disconnect mid-compare cannot
	// cancel a round the audit row already announced.
	failSlug, headSHA := s.runConsolidatedReviewRound(context.WithoutCancel(ctx), runRow, stageID, base,
		orchestrator.ConsolidatedBranch(runID), target.comparer, target.scope, target.repo)

	latest, _, ok, err := s.latestReviewStarted(ctx, runID, stage)
	if err != nil {
		checkFailed(err)
		return
	}
	if ok {
		s.writeJSON(w, r, http.StatusOK, rerunReviewResponse{
			RunID:            runID.String(),
			Stage:            stage.label,
			StageID:          stageID.String(),
			RerunOfSequence:  0,
			NewRoundSequence: latest.Sequence,
			ConfiguredAgents: target.agents,
		})
		return
	}
	if failSlug == "" {
		failSlug = redispatchSlugRoundNotStarted
	}
	details["reason"] = failSlug
	details["stage_id"] = stageID.String()
	msg := "the first consolidated round was recorded but started no round: " + failSlug
	if failSlug == consolidatedRoundSlugFanInVerifyHeld {
		state := rerunMergeCandidateUnreadable
		if st, serr := s.mergeCandidateVerifyState(ctx, runRow, headSHA); serr == nil {
			state = st.State
		}
		details["merge_candidate_state"] = state
		msg += " — " + fanInHeldHint(state)
	}
	s.writeError(w, r, http.StatusBadGateway, rerunCodeNotStarted, msg, details)
}

// fanInHeldHint is the remedy a fanin_verify_held 502 names, by the consolidated
// head's merge-candidate state (ADR-090 D5).
func fanInHeldHint(state string) string {
	switch state {
	case mergeCandidateStateFailed:
		return "the consolidated head's merge-candidate verify FAILED; route fishhawk_fixup_stage on the parent implement stage (details.stage_id): the fix-up's pushed head is runner-gated and dispatches the review"
	case mergeCandidateStateInFlight:
		return "a merge-candidate verify pass is in flight for the consolidated head (this call may have just started it); await it: its report dispatches the round"
	}
	return "the merge-candidate verify state is " + state + ", so the fan-in hold could neither start nor confirm a pass; read the fishhawkd log for the hold's reason"
}

// countReviewVerdictsAfter counts the run's implement_reviewed entries
// sequenced after roundSeq: the round's real verdicts.
func (s *Server) countReviewVerdictsAfter(ctx context.Context, runID uuid.UUID, roundSeq int64) (int, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "implement_reviewed")
	if err != nil {
		return 0, fmt.Errorf("list implement_reviewed for run %s: %w", runID, err)
	}
	n := 0
	for _, e := range entries {
		if e.Sequence > roundSeq {
			n++
		}
	}
	return n, nil
}

// rerunRoundStarted returns the sequence of the implement_review_started
// entry, sequenced after rerunOf, whose rerun_of names rerunOf — the round
// this call started — or 0 when there is none.
func (s *Server) rerunRoundStarted(ctx context.Context, runID uuid.UUID, rerunOf int64) (int64, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "implement_review_started")
	if err != nil {
		return 0, fmt.Errorf("list implement_review_started for run %s: %w", runID, err)
	}
	var found int64
	for _, e := range entries {
		if e.Sequence <= rerunOf {
			continue
		}
		var p planreview.ReviewStartedPayload
		if json.Unmarshal(e.Payload, &p) != nil || p.RerunOf != rerunOf {
			continue
		}
		if e.Sequence > found {
			found = e.Sequence
		}
	}
	return found, nil
}
