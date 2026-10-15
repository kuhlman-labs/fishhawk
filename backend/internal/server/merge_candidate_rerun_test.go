package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concurrency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
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

// TestRebaseRerunFailedVerify_RefusedBesideOperatorRoutedFixup (V3, rewritten
// for E86.54): a failed head whose implement stage is pending behind an
// OPERATOR-routed (non-D6) queued fix-up is refused. The COMPLETE audit-chain
// delta of the call is exactly one branch_rebased row
// (already_up_to_date:true). MECHANISM: the fix-up trigger carries no
// `delegated` key, so queuedMergeCandidateFixupFor's D6 check is the only
// refusal in the path; making it always true lands the marker (RED).
func TestRebaseRerunFailedVerify_RefusedBesideOperatorRoutedFixup(t *testing.T) {
	sd := seedMCVRebase(t, upToDateRebaseStub())
	impl, _, _ := seedFailedRebaseHead(t, sd)
	seedMCVRow(sd, &impl.ID, CategoryStageFixupTriggered, map[string]any{
		"prior_state": "succeeded", "reparked_review_stage_id": sd.review.ID.String()})
	impl.State = run.StageStatePending
	before := chainLen(sd.au)

	resp := decodeRebase(t, postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Confirm: true, Reason: rerunReason, RerunFailedVerify: true}, withRebaseOperator))

	delta := chainDelta(sd.au, before)
	if len(delta) != 1 || delta[0].Category != CategoryBranchRebased {
		t.Fatalf("audit-chain delta = %v, want exactly [branch_rebased]", deltaCategories(delta))
	}
	for _, want := range []string{"fishhawk_dispatch_stage", "rerun_failed_verify", "operator-routed", string(run.StageStatePending), impl.ID.String(), "NOT voided", "NOTHING was written"} {
		if !strings.Contains(resp.MergeCandidateVerifyRefusal, want) {
			t.Errorf("refusal must name %q: %q", want, resp.MergeCandidateVerifyRefusal)
		}
	}
	if resp.MergeCandidateVerifyRerun || resp.MergeCandidateVerifyTriggered || resp.MergeCandidateFixupSuperseded ||
		resp.MergeCandidateVerifyState != mergeCandidateStateFailed {
		t.Errorf("response = rerun %v triggered %v superseded %v state %q, want false/false/false/failed",
			resp.MergeCandidateVerifyRerun, resp.MergeCandidateVerifyTriggered, resp.MergeCandidateFixupSuperseded, resp.MergeCandidateVerifyState)
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

// --- E86.54 / #4391: in-place conversion of a queued D6 fix-up ---

func deltaCategories(rows []audit.ChainAppendParams) []string {
	out := []string{}
	for _, r := range rows {
		out = append(out, r.Category)
	}
	return out
}

// seedQueuedD6Fixup is the queued-D6 shape: a real failed pass for the live
// head, then the D6 route's stage_fixup_triggered (the production `delegated`
// key, prior_state succeeded, the review stage re-parked) sequenced after it,
// with the implement stage at state. It returns the stage and the trigger and
// failed-verdict sequences.
func seedQueuedD6Fixup(t *testing.T, sd *rebaseSeed, state run.StageState) (*run.Stage, int64, int64) {
	t.Helper()
	impl, _, failedSeq := seedFailedRebaseHead(t, sd)
	seedMCVRow(sd, &impl.ID, CategoryStageFixupTriggered, map[string]any{
		"delegated": mergeCandidateDelegatedRule, "prior_state": string(run.StageStateSucceeded),
		"reparked_review_stage_id": sd.review.ID.String(), "concern_ids": []string{}})
	impl.State = state
	return impl, int64(chainLen(sd.au)), failedSeq
}

// rerunStatusStore is a test-local concurrency.Store answering StatusForStages
// from a fixed map (or an error); Admit and Promote are never reached.
type rerunStatusStore struct {
	concurrency.Store
	status map[uuid.UUID]concurrency.Status
	err    error
}

func (r *rerunStatusStore) StatusForStages(context.Context, []uuid.UUID) (map[uuid.UUID]concurrency.Status, error) {
	return r.status, r.err
}

// rerunHookAudit wraps the auditFake with a hook fired after a list of one
// category.
type rerunHookAudit struct {
	*auditFake
	hookCategory string
	afterList    func(ctx context.Context)
}

// rerunCallerKey tags each concurrent caller's context.
type rerunCallerKey struct{}

func (h *rerunHookAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	entries, err := h.auditFake.ListForRunByCategory(ctx, runID, category)
	if err == nil && category == h.hookCategory && h.afterList != nil {
		h.afterList(ctx)
	}
	return entries, err
}

func postFixupRerun(t *testing.T, sd *rebaseSeed) rebaseBranchResponse {
	t.Helper()
	return decodeRebase(t, postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Confirm: true, Reason: rerunReason, RerunFailedVerify: true}, withRebaseOperator))
}

