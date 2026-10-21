// Package pglock runs Postgres SESSION-level advisory locks that hold one
// pooled connection while the caller's fn runs on others, and puts every such
// lock holder on ONE holder budget per pool (#4591).
//
// The hazard it closes: a holder keeps its lock connection for as long as fn
// runs, and fn acquires further connections from the SAME pool. With enough
// concurrent holders the lock connections alone exhaust the pool and every fn
// waits forever for a connection only another waiting fn could release
// (hold-and-wait). A per-helper bound does not close it, because two helpers
// with independent bounds can together fill the pool; the budget must be
// shared by every helper on the pool, which BudgetFor provides.
//
// # Budget sizing
//
// HolderLimit(pool) = max(1, (MaxConns - ReservedConns) / 2), with
// ReservedConns = 2. Holders together hold at most HolderLimit lock
// connections plus HolderLimit fn connections, so at least ReservedConns
// connections stay free for non-lock work (health checks, the boot sweep,
// tickers, handlers) even when every holder slot is taken. A pool smaller
// than MinPoolConns (ReservedConns + one lock connection + one fn connection)
// cannot run a holder without eating into that headroom, so WithSessionLock
// refuses it with ErrPoolTooSmall rather than risk a stall.
//
// The sizing is deadlock-free only under two invariants:
//
//  1. At most ONE extra connection per fn. A holder's fn holds at most one
//     further pool connection at a time (sequential queries, or one
//     transaction at a time). A fn that holds a transaction open while it
//     acquires a second connection reopens hold-and-wait.
//  2. No nesting. A fn never takes another pglock lock. This is ENFORCED: fn
//     runs on a context marked by this package, and WithSessionLock refuses a
//     marked context with ErrNestedLock before it takes a slot or a
//     connection.
//
// Goroutines spawned from fn and detached from it (for example the server's
// pumpMergeCandidateQueueDetached) are ordinary NON-lock load bounded by their
// own contexts; they are NOT fn's one extra connection. Such work must start
// from Detach(ctx), which drops fn's cancellation and clears the nesting
// marker, so a lock it legitimately takes later is not refused as nested.
//
// # Acquisition
//
// Each attempt takes a budget slot, then a pooled connection, then TRIES the
// lock (pg_try_advisory_lock). A held lock releases both the slot and the
// connection and the call re-tries every Poll, so a waiter holds nothing
// between tries; the wait is bounded only by the caller's ctx, and a canceled
// ctx returns its error with fn never run.
//
// # fn deadline, unlock, lapse
//
// fn runs on a context bounded by Options.FnTimeout (required, and applied
// even when the caller's ctx has no deadline). The bound is cooperative: it
// reaches fn only through the ctx fn is handed, and WithSessionLock never
// abandons a running fn, because releasing the lock under it would break the
// exclusion. The unlock is deferred, so a panic in fn still unlocks, releases
// the connection and the slot, then propagates. The unlock runs on a context
// that ignores the caller's cancellation (bounded by UnlockTimeout). When it
// errors or finds no lock held, the connection is CLOSED before release, so a
// lock-holding session never returns to the pool, and if fn returned nil the
// call returns an error wrapping ErrLockLapsed (the session died mid-fn, so
// the exclusion may have lapsed). fn's own error always wins.
package pglock

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"
	"weak"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// ReservedConns is the number of pool connections the holder budget
	// leaves to non-lock work when every holder slot is taken.
	ReservedConns = 2
	// MinPoolConns is the smallest pool a holder may run on: the reserve
	// plus one lock connection and one fn connection.
	MinPoolConns = ReservedConns + 2
	// Poll is how often WithSessionLock re-tries a held lock.
	Poll = 25 * time.Millisecond
	// UnlockTimeout bounds the unlock after fn.
	UnlockTimeout = 5 * time.Second
)

