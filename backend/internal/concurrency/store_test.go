package concurrency

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// Every time fixture here is set by SQL relative to the database clock.
// dispatched_at is backdated with a direct UPDATE (the 0072 trigger stamps
// only on a transition INTO dispatched), and every such backdate moves the
// slot row's held_dispatched_at with it so the holder stays the SAME attempt
// the row admitted (approval condition 5). updated_at is never used: the 0001
// trigger overwrites it.

const testGroup = "local-implement:h1"

type fixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	repo  run.Repository
	store *PostgresStore
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := pgtest.NewPool(t)
	return &fixture{t: t, pool: pool, repo: run.NewPostgresRepository(pool), store: NewPostgresStore(pool)}
}

// stage creates a run with one implement stage parked at
// awaiting_host_dispatch.
func (f *fixture) stage() *run.Stage {
	f.t.Helper()
	ctx := context.Background()
	r, err := f.repo.CreateRun(ctx, run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "deadbeef", TriggerSource: run.TriggerCLI,
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
	st, err = f.repo.TransitionStage(ctx, st.ID, run.StageStateAwaitingHostDispatch, nil)
	if err != nil {
		f.t.Fatalf("park stage: %v", err)
	}
	return st
}

func req(st *run.Stage, limit int) Request {
	return Request{StageID: st.ID, RunID: st.RunID, From: run.StageStateAwaitingHostDispatch, GroupKey: testGroup, Limit: limit, Host: "h1"}
}

func (f *fixture) admit(st *run.Stage, limit int) Admission {
	f.t.Helper()
	a, err := f.store.Admit(context.Background(), req(st, limit))
	if err != nil {
		f.t.Fatalf("Admit(%s): %v", st.ID, err)
	}
	return a
}

func (f *fixture) mustAdmit(st *run.Stage) Admission {
	f.t.Helper()
	a := f.admit(st, 1)
	if !a.Admitted {
		f.t.Fatalf("Admit(%s) = queued at position %d (holders %v), want admitted", st.ID, a.Position, a.Holders)
	}
	return a
}

func (f *fixture) mustQueue(st *run.Stage, wantPos int) Admission {
	f.t.Helper()
	a := f.admit(st, 1)
	if a.Admitted {
		f.t.Fatalf("Admit(%s) admitted, want queued at position %d", st.ID, wantPos)
	}
	if a.Position != wantPos {
		f.t.Fatalf("Admit(%s) position = %d, want %d", st.ID, a.Position, wantPos)
	}
	return a
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (f *fixture) stageState(id uuid.UUID) run.StageState {
	f.t.Helper()
	st, err := f.repo.GetStage(context.Background(), id)
	if err != nil {
		f.t.Fatalf("get stage: %v", err)
	}
	return st.State
}

type slotRow struct {
	state      string
	enqueuedAt time.Time
	lastSeenAt time.Time
	acquiredAt *time.Time
}

func (f *fixture) slot(id uuid.UUID) (slotRow, bool) {
	f.t.Helper()
	var r slotRow
	err := f.pool.QueryRow(context.Background(),
		`SELECT state, enqueued_at, last_seen_at, acquired_at FROM stage_concurrency_slots WHERE stage_id = $1`, id).
		Scan(&r.state, &r.enqueuedAt, &r.lastSeenAt, &r.acquiredAt)
	if err != nil {
		return slotRow{}, false
	}
	return r, true
}

func (f *fixture) to(id uuid.UUID, states ...run.StageState) {
	f.t.Helper()
	for _, s := range states {
		if _, err := f.repo.TransitionStage(context.Background(), id, s, nil); err != nil {
			f.t.Fatalf("transition %s -> %s: %v", id, s, err)
		}
	}
}

// backdateHolder moves a holder's dispatched_at back by `ago` AND keeps the
// slot row's held_dispatched_at on the same attempt.
func (f *fixture) backdateHolder(id uuid.UUID, ago string) {
	f.t.Helper()
	f.exec(`UPDATE stages SET dispatched_at = now() - $2::interval WHERE id = $1`, id, ago)
	f.exec(`UPDATE stage_concurrency_slots s SET held_dispatched_at = st.dispatched_at FROM stages st WHERE st.id = s.stage_id AND s.stage_id = $1`, id)
}

func (f *fixture) staleQueued(id uuid.UUID) {
	f.t.Helper()
	f.exec(`UPDATE stage_concurrency_slots SET last_seen_at = clock_timestamp() - interval '61 seconds' WHERE stage_id = $1`, id)
}

// holdGroupLock takes the group's advisory lock on a dedicated connection so
// the next Admit misses it deterministically; the returned func releases it.
func (f *fixture) holdGroupLock() func() {
	f.t.Helper()
	ctx := context.Background()
	conn, err := f.pool.Acquire(ctx)
	if err != nil {
		f.t.Fatalf("acquire: %v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockKey(nil, testGroup)); err != nil {
		f.t.Fatalf("hold group lock: %v", err)
	}
	return func() {
		_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, lockKey(nil, testGroup))
		conn.Release()
	}
}

func TestAdmit_SecondQueuedBehindHolder(t *testing.T) {
	f := newFixture(t)
	a, b := f.stage(), f.stage()
	adm := f.mustAdmit(a)
	if adm.Stage == nil || adm.Stage.State != run.StageStateDispatched || adm.Stage.DispatchedAt == nil {
		t.Fatalf("admitted stage = %+v, want dispatched with dispatched_at", adm.Stage)
	}
	if row, _ := f.slot(a.ID); row.state != "held" {
		t.Fatalf("A slot state = %q, want held", row.state)
	}
	q := f.mustQueue(b, 1)
	if len(q.Holders) != 1 || q.Holders[0].StageID != a.ID || q.Holders[0].RunID != a.RunID {
		t.Fatalf("B holders = %+v, want [A]", q.Holders)
	}
	if !q.NewlyQueued || q.Contended {
		t.Fatalf("B NewlyQueued=%v Contended=%v, want true/false", q.NewlyQueued, q.Contended)
	}
	if got := f.stageState(b.ID); got != run.StageStateAwaitingHostDispatch {
		t.Fatalf("B stage = %s, want awaiting_host_dispatch (queued, never transitioned)", got)
	}
	// A second poll is not a new queue episode.
	if again := f.mustQueue(b, 1); again.NewlyQueued || !again.EnqueuedAt.Equal(q.EnqueuedAt) {
		t.Fatalf("second poll NewlyQueued=%v enqueued_at %v→%v, want false and unchanged", again.NewlyQueued, q.EnqueuedAt, again.EnqueuedAt)
	}
}

func TestAdmit_EmptyHoldersRenderAsList(t *testing.T) {
	f := newFixture(t)
	a := f.mustAdmit(f.stage())
	if a.Holders == nil {
		t.Fatal("Holders is nil, want an empty non-nil slice")
	}
	b, err := json.Marshal(a.Holders)
	if err != nil || string(b) != "[]" {
		t.Fatalf("holders JSON = %s (%v), want []", b, err)
	}
}

func TestAdmit_ReleasesWhenHolderSettlesOrParks(t *testing.T) {
	f := newFixture(t)
	a, b, c := f.stage(), f.stage(), f.stage()
	f.mustAdmit(a)
	f.mustQueue(b, 1)
	f.to(a.ID, run.StageStateRunning, run.StageStateSucceeded)
	f.mustAdmit(b)

	// B parks at a scope decision: not dispatched/running, so it releases.
	f.mustQueue(c, 1)
	f.to(b.ID, run.StageStateRunning, run.StageStateAwaitingScopeDecision)
	f.mustAdmit(c)
}

func TestAdmit_FIFO(t *testing.T) {
	f := newFixture(t)
	a, b, c := f.stage(), f.stage(), f.stage()
	f.mustAdmit(a)
	f.mustQueue(b, 1)
	f.mustQueue(c, 2)
	f.to(a.ID, run.StageStateRunning, run.StageStateSucceeded)
	f.mustQueue(c, 2) // B is still ahead
	f.mustAdmit(b)
}

func TestAdmit_StaleWaiterSkipped(t *testing.T) {
	f := newFixture(t)
	a, b, c := f.stage(), f.stage(), f.stage()
	f.mustAdmit(a)
	q := f.mustQueue(b, 1)
	f.mustQueue(c, 2)
	f.to(a.ID, run.StageStateRunning, run.StageStateSucceeded)
	f.staleQueued(b.ID)
	f.mustAdmit(c)
	// B's next poll restarts at the tail of a new episode.
	again := f.mustQueue(b, 1)
	if !again.NewlyQueued || !again.EnqueuedAt.After(q.EnqueuedAt) {
		t.Fatalf("B after stale: NewlyQueued=%v enqueued_at %v→%v, want a restart at the tail", again.NewlyQueued, q.EnqueuedAt, again.EnqueuedAt)
	}
}

// TestAdmit_StaleWaiterRestartsAtTailOnLockMiss is approval condition 1's
// interleaving: B goes stale while C stays fresh, B then MISSES the group
// lock and refreshes, and on B's next lock hit C is still ahead of B.
func TestAdmit_StaleWaiterRestartsAtTailOnLockMiss(t *testing.T) {
	f := newFixture(t)
	a, b, c := f.stage(), f.stage(), f.stage()
	f.mustAdmit(a)
	before := f.mustQueue(b, 1)
	f.mustQueue(c, 2)
	f.staleQueued(b.ID)

	release := f.holdGroupLock()
	miss := f.admit(b, 1)
	release()
	if !miss.Contended || miss.Admitted {
		t.Fatalf("B under a held group lock: Contended=%v Admitted=%v, want a contended queue", miss.Contended, miss.Admitted)
	}
	row, _ := f.slot(b.ID)
	if row.state != "queued" || !row.enqueuedAt.After(before.EnqueuedAt) || !miss.NewlyQueued {
		t.Fatalf("B row after contended refresh = %+v (NewlyQueued=%v), want queued with enqueued_at restarted past %v", row, miss.NewlyQueued, before.EnqueuedAt)
	}
	if miss.Position != 2 {
		t.Fatalf("B contended position = %d, want 2 (behind C)", miss.Position)
	}
	f.mustQueue(b, 2) // lock hit: C still ahead
	f.mustQueue(c, 1)
}

// TestAdmit_HeldPreviousEpisodeRestartsOnLockMiss is condition 7's second
// contended-path case: a previous-episode held row restarts as queued on a
// lock miss.
func TestAdmit_HeldPreviousEpisodeRestartsOnLockMiss(t *testing.T) {
	f := newFixture(t)
	a, b := f.stage(), f.stage()
	f.mustAdmit(a)
	// Re-open A (fix-up shape) and park it for host dispatch again.
	f.to(a.ID, run.StageStateRunning, run.StageStateAwaitingApproval, run.StageStatePending, run.StageStateAwaitingHostDispatch)
	f.mustAdmit(b)
	old, _ := f.slot(a.ID)
	if old.state != "held" || old.acquiredAt == nil {
		t.Fatalf("A row before = %+v, want the episode-1 held row", old)
	}

	release := f.holdGroupLock()
	miss := f.admit(a, 1)
	release()
	row, _ := f.slot(a.ID)
	if !miss.Contended || row.state != "queued" || !row.enqueuedAt.After(*old.acquiredAt) || row.acquiredAt != nil {
		t.Fatalf("A after contended refresh: Contended=%v row=%+v, want queued restarted after %v", miss.Contended, row, *old.acquiredAt)
	}
}

func TestAdmit_LivenessBackstop(t *testing.T) {
	t.Run("running past RunningStaleAfter without heartbeat releases", func(t *testing.T) {
		f := newFixture(t)
		a, b := f.stage(), f.stage()
		f.mustAdmit(a)
		f.to(a.ID, run.StageStateRunning)
		f.backdateHolder(a.ID, "46 minutes")
		f.mustAdmit(b)
	})
	t.Run("dispatched past DispatchedStaleAfter releases, running at the same age holds", func(t *testing.T) {
		f := newFixture(t)
		a, b := f.stage(), f.stage()
		f.mustAdmit(a)
		f.backdateHolder(a.ID, "20 minutes")
		f.mustAdmit(b)

		g := newFixture(t)
		c, d := g.stage(), g.stage()
		g.mustAdmit(c)
		g.to(c.ID, run.StageStateRunning)
		g.backdateHolder(c.ID, "20 minutes")
		g.mustQueue(d, 1)
	})
	t.Run("fresh heartbeat keeps a holder past the backstop", func(t *testing.T) {
		f := newFixture(t)
		a, b := f.stage(), f.stage()
		f.mustAdmit(a)
		f.to(a.ID, run.StageStateRunning)
		f.backdateHolder(a.ID, "46 minutes")
		f.exec(`UPDATE stages SET progress = jsonb_build_object('reported_at', to_jsonb(now())) WHERE id = $1`, a.ID)
		q := f.mustQueue(b, 1)
		if len(q.Holders) != 1 || q.Holders[0].StageID != a.ID {
			t.Fatalf("holders = %+v, want [A] kept live by its heartbeat", q.Holders)
		}
	})
	for _, bad := range []string{`"garbage"`, `"infinity"`, `"2026-13-45T00:00:00Z"`, `12`} {
		t.Run("malformed heartbeat "+bad+" falls back to dispatched_at", func(t *testing.T) {
			f := newFixture(t)
			a, b := f.stage(), f.stage()
			f.mustAdmit(a)
			f.to(a.ID, run.StageStateRunning)
			f.backdateHolder(a.ID, "46 minutes")
			f.exec(`UPDATE stages SET progress = jsonb_build_object('reported_at', $2::jsonb) WHERE id = $1`, a.ID, bad)
			f.mustAdmit(b)
		})
	}
}

func TestAdmit_StaleHeldRowFromPreviousEpisode(t *testing.T) {
	t.Run("re-opened holder queues behind the live holder", func(t *testing.T) {
		f := newFixture(t)
		a, b, c := f.stage(), f.stage(), f.stage()
		f.mustAdmit(a)
		f.to(a.ID, run.StageStateRunning, run.StageStateAwaitingApproval, run.StageStatePending, run.StageStateAwaitingHostDispatch)
		f.mustAdmit(b)
		old, _ := f.slot(a.ID)
		q := f.mustQueue(a, 1)
		row, _ := f.slot(a.ID)
		if row.state != "queued" || !row.enqueuedAt.After(*old.acquiredAt) || !q.NewlyQueued {
			t.Fatalf("A row = %+v NewlyQueued=%v, want queued, restarted after %v", row, q.NewlyQueued, *old.acquiredAt)
		}
		if len(q.Holders) != 1 || q.Holders[0].StageID != b.ID {
			t.Fatalf("holders = %+v, want [B] (A's old held row must not count)", q.Holders)
		}
		f.mustQueue(c, 2)
	})
	t.Run("a bypass re-dispatch is not the admitted attempt", func(t *testing.T) {
		f := newFixture(t)
		a, b := f.stage(), f.stage()
		f.mustAdmit(a)
		f.to(a.ID, run.StageStateRunning, run.StageStateAwaitingApproval, run.StageStatePending)
		// Bypass: straight to dispatched without the marker → a new dispatched_at.
		f.exec(`UPDATE stages SET state = 'dispatched' WHERE id = $1`, a.ID)
		f.mustAdmit(b)
	})
}

// TestAdmit_FailedTransactionCommitsNothing is the ONE failure contract: an
// error after the CAS rolls the whole admission back.
func TestAdmit_FailedTransactionCommitsNothing(t *testing.T) {
	boom := errors.New("injected after CAS")
	t.Run("never queued: nothing written, next stage admitted", func(t *testing.T) {
		f := newFixture(t)
		a, c := f.stage(), f.stage()
		f.store.afterCAS = func(context.Context) error { return boom }
		if _, err := f.store.Admit(context.Background(), req(a, 1)); !errors.Is(err, boom) {
			t.Fatalf("Admit err = %v, want the injected error", err)
		}
		f.store.afterCAS = nil
		if got := f.stageState(a.ID); got != run.StageStateAwaitingHostDispatch {
			t.Fatalf("A stage after failed admission = %s, want awaiting_host_dispatch", got)
		}
		if _, ok := f.slot(a.ID); ok {
			t.Fatal("A slot row exists after a failed admission, want none")
		}
		f.mustAdmit(c)
	})
	t.Run("queued before: row unchanged, next stage queues behind it until stale", func(t *testing.T) {
		f := newFixture(t)
		a, b, c := f.stage(), f.stage(), f.stage()
		f.mustAdmit(a)
		f.mustQueue(b, 1)
		before, _ := f.slot(b.ID)
		f.to(a.ID, run.StageStateRunning, run.StageStateSucceeded)
		f.store.afterCAS = func(context.Context) error { return boom }
		if _, err := f.store.Admit(context.Background(), req(b, 1)); !errors.Is(err, boom) {
			t.Fatalf("Admit err = %v, want the injected error", err)
		}
		f.store.afterCAS = nil
		if got := f.stageState(b.ID); got != run.StageStateAwaitingHostDispatch {
			t.Fatalf("B stage after failed admission = %s, want awaiting_host_dispatch", got)
		}
		after, _ := f.slot(b.ID)
		if after.state != before.state || !after.enqueuedAt.Equal(before.enqueuedAt) || !after.lastSeenAt.Equal(before.lastSeenAt) {
			t.Fatalf("B row changed by a failed admission: %+v → %+v", before, after)
		}
		f.mustQueue(c, 2)
		f.staleQueued(b.ID)
		f.mustAdmit(c)
	})
}

// TestAdmit_PoolSmallerThanConcurrentAdmits: eight admitters on a two-
// connection pool all return, and exactly one is admitted in the round.
func TestAdmit_PoolSmallerThanConcurrentAdmits(t *testing.T) {
	cfg, err := pgxpool.ParseConfig(pgtest.NewURL(t))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.MaxConns = 2
	small, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("small pool: %v", err)
	}
	t.Cleanup(small.Close)
	g := &fixture{t: t, pool: small, repo: run.NewPostgresRepository(small), store: NewPostgresStore(small)}
	const n = 8
	stages := make([]*run.Stage, n)
	for i := range stages {
		stages[i] = g.stage()
	}
	start := make(chan struct{})
	results := make([]Admission, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range stages {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), timescale.D(10*time.Second))
			defer cancel()
			<-start
			results[i], errs[i] = g.store.Admit(ctx, req(stages[i], 1))
		}(i)
	}
	close(start)
	wg.Wait()
	admitted := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("admitter %d: %v", i, err)
		}
		if results[i].Admitted {
			admitted++
		}
	}
	if admitted != 1 {
		t.Fatalf("admitted = %d of %d, want exactly 1 (never 0 while the slot is free)", admitted, n)
	}
	var dispatched, held int
	if err := small.QueryRow(context.Background(), `SELECT
  (SELECT count(*) FROM stages WHERE state = 'dispatched'),
  (SELECT count(*) FROM stage_concurrency_slots WHERE state = 'held')`).Scan(&dispatched, &held); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if dispatched != 1 || held != 1 {
		t.Fatalf("dispatched=%d held=%d, want 1/1", dispatched, held)
	}
}

