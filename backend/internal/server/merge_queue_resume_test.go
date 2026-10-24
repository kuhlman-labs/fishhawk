package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// ADR-093 D8 (E87.7 / #4298), slice 2: the merging/settling restart resume,
// the boot pass and the pump routing, over the REAL Postgres queue, D1 ledger
// and audit chain (mqmWorld). A RESTART is a fresh *Server built by New from
// the same Config with MergeQueueBootAt = store.DatabaseNow(): an empty
// in-process registry, then its boot pass. Crash-left state is seeded by
// running the real merge-on-pass decision against a 502 merger (verdict,
// authorization, merging stamp and claim all real), then putting the claim
// back to 'dispatched' in SQL with claimed_at backdated against
// clock_timestamp() (#3048): the state a process that died between the claim
// and the outcome classification leaves.

// mqrAged backdates a claim past mergeQueueCrashLeftClaimAge; mqrYoung keeps
// it inside the window (both relative to the DATABASE clock).
const (
	mqrAged  = "clock_timestamp() - interval '170 seconds'"
	mqrYoung = "clock_timestamp() - interval '130 seconds'"
)

// mqrCrashLeft seeds r live in phase merging at h1 with a dispatched QUEUE
// claim claimed at claimedAt (an SQL expression), plus a held run b on the
// same base, and returns b.
func (w *mqmWorld) mqrCrashLeft(r *mcqRun, claimedAt string) *mcqRun {
	w.t.Helper()
	req := w.realLedger(w.live(r, "h1"))
	w.merger.err = errors.New("github: 502 bad gateway")
	requireOutcome(w.t, w.decide(req), mergeOnPassHeld, mergeOnPassReasonTransient)
	w.merger.err = nil
	w.srv.mergeInFlight.clear(r.id) // the process that marked it is gone
	if e := w.storeEntry(r); e.Phase != mergequeue.PhaseMerging {
		w.t.Fatalf("fixture: phase after the 502 = %q, want merging", e.Phase)
	}
	w.exec(`UPDATE merge_dispatch_claims SET state = 'dispatched', failed_at = NULL, failure_cause = NULL, claimed_at = `+claimedAt+` WHERE run_id = $1`, r.id)
	b := w.seed("held-"+r.branch, "b1", []byte(mcvUndelegatedSpecYAML))
	w.mqpHeldIntent(b, "b1", false)
	return b
}

// mqrRestart replaces the world's server with a fresh one built from the same
// Config, its boot instant read from the database clock.
func (w *mqmWorld) mqrRestart() {
	w.t.Helper()
	now, err := w.store.DatabaseNow(context.Background())
	if err != nil {
		w.t.Fatalf("database now: %v", err)
	}
	cfg := w.srv.cfg
	cfg.MergeQueueBootAt = now
	old := w.srv
	w.srv = New(cfg)
	w.srv.postMergeHeadReadBackoff = old.postMergeHeadReadBackoff
}

// mqrBoot runs the fresh server's boot pass and one periodic tick.
func (w *mqmWorld) mqrBoot() {
	w.srv.resumeMergeCandidateQueues(context.Background())
	w.srv.PumpMergeCandidateQueues(context.Background())
}

func (w *mqmWorld) mqrPR(r *mcqRun, merged bool, state string) {
	w.gh.set(func() { w.gh.prs[r.pr].merged, w.gh.prs[r.pr].state = merged, state })
}

// mqrResumed returns r's merge_queue_resumed payloads.
func (w *mqmWorld) mqrResumed(r *mcqRun) []map[string]any {
	var out []map[string]any
	for _, row := range w.rows(r, CategoryMergeQueueResumed) {
		out = append(out, w.payload(row))
	}
	return out
}

func (w *mqmWorld) mqrClaim(r *mcqRun) mergequeue.DispatchClaim {
	w.t.Helper()
	c, found, err := w.store.MergeDispatch(context.Background(), r.id)
	if err != nil || !found {
		w.t.Fatalf("claim: found=%v err=%v", found, err)
	}
	return c
}

func (w *mqmWorld) mqrCount(want string) int {
	n := 0
	for _, h := range w.merger.calls() {
		if h == want {
			n++
		}
	}
	return n
}

