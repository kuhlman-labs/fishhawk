package server

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// ADR-090 D5 (E83.33 / #4018): a decomposed parent's consolidated implement
// review is HELD until the consolidated fan-in head carries a `passed`
// merge-candidate verify, and that result is the authoritative parent-level
// verify evidence the review reads.
//
//   - holdConsolidatedReviewForFanInVerify is consulted by
//     DispatchConsolidatedReview once the compare has resolved the consolidated
//     head. It triggers the verify-only pass (system actor) when the head is
//     unverified and holds the round until a verdict lands. The pass ALWAYS
//     starts with cause fan_in (#4812, ADR-092 D1: fan_in precedes approval),
//     whatever the head classifies as, and fanInHoldOnStart reads the start
//     answer explicitly: only a triggered in_flight answer is a start.
//   - fanInVerifyGateRun turns the head's recorded `passed` row into the
//     GateVerifyRun runImplementReviewsForTree injects on a consolidated round.
//
//   - FanInVerifySettled (#4785) is the orchestrator's read-only
//     FanInVerifyGate: in a workflow that orders acceptance BEFORE review
//     (feature_change), Advance holds the parent's never-dispatched acceptance
//     stage `pending` while it answers false, and fires
//     DispatchConsolidatedReview (whose hold below starts the pass).
//
// RELEASE needs no code here. The pass re-opened the parent implement stage and
// re-parked its review stage; the merge_candidate_verified report arm
// (succeedMergeCandidateVerifiedStage) settles the implement stage to the
// trigger's recorded PriorState and calls Orchestrator.Advance explicitly
// (approval condition C1 on #4018), which re-walks the review stage and
// re-enters DispatchConsolidatedReview; the #1063 existing-children guard keeps
// the re-opened parent implement from re-minting children. With acceptance
// ahead of review (#4785) the pass re-parks no review (it is still pending) and
// that same Advance reaches the pending acceptance stage instead: it finds the
// head settled through FanInVerifySettled, dispatches acceptance and re-fires
// DispatchConsolidatedReview, whose hold now releases the consolidated round.

// holdConsolidatedReviewForFanInVerify reports whether the consolidated review
// for headSHA must be held (ADR-090 D5). runRow is a decomposed parent (the
// caller has already established it has children); base and branch are the
// consolidated compare's base ref and branch.
//
// Answers, by the head's merge-candidate state:
//
//   - not_required (no declared verify command, D4, or a head the runner
//     already gated) and passed: no hold — the review dispatches.
//   - failed: hold, and start nothing. The merge gate names fishhawk_fixup_stage
//     and the report arm already applied D6; a fix-up's new head is runner-gated.
//   - in_flight: hold; the live pass's report releases it.
//   - unverified: start the pass under the system actor with cause fan_in and
//     hold (#4812). The cause is FORCED: a consolidated head carrying a base
//     merge classifies base_advance (or conflict_resolution), a serialized
//     cause the ADR-092 queue would route into serializeMergeCandidateVerifyPass,
//     which answers not_queued (and starts nothing) for a parent whose
//     consolidated review has not run. The start answer is read by
//     fanInHoldOnStart: not_required or passed (the chain moved between the two
//     reads) releases; a triggered in_flight is the only answer logged as a
//     started pass; every other answer holds with a log naming it.
//
// FAIL-CLOSED: a state read error, a refused start, or an unknown state holds
// with a WARN. Dispatching the gating review on a head whose verify state is
// unknown is exactly the gap D5 closes; the merge gate fails closed on the same
// read, so the hold is never the only signal.
func (s *Server) holdConsolidatedReviewForFanInVerify(ctx context.Context, runRow *run.Run, base, branch, headSHA string) bool {
	logAttrs := []slog.Attr{
		slog.String("run_id", runRow.ID.String()),
		slog.String("head_sha", headSHA),
	}
	state, err := s.mergeCandidateVerifyState(ctx, runRow, headSHA)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"consolidated review: merge-candidate verify state unreadable — holding the review (ADR-090 D5, fail-closed)",
			append(logAttrs, slog.String("error", err.Error()))...)
		return true
	}
	switch state.State {
	case mergeCandidateStateNotRequired, mergeCandidateStatePassed:
		return false
	case mergeCandidateStateFailed:
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"consolidated review: merge-candidate verify FAILED for the fan-in head — holding the review; route a fix-up with fishhawk_fixup_stage",
			append(logAttrs, slog.String("cause", state.Cause))...)
		return true
	case mergeCandidateStateInFlight:
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"consolidated review: merge-candidate verify pass in flight for the fan-in head — holding the review until it reports",
			append(logAttrs, slog.String("stage_id", state.StageID.String()))...)
		return true
	case mergeCandidateStateUnverified:
		// #4812, ADR-092 D1 (fan_in precedes approval): the hold gates the
		// consolidated review that precedes approval, so its pass is never a
		// queue-serialized cause, even when the head classifies base_advance (an
		// operator-vouched or server-advanced base merge) or conflict_resolution.
		// The merge gate still reads a verdict for exactly the merge-time head,
		// and a later base advance at merge goes through the queue as today.
		cause := mergeCandidateCauseFanIn
		logAttrs = append(logAttrs, slog.String("head_cause", state.Cause))
		start, refusal := s.startMergeCandidateVerifyPass(ctx, runRow.ID, mergeCandidateVerifyParams{
			Branch:  branch,
			BaseRef: base,
			HeadSHA: headSHA,
			Cause:   cause,
		}, mergeCandidateSystemActor())
		return s.fanInHoldOnStart(ctx, logAttrs, cause, start, refusal)
	default:
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"consolidated review: unknown merge-candidate verify state — holding the review",
			append(logAttrs, slog.String("state", state.State))...)
		return true
	}
}