var (
	// ErrLockLapsed is a call whose fn RAN and returned nil, but whose lock
	// could not be confirmed held through its release (the lock session
	// failed, or the unlock found no lock), so another holder may have
	// overlapped fn. fn's writes stand; the caller decides.
	ErrLockLapsed = errors.New("pglock: advisory lock lapsed")
	// ErrInvalidOptions is a call refused before any slot or connection is
	// taken: a nil pool or a non-positive FnTimeout. fn never ran.
	ErrInvalidOptions = errors.New("pglock: invalid lock options")
	// ErrBudgetMismatch is a nil budget, or one BudgetFor built for another
	// pool. fn never ran.
	ErrBudgetMismatch = errors.New("pglock: budget does not belong to the pool")
	// ErrPoolTooSmall is a pool with fewer than MinPoolConns connections.
	// fn never ran.
	ErrPoolTooSmall = errors.New("pglock: pool too small for a lock holder")
	// ErrNestedLock is a lock requested on a context marked as inside
	// another pglock fn (invariant 2). fn never ran.
	ErrNestedLock = errors.New("pglock: nested lock refused")
)

// Budget is one pool's holder budget: at most HolderLimit concurrent lock
// holders across every helper built on that pool. It keeps no strong
// reference to the pool, so the registry never keeps a closed pool alive.
type Budget struct {
	pool     weak.Pointer[pgxpool.Pool]
	maxConns int
	slots    chan struct{}
}

// HolderLimit is max(1, (MaxConns - ReservedConns) / 2); 1 for a nil pool.
func HolderLimit(pool *pgxpool.Pool) int {
	if pool == nil {
		return 1
	}
	return holderLimit(int(pool.Config().MaxConns))
}

func holderLimit(maxConns int) int {
	return max(1, (maxConns-ReservedConns)/2)
}

var (
	registryMu sync.Mutex
	registry   = map[weak.Pointer[pgxpool.Pool]]*Budget{}
)

// BudgetFor returns the ONE budget every caller on pool shares. The entry is
// dropped once the pool is garbage collected. A nil pool gets a fresh,
// unregistered 1-slot budget (WithSessionLock refuses a nil pool anyway).
func BudgetFor(pool *pgxpool.Pool) *Budget {
	if pool == nil {
		return &Budget{slots: make(chan struct{}, 1)}
	}
	key := weak.Make(pool)
	registryMu.Lock()
	defer registryMu.Unlock()
	if b, ok := registry[key]; ok {
		return b
	}
	n := int(pool.Config().MaxConns)
	b := &Budget{pool: key, maxConns: n, slots: make(chan struct{}, holderLimit(n))}
	registry[key] = b
	// The cleanup arg is the weak key, never the pool, so it cannot keep the
	// pool reachable.
	runtime.AddCleanup(pool, forget, key)
	return b
}

// forget drops a collected pool's budget.
func forget(key weak.Pointer[pgxpool.Pool]) {
	registryMu.Lock()
	defer registryMu.Unlock()
	delete(registry, key)
}

// Options configures one lock helper.
type Options struct {
	// Name prefixes every error and names the lock in an ErrNestedLock.
	Name string
	// FnTimeout bounds fn's context. Required (> 0).
	FnTimeout time.Duration
}

// heldKey is the context key marking a context as inside a pglock fn.
type heldKey struct{}

// held is the marker value; a nil *held (set by Detach) clears it.
type held struct{ name string }

func heldBy(ctx context.Context) *held {
	h, _ := ctx.Value(heldKey{}).(*held)
	return h
}

// InLock reports whether ctx is (derived from) a context a pglock fn was
// handed and not since passed through Detach.
func InLock(ctx context.Context) bool { return heldBy(ctx) != nil }

// Detach returns a context for work spawned from fn that outlives or runs
// beside it: fn's cancellation and deadline are dropped (context values are
// kept) and the nesting marker is cleared. The caller must bound the detached
// work with its own deadline; it is ordinary non-lock load, not fn's extra
// connection.
func Detach(ctx context.Context) context.Context {
	return context.WithValue(context.WithoutCancel(ctx), heldKey{}, (*held)(nil))
}

