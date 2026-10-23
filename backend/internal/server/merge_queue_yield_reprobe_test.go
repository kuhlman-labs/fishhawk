package server

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// ADR-112 R3/R5 (#4632, slice 3): the yield re-probe and its surfacing, over
// the real Postgres queue, the real audit repository and the mcq fake forge
// on the non-Fishhawk fixture repository x/y (ADR-110).

// TestYieldReprobeDue_Table pins the stateless schedule: nothing before one
// tick, then the doubling points interval·{1,2,4,8,16} (each due on exactly
// the tick that crosses it), then every 16 ticks.
func TestYieldReprobeDue_Table(t *testing.T) {
	const i = time.Minute
	for _, tc := range []struct {
		age  time.Duration
		want bool
	}{
		{0, false}, {59 * time.Second, false},
		{i, true}, {i + 59*time.Second, true}, {2 * i, true}, {2*i + 30*time.Second, true},
		{3 * i, false}, {3*i + 59*time.Second, false}, {4 * i, true}, {5 * i, false},
		{8 * i, true}, {9 * i, false}, {15 * i, false}, {16 * i, true}, {17 * i, false},
		{31 * i, false}, {32 * i, true}, {32*i + 59*time.Second, true}, {33 * i, false}, {48 * i, true},
	} {
		if got := yieldReprobeDue(tc.age, i); got != tc.want {
			t.Errorf("yieldReprobeDue(%s, 1m) = %v, want %v", tc.age, got, tc.want)
		}
	}
	if yieldReprobeDue(time.Hour, 0) {
		t.Error("a non-positive interval is due")
	}
}

// yieldDismissed drives r (held with intent at a1, its only review DISMISSED)
// through a wait and, past the window, a yield, and returns the yielded entry.
func (w *mqmWorld) yieldDismissed(r *mcqRun) mergequeue.Entry {
	w.t.Helper()
	w.forgeReviews(r, []string{"alice"}, mqmReview(1, "alice", "DISMISSED", "a1"))
	w.pumpBase(r)
	w.backdateApprovalWait(r)
	w.pumpBase(r)
	return w.requireYielded(r, mergeCandidateQueueReasonForgeApprovalDismissed, forgeApprovalNextApprovePR, mergeQueueYieldSiteMergeOnPass)
}

func newYieldWorld(t *testing.T) (*mqmWorld, *mcqRun, mergequeue.Entry) {
	t.Helper()
	w := newMQELineWorld(t)
	a := w.seed("a", "a1", []byte(mqeSpecYAML))
	w.mqpHeldIntent(a, "a1", false)
	return w, a, w.yieldDismissed(a)
}

