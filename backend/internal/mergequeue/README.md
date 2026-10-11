# backend/internal/mergequeue

Persisted held-pass queue for ADR-092 D1 (#4200, `docs/adr/092-serialize-merge-candidate-verify-passes.md`).
At most ONE merge-candidate verify pass is live per `(repository, base ref)`
inside one account. Every other eligible pass is held FIFO by eligibility time
and consumes no runner slot. Table: `merge_candidate_queue_entries`
(migration 0101). The SQL is hand-written pgx, with no sqlc (same precedent as
`backend/internal/concurrency`).

This package is persistence only. It never reads stage, PR or forge state and
never writes audit rows. The server's serializer (pump, auto-advance, pass
start, verdict/merge/close hooks, audit categories) owns when each method is
called and what reason a settle carries.

## Rows

One row per queue EPISODE of a run.

| Column | Meaning |
|---|---|
| `state` | `held`, `live` (ACTIVE); `merged`, `ejected`, `dropped` (TERMINAL, never change again). CHECK-constrained. |
| `phase` | Free TEXT, no CHECK, NULL when unset. ADR-092 writes `admitted`, `verifying`, `passed`, `unverified`; ADR-093 adds its own without a migration. |
| `eject_reason` | Required (non-empty) for `ejected`/`dropped` (DB CHECK + `ErrInvalidRequest`). |
| `enqueued_at` | Eligibility time, the FIFO key (ties broken by `id`). Kept across re-anchor and `Unadmit`. |
| `anchored_head_sha` / `anchored_base_sha` / `anchored_at` | The head the pass is bound to, the base tip it was produced against (NULL when unknown), and when. |
| `admitted_at` | Set by `AdmitNext`; required while `live` (DB CHECK). |
| `settled_at` | Set exactly when the row is terminal (DB CHECK). |
| `updated_at` | `fishhawk_set_updated_at()` trigger; a call that "writes nothing" leaves it unchanged. |

Invariants: at most one ACTIVE row per run (partial `UNIQUE (run_id) WHERE
state IN ('held','live')`); at most one `live` row per base and account (the
advisory lock, below — not a DB constraint, because the table carries no
account column). `run_id` cascades on run delete.

## Contract

All methods return wrapped sentinels: `ErrInvalidRequest` (refused before any
SQL), `ErrNotFound` (unknown run or entry; wraps `run.ErrNotFound`), and
`ErrStateConflict` (entry not in a state the call accepts). `Base{AccountID,
Repo, BaseRef}` takes `AccountID` in `run.Run.AccountID`'s shape (`""` =
untenanted); a non-UUID account is `ErrInvalidRequest`.

- **`Enqueue(EnqueueRequest{RunID, BaseRef, HeadSHA, BaseSHA})`**. `RunID`,
  `BaseRef`, `HeadSHA` are required. Repository and account come from the run
  row, never the caller.
  - No active entry (none, or only terminal ones) → a new `held` row at the
    TAIL, `EnqueueInserted`. A run re-entering after a terminal row gets a
    FRESH row at the tail (ADR-093 Q2); the terminal row is untouched.
  - Active entry on the SAME base → re-anchored in place (`EnqueueReanchored`):
    head/base SHA and `anchored_at` move; `state`, `phase` and `enqueued_at`
    are kept. `AnchorChanged` reports whether either SHA differed; an
    unchanged anchor writes nothing. Phase is NOT reset: a caller that
    re-anchors a `passed` live entry sets the phase it wants.
  - Active entry on ANOTHER base → that entry is `dropped` with
    `ReasonBaseChanged` and a new `held` row is inserted at the new base's
    tail (`EnqueueBaseChanged`, `Dropped` = the old entry). When the dropped
    entry was live, its base has no live entry now: the caller pumps it.
- **`AdmitNext(base)`**.
  - Base has a live entry → no admission; `Entry` = that live entry.
  - …unless that live entry is in phase `admitted` with `admitted_at` older
    than `AdmissionStaleAfter`: `admitted_at` is re-stamped and the call
    returns `Admitted=true, Readmitted=true` for it, so exactly one pump
    re-runs a crashed admission. A live entry in any other phase is never
    re-claimed.
  - No live entry → the oldest `held` entry by `(enqueued_at, id)` becomes
    `live`, phase `admitted`, `admitted_at = now`; `Admitted=true`.
  - Empty base → zero `Admission` (`Entry` nil).
- **`Unadmit(id)`**: `live` → `held` at its ORIGINAL place (`enqueued_at`
  kept), phase and `admitted_at` cleared — for a transient admission failure.
  Non-live → `ErrStateConflict`, nothing written.
- **`SetPhase(id, phase)`**: active entries only (`ErrStateConflict` on
  terminal); empty phase is `ErrInvalidRequest`; an unchanged phase writes
  nothing.
