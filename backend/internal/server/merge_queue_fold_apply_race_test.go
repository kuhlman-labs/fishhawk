package server

// #4876: a merge-on-pass outcome decided under the run's merge-intent lock is
// applied by the pump OUTSIDE that lock. These tests interleave two Servers
// over ONE database so that the stale apply lands after another pump stamped
// merging and claimed the dispatch.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// TestMergeOnPassApply_TwoServerStaleApplyAfterClaim crosses the pump's
// out-of-lock apply (applyMergeOnPassOutcome), the store's phase-guarded
// compare-and-swaps (UnadmitPassed / YieldPassed over Postgres) and the real
// D1 dispatch ledger. Server A is the world's Server; Server B is New(a copy
// of A's Config) with its OWN mergequeue.PostgresStore over the same pool
// (independent in-process lock bookkeeping), its own in-flight registry and
// its own parked merger. Per arm: A runs its real decision and the test
// checks the outcome; the forge then flips so B can merge; B runs its real
// decision and parks inside MergePullRequestAtHead with the entry merging and
// its queue claim dispatched; A then applies its stale outcome. While B is
// parked the apply must return false and leave the entry live, merging, not
// yielded, with no merge_queue_yielded row and B's claim dispatched. Released,
// B settles the entry merged with its single pinned PUT.
//
// MECHANISM: SetPhase(merging) keeps the admission epoch, so without the
// phase guard A's stale Yield or Unadmit (live + epoch) matches and moves the
// claimed entry to held (yielded for arms a/b). Nothing downstream masks it:
// the apply consults no registry, and ApprovalWait compares only live+epoch.
// Arm (d) also needs the in-lock return to passed after A's released claim:
// without it the entry stays merging and B's pre-lock check holds not_passed,
// so B never reaches its PUT (a fixture failure, not an assertion).
func TestMergeOnPassApply_TwoServerStaleApplyAfterClaim(t *testing.T) {
	arms := []struct {
		name   string
		spec   string
		setup  func(w *mqmWorld, r *mcqRun)
		after  func(t *testing.T, w *mqmWorld, r *mcqRun, e mergequeue.Entry)
		flip   func(w *mqmWorld, r *mcqRun)
		kind   mergeOnPassKind
		reason string
		// postRelease (arm d): A claimed, its PUT was refused 409 and its claim
		// released, so A's own in-flight mark stands at apply time.
		postRelease bool
	}{
		{
			name: "a immediate veto yield", spec: mqeSpecYAML,
			setup: func(w *mqmWorld, r *mcqRun) {
				w.forgeReviews(r, []string{"alice", "bob"}, mqmReview(1, "alice", "APPROVED", "h1"), mqmReview(2, "bob", "CHANGES_REQUESTED", "h1"))
			},
			flip: func(w *mqmWorld, r *mcqRun) {
				w.forgeReviews(r, []string{"alice"}, mqmReview(1, "alice", "APPROVED", "h1"))
			},
			kind:   mergeOnPassYield,
			reason: mergeCandidateQueueReasonForgeApprovalVetoed,
		},
		{
			name: "b await_approval expired", spec: mqeSpecYAML,
			setup: func(w *mqmWorld, r *mcqRun) {
				w.forgeReviews(r, []string{"alice"}, mqmReview(1, "alice", "DISMISSED", "h1"))
			},
			after: func(t *testing.T, w *mqmWorld, r *mcqRun, e mergequeue.Entry) {
				// The wait anchor is stamped and backdated past the window, so
				// the stale apply's own ApprovalWait reports expired, then yields.
				if _, err := w.store.ApprovalWait(context.Background(), e.ID, e.AdmissionEpoch, DefaultMergeQueueApprovalWait); err != nil {
					t.Fatalf("stamp the approval wait: %v", err)
				}
				w.backdateApprovalWait(r)
			},
			flip: func(w *mqmWorld, r *mcqRun) {
				w.forgeReviews(r, []string{"alice"}, mqmReview(1, "alice", "APPROVED", "h1"))
			},
			kind:   mergeOnPassAwaitApproval,
			reason: mergeCandidateQueueReasonForgeApprovalDismissed,
		},
		{
			name: "c base-moved readvance", spec: mcvUndelegatedSpecYAML,
			setup:  func(w *mqmWorld, r *mcqRun) { w.behind(r, "h1", "h2") },
			flip:   func(w *mqmWorld, r *mcqRun) { w.gh.set(func() { delete(w.gh.behind, "h1") }) },
			kind:   mergeOnPassReadvance,
			reason: mergeOnPassReasonBaseMoved,
		},
		{
			name: "d post-release readvance", spec: mcvUndelegatedSpecYAML,
			setup:       func(w *mqmWorld, _ *mcqRun) { w.merger.err = fmt.Errorf("pinned: %w", forge.ErrMergeConflict) },
			flip:        func(*mqmWorld, *mcqRun) {},
			kind:        mergeOnPassReadvance,
			reason:      mergeOnPassReasonForgeHeadMismatch,
			postRelease: true,
		},
	}
	for _, arm := range arms {
		t.Run(arm.name, func(t *testing.T) {
			ctx := context.Background()
			w := newMQMWorld(t)
			r, req := w.ready("fa", "h1", arm.spec)
			req = w.realLedger(req)
			srvA := w.srv
			arm.setup(w, r)

			stale := srvA.mergeQueueMergeOnPass(ctx, req)
			requireOutcome(t, stale, arm.kind, arm.reason)
			if stale.InFlightMarked != arm.postRelease {
				t.Fatalf("A's decision InFlightMarked = %v, want %v", stale.InFlightMarked, arm.postRelease)
			}
			if arm.after != nil {
				arm.after(t, w, r, req.Entry)
			}
			arm.flip(w, r)

			mergerB := &mqmMerger{f: w.mcqFixture, park: make(chan struct{}), entered: make(chan struct{}, 1)}
			cfgB := srvA.cfg
			cfgB.MergeQueue = mergequeue.NewPostgresStore(w.pool)
			cfgB.GateMerger = mergerB
			srvB := New(cfgB)
			srvB.postMergeHeadReadBackoff = srvA.postMergeHeadReadBackoff
			if srvB.mergeInFlight == srvA.mergeInFlight {
				t.Fatal("fixture: Server B shares Server A's in-flight registry")
			}
			if srvB.mergeInFlight.has(r.id) {
				t.Fatal("fixture: Server B's registry holds a mark for the run before its decision")
			}

			reqB := mergeOnPassRequest{Entry: w.active(r), IntentSequence: req.IntentSequence, Ledger: srvB.mergeDispatchLedger()}
			var releaseOnce sync.Once
			releaseB := func() { releaseOnce.Do(func() { close(mergerB.park) }) }
			var outB mergeOnPassOutcome
			bDone := make(chan struct{})
			go func() {
				defer close(bDone)
				outB = srvB.mergeQueueMergeOnPass(ctx, reqB)
			}()
			t.Cleanup(func() {
				releaseB()
				select {
				case <-bDone:
				case <-time.After(timescale.D(30 * time.Second)):
				}
			})
			select {
			case <-mergerB.entered:
			case <-bDone:
				t.Fatalf("fixture: B's decision returned %s/%q (%s) before its pinned merge; want it parked inside MergePullRequestAtHead",
					outB.Kind, outB.Reason, outB.Detail)
			case <-time.After(timescale.D(30 * time.Second)):
				t.Fatal("fixture: B never reached its pinned merge")
			}

			// C2: arms a-c never marked A's registry; arm d's A carries its own
			// mark (the pump clears it only after the apply), and B's registry
			// is a distinct instance that held no mark before B's decision.
			if arm.postRelease {
				if !srvA.mergeInFlight.has(r.id) {
					t.Fatal("arm d: A's own in-flight mark is gone at apply time, want it present until the pump applied the outcome")
				}
			} else if srvA.mergeInFlight.has(r.id) {
				t.Fatal("A's in-flight registry holds a mark for the run at apply time")
			}

			readvances := 0
			if srvA.applyMergeOnPassOutcome(ctx, req.Entry, stale, &readvances) {
				t.Errorf("A's stale %s apply = true, want false (the entry moved past passed)", stale.Kind)
			}
			if readvances != 0 {
				t.Errorf("readvances = %d, want 0", readvances)
			}
			e := w.active(r)
			if e.State != mergequeue.StateLive || e.Phase != mergequeue.PhaseMerging || e.YieldedAt != nil ||
				e.AdmissionEpoch != req.Entry.AdmissionEpoch {
				t.Errorf("entry while B is parked = %s/%s yielded_at=%v epoch %d, want live/merging, not yielded, epoch %d",
					e.State, e.Phase, e.YieldedAt, e.AdmissionEpoch, req.Entry.AdmissionEpoch)
			}
			if n := len(w.rows(r, CategoryMergeQueueYielded)); n != 0 {
				t.Errorf("merge_queue_yielded rows = %d, want 0", n)
			}
			c := w.mqrClaim(r)
			if c.State != mergequeue.DispatchDispatched || c.Path != mergeDispatchPathQueue || c.HeadSHA != "h1" {
				t.Errorf("claim while B is parked = %+v, want B's queue claim dispatched at h1", c)
			}

			releaseB()
			select {
			case <-bDone:
			case <-time.After(timescale.D(30 * time.Second)):
				t.Fatal("B's decision did not return after its park was released")
			}
			requireOutcome(t, outB, mergeOnPassMerged, mergeCandidateQueueReasonMergedByQueue)
			w.requireState(r, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
			if got := mergerB.calls(); len(got) != 1 || got[0] != "h1" {
				t.Fatalf("B's pinned merges = %v, want [h1]", got)
			}
			wantA := 0
			if arm.postRelease {
				wantA = 1 // A's refused 409 PUT
			}
			if got := w.merger.calls(); len(got) != wantA {
				t.Fatalf("A's pinned merges = %v, want %d", got, wantA)
			}
		})
	}
}
