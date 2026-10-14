package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// --- #4391: the operator's verify-only re-run of a FAILED merge-candidate
// head (fishhawk_rebase_run_branch rerun_failed_verify) ---
//
// Every arm reads COMMITTED state back — the audit-chain delta the call
// appended and the implement stage's state — never only the response, because
// a refusal string is identical whether or not the guard fired for the right
// reason.

const rerunReason = "gate host starved (load 140 on 10 cores)"

// chainLen is the number of rows appended so far.
func chainLen(au *auditFake) int {
	au.mu.Lock()
	defer au.mu.Unlock()
	return len(au.appended)
}

// chainDelta returns every row appended after the first n.
func chainDelta(au *auditFake, n int) []audit.ChainAppendParams {
	au.mu.Lock()
	defer au.mu.Unlock()
	return append([]audit.ChainAppendParams(nil), au.appended[n:]...)
}

// mcRows filters rows to the two merge-candidate pass categories (verdicts and
// triggers), keeping their order.
func mcRows(rows []audit.ChainAppendParams) []audit.ChainAppendParams {
	var out []audit.ChainAppendParams
	for _, r := range rows {
		if r.Category == CategoryMergeCandidateVerified || r.Category == CategoryStageMergeCandidateVerifyTriggered {
			out = append(out, r)
		}
	}
	return out
}

func decodeVerified(t *testing.T, raw []byte) mergeCandidateVerifiedPayload {
	t.Helper()
	var p mergeCandidateVerifiedPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode merge_candidate_verified: %v", err)
	}
	return p
}

// seedFailedRebaseHead makes the up-to-date stub's live head
// (rebaseNewHeadSHA) a base-advance head with a REAL recorded failed pass: a
// performed branch_rebased row, a trigger, then the runner's failed report.
// The implement stage is left at its gate (succeeded), as the report arm
// leaves it. It returns the implement stage and the chain sequences of the
// trigger and the failed row (the fake stamps 1-based append order).
func seedFailedRebaseHead(t *testing.T, sd *rebaseSeed) (*run.Stage, int64, int64) {
	t.Helper()
	impl := crImplementStage(t, sd)
	seedMCVRow(sd, nil, CategoryBranchRebased, map[string]any{
		"new_head_sha": rebaseNewHeadSHA, "merge_commit_sha": rebaseNewHeadSHA, "already_up_to_date": false,
	})
	seedMCVRow(sd, &impl.ID, CategoryStageMergeCandidateVerifyTriggered, mergeCandidateVerifyTrigger{
		Branch: rebaseBranchName, BaseRef: rebaseBaseRef, ExpectedHeadSHA: rebaseNewHeadSHA,
		Cause: mergeCandidateCauseBaseAdvance, VerifyCommand: mcvCommand,
		PriorState: string(run.StageStateSucceeded),
	})
	triggerSeq := int64(chainLen(sd.au))
	if _, err := sd.s.recordMergeCandidateVerified(context.Background(), sd.runID, impl.ID,
		mergeCandidateResultFailed, "", "FAIL: TestX (load-starved)", audit.ActorAgent, "runner"); err != nil {
		t.Fatalf("seed failed verdict: %v", err)
	}
	failedSeq := int64(chainLen(sd.au))
	impl.State = run.StageStateSucceeded
	return impl, triggerSeq, failedSeq
}

func (sd *rebaseSeed) verifyState(t *testing.T, head string) string {
	t.Helper()
	st, err := sd.s.mergeCandidateVerifyState(context.Background(), sd.rr.getRuns[sd.runID], head)
	if err != nil {
		t.Fatalf("mergeCandidateVerifyState: %v", err)
	}
	return st.State
}

