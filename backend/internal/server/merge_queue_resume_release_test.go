package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
)

// ADR-109 Amendment A2 (#4862): the restart resume's orphaned_claim_open arm
// RELEASES a crash-left queue dispatch claim before the entry returns to
// passed, so the claim never reaches the ADR-112 fold. Every test runs over
// the REAL Postgres queue, D1 ledger and audit chain (mqmWorld); a restart is
// a fresh Server (mqrRestart), and every verdict is read back from committed
// state. The store wrappers below embed *mergequeue.PostgresStore so the
// Resumer capability (DatabaseNow) survives the wrap.

// mqrrRecordQueue runs onRelease before ReleaseMergeDispatch and onPhase
// before SetPhase, then the real store.
type mqrrRecordQueue struct {
	*mergequeue.PostgresStore
	onRelease func(mergequeue.DispatchClaim)
	onPhase   func(mergequeue.Phase)
}

func (q mqrrRecordQueue) ReleaseMergeDispatch(ctx context.Context, claim mergequeue.DispatchClaim) error {
	if q.onRelease != nil {
		q.onRelease(claim)
	}
	return q.PostgresStore.ReleaseMergeDispatch(ctx, claim)
}

func (q mqrrRecordQueue) SetPhase(ctx context.Context, id uuid.UUID, epoch int64, phase mergequeue.Phase) (mergequeue.Entry, error) {
	if q.onPhase != nil {
		q.onPhase(phase)
	}
	return q.PostgresStore.SetPhase(ctx, id, epoch, phase)
}

// mqrrClockQueue answers DatabaseNow from now (a pinned clock, or one that
// fails after N reads).
type mqrrClockQueue struct {
	*mergequeue.PostgresStore
	now func() (time.Time, error)
}

func (q mqrrClockQueue) DatabaseNow(context.Context) (time.Time, error) { return q.now() }

// mqrrZeroClaimQueue reads the real claim with claimed_at zeroed.
type mqrrZeroClaimQueue struct{ *mergequeue.PostgresStore }

func (q mqrrZeroClaimQueue) MergeDispatch(ctx context.Context, runID uuid.UUID) (mergequeue.DispatchClaim, bool, error) {
	c, found, err := q.PostgresStore.MergeDispatch(ctx, runID)
	c.ClaimedAt = time.Time{}
	return c, found, err
}

// mqrrLapseQueue runs fn under the real lock, then reports the lock lapsed.
type mqrrLapseQueue struct{ *mergequeue.PostgresStore }

func (q mqrrLapseQueue) WithMergeIntentLock(ctx context.Context, runID uuid.UUID, fn func(context.Context) error) error {
	if err := q.PostgresStore.WithMergeIntentLock(ctx, runID, fn); err != nil {
		return err
	}
	return mergequeue.ErrIntentLockLapsed
}

// mqrrLog is a goroutine-safe log sink.
type mqrrLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *mqrrLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *mqrrLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// mqrrSeed seeds run a on spec with alice's approval at h1, crash-left in
// phase merging with a dispatched queue claim claimed at age and its PR open
// and unmerged, plus a held run b on the same base.
func mqrrSeed(t *testing.T, spec, age string) (*mqmWorld, *mcqRun, *mcqRun) {
	t.Helper()
	w := newMQMWorld(t)
	a := w.seed("a", "h1", []byte(spec))
	w.mqeRebased(a, "pre-h1", "h1", "h1", false)
	w.mqePassed(a, "h1")
	w.forgeReviews(a, []string{"alice"}, mqmReview(1, "alice", "APPROVED", "h1"))
	b := w.mqrCrashLeft(a, age)
	return w, a, b
}

// mqrrActions counts a's merge_queue_resumed rows with action (and reason,
// when non-empty).
func (w *mqmWorld) mqrrActions(a *mcqRun, action, reason string) int {
	n := 0
	for _, p := range w.mqrResumed(a) {
		if p["action"] == action && (reason == "" || p["reason"] == reason) {
			n++
		}
	}
	return n
}

// mqrrResume runs one direct resume of a's live entry.
func (w *mqmWorld) mqrrResume(a *mcqRun) mergeOnPassOutcome {
	return w.srv.mergeQueueResumeMerge(context.Background(), w.active(a))
}

// mqrrRequirePhase asserts a's live entry reads phase.
func (w *mqmWorld) mqrrRequirePhase(a *mcqRun, phase mergequeue.Phase) {
	w.t.Helper()
	if e := w.storeEntry(a); e.State != mergequeue.StateLive || e.Phase != phase {
		w.t.Fatalf("A = %s/%s, want live %s", e.State, e.Phase, phase)
	}
}

// mqrrRequireAdmitted asserts b was admitted after A yielded.
func (w *mqmWorld) mqrrRequireAdmitted(b *mcqRun) {
	w.t.Helper()
	if e := w.storeEntry(b); e.State != mergequeue.StateLive && e.State != mergequeue.StateMerged {
		w.t.Fatalf("B = %s, want admitted after A yielded", e.State)
	}
}