// TestRerunQueuedFixup_ConvertsInPlace (T1): on a stage pending or
// awaiting_host_dispatch behind a queued D6 fix-up, the re-run appends
// EXACTLY [branch_rebased, stage_fixup_superseded, merge_candidate_verified
// (void), stage_merge_candidate_verify_triggered], leaves the stage state
// UNCHANGED, and moves its dispatch group from local-implement to
// local-verify. MECHANISM (CF): routing the pending/awaiting_host_dispatch
// branch of rebaseRerunFailedMergeCandidateVerify back to the not-at-gate
// refusal leaves the delta [branch_rebased] only.
func TestRerunQueuedFixup_ConvertsInPlace(t *testing.T) {
	for _, state := range []run.StageState{run.StageStatePending, run.StageStateAwaitingHostDispatch} {
		t.Run(string(state), func(t *testing.T) {
			sd := seedMCVRebase(t, upToDateRebaseStub())
			impl, tSeq, fSeq := seedQueuedD6Fixup(t, sd, state)
			ctx := context.Background()
			runRow := sd.rr.getRuns[sd.runID]
			if g, _, _ := sd.s.resolveStageConcurrency(ctx, runRow, impl, "h1"); g != "local-implement:h1" {
				t.Fatalf("fixture: group before = %q, want local-implement:h1", g)
			}
			before := chainLen(sd.au)

			resp := postFixupRerun(t, sd)

			delta := chainDelta(sd.au, before)
			want := []string{CategoryBranchRebased, CategoryStageFixupSuperseded, CategoryMergeCandidateVerified, CategoryStageMergeCandidateVerifyTriggered}
			if got := deltaCategories(delta); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("audit-chain delta = %v, want %v (refusal %q)", got, want, resp.MergeCandidateVerifyRefusal)
			}
			var marker stageFixupSupersededPayload
			if err := json.Unmarshal(delta[1].Payload, &marker); err != nil {
				t.Fatalf("decode marker: %v", err)
			}
			if marker.TriggerSequence != tSeq || marker.SupersededBy != fixupSupersededByVerifyRerun ||
				marker.FailedVerdictSequence != fSeq || marker.Reason != mergeCandidateRerunReasonPrefix+rerunReason {
				t.Errorf("marker = %+v, want trigger %d, superseded_by %s, failed %d, the operator_rerun reason", marker, tSeq, fixupSupersededByVerifyRerun, fSeq)
			}
			if delta[1].StageID == nil || *delta[1].StageID != impl.ID {
				t.Errorf("marker stage = %v, want %s", delta[1].StageID, impl.ID)
			}
			var tr mergeCandidateVerifyTrigger
			if err := json.Unmarshal(delta[3].Payload, &tr); err != nil {
				t.Fatalf("decode trigger: %v", err)
			}
			if tr.SupersedesFixupTriggerSequence != tSeq || tr.PriorState != string(run.StageStateSucceeded) ||
				tr.ReparkedReviewStageID != sd.review.ID.String() || tr.ExpectedHeadSHA != rebaseNewHeadSHA {
				t.Errorf("conversion trigger = %+v, want supersedes %d with T's anchors for %s", tr, tSeq, rebaseNewHeadSHA)
			}
			if impl.State != state {
				t.Errorf("implement stage = %q, want %q (no transition)", impl.State, state)
			}
			if resp.MergeCandidateVerifyState != mergeCandidateStateInFlight || !resp.MergeCandidateVerifyTriggered ||
				!resp.MergeCandidateVerifyRerun || !resp.MergeCandidateFixupSuperseded || resp.MergeCandidateVerifyRefusal != "" {
				t.Errorf("response = state %q triggered %v rerun %v superseded %v refusal %q, want in_flight/true/true/true/none",
					resp.MergeCandidateVerifyState, resp.MergeCandidateVerifyTriggered, resp.MergeCandidateVerifyRerun,
					resp.MergeCandidateFixupSuperseded, resp.MergeCandidateVerifyRefusal)
			}
			if !strings.Contains(resp.MergeCandidateVerifyNote, "keeps its place") || !strings.Contains(resp.MergeCandidateVerifyNote, "refunded") {
				t.Errorf("note = %q, want the in-place conversion named", resp.MergeCandidateVerifyNote)
			}
			if g, _, _ := sd.s.resolveStageConcurrency(ctx, runRow, impl, "h1"); g != "local-verify:h1" {
				t.Errorf("group after = %q, want local-verify:h1", g)
			}
		})
	}
}

