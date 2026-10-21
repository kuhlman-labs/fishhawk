package pglock

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
	"weak"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// newPool builds a pool of maxConns connections on url and closes it at test
// end. A test whose RED run could strand a holder registers its caller-ctx
// cancel AFTER this, so the cancel runs first and pool.Close cannot hang.
func newPool(t *testing.T, url string, maxConns int32) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// callerCtx is a deadline-free caller context canceled at test end (after
// every earlier cleanup is registered, so it unwinds first).
func callerCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

// waitRelease parks a holder until release closes or its fn ctx ends, so a
// RED run's cleanup (which cancels the caller ctx) unwinds it.
func waitRelease(fctx context.Context, release <-chan struct{}) {
	select {
	case <-release:
	case <-fctx.Done():
	}
}

func opts(name string) Options { return Options{Name: name, FnTimeout: timescale.D(30 * time.Second)} }

func TestHolderLimit(t *testing.T) {
	if got := HolderLimit(nil); got != 1 {
		t.Errorf("HolderLimit(nil) = %d, want 1", got)
	}
	for _, tc := range []struct{ maxConns, want int }{
		{1, 1}, {2, 1}, {3, 1}, {4, 1}, {5, 1}, {6, 2}, {10, 4}, {12, 5},
	} {
		if got := holderLimit(tc.maxConns); got != tc.want {
			t.Errorf("holderLimit(MaxConns=%d) = %d, want %d (ReservedConns=%d)", tc.maxConns, got, tc.want, ReservedConns)
		}
	}
	url := pgtest.NewURL(t)
	if got := HolderLimit(newPool(t, url, 10)); got != 4 {
		t.Errorf("HolderLimit(10-connection pool) = %d, want 4", got)
	}
	if ReservedConns < 2 || MinPoolConns != ReservedConns+2 {
		t.Errorf("ReservedConns=%d MinPoolConns=%d, want ReservedConns >= 2 and MinPoolConns = ReservedConns+2", ReservedConns, MinPoolConns)
	}
}

func TestBudgetFor_OneBudgetPerPool(t *testing.T) {
	url := pgtest.NewURL(t)
	p1, p2 := newPool(t, url, 10), newPool(t, url, 6)
	b1 := BudgetFor(p1)
	if BudgetFor(p1) != b1 {
		t.Fatal("BudgetFor(pool) returned two budgets for one pool")
	}
	b2 := BudgetFor(p2)
	if b2 == b1 {
		t.Fatal("two pools share one budget")
	}
	if cap(b1.slots) != HolderLimit(p1) || cap(b2.slots) != HolderLimit(p2) {
		t.Fatalf("budget caps = %d, %d, want HolderLimit = %d, %d", cap(b1.slots), cap(b2.slots), HolderLimit(p1), HolderLimit(p2))
	}
	if nb := BudgetFor(nil); nb == nil || cap(nb.slots) != 1 || BudgetFor(nil) == nb {
		t.Fatal("BudgetFor(nil) must be a fresh, unregistered 1-slot budget")
	}
}

func TestBudgetFor_ForgetDropsEntry(t *testing.T) {
	pool := newPool(t, pgtest.NewURL(t), 6)
	b1 := BudgetFor(pool)
	key := weak.Make(pool)
	forget(key)
	registryMu.Lock()
	_, ok := registry[key]
	registryMu.Unlock()
	if ok {
		t.Fatal("forget left the pool's registry entry")
	}
	if BudgetFor(pool) == b1 {
		t.Fatal("BudgetFor after forget returned the forgotten budget")
	}
}

func TestWithSessionLock_RefusesInvalidCalls(t *testing.T) {
	url := pgtest.NewURL(t)
	pool := newPool(t, url, 6)
	other := newPool(t, url, 6)
	small := newPool(t, url, MinPoolConns-1)
	for _, tc := range []struct {
		name string
		pool *pgxpool.Pool
		b    *Budget
		opts Options
		want error
	}{
		{"nil pool", nil, BudgetFor(nil), opts("nil pool"), ErrInvalidOptions},
		{"zero FnTimeout", pool, BudgetFor(pool), Options{Name: "zero"}, ErrInvalidOptions},
		{"negative FnTimeout", pool, BudgetFor(pool), Options{Name: "neg", FnTimeout: -time.Second}, ErrInvalidOptions},
		{"nil budget", pool, nil, opts("nil budget"), ErrBudgetMismatch},
		{"foreign budget", pool, BudgetFor(other), opts("foreign"), ErrBudgetMismatch},
		{"pool too small", small, BudgetFor(small), opts("small"), ErrPoolTooSmall},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ran := false
			err := WithSessionLock(callerCtx(t), tc.pool, tc.b, 1, tc.opts, func(context.Context) error { ran = true; return nil })
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if ran {
				t.Fatal("fn ran on a refused call")
			}
		})
	}
}

