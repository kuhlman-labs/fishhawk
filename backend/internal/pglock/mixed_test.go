package pglock_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pglock"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/refinement"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// mixedPoolConns is the smallest pool on which three per-helper budgets (the
// pre-#4591 shape) can hold every connection as a lock session:
// 3 * HolderLimit(6) = 3 * 2 = 6. The shared budget holds 2.
const mixedPoolConns = 6

// lockHelper is one pglock-backed helper under test, bound to a fresh key.
type lockHelper struct {
	name string
	call func(ctx context.Context, fn func(context.Context) error) error
}

// mixedHelpers builds the three production helpers over ONE pool, through the
// constructors serve.go wires.
func mixedHelpers(t *testing.T, pool *pgxpool.Pool) []lockHelper {
	t.Helper()
	mq := mergequeue.NewPostgresStore(pool)
	locker, ok := run.NewPostgresRepository(pool).(run.ConsolidatedPROpenLocker)
	if !ok {
		t.Fatal("run.NewPostgresRepository does not implement run.ConsolidatedPROpenLocker")
	}
	ref := refinement.NewPostgresRepository(pool)
	return []lockHelper{
		{"merge intent", func(ctx context.Context, fn func(context.Context) error) error {
			return mq.WithMergeIntentLock(ctx, uuid.New(), fn)
		}},
		{"consolidated PR open", func(ctx context.Context, fn func(context.Context) error) error {
			return locker.WithConsolidatedPROpenLock(ctx, uuid.New(), fn)
		}},
		{"filing", func(ctx context.Context, fn func(context.Context) error) error {
			return ref.WithFilingLock(ctx, uuid.New(), fn)
		}},
	}
}

// newMixedPool opens a mixedPoolConns pool, pre-dials every connection (so
// dial latency never decides who wins a connection), and closes it at test
// end. Cleanups registered after it run first.
func newMixedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(pgtest.NewURL(t))
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	cfg.MaxConns = mixedPoolConns
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	conns := make([]*pgxpool.Conn, 0, mixedPoolConns)
	for range mixedPoolConns {
		c, err := pool.Acquire(context.Background())
		if err != nil {
			t.Fatalf("warm pool: %v", err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Release()
	}
	return pool
}

// startLoad runs n non-lock load loops on pool until test end. The stop is
// registered after the pool, so it unwinds before pool.Close.
func startLoad(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int) {
	t.Helper()
	lctx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for lctx.Err() == nil {
				_, _ = pool.Exec(lctx, `SELECT pg_sleep(0.005)`)
			}
		}()
	}
	t.Cleanup(func() { stop(); wg.Wait() })
}

// storeMax raises a to n when n is larger.
func storeMax(a *atomic.Int32, n int32) {
	for {
		m := a.Load()
		if n <= m || a.CompareAndSwap(m, n) {
			return
		}
	}
}

// TestSharedBudget_MixedHelpersUnderLoadProgress is #4591's done-means: 12
// distinct-key holders across the three helpers (4 each) on ONE 6-connection
// pool under 3 non-lock load loops, every fn needing two further pool
// connections, must all finish within a bound with no caller cancellation,
// and never more than HolderLimit fns run at once.
//
// Each fn waits (bounded) until mixedPoolConns holders are inside before it
// acquires, so if lock holders can ever take every connection they do.
// Counterfactual (CF-A, observed): a per-store budget (BudgetFor skipping the
// registry) admits 3 * 2 = 6 holders whose lock sessions take all 6
// connections; every fn then waits for a seventh, and the bound fails, RED.
func TestSharedBudget_MixedHelpersUnderLoadProgress(t *testing.T) {
	pool := newMixedPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	limit := pglock.HolderLimit(pool)
	if limit != 2 {
		t.Fatalf("HolderLimit(%d-connection pool) = %d, want 2", mixedPoolConns, limit)
	}
	startLoad(t, ctx, pool, 3)
	helpers := mixedHelpers(t, pool)

	var inside, maxInside atomic.Int32
	fn := func(fctx context.Context) error {
		n := inside.Add(1)
		defer inside.Add(-1)
		storeMax(&maxInside, n)
		barrier := time.Now().Add(timescale.D(100 * time.Millisecond))
		for inside.Load() < mixedPoolConns && time.Now().Before(barrier) && fctx.Err() == nil {
			time.Sleep(time.Millisecond)
		}
		for range 2 {
			var one int
			if err := pool.QueryRow(fctx, `SELECT 1`).Scan(&one); err != nil {
				return fmt.Errorf("fn query: %w", err)
			}
		}
		return nil
	}

	const perHelper = 4
	type result struct {
		helper string
		err    error
	}
	results := make(chan result, perHelper*len(helpers))
	for range perHelper {
		for _, h := range helpers {
			go func() { results <- result{h.name, h.call(ctx, fn)} }()
		}
	}
	timeout := time.After(timescale.D(20 * time.Second))
	for i := range perHelper * len(helpers) {
		select {
		case r := <-results:
			if r.err != nil {
				t.Errorf("%s holder: %v", r.helper, r.err)
			}
		case <-timeout:
			t.Fatalf("lock holders stalled: %d of %d returned within the bound (fns inside = %d, max inside = %d, pool stat: %d acquired of %d): lock sessions hold the connections their fns need",
				i, perHelper*len(helpers), inside.Load(), maxInside.Load(), pool.Stat().AcquiredConns(), mixedPoolConns)
		}
	}
	if got := maxInside.Load(); int(got) > limit {
		t.Fatalf("max concurrent fns across the three helpers = %d, want <= HolderLimit = %d (the budget is not shared)", got, limit)
	}
}

