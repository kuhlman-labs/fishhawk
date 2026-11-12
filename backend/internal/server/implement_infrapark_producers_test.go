package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/dispatchwatchdog"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// #4816 slice 3: producer-level pins for a category-C failure of a LIVE
// merge-candidate verify-only implement pass. Only an OPERATOR runner_lost
// reap stamped with the stage's current failure episode parks the run
// (implement_infrapark_test.go pins that arm); every producer below keeps
// today's fail-the-run path. Each test runs on the M1 fakes
// (implParkEpisodeRepo, implParkSeqAudit) and asserts its read-back sequences
// as a precondition, so a park refusal is the classifier's, not a fake's.

// newImplParkProducerWorld is newImplParkReplayWorld's shape with the
// merge-candidate trigger optional: withTrigger=false leaves the implement
// stage re-opened for no verify-only pass (the fix-up / conflict-resolution
// rows of the non-replayable table seed their own pass row).
func newImplParkProducerWorld(t *testing.T, withTrigger bool) *implParkReplayWorld {
	t.Helper()
	if withTrigger {
		return newImplParkReplayWorld(t)
	}
	base := &fixupRecoveryRepo{orchestratorRepo: newOrchestratorRepo()}
	rr := &implParkEpisodeRepo{fixupRecoveryRepo: base}
	au := newImplParkSeqAudit()
	cr := newFakeConcernRepo()
	sf := newSigningFake()
	s := New(Config{
		Addr: "127.0.0.1:0", SigningRepo: sf, ArtifactRepo: newFakeArtifactRepo(),
		AuditRepo: au, RunRepo: rr, ConcernRepo: cr,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr, Audit: au},
	})
	runRow := base.seedRun()
	plan := base.seedStage(runRow.ID, 1, run.StageStateSucceeded)
	impl := base.seedStage(runRow.ID, 2, run.StageStateDispatched)
	review := base.seedStage(runRow.ID, 3, run.StageStatePending)
	base.mu.Lock()
	plan.Type = run.StageTypePlan
	impl.Type = run.StageTypeImplement
	impl.SelfRetryCount = 1
	review.Type = run.StageTypeReview
	base.mu.Unlock()
	implParkAppend(t, au, runRow.ID, impl.ID, "pull_request_opened", map[string]any{"stage_id": impl.ID.String(), "pr_number": 42})
	priv, _ := sf.issue(t, runRow.ID)
	return &implParkReplayWorld{s: s, rr: rr, au: au, cr: cr, priv: priv, runID: runRow.ID, impl: impl, review: review}
}

// implParkAppend appends a stage-scoped row in chain order, so it reads back
// through ListForRun with the next sequence.
func implParkAppend(t *testing.T, au *implParkSeqAudit, runID, stageID uuid.UUID, category string, payload any) {
	t.Helper()
	raw, _ := json.Marshal(payload)
	kind := audit.ActorSystem
	if _, err := au.sequencedAuditFake.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, StageID: &stageID, Category: category, ActorKind: &kind, Payload: raw,
	}); err != nil {
		t.Fatalf("seed %s: %v", category, err)
	}
}

// setStageRunning moves the implement stage to running with a dispatch stamp,
// the state a runner's self-report, a signed PR report and a trace upload
// find it in.
func (w *implParkReplayWorld) setStageRunning(t *testing.T) {
	t.Helper()
	cur, _ := w.rr.GetStage(t.Context(), w.impl.ID)
	if cur.State == run.StageStatePending {
		if _, err := w.rr.TransitionStageFrom(t.Context(), w.impl.ID, run.StageStatePending, run.StageStateDispatched, nil); err != nil {
			t.Fatalf("dispatch the implement stage: %v", err)
		}
	}
	if cur, _ = w.rr.GetStage(t.Context(), w.impl.ID); cur.State == run.StageStateDispatched {
		if _, err := w.rr.TransitionStageFrom(t.Context(), w.impl.ID, run.StageStateDispatched, run.StageStateRunning, nil); err != nil {
			t.Fatalf("start the implement stage: %v", err)
		}
	}
	w.rr.mu.Lock()
	if w.rr.stagesByID[w.impl.ID].DispatchedAt == nil {
		now := time.Now().UTC().Truncate(time.Microsecond)
		w.rr.stagesByID[w.impl.ID].DispatchedAt = &now
	}
	w.rr.mu.Unlock()
}