// TestRebaseRerunFailedVerify_VoidsThenTriggers (V1/V7, through the HTTP
// handler): on the up-to-date arm with the implement stage at its gate, the
// call appends EXACTLY the operator void row and THEN the trigger, re-opens
// the stage, and answers in_flight + triggered + merge_candidate_verify_rerun.
// A passed report then settles the head passed. MECHANISM: without the void
// the head's newest verdict is failed and beginMergeCandidateVerifyPass
// short-circuits to state failed, so no trigger row lands.
func TestRebaseRerunFailedVerify_VoidsThenTriggers(t *testing.T) {
	sd := seedMCVRebase(t, upToDateRebaseStub())
	impl, triggerSeq, failedSeq := seedFailedRebaseHead(t, sd)
	if got := sd.verifyState(t, rebaseNewHeadSHA); got != mergeCandidateStateFailed {
		t.Fatalf("fixture: head state = %q, want failed", got)
	}
	before := chainLen(sd.au)

	w := postRebaseBranch(t, sd.s, sd.runID, rebaseBranchRequest{Confirm: true, Reason: rerunReason, RerunFailedVerify: true}, withRebaseOperator)
	resp := decodeRebase(t, w)

	mc := mcRows(chainDelta(sd.au, before))
	if len(mc) != 2 || mc[0].Category != CategoryMergeCandidateVerified || mc[1].Category != CategoryStageMergeCandidateVerifyTriggered {
		cats := []string{}
		for _, r := range mc {
			cats = append(cats, r.Category)
		}
		t.Fatalf("merge-candidate rows appended = %v, want [merge_candidate_verified (void), stage_merge_candidate_verify_triggered]", cats)
	}
	void := decodeVerified(t, mc[0].Payload)
	if void.Result != mergeCandidateResultNotExecuted || void.HeadSHA != rebaseNewHeadSHA ||
		void.Cause != mergeCandidateCauseBaseAdvance || void.VerifyCommand != mcvCommand || !void.OutputUntrusted {
		t.Errorf("void row = %+v, want not_executed for %s, cause/command copied, output_untrusted", void, rebaseNewHeadSHA)
	}
	if void.Reason != mergeCandidateRerunReasonPrefix+rerunReason {
		t.Errorf("void reason = %q, want %q", void.Reason, mergeCandidateRerunReasonPrefix+rerunReason)
	}
	if void.VoidsSequence != failedSeq || void.TriggerSequence != triggerSeq {
		t.Errorf("void voids_sequence/trigger_sequence = %d/%d, want %d/%d", void.VoidsSequence, void.TriggerSequence, failedSeq, triggerSeq)
	}
	if mc[0].StageID == nil || *mc[0].StageID != impl.ID {
		t.Errorf("void stage = %v, want the failed row's stage %s", mc[0].StageID, impl.ID)
	}
	if mc[0].ActorKind == nil || *mc[0].ActorKind != audit.ActorUser || mc[0].ActorSubject == nil || *mc[0].ActorSubject != "github:ops" {
		t.Errorf("void actor = %v/%v, want the operator user github:ops", mc[0].ActorKind, mc[0].ActorSubject)
	}
	var tr mergeCandidateVerifyTrigger
	if err := json.Unmarshal(mc[1].Payload, &tr); err != nil || tr.ExpectedHeadSHA != rebaseNewHeadSHA || tr.Cause != mergeCandidateCauseBaseAdvance {
		t.Errorf("trigger = %+v (err %v), want base_advance for %s", tr, err, rebaseNewHeadSHA)
	}
	if !resp.AlreadyUpToDate || resp.MergeCandidateVerifyState != mergeCandidateStateInFlight ||
		!resp.MergeCandidateVerifyTriggered || !resp.MergeCandidateVerifyRerun || resp.MergeCandidateVerifyRefusal != "" {
		t.Errorf("response = up_to_date %v state %q triggered %v rerun %v refusal %q, want true/in_flight/true/true/none",
			resp.AlreadyUpToDate, resp.MergeCandidateVerifyState, resp.MergeCandidateVerifyTriggered, resp.MergeCandidateVerifyRerun, resp.MergeCandidateVerifyRefusal)
	}
	if resp.MergeCandidateVerifyStageID != impl.ID.String() || strings.Contains(resp.MergeCandidateVerifyNote, "had no effect") {
		t.Errorf("stage id/note = %q/%q, want %s and the triggered note", resp.MergeCandidateVerifyStageID, resp.MergeCandidateVerifyNote, impl.ID)
	}
	if impl.State != run.StageStatePending {
		t.Errorf("implement stage = %q, want pending (re-opened for the pass)", impl.State)
	}

	// The fresh pass settles the head.
	if _, err := sd.s.recordMergeCandidateVerified(context.Background(), sd.runID, impl.ID,
		mergeCandidateResultPassed, "", "", audit.ActorAgent, "runner"); err != nil {
		t.Fatalf("record passed: %v", err)
	}
	if got := sd.verifyState(t, rebaseNewHeadSHA); got != mergeCandidateStatePassed {
		t.Errorf("head state after the re-run passed = %q, want passed", got)
	}
}

