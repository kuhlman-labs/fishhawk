package concurrency

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// Set-wide Promote proofs (#4191 option a for ADR-094 D2 sets, #4240 slice
// 1b). Every verdict is read back from COMMITTED slot rows after the call
// returns. The group keys and set helpers are multigroup_test.go's.

// blockGroupLock takes one group's advisory lock (the untenanted scope the
// fixture's runs live in) inside a separate open transaction, so a promote
// that needs it waits; the returned func commits that transaction.
func (f *fixture) blockGroupLock(group string) func() {
	f.t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		f.t.Fatalf("begin blocker: %v", err)
	}
	if hit, err := TryGroupLockTx(ctx, tx, nil, group); err != nil || !hit {
		_ = tx.Rollback(ctx)
		f.t.Fatalf("blocker TryGroupLockTx(%s) = %v, %v", group, hit, err)
	}
	return func() {
		if err := tx.Commit(ctx); err != nil {
			f.t.Fatalf("commit blocker: %v", err)
		}
	}
}

// minOtherKey is the minimum effective key over every OTHER queued row in
// the listed groups.
func (f *fixture) minOtherKey(self uuid.UUID, groups ...string) time.Time {
	f.t.Helper()
	var k time.Time
	if err := f.pool.QueryRow(context.Background(), `SELECT min(COALESCE(order_key, enqueued_at))
FROM stage_concurrency_slots WHERE stage_id <> $1 AND state = 'queued' AND group_key = ANY($2::text[])`, self, groups).Scan(&k); err != nil {
		f.t.Fatalf("min other key: %v", err)
	}
	return k
}

// TestPromote_MultiGroupOneKeyAcrossSet: S is queued at position 2 in G1
// (behind A) and position 3 in G2 (behind B and C). One promote writes the
// SAME order key to both of S's rows, strictly ahead of every other waiter in
// either group, so S is first in both. Promotion.Groups carries each group's
// positions and holders; the top-level fields describe the binding group G2.
func TestPromote_MultiGroupOneKeyAcrossSet(t *testing.T) {
	f := newFixture(t)
	h1, h2, a, b, c, s := f.stage(), f.stage(), f.stage(), f.stage(), f.stage(), f.stage()
	f.mustAdmitSet(h1, gr(mg1, 1))
	f.mustAdmitSet(h2, gr(mg2, 1))
	f.mustQueueSet(a, 1, gr(mg1, 1))
	f.mustQueueSet(b, 1, gr(mg2, 1))
	f.mustQueueSet(c, 2, gr(mg2, 1))
	f.mustQueueSet(s, 3, gr(mg1, 1), gr(mg2, 1))
	before := f.setRows(s.ID)

	rec := &recorder{}
	p, err := f.store.Promote(context.Background(), PromoteRequest{StageID: s.ID, Record: rec.record})
	if err != nil {
		t.Fatalf("Promote(S): %v", err)
	}
	if !p.Changed || p.GroupKey != mg2 || p.OldPosition != 3 || p.NewPosition != 1 || len(p.Holders) != 1 || p.Holders[0].StageID != h2.ID {
		t.Fatalf("promotion top level = %+v, want the binding group G2 changed 3 -> 1 with holder H2", p)
	}
	want := []struct {
		key      string
		old      int
		holderID uuid.UUID
	}{{mg1, 2, h1.ID}, {mg2, 3, h2.ID}}
	if len(p.Groups) != len(want) {
		t.Fatalf("Promotion.Groups = %+v, want two groups", p.Groups)
	}
	for i, w := range want {
		g := p.Groups[i]
		if g.Key != w.key || g.Limit != 1 || g.OldPosition != w.old || g.NewPosition != 1 || len(g.Holders) != 1 || g.Holders[0].StageID != w.holderID {
			t.Fatalf("Promotion.Groups[%d] = %+v, want %s %d -> 1 with holder %s", i, g, w.key, w.old, w.holderID)
		}
	}
	if rec.count() != 1 {
		t.Fatalf("Record calls = %d, want 1", rec.count())
	}

	rows := f.setRows(s.ID)
	if len(rows) != 2 || rows[0].orderKey == nil || rows[1].orderKey == nil || !rows[0].orderKey.Equal(*rows[1].orderKey) {
		t.Fatalf("S rows after promote = %+v, want ONE order_key on both rows", rows)
	}
	if other := f.minOtherKey(s.ID, mg1, mg2); !rows[0].orderKey.Before(other) {
		t.Fatalf("S order_key %v, want strictly before every other waiter's key (min %v)", *rows[0].orderKey, other)
	}
	for i, r := range rows {
		if r.state != "queued" || !r.enqueuedAt.Equal(before[i].enqueuedAt) || !r.lastSeenAt.Equal(before[i].lastSeenAt) {
			t.Fatalf("S row %s = %+v, want queued with enqueued_at/last_seen_at untouched (was %+v)", r.group, r, before[i])
		}
	}
	st := f.positions(s.ID)[s.ID]
	if !st.Promoted || st.Position != 1 || len(st.Groups) != 2 || st.Groups[0].Position != 1 || st.Groups[1].Position != 1 {
		t.Fatalf("S status = %+v, want promoted at position 1 in both groups", st)
	}
}

