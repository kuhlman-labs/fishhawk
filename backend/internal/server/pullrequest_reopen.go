package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// reopenReviveWindow is how long after the PR close that cancelled a run a
// GitHub reopen of the same PR still revives that run (#4082). Measured from
// the pr_closed_without_merge row's Timestamp (stamped by this process's clock
// at append) to time.Now() in the reopen handler — one clock domain, so the
// #3048 cross-clock trap does not apply.
const reopenReviveWindow = 10 * time.Minute

// Audit categories the PR-reopen revive writes (#4082). Both are registered in
// backend/internal/audit/categories.go.
const (
	// CategoryRunRevivedOnReopen records that a quick reopen of a run's PR
	// revived the PR-close-cancelled run to its review gate: the review stage
	// re-parked at awaiting_approval and the run back to running. Operator-
	// visible: the writer marks it with notifyOperatorVisible, so it renders
	// on the issue anchor.
	CategoryRunRevivedOnReopen = "run_revived_on_reopen"
	// CategoryRunReviveOnReopenRefused records that a reopen of a run whose
	// review stage was cancelled by a PR close was NOT allowed to revive it,
	// naming the first failed guard in the payload's `reason`. Internal only
	// (not on the anchor): the run stays cancelled, which the anchor already
	// shows, and get_run_status names the fresh-run fallback.
	CategoryRunReviveOnReopenRefused = "run_revive_on_reopen_refused"
)

// The `reason` vocabulary of a run_revive_on_reopen_refused entry, one per
// guard of handlePullRequestReopened, in guard order.
const (
	reopenRefusedAuditUnreadable              = "audit_unreadable"
	reopenRefusedCloseDidNotCancelRun         = "close_did_not_cancel_run"
	reopenRefusedReviewNotParkedAtClose       = "review_not_parked_at_close"
	reopenRefusedWindowElapsed                = "window_elapsed"
	reopenRefusedCloseHeadUnrecorded          = "close_head_unrecorded"
	reopenRefusedHeadChanged                  = "head_changed"
	reopenRefusedAcceptanceRetirementsDropped = "acceptance_retirements_dropped"
	reopenRefusedStageInFlight                = "stage_in_flight"
	reopenRefusedReviverUnavailable           = "reviver_unavailable"
	reopenRefusedReviveTransitionRefused      = "revive_transition_refused"
)

// reopenRefusal is a failed guard: the reason code plus a human detail.
type reopenRefusal struct {
	reason string
	detail string
}