// TestRebaseRerunFailedVerify_NotFailedHasNoEffect (V2): the flag on a head
// that is not failed takes the ordinary path. passed starts nothing and
// appends no row; unverified takes the ordinary trigger with
// merge_candidate_verify_rerun false. Both notes say the flag had no effect.
// MECHANISM: deleting the handler's failed-state guard routes a passed head
// into the re-run arm, whose refusal (the helper finds nothing to void) lands
// on the response — state failed, a refusal — and the assertions go RED.
func TestRebaseRerunFailedVerify_NotFailedHasNoEffect(t *testing.T) {
	t.Run("passed", func(t *testing.T) {
		sd := seedMCVRebase(t, upToDateRebaseStub())
		impl := crImplementStage(t, sd)
		seedMCVRow(sd, nil, CategoryBranchRebased, map[string]any{
			"new_head_sha": rebaseNewHeadSHA, "merge_commit_sha": rebaseNewHeadSHA, "already_up_to_date": false})
		seedMCVRow(sd, &impl.ID, CategoryMergeCandidateVerified, mergeCandidateVerifiedPayload{
			HeadSHA: rebaseNewHeadSHA, Cause: mergeCandidateCauseBaseAdvance, VerifyCommand: mcvCommand,
			Result: mergeCandidateResultPassed, OutputUntrusted: true})
		before := chainLen(sd.au)

		resp := decodeRebase(t, postRebaseBranch(t, sd.s, sd.runID,
			rebaseBranchRequest{Confirm: true, Reason: rerunReason, RerunFailedVerify: true}, withRebaseOperator))
		if mc := mcRows(chainDelta(sd.au, before)); len(mc) != 0 {
			t.Fatalf("merge-candidate rows appended = %d, want 0 (a passed head is never voided)", len(mc))
		}
		if resp.MergeCandidateVerifyState != mergeCandidateStatePassed || resp.MergeCandidateVerifyRerun ||
			resp.MergeCandidateVerifyTriggered || resp.MergeCandidateVerifyRefusal != "" {
			t.Errorf("response = state %q rerun %v triggered %v refusal %q, want passed/false/false/none",
				resp.MergeCandidateVerifyState, resp.MergeCandidateVerifyRerun, resp.MergeCandidateVerifyTriggered, resp.MergeCandidateVerifyRefusal)
		}
		if !strings.Contains(resp.MergeCandidateVerifyNote, "rerun_failed_verify had no effect") ||
			!strings.Contains(resp.MergeCandidateVerifyNote, "passed") {
			t.Errorf("note = %q, want the no-effect sentence naming the passed state", resp.MergeCandidateVerifyNote)
		}
		if impl.State != run.StageStateSucceeded {
			t.Errorf("implement stage = %q, want succeeded (untouched)", impl.State)
		}
	})
	t.Run("unverified", func(t *testing.T) {
		sd := seedMCVRebase(t, upToDateRebaseStub())
		impl := crImplementStage(t, sd)
		seedMCVRow(sd, nil, CategoryBranchRebased, map[string]any{
			"new_head_sha": rebaseNewHeadSHA, "merge_commit_sha": rebaseNewHeadSHA, "already_up_to_date": false})
		before := chainLen(sd.au)

		resp := decodeRebase(t, postRebaseBranch(t, sd.s, sd.runID,
			rebaseBranchRequest{Confirm: true, Reason: rerunReason, RerunFailedVerify: true}, withRebaseOperator))
		mc := mcRows(chainDelta(sd.au, before))
		if len(mc) != 1 || mc[0].Category != CategoryStageMergeCandidateVerifyTriggered {
			t.Fatalf("merge-candidate rows appended = %+v, want exactly the ordinary trigger (no void row)", mc)
		}
		if !resp.MergeCandidateVerifyTriggered || resp.MergeCandidateVerifyRerun || resp.MergeCandidateVerifyState != mergeCandidateStateInFlight {
			t.Errorf("response = triggered %v rerun %v state %q, want true/false/in_flight",
				resp.MergeCandidateVerifyTriggered, resp.MergeCandidateVerifyRerun, resp.MergeCandidateVerifyState)
		}
		if !strings.Contains(resp.MergeCandidateVerifyNote, "rerun_failed_verify had no effect") ||
			!strings.Contains(resp.MergeCandidateVerifyNote, "fishhawk_dispatch_stage") {
			t.Errorf("note = %q, want the triggered note plus the no-effect sentence", resp.MergeCandidateVerifyNote)
		}
		if impl.State != run.StageStatePending {
			t.Errorf("implement stage = %q, want pending (ordinary re-open)", impl.State)
		}
	})
}