// operatorReap posts an operator reap of the implement stage and returns the
// recorder; the caller asserts the code.
func (w *implParkReplayWorld) operatorReap(t *testing.T, category string) *httptest.ResponseRecorder {
	t.Helper()
	return postReapFailure(t, w.s, w.runID, w.impl.ID,
		reapFailureRequest{Category: category, Reason: run.InfraParkRunnerLostReason}, withReapOperator)
}

// shipFailedC sends the signed failed/C pull-request report for the running
// implement stage.
func (w *implParkReplayWorld) shipFailedC(t *testing.T) {
	t.Helper()
	w.setStageRunning(t)
	body := []byte(`{"outcome":"failed","category":"C","reason":"open PR: 503 from the forge"}`)
	if rec := shipPRRequest(t, w.s, w.runID, w.impl.ID, w.priv, body, ""); rec.Code != http.StatusOK {
		t.Fatalf("signed failed/C report = %d: %s", rec.Code, rec.Body.String())
	}
}

// requireFailedNoPark is the committed state of every FAIL producer: implement
// failed/C, the run failed, and no stage_infra_parked row for the stage.
func (w *implParkReplayWorld) requireFailedNoPark(t *testing.T) {
	t.Helper()
	implParkRequireStageFailedC(t, w.rr, w.impl.ID)
	implParkRequireRunState(t, w.rr.fixupRecoveryRepo, w.runID, run.StateFailed)
	if n := len(implParkRows(w.au.auditFake, w.impl.ID)); n != 0 {
		t.Errorf("stage_infra_parked rows = %d, want 0 (this producer never parks)", n)
	}
}

