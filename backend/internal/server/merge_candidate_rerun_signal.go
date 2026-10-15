package server

import (
	"context"
	"encoding/json"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// runMergeCandidateRerunPayload is the run read's ADVISORY merge_candidate_rerun
// block (E86.54 / #4391): the run's newest merge-candidate verdict is FAILED
// and nothing has re-opened or rewritten the head since, so the operator may
// judge the failure infrastructure and call fishhawk_rebase_run_branch with
// rerun_failed_verify:true. The MCP mirror
// (backend/internal/mcpserver/client.go runMergeCandidateRerun) decodes it; the
// json tags MUST byte-match or a field silently decodes to its zero value.
type runMergeCandidateRerunPayload struct {
	// HeadSHA is the head the failed verdict judged.
	HeadSHA string `json:"head_sha"`
	// Cause is the failed pass's cause: base_advance, conflict_resolution or
	// fan_in.
	Cause string `json:"cause"`
	// FailedSequence is the failed merge_candidate_verified row's chain
	// sequence; a re-run voids exactly that row.
	FailedSequence int64 `json:"failed_sequence"`
	// QueuedFixupStageID and QueuedFixupSequence name the queued,
	// never-dispatched ADR-090 D6 fix-up the re-run would supersede in place
	// (and refund). Omitted when the implement stage sits at its review gate.
	QueuedFixupStageID  string `json:"queued_fixup_stage_id,omitempty"`
	QueuedFixupSequence int64  `json:"queued_fixup_sequence,omitempty"`
	// Note is one sentence naming what the re-run would do.
	Note string `json:"note"`
}

// mergeCandidateRerunHeadWritingCategories are the rows that put a NEW head on
// the run branch; one sequenced after the failed verdict means the failed head
// is no longer the one a re-run would judge. A performed branch_rebased is
// filtered separately (an already-up-to-date one writes nothing).
var mergeCandidateRerunHeadWritingCategories = []string{
	CategoryFixupPushed, CategoryConflictResolutionPushed, "child_pushed", CategoryOperatorCommitVouched,
}

// mergeCandidateRerunSignalFor builds the run read's merge_candidate_rerun
// block, or (nil, nil) when no re-run is advertisable. AUDIT-FIRST with NO forge
// call (the run read is hot), so it is ADVISORY: the verb re-reads the live PR
// head and is the authority. In order it answers nil when:
//
//   - the run is terminal, or no audit/run repository is wired;
//   - the run's newest merge_candidate_verified row is not failed (a passed,
//     not_executed or operator-void row);
//   - a stage_merge_candidate_verify_triggered row, or a head-writing row
//     (fixup_pushed, conflict_resolution_pushed, child_pushed,
//     operator_commit_vouched, a PERFORMED branch_rebased), follows it;
//   - the run has no implement stage.
//
// At the review gate (succeeded or awaiting_approval) the block carries the
// head, cause and failed sequence: the verb voids and starts the pass there,
// and an occupied ADR-092 base HOLDS that pass rather than refusing it. When the
// stage is pending or awaiting_host_dispatch it calls the verb's own
// eligibility predicate (queuedMergeCandidateFixupFor, including its
// occupied-base pre-check) with the base ref and branch the failed pass's
// consumed trigger recorded, and carries the block plus queued_fixup_* ONLY
// when the predicate admits, so the advertisement never offers a call the verb
// would refuse up front. Any other state answers nil. Every read error is
// returned for the caller's degrade (warn and omit the block).
func (s *Server) mergeCandidateRerunSignalFor(ctx context.Context, runRow *run.Run) (*runMergeCandidateRerunPayload, error) {
	if runRow == nil || s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil || runRow.State.IsTerminal() {
		return nil, nil
	}
	verdicts, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runRow.ID, CategoryMergeCandidateVerified)
	if err != nil {
		return nil, err
	}
	failed := newestEntry(verdicts)
	if failed == nil {
		return nil, nil
	}
	var verdict mergeCandidateVerifiedPayload
	if json.Unmarshal(failed.Payload, &verdict) != nil || verdict.Result != mergeCandidateResultFailed || verdict.HeadSHA == "" {
		return nil, nil
	}
	triggers, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runRow.ID, CategoryStageMergeCandidateVerifyTriggered)
	if err != nil {
		return nil, err
	}
	var consumed *mergeCandidateVerifyTrigger
	for _, e := range triggers {
		if e.Sequence > failed.Sequence {
			return nil, nil
		}
		if e.Sequence == verdict.TriggerSequence {
			var t mergeCandidateVerifyTrigger
			if json.Unmarshal(e.Payload, &t) == nil {
				consumed = &t
			}
		}
	}
	if moved, err := s.mergeCandidateHeadWrittenAfter(ctx, runRow, failed.Sequence); err != nil || moved {
		return nil, err
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runRow.ID)
	if err != nil {
		return nil, err
	}
	var impl *run.Stage
	for _, st := range stages {
		if st.Type == run.StageTypeImplement {
			impl = st
			break
		}
	}
	if impl == nil {
		return nil, nil
	}
	out := &runMergeCandidateRerunPayload{HeadSHA: verdict.HeadSHA, Cause: verdict.Cause, FailedSequence: failed.Sequence}
	switch impl.State {
	case run.StageStateSucceeded, run.StageStateAwaitingApproval:
		out.Note = "the newest merge-candidate verdict for " + verdict.HeadSHA + " FAILED with the implement stage at its review gate; if the failure was infrastructure, not the change, fishhawk_rebase_run_branch rerun_failed_verify:true (with a reason) voids it and starts a verify-only pass (held behind another run's live pass on an occupied ADR-092 base). Advisory: the verb re-reads the live PR head."
		return out, nil
	case run.StageStatePending, run.StageStateAwaitingHostDispatch:
	default:
		return nil, nil
	}
	if consumed == nil || consumed.BaseRef == "" || consumed.Branch == "" {
		// The failed pass's trigger is unreadable, so the base the verb's
		// queue pre-check needs is unknown: never advertise a call the verb
		// might refuse.
		return nil, nil
	}
	p := mergeCandidateVerifyParams{Branch: consumed.Branch, BaseRef: consumed.BaseRef, HeadSHA: verdict.HeadSHA, Cause: verdict.Cause}
	q, refusal, err := s.queuedMergeCandidateFixupFor(ctx, runRow, impl, p, rebaseOperatorActor(ctx))
	if err != nil || refusal != "" || q == nil {
		return nil, err
	}
	out.QueuedFixupStageID = impl.ID.String()
	out.QueuedFixupSequence = q.Trigger.Sequence
	out.Note = "the newest merge-candidate verdict for " + verdict.HeadSHA + " FAILED and its ADR-090 D6 fix-up is queued, never dispatched; if the failure was infrastructure, not the change, fishhawk_rebase_run_branch rerun_failed_verify:true (with a reason) supersedes that fix-up IN PLACE with a verify-only pass and refunds its fix-up pass (the stage keeps its place). Advisory: the verb re-reads the live PR head and re-checks eligibility under the stage's admission fence."
	return out, nil
}