func TestWithSessionLock_SerializesOneKey(t *testing.T) {
	pool := newPool(t, pgtest.NewURL(t), 6)
	ctx := callerCtx(t)
	b := BudgetFor(pool)
	held, release := make(chan struct{}), make(chan struct{})
	aDone := make(chan error, 1)
	go func() {
		aDone <- WithSessionLock(ctx, pool, b, 7, opts("A"), func(fctx context.Context) error {
			close(held)
			waitRelease(fctx, release)
			return nil
		})
	}()
	<-held
	var bRan atomic.Bool
	bDone := make(chan error, 1)
	go func() {
		bDone <- WithSessionLock(ctx, pool, b, 7, opts("B"), func(context.Context) error { bRan.Store(true); return nil })
	}()
	time.Sleep(timescale.D(200 * time.Millisecond))
	if bRan.Load() {
		t.Fatal("B ran while A held the key")
	}
	close(release)
	if err := <-aDone; err != nil {
		t.Fatalf("A: %v", err)
	}
	if err := <-bDone; err != nil || !bRan.Load() {
		t.Fatalf("B = %v ran=%v, want it to run once A released", err, bRan.Load())
	}
}

func TestWithSessionLock_DistinctKeysDoNotBlock(t *testing.T) {
	pool := newPool(t, pgtest.NewURL(t), 6)
	ctx := callerCtx(t)
	b := BudgetFor(pool)
	held, release := make(chan struct{}), make(chan struct{})
	aDone := make(chan error, 1)
	go func() {
		aDone <- WithSessionLock(ctx, pool, b, 11, opts("A"), func(fctx context.Context) error {
			close(held)
			waitRelease(fctx, release)
			return nil
		})
	}()
	<-held
	bctx, cancel := context.WithTimeout(ctx, timescale.D(2*time.Second))
	defer cancel()
	ran := false
	err := WithSessionLock(bctx, pool, b, 12, opts("B"), func(context.Context) error { ran = true; return nil })
	close(release)
	if err != nil || !ran {
		t.Fatalf("distinct key while A held = %v ran=%v, want it to run", err, ran)
	}
	if err := <-aDone; err != nil {
		t.Fatalf("A: %v", err)
	}
}

// TestWithSessionLock_WaiterHoldsNoSlot: on a 6-connection pool (budget 2) A
// holds key k and B waits on k; C on a distinct key must still enter fn,
// because a waiter releases its slot between tries. Counterfactual: a
// blocking pg_advisory_lock keeps B's slot, so C never gets one, RED.
func TestWithSessionLock_WaiterHoldsNoSlot(t *testing.T) {
	pool := newPool(t, pgtest.NewURL(t), 6)
	ctx := callerCtx(t)
	b := BudgetFor(pool)
	if cap(b.slots) != 2 {
		t.Fatalf("budget = %d, want 2 on a 6-connection pool", cap(b.slots))
	}
	held, release := make(chan struct{}), make(chan struct{})
	aDone := make(chan error, 1)
	go func() {
		aDone <- WithSessionLock(ctx, pool, b, 21, opts("A"), func(fctx context.Context) error {
			close(held)
			waitRelease(fctx, release)
			return nil
		})
	}()
	<-held
	var bRan atomic.Bool
	bDone := make(chan error, 1)
	go func() {
		bDone <- WithSessionLock(ctx, pool, b, 21, opts("B"), func(context.Context) error { bRan.Store(true); return nil })
	}()
	time.Sleep(timescale.D(100 * time.Millisecond)) // B is waiting on k
	cDone := make(chan error, 1)
	go func() {
		cDone <- WithSessionLock(ctx, pool, b, 22, opts("C"), func(context.Context) error { return nil })
	}()
	select {
	case err := <-cDone:
		if err != nil {
			t.Fatalf("C: %v", err)
		}
	case <-time.After(timescale.D(2 * time.Second)):
		t.Fatal("C on a distinct key never entered fn while B waited on a held key: the waiter keeps its budget slot")
	}
	if bRan.Load() {
		t.Fatal("B ran while A held the key")
	}
	close(release)
	if err := <-aDone; err != nil {
		t.Fatalf("A: %v", err)
	}
	if err := <-bDone; err != nil || !bRan.Load() {
		t.Fatalf("B = %v ran=%v", err, bRan.Load())
	}
}