func (w *implParkReplayWorld) entries(t *testing.T) []*audit.Entry {
	t.Helper()
	entries, err := w.au.ListForRun(t.Context(), w.runID)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func (w *implParkReplayWorld) requireTriggerLive(t *testing.T, want bool) {
	t.Helper()
	live := w.s.resolveMergeCandidateVerifyTrigger(t.Context(), w.runID, w.impl.ID)
	if want && (live == nil || live.ExpectedHeadSHA != mcvHead) {
		t.Fatalf("trigger = %+v, want still live at %s", live, mcvHead)
	}
	if !want && live != nil {
		t.Fatalf("trigger = %+v, want consumed", live)
	}
}

// reapPayloads decodes every dispatch_reaper_failed row, oldest first.
func (w *implParkReplayWorld) reapPayloads(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, row := range auditEntries(w.au.auditFake, CategoryDispatchReaperFailed) {
		var p map[string]any
		if err := json.Unmarshal(row.Payload, &p); err != nil {
			t.Fatalf("decode reap row: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// TestImplementInfraFailure_RunBoundReapOfLiveVerifyPassFailsRun is N1: the
// runner's own attempt-pinned self-report (#3598) of a live verify-only pass,
// even with reason runner_lost, carries reported_by and no episode stamp, so
// the run fails as before #4816 and the trigger row is untouched.
func TestImplementInfraFailure_RunBoundReapOfLiveVerifyPassFailsRun(t *testing.T) {
	w := newImplParkReplayWorld(t)
	w.setStageRunning(t)
	triggersBefore := auditEntries(w.au.auditFake, CategoryStageMergeCandidateVerifyTriggered)
	body, _ := json.Marshal(map[string]any{
		"category": "C", "reason": run.InfraParkRunnerLostReason, "exit_code": 1,
		"expected_state": "running", "expected_attempt": liveAttempt(t, w.rr.orchestratorRepo, w.impl.ID),
	})
	rec := postReapFailureRaw(t, w.s, w.runID, w.impl.ID, body, withRunBoundToken(w.runID))
	if rec.Code != http.StatusOK {
		t.Fatalf("run-bound self-report = %d: %s", rec.Code, rec.Body.String())
	}
	var resp reapFailureResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || !resp.Transitioned {
		t.Fatalf("response = %s (err %v), want transitioned", rec.Body.String(), err)
	}
	implParkRequireDistinctSequences(t, w.entries(t), w.impl.ID,
		"pull_request_opened", CategoryStageMergeCandidateVerifyTriggered, CategoryDispatchReaperFailed)
	reaps := w.reapPayloads(t)
	if len(reaps) != 1 || reaps[0]["reported_by"] != "runner" {
		t.Fatalf("reap rows = %v, want one carrying reported_by runner", reaps)
	}
	if _, ok := reaps[0]["failed_at"]; ok {
		t.Errorf("run-bound reap row carries failed_at %v; the self-report arm is never stamped", reaps[0]["failed_at"])
	}
	w.requireFailedNoPark(t)
	if after := auditEntries(w.au.auditFake, CategoryStageMergeCandidateVerifyTriggered); !reflectEqualParams(triggersBefore, after) {
		t.Errorf("trigger rows changed: before %d, after %d", len(triggersBefore), len(after))
	}
}

func reflectEqualParams(a, b []audit.ChainAppendParams) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Category != b[i].Category || string(a[i].Payload) != string(b[i].Payload) {
			return false
		}
	}
	return true
}

// implParkLivenessLister answers the dispatch watchdog's liveness read with
// fixed rows.
type implParkLivenessLister struct{ rows []run.DispatchedStageLiveness }

func (l implParkLivenessLister) ListDispatchedStageLiveness(context.Context) ([]run.DispatchedStageLiveness, error) {
	return l.rows, nil
}

// TestImplementInfraFailure_WatchdogElapsedLiveVerifyPassFailsRun is N1: a
// REAL dispatchwatchdog.Ticker times out a dispatched live verify-only pass.
// The classifier never reads dispatch_watchdog_elapsed, so the run fails. This
// pins an ABSENCE (no watchdog stamp, no watchdog read); its classifier-side
// twin is the run-table row whose watchdog row carries a current-episode stamp.
func TestImplementInfraFailure_WatchdogElapsedLiveVerifyPassFailsRun(t *testing.T) {
	w := newImplParkReplayWorld(t)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	dispatched := now.Add(-2 * time.Hour)
	ticker := &dispatchwatchdog.Ticker{
		Repo: w.rr,
		Liveness: implParkLivenessLister{rows: []run.DispatchedStageLiveness{{
			StageID: w.impl.ID, RunID: w.runID, DispatchedAt: &dispatched, UpdatedAt: dispatched,
		}}},
		Audit: w.au,
		Advance: func(ctx context.Context, runID uuid.UUID) error {
			_, err := w.s.cfg.Orchestrator.Advance(ctx, runID)
			return err
		},
		Timeout: time.Hour,
		Now:     func() time.Time { return now },
	}
	ticker.Tick(t.Context())

	if n := len(auditEntries(w.au.auditFake, dispatchwatchdog.CategoryDispatchWatchdogElapsed)); n != 1 {
		t.Fatalf("dispatch_watchdog_elapsed rows = %d, want 1", n)
	}
	implParkRequireDistinctSequences(t, w.entries(t), w.impl.ID,
		"pull_request_opened", CategoryStageMergeCandidateVerifyTriggered, dispatchwatchdog.CategoryDispatchWatchdogElapsed)
	if st, _ := w.rr.GetStage(t.Context(), w.impl.ID); st.EndedAt == nil {
		t.Fatal("PRECONDITION: the failed implement stage carries no ended_at")
	}
	if n := len(auditEntries(w.au.auditFake, CategoryDispatchReaperFailed)); n != 0 {
		t.Fatalf("PRECONDITION: dispatch_reaper_failed rows = %d, want 0", n)
	}
	w.requireFailedNoPark(t)
}

// TestImplementInfraFailure_MergeCandidateRecoveryFallbackStillFailsRun pins
// failPullRequestStage's two merge-candidate fallbacks on a live pass: the
// not_executed record fails (trigger still live), or the restore fails
// (trigger consumed). Neither writes an operator reap row, so both fail.
func TestImplementInfraFailure_MergeCandidateRecoveryFallbackStillFailsRun(t *testing.T) {
	t.Run("record-failed: trigger stays live", func(t *testing.T) {
		w := newImplParkReplayWorld(t)
		w.au.setFailCategories(CategoryMergeCandidateVerified)
		w.shipFailedC(t)
		w.au.setFailCategories()
		implParkRequireDistinctSequences(t, w.entries(t), w.impl.ID, "pull_request_opened", CategoryStageMergeCandidateVerifyTriggered)
		if n := len(auditEntries(w.au.auditFake, CategoryMergeCandidateVerified)); n != 0 {
			t.Fatalf("PRECONDITION: merge_candidate_verified rows = %d, want 0 (the record was refused)", n)
		}
		w.requireTriggerLive(t, true)
		w.requireFailedNoPark(t)
	})
	t.Run("restore-failed: trigger consumed", func(t *testing.T) {
		w := newImplParkReplayWorld(t)
		// A newer trigger whose re-parked review id names no stage: the
		// not_executed row lands (consuming it) and the restore fails.
		if err := w.s.appendMergeCandidateTrigger(t.Context(), w.runID, w.impl.ID, mergeCandidateVerifyTrigger{
			Branch: mcvBranch, BaseRef: mcvBaseRef, ExpectedHeadSHA: mcvHead, Cause: mergeCandidateCauseBaseAdvance,
			VerifyCommand: mcvCommand, PriorState: string(run.StageStateSucceeded), ReparkedReviewStageID: uuid.NewString(),
		}, mcvOperator()); err != nil {
			t.Fatalf("append trigger: %v", err)
		}
		w.shipFailedC(t)
		triggerSeq, verdictSeq := implParkRequireDistinctSequences(t, w.entries(t), w.impl.ID,
			CategoryStageMergeCandidateVerifyTriggered, CategoryMergeCandidateVerified)
		if verdictSeq <= triggerSeq {
			t.Fatalf("PRECONDITION: verdict seq %d <= trigger seq %d", verdictSeq, triggerSeq)
		}
		w.requireTriggerLive(t, false)
		w.requireFailedNoPark(t)
	})
}

// TestImplementInfraFailure_TraceCategoryCOnLivePassStillFailsRun pins the
// trace handler's failStageCategoryC (L1) on a running live verify-only pass:
// advanceAfterFailure runs no merge-candidate recovery and writes no reap row,
// so the run fails with the trigger still live.
func TestImplementInfraFailure_TraceCategoryCOnLivePassStillFailsRun(t *testing.T) {
	w := newImplParkReplayWorld(t)
	w.setStageRunning(t)
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+w.runID.String()+"/stages/"+w.impl.ID.String()+"/trace", nil)
	w.s.failStageCategoryC(req, w.runID, w.impl.ID, "implement produced an empty diff", nil)
	implParkRequireDistinctSequences(t, w.entries(t), w.impl.ID, "pull_request_opened", CategoryStageMergeCandidateVerifyTriggered)
	if st, _ := w.rr.GetStage(t.Context(), w.impl.ID); st.EndedAt == nil {
		t.Fatal("PRECONDITION: the failed implement stage carries no ended_at")
	}
	w.requireTriggerLive(t, true)
	w.requireFailedNoPark(t)
}

// TestImplementInfraFailure_SupersedeRestoreFailureStillFailsRun pins the
// merge_candidate_supersede.go restore-failed arm (L1): a stale pending pass is
// superseded (trigger consumed) but its restore is refused, so the stage is
// failed/C and supersedeStalePassOnQueueAnswer's Advance fails the run.
func TestImplementInfraFailure_SupersedeRestoreFailureStillFailsRun(t *testing.T) {
	f := newSupFixture(t, run.StageStateAwaitingHostDispatch)
	seq := &implParkListSeqAudit{auditFake: f.au}
	f.s.cfg.Orchestrator = &orchestrator.Orchestrator{Runs: f.repo, Audit: seq}
	f.headRow("fixup_pushed", "head_sha", supHeadNew)
	f.repo.refuseTo = run.StageStateAwaitingApproval
	err := f.s.supersedeStalePassOnQueueAnswer(t.Context(), f.runID, supHeadNew, mergeCandidateStateHeld)
	f.repo.refuseTo = ""
	if err != nil {
		t.Fatalf("supersedeStalePassOnQueueAnswer: %v", err)
	}
	entries, lerr := seq.ListForRun(t.Context(), f.runID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	triggerSeq, verdictSeq := implParkRequireDistinctSequences(t, entries, f.impl.ID,
		CategoryStageMergeCandidateVerifyTriggered, CategoryMergeCandidateVerified)
	if verdictSeq <= triggerSeq {
		t.Fatalf("PRECONDITION: verdict seq %d <= trigger seq %d", verdictSeq, triggerSeq)
	}
	implParkRequireStageFailedC(t, f.repo, f.impl.ID)
	if got := f.runRow(t).State; got != run.StateFailed {
		t.Errorf("run state = %q, want failed", got)
	}
	if n := len(implParkRows(f.au, f.impl.ID)); n != 0 {
		t.Errorf("stage_infra_parked rows = %d, want 0", n)
	}
}

// TestImplementInfraFailure_ReapedNonReplayablePassStillFailsRun: an operator
// runner_lost reap whose pass is NOT a live, newest merge-candidate verify-only
// pass fails the run, because a retry would not replay it.
func TestImplementInfraFailure_ReapedNonReplayablePassStillFailsRun(t *testing.T) {
	for _, tc := range []struct {
		name        string
		withTrigger bool
		seed        func(t *testing.T, w *implParkReplayWorld)
		order       []string
	}{
		{"fix-up pass, no merge-candidate trigger", false, func(t *testing.T, w *implParkReplayWorld) {
			implParkAppend(t, w.au, w.runID, w.impl.ID, CategoryStageFixupTriggered, map[string]any{"prior_state": "succeeded"})
		}, []string{"pull_request_opened", CategoryStageFixupTriggered, CategoryDispatchReaperFailed}},
		{"conflict-resolution pass, no merge-candidate trigger", false, func(t *testing.T, w *implParkReplayWorld) {
			implParkAppend(t, w.au, w.runID, w.impl.ID, CategoryStageConflictResolutionTriggered, map[string]any{"prior_state": "succeeded"})
		}, []string{"pull_request_opened", CategoryStageConflictResolutionTriggered, CategoryDispatchReaperFailed}},
		{"merge-candidate trigger consumed before the reap", true, func(t *testing.T, w *implParkReplayWorld) {
			implParkAppend(t, w.au, w.runID, w.impl.ID, CategoryMergeCandidateVerified, map[string]any{"result": mergeCandidateResultPassed})
		}, []string{CategoryStageMergeCandidateVerifyTriggered, CategoryMergeCandidateVerified, CategoryDispatchReaperFailed}},
		{"merge-candidate trigger older than a fix-up trigger", true, func(t *testing.T, w *implParkReplayWorld) {
			implParkAppend(t, w.au, w.runID, w.impl.ID, CategoryStageFixupTriggered, map[string]any{"prior_state": "succeeded"})
		}, []string{CategoryStageMergeCandidateVerifyTriggered, CategoryStageFixupTriggered, CategoryDispatchReaperFailed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newImplParkProducerWorld(t, tc.withTrigger)
			tc.seed(t, w)
			if rec := w.operatorReap(t, "C"); rec.Code != http.StatusOK {
				t.Fatalf("operator reap = %d: %s", rec.Code, rec.Body.String())
			}
			implParkRequireDistinctSequences(t, w.entries(t), w.impl.ID, tc.order...)
			failedStage, _ := w.rr.GetStage(t.Context(), w.impl.ID)
			at, _ := run.InfraParkEpisode(failedStage)
			if reaps := w.reapPayloads(t); len(reaps) != 1 || reaps[0]["failed_at"] != at || at == "" {
				t.Fatalf("PRECONDITION: reap rows %v, want one stamped with the current episode %q", reaps, at)
			}
			w.requireFailedNoPark(t)
		})
	}
}

// TestImplementInfraFailure_BReapThenOverrideRetryThenDeliberateFailureFailsRun
// is H2 case (a): an operator runner_lost reap with category B is stamped but
// fails the run; an override retry re-opens the stage (a new episode); a later
// category-C failure whose ONLY operator reap row is that earlier-episode stamp
// fails the run. Only the episode equality refuses the stale row (CF-C2).
func TestImplementInfraFailure_BReapThenOverrideRetryThenDeliberateFailureFailsRun(t *testing.T) {
	w := newImplParkReplayWorld(t)
	if rec := w.operatorReap(t, "B"); rec.Code != http.StatusOK {
		t.Fatalf("operator B reap = %d: %s", rec.Code, rec.Body.String())
	}
	w.requireLiveReapPreconditions(t)
	implParkRequireRunState(t, w.rr.fixupRecoveryRepo, w.runID, run.StateFailed)
	firstStage, _ := w.rr.GetStage(t.Context(), w.impl.ID)
	firstAt, firstOrdinal := run.InfraParkEpisode(firstStage)
	if reaps := w.reapPayloads(t); len(reaps) != 1 || reaps[0]["failed_at"] != firstAt || firstAt == "" {
		t.Fatalf("PRECONDITION: reap rows %v, want one stamped %q", reaps, firstAt)
	}

	id := Identity{Subject: "operator@example.test", TokenID: "tok-4816", Scopes: []string{"write:stages"}}
	if _, err := w.s.retryStageAs(t.Context(), id, retryActionParams{
		StageID: w.impl.ID, Override: true, OverrideReason: "the runner host was rebooted mid-pass",
	}); err != nil {
		t.Fatalf("override retry: %v", err)
	}
	retried, _ := w.rr.GetStage(t.Context(), w.impl.ID)
	// The retry re-opens the stage (failed -> pending) and the orchestrator
	// handoff may dispatch it at once; either way it left failed.
	if retried.State == run.StageStateFailed || retried.SelfRetryCount != firstOrdinal+1 || retried.EndedAt != nil {
		t.Fatalf("after override retry: %s ordinal %d ended_at %v, want re-opened, ordinal %d, ended_at cleared",
			retried.State, retried.SelfRetryCount, retried.EndedAt, firstOrdinal+1)
	}
	implParkRequireRunState(t, w.rr.fixupRecoveryRepo, w.runID, run.StateRunning)

	w.au.setFailCategories(CategoryMergeCandidateVerified)
	w.shipFailedC(t)
	w.au.setFailCategories()
	w.requireTriggerLive(t, true)
	second, _ := w.rr.GetStage(t.Context(), w.impl.ID)
	if at, ord := run.InfraParkEpisode(second); at == firstAt || ord == firstOrdinal {
		t.Fatalf("PRECONDITION: second episode %q/%d equals the first %q/%d", at, ord, firstAt, firstOrdinal)
	}
	if n := len(w.reapPayloads(t)); n != 1 {
		t.Fatalf("PRECONDITION: reap rows = %d, want only the earlier-episode row", n)
	}
	w.requireFailedNoPark(t)
}

// TestImplementInfraFailure_LostParkRowThenDeliberateFailureFailsRun is H2
// case (b): an operator runner_lost reap parks the run but its park row is
// lost; a retry opens a new episode; a later category-C failure (record-failed,
// trigger live) whose only reap row is the earlier episode's fails the run.
func TestImplementInfraFailure_LostParkRowThenDeliberateFailureFailsRun(t *testing.T) {
	w := newImplParkReplayWorld(t)
	w.au.setFailCategories(run.CategoryStageInfraParked)
	if rec := w.operatorReap(t, "C"); rec.Code != http.StatusOK {
		t.Fatalf("operator reap = %d: %s", rec.Code, rec.Body.String())
	}
	w.au.setFailCategories()
	w.requireLiveReapPreconditions(t)
	implParkRequireRunState(t, w.rr.fixupRecoveryRepo, w.runID, run.StateRunning)
	if n := len(implParkRows(w.au.auditFake, w.impl.ID)); n != 0 {
		t.Fatalf("PRECONDITION: stage_infra_parked rows = %d, want 0 (the park row was lost)", n)
	}
	firstStage, _ := w.rr.GetStage(t.Context(), w.impl.ID)
	firstAt, firstOrdinal := run.InfraParkEpisode(firstStage)

	if rec := postRetry(t, w.s, w.impl.ID); rec.Code != http.StatusOK {
		t.Fatalf("retry = %d: %s", rec.Code, rec.Body.String())
	}
	if st, _ := w.rr.GetStage(t.Context(), w.impl.ID); st.State == run.StageStateFailed || st.SelfRetryCount != firstOrdinal+1 {
		t.Fatalf("after retry: %s ordinal %d, want re-opened at %d", st.State, st.SelfRetryCount, firstOrdinal+1)
	}

	w.au.setFailCategories(CategoryMergeCandidateVerified)
	w.shipFailedC(t)
	w.au.setFailCategories()
	w.requireTriggerLive(t, true)
	second, _ := w.rr.GetStage(t.Context(), w.impl.ID)
	if at, ord := run.InfraParkEpisode(second); at == firstAt || ord == firstOrdinal {
		t.Fatalf("PRECONDITION: second episode %q/%d equals the first %q/%d", at, ord, firstAt, firstOrdinal)
	}
	if n := len(w.reapPayloads(t)); n != 1 {
		t.Fatalf("PRECONDITION: reap rows = %d, want only the earlier-episode row", n)
	}
	w.requireFailedNoPark(t)
}

// TestImplementInfraFailure_OperatorReapRespectsParkBudget is L4 under N1: the
// operator-reaped arm shares the stage's lifetime park budget. Three earlier
// episode parks exhaust it, so a fourth (an operator runner_lost reap of a live
// pass) fails the run with one budget-exhausted row and no new park row.
func TestImplementInfraFailure_OperatorReapRespectsParkBudget(t *testing.T) {
	w := newImplParkReplayWorld(t)
	for i := 0; i < run.InfraFailureParkRetryBudget; i++ {
		implParkAppend(t, w.au, w.runID, w.impl.ID, run.CategoryStageInfraParked, map[string]any{
			"stage_type": "implement", "failed_at": time.Date(2026, 1, i+1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano),
			"retry_ordinal": i, "park_ordinal": i + 1,
		})
	}
	if rec := w.operatorReap(t, "C"); rec.Code != http.StatusOK {
		t.Fatalf("operator reap = %d: %s", rec.Code, rec.Body.String())
	}
	w.requireLiveReapPreconditions(t)
	if n := len(implParkRows(w.au.auditFake, w.impl.ID)); n != run.InfraFailureParkRetryBudget {
		t.Errorf("stage_infra_parked rows = %d, want only the %d seeded", n, run.InfraFailureParkRetryBudget)
	}
	exhausted := 0
	for _, e := range auditEntries(w.au.auditFake, run.CategoryStageInfraParkBudgetExhausted) {
		if e.StageID != nil && *e.StageID == w.impl.ID {
			exhausted++
		}
	}
	if exhausted != 1 {
		t.Errorf("stage_infra_park_budget_exhausted rows = %d, want 1", exhausted)
	}
	implParkRequireStageFailedC(t, w.rr, w.impl.ID)
	implParkRequireRunState(t, w.rr.fixupRecoveryRepo, w.runID, run.StateFailed)
}
