package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Reap-path fix-up recovery (#4856): handleReapStageFailure routes a reaped
// fix-up re-dispatch through the #788 recovery (maybeRecoverReapedFixupFailure)
// after its dispatch_reaper_failed append and before Advance. These tests seed
// every fixture BY CONSTRUCTION through the fakes' real append and transition
// methods, and read COMMITTED STATE (stage rows, run row, audit chain) after the
// handler returns.

// reapFixupFixture is a running run with an implement stage in implState and a
// review stage parked pending, the shape a push_and_open_pr fix-up re-dispatch
// leaves behind. The audit fake stamps append-order sequences, which
// fixupTriggerActive and the live-trigger reads compare.
type reapFixupFixture struct {
	s      *Server
	rr     *fixupRecoveryRepo
	au     *auditFake
	runID  uuid.UUID
	impl   *run.Stage
	review *run.Stage
}

func newReapFixupFixture(t *testing.T, implState run.StageState) *reapFixupFixture {
	t.Helper()
	rr := &fixupRecoveryRepo{orchestratorRepo: newOrchestratorRepo()}
	au := newAuditFake()
	au.stampSequence = true
	runRow := rr.seedRun()
	impl := rr.seedStage(runRow.ID, 1, implState)
	review := rr.seedStage(runRow.ID, 2, run.StageStatePending)
	rr.mu.Lock()
	impl.Type = run.StageTypeImplement
	review.Type = run.StageTypeReview
	rr.mu.Unlock()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		AuditRepo:    au,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})
	return &reapFixupFixture{s: s, rr: rr, au: au, runID: runRow.ID, impl: impl, review: review}
}

// appendRow appends a stage-scoped chained row and returns the sequence the
// fake will stamp on it (its 1-based append index).
func (f *reapFixupFixture) appendRow(t *testing.T, category string, stageID uuid.UUID, payload any) int64 {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s: %v", category, err)
	}
	f.au.mu.Lock()
	seq := int64(len(f.au.appended) + 1)
	f.au.mu.Unlock()
	sid := stageID
	kind := audit.ActorSystem
	if _, err := f.au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: f.runID, StageID: &sid, Category: category, ActorKind: &kind, Payload: raw,
	}); err != nil {
		t.Fatalf("append %s: %v", category, err)
	}
	return seq
}

// seedActiveTrigger appends the stage_fixup_triggered row a push_and_open_pr
// fix-up pass writes: prior gate succeeded, the review stage it re-parked, and
// pass ordinal 1.
func (f *reapFixupFixture) seedActiveTrigger(t *testing.T) int64 {
	t.Helper()
	return f.appendRow(t, CategoryStageFixupTriggered, f.impl.ID, map[string]any{
		"stage_id":                 f.impl.ID.String(),
		"prior_state":              string(run.StageStateSucceeded),
		"reparked_review_stage_id": f.review.ID.String(),
		"pass_ordinal":             1,
	})
}

func (f *reapFixupFixture) stageState(t *testing.T, id uuid.UUID) run.StageState {
	t.Helper()
	st, err := f.rr.GetStage(context.Background(), id)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	return st.State
}

func (f *reapFixupFixture) runState(t *testing.T) run.State {
	t.Helper()
	rn, err := f.rr.GetRun(context.Background(), f.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	return rn.State
}

func reapFixupOperatorBody() reapFailureRequest {
	return reapFailureRequest{Category: "C", Reason: "fixup_base_mismatch", Detail: "tip is not the recorded head", ExitCode: 1}
}

// selfReportFixupBody is the runner's attempt-pinned self-report (#3598) for a
// pre-agent fixup_base_mismatch exit.
func selfReportFixupBody(t *testing.T, attempt string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"category": "C", "reason": "fixup_base_mismatch", "exit_code": 1,
		"expected_state": "running", "expected_attempt": attempt,
	})
	if err != nil {
		t.Fatalf("marshal self-report: %v", err)
	}
	return raw
}

func decodeReapResponse(t *testing.T, w *httptest.ResponseRecorder) reapFailureResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp reapFailureResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return resp
}

