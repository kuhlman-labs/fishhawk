package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// ADR-093 D8 (E87.7 / #4298) slice 1: the verifying recovery over the REAL
// Postgres merge queue and audit chain (the mqmWorld fixture). A lost queue
// hook is seeded by appending merge_candidate_verified DIRECTLY on the raw
// audit repository (mqrvVerdict), bypassing recordMergeCandidateVerifiedEvidence
// and so the hook: exactly the persisted state a crash between the append
// and the hook leaves. Every helper this slice adds lives in this file.

// mqrvSeed seeds a run at a base-advance head (so the head needs a
// merge-candidate verify), enqueues it (with merge intent and its
// merge_queue_entered row when intent), admits it through the real store and
// records phase through the real SetPhase CAS.
func mqrvSeed(w *mqmWorld, name, head string, phase mergequeue.Phase, intent bool) (*mcqRun, mergequeue.Entry) {
	w.t.Helper()
	ctx := context.Background()
	r := w.seed(name, head, []byte(mcvUndelegatedSpecYAML))
	w.mqeRebased(r, "pre-"+head, head, head, false)
	req := mergequeue.EnqueueRequest{RunID: r.id, BaseRef: mcqBase, HeadSHA: head, MergeIntent: intent}
	if intent {
		req.IntentSequence = w.mqeIntent(r, head).Sequence
	}
	res, err := w.store.Enqueue(ctx, req)
	if err != nil {
		w.t.Fatalf("enqueue %s: %v", name, err)
	}
	if adm, err := w.store.AdmitNext(ctx, res.Entry.Base()); err != nil || !adm.Admitted {
		w.t.Fatalf("admit %s: %+v %v", name, adm, err)
	}
	e := w.active(r)
	if e.Phase != phase {
		if e, err = w.store.SetPhase(ctx, e.ID, e.AdmissionEpoch, phase); err != nil {
			w.t.Fatalf("set phase %s: %v", phase, err)
		}
	}
	return r, e
}

// mqrvVerdict appends a merge_candidate_verified row at head on r's
// implement stage BY CONSTRUCTION: no queue hook runs.
func mqrvVerdict(w *mqmWorld, r *mcqRun, head, result, reason string) *audit.Entry {
	w.t.Helper()
	raw, _ := json.Marshal(mergeCandidateVerifiedPayload{HeadSHA: head, Cause: mergeCandidateCauseBaseAdvance,
		VerifyCommand: "scripts/test verify", Result: result, Reason: reason, OutputUntrusted: true})
	kind, subj := audit.ActorSystem, "test"
	stage := r.impl.ID
	e, err := w.mcqFixture.audit.AppendChained(context.Background(), audit.ChainAppendParams{RunID: r.id, StageID: &stage,
		Timestamp: time.Now().UTC(), Category: CategoryMergeCandidateVerified, ActorKind: &kind, ActorSubject: &subj, Payload: raw})
	if err != nil {
		w.t.Fatalf("seed %s verdict: %v", result, err)
	}
	return e
}

// mqrvResumed decodes r's merge_queue_resumed rows.
func mqrvResumed(w *mqmWorld, r *mcqRun) []map[string]any {
	w.t.Helper()
	var out []map[string]any
	for _, e := range w.rows(r, CategoryMergeQueueResumed) {
		out = append(out, w.payload(e))
	}
	return out
}

// mqrvRequireActions asserts r's merge_queue_resumed rows carry exactly the
// actions, in order.
func mqrvRequireActions(w *mqmWorld, r *mcqRun, want ...string) []map[string]any {
	w.t.Helper()
	rows := mqrvResumed(w, r)
	var got []string
	for _, p := range rows {
		got = append(got, p["action"].(string))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		w.t.Fatalf("merge_queue_resumed actions = %v, want %v", got, want)
	}
	return rows
}