// TestAdmit_TryLockSerializesDecision parks admitter X between its decision
// and its CAS (afterDecision) while Y runs: Y misses the group lock and is
// queued, and exactly one stage is admitted.
func TestAdmit_TryLockSerializesDecision(t *testing.T) {
	f := newFixture(t)
	x, y := f.stage(), f.stage()
	parked, resume := make(chan struct{}), make(chan struct{})
	xs := NewPostgresStore(f.pool)
	xs.afterDecision = func(context.Context, bool) { close(parked); <-resume }
	var xa Admission
	var xerr error
	done := make(chan struct{})
	go func() { defer close(done); xa, xerr = xs.Admit(context.Background(), req(x, 1)) }()
	<-parked
	ya, err := f.store.Admit(context.Background(), req(y, 1))
	close(resume)
	<-done
	if err != nil || xerr != nil {
		t.Fatalf("errors: x=%v y=%v", xerr, err)
	}
	if !xa.Admitted || ya.Admitted || !ya.Contended {
		t.Fatalf("x admitted=%v, y admitted=%v contended=%v; want x admitted, y contended-queued", xa.Admitted, ya.Admitted, ya.Contended)
	}
	if got := f.stageState(y.ID); got != run.StageStateAwaitingHostDispatch {
		t.Fatalf("y stage = %s, want awaiting_host_dispatch", got)
	}
}