// assertReapRecovered asserts the restored gate: implement succeeded, review
// awaiting_approval, exactly one dispatch_reaper_failed and one
// stage_fixup_recovered (source category C) with the reaper row FIRST, the
// response naming the restored state, and the run still running because
// Advance was skipped.
func assertReapRecovered(t *testing.T, f *reapFixupFixture, w *httptest.ResponseRecorder) {
	t.Helper()
	resp := decodeReapResponse(t, w)
	if !resp.Transitioned {
		t.Error("transitioned = false, want true")
	}
	if resp.StageState != string(run.StageStateSucceeded) {
		t.Errorf("response stage_state = %q, want succeeded (the restored gate state)", resp.StageState)
	}
	if got := f.stageState(t, f.impl.ID); got != run.StageStateSucceeded {
		t.Errorf("implement state = %q, want succeeded (restored)", got)
	}
	if got := f.stageState(t, f.review.ID); got != run.StageStateAwaitingApproval {
		t.Errorf("review state = %q, want awaiting_approval (gate restored)", got)
	}
	if got := f.runState(t); got != run.StateRunning {
		t.Errorf("run state = %q, want running (Advance skipped on recovery)", got)
	}
	reaped := auditEntries(f.au, CategoryDispatchReaperFailed)
	recovered := auditEntries(f.au, CategoryStageFixupRecovered)
	if len(reaped) != 1 || len(recovered) != 1 {
		t.Fatalf("dispatch_reaper_failed = %d, stage_fixup_recovered = %d, want 1/1 (chain %v)",
			len(reaped), len(recovered), appendedCategories(f.au))
	}
	reaperAt, recoveredAt := -1, -1
	for i, c := range appendedCategories(f.au) {
		switch c {
		case CategoryDispatchReaperFailed:
			reaperAt = i
		case CategoryStageFixupRecovered:
			recoveredAt = i
		}
	}
	if reaperAt > recoveredAt {
		t.Errorf("chain %v: dispatch_reaper_failed must PRECEDE stage_fixup_recovered (the #1957 refund window)", appendedCategories(f.au))
	}
	p := decodePayload(t, recovered[0].Payload)
	if p["source_failure_category"] != string(run.FailureC) {
		t.Errorf("source_failure_category = %v, want C", p["source_failure_category"])
	}
	if p["restored_review_stage_id"] != f.review.ID.String() {
		t.Errorf("restored_review_stage_id = %v, want %s", p["restored_review_stage_id"], f.review.ID)
	}
}

// assertReapFailedAndAdvanced asserts today's unchanged path: implement failed,
// nothing restored, no stage_fixup_recovered row, and Advance ran (the run is
// failed).
func assertReapFailedAndAdvanced(t *testing.T, f *reapFixupFixture, w *httptest.ResponseRecorder) {
	t.Helper()
	resp := decodeReapResponse(t, w)
	if !resp.Transitioned || resp.StageState != string(run.StageStateFailed) {
		t.Errorf("response = %+v, want {transitioned:true stage_state:failed}", resp)
	}
	if got := f.stageState(t, f.impl.ID); got != run.StageStateFailed {
		t.Errorf("implement state = %q, want failed (not recovered)", got)
	}
	if got := f.stageState(t, f.review.ID); got != run.StageStatePending {
		t.Errorf("review state = %q, want pending (gate not restored)", got)
	}
	if got := len(auditEntries(f.au, CategoryDispatchReaperFailed)); got != 1 {
		t.Errorf("dispatch_reaper_failed entries = %d, want 1", got)
	}
	// No recovery row AFTER this reap's row (a fixture may seed an earlier one).
	reaped := false
	for _, c := range appendedCategories(f.au) {
		switch {
		case c == CategoryDispatchReaperFailed:
			reaped = true
		case reaped && c == CategoryStageFixupRecovered:
			t.Errorf("chain %v: stage_fixup_recovered written by the reap, want none", appendedCategories(f.au))
		}
	}
	if got := f.runState(t); got != run.StateFailed {
		t.Errorf("run state = %q, want failed (Advance called)", got)
	}
}