// TestRebaseRerunFailedVerify_RefusedBesideQueuedFixup (V3, approval
// condition C1, through the HTTP handler): a failed head whose implement stage
// is pending behind a queued fix-up (the D6 auto-route) is refused. The
// COMPLETE audit-chain delta of the call is exactly one branch_rebased row
// (already_up_to_date:true) — zero merge_candidate_verified and zero
// stage_merge_candidate_verify_triggered rows — and the refusal names
// fishhawk_dispatch_stage and the re-run. MECHANISM: deleting the pre-check
// lets the void land before beginMergeCandidateVerifyPass refuses on state
// pending, so the delta gains a merge_candidate_verified row.
func TestRebaseRerunFailedVerify_RefusedBesideQueuedFixup(t *testing.T) {
	sd := seedMCVRebase(t, upToDateRebaseStub())
	impl, _, _ := seedFailedRebaseHead(t, sd)
	seedMCVRow(sd, &impl.ID, CategoryStageFixupTriggered, map[string]any{"delegated_rule": mergeCandidateDelegatedRule})
	impl.State = run.StageStatePending
	before := chainLen(sd.au)

	resp := decodeRebase(t, postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Confirm: true, Reason: rerunReason, RerunFailedVerify: true}, withRebaseOperator))

	delta := chainDelta(sd.au, before)
	if len(delta) != 1 || delta[0].Category != CategoryBranchRebased {
		cats := []string{}
		for _, r := range delta {
			cats = append(cats, r.Category)
		}
		t.Fatalf("audit-chain delta = %v, want exactly [branch_rebased]", cats)
	}
	var br map[string]any
	if err := json.Unmarshal(delta[0].Payload, &br); err != nil || br["already_up_to_date"] != true {
		t.Errorf("branch_rebased payload = %v (err %v), want already_up_to_date:true", br, err)
	}
	for _, want := range []string{"fishhawk_dispatch_stage", "rerun_failed_verify", string(run.StageStatePending), impl.ID.String(), "NOT voided"} {
		if !strings.Contains(resp.MergeCandidateVerifyRefusal, want) {
			t.Errorf("refusal must name %q: %q", want, resp.MergeCandidateVerifyRefusal)
		}
	}
	if resp.MergeCandidateVerifyRerun || resp.MergeCandidateVerifyTriggered || resp.MergeCandidateVerifyState != mergeCandidateStateFailed {
		t.Errorf("response = rerun %v triggered %v state %q, want false/false/failed",
			resp.MergeCandidateVerifyRerun, resp.MergeCandidateVerifyTriggered, resp.MergeCandidateVerifyState)
	}
	if strings.Contains(resp.MergeCandidateVerifyNote, "had no effect") {
		t.Errorf("note = %q: a decided refusal must not also claim the flag had no effect", resp.MergeCandidateVerifyNote)
	}
	if impl.State != run.StageStatePending {
		t.Errorf("implement stage = %q, want pending (the queued fix-up untouched)", impl.State)
	}
	if got := sd.verifyState(t, rebaseNewHeadSHA); got != mergeCandidateStateFailed {
		t.Errorf("head state = %q, want still failed", got)
	}
}

// TestRebaseRerunFailedVerify_NoImplementStage: a run with no implement stage
// to re-open is refused before anything is voided. MECHANISM: deleting the
// nil guard dereferences the missing stage (a panic, RED).
func TestRebaseRerunFailedVerify_NoImplementStage(t *testing.T) {
	sd := seedMCVRebase(t, upToDateRebaseStub())
	impl, _, _ := seedFailedRebaseHead(t, sd)
	var kept []*run.Stage
	for _, st := range sd.rr.stagesByRunID[sd.runID] {
		if st.ID != impl.ID {
			kept = append(kept, st)
		}
	}
	sd.rr.stagesByRunID[sd.runID] = kept
	before := chainLen(sd.au)

	resp := decodeRebase(t, postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Confirm: true, Reason: rerunReason, RerunFailedVerify: true}, withRebaseOperator))
	if mc := mcRows(chainDelta(sd.au, before)); len(mc) != 0 {
		t.Fatalf("merge-candidate rows appended = %d, want 0", len(mc))
	}
	if !strings.Contains(resp.MergeCandidateVerifyRefusal, "no implement stage") || !strings.Contains(resp.MergeCandidateVerifyRefusal, "NOT voided") {
		t.Errorf("refusal = %q, want the missing implement stage named", resp.MergeCandidateVerifyRefusal)
	}
	if resp.MergeCandidateVerifyRerun {
		t.Error("merge_candidate_verify_rerun = true, want false")
	}
}