// mqrrUnapprovedYield restarts over a crash-left, now-unapproved A: the
// resume releases the claim, the fold waits (approval-wait anchor set, no
// claim, no PUT, B not admitted), and past the window the next pump yields A
// forge_approval_absent with B admitted.
func mqrrUnapprovedYield(t *testing.T) (*mqmWorld, *mcqRun, *mcqRun) {
	t.Helper()
	w, a, b := mqrrSeed(t, mqeSpecYAML, mqrAged)
	w.forgeReviews(a, nil)
	before := w.mqrCount("h1")
	w.mqrRestart()
	w.mqrBoot()
	if n := w.mqmReleasedRows(a); n != 1 {
		t.Fatalf("released rows = %d, want 1 (the resume releases the orphan)", n)
	}
	w.requireClaim(a, "")
	if e := w.active(a); e.State != mergequeue.StateLive || e.Phase != mergequeue.PhasePassed || e.ApprovalWaitStartedAt == nil {
		t.Fatalf("A = %s/%s approval_wait=%v, want live passed with the approval wait open", e.State, e.Phase, e.ApprovalWaitStartedAt)
	}
	if got := w.mqrCount("h1"); got != before {
		t.Fatalf("pinned merges = %d, want %d before approval", got, before)
	}
	if e := w.storeEntry(b); e.State != mergequeue.StateHeld {
		t.Fatalf("B = %s, want held while A waits at the head", e.State)
	}
	w.backdateApprovalWait(a)
	w.srv.PumpMergeCandidateQueues(context.Background())
	w.requireYielded(a, mergeCandidateQueueReasonForgeApprovalAbsent, forgeApprovalNextApprovePR, mergeQueueYieldSiteMergeOnPass)
	w.requireClaim(a, "")
	w.mqrrRequireAdmitted(b)
	if got := w.mqrCount("h1"); got != before {
		t.Fatalf("pinned merges at h1 = %d, want %d (A never merged)", got, before)
	}
	return w, a, b
}

// A2 point 2: an unapproved fold after the release waits, then yields with
// NO claim row standing, and the line continues. CF-RELEASE (the arm answers
// true without releasing): the fold's expiry yield still lands
// (yieldMergeQueueEntry never reads the claim), so the absent-claim
// assertion goes RED.
func TestMergeQueueResumeRelease_UnapprovedFoldWaitsThenYieldsUnclaimed(t *testing.T) {
	mqrrUnapprovedYield(t)
}

// A2 point 3: a red required check at h1 yields merge_checks_red in the same
// pump, with no hold, and B is admitted. CF-RELEASE: the immediate yield's
// pre-yield refusal sees the claim and holds merge_already_dispatched with
// no marker, RED.
func TestMergeQueueResumeRelease_RedChecksYield(t *testing.T) {
	w, a, b := mqrrSeed(t, mcvUndelegatedSpecYAML, mqrAged)
	w.mqmRequireCI(a)
	w.gh.set(func() { w.gh.checks["h1"] = mqmRedCI })
	w.mqrRestart()
	w.mqrBoot()
	w.requireYielded(a, mergeQueueYieldMergeChecksRed, mergeQueueYieldNextRetriggerCI, mergeQueueYieldSiteMergeOnPass)
	w.requireClaim(a, "")
	if n := w.mqmReleasedRows(a); n != 1 {
		t.Fatalf("released rows = %d, want 1", n)
	}
	w.mqrrRequireAdmitted(b)
}

// A2 point 3, the other expiries: each reaches mergeOnPassPreYieldRefusal
// with no claim standing and yields. CF-RELEASE: each row holds
// merge_already_dispatched instead, RED per row.
func TestMergeQueueResumeRelease_OtherExpiriesYield(t *testing.T) {
	cases := []struct {
		name   string
		spec   string
		pre    func(w *mqmWorld, a *mcqRun) // before the crash-left seed's restart
		post   func(w *mqmWorld, a *mcqRun) // after the restart and boot
		reason string
		next   string
	}{
		{name: "base_moved readvance past an expired approval wait", spec: mqeSpecYAML,
			pre: func(w *mqmWorld, a *mcqRun) { w.forgeReviews(a, nil) },
			post: func(w *mqmWorld, a *mcqRun) {
				w.gh.set(func() { w.gh.behind["h1"] = []string{"base-x"} })
				w.backdateApprovalWait(a)
			}, reason: mergeCandidateQueueReasonForgeApprovalAbsent, next: forgeApprovalNextApprovePR},
		{name: "transient hold past an expired approval wait", spec: mqeSpecYAML,
			pre: func(w *mqmWorld, a *mcqRun) { w.forgeReviews(a, nil) },
			post: func(w *mqmWorld, a *mcqRun) {
				// The base re-probe (a read the resume's open arm never
				// makes) fails: a transient hold carrying its expiry yield.
				w.gh.set(func() { w.gh.compareStatus["h1"] = http.StatusInternalServerError })
				w.backdateApprovalWait(a)
			}, reason: mergeCandidateQueueReasonForgeApprovalUndetermined, next: mergeQueueYieldNextStep(mergeCandidateQueueReasonForgeApprovalUndetermined)},
		{name: "pending-check gate window past expiry", spec: mcvUndelegatedSpecYAML,
			pre: func(w *mqmWorld, a *mcqRun) { w.gh.set(func() { w.gh.checks["h1"] = mqmPendingCI }) },
			post: func(w *mqmWorld, a *mcqRun) {
				w.exec(`UPDATE merge_candidate_queue_entries SET gate_wait_started_at = gate_wait_started_at - interval '2 hours' WHERE run_id = $1 AND state = 'live'`, a.id)
			}, reason: mergeQueueYieldMergeChecksPending, next: forgeApprovalNextRetryAfter},
		{name: "review-gate window past expiry", spec: mqeGatedReviewersSpec,
			pre: func(w *mqmWorld, a *mcqRun) {
				// reviewPending (TestAdmission_A33Table's closure): a fresh
				// implement-review round started, not yet terminal.
				w.mqeAppend(a, "implement_review_started", map[string]any{"configured_agents": 1})
			},
			post: func(w *mqmWorld, a *mcqRun) {
				w.exec(`UPDATE merge_candidate_queue_entries SET gate_wait_started_at = gate_wait_started_at - interval '2 hours' WHERE run_id = $1 AND state = 'live'`, a.id)
			}, reason: mergeQueueYieldMergeReviewPending, next: forgeApprovalNextRetryAfter},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newMQMWorld(t)
			a := w.seed("a", "h1", []byte(tc.spec))
			w.mqeRebased(a, "pre-h1", "h1", "h1", false)
			w.mqePassed(a, "h1")
			w.forgeReviews(a, []string{"alice"}, mqmReview(1, "alice", "APPROVED", "h1"))
			if tc.spec == mqeGatedReviewersSpec {
				// A settled round so the crash-left decision reaches its claim.
				w.mqeAppend(a, "implement_review_started", map[string]any{"configured_agents": 1})
				w.mqeAppend(a, "implement_reviewed", map[string]any{})
			}
			b := w.mqrCrashLeft(a, mqrAged)
			tc.pre(w, a)
			before := w.mqrCount("h1")
			w.mqrRestart()
			w.mqrBoot()
			if n := w.mqmReleasedRows(a); n != 1 {
				t.Fatalf("released rows = %d, want 1", n)
			}
			w.mqrrRequirePhase(a, mergequeue.PhasePassed)
			tc.post(w, a)
			w.srv.PumpMergeCandidateQueues(context.Background())
			w.requireYielded(a, tc.reason, tc.next, mergeQueueYieldSiteMergeOnPass)
			w.requireClaim(a, "")
			w.mqrrRequireAdmitted(b)
			if got := w.mqrCount("h1"); got != before {
				t.Fatalf("pinned merges at h1 = %d, want %d", got, before)
			}
		})
	}
}

