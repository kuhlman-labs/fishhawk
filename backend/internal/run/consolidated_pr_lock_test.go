package run_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// consolidatedPROpenLocker probes the ConsolidatedPROpenLocker capability on a
// repository built by run.NewPostgresRepository — the exact constructor
// serve.go wires — so the tests fail if production loses the capability.
func consolidatedPROpenLocker(t *testing.T, repo run.Repository) run.ConsolidatedPROpenLocker {
	t.Helper()
	locker, ok := repo.(run.ConsolidatedPROpenLocker)
	if !ok {
		t.Fatalf("run.NewPostgresRepository returned %T, which does not implement run.ConsolidatedPROpenLocker", repo)
	}
	return locker
}

// heldConsolidatedPROpenLock takes the lock for runID on a goroutine and parks
// inside fn until the returned release is called. It returns once fn has
// started (the lock is held) and a channel carrying the holder's result.
// release is idempotent and also registered as a cleanup, so a failing test
// never strands the holder (and with it the pool close).
func heldConsolidatedPROpenLock(t *testing.T, locker run.ConsolidatedPROpenLocker, runID uuid.UUID) (release func(), done <-chan error) {
	t.Helper()
	held := make(chan struct{})
	releaseCh := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(releaseCh) }) }
	t.Cleanup(release)
	result := make(chan error, 1)
	go func() {
		result <- locker.WithConsolidatedPROpenLock(context.Background(), runID, func(context.Context) error {
			close(held)
			<-releaseCh
			return nil
		})
	}()
	select {
	case <-held:
	case err := <-result:
		t.Fatalf("holder returned before entering fn: %v", err)
	case <-time.After(timescale.D(5 * time.Second)):
		t.Fatal("holder never acquired the consolidated PR open lock")
	}
	return release, result
}

// awaitLockResult waits up to bound for a WithConsolidatedPROpenLock call's
// result, failing the test when it does not arrive.
func awaitLockResult(t *testing.T, ch <-chan error, bound time.Duration, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(bound):
		t.Fatalf("%s did not return within %s", what, bound)
		return nil
	}
}

// TestPostgres_WithConsolidatedPROpenLock_SerializesSameRun pins the mutual
// exclusion (#4110): while one caller holds the lock for a run, a second
// caller for the SAME run must not run its fn. Fixture mechanism: the holder
// parks inside fn; the waiter's fn sets a flag. With the pg_advisory_lock
// statement neutered the waiter's fn runs immediately, inside the window.
func TestPostgres_WithConsolidatedPROpenLock_SerializesSameRun(t *testing.T) {
	pool := pgtest.NewPool(t)
	locker := consolidatedPROpenLocker(t, run.NewPostgresRepository(pool))
	runID := uuid.New()

	release, holderDone := heldConsolidatedPROpenLock(t, locker, runID)

	var ran atomic.Bool
	waiterDone := make(chan error, 1)
	go func() {
		waiterDone <- locker.WithConsolidatedPROpenLock(context.Background(), runID, func(context.Context) error {
			ran.Store(true)
			return nil
		})
	}()

	select {
	case err := <-waiterDone:
		t.Fatalf("same-run waiter returned (err=%v, fn ran=%v) while the holder still held the lock", err, ran.Load())
	case <-time.After(timescale.D(300 * time.Millisecond)):
	}
	if ran.Load() {
		t.Fatal("same-run waiter's fn ran while the holder still held the lock")
	}

	release()
	if err := awaitLockResult(t, holderDone, timescale.D(5*time.Second), "holder"); err != nil {
		t.Fatalf("holder: %v", err)
	}
	if err := awaitLockResult(t, waiterDone, timescale.D(5*time.Second), "same-run waiter after release"); err != nil {
		t.Fatalf("waiter: %v", err)
	}
	if !ran.Load() {
		t.Fatal("same-run waiter's fn never ran after the holder released")
	}
}

