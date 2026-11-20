package concurrency

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// Multi-group admission and status (ADR-094 D2, #4240 slice 1a). Group keys
// here are repository-scoped spec keys so G1 sorts before G2 before G3.
const (
	mg1 = "spec:o/r:g1"
	mg2 = "spec:o/r:g2"
	mg3 = "spec:o/r:g3"
)

func gr(key string, limit int) GroupRequest { return GroupRequest{Key: key, Limit: limit} }

func setReq(st *run.Stage, groups ...GroupRequest) Request {
	return Request{StageID: st.ID, RunID: st.RunID, From: run.StageStateAwaitingHostDispatch, Groups: groups, Host: "h1"}
}

func (f *fixture) admitSet(st *run.Stage, groups ...GroupRequest) Admission {
	f.t.Helper()
	a, err := f.store.Admit(context.Background(), setReq(st, groups...))
	if err != nil {
		f.t.Fatalf("Admit(%s, %v): %v", st.ID, groups, err)
	}
	return a
}

func (f *fixture) mustAdmitSet(st *run.Stage, groups ...GroupRequest) Admission {
	f.t.Helper()
	a := f.admitSet(st, groups...)
	if !a.Admitted {
		f.t.Fatalf("Admit(%s, %v) = queued at position %d (groups %+v), want admitted", st.ID, groups, a.Position, a.Groups)
	}
	return a
}

func (f *fixture) mustQueueSet(st *run.Stage, wantPos int, groups ...GroupRequest) Admission {
	f.t.Helper()
	a := f.admitSet(st, groups...)
	if a.Admitted {
		f.t.Fatalf("Admit(%s, %v) admitted, want queued at position %d", st.ID, groups, wantPos)
	}
	if a.Position != wantPos {
		f.t.Fatalf("Admit(%s, %v) position = %d, want %d (groups %+v)", st.ID, groups, a.Position, wantPos, a.Groups)
	}
	return a
}

type setRow struct {
	group            string
	state            string
	enqueuedAt       time.Time
	lastSeenAt       time.Time
	orderKey         *time.Time
	acquiredAt       *time.Time
	heldDispatchedAt *time.Time
}