// mergeCandidateHeadWrittenAfter reports whether a head-writing row is
// sequenced after seq: one of mergeCandidateRerunHeadWritingCategories, or a
// branch_rebased row that performed a merge.
func (s *Server) mergeCandidateHeadWrittenAfter(ctx context.Context, runRow *run.Run, seq int64) (bool, error) {
	for _, c := range mergeCandidateRerunHeadWritingCategories {
		rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runRow.ID, c)
		if err != nil {
			return false, err
		}
		for _, e := range rows {
			if e.Sequence > seq {
				return true, nil
			}
		}
	}
	rebased, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runRow.ID, CategoryBranchRebased)
	if err != nil {
		return false, err
	}
	for _, e := range rebased {
		if e.Sequence <= seq {
			continue
		}
		var p struct {
			AlreadyUpToDate bool `json:"already_up_to_date"`
		}
		if json.Unmarshal(e.Payload, &p) != nil || !p.AlreadyUpToDate {
			return true, nil
		}
	}
	return false, nil
}

// newestEntry returns the highest-sequence entry, or nil.
func newestEntry(entries []*audit.Entry) *audit.Entry {
	var newest *audit.Entry
	for _, e := range entries {
		if newest == nil || e.Sequence > newest.Sequence {
			newest = e
		}
	}
	return newest
}