// TestPromote_MultiGroupNeverWedges is #4191's constraint: a promoted
// multi-group stage never holds one group while it is queued in another. X
// holds G2, and W, a G1-only waiter, is queued ahead of S on {G1, G2}.
// Promoting S puts it first in G1, which is then free for S on its own, so
// only all-or-nothing admission keeps S from holding G1 alone while X holds
// G2. W, now behind S, stays queued although nobody holds G1. X settling
// admits S into both groups; S settling admits W.
func TestPromote_MultiGroupNeverWedges(t *testing.T) {
	f := newFixture(t)
	x, w, s := f.stage(), f.stage(), f.stage()
	f.mustAdmitSet(x, gr(mg2, 1))
	f.makeContendedWaiter(w, mg1, 1)
	f.mustQueueSet(s, 2, gr(mg1, 1), gr(mg2, 1))

	p, err := f.promote(s.ID)
	if err != nil || !p.Changed || len(p.Groups) != 2 || p.Groups[0].OldPosition != 2 || p.Groups[0].NewPosition != 1 {
		t.Fatalf("Promote(S) = %+v, %v; want changed, G1 2 -> 1", p, err)
	}
	q := f.mustQueueSet(s, 1, gr(mg1, 1), gr(mg2, 1))
	if len(q.Groups) != 2 || q.Groups[0].Position != 1 || q.Groups[1].Position != 1 {
		t.Fatalf("promoted S groups = %+v, want position 1 in both", q.Groups)
	}
	if got := f.stageState(s.ID); got != run.StageStateAwaitingHostDispatch {
		t.Fatalf("S stage = %s, want awaiting_host_dispatch", got)
	}
	for _, r := range f.setRows(s.ID) {
		if r.state != "queued" || r.acquiredAt != nil || r.heldDispatchedAt != nil {
			t.Fatalf("S row %s = %+v while X holds G2, want queued (no partial hold of G1)", r.group, r)
		}
	}
	if wq := f.mustQueueSet(w, 2, gr(mg1, 1)); len(wq.Holders) != 0 {
		t.Fatalf("W G1 holders = %v, want none (S holds nothing)", holderIDs(wq.Holders))
	}

	f.to(x.ID, run.StageStateRunning, run.StageStateSucceeded)
	f.mustAdmitSet(s, gr(mg1, 1), gr(mg2, 1))
	for _, r := range f.setRows(s.ID) {
		if r.state != "held" {
			t.Fatalf("S row %s = %s after admission, want held in both groups", r.group, r.state)
		}
	}
	f.mustQueueSet(w, 1, gr(mg1, 1))

	f.to(s.ID, run.StageStateRunning, run.StageStateSucceeded)
	f.mustAdmitSet(w, gr(mg1, 1))
}

