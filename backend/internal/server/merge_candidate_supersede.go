package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// #4811: a merge-candidate verify pass triggered for head H1 and still
// UNDISPATCHED (its implement stage pending or awaiting_host_dispatch) is
// stale once the run's head moves to H2. Before this file, a held or
// not_queued answer from serializeMergeCandidateVerifyPass returned before
// beginMergeCandidateVerifyPass, so begin's PENDING REPLACEMENT arm never ran
// and the slot waiter later dispatched the H1 pass on an H2 branch. This file
// is the shared supersede core both arms call:
//
//   - detectStaleMergeCandidatePass is READ-ONLY: it names the exact live
//     trigger it checked (its chain sequence) and the recorded head write
//     sequenced after it that moved the head (or the producer's head);
//   - settleStaleMergeCandidatePass settles THAT trigger not_executed through
//     settleMergeCandidatePassNotExecutedPinned, a compare-and-swap on its
//     sequence (R1): a replacement appended since detection stays live;
//   - supersedeStaleMergeCandidatePass runs detect then settle under the
//     caller's implement-stage admission fence and logs each step;
//   - supersedeStalePassOnQueueAnswer / answerQueuedWithoutPass are the
//     producer arm on the held / not_queued answers (slice 1 wires them);
//     the host-dispatch marker arm (slice 2) calls the core directly.
//
// Declared residuals: a DISPATCHED pass whose runner died before it fetched
// the prompt is left to the runner's head_moved not_executed (#5164); and the
// window between the record's own trigger read and its AppendChained is
// closed only by the process-local admission fence (one fishhawkd per
// database, as #1936 already assumes).

// mergeCandidateReasonSuperseded leads the not_executed reason (and the
// stage's transient failure reason) a supersession records.
const mergeCandidateReasonSuperseded = "superseded"

// hostDispatchReasonMergeCandidatePassSuperseded is the host-dispatch marker's
// 409 details.reason when it refuses a superseded pass (the marker arm).
const hostDispatchReasonMergeCandidatePassSuperseded = "merge_candidate_pass_superseded"

// mergeCandidateSupersedeProducerSource names the producer's own head as the
// superseding source when no recorded head write after the trigger moved it.
const mergeCandidateSupersedeProducerSource = "producer_head"

// implementStageStrict returns the run's first implement stage. Unlike
// findImplementStage it returns a stage-list read error instead of reading it
// as absence (R2): (nil, nil) only when the list read SUCCEEDED and holds no
// implement stage.
func (s *Server) implementStageStrict(ctx context.Context, runID uuid.UUID) (*run.Stage, error) {
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		return nil, err // stage list unreadable: fail closed (#4811 R2)
	}
	for _, st := range stages {
		if st.Type == run.StageTypeImplement {
			return st, nil
		}
	}
	return nil, nil
}

// recordedHeadWrite is one chain row that recorded a new run head.
type recordedHeadWrite struct {
	head, category string
	sequence       int64
}

// newestRecordedHeadAfter returns the highest-sequence row across
// gateViewCurrentHeadSources sequenced STRICTLY after afterSeq, with
// gateViewCurrentHead's per-source rules: an already_up_to_date branch_rebased
// is skipped, a branch_rebased without new_head_sha falls back to
// merge_commit_sha, and an undecodable row or empty sha is skipped. found is
// false when no such row exists. Every read error is returned.
func (s *Server) newestRecordedHeadAfter(ctx context.Context, runID uuid.UUID, afterSeq int64) (recordedHeadWrite, bool, error) {
	var best recordedHeadWrite
	found := false
	for _, src := range gateViewCurrentHeadSources {
		entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, src.category)
		if err != nil {
			return recordedHeadWrite{}, false, err
		}
		for _, e := range entries {
			if e.Sequence <= afterSeq {
				continue
			}
			var p map[string]any
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			if src.category == CategoryBranchRebased {
				if up, _ := p["already_up_to_date"].(bool); up {
					continue
				}
			}
			sha, _ := p[src.field].(string)
			if sha == "" && src.category == CategoryBranchRebased {
				sha, _ = p["merge_commit_sha"].(string)
			}
			if sha == "" || (found && e.Sequence < best.sequence) {
				continue
			}
			best, found = recordedHeadWrite{head: sha, category: src.category, sequence: e.Sequence}, true
		}
	}
	return best, found, nil
}

