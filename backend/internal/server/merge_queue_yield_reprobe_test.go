package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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

// TestYieldReprobeDue_Table pins the schedule for sweeps one interval apart
// (since = interval): nothing before one tick, then the doubling points
// interval·{1,2,4,8,16} (each due on exactly the tick that crosses it), then
// every 16 ticks.
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
		if got := yieldReprobeDue(tc.age, i, i); got != tc.want {
			t.Errorf("yieldReprobeDue(%s, since 1m, 1m) = %v, want %v", tc.age, got, tc.want)
		}
	}
	if yieldReprobeDue(time.Hour, time.Hour, 0) {
		t.Error("a non-positive interval is due")
	}
}

// TestYieldReprobeDue_OverrunStridePastTheCapBecomesDue pins the window as
// the time since the previous sweep (concern 8b3f29f5): sweeps that each
// overran into a 2x-interval stride, sampling a row past the 16-tick cap at
// odd-interval ages (19.5, 21.5, ... intervals), still reach the 32-interval
// point (due at 33.5). A first sweep after start (since 0) judges (0, age], so
// a row between points (3 intervals: past 1 and 2, before 4) is due, which a
// one-interval-apart sweep is not. Counterfactual: the fixed one-interval
// window (age − interval, age] never covers a multiple of 16 on that
// odd-interval stride, and misses the restart row, RED.
func TestYieldReprobeDue_OverrunStridePastTheCapBecomesDue(t *testing.T) {
	const i = time.Minute
	dueAt := time.Duration(-1)
	for age := 19*i + 30*time.Second; age <= 19*i+30*time.Second+16*i; age += 2 * i {
		if yieldReprobeDue(age, 2*i, i) {
			dueAt = age
			break
		}
	}
	if dueAt != 33*i+30*time.Second {
		t.Fatalf("2x-interval stride past the cap: first due at %s, want 33m30s (the 32-interval point)", dueAt)
	}
	if !yieldReprobeDue(3*i, 0, i) || yieldReprobeDue(3*i, i, i) {
		t.Fatal("restart window: want (0, age] due at 3 intervals, and (2, 3] intervals not due")
	}
	if yieldReprobeDue(30*time.Second, 0, i) {
		t.Fatal("a row younger than one interval is due on the first sweep")
	}
}

// TestYieldReprobeDueAfter_Table pins the A2.1 per-entry window (servicedAge,
// age] (ADR-112 Amendment A2.1): a never-serviced row (servicedAge 0) judges
// (0, age]; a serviced row is due only once a schedule point falls past the
// age it was serviced at; nothing younger than one interval is due; and past
// the 16-interval cap only multiples of 16 intervals are points.
func TestYieldReprobeDueAfter_Table(t *testing.T) {
	const i = time.Minute
	for _, tc := range []struct {
		age, serviced time.Duration
		want          bool
	}{
		{3 * i, 0, true}, {i, 0, true}, {59 * time.Second, 0, false},
		{3 * i, 2*i + 30*time.Second, false}, {4 * i, 2*i + 30*time.Second, true},
		{30 * time.Second, 0, false},
		{31 * i, 16 * i, false}, {32 * i, 16 * i, true},
		{31 * i, 17 * i, false}, // 24 intervals is no point past the cap
		{47 * i, 32 * i, false}, {48 * i, 32 * i, true}, {48 * i, 48 * i, false},
	} {
		if got := yieldReprobeDueAfter(tc.age, tc.serviced, i); got != tc.want {
			t.Errorf("yieldReprobeDueAfter(%s, serviced %s, 1m) = %v, want %v", tc.age, tc.serviced, got, tc.want)
		}
	}
	if yieldReprobeDueAfter(time.Hour, 0, 0) {
		t.Error("a non-positive interval is due")
	}
}