// TestRerunQueuedFixup_RefusesIneligible (T2): each subtest seeds ONLY its own
// disqualifier on the T1 fixture; the delta is [branch_rebased], the head is
// still failed, the stage unchanged, and the refusal names the reason.
// MECHANISM (CF per arm): delete the named check in queuedMergeCandidateFixupFor
// and only that subtest's marker lands.
func TestRerunQueuedFixup_RefusesIneligible(t *testing.T) {
	type tc struct {
		name  string
		setup func(sd *rebaseSeed, impl *run.Stage)
		state run.StageState
		want  string
	}
	cases := []tc{
		{name: "dispatched", state: run.StageStateDispatched, want: `state "dispatched"`},
		{name: "running", state: run.StageStateRunning, want: `state "running"`},
		{name: "failed", state: run.StageStateFailed, want: `state "failed"`},
		{name: "fixup_older_than_verdict", want: "older than the failed verdict", setup: func(sd *rebaseSeed, impl *run.Stage) {
			// Re-seed a fresh failed verdict AFTER the D6 trigger.
			seedMCVRow(sd, &impl.ID, CategoryMergeCandidateVerified, mergeCandidateVerifiedPayload{
				HeadSHA: rebaseNewHeadSHA, Cause: mergeCandidateCauseBaseAdvance, VerifyCommand: mcvCommand,
				Result: mergeCandidateResultFailed, OutputUntrusted: true})
		}},
		{name: "unrestorable_prior_state", want: "not a review-gate state", setup: func(sd *rebaseSeed, impl *run.Stage) {
			seedMCVRow(sd, &impl.ID, CategoryStageFixupTriggered, map[string]any{
				"delegated": mergeCandidateDelegatedRule, "prior_state": "running"})
		}},
		{name: "unparseable_reparked_review", want: "is unparseable", setup: func(sd *rebaseSeed, impl *run.Stage) {
			seedMCVRow(sd, &impl.ID, CategoryStageFixupTriggered, map[string]any{
				"delegated": mergeCandidateDelegatedRule, "prior_state": "succeeded", "reparked_review_stage_id": "not-a-uuid"})
		}},
		{name: "fixup_trigger_read_error", want: "eligibility could not be read", setup: func(sd *rebaseSeed, _ *run.Stage) {
			sd.au.listByCategoryErrCategory = CategoryStageFixupTriggered
		}},
		{name: "disqualifying_read_error", want: "eligibility could not be read", setup: func(sd *rebaseSeed, _ *run.Stage) {
			sd.au.listByCategoryErrCategory = CategoryStageConcurrencyAdmitted
		}},
	}
	// A LITERAL list, never the production set: a subtest derived from the set
	// would vanish with the category it guards.
	for _, c := range []string{
		"fixup_pushed", "fixup_no_changes", "stage_fixup_recovered", "stage_fixup_superseded", "dispatch_reaper_failed",
		"stage_retried", "stage_override_retried", "stage_concurrency_admitted", "installation_token_issued",
		"stage_concurrency_queued", "stage_merge_candidate_verify_triggered", "stage_conflict_resolution_triggered",
	} {
		cat := c
		cases = append(cases, tc{name: "after_" + cat, want: "a " + cat + " row", setup: func(sd *rebaseSeed, impl *run.Stage) {
			seedMCVRow(sd, &impl.ID, cat, map[string]any{"seeded": true})
		}})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sd := seedMCVRebase(t, upToDateRebaseStub())
			state := run.StageStatePending
			if c.state != "" {
				state = c.state
			}
			impl, _, _ := seedQueuedD6Fixup(t, sd, state)
			if c.setup != nil {
				c.setup(sd, impl)
			}
			before := chainLen(sd.au)
			resp := postFixupRerun(t, sd)
			if delta := chainDelta(sd.au, before); len(delta) != 1 || delta[0].Category != CategoryBranchRebased {
				t.Fatalf("audit-chain delta = %v, want exactly [branch_rebased]", deltaCategories(delta))
			}
			if !strings.Contains(resp.MergeCandidateVerifyRefusal, c.want) || !strings.Contains(resp.MergeCandidateVerifyRefusal, "NOT voided") {
				t.Errorf("refusal = %q, want it to name %q and NOT voided", resp.MergeCandidateVerifyRefusal, c.want)
			}
			if resp.MergeCandidateFixupSuperseded || resp.MergeCandidateVerifyRerun {
				t.Errorf("superseded/rerun = %v/%v, want false/false", resp.MergeCandidateFixupSuperseded, resp.MergeCandidateVerifyRerun)
			}
			if impl.State != state {
				t.Errorf("implement stage = %q, want %q", impl.State, state)
			}
			sd.au.listByCategoryErrCategory = ""
			if got := sd.verifyState(t, rebaseNewHeadSHA); got != mergeCandidateStateFailed {
				t.Errorf("head state = %q, want still failed", got)
			}
		})
	}
}