// TestRebaseRerunFailedVerify_VoidAppendFails (V4): the void append failing
// is a refusal with nothing triggered and the verdict still failed.
// MECHANISM: ignoring the void error falls through to the start, which reads
// the still-failed verdict and answers state failed with NO refusal and
// merge_candidate_verify_rerun true — the refusal assertion goes RED.
func TestRebaseRerunFailedVerify_VoidAppendFails(t *testing.T) {
	sd := seedMCVRebase(t, upToDateRebaseStub())
	impl, _, _ := seedFailedRebaseHead(t, sd)
	sd.au.appendErrCategory = CategoryMergeCandidateVerified
	before := chainLen(sd.au)

	resp := decodeRebase(t, postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Confirm: true, Reason: rerunReason, RerunFailedVerify: true}, withRebaseOperator))
	if mc := mcRows(chainDelta(sd.au, before)); len(mc) != 0 {
		t.Fatalf("merge-candidate rows appended = %d, want 0", len(mc))
	}
	if !strings.Contains(resp.MergeCandidateVerifyRefusal, "voiding the failed merge-candidate verdict") ||
		!strings.Contains(resp.MergeCandidateVerifyRefusal, "still failed") {
		t.Errorf("refusal = %q, want the void failure named", resp.MergeCandidateVerifyRefusal)
	}
	if resp.MergeCandidateVerifyRerun || resp.MergeCandidateVerifyTriggered {
		t.Errorf("rerun/triggered = %v/%v, want false/false", resp.MergeCandidateVerifyRerun, resp.MergeCandidateVerifyTriggered)
	}
	if impl.State != run.StageStateSucceeded {
		t.Errorf("implement stage = %q, want succeeded (not re-opened)", impl.State)
	}
	sd.au.appendErrCategory = ""
	if got := sd.verifyState(t, rebaseNewHeadSHA); got != mergeCandidateStateFailed {
		t.Errorf("head state = %q, want still failed", got)
	}
}

// TestRebaseRerunFailedVerify_StartRefusedAfterVoid: the void landed but the
// pass could not start (a failed trigger append stands in for the TOCTOU arm,
// a stage that left its gate after the pre-check). The 200 says the verdict
// WAS voided, the head reads unverified, and the stage is not re-opened.
func TestRebaseRerunFailedVerify_StartRefusedAfterVoid(t *testing.T) {
	sd := seedMCVRebase(t, upToDateRebaseStub())
	impl, _, _ := seedFailedRebaseHead(t, sd)
	sd.au.appendErrCategory = CategoryStageMergeCandidateVerifyTriggered
	before := chainLen(sd.au)

	resp := decodeRebase(t, postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Confirm: true, Reason: rerunReason, RerunFailedVerify: true}, withRebaseOperator))
	mc := mcRows(chainDelta(sd.au, before))
	if len(mc) != 1 || mc[0].Category != CategoryMergeCandidateVerified {
		t.Fatalf("merge-candidate rows appended = %+v, want exactly the void row", mc)
	}
	if !resp.MergeCandidateVerifyRerun || resp.MergeCandidateVerifyState != mergeCandidateStateUnverified ||
		!strings.Contains(resp.MergeCandidateVerifyRefusal, "WAS voided") {
		t.Errorf("response = rerun %v state %q refusal %q, want true/unverified naming the void",
			resp.MergeCandidateVerifyRerun, resp.MergeCandidateVerifyState, resp.MergeCandidateVerifyRefusal)
	}
	if impl.State != run.StageStateSucceeded {
		t.Errorf("implement stage = %q, want succeeded (not re-opened)", impl.State)
	}
	sd.au.appendErrCategory = ""
	if got := sd.verifyState(t, rebaseNewHeadSHA); got != mergeCandidateStateUnverified {
		t.Errorf("head state = %q, want unverified (voided, re-triggerable)", got)
	}
}