// TestAdmit_SameRoundLoserDoesNotPreemptWinner is approval condition 2: the
// lock winner W is parked right after winning (afterLock) while loser L, which
// started its queue episode EARLIER in the same round, misses and commits a
// queued row. W must still be admitted — a free slot always admits someone.
func TestAdmit_SameRoundLoserDoesNotPreemptWinner(t *testing.T) {
	f := newFixture(t)
	w, l := f.stage(), f.stage()
	parked, resume := make(chan struct{}), make(chan struct{})
	ws := NewPostgresStore(f.pool)
	ws.afterLock = func(_ context.Context, hit bool) {
		if hit {
			close(parked)
			<-resume
		}
	}
	var wa Admission
	var werr error
	done := make(chan struct{})
	go func() { defer close(done); wa, werr = ws.Admit(context.Background(), req(w, 1)) }()
	<-parked
	la, err := f.store.Admit(context.Background(), req(l, 1))
	if err != nil || !la.Contended || la.Admitted {
		t.Fatalf("loser: %+v err=%v, want contended queue", la, err)
	}
	close(resume)
	<-done
	if werr != nil {
		t.Fatalf("winner: %v", werr)
	}
	lrow, _ := f.slot(l.ID)
	if !lrow.enqueuedAt.Before(wa.EnqueuedAt) {
		t.Fatalf("fixture: loser enqueued_at %v not before winner %v — the pre-emption case is not constructed", lrow.enqueuedAt, wa.EnqueuedAt)
	}
	if !wa.Admitted {
		t.Fatalf("winner queued at position %d behind a same-round loser, want admitted", wa.Position)
	}
	// Next round, the loser is first in line behind the holder.
	f.mustQueue(l, 1)
}

