package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// --- E86.54 / #4391: the in-place conversion of a queued D6 fix-up on the
// REAL Postgres run/audit/queue/concurrency stores (newMCQFixture), through
// the HTTP verb and the ADR-092 queue hand-off. ---

// mcqAppend appends one stage-bound row for r's implement stage.
func (f *mcqFixture) mcqAppend(r *mcqRun, category string, payload any) int64 {
	f.t.Helper()
	raw, _ := json.Marshal(payload)
	kind, subject := audit.ActorSystem, mergeCandidateSystemSubject
	e, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: r.id, StageID: &r.impl.ID, Timestamp: time.Now().UTC(), Category: category,
		ActorKind: &kind, ActorSubject: &subject, Payload: raw,
	})
	if err != nil {
		f.t.Fatalf("append %s: %v", category, err)
	}
	return e.Sequence
}

// queuedD6 drives r through a REAL base-advance pass that FAILS (the verdict
// ejects r's queue entry), then parks the implement stage at
// awaiting_host_dispatch behind a D6 stage_fixup_triggered with the
// production `delegated` key, sequenced after the failed verdict. It returns
// the fix-up trigger's sequence and the head.
func (f *mcqFixture) queuedD6(r *mcqRun, head, mergeSHA string) int64 {
	f.t.Helper()
	f.behind(r, head, mergeSHA)
	if resp := f.rebase(r); resp.MergeCandidateVerifyState != mergeCandidateStateInFlight {
		f.t.Fatalf("fixture: %s rebase state = %q, want in_flight (refusal %q)", r.id, resp.MergeCandidateVerifyState, resp.MergeCandidateVerifyRefusal)
	}
	f.verdict(r, mergeCandidateResultFailed)
	f.requireState(r, mergequeue.StateEjected, mergeCandidateQueueReasonVerifyFailed)
	f.setStage(r.impl.ID, run.StageStateAwaitingHostDispatch)
	return f.mcqAppend(r, CategoryStageFixupTriggered, map[string]any{
		"delegated": mergeCandidateDelegatedRule, "prior_state": string(run.StageStateSucceeded),
		"reparked_review_stage_id": r.review.ID.String(), "concern_ids": []string{}})
}

func (f *mcqFixture) rerun(r *mcqRun) rebaseBranchResponse {
	f.t.Helper()
	w := postRebaseBranch(f.t, f.srv, r.id, rebaseBranchRequest{Confirm: true, Reason: rerunReason, RerunFailedVerify: true}, withRebaseOperator)
	if w.Code != http.StatusOK {
		f.t.Fatalf("rerun %s = %d:\n%s", r.id, w.Code, w.Body.String())
	}
	return decodeRebase(f.t, w)
}

// conversionTriggers returns r's triggers carrying supersedes_fixup_trigger_sequence.
func (f *mcqFixture) conversionTriggers(r *mcqRun) []mergeCandidateVerifyTrigger {
	var out []mergeCandidateVerifyTrigger
	for _, tr := range f.triggers(r) {
		if tr.SupersedesFixupTriggerSequence > 0 {
			out = append(out, tr)
		}
	}
	return out
}