// handlePullRequestReopened handles GitHub `pull_request.reopened` (#4082).
// Closing a run's PR without merging cancels the run (resolveReviewStageOnMerge,
// ADR-018); this handler makes a QUICK reopen at the SAME head undo that, so
// the natural "close and reopen to re-trigger CI" move no longer destroys the
// run. (The safe CI re-trigger is fishhawk_retrigger_ci, which never touches PR
// state.)
//
// A delivery is a revive CANDIDATE only when the PR's run is cancelled AND its
// review stage is cancelled. Anything else — the run is live (a redelivered or
// out-of-order reopen), the run has no review stage, the review is not
// cancelled, the PR is not Fishhawk-managed, the stage list is unreadable —
// logs and returns with NO audit entry, so redeliveries and unrelated reopens
// are inert.
//
// For a candidate the guards run in this fixed order, and the FIRST that fails
// appends ONE run_revive_on_reopen_refused entry naming it and returns:
//
//	(a) audit_unreadable               an audit read the guards need failed
//	(b) close_did_not_cancel_run       no pr_closed_without_merge row whose
//	                                   run_state_at_close is non-terminal, or
//	                                   that row was already revived (an operator-
//	                                   or budget-cancelled run whose PR closed
//	                                   afterwards; a legacy row without the field)
//	(c) review_not_parked_at_close     that row's review_state_at_close is not
//	                                   awaiting_approval
//	(d) window_elapsed                 more than reopenReviveWindow since it
//	(e) close_head_unrecorded          neither the row nor the run's reported
//	                                   heads name the head the PR closed at
//	(f) head_changed                   the reopened PR's head differs from it
//	(g) acceptance_retirements_dropped the cancel recorded a stage_cancelled
//	                                   acceptance retirement-drop row, which a
//	                                   revive would contradict
//	(h) stage_in_flight                another stage is dispatched or running
//	(i) reviver_unavailable            RunRepo lacks run.RunReopenReviver
//	(j) revive_transition_refused      the atomic revive refused or failed
//
// On success the review stage is re-parked at awaiting_approval and the run is
// running again (one transaction, run.RunReopenReviver), a run_revived_on_reopen
// entry is appended and the anchor is refreshed. The orchestrator is NOT
// advanced: like ReviveRun the revive re-parks only, so an acceptance stage the
// cancel left pending is not dispatched behind the operator's back.
//
// Best-effort throughout: nothing here surfaces as a 5xx.
func (s *Server) handlePullRequestReopened(ctx context.Context, raw []byte) {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return
	}
	var p pullRequestClosedPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"pull_request.reopened: parse failed",
			slog.String("error", err.Error()))
		return
	}
	prURL := p.PullRequest.HTMLURL
	if prURL == "" {
		return
	}
	target := s.findRunByPullRequestURL(ctx, prURL, "pull_request.reopened")
	if target == nil {
		return
	}

	review, others, ok := s.reopenReviveCandidate(ctx, target)
	if !ok {
		return
	}

	reopenHead := p.PullRequest.Head.SHA
	reopenedBy := p.Sender.Login
	anchor, refusal := s.evaluateReopenReviveGuards(ctx, target.ID, others, reopenHead)
	if refusal != nil {
		s.appendReopenReviveRefused(ctx, target.ID, review.ID, prURL, reopenedBy, reopenHead, *refusal)
		return
	}

	reviver, ok := s.cfg.RunRepo.(run.RunReopenReviver)
	if !ok {
		s.appendReopenReviveRefused(ctx, target.ID, review.ID, prURL, reopenedBy, reopenHead, reopenRefusal{
			reason: reopenRefusedReviverUnavailable,
			detail: fmt.Sprintf("run repository %T does not implement run.RunReopenReviver", s.cfg.RunRepo),
		})
		return
	}
	if _, _, err := reviver.ReviveRunOnReopen(ctx, target.ID, review.ID); err != nil {
		s.appendReopenReviveRefused(ctx, target.ID, review.ID, prURL, reopenedBy, reopenHead, reopenRefusal{
			reason: reopenRefusedReviveTransitionRefused,
			detail: err.Error(),
		})
		return
	}

	elapsed := time.Now().UTC().Sub(anchor.entry.Timestamp)
	closedBy := ""
	if anchor.entry.ActorSubject != nil {
		closedBy = *anchor.entry.ActorSubject
	}
	payload, _ := json.Marshal(map[string]any{
		"pr_url":          prURL,
		"reopened_by":     reopenedBy,
		"head_sha":        reopenHead,
		"closed_at":       anchor.entry.Timestamp.UTC().Format(time.RFC3339Nano),
		"closed_by":       closedBy,
		"elapsed_seconds": int(elapsed.Seconds()),
		"window_seconds":  int(reopenReviveWindow.Seconds()),
		"review_stage_id": review.ID.String(),
	})
	actorKind := audit.ActorKind("user")
	reviewID := review.ID
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        target.ID,
		StageID:      &reviewID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryRunRevivedOnReopen,
		ActorKind:    &actorKind,
		ActorSubject: &reopenedBy,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
			"pull_request.reopened: run_revived_on_reopen audit append failed; the run WAS revived",
			slog.String("run_id", target.ID.String()),
			slog.String("error", err.Error()))
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
		"pull_request.reopened: run revived to its review gate",
		slog.String("pr_url", prURL),
		slog.String("run_id", target.ID.String()),
		slog.String("stage_id", review.ID.String()),
		slog.String("reopened_by", reopenedBy),
		slog.Int("elapsed_seconds", int(elapsed.Seconds())))
	s.notifyOperatorVisible(ctx, target.ID, CategoryRunRevivedOnReopen)
}