- **`Reanchor(id, head, base)`**: active entries only; same in-place semantics
  as the Enqueue re-anchor arm.
- **`Settle(id, state, reason)`**: `state` ∈ `merged|ejected|dropped`
  (`ErrInvalidRequest` otherwise); `reason` required for `ejected`/`dropped`.
  Idempotent: on an already-terminal row it writes nothing and returns
  `Settled=false` with the row's FIRST state and reason.
- **`ActiveForRun(runID)`** → `(Status, found, error)`: the run's active entry,
  its 1-based `Position` among the base's held entries (0 when live), and
  `LiveRunID` (the base's live run; the entry's own run when it is live; nil
  when none). Position and live run count the run's OWN account only.
- **`ListForBase(base)`**: the base's active entries, live first, then held by
  `(enqueued_at, id)`. Never nil. The seed for ADR-093's queue read surface.

## Reasons

The store writes exactly one reason itself: `base_changed` (`ReasonBaseChanged`).
Every other reason is the caller's string. ADR-092's set (owned by the server
serializer): dropped `run_terminal`, `run_cancelled`, `pr_merged_externally`,
`pr_closed`, `base_changed`; ejected `verify_failed`, `advance_conflict`,
`pass_not_startable`.

## Atomicity and lock order

Every mutation is ONE transaction on ONE pooled connection under
`pg_advisory_xact_lock(lockKey(account, repo, base))` — a BLOCKING lock (a
mutation waits its turn; it never answers without deciding) whose key is the
first 8 bytes of `sha256("fishhawk:mergequeue:" + account-or-"untenanted" +
0x00 + repo + 0x00 + base)`, a domain disjoint from `concurrency.lockKey`.

- `Enqueue`: per-run lock (`"fishhawk:mergequeue-run:" + run id`) → peek the
  run's active entry → every base lock it needs (new base, and the active
  entry's base) in ascending key order → re-read the peeked entry `FOR UPDATE`.
  The run lock serializes every Enqueue of one run, so the peeked entry can
  only stay put or be settled by a by-entry mutation; a settled one is treated
  as absent (fresh row), never re-anchored
  (`TestEnqueue_EntrySettledBetweenPeekAndLockGetsFreshRow`).
- By-entry mutations (`Unadmit`, `SetPhase`, `Reanchor`, `Settle`): read the
  entry's base → its base lock → the row `FOR UPDATE`. `repo`/`base_ref` never
  change on a row, so the derived key stays correct.
- `AdmitNext`: the base lock → the live check → the oldest held row.

No path takes a row lock before an advisory lock, and multi-key acquisition is
ascending, so the order is acyclic. The one-live-per-base guarantee is the
base lock: `TestAdmitNext_LockSerializesTheLiveCheck` (deterministic, parks an
admitter between its live check and its held select) and
`TestAdmitNext_ConcurrentAdmittersAdmitExactlyOne` (8 goroutines) both go RED
with the lock removed. Reads (`ActiveForRun`, `ListForBase`) are single
statements without the lock.

Every time comparison uses the database clock (`clock_timestamp()`); no Go
`time.Now()` value is passed into SQL.

## Tenancy

No `account_id` column and no RLS, like `stage_concurrency_slots` (0097;
`TestMigrateDown_MergeCandidateQueueReversal` pins both absent). Every
per-base read joins `runs.account_id` and matches the base's account with
`IS NOT DISTINCT FROM` (untenanted runs share one scope), and the lock key is
per account, so another account's live entry neither blocks nor is disclosed
(`TestAdmitNext_OtherAccountsLiveEntryDoesNotBlock`). Residual, as in the
concurrency README: under a future NOBYPASSRLS runtime role these reads need
the BYPASSRLS system context.

## Constants

| Constant | Value | Why |
|---|---|---|
| `AdmissionStaleAfter` | 10 min | Admission work (auto-advance merge + bounded post-merge read + trigger append + re-open) takes seconds; 10 min is far past a slow forge, so a re-claim means a crash, not a slow pump. The re-run is idempotent (advance is a no-op when up to date; the pass start is per-head idempotent). |

## Residuals

- The store never observes run or PR state. A `live` entry holds its base
  until a caller settles it, so a run that goes terminal without a settle
  STALLS the base. The serializer's contract (ADR-092 D4, #4200 approval
  condition 4) is to release on the run-terminal transition and to drop a
  terminal-run live entry when it pumps; until that is wired, recovery is any
  call that settles the entry (the run's cancel path, or the next pump on that
  base).
- A `passed` live entry holds the line until its PR merges (ADR-092 D4); an
  operator who never merges stalls the base. ADR-093 merge-on-pass removes it.