// TestPromote_MultiGroupWaitsForEveryGroupLock: another transaction holds
// ONLY G2's advisory lock, and G2 sorts second in S's set. Promote takes
// every group lock, so it blocks (an advisory-lock waiter is visible and it
// has not returned after 300 ms) and writes nothing until that transaction
// commits.
func TestPromote_MultiGroupWaitsForEveryGroupLock(t *testing.T) {
	f := newFixture(t)
	h, a, s := f.stage(), f.stage(), f.stage()
	f.mustAdmitSet(h, gr(mg1, 1))
	f.mustQueueSet(a, 1, gr(mg1, 1))
	f.mustQueueSet(s, 2, gr(mg1, 1), gr(mg2, 1))
	release := f.blockGroupLock(mg2)

	var p Promotion
	var err error
	done := make(chan struct{})
	go func() { defer close(done); p, err = f.promote(s.ID) }()
	select {
	case <-done:
		release()
		t.Fatalf("Promote returned (%+v, %v) while G2's lock was held, want it to wait", p, err)
	case <-time.After(timescale.D(300 * time.Millisecond)):
	}
	waiting := f.awaitAdvisoryWaiter(done)
	rows := f.setRows(s.ID)
	release()
	<-done
	if !waiting {
		t.Fatalf("Promote returned (%+v, %v) while G2's lock was held, want it to wait on the lock", p, err)
	}
	for _, r := range rows {
		if r.orderKey != nil {
			t.Fatalf("S row %s order_key = %v while G2's lock was held, want NULL", r.group, *r.orderKey)
		}
	}
	if err != nil || !p.Changed || p.NewPosition != 1 {
		t.Fatalf("Promote after release = %+v, %v; want changed to position 1", p, err)
	}
	after := f.setRows(s.ID)
	if len(after) != 2 || after[0].orderKey == nil || after[1].orderKey == nil || !after[0].orderKey.Equal(*after[1].orderKey) {
		t.Fatalf("S rows after the promote committed = %+v, want one order_key on both", after)
	}
}

// TestPromote_MultiGroupSetChanged: the stage's group set changes between
// the unlocked peek and the locked re-read (a row deleted, a row added).
// Promote answers ErrSlotChanged, writes no order key and calls no Record.
func TestPromote_MultiGroupSetChanged(t *testing.T) {
	for name, mutate := range map[string]string{
		"G2 row deleted": `DELETE FROM stage_concurrency_slots WHERE stage_id = $1 AND group_key = '` + mg2 + `'`,
		"G3 row added": `INSERT INTO stage_concurrency_slots (stage_id, run_id, group_key, slot_limit, host, state, enqueued_at, last_seen_at)
SELECT stage_id, run_id, '` + mg3 + `', slot_limit, host, state, enqueued_at, last_seen_at
FROM stage_concurrency_slots WHERE stage_id = $1 AND group_key = '` + mg1 + `'`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			h, a, s := f.stage(), f.stage(), f.stage()
			f.mustAdmitSet(h, gr(mg1, 1))
			f.mustQueueSet(a, 1, gr(mg1, 1))
			f.mustQueueSet(s, 2, gr(mg1, 1), gr(mg2, 1))
			ps := NewPostgresStore(f.pool)
			ps.afterPromotePeek = func(context.Context) { f.exec(mutate, s.ID) }
			rec := &recorder{}
			p, err := ps.Promote(context.Background(), PromoteRequest{StageID: s.ID, Record: rec.record})
			if !errors.Is(err, ErrSlotChanged) {
				t.Fatalf("Promote = %+v, %v; want ErrSlotChanged", p, err)
			}
			for _, r := range f.setRows(s.ID) {
				if r.orderKey != nil {
					t.Fatalf("S row %s order_key = %v, want NULL", r.group, *r.orderKey)
				}
			}
			if rec.count() != 0 {
				t.Fatalf("Record calls = %d, want 0", rec.count())
			}
		})
	}
}

// TestPromote_MultiGroupAnyStaleRowRefused: freshness is checked on EVERY
// row of the set. Only S's G2 row (which sorts second) is backdated past
// QueueTTL, seeded by construction, so a check of the first row alone would
// promote. Promote answers ErrWaiterNotLive and writes nothing.
func TestPromote_MultiGroupAnyStaleRowRefused(t *testing.T) {
	f := newFixture(t)
	h, a, s := f.stage(), f.stage(), f.stage()
	f.mustAdmitSet(h, gr(mg1, 1))
	f.mustQueueSet(a, 1, gr(mg1, 1))
	f.mustQueueSet(s, 2, gr(mg1, 1), gr(mg2, 1))
	f.exec(`UPDATE stage_concurrency_slots SET last_seen_at = clock_timestamp() - interval '61 seconds' WHERE stage_id = $1 AND group_key = '`+mg2+`'`, s.ID)
	rec := &recorder{}
	p, err := f.store.Promote(context.Background(), PromoteRequest{StageID: s.ID, Record: rec.record})
	if !errors.Is(err, ErrWaiterNotLive) {
		t.Fatalf("Promote = %+v, %v; want ErrWaiterNotLive", p, err)
	}
	for _, r := range f.setRows(s.ID) {
		if r.orderKey != nil {
			t.Fatalf("S row %s order_key = %v, want NULL", r.group, *r.orderKey)
		}
	}
	if rec.count() != 0 {
		t.Fatalf("Record calls = %d, want 0", rec.count())
	}
}
