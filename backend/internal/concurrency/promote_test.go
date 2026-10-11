package concurrency

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// Promote tests (#4191). Every verdict is read back from COMMITTED slot rows
// after the call returns, never inferred from the error alone.

// recorder is a Record callback that counts its calls and can fail.
type recorder struct {
	mu    sync.Mutex
	calls []Promotion
	err   error
}

func (r *recorder) record(_ context.Context, _ pgx.Tx, p Promotion) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, p)
	return r.err
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (f *fixture) promote(id uuid.UUID) (Promotion, error) {
	f.t.Helper()
	rec := &recorder{}
	return f.store.Promote(context.Background(), PromoteRequest{StageID: id, Record: rec.record})
}

func (f *fixture) mustPromote(id uuid.UUID, wantOld int) Promotion {
	f.t.Helper()
	p, err := f.promote(id)
	if err != nil {
		f.t.Fatalf("Promote(%s): %v", id, err)
	}
	if !p.Changed || p.OldPosition != wantOld || p.NewPosition != 1 {
		f.t.Fatalf("Promote(%s) = %+v, want changed %d -> 1", id, p, wantOld)
	}
	return p
}

// orderKey reads a row's committed order_key (nil when NULL or no row).
func (f *fixture) orderKey(id uuid.UUID) *time.Time {
	f.t.Helper()
	var k *time.Time
	if err := f.pool.QueryRow(context.Background(), `SELECT order_key FROM stage_concurrency_slots WHERE stage_id = $1`, id).Scan(&k); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		f.t.Fatalf("read order_key of %s: %v", id, err)
	}
	return k
}

// rowJSON is the committed slot row, whole, as text: a byte-identity probe.
func (f *fixture) rowJSON(id uuid.UUID) string {
	f.t.Helper()
	var s string
	if err := f.pool.QueryRow(context.Background(), `SELECT to_jsonb(s)::text FROM stage_concurrency_slots s WHERE stage_id = $1`, id).Scan(&s); err != nil {
		f.t.Fatalf("read slot row of %s: %v", id, err)
	}
	return s
}

func (f *fixture) positions(ids ...uuid.UUID) map[uuid.UUID]Status {
	f.t.Helper()
	st, err := f.store.StatusForStages(context.Background(), ids)
	if err != nil {
		f.t.Fatalf("StatusForStages: %v", err)
	}
	return st
}

// awaitAdvisoryWaiter polls pg_stat_activity until a backend of this test's
// database waits on an advisory lock, or done closes first. It reports
// whether the waiter was seen; a bound with neither fails the test.
func (f *fixture) awaitAdvisoryWaiter(done <-chan struct{}) bool {
	f.t.Helper()
	deadline := time.Now().Add(timescale.D(10 * time.Second))
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return false
		default:
		}
		var n int
		if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