// TestRerunQueuedFixup_RefusesSlotStoreWaiterWithoutAuditRow (T3): the slot
// store holds a queued (or held) row for the stage while the chain carries NO
// stage_concurrency_queued row, or the store read errors. MECHANISM (CF):
// replacing the StatusForStages call with an empty map (or treating the error
// as empty) lands the marker.
func TestRerunQueuedFixup_RefusesSlotStoreWaiterWithoutAuditRow(t *testing.T) {
	cases := []struct {
		name  string
		store func(stageID uuid.UUID) *rerunStatusStore
		want  []string
	}{
		{name: "queued", want: []string{"local-implement:h1", "position 2"}, store: func(id uuid.UUID) *rerunStatusStore {
			return &rerunStatusStore{status: map[uuid.UUID]concurrency.Status{id: {
				State: concurrency.SlotQueued, GroupKey: "local-implement:h1", Position: 2, WaiterLive: true}}}
		}},
		{name: "held", want: []string{"holds a concurrency slot", "local-implement:h1"}, store: func(id uuid.UUID) *rerunStatusStore {
			return &rerunStatusStore{status: map[uuid.UUID]concurrency.Status{id: {
				State: concurrency.SlotHeld, GroupKey: "local-implement:h1"}}}
		}},
		{name: "read_error", want: []string{"slot store could not be read"}, store: func(uuid.UUID) *rerunStatusStore {
			return &rerunStatusStore{err: errors.New("pool closed")}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sd := seedMCVRebase(t, upToDateRebaseStub())
			impl, _, _ := seedQueuedD6Fixup(t, sd, run.StageStatePending)
			sd.s.cfg.Concurrency = c.store(impl.ID)
			before := chainLen(sd.au)
			resp := postFixupRerun(t, sd)
			if delta := chainDelta(sd.au, before); len(delta) != 1 {
				t.Fatalf("audit-chain delta = %v, want exactly [branch_rebased]", deltaCategories(delta))
			}
			for _, w := range c.want {
				if !strings.Contains(resp.MergeCandidateVerifyRefusal, w) {
					t.Errorf("refusal = %q, want it to name %q", resp.MergeCandidateVerifyRefusal, w)
				}
			}
		})
	}
}

// TestRerunQueuedFixup_ConcurrentSecondRerunRefused (T4): two reruns race on
// one stage with a real Orchestrator (LockStageAdmission). A two-party barrier
// fires after each caller's stage_fixup_superseded eligibility read. Under the
// fence the second caller cannot arrive while the first waits, the barrier
// times out, and the second reads the first's rows: ONE marker, ONE void, ONE
// conversion trigger. MECHANISM (CF): a no-op holdStageAdmission lets both
// pass the barrier having read no marker → `markers = 2, want 1`.
func TestRerunQueuedFixup_ConcurrentSecondRerunRefused(t *testing.T) {
	sd := seedMCVRebase(t, upToDateRebaseStub())
	impl, _, _ := seedQueuedD6Fixup(t, sd, run.StageStatePending)
	sd.s.cfg.Orchestrator = &orchestrator.Orchestrator{Runs: sd.rr}
	barrier := newTwoPartyBarrier(timescale.D(300 * time.Millisecond))
	var once sync.Map
	sd.s.cfg.AuditRepo = &rerunHookAudit{auditFake: sd.au, hookCategory: CategoryStageFixupSuperseded, afterList: func(ctx context.Context) {
		// Only each caller's FIRST marker read (the eligibility scan) arrives.
		if _, loaded := once.LoadOrStore(ctx.Value(rerunCallerKey{}), true); !loaded {
			barrier.arrive()
		}
	}}
	runRow := sd.rr.getRuns[sd.runID]
	before := chainLen(sd.au)
	var wg sync.WaitGroup
	results := make([]rebaseMergeCandidateFields, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.WithValue(context.Background(), rerunCallerKey{}, i)
			results[i] = sd.s.rerunQueuedMergeCandidateFixup(ctx, runRow, impl,
				rebaseBranchName, rebaseBaseRef, rebaseNewHeadSHA, mergeCandidateCauseBaseAdvance, rerunReason)
		}(i)
	}
	wg.Wait()
	counts := map[string]int{}
	for _, r := range chainDelta(sd.au, before) {
		counts[r.Category]++
	}
	if counts[CategoryStageFixupSuperseded] != 1 || counts[CategoryMergeCandidateVerified] != 1 || counts[CategoryStageMergeCandidateVerifyTriggered] != 1 {
		t.Fatalf("markers = %d, voids = %d, triggers = %d, want 1/1/1", counts[CategoryStageFixupSuperseded],
			counts[CategoryMergeCandidateVerified], counts[CategoryStageMergeCandidateVerifyTriggered])
	}
	converted, refused := 0, 0
	for _, r := range results {
		if r.FixupSuperseded && r.State == mergeCandidateStateInFlight {
			converted++
		} else if strings.Contains(r.Refusal, "NOTHING was written") {
			refused++
		}
	}
	if converted != 1 || refused != 1 {
		t.Errorf("results = %+v, want one conversion and one refusal", results)
	}
}

