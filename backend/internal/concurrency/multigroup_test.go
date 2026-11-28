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
	"github.com/jackc/pgx/v5"
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
	for g := range limits {
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
	f := newRacePoolFixture(t)
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
	// The hook keeps each decider inside its transaction, and so holding
	// every group lock, for a widened window in every round.
	f.raceAndDrain(trials, limits, 5, func(int) bool { return true })
}

// raceConns is the racing tests' pool size: fewer connections than admitters.
const raceConns = 3

// newRacePoolFixture is a fixture over a fresh database on a raceConns-
// connection pool, warmed so connection setup does not stagger the first
// round.
func newRacePoolFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(pgtest.NewURL(t))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.MaxConns = raceConns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("small pool: %v", err)
	}
	t.Cleanup(pool.Close)
	conns := make([]*pgxpool.Conn, 0, raceConns)
	for range raceConns {
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("warm the pool: %v", err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Release()
	}
	return &fixture{t: t, pool: pool, repo: run.NewPostgresRepository(pool), store: NewPostgresStore(pool)}
}

// raceAndDrain runs up to rounds racing rounds over the trials, then a
// sequential drain, reading the committed rows back after every round and
// pass (assertCommittedSlots) and asserting every trial is admitted exactly
// once. widen(round) reports whether that round parks each decider for 50ms
// between its decision and its CAS, holding every group lock of its set.
func (f *fixture) raceAndDrain(trials []raceTrial, limits map[string]int, rounds int, widen func(round int) bool) {
	t := f.t
	t.Helper()
	admittedCount := make([]int, len(trials))
	release := func(asked map[uuid.UUID]bool) {
		t.Helper()
		for id, admitted := range asked {
			if admitted {
				f.to(id, run.StageStateRunning, run.StageStateSucceeded)
			}
		}
	}

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
		f.store.afterDecision = nil
		if widen(round) {
			f.store.afterDecision = func(context.Context, bool) { time.Sleep(timescale.D(50 * time.Millisecond)) }
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
		admittedNow := 0
		for _, a := range asked {
			if a {
				admittedNow++
			}
		}
		t.Logf("%s: %d asked, %d decided, %d admitted", phase, len(pending), decided, admittedNow)
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
		t.Logf("%s: %d asked, %d admitted", phase, len(asked), admitted)
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

// Host capacity group (ADR-094 D4, #4241): every host-dispatched agent stage
// also holds one slot of host:<label>:capacity, which is exempt from rule (b)
// (Captain ruling 2026-11-20) and whose rule (a) counts only capacity-bound
// earlier waiters (ADR-094 A4). README § "Host capacity group".

// liveHeld counts the held rows of group whose stage is still live
// (dispatched or running): a released stage's rows stay 'held'.
func (f *fixture) liveHeld(group string) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM stage_concurrency_slots s
JOIN stages st ON st.id = s.stage_id
WHERE s.group_key = $1 AND s.state = 'held' AND st.state IN ('dispatched', 'running')`, group).Scan(&n); err != nil {
		f.t.Fatalf("count held %s rows: %v", group, err)
	}
	return n
}

func groupByKey(t *testing.T, gs []GroupState, key string) GroupState {
	t.Helper()
	for _, g := range gs {
		if g.Key == key {
			return g
		}
	}
	t.Fatalf("groups %+v have no %s", gs, key)
	return GroupState{}
}

func (f *fixture) assertAllQueued(st *run.Stage, n int) {
	f.t.Helper()
	rows := f.setRows(st.ID)
	if len(rows) != n {
		f.t.Fatalf("stage %s rows = %+v, want %d", st.ID, rows, n)
	}
	for _, r := range rows {
		if r.state != "queued" || r.acquiredAt != nil {
			f.t.Fatalf("stage %s row %s = %+v, want queued (no partial hold)", st.ID, r.group, r)
		}
	}
	if got := f.stageState(st.ID); got != run.StageStateAwaitingHostDispatch {
		f.t.Fatalf("stage %s = %s, want awaiting_host_dispatch", st.ID, got)
	}
}

// TestAdmitSet_HostCapacityBoundsMixedTypes: at capacity 3 with implement
// limit 2, two implements and one acceptance fill the host, and a fourth
// stage of every type queues on capacity alone: every other group of its set
// is free. Exactly 3 capacity rows are held.
func TestAdmitSet_HostCapacityBoundsMixedTypes(t *testing.T) {
	capKey, impl := CapacityGroupKey("h1"), DefaultGroupKey("h1")
	capG := gr(capKey, DefaultHostCapacity)
	fourth := []struct {
		name string
		set  []GroupRequest
	}{
		{"plan", []GroupRequest{capG}},
		{"verify pass", []GroupRequest{capG, gr(VerifyGroupKey("h1"), DefaultVerifyLimit)}},
		{"acceptance in another repository", []GroupRequest{capG, gr(NamedGroupKey("org/two", "local-acceptance"), 1)}},
	}
	for _, tc := range fourth {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			i1, i2, a1, s := f.stage(), f.stage(), f.stage(), f.stage()
			f.mustAdmitSet(i1, capG, gr(impl, 2))
			f.mustAdmitSet(i2, capG, gr(impl, 2))
			f.mustAdmitSet(a1, capG, gr(NamedGroupKey("org/one", "local-acceptance"), 1))
			q := f.mustQueueSet(s, 1, tc.set...)
			if c := groupByKey(t, q.Groups, capKey); c.Limit != DefaultHostCapacity || c.Position != 1 || len(c.Holders) != 3 {
				t.Fatalf("capacity group = %+v, want limit 3, position 1, three holders", c)
			}
			for _, g := range q.Groups {
				if g.Key != capKey && len(g.Holders) != 0 {
					t.Fatalf("group %s holders = %v, want free: capacity alone queues the fourth stage", g.Key, holderIDs(g.Holders))
				}
			}
			f.assertAllQueued(s, len(tc.set))
			if n := f.liveHeld(capKey); n != DefaultHostCapacity {
				t.Fatalf("held %s rows = %d, want exactly %d", capKey, n, DefaultHostCapacity)
			}
		})
	}
}

// TestAdmitSet_CapacityExemptFromFIFO (C1 test 1): I3 {capacity, implement}
// is an earlier fresh MULTI-group waiter on capacity, queued only on
// local-implement. At capacity 6, (a) passes for each later stage (2+1, 3+1
// and 4+1 are all below 6), so only rule (b) could queue them; capacity is
// exempt, so a later plan, acceptance and verify pass are each admitted and
// I3 stays queued.
func TestAdmitSet_CapacityExemptFromFIFO(t *testing.T) {
	f := newFixture(t)
	capKey := CapacityGroupKey("h1")
	capG, implG := gr(capKey, 6), gr(DefaultGroupKey("h1"), 2)
	i1, i2, i3 := f.stage(), f.stage(), f.stage()
	f.mustAdmitSet(i1, capG, implG)
	f.mustAdmitSet(i2, capG, implG)
	f.mustQueueSet(i3, 1, capG, implG)
	for _, later := range []struct {
		name string
		set  []GroupRequest
	}{
		{"plan", []GroupRequest{capG}},
		{"acceptance", []GroupRequest{capG, gr(NamedGroupKey("o/r", "local-acceptance"), 1)}},
		{"verify pass", []GroupRequest{capG, gr(VerifyGroupKey("h1"), DefaultVerifyLimit)}},
	} {
		if a := f.admitSet(f.stage(), later.set...); !a.Admitted {
			t.Fatalf("later %s = queued (groups %+v), want admitted: capacity is exempt from rule (b) behind the queued implement", later.name, a.Groups)
		}
	}
	f.assertAllQueued(i3, 2)
	if n := f.liveHeld(capKey); n != 5 {
		t.Fatalf("held %s rows = %d, want 5 (I1, I2 and the three later stages)", capKey, n)
	}
}

// TestAdmitSet_BlockedWaiterReservesNoCapacity (ADR-094 A4, inverting A2's
// TestAdmitSet_CapacityReservationCountsAsFull): on capacity, condition (a)
// counts only CAPACITY-BOUND earlier waiters. At capacity 3 with implement
// limit 2, I1 and I2 hold and I3 queues on {capacity, implement}; I3's only
// blocker is local-implement (2 holders at limit 2), so it reserves no
// capacity slot and a later plan is ADMITTED (2 holders + 0 < 3). I3 stays
// queued, and a further capacity-only stage queues behind 3 holders.
func TestAdmitSet_BlockedWaiterReservesNoCapacity(t *testing.T) {
	f := newFixture(t)
	capKey := CapacityGroupKey("h1")
	capG, implG := gr(capKey, 3), gr(DefaultGroupKey("h1"), 2)
	i1, i2, i3, p := f.stage(), f.stage(), f.stage(), f.stage()
	f.mustAdmitSet(i1, capG, implG)
	f.mustAdmitSet(i2, capG, implG)
	f.mustQueueSet(i3, 1, capG, implG)
	if a := f.admitSet(p, capG); !a.Admitted {
		t.Fatalf("plan = queued (groups %+v), want admitted: I3 is blocked on local-implement and reserves no capacity slot (2 holders + 0 < 3)", a.Groups)
	}
	if n := f.liveHeld(capKey); n != 3 {
		t.Fatalf("held %s rows = %d, want 3 (I1, I2 and the plan)", capKey, n)
	}
	f.assertAllQueued(i3, 2)
	q := f.mustQueueSet(f.stage(), 2, capG)
	if c := groupByKey(t, q.Groups, capKey); len(c.Holders) != 3 {
		t.Fatalf("further stage capacity group = %+v, want three holders", c)
	}
}

// TestAdmitSet_CapacityBoundWaiterKeepsPosition is ADR-094 A4 item 3's
// fixture: capacity 3, implement limit 2. I1 and I2 hold; I3 queues on
// {capacity, implement}; plan P1 is admitted (three capacity holders) and
// plan P2 queues on capacity. When I2 settles, I3's other group would admit
// it, so I3 is capacity-bound at its original position before it polls: P2's
// re-ask is REFUSED with only 2 capacity holders (2 + I3's reservation = 3),
// and I3 is admitted on its next ask.
func TestAdmitSet_CapacityBoundWaiterKeepsPosition(t *testing.T) {
	f := newFixture(t)
	capKey := CapacityGroupKey("h1")
	capG, implG := gr(capKey, 3), gr(DefaultGroupKey("h1"), 2)
	i1, i2, i3, p1, p2 := f.stage(), f.stage(), f.stage(), f.stage(), f.stage()
	f.mustAdmitSet(i1, capG, implG)
	f.mustAdmitSet(i2, capG, implG)
	f.mustQueueSet(i3, 1, capG, implG)
	if a := f.admitSet(p1, capG); !a.Admitted {
		t.Fatalf("P1 = queued (groups %+v), want admitted: I3 is not capacity-bound while local-implement is full", a.Groups)
	}
	f.mustQueueSet(p2, 2, capG)
	f.to(i2.ID, run.StageStateRunning, run.StageStateSucceeded)
	a := f.admitSet(p2, capG)
	if a.Admitted {
		t.Fatal("P2 admitted after I2 settled, want refused: I3 is now capacity-bound and earlier (2 holders + 1 reservation = 3)")
	}
	if c := groupByKey(t, a.Groups, capKey); len(c.Holders) != 2 {
		t.Fatalf("P2 capacity group = %+v, want exactly two holders: the queue comes from I3's reservation, not from holders", c)
	}
	f.mustAdmitSet(i3, capG, implG)
	if n := f.liveHeld(capKey); n != 3 {
		t.Fatalf("held %s rows = %d, want 3 (I1, P1 and I3)", capKey, n)
	}
	f.assertAllQueued(p2, 1)
}

// argFaultTracer cancels the context of the first statement whose SQL is
// exactly sql and whose arguments include the string arg, so exactly that
// statement fails before it is sent.
type argFaultTracer struct {
	mu    sync.Mutex
	sql   string
	arg   string
	fired bool
}

func (ft *argFaultTracer) arm(sql, arg string) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.sql, ft.arg, ft.fired = sql, arg, false
}

// disarm stops injecting and reports whether a statement was failed.
func (ft *argFaultTracer) disarm() bool {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	fired := ft.fired
	ft.sql, ft.arg = "", ""
	return fired
}

func (ft *argFaultTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if ft.fired || ft.sql == "" || data.SQL != ft.sql {
		return ctx
	}
	for _, a := range data.Args {
		if s, ok := a.(string); ok && s == ft.arg {
			ft.fired = true
			failed, cancel := context.WithCancel(ctx)
			cancel()
			return failed
		}
	}
	return ctx
}

func (*argFaultTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestAdmitSet_CapacityClassificationStatementFailure fails each statement
// of the capacity-bound classification (ADR-094 A4) in turn, on
// TestAdmitSet_CapacityBoundWaiterKeepsPosition's fixture after I2 settles:
// P2 {capacity} must classify I3 {capacity, implement}. Every failure
// surfaces as Admit's error and commits nothing, and the fixture still
// decides as before once the faults stop.
func TestAdmitSet_CapacityClassificationStatementFailure(t *testing.T) {
	cfg, err := pgxpool.ParseConfig(pgtest.NewURL(t))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	ft := &argFaultTracer{}
	cfg.ConnConfig.Tracer = ft
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	f := &fixture{t: t, pool: pool, repo: run.NewPostgresRepository(pool), store: NewPostgresStore(pool)}
	capKey, implKey := CapacityGroupKey("h1"), DefaultGroupKey("h1")
	capG, implG := gr(capKey, 3), gr(implKey, 2)
	i1, i2, i3, p1, p2 := f.stage(), f.stage(), f.stage(), f.stage(), f.stage()
	f.mustAdmitSet(i1, capG, implG)
	f.mustAdmitSet(i2, capG, implG)
	f.mustQueueSet(i3, 1, capG, implG)
	f.mustAdmitSet(p1, capG)
	f.mustQueueSet(p2, 2, capG)
	f.to(i2.ID, run.StageStateRunning, run.StageStateSucceeded)

	// P2's set is {capacity} alone, so implKey reaches a statement only
	// through the classification of I3's other group.
	for _, tc := range []struct{ name, sql, arg string }{
		{"list capacity waiters", capacityWaitersSQL, capKey},
		{"waiter group holders", holdersSQL, implKey},
		{"waiter group job holders", jobHoldersSQL, implKey},
		{"waiter queue ahead", waiterAheadSQL, implKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ft.arm(tc.sql, tc.arg)
			_, err := f.store.Admit(context.Background(), setReq(p2, capG))
			if !ft.disarm() {
				t.Fatalf("fixture: no statement ran the %s SQL with %q", tc.name, tc.arg)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Admit err = %v, want the failed statement's error", err)
			}
			if n := f.liveHeld(capKey); n != 2 {
				t.Fatalf("held %s rows = %d, want 2 (I1 and P1): a failed classification committed a hold", capKey, n)
			}
			f.assertAllQueued(p2, 1)
		})
	}
	if a := f.admitSet(p2, capG); a.Admitted {
		t.Fatal("P2 admitted after the faults stopped, want refused: I3 is capacity-bound and earlier")
	}
	f.mustAdmitSet(i3, capG, implG)
}

// TestAdmitSet_CapacityOnlyWaiterReserves is ADR-094 A4 item 1's unchanged
// case: a {capacity}-only waiter is always capacity-bound. At capacity 2, H1
// and H2 hold and W {capacity} queues. Once H2 settles, a new capacity-only
// stage queues with exactly ONE capacity holder (1 + W's reservation = 2),
// and W is admitted on its next ask.
func TestAdmitSet_CapacityOnlyWaiterReserves(t *testing.T) {
	f := newFixture(t)
	capKey := CapacityGroupKey("h1")
	capG := gr(capKey, 2)
	h1, h2, w := f.stage(), f.stage(), f.stage()
	f.mustAdmitSet(h1, capG)
	f.mustAdmitSet(h2, capG)
	f.mustQueueSet(w, 1, capG)
	f.to(h2.ID, run.StageStateRunning, run.StageStateSucceeded)
	a := f.admitSet(f.stage(), capG)
	if a.Admitted {
		t.Fatal("new capacity-only stage admitted, want queued: W is {capacity}-only, so it is capacity-bound and reserves the free slot (1 holder + 1 reservation = 2)")
	}
	if c := groupByKey(t, a.Groups, capKey); len(c.Holders) != 1 {
		t.Fatalf("new stage capacity group = %+v, want exactly one holder", c)
	}
	f.mustAdmitSet(w, capG)
	if n := f.liveHeld(capKey); n != 2 {
		t.Fatalf("held %s rows = %d, want 2 (H1 and W)", capKey, n)
	}
}

// TestAdmitSet_StaleCapacityReservationEnds (#3969 carry-forward item 1,
// re-based on a capacity-bound waiter by ADR-094 A4): a capacity reservation
// lasts only while its waiter refreshes the row within QueueTTL. At capacity
// 2, H1 and H2 hold and W {capacity} queues; W is {capacity}-only, so it is
// capacity-bound (TestAdmitSet_CapacityOnlyWaiterReserves). H2 settles FIRST
// and only then does ONLY W's row age past QueueTTL (its waiter died), so a
// NEW capacity-only stage is admitted at once: the freshness filter is the
// only thing between it and a queue.
func TestAdmitSet_StaleCapacityReservationEnds(t *testing.T) {
	f := newFixture(t)
	capKey := CapacityGroupKey("h1")
	capG := gr(capKey, 2)
	h1, h2, w := f.stage(), f.stage(), f.stage()
	f.mustAdmitSet(h1, capG)
	f.mustAdmitSet(h2, capG)
	f.mustQueueSet(w, 1, capG)
	f.to(h2.ID, run.StageStateRunning, run.StageStateSucceeded)
	f.staleQueued(w.ID)
	if a := f.admitSet(f.stage(), capG); !a.Admitted {
		t.Fatalf("new capacity-only stage = queued (groups %+v), want admitted: W's stale row (past QueueTTL) must not reserve a capacity slot", a.Groups)
	}
	if n := f.liveHeld(capKey); n != 2 {
		t.Fatalf("held %s rows = %d, want 2 (H1 and the new stage)", capKey, n)
	}
}

// TestAdmitSet_SecondInLineWaiterReservesNoCapacity pins the rule-(b) arm of
// the capacity classification (ADR-094 A4 "Second-in-line waiters") and its
// (a)-by-holders arm. At capacity 2, H0 {capacity} holds and Z holds
// repository group Y (limit 1). X {capacity, G:5, Y:1} queues because Y is
// full by holder. W {capacity, G:5} queues ONLY by rule (b) on G behind the
// multi-group X: on G, 0 holders + 1 ahead < 5. A plan P {capacity} is
// ADMITTED: X is not bound (Y full) and W is not bound (rule (b) on G), so
// capacity sees 1 holder + 0 < 2.
func TestAdmitSet_SecondInLineWaiterReservesNoCapacity(t *testing.T) {
	f := newFixture(t)
	capKey := CapacityGroupKey("h1")
	capG, gG, yG := gr(capKey, 2), gr(mg1, 5), gr(mg2, 1)
	h0, z, x, w, p := f.stage(), f.stage(), f.stage(), f.stage(), f.stage()
	f.mustAdmitSet(h0, capG)
	f.mustAdmitSet(z, yG)
	if a := f.admitSet(x, capG, gG, yG); a.Admitted {
		t.Fatal("fixture: X admitted, want queued on Y")
	} else if y := groupByKey(t, a.Groups, mg2); len(y.Holders) != 1 {
		t.Fatalf("fixture: X's Y group = %+v, want full by Z", y)
	}
	if a := f.admitSet(w, capG, gG); a.Admitted {
		t.Fatal("fixture: W admitted, want queued by rule (b) on G behind multi-group X")
	} else if g := groupByKey(t, a.Groups, mg1); len(g.Holders) != 0 || g.Position != 2 {
		t.Fatalf("fixture: W's G group = %+v, want no holder at position 2 (rule (b) alone)", g)
	}
	if a := f.admitSet(p, capG); !a.Admitted {
		t.Fatalf("plan P = queued (groups %+v), want admitted: neither X (Y full) nor W (rule (b) on G) is capacity-bound", a.Groups)
	}
	f.assertAllQueued(x, 3)
	f.assertAllQueued(w, 2)
}

// TestAdmitSet_JobHeldGroupReservesNoCapacity pins the job-holder term of
// the classification's condition (a). At capacity 2, H0 {capacity} holds and
// a running specialist job occupies local-verify:h1 (no stage holder, no
// waiter), so a verify pass V {capacity, local-verify:1} queues on the job
// alone. V is not capacity-bound, so a plan P {capacity} is ADMITTED.
func TestAdmitSet_JobHeldGroupReservesNoCapacity(t *testing.T) {
	f := newFixture(t)
	capKey, verify := CapacityGroupKey("h1"), VerifyGroupKey("h1")
	capG, verifyG := gr(capKey, 2), gr(verify, DefaultVerifyLimit)
	h0, v, p := f.stage(), f.stage(), f.stage()
	f.mustAdmitSet(h0, capG)
	f.seedRunningJob(verify)
	if a := f.admitSet(v, capG, verifyG); a.Admitted {
		t.Fatal("fixture: V admitted, want queued behind the running job")
	} else if g := groupByKey(t, a.Groups, verify); len(g.Holders) != 0 {
		t.Fatalf("fixture: V's verify group = %+v, want no stage holder (the job alone blocks)", g)
	}
	if a := f.admitSet(p, capG); !a.Admitted {
		t.Fatalf("plan P = queued (groups %+v), want admitted: V is blocked by a job holder and reserves no capacity slot", a.Groups)
	}
	f.assertAllQueued(v, 2)
}

// TestAdmitSet_OtherGroupEarlierWaiterUnbindsCapacity pins the earlier-waiter
// term of the classification's condition (a), counted from W's OWN effective
// key. H is repository group G (limit 2) with one holder A, and every waiter
// on it is single-group, so rule (b) never applies. At capacity 2, H0
// {capacity} holds. E {G} is a fresh earlier waiter, and W {capacity, G}
// queues on G (1 holder + E = 2). L {G} queues AFTER W but before the
// callers.
//
// Phase 1: on G, 1 holder < 2 but 1 + E >= 2, so W is not capacity-bound by
// that term alone and plan P1 is ADMITTED. Phase 2: P1 settles and E's row
// ages past QueueTTL. W's only remaining waiter on G is L, which is behind W
// and must not count: 1 holder + 0 < 2, so W is capacity-bound and plan P2
// QUEUES with one capacity holder (1 + W's reservation = 2).
func TestAdmitSet_OtherGroupEarlierWaiterUnbindsCapacity(t *testing.T) {
	f := newFixture(t)
	capKey := CapacityGroupKey("h1")
	capG, hG := gr(capKey, 2), gr(mg1, 2)
	h0, a, e, w, l, p1, p2 := f.stage(), f.stage(), f.stage(), f.stage(), f.stage(), f.stage(), f.stage()
	f.mustAdmitSet(h0, capG)
	f.mustAdmitSet(a, hG)
	f.makeContendedWaiter(e, mg1, 2)
	if q := f.admitSet(w, capG, hG); q.Admitted {
		t.Fatal("fixture: W admitted, want queued on G behind holder A and earlier waiter E")
	} else if g := groupByKey(t, q.Groups, mg1); len(g.Holders) != 1 || g.Position != 2 {
		t.Fatalf("fixture: W's G group = %+v, want one holder at position 2", g)
	}
	f.makeContendedWaiter(l, mg1, 2)
	if q := f.admitSet(p1, capG); !q.Admitted {
		t.Fatalf("P1 = queued (groups %+v), want admitted: W is not capacity-bound (G: 1 holder + earlier waiter E = 2)", q.Groups)
	}
	f.to(p1.ID, run.StageStateRunning, run.StageStateSucceeded)
	f.staleQueued(e.ID)
	q := f.admitSet(p2, capG)
	if q.Admitted {
		t.Fatal("P2 admitted, want queued: W is capacity-bound (G: 1 holder + 0 earlier waiters < 2; L is behind W and must not count)")
	}
	if c := groupByKey(t, q.Groups, capKey); len(c.Holders) != 1 {
		t.Fatalf("P2 capacity group = %+v, want exactly one holder (the queue is W's reservation)", c)
	}
	f.mustAdmitSet(w, capG, hG)
}

// TestAdmitSet_CapacityBoundClassificationAccountScoped (ADR-094 A4 item 7):
// the classification of an earlier capacity waiter reads only the run's
// account. W, the capacity holders and the plan run in account 1; the other
// scope is the untenanted one. At capacity 2, H0 and H1 {capacity} hold and W
// {capacity, G:1} queues on capacity; H1 then settles. Another scope's
// holder, earlier waiter or running specialist job on G must leave W
// capacity-bound, so the plan QUEUES with one capacity holder (1 + W's
// reservation = 2), and W is admitted on its next ask.
func TestAdmitSet_CapacityBoundClassificationAccountScoped(t *testing.T) {
	for _, tc := range []struct {
		name  string
		other func(f *fixture, st *run.Stage)
		// before reports whether the other scope's occupant is installed
		// before W queues (an earlier waiter must be ahead of W's key).
		before bool
	}{
		{"holder", func(f *fixture, st *run.Stage) { f.mustAdmitSet(st, gr(mg1, 1)) }, false},
		{"earlier waiter", func(f *fixture, st *run.Stage) { f.makeContendedWaiter(st, mg1, 1) }, true},
		// A RUNNING specialist job in the other scope fills G there (limit
		// 1), so a job-holder read without the account would unbind W.
		{"running job", func(f *fixture, _ *run.Stage) { f.seedRunningJob(mg1) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			acct := uuid.New()
			f.exec(`INSERT INTO accounts (id, account_key) VALUES ($1, $2)`, acct, "cc-"+acct.String())
			capKey := CapacityGroupKey("h1")
			capG, hG := gr(capKey, 2), gr(mg1, 1)
			h0, h1, w, p, other := f.stage(), f.stage(), f.stage(), f.stage(), f.stage()
			for _, st := range []*run.Stage{h0, h1, w, p} {
				f.exec(`UPDATE runs SET account_id = $1 WHERE id = $2`, acct, st.RunID)
			}
			if tc.before {
				tc.other(f, other)
			}
			f.mustAdmitSet(h0, capG)
			f.mustAdmitSet(h1, capG)
			if q := f.admitSet(w, capG, hG); q.Admitted {
				t.Fatal("fixture: W admitted, want queued on capacity")
			} else if g := groupByKey(t, q.Groups, mg1); len(g.Holders) != 0 || g.Position != 1 {
				t.Fatalf("fixture: W's G group in account 1 = %+v, want free (no holder, position 1)", g)
			}
			if !tc.before {
				tc.other(f, other)
			}
			f.to(h1.ID, run.StageStateRunning, run.StageStateSucceeded)
			q := f.admitSet(p, capG)
			if q.Admitted {
				t.Fatalf("plan admitted, want queued: another account's %s on G must not unbind W", tc.name)
			}
			if c := groupByKey(t, q.Groups, capKey); len(c.Holders) != 1 {
				t.Fatalf("plan capacity group = %+v, want exactly one holder (the queue is W's reservation)", c)
			}
			f.mustAdmitSet(w, capG, hG)
		})
	}
}

// openCapacityClassificationWindow builds the shared fixture of the
// capacity-waiter window tests: at capacity 2, H0 {capacity} holds and Z
// holds repository group Y (limit 1), so X {capacity, Y:1} queues on Y and is
// a fresh earlier waiter on capacity that is NOT capacity-bound. A later
// capacity admitter then sees used = 1 < 2 <= 1 + round_ahead, so the
// classification runs (store.go step 6), and only a bound waiter it wrongly
// lists can queue that admitter. It returns the capacity request and X.
func (f *fixture) openCapacityClassificationWindow() (GroupRequest, *run.Stage) {
	f.t.Helper()
	capG, yG := gr(CapacityGroupKey("h1"), 2), gr(mg2, 1)
	h0, z, x := f.stage(), f.stage(), f.stage()
	f.mustAdmitSet(h0, capG)
	f.mustAdmitSet(z, yG)
	if a := f.admitSet(x, capG, yG); a.Admitted {
		f.t.Fatal("fixture: X admitted, want queued on Y (full by Z)")
	} else if y := groupByKey(f.t, a.Groups, mg2); len(y.Holders) != 1 {
		f.t.Fatalf("fixture: X's Y group = %+v, want full by Z", y)
	}
	return capG, x
}

// TestAdmitSet_CapacityClassificationSkipsSameRoundWaiter pins
// capacityWaitersSQL's round-start filter (enqueued_at <= the caller's round
// start). In the open window, caller C {capacity} wins the capacity lock and
// is parked (afterLock) while Y {capacity} misses it and commits a queued row.
// Y is {capacity}-only, so it would count as capacity-bound, and C is fresh,
// so Y's row is AHEAD of C's effective key: only the round-start filter keeps
// Y out of the classification. C must be ADMITTED (1 holder + 0 bound < 2).
func TestAdmitSet_CapacityClassificationSkipsSameRoundWaiter(t *testing.T) {
	f := newFixture(t)
	capG, x := f.openCapacityClassificationWindow()
	c, y := f.stage(), f.stage()
	parked, resume := make(chan struct{}), make(chan struct{})
	cs := NewPostgresStore(f.pool)
	cs.afterLock = func(_ context.Context, hit bool) {
		if hit {
			close(parked)
			<-resume
		}
	}
	var ca Admission
	var cerr error
	done := make(chan struct{})
	go func() { defer close(done); ca, cerr = cs.Admit(context.Background(), setReq(c, capG)) }()
	<-parked
	ya, err := f.store.Admit(context.Background(), setReq(y, capG))
	close(resume)
	<-done
	if err != nil || ya.Admitted || !ya.Contended {
		t.Fatalf("fixture: Y while C holds the capacity lock = %+v, %v; want a contended queue", ya, err)
	}
	if cerr != nil {
		t.Fatalf("C: %v", cerr)
	}
	if !ya.EnqueuedAt.Before(ca.EnqueuedAt) {
		t.Fatalf("fixture: Y enqueued %v not before C %v: Y is not ahead of C's key, so the round-start case is not constructed", ya.EnqueuedAt, ca.EnqueuedAt)
	}
	if !ca.Admitted {
		t.Fatalf("C = queued (groups %+v), want admitted: Y committed during C's round and must not count as a capacity-bound reservation", ca.Groups)
	}
	f.assertAllQueued(x, 2)
	f.assertAllQueued(y, 1)
}

// TestAdmitSet_CapacityClassificationSkipsWaiterBehindCaller pins
// capacityWaitersSQL's (effective key, stage_id) < caller bound. In the open
// window, C {capacity} queues contended (without deciding) and then B
// {capacity} queues behind it: B is {capacity}-only and capacity-bound, and
// was enqueued before C's next round starts, so only the key bound keeps it
// out of C's classification. C's re-ask must be ADMITTED (1 holder + 0 bound
// ahead < 2).
func TestAdmitSet_CapacityClassificationSkipsWaiterBehindCaller(t *testing.T) {
	f := newFixture(t)
	capG, x := f.openCapacityClassificationWindow()
	c, b := f.stage(), f.stage()
	f.makeContendedWaiter(c, capG.Key, capG.Limit)
	// B classifies X (not bound) and C ({capacity}-only, bound): 1 + 1 = 2.
	f.mustQueueSet(b, 3, capG)
	if a := f.admitSet(c, capG); !a.Admitted {
		t.Fatalf("C = queued (groups %+v), want admitted: B is behind C and must not count as a capacity-bound reservation", a.Groups)
	}
	f.assertAllQueued(x, 2)
	f.assertAllQueued(b, 1)
}

// TestAdmitSet_CapacityClassificationSkipsStaleWaiter pins
// capacityWaitersSQL's freshness predicate (QueueTTL). In the open window, S
// {capacity} queues contended ahead of the caller and its row then ages past
// QueueTTL. Fresh X keeps the window open (round_ahead counts only fresh
// rows), so the classification runs, and S is {capacity}-only: only the
// freshness predicate keeps it out. A new plan P must be ADMITTED (1 holder +
// 0 bound < 2).
func TestAdmitSet_CapacityClassificationSkipsStaleWaiter(t *testing.T) {
	f := newFixture(t)
	capG, x := f.openCapacityClassificationWindow()
	s, p := f.stage(), f.stage()
	f.makeContendedWaiter(s, capG.Key, capG.Limit)
	f.staleQueued(s.ID)
	if a := f.admitSet(p, capG); !a.Admitted {
		t.Fatalf("plan P = queued (groups %+v), want admitted: S is stale past QueueTTL and must not count as a capacity-bound reservation", a.Groups)
	}
	f.assertAllQueued(x, 2)
}

// TestAdmitSet_RacingCapacityAcrossTypes races {capacity, implement},
// {capacity, acceptance}, {capacity, verify} and {capacity} sets on one host
// (modelled on TestAdmitSet_RacingOverlappingSets, #5480). After every round
// and drain pass: held capacity rows <= the capacity limit (2), held
// local-implement <= its limit (2), held local-verify and acceptance <= 1,
// and every stage is all held or all queued. Odd rounds park each decider
// between decision and CAS (maximal contention); even rounds do not, so
// several deciders commit in sequence within a round and each must count the
// committed holders before it. Capacity's lock sorts first in every set
// (host: < local- < spec:), so its holder always completes its lock set:
// property (3) is deterministic.
func TestAdmitSet_RacingCapacityAcrossTypes(t *testing.T) {
	f := newRacePoolFixture(t)
	capKey, impl, verify, acc := CapacityGroupKey("h1"), DefaultGroupKey("h1"), VerifyGroupKey("h1"), NamedGroupKey("o/r", "local-acceptance")
	// Capacity 2 sits below the type groups' combined bound (2 + 1 + 1, and
	// plans are unbounded), so capacity is the only thing that can hold a
	// pass to 2 admissions: an over-admission on capacity shows as > 2 held.
	limits := map[string]int{capKey: 2, impl: 2, verify: DefaultVerifyLimit, acc: 1}
	g := func(key string) GroupRequest { return gr(key, limits[key]) }
	var trials []raceTrial
	for i := range 3 {
		sets := [][]GroupRequest{{g(capKey), g(impl)}, {g(capKey), g(acc)}, {g(capKey), g(verify)}}
		if i%2 == 1 {
			for _, set := range sets {
				slices.Reverse(set)
			}
		}
		trials = append(trials,
			raceTrial{shape: "implement", st: f.stage(), set: sets[0]},
			raceTrial{shape: "acceptance", st: f.stage(), set: sets[1]},
			raceTrial{shape: "verify", st: f.stage(), set: sets[2]},
			raceTrial{shape: "plan", st: f.stage(), set: []GroupRequest{g(capKey)}})
	}
	f.raceAndDrain(trials, limits, 6, func(round int) bool { return round%2 == 1 })
}

// TestStatus_BindingSkipsCapacity pins the binding preference at every store
// site: the top-level fields describe the stage-type group, not the capacity
// group. P holds capacity only, so the two groups' holder lists differ and a
// capacity-first pick is visible.
func TestStatus_BindingSkipsCapacity(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	capKey, impl := CapacityGroupKey("h1"), DefaultGroupKey("h1")
	capG, implG := gr(capKey, DefaultHostCapacity), gr(impl, DefaultLimit)
	p, i1, i2 := f.stage(), f.stage(), f.stage()
	f.mustAdmitSet(p, capG)
	if adm := f.mustAdmitSet(i1, capG, implG); len(adm.Holders) != 0 {
		t.Fatalf("admitted I1 top-level holders = %v, want local-implement's (none), not capacity's [P]", holderIDs(adm.Holders))
	}
	// I2 is position 1 in both groups (a tie): local-implement binds.
	if q := f.mustQueueSet(i2, 1, capG, implG); len(q.Holders) != 1 || q.Holders[0].StageID != i1.ID {
		t.Fatalf("queued I2 top-level holders = %v, want local-implement's [I1], not capacity's [P I1]", holderIDs(q.Holders))
	}
	st := f.positions(i1.ID, i2.ID)
	if h := st[i1.ID]; h.State != SlotHeld || h.GroupKey != impl || h.Limit != DefaultLimit || len(h.Holders) != 1 || h.Holders[0].StageID != i1.ID || len(h.Groups) != 2 {
		t.Fatalf("held I1 status = %+v, want top-level %s at limit 1 held by I1", h, impl)
	}
	if q := st[i2.ID]; q.State != SlotQueued || q.GroupKey != impl || q.Limit != DefaultLimit || q.Position != 1 || len(q.Holders) != 1 || len(q.Groups) != 2 {
		t.Fatalf("queued I2 status = %+v, want top-level %s at position 1", q, impl)
	}
	pr, err := f.store.Promote(ctx, PromoteRequest{StageID: i2.ID, Record: func(context.Context, pgx.Tx, Promotion) error { return nil }})
	if err != nil || pr.Changed || pr.GroupKey != impl || pr.Limit != DefaultLimit || len(pr.Holders) != 1 {
		t.Fatalf("Promote(I2) = %+v, %v; want an unchanged promotion naming %s", pr, err, impl)
	}
}