// mqrvRequirePhase asserts r's active entry is live in phase.
func mqrvRequirePhase(w *mqmWorld, r *mcqRun, phase mergequeue.Phase) mergequeue.Entry {
	w.t.Helper()
	e := w.active(r)
	if e.State != mergequeue.StateLive || e.Phase != phase {
		w.t.Fatalf("entry = %s/%s, want live/%s", e.State, e.Phase, phase)
	}
	return e
}

// mqrvStagesErrRepo fails ListStagesForRun; every other method is the real
// run repository.
type mqrvStagesErrRepo struct{ run.Repository }

func (mqrvStagesErrRepo) ListStagesForRun(context.Context, uuid.UUID) ([]*run.Stage, error) {
	return nil, errors.New("injected stage list failure")
}

// mqrvListErrAudit fails ListForRunByCategory for one category; every other
// method is the wrapped repository.
type mqrvListErrAudit struct {
	audit.Repository
	failCategory string
}

func (a mqrvListErrAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if category == a.failCategory {
		return nil, errors.New("injected list failure")
	}
	return a.Repository.ListForRunByCategory(ctx, runID, category)
}

// TestMergeQueueResumeVerifying_LostPassedHookApplied: a passed verdict row at
// the anchored head whose hook was lost (the entry is still verifying) is
// applied: phase passed, ONE merge_queue_verified source pass, ONE
// merge_queue_resumed apply_verdict written BEFORE it, and the pump continues.
func TestMergeQueueResumeVerifying_LostPassedHookApplied(t *testing.T) {
	w := newMQMWorld(t)
	r, e := mqrvSeed(w, "a", "a1", mergequeue.PhaseVerifying, true)
	mqrvVerdict(w, r, "a1", mergeCandidateResultPassed, "")

	if !w.srv.mergeQueueResumeVerifying(context.Background(), e) {
		t.Fatal("cont = false, want true (the passed entry continues to merge-on-pass)")
	}
	mqrvRequirePhase(w, r, mergequeue.PhasePassed)
	verified := w.rows(r, CategoryMergeQueueVerified)
	if len(verified) != 1 || w.payload(verified[0])["source"] != mergeQueueVerifiedSourcePass {
		t.Fatalf("merge_queue_verified rows = %d (%v), want one source pass", len(verified), verified)
	}
	rows := mqrvRequireActions(w, r, mergeQueueResumeActionApplyVerdict)
	if rows[0]["step"] != string(mergequeue.PhaseVerifying) || rows[0]["reason"] != mergeQueueResumeReasonLostPassedHook {
		t.Fatalf("row = %v, want step verifying, reason lost_passed_hook", rows[0])
	}
	if resumed := w.rows(r, CategoryMergeQueueResumed); resumed[0].Sequence > verified[0].Sequence {
		t.Fatal("merge_queue_resumed was appended AFTER the action it records")
	}
}

// TestMergeQueueResumeVerifying_LostFailedHookEjectsFailClosed (ruling R8b):
// a failed verdict row whose hook was lost ejects verify_failed, and the
// ejection records fixup_routed=false and next_step fishhawk_fixup_stage even
// though a D6 delegated fix-up row FOLLOWS the verdict: the derived default
// would answer routed=true there, so this fixture isolates the explicit
// fail-closed detail.
func TestMergeQueueResumeVerifying_LostFailedHookEjectsFailClosed(t *testing.T) {
	w := newMQMWorld(t)
	r, e := mqrvSeed(w, "a", "a1", mergequeue.PhaseVerifying, true)
	mqrvVerdict(w, r, "a1", mergeCandidateResultFailed, "")
	raw, _ := json.Marshal(map[string]any{"delegated": mergeCandidateDelegatedRule, "prior_state": "succeeded"})
	kind, subj := audit.ActorSystem, "test"
	stage := r.impl.ID
	if _, err := w.mcqFixture.audit.AppendChained(context.Background(), audit.ChainAppendParams{RunID: r.id, StageID: &stage,
		Timestamp: time.Now().UTC(), Category: CategoryStageFixupTriggered, ActorKind: &kind, ActorSubject: &subj, Payload: raw}); err != nil {
		t.Fatalf("seed D6 fix-up row: %v", err)
	}

	if !w.srv.mergeQueueResumeVerifying(context.Background(), e) {
		t.Fatal("cont = false, want true after the ejection settled")
	}
	w.requireState(r, mergequeue.StateEjected, mergeCandidateQueueReasonVerifyFailed)
	ejected := w.rows(r, CategoryMergeQueueEjected)
	if len(ejected) != 1 {
		t.Fatalf("merge_queue_ejected rows = %d, want 1", len(ejected))
	}
	p := w.payload(ejected[0])
	if p["fixup_routed"] != false || p["next_step"] != mergeQueueEjectNextFixupStage {
		t.Fatalf("ejection = fixup_routed %v next_step %v, want false / %s", p["fixup_routed"], p["next_step"], mergeQueueEjectNextFixupStage)
	}
	if !strings.Contains(p["refusal"].(string), "queue hook was lost to a restart") {
		t.Fatalf("refusal = %q, want the lost-hook reason", p["refusal"])
	}
	mqrvRequireActions(w, r, mergeQueueResumeActionEject)
}