// A2 point 3: after the release and the expiry yield, the operator's
// withdraw succeeds through the real handler. CF-RELEASE: the entry still
// yields, but withdrawIntentTx finds the claim and answers 409
// merge_dispatch_claimed, RED.
func TestMergeQueueResumeRelease_WithdrawIntentSucceeds(t *testing.T) {
	w, a, _ := mqrrUnapprovedYield(t)
	rec := mqwWithdraw(w.srv, a.id.String(), mqwBody("operator withdraw after the A2 release"), &promoteOperator)
	if resp := decodeMQWithdraw(t, rec); !resp.Withdrawn {
		t.Fatalf("withdraw = %+v, want withdrawn", resp)
	}
	w.requireState(a, mergequeue.StateDropped, mergequeue.ReasonIntentWithdrawn)
	if n := len(w.rows(a, CategoryMergeQueueIntentWithdrawn)); n != 1 {
		t.Fatalf("merge_queue_intent_withdrawn rows = %d, want 1", n)
	}
}

// A2's order: resumed row < settled row on the chain; at the DELETE both rows
// exist and the phase is still merging; at SetPhase(passed) the claim row is
// gone. Mutation A (SetPhase before the release call): at SetPhase the claim
// still stands, RED. Mutation B (the helper's append and DELETE swapped): at
// the DELETE the settled row is absent, RED.
func TestMergeQueueResumeRelease_Order(t *testing.T) {
	w, a, _ := mqrrSeed(t, mcvUndelegatedSpecYAML, mqrAged)
	w.mqrRestart()
	var (
		releaseCalls, phaseCalls      int
		atReleaseResumed, atReleaseSt int
		atReleasePhase                mergequeue.Phase
		atPhaseClaim                  bool
	)
	w.srv.cfg.MergeQueue = mqrrRecordQueue{PostgresStore: w.store,
		onRelease: func(mergequeue.DispatchClaim) {
			releaseCalls++
			atReleaseResumed = w.mqrrActions(a, mergeQueueResumeActionRedecide, mergeQueueResumeReasonOrphanedClaimOpen)
			atReleaseSt = w.mqmReleasedRows(a)
			atReleasePhase = w.storeEntry(a).Phase
		},
		onPhase: func(p mergequeue.Phase) {
			if p != mergequeue.PhasePassed {
				return
			}
			phaseCalls++
			_, found, err := w.store.MergeDispatch(context.Background(), a.id)
			atPhaseClaim = found || err != nil
		},
	}
	requireOutcome(t, w.mqrrResume(a), mergeOnPassRedecide, mergeQueueResumeReasonOrphanedClaimOpen)
	if releaseCalls != 1 || phaseCalls != 1 {
		t.Fatalf("release calls %d, SetPhase(passed) calls %d, want 1 and 1", releaseCalls, phaseCalls)
	}
	if atReleaseResumed != 1 || atReleaseSt != 1 || atReleasePhase != mergequeue.PhaseMerging {
		t.Fatalf("at the DELETE: resumed rows %d, released rows %d, phase %s; want 1, 1, merging (record before action)",
			atReleaseResumed, atReleaseSt, atReleasePhase)
	}
	if atPhaseClaim {
		t.Fatal("at SetPhase(passed) the claim row still stands; the release must precede the phase write")
	}
	resumed, settled := w.rows(a, CategoryMergeQueueResumed), w.rows(a, categoryMergeDispatchSettled)
	if len(resumed) != 1 || len(settled) == 0 || resumed[0].Sequence >= settled[len(settled)-1].Sequence {
		t.Fatalf("resumed %d rows, settled %d rows: want the resumed row before the released row", len(resumed), len(settled))
	}
	w.requireClaim(a, "")
	w.mqrrRequirePhase(a, mergequeue.PhasePassed)
}