// TestAdmit_ContendedHeadAdmittedOnRetry pins the round the same-round rule
// does NOT cover, and the retry that closes it: the slot is free, older B and
// newer C are both queued and fresh, and their polls collide. C wins the lock
// and is parked (afterLock) while B polls: B misses and queues contended, C
// counts B ahead and stays queued, so nobody is admitted that round. B's
// immediate retry (the slot waiter's contended retry) then admits B.
func TestAdmit_ContendedHeadAdmittedOnRetry(t *testing.T) {
	f := newFixture(t)
	a, b, c := f.stage(), f.stage(), f.stage()
	f.mustAdmit(a)
	f.mustQueue(b, 1)
	f.mustQueue(c, 2)
	f.to(a.ID, run.StageStateRunning, run.StageStateSucceeded) // the slot is free

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
	go func() { defer close(done); ca, cerr = cs.Admit(context.Background(), req(c, 1)) }()
	<-parked
	ba, err := f.store.Admit(context.Background(), req(b, 1))
	if err != nil || ba.Admitted || !ba.Contended {
		t.Fatalf("B while C holds the lock: %+v err=%v, want contended queue", ba, err)
	}
	close(resume)
	<-done
	if cerr != nil || ca.Admitted || ca.Position != 2 {
		t.Fatalf("C: %+v err=%v, want queued at 2 behind the older B", ca, cerr)
	}
	if f.stageState(b.ID) != run.StageStateAwaitingHostDispatch || f.stageState(c.ID) != run.StageStateAwaitingHostDispatch {
		t.Fatal("fixture: someone was admitted in the colliding round — the gap is not constructed")
	}
	// B's immediate retry: the lock is free and B is first in line.
	if retry := f.admit(b, 1); !retry.Admitted {
		t.Fatalf("B retry queued at position %d, want admitted on the free slot", retry.Position)
	}
}