// TestMergeQueueResumeVerifying_InFlightHolds: a live trigger names the head,
// so the entry holds: no phase write, no row, no second trigger.
func TestMergeQueueResumeVerifying_InFlightHolds(t *testing.T) {
	w := newMQMWorld(t)
	r, e := mqrvSeed(w, "a", "a1", mergequeue.PhaseVerifying, true)
	mqrvVerdict(w, r, "a1", mergeCandidateResultNotExecuted, "runner_failed: crashed")
	w.seedTrigger(r, "a1")

	if w.srv.mergeQueueResumeVerifying(context.Background(), e) {
		t.Fatal("cont = true, want a hold")
	}
	mqrvRequirePhase(w, r, mergequeue.PhaseVerifying)
	mqrvRequireActions(w, r)
	if n := len(w.triggers(r)); n != 1 {
		t.Fatalf("triggers = %d, want the one seeded", n)
	}
}

// TestMergeQueueResumeVerifying_NoEvidenceHoldsUnchanged: no verdict row and
// no trigger (the integration fixtures' live-verifying shape) changes nothing.
func TestMergeQueueResumeVerifying_NoEvidenceHoldsUnchanged(t *testing.T) {
	w := newMQMWorld(t)
	r, e := mqrvSeed(w, "a", "a1", mergequeue.PhaseVerifying, true)
	if w.srv.mergeQueueResumeVerifying(context.Background(), e) {
		t.Fatal("cont = true, want a hold")
	}
	mqrvRequirePhase(w, r, mergequeue.PhaseVerifying)
	mqrvRequireActions(w, r)
	if n := len(w.rows(r, CategoryMergeQueueVerified)); n != 0 {
		t.Fatalf("merge_queue_verified rows = %d, want 0", n)
	}
}

// TestMergeQueueResumeVerifying_NotLiveOrOtherPhaseIgnored: only a live
// verifying or unverified entry is resumed here.
func TestMergeQueueResumeVerifying_NotLiveOrOtherPhaseIgnored(t *testing.T) {
	w := newMQMWorld(t)
	r, e := mqrvSeed(w, "a", "a1", mergequeue.PhasePassed, true)
	mqrvVerdict(w, r, "a1", mergeCandidateResultFailed, "")
	if w.srv.mergeQueueResumeVerifying(context.Background(), e) {
		t.Fatal("a passed entry was resumed by the verifying recovery")
	}
	mqrvRequirePhase(w, r, mergequeue.PhasePassed)
	mqrvRequireActions(w, r)
}