// A2's append-failure rows: either append fails closed with
// resume_unrecorded, the claim read back unchanged and phase merging.
// Mutation (settled row): pass mergeOnPassReasonOrphanReleaseFailed for
// unrecordedReason → the hold reason differs, RED. Mutation (redecide row):
// ignore record's false → the claim is released with the row absent, RED.
func TestMergeQueueResumeRelease_AppendFailuresHoldUnrecorded(t *testing.T) {
	for _, tc := range []struct {
		fail    string
		resumed int
	}{
		{CategoryMergeQueueResumed, 0},
		{categoryMergeDispatchSettled, 1},
	} {
		t.Run(tc.fail, func(t *testing.T) {
			w, a, _ := mqrrSeed(t, mcvUndelegatedSpecYAML, mqrAged)
			seed := w.mqrClaim(a)
			released := w.mqmReleasedRows(a)
			w.mqrRestart()
			w.audit.failCategory = tc.fail
			requireOutcome(t, w.mqrrResume(a), mergeOnPassHeld, mergeQueueResumeReasonUnrecorded)
			w.audit.failCategory = ""
			w.mqmRequireClaimUnchanged(a, seed)
			w.mqrrRequirePhase(a, mergequeue.PhaseMerging)
			if n := len(w.mqrResumed(a)); n != tc.resumed {
				t.Fatalf("merge_queue_resumed rows = %d, want %d", n, tc.resumed)
			}
			if n := w.mqmReleasedRows(a); n != released {
				t.Fatalf("released rows = %d, want %d", n, released)
			}
		})
	}
}

// A2's DELETE-failure rows: a transport error, a real CAS miss (claimed_at
// changed, still aged) and a real CAS miss (the claim deleted) each hold
// orphaned_claim_release_failed in phase merging with the released row
// recorded FIRST; the next resume on the real store re-judges: the first two
// release again (a second released row), the third takes redecide
// no_dispatch_claim. Mutation: ignore the DELETE error in the helper → the
// arm proceeds and the phase moves to passed, RED.
func TestMergeQueueResumeRelease_DeleteFailureHoldsAndReJudges(t *testing.T) {
	for _, tc := range []struct {
		name     string
		queue    func(w *mqmWorld, a *mcqRun) mergequeue.Store
		next     string
		released int
	}{
		{"transport error", func(w *mqmWorld, _ *mcqRun) mergequeue.Store { return mqmReleaseErrQueue{w.store} },
			mergeQueueResumeReasonOrphanedClaimOpen, 2},
		{"CAS miss, claimed_at changed but aged", func(w *mqmWorld, a *mcqRun) mergequeue.Store {
			return mqrrRecordQueue{PostgresStore: w.store, onRelease: func(mergequeue.DispatchClaim) {
				w.exec(`UPDATE merge_dispatch_claims SET claimed_at = claimed_at - interval '1 second' WHERE run_id = $1`, a.id)
			}}
		}, mergeQueueResumeReasonOrphanedClaimOpen, 2},
		{"CAS miss, claim deleted", func(w *mqmWorld, a *mcqRun) mergequeue.Store {
			return mqrrRecordQueue{PostgresStore: w.store, onRelease: func(mergequeue.DispatchClaim) {
				w.exec(`DELETE FROM merge_dispatch_claims WHERE run_id = $1`, a.id)
			}}
		}, mergeQueueResumeReasonNoClaim, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, a, _ := mqrrSeed(t, mcvUndelegatedSpecYAML, mqrAged)
			w.mqrRestart()
			w.srv.cfg.MergeQueue = tc.queue(w, a)
			requireOutcome(t, w.mqrrResume(a), mergeOnPassHeld, mergeOnPassReasonOrphanReleaseFailed)
			w.srv.cfg.MergeQueue = w.store
			w.mqrrRequirePhase(a, mergequeue.PhaseMerging)
			if n := w.mqmReleasedRows(a); n != 1 {
				t.Fatalf("released rows = %d, want 1 (recorded before the failed DELETE)", n)
			}
			requireOutcome(t, w.mqrrResume(a), mergeOnPassRedecide, tc.next)
			w.requireClaim(a, "")
			w.mqrrRequirePhase(a, mergequeue.PhasePassed)
			if n := w.mqmReleasedRows(a); n != tc.released {
				t.Fatalf("released rows after the re-judgement = %d, want %d", n, tc.released)
			}
		})
	}
}

// A2's crash after the release, before the phase write: SetPhase(passed)
// fails once (a plain error) → held entry_changed with the claim gone and
// phase merging. With the real store restored BEFORE the restart (approval
// condition C3), the next resume takes the no-claim arm: redecide
// no_dispatch_claim, back to passed, no second released row. Mutation:
// remove the no-claim arm's SetPhase → the entry stays merging, RED.
func TestMergeQueueResumeRelease_CrashBeforePhaseWriteTakesNoClaimArm(t *testing.T) {
	w, a, _ := mqrrSeed(t, mcvUndelegatedSpecYAML, mqrAged)
	w.mqrRestart()
	w.srv.cfg.MergeQueue = mqrPhaseErrQueue{PostgresStore: w.store, phase: mergequeue.PhasePassed, err: errors.New("injected phase failure")}
	requireOutcome(t, w.mqrrResume(a), mergeOnPassHeld, mergeOnPassReasonEntryChanged)
	w.requireClaim(a, "")
	w.mqrrRequirePhase(a, mergequeue.PhaseMerging)
	w.srv.cfg.MergeQueue = w.store
	w.mqrRestart()
	if _, ok := w.srv.cfg.MergeQueue.(*mergequeue.PostgresStore); !ok {
		t.Fatalf("the restarted server's queue is %T, want the real store", w.srv.cfg.MergeQueue)
	}
	requireOutcome(t, w.mqrrResume(a), mergeOnPassRedecide, mergeQueueResumeReasonNoClaim)
	w.requireClaim(a, "")
	w.mqrrRequirePhase(a, mergequeue.PhasePassed)
	if n := w.mqmReleasedRows(a); n != 1 {
		t.Fatalf("released rows = %d, want 1 (no second release)", n)
	}
	if n := w.mqrrActions(a, mergeQueueResumeActionRedecide, mergeQueueResumeReasonNoClaim); n != 1 {
		t.Fatalf("redecide no_dispatch_claim rows = %d, want 1", n)
	}
}