// Operator arm: a reap of an ACTIVE fix-up re-dispatch restores the implement
// stage and the review gate instead of failing the run. Without the call site
// the stage stays failed and review stays pending.
func TestReapStageFailure_FixupRedispatch_RestoresReviewGate(t *testing.T) {
	f := newReapFixupFixture(t, run.StageStateDispatched)
	f.seedActiveTrigger(t)

	w := postReapFailure(t, f.s, f.runID, f.impl.ID, reapFixupOperatorBody(), withReapOperator)
	assertReapRecovered(t, f, w)
}

// Run-bound arm (C2, #3598): the runner's own attempt-pinned self-report of a
// pre-agent fixup_base_mismatch exit takes the same recovery.
func TestReapStageFailure_SelfReportFixupRedispatch_RestoresReviewGate(t *testing.T) {
	f := newReapFixupFixture(t, run.StageStateRunning)
	f.seedActiveTrigger(t)
	attempt := liveAttempt(t, f.rr.orchestratorRepo, f.impl.ID)
	if attempt == "" {
		t.Fatal("PRECONDITION: seeded running implement stage carries no attempt stamp")
	}

	w := postReapFailureRaw(t, f.s, f.runID, f.impl.ID, selfReportFixupBody(t, attempt), withRunBoundToken(f.runID))
	assertReapRecovered(t, f, w)
	reaped := auditEntries(f.au, CategoryDispatchReaperFailed)
	if p := decodePayload(t, reaped[0].Payload); p["reported_by"] != "runner" {
		t.Errorf("reported_by = %v, want runner (the self-report arm ran)", p["reported_by"])
	}
}