// TestMergeQueueYieldReprobe_ApprovedClearsAndTheLineMerges: a qualifying
// approval at the anchored head clears the marker through the CAS, records
// merge_queue_yield_cleared naming the episode, and the detached pump admits
// the row and merges it pinned to its head.
func TestMergeQueueYieldReprobe_ApprovedClearsAndTheLineMerges(t *testing.T) {
	w, a, y := newYieldWorld(t)
	w.forgeReviews(a, []string{"alice"}, mqmReview(2, "alice", "APPROVED", "a1"))

	if !w.srv.ReprobeMergeQueueYield(context.Background(), a.id) {
		t.Fatal("the targeted re-probe did not clear an approved yield")
	}
	w.srv.bgMergeQueuePump.Wait()

	w.requirePuts("a1")
	w.requireState(a, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
	rows := w.rows(a, CategoryMergeQueueYieldCleared)
	if len(rows) != 1 {
		t.Fatalf("merge_queue_yield_cleared rows = %d, want 1", len(rows))
	}
	_, yieldedAt := mergeQueueYieldEpisode(y)
	if p := w.payload(rows[0]); p["entry_id"] != y.ID.String() || p["yielded_at"] != yieldedAt ||
		p["yield_reason"] != mergeCandidateQueueReasonForgeApprovalDismissed || p["head_sha"] != "a1" {
		t.Fatalf("merge_queue_yield_cleared payload = %v", p)
	}
}

// TestMergeQueueYieldReprobe_SameReasonWritesNothing: an unchanged
// non-approved fold is deduplicated: no row, the marker untouched.
func TestMergeQueueYieldReprobe_SameReasonWritesNothing(t *testing.T) {
	w, a, y := newYieldWorld(t)

	if got := w.srv.reprobeMergeQueueYield(context.Background(), w.active(a)); got != reprobeUnchanged {
		t.Fatalf("outcome = %s, want %s", got, reprobeUnchanged)
	}
	if n := len(w.rows(a, CategoryMergeQueueYielded)); n != 1 {
		t.Fatalf("merge_queue_yielded rows = %d, want 1 (dedup)", n)
	}
	if e := w.active(a); !e.YieldedAt.Equal(*y.YieldedAt) || e.YieldReason != y.YieldReason {
		t.Fatalf("marker = %v/%s, want unchanged %v/%s", e.YieldedAt, e.YieldReason, y.YieldedAt, y.YieldReason)
	}
}

// TestMergeQueueYieldReprobe_ChangedReasonTransitions: a write-level veto
// replaces the dismissal, so UpdateYield rewrites the stored reason (same
// episode) and ONE merge_queue_yielded transition row names the prior reason.
func TestMergeQueueYieldReprobe_ChangedReasonTransitions(t *testing.T) {
	w, a, y := newYieldWorld(t)
	w.forgeReviews(a, []string{"bob"}, mqmReview(2, "bob", "CHANGES_REQUESTED", "a1"))

	if got := w.srv.reprobeMergeQueueYield(context.Background(), w.active(a)); got != reprobeTransitioned {
		t.Fatalf("outcome = %s, want %s", got, reprobeTransitioned)
	}
	e := w.active(a)
	if e.YieldReason != mergeCandidateQueueReasonForgeApprovalVetoed || !e.YieldedAt.Equal(*y.YieldedAt) || e.State != mergequeue.StateHeld {
		t.Fatalf("entry = %s %s yielded_at %v, want held vetoed in the same episode %v", e.State, e.YieldReason, e.YieldedAt, y.YieldedAt)
	}
	rows := w.rows(a, CategoryMergeQueueYielded)
	if len(rows) != 2 {
		t.Fatalf("merge_queue_yielded rows = %d, want 2 (the yield and one transition)", len(rows))
	}
	if p := w.payload(rows[1]); p["transition_from"] != mergeCandidateQueueReasonForgeApprovalDismissed ||
		p["yield_reason"] != mergeCandidateQueueReasonForgeApprovalVetoed || p["next_step"] != forgeApprovalNextDismissReview ||
		p["site"] != mergeQueueYieldSiteReprobe {
		t.Fatalf("transition payload = %v", p)
	}
}

// TestMergeQueueYieldReprobe_HeadMovedReanchorsKeepingTheMarker pins approval
// condition C2: the PR head moves after the yield, so the re-probe RE-ANCHORS
// the yielded row to the new head and KEEPS the marker, yielded_at and the
// wait anchor (a head move never clears); once the new head carries a
// qualifying approval the next re-probe clears it and the row is admitted
// again. Counterfactual: dropping the head-moved re-anchor leaves the row
// anchored at a1 and marked after the approval at a3, RED.
func TestMergeQueueYieldReprobe_HeadMovedReanchorsKeepingTheMarker(t *testing.T) {
	w, a, y := newYieldWorld(t)
	w.gh.set(func() { w.gh.prs[a.pr].head = "a3" })

	w.srv.reprobeMergeQueueYield(context.Background(), w.active(a))

	e := w.active(a)
	if e.AnchoredHeadSHA != "a3" || e.YieldedAt == nil || !e.YieldedAt.Equal(*y.YieldedAt) ||
		e.ApprovalWaitStartedAt == nil || !e.ApprovalWaitStartedAt.Equal(*y.ApprovalWaitStartedAt) {
		t.Fatalf("after the head move = head %s yielded_at %v wait %v, want a3 with the episode kept (%v, %v)",
			e.AnchoredHeadSHA, e.YieldedAt, e.ApprovalWaitStartedAt, y.YieldedAt, y.ApprovalWaitStartedAt)
	}

	admitted := len(w.rows(a, CategoryMergeCandidateQueueAdmitted))
	w.forgeReviews(a, []string{"alice"}, mqmReview(2, "alice", "APPROVED", "a3"))
	if !w.srv.ReprobeMergeQueueYield(context.Background(), a.id) {
		t.Fatal("an approval at the re-anchored head did not clear the marker")
	}
	w.srv.bgMergeQueuePump.Wait()
	if n := len(w.rows(a, CategoryMergeCandidateQueueAdmitted)); n != admitted+1 {
		t.Fatalf("admissions after the clear = %d, want %d (the cleared row rejoins the line)", n, admitted+1)
	}
	if p := w.payload(w.rows(a, CategoryMergeQueueYieldCleared)[0]); p["head_sha"] != "a3" {
		t.Fatalf("cleared at head %v, want a3", p["head_sha"])
	}
}

// TestMergeQueueYieldReprobe_NothingBranches: a stale episode (the CAS
// conflict), a transport failure, a merged PR, a terminal run and an entry
// that is not yielded each write nothing and leave the marker in place.
func TestMergeQueueYieldReprobe_NothingBranches(t *testing.T) {
	ctx := context.Background()
	t.Run("stale episode", func(t *testing.T) {
		w, a, y := newYieldWorld(t)
		w.forgeReviews(a, []string{"alice"}, mqmReview(2, "alice", "APPROVED", "a1"))
		stale := y
		old := y.YieldedAt.Add(-time.Hour)
		stale.YieldedAt = &old
		if got := w.srv.reprobeMergeQueueYield(ctx, stale); got != reprobeNothing {
			t.Fatalf("outcome = %s, want %s", got, reprobeNothing)
		}
		if e := w.active(a); e.YieldedAt == nil || len(w.rows(a, CategoryMergeQueueYieldCleared)) != 0 {
			t.Fatalf("a stale-episode probe cleared the marker: %+v", e)
		}
	})
	t.Run("transport failure", func(t *testing.T) {
		w, a, _ := newYieldWorld(t)
		w.forgeReviews(a, []string{"alice"}, mqmReview(2, "alice", "APPROVED", "a1"))
		w.gh.set(func() { w.gh.graphQLFail[a.pr] = 10 })
		if got := w.srv.reprobeMergeQueueYield(ctx, w.active(a)); got != reprobeNothing || w.active(a).YieldedAt == nil {
			t.Fatalf("outcome = %s, want %s with the marker kept", got, reprobeNothing)
		}
	})
	t.Run("merged", func(t *testing.T) {
		w, a, _ := newYieldWorld(t)
		w.gh.set(func() { w.gh.prs[a.pr].state, w.gh.prs[a.pr].merged = "closed", true })
		if got := w.srv.reprobeMergeQueueYield(ctx, w.active(a)); got != reprobeNothing || w.active(a).YieldedAt == nil {
			t.Fatalf("outcome = %s, want %s with the marker kept", got, reprobeNothing)
		}
	})
	t.Run("terminal run", func(t *testing.T) {
		w, a, y := newYieldWorld(t)
		w.forgeReviews(a, []string{"alice"}, mqmReview(2, "alice", "APPROVED", "a1"))
		w.exec(`UPDATE runs SET state = 'cancelled' WHERE id = $1`, a.id)
		if got := w.srv.reprobeMergeQueueYield(ctx, y); got != reprobeNothing {
			t.Fatalf("outcome = %s, want %s", got, reprobeNothing)
		}
	})
	t.Run("not yielded", func(t *testing.T) {
		w := newMQELineWorld(t)
		a := w.seed("a", "a1", []byte(mqeSpecYAML))
		w.mqpHeldIntent(a, "a1", false)
		if w.srv.ReprobeMergeQueueYield(ctx, a.id) || w.srv.ReprobeMergeQueueYield(ctx, uuid.New()) {
			t.Fatal("an unmarked entry or an unknown run reported a clear")
		}
	})
}

// TestMergeQueueYieldReprobe_ClearAppendFailureKeepsTheClear pins approval
// condition C7 on the clear: the ClearYield CAS committed, so a failed
// merge_queue_yield_cleared append is logged at ERROR naming entry_id and
// yielded_at and the row is NOT re-marked.
func TestMergeQueueYieldReprobe_ClearAppendFailureKeepsTheClear(t *testing.T) {
	w, a, y := newYieldWorld(t)
	var logs bytes.Buffer
	w.srv.cfg.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	w.audit.failCategory = CategoryMergeQueueYieldCleared
	w.forgeReviews(a, []string{"alice"}, mqmReview(2, "alice", "APPROVED", "a1"))

	if !w.srv.ReprobeMergeQueueYield(context.Background(), a.id) {
		t.Fatal("the clear did not land")
	}
	w.srv.bgMergeQueuePump.Wait()
	var marked bool
	if err := w.pool.QueryRow(context.Background(), `SELECT yielded_at IS NOT NULL FROM merge_candidate_queue_entries WHERE id = $1`, y.ID).Scan(&marked); err != nil || marked {
		t.Fatalf("entry re-marked after a failed append (marked=%v, err=%v)", marked, err)
	}
	_, yieldedAt := mergeQueueYieldEpisode(y)
	out := logs.String()
	for _, want := range []string{"level=ERROR", "merge_queue_yield_cleared could not be recorded", "entry_id=" + y.ID.String(), "yielded_at=" + yieldedAt} {
		if !strings.Contains(out, want) {
			t.Errorf("log does not carry %q:\n%s", want, out)
		}
	}
}

// TestMergeQueueYieldSurface_HealsAFailedYieldedAppend pins approval
// condition C7 on the yield: merge_queue_yielded fails to append after the
// Yield CAS committed (the marker stands, logged at ERROR), and the next sweep
// re-appends exactly one row for that (entry_id, yielded_at). Counterfactual:
// deleting the heal leaves zero rows, RED.
func TestMergeQueueYieldSurface_HealsAFailedYieldedAppend(t *testing.T) {
	w := newMQELineWorld(t)
	a := w.seed("a", "a1", []byte(mqeSpecYAML))
	w.mqpHeldIntent(a, "a1", false)
	w.forgeReviews(a, []string{"alice"}, mqmReview(1, "alice", "DISMISSED", "a1"))
	w.pumpBase(a)
	w.backdateApprovalWait(a)
	w.audit.failCategory = CategoryMergeQueueYielded
	w.pumpBase(a)
	e := w.active(a)
	if e.YieldedAt == nil || len(w.rows(a, CategoryMergeQueueYielded)) != 0 {
		t.Fatalf("precondition: yielded_at %v rows %d, want marked with no yielded row", e.YieldedAt, len(w.rows(a, CategoryMergeQueueYielded)))
	}
	w.audit.failCategory = ""

	time.Sleep(timescale.D(20 * time.Millisecond))
	w.srv.sweepMergeQueueYields(context.Background(), time.Millisecond)
	w.srv.sweepMergeQueueYields(context.Background(), time.Millisecond)

	rows := w.rows(a, CategoryMergeQueueYielded)
	if len(rows) != 1 {
		t.Fatalf("merge_queue_yielded rows after two sweeps = %d, want exactly 1 (healed once)", len(rows))
	}
	entryID, yieldedAt := mergeQueueYieldEpisode(e)
	if p := w.payload(rows[0]); p["entry_id"] != entryID || p["yielded_at"] != yieldedAt || p["site"] != mergeQueueYieldSiteHealed ||
		p["yield_reason"] != mergeCandidateQueueReasonForgeApprovalDismissed || p["next_step"] != forgeApprovalNextApprovePR {
		t.Fatalf("healed payload = %v", p)
	}
}

// TestMergeQueueYieldSurface_PostedOncePerEpisode: a yield older than one
// tick is posted exactly once across two sweeps, naming the episode with the
// sanitised detail; a FRESH yield (younger than the tick) is not posted.
// Counterfactuals: dropping the dedup scan posts twice; dropping the age check
// posts the fresh yield; both RED.
func TestMergeQueueYieldSurface_PostedOncePerEpisode(t *testing.T) {
	ctx := context.Background()
	w, a, y := newYieldWorld(t)

	w.srv.sweepMergeQueueYields(ctx, time.Hour)
	if n := len(w.rows(a, CategoryMergeQueueYieldPosted)); n != 0 {
		t.Fatalf("a fresh yield posted %d rows, want 0", n)
	}

	time.Sleep(timescale.D(20 * time.Millisecond))
	w.srv.sweepMergeQueueYields(ctx, time.Millisecond)
	w.srv.sweepMergeQueueYields(ctx, time.Millisecond)

	rows := w.rows(a, CategoryMergeQueueYieldPosted)
	if len(rows) != 1 {
		t.Fatalf("merge_queue_yield_posted rows = %d, want exactly 1 per episode", len(rows))
	}
	entryID, yieldedAt := mergeQueueYieldEpisode(y)
	if p := w.payload(rows[0]); p["entry_id"] != entryID || p["yielded_at"] != yieldedAt ||
		p["yield_reason"] != mergeCandidateQueueReasonForgeApprovalDismissed || p["next_step"] != forgeApprovalNextApprovePR ||
		p["yield_detail"] != mergeQueueYieldDetail(y.YieldDetail) {
		t.Fatalf("posted payload = %v", p)
	}
}

// yrBarrierAudit makes the posted-once dedup scans of two concurrent
// surfacings meet: each ListForRunByCategory of the posted category waits (up
// to a bound) for the other to arrive, so without the merge-intent lock both
// scans see no row and both append. Under the lock the second scan only runs
// after the first append, and the bounded wait lets it through.
type yrBarrierAudit struct {
	*mqmAudit
	mu      sync.Mutex
	arrived int
	both    chan struct{}
}

func (a *yrBarrierAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if category == CategoryMergeQueueYieldPosted {
		a.mu.Lock()
		a.arrived++
		if a.arrived == 2 {
			close(a.both)
		}
		a.mu.Unlock()
		select {
		case <-a.both:
		case <-time.After(timescale.D(300 * time.Millisecond)):
		}
	}
	return a.mqmAudit.ListForRunByCategory(ctx, runID, category)
}

// TestMergeQueueYieldSurface_ConcurrentSweepsPostOnce pins approval condition
// C6: two concurrent surfacings of one episode (two replicas' sweeps) append
// exactly ONE merge_queue_yield_posted row, because the dedup scan and its
// append run under the run's merge-intent lock. Counterfactual: running the
// scan and append outside the lock lets both barrier-aligned scans miss and
// both append, RED (two rows).
func TestMergeQueueYieldSurface_ConcurrentSweepsPostOnce(t *testing.T) {
	w, a, y := newYieldWorld(t)
	ba := &yrBarrierAudit{mqmAudit: w.audit, both: make(chan struct{})}
	w.srv.cfg.AuditRepo = ba

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.srv.surfaceMergeQueueYield(context.Background(), y)
		}()
	}
	wg.Wait()

	if n := len(w.rows(a, CategoryMergeQueueYieldPosted)); n != 1 {
		t.Fatalf("merge_queue_yield_posted rows after two concurrent surfacings = %d, want exactly 1", n)
	}
}

