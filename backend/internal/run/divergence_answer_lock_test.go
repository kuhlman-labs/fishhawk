package run_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/pglock"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// divergenceAnswerLocker probes the DivergenceAnswerLocker capability on a
// repository built by run.NewPostgresRepository, the constructor serve.go
// wires, so the tests fail if production loses the capability (#3891).
func divergenceAnswerLocker(t *testing.T, repo run.Repository) run.DivergenceAnswerLocker {
	t.Helper()
	locker, ok := repo.(run.DivergenceAnswerLocker)
	if !ok {
		t.Fatalf("run.NewPostgresRepository returned %T, which does not implement run.DivergenceAnswerLocker", repo)
	}
	return locker
}

// heldDivergenceAnswerLock takes the lock for (runID, seq) on a goroutine and
// parks inside fn until the returned release is called. It returns once fn has
// started (the lock is held) and a channel carrying the holder's result.
// release is idempotent and registered as a cleanup.
func heldDivergenceAnswerLock(t *testing.T, locker run.DivergenceAnswerLocker, runID uuid.UUID, seq int64) (release func(), done <-chan error) {
	t.Helper()
	held := make(chan struct{})
	releaseCh := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(releaseCh) }) }
	t.Cleanup(release)
	result := make(chan error, 1)
	go func() {
		result <- locker.WithDivergenceAnswerLock(context.Background(), runID, seq, func(context.Context) error {
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
		t.Fatal("holder never acquired the divergence answer lock")
	}
	return release, result
}

// TestPostgres_WithDivergenceAnswerLock_SerializesSameQuestion pins the mutual
// exclusion (#3891): while one caller holds the lock for (run, seq), a second
// caller for the SAME (run, seq) must not run its fn until the holder
// releases. Fixture mechanism: the holder parks inside fn; the waiter's fn sets
// a flag. Without the advisory lock the waiter's fn runs inside the window.
func TestPostgres_WithDivergenceAnswerLock_SerializesSameQuestion(t *testing.T) {
	locker := divergenceAnswerLocker(t, run.NewPostgresRepository(pgtest.NewPool(t)))
	runID := uuid.New()

	release, holderDone := heldDivergenceAnswerLock(t, locker, runID, 7)

	var ran atomic.Bool
	waiterDone := make(chan error, 1)
	go func() {
		waiterDone <- locker.WithDivergenceAnswerLock(context.Background(), runID, 7, func(context.Context) error {
			ran.Store(true)
			return nil
		})
	}()

	select {
	case err := <-waiterDone:
		t.Fatalf("same-question waiter returned (err=%v, fn ran=%v) while the holder still held the lock", err, ran.Load())
	case <-time.After(timescale.D(300 * time.Millisecond)):
	}
	if ran.Load() {
		t.Fatal("same-question waiter's fn ran while the holder still held the lock")
	}

	release()
	if err := awaitLockResult(t, holderDone, timescale.D(5*time.Second), "holder"); err != nil {
		t.Fatalf("holder: %v", err)
	}
	if err := awaitLockResult(t, waiterDone, timescale.D(5*time.Second), "same-question waiter after release"); err != nil {
		t.Fatalf("waiter: %v", err)
	}
	if !ran.Load() {
		t.Fatal("same-question waiter's fn never ran after the holder released")
	}
}

// TestPostgres_WithDivergenceAnswerLock_DistinctQuestionsDoNotBlock pins the
// key scoping: holding (run, seq) never blocks (run, seq+1) or (otherRun, seq).
func TestPostgres_WithDivergenceAnswerLock_DistinctQuestionsDoNotBlock(t *testing.T) {
	locker := divergenceAnswerLocker(t, run.NewPostgresRepository(pgtest.NewPool(t)))
	runID := uuid.New()
	const seq int64 = 7

	release, holderDone := heldDivergenceAnswerLock(t, locker, runID, seq)

	for _, tc := range []struct {
		name  string
		runID uuid.UUID
		seq   int64
	}{
		{"same run, next sequence", runID, seq + 1},
		{"other run, same sequence", uuid.New(), seq},
	} {
		var ran atomic.Bool
		done := make(chan error, 1)
		go func() {
			done <- locker.WithDivergenceAnswerLock(context.Background(), tc.runID, tc.seq, func(context.Context) error {
				ran.Store(true)
				return nil
			})
		}()
		if err := awaitLockResult(t, done, timescale.D(5*time.Second), tc.name+" while (run, seq) is held"); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !ran.Load() {
			t.Fatalf("%s returned without running fn", tc.name)
		}
	}

	release()
	if err := awaitLockResult(t, holderDone, timescale.D(5*time.Second), "holder"); err != nil {
		t.Fatalf("holder: %v", err)
	}
}

// divergenceAnswerFnTimeout mirrors run's unexported constant (5m); a change
// there must change here.
const divergenceAnswerFnTimeout = 5 * time.Minute

// TestPostgres_WithDivergenceAnswerLock_FnCarriesDeadline: under a
// deadline-free caller ctx, fn's ctx carries the lock's fn deadline
// (divergenceAnswerFnTimeout) and the pglock nesting marker.
func TestPostgres_WithDivergenceAnswerLock_FnCarriesDeadline(t *testing.T) {
	locker := divergenceAnswerLocker(t, run.NewPostgresRepository(pgtest.NewPool(t)))
	start := time.Now()
	var deadline time.Time
	var hasDeadline, inLock bool
	if err := locker.WithDivergenceAnswerLock(context.Background(), uuid.New(), 1, func(fctx context.Context) error {
		deadline, hasDeadline = fctx.Deadline()
		inLock = pglock.InLock(fctx)
		return nil
	}); err != nil {
		t.Fatalf("WithDivergenceAnswerLock: %v", err)
	}
	if !hasDeadline {
		t.Fatal("fn's ctx has no deadline under a deadline-free caller ctx")
	}
	const slack = time.Minute
	if lo, hi := start.Add(divergenceAnswerFnTimeout-slack), time.Now().Add(divergenceAnswerFnTimeout+slack); deadline.Before(lo) || deadline.After(hi) {
		t.Fatalf("fn deadline = %s, want within [%s, %s] (divergenceAnswerFnTimeout = %s)", deadline, lo, hi, divergenceAnswerFnTimeout)
	}
	if !inLock {
		t.Fatal("fn's ctx is not marked as inside a pglock lock, so a nested lock would not be refused")
	}
}