func TestWithSessionLock_CanceledWaitNeverRunsFn(t *testing.T) {
	pool := newPool(t, pgtest.NewURL(t), 6)
	ctx := callerCtx(t)
	b := BudgetFor(pool)
	held, release := make(chan struct{}), make(chan struct{})
	aDone := make(chan error, 1)
	go func() {
		aDone <- WithSessionLock(ctx, pool, b, 31, opts("A"), func(fctx context.Context) error {
			close(held)
			waitRelease(fctx, release)
			return nil
		})
	}()
	<-held
	wctx, cancel := context.WithTimeout(ctx, timescale.D(100*time.Millisecond))
	defer cancel()
	ran := false
	err := WithSessionLock(wctx, pool, b, 31, opts("waiter"), func(context.Context) error { ran = true; return nil })
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) || ran {
		t.Fatalf("waiter past its deadline = %v ran=%v, want DeadlineExceeded and fn not run", err, ran)
	}
	if err := <-aDone; err != nil {
		t.Fatalf("A: %v", err)
	}
	canceled, cancelNow := context.WithCancel(ctx)
	cancelNow()
	if err := WithSessionLock(canceled, pool, b, 31, opts("canceled"), func(context.Context) error { ran = true; return nil }); !errors.Is(err, context.Canceled) || ran {
		t.Fatalf("canceled ctx = %v ran=%v, want context.Canceled and fn not run", err, ran)
	}
}

// TestWithSessionLock_BoundsFnWithDeadline: a caller ctx with NO deadline;
// fn blocks on its ctx, so only FnTimeout ends it. Counterfactual: fn on a
// plain WithCancel of the caller ctx never returns, RED.
func TestWithSessionLock_BoundsFnWithDeadline(t *testing.T) {
	pool := newPool(t, pgtest.NewURL(t), 6)
	ctx := callerCtx(t)
	b := BudgetFor(pool)
	done := make(chan error, 1)
	var hasDeadline bool
	go func() {
		done <- WithSessionLock(ctx, pool, b, 41, Options{Name: "bounded", FnTimeout: timescale.D(200 * time.Millisecond)},
			func(fctx context.Context) error {
				_, hasDeadline = fctx.Deadline()
				<-fctx.Done()
				return fctx.Err()
			})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("fn past FnTimeout = %v, want DeadlineExceeded", err)
		}
	case <-time.After(timescale.D(5 * time.Second)):
		t.Fatal("fn ran past FnTimeout under a deadline-free caller ctx")
	}
	if !hasDeadline {
		t.Fatal("fn's ctx carried no deadline")
	}
	nctx, cancel := context.WithTimeout(ctx, timescale.D(time.Second))
	defer cancel()
	ran := false
	if err := WithSessionLock(nctx, pool, b, 41, opts("next"), func(context.Context) error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("same-key holder after the bounded fn = %v ran=%v", err, ran)
	}
}