// fanInHoldOnStart reads the answer of the D5 hold's pass start and reports
// whether the consolidated review must be held (#4812). Only an in_flight answer
// that THIS start triggered is logged as a started pass; every other answer is
// named by its state, so a start that enqueued or started nothing can never
// read as a live pass:
//
//   - a refusal or a nil answer: hold, WARN;
//   - not_required or passed: release (the chain moved between the two reads);
//   - in_flight, triggered: hold, INFO "pass started";
//   - in_flight, untriggered: hold, INFO (a live pass already runs for the head);
//   - failed: hold, WARN naming the fix-up;
//   - not_queued, held, or an unknown state: hold, WARN naming start_state.
func (s *Server) fanInHoldOnStart(ctx context.Context, logAttrs []slog.Attr, cause string,
	start *mergeCandidateVerifyStart, refusal *mergeCandidateVerifyRefusal) bool {
	if refusal != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"consolidated review: merge-candidate verify pass could not be started — holding the review",
			append(logAttrs, slog.String("refusal", refusal.Reason))...)
		return true
	}
	if start == nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"consolidated review: merge-candidate verify start returned no answer — holding the review",
			append(logAttrs, slog.String("cause", cause))...)
		return true
	}
	if start.State == mergeCandidateStateNotRequired || start.State == mergeCandidateStatePassed {
		return false
	}
	if start.State == mergeCandidateStateInFlight && start.Triggered {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"consolidated review: merge-candidate verify pass started for the fan-in head — holding the review; dispatch the parent implement stage",
			append(logAttrs,
				slog.String("cause", cause),
				slog.String("stage_id", start.StageID.String()),
				slog.Bool("triggered", true))...)
		return true
	}
	attrs := append(logAttrs, slog.String("cause", cause), slog.String("start_state", start.State))
	switch start.State {
	case mergeCandidateStateInFlight:
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"consolidated review: merge-candidate verify pass already in flight for the fan-in head — holding the review until it reports",
			append(attrs, slog.String("stage_id", start.StageID.String()))...)
	case mergeCandidateStateFailed:
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"consolidated review: merge-candidate verify FAILED for the fan-in head — holding the review; route a fix-up with fishhawk_fixup_stage",
			attrs...)
	case mergeCandidateStateNotQueued:
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"consolidated review: the merge-candidate verify start answered not_queued: nothing was enqueued or started — holding the review",
			attrs...)
	case mergeCandidateStateHeld:
		liveRunID := ""
		if start.LiveRunID != nil {
			liveRunID = start.LiveRunID.String()
		}
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"consolidated review: the merge-candidate verify start answered held behind the base's live pass: nothing was started — holding the review",
			append(attrs, slog.Int("position", start.Position), slog.String("live_run_id", liveRunID))...)
	default:
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"consolidated review: unknown merge-candidate verify start answer — holding the review",
			attrs...)
	}
	return true
}

var _ orchestrator.FanInVerifyGate = (*Server)(nil)