// TestMergeQueueResumeVerifying_LostNotExecutedHookRecordsUnverified: a
// not_executed verdict that is not a crashed runner's leaves a verifying
// entry in unverified (the lost hook's write), with no re-dispatch.
func TestMergeQueueResumeVerifying_LostNotExecutedHookRecordsUnverified(t *testing.T) {
	w := newMQMWorld(t)
	r, e := mqrvSeed(w, "a", "a1", mergequeue.PhaseVerifying, true)
	mqrvVerdict(w, r, "a1", mergeCandidateResultNotExecuted, "head_moved")
	if w.srv.mergeQueueResumeVerifying(context.Background(), e) {
		t.Fatal("cont = true, want the unverified entry to hold the line")
	}
	mqrvRequirePhase(w, r, mergequeue.PhaseUnverified)
	rows := mqrvRequireActions(w, r, mergeQueueResumeActionRecordUnverified)
	if rows[0]["reason"] != mergeQueueResumeReasonLostNotExecutedHook {
		t.Fatalf("reason = %v, want lost_not_executed_hook", rows[0]["reason"])
	}
	if n := len(w.triggers(r)); n != 0 {
		t.Fatalf("triggers = %d, want 0 (no re-dispatch)", n)
	}
}

// TestMergeQueueResumeVerifying_RedispatchOnce: ONE runner_failed not_executed
// verdict at the anchored head, merge intent, no live stage, the implement
// stage at its review gate and the PR still at the head: the lost hook is
// recorded (unverified), then exactly ONE new trigger is appended for the
// head and the entry is verifying again.
func TestMergeQueueResumeVerifying_RedispatchOnce(t *testing.T) {
	w := newMQMWorld(t)
	r, e := mqrvSeed(w, "a", "a1", mergequeue.PhaseVerifying, true)
	mqrvVerdict(w, r, "a1", mergeCandidateResultNotExecuted, "runner_failed: the runner crashed")

	w.srv.mergeQueueResumeVerifying(context.Background(), e)
	trig := w.triggers(r)
	if len(trig) != 1 || trig[0].ExpectedHeadSHA != "a1" {
		t.Fatalf("triggers = %+v, want one for a1", trig)
	}
	mqrvRequirePhase(w, r, mergequeue.PhaseVerifying)
	rows := mqrvRequireActions(w, r, mergeQueueResumeActionRecordUnverified, mergeQueueResumeActionRedispatchPass)
	if rows[1]["step"] != string(mergequeue.PhaseUnverified) {
		t.Fatalf("redispatch step = %v, want unverified", rows[1]["step"])
	}
	// A second resume while the re-dispatched pass is in flight starts nothing.
	w.srv.mergeQueueResumeVerifying(context.Background(), w.active(r))
	if n := len(w.triggers(r)); n != 1 {
		t.Fatalf("triggers after a second resume = %d, want 1", n)
	}
}