// A2: a phase-write CAS conflict after the release holds entry_changed, not
// redecide. Mutation: ignore the SetPhase error → redecide, RED.
func TestMergeQueueResumeRelease_PhaseConflictHolds(t *testing.T) {
	w, a, _ := mqrrSeed(t, mcvUndelegatedSpecYAML, mqrAged)
	w.mqrRestart()
	w.srv.cfg.MergeQueue = mqrPhaseErrQueue{PostgresStore: w.store, phase: mergequeue.PhasePassed, err: mergequeue.ErrStateConflict}
	requireOutcome(t, w.mqrrResume(a), mergeOnPassHeld, mergeOnPassReasonEntryChanged)
	w.srv.cfg.MergeQueue = w.store
	w.requireClaim(a, "")
	w.mqrrRequirePhase(a, mergequeue.PhaseMerging)
}

// The helper's third state at the resume: the database clock answers the
// arm's judgement, then fails on the helper's re-judgement, so the helper
// returns (false, "", ""). The arm holds merge_dispatch_in_progress with one
// redecide row, zero released rows, the claim unchanged and phase merging.
// Mutation (`if false && !released`): the arm falls through to SetPhase and
// the entry reads passed with the claim standing, RED.
func TestMergeQueueResumeRelease_ClockLostOnReJudgeHolds(t *testing.T) {
	w, a, _ := mqrrSeed(t, mcvUndelegatedSpecYAML, mqrAged)
	seed := w.mqrClaim(a)
	w.mqrRestart()
	var mu sync.Mutex
	reads := 0
	w.srv.cfg.MergeQueue = mqrrClockQueue{PostgresStore: w.store, now: func() (time.Time, error) {
		mu.Lock()
		defer mu.Unlock()
		reads++
		if reads > 1 {
			return time.Time{}, errors.New("injected clock failure on the re-judgement")
		}
		return w.store.DatabaseNow(context.Background())
	}}
	out := w.mqrrResume(a)
	requireOutcome(t, out, mergeOnPassHeld, mergeQueueResumeReasonInProgress)
	if !strings.Contains(out.Detail, "could not be re-confirmed") {
		t.Fatalf("detail = %q, want it to name the unconfirmed re-judgement", out.Detail)
	}
	w.srv.cfg.MergeQueue = w.store
	if n := w.mqrrActions(a, mergeQueueResumeActionRedecide, mergeQueueResumeReasonOrphanedClaimOpen); n != 1 {
		t.Fatalf("redecide rows = %d, want 1", n)
	}
	if n := w.mqmReleasedRows(a); n != 0 {
		t.Fatalf("released rows = %d, want 0", n)
	}
	w.mqmRequireClaimUnchanged(a, seed)
	w.mqrrRequirePhase(a, mergequeue.PhaseMerging)
}

// The age boundary on a pinned database clock: claimed_at +
// mergeQueueCrashLeftClaimAge exactly is NOT crash-left (hold, nothing
// written); one microsecond later it is (released). It also pins the bound
// that makes the PR read before the judgement safe. Mutation
// (`claim.ClaimedAt.After(...)` for `!claim.ClaimedAt.Before(...)`): the
// boundary claim counts as crash-left and is released, RED.
func TestMergeQueueResumeRelease_ExactAgeBoundaryHolds(t *testing.T) {
	if mergeOnPassForgeCallTimeout >= mergeQueueCrashLeftClaimMargin {
		t.Fatalf("mergeOnPassForgeCallTimeout %s must stay under mergeQueueCrashLeftClaimMargin %s: the resume reads the PR before judging the claim's age",
			mergeOnPassForgeCallTimeout, mergeQueueCrashLeftClaimMargin)
	}
	w, a, _ := mqrrSeed(t, mcvUndelegatedSpecYAML, mqrAged)
	seed := w.mqrClaim(a)
	w.mqrRestart()
	pinned := seed.ClaimedAt.Add(mergeQueueCrashLeftClaimAge)
	w.srv.cfg.MergeQueue = mqrrClockQueue{PostgresStore: w.store, now: func() (time.Time, error) { return pinned, nil }}
	requireOutcome(t, w.mqrrResume(a), mergeOnPassHeld, mergeQueueResumeReasonInProgress)
	if n := len(w.mqrResumed(a)); n != 0 || w.mqmReleasedRows(a) != 0 {
		t.Fatalf("rows at the exact boundary: resumed %d released %d, want none", n, w.mqmReleasedRows(a))
	}
	w.mqmRequireClaimUnchanged(a, seed)
	pinned = pinned.Add(time.Microsecond)
	w.srv.cfg.MergeQueue = mqrrClockQueue{PostgresStore: w.store, now: func() (time.Time, error) { return pinned, nil }}
	requireOutcome(t, w.mqrrResume(a), mergeOnPassRedecide, mergeQueueResumeReasonOrphanedClaimOpen)
	w.srv.cfg.MergeQueue = w.store
	w.requireClaim(a, "")
	if n := w.mqmReleasedRows(a); n != 1 {
		t.Fatalf("released rows one microsecond past the boundary = %d, want 1", n)
	}
}