// TestRerunQueuedFixup_MarkerAppendFails (T6a): the marker append failing
// refuses with nothing written. MECHANISM (CF): ignoring the append error
// lets the void land and the delta assertion fails.
func TestRerunQueuedFixup_MarkerAppendFails(t *testing.T) {
	sd := seedMCVRebase(t, upToDateRebaseStub())
	impl, _, _ := seedQueuedD6Fixup(t, sd, run.StageStatePending)
	sd.au.appendErrCategory = CategoryStageFixupSuperseded
	before := chainLen(sd.au)
	resp := postFixupRerun(t, sd)
	if delta := chainDelta(sd.au, before); len(delta) != 1 || delta[0].Category != CategoryBranchRebased {
		t.Fatalf("audit-chain delta = %v, want exactly [branch_rebased]", deltaCategories(delta))
	}
	if !strings.Contains(resp.MergeCandidateVerifyRefusal, "NOTHING was written") ||
		!strings.Contains(resp.MergeCandidateVerifyRefusal, "still queued and dispatchable") {
		t.Errorf("refusal = %q, want nothing-written and the fix-up still dispatchable", resp.MergeCandidateVerifyRefusal)
	}
	if impl.State != run.StageStatePending {
		t.Errorf("stage = %q, want pending", impl.State)
	}
}

// TestRerunQueuedFixup_VoidFailsAfterMarker (T6b): the marker landed but the
// void failed; the refusal names the marker's sequence, NOT voided and the
// fix-up still dispatchable as a fix-up, and a second re-run is refused
// because the marker follows T. MECHANISM (CF): ignoring the void error
// continues to begin (which answers failed); the refusal no longer names the
// marker.
func TestRerunQueuedFixup_VoidFailsAfterMarker(t *testing.T) {
	sd := seedMCVRebase(t, upToDateRebaseStub())
	seedQueuedD6Fixup(t, sd, run.StageStatePending)
	sd.au.appendErrCategory = CategoryMergeCandidateVerified
	before := chainLen(sd.au)
	resp := postFixupRerun(t, sd)
	delta := chainDelta(sd.au, before)
	if got := deltaCategories(delta); strings.Join(got, ",") != CategoryBranchRebased+","+CategoryStageFixupSuperseded {
		t.Fatalf("audit-chain delta = %v, want [branch_rebased, stage_fixup_superseded]", got)
	}
	markerSeq := fmt.Sprintf("sequence %d", before+2)
	for _, w := range []string{markerSeq, "NOT voided", "dispatchable as a fix-up"} {
		if !strings.Contains(resp.MergeCandidateVerifyRefusal, w) {
			t.Errorf("refusal = %q, want it to name %q", resp.MergeCandidateVerifyRefusal, w)
		}
	}
	sd.au.appendErrCategory = ""
	again := postFixupRerun(t, sd)
	if !strings.Contains(again.MergeCandidateVerifyRefusal, "a "+CategoryStageFixupSuperseded+" row") {
		t.Errorf("second re-run refusal = %q, want the spent marker named", again.MergeCandidateVerifyRefusal)
	}
}

// TestRerunQueuedFixup_StartRefusedThenResumedByRebase (T6c): the conversion
// trigger append fails, so marker + void land with no trigger and the refusal
// says re-invoke. A plain rebase WITHOUT the flag then reaches begin's arm and
// converts. MECHANISM (CF): deleting the beginSupersededFixupPass call makes
// begin fall to the prior-state refusal on the pending stage; no trigger.
func TestRerunQueuedFixup_StartRefusedThenResumedByRebase(t *testing.T) {
	sd := seedMCVRebase(t, upToDateRebaseStub())
	impl, tSeq, _ := seedQueuedD6Fixup(t, sd, run.StageStateAwaitingHostDispatch)
	sd.au.appendErrCategory = CategoryStageMergeCandidateVerifyTriggered
	before := chainLen(sd.au)
	resp := postFixupRerun(t, sd)
	if got := deltaCategories(chainDelta(sd.au, before)); strings.Join(got, ",") !=
		strings.Join([]string{CategoryBranchRebased, CategoryStageFixupSuperseded, CategoryMergeCandidateVerified}, ",") {
		t.Fatalf("audit-chain delta = %v, want [branch_rebased, marker, void]", got)
	}
	if resp.MergeCandidateVerifyState != mergeCandidateStateUnverified || !resp.MergeCandidateVerifyRerun ||
		resp.MergeCandidateFixupSuperseded || !strings.Contains(resp.MergeCandidateVerifyRefusal, "re-invoke fishhawk_rebase_run_branch (no flag needed)") {
		t.Fatalf("response = state %q rerun %v superseded %v refusal %q, want unverified/true/false naming the re-invoke",
			resp.MergeCandidateVerifyState, resp.MergeCandidateVerifyRerun, resp.MergeCandidateFixupSuperseded, resp.MergeCandidateVerifyRefusal)
	}
	sd.au.appendErrCategory = ""
	before = chainLen(sd.au)
	plain := decodeRebase(t, postRebaseBranch(t, sd.s, sd.runID, rebaseBranchRequest{Confirm: true, Reason: "resume"}, withRebaseOperator))
	mc := mcRows(chainDelta(sd.au, before))
	if len(mc) != 1 || mc[0].Category != CategoryStageMergeCandidateVerifyTriggered {
		t.Fatalf("plain rebase rows = %v, want the conversion trigger (refusal %q)", deltaCategories(mc), plain.MergeCandidateVerifyRefusal)
	}
	var tr mergeCandidateVerifyTrigger
	_ = json.Unmarshal(mc[0].Payload, &tr)
	if tr.SupersedesFixupTriggerSequence != tSeq || tr.ReparkedReviewStageID != sd.review.ID.String() {
		t.Errorf("trigger = %+v, want supersedes %d with T's anchors", tr, tSeq)
	}
	if impl.State != run.StageStateAwaitingHostDispatch {
		t.Errorf("stage = %q, want awaiting_host_dispatch (kept its place)", impl.State)
	}
}