// TestRerunQueuedFixup_Postgres_QueueAdmitsOwnEntryWithoutDeadlock (E2): the
// base is free, so the rerun's start enqueues B's entry and the pump admits
// B's OWN entry on the calling goroutine while the rerun holds B's admission
// fence; begin's arm sees the context marker (mergeCandidateAdmissionContext
// keeps values) and converts. MECHANISM (CF): make stageAdmissionHeldBy
// always false; the arm then waits stageAdmissionTryWait on its own
// goroutine's fence, answers transient, the entry is unadmitted and no
// conversion trigger lands (RED on the trigger assertion; with an unbounded
// Lock it would deadlock, caught by the bounded wait).
func TestRerunQueuedFixup_Postgres_QueueAdmitsOwnEntryWithoutDeadlock(t *testing.T) {
	f := newMCQFixture(t)
	f.srv.cfg.Orchestrator = &orchestrator.Orchestrator{Runs: f.repo}
	b := f.seed("b", "b0", []byte(mcvUndelegatedSpecYAML))
	tSeq := f.queuedD6(b, "b0", "b1")

	done := make(chan rebaseBranchResponse, 1)
	go func() { done <- f.rerun(b) }()
	var resp rebaseBranchResponse
	select {
	case resp = <-done:
	case <-time.After(timescale.D(20 * time.Second)):
		t.Fatal("the rerun did not return: the same-goroutine queue admission deadlocked on the held fence")
	}
	if n := len(f.rows(b, CategoryStageFixupSuperseded)); n != 1 {
		t.Fatalf("markers = %d, want 1 (refusal %q)", n, resp.MergeCandidateVerifyRefusal)
	}
	conv := f.conversionTriggers(b)
	if len(conv) != 1 || conv[0].SupersedesFixupTriggerSequence != tSeq || conv[0].ExpectedHeadSHA != "b1" {
		t.Fatalf("conversion triggers = %+v, want one superseding %d at b1 (response %+v)", conv, tSeq, resp)
	}
	if !resp.MergeCandidateFixupSuperseded || resp.MergeCandidateVerifyState != mergeCandidateStateInFlight {
		t.Errorf("response = superseded %v state %q, want true/in_flight", resp.MergeCandidateFixupSuperseded, resp.MergeCandidateVerifyState)
	}
	if e := f.requireState(b, mergequeue.StateLive, ""); e.Phase != mergequeue.PhaseVerifying {
		t.Errorf("B entry phase = %q, want verifying", e.Phase)
	}
	if st := f.stageState(b.impl.ID); st != run.StageStateAwaitingHostDispatch {
		t.Errorf("B implement = %s, want awaiting_host_dispatch (kept its place)", st)
	}
}

// TestRerunQueuedFixup_Postgres_RefusedWhenBaseOccupied (E3): run A holds the
// base's live pass, so B's re-run is refused up front naming A, with nothing
// written for B and B's queue rows unchanged. MECHANISM (CF): deleting the
// would-hold check in queuedMergeCandidateFixupFor lands B's marker and void
// and the pass is held (RED on the marker count).
func TestRerunQueuedFixup_Postgres_RefusedWhenBaseOccupied(t *testing.T) {
	f := newMCQFixture(t)
	f.srv.cfg.Orchestrator = &orchestrator.Orchestrator{Runs: f.repo}
	spec := []byte(mcvUndelegatedSpecYAML)
	a := f.seed("a", "a0", spec)
	b := f.seed("b", "b0", spec)
	f.queuedD6(b, "b0", "b1")
	f.behind(a, "a0", "a1")
	if resp := f.rebase(a); resp.MergeCandidateVerifyState != mergeCandidateStateInFlight {
		t.Fatalf("fixture: A rebase = %q, want in_flight", resp.MergeCandidateVerifyState)
	}
	entriesBefore := len(f.entries(b))

	resp := f.rerun(b)
	if !strings.Contains(resp.MergeCandidateVerifyRefusal, a.id.String()) || !strings.Contains(resp.MergeCandidateVerifyRefusal, "NOTHING was written") {
		t.Fatalf("refusal = %q, want A named and nothing written", resp.MergeCandidateVerifyRefusal)
	}
	if n := len(f.rows(b, CategoryStageFixupSuperseded)); n != 0 {
		t.Errorf("B markers = %d, want 0", n)
	}
	if st, _ := f.srv.mergeCandidateVerifyState(context.Background(), f.mcqRunRow(t, b), "b1"); st.State != mergeCandidateStateFailed {
		t.Errorf("B head state = %q, want still failed", st.State)
	}
	if n := len(f.entries(b)); n != entriesBefore {
		t.Errorf("B queue entries = %d, want %d (unchanged)", n, entriesBefore)
	}
}

func (f *mcqFixture) mcqRunRow(t *testing.T, r *mcqRun) *run.Run {
	t.Helper()
	row, err := f.repo.GetRun(context.Background(), r.id)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	return row
}