// FanInVerifySettled reports whether a decomposed parent's consolidated head
// has a SETTLED merge-candidate verify (#4785) — the orchestrator's read-only
// FanInVerifyGate, consulted before it dispatches the parent's acceptance stage
// in a workflow that orders acceptance ahead of review. It starts nothing:
// DispatchConsolidatedReview's D5 hold owns the start.
//
// The head is resolved with the SAME forgeCompareFor + ComparePatch(base,
// branch) runConsolidatedReviewRound uses, so this read and the D5 hold bind to
// the identical head. Answers:
//
//   - a child run, a run with no decomposed children, or a forge that is not
//     wired: settled (true) — DispatchConsolidatedReview skips those too, so
//     holding would wedge acceptance with nothing to release it;
//   - passed, not_required and FAILED: settled. FAILED counts as settled ON
//     PURPOSE: holding acceptance on a failed head would deadlock, because the
//     operator fix-up's #3116 admission still refuses while acceptance is
//     pending. The consolidated review's own D5 hold and the ADR-090 merge gate
//     keep the failed verdict blocking;
//   - unverified (which covers a not_executed verdict) and in_flight, or any
//     unknown state: not settled (false);
//   - a run, children, compare or state read error: the error (the caller
//     keeps acceptance pending and the next Advance retries).
func (s *Server) FanInVerifySettled(ctx context.Context, runID uuid.UUID, base, branch string) (bool, error) {
	if s.cfg.RunRepo == nil {
		return true, nil
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		return false, fmt.Errorf("fan-in verify: get run: %w", err)
	}
	if runRow.DecomposedFrom != nil {
		return true, nil
	}
	children, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{DecomposedFrom: &runID, Limit: 1})
	if err != nil {
		return false, fmt.Errorf("fan-in verify: list children: %w", err)
	}
	if len(children) == 0 {
		return true, nil
	}
	comparer, scope, repo, reason := s.forgeCompareFor(runRow)
	if reason != "" {
		return true, nil
	}
	cmp, err := comparer.ComparePatch(ctx, scope, repo, base, branch)
	if err != nil {
		return false, fmt.Errorf("fan-in verify: compare patch: %w", err)
	}
	state, err := s.mergeCandidateVerifyState(ctx, runRow, cmp.HeadSHA)
	if err != nil {
		return false, fmt.Errorf("fan-in verify: merge-candidate verify state: %w", err)
	}
	switch state.State {
	case mergeCandidateStateNotRequired, mergeCandidateStatePassed, mergeCandidateStateFailed:
		return true, nil
	default:
		return false, nil
	}
}

// fanInVerifyGateRun returns the authoritative parent-level verify evidence for
// a consolidated review round of headSHA: the recorded `passed`
// merge-candidate verify bound to EXACTLY that head (D2), as one GateVerifyRun
// plus a matching summary. It answers nil, nil for every other round — a round
// whose context carries no consolidated origin (every ordinary run, and a
// parent's fix-up re-review, which carries its own bundle evidence), a head
// with no passed verdict — so those prompts stay byte-identical.
//
// The origin key is the #4077 round source, which DispatchConsolidatedReview
// stamps and the boot re-dispatch of an orphaned consolidated round restores,
// so a re-dispatched round reads the same evidence. The output tail is the
// runner-redacted, server-bounded tail of the recorded row; it is UNTRUSTED,
// and the gate-evidence renderer frames every verify tail inside the #3192
// verify-output envelope. A chain read error is WARN-logged and contributes
// nothing: the hold already decided this round may run, so the degrade is the
// pre-ADR-090 evidence (the per-slice rollup and its named
// decomposed_parent_no_parent_level_verify absence), never a blocked review.
func (s *Server) fanInVerifyGateRun(ctx context.Context, runID uuid.UUID, headSHA string) (*prompt.GateVerifyRun, *prompt.GateVerifySummary) {
	src, ok := reviewRoundSourceFrom(ctx)
	if !ok || src.Origin != reviewRoundOriginConsolidated || headSHA == "" || s.cfg.AuditRepo == nil {
		return nil, nil
	}
	verdict, err := s.newestMergeCandidateVerdict(ctx, runID, headSHA)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"consolidated review: merge-candidate verdict unreadable — no parent-level verify evidence for this round",
			slog.String("run_id", runID.String()),
			slog.String("head_sha", headSHA),
			slog.String("error", err.Error()))
		return nil, nil
	}
	if verdict == nil || verdict.Result != mergeCandidateResultPassed {
		return nil, nil
	}
	return &prompt.GateVerifyRun{
			Command:    verdict.VerifyCommand,
			ExitCode:   0,
			HeadSHA:    headSHA,
			Outcome:    mergeCandidateResultPassed,
			OutputTail: verdict.OutputTail,
		}, &prompt.GateVerifySummary{
			Outcome:       mergeCandidateResultPassed,
			Iterations:    1,
			MaxIterations: 1,
		}
}