// startRerunHead calls startMergeCandidateVerifyPass for the seeded head.
func startRerunHead(ctx context.Context, sd *rebaseSeed) (*mergeCandidateVerifyStart, *mergeCandidateVerifyRefusal) {
	return sd.s.startMergeCandidateVerifyPass(ctx, sd.runID, mergeCandidateVerifyParams{
		Branch: rebaseBranchName, BaseRef: rebaseBaseRef, HeadSHA: rebaseNewHeadSHA, Cause: mergeCandidateCauseBaseAdvance,
	}, mergeCandidateSystemActor())
}

// seedMarkerAndVoid adds the stage_fixup_superseded marker for tSeq and voids
// the failed verdict, as the verb does before its start.
func seedMarkerAndVoid(t *testing.T, sd *rebaseSeed, impl *run.Stage, tSeq, fSeq int64) {
	t.Helper()
	seedMCVRow(sd, &impl.ID, CategoryStageFixupSuperseded, stageFixupSupersededPayload{
		TriggerSequence: tSeq, SupersededBy: fixupSupersededByVerifyRerun, FailedVerdictSequence: fSeq, Reason: "operator_rerun: x"})
	if err := sd.s.voidFailedMergeCandidateVerdict(context.Background(), sd.runID, rebaseNewHeadSHA, mergeCandidateSystemActor(), "x"); err != nil {
		t.Fatalf("void: %v", err)
	}
}

// TestBeginSupersededFixupArm (T7): the arm's refusals and fall-through.
// MECHANISM (CF per row): drop the post-marker disqualifying scan (inert row's
// trigger lands), skip the slot read (slot row's trigger lands), delete the
// beginSupersededFixupPass call (converts row gets the prior-state refusal).
func TestBeginSupersededFixupArm(t *testing.T) {
	t.Run("converts", func(t *testing.T) {
		sd := seedMCVRebase(t, upToDateRebaseStub())
		impl, tSeq, fSeq := seedQueuedD6Fixup(t, sd, run.StageStatePending)
		seedMarkerAndVoid(t, sd, impl, tSeq, fSeq)
		start, refusal := startRerunHead(context.Background(), sd)
		if refusal != nil || start.SupersededFixupSequence != tSeq || !start.Triggered {
			t.Fatalf("start = %+v refusal %+v, want a conversion naming %d", start, refusal, tSeq)
		}
	})
	t.Run("refuses_inert_marker", func(t *testing.T) {
		sd := seedMCVRebase(t, upToDateRebaseStub())
		impl, tSeq, fSeq := seedQueuedD6Fixup(t, sd, run.StageStatePending)
		seedMarkerAndVoid(t, sd, impl, tSeq, fSeq)
		seedMCVRow(sd, &impl.ID, CategoryStageConcurrencyAdmitted, map[string]any{"group": "local-implement:h1"})
		before := chainLen(sd.au)
		_, refusal := startRerunHead(context.Background(), sd)
		if refusal == nil || !strings.Contains(refusal.Reason, CategoryStageConcurrencyAdmitted) || !strings.Contains(refusal.Reason, "INERT") || refusal.Transient {
			t.Fatalf("refusal = %+v, want the inert marker named (not transient)", refusal)
		}
		if n := len(chainDelta(sd.au, before)); n != 0 {
			t.Errorf("rows appended = %d, want 0", n)
		}
	})
	t.Run("refuses_slot_waiter", func(t *testing.T) {
		sd := seedMCVRebase(t, upToDateRebaseStub())
		impl, tSeq, fSeq := seedQueuedD6Fixup(t, sd, run.StageStatePending)
		seedMarkerAndVoid(t, sd, impl, tSeq, fSeq)
		sd.s.cfg.Concurrency = &rerunStatusStore{status: map[uuid.UUID]concurrency.Status{impl.ID: {
			State: concurrency.SlotQueued, GroupKey: "local-implement:h1", Position: 1}}}
		before := chainLen(sd.au)
		_, refusal := startRerunHead(context.Background(), sd)
		if refusal == nil || !strings.Contains(refusal.Reason, "waiting for a slot") {
			t.Fatalf("refusal = %+v, want the slot waiter named", refusal)
		}
		if n := len(chainDelta(sd.au, before)); n != 0 {
			t.Errorf("rows appended = %d, want 0", n)
		}
	})
	t.Run("not_handled_falls_through", func(t *testing.T) {
		sd := seedMCVRebase(t, upToDateRebaseStub())
		seedQueuedD6Fixup(t, sd, run.StageStatePending)
		if err := sd.s.voidFailedMergeCandidateVerdict(context.Background(), sd.runID, rebaseNewHeadSHA, mergeCandidateSystemActor(), "x"); err != nil {
			t.Fatalf("void: %v", err)
		}
		_, refusal := startRerunHead(context.Background(), sd)
		want := `the implement stage is in state "pending"; only a stage parked at its review gate (succeeded or awaiting_approval) can be re-opened for a merge-candidate verify pass`
		if refusal == nil || refusal.Reason != want {
			t.Fatalf("refusal = %+v, want today's prior-state refusal byte-unchanged", refusal)
		}
	})
}