WHERE datname = current_database() AND wait_event_type = 'Lock' AND wait_event = 'advisory'`).Scan(&n); err != nil {
			f.t.Fatalf("pg_stat_activity: %v", err)
		}
		if n > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.t.Fatal("no advisory-lock waiter appeared and Promote did not return within the bound")
	return false
}

// queueBehindHolder admits h and queues the rest in order (positions 1..n).
func (f *fixture) queueBehindHolder(h *run.Stage, rest ...*run.Stage) {
	f.t.Helper()
	f.mustAdmit(h)
	for i, st := range rest {
		f.mustQueue(st, i+1)
	}
}

// TestPromote_PromotedWaiterAdmittedNextThenFIFO: A, B, C queued behind H,
// C promoted, H settles. A polls FIRST in every round, so only the promote's
// effective key keeps A from taking the slot ahead of C.
func TestPromote_PromotedWaiterAdmittedNextThenFIFO(t *testing.T) {
	f := newFixture(t)
	h, a, b, c := f.stage(), f.stage(), f.stage(), f.stage()
	f.queueBehindHolder(h, a, b, c)
	before := f.rowJSON(h.ID)
	cBefore, _ := f.slot(c.ID)
	p := f.mustPromote(c.ID, 3)
	if p.RunID != c.RunID || p.GroupKey != testGroup || p.Limit != 1 || len(p.Holders) != 1 || p.Holders[0].StageID != h.ID {
		t.Fatalf("promotion = %+v, want C's run, the group, limit 1, holder H", p)
	}
	if after := f.rowJSON(h.ID); after != before {
		t.Fatalf("holder row changed by a promote:\n before %s\n after  %s", before, after)
	}
	if cAfter, _ := f.slot(c.ID); !cAfter.enqueuedAt.Equal(cBefore.enqueuedAt) || !cAfter.lastSeenAt.Equal(cBefore.lastSeenAt) {
		t.Fatalf("C enqueued_at/last_seen_at %v/%v -> %v/%v, want untouched", cBefore.enqueuedAt, cBefore.lastSeenAt, cAfter.enqueuedAt, cAfter.lastSeenAt)
	}
	f.to(h.ID, run.StageStateRunning, run.StageStateSucceeded)

	// Round 1: A first, then C, then B.
	if q := f.admit(a, 1); q.Admitted {
		t.Fatal("A admitted ahead of the promoted C")
	} else if q.Position != 2 {
		t.Fatalf("A position = %d, want 2 (behind the promoted C)", q.Position)
	}
	adm := f.mustAdmit(c)
	if !adm.QueuedBefore || !adm.EnqueuedAt.Equal(cBefore.enqueuedAt) {
		t.Fatalf("C admission QueuedBefore=%v enqueued_at=%v, want the original episode %v", adm.QueuedBefore, adm.EnqueuedAt, cBefore.enqueuedAt)
	}
	if k := f.orderKey(c.ID); k != nil {
		t.Fatalf("C order_key after admission = %v, want NULL (the held mark ends the promotion)", k)
	}
	f.mustQueue(b, 2)

	// Round 2: strict FIFO again.
	f.to(c.ID, run.StageStateRunning, run.StageStateSucceeded)
	f.mustAdmit(a)
	f.mustQueue(b, 1)
	f.to(a.ID, run.StageStateRunning, run.StageStateSucceeded)
	f.mustAdmit(b)
}

// TestPromote_NewestPromotionWins is approval condition 4: promote C, then
// promote B -> B, C, A.
func TestPromote_NewestPromotionWins(t *testing.T) {
	f := newFixture(t)
	h, a, b, c := f.stage(), f.stage(), f.stage(), f.stage()
	f.queueBehindHolder(h, a, b, c)
	f.mustPromote(c.ID, 3)
	f.mustPromote(b.ID, 3)
	st := f.positions(a.ID, b.ID, c.ID)
	if st[b.ID].Position != 1 || st[c.ID].Position != 2 || st[a.ID].Position != 3 {
		t.Fatalf("positions B=%d C=%d A=%d, want 1/2/3 (the newest promote wins)", st[b.ID].Position, st[c.ID].Position, st[a.ID].Position)
	}
	if !st[b.ID].Promoted || !st[c.ID].Promoted || st[a.ID].Promoted {
		t.Fatalf("promoted B=%v C=%v A=%v, want true/true/false", st[b.ID].Promoted, st[c.ID].Promoted, st[a.ID].Promoted)
	}
}

// TestPromote_HeldStageRefusedNeverPreempted: a holder is refused with
// ErrSlotHeld and every held row stays byte-identical.
func TestPromote_HeldStageRefusedNeverPreempted(t *testing.T) {
	f := newFixture(t)
	h, a := f.stage(), f.stage()
	f.queueBehindHolder(h, a)
	f.to(h.ID, run.StageStateRunning)
	before := f.rowJSON(h.ID)
	p, err := f.promote(h.ID)
	if !errors.Is(err, ErrSlotHeld) {
		t.Fatalf("Promote(holder) = %+v, %v; want ErrSlotHeld", p, err)
	}
	if after := f.rowJSON(h.ID); after != before {
		t.Fatalf("held row changed by a refused promote:\n before %s\n after  %s", before, after)
	}
	if k := f.orderKey(a.ID); k != nil {
		t.Fatalf("A order_key = %v after a refused promote, want NULL", k)
	}
}

// TestPromote_UnknownAndUnqueuedRefused: one assertion per refusal mode.
func TestPromote_UnknownAndUnqueuedRefused(t *testing.T) {
	t.Run("unknown stage", func(t *testing.T) {
		f := newFixture(t)
		if _, err := f.promote(uuid.New()); !errors.Is(err, run.ErrNotFound) {
			t.Fatalf("Promote(random) err = %v, want run.ErrNotFound", err)
		}
	})
	t.Run("no slot row", func(t *testing.T) {
		f := newFixture(t)
		st := f.stage()
		if _, err := f.promote(st.ID); !errors.Is(err, ErrNotQueued) {
			t.Fatalf("Promote(no row) err = %v, want ErrNotQueued", err)
		}
		if _, ok := f.slot(st.ID); ok {
			t.Fatal("a refused promote wrote a slot row")
		}
	})
	t.Run("queued row whose stage was cancelled", func(t *testing.T) {
		f := newFixture(t)
		h, a, b := f.stage(), f.stage(), f.stage()
		f.queueBehindHolder(h, a, b)
		f.to(b.ID, run.StageStateCancelled)
		if _, err := f.promote(b.ID); !errors.Is(err, ErrNotQueued) {
			t.Fatalf("Promote(cancelled) err = %v, want ErrNotQueued", err)
		}
		if k := f.orderKey(b.ID); k != nil {
			t.Fatalf("cancelled B order_key = %v, want NULL", k)
		}
	})
	t.Run("previous-episode held row not yet re-polled", func(t *testing.T) {
		f := newFixture(t)
		h, a := f.stage(), f.stage()
		f.queueBehindHolder(h, a)
		// Re-open H for host dispatch; it has not polled, so its row is the
		// previous episode's held row. Seed it fresh and BEHIND A by
		// construction, so only the classification refuses it.
		f.to(h.ID, run.StageStateRunning, run.StageStateAwaitingApproval, run.StageStatePending, run.StageStateAwaitingHostDispatch)
		f.exec(`UPDATE stage_concurrency_slots SET enqueued_at = clock_timestamp(), last_seen_at = clock_timestamp() WHERE stage_id = $1`, h.ID)
		before := f.rowJSON(h.ID)
		if _, err := f.promote(h.ID); !errors.Is(err, ErrNotQueued) {
			t.Fatalf("Promote(previous-episode held) err = %v, want ErrNotQueued", err)
		}
		if after := f.rowJSON(h.ID); after != before {
			t.Fatalf("held row changed:\n before %s\n after  %s", before, after)
		}
	})
}

// TestPromote_StaleWaiterRefused: a stale waiter counts for nobody and would
// restart at the tail, so it is refused and its row is untouched.
func TestPromote_StaleWaiterRefused(t *testing.T) {
	f := newFixture(t)
	h, a, c := f.stage(), f.stage(), f.stage()
	f.queueBehindHolder(h, a, c)
	f.staleQueued(c.ID)
	if _, err := f.promote(c.ID); !errors.Is(err, ErrWaiterNotLive) {
		t.Fatalf("Promote(stale) err = %v, want ErrWaiterNotLive", err)
	}
	if k := f.orderKey(c.ID); k != nil {
		t.Fatalf("stale C order_key = %v, want NULL", k)
	}
}

// TestPromote_SlotChangedOrVanishedUnderPeek: the row is re-keyed (or
// deleted) between the unlocked peek and the locked re-read.
func TestPromote_SlotChangedOrVanishedUnderPeek(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate string
		want   error
	}{
		"re-keyed into another group": {`UPDATE stage_concurrency_slots SET group_key = 'local-implement:h2' WHERE stage_id = $1`, ErrSlotChanged},
		"row deleted":                 {`DELETE FROM stage_concurrency_slots WHERE stage_id = $1`, ErrNotQueued},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			h, a, c := f.stage(), f.stage(), f.stage()
			f.queueBehindHolder(h, a, c)
			ps := NewPostgresStore(f.pool)
			ps.afterPromotePeek = func(context.Context) { f.exec(tc.mutate, c.ID) }
			rec := &recorder{}
			if _, err := ps.Promote(context.Background(), PromoteRequest{StageID: c.ID, Record: rec.record}); !errors.Is(err, tc.want) {
				t.Fatalf("Promote err = %v, want %v", err, tc.want)
			}
			if k := f.orderKey(c.ID); k != nil || rec.count() != 0 {
				t.Fatalf("order_key = %v, Record calls = %d; want NULL and 0", k, rec.count())
			}
		})
	}
}

// TestPromote_WaitsForGroupLock: with the group lock held elsewhere, Promote
// blocks on it (an advisory-lock waiter is visible) and writes nothing until
// the lock is released.
func TestPromote_WaitsForGroupLock(t *testing.T) {
	f := newFixture(t)
	h, a, c := f.stage(), f.stage(), f.stage()
	f.queueBehindHolder(h, a, c)
	release := f.holdGroupLock()
	var p Promotion
	var err error
	done := make(chan struct{})
	go func() { defer close(done); p, err = f.promote(c.ID) }()
	waiting := f.awaitAdvisoryWaiter(done)
	k := f.orderKey(c.ID)
	release()
	<-done
	if !waiting {
		t.Fatalf("Promote returned (%+v, %v) while the group lock was held, want it to wait", p, err)
	}
	if k != nil {
		t.Fatalf("C order_key = %v while the group lock was held, want NULL", k)
	}
	if err != nil || !p.Changed || p.NewPosition != 1 {
		t.Fatalf("Promote after release = %+v, %v; want changed to position 1", p, err)
	}
	if f.orderKey(c.ID) == nil {
		t.Fatal("C order_key NULL after the promote committed")
	}
}

// TestPromote_RacingAdmitNeverDoubleAdmits: A's admission parks holding the
// group lock (admit decided) while Promote(C) starts. Promote must not write
// mid-round; after A commits, C is promoted past B, and exactly one held row
// is live.
func TestPromote_RacingAdmitNeverDoubleAdmits(t *testing.T) {
	f := newFixture(t)
	h, a, b, c := f.stage(), f.stage(), f.stage(), f.stage()
	f.queueBehindHolder(h, a, b, c)
	f.to(h.ID, run.StageStateRunning, run.StageStateSucceeded)

	parked, resume := make(chan struct{}), make(chan struct{})
	as := NewPostgresStore(f.pool)
	as.afterDecision = func(_ context.Context, admit bool) {
		if admit {
			close(parked)
			<-resume
		}
	}
	var aa Admission
	var aerr error
	adone := make(chan struct{})
	go func() { defer close(adone); aa, aerr = as.Admit(context.Background(), req(a, 1)) }()
	<-parked

	var p Promotion
	var perr error
	pdone := make(chan struct{})
	go func() { defer close(pdone); p, perr = f.promote(c.ID) }()
	waiting := f.awaitAdvisoryWaiter(pdone)
	k := f.orderKey(c.ID)
	close(resume)
	<-adone
	<-pdone
	if !waiting || k != nil {
		t.Fatalf("Promote while A held the group lock mid-round: waited=%v order_key=%v, want it to wait with order_key NULL", waiting, k)
	}
	if aerr != nil || !aa.Admitted {
		t.Fatalf("A = %+v, %v; want admitted", aa, aerr)
	}
	if perr != nil || !p.Changed || p.OldPosition != 2 || p.NewPosition != 1 || len(p.Holders) != 1 || p.Holders[0].StageID != a.ID {
		t.Fatalf("Promote = %+v, %v; want changed 2 -> 1 with holder A", p, perr)
	}
	var held int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM stage_concurrency_slots WHERE state = 'held' AND group_key = $1`, testGroup).Scan(&held); err != nil {
		t.Fatalf("count held: %v", err)
	}
	if held != 2 { // H's settled row + A's live one
		t.Fatalf("held rows = %d, want 2 (H settled, A live)", held)
	}
	if q := f.mustQueue(c, 1); len(q.Holders) != 1 {
		t.Fatalf("C holders = %+v, want [A]", q.Holders)
	}
	f.mustQueue(b, 2)
}