// TestMergeQueueYieldSweep_ProbesOnlyTheOldestDuePerBase: two yielded rows on
// one base are both due, and the sweep re-probes only the OLDEST by
// yielded_at (A clears and merges; B stays marked though it is approved too).
func TestMergeQueueYieldSweep_ProbesOnlyTheOldestDuePerBase(t *testing.T) {
	w := newMQELineWorld(t)
	a := w.seed("a", "a1", []byte(mqeSpecYAML))
	b := w.seed("b", "b1", []byte(mqeSpecYAML))
	w.mqpHeldIntent(a, "a1", false)
	w.mqpHeldIntent(b, "b1", false)
	w.forgeReviews(a, []string{"alice"}, mqmReview(1, "alice", "DISMISSED", "a1"))
	w.forgeReviews(b, []string{"alice"}, mqmReview(2, "alice", "DISMISSED", "b1"))
	w.pumpBase(a)
	w.backdateApprovalWait(a)
	w.pumpBase(a) // A yields; B is admitted and waits
	w.backdateApprovalWait(b)
	w.pumpBase(b) // B yields
	// Both 70s/65s old on the database clock: due at the 1-minute point.
	w.exec(`UPDATE merge_candidate_queue_entries SET yielded_at = clock_timestamp() - interval '70 seconds' WHERE run_id = $1 AND state = 'held'`, a.id)
	w.exec(`UPDATE merge_candidate_queue_entries SET yielded_at = clock_timestamp() - interval '65 seconds' WHERE run_id = $1 AND state = 'held'`, b.id)
	w.forgeReviews(a, []string{"alice"}, mqmReview(3, "alice", "APPROVED", "a1"))
	w.forgeReviews(b, []string{"alice"}, mqmReview(4, "alice", "APPROVED", "b1"))

	if n := w.srv.sweepMergeQueueYields(context.Background(), time.Minute); n != 1 {
		t.Fatalf("re-probes = %d, want 1 (at most one per base per tick)", n)
	}
	w.srv.bgMergeQueuePump.Wait()

	if n := len(w.rows(a, CategoryMergeQueueYieldCleared)); n != 1 {
		t.Fatalf("A cleared rows = %d, want 1 (the oldest due row)", n)
	}
	if e := w.active(b); e.YieldedAt == nil || len(w.rows(b, CategoryMergeQueueYieldCleared)) != 0 {
		t.Fatalf("B = %+v, want still marked (not probed this tick)", e)
	}
}

// TestRunMergeQueueYieldReprobe_TicksAndStops: the loop sweeps on its tick
// (a yield older than the tick is posted) and returns on cancellation,
// draining through bgMergeQueuePump.
func TestRunMergeQueueYieldReprobe_TicksAndStops(t *testing.T) {
	w, a, _ := newYieldWorld(t)
	ctx, cancel := context.WithCancel(context.Background())
	go w.srv.RunMergeQueueYieldReprobe(ctx, 5*time.Millisecond)

	deadline := time.Now().Add(timescale.D(10 * time.Second))
	for len(w.rows(a, CategoryMergeQueueYieldPosted)) == 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the loop posted no yield within the deadline")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	w.srv.bgMergeQueuePump.Wait()
	if n := len(w.rows(a, CategoryMergeQueueYieldPosted)); n != 1 {
		t.Fatalf("posted rows = %d, want 1", n)
	}
}