// reopenReviveCandidate reports whether target is a revive candidate — the
// run is cancelled and its review stage is cancelled — returning that review
// stage and every OTHER stage. A stage-list failure is logged and treated as
// not-a-candidate: without the stages the handler cannot even tell whether
// the reopen concerns a PR-close cancel, so it records nothing.
func (s *Server) reopenReviveCandidate(ctx context.Context, target *run.Run) (*run.Stage, []*run.Stage, bool) {
	if target.State != run.StateCancelled {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelDebug,
			"pull_request.reopened: run is not cancelled; nothing to revive",
			slog.String("run_id", target.ID.String()),
			slog.String("run_state", string(target.State)))
		return nil, nil, false
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, target.ID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"pull_request.reopened: list stages failed; revive not attempted",
			slog.String("run_id", target.ID.String()),
			slog.String("error", err.Error()))
		return nil, nil, false
	}
	var review *run.Stage
	var others []*run.Stage
	for _, st := range stages {
		if review == nil && st.Type == run.StageTypeReview {
			review = st
			continue
		}
		others = append(others, st)
	}
	if review == nil || review.State != run.StageStateCancelled {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelDebug,
			"pull_request.reopened: run has no cancelled review stage; nothing to revive",
			slog.String("run_id", target.ID.String()))
		return nil, nil, false
	}
	return review, others, true
}

// reopenCloseAnchor is the pr_closed_without_merge row that cancelled the run,
// with the payload fields the guards read.
type reopenCloseAnchor struct {
	entry              *audit.Entry
	headSHA            string
	reviewStateAtClose string
}