// heldSupersededB is the held-coexistence shape: A holds the base's live
// pass, and B carries a queued D6 fix-up with the supersede marker and the
// void landed (by construction) but NO conversion trigger; a plain rebase then
// queues B's pass HELD behind A.
func (f *mcqFixture) heldSupersededB() (a, b *mcqRun, tSeq int64) {
	f.t.Helper()
	spec := []byte(mcvUndelegatedSpecYAML)
	a = f.seed("a", "a0", spec)
	b = f.seed("b", "b0", spec)
	tSeq = f.queuedD6(b, "b0", "b1")
	f.behind(a, "a0", "a1")
	if resp := f.rebase(a); resp.MergeCandidateVerifyState != mergeCandidateStateInFlight {
		f.t.Fatalf("fixture: A rebase = %q, want in_flight", resp.MergeCandidateVerifyState)
	}
	failed, _, err := f.srv.newestRecordedVerdict(context.Background(), b.id, "b1")
	if err != nil || failed == nil {
		f.t.Fatalf("fixture: B's failed verdict unreadable: %v", err)
	}
	f.mcqAppend(b, CategoryStageFixupSuperseded, stageFixupSupersededPayload{TriggerSequence: tSeq,
		SupersededBy: fixupSupersededByVerifyRerun, FailedVerdictSequence: failed.Sequence, Reason: "operator_rerun: x"})
	if err := f.srv.voidFailedMergeCandidateVerdict(context.Background(), b.id, "b1", mergeCandidateSystemActor(), "x"); err != nil {
		f.t.Fatalf("fixture: void: %v", err)
	}
	if resp := f.rebase(b); resp.MergeCandidateVerifyState != mergeCandidateStateHeld {
		f.t.Fatalf("fixture: B plain rebase = %q, want held behind A (refusal %q)", resp.MergeCandidateVerifyState, resp.MergeCandidateVerifyRefusal)
	}
	return a, b, tSeq
}

// TestRerunQueuedFixup_Postgres_AdmissionFenceArms (approval condition C1 and
// the amendment's both-arms test): B's held, superseded-but-unconverted pass
// is admitted when A's line frees.
//
//   - transient: the test HOLDS B's admission fence and the pump runs inside
//     another stage S's critical section (a context marked for S). Begin's arm
//     answers a TRANSIENT refusal within its bound, the admission UNADMITS B
//     (held, not ejected), and no trigger lands; once the fence is released a
//     later pump converts B in place. CF: an unbounded Lock in the arm
//     deadlocks (caught by the bounded wait); treating Transient like any
//     refusal ejects B pass_not_startable (RED on requireState held).
//   - non_transient: an inert marker (a stage_concurrency_admitted row after
//     it) is a non-transient refusal, so B is ejected pass_not_startable.
func TestRerunQueuedFixup_Postgres_AdmissionFenceArms(t *testing.T) {
	t.Run("transient", func(t *testing.T) {
		f := newMCQFixture(t)
		orch := &orchestrator.Orchestrator{Runs: f.repo}
		f.srv.cfg.Orchestrator = orch
		a, b, tSeq := f.heldSupersededB()
		release := orch.LockStageAdmission(b.impl.ID)
		sctx, unlockS := f.srv.holdStageAdmission(context.Background(), uuid.New())

		done := make(chan struct{})
		go func() {
			defer close(done)
			f.freeLine(a)
			f.pumpBaseCtx(sctx, b)
		}()
		select {
		case <-done:
		case <-time.After(timescale.D(stageAdmissionTryWait + 15*time.Second)):
			release()
			unlockS()
			t.Fatal("admitting B from inside another stage's critical section did not return within the bound (deadlock)")
		}
		unlockS()
		f.requireState(b, mergequeue.StateHeld, "")
		if n := len(f.conversionTriggers(b)); n != 0 {
			t.Fatalf("B conversion triggers while its fence was held = %d, want 0", n)
		}

		release()
		f.pumpBase(b)
		conv := f.conversionTriggers(b)
		if len(conv) != 1 || conv[0].SupersedesFixupTriggerSequence != tSeq {
			t.Fatalf("B conversion triggers after the later pump = %+v, want one superseding %d", conv, tSeq)
		}
		if e := f.requireState(b, mergequeue.StateLive, ""); e.Phase != mergequeue.PhaseVerifying {
			t.Errorf("B phase = %q, want verifying", e.Phase)
		}
	})
	t.Run("non_transient", func(t *testing.T) {
		f := newMCQFixture(t)
		f.srv.cfg.Orchestrator = &orchestrator.Orchestrator{Runs: f.repo}
		a, b, _ := f.heldSupersededB()
		f.mcqAppend(b, CategoryStageConcurrencyAdmitted, map[string]any{"group": "local-implement:h1"})
		res := f.handOff(a, b)
		if res.refusal == nil || res.refusal.Transient || !strings.Contains(res.refusal.Reason, "INERT") {
			t.Fatalf("B admission = %+v, want a non-transient inert-marker refusal", res.refusal)
		}
		f.requireState(b, mergequeue.StateEjected, mergeCandidateQueueReasonPassNotStartable)
		if n := len(f.conversionTriggers(b)); n != 0 {
			t.Errorf("B conversion triggers = %d, want 0", n)
		}
	})
}