// TestMergeQueueResumeVerifying_RedispatchRefusals: every unmet re-dispatch
// precondition holds the unverified entry for the operator's
// fishhawk_rebase_run_branch with NO new trigger and no redispatch row.
func TestMergeQueueResumeVerifying_RedispatchRefusals(t *testing.T) {
	cases := []struct {
		name   string
		intent bool
		setup  func(w *mqmWorld, r *mcqRun)
	}{
		{"RedispatchAtMostOnce", true, func(w *mqmWorld, r *mcqRun) {
			mqrvVerdict(w, r, "a1", mergeCandidateResultNotExecuted, "runner_failed: first crash")
			mqrvVerdict(w, r, "a1", mergeCandidateResultNotExecuted, "runner_failed: second crash")
		}},
		// The head carries exactly ONE runner_failed row, so the count check
		// passes and only the NEWEST verdict's reason prefix refuses.
		{"RedispatchReasonFiltered", true, func(w *mqmWorld, r *mcqRun) {
			mqrvVerdict(w, r, "a1", mergeCandidateResultNotExecuted, "runner_failed: crashed")
			mqrvVerdict(w, r, "a1", mergeCandidateResultNotExecuted, mergeCandidateReasonRunnerCapabilityMissing+": stale runner")
		}},
		{"RedispatchRequiresIntent", false, func(w *mqmWorld, r *mcqRun) {
			mqrvVerdict(w, r, "a1", mergeCandidateResultNotExecuted, "runner_failed: crashed")
		}},
		{"RedispatchHeldWhileStageLive", true, func(w *mqmWorld, r *mcqRun) {
			mqrvVerdict(w, r, "a1", mergeCandidateResultNotExecuted, "runner_failed: crashed")
			w.setStage(r.review.ID, run.StageStateRunning)
		}},
		{"RedispatchHeldWhenImplementNotAtGate", true, func(w *mqmWorld, r *mcqRun) {
			mqrvVerdict(w, r, "a1", mergeCandidateResultNotExecuted, "runner_failed: crashed")
			w.setStage(r.impl.ID, run.StageStateFailed)
		}},
		{"RedispatchHeldWhenHeadMoved", true, func(w *mqmWorld, r *mcqRun) {
			mqrvVerdict(w, r, "a1", mergeCandidateResultNotExecuted, "runner_failed: crashed")
			w.gh.set(func() { w.gh.prs[r.pr].head = "a2" })
		}},
		{"RedispatchHeldWhenPRReadFails", true, func(w *mqmWorld, r *mcqRun) {
			mqrvVerdict(w, r, "a1", mergeCandidateResultNotExecuted, "runner_failed: crashed")
			w.mu.Lock()
			w.prReadFail = true
			w.mu.Unlock()
		}},
		{"RedispatchHeldWhenPRClosed", true, func(w *mqmWorld, r *mcqRun) {
			mqrvVerdict(w, r, "a1", mergeCandidateResultNotExecuted, "runner_failed: crashed")
			w.gh.set(func() { w.gh.prs[r.pr].state = "closed" })
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newMQMWorld(t)
			r, e := mqrvSeed(w, "a", "a1", mergequeue.PhaseUnverified, tc.intent)
			tc.setup(w, r)
			if w.srv.mergeQueueResumeVerifying(context.Background(), e) {
				t.Fatal("cont = true, want a hold")
			}
			if n := len(w.triggers(r)); n != 0 {
				t.Fatalf("triggers = %d, want 0", n)
			}
			mqrvRequirePhase(w, r, mergequeue.PhaseUnverified)
			mqrvRequireActions(w, r)
		})
	}
}

// TestMergeQueueResumeVerifying_AppendFailureHoldsUnchanged (ruling R8d): the
// merge_queue_resumed append fails, so the lost passed hook is NOT applied:
// the entry stays verifying and no merge_queue_verified row is written.
func TestMergeQueueResumeVerifying_AppendFailureHoldsUnchanged(t *testing.T) {
	w := newMQMWorld(t)
	r, e := mqrvSeed(w, "a", "a1", mergequeue.PhaseVerifying, true)
	mqrvVerdict(w, r, "a1", mergeCandidateResultPassed, "")
	w.audit.failCategory = CategoryMergeQueueResumed

	if w.srv.mergeQueueResumeVerifying(context.Background(), e) {
		t.Fatal("cont = true, want a hold on the failed append")
	}
	mqrvRequirePhase(w, r, mergequeue.PhaseVerifying)
	if n := len(w.rows(r, CategoryMergeQueueVerified)); n != 0 {
		t.Fatalf("merge_queue_verified rows = %d, want 0", n)
	}
}

// TestMergeQueueResumeVerifying_AppendFailureEjectsNothing (R8d, eject arm):
// a failed append leaves the failed-verdict entry live.
func TestMergeQueueResumeVerifying_AppendFailureEjectsNothing(t *testing.T) {
	w := newMQMWorld(t)
	r, e := mqrvSeed(w, "a", "a1", mergequeue.PhaseVerifying, true)
	mqrvVerdict(w, r, "a1", mergeCandidateResultFailed, "")
	w.audit.failCategory = CategoryMergeQueueResumed
	w.srv.mergeQueueResumeVerifying(context.Background(), e)
	mqrvRequirePhase(w, r, mergequeue.PhaseVerifying)
	if n := len(w.rows(r, CategoryMergeQueueEjected)); n != 0 {
		t.Fatalf("merge_queue_ejected rows = %d, want 0", n)
	}
}