// Cross-boundary inside the server: the REAL fix-up handler opens a pass, the
// REAL reap handler recovers its reaped re-dispatch, and a second REAL fix-up
// is admitted within the one-pass budget because the window was refunded once.
func TestReapStageFailure_RealFixupThenReap_RestoresGateAndRefundsPass(t *testing.T) {
	f := newReapFixupFixture(t, run.StageStateSucceeded)
	ctx := context.Background()
	if _, err := f.rr.TransitionStage(ctx, f.review.ID, run.StageStateAwaitingApproval, nil); err != nil {
		t.Fatalf("park review: %v", err)
	}
	seedConcernsReview(f.au, f.impl,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	if w := postFixup(t, f.s, f.impl.ID, fixupRequest{Concerns: []int{0}, Reason: "first pass"}); w.Code != http.StatusOK {
		t.Fatalf("first fixup status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	// The fix-up handler's Advance dispatches the re-opened implement stage.
	if got := f.stageState(t, f.impl.ID); got != run.StageStateDispatched {
		t.Fatalf("PRECONDITION: implement after fix-up = %q, want dispatched", got)
	}

	w := postReapFailure(t, f.s, f.runID, f.impl.ID, reapFixupOperatorBody(), withReapOperator)
	assertReapRecovered(t, f, w)

	w2 := postFixup(t, f.s, f.impl.ID, fixupRequest{Concerns: []int{0}, Reason: "second pass after the reaped base mismatch"})
	if w2.Code != http.StatusOK {
		t.Fatalf("second fixup status = %d, want 200 (the reaped pass was refunded):\n%s", w2.Code, w2.Body.String())
	}
	payload := lastFixupTriggeredPayload(t, f.au)
	if payload["refunded_passes"] != float64(1) {
		t.Errorf("refunded_passes = %v, want 1 (one refund per window)", payload["refunded_passes"])
	}
	if payload["forced"] != false {
		t.Errorf("forced = %v, want false (admitted within budget)", payload["forced"])
	}
}

// A live merge-candidate verify-only pass owns the stage: the reap must not
// restore from the OLDER fix-up trigger. Deleting the guard lets the recovery
// restore from that stale trigger.
func TestReapStageFailure_LiveMergeCandidatePass_NotFixupRecovered(t *testing.T) {
	f := newReapFixupFixture(t, run.StageStateDispatched)
	f.seedActiveTrigger(t)
	f.appendRow(t, CategoryStageMergeCandidateVerifyTriggered, f.impl.ID, mergeCandidateVerifyTrigger{
		Branch: "fishhawk/run", BaseRef: "main", ExpectedHeadSHA: "abc123",
		Cause: mergeCandidateCauseBaseAdvance, VerifyCommand: "scripts/test verify",
		PriorState: string(run.StageStateSucceeded), ReparkedReviewStageID: f.review.ID.String(),
	})

	w := postReapFailure(t, f.s, f.runID, f.impl.ID, reapFixupOperatorBody(), withReapOperator)
	assertReapFailedAndAdvanced(t, f, w)
}

// A live conflict-resolution pass owns the stage: same shape as the
// merge-candidate guard, with no consumption rows.
func TestReapStageFailure_LiveConflictResolutionPass_NotFixupRecovered(t *testing.T) {
	f := newReapFixupFixture(t, run.StageStateDispatched)
	f.seedActiveTrigger(t)
	f.appendRow(t, CategoryStageConflictResolutionTriggered, f.impl.ID, conflictResolutionTrigger{
		Branch: "fishhawk/run", BaseRef: "main", ExpectedHeadSHA: "abc123", Pass: 1,
		PriorState: string(run.StageStateSucceeded), ReparkedReviewStageID: f.review.ID.String(),
	})

	w := postReapFailure(t, f.s, f.runID, f.impl.ID, reapFixupOperatorBody(), withReapOperator)
	assertReapFailedAndAdvanced(t, f, w)
}

// A read error on any trigger state the gate depends on keeps today's failure
// path: a possibly stale fix-up trigger is never restored from. The
// fixup_no_changes row is read only by fixupTriggerActive on this path, so its
// row isolates C1's read-error arm.
func TestReapStageFailure_RecoveryTriggerReadError_KeepsFailurePath(t *testing.T) {
	cases := []struct {
		name     string
		category string
	}{
		{"merge-candidate trigger unreadable", CategoryStageMergeCandidateVerifyTriggered},
		{"conflict-resolution trigger unreadable", CategoryStageConflictResolutionTriggered},
		{"fix-up trigger state unreadable", CategoryFixupNoChanges},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReapFixupFixture(t, run.StageStateDispatched)
			f.seedActiveTrigger(t)
			f.au.listByCategoryErrCategory = tc.category

			w := postReapFailure(t, f.s, f.runID, f.impl.ID, reapFixupOperatorBody(), withReapOperator)
			assertReapFailedAndAdvanced(t, f, w)
		})
	}
}

// A malformed newest merge-candidate trigger is an error from
// liveMergeCandidateTrigger, so it never falls through to the fix-up restore.
func TestReapStageFailure_MalformedMergeCandidateTrigger_KeepsFailurePath(t *testing.T) {
	f := newReapFixupFixture(t, run.StageStateDispatched)
	f.seedActiveTrigger(t)
	f.appendRow(t, CategoryStageMergeCandidateVerifyTriggered, f.impl.ID, map[string]any{"branch": "half-populated"})

	w := postReapFailure(t, f.s, f.runID, f.impl.ID, reapFixupOperatorBody(), withReapOperator)
	assertReapFailedAndAdvanced(t, f, w)
}

// C1: a SPENT newest trigger is not restored from. Each row settles or
// supersedes the pass after its trigger; maybeRecoverFixupFailure alone would
// restore every one of them.
func TestReapStageFailure_SpentFixupTrigger_StaysFailed(t *testing.T) {
	cases := []struct {
		name  string
		spend func(t *testing.T, f *reapFixupFixture, triggerSeq int64)
	}{
		{"fixup_pushed", func(t *testing.T, f *reapFixupFixture, _ int64) {
			f.appendRow(t, CategoryFixupPushed, f.impl.ID, map[string]any{"head_sha": "abc123"})
		}},
		{"fixup_no_changes", func(t *testing.T, f *reapFixupFixture, _ int64) {
			f.appendRow(t, CategoryFixupNoChanges, f.impl.ID, map[string]any{"stage_id": f.impl.ID.String()})
		}},
		{"stage_fixup_recovered", func(t *testing.T, f *reapFixupFixture, _ int64) {
			f.appendRow(t, CategoryStageFixupRecovered, f.impl.ID, map[string]any{"restored_state": "succeeded"})
		}},
		{"confirmed supersession", func(t *testing.T, f *reapFixupFixture, triggerSeq int64) {
			f.appendRow(t, CategoryStageFixupSuperseded, f.impl.ID, stageFixupSupersededPayload{
				TriggerSequence: triggerSeq, SupersededBy: "merge_candidate_verify_rerun",
			})
			f.appendRow(t, CategoryStageMergeCandidateVerifyTriggered, f.impl.ID, mergeCandidateVerifyTrigger{
				Branch: "fishhawk/run", BaseRef: "main", ExpectedHeadSHA: "abc123",
				Cause: mergeCandidateCauseBaseAdvance, VerifyCommand: "scripts/test verify",
				SupersedesFixupTriggerSequence: triggerSeq,
			})
			// The conversion pass settled, so the merge-candidate guard passes
			// and only the supersession spends the fix-up trigger.
			f.appendRow(t, CategoryMergeCandidateVerified, f.impl.ID, map[string]any{"result": "passed"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReapFixupFixture(t, run.StageStateDispatched)
			seq := f.seedActiveTrigger(t)
			tc.spend(t, f, seq)

			w := postReapFailure(t, f.s, f.runID, f.impl.ID, reapFixupOperatorBody(), withReapOperator)
			assertReapFailedAndAdvanced(t, f, w)
		})
	}
}

// C1: the fix-up pass pushed (spending its trigger), the stage later failed and
// a retry_stage re-dispatch of it was reaped. That dispatch is not the fix-up
// pass, so nothing is restored.
func TestReapStageFailure_SpentTriggerThenRetryRedispatch_StaysFailed(t *testing.T) {
	f := newReapFixupFixture(t, run.StageStateRunning)
	ctx := context.Background()
	f.seedActiveTrigger(t)
	f.appendRow(t, CategoryFixupPushed, f.impl.ID, map[string]any{"head_sha": "abc123"})
	cat, reason := run.FailureC, "pull request open failed"
	if _, err := f.rr.TransitionStageFrom(ctx, f.impl.ID, run.StageStateRunning, run.StageStateFailed,
		&run.StageCompletion{FailureCategory: &cat, FailureReason: &reason}); err != nil {
		t.Fatalf("fail the pass: %v", err)
	}
	if _, err := f.rr.RetryStage(ctx, f.impl.ID, run.StageStatePending); err != nil {
		t.Fatalf("retry_stage: %v", err)
	}
	if _, err := f.rr.TransitionStageFrom(ctx, f.impl.ID, run.StageStatePending, run.StageStateDispatched, nil); err != nil {
		t.Fatalf("re-dispatch: %v", err)
	}

	w := postReapFailure(t, f.s, f.runID, f.impl.ID, reapFixupOperatorBody(), withReapOperator)
	assertReapFailedAndAdvanced(t, f, w)
}

// An active trigger whose restore cannot land (an unparseable
// reparked_review_stage_id) falls through to fail-and-Advance.
func TestReapStageFailure_FixupRestoreFails_FallsThroughToAdvance(t *testing.T) {
	f := newReapFixupFixture(t, run.StageStateDispatched)
	f.appendRow(t, CategoryStageFixupTriggered, f.impl.ID, map[string]any{
		"stage_id":                 f.impl.ID.String(),
		"prior_state":              string(run.StageStateSucceeded),
		"reparked_review_stage_id": "not-a-uuid",
		"pass_ordinal":             1,
	})

	w := postReapFailure(t, f.s, f.runID, f.impl.ID, reapFixupOperatorBody(), withReapOperator)
	assertReapFailedAndAdvanced(t, f, w)
}

// An ordinary first-dispatch reap (no fix-up trigger) is unchanged.
func TestReapStageFailure_NoFixupTrigger_FailsAndAdvances(t *testing.T) {
	f := newReapFixupFixture(t, run.StageStateDispatched)

	w := postReapFailure(t, f.s, f.runID, f.impl.ID, reapFixupOperatorBody(), withReapOperator)
	assertReapFailedAndAdvanced(t, f, w)
}

// rereadFailRepo fails every GetStage once the implement stage has been
// restored, so the handler's post-recovery re-read errors.
type rereadFailRepo struct {
	*fixupRecoveryRepo
	implID   uuid.UUID
	restored atomic.Bool
}

func (r *rereadFailRepo) TransitionStage(ctx context.Context, id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	st, err := r.fixupRecoveryRepo.TransitionStage(ctx, id, to, c)
	if err == nil && id == r.implID && to == run.StageStateSucceeded {
		r.restored.Store(true)
	}
	return st, err
}

func (r *rereadFailRepo) GetStage(ctx context.Context, id uuid.UUID) (*run.Stage, error) {
	if r.restored.Load() {
		return nil, errors.New("stage store down")
	}
	return r.fixupRecoveryRepo.GetStage(ctx, id)
}

// A recovered reap whose stage re-read fails still answers 200
// {transitioned:true} and reports failed, and still skips Advance: the restore
// already committed.
func TestReapStageFailure_RecoveredStageRereadFails_ReportsFailed(t *testing.T) {
	f := newReapFixupFixture(t, run.StageStateDispatched)
	f.seedActiveTrigger(t)
	repo := &rereadFailRepo{fixupRecoveryRepo: f.rr, implID: f.impl.ID}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      repo,
		AuditRepo:    f.au,
		Orchestrator: &orchestrator.Orchestrator{Runs: repo},
	})

	w := postReapFailure(t, s, f.runID, f.impl.ID, reapFixupOperatorBody(), withReapOperator)
	resp := decodeReapResponse(t, w)
	if !resp.Transitioned || resp.StageState != string(run.StageStateFailed) {
		t.Errorf("response = %+v, want {transitioned:true stage_state:failed} on a failed re-read", resp)
	}
	if got := f.stageState(t, f.impl.ID); got != run.StageStateSucceeded {
		t.Errorf("implement state = %q, want succeeded (the restore committed)", got)
	}
	if got := len(auditEntries(f.au, CategoryStageFixupRecovered)); got != 1 {
		t.Errorf("stage_fixup_recovered entries = %d, want 1", got)
	}
	if got := f.runState(t); got != run.StateRunning {
		t.Errorf("run state = %q, want running (Advance skipped on recovery)", got)
	}
}

// C8: a run-bound mcp:run:* token is still refused on the operator reap route
// after this change. Another run's token is 403 run_not_entitled; the run's own
// token without the attempt pins is 400 expected_state_required. Both refusals
// precede the recovery, so an ACTIVE fix-up trigger restores nothing.
func TestReapStageFailure_RunBoundTokenRefusedOnOperatorRoute(t *testing.T) {
	cases := []struct {
		name     string
		token    func(f *reapFixupFixture) func(*http.Request) *http.Request
		wantCode int
		wantErr  string
	}{
		{"another run's token", func(*reapFixupFixture) func(*http.Request) *http.Request {
			return withRunBoundToken(uuid.New())
		}, http.StatusForbidden, "run_not_entitled"},
		{"own run's token, unpinned operator body", func(f *reapFixupFixture) func(*http.Request) *http.Request {
			return withRunBoundToken(f.runID)
		}, http.StatusBadRequest, "expected_state_required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReapFixupFixture(t, run.StageStateDispatched)
			f.seedActiveTrigger(t)

			w := postReapFailure(t, f.s, f.runID, f.impl.ID, reapFixupOperatorBody(), tc.token(f))
			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d:\n%s", w.Code, tc.wantCode, w.Body.String())
			}
			if code, _ := decodeReapError(t, w); code != tc.wantErr {
				t.Errorf("error code = %q, want %q", code, tc.wantErr)
			}
			if got := f.stageState(t, f.impl.ID); got != run.StageStateDispatched {
				t.Errorf("implement state = %q, want dispatched (nothing reaped)", got)
			}
			if got := f.stageState(t, f.review.ID); got != run.StageStatePending {
				t.Errorf("review state = %q, want pending (nothing restored)", got)
			}
			if n := len(auditEntries(f.au, CategoryDispatchReaperFailed)) + len(auditEntries(f.au, CategoryStageFixupRecovered)); n != 0 {
				t.Errorf("reap/recovery rows = %d, want 0 (chain %v)", n, appendedCategories(f.au))
			}
		})
	}
}