// A2's exclusions: each row releases nothing (zero released rows, the claim
// unchanged where it should stand), with its isolating mutation named on the
// row.
func TestMergeQueueResumeRelease_Exclusions(t *testing.T) {
	type tcase struct {
		name  string
		age   string
		setup func(w *mqmWorld, a *mcqRun) mergequeue.Entry // returns the snapshot to resume
		kind  mergeOnPassKind
		// reason is the outcome reason; redecides is the number of
		// redecide/orphaned_claim_open rows the arm must write (always 0).
		reason string
		claim  mergequeue.DispatchState // the claim's state after the resume
	}
	active := func(w *mqmWorld, a *mcqRun) mergequeue.Entry { return w.active(a) }
	cases := []tcase{
		// CF: delete the arm's `!aged` return → a redecide row, RED.
		{name: "young claim", age: mqrYoung, setup: active, kind: mergeOnPassHeld, reason: mergeQueueResumeReasonInProgress, claim: mergequeue.DispatchDispatched},
		// CF: make the IsZero branch unreachable → a row and orphaned_claim_release_failed, RED.
		{name: "zero claimed_at", age: mqrAged, setup: func(w *mqmWorld, a *mcqRun) mergequeue.Entry {
			w.srv.cfg.MergeQueue = mqrrZeroClaimQueue{w.store}
			return w.active(a)
		}, kind: mergeOnPassHeld, reason: mergeQueueResumeReasonInProgress, claim: mergequeue.DispatchDispatched},
		// CF: delete the arm's `!aged` return → a redecide row, RED.
		{name: "unreadable clock", age: mqrAged, setup: func(w *mqmWorld, a *mcqRun) mergequeue.Entry {
			w.srv.cfg.MergeQueue = mqrClockErrQueue{w.store}
			return w.active(a)
		}, kind: mergeOnPassHeld, reason: mergeQueueResumeReasonInProgress, claim: mergequeue.DispatchDispatched},
		// CF: delete the epoch compare in mergeQueueResumeLiveEntry → released, RED.
		{name: "stale epoch", age: mqrAged, setup: func(w *mqmWorld, a *mcqRun) mergequeue.Entry {
			e := w.active(a)
			w.exec(`UPDATE merge_candidate_queue_entries SET admission_epoch = admission_epoch + 1 WHERE run_id = $1 AND state = 'live'`, a.id)
			return e
		}, kind: mergeOnPassHeld, reason: mergeOnPassReasonEntryChanged, claim: mergequeue.DispatchDispatched},
		// CF: delete the state compare in mergeQueueResumeLiveEntry → released, RED.
		{name: "snapshot names a non-live entry", age: mqrAged, setup: func(w *mqmWorld, a *mcqRun) mergequeue.Entry {
			e := w.active(a)
			w.exec(`UPDATE merge_candidate_queue_entries SET state = 'held' WHERE id = $1`, e.ID)
			return e
		}, kind: mergeOnPassHeld, reason: mergeOnPassReasonEntryChanged, claim: mergequeue.DispatchDispatched},
		// CF: delete the settling+open arm → released, RED.
		{name: "settling", age: mqrAged, setup: func(w *mqmWorld, a *mcqRun) mergequeue.Entry {
			w.exec(`UPDATE merge_candidate_queue_entries SET phase = 'settling' WHERE run_id = $1 AND state = 'live'`, a.id)
			return w.active(a)
		}, kind: mergeOnPassHeld, reason: mergeQueueResumeReasonSettlingUnconfirmed, claim: mergequeue.DispatchDispatched},
		// CF: drop the path condition from queueDispatched → the helper's
		// guard answers false, so the arm holds merge_dispatch_in_progress
		// with a redecide row, RED on reason and row count.
		{name: "inline claim", age: mqrAged, setup: func(w *mqmWorld, a *mcqRun) mergequeue.Entry {
			w.exec(`UPDATE merge_dispatch_claims SET path = 'inline' WHERE run_id = $1`, a.id)
			return w.active(a)
		}, kind: mergeOnPassHeld, reason: mergeOnPassReasonAlreadyDispatched, claim: mergequeue.DispatchDispatched},
		{name: "delegated claim", age: mqrAged, setup: func(w *mqmWorld, a *mcqRun) mergequeue.Entry {
			w.exec(`UPDATE merge_dispatch_claims SET path = 'delegated' WHERE run_id = $1`, a.id)
			return w.active(a)
		}, kind: mergeOnPassHeld, reason: mergeOnPassReasonAlreadyDispatched, claim: mergequeue.DispatchDispatched},
		// CF: drop the state condition from queueDispatched → same mechanism, RED.
		{name: "dispatch_failed claim", age: mqrAged, setup: func(w *mqmWorld, a *mcqRun) mergequeue.Entry {
			w.exec(`UPDATE merge_dispatch_claims SET state = 'dispatch_failed', failed_at = clock_timestamp(), failure_cause = 'x' WHERE run_id = $1`, a.id)
			return w.active(a)
		}, kind: mergeOnPassHeld, reason: mergeOnPassReasonAlreadyDispatched, claim: mergequeue.DispatchFailed},
		// CF: delete mergeQueueResumeForgeTarget's installation check → the PR
		// read proceeds and the claim is released, RED.
		{name: "run with no installation (GitLab family)", age: mqrAged, setup: func(w *mqmWorld, a *mcqRun) mergequeue.Entry {
			w.exec(`UPDATE runs SET installation_id = NULL WHERE id = $1`, a.id)
			return w.active(a)
		}, kind: mergeOnPassHeld, reason: mergeOnPassReasonTransient, claim: mergequeue.DispatchDispatched},
		// The same guard's zero-id half: with the check deleted the PR read
		// proceeds (the fake token provider ignores the id) and the claim is
		// released, RED on the assertions rather than a nil dereference.
		{name: "run with installation id 0", age: mqrAged, setup: func(w *mqmWorld, a *mcqRun) mergequeue.Entry {
			w.exec(`UPDATE runs SET installation_id = 0 WHERE id = $1`, a.id)
			return w.active(a)
		}, kind: mergeOnPassHeld, reason: mergeOnPassReasonTransient, claim: mergequeue.DispatchDispatched},
		// CF: route the closed arm to mergeQueueResumeOrphanedOpen → released, RED.
		{name: "closed PR", age: mqrAged, setup: func(w *mqmWorld, a *mcqRun) mergequeue.Entry {
			w.mqrPR(a, false, "closed")
			return w.active(a)
		}, kind: mergeOnPassSettle, reason: mergeCandidateQueueReasonPRClosed, claim: mergequeue.DispatchFailed},
		// CF: route the indeterminate arm to mergeQueueResumeOrphanedOpen → released, RED.
		{name: "indeterminate PR read", age: mqrAged, setup: func(w *mqmWorld, a *mcqRun) mergequeue.Entry {
			w.mu.Lock()
			w.prReadFail = true
			w.mu.Unlock()
			return w.active(a)
		}, kind: mergeOnPassHeld, reason: mergeQueueResumeReasonDispatchUncertain, claim: mergequeue.DispatchFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, a, _ := mqrrSeed(t, mcvUndelegatedSpecYAML, tc.age)
			seed := w.mqrClaim(a)
			w.mqrRestart()
			e := tc.setup(w, a)
			if tc.name == "inline claim" || tc.name == "delegated claim" || tc.name == "dispatch_failed claim" {
				seed = w.mqrClaim(a)
			}
			out := w.srv.mergeQueueResumeMerge(context.Background(), e)
			w.srv.cfg.MergeQueue = w.store
			w.mu.Lock()
			w.prReadFail = false
			w.mu.Unlock()
			requireOutcome(t, out, tc.kind, tc.reason)
			if n := w.mqmReleasedRows(a); n != 0 {
				t.Fatalf("released rows = %d, want 0", n)
			}
			if n := w.mqrrActions(a, mergeQueueResumeActionRedecide, ""); n != 0 {
				t.Fatalf("redecide rows = %d, want 0", n)
			}
			c := w.mqrClaim(a)
			if c.State != tc.claim {
				t.Fatalf("claim = %+v, want state %s", c, tc.claim)
			}
			if tc.claim == mergequeue.DispatchDispatched {
				w.mqmRequireClaimUnchanged(a, seed)
			} else if tc.reason != mergeOnPassReasonAlreadyDispatched && c.FailureCause != mergeQueueResumeReasonOrphanedByRestart {
				t.Fatalf("claim = %+v, want dispatch_failed orphaned_by_restart", c)
			}
		})
	}
}