// TestMergeQueueResumeVerifying_EntryChangedInLockHolds: the admission epoch
// is bumped between the unlocked pre-read and the lock, so nothing is
// written.
func TestMergeQueueResumeVerifying_EntryChangedInLockHolds(t *testing.T) {
	w := newMQMWorld(t)
	r, e := mqrvSeed(w, "a", "a1", mergequeue.PhaseVerifying, true)
	mqrvVerdict(w, r, "a1", mergeCandidateResultPassed, "")
	w.srv.cfg.MergeQueue = mqmOnLockQueue{Store: w.store, onLock: func() {
		w.exec(`UPDATE merge_candidate_queue_entries SET admission_epoch = admission_epoch + 1 WHERE id = $1`, e.ID)
	}}
	if w.srv.mergeQueueResumeVerifying(context.Background(), e) {
		t.Fatal("cont = true, want a hold")
	}
	mqrvRequirePhase(w, r, mergequeue.PhaseVerifying)
	mqrvRequireActions(w, r)
}

// TestMergeQueueResumeVerifying_EvidenceChangedInLockHolds: the verdict the
// pre-read decided on is superseded before the lock (a newer failed row), so
// the in-lock re-derivation differs and nothing is written.
func TestMergeQueueResumeVerifying_EvidenceChangedInLockHolds(t *testing.T) {
	w := newMQMWorld(t)
	r, e := mqrvSeed(w, "a", "a1", mergequeue.PhaseVerifying, true)
	mqrvVerdict(w, r, "a1", mergeCandidateResultPassed, "")
	w.srv.cfg.MergeQueue = mqmOnLockQueue{Store: w.store, onLock: func() {
		mqrvVerdict(w, r, "a1", mergeCandidateResultFailed, "")
	}}
	w.srv.mergeQueueResumeVerifying(context.Background(), e)
	mqrvRequirePhase(w, r, mergequeue.PhaseVerifying)
	mqrvRequireActions(w, r)
}

// TestMergeQueueResumeVerifying_LockOrReadFailureHolds: an unavailable
// merge-intent lock and an unreadable verdict chain both hold with nothing
// written.
func TestMergeQueueResumeVerifying_LockOrReadFailureHolds(t *testing.T) {
	t.Run("lock", func(t *testing.T) {
		w := newMQMWorld(t)
		r, e := mqrvSeed(w, "a", "a1", mergequeue.PhaseVerifying, true)
		mqrvVerdict(w, r, "a1", mergeCandidateResultPassed, "")
		w.srv.cfg.MergeQueue = mqmLockQueue{Store: w.store, lockErr: errors.New("no lock")}
		if w.srv.mergeQueueResumeVerifying(context.Background(), e) {
			t.Fatal("cont = true, want a hold")
		}
		mqrvRequirePhase(w, r, mergequeue.PhaseVerifying)
		mqrvRequireActions(w, r)
	})
	t.Run("read", func(t *testing.T) {
		w := newMQMWorld(t)
		r, e := mqrvSeed(w, "a", "a1", mergequeue.PhaseVerifying, true)
		mqrvVerdict(w, r, "a1", mergeCandidateResultPassed, "")
		w.srv.cfg.AuditRepo = mqrvListErrAudit{Repository: w.audit, failCategory: CategoryMergeCandidateVerified}
		if w.srv.mergeQueueResumeVerifying(context.Background(), e) {
			t.Fatal("cont = true, want a hold")
		}
		mqrvRequirePhase(w, r, mergequeue.PhaseVerifying)
		mqrvRequireActions(w, r)
	})
}