// TestSharedBudget_MixedHelpersReserveHeadroom (lock-headroom-reserved): with
// every holder slot taken across the three helpers, and every fn holding its
// one extra connection, under non-lock load, a plain non-lock query still
// completes within a bound. Counterfactual (observed): ReservedConns = 0 makes
// HolderLimit 3, the holders take 3 + 3 = all 6 connections, and the query
// times out, RED.
func TestSharedBudget_MixedHelpersReserveHeadroom(t *testing.T) {
	pool := newMixedPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startLoad(t, ctx, pool, 3)
	helpers := mixedHelpers(t, pool)
	limit := pglock.HolderLimit(pool)

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseAll)
	var holding atomic.Int32
	fn := func(fctx context.Context) error {
		c, err := pool.Acquire(fctx)
		if err != nil {
			return err
		}
		defer c.Release()
		holding.Add(1)
		select {
		case <-release:
		case <-fctx.Done():
		}
		return nil
	}
	const perHelper = 2
	done := make(chan error, perHelper*len(helpers))
	for range perHelper {
		for _, h := range helpers {
			go func() { done <- h.call(ctx, fn) }()
		}
	}
	deadline := time.Now().Add(timescale.D(5 * time.Second))
	for int(holding.Load()) < limit && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := int(holding.Load()); got != limit {
		t.Fatalf("fns holding their extra connection = %d, want HolderLimit = %d", got, limit)
	}
	qctx, qcancel := context.WithTimeout(ctx, timescale.D(time.Second))
	defer qcancel()
	var one int
	qerr := pool.QueryRow(qctx, `SELECT 1`).Scan(&one)
	releaseAll()
	if qerr != nil {
		t.Fatalf("plain non-lock query while every holder slot is taken = %v, want it to complete (ReservedConns = %d of %d connections stay free)", qerr, pglock.ReservedConns, mixedPoolConns)
	}
	for range perHelper * len(helpers) {
		if err := <-done; err != nil {
			t.Fatalf("holder: %v", err)
		}
	}
}

// TestSharedBudget_CrossHelperNestingRefused (nested-lock-refused, across
// helpers): each helper's fn ctx is marked, so taking ANY other helper's lock
// inside it fails closed with pglock.ErrNestedLock and never runs the inner
// fn, while a pglock.Detach'ed child of the same ctx acquires. Counterfactual
// (observed): without the marker check the inner lock is granted, RED.
func TestSharedBudget_CrossHelperNestingRefused(t *testing.T) {
	pool := newMixedPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	helpers := mixedHelpers(t, pool)
	for i, outer := range helpers {
		inner := helpers[(i+1)%len(helpers)]
		t.Run(outer.name+" then "+inner.name, func(t *testing.T) {
			var nestedErr, detachedErr error
			innerRan, detachedRan := false, false
			if err := outer.call(ctx, func(fctx context.Context) error {
				nestedErr = inner.call(fctx, func(context.Context) error { innerRan = true; return nil })
				dctx, dcancel := context.WithTimeout(pglock.Detach(fctx), timescale.D(2*time.Second))
				defer dcancel()
				detachedErr = inner.call(dctx, func(context.Context) error { detachedRan = true; return nil })
				return nil
			}); err != nil {
				t.Fatalf("outer %s: %v", outer.name, err)
			}
			if !errors.Is(nestedErr, pglock.ErrNestedLock) || innerRan {
				t.Fatalf("%s inside %s's fn = %v ran=%v, want pglock.ErrNestedLock and the inner fn never run", inner.name, outer.name, nestedErr, innerRan)
			}
			if detachedErr != nil || !detachedRan {
				t.Fatalf("%s on a Detach'ed child of %s's fn ctx = %v ran=%v, want it to acquire", inner.name, outer.name, detachedErr, detachedRan)
			}
		})
	}
}