// staleMergeCandidatePass names the exact live trigger detection checked and
// what superseded it. SupersedingSequence is 0 when the producer's head (not
// a recorded row) superseded it.
type staleMergeCandidatePass struct {
	StageID             uuid.UUID
	StageState          run.StageState
	TriggerSequence     int64
	ExpectedHead        string
	SupersedingHead     string
	Source              string
	SupersedingSequence int64
}

// detectStaleMergeCandidatePass is READ-ONLY. It answers the run's live
// merge-candidate pass when it is stale, or nil:
//
//   - the implement stage via implementStageStrict (a list error is returned);
//   - stageID (when not uuid.Nil) must be that stage, so a caller holding a
//     different stage short-circuits before any write (approval condition D5);
//   - only a pending or awaiting_host_dispatch stage: a dispatched or running
//     pass belongs to its runner;
//   - a malformed or half-populated newest trigger reads as NO live pass
//     (R6), matching resolveMergeCandidateVerifyTrigger, so a dispatch never
//     loops on a 500; every other read error is returned;
//   - stale when a recorded head write sequenced after the trigger names a
//     different head (a recorded row wins for the details), or when
//     producerHead is set and differs from the trigger's expected head.
func (s *Server) detectStaleMergeCandidatePass(ctx context.Context, runID, stageID uuid.UUID, producerHead string) (*staleMergeCandidatePass, error) {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return nil, nil
	}
	impl, err := s.implementStageStrict(ctx, runID)
	if err != nil {
		return nil, err
	}
	if impl == nil || (stageID != uuid.Nil && impl.ID != stageID) {
		return nil, nil
	}
	if impl.State != run.StageStatePending && impl.State != run.StageStateAwaitingHostDispatch {
		return nil, nil
	}
	live, err := s.liveMergeCandidateTrigger(ctx, runID, impl.ID)
	if err != nil {
		if errors.Is(err, errMergeCandidateTriggerPayload) {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"merge-candidate supersede: the newest verify trigger is malformed — reading it as no live pass",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", impl.ID.String()))
			return nil, nil
		}
		return nil, err
	}
	if live == nil {
		return nil, nil
	}
	rec, found, err := s.newestRecordedHeadAfter(ctx, runID, live.sequence)
	if err != nil {
		return nil, err
	}
	stale := &staleMergeCandidatePass{StageID: impl.ID, StageState: impl.State,
		TriggerSequence: live.sequence, ExpectedHead: live.payload.ExpectedHeadSHA}
	switch {
	case found && rec.head != live.payload.ExpectedHeadSHA:
		stale.SupersedingHead, stale.Source, stale.SupersedingSequence = rec.head, rec.category, rec.sequence
	case producerHead != "" && producerHead != live.payload.ExpectedHeadSHA:
		stale.SupersedingHead, stale.Source = producerHead, mergeCandidateSupersedeProducerSource
	default:
		return nil, nil
	}
	return stale, nil
}

// supersededReason is the not_executed reason a supersession records.
func supersededReason(stale *staleMergeCandidatePass) string {
	at := ""
	if stale.SupersedingSequence > 0 {
		at = fmt.Sprintf(" at sequence %d", stale.SupersedingSequence)
	}
	return fmt.Sprintf("%s: the run's head moved to %s (%s%s) after this verify-only pass was triggered for %s; no verify ran",
		mergeCandidateReasonSuperseded, stale.SupersedingHead, stale.Source, at, stale.ExpectedHead)
}

// settleStaleMergeCandidatePass settles the detected trigger not_executed,
// PINNED to stale.TriggerSequence: when the stage's newest trigger is no
// longer that one (a replacement landed since detection) it writes nothing
// and answers mergeCandidateSettleTriggerReplaced. beforeRestore fails the
// stage (FailureC) so RestoreFixupStage has a failed stage to restore.
func (s *Server) settleStaleMergeCandidatePass(ctx context.Context, runID uuid.UUID, stale *staleMergeCandidatePass) (mergeCandidateSettleStep, error) {
	reason := supersededReason(stale)
	return s.settleMergeCandidatePassNotExecutedPinned(ctx, runID, stale.StageID, stale.TriggerSequence, reason, func() error {
		_, err := run.FailStage(ctx, s.cfg.RunRepo, stale.StageID, run.FailureC, reason)
		return err
	})
}