// TestMergeQueueRunStageLive pins the R7 gate's arms.
func TestMergeQueueRunStageLive(t *testing.T) {
	cases := []struct {
		name  string
		setup func(w *mqmWorld, r *mcqRun)
		live  bool
	}{
		{"dispatched", func(w *mqmWorld, r *mcqRun) { w.setStage(r.review.ID, run.StageStateDispatched) }, true},
		{"running", func(w *mqmWorld, r *mcqRun) { w.setStage(r.impl.ID, run.StageStateRunning) }, true},
		{"trigger_with_implement_pending", func(w *mqmWorld, r *mcqRun) {
			w.seedTrigger(r, "a1")
			w.setStage(r.impl.ID, run.StageStatePending)
		}, true},
		{"none", func(*mqmWorld, *mcqRun) {}, false},
		{"stage_list_error", func(w *mqmWorld, _ *mcqRun) {
			w.srv.cfg.RunRepo = mqrvStagesErrRepo{Repository: w.repo}
		}, true},
		{"trigger_read_error", func(w *mqmWorld, _ *mcqRun) {
			w.srv.cfg.AuditRepo = mqrvListErrAudit{Repository: w.audit, failCategory: CategoryStageMergeCandidateVerifyTriggered}
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newMQMWorld(t)
			r := w.seed("a", "a1", []byte(mcvUndelegatedSpecYAML))
			tc.setup(w, r)
			live, why := w.srv.mergeQueueRunStageLive(context.Background(), r.id)
			if live != tc.live {
				t.Fatalf("live = %v (%q), want %v", live, why, tc.live)
			}
			if live && why == "" {
				t.Fatal("a live answer carries no reason")
			}
		})
	}
}

// TestAppendMergeQueueResumed_DedupsOnlyHolds (approval condition C7): a
// repeated hold writes ONE row; a hold with a different reason writes its
// own; a repeated state-changing action always writes two; an action outside
// the closed vocabulary, or a hold whose dedup read fails, writes nothing and
// returns the error (fail closed).
func TestAppendMergeQueueResumed_DedupsOnlyHolds(t *testing.T) {
	w := newMQMWorld(t)
	r, e := mqrvSeed(w, "a", "a1", mergequeue.PhaseMerging, true)
	ctx := context.Background()
	hold := mergeQueueResumed{Step: "merging", Action: mergeQueueResumeActionHold, Reason: "merge_dispatch_uncertain"}
	for i, want := range []bool{true, false} {
		if got, err := w.srv.appendMergeQueueResumed(ctx, e, hold); err != nil || got != want {
			t.Fatalf("hold #%d appended = %v, %v; want %v", i+1, got, err, want)
		}
	}
	other := hold
	other.Reason = "settling_unconfirmed"
	if got, err := w.srv.appendMergeQueueResumed(ctx, e, other); err != nil || !got {
		t.Fatalf("hold with a new reason appended = %v, %v; want true", got, err)
	}
	for i := 0; i < 2; i++ {
		if got, err := w.srv.appendMergeQueueResumed(ctx, e, mergeQueueResumed{Step: "merging", Action: mergeQueueResumeActionRedecide}); err != nil || !got {
			t.Fatalf("redecide #%d appended = %v, %v; want true", i+1, got, err)
		}
	}
	mqrvRequireActions(w, r, mergeQueueResumeActionHold, mergeQueueResumeActionHold,
		mergeQueueResumeActionRedecide, mergeQueueResumeActionRedecide)

	if _, err := w.srv.appendMergeQueueResumed(ctx, e, mergeQueueResumed{Step: "merging", Action: "merge"}); err == nil {
		t.Fatal("an out-of-vocabulary action was accepted")
	}
	if _, err := w.srv.appendMergeQueueResumed(ctx, e, mergeQueueResumed{Action: mergeQueueResumeActionHold}); err == nil {
		t.Fatal("a row with no step was accepted")
	}
	w.srv.cfg.AuditRepo = mqrvListErrAudit{Repository: w.audit, failCategory: CategoryMergeQueueResumed}
	third := hold
	third.Reason = "claim_unreadable"
	if got, err := w.srv.appendMergeQueueResumed(ctx, e, third); err == nil || got {
		t.Fatalf("a hold with an unreadable dedup row appended = %v, %v; want an error", got, err)
	}
	if n := len(mqrvResumed(w, r)); n != 4 {
		t.Fatalf("rows = %d after the refused appends, want 4", n)
	}
	w.srv.cfg.AuditRepo = nil
	if _, err := w.srv.appendMergeQueueResumed(ctx, e, hold); err == nil {
		t.Fatal("an unwired audit repository appended")
	}
}