// TestWithSessionLock_PanicInFnReleasesLock: a panic propagates after the
// unlock, the connection release and the slot release. The probe is a
// SEPARATE non-pooled session: advisory locks are re-entrant per session, so
// a pooled probe could reuse a leaked one. Counterfactual: an inline (not
// deferred) unlock leaks the lock, RED.
func TestWithSessionLock_PanicInFnReleasesLock(t *testing.T) {
	url := pgtest.NewURL(t)
	pool := newPool(t, url, 6)
	b := BudgetFor(pool)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("fn's panic did not propagate")
			}
		}()
		_ = WithSessionLock(callerCtx(t), pool, b, 51, opts("panics"), func(context.Context) error { panic("boom") })
	}()
	probe, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	defer func() { _ = probe.Close(context.Background()) }()
	var took bool
	if err := probe.QueryRow(context.Background(), `SELECT pg_try_advisory_lock(51)`).Scan(&took); err != nil {
		t.Fatalf("probe lock: %v", err)
	}
	if !took {
		t.Fatal("a separate session cannot take the key after fn panicked: the lock leaked")
	}
	if _, err := probe.Exec(context.Background(), `SELECT pg_advisory_unlock(51)`); err != nil {
		t.Fatalf("probe unlock: %v", err)
	}
	if n := len(b.slots); n != 0 {
		t.Fatalf("budget slots in use after the panic = %d, want 0", n)
	}
}

// lockSessionPID is the backend pid holding the session advisory lock key.
func lockSessionPID(ctx context.Context, pool *pgxpool.Pool, key int64) (int32, error) {
	k := uint64(key)
	var pid int32
	err := pool.QueryRow(ctx, `SELECT pid FROM pg_locks WHERE locktype = 'advisory'
  AND classid = $1::bigint::oid AND objid = $2::bigint::oid AND objsubid = 1 AND granted`,
		int64(k>>32), int64(k&0xffffffff)).Scan(&pid)
	return pid, err
}