// rerunListHook wraps the Postgres queue store: it returns the REAL
// ListForBase result and then, once, runs after (so another run goes live
// AFTER the verb's occupied-base pre-check).
type rerunListHook struct {
	mergequeue.Store
	once  sync.Once
	after func()
}

func (h *rerunListHook) ListForBase(ctx context.Context, base mergequeue.Base) ([]mergequeue.Entry, error) {
	out, err := h.Store.ListForBase(ctx, base)
	h.once.Do(h.after)
	return out, err
}

// TestRerunQueuedFixup_Postgres_HeldThenConvertedAtAdmission (E4): run A goes
// live AFTER B's pre-check (the TOCTOU the stage fence cannot close), so B's
// re-run answers held with the marker and the void landed and NO conversion
// trigger, its stage still awaiting_host_dispatch. When A passes and merges,
// the hand-off admits B and the admission's begin arm writes the conversion
// trigger. MECHANISM (CF): deleting the beginSupersededFixupPass call makes
// the admission's begin refuse on the pending stage, so B is ejected
// pass_not_startable with no trigger.
func TestRerunQueuedFixup_Postgres_HeldThenConvertedAtAdmission(t *testing.T) {
	f := newMCQFixture(t)
	f.srv.cfg.Orchestrator = &orchestrator.Orchestrator{Runs: f.repo}
	spec := []byte(mcvUndelegatedSpecYAML)
	a := f.seed("a", "a0", spec)
	b := f.seed("b", "b0", spec)
	tSeq := f.queuedD6(b, "b0", "b1")
	f.behind(a, "a0", "a1")
	f.srv.cfg.MergeQueue = &rerunListHook{Store: f.store, after: func() {
		if resp := f.rebase(a); resp.MergeCandidateVerifyState != mergeCandidateStateInFlight {
			t.Errorf("fixture: A rebase inside the hook = %q, want in_flight", resp.MergeCandidateVerifyState)
		}
	}}

	resp := f.rerun(b)
	if resp.MergeCandidateVerifyState != mergeCandidateStateHeld || resp.MergeCandidateFixupSuperseded || !resp.MergeCandidateVerifyRerun {
		t.Fatalf("response = state %q superseded %v rerun %v refusal %q, want held/false/true",
			resp.MergeCandidateVerifyState, resp.MergeCandidateFixupSuperseded, resp.MergeCandidateVerifyRerun, resp.MergeCandidateVerifyRefusal)
	}
	if !strings.Contains(resp.MergeCandidateVerifyNote, a.id.String()) || !strings.Contains(resp.MergeCandidateVerifyNote, "still dispatchable as a fix-up") {
		t.Errorf("note = %q, want A named and the fix-up still dispatchable", resp.MergeCandidateVerifyNote)
	}
	if n := len(f.rows(b, CategoryStageFixupSuperseded)); n != 1 {
		t.Fatalf("B markers = %d, want 1", n)
	}
	if n := len(f.conversionTriggers(b)); n != 0 {
		t.Fatalf("B conversion triggers while held = %d, want 0", n)
	}
	if st := f.stageState(b.impl.ID); st != run.StageStateAwaitingHostDispatch {
		t.Errorf("B implement = %s, want awaiting_host_dispatch", st)
	}
	f.requireState(b, mergequeue.StateHeld, "")

	f.verdict(a, mergeCandidateResultPassed)
	f.closePR(a, true)
	conv := f.conversionTriggers(b)
	if len(conv) != 1 || conv[0].SupersedesFixupTriggerSequence != tSeq {
		t.Fatalf("B conversion triggers after the hand-off = %+v, want one superseding %d", conv, tSeq)
	}
	if e := f.requireState(b, mergequeue.StateLive, ""); e.Phase != mergequeue.PhaseVerifying {
		t.Errorf("B phase = %q, want verifying", e.Phase)
	}
}