// TestRebaseRerunFailedVerify_PerformedMergeIgnoresFlag (V7): on a call that
// PERFORMS a base merge the new head gets its own pass; the flag voids
// nothing — even with a failed verdict on the pre-merge head — and the note
// says it had no effect.
func TestRebaseRerunFailedVerify_PerformedMergeIgnoresFlag(t *testing.T) {
	sd := seedMCVRebase(t, cleanRebaseStub())
	impl := crImplementStage(t, sd)
	seedMCVRow(sd, &impl.ID, CategoryMergeCandidateVerified, mergeCandidateVerifiedPayload{
		HeadSHA: rebasePriorHeadSHA, Cause: mergeCandidateCauseBaseAdvance, VerifyCommand: mcvCommand,
		Result: mergeCandidateResultFailed, OutputUntrusted: true})
	before := chainLen(sd.au)

	resp := decodeRebase(t, postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Confirm: true, Reason: rerunReason, RerunFailedVerify: true}, withRebaseOperator))
	for _, r := range chainDelta(sd.au, before) {
		if r.Category == CategoryMergeCandidateVerified {
			t.Fatalf("a performed merge appended a merge_candidate_verified row: %s", r.Payload)
		}
	}
	if resp.AlreadyUpToDate || !resp.MergeCandidateVerifyTriggered || resp.MergeCandidateVerifyRerun {
		t.Errorf("response = up_to_date %v triggered %v rerun %v, want false/true/false",
			resp.AlreadyUpToDate, resp.MergeCandidateVerifyTriggered, resp.MergeCandidateVerifyRerun)
	}
	if !strings.Contains(resp.MergeCandidateVerifyNote, "performed a base merge") {
		t.Errorf("note = %q, want the performed-merge no-effect sentence", resp.MergeCandidateVerifyNote)
	}
}