// DONE-MEANS 1 (and counterfactual B1): a crash-left merging entry whose PR
// the forge reports merged at the claim's head settles merged through the
// normal merged tail with ZERO merge calls. The state-open arm isolates the
// resume's one re-read (the fake serves merged and state independently), so
// `merged := false` would run the not-merged arm and HOLD the entry.
func TestMergeQueueResume_MergingPRAlreadyMerged_SettlesWithoutMerge(t *testing.T) {
	for _, state := range []string{"closed", "open"} {
		t.Run(state, func(t *testing.T) {
			w := newMQMWorld(t)
			a := w.seed("a", "h1", []byte(mcvUndelegatedSpecYAML))
			w.mqeRebased(a, "pre-h1", "h1", "h1", false)
			w.mqePassed(a, "h1")
			b := w.mqrCrashLeft(a, mqrAged)
			entry := w.active(a)
			before := w.mqrCount("h1")
			w.mqrPR(a, true, state)
			w.mqrRestart()
			w.mqrBoot()
			if got := w.mqrCount("h1"); got != before {
				t.Fatalf("pinned merges at h1 = %d after the restart, want %d (no merge call)", got, before)
			}
			w.requireState(a, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
			rows := w.rows(a, CategoryMergeQueueMerged)
			if len(rows) != 1 {
				t.Fatalf("merge_queue_merged rows = %d, want 1", len(rows))
			}
			p := w.payload(rows[0])
			if p["entry_id"] != entry.ID.String() || p["resumed"] != true || p["intent_sequence"] == float64(0) || p["verdict_sequence"] == float64(0) {
				t.Fatalf("merge_queue_merged = %v, want entry %s, resumed, intent and verdict sequences", p, entry.ID)
			}
			res := w.mqrResumed(a)
			if len(res) != 1 || res[0]["action"] != mergeQueueResumeActionSettleMerged || res[0][mergeQueueMergeActorVerifiedKey] != false {
				t.Fatalf("merge_queue_resumed = %v, want one settle_merged with merge_actor_verified false", res)
			}
			if c := w.mqrClaim(a); c.State != mergequeue.DispatchDispatched {
				t.Fatalf("claim = %+v, want still dispatched (no ledger transition)", c)
			}
			if e := w.storeEntry(b); e.State != mergequeue.StateLive && e.State != mergequeue.StateMerged {
				t.Fatalf("B = %s, want admitted after A settled", e.State)
			}
		})
	}
}

// DONE-MEANS 2, restated per R1 (and C2/C3, counterfactual B2): PR open and
// unmerged at the claim's head, claim aged past the window → zero merge
// calls, the claim moves to dispatch_failed with cause orphaned_by_restart,
// one merge_dispatch_settled {dispatch_failed, orphaned_by_restart} precedes
// one hold row naming the operator re-arm, and B is not admitted. A second
// restart writes nothing. After the documented re-arm the next pump
// re-decides and merges ONCE at h1.
func TestMergeQueueResume_MergingUnmerged_HeldRemedyNamed(t *testing.T) {
	w := newMQMWorld(t)
	a := w.seed("a", "h1", []byte(mcvUndelegatedSpecYAML))
	w.mqeRebased(a, "pre-h1", "h1", "h1", false)
	w.mqePassed(a, "h1")
	b := w.mqrCrashLeft(a, mqrAged)
	settledBefore := len(w.rows(a, categoryMergeDispatchSettled))
	before := w.mqrCount("h1")
	w.mqrRestart()
	w.mqrBoot()
	if got := w.mqrCount("h1"); got != before {
		t.Fatalf("pinned merges = %d, want %d", got, before)
	}
	if e := w.storeEntry(a); e.State != mergequeue.StateLive || e.Phase != mergequeue.PhaseMerging {
		t.Fatalf("A = %s/%s, want live merging (held)", e.State, e.Phase)
	}
	if c := w.mqrClaim(a); c.State != mergequeue.DispatchFailed || c.FailureCause != mergeQueueResumeReasonOrphanedByRestart {
		t.Fatalf("claim = %+v, want dispatch_failed cause %s", c, mergeQueueResumeReasonOrphanedByRestart)
	}
	settled := w.rows(a, categoryMergeDispatchSettled)
	if len(settled) != settledBefore+1 {
		t.Fatalf("merge_dispatch_settled rows = %d, want %d", len(settled), settledBefore+1)
	}
	sp := w.payload(settled[len(settled)-1])
	if sp["outcome"] != string(mergeDispatchUncertain) || sp["reason"] != mergeQueueResumeReasonOrphanedByRestart {
		t.Fatalf("merge_dispatch_settled = %v, want dispatch_failed/orphaned_by_restart", sp)
	}
	res := w.rows(a, CategoryMergeQueueResumed)
	if len(res) != 1 || res[0].Sequence < settled[len(settled)-1].Sequence {
		t.Fatalf("merge_queue_resumed rows = %d, want one after the settled row", len(res))
	}
	hp := w.payload(res[0])
	remedy, _ := hp["remedy"].(string)
	if hp["action"] != mergeQueueResumeActionHold || hp["reason"] != mergeQueueResumeReasonDispatchUncertain ||
		hp["next_step"] != mergeQueueResumeNextOperatorRearm || !strings.Contains(remedy, "NO fishhawk_merge_run") ||
		!strings.Contains(remedy, "dispatch_failed") {
		t.Fatalf("hold row = %v, want hold/merge_dispatch_uncertain/operator_rearm naming the re-arm (C8a)", hp)
	}
	if e := w.storeEntry(b); e.State != mergequeue.StateHeld {
		t.Fatalf("B = %s, want held behind A", e.State)
	}

	w.mqrRestart()
	w.mqrBoot()
	if n := len(w.rows(a, CategoryMergeQueueResumed)); n != 1 {
		t.Fatalf("merge_queue_resumed rows after a second restart = %d, want 1 (hold deduped)", n)
	}
	if n := len(w.rows(a, categoryMergeDispatchSettled)); n != settledBefore+1 {
		t.Fatalf("merge_dispatch_settled rows after a second restart = %d, want %d", n, settledBefore+1)
	}

	// The documented operator re-arm, then the pump re-decides from scratch.
	w.exec(`DELETE FROM merge_dispatch_claims WHERE run_id = $1 AND state = 'dispatch_failed'`, a.id)
	w.srv.PumpMergeCandidateQueues(context.Background())
	if got := w.mqrCount("h1"); got != before+1 {
		t.Fatalf("pinned merges at h1 after the re-arm = %d, want %d (merged once)", got, before+1)
	}
	w.requireState(a, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
}

// C2: a queue claim seeded just INSIDE the crash-left window (relative to the
// database clock) holds with nothing written; the same claim aged past the
// window moves to dispatch_failed.
func TestMergeQueueResume_YoungQueueClaimHolds(t *testing.T) {
	w := newMQMWorld(t)
	a := w.seed("a", "h1", []byte(mcvUndelegatedSpecYAML))
	w.mqeRebased(a, "pre-h1", "h1", "h1", false)
	w.mqePassed(a, "h1")
	w.mqrCrashLeft(a, mqrYoung)
	settledBefore := len(w.rows(a, categoryMergeDispatchSettled))
	w.mqrRestart()
	out := w.srv.mergeQueueResumeMerge(context.Background(), w.active(a))
	requireOutcome(t, out, mergeOnPassHeld, mergeQueueResumeReasonInProgress)
	if c := w.mqrClaim(a); c.State != mergequeue.DispatchDispatched {
		t.Fatalf("young claim = %+v, want still dispatched", c)
	}
	if n := len(w.rows(a, categoryMergeDispatchSettled)); n != settledBefore || len(w.mqrResumed(a)) != 0 {
		t.Fatalf("rows written for a young claim: settled=%d resumed=%d", n, len(w.mqrResumed(a)))
	}
	w.exec(`UPDATE merge_dispatch_claims SET claimed_at = `+mqrAged+` WHERE run_id = $1`, a.id)
	requireOutcome(t, w.srv.mergeQueueResumeMerge(context.Background(), w.active(a)), mergeOnPassHeld, mergeQueueResumeReasonDispatchUncertain)
	if c := w.mqrClaim(a); c.State != mergequeue.DispatchFailed {
		t.Fatalf("aged claim = %+v, want dispatch_failed", c)
	}
}

// C1: an armed INLINE (or delegated) dispatched claim is never transitioned:
// the resume holds with no ledger write and the claim reads back unchanged.
func TestMergeQueueResume_NonQueueClaimUntouched(t *testing.T) {
	for _, path := range []string{mergequeue.DispatchPathInline, mergequeue.DispatchPathDelegated} {
		t.Run(path, func(t *testing.T) {
			w := newMQMWorld(t)
			a := w.seed("a", "h1", []byte(mcvUndelegatedSpecYAML))
			w.mqeRebased(a, "pre-h1", "h1", "h1", false)
			w.mqePassed(a, "h1")
			w.mqrCrashLeft(a, mqrAged)
			w.exec(`UPDATE merge_dispatch_claims SET path = $2 WHERE run_id = $1`, a.id, path)
			want := w.mqrClaim(a)
			settledBefore := len(w.rows(a, categoryMergeDispatchSettled))
			w.mqrRestart()
			requireOutcome(t, w.srv.mergeQueueResumeMerge(context.Background(), w.active(a)), mergeOnPassHeld, mergeOnPassReasonAlreadyDispatched)
			if got := w.mqrClaim(a); got.State != want.State || got.Path != want.Path || got.FailureCause != "" || !got.ClaimedAt.Equal(want.ClaimedAt) {
				t.Fatalf("claim = %+v, want unchanged %+v", got, want)
			}
			if n := len(w.rows(a, categoryMergeDispatchSettled)); n != settledBefore {
				t.Fatalf("merge_dispatch_settled rows = %d, want %d (no ledger write)", n, settledBefore)
			}
		})
	}
}

// TestMergeQueueResume_MergeArms: one row per remaining arm, asserting the
// outcome and the READ-BACK claim/entry state.
func TestMergeQueueResume_MergeArms(t *testing.T) {
	type arm struct {
		name   string
		setup  func(w *mqmWorld, a *mcqRun)
		kind   mergeOnPassKind
		reason string
		check  func(t *testing.T, w *mqmWorld, a *mcqRun)
	}
	claimState := func(want mergequeue.DispatchState) func(t *testing.T, w *mqmWorld, a *mcqRun) {
		return func(t *testing.T, w *mqmWorld, a *mcqRun) {
			if c := w.mqrClaim(a); c.State != want {
				t.Fatalf("claim = %+v, want %s", c, want)
			}
		}
	}
	arms := []arm{
		{name: "merged claim-head mismatch", setup: func(w *mqmWorld, a *mcqRun) {
			w.exec(`UPDATE merge_dispatch_claims SET head_sha = 'h0' WHERE run_id = $1`, a.id)
			w.mqrPR(a, true, "closed")
		}, kind: mergeOnPassHeld, reason: mergeOnPassReasonMergedClaimMismatch, check: claimState(mergequeue.DispatchDispatched)},
		{name: "merged no claim", setup: func(w *mqmWorld, a *mcqRun) {
			w.exec(`DELETE FROM merge_dispatch_claims WHERE run_id = $1`, a.id)
			w.mqrPR(a, true, "closed")
		}, kind: mergeOnPassSettle, reason: mergeCandidateQueueReasonPRMerged, check: func(t *testing.T, w *mqmWorld, a *mcqRun) {
			if n := len(w.rows(a, CategoryMergeQueueMerged)); n != 0 {
				t.Fatalf("merge_queue_merged rows = %d, want 0", n)
			}
		}},
		{name: "merged inline claim at the merged head", setup: func(w *mqmWorld, a *mcqRun) {
			w.exec(`UPDATE merge_dispatch_claims SET path = 'inline' WHERE run_id = $1`, a.id)
			w.mqrPR(a, true, "closed")
		}, kind: mergeOnPassSettle, reason: mergeCandidateQueueReasonPRMerged, check: func(t *testing.T, w *mqmWorld, a *mcqRun) {
			if c := w.mqrClaim(a); c.State != mergequeue.DispatchDispatched || c.Path != mergequeue.DispatchPathInline {
				t.Fatalf("claim = %+v, want the inline claim untouched (C1)", c)
			}
			if res := w.mqrResumed(a); len(res) != 1 || res[0]["action"] != mergeQueueResumeActionSettlePRMerged || res[0]["claim_path"] != mergequeue.DispatchPathInline {
				t.Fatalf("merge_queue_resumed = %v, want one settle_pr_merged naming the inline claim", res)
			}
		}},
		{name: "closed unmerged no claim", setup: func(w *mqmWorld, a *mcqRun) {
			w.exec(`DELETE FROM merge_dispatch_claims WHERE run_id = $1`, a.id)
			w.mqrPR(a, false, "closed")
		}, kind: mergeOnPassSettle, reason: mergeCandidateQueueReasonPRClosed, check: func(t *testing.T, w *mqmWorld, a *mcqRun) {
			if res := w.mqrResumed(a); len(res) != 1 || res[0]["action"] != mergeQueueResumeActionDropPRClosed {
				t.Fatalf("merge_queue_resumed = %v, want one drop_pr_closed", res)
			}
		}},
		{name: "merged dispatch_failed queue claim", setup: func(w *mqmWorld, a *mcqRun) {
			w.exec(`UPDATE merge_dispatch_claims SET state = 'dispatch_failed', failed_at = clock_timestamp(), failure_cause = 'x' WHERE run_id = $1`, a.id)
			w.mqrPR(a, true, "closed")
		}, kind: mergeOnPassMerged, reason: mergeCandidateQueueReasonMergedByQueue, check: func(t *testing.T, w *mqmWorld, a *mcqRun) {
			got := w.settledOutcomes(a)
			if len(got) == 0 || got[len(got)-1] != mergeDispatchReconciledMerged {
				t.Fatalf("merge_dispatch_settled outcomes = %v, want a final reconciled_merged", got)
			}
		}},
		{name: "closed unmerged dispatched claim", setup: func(w *mqmWorld, a *mcqRun) {
			w.mqrPR(a, false, "closed")
		}, kind: mergeOnPassSettle, reason: mergeCandidateQueueReasonPRClosed, check: claimState(mergequeue.DispatchFailed)},
		{name: "dispatch_failed claim open", setup: func(w *mqmWorld, a *mcqRun) {
			w.exec(`UPDATE merge_dispatch_claims SET state = 'dispatch_failed', failed_at = clock_timestamp(), failure_cause = 'x' WHERE run_id = $1`, a.id)
		}, kind: mergeOnPassHeld, reason: mergeOnPassReasonAlreadyDispatched, check: claimState(mergequeue.DispatchFailed)},
		{name: "settling open dispatched", setup: func(w *mqmWorld, a *mcqRun) {
			w.exec(`UPDATE merge_candidate_queue_entries SET phase = 'settling' WHERE run_id = $1 AND state = 'live'`, a.id)
		}, kind: mergeOnPassHeld, reason: mergeQueueResumeReasonSettlingUnconfirmed, check: func(t *testing.T, w *mqmWorld, a *mcqRun) {
			claimState(mergequeue.DispatchDispatched)(t, w, a)
			res := w.mqrResumed(a)
			if len(res) != 1 || !strings.Contains(res[0]["remedy"].(string), "fishhawk_reconcile_merge") ||
				!strings.Contains(res[0]["remedy"].(string), "converg") {
				t.Fatalf("settling hold row = %v, want the C8b operator path", res)
			}
		}},
		{name: "settling closed dispatched", setup: func(w *mqmWorld, a *mcqRun) {
			w.exec(`UPDATE merge_candidate_queue_entries SET phase = 'settling' WHERE run_id = $1 AND state = 'live'`, a.id)
			w.mqrPR(a, false, "closed")
		}, kind: mergeOnPassSettle, reason: mergeCandidateQueueReasonPRClosed, check: claimState(mergequeue.DispatchFailed)},
		{name: "PR read error with a young claim", setup: func(w *mqmWorld, a *mcqRun) {
			w.exec(`UPDATE merge_dispatch_claims SET claimed_at = `+mqrYoung+` WHERE run_id = $1`, a.id)
			w.mu.Lock()
			w.prReadFail = true
			w.mu.Unlock()
		}, kind: mergeOnPassHeld, reason: mergeQueueResumeReasonInProgress, check: claimState(mergequeue.DispatchDispatched)},
		{name: "PR read error, aged claim (indeterminate, C3)", setup: func(w *mqmWorld, a *mcqRun) {
			w.mu.Lock()
			w.prReadFail = true
			w.mu.Unlock()
		}, kind: mergeOnPassHeld, reason: mergeQueueResumeReasonDispatchUncertain, check: claimState(mergequeue.DispatchFailed)},
		{name: "settling PR read error, aged claim (never orphaned)", setup: func(w *mqmWorld, a *mcqRun) {
			w.exec(`UPDATE merge_candidate_queue_entries SET phase = 'settling' WHERE run_id = $1 AND state = 'live'`, a.id)
			w.mu.Lock()
			w.prReadFail = true
			w.mu.Unlock()
		}, kind: mergeOnPassHeld, reason: mergeQueueResumeReasonSettlingUnconfirmed, check: func(t *testing.T, w *mqmWorld, a *mcqRun) {
			claimState(mergequeue.DispatchDispatched)(t, w, a)
			if e := w.storeEntry(a); e.State != mergequeue.StateLive || e.Phase != mergequeue.PhaseSettling {
				t.Fatalf("entry = %s/%s, want live settling", e.State, e.Phase)
			}
			res := w.mqrResumed(a)
			if len(res) != 1 || res[0]["action"] != mergeQueueResumeActionHold || res[0]["reason"] != mergeQueueResumeReasonSettlingUnconfirmed {
				t.Fatalf("merge_queue_resumed = %v, want one settling_unconfirmed hold", res)
			}
		}},
		{name: "PR read error, no claim", setup: func(w *mqmWorld, a *mcqRun) {
			w.exec(`DELETE FROM merge_dispatch_claims WHERE run_id = $1`, a.id)
			w.mu.Lock()
			w.prReadFail = true
			w.mu.Unlock()
		}, kind: mergeOnPassHeld, reason: mergeOnPassReasonTransient, check: func(t *testing.T, w *mqmWorld, a *mcqRun) {
			if e := w.storeEntry(a); e.Phase != mergequeue.PhaseMerging || len(w.mqrResumed(a)) != 0 {
				t.Fatalf("entry %s rows %d, want merging with nothing written", e.Phase, len(w.mqrResumed(a)))
			}
		}},
		{name: "claim read error", setup: func(w *mqmWorld, a *mcqRun) {
			w.srv.cfg.MergeQueue = mqrClaimErrQueue{w.store}
		}, kind: mergeOnPassHeld, reason: mergeOnPassReasonTransient, check: claimState(mergequeue.DispatchDispatched)},
		{name: "entry changed in-lock", setup: func(w *mqmWorld, a *mcqRun) {
			w.exec(`UPDATE merge_candidate_queue_entries SET admission_epoch = admission_epoch + 1 WHERE run_id = $1 AND state = 'live'`, a.id)
		}, kind: mergeOnPassHeld, reason: mergeOnPassReasonEntryChanged, check: claimState(mergequeue.DispatchDispatched)},
		{name: "merge-intent lock unavailable", setup: func(w *mqmWorld, a *mcqRun) {
			w.srv.cfg.MergeQueue = mqrLockErrQueue{w.store}
		}, kind: mergeOnPassHeld, reason: mergeOnPassReasonLockUnavailable, check: claimState(mergequeue.DispatchDispatched)},
	}
	for _, tc := range arms {
		t.Run(tc.name, func(t *testing.T) {
			w := newMQMWorld(t)
			a := w.seed("a", "h1", []byte(mcvUndelegatedSpecYAML))
			w.mqeRebased(a, "pre-h1", "h1", "h1", false)
			w.mqePassed(a, "h1")
			w.mqrCrashLeft(a, mqrAged)
			e := w.active(a)
			before := w.mqrCount("h1")
			w.mqrRestart()
			tc.setup(w, a)
			out := w.srv.mergeQueueResumeMerge(context.Background(), e)
			requireOutcome(t, out, tc.kind, tc.reason)
			if got := w.mqrCount("h1"); got != before {
				t.Fatalf("pinned merges = %d, want %d (the resume never calls the forge merge)", got, before)
			}
			w.srv.cfg.MergeQueue = w.store
			tc.check(t, w, a)
		})
	}
}

// mqrClaimErrQueue fails MergeDispatch; mqrLockErrQueue fails the
// merge-intent lock before fn runs.
type mqrClaimErrQueue struct{ *mergequeue.PostgresStore }

func (mqrClaimErrQueue) MergeDispatch(context.Context, uuid.UUID) (mergequeue.DispatchClaim, bool, error) {
	return mergequeue.DispatchClaim{}, false, errors.New("injected claim read failure")
}

type mqrLockErrQueue struct{ *mergequeue.PostgresStore }

func (mqrLockErrQueue) WithMergeIntentLock(context.Context, uuid.UUID, func(context.Context) error) error {
	return errors.New("injected lock failure")
}

// Counterfactual B12: a merging entry with NO claim is returned to passed and
// ordinary merge-on-pass merges it once at h1.
func TestMergeQueueResume_MergingNoClaimRedecides(t *testing.T) {
	w := newMQMWorld(t)
	a := w.seed("a", "h1", []byte(mcvUndelegatedSpecYAML))
	w.mqeRebased(a, "pre-h1", "h1", "h1", false)
	w.mqePassed(a, "h1")
	w.mqrCrashLeft(a, mqrAged)
	w.exec(`DELETE FROM merge_dispatch_claims WHERE run_id = $1`, a.id)
	before := w.mqrCount("h1")
	w.mqrRestart()
	w.mqrBoot()
	if got := w.mqrCount("h1"); got != before+1 {
		t.Fatalf("pinned merges at h1 = %d, want %d", got, before+1)
	}
	w.requireState(a, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
	res := w.mqrResumed(a)
	if len(res) != 1 || res[0]["action"] != mergeQueueResumeActionRedecide {
		t.Fatalf("merge_queue_resumed = %v, want one redecide", res)
	}
}

// Counterfactual B5 (ruling R4): a merge_queue_merged row naming the entry
// already exists (a crash after the append, before the settle) → the resume
// settles with still exactly one row.
func TestMergeQueueResume_MergedTailDedupsByEntry(t *testing.T) {
	w := newMQMWorld(t)
	a := w.seed("a", "h1", []byte(mcvUndelegatedSpecYAML))
	w.mqeRebased(a, "pre-h1", "h1", "h1", false)
	w.mqePassed(a, "h1")
	w.mqrCrashLeft(a, mqrAged)
	e := w.active(a)
	w.appendSystemRow(a, CategoryMergeQueueMerged, map[string]any{"entry_id": e.ID.String(), "head_sha": "h1"})
	w.mqrPR(a, true, "closed")
	w.mqrRestart()
	w.mqrBoot()
	w.requireState(a, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
	if n := len(w.rows(a, CategoryMergeQueueMerged)); n != 1 {
		t.Fatalf("merge_queue_merged rows = %d, want 1", n)
	}
}

// Ruling R8d (counterfactual B6): every resume append fails closed.
func TestMergeQueueResume_AppendFailuresHoldUnchanged(t *testing.T) {
	cases := []struct {
		name, fail string
		merged     bool
		claim      mergequeue.DispatchState
	}{
		{"merge_queue_resumed", CategoryMergeQueueResumed, true, mergequeue.DispatchDispatched},
		{"merge_dispatch_settled", categoryMergeDispatchSettled, false, mergequeue.DispatchDispatched},
		{"merge_queue_merged", CategoryMergeQueueMerged, true, mergequeue.DispatchDispatched},
		// The hold row after the crash-left transition: the settled row and the
		// ledger move landed first, so the claim reads dispatch_failed.
		{"merge_queue_resumed unmerged", CategoryMergeQueueResumed, false, mergequeue.DispatchFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newMQMWorld(t)
			a := w.seed("a", "h1", []byte(mcvUndelegatedSpecYAML))
			w.mqeRebased(a, "pre-h1", "h1", "h1", false)
			w.mqePassed(a, "h1")
			w.mqrCrashLeft(a, mqrAged)
			if tc.merged {
				w.mqrPR(a, true, "closed")
			}
			w.audit.failCategory = tc.fail
			w.mqrRestart()
			w.mqrBoot()
			if e := w.storeEntry(a); e.State != mergequeue.StateLive || e.Phase != mergequeue.PhaseMerging {
				t.Fatalf("A = %s/%s, want live merging (nothing settled)", e.State, e.Phase)
			}
			if c := w.mqrClaim(a); c.State != tc.claim {
				t.Fatalf("claim = %+v, want %s", c, tc.claim)
			}
			w.audit.failCategory = ""
			if tc.fail == CategoryMergeQueueMerged {
				w.srv.PumpMergeCandidateQueues(context.Background())
				w.requireState(a, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
				if n := len(w.rows(a, CategoryMergeQueueMerged)); n != 1 {
					t.Fatalf("merge_queue_merged rows after recovery = %d, want 1", n)
				}
			}
		})
	}
}

// C6a: a DECLARED approval gate with no merge_queue_authorized row (path
// queue, claim head) holds authorization_unrecorded, never settling merged
// with authorization_sequence 0.
func TestMergeQueueResume_DeclaredGateWithoutAuthorizationHolds(t *testing.T) {
	w := newMQMWorld(t)
	a := w.seed("a", "h1", []byte(mcvUndelegatedSpecYAML))
	w.mqeRebased(a, "pre-h1", "h1", "h1", false)
	w.mqePassed(a, "h1")
	w.mqrCrashLeft(a, mqrAged)
	e := w.active(a)
	// The run's recorded spec now declares the gate (the gate reader is the
	// run row), while the chain carries no queue authorization at h1.
	w.exec(`UPDATE runs SET workflow_spec = $2 WHERE id = $1`, a.id, []byte(mqrDeclaredGateSpec(t, w, a)))
	w.mqrPR(a, true, "closed")
	w.mqrRestart()
	out := w.srv.mergeQueueResumeMerge(context.Background(), e)
	requireOutcome(t, out, mergeOnPassHeld, mergeOnPassReasonAuthorizationUnrecorded)
	if n := len(w.rows(a, CategoryMergeQueueMerged)); n != 0 {
		t.Fatalf("merge_queue_merged rows = %d, want 0", n)
	}
	res := w.mqrResumed(a)
	if len(res) != 1 || res[0]["reason"] != mergeOnPassReasonAuthorizationUnrecorded {
		t.Fatalf("merge_queue_resumed = %v, want a hold authorization_unrecorded", res)
	}
}

// mqrDeclaredGateSpec returns a spec forgeApprovalGateFor reads as DECLARED,
// skipping the test when the repository carries none.
func mqrDeclaredGateSpec(t *testing.T, w *mqmWorld, a *mcqRun) string {
	t.Helper()
	for _, spec := range []string{faeGateSpecYAML} {
		rr := *w.runRow(a)
		rr.WorkflowSpec = []byte(spec)
		if declared, _, err := forgeApprovalGateFor(&rr); err == nil && declared {
			return spec
		}
	}
	t.Fatalf("no declared-gate spec fixture")
	return ""
}

// C4: a late or duplicate verdict at the anchored head leaves a merging (or
// settling) entry alone: no phase reset, no unverified, no eject.
func TestMergeQueueResume_VerdictHookLeavesMergingPhase(t *testing.T) {
	for _, phase := range []mergequeue.Phase{mergequeue.PhaseMerging, mergequeue.PhaseSettling} {
		for _, result := range []string{mergeCandidateResultPassed, mergeCandidateResultNotExecuted, mergeCandidateResultFailed} {
			t.Run(string(phase)+"/"+result, func(t *testing.T) {
				w := newMQMWorld(t)
				a, _ := w.ready("a", "h1", mcvUndelegatedSpecYAML)
				w.exec(`UPDATE merge_candidate_queue_entries SET phase = $2 WHERE run_id = $1 AND state = 'live'`, a.id, string(phase))
				if got := w.srv.mergeCandidateQueueOnVerdict(context.Background(), a.id, "h1", result); got != nil {
					t.Fatalf("hook ejected %+v", got)
				}
				if e := w.storeEntry(a); e.State != mergequeue.StateLive || e.Phase != phase {
					t.Fatalf("entry = %s/%s, want live %s", e.State, e.Phase, phase)
				}
			})
		}
	}
}

// Counterfactual B7: while THIS process has the run's merge in flight the
// pump never resumes the run's merging entry (here: it would otherwise
// settle a PR the forge reports merged).
func TestMergeQueueResume_InFlightRunSkipped(t *testing.T) {
	w := newMQMWorld(t)
	a := w.seed("a", "h1", []byte(mcvUndelegatedSpecYAML))
	w.mqeRebased(a, "pre-h1", "h1", "h1", false)
	w.mqePassed(a, "h1")
	w.mqrCrashLeft(a, mqrAged)
	w.mqrPR(a, true, "closed")
	w.srv.mergeInFlight.mark(a.id)
	w.srv.PumpMergeCandidateQueues(context.Background())
	if e := w.storeEntry(a); e.State != mergequeue.StateLive || len(w.mqrResumed(a)) != 0 {
		t.Fatalf("A = %s with %d resumed rows, want live and untouched while in flight", e.State, len(w.mqrResumed(a)))
	}
	w.srv.mergeInFlight.clear(a.id)
	w.srv.PumpMergeCandidateQueues(context.Background())
	w.requireState(a, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
}

// Counterfactuals B10/B11 (ruling R8a): the boot pass re-claims an advancing
// entry admitted BEFORE the boot instant (epoch +1, a reclaim row, the
// admission re-run), and leaves one THIS server admitted after it alone.
func TestMergeQueueResume_BootReclaimsAdvancing(t *testing.T) {
	w := newMQMWorld(t)
	a := w.seed("a", "h1", []byte(mcvUndelegatedSpecYAML))
	held := w.mqpHeldIntent(a, "h1", false)
	adm, err := w.store.AdmitNext(context.Background(), held.Base())
	if err != nil || !adm.Admitted {
		t.Fatalf("admit: %+v %v", adm, err)
	}
	if _, err := w.store.SetPhase(context.Background(), adm.Entry.ID, adm.Entry.AdmissionEpoch, mergequeue.PhaseAdvancing); err != nil {
		t.Fatalf("set advancing: %v", err)
	}
	w.mqrRestart()
	w.srv.resumeMergeCandidateQueues(context.Background())
	res := w.mqrResumed(a)
	if len(res) == 0 || res[0]["action"] != mergeQueueResumeActionReclaim || res[0]["step"] != string(mergequeue.PhaseAdvancing) {
		t.Fatalf("merge_queue_resumed = %v, want a reclaim at step advancing", res)
	}
	if res[0]["admission_epoch"] != float64(adm.Entry.AdmissionEpoch+1) {
		t.Fatalf("reclaim epoch = %v, want %d", res[0]["admission_epoch"], adm.Entry.AdmissionEpoch+1)
	}
	w.requireState(a, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
}

func TestMergeQueueResume_BootDoesNotReclaimSameProcessAdmission(t *testing.T) {
	w := newMQMWorld(t)
	a := w.seed("a", "h1", []byte(mcvUndelegatedSpecYAML))
	held := w.mqpHeldIntent(a, "h1", false)
	w.mqrRestart() // the boot instant is captured BEFORE this server admits
	adm, err := w.store.AdmitNext(context.Background(), held.Base())
	if err != nil || !adm.Admitted {
		t.Fatalf("admit: %+v %v", adm, err)
	}
	w.srv.resumeMergeCandidateQueues(context.Background())
	e := w.storeEntry(a)
	if e.AdmissionEpoch != adm.Entry.AdmissionEpoch || len(w.mqrResumed(a)) != 0 {
		t.Fatalf("X epoch %d (want %d), %d reclaim rows (want 0)", e.AdmissionEpoch, adm.Entry.AdmissionEpoch, len(w.mqrResumed(a)))
	}
}

// Ruling R7 (counterfactual B9) and approval condition C5: a re-claimed
// admission whose run has a live stage never advances, even with the head
// BEHIND, and holds with no phase write; once the stage settles the next
// periodic tick re-checks the gate and the admission proceeds. A live
// trigger AT the anchored head (no verdict there, so the pass is in flight)
// stamps the entry verifying instead (the gate's in_flight branch). The
// plain AdmitNext stale re-claim (no restart, admitted past
// AdmissionStaleAfter) takes the same gate (adm.Readmitted).
func TestMergeQueueResume_ReclaimNoAdvanceWhileStageLive(t *testing.T) {
	stale := fmt.Sprintf("clock_timestamp() - interval '%d seconds'", int(mergequeue.AdmissionStaleAfter/time.Second)+60)
	arms := []struct {
		name      string
		noVerdict bool // no passed verdict at h1, so a trigger there is in flight
		setup     func(w *mqmWorld, a *mcqRun)
		restart   bool // false: the plain AdmitNext stale re-claim
		phase     mergequeue.Phase
	}{
		{name: "review running", restart: true, phase: mergequeue.PhaseAdmitted, setup: func(w *mqmWorld, a *mcqRun) {
			w.setStage(a.review.ID, run.StageStateRunning)
		}},
		{name: "trigger with implement pending", restart: true, phase: mergequeue.PhaseAdmitted, setup: func(w *mqmWorld, a *mcqRun) {
			w.seedTrigger(a, "x0") // a live trigger for another head
			w.setStage(a.impl.ID, run.StageStatePending)
		}},
		{name: "trigger at the anchored head with implement pending", noVerdict: true, restart: true, phase: mergequeue.PhaseVerifying,
			setup: func(w *mqmWorld, a *mcqRun) {
				w.seedTrigger(a, "h1") // the pass at the anchored head is in flight
				w.setStage(a.impl.ID, run.StageStatePending)
			}},
		{name: "stale re-claim with a dispatched stage", phase: mergequeue.PhaseAdmitted, setup: func(w *mqmWorld, a *mcqRun) {
			w.setStage(a.impl.ID, run.StageStateDispatched)
			w.exec(`UPDATE merge_candidate_queue_entries SET admitted_at = `+stale+` WHERE run_id = $1 AND state = 'live'`, a.id)
		}},
	}
	for _, arm := range arms {
		t.Run(arm.name, func(t *testing.T) {
			w := newMQMWorld(t)
			a := w.seed("a", "h1", []byte(mcvUndelegatedSpecYAML))
			held := w.mqpHeldIntent(a, "h1", arm.noVerdict)
			adm, err := w.store.AdmitNext(context.Background(), held.Base())
			if err != nil || !adm.Admitted {
				t.Fatalf("admit: %+v %v", adm, err)
			}
			w.behind(a, "h1", "m1")
			arm.setup(w, a)
			if arm.restart {
				w.mqrRestart()
				w.mqrBoot()
			} else {
				w.srv.PumpMergeCandidateQueues(context.Background())
			}
			if n := w.mergesInto(a); n != 0 {
				t.Fatalf("advances = %d, want 0 while the run is live", n)
			}
			if n, m := len(w.gh.mergeCalls()), len(w.merger.calls()); n != 0 || m != 0 {
				t.Fatalf("merges-API calls = %d, pinned merges = %d, want 0 while the run is live", n, m)
			}
			e := w.storeEntry(a)
			if e.State != mergequeue.StateLive || e.Phase != arm.phase || e.AdmissionEpoch != adm.Entry.AdmissionEpoch+1 {
				t.Fatalf("entry = %s/%s epoch %d, want live %s at epoch %d", e.State, e.Phase, e.AdmissionEpoch, arm.phase, adm.Entry.AdmissionEpoch+1)
			}
			if arm.name != "review running" {
				return
			}
			w.setStage(a.review.ID, run.StageStateAwaitingApproval)
			w.srv.PumpMergeCandidateQueues(context.Background())
			if n := w.mergesInto(a); n != 1 {
				t.Fatalf("advances after the stage settled = %d, want 1 (C5 re-check)", n)
			}
		})
	}
}

// Ruling R8c (counterfactual B8): the in-flight mark is cleared AFTER the
// pump applied the outcome: at Unadmit time (a 409 readvance) it is still
// set, and after the pump returns it is gone.
func TestMergeQueueMergeInFlight_ClearedAfterOutcomeApplied(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("ifl", "h1", mcvUndelegatedSpecYAML)
	w.merger.err = fmt.Errorf("pinned: %w", forge.ErrMergeConflict)
	var atUnadmit []bool
	fs := &mcqFaultStore{Store: w.store}
	fs.unadmitAt = func(ctx context.Context, id uuid.UUID, epoch int64) (mergequeue.Entry, error) {
		atUnadmit = append(atUnadmit, w.srv.mergeInFlight.has(r.id))
		return w.store.Unadmit(ctx, id, epoch)
	}
	w.srv.cfg.MergeQueue = fs
	w.srv.pumpMergeCandidateQueue(context.Background(), req.Entry.Base())
	if len(atUnadmit) == 0 || !atUnadmit[0] {
		t.Fatalf("in-flight at Unadmit = %v, want true (cleared only after the outcome applied)", atUnadmit)
	}
	if w.srv.mergeInFlight.has(r.id) {
		t.Fatal("in-flight mark survived the pump")
	}
}

// #4298 review 48a65a85: the pump clears only the mark its OWN decision made.
// An overlapping decision on the run (its forge call in flight) holds a mark
// throughout: a pump whose decision held before the merging stamp (the PR
// read fails, so it never marked) leaves that mark set, and a pump whose
// decision marked and merged clears only its own count (the refcount).
// Restoring the unconditional clear reddens the first check; a set in place
// of the refcount reddens the second.
func TestMergeQueueMergeInFlight_OverlappingDecisionKeepsMark(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("ifo", "h1", mcvUndelegatedSpecYAML)
	w.srv.mergeInFlight.mark(r.id) // the overlapping decision, mid forge call
	w.mu.Lock()
	w.prReadFail = true
	w.mu.Unlock()
	w.srv.pumpMergeCandidateQueue(context.Background(), req.Entry.Base())
	if e := w.storeEntry(r); e.State != mergequeue.StateLive || e.Phase != mergequeue.PhasePassed {
		t.Fatalf("fixture: entry = %s/%s, want live passed (the decision held before it marked)", e.State, e.Phase)
	}
	if !w.srv.mergeInFlight.has(r.id) {
		t.Fatal("a decision that never marked cleared the overlapping decision's in-flight mark")
	}
	w.mu.Lock()
	w.prReadFail = false
	w.mu.Unlock()
	w.srv.pumpMergeCandidateQueue(context.Background(), req.Entry.Base())
	w.requireState(r, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
	if !w.srv.mergeInFlight.has(r.id) {
		t.Fatal("a marking decision's clear erased the overlapping decision's in-flight mark (want a refcount)")
	}
	w.srv.mergeInFlight.clear(r.id) // the overlapping decision applies its outcome
	if w.srv.mergeInFlight.has(r.id) {
		t.Fatal("the in-flight mark survived every decision's clear")
	}
}

// The expectation basis of acceptance criterion
// resume-verifying-recovers-pass: a FRESH Server's boot pass and pump route
// a live verifying entry through the pump's verifying/unverified arm.
// (a) the passed hook was lost: the boot pass applies the verdict row and the
// entry merges exactly once. (b) the pass is in flight across the restart:
// no second trigger, and the later recordMergeCandidateVerified(passed)
// merges once. Deleting the pump's verifying/unverified case reddens (a).
func TestMergeQueueResume_VerifyingRecoveredAfterRestart(t *testing.T) {
	t.Run("lost passed hook", func(t *testing.T) {
		w := newMQMWorld(t)
		r, _ := mqrvSeed(w, "a", "a1", mergequeue.PhaseVerifying, true)
		mqrvVerdict(w, r, "a1", mergeCandidateResultPassed, "")
		w.mqrRestart()
		w.mqrBoot()
		if got := w.mqrCount("a1"); got != 1 {
			t.Fatalf("pinned merges at a1 = %d, want 1 (the lost hook applied, merged once)", got)
		}
		w.requireState(r, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
		mqrvRequireActions(w, r, mergeQueueResumeActionApplyVerdict)
		w.srv.PumpMergeCandidateQueues(context.Background())
		if got := w.mqrCount("a1"); got != 1 {
			t.Fatalf("pinned merges at a1 after another tick = %d, want 1", got)
		}
	})
	t.Run("pass in flight across the restart", func(t *testing.T) {
		w := newMQMWorld(t)
		r, _ := mqrvSeed(w, "a", "a1", mergequeue.PhaseVerifying, true)
		w.seedTrigger(r, "a1")
		w.mqrRestart()
		w.mqrBoot()
		if n := len(w.triggers(r)); n != 1 {
			t.Fatalf("triggers after the boot pass = %d, want the one seeded (no second trigger)", n)
		}
		mqrvRequirePhase(w, r, mergequeue.PhaseVerifying)
		if got := w.mqrCount("a1"); got != 0 {
			t.Fatalf("pinned merges at a1 = %d, want 0 while the pass is in flight", got)
		}
		w.verdict(r, mergeCandidateResultPassed)
		w.srv.bgMergeQueuePump.Wait()
		w.srv.PumpMergeCandidateQueues(context.Background())
		if got := w.mqrCount("a1"); got != 1 {
			t.Fatalf("pinned merges at a1 after the verdict = %d, want 1", got)
		}
		w.requireState(r, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
	})
}

// Ruling R4 under concurrent boot: two fresh Servers run their boot pass at
// once on a crash-left entry whose PR the forge reports merged at the queue
// claim's head. The run's merge-intent lock serialises the resumes and the
// in-lock re-read makes the loser hold, so there is exactly ONE
// merge_queue_merged row and ONE settle_merged row, with no merge call.
func TestMergeQueueResume_ConcurrentBootPumpsOneMergedRow(t *testing.T) {
	w := newMQMWorld(t)
	a := w.seed("a", "h1", []byte(mcvUndelegatedSpecYAML))
	w.mqeRebased(a, "pre-h1", "h1", "h1", false)
	w.mqePassed(a, "h1")
	w.mqrCrashLeft(a, mqrAged)
	w.mqrPR(a, true, "closed")
	before := w.mqrCount("h1")
	w.mqrRestart()
	s1 := w.srv
	w.mqrRestart()
	s2 := w.srv
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, s := range []*Server{s1, s2} {
		wg.Add(1)
		go func(s *Server) {
			defer wg.Done()
			<-start
			s.resumeMergeCandidateQueues(context.Background())
		}(s)
	}
	close(start)
	wg.Wait()
	s1.bgMergeQueuePump.Wait()
	s2.bgMergeQueuePump.Wait()
	if got := w.mqrCount("h1"); got != before {
		t.Fatalf("pinned merges at h1 = %d, want %d (no merge call)", got, before)
	}
	w.requireState(a, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
	if n := len(w.rows(a, CategoryMergeQueueMerged)); n != 1 {
		t.Fatalf("merge_queue_merged rows = %d, want 1", n)
	}
	settled := 0
	for _, p := range w.mqrResumed(a) {
		if p["action"] == mergeQueueResumeActionSettleMerged {
			settled++
		}
	}
	if settled != 1 {
		t.Fatalf("settle_merged rows = %d, want 1 (resumed rows %v)", settled, w.mqrResumed(a))
	}
}

// The in-flight registry is nil-safe: a Server built without one never marks
// a run in flight.
func TestMergeInFlightRegistry_NilSafe(t *testing.T) {
	var r *mergeInFlightRegistry
	id := uuid.New()
	r.mark(id)
	r.clear(id)
	if r.has(id) {
		t.Fatal("a nil registry reports a run in flight")
	}
}

// mqrClockErrQueue is a Resumer whose database clock cannot be read.
type mqrClockErrQueue struct{ *mergequeue.PostgresStore }

func (mqrClockErrQueue) DatabaseNow(context.Context) (time.Time, error) {
	return time.Time{}, errors.New("injected clock failure")
}

// mqrPhaseErrQueue fails SetPhase to one phase with err; every other call is
// the real store.
type mqrPhaseErrQueue struct {
	*mergequeue.PostgresStore
	phase mergequeue.Phase
	err   error
}

func (q mqrPhaseErrQueue) SetPhase(ctx context.Context, id uuid.UUID, epoch int64, phase mergequeue.Phase) (mergequeue.Entry, error) {
	if phase == q.phase {
		return mergequeue.Entry{}, q.err
	}
	return q.PostgresStore.SetPhase(ctx, id, epoch, phase)
}

// mqrListErrAudit fails ListForRunByCategory for one category.
type mqrListErrAudit struct {
	audit.Repository
	fail string
}

func (a mqrListErrAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if category == a.fail {
		return nil, errors.New("injected list failure")
	}
	return a.Repository.ListForRunByCategory(ctx, runID, category)
}

// The resume's fail-closed preconditions: an unwired server holds, an
// undecidable claim age is never crash-left, the strict settled append
// refuses with no audit repository, the boot pass pumps nothing when the
// queue or its bases cannot be read, and a nil ledger reconciles trivially.
func TestMergeQueueResume_UnwiredPreconditions(t *testing.T) {
	ctx := context.Background()
	bare := New(Config{Addr: "127.0.0.1:0"})
	requireOutcome(t, bare.mergeQueueResumeMerge(ctx, mergequeue.Entry{RunID: uuid.New()}), mergeOnPassHeld, mergeOnPassReasonTransient)
	if n := bare.resumeMergeCandidateQueues(ctx); n != 0 {
		t.Fatalf("boot pass with no queue pumped %d bases, want 0", n)
	}
	if err := bare.appendMergeDispatchSettledStrict(ctx, mergequeue.DispatchClaim{RunID: uuid.New()}, mergeDispatchReconciledMerged, "x"); err == nil {
		t.Fatal("strict merge_dispatch_settled append with no audit repository returned nil")
	}
	if ok, _, _, err := bare.mergeOnPassReconcileMergedStrict(ctx, nil, uuid.New(), "h1"); !ok || err != nil {
		t.Fatalf("nil ledger reconcile = ok %v err %v, want ok with no error", ok, err)
	}

	basesErr := New(Config{Addr: "127.0.0.1:0", MergeQueue: &mcqFaultStore{
		activeBases: func() ([]mergequeue.Base, error) { return nil, errMCQStore },
	}})
	if n := basesErr.resumeMergeCandidateQueues(ctx); n != 0 {
		t.Fatalf("boot pass with unreadable bases pumped %d, want 0", n)
	}

	claim := mergequeue.DispatchClaim{RunID: uuid.New(), ClaimedAt: time.Unix(0, 0)}
	for name, q := range map[string]mergequeue.Store{
		"not a resumer":    &mcqFaultStore{},
		"clock unreadable": mqrClockErrQueue{},
	} {
		srv := New(Config{Addr: "127.0.0.1:0", MergeQueue: q})
		if aged, why := srv.mergeQueueClaimCrashLeft(ctx, claim); aged || why == "" {
			t.Fatalf("%s: crash-left = %v (%q), want false with a reason", name, aged, why)
		}
	}
}

// A resumed entry whose run is gone drops run_terminal; a run with no
// installation to read its pull request through holds transient.
func TestMergeQueueResume_RunUnreadable(t *testing.T) {
	w := newMQMWorld(t)
	out := w.srv.mergeQueueResumeMerge(context.Background(), mergequeue.Entry{ID: uuid.New(), RunID: uuid.New(), AnchoredHeadSHA: "h1"})
	requireOutcome(t, out, mergeOnPassSettle, mergeCandidateQueueReasonRunTerminal)
	if out.State != mergequeue.StateDropped {
		t.Fatalf("state = %s, want dropped", out.State)
	}

	a := w.seed("a", "h1", []byte(mcvUndelegatedSpecYAML))
	w.mqeRebased(a, "pre-h1", "h1", "h1", false)
	w.mqePassed(a, "h1")
	w.mqrCrashLeft(a, mqrAged)
	w.exec(`UPDATE runs SET installation_id = NULL WHERE id = $1`, a.id)
	w.mqrRestart()
	requireOutcome(t, w.srv.mergeQueueResumeMerge(context.Background(), w.active(a)), mergeOnPassHeld, mergeOnPassReasonTransient)
	if c := w.mqrClaim(a); c.State != mergequeue.DispatchDispatched {
		t.Fatalf("claim = %+v, want untouched", c)
	}
}

// Ruling R8d on the merged tail's reads: an unreadable verdict, intent,
// authorization or merge_queue_merged list holds with nothing settled.
func TestMergeQueueResume_MergedTailReadFailuresHold(t *testing.T) {
	cases := []struct{ category, reason string }{
		{CategoryMergeVerdictRecorded, mergeOnPassReasonTransient},
		{CategoryMergeQueueEntered, mergeOnPassReasonTransient},
		{CategoryMergeQueueAuthorized, mergeOnPassReasonTransient},
		{CategoryMergeQueueMerged, mergeQueueResumeReasonUnrecorded},
	}
	for _, tc := range cases {
		t.Run(tc.category, func(t *testing.T) {
			w := newMQMWorld(t)
			a := w.seed("a", "h1", []byte(mcvUndelegatedSpecYAML))
			w.mqeRebased(a, "pre-h1", "h1", "h1", false)
			w.mqePassed(a, "h1")
			w.mqrCrashLeft(a, mqrAged)
			e := w.active(a)
			w.mqrPR(a, true, "closed")
			w.mqrRestart()
			w.srv.cfg.AuditRepo = mqrListErrAudit{Repository: w.audit, fail: tc.category}
			requireOutcome(t, w.srv.mergeQueueResumeMerge(context.Background(), e), mergeOnPassHeld, tc.reason)
			w.srv.cfg.AuditRepo = w.audit
			if got := w.storeEntry(a); got.State != mergequeue.StateLive {
				t.Fatalf("A = %s, want live (nothing settled)", got.State)
			}
			if n := len(w.rows(a, CategoryMergeQueueMerged)); n != 0 {
				t.Fatalf("merge_queue_merged rows = %d, want 0", n)
			}
		})
	}
}

// Ruling R4 / C6a: under a DECLARED approval gate the merged tail reads its
// authorization back from the newest merge_queue_authorized row whose path is
// queue and whose head is the claim head; a row for another path or head is
// ignored, and the entry settles merged citing the matching row.
func TestMergeQueueResume_DeclaredGateWithAuthorizationSettles(t *testing.T) {
	w := newMQMWorld(t)
	a := w.seed("a", "h1", []byte(mcvUndelegatedSpecYAML))
	w.mqeRebased(a, "pre-h1", "h1", "h1", false)
	w.mqePassed(a, "h1")
	w.mqrCrashLeft(a, mqrAged)
	e := w.active(a)
	w.exec(`UPDATE runs SET workflow_spec = $2 WHERE id = $1`, a.id, []byte(mqrDeclaredGateSpec(t, w, a)))
	w.appendSystemRow(a, CategoryMergeQueueAuthorized, map[string]any{"path": mergeDispatchPathQueue, "head_sha": "h1", "basis": "recorded-basis"})
	auths := w.rows(a, CategoryMergeQueueAuthorized)
	want := auths[len(auths)-1].Sequence
	w.appendSystemRow(a, CategoryMergeQueueAuthorized, map[string]any{"path": mergequeue.DispatchPathInline, "head_sha": "h1", "basis": "inline"})
	w.appendSystemRow(a, CategoryMergeQueueAuthorized, map[string]any{"path": mergeDispatchPathQueue, "head_sha": "h0", "basis": "stale"})
	w.mqrPR(a, true, "closed")
	w.mqrRestart()
	requireOutcome(t, w.srv.mergeQueueResumeMerge(context.Background(), e), mergeOnPassMerged, mergeCandidateQueueReasonMergedByQueue)
	rows := w.rows(a, CategoryMergeQueueMerged)
	if len(rows) != 1 {
		t.Fatalf("merge_queue_merged rows = %d, want 1", len(rows))
	}
	p := w.payload(rows[0])
	if p["authorization_sequence"] != float64(want) || p["approval_basis"] != "recorded-basis" {
		t.Fatalf("merge_queue_merged = %v, want authorization_sequence %d and the queue row's basis", p, want)
	}
}

// ADR-093 D8: a failed advancing-step write ends the admission before the
// behind-probe: a CAS conflict answers stale (another admission owns the
// entry), any other error returns the entry to held; neither advances.
func TestMergeCandidateQueue_AdvancingStepUnrecorded(t *testing.T) {
	t.Run("conflict", func(t *testing.T) {
		f := newMCQFixture(t)
		a, b := f.wave()
		f.freeLine(a)
		runRow, err := f.repo.GetRun(context.Background(), b.id)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		adm, err := f.store.AdmitNext(context.Background(), mergequeue.Base{AccountID: runRow.AccountID, Repo: runRow.Repo, BaseRef: mcqBase})
		if err != nil || !adm.Admitted || adm.Entry.RunID != b.id {
			t.Fatalf("admit B: %+v %v", adm, err)
		}
		merges := f.mergesInto(b)
		f.srv.cfg.MergeQueue = mqrPhaseErrQueue{PostgresStore: f.store, phase: mergequeue.PhaseAdvancing,
			err: fmt.Errorf("injected: %w", mergequeue.ErrStateConflict)}
		res := f.srv.admitMergeCandidateEntry(context.Background(), *adm.Entry)
		if res.outcome != admitStale || res.refusal == nil || !strings.Contains(res.refusal.Reason, "another merge-candidate queue admission owns") {
			t.Fatalf("answer = %+v, want stale naming the owning admission", res)
		}
		if n := f.mergesInto(b) - merges; n != 0 {
			t.Fatalf("advances = %d, want 0", n)
		}
	})
	t.Run("error", func(t *testing.T) {
		f := newMCQFixture(t)
		a, b := f.wave()
		merges := f.mergesInto(b)
		f.srv.cfg.MergeQueue = mqrPhaseErrQueue{PostgresStore: f.store, phase: mergequeue.PhaseAdvancing, err: errMCQStore}
		res := f.handOff(a, b)
		if res.refusal == nil || !strings.Contains(res.refusal.Reason, "the advancing step could not be recorded") {
			t.Fatalf("answer = %+v, want the advancing-step refusal", res)
		}
		f.requireState(b, mergequeue.StateHeld, "")
		if n := f.mergesInto(b) - merges; n != 0 {
			t.Fatalf("advances = %d, want 0", n)
		}
	})
}