// setRows reads every slot row of a stage in canonical key order.
func (f *fixture) setRows(id uuid.UUID) []setRow {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT group_key, state, enqueued_at, last_seen_at, order_key, acquired_at, held_dispatched_at
FROM stage_concurrency_slots WHERE stage_id = $1 ORDER BY group_key COLLATE "C"`, id)
	if err != nil {
		f.t.Fatalf("read slot rows: %v", err)
	}
	defer rows.Close()
	var out []setRow
	for rows.Next() {
		var r setRow
		if err := rows.Scan(&r.group, &r.state, &r.enqueuedAt, &r.lastSeenAt, &r.orderKey, &r.acquiredAt, &r.heldDispatchedAt); err != nil {
			f.t.Fatalf("scan slot row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("slot rows: %v", err)
	}
	return out
}

// makeContendedWaiter makes st a fresh queued SINGLE-group waiter on group
// without deciding: another transaction holds the group's advisory lock
// (TryGroupLockTx), so st's Admit answers Contended; the blocker then commits.
func (f *fixture) makeContendedWaiter(st *run.Stage, group string, limit int) {
	f.t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		f.t.Fatalf("begin blocker: %v", err)
	}
	if hit, err := TryGroupLockTx(ctx, tx, nil, group); err != nil || !hit {
		_ = tx.Rollback(ctx)
		f.t.Fatalf("blocker TryGroupLockTx = %v, %v", hit, err)
	}
	a, err := f.store.Admit(ctx, setReq(st, gr(group, limit)))
	if err != nil || a.Admitted || !a.Contended {
		_ = tx.Rollback(ctx)
		f.t.Fatalf("fixture: contended Admit = %+v, %v; want a contended queue", a, err)
	}
	if err := tx.Commit(ctx); err != nil {
		f.t.Fatalf("commit blocker: %v", err)
	}
}

func holderIDs(hs []Holder) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(hs))
	for _, h := range hs {
		out = append(out, h.StageID)
	}
	return out
}

func hasHolder(hs []Holder, id uuid.UUID) bool {
	for _, h := range hs {
		if h.StageID == id {
			return true
		}
	}
	return false
}

func TestAdmitSet_BothFreeAdmitsEveryGroup(t *testing.T) {
	f := newFixture(t)
	s := f.stage()
	adm := f.mustAdmitSet(s, gr(mg2, 1), gr(mg1, 1))
	if len(adm.Groups) != 2 || adm.Groups[0].Key != mg1 || adm.Groups[1].Key != mg2 || adm.Groups[0].Scope != ScopeRepository {
		t.Fatalf("Admission.Groups = %+v, want [g1 g2] in canonical order with scope repository", adm.Groups)
	}
	rows := f.setRows(s.ID)
	if len(rows) != 2 || rows[0].state != "held" || rows[1].state != "held" {
		t.Fatalf("slot rows = %+v, want two held rows", rows)
	}
	st := f.positions(s.ID)[s.ID]
	if st.State != SlotHeld || len(st.Groups) != 2 {
		t.Fatalf("status = %+v, want held with two groups", st)
	}
	for _, g := range st.Groups {
		if !hasHolder(g.Holders, s.ID) {
			t.Fatalf("status group %s holders = %v, want S listed", g.Key, holderIDs(g.Holders))
		}
	}
}

func TestAdmitSet_OneGroupFullHoldsNeither(t *testing.T) {
	for name, tc := range map[string]struct{ full, free string }{
		"full group sorts last":  {full: mg2, free: mg1},
		"full group sorts first": {full: mg1, free: mg2},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			x, s := f.stage(), f.stage()
			f.mustAdmitSet(x, gr(tc.full, 1))
			q := f.mustQueueSet(s, 1, gr(mg1, 1), gr(mg2, 1))
			if q.Groups[0].Key != mg1 || q.Groups[1].Key != mg2 {
				t.Fatalf("groups = %+v, want canonical order", q.Groups)
			}
			if got := f.stageState(s.ID); got != run.StageStateAwaitingHostDispatch {
				t.Fatalf("S stage = %s, want awaiting_host_dispatch", got)
			}
			rows := f.setRows(s.ID)
			if len(rows) != 2 {
				t.Fatalf("S rows = %+v, want 2", rows)
			}
			for _, r := range rows {
				if r.state != "queued" || r.acquiredAt != nil || r.heldDispatchedAt != nil {
					t.Fatalf("S row %s = %+v, want queued with acquired_at and held_dispatched_at NULL (no partial hold)", r.group, r)
				}
			}
			// A probe on the free group queues behind S (a fresh waiter there)
			// and sees no holder: S holds nothing.
			if probe := f.admitSet(f.stage(), gr(tc.free, 1)); probe.Admitted || len(probe.Holders) != 0 {
				t.Fatalf("free-group probe = admitted:%v holders %v, want queued behind S with S absent from the holders", probe.Admitted, holderIDs(probe.Holders))
			}
		})
	}
}

func TestAdmitSet_StrictPerGroupFIFO(t *testing.T) {
	f := newFixture(t)
	h, w1, w2 := f.stage(), f.stage(), f.stage()
	f.mustAdmitSet(h, gr(mg2, 1))
	f.mustQueueSet(w1, 1, gr(mg1, 1), gr(mg2, 1))
	q := f.mustQueueSet(w2, 2, gr(mg1, 1))
	if len(q.Holders) != 0 {
		t.Fatalf("W2 G1 holders = %v, want none (blocked only by the earlier waiter)", holderIDs(q.Holders))
	}
	if rows := f.setRows(w2.ID); len(rows) != 1 || rows[0].state != "queued" {
		t.Fatalf("W2 rows = %+v, want one queued row", rows)
	}
	f.to(h.ID, run.StageStateRunning, run.StageStateSucceeded)
	f.mustQueueSet(w2, 2, gr(mg1, 1)) // W1 is still ahead
	f.mustAdmitSet(w1, gr(mg1, 1), gr(mg2, 1))
	f.mustQueueSet(w2, 1, gr(mg1, 1))
	f.to(w1.ID, run.StageStateRunning, run.StageStateSucceeded)
	f.mustAdmitSet(w2, gr(mg1, 1))
}

// TestAdmitSet_MultiGroupEarlierWaiterBlocksAtLimit2 (R2): condition (a) is
// 0 + 0 + 1 < 2 for W2, so only condition (b) can block it.
func TestAdmitSet_MultiGroupEarlierWaiterBlocksAtLimit2(t *testing.T) {
	f := newFixture(t)
	h, w1, w2 := f.stage(), f.stage(), f.stage()
	f.mustAdmitSet(h, gr(mg2, 1))
	f.mustQueueSet(w1, 1, gr(mg1, 2), gr(mg2, 1))
	q := f.mustQueueSet(w2, 2, gr(mg1, 2))
	if len(q.Holders) != 0 {
		t.Fatalf("W2 G1 holders = %v, want zero (G1 is empty)", holderIDs(q.Holders))
	}
	if rows := f.setRows(w2.ID); len(rows) != 1 || rows[0].state != "queued" {
		t.Fatalf("W2 rows = %+v, want one queued row, not held", rows)
	}
	f.to(h.ID, run.StageStateRunning, run.StageStateSucceeded)
	f.mustAdmitSet(w1, gr(mg1, 2), gr(mg2, 1))
	adm := f.mustAdmitSet(w2, gr(mg1, 2))
	if len(adm.Holders) != 1 || adm.Holders[0].StageID != w1.ID {
		t.Fatalf("W2 admitted beside holders %v, want [W1] (one holder of two)", holderIDs(adm.Holders))
	}
}

// TestAdmitSet_SingleGroupQueuesUnchangedByMultiGroupRule (R2 inverse): with
// every waiter single-group, condition (b) is vacuous and ADR-087 decides.
func TestAdmitSet_SingleGroupQueuesUnchangedByMultiGroupRule(t *testing.T) {
	for _, limit := range []int{2, 1} {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			f := newFixture(t)
			w1, w2 := f.stage(), f.stage()
			f.makeContendedWaiter(w1, mg1, limit)
			if limit == 2 {
				f.mustAdmitSet(w2, gr(mg1, limit)) // 0 + 0 + 1 < 2
				adm := f.mustAdmitSet(w1, gr(mg1, limit))
				if len(adm.Holders) != 1 || adm.Holders[0].StageID != w2.ID {
					t.Fatalf("W1 admitted beside %v, want [W2] (two holders of two)", holderIDs(adm.Holders))
				}
				return
			}
			f.mustQueueSet(w2, 2, gr(mg1, limit))
			f.mustAdmitSet(w1, gr(mg1, limit))
		})
	}
}

// TestAdmitSet_MultiGroupLaterWaiterBehindSingleGroupAdmitted is the S2
// mixed case: a SINGLE-group earlier waiter at limit 2 with a free unreserved
// slot does not block a later MULTI-group waiter.
func TestAdmitSet_MultiGroupLaterWaiterBehindSingleGroupAdmitted(t *testing.T) {
	f := newFixture(t)
	w1, w2 := f.stage(), f.stage()
	f.makeContendedWaiter(w1, mg1, 2)
	f.mustAdmitSet(w2, gr(mg1, 2), gr(mg3, 1))
	rows := f.setRows(w2.ID)
	if len(rows) != 2 || rows[0].state != "held" || rows[1].state != "held" ||
		rows[0].acquiredAt == nil || rows[1].acquiredAt == nil || !rows[0].acquiredAt.Equal(*rows[1].acquiredAt) {
		t.Fatalf("W2 rows = %+v, want two held rows with one acquired_at", rows)
	}
	adm := f.mustAdmitSet(w1, gr(mg1, 2))
	if len(adm.Holders) != 1 || adm.Holders[0].StageID != w2.ID {
		t.Fatalf("W1 admitted beside %v, want [W2] (G1 holds two of two)", holderIDs(adm.Holders))
	}
}

func TestAdmitSet_ShorthandEqualsOneElementSet(t *testing.T) {
	f := newFixture(t)
	h, a, b := f.stage(), f.stage(), f.stage()
	f.mustAdmit(h)
	qa := f.mustQueue(a, 1)
	qb := f.mustQueueSet(b, 2, gr(testGroup, 1))
	if len(qa.Groups) != 1 || len(qb.Groups) != 1 || qa.Groups[0].Key != qb.Groups[0].Key || qa.Groups[0].Limit != qb.Groups[0].Limit ||
		qa.Groups[0].Scope != ScopeHost || qa.NewlyQueued != qb.NewlyQueued || qa.Contended != qb.Contended {
		t.Fatalf("shorthand %+v vs set %+v, want identical verdict shape", qa, qb)
	}
	ra, rb := f.setRows(a.ID), f.setRows(b.ID)
	if len(ra) != 1 || len(rb) != 1 || ra[0].group != rb[0].group || ra[0].state != rb[0].state {
		t.Fatalf("rows shorthand %+v vs set %+v, want one identical row each", ra, rb)
	}
}

func TestAdmitSet_SetChangeRestartsWholeSetAtTail(t *testing.T) {
	f := newFixture(t)
	h1, h2, s, later := f.stage(), f.stage(), f.stage(), f.stage()
	f.mustAdmitSet(h1, gr(mg1, 1))
	f.mustAdmitSet(h2, gr(mg2, 1))
	first := f.mustQueueSet(s, 1, gr(mg1, 1), gr(mg2, 1))
	f.mustQueueSet(later, 2, gr(mg2, 1))

	q := f.mustQueueSet(s, 1, gr(mg1, 1))
	if !q.NewlyQueued || !q.EnqueuedAt.After(first.EnqueuedAt) {
		t.Fatalf("re-asked set NewlyQueued=%v enqueued %v (was %v), want restarted at the tail", q.NewlyQueued, q.EnqueuedAt, first.EnqueuedAt)
	}
	if rows := f.setRows(s.ID); len(rows) != 1 || rows[0].group != mg1 {
		t.Fatalf("S rows after re-asking {G1} = %+v, want only the G1 row", rows)
	}
	// S no longer counts ahead of G2's waiters.
	f.mustQueueSet(later, 1, gr(mg2, 1))
}

func manyGroups(n int) []GroupRequest {
	out := make([]GroupRequest, n)
	for i := range out {
		out[i] = gr(fmt.Sprintf("spec:o/r:m%02d", i), 1)
	}
	return out
}

func assertSharedEpisode(t *testing.T, rows []setRow, n int, phase string) {
	t.Helper()
	if len(rows) != n {
		t.Fatalf("%s: %d rows, want %d", phase, len(rows), n)
	}
	for _, r := range rows[1:] {
		if !r.enqueuedAt.Equal(rows[0].enqueuedAt) || !r.lastSeenAt.Equal(rows[0].lastSeenAt) || (r.orderKey == nil) != (rows[0].orderKey == nil) ||
			(r.orderKey != nil && !r.orderKey.Equal(*rows[0].orderKey)) {
			t.Fatalf("%s: row %s = %+v differs from row %s = %+v (episode state must be shared)", phase, r.group, r, rows[0].group, rows[0])
		}
	}
}

// TestAdmitSet_RowsShareEpisodeState (R3, R7): a 16-group set (MaxGroups,
// above the schema's 8) shares one timestamp across every row in each phase.
func TestAdmitSet_RowsShareEpisodeState(t *testing.T) {
	f := newFixture(t)
	// A literal 16, not MaxGroups: the test pins that the store accepts the
	// headroom above the schema's 8, so it must not shrink with the constant.
	const setSize = 16
	groups := manyGroups(setSize)
	h, s := f.stage(), f.stage()
	f.mustAdmitSet(h, groups[len(groups)-1])

	f.mustQueueSet(s, 1, groups...)
	queued := f.setRows(s.ID)
	assertSharedEpisode(t, queued, setSize, "queued")

	f.mustQueueSet(s, 1, groups...)
	kept := f.setRows(s.ID)
	assertSharedEpisode(t, kept, setSize, "kept")
	if !kept[0].enqueuedAt.Equal(queued[0].enqueuedAt) {
		t.Fatalf("kept enqueued_at %v, want %v", kept[0].enqueuedAt, queued[0].enqueuedAt)
	}

	f.staleQueued(s.ID)
	if q := f.mustQueueSet(s, 1, groups...); !q.NewlyQueued {
		t.Fatal("stale set re-ask NewlyQueued = false, want a restart")
	}
	restarted := f.setRows(s.ID)
	assertSharedEpisode(t, restarted, setSize, "restarted")
	if !restarted[0].enqueuedAt.After(kept[0].enqueuedAt) {
		t.Fatalf("restarted enqueued_at %v, want after %v", restarted[0].enqueuedAt, kept[0].enqueuedAt)
	}

	f.to(h.ID, run.StageStateRunning, run.StageStateSucceeded)
	f.mustAdmitSet(s, groups...)
	held := f.setRows(s.ID)
	for _, r := range held {
		if r.state != "held" || r.acquiredAt == nil || r.heldDispatchedAt == nil ||
			!r.acquiredAt.Equal(*held[0].acquiredAt) || !r.heldDispatchedAt.Equal(*held[0].heldDispatchedAt) {
			t.Fatalf("held row %s = %+v vs %s = %+v, want every row held with ONE acquired_at and held_dispatched_at", r.group, r, held[0].group, held[0])
		}
	}
}

func TestAdmitSet_ReleaseFreesEveryGroup(t *testing.T) {
	t.Run("stage failed", func(t *testing.T) {
		f := newFixture(t)
		s, w1, w2 := f.stage(), f.stage(), f.stage()
		f.mustAdmitSet(s, gr(mg1, 1), gr(mg2, 1))
		f.mustQueueSet(w1, 1, gr(mg1, 1))
		f.mustQueueSet(w2, 1, gr(mg2, 1))
		f.to(s.ID, run.StageStateRunning)
		cat := run.FailureA
		if _, err := f.repo.TransitionStage(context.Background(), s.ID, run.StageStateFailed, &run.StageCompletion{FailureCategory: &cat}); err != nil {
			t.Fatalf("fail S: %v", err)
		}
		f.mustAdmitSet(w1, gr(mg1, 1))
		f.mustAdmitSet(w2, gr(mg2, 1))
	})
	t.Run("dispatched past the backstop", func(t *testing.T) {
		f := newFixture(t)
		s, w1, w2 := f.stage(), f.stage(), f.stage()
		f.mustAdmitSet(s, gr(mg1, 1), gr(mg2, 1))
		f.backdateHolder(s.ID, "16 minutes")
		for _, g := range []string{mg1, mg2} {
			if adm := f.mustAdmitSet(map[string]*run.Stage{mg1: w1, mg2: w2}[g], gr(g, 1)); hasHolder(adm.Holders, s.ID) {
				t.Fatalf("group %s holders = %v, want the stale S gone", g, holderIDs(adm.Holders))
			}
		}
	})
}

func TestAdmitSet_ContendedOnAnyGroupLock(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s := f.stage()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if hit, err := TryGroupLockTx(ctx, tx, nil, mg2); err != nil || !hit {
		t.Fatalf("TryGroupLockTx = %v, %v", hit, err)
	}
	adm := f.admitSet(s, gr(mg1, 1), gr(mg2, 1))
	if adm.Admitted || !adm.Contended {
		t.Fatalf("Admit with G2's lock held elsewhere = %+v, want a contended queue", adm)
	}
	rows := f.setRows(s.ID)
	if len(rows) != 2 || rows[0].state != "queued" || rows[1].state != "queued" {
		t.Fatalf("rows = %+v, want both queued, nothing held", rows)
	}
	if got := f.stageState(s.ID); got != run.StageStateAwaitingHostDispatch {
		t.Fatalf("stage = %s, want awaiting_host_dispatch", got)
	}
}

func TestAdmitSet_ValidateRefusesBeforeSQL(t *testing.T) {
	f := newFixture(t)
	s := f.stage()
	cases := map[string]func(*Request){
		"both forms":    func(r *Request) { r.GroupKey, r.Limit = mg1, 1 },
		"empty set":     func(r *Request) { r.Groups = []GroupRequest{} },
		"17 groups":     func(r *Request) { r.Groups = manyGroups(17) },
		"empty key":     func(r *Request) { r.Groups = []GroupRequest{gr(mg1, 1), gr("", 1)} },
		"duplicate key": func(r *Request) { r.Groups = []GroupRequest{gr(mg1, 1), gr(mg1, 2)} },
		"limit 0":       func(r *Request) { r.Groups = []GroupRequest{gr(mg1, 0)} },
		"limit 65":      func(r *Request) { r.Groups = []GroupRequest{gr(mg1, MaxLimit+1)} },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			r := setReq(s, gr(mg1, 1), gr(mg2, 1))
			mut(&r)
			if _, err := f.store.Admit(context.Background(), r); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
			if rows := f.setRows(s.ID); len(rows) != 0 {
				t.Fatalf("refused request wrote slot rows %+v", rows)
			}
		})
	}
}

func TestStatusForStages_ListsGroupsAndBindingTopLevel(t *testing.T) {
	f := newFixture(t)
	h, a, b, s := f.stage(), f.stage(), f.stage(), f.stage()
	f.mustAdmitSet(h, gr(mg2, 1))
	f.mustQueueSet(a, 1, gr(mg2, 1))
	f.mustQueueSet(b, 2, gr(mg2, 1))
	q := f.mustQueueSet(s, 3, gr(mg1, 1), gr(mg2, 1))
	if len(q.Holders) != 1 || q.Holders[0].StageID != h.ID {
		t.Fatalf("Admission binding holders = %v, want [H] (G2)", holderIDs(q.Holders))
	}
	st := f.positions(s.ID)[s.ID]
	if st.State != SlotQueued || st.GroupKey != mg2 || st.Position != 3 || st.Limit != 1 || len(st.Holders) != 1 || st.Holders[0].StageID != h.ID {
		t.Fatalf("top-level status = %+v, want the binding group G2 at position 3 held by H", st)
	}
	if len(st.Groups) != 2 {
		t.Fatalf("status groups = %+v, want 2", st.Groups)
	}
	g1, g2 := st.Groups[0], st.Groups[1]
	if g1.Key != mg1 || g1.Scope != ScopeRepository || g1.Limit != 1 || g1.Position != 1 || len(g1.Holders) != 0 {
		t.Fatalf("status G1 = %+v, want position 1, no holders", g1)
	}
	if g2.Key != mg2 || g2.Scope != ScopeRepository || g2.Limit != 1 || g2.Position != 3 || len(g2.Holders) != 1 {
		t.Fatalf("status G2 = %+v, want position 3, one holder", g2)
	}
}

// TestAdmitSet_SetGrowthRestartsAtTail: a stage queued on {G1} that re-asks
// with {G1, G2} has a different set, so the whole set restarts at the tail,
// behind a G1 waiter that queued after it.
func TestAdmitSet_SetGrowthRestartsAtTail(t *testing.T) {
	f := newFixture(t)
	h, s, later := f.stage(), f.stage(), f.stage()
	f.mustAdmitSet(h, gr(mg1, 1))
	first := f.mustQueueSet(s, 1, gr(mg1, 1))
	f.mustQueueSet(later, 2, gr(mg1, 1))
	q := f.mustQueueSet(s, 2, gr(mg1, 1), gr(mg2, 1))
	if !q.NewlyQueued || !q.EnqueuedAt.After(first.EnqueuedAt) {
		t.Fatalf("grown set NewlyQueued=%v enqueued %v (was %v), want restarted at the tail", q.NewlyQueued, q.EnqueuedAt, first.EnqueuedAt)
	}
	f.mustQueueSet(later, 1, gr(mg1, 1))
}

// stageInRepo is f.stage for a run of another repository.
func (f *fixture) stageInRepo(repo string) *run.Stage {
	f.t.Helper()
	ctx := context.Background()
	r, err := f.repo.CreateRun(ctx, run.CreateRunParams{
		Repo: repo, WorkflowID: "feature_change", WorkflowSHA: "deadbeef", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		f.t.Fatalf("create run: %v", err)
	}
	st, err := f.repo.CreateStage(ctx, run.CreateStageParams{
		RunID: r.ID, Sequence: 1, Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		f.t.Fatalf("create stage: %v", err)
	}
	if st, err = f.repo.TransitionStage(ctx, st.ID, run.StageStateAwaitingHostDispatch, nil); err != nil {
		f.t.Fatalf("park stage: %v", err)
	}
	return st
}

// TestAdmitSet_DeclaredHostVerifyBlocksPassesAndJobs is a STORE-level test of
// how the store treats an ordinary stage whose set includes local-verify:<host>,
// the group every merge-candidate verify pass and specialist job of the
// account on the host uses. No spec can put a stage there: both validators
// refuse {group: verify, scope: host} and the server's declaredGroupRequests
// refuses an entry for local-verify:<host> (README § "Verify group"); the
// store itself does not enforce that reservation, which is what this pins.
// D declares {local-verify:h1, a long-held repository group}. While D waits
// there it holds nothing, yet (1) a merge-candidate pass from ANOTHER
// repository queues behind it with no holder in the group, and (2)
// GroupOccupancyTx reports a fresh stage waiter, so specialistjob.AdmitJob
// (which admits only at FreshStageWaiters == 0) refuses every job. Once D
// holds the group, the pass waits behind D for D's whole stage.
func TestAdmitSet_DeclaredHostVerifyBlocksPassesAndJobs(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	verify := VerifyGroupKey("h1")
	long, d := f.stage(), f.stage()
	f.mustAdmitSet(long, gr(mg1, 1))
	f.mustQueueSet(d, 1, gr(verify, DefaultVerifyLimit), gr(mg1, 1))
	occupancy := func() Occupancy {
		t.Helper()
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if hit, err := TryGroupLockTx(ctx, tx, nil, verify); err != nil || !hit {
			t.Fatalf("TryGroupLockTx = %v, %v", hit, err)
		}
		occ, err := GroupOccupancyTx(ctx, tx, nil, verify)
		if err != nil {
			t.Fatalf("GroupOccupancyTx: %v", err)
		}
		return occ
	}
	if occ := occupancy(); occ != (Occupancy{FreshStageWaiters: 1}) {
		t.Fatalf("local-verify:h1 occupancy while D waits = %+v, want one fresh stage waiter and no holder (a specialist job is refused)", occ)
	}
	pass := f.stageInRepo("other-org/other-repo")
	q := f.mustQueueSet(pass, 2, gr(verify, DefaultVerifyLimit))
	if len(q.Holders) != 0 {
		t.Fatalf("pass holders = %v, want none: D holds nothing yet blocks the pass", holderIDs(q.Holders))
	}
	f.to(long.ID, run.StageStateRunning, run.StageStateSucceeded)
	f.mustAdmitSet(d, gr(verify, DefaultVerifyLimit), gr(mg1, 1))
	if q := f.mustQueueSet(pass, 1, gr(verify, DefaultVerifyLimit)); len(q.Holders) != 1 || q.Holders[0].StageID != d.ID {
		t.Fatalf("pass holders after D is admitted = %v, want [D]", holderIDs(q.Holders))
	}
	f.to(d.ID, run.StageStateRunning, run.StageStateSucceeded)
	f.mustAdmitSet(pass, gr(verify, DefaultVerifyLimit))
}

// raceTrial is one stage of TestAdmitSet_RacingOverlappingSets and the group
// set it asks with on every call.
type raceTrial struct {
	shape string
	st    *run.Stage
	set   []GroupRequest
}

// assertCommittedSlots reads every COMMITTED slot row with its stage's state
// in ONE query and checks it against the racing test's properties. asked maps
// each stage asked in this phase to its Admitted verdict; a trial absent from
// it was admitted and released in an earlier phase. Nothing else from an
// Admission is trusted.
func (f *fixture) assertCommittedSlots(phase string, trials []raceTrial, limits map[string]int, asked map[uuid.UUID]bool) {
	f.t.Helper()
	type stageRows struct {
		stageState    run.StageState
		groups, state []string
	}
	rows, err := f.pool.Query(context.Background(), `SELECT s.stage_id, s.group_key, s.state, st.state
FROM stage_concurrency_slots s JOIN stages st ON st.id = s.stage_id
ORDER BY s.stage_id, s.group_key COLLATE "C"`)
	if err != nil {
		f.t.Fatalf("%s: read committed slot rows: %v", phase, err)
	}
	defer rows.Close()
	got := map[uuid.UUID]*stageRows{}
	liveHeld := map[string]int{}
	for rows.Next() {
		var id uuid.UUID
		var group, state, stageState string
		if err := rows.Scan(&id, &group, &state, &stageState); err != nil {
			f.t.Fatalf("%s: scan slot row: %v", phase, err)
		}
		r := got[id]
		if r == nil {
			r = &stageRows{stageState: run.StageState(stageState)}
			got[id] = r
		}
		r.groups = append(r.groups, group)
		r.state = append(r.state, state)
		// A released stage's rows stay 'held'; only a live stage occupies.
		if state == "held" && (r.stageState == run.StageStateDispatched || r.stageState == run.StageStateRunning) {
			liveHeld[group]++
		}
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("%s: slot rows: %v", phase, err)
	}
	if len(got) != len(trials) {
		f.t.Fatalf("%s: slot rows cover %d stages, want exactly the %d trials", phase, len(got), len(trials))
	}
	// PROPERTY 2: every stage's rows are exactly its set, all held (stage
	// admitted) or all queued (stage still waiting), never a partial hold.
	for _, tr := range trials {
		r := got[tr.st.ID]
		want := make([]string, 0, len(tr.set))
		for _, g := range tr.set {
			want = append(want, g.Key)
		}
		slices.Sort(want)
		if r == nil || !slices.Equal(r.groups, want) {
			f.t.Fatalf("%s: %s stage %s has slot rows %+v, want exactly one row per group of its set %v", phase, tr.shape, tr.st.ID, r, want)
		}
		held := 0
		for _, s := range r.state {
			if s == "held" {
				held++
			}
		}
		if held != 0 && held != len(r.state) {
			f.t.Fatalf("%s: %s stage %s rows %v are %v: a partial hold, want all held or all queued", phase, tr.shape, tr.st.ID, r.groups, r.state)
		}
		admitted, wasAsked := asked[tr.st.ID]
		var wantHeld bool
		var wantState run.StageState
		switch {
		case wasAsked && admitted:
			wantHeld, wantState = true, run.StageStateDispatched
		case wasAsked:
			wantHeld, wantState = false, run.StageStateAwaitingHostDispatch
		default:
			wantHeld, wantState = true, run.StageStateSucceeded
		}
		if (held > 0) != wantHeld || r.stageState != wantState {
			f.t.Fatalf("%s: %s stage %s (asked %v, admitted %v) is %s with rows %v, want stage %s with every row held=%v",
				phase, tr.shape, tr.st.ID, wasAsked, admitted, r.stageState, r.state, wantState, wantHeld)
		}
	}
	// PROPERTY 1: no group holds more live stages than its limit.
	for _, g := range []string{mg1, mg2, mg3} {
		if liveHeld[g] > limits[g] {
			f.t.Fatalf("%s: group %s has %d held rows of live stages, want at most its limit %d (over-admitted)", phase, g, liveHeld[g], limits[g])
		}
	}
}

// TestAdmitSet_RacingOverlappingSets races admitters over partly overlapping
// group sets (A {G1,G2}, B {G2,G3}, C {G1}) on a 3-connection pool, and reads
// the committed rows back after every round: (1) no group holds more than its
// limit; (2) every stage is all held or all queued, never a partial hold; (3)
// some admitter takes its whole lock set and decides in every round. A
// sequential drain then admits every stage, so each set shape is admitted at
// least once and no property is vacuous.
//
// (3) is deterministic for these sets: locks are tried in canonical order
// G1 < G2 < G3 and released together at commit, so a holder of G2 always
// completes its set (G2 is A's last lock; G3 is only ever held by a B that
// already holds G2), and C's one lock is its whole set. A round need not
// ADMIT anyone (README § "Admit, step by step" step 7), so only the drain
// asserts admissions.
func TestAdmitSet_RacingOverlappingSets(t *testing.T) {
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(pgtest.NewURL(t))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.MaxConns = 3
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("small pool: %v", err)
	}
	t.Cleanup(pool.Close)
	f := &fixture{t: t, pool: pool, repo: run.NewPostgresRepository(pool), store: NewPostgresStore(pool)}

	limits := map[string]int{mg1: 1, mg2: 2, mg3: 1}
	g := func(key string) GroupRequest { return gr(key, limits[key]) }
	var trials []raceTrial
	for i := range 3 {
		// Every other multi-group request is in non-canonical order, so the
		// race also exercises normalised()'s sort.
		a, b := []GroupRequest{g(mg1), g(mg2)}, []GroupRequest{g(mg2), g(mg3)}
		if i%2 == 1 {
			slices.Reverse(a)
			slices.Reverse(b)
		}
		trials = append(trials,
			raceTrial{shape: "A", st: f.stage(), set: a},
			raceTrial{shape: "B", st: f.stage(), set: b},
			raceTrial{shape: "C", st: f.stage(), set: []GroupRequest{g(mg1)}})
	}

	// Warm the pool: hold all three connections at once, so connection setup
	// does not stagger the first round.
	conns := make([]*pgxpool.Conn, 0, cfg.MaxConns)
	for range cfg.MaxConns {
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("warm the pool: %v", err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Release()
	}

	admittedCount := make([]int, len(trials))
	release := func(asked map[uuid.UUID]bool) {
		t.Helper()
		for id, admitted := range asked {
			if admitted {
				f.to(id, run.StageStateRunning, run.StageStateSucceeded)
			}
		}
	}

	// Racing rounds. The hook keeps each decider inside its transaction, and
	// so holding every group lock, for a widened window between its decision
	// and its CAS.
	f.store.afterDecision = func(context.Context, bool) { time.Sleep(timescale.D(50 * time.Millisecond)) }
	const rounds = 5
	for round := 1; round <= rounds; round++ {
		var pending []int
		for i := range trials {
			if admittedCount[i] == 0 {
				pending = append(pending, i)
			}
		}
		if len(pending) == 0 {
			break
		}
		start := make(chan struct{})
		results := make([]Admission, len(pending))
		errs := make([]error, len(pending))
		var wg sync.WaitGroup
		for j, i := range pending {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), timescale.D(10*time.Second))
				defer cancel()
				<-start
				results[j], errs[j] = f.store.Admit(ctx, setReq(trials[i].st, trials[i].set...))
			}()
		}
		close(start)
		wg.Wait()
		phase := fmt.Sprintf("racing round %d", round)
		asked := map[uuid.UUID]bool{}
		decided := 0
		for j, i := range pending {
			if errs[j] != nil {
				t.Fatalf("%s: %s admitter %s: %v", phase, trials[i].shape, trials[i].st.ID, errs[j])
			}
			if !results[j].Contended {
				decided++
			}
			asked[trials[i].st.ID] = results[j].Admitted
			if results[j].Admitted {
				admittedCount[i]++
			}
		}
		// PROPERTY 3: some admitter took its whole lock set and decided.
		if decided == 0 {
			t.Fatalf("%s: all %d admitters answered Contended, want at least one to take its whole lock set and decide", phase, len(pending))
		}
		f.assertCommittedSlots(phase, trials, limits, asked)
		release(asked)
	}
	f.store.afterDecision = nil

	// Deterministic drain: the global FIFO head is first in every group of its
	// set, so each pass admits it unless a stage admitted earlier in the same
	// pass holds one of its groups, and then that stage was admitted.
	for pass := 1; ; pass++ {
		asked := map[uuid.UUID]bool{}
		admitted := 0
		for i, tr := range trials {
			if admittedCount[i] > 0 {
				continue
			}
			a := f.admitSet(tr.st, tr.set...)
			asked[tr.st.ID] = a.Admitted
			if a.Admitted {
				admittedCount[i]++
				admitted++
			}
		}
		if len(asked) == 0 {
			break
		}
		if pass > len(trials) {
			t.Fatalf("queue still holds %d stages after %d drain passes, want it drained within %d", len(asked), pass-1, len(trials))
		}
		phase := fmt.Sprintf("drain pass %d", pass)
		f.assertCommittedSlots(phase, trials, limits, asked)
		if admitted == 0 {
			t.Fatalf("%s: admitted none of %d queued stages, want the FIFO head admitted", phase, len(asked))
		}
		release(asked)
	}
	for i, tr := range trials {
		if admittedCount[i] != 1 {
			t.Fatalf("%s stage %s admitted %d times, want exactly once", tr.shape, tr.st.ID, admittedCount[i])
		}
	}
	f.assertCommittedSlots("after the drain", trials, limits, map[uuid.UUID]bool{})
}