// mergeCandidateSupersession is supersedeStaleMergeCandidatePass's answer.
// Superseded says detection found a stale pass; Settled says the settle
// reached mergeCandidateSettled. LeftState is the stage state re-read after a
// non-settled step ("" when unread).
type mergeCandidateSupersession struct {
	Superseded bool
	Settled    bool
	Step       mergeCandidateSettleStep
	Stale      staleMergeCandidatePass
	LeftState  run.StageState
}

// mergeCandidateSettleStepName renders a settle step for logs and details.
func mergeCandidateSettleStepName(step mergeCandidateSettleStep) string {
	switch step {
	case mergeCandidateSettleNoLiveTrigger:
		return "no_live_trigger"
	case mergeCandidateSettleRecordFailed:
		return "record_failed"
	case mergeCandidateSettleBeforeRestoreFailed:
		return "before_restore_failed"
	case mergeCandidateSettleRestoreFailed:
		return "restore_failed"
	case mergeCandidateSettled:
		return "settled"
	case mergeCandidateSettleTriggerReplaced:
		return "trigger_replaced"
	case mergeCandidateSettleReadFailed:
		return "read_failed"
	}
	return "unknown"
}

// supersedeStaleMergeCandidatePass detects and settles a stale undispatched
// pass on the implement stage stageID (uuid.Nil = the run's implement stage).
// The CALLER holds that stage's admission fence across the call. A detection
// read error, and a settle-time read error (mergeCandidateSettleReadFailed,
// approval condition D3), are returned; every other step answers nil with the
// step named:
//
//   - settled: notified;
//   - trigger_replaced / no_live_trigger: nothing written, the newer trigger
//     (if any) is live and left alone;
//   - record_failed: nothing written, the stage untouched, the trigger live;
//   - before_restore_failed (R5): the trigger is consumed but FailStage did
//     not finish, so the stage is left where its walk stopped (pending,
//     dispatched or running) with no runner — recovered by
//     fishhawk_reap_stage, or the dispatch watchdog for a dispatched stage;
//   - restore_failed: the stage is failed.
func (s *Server) supersedeStaleMergeCandidatePass(ctx context.Context, runID, stageID uuid.UUID, producerHead string) (mergeCandidateSupersession, error) {
	stale, err := s.detectStaleMergeCandidatePass(ctx, runID, stageID, producerHead)
	if err != nil || stale == nil {
		return mergeCandidateSupersession{}, err
	}
	step, serr := s.settleStaleMergeCandidatePass(ctx, runID, stale)
	sup := mergeCandidateSupersession{Superseded: true, Settled: step == mergeCandidateSettled, Step: step, Stale: *stale}
	if step == mergeCandidateSettleReadFailed {
		return sup, fmt.Errorf("settling the superseded merge-candidate pass could not read its trigger: %w", serr)
	}
	if step != mergeCandidateSettled {
		if st, gerr := s.cfg.RunRepo.GetStage(ctx, stale.StageID); gerr == nil {
			sup.LeftState = st.State
		}
	}
	attrs := []slog.Attr{
		slog.String("run_id", runID.String()),
		slog.String("stage_id", stale.StageID.String()),
		slog.Int64("trigger_sequence", stale.TriggerSequence),
		slog.String("expected_head_sha", stale.ExpectedHead),
		slog.String("superseding_head_sha", stale.SupersedingHead),
		slog.String("superseding_source", stale.Source),
		slog.String("settle_step", mergeCandidateSettleStepName(step)),
	}
	if serr != nil {
		attrs = append(attrs, slog.String("error", serr.Error()))
	}
	switch step {
	case mergeCandidateSettled:
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"merge-candidate supersede: a stale undispatched verify pass was settled not_executed", attrs...)
		s.notifyStatusUpdate(ctx, runID, "merge_candidate_verify_superseded")
	case mergeCandidateSettleTriggerReplaced, mergeCandidateSettleNoLiveTrigger:
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"merge-candidate supersede: the checked trigger is no longer the stage's live one — nothing written, any replacement left live", attrs...)
	case mergeCandidateSettleRecordFailed:
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge-candidate supersede: the not_executed row was not persisted — stage untouched, trigger still live", attrs...)
	case mergeCandidateSettleBeforeRestoreFailed:
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			fmt.Sprintf("merge-candidate supersede: the trigger was consumed but failing the stage did not finish; the stage is left %s with no runner; recover it with fishhawk_reap_stage (POST /v0/runs/{run_id}/stages/{stage_id}/reap-failure); a stage left dispatched is also failed by the dispatch watchdog when --enable-dispatch-watchdog is set", sup.LeftState),
			attrs...)
	case mergeCandidateSettleRestoreFailed:
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge-candidate supersede: the trigger was consumed and the stage failed, but the restore failed — the stage is failed", attrs...)
	}
	return sup, nil
}