// TestPostgres_WithConsolidatedPROpenLock_DistinctRunsDoNotBlock pins the key
// scoping: holding run X's lock never blocks run Y.
func TestPostgres_WithConsolidatedPROpenLock_DistinctRunsDoNotBlock(t *testing.T) {
	pool := pgtest.NewPool(t)
	locker := consolidatedPROpenLocker(t, run.NewPostgresRepository(pool))

	release, holderDone := heldConsolidatedPROpenLock(t, locker, uuid.New())

	var ran atomic.Bool
	otherDone := make(chan error, 1)
	go func() {
		otherDone <- locker.WithConsolidatedPROpenLock(context.Background(), uuid.New(), func(context.Context) error {
			ran.Store(true)
			return nil
		})
	}()
	if err := awaitLockResult(t, otherDone, timescale.D(5*time.Second), "distinct-run call while another run's lock is held"); err != nil {
		t.Fatalf("distinct-run call: %v", err)
	}
	if !ran.Load() {
		t.Fatal("distinct-run call returned without running fn")
	}

	release()
	if err := awaitLockResult(t, holderDone, timescale.D(5*time.Second), "holder"); err != nil {
		t.Fatalf("holder: %v", err)
	}
}

// TestPostgres_WithConsolidatedPROpenLock_CancelledWaitDoesNotRunFn pins the
// fail-closed acquisition path: a waiter whose ctx expires while the lock is
// held returns an error naming the lock and never runs fn. Fixture mechanism:
// a mutation that runs fn (or returns nil) on the acquisition-error path sets
// the flag (or drops the error). The follow-up acquisition runs on a SECOND
// pool — a guaranteed-distinct session, since advisory session locks are
// reentrant within one session — so a lock the cancelled waiter leaked into
// pool 1 would make it hang past its bound.
func TestPostgres_WithConsolidatedPROpenLock_CancelledWaitDoesNotRunFn(t *testing.T) {
	dbURL := pgtest.NewURL(t)
	newPool := func() *pgxpool.Pool {
		p, err := pgxpool.New(context.Background(), dbURL)
		if err != nil {
			t.Fatalf("connect pool: %v", err)
		}
		// Registered after NewURL's drop cleanup, so it runs first (LIFO).
		t.Cleanup(p.Close)
		return p
	}
	locker := consolidatedPROpenLocker(t, run.NewPostgresRepository(newPool()))
	otherLocker := consolidatedPROpenLocker(t, run.NewPostgresRepository(newPool()))
	runID := uuid.New()

	release, holderDone := heldConsolidatedPROpenLock(t, locker, runID)

	var ran atomic.Bool
	ctx, cancel := context.WithTimeout(context.Background(), timescale.D(200*time.Millisecond))
	err := locker.WithConsolidatedPROpenLock(ctx, runID, func(context.Context) error {
		ran.Store(true)
		return nil
	})
	cancel()
	if err == nil {
		t.Fatal("waiter with an expired ctx returned nil; want an acquisition error")
	}
	if !strings.Contains(err.Error(), "consolidated PR open lock") {
		t.Errorf("waiter error = %q, want it to name the consolidated PR open lock", err)
	}
	if ran.Load() {
		t.Fatal("waiter's fn ran although the lock was never acquired")
	}

	release()
	if err := awaitLockResult(t, holderDone, timescale.D(5*time.Second), "holder"); err != nil {
		t.Fatalf("holder: %v", err)
	}

	var reacquired atomic.Bool
	ctx2, cancel2 := context.WithTimeout(context.Background(), timescale.D(5*time.Second))
	defer cancel2()
	if err := otherLocker.WithConsolidatedPROpenLock(ctx2, runID, func(context.Context) error {
		reacquired.Store(true)
		return nil
	}); err != nil {
		t.Fatalf("re-acquire from a distinct session after the holder released: %v (a leaked lock would hang here)", err)
	}
	if !reacquired.Load() {
		t.Fatal("re-acquire returned without running fn")
	}
}