// TestMergeQueueResumed_PayloadCarriesOnlyOwnRun (approval condition C10,
// ADR-106): the row lands on its own run, its keys are the documented
// vocabulary plus Extra, its identity keys cannot be overwritten through
// Extra, and no value carries another run's id or entry id.
func TestMergeQueueResumed_PayloadCarriesOnlyOwnRun(t *testing.T) {
	w := newMQMWorld(t)
	a, ea := mqrvSeed(w, "a", "a1", mergequeue.PhaseMerging, true)
	b := w.seed("b", "b1", []byte(mcvUndelegatedSpecYAML))
	resB, err := w.store.Enqueue(context.Background(), mergequeue.EnqueueRequest{RunID: b.id, BaseRef: mcqBase, HeadSHA: "b1"})
	if err != nil {
		t.Fatalf("enqueue b: %v", err)
	}
	if _, err := w.srv.appendMergeQueueResumed(context.Background(), ea, mergeQueueResumed{
		Step: "merging", Action: mergeQueueResumeActionSettleMerged, Reason: "pr_merged",
		ClaimState: "dispatched", ClaimPath: "queue", ClaimHeadSHA: "a1", NextStep: "none", Remedy: "nothing to do",
		Extra: map[string]any{"entry_id": resB.Entry.ID.String(), "step": "forged", "verdict_sequence": 7},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if n := len(w.rows(b, CategoryMergeQueueResumed)); n != 0 {
		t.Fatalf("run b carries %d merge_queue_resumed rows, want 0", n)
	}
	rows := w.rows(a, CategoryMergeQueueResumed)
	if len(rows) != 1 || rows[0].RunID == nil || *rows[0].RunID != a.id {
		t.Fatalf("run a rows = %d, want one on run a", len(rows))
	}
	p := w.payload(rows[0])
	allowed := map[string]bool{"entry_id": true, "base_ref": true, "step": true, "action": true, "reason": true,
		"head_sha": true, "admission_epoch": true, "next_step": true, "remedy": true, "claim_state": true,
		"claim_path": true, "claim_head_sha": true, "verdict_sequence": true}
	for k, v := range p {
		if !allowed[k] {
			t.Errorf("payload key %q is outside the vocabulary", k)
		}
		if s, ok := v.(string); ok && (s == b.id.String() || s == resB.Entry.ID.String()) {
			t.Errorf("payload %q carries another run's id %q", k, s)
		}
	}
	if p["entry_id"] != ea.ID.String() || p["step"] != "merging" || p["head_sha"] != "a1" || p["claim_path"] != "queue" {
		t.Fatalf("payload = %v, want run a's own identity", p)
	}
}

// TestMergeQueueResumeForgeTarget pins the target resolution's refusals.
func TestMergeQueueResumeForgeTarget(t *testing.T) {
	inst := int64(7)
	zero := int64(0)
	pr := "https://github.com/x/y/pull/3"
	for name, r := range map[string]*run.Run{
		"no_installation": {Repo: "x/y", PullRequestURL: &pr},
		"zero_install":    {InstallationID: &zero, Repo: "x/y", PullRequestURL: &pr},
		"bad_repo":        {InstallationID: &inst, Repo: "nope", PullRequestURL: &pr},
		"no_pr":           {InstallationID: &inst, Repo: "x/y"},
	} {
		if _, err := mergeQueueResumeForgeTarget(r); err == nil {
			t.Errorf("%s: resolved a target, want a refusal", name)
		}
	}
	got, err := mergeQueueResumeForgeTarget(&run.Run{InstallationID: &inst, Repo: "x/y", PullRequestURL: &pr})
	if err != nil || got.number != 3 {
		t.Fatalf("target = %+v, %v; want PR 3", got, err)
	}
}