// killLockSession terminates the session holding key and waits for it to exit.
func killLockSession(ctx context.Context, pool *pgxpool.Pool, key int64) error {
	pid, err := lockSessionPID(ctx, pool, key)
	if err != nil {
		return fmt.Errorf("find the lock session: %w", err)
	}
	if _, err := pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
		return fmt.Errorf("terminate the lock session: %w", err)
	}
	deadline := time.Now().Add(timescale.D(5 * time.Second))
	for time.Now().Before(deadline) {
		var alive bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1)`, pid).Scan(&alive); err != nil {
			return err
		}
		if !alive {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("the lock session never exited")
}

// TestWithSessionLock_LapsedSessionSurfaces: the lock session dies mid-fn. A
// nil fn becomes ErrLockLapsed; an fn error is returned as is; the key is
// free afterwards. Counterfactual: ignoring the unlock result returns nil, RED.
func TestWithSessionLock_LapsedSessionSurfaces(t *testing.T) {
	pool := newPool(t, pgtest.NewURL(t), 6)
	ctx := callerCtx(t)
	b := BudgetFor(pool)
	err := WithSessionLock(ctx, pool, b, 61, opts("lapses"), func(fctx context.Context) error {
		return killLockSession(Detach(fctx), pool, 61)
	})
	if !errors.Is(err, ErrLockLapsed) {
		t.Fatalf("nil fn after its session died = %v, want ErrLockLapsed", err)
	}
	errFn := errors.New("fn failed")
	err = WithSessionLock(ctx, pool, b, 61, opts("lapses with error"), func(fctx context.Context) error {
		if kerr := killLockSession(Detach(fctx), pool, 61); kerr != nil {
			return kerr
		}
		return errFn
	})
	if !errors.Is(err, errFn) || errors.Is(err, ErrLockLapsed) {
		t.Fatalf("fn error after its session died = %v, want fn's error as is", err)
	}
	nctx, cancel := context.WithTimeout(ctx, timescale.D(time.Second))
	defer cancel()
	ran := false
	if err := WithSessionLock(nctx, pool, b, 61, opts("next"), func(context.Context) error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("next holder = %v ran=%v, want the key free", err, ran)
	}
}

// TestWithSessionLock_ReservesHeadroom (lock-headroom-reserved): on a
// MinPoolConns pool, saturate every holder slot with fns that each hold one
// extra connection, then a plain non-lock query must still complete within a
// bound. Counterfactual: ReservedConns = 0 lets the holders take every
// connection, so the query times out, RED.
func TestWithSessionLock_ReservesHeadroom(t *testing.T) {
	pool := newPool(t, pgtest.NewURL(t), MinPoolConns)
	ctx := callerCtx(t)
	b := BudgetFor(pool)
	limit := HolderLimit(pool)
	release := make(chan struct{})
	var acquired atomic.Int32
	done := make(chan error, limit+2)
	for i := 0; i < limit+2; i++ {
		key := int64(71 + i)
		go func() {
			done <- WithSessionLock(ctx, pool, b, key, opts("saturating"), func(fctx context.Context) error {
				c, err := pool.Acquire(fctx)
				if err != nil {
					return err
				}
				defer c.Release()
				acquired.Add(1)
				select {
				case <-release:
				case <-fctx.Done():
				}
				return nil
			})
		}()
	}
	deadline := time.Now().Add(timescale.D(5 * time.Second))
	for int(acquired.Load()) < limit && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := int(acquired.Load()); got != limit {
		close(release)
		t.Fatalf("holders inside with their extra connection = %d, want HolderLimit = %d", got, limit)
	}
	qctx, cancel := context.WithTimeout(ctx, timescale.D(time.Second))
	defer cancel()
	var one int
	qerr := pool.QueryRow(qctx, `SELECT 1`).Scan(&one)
	close(release)
	if qerr != nil {
		t.Fatalf("plain non-lock query while every holder slot is taken = %v, want it to complete (ReservedConns=%d headroom)", qerr, ReservedConns)
	}
	for i := 0; i < limit+2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("holder: %v", err)
		}
	}
}

// TestWithSessionLock_RefusesNestedLock (nested-lock-refused): a lock taken
// on fn's ctx, or any ctx derived from it, is refused with ErrNestedLock and
// never runs. Counterfactual: without the marker check the nested call waits
// on the 1-slot budget the outer fn holds until fn's deadline, RED.
func TestWithSessionLock_RefusesNestedLock(t *testing.T) {
	pool := newPool(t, pgtest.NewURL(t), MinPoolConns) // budget 1
	b := BudgetFor(pool)
	ctx := callerCtx(t)
	if InLock(ctx) {
		t.Fatal("a caller ctx reports InLock")
	}
	var nestedErr, derivedErr error
	nestedRan := false
	err := WithSessionLock(ctx, pool, b, 81, Options{Name: "outer", FnTimeout: timescale.D(2 * time.Second)}, func(fctx context.Context) error {
		if !InLock(fctx) {
			return errors.New("fn's ctx is not marked")
		}
		nestedErr = WithSessionLock(fctx, pool, b, 82, opts("inner"), func(context.Context) error { nestedRan = true; return nil })
		derived, cancel := context.WithTimeout(fctx, timescale.D(time.Second))
		defer cancel()
		derivedErr = WithSessionLock(derived, pool, b, 82, opts("inner derived"), func(context.Context) error { nestedRan = true; return nil })
		return nil
	})
	if err != nil {
		t.Fatalf("outer: %v", err)
	}
	if !errors.Is(nestedErr, ErrNestedLock) || !errors.Is(derivedErr, ErrNestedLock) || nestedRan {
		t.Fatalf("nested = %v, derived = %v, ran=%v; want ErrNestedLock twice and fn never run", nestedErr, derivedErr, nestedRan)
	}
}

// TestDetach_ChildCanAcquire: work detached from fn (pglock.Detach) is not
// refused as nested, outlives fn's cancellation, and keeps ctx values.
func TestDetach_ChildCanAcquire(t *testing.T) {
	pool := newPool(t, pgtest.NewURL(t), MinPoolConns)
	b := BudgetFor(pool)
	type valKey struct{}
	ctx := context.WithValue(callerCtx(t), valKey{}, "kept")
	var captured context.Context
	if err := WithSessionLock(ctx, pool, b, 91, opts("parent"), func(fctx context.Context) error {
		captured = fctx
		return nil
	}); err != nil {
		t.Fatalf("parent: %v", err)
	}
	if captured.Err() == nil {
		t.Fatal("fn's ctx is still live after the call returned")
	}
	child := Detach(captured)
	if InLock(child) || child.Err() != nil || child.Value(valKey{}) != "kept" {
		t.Fatalf("Detach: InLock=%v Err=%v value=%v, want unmarked, live, values kept", InLock(child), child.Err(), child.Value(valKey{}))
	}
	cctx, cancel := context.WithTimeout(child, timescale.D(time.Second))
	defer cancel()
	ran := false
	if err := WithSessionLock(cctx, pool, b, 92, opts("detached child"), func(context.Context) error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("detached child = %v ran=%v, want it to acquire", err, ran)
	}
}
