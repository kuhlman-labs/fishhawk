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
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
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
// round settled with ZERO verdicts (#4219). Refusals, in order, each decided
// before anything is written:
//
//	409 no_review_round          — the run has no implement_review_started.
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
	if _, err := s.cfg.RunRepo.GetRun(r.Context(), runID); err != nil {
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
		s.writeError(w, r, http.StatusConflict, rerunCodeNoReviewRound,
			"this run has no implement-review round to re-run",
			map[string]any{"run_id": runID.String()})
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