// TestPromote_StressNeverExceedsLimit is a regression guard: concurrent
// admits and promotes never admit more than Limit stages.
func TestPromote_StressNeverExceedsLimit(t *testing.T) {
	f := newFixture(t)
	h := f.stage()
	stages := make([]*run.Stage, 8)
	for i := range stages {
		stages[i] = f.stage()
	}
	f.queueBehindHolder(h, stages...)
	f.to(h.ID, run.StageStateRunning, run.StageStateSucceeded)
	var wg sync.WaitGroup
	errs := make(chan error, 3*len(stages))
	for i := range stages {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			admitted := false
			admit := func() {
				if admitted {
					return
				}
				a, err := f.store.Admit(context.Background(), req(stages[i], 1))
				if err != nil {
					errs <- err
				}
				admitted = a.Admitted
			}
			admit()
			_, err := f.store.Promote(context.Background(), PromoteRequest{StageID: stages[(i+1)%len(stages)].ID, Record: (&recorder{}).record})
			if err != nil && !errors.Is(err, ErrSlotHeld) && !errors.Is(err, ErrNotQueued) {
				errs <- err
			}
			admit()
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent call: %v", err)
	}
	var dispatched int
	ids := make([]uuid.UUID, len(stages))
	for i, st := range stages {
		ids[i] = st.ID
	}
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM stages WHERE id = ANY($1) AND state = 'dispatched'`, ids).Scan(&dispatched); err != nil {
		t.Fatalf("count dispatched: %v", err)
	}
	if dispatched > 1 {
		t.Fatalf("dispatched stages = %d, want <= Limit (1)", dispatched)
	}
}

// TestPromote_RecordErrorRollsBack: a failing Record rolls the promotion
// back; the committed order_key stays NULL.
func TestPromote_RecordErrorRollsBack(t *testing.T) {
	f := newFixture(t)
	h, a, c := f.stage(), f.stage(), f.stage()
	f.queueBehindHolder(h, a, c)
	boom := errors.New("audit append failed")
	rec := &recorder{err: boom}
	if _, err := f.store.Promote(context.Background(), PromoteRequest{StageID: c.ID, Record: rec.record}); !errors.Is(err, boom) {
		t.Fatalf("Promote err = %v, want the Record error", err)
	}
	if rec.count() != 1 {
		t.Fatalf("Record calls = %d, want 1", rec.count())
	}
	if rec.calls[0].OldPosition != 2 || rec.calls[0].NewPosition != 1 || !rec.calls[0].Changed {
		t.Fatalf("Record saw %+v, want changed 2 -> 1", rec.calls[0])
	}
	if k := f.orderKey(c.ID); k != nil {
		t.Fatalf("C order_key after a failed Record = %v, want NULL (rolled back)", k)
	}
	if st := f.positions(c.ID); st[c.ID].Position != 2 || st[c.ID].Promoted {
		t.Fatalf("C status after rollback = %+v, want position 2, not promoted", st[c.ID])
	}
}

// TestPromote_RecordRequired: a nil Record or stage id is refused before any
// write.
func TestPromote_RecordRequired(t *testing.T) {
	f := newFixture(t)
	h, a, c := f.stage(), f.stage(), f.stage()
	f.queueBehindHolder(h, a, c)
	if _, err := f.store.Promote(context.Background(), PromoteRequest{StageID: c.ID}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Promote(nil Record) err = %v, want ErrInvalidRequest", err)
	}
	if k := f.orderKey(c.ID); k != nil {
		t.Fatalf("C order_key after a refused promote = %v, want NULL", k)
	}
	if _, err := f.store.Promote(context.Background(), PromoteRequest{Record: (&recorder{}).record}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Promote(nil stage) err = %v, want ErrInvalidRequest", err)
	}
}

// TestPromote_EpisodeRestartClearsPromotion: a promoted waiter that goes
// stale restarts at the tail on its next poll (here a lock miss), with its
// promotion cleared.
func TestPromote_EpisodeRestartClearsPromotion(t *testing.T) {
	f := newFixture(t)
	h, a, b, c := f.stage(), f.stage(), f.stage(), f.stage()
	f.queueBehindHolder(h, a, b, c)
	f.mustPromote(c.ID, 3)
	f.staleQueued(c.ID)
	release := f.holdGroupLock()
	miss := f.admit(c, 1)
	release()
	if !miss.Contended || !miss.NewlyQueued || miss.Position != 3 {
		t.Fatalf("C after restart: %+v, want contended, newly queued at the tail (3)", miss)
	}
	if k := f.orderKey(c.ID); k != nil {
		t.Fatalf("C order_key after an episode restart = %v, want NULL", k)
	}
	if st := f.positions(c.ID); st[c.ID].Promoted || st[c.ID].Position != 3 {
		t.Fatalf("C status = %+v, want position 3, not promoted", st[c.ID])
	}
}

// TestPromote_AlreadyFirstIsNoop: the head waiter's promote writes nothing
// and records nothing.
func TestPromote_AlreadyFirstIsNoop(t *testing.T) {
	f := newFixture(t)
	h, a, b := f.stage(), f.stage(), f.stage()
	f.queueBehindHolder(h, a, b)
	rec := &recorder{}
	p, err := f.store.Promote(context.Background(), PromoteRequest{StageID: a.ID, Record: rec.record})
	if err != nil || p.Changed || p.OldPosition != 1 || p.NewPosition != 1 || p.Holders == nil {
		t.Fatalf("Promote(head) = %+v, %v; want an unchanged 1 -> 1 with non-nil holders", p, err)
	}
	if rec.count() != 0 {
		t.Fatalf("Record calls = %d on a no-op, want 0", rec.count())
	}
	if k := f.orderKey(a.ID); k != nil {
		t.Fatalf("A order_key after a no-op = %v, want NULL", k)
	}
}

// TestStatusForStages_PromotedAndPositions: positions follow the effective
// key, Promoted is set only on the promoted queued row, and a promote in
// another account's queue under the same group key moves neither queue's
// other waiters.
func TestStatusForStages_PromotedAndPositions(t *testing.T) {
	f := newFixture(t)
	acct := uuid.New()
	f.exec(`INSERT INTO accounts (id, account_key) VALUES ($1, $2)`, acct, "cc-"+acct.String())
	h, a, b, c, h2, d, e := f.stage(), f.stage(), f.stage(), f.stage(), f.stage(), f.stage(), f.stage()
	f.exec(`UPDATE runs SET account_id = $1 WHERE id IN ($2, $3, $4)`, acct, h2.RunID, d.RunID, e.RunID)
	f.queueBehindHolder(h, a, b, c)
	f.mustAdmit(h2)
	f.mustQueue(d, 1)
	f.mustQueue(e, 2)
	f.mustPromote(c.ID, 3)
	if p := f.mustPromote(e.ID, 2); len(p.Holders) != 1 || p.Holders[0].StageID != h2.ID {
		t.Fatalf("account-2 promotion holders = %+v, want only H2", p.Holders)
	}

	st := f.positions(h.ID, a.ID, b.ID, c.ID, d.ID, e.ID)
	for _, w := range []struct {
		id       uuid.UUID
		pos      int
		promoted bool
	}{{c.ID, 1, true}, {a.ID, 2, false}, {b.ID, 3, false}, {e.ID, 1, true}, {d.ID, 2, false}} {
		if got := st[w.id]; got.State != SlotQueued || got.Position != w.pos || got.Promoted != w.promoted {
			t.Errorf("status of %s = %+v, want queued at %d promoted=%v", w.id, got, w.pos, w.promoted)
		}
	}
	if st[h.ID].State != SlotHeld || st[h.ID].Promoted {
		t.Errorf("holder status = %+v, want held and never promoted", st[h.ID])
	}
}

// faultTracer cancels the context of the nth (1-based) statement whose SQL
// contains match, so exactly that statement fails before it is sent.
type faultTracer struct {
	mu    sync.Mutex
	match string
	nth   int
	seen  int
	fired bool
}

func (ft *faultTracer) arm(match string, nth int) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.match, ft.nth, ft.seen, ft.fired = match, nth, 0, false
}

func (ft *faultTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if ft.match == "" || !strings.Contains(data.SQL, ft.match) {
		return ctx
	}
	ft.seen++
	if ft.seen != ft.nth {
		return ctx
	}
	ft.fired = true
	failed, cancel := context.WithCancel(ctx)
	cancel()
	return failed
}

func (*faultTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestPromoteAndAdmit_StatementFailureRollsBack fails each statement of a
// promote (and the two upsert statements of an admit) in turn: every
// failure surfaces as an error and commits nothing.
func TestPromoteAndAdmit_StatementFailureRollsBack(t *testing.T) {
	cfg, err := pgxpool.ParseConfig(pgtest.NewURL(t))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	ft := &faultTracer{}
	cfg.ConnConfig.Tracer = ft
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	f := &fixture{t: t, pool: pool, repo: run.NewPostgresRepository(pool), store: NewPostgresStore(pool)}
	h, a, c := f.stage(), f.stage(), f.stage()
	f.queueBehindHolder(h, a, c)

	for _, tc := range []struct {
		name, match string
		nth         int
	}{
		{"read stage", "LEFT JOIN stage_concurrency_slots", 1},
		{"group lock", "pg_advisory_xact_lock", 1},
		{"lock stage", "SELECT state FROM stages WHERE id = $1 FOR UPDATE", 1},
		{"lock slot row", "FROM stage_concurrency_slots WHERE stage_id = $1 FOR UPDATE", 1},
		{"list holders", "ORDER BY s.acquired_at, s.stage_id", 1},
		{"old position", "round_ahead", 1},
		{"write order key", "SET order_key = (", 1},
		{"new position", "round_ahead", 2},
	} {
		t.Run("promote/"+tc.name, func(t *testing.T) {
			rec := &recorder{}
			ft.arm(tc.match, tc.nth)
			_, err := f.store.Promote(context.Background(), PromoteRequest{StageID: c.ID, Record: rec.record})
			ft.mu.Lock()
			fired := ft.fired
			ft.mu.Unlock()
			ft.arm("", 0)
			if !fired {
				t.Fatalf("fixture: no statement matched %q #%d", tc.match, tc.nth)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Promote err = %v, want the failed statement's error", err)
			}
			if k := f.orderKey(c.ID); k != nil || rec.count() != 0 {
				t.Fatalf("after a failed statement: order_key = %v, Record calls = %d; want NULL and 0", k, rec.count())
			}
		})
	}

	for _, tc := range []struct{ name, match string }{
		{"read slot row", "SELECT true,"},
		{"upsert slot row", "ON CONFLICT (stage_id) DO UPDATE"},
	} {
		t.Run("admit/"+tc.name, func(t *testing.T) {
			fresh := f.stage()
			ft.arm(tc.match, 1)
			_, err := f.store.Admit(context.Background(), req(fresh, 1))
			ft.arm("", 0)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Admit err = %v, want the failed statement's error", err)
			}
			if _, ok := f.slot(fresh.ID); ok {
				t.Fatal("a failed admission committed a slot row")
			}
		})
	}
	// The fixture survives every injected failure: the real promote works.
	f.mustPromote(c.ID, 2)
}