// WithSessionLock runs fn while holding the session-level advisory lock on
// key, under budget b (which must be BudgetFor(pool)). See the package doc
// for the acquisition, deadline, unlock and lapse contract.
func WithSessionLock(ctx context.Context, pool *pgxpool.Pool, b *Budget, key int64, opts Options, fn func(context.Context) error) error {
	if outer := heldBy(ctx); outer != nil {
		return fmt.Errorf("%w: %s requested inside %s's fn (a lock fn must not take another pglock lock)",
			ErrNestedLock, opts.Name, outer.name)
	}
	if pool == nil {
		return fmt.Errorf("%w: %s: nil pool", ErrInvalidOptions, opts.Name)
	}
	if opts.FnTimeout <= 0 {
		return fmt.Errorf("%w: %s: FnTimeout %s must be positive", ErrInvalidOptions, opts.Name, opts.FnTimeout)
	}
	if b == nil || b.pool != weak.Make(pool) {
		return fmt.Errorf("%w: %s", ErrBudgetMismatch, opts.Name)
	}
	if b.maxConns < MinPoolConns {
		return fmt.Errorf("%w: %s: pool MaxConns=%d, need at least %d (%d reserved for non-lock work + one lock connection + one fn connection)",
			ErrPoolTooSmall, opts.Name, b.maxConns, MinPoolConns, ReservedConns)
	}
	for {
		ran, err := attempt(ctx, pool, b, key, opts, fn)
		if ran || err != nil {
			return err
		}
		t := time.NewTimer(Poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("%s: %w", opts.Name, ctx.Err())
		case <-t.C:
		}
	}
}

// attempt is one try: ran reports fn ran. The slot and the connection are
// released before it returns, panic or not.
func attempt(ctx context.Context, pool *pgxpool.Pool, b *Budget, key int64, opts Options, fn func(context.Context) error) (bool, error) {
	select {
	case b.slots <- struct{}{}:
	case <-ctx.Done():
		return false, fmt.Errorf("%s: %w", opts.Name, ctx.Err())
	}
	defer func() { <-b.slots }()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("%s: acquire connection: %w", opts.Name, err)
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&got); err != nil {
		// The lock may have been granted before the error: never return a
		// possibly lock-holding session to the pool.
		_ = conn.Conn().Close(context.Background())
		return false, fmt.Errorf("%s: %w", opts.Name, err)
	}
	if !got {
		return false, nil
	}
	return true, runLocked(ctx, conn, key, opts, fn)
}

// runLocked runs fn on its bounded, marked context and unlocks in a defer, so
// a panic still unlocks. err is named so endLock can report a lapse.
func runLocked(ctx context.Context, conn *pgxpool.Conn, key int64, opts Options, fn func(context.Context) error) (err error) {
	fnCtx, cancel := context.WithTimeout(ctx, opts.FnTimeout)
	defer cancel()
	defer endLock(ctx, conn, key, opts.Name, &err)
	return fn(context.WithValue(fnCtx, heldKey{}, &held{name: opts.Name}))
}

// endLock releases the lock on a context that ignores the caller's
// cancellation. A failed or empty unlock closes the connection and, when fn
// returned nil, turns the result into ErrLockLapsed.
func endLock(ctx context.Context, conn *pgxpool.Conn, key int64, name string, err *error) {
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), UnlockTimeout)
	defer cancel()
	var unlocked bool
	uerr := conn.QueryRow(uctx, `SELECT pg_advisory_unlock($1)`, key).Scan(&unlocked)
	if uerr == nil && unlocked {
		return
	}
	_ = conn.Conn().Close(context.Background())
	if *err != nil {
		return
	}
	if uerr == nil {
		uerr = errors.New("the unlock found no lock held")
	}
	*err = fmt.Errorf("%w: %s: %v", ErrLockLapsed, name, uerr)
}