// TestRebaseRerunFailedVerify_BlankReason400 (V6 + approval condition C4):
// rerun_failed_verify with a blank or whitespace reason is a 400
// validation_failed naming the reason field, on the failed-head fixture, and
// nothing is appended. The check sits AFTER the 503 guard (an unconfigured
// server still answers 503) and BEFORE the run lookup (an unknown run answers
// 400, not 404). MECHANISM: deleting the check lets the failed-head call
// proceed to a 200 that voids the verdict.
func TestRebaseRerunFailedVerify_BlankReason400(t *testing.T) {
	for _, reason := range []string{"", "  \t "} {
		sd := seedMCVRebase(t, upToDateRebaseStub())
		seedFailedRebaseHead(t, sd)
		before := chainLen(sd.au)
		w := postRebaseBranch(t, sd.s, sd.runID, rebaseBranchRequest{Confirm: true, Reason: reason, RerunFailedVerify: true}, withRebaseOperator)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("reason %q: status = %d, want 400:\n%s", reason, w.Code, w.Body.String())
		}
		var env struct {
			Error struct {
				Code    string         `json:"code"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || env.Error.Code != "validation_failed" || env.Error.Details["field"] != "reason" {
			t.Errorf("reason %q: body = %s, want validation_failed field reason", reason, w.Body.String())
		}
		if n := len(chainDelta(sd.au, before)); n != 0 {
			t.Errorf("reason %q: rows appended = %d, want 0", reason, n)
		}
	}

	// BEFORE the run lookup: an unknown run is still a 400.
	sd := seedMCVRebase(t, upToDateRebaseStub())
	delete(sd.rr.getRuns, sd.runID)
	if w := postRebaseBranch(t, sd.s, sd.runID, rebaseBranchRequest{Confirm: true, RerunFailedVerify: true}, withRebaseOperator); w.Code != http.StatusBadRequest {
		t.Errorf("unknown run + blank reason: status = %d, want 400 (checked before the run lookup)", w.Code)
	}
	// AFTER the 503 guard: an unconfigured server still answers 503.
	sd = seedMCVRebase(t, upToDateRebaseStub())
	sd.s.cfg.GitHub = nil
	if w := postRebaseBranch(t, sd.s, sd.runID, rebaseBranchRequest{Confirm: true, RerunFailedVerify: true}, withRebaseOperator); w.Code != http.StatusServiceUnavailable {
		t.Errorf("unconfigured + blank reason: status = %d, want 503 (the 503 guard runs first)", w.Code)
	}
}

// --- voidFailedMergeCandidateVerdict (the helper) ---

// TestVoidFailedMergeCandidateVerdict_ReplayOfOriginalIsDuplicate (V5): after
// the void and BEFORE any new trigger, a runner replay of the ORIGINAL failed
// report is answered Duplicate (result not_executed) with no row appended —
// the void copied the failed row's trigger_sequence. MECHANISM: a void
// without that copy leaves the trigger consumed by a row naming another
// sequence, so the replay is refused errMergeCandidateNoLiveTrigger instead.
func TestVoidFailedMergeCandidateVerdict_ReplayOfOriginalIsDuplicate(t *testing.T) {
	f := newMCVFixture(t, mcvUndelegatedSpecYAML)
	recordFailedPass(t, f)
	if err := f.s.voidFailedMergeCandidateVerdict(context.Background(), f.runID, mcvHead, mcvOperator(), "infra"); err != nil {
		t.Fatalf("void: %v", err)
	}
	before := chainLen(f.au)
	rec, err := f.s.recordMergeCandidateVerifiedEvidence(context.Background(), f.runID, f.impl.ID,
		mergeCandidateResultFailed, "", mergeCandidateOutputEvidence{Tail: "replayed FAIL"}, audit.ActorAgent, "runner")
	if err != nil {
		t.Fatalf("replay = %v, want a Duplicate answer", err)
	}
	if !rec.Duplicate || rec.Result != mergeCandidateResultNotExecuted {
		t.Errorf("replay = duplicate %v result %q, want true/not_executed", rec.Duplicate, rec.Result)
	}
	if n := chainLen(f.au) - before; n != 0 {
		t.Errorf("replay appended %d rows, want 0", n)
	}
	st, err := f.s.mergeCandidateVerifyState(context.Background(), f.runRow(t), mcvHead)
	if err != nil || st.State != mergeCandidateStateUnverified {
		t.Errorf("head state = %q (err %v), want unverified (voided, not re-failed)", st.State, err)
	}
}

// TestVoidFailedMergeCandidateVerdict_Refusals pins every refusal by
// committed state: nothing is appended unless the head's NEWEST recorded
// verdict is failed. MECHANISM (not-failed arms): deleting the failed guard
// appends a void over a passed/not_executed verdict or a verdict for another
// head, and the zero-rows assertion goes RED.
func TestVoidFailedMergeCandidateVerdict_Refusals(t *testing.T) {
	cases := []struct {
		name    string
		seed    func(f *mcvFixture)
		actor   mergeCandidateActor
		listErr bool
		wantErr error
	}{
		{"no verdict for the head", func(f *mcvFixture) { f.seedVerdict(mcvOtherHead, mergeCandidateResultFailed) }, mcvOperator(), false, errMergeCandidateVoidNotFailed},
		{"passed", func(f *mcvFixture) { f.seedVerdict(mcvHead, mergeCandidateResultPassed) }, mcvOperator(), false, errMergeCandidateVoidNotFailed},
		{"newest is not_executed (already voided)", func(f *mcvFixture) {
			f.seedVerdict(mcvHead, mergeCandidateResultFailed)
			f.seedVerdict(mcvHead, mergeCandidateResultNotExecuted)
		}, mcvOperator(), false, errMergeCandidateVoidNotFailed},
		{"no actor", func(f *mcvFixture) { f.seedVerdict(mcvHead, mergeCandidateResultFailed) }, mergeCandidateActor{}, false, nil},
		{"chain unreadable", func(f *mcvFixture) { f.seedVerdict(mcvHead, mergeCandidateResultFailed) }, mcvOperator(), true, nil},
		{"failed row carries no stage", func(f *mcvFixture) {
			f.appendRow(nil, CategoryMergeCandidateVerified, mergeCandidateVerifiedPayload{
				HeadSHA: mcvHead, Result: mergeCandidateResultFailed, OutputUntrusted: true})
		}, mcvOperator(), false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newMCVFixture(t, mcvUndelegatedSpecYAML)
			tc.seed(f)
			if tc.listErr {
				f.au.listByCategoryErrCategory = CategoryMergeCandidateVerified
			}
			before := chainLen(f.au)
			err := f.s.voidFailedMergeCandidateVerdict(context.Background(), f.runID, mcvHead, tc.actor, "infra")
			if err == nil {
				t.Fatal("void = nil, want a refusal")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("void = %v, want %v", err, tc.wantErr)
			}
			if n := chainLen(f.au) - before; n != 0 {
				t.Errorf("rows appended = %d, want 0", n)
			}
		})
	}
}

// --- approval condition C2: the re-run is serialized by the ADR-092 queue ---

// TestRebaseRerunFailedVerify_HeldBehindLivePass (V1b, approval condition
// C2; REAL Postgres run/audit/queue stores): run B's base_advance head b1
// failed and B was ejected; run A then took the base's live pass. B's re-run
// lands the void, answers state held with merge_candidate_verify_rerun true,
// and writes NO trigger yet. Once A passes and merges, the queue admits B and
// the trigger for b1 appears. MECHANISM: the queue is wired and A holds the
// line, so only the queued start (startMergeCandidateVerifyPass) holds B;
// calling the direct start (beginMergeCandidateVerifyPass) starts B out of
// turn and the held assertion goes RED.
func TestRebaseRerunFailedVerify_HeldBehindLivePass(t *testing.T) {
	f := newMCQFixture(t)
	spec := []byte(mcvUndelegatedSpecYAML)
	b := f.seed("b", "b0", spec)
	f.behind(b, "b0", "b1")
	if resp := f.rebase(b); resp.MergeCandidateVerifyState != mergeCandidateStateInFlight {
		t.Fatalf("fixture: B rebase state = %q, want in_flight", resp.MergeCandidateVerifyState)
	}
	f.verdict(b, mergeCandidateResultFailed)
	f.requireState(b, mergequeue.StateEjected, mergeCandidateQueueReasonVerifyFailed)
	// The report arm restores the pre-pass gate: implement succeeded and the
	// review stage the pass re-parked back at awaiting_approval.
	f.setStage(b.impl.ID, run.StageStateSucceeded)
	f.setStage(b.review.ID, run.StageStateAwaitingApproval)

	a := f.seed("a", "a0", spec)
	f.behind(a, "a0", "a1")
	if resp := f.rebase(a); resp.MergeCandidateVerifyState != mergeCandidateStateInFlight || !resp.MergeCandidateVerifyTriggered {
		t.Fatalf("fixture: A rebase = %q triggered %v, want A to hold the base's live pass", resp.MergeCandidateVerifyState, resp.MergeCandidateVerifyTriggered)
	}
	f.requireState(a, mergequeue.StateLive, "")
	failedRows := f.rows(b, CategoryMergeCandidateVerified)
	if len(failedRows) != 1 {
		t.Fatalf("fixture: B verdict rows = %d, want the one failed row", len(failedRows))
	}
	triggersBefore := len(f.triggers(b))

	w := postRebaseBranch(t, f.srv, b.id, rebaseBranchRequest{Confirm: true, Reason: rerunReason, RerunFailedVerify: true}, withRebaseOperator)
	resp := decodeRebase(t, w)
	if !resp.AlreadyUpToDate {
		t.Fatalf("fixture: B rerun took the merge arm, want already-up-to-date")
	}
	verdicts := f.rows(b, CategoryMergeCandidateVerified)
	if len(verdicts) != 2 {
		t.Fatalf("B verdict rows = %d, want 2 (the failed row and the void)", len(verdicts))
	}
	void := decodeVerified(t, verdicts[1].Payload)
	if void.Result != mergeCandidateResultNotExecuted || void.HeadSHA != "b1" ||
		!strings.HasPrefix(void.Reason, mergeCandidateRerunReasonPrefix) || void.VoidsSequence != failedRows[0].Sequence {
		t.Errorf("void row = %+v, want not_executed for b1 voiding sequence %d", void, failedRows[0].Sequence)
	}
	if resp.MergeCandidateVerifyState != mergeCandidateStateHeld || !resp.MergeCandidateVerifyRerun || resp.MergeCandidateVerifyTriggered {
		t.Fatalf("B rerun = state %q rerun %v triggered %v, want held/true/false (A holds the line)",
			resp.MergeCandidateVerifyState, resp.MergeCandidateVerifyRerun, resp.MergeCandidateVerifyTriggered)
	}
	if n := len(f.triggers(b)); n != triggersBefore {
		t.Fatalf("B triggers = %d, want %d (no trigger while held)", n, triggersBefore)
	}
	f.requireState(b, mergequeue.StateHeld, "")
	if st := f.stageState(b.impl.ID); st != run.StageStateSucceeded {
		t.Fatalf("B implement = %s, want succeeded (untouched while held)", st)
	}

	// A settles and merges: the hand-off admits B, whose pass now starts.
	f.verdict(a, mergeCandidateResultPassed)
	f.closePR(a, true)
	f.requireState(b, mergequeue.StateLive, "")
	tr := f.triggers(b)
	if len(tr) != triggersBefore+1 || tr[len(tr)-1].ExpectedHeadSHA != "b1" || tr[len(tr)-1].Cause != mergeCandidateCauseBaseAdvance {
		t.Fatalf("B triggers after admission = %+v, want a new base_advance trigger for b1", tr)
	}
	if st := f.stageState(b.impl.ID); st != run.StageStatePending {
		t.Fatalf("B implement after admission = %s, want pending (re-opened)", st)
	}
}
