package server

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

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
//     unverified and holds the round until a verdict lands.
//   - fanInVerifyGateRun turns the head's recorded `passed` row into the
//     GateVerifyRun runImplementReviewsForTree injects on a consolidated round.
//
// RELEASE needs no code here. The pass re-opened the parent implement stage and
// re-parked its review stage; the merge_candidate_verified report arm
// (succeedMergeCandidateVerifiedStage) settles the implement stage to the
// trigger's recorded PriorState and calls Orchestrator.Advance explicitly
// (approval condition C1 on #4018), which re-walks the review stage and
// re-enters DispatchConsolidatedReview; the #1063 existing-children guard keeps
// the re-opened parent implement from re-minting children.

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
//   - unverified: start the pass under the system actor and hold. A start that
//     answers not_required or passed (the chain moved between the two reads)
//     releases instead.
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
		cause := state.Cause
		if !validMergeCandidateCause(cause) {
			cause = mergeCandidateCauseFanIn
		}
		start, refusal := s.startMergeCandidateVerifyPass(ctx, runRow.ID, mergeCandidateVerifyParams{
			Branch:  branch,
			BaseRef: base,
			HeadSHA: headSHA,
			Cause:   cause,
		}, mergeCandidateSystemActor())
		if refusal != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"consolidated review: merge-candidate verify pass could not be started — holding the review",
				append(logAttrs, slog.String("refusal", refusal.Reason))...)
			return true
		}
		if start.State == mergeCandidateStateNotRequired || start.State == mergeCandidateStatePassed {
			return false
		}
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"consolidated review: merge-candidate verify pass started for the fan-in head — holding the review; dispatch the parent implement stage",
			append(logAttrs,
				slog.String("cause", cause),
				slog.String("stage_id", start.StageID.String()),
				slog.Bool("triggered", start.Triggered))...)
		return true
	default:
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"consolidated review: unknown merge-candidate verify state — holding the review",
			append(logAttrs, slog.String("state", state.State))...)
		return true
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
