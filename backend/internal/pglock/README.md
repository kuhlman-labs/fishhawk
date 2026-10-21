# pglock

Postgres SESSION-level advisory locks that hold one pooled connection while
the caller's `fn` runs on others, with every such holder on ONE holder budget
per pool (#4591). Package doc (`pglock.go`) carries the same contract.

## Hazard

A holder keeps its lock connection for as long as `fn` runs, and `fn`
acquires further connections from the SAME pool. With enough concurrent
holders the lock connections alone exhaust the pool and every `fn` waits for
a connection only another waiting `fn` could release (hold-and-wait). A
per-helper bound does not close it: two helpers with independent bounds can
together fill the pool. The budget must be shared by every helper on the
pool.

## Budget

- `BudgetFor(pool)` returns the ONE `*Budget` every caller on that pool
  shares. Registry: a package-level `map[weak.Pointer[pgxpool.Pool]]*Budget`
  under a mutex; `weak.Make` on one pool yields equal keys. A miss builds the
  budget and registers `runtime.AddCleanup(pool, forget, key)` (the cleanup
  arg is the weak key, never the pool), so a collected pool's entry is
  dropped. `Budget` holds no strong pool reference. A nil pool gets a fresh,
  unregistered 1-slot budget.
- `HolderLimit(pool)` = max(1, (`MaxConns` − `ReservedConns`) / 2), with
  `ReservedConns` = **R = 2**. Holders together hold at most `HolderLimit`
  lock connections plus `HolderLimit` fn connections, so at least R
  connections stay free for non-lock work (health checks, the boot sweep,
  tickers, handlers) even when every slot is taken. fishhawkd's pool
  (`MaxConns` = 10, `backend/internal/postgres`) admits 4 holders.
- `MinPoolConns` = R + 2 = 4. A smaller pool cannot run a holder without
  eating into the reserve, so `WithSessionLock` refuses it with
  `ErrPoolTooSmall`, fn never run, instead of risking a stall.

## Invariants the sizing relies on

1. **At most ONE extra connection per fn.** A holder's `fn` holds at most one
   further pool connection at a time (sequential queries, or one transaction
   at a time). A `fn` that holds a transaction open while it acquires a
   second connection reopens hold-and-wait. Not enforced at runtime; pinned
   for the three production fns by the mixed-helper test.
2. **No nesting — ENFORCED.** `fn` runs on a context marked by this package;
   `WithSessionLock` on a marked context (or any context derived from it)
   fails closed with `ErrNestedLock` before it takes a slot or a connection.
   `InLock(ctx)` reports the marker.

**Detached work is not fn's extra connection.** Goroutines spawned from `fn`
and detached from it — e.g. the server's `pumpMergeCandidateQueueDetached`
from the merge-intent decision — are ordinary NON-lock load bounded by their
own contexts. They must start from `Detach(ctx)`: it drops fn's cancellation
and deadline (`context.WithoutCancel`, values kept) and CLEARS the nesting
marker, so a lock the detached work legitimately takes later is not refused
as nested. `context.WithoutCancel` alone keeps the marker.

## Acquisition

`WithSessionLock(ctx, pool, b, key, Options{Name, FnTimeout}, fn)`. Refused
before any slot or connection (fn never run): a marked ctx
(`ErrNestedLock`), a nil pool or `FnTimeout <= 0` (`ErrInvalidOptions`), a
nil budget or one built for another pool (`ErrBudgetMismatch`), a pool below
`MinPoolConns` (`ErrPoolTooSmall`). Otherwise each attempt takes a budget
slot, then a pooled connection, then runs `pg_try_advisory_lock`. A held lock
releases both and re-tries every `Poll` (25ms), so a waiter holds nothing
between tries; the wait is bounded only by the caller's ctx, and a canceled
ctx returns its error with fn never run. A try-lock query error closes the
connection before release (the lock may have been granted). Trade-off: no
FIFO fairness for same-key waiters (same-key contention is one run or one
draft).

## fn deadline, unlock, lapse

- `fn` runs on a context bounded by `FnTimeout` (required), applied even when
  the caller's ctx has no deadline. Cooperative: it reaches fn only through
  that context; `WithSessionLock` never abandons a running fn, because
  releasing the lock under it would break the exclusion.
- The unlock is DEFERRED, so a panic in fn still unlocks, releases the
  connection and the slot, then propagates. It runs on
  `context.WithoutCancel(ctx)` bounded by `UnlockTimeout` (5s).
- When the unlock errors or finds no lock held, the connection is CLOSED
  before release (a lock-holding session never returns to the pool) and, if
  fn returned nil, the call returns an error wrapping `ErrLockLapsed`: the
  session died mid-fn, so the exclusion may have lapsed. fn's own error
  always wins.

## Helpers on the budget

| Helper | Key domain | FnTimeout | Lapse surfaces as |
|---|---|---|---|
| `mergequeue.(*PostgresStore).WithMergeIntentLock` | per-run intent key | `intentFnTimeout` 30s | `ErrIntentLockLapsed` wrapping `ErrLockLapsed` |
| `run` postgres repo `WithConsolidatedPROpenLock` | sha256 `fishhawk:consolidated-pr-open:` + run | `consolidatedPROpenFnTimeout` 5m | `ErrLockLapsed` (was swallowed) |
| `refinement` postgres repo `WithFilingLock` | draft UUID first 8 bytes | `filingLockFnTimeout` 30m | `ErrLockLapsed` (was discarded) |

Each store takes `BudgetFor(pool)` in its constructor. A new helper on the
pool must go through `WithSessionLock` with the shared budget; a hand-rolled
session lock bypasses both the budget and the nesting check.

Separate from the ADR-094 admission groups: those are server-coordinated,
persisted slots bounding host and provider resources across stages and review
rounds; this is an in-memory bound on connection holders inside one process's
pool, and admits nothing.

## Pins

`pglock_test.go` (real Postgres): `TestHolderLimit`,
`TestBudgetFor_OneBudgetPerPool`, `TestBudgetFor_ForgetDropsEntry`,
`TestWithSessionLock_RefusesInvalidCalls` (one subtest per refusal),
`_SerializesOneKey`, `_DistinctKeysDoNotBlock`, `_WaiterHoldsNoSlot`,
`_CanceledWaitNeverRunsFn`, `_BoundsFnWithDeadline`,
`_PanicInFnReleasesLock` (separate non-pooled probe session: advisory locks
are re-entrant per session), `_LapsedSessionSurfaces`, `_ReservesHeadroom`,
`_RefusesNestedLock`, `TestDetach_ChildCanAcquire`.

`mixed_test.go` (the three production helpers over ONE 6-connection pool,
`HolderLimit` 2):

- `TestSharedBudget_MixedHelpersUnderLoadProgress` — #4591's done-means: 12
  distinct-key holders (4 per helper) under 3 non-lock load loops, each fn
  needing two further connections, all return within `timescale.D(20s)` with
  no caller cancellation, max concurrent fns ≤ `HolderLimit`. Each fn waits
  (bounded) for 6 holders inside before it acquires, so a per-store budget
  (3 × 2 = 6 lock sessions on 6 connections) deadlocks — observed RED.
  6 is the smallest pool where that happens under R = 2.
- `TestSharedBudget_MixedHelpersReserveHeadroom` — every slot taken, every fn
  holding its extra connection, under load: a plain `SELECT 1` completes
  within `timescale.D(1s)`. R = 0 → observed RED.
- `TestSharedBudget_CrossHelperNestingRefused` — each helper inside another's
  fn gets `ErrNestedLock`; a `Detach`ed child acquires.

Per-helper pins: `mergequeue/store_test.go` `TestWithMergeIntentLock_*`,
`run/consolidated_pr_lock_test.go`, `refinement/postgres_test.go`
`TestPostgres_WithFilingLock_*` (each: `_FnCarriesDeadline`,
`_PanicReleasesLock`, `_LapsedSessionSurfaces`).