// evaluateReopenReviveGuards runs guards (a) through (h) of
// handlePullRequestReopened in order and returns the anchoring close row, or
// the first refusal. Guards (i) and (j) need the repository and run in the
// caller.
func (s *Server) evaluateReopenReviveGuards(ctx context.Context, runID uuid.UUID, others []*run.Stage, reopenHead string) (*reopenCloseAnchor, *reopenRefusal) {
	// (a) every audit read the guards need, up front.
	closes, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryPRClosedWithoutMerge)
	if err != nil {
		return nil, &reopenRefusal{reopenRefusedAuditUnreadable, "list " + CategoryPRClosedWithoutMerge + ": " + err.Error()}
	}
	revives, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryRunRevivedOnReopen)
	if err != nil {
		return nil, &reopenRefusal{reopenRefusedAuditUnreadable, "list " + CategoryRunRevivedOnReopen + ": " + err.Error()}
	}
	drops, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryAcceptanceScenarioRetirementDropped)
	if err != nil {
		return nil, &reopenRefusal{reopenRefusedAuditUnreadable, "list " + CategoryAcceptanceScenarioRetirementDropped + ": " + err.Error()}
	}

	// (b) the close is what cancelled a RUNNING run, and it has not already
	// been revived.
	var anchor *reopenCloseAnchor
	for _, e := range closes {
		var cp struct {
			HeadSHA            string `json:"head_sha"`
			RunStateAtClose    string `json:"run_state_at_close"`
			ReviewStateAtClose string `json:"review_state_at_close"`
		}
		if e == nil || json.Unmarshal(e.Payload, &cp) != nil {
			continue
		}
		if st := run.State(cp.RunStateAtClose); st == "" || st.IsTerminal() {
			continue
		}
		if anchor == nil || !auditEntryBefore(e, anchor.entry) {
			anchor = &reopenCloseAnchor{entry: e, headSHA: cp.HeadSHA, reviewStateAtClose: cp.ReviewStateAtClose}
		}
	}
	if anchor == nil {
		return nil, &reopenRefusal{reopenRefusedCloseDidNotCancelRun,
			"no pr_closed_without_merge entry records a non-terminal run_state_at_close; the run was cancelled before (or apart from) the PR close"}
	}
	for _, rv := range revives {
		if rv != nil && !auditEntryBefore(rv, anchor.entry) {
			return nil, &reopenRefusal{reopenRefusedCloseDidNotCancelRun,
				"the newest PR close that cancelled a running run was already revived; the run's current cancel came from elsewhere"}
		}
	}

	// (c) the review was parked at its gate when the PR closed.
	if anchor.reviewStateAtClose != string(run.StageStateAwaitingApproval) {
		return nil, &reopenRefusal{reopenRefusedReviewNotParkedAtClose,
			fmt.Sprintf("review_state_at_close is %q, want %q", anchor.reviewStateAtClose, run.StageStateAwaitingApproval)}
	}

	// (d) the reopen landed inside the window.
	if elapsed := time.Now().UTC().Sub(anchor.entry.Timestamp); elapsed > reopenReviveWindow {
		return nil, &reopenRefusal{reopenRefusedWindowElapsed,
			fmt.Sprintf("the PR closed %s ago; a reopen revives the run only within %s", elapsed.Round(time.Second), reopenReviveWindow)}
	}

	// (e) the head the PR closed at is known. The merge-reconciler poll
	// records no head, so fall back to the run's own last reported head.
	recordedHead := anchor.headSHA
	if recordedHead == "" {
		head, ok, err := s.latestRunHeadSHA(ctx, runID)
		if err != nil {
			return nil, &reopenRefusal{reopenRefusedAuditUnreadable, "resolve the run's reported head: " + err.Error()}
		}
		if ok {
			recordedHead = head
		}
	}
	if recordedHead == "" {
		return nil, &reopenRefusal{reopenRefusedCloseHeadUnrecorded,
			"neither the close entry nor the run's reported heads name the head the PR closed at"}
	}

	// (f) the PR was reopened at that same head.
	if reopenHead != recordedHead {
		return nil, &reopenRefusal{reopenRefusedHeadChanged,
			fmt.Sprintf("the PR reopened at head %q but closed at %q", reopenHead, recordedHead)}
	}

	// (g) the cancel recorded no stage_cancelled acceptance retirement-drop
	// row: that writer's guarantee is that a recorded row is never
	// contradicted (acceptance_retirement_cancel.go), and a revived run's
	// acceptance stage could still persist the retirement.
	for _, d := range drops {
		if d == nil || auditEntryBefore(d, anchor.entry) {
			continue
		}
		var dp struct {
			CancelSource string `json:"cancel_source"`
		}
		if json.Unmarshal(d.Payload, &dp) == nil && dp.CancelSource == cancelSourceStageCancelled {
			return nil, &reopenRefusal{reopenRefusedAcceptanceRetirementsDropped,
				"the PR-close cancel recorded an acceptance_scenario_retirement_dropped row; reviving would contradict it"}
		}
	}

	// (h) no other stage is in flight.
	for _, st := range others {
		if st.State == run.StageStateDispatched || st.State == run.StageStateRunning {
			return nil, &reopenRefusal{reopenRefusedStageInFlight,
				fmt.Sprintf("%s stage %s is %s", st.Type, st.ID, st.State)}
		}
	}
	return anchor, nil
}

// auditEntryBefore reports whether a sits strictly before b on the run's
// chain: by Sequence when they differ, else by Timestamp.
func auditEntryBefore(a, b *audit.Entry) bool {
	if a.Sequence != b.Sequence {
		return a.Sequence < b.Sequence
	}
	return a.Timestamp.Before(b.Timestamp)
}

// appendReopenReviveRefused appends ONE run_revive_on_reopen_refused entry for
// a candidate reopen the guards refused. Best-effort; a failed append logs.
func (s *Server) appendReopenReviveRefused(ctx context.Context, runID, reviewStageID uuid.UUID, prURL, reopenedBy, reopenHead string, r reopenRefusal) {
	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
		"pull_request.reopened: run revive refused",
		slog.String("pr_url", prURL),
		slog.String("run_id", runID.String()),
		slog.String("reason", r.reason),
		slog.String("detail", r.detail))
	payload, _ := json.Marshal(map[string]any{
		"pr_url":          prURL,
		"reason":          r.reason,
		"detail":          r.detail,
		"reopened_by":     reopenedBy,
		"reopen_head_sha": reopenHead,
	})
	systemKind := audit.ActorSystem
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &reviewStageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryRunReviveOnReopenRefused,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
			"pull_request.reopened: run_revive_on_reopen_refused audit append failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
	}
}