// This process's own merge in flight: the pump never resumes the run, so the
// claim stands untouched; once cleared, the next pump's resume releases it
// and the ordinary path merges. Mutation (`if false &&
// s.mergeInFlight.has(e.RunID)`): the resume releases during the in-flight
// window, RED.
func TestMergeQueueResumeRelease_InFlightRunSkipped(t *testing.T) {
	w, a, _ := mqrrSeed(t, mcvUndelegatedSpecYAML, mqrAged)
	seed := w.mqrClaim(a)
	w.srv.mergeInFlight.mark(a.id)
	w.srv.PumpMergeCandidateQueues(context.Background())
	if n := len(w.mqrResumed(a)); n != 0 || w.mqmReleasedRows(a) != 0 {
		t.Fatalf("rows while in flight: resumed %d released %d, want none", n, w.mqmReleasedRows(a))
	}
	w.mqmRequireClaimUnchanged(a, seed)
	w.srv.mergeInFlight.clear(a.id)
	w.srv.PumpMergeCandidateQueues(context.Background())
	if n := w.mqmReleasedRows(a); n != 1 {
		t.Fatalf("released rows after the clear = %d, want 1", n)
	}
	w.requireState(a, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
}

// A second resume with the stale merging snapshot after the first release
// holds entry_changed with still one released row. Mutation (PhasePassed
// added to the mergeQueueResumeLiveEntry call): the second resume takes the
// no-claim arm and answers redecide, RED.
func TestMergeQueueResumeRelease_SecondResumeHoldsEntryChanged(t *testing.T) {
	w, a, _ := mqrrSeed(t, mcvUndelegatedSpecYAML, mqrAged)
	w.mqrRestart()
	stale := w.active(a)
	requireOutcome(t, w.srv.mergeQueueResumeMerge(context.Background(), stale), mergeOnPassRedecide, mergeQueueResumeReasonOrphanedClaimOpen)
	requireOutcome(t, w.srv.mergeQueueResumeMerge(context.Background(), stale), mergeOnPassHeld, mergeOnPassReasonEntryChanged)
	if n := w.mqmReleasedRows(a); n != 1 {
		t.Fatalf("released rows = %d, want 1", n)
	}
	if n := len(w.mqrResumed(a)); n != 1 {
		t.Fatalf("merge_queue_resumed rows = %d, want 1", n)
	}
}

// A2's residual: a late PUT from the crashed process merges the PR after the
// release; the next pump settles merged pr_merged with zero PUTs and zero
// merge_queue_merged rows. Mutation (mergeOnPassReconcileMergedStrict's
// `!found` branch holds): with no claim the merged PR stays held, RED.
func TestMergeQueueResumeRelease_LateMergeSettlesPRMergedNoPut(t *testing.T) {
	w, a, _ := mqrrSeed(t, mcvUndelegatedSpecYAML, mqrAged)
	w.mqrRestart()
	requireOutcome(t, w.mqrrResume(a), mergeOnPassRedecide, mergeQueueResumeReasonOrphanedClaimOpen)
	w.requireClaim(a, "")
	w.mqrPR(a, true, "closed")
	before := w.mqrCount("h1")
	w.srv.PumpMergeCandidateQueues(context.Background())
	w.requireState(a, mergequeue.StateMerged, mergeCandidateQueueReasonPRMerged)
	if got := w.mqrCount("h1"); got != before {
		t.Fatalf("pinned merges = %d, want %d (no PUT)", got, before)
	}
	if n := len(w.rows(a, CategoryMergeQueueMerged)); n != 0 {
		t.Fatalf("merge_queue_merged rows = %d, want 0", n)
	}
}

// Step 8's backstop: a YIELDED (non-merging) entry carrying an aged
// dispatched queue claim left before A2 is approved, cleared by the re-probe
// and re-admitted; its merge-on-pass releases the orphan in step 8 AFTER the
// fresh merge_queue_authorized row and merges once. Mutation (the step-8
// helper call answers false, "", ""): the decision holds
// merge_already_dispatched with zero PUTs, RED.
func TestMergeQueueResumeRelease_Step8BackstopOnYieldedPreA2Orphan(t *testing.T) {
	w, a, _ := newYieldWorld(t)
	ctx := context.Background()
	if _, err := w.store.ClaimMergeDispatch(ctx, a.id, mergeDispatchPathQueue, "a1"); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	w.exec(`UPDATE merge_dispatch_claims SET claimed_at = `+mqrAged+` WHERE run_id = $1`, a.id)
	authBefore := len(w.rows(a, CategoryMergeQueueAuthorized))
	w.forgeReviews(a, []string{"alice"}, mqmReview(2, "alice", "APPROVED", "a1"))
	if !w.srv.ReprobeMergeQueueYield(ctx, a.id, false) {
		t.Fatal("the targeted re-probe did not clear an approved yield")
	}
	w.srv.bgMergeQueuePump.Wait()
	w.requirePuts("a1")
	w.requireState(a, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
	auth := w.rows(a, CategoryMergeQueueAuthorized)
	if len(auth) != authBefore+1 {
		t.Fatalf("merge_queue_authorized rows = %d, want %d", len(auth), authBefore+1)
	}
	var releasedSeq []int64
	for _, row := range w.rows(a, categoryMergeDispatchSettled) {
		if p := w.payload(row); p["outcome"] == string(mergeDispatchRelease) && p["reason"] == mergeDispatchReasonReleasedOrphanedClaim {
			releasedSeq = append(releasedSeq, row.Sequence)
		}
	}
	if len(releasedSeq) != 1 || releasedSeq[0] <= auth[len(auth)-1].Sequence {
		t.Fatalf("released rows at %v, want one after the fresh merge_queue_authorized row %d (step 8)", releasedSeq, auth[len(auth)-1].Sequence)
	}
}

// The resume's lock-lapse WARN (approval condition C5): a lock that runs fn
// and then lapses leaves the outcome unchanged (redecide, the claim
// released) and logs the WARN with run_id; a no-lapse control logs no such
// line. Mutation (`if false && lockErr != nil`): the lapse produces no line,
// RED.
func TestMergeQueueResumeRelease_LockLapseWarns(t *testing.T) {
	const msg = "merge queue: the restart resume's merge-intent lock lapsed after its judgement ran; its writes stand"
	for _, lapse := range []bool{true, false} {
		name := "no lapse"
		if lapse {
			name = "lapse"
		}
		t.Run(name, func(t *testing.T) {
			w, a, _ := mqrrSeed(t, mcvUndelegatedSpecYAML, mqrAged)
			w.mqrRestart()
			sink := &mqrrLog{}
			w.srv.cfg.Logger = slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
			if lapse {
				w.srv.cfg.MergeQueue = mqrrLapseQueue{w.store}
			}
			requireOutcome(t, w.mqrrResume(a), mergeOnPassRedecide, mergeQueueResumeReasonOrphanedClaimOpen)
			w.srv.cfg.MergeQueue = w.store
			w.requireClaim(a, "")
			if n := w.mqmReleasedRows(a); n != 1 {
				t.Fatalf("released rows = %d, want 1", n)
			}
			var line string
			for _, l := range strings.Split(sink.String(), "\n") {
				if strings.Contains(l, msg) {
					line = l
				}
			}
			switch {
			case lapse && (line == "" || !strings.Contains(line, "level=WARN") || !strings.Contains(line, "run_id="+a.id.String()) ||
				!strings.Contains(line, "outcome="+string(mergeOnPassRedecide))):
				t.Fatalf("log = %q, want a WARN %q naming run_id %s and outcome redecide", sink.String(), msg, a.id)
			case !lapse && line != "":
				t.Fatalf("log line %q without a lapse, want none", line)
			}
		})
	}
}