// TestYieldServicedBefore_Table pins the A2.2 order (ADR-112 Amendment A2.2):
// a never-serviced row (seq 0) before a serviced one even when its yield is
// younger; between serviced rows the lower seq first regardless of
// yielded_at; equal seq by yielded_at, then by id.
func TestYieldServicedBefore_Table(t *testing.T) {
	t0 := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	mk := func(id string, ago time.Duration) mergequeue.Entry {
		at := t0.Add(-ago)
		return mergequeue.Entry{ID: uuid.MustParse(id), YieldedAt: &at}
	}
	older := mk("00000000-0000-0000-0000-000000000002", 2*time.Hour)
	younger := mk("00000000-0000-0000-0000-000000000001", time.Hour)
	tieLo := mk("00000000-0000-0000-0000-000000000003", 3*time.Hour)
	tieHi := mk("00000000-0000-0000-0000-000000000004", 3*time.Hour)
	for _, tc := range []struct {
		name       string
		aSeq, bSeq uint64
		a, b       mergequeue.Entry
		want       bool
	}{
		{"never serviced before serviced, though younger", 0, 3, younger, older, true},
		{"serviced after never serviced, though older", 3, 0, older, younger, false},
		{"lower seq first, though younger", 2, 5, younger, older, true},
		{"higher seq after, though older", 5, 2, older, younger, false},
		{"equal seq: older yield first", 4, 4, older, younger, true},
		{"equal seq: younger yield after", 4, 4, younger, older, false},
		{"both never serviced: older yield first", 0, 0, older, younger, true},
		{"equal seq and yield: lower id first", 1, 1, tieLo, tieHi, true},
		{"equal seq and yield: higher id after", 1, 1, tieHi, tieLo, false},
	} {
		if got := yieldServicedBefore(tc.aSeq, tc.a, tc.bSeq, tc.b); got != tc.want {
			t.Errorf("%s: yieldServicedBefore = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// yieldSweptAgo rewinds every A2.1 service mark's age by ago (floor 0),
// keeping its seq: a mark a sweep recorded just now then reads as one
// recorded a tick of length ago earlier, the window the previous-sweep clock
// it replaced judged.
func (s *Server) yieldSweptAgo(ago time.Duration) {
	s.yieldSweepMu.Lock()
	defer s.yieldSweepMu.Unlock()
	for id, m := range s.yieldServiced {
		m.age = max(m.age-ago, 0)
		s.yieldServiced[id] = m
	}
}

// yieldMark reads entry id's A2.1 service mark under yieldSweepMu.
func (s *Server) yieldMark(id uuid.UUID) (yieldServicedMark, bool) {
	s.yieldSweepMu.Lock()
	defer s.yieldSweepMu.Unlock()
	m, ok := s.yieldServiced[id]
	return m, ok
}

// yrListFaultQueue overrides the sweep's two listings; a nil func delegates
// to the embedded store, as does every other method.
type yrListFaultQueue struct {
	mergequeue.Store
	activeBases func(context.Context) ([]mergequeue.Base, error)
	list        func(context.Context, mergequeue.Base) ([]mergequeue.Entry, error)
}

func (q yrListFaultQueue) ActiveBases(ctx context.Context) ([]mergequeue.Base, error) {
	if q.activeBases != nil {
		return q.activeBases(ctx)
	}
	return q.Store.ActiveBases(ctx)
}

func (q yrListFaultQueue) ListForBase(ctx context.Context, b mergequeue.Base) ([]mergequeue.Entry, error) {
	if q.list != nil {
		return q.list(ctx, b)
	}
	return q.Store.ListForBase(ctx, b)
}

// backdateYielded moves every held, yielded row's yielded_at d into the past
// on the database clock: d of schedule time passes, and the earlier
// yielded_at keeps each row's episode (and so its service mark).
func (w *mqmWorld) backdateYielded(d time.Duration) {
	w.t.Helper()
	w.exec(`UPDATE merge_candidate_queue_entries SET yielded_at = yielded_at - $1::bigint * interval '1 microsecond'
WHERE state = 'held' AND yielded_at IS NOT NULL`, d.Microseconds())
}

// TestMergeQueueYieldSweep_ServicedMarks pins the A2.1 mark's lifecycle, one
// assertion per mode. Each subtest yields A, ages it to 32 intervals (an hour
// each) and sweeps once, which probes A and records its mark.
func TestMergeQueueYieldSweep_ServicedMarks(t *testing.T) {
	const interval = time.Hour
	ctx := context.Background()
	serviced := func(t *testing.T) (*mqmWorld, *mcqRun, mergequeue.Entry) {
		t.Helper()
		w, a, _ := newYieldWorld(t)
		w.exec(`UPDATE merge_candidate_queue_entries SET yielded_at = clock_timestamp() - interval '32 hours 10 seconds' WHERE run_id = $1 AND state = 'held'`, a.id)
		if n := w.srv.sweepMergeQueueYields(ctx, interval); n != 1 {
			t.Fatalf("precondition: the first sweep re-probed %d rows, want 1", n)
		}
		e := w.active(a)
		if m, ok := w.srv.yieldMark(e.ID); !ok || m.seq != 1 || !m.yieldedAt.Equal(*e.YieldedAt) || m.age < 32*interval {
			t.Fatalf("precondition: A's mark = %+v (present %v), want seq 1 at the listed episode, age >= 32h", m, ok)
		}
		return w, a, e
	}

	// (1) MECHANISM: the stale mark's age (~32 intervals) exceeds the new
	// episode's latest point (1 interval), so without the episode check the
	// row is not due and the sweep probes 0 rows.
	t.Run("a later yielded_at is a new episode", func(t *testing.T) {
		w, a, _ := serviced(t)
		w.exec(`UPDATE merge_candidate_queue_entries SET yielded_at = clock_timestamp() - interval '1 hour 5 seconds' WHERE run_id = $1 AND state = 'held'`, a.id)
		if n := w.srv.sweepMergeQueueYields(ctx, interval); n != 1 {
			t.Fatalf("sweep after a new episode re-probed %d rows, want 1 (judged over (0, age])", n)
		}
		e := w.active(a)
		if m, ok := w.srv.yieldMark(e.ID); !ok || m.seq != 2 || !m.yieldedAt.Equal(*e.YieldedAt) {
			t.Fatalf("A's mark = %+v (present %v), want seq 2 at the new episode %v", m, ok, e.YieldedAt)
		}
	})

	// (2) A fully listed sweep that no longer lists the entry prunes its mark.
	t.Run("pruned after a fully listed sweep", func(t *testing.T) {
		w, _, e := serviced(t)
		w.srv.cfg.MergeQueue = yrListFaultQueue{Store: w.store, list: func(ctx context.Context, b mergequeue.Base) ([]mergequeue.Entry, error) {
			entries, err := w.store.ListForBase(ctx, b)
			var kept []mergequeue.Entry
			for _, x := range entries {
				if x.ID != e.ID {
					kept = append(kept, x)
				}
			}
			return kept, err
		}}
		w.srv.sweepMergeQueueYields(ctx, interval)
		if m, ok := w.srv.yieldMark(e.ID); ok {
			t.Fatalf("A's mark = %+v after a fully listed sweep that no longer lists A, want pruned", m)
		}
	})

	// (3) MECHANISM: the failed listing contributes no ids, so an unguarded
	// prune drops the mark.
	t.Run("kept when a base listing fails", func(t *testing.T) {
		w, _, e := serviced(t)
		w.srv.cfg.MergeQueue = yrListFaultQueue{Store: w.store, list: func(context.Context, mergequeue.Base) ([]mergequeue.Entry, error) {
			return nil, errors.New("injected ListForBase failure")
		}}
		w.srv.sweepMergeQueueYields(ctx, interval)
		if _, ok := w.srv.yieldMark(e.ID); !ok {
			t.Fatal("A's mark was pruned by a sweep whose base listing failed, want kept")
		}
	})

	// (4) A failed ActiveBases lists nothing, so an unguarded prune drops the
	// mark.
	t.Run("kept when ActiveBases fails", func(t *testing.T) {
		w, _, e := serviced(t)
		w.srv.cfg.MergeQueue = yrListFaultQueue{Store: w.store, activeBases: func(context.Context) ([]mergequeue.Base, error) {
			return nil, errors.New("injected ActiveBases failure")
		}}
		w.srv.sweepMergeQueueYields(ctx, interval)
		if _, ok := w.srv.yieldMark(e.ID); !ok {
			t.Fatal("A's mark was pruned by a sweep whose ActiveBases failed, want kept")
		}
	})
}

// TestMergeQueueYieldSweep_FirstSweepAfterStartProbesAPastPointRow: a fresh
// process (no previous sweep) finds a yielded row 3 minutes old, between the
// 2- and 4-minute points, and re-probes it (the window is (0, age]); the
// approved fold clears it. Counterfactual: the fixed one-interval window
// (2m, 3m] holds no point, so nothing is probed, RED.
func TestMergeQueueYieldSweep_FirstSweepAfterStartProbesAPastPointRow(t *testing.T) {
	w, a, _ := newYieldWorld(t)
	w.exec(`UPDATE merge_candidate_queue_entries SET yielded_at = clock_timestamp() - interval '3 minutes' WHERE run_id = $1 AND state = 'held'`, a.id)
	w.forgeReviews(a, []string{"alice"}, mqmReview(2, "alice", "APPROVED", "a1"))

	if n := w.srv.sweepMergeQueueYields(context.Background(), time.Minute); n != 1 {
		t.Fatalf("first sweep after start re-probed %d rows, want 1", n)
	}
	w.srv.bgMergeQueuePump.Wait()
	if n := len(w.rows(a, CategoryMergeQueueYieldCleared)); n != 1 {
		t.Fatalf("merge_queue_yield_cleared rows = %d, want 1", n)
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

	if !w.srv.ReprobeMergeQueueYield(context.Background(), a.id, false) {
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

	if got := w.srv.reprobeMergeQueueYield(context.Background(), w.active(a), false); got != reprobeUnchanged {
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

	if got := w.srv.reprobeMergeQueueYield(context.Background(), w.active(a), false); got != reprobeTransitioned {
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

	w.srv.reprobeMergeQueueYield(context.Background(), w.active(a), false)

	e := w.active(a)
	if e.AnchoredHeadSHA != "a3" || e.YieldedAt == nil || !e.YieldedAt.Equal(*y.YieldedAt) ||
		e.ApprovalWaitStartedAt == nil || !e.ApprovalWaitStartedAt.Equal(*y.ApprovalWaitStartedAt) {
		t.Fatalf("after the head move = head %s yielded_at %v wait %v, want a3 with the episode kept (%v, %v)",
			e.AnchoredHeadSHA, e.YieldedAt, e.ApprovalWaitStartedAt, y.YieldedAt, y.ApprovalWaitStartedAt)
	}

	admitted := len(w.rows(a, CategoryMergeCandidateQueueAdmitted))
	w.forgeReviews(a, []string{"alice"}, mqmReview(2, "alice", "APPROVED", "a3"))
	if !w.srv.ReprobeMergeQueueYield(context.Background(), a.id, false) {
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

// TestMergeQueueYieldReprobe_NothingBranches: a stale revision (the CAS
// conflict, docs/adr/112 A3.6), a transport failure, a merged PR, a terminal run and an entry
// that is not yielded each write nothing and leave the marker in place.
func TestMergeQueueYieldReprobe_NothingBranches(t *testing.T) {
	ctx := context.Background()
	t.Run("stale revision", func(t *testing.T) {
		w, a, y := newYieldWorld(t)
		w.forgeReviews(a, []string{"alice"}, mqmReview(2, "alice", "APPROVED", "a1"))
		stale := y
		stale.YieldRevision = y.YieldRevision - 1
		if got := w.srv.reprobeMergeQueueYield(ctx, stale, false); got != reprobeNothing {
			t.Fatalf("outcome = %s, want %s", got, reprobeNothing)
		}
		if e := w.active(a); e.YieldedAt == nil || len(w.rows(a, CategoryMergeQueueYieldCleared)) != 0 {
			t.Fatalf("a stale-revision probe cleared the marker: %+v", e)
		}
	})
	t.Run("transport failure", func(t *testing.T) {
		w, a, _ := newYieldWorld(t)
		w.forgeReviews(a, []string{"alice"}, mqmReview(2, "alice", "APPROVED", "a1"))
		w.gh.set(func() { w.gh.graphQLFail[a.pr] = 10 })
		if got := w.srv.reprobeMergeQueueYield(ctx, w.active(a), false); got != reprobeNothing || w.active(a).YieldedAt == nil {
			t.Fatalf("outcome = %s, want %s with the marker kept", got, reprobeNothing)
		}
	})
	t.Run("merged", func(t *testing.T) {
		w, a, _ := newYieldWorld(t)
		w.gh.set(func() { w.gh.prs[a.pr].state, w.gh.prs[a.pr].merged = "closed", true })
		if got := w.srv.reprobeMergeQueueYield(ctx, w.active(a), false); got != reprobeNothing || w.active(a).YieldedAt == nil {
			t.Fatalf("outcome = %s, want %s with the marker kept", got, reprobeNothing)
		}
	})
	t.Run("terminal run", func(t *testing.T) {
		w, a, y := newYieldWorld(t)
		w.forgeReviews(a, []string{"alice"}, mqmReview(2, "alice", "APPROVED", "a1"))
		w.exec(`UPDATE runs SET state = 'cancelled' WHERE id = $1`, a.id)
		if got := w.srv.reprobeMergeQueueYield(ctx, y, false); got != reprobeNothing {
			t.Fatalf("outcome = %s, want %s", got, reprobeNothing)
		}
	})
	t.Run("not yielded", func(t *testing.T) {
		w := newMQELineWorld(t)
		a := w.seed("a", "a1", []byte(mqeSpecYAML))
		w.mqpHeldIntent(a, "a1", false)
		if w.srv.ReprobeMergeQueueYield(ctx, a.id, false) || w.srv.ReprobeMergeQueueYield(ctx, uuid.New(), false) {
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

	if !w.srv.ReprobeMergeQueueYield(context.Background(), a.id, false) {
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
// one base are both due and both never serviced, so ADR-112 Amendment A2.2's
// tie-break by yielded_at decides: the sweep re-probes only the OLDEST yield.
// The fixture makes FIFO order and yield order disagree: A was enqueued first
// (so it lists first) but carries the YOUNGER yielded_at, so only B, the
// older yield, clears and A stays marked though it is approved too.
// Counterfactual: deleting the yieldedBefore comparison takes the first
// listed due row (A), RED.
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
	// A (enqueued first) 65s old, B 70s old, on the database clock: both due
	// at the 1-minute point, B the older yield.
	w.exec(`UPDATE merge_candidate_queue_entries SET yielded_at = clock_timestamp() - interval '65 seconds' WHERE run_id = $1 AND state = 'held'`, a.id)
	w.exec(`UPDATE merge_candidate_queue_entries SET yielded_at = clock_timestamp() - interval '70 seconds' WHERE run_id = $1 AND state = 'held'`, b.id)
	if ea, eb := w.active(a), w.active(b); !ea.EnqueuedAt.Before(eb.EnqueuedAt) {
		t.Fatalf("precondition: A enqueued_at %v, want before B's %v (A lists first)", ea.EnqueuedAt, eb.EnqueuedAt)
	}
	w.forgeReviews(a, []string{"alice"}, mqmReview(3, "alice", "APPROVED", "a1"))
	w.forgeReviews(b, []string{"alice"}, mqmReview(4, "alice", "APPROVED", "b1"))

	if n := w.srv.sweepMergeQueueYields(context.Background(), time.Minute); n != 1 {
		t.Fatalf("re-probes = %d, want 1 (at most one per base per tick)", n)
	}
	w.srv.bgMergeQueuePump.Wait()

	if n := len(w.rows(b, CategoryMergeQueueYieldCleared)); n != 1 {
		t.Fatalf("B cleared rows = %d, want 1 (the oldest due row by yielded_at)", n)
	}
	if e := w.active(a); e.YieldedAt == nil || len(w.rows(a, CategoryMergeQueueYieldCleared)) != 0 {
		t.Fatalf("A = %+v, want still marked (listed first but the younger yield, not probed this tick)", e)
	}
}

// TestMergeQueueYieldSweep_YoungerDueRowIsProbedUnderContention (CF7's
// vehicle) pins ADR-112 Amendment A2.1: only the PROBED row's service mark
// advances. A and B are yielded on one base, A enqueued first (it lists
// first) with the older yield; aged 32 intervals + 10 s (A) and + 5 s (B) at
// an hour-long interval, A's only review DISMISSED and B's APPROVED. Between
// sweeps every yielded row ages one interval. Sweep 1 probes A (both never
// serviced, A the older yield); sweep 2 finds A serviced past its latest
// point and B never serviced, so B is probed and clears, and the detached
// pump merges b1 while A stays yielded. MECHANISM: if every due row's mark
// advances (CF7), sweep 1 marks A and B though only A is probed; both next
// become due together at the 48-interval point, where A wins again, so B is
// never probed, RED.
func TestMergeQueueYieldSweep_YoungerDueRowIsProbedUnderContention(t *testing.T) {
	const interval = time.Hour
	ctx := context.Background()
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
	w.exec(`UPDATE merge_candidate_queue_entries SET yielded_at = clock_timestamp() - interval '32 hours 10 seconds' WHERE run_id = $1 AND state = 'held'`, a.id)
	w.exec(`UPDATE merge_candidate_queue_entries SET yielded_at = clock_timestamp() - interval '32 hours 5 seconds' WHERE run_id = $1 AND state = 'held'`, b.id)
	ea, eb := w.active(a), w.active(b)
	if !ea.Yielded() || !eb.Yielded() || !ea.EnqueuedAt.Before(eb.EnqueuedAt) || !ea.YieldedAt.Before(*eb.YieldedAt) {
		t.Fatalf("precondition: A (enqueued %v, yielded %v) and B (enqueued %v, yielded %v), want both yielded, A enqueued first with the older yield",
			ea.EnqueuedAt, ea.YieldedAt, eb.EnqueuedAt, eb.YieldedAt)
	}
	listed, err := w.store.ListForBase(ctx, ea.Base())
	if err != nil || len(listed) != 2 || listed[0].ID != ea.ID {
		t.Fatalf("precondition: ListForBase = %d entries (err %v), want 2 with A first", len(listed), err)
	}
	w.forgeReviews(b, []string{"alice"}, mqmReview(3, "alice", "APPROVED", "b1"))

	clearedAt := 0
	for k := 1; k <= 40 && clearedAt == 0; k++ {
		if k > 1 {
			w.backdateYielded(interval)
		}
		w.srv.sweepMergeQueueYields(ctx, interval)
		w.srv.bgMergeQueuePump.Wait()
		if len(w.rows(b, CategoryMergeQueueYieldCleared)) == 1 {
			clearedAt = k
		}
	}
	if clearedAt == 0 {
		t.Fatal("B (approved, the younger due yield) was never re-probed in 40 sweeps: A won every contended sweep")
	}
	t.Logf("B cleared at sweep %d", clearedAt)
	w.requirePuts("b1")
	w.requireState(b, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
	if e := w.active(a); e.YieldedAt == nil || len(w.rows(a, CategoryMergeQueueYieldCleared)) != 0 {
		t.Fatalf("A = %+v, want still yielded (its only review is dismissed)", e)
	}
}

// TestMergeQueueYieldSweep_RoundRobinReachesTheEligibleRowBehindSixteenBlocked
// is ADR-112 Amendment A2's N=17 evidence (CF8's vehicle): 17 runs on ONE
// base yield in FIFO order (r1 the oldest yield, r17 the youngest), aged on
// the database clock to 32 intervals + (18 − i) s at an hour-long interval.
// r1..r16 keep a DISMISSED review (permanently blocked) and r17 is APPROVED.
// Every yielded row ages one interval between sweeps, and each sweep
// re-probes exactly one row. Sweeps 1-16 (ages in [32, 48) intervals, latest
// point 32) probe r1..r16 once each, oldest yield first among the
// never-serviced; each is then not due until the 48-interval point. At sweep
// 17 all 17 rows are due and round-robin (A2.2) picks r17, never serviced, so
// r17 clears and the detached pump merges it while r1..r16 stay yielded.
// MECHANISM: oldest-by-yielded_at (CF8) picks r1 again at sweep 17, so r17 is
// not probed within 17 sweeps, RED.
func TestMergeQueueYieldSweep_RoundRobinReachesTheEligibleRowBehindSixteenBlocked(t *testing.T) {
	const interval, n = time.Hour, 17
	ctx := context.Background()
	w := newMQELineWorld(t)
	head := func(i int) string { return fmt.Sprintf("r%dh", i+1) }
	runs := make([]*mcqRun, n)
	for i := range runs {
		runs[i] = w.seed(fmt.Sprintf("r%d", i+1), head(i), []byte(mqeSpecYAML))
	}
	for i, r := range runs {
		w.mqpHeldIntent(r, head(i), false)
		w.forgeReviews(r, []string{"alice"}, mqmReview(int64(i+1), "alice", "DISMISSED", head(i)))
	}
	w.pumpBase(runs[0]) // r1 is admitted and waits
	for _, r := range runs {
		w.backdateApprovalWait(r)
		w.pumpBase(r) // r yields; the next run is admitted and waits
	}
	for i, r := range runs {
		w.exec(`UPDATE merge_candidate_queue_entries SET yielded_at = clock_timestamp() - interval '32 hours' - $2::int * interval '1 second'
WHERE run_id = $1 AND state = 'held'`, r.id, n+1-(i+1))
		if e := w.active(r); !e.Yielded() || e.State != mergequeue.StateHeld {
			t.Fatalf("precondition: r%d = %s yielded_at %v, want held and yielded", i+1, e.State, e.YieldedAt)
		}
	}
	last := runs[n-1]
	w.forgeReviews(last, []string{"alice"}, mqmReview(100, "alice", "APPROVED", head(n-1)))

	clearedAt := 0
	for k := 1; k <= n && clearedAt == 0; k++ {
		if k > 1 {
			w.backdateYielded(interval)
		}
		if got := w.srv.sweepMergeQueueYields(ctx, interval); got != 1 {
			t.Fatalf("sweep %d re-probed %d rows, want exactly 1", k, got)
		}
		w.srv.bgMergeQueuePump.Wait()
		if len(w.rows(last, CategoryMergeQueueYieldCleared)) == 1 {
			clearedAt = k
		}
	}
	if clearedAt == 0 {
		t.Fatalf("r%d (approved, the youngest yield) was not re-probed within %d sweeps: the blocked rows starved it", n, n)
	}
	t.Logf("r%d cleared at sweep %d", n, clearedAt)
	w.requirePuts(head(n - 1))
	w.requireState(last, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
	for i, r := range runs[:n-1] {
		if e := w.active(r); e.YieldedAt == nil || len(w.rows(r, CategoryMergeQueueYieldCleared)) != 0 {
			t.Fatalf("r%d = %+v, want still yielded (its only review is dismissed)", i+1, e)
		}
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

// newRowFourYieldWorld is a gate-less run A ALONE on its base (so a
// re-admission meets an unmoved base and re-attempts the pinned PUT at a1)
// whose pinned merge the forge refuses (405, a typed RELEASE outcome) while
// the fresh snapshot reads BLOCKED: one pump yields A forge_review_required
// (ADR-112 R3 row 4) after exactly one PUT. yielded_at is then backdated an
// hour on the database clock (so a one-hour-interval sweep finds the row due
// and a fresh stamp is told apart), and the backdated entry is returned.
func newRowFourYieldWorld(t *testing.T) (*mqmWorld, *mcqRun, mergequeue.Entry) {
	t.Helper()
	w := newMQELineWorld(t)
	a := w.seed("a", "a1", []byte(mcvUndelegatedSpecYAML))
	w.mqpHeldIntent(a, "a1", false)
	w.gh.set(func() { w.gh.putStatus[a.pr] = http.StatusMethodNotAllowed })
	w.gh.setReviewSnapshot(a.pr, mcqReviewSnapshot{MergeStateStatus: "BLOCKED"})
	w.pumpBase(a)
	w.requireYielded(a, mergeQueueYieldForgeReviewRequired, forgeApprovalNextApprovePR, mergeQueueYieldSiteMergeOnPass)
	w.requirePuts("a1")
	w.exec(`UPDATE merge_candidate_queue_entries SET yielded_at = yielded_at - interval '1 hour' WHERE run_id = $1 AND state = 'held'`, a.id)
	return w, a, w.active(a)
}

// requireRowFourKept asserts A is still yielded forge_review_required in the
// episode y, with no merge_queue_yield_cleared row.
func (w *mqmWorld) requireRowFourKept(a *mcqRun, y mergequeue.Entry) {
	w.t.Helper()
	e := w.active(a)
	if e.State != mergequeue.StateHeld || e.YieldReason != mergeQueueYieldForgeReviewRequired || e.YieldedAt == nil || !e.YieldedAt.Equal(*y.YieldedAt) {
		w.t.Fatalf("A = %s %q yielded_at %v, want held forge_review_required in the episode %v", e.State, e.YieldReason, e.YieldedAt, y.YieldedAt)
	}
	if n := len(w.rows(a, CategoryMergeQueueYieldCleared)); n != 0 {
		w.t.Fatalf("merge_queue_yield_cleared rows = %d, want 0", n)
	}
}

// TestMergeQueueYieldReprobe_ForgeReviewRequiredClearsOnceTheSnapshotShowsApproved:
// the row-4 yield's fresh snapshot now reads APPROVED / CLEAN (and the forge
// accepts the PUT), so the targeted, unforced re-probe clears it and the line
// merges A pinned to a1.
func TestMergeQueueYieldReprobe_ForgeReviewRequiredClearsOnceTheSnapshotShowsApproved(t *testing.T) {
	w, a, _ := newRowFourYieldWorld(t)
	w.gh.setReviewSnapshot(a.pr, mcqReviewSnapshot{ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN"})
	w.gh.set(func() { delete(w.gh.putStatus, a.pr) })

	if !w.srv.ReprobeMergeQueueYield(context.Background(), a.id, false) {
		t.Fatal("an APPROVED/CLEAN snapshot did not clear the forge_review_required yield")
	}
	w.srv.bgMergeQueuePump.Wait()

	w.requirePuts("a1", "a1")
	w.requireState(a, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
}

// TestMergeQueueYieldReprobe_ForgeReviewRequiredSnapshotErrorKeepsTheYield:
// the fixture snapshot reads APPROVED / CLEAN (so a read would clear), but the
// re-probe's fresh snapshot read fails (the gate-less fold makes no forge
// read, so the tier-3 read is the one that fails): the row stays yielded and
// nothing is written, fail closed (mergeQueueYieldTier ok=false).
// Counterfactual: treating a read error as the condition gone clears it, RED.
func TestMergeQueueYieldReprobe_ForgeReviewRequiredSnapshotErrorKeepsTheYield(t *testing.T) {
	w, a, y := newRowFourYieldWorld(t)
	w.gh.setReviewSnapshot(a.pr, mcqReviewSnapshot{ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN"})
	w.gh.set(func() { w.gh.graphQLFail[a.pr] = 10 })

	if got := w.srv.reprobeMergeQueueYield(context.Background(), w.active(a), false); got != reprobeNothing {
		t.Fatalf("outcome = %s, want %s", got, reprobeNothing)
	}
	w.srv.bgMergeQueuePump.Wait()
	w.requireRowFourKept(a, y)
	w.requirePuts("a1")
}

// TestMergeQueueYieldReprobe_ForgeReviewRequiredWebhookWithoutForgeChangeKeepsTheYield
// (H2): an approved review webhook at the anchored head triggers the targeted
// re-probe, but the forge still reads BLOCKED, so the shared clear predicate
// keeps the row yielded and nothing is PUT. The trigger is asserted to have
// RUN (one pr_approved_on_github row, and exactly one more review-snapshot
// read: the gate-less fold reads nothing, so it is the predicate's), so a
// silently dropped trigger cannot pass. Counterfactuals: applying the
// snapshot check only in the sweep (the webhook path clears on the approved
// gate-less fold) clears and re-attempts, RED; dropping the webhook's trigger
// leaves the snapshot reads unchanged, RED.
func TestMergeQueueYieldReprobe_ForgeReviewRequiredWebhookWithoutForgeChangeKeepsTheYield(t *testing.T) {
	w, a, y := newRowFourYieldWorld(t)
	reads := w.gh.snapshotCalls()

	prReviewYieldWebhook(w, a, "approved", "a1")

	if n := len(w.rows(a, CategoryPRApprovedOnGitHub)); n != 1 {
		t.Fatalf("pr_approved_on_github rows = %d, want 1 (the webhook reached the trigger)", n)
	}
	if n := w.gh.snapshotCalls() - reads; n != 1 {
		t.Fatalf("review-snapshot reads during the webhook = %d, want 1 (the re-probe's fresh snapshot)", n)
	}
	w.requireRowFourKept(a, y)
	w.requirePuts("a1")
}

// TestMergeQueueYieldReprobe_ForgeReviewRequiredReyieldKeepsTheEpisode (H3):
// the operator's merge_run FORCES one re-attempt (the clear skips the
// snapshot), the forge refuses again on an unchanged state and A re-yields
// forge_review_required on the same head and base: it KEEPS the backdated
// yielded_at, so its age (the re-probe backoff) keeps growing from the
// original episode, and the episode is posted once. Counterfactual: stamping
// a fresh yielded_at on the same-reason re-yield restarts the age at ~0, RED.
func TestMergeQueueYieldReprobe_ForgeReviewRequiredReyieldKeepsTheEpisode(t *testing.T) {
	w, a, y := newRowFourYieldWorld(t)

	if !w.srv.ReprobeMergeQueueYield(context.Background(), a.id, true) {
		t.Fatal("the forced re-probe did not clear the forge_review_required yield")
	}
	w.srv.bgMergeQueuePump.Wait()

	w.requirePuts("a1", "a1")
	e := w.active(a)
	if e.State != mergequeue.StateHeld || e.YieldReason != mergeQueueYieldForgeReviewRequired || e.YieldedAt == nil ||
		!e.YieldedAt.Equal(*y.YieldedAt) || e.YieldAge < time.Hour {
		t.Fatalf("A after the re-yield = %s %q yielded_at %v age %s, want held forge_review_required in the episode %v (age >= 1h)",
			e.State, e.YieldReason, e.YieldedAt, e.YieldAge, y.YieldedAt)
	}
	_, yieldedAt := mergeQueueYieldEpisode(y)
	rows := w.rows(a, CategoryMergeQueueYielded)
	if len(rows) != 2 || w.payload(rows[1])["yielded_at"] != yieldedAt {
		t.Fatalf("merge_queue_yielded rows = %d, want 2, the re-yield naming the episode %s", len(rows), yieldedAt)
	}
	w.srv.sweepMergeQueueYields(context.Background(), time.Minute)
	if n := len(w.rows(a, CategoryMergeQueueYieldPosted)); n != 1 {
		t.Fatalf("merge_queue_yield_posted rows = %d, want 1 (one episode)", n)
	}
}

// --- docs/adr/112 Amendment A3 (#4780): the re-probe's head discovery,
// clear predicate, tiers, row-4 flag, revision CAS and force rule, driven
// through the existing mcq fake forge (prs, checks, reviewSnapshot).

// yrRequireCI declares "ci" as r's required check (the run's
// RequiredChecksSnapshot), so a red "ci" is A3.1(a) checks_red.
func (w *mqmWorld) yrRequireCI(r *mcqRun) {
	w.t.Helper()
	w.exec(`UPDATE runs SET required_checks_snapshot = '{"contexts":["ci"]}' WHERE id = $1`, r.id)
}

// yrChecks serves one completed "ci" check run with conclusion at head.
func (w *mqmWorld) yrChecks(head, conclusion string) {
	w.gh.set(func() {
		w.gh.checks[head] = `[{"id":2,"name":"ci","status":"completed","conclusion":"` + conclusion +
			`","started_at":"2026-01-01T00:00:00Z","completed_at":"2026-01-01T00:01:00Z","check_suite":{"id":1},"app":{"slug":"github-actions"}}]`
	})
}

// yrYieldStored admits r's held intent entry and yields it with reason
// through the store (no merge-on-pass), returning the yielded entry.
func (w *mqmWorld) yrYieldStored(r *mcqRun, reason string) mergequeue.Entry {
	w.t.Helper()
	ctx := context.Background()
	e := w.active(r)
	adm, err := w.store.AdmitNext(ctx, e.Base())
	if err != nil || !adm.Admitted || adm.Entry.ID != e.ID {
		w.t.Fatalf("admit %s: %+v %v", r.id, adm, err)
	}
	y, err := w.store.Yield(ctx, e.ID, adm.Entry.AdmissionEpoch, newMergeQueueYield(reason, "seeded", mergeQueueYieldRow(reason)).marker())
	if err != nil {
		w.t.Fatalf("yield %s: %v", r.id, err)
	}
	return y
}

// newGatelessRedYieldWorld seeds a GATE-LESS run held with intent at a1,
// "ci" required and red at a1, yielded merge_checks_red through the store.
func newGatelessRedYieldWorld(t *testing.T) (*mqmWorld, *mcqRun, mergequeue.Entry) {
	t.Helper()
	w := newMQELineWorld(t)
	a := w.seed("a", "a1", []byte(mcvUndelegatedSpecYAML))
	w.mqpHeldIntent(a, "a1", false)
	w.yrRequireCI(a)
	w.yrChecks("a1", "failure")
	return w, a, w.yrYieldStored(a, mergeQueueYieldMergeChecksRed)
}

// TestReprobe_A34_ApprovedFoldWithFailingLegDoesNotClear (T20, CF18
// vehicle): a GATED run yielded on a dismissal now carries a qualifying
// approval at the anchored head, but its required check is red there: the
// clear predicate (A3.4) refuses, the row stays yielded and transitions to
// merge_checks_red / retrigger_ci, and nothing is cleared. MECHANISM: the
// fold alone approves, so only the merge-gate legs block.
func TestReprobe_A34_ApprovedFoldWithFailingLegDoesNotClear(t *testing.T) {
	w, a, y := newYieldWorld(t)
	w.yrRequireCI(a)
	w.yrChecks("a1", "failure")
	w.forgeReviews(a, []string{"alice"}, mqmReview(2, "alice", "APPROVED", "a1"))

	if got := w.srv.reprobeMergeQueueYield(context.Background(), w.active(a), false); got != reprobeTransitioned {
		t.Fatalf("outcome = %s, want %s", got, reprobeTransitioned)
	}
	e := w.active(a)
	if e.State != mergequeue.StateHeld || e.YieldReason != mergeQueueYieldMergeChecksRed || !e.YieldedAt.Equal(*y.YieldedAt) {
		t.Fatalf("A = %s %q yielded_at %v, want held merge_checks_red in the episode %v", e.State, e.YieldReason, e.YieldedAt, y.YieldedAt)
	}
	rows := w.rows(a, CategoryMergeQueueYielded)
	if p := w.payload(rows[len(rows)-1]); p["next_step"] != mergeQueueYieldNextRetriggerCI || p["precedence_row"] != float64(mergeQueueYieldRowEligibility) {
		t.Fatalf("transition payload = %v, want retrigger_ci row 1", p)
	}
	if n := len(w.rows(a, CategoryMergeQueueYieldCleared)); n != 0 {
		t.Fatalf("merge_queue_yield_cleared rows = %d, want 0", n)
	}
}

// TestReprobe_A35_TierTransitions (T21): one subtest per A3.5 tier
// transition.
func TestReprobe_A35_TierTransitions(t *testing.T) {
	ctx := context.Background()
	t.Run("veto beats merge-gate", func(t *testing.T) {
		w, a, _ := newYieldWorld(t)
		w.yrRequireCI(a)
		w.yrChecks("a1", "failure")
		w.forgeReviews(a, []string{"bob"}, mqmReview(2, "bob", "CHANGES_REQUESTED", "a1"))
		w.srv.reprobeMergeQueueYield(ctx, w.active(a), false)
		if e := w.active(a); e.YieldReason != mergeCandidateQueueReasonForgeApprovalVetoed {
			t.Fatalf("reason = %q, want forge_approval_vetoed (tier 1 over a red leg)", e.YieldReason)
		}
	})
	t.Run("merge-gate beats row-4", func(t *testing.T) {
		w, a, _ := newRowFourYieldWorld(t)
		w.yrRequireCI(a)
		w.yrChecks("a1", "failure")
		w.srv.reprobeMergeQueueYield(ctx, w.active(a), false)
		if e := w.active(a); e.YieldReason != mergeQueueYieldMergeChecksRed || !e.Row4Flag {
			t.Fatalf("reason = %q flag=%v, want merge_checks_red with row4_flag kept", e.YieldReason, e.Row4Flag)
		}
	})
	t.Run("row-4 beats a non-approved fold", func(t *testing.T) {
		w, a, _ := newYieldWorld(t)
		w.exec(`UPDATE merge_candidate_queue_entries SET row4_flag = true WHERE run_id = $1 AND state = 'held'`, a.id)
		snap := w.gh.reviewSnapshot[a.pr]
		w.gh.setReviewSnapshot(a.pr, mcqReviewSnapshot{Reviews: snap.Reviews, Author: snap.Author, MergeStateStatus: "BLOCKED"})
		w.srv.reprobeMergeQueueYield(ctx, w.active(a), false)
		if e := w.active(a); e.YieldReason != mergeQueueYieldForgeReviewRequired {
			t.Fatalf("reason = %q, want forge_review_required (tier 3 over a dismissed fold)", e.YieldReason)
		}
	})
	t.Run("row-4 then red then green while BLOCKED reads forge_review_required", func(t *testing.T) {
		w, a, _ := newRowFourYieldWorld(t)
		w.yrRequireCI(a)
		w.yrChecks("a1", "failure")
		w.srv.reprobeMergeQueueYield(ctx, w.active(a), false)
		if e := w.active(a); e.YieldReason != mergeQueueYieldMergeChecksRed {
			t.Fatalf("after red = %q, want merge_checks_red", e.YieldReason)
		}
		w.yrChecks("a1", "success")
		if got := w.srv.reprobeMergeQueueYield(ctx, w.active(a), false); got != reprobeTransitioned {
			t.Fatalf("after green = %s, want transitioned", got)
		}
		if e := w.active(a); e.YieldReason != mergeQueueYieldForgeReviewRequired || !e.Yielded() {
			t.Fatalf("after green while BLOCKED = %q yielded=%v, want forge_review_required", e.YieldReason, e.Yielded())
		}
	})
}

// TestReprobe_A36_FlagSurvivesBlockedGoneWhileRed (T22, CF19 vehicle): a
// row-4 yield turns red (merge_checks_red), the snapshot then reads BLOCKED
// GONE while red, then CI goes green with BLOCKED back: the row stays yielded
// as forge_review_required and is NOT cleared. MECHANISM: the gone read
// happens while tier 2 blocks, so only the persisted flag (never cleared by a
// reason transition or a gone read without a clear) keeps the row-4
// obligation; with UpdateYield assigning row4_flag = marker.Row4 the red
// transition drops it and the last probe clears, RED.
func TestReprobe_A36_FlagSurvivesBlockedGoneWhileRed(t *testing.T) {
	ctx := context.Background()
	w, a, _ := newRowFourYieldWorld(t)
	w.yrRequireCI(a)
	w.yrChecks("a1", "failure")
	w.srv.reprobeMergeQueueYield(ctx, w.active(a), false)
	w.gh.setReviewSnapshot(a.pr, mcqReviewSnapshot{ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN"})
	w.srv.reprobeMergeQueueYield(ctx, w.active(a), false)
	if e := w.active(a); e.YieldReason != mergeQueueYieldMergeChecksRed || !e.Row4Flag {
		t.Fatalf("gone while red = %q flag=%v, want merge_checks_red with the flag kept", e.YieldReason, e.Row4Flag)
	}
	w.yrChecks("a1", "success")
	w.gh.setReviewSnapshot(a.pr, mcqReviewSnapshot{MergeStateStatus: "BLOCKED"})
	if got := w.srv.reprobeMergeQueueYield(ctx, w.active(a), false); got == reprobeCleared {
		t.Fatal("the row cleared while the forge reads BLOCKED again")
	}
	if e := w.active(a); e.YieldReason != mergeQueueYieldForgeReviewRequired || !e.Yielded() {
		t.Fatalf("green with BLOCKED back = %q yielded=%v, want forge_review_required", e.YieldReason, e.Yielded())
	}
	if n := len(w.rows(a, CategoryMergeQueueYieldCleared)); n != 0 {
		t.Fatalf("merge_queue_yield_cleared rows = %d, want 0", n)
	}
}

// TestReprobe_A36_DelayedProbeCannotClearOrOverwriteNewer (T23): a probe
// holding the entry read at revision r runs AFTER a newer probe transitioned
// the reason (same head and yielded_at): its clear (the forge now approves)
// and its transition both miss the revision CAS, and a READ shows the newer
// marker and reason intact.
func TestReprobe_A36_DelayedProbeCannotClearOrOverwriteNewer(t *testing.T) {
	ctx := context.Background()
	w, a, _ := newYieldWorld(t)
	stale := w.active(a)
	w.forgeReviews(a, []string{"bob"}, mqmReview(2, "bob", "CHANGES_REQUESTED", "a1"))
	if got := w.srv.reprobeMergeQueueYield(ctx, w.active(a), false); got != reprobeTransitioned {
		t.Fatalf("newer probe = %s, want transitioned", got)
	}
	newer := w.active(a)

	w.forgeReviews(a, []string{"alice"}, mqmReview(3, "alice", "APPROVED", "a1"))
	if got := w.srv.reprobeMergeQueueYield(ctx, stale, false); got != reprobeNothing {
		t.Fatalf("delayed clear = %s, want nothing", got)
	}
	w.forgeReviews(a, []string{"alice"}, mqmReview(4, "alice", "DISMISSED", "a1"))
	stale.YieldReason = mergeCandidateQueueReasonForgeApprovalAbsent // the delayed probe's own stale read differs from the fold
	if got := w.srv.reprobeMergeQueueYield(ctx, stale, false); got != reprobeNothing {
		t.Fatalf("delayed transition = %s, want nothing", got)
	}
	e := w.active(a)
	if !e.Yielded() || e.YieldReason != mergeCandidateQueueReasonForgeApprovalVetoed || e.YieldRevision != newer.YieldRevision ||
		len(w.rows(a, CategoryMergeQueueYieldCleared)) != 0 {
		t.Fatalf("after the delayed probes = %q rev %d yielded=%v, want the newer vetoed marker at rev %d", e.YieldReason, e.YieldRevision, e.Yielded(), newer.YieldRevision)
	}
}

// TestReprobe_A39_ForceSkipsOnlySnapshot (T24, CF20 vehicle): (1) a forced
// probe with the row-4 flag set and every leg green clears WITHOUT a
// snapshot read and KEEPS row4_flag (read after the line merges); (2) a
// forced probe with a red leg does NOT clear: force never skips the
// merge-gate legs.
func TestReprobe_A39_ForceSkipsOnlySnapshot(t *testing.T) {
	ctx := context.Background()
	t.Run("green legs clear without a snapshot and keep the flag", func(t *testing.T) {
		w, a, y := newRowFourYieldWorld(t)
		w.gh.set(func() { delete(w.gh.putStatus, a.pr) })
		reads := w.gh.snapshotCalls()
		if got := w.srv.reprobeMergeQueueYield(ctx, w.active(a), true); got != reprobeCleared {
			t.Fatalf("forced probe = %s, want cleared", got)
		}
		if n := w.gh.snapshotCalls() - reads; n != 0 {
			t.Fatalf("snapshot reads during the forced probe = %d, want 0", n)
		}
		w.srv.bgMergeQueuePump.Wait()
		var flag bool
		if err := w.pool.QueryRow(ctx, `SELECT row4_flag FROM merge_candidate_queue_entries WHERE id = $1`, y.ID).Scan(&flag); err != nil || !flag {
			t.Fatalf("row4_flag after a forced clear = %v (%v), want kept", flag, err)
		}
	})
	t.Run("a red leg is never forced", func(t *testing.T) {
		w, a, _ := newRowFourYieldWorld(t)
		w.yrRequireCI(a)
		w.yrChecks("a1", "failure")
		if got := w.srv.reprobeMergeQueueYield(ctx, w.active(a), true); got == reprobeCleared {
			t.Fatal("a forced probe cleared a red leg")
		}
		if e := w.active(a); !e.Yielded() || e.YieldReason != mergeQueueYieldMergeChecksRed {
			t.Fatalf("A = %q yielded=%v, want merge_checks_red", e.YieldReason, e.Yielded())
		}
	})
}

// TestReprobe_A34_GatelessRepairPushReanchorsAndClears (T25, CF21 vehicle):
// a GATE-LESS run yielded merge_checks_red at a1; the repair push moves the
// PR head to a2, whose checks are green: ONE probe re-anchors to a2 and
// clears there, keeping the episode's yielded_at. MECHANISM: a gate-less
// fold is approved without reading the PR, so only the gate-independent PR
// read discovers a2; without it the legs are judged red at a1, RED.
func TestReprobe_A34_GatelessRepairPushReanchorsAndClears(t *testing.T) {
	w, a, y := newGatelessRedYieldWorld(t)
	w.gh.set(func() { w.gh.prs[a.pr].head = "a2" })

	if got := w.srv.reprobeMergeQueueYield(context.Background(), w.active(a), false); got != reprobeCleared {
		t.Fatalf("outcome = %s, want cleared", got)
	}
	w.srv.bgMergeQueuePump.Wait()
	rows := w.rows(a, CategoryMergeQueueYieldCleared)
	if len(rows) != 1 {
		t.Fatalf("merge_queue_yield_cleared rows = %d, want 1", len(rows))
	}
	_, yieldedAt := mergeQueueYieldEpisode(y)
	if p := w.payload(rows[0]); p["head_sha"] != "a2" || p["yielded_at"] != yieldedAt || p["yield_reason"] != mergeQueueYieldMergeChecksRed {
		t.Fatalf("cleared payload = %v, want head a2 in the episode %s", p, yieldedAt)
	}
}

// TestReprobe_A37 (T26): docs/adr/112 A3.7's episode. (1) A flap: a
// merge_checks_red yield clears on green and the line re-yields (the forge
// refuses the pinned merge while BLOCKED, forge_review_required): the
// re-yield CONTINUES the cleared episode's yielded_at whatever the reason,
// so the A2.1 service mark still applies (the backoff keeps growing), and
// the hold budget is untouched by the clear and the re-yield. (2) A head
// move keeps the episode (yielded_at and the approval anchor).
func TestReprobe_A37(t *testing.T) {
	ctx := context.Background()
	t.Run("a flap continues the episode, the backoff and the budget", func(t *testing.T) {
		w, a, y := newGatelessRedYieldWorld(t)
		w.exec(`UPDATE merge_candidate_queue_entries SET yielded_at = yielded_at - interval '1 hour', gate_hold_tokens = 1 WHERE id = $1`, y.ID)
		y = w.active(a)
		w.srv.markYieldReprobeServiced(y)
		w.yrChecks("a1", "success")
		w.gh.set(func() { w.gh.putStatus[a.pr] = http.StatusMethodNotAllowed })
		w.gh.setReviewSnapshot(a.pr, mcqReviewSnapshot{MergeStateStatus: "BLOCKED"})

		if !w.srv.ReprobeMergeQueueYield(ctx, a.id, false) {
			t.Fatal("green checks did not clear the merge_checks_red yield")
		}
		w.srv.bgMergeQueuePump.Wait()

		e := w.active(a)
		if !e.Yielded() || e.YieldReason != mergeQueueYieldForgeReviewRequired || !e.YieldedAt.Equal(*y.YieldedAt) {
			t.Fatalf("after the flap = %q yielded_at %v, want forge_review_required continuing %v", e.YieldReason, e.YieldedAt, y.YieldedAt)
		}
		if _, seq := w.srv.yieldReprobeDueFor(e, time.Minute); seq == 0 {
			t.Fatal("the re-yield lost its service mark (a fresh episode), want the backoff continued")
		}
		if e.GateHoldTokens != 1 {
			t.Fatalf("hold tokens after the flap = %d, want 1 (untouched)", e.GateHoldTokens)
		}
	})
	t.Run("a head move keeps the episode", func(t *testing.T) {
		w, a, y := newYieldWorld(t)
		w.gh.set(func() { w.gh.prs[a.pr].head = "a3" })
		if got := w.srv.reprobeMergeQueueYield(ctx, w.active(a), false); got == reprobeCleared || got == reprobeNothing {
			t.Fatalf("outcome = %s, want the row re-anchored and still yielded", got)
		}
		e := w.active(a)
		if e.AnchoredHeadSHA != "a3" || !e.YieldedAt.Equal(*y.YieldedAt) || !e.ApprovalWaitStartedAt.Equal(*y.ApprovalWaitStartedAt) {
			t.Fatalf("after the head move = %s %v %v, want a3 with the episode (%v, %v)", e.AnchoredHeadSHA, e.YieldedAt, e.ApprovalWaitStartedAt, y.YieldedAt, y.ApprovalWaitStartedAt)
		}
	})
}