// TestAdmit_RecordsAdmissionNonce: an admission records the request's nonce on
// the held row and StatusForStages reports it; a queue upsert clears it (a
// queued row never carries one); an admission without a nonce records NULL.
func TestAdmit_RecordsAdmissionNonce(t *testing.T) {
	f := newFixture(t)
	nonce := func(id uuid.UUID) *string {
		t.Helper()
		var n *string
		if err := f.pool.QueryRow(context.Background(), `SELECT admission_nonce FROM stage_concurrency_slots WHERE stage_id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("read nonce: %v", err)
		}
		return n
	}
	a, b := f.stage(), f.stage()
	ra := req(a, 1)
	ra.AdmissionNonce = "nonce-a"
	if adm, err := f.store.Admit(context.Background(), ra); err != nil || !adm.Admitted {
		t.Fatalf("Admit(a) = %+v, %v; want admitted", adm, err)
	}
	if n := nonce(a.ID); n == nil || *n != "nonce-a" {
		t.Fatalf("a's row nonce = %v, want nonce-a", n)
	}
	st, err := f.store.StatusForStages(context.Background(), []uuid.UUID{a.ID})
	if err != nil || st[a.ID].AdmissionNonce != "nonce-a" {
		t.Fatalf("a status = %+v, %v; want admission nonce nonce-a", st[a.ID], err)
	}

	rb := req(b, 1)
	rb.AdmissionNonce = "nonce-b"
	if adm, err := f.store.Admit(context.Background(), rb); err != nil || adm.Admitted {
		t.Fatalf("Admit(b) = %+v, %v; want queued", adm, err)
	}
	if n := nonce(b.ID); n != nil {
		t.Fatalf("queued b's row nonce = %q, want NULL (only an admission records one)", *n)
	}

	// a's attempt ends and the stage re-opens: its previous-episode held row
	// restarts as queued and loses the old admission's nonce.
	f.to(a.ID, run.StageStateRunning, run.StageStateAwaitingApproval, run.StageStatePending, run.StageStateAwaitingHostDispatch)
	f.mustQueue(a, 2)
	if n := nonce(a.ID); n != nil {
		t.Fatalf("re-queued a's row nonce = %q, want NULL", *n)
	}

	// b is admitted by a request carrying no nonce: NULL, reported as "".
	if adm := f.admit(b, 1); !adm.Admitted {
		t.Fatalf("Admit(b, no nonce) queued at %d, want admitted", adm.Position)
	}
	if n := nonce(b.ID); n != nil {
		t.Fatalf("b's row nonce = %q, want NULL for a nonce-less admission", *n)
	}
	if st, err := f.store.StatusForStages(context.Background(), []uuid.UUID{b.ID}); err != nil || st[b.ID].State != SlotHeld || st[b.ID].AdmissionNonce != "" {
		t.Fatalf("b status = %+v, %v; want held with no admission nonce", st[b.ID], err)
	}
}

func TestAdmit_LimitTwoAdmitsTwo(t *testing.T) {
	f := newFixture(t)
	a, b, c := f.stage(), f.stage(), f.stage()
	for _, st := range []*run.Stage{a, b} {
		if !f.admit(st, 2).Admitted {
			t.Fatalf("limit 2: %s queued, want admitted", st.ID)
		}
	}
	if q := f.admit(c, 2); q.Admitted || q.Position != 1 || len(q.Holders) != 2 {
		t.Fatalf("limit 2 third: %+v, want queued at 1 behind two holders", q)
	}
}

func TestAdmit_StageDriftRefused(t *testing.T) {
	f := newFixture(t)
	a := f.stage()
	r := req(a, 1)
	r.From = run.StageStatePending
	_, err := f.store.Admit(context.Background(), r)
	var changed run.StageStateChangedError
	if !errors.As(err, &changed) || changed.Actual != run.StageStateAwaitingHostDispatch {
		t.Fatalf("err = %v, want StageStateChangedError(actual awaiting_host_dispatch)", err)
	}
	if _, ok := f.slot(a.ID); ok {
		t.Fatal("slot row written on drift, want none")
	}
	if got := f.stageState(a.ID); got != run.StageStateAwaitingHostDispatch {
		t.Fatalf("stage = %s after drift refusal", got)
	}
}

func TestAdmit_InvalidRequestRefused(t *testing.T) {
	f := newFixture(t)
	a := f.stage()
	cases := map[string]func(*Request){
		"nil stage":         func(r *Request) { r.StageID = uuid.Nil },
		"nil run":           func(r *Request) { r.RunID = uuid.Nil },
		"empty group":       func(r *Request) { r.GroupKey = "" },
		"limit 0":           func(r *Request) { r.Limit = 0 },
		"limit 65":          func(r *Request) { r.Limit = MaxLimit + 1 },
		"from dispatched":   func(r *Request) { r.From = run.StageStateDispatched },
		"from running":      func(r *Request) { r.From = run.StageStateRunning },
		"from empty string": func(r *Request) { r.From = "" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			r := req(a, 1)
			mut(&r)
			if _, err := f.store.Admit(context.Background(), r); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
		})
	}
	t.Run("run id of another run", func(t *testing.T) {
		r := req(a, 1)
		r.RunID = f.stage().RunID
		if _, err := f.store.Admit(context.Background(), r); !errors.Is(err, run.ErrNotFound) {
			t.Fatalf("err = %v, want run.ErrNotFound", err)
		}
	})
	if _, ok := f.slot(a.ID); ok {
		t.Fatal("slot row written by a refused request")
	}
}

// TestAdmit_OtherAccountHolderNeitherCountedNorDisclosed is approval
// condition 6. The pgtest role is a superuser and BYPASSES RLS, which is
// exactly the production runtime posture today; the scoping under test is the
// store's explicit runs.account_id match, not RLS.
func TestAdmit_OtherAccountHolderNeitherCountedNorDisclosed(t *testing.T) {
	f := newFixture(t)
	acct1, acct2 := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{acct1, acct2} {
		f.exec(`INSERT INTO accounts (id, account_key) VALUES ($1, $2)`, id, "cc-"+id.String())
	}
	a, b, c := f.stage(), f.stage(), f.stage()
	f.exec(`UPDATE runs SET account_id = $1 WHERE id = $2`, acct1, a.RunID)
	f.exec(`UPDATE runs SET account_id = $1 WHERE id = $2`, acct2, b.RunID)
	f.exec(`UPDATE runs SET account_id = $1 WHERE id = $2`, acct1, c.RunID)
	f.mustAdmit(a)
	adm := f.mustAdmit(b) // same group key, other account
	if len(adm.Holders) != 0 {
		t.Fatalf("account-2 admission disclosed holders %+v, want none", adm.Holders)
	}
	q := f.mustQueue(c, 1)
	if len(q.Holders) != 1 || q.Holders[0].StageID != a.ID {
		t.Fatalf("account-1 holders = %+v, want only A", q.Holders)
	}
	st, err := f.store.StatusForStages(context.Background(), []uuid.UUID{b.ID, c.ID})
	if err != nil {
		t.Fatalf("StatusForStages: %v", err)
	}
	for _, h := range st[c.ID].Holders {
		if h.StageID == b.ID {
			t.Fatalf("C's status disclosed account-2 holder B: %+v", st[c.ID].Holders)
		}
	}
	for _, h := range st[b.ID].Holders {
		if h.StageID == a.ID {
			t.Fatalf("B's status disclosed account-1 holder A: %+v", st[b.ID].Holders)
		}
	}
}

func TestStatusForStages(t *testing.T) {
	f := newFixture(t)
	a, b, settled, bypass := f.stage(), f.stage(), f.stage(), f.stage()
	held := f.mustAdmit(a)
	f.mustQueue(b, 1)
	// settled: admitted then finished.
	f.to(a.ID, run.StageStateRunning)
	f.exec(`UPDATE stages SET progress = jsonb_build_object('reported_at', to_jsonb(now())) WHERE id = $1`, a.ID)
	f.exec(`INSERT INTO stage_concurrency_slots (stage_id, run_id, group_key, slot_limit, host, state, acquired_at, held_dispatched_at)
VALUES ($1, $2, $3, 1, 'h1', 'held', now(), now() - interval '1 hour')`, settled.ID, settled.RunID, testGroup)
	f.to(bypass.ID, run.StageStateDispatched) // dispatched without the marker: no row

	got, err := f.store.StatusForStages(context.Background(), []uuid.UUID{a.ID, b.ID, settled.ID, bypass.ID})
	if err != nil {
		t.Fatalf("StatusForStages: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("statuses = %v, want only A (held) and B (queued)", got)
	}
	sa := got[a.ID]
	if sa.State != SlotHeld || sa.AcquiredAt == nil || sa.Host != "h1" || sa.HeldDispatchedAt == nil ||
		!sa.HeldDispatchedAt.Equal(*held.Stage.DispatchedAt) || sa.GroupKey != testGroup || sa.Limit != 1 {
		t.Fatalf("A status = %+v, want held with host h1 and the admitted attempt", sa)
	}
	sb := got[b.ID]
	if sb.State != SlotQueued || sb.Position != 1 || !sb.WaiterLive || len(sb.Holders) != 1 || sb.Holders[0].StageID != a.ID || sb.AcquiredAt != nil {
		t.Fatalf("B status = %+v, want queued at 1, waiter live, holder A", sb)
	}
	f.staleQueued(b.ID)
	got, err = f.store.StatusForStages(context.Background(), []uuid.UUID{b.ID})
	if err != nil {
		t.Fatalf("StatusForStages: %v", err)
	}
	if got[b.ID].WaiterLive {
		t.Fatalf("B status after stale = %+v, want waiter_live false", got[b.ID])
	}
	if empty, err := f.store.StatusForStages(context.Background(), nil); err != nil || len(empty) != 0 {
		t.Fatalf("StatusForStages(nil) = %v, %v", empty, err)
	}
}

func TestGroupKeysAndLockDomain(t *testing.T) {
	if got := DefaultGroupKey("h1"); got != "local-implement:h1" {
		t.Fatalf("DefaultGroupKey = %q", got)
	}
	if got := DefaultGroupKey(""); got != "local-implement:unknown" {
		t.Fatalf("DefaultGroupKey(\"\") = %q", got)
	}
	if got := NamedGroupKey("o/r", "deploy-target"); got != "spec:o/r:deploy-target" {
		t.Fatalf("NamedGroupKey = %q", got)
	}
	acct := uuid.New()
	if lockKey(nil, "g") == lockKey(&acct, "g") || lockKey(nil, "g") == lockKey(nil, "h") {
		t.Fatal("lock keys collide across account or group")
	}
	if k1, k2 := lockKey(&acct, "g"), lockKey(&acct, "g"); k1 != k2 {
		t.Fatal("lock key is not deterministic")
	}
}