// TestBeginSupersededFixupArm_OtherStageFenceIsBounded (approval condition C1,
// unit half): with stage T's admission mutex HELD by the test, begin's arm for
// T reached from inside stage S's critical section (a context marked for S)
// answers a TRANSIENT refusal within the bound, writing nothing. MECHANISM
// (CF): an unbounded LockStageAdmission in the arm deadlocks; the bounded
// wait below fails it.
func TestBeginSupersededFixupArm_OtherStageFenceIsBounded(t *testing.T) {
	sd := seedMCVRebase(t, upToDateRebaseStub())
	impl, tSeq, fSeq := seedQueuedD6Fixup(t, sd, run.StageStatePending)
	seedMarkerAndVoid(t, sd, impl, tSeq, fSeq)
	orch := &orchestrator.Orchestrator{Runs: sd.rr}
	sd.s.cfg.Orchestrator = orch
	release := orch.LockStageAdmission(impl.ID)
	defer release()
	sctx, unlockS := sd.s.holdStageAdmission(context.Background(), uuid.New())
	defer unlockS()
	before := chainLen(sd.au)

	type answer struct {
		refusal *mergeCandidateVerifyRefusal
	}
	done := make(chan answer, 1)
	go func() {
		_, refusal := startRerunHead(sctx, sd)
		done <- answer{refusal}
	}()
	select {
	case a := <-done:
		if a.refusal == nil || !a.refusal.Transient || !strings.Contains(a.refusal.Reason, "admission fence is held") {
			t.Fatalf("refusal = %+v, want a transient fence refusal", a.refusal)
		}
	case <-time.After(timescale.D(stageAdmissionTryWait + 10*time.Second)):
		t.Fatal("begin's arm did not return within its bound while another stage's fence was held (deadlock)")
	}
	if n := len(chainDelta(sd.au, before)); n != 0 {
		t.Errorf("rows appended = %d, want 0", n)
	}
}

// TestConfirmedFixupSupersessions (T8): the pure reader rule. MECHANISM (CF):
// dropping V.Sequence > M.Sequence confirms the "v_before_marker" row;
// dropping the field match confirms the "v_names_other_trigger" row.
func TestConfirmedFixupSupersessions(t *testing.T) {
	stage, other := uuid.New(), uuid.New()
	entry := func(seq int64, sid uuid.UUID, payload any) *audit.Entry {
		raw, _ := json.Marshal(payload)
		return &audit.Entry{Sequence: seq, StageID: &sid, Payload: raw}
	}
	marker := func(seq, trigger int64) *audit.Entry {
		return entry(seq, stage, stageFixupSupersededPayload{TriggerSequence: trigger, SupersededBy: fixupSupersededByVerifyRerun})
	}
	v := func(seq int64, sid uuid.UUID, supersedes int64) *audit.Entry {
		return entry(seq, sid, mergeCandidateVerifyTrigger{Branch: "b", SupersedesFixupTriggerSequence: supersedes})
	}
	cases := []struct {
		name     string
		markers  []*audit.Entry
		triggers []*audit.Entry
		want     bool
	}{
		{"confirmed", []*audit.Entry{marker(10, 5)}, []*audit.Entry{v(12, stage, 5)}, true},
		{"inert_marker", []*audit.Entry{marker(10, 5)}, nil, false},
		{"v_names_other_trigger", []*audit.Entry{marker(10, 5)}, []*audit.Entry{v(12, stage, 6)}, false},
		{"v_without_field", []*audit.Entry{marker(10, 5)}, []*audit.Entry{v(12, stage, 0)}, false},
		{"v_before_marker", []*audit.Entry{marker(10, 5)}, []*audit.Entry{v(9, stage, 5)}, false},
		{"v_other_stage", []*audit.Entry{marker(10, 5)}, []*audit.Entry{v(12, other, 5)}, false},
		{"undecodable_marker", []*audit.Entry{{Sequence: 10, StageID: &stage, Payload: []byte("{")}}, []*audit.Entry{v(12, stage, 5)}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := confirmedFixupSupersessions(c.markers, c.triggers)
			seq, ok := got[fixupSupersessionKey{StageID: stage, TriggerSequence: 5}]
			if ok != c.want || (ok && seq != 10) {
				t.Fatalf("confirmed = %v (seq %d), want %v", ok, seq, c.want)
			}
		})
	}
}