// mergeCandidateSupersedeAdvance runs Orchestrator.Advance after a settle. It
// is a package-level var only as a test seam (R4, the upkeepListOpenPulls
// precedent): tests swap it with t.Cleanup and never run in parallel. It
// nil-guards the Orchestrator itself (approval condition D2), so a call site
// that forgets its own guard cannot panic.
var mergeCandidateSupersedeAdvance = func(ctx context.Context, s *Server, runID uuid.UUID) error {
	if s.cfg.Orchestrator == nil {
		return nil
	}
	_, err := s.cfg.Orchestrator.Advance(ctx, runID)
	return err
}

// mergeCandidateSupersedeAdvances is the producer arm's Advance condition
// (approval condition D4): settled, and restore_failed so the failure path
// stays in force (the refuseMergeCandidatePassToIncapableRunner precedent).
// Every other step leaves the run where it was and advances nothing.
func mergeCandidateSupersedeAdvances(step mergeCandidateSettleStep) bool {
	return step == mergeCandidateSettled || step == mergeCandidateSettleRestoreFailed
}

// supersedeStalePassOnQueueAnswer is the producer arm on a held / not_queued
// answer: under the implement stage's admission fence (bounded by
// stageAdmissionTryWait) it settles a stale undispatched pass, then releases
// the fence BEFORE any Advance. A fence it cannot take is a WARN and nil: the
// host-dispatch marker check is the backstop. Advance is skipped when the
// caller already held the fence (it would run under the admission lock) and
// when no Orchestrator is wired. Every read error is returned.
func (s *Server) supersedeStalePassOnQueueAnswer(ctx context.Context, runID uuid.UUID, producerHead, answer string) error {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return nil
	}
	impl, err := s.implementStageStrict(ctx, runID)
	if err != nil {
		return err
	}
	if impl == nil {
		return nil
	}
	callerHeld := stageAdmissionHeldBy(ctx, impl.ID)
	fctx, unlock, acquired := s.tryHoldStageAdmission(ctx, impl.ID, stageAdmissionTryWait)
	if !acquired {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge-candidate supersede: the implement stage's admission fence is busy, so a stale pending pass was not checked; the host-dispatch marker check is the backstop",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", impl.ID.String()),
			slog.String("queue_answer", answer))
		return nil
	}
	sup, err := s.supersedeStaleMergeCandidatePass(fctx, runID, impl.ID, producerHead)
	unlock() // release the fence BEFORE Advance (#4811)
	if err != nil {
		return err
	}
	if mergeCandidateSupersedeAdvances(sup.Step) && !callerHeld && s.cfg.Orchestrator != nil {
		if aerr := mergeCandidateSupersedeAdvance(ctx, s, runID); aerr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"merge-candidate supersede: orchestrator advance after the settle failed",
				slog.String("run_id", runID.String()),
				slog.String("error", aerr.Error()))
		}
	}
	return nil
}

// answerQueuedWithoutPass returns the serializer's held / not_queued start
// after the producer arm ran. A read error turns the answer into a refusal
// (R2's producer posture): the caller holds on it and nothing was started.
func (s *Server) answerQueuedWithoutPass(ctx context.Context, runID uuid.UUID, head string,
	start *mergeCandidateVerifyStart) (*mergeCandidateVerifyStart, *mergeCandidateVerifyRefusal) {
	if err := s.supersedeStalePassOnQueueAnswer(ctx, runID, head, start.State); err != nil {
		return nil, &mergeCandidateVerifyRefusal{Reason: fmt.Sprintf(
			"the run's stages or merge-candidate chain could not be read to settle a stale pending verify pass (%s); the queue answered %s and nothing was started; re-invoke fishhawk_rebase_run_branch to retry",
			err.Error(), start.State)}
	}
	return start, nil
}