// rerunReReadRepo answers GetStage for one stage with a copy at a different
// state, so the re-read under the fence diverges from the caller's view (the
// TOCTOU the fence exists for) while ListStages keeps the original.
type rerunReReadRepo struct {
	*promptRunRepo
	stageID uuid.UUID
	state   run.StageState
}

func (r *rerunReReadRepo) GetStage(ctx context.Context, id uuid.UUID) (*run.Stage, error) {
	st, err := r.promptRunRepo.GetStage(ctx, id)
	if err != nil || id != r.stageID {
		return st, err
	}
	cp := *st
	cp.State = r.state
	return &cp, nil
}

// TestRerunQueuedFixup_ReReadUnderFence: the verb's caller saw the stage
// pending, but the re-read under the fence finds it running (a dispatch
// landed first). Both the verb's predicate (a) and begin's arm refuse with
// nothing written. MECHANISM (CF): dropping the state check in
// queuedMergeCandidateFixupFor (verb) or the arm's re-read state check lands
// a row. The verb's own switch read the stale pending, so the predicate's
// check is the only refusal in the path.
func TestRerunQueuedFixup_ReReadUnderFence(t *testing.T) {
	t.Run("verb", func(t *testing.T) {
		sd := seedMCVRebase(t, upToDateRebaseStub())
		impl, _, _ := seedQueuedD6Fixup(t, sd, run.StageStatePending)
		sd.s.cfg.RunRepo = &rerunReReadRepo{promptRunRepo: sd.rr, stageID: impl.ID, state: run.StageStateRunning}
		before := chainLen(sd.au)
		resp := postFixupRerun(t, sd)
		if delta := chainDelta(sd.au, before); len(delta) != 1 {
			t.Fatalf("audit-chain delta = %v, want exactly [branch_rebased]", deltaCategories(delta))
		}
		if !strings.Contains(resp.MergeCandidateVerifyRefusal, `state "running"`) {
			t.Errorf("refusal = %q, want the re-read state named", resp.MergeCandidateVerifyRefusal)
		}
	})
	t.Run("begin_arm", func(t *testing.T) {
		sd := seedMCVRebase(t, upToDateRebaseStub())
		impl, tSeq, fSeq := seedQueuedD6Fixup(t, sd, run.StageStatePending)
		seedMarkerAndVoid(t, sd, impl, tSeq, fSeq)
		sd.s.cfg.RunRepo = &rerunReReadRepo{promptRunRepo: sd.rr, stageID: impl.ID, state: run.StageStateRunning}
		before := chainLen(sd.au)
		_, refusal := startRerunHead(context.Background(), sd)
		if refusal == nil || !strings.Contains(refusal.Reason, "left its queued state") {
			t.Fatalf("refusal = %+v, want the arm's re-read refusal", refusal)
		}
		if n := len(chainDelta(sd.au, before)); n != 0 {
			t.Errorf("rows appended = %d, want 0", n)
		}
	})
}

// TestBeginSupersededFixupArm_RefusesAlreadyConverted: a conversion trigger
// already follows the marker (its pass was consumed by a not_executed verdict
// without restoring the gate), so the arm refuses naming the conversion.
// MECHANISM (CF): dropping the already-converted check falls to the
// disqualifying scan, whose INERT wording replaces it (RED on the text).
func TestBeginSupersededFixupArm_RefusesAlreadyConverted(t *testing.T) {
	sd := seedMCVRebase(t, upToDateRebaseStub())
	impl, tSeq, fSeq := seedQueuedD6Fixup(t, sd, run.StageStatePending)
	seedMarkerAndVoid(t, sd, impl, tSeq, fSeq)
	seedMCVRow(sd, &impl.ID, CategoryStageMergeCandidateVerifyTriggered, mergeCandidateVerifyTrigger{
		Branch: rebaseBranchName, BaseRef: rebaseBaseRef, ExpectedHeadSHA: rebaseNewHeadSHA, Cause: mergeCandidateCauseBaseAdvance,
		VerifyCommand: mcvCommand, PriorState: string(run.StageStateSucceeded), SupersedesFixupTriggerSequence: tSeq})
	vSeq := int64(chainLen(sd.au))
	seedMCVRow(sd, &impl.ID, CategoryMergeCandidateVerified, mergeCandidateVerifiedPayload{
		HeadSHA: rebaseNewHeadSHA, Cause: mergeCandidateCauseBaseAdvance, VerifyCommand: mcvCommand,
		Result: mergeCandidateResultNotExecuted, TriggerSequence: vSeq, OutputUntrusted: true})
	before := chainLen(sd.au)
	_, refusal := startRerunHead(context.Background(), sd)
	if refusal == nil || !strings.Contains(refusal.Reason, "already converted") {
		t.Fatalf("refusal = %+v, want the existing conversion named", refusal)
	}
	if n := len(chainDelta(sd.au, before)); n != 0 {
		t.Errorf("rows appended = %d, want 0", n)
	}
}
