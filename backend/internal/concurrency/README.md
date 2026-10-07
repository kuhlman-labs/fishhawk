# backend/internal/concurrency

Slot store for local stage concurrency groups (#3964 / ADR-087 #3968). The
host-dispatch spawn marker asks `Admit` for a slot before it moves a
host-dispatched stage to `dispatched`. Read surfaces call `StatusForStages`.
Table: `stage_concurrency_slots` (migration 0097). The SQL is hand-written
pgx, with no sqlc (same precedent as `captain/store.go`).

## Contract

- `Admit(ctx, Request) (Admission, error)`. `Request{StageID, RunID, From,
  GroupKey, Limit, Host, AdmissionNonce}`. `From` must be `pending` or
  `awaiting_host_dispatch`, `Limit` must be `1..MaxLimit`, and `RunID` must be
  the stage's own run. Each violation is refused before any SQL runs:
  `ErrInvalidRequest` for the first two, and `run.ErrNotFound` for a stage/run
  mismatch.
  - **Admitted**: the stage was CAS'd `From → dispatched`
    (`run.TransitionStageFromLiveRunTx`, which runs inside the admission
    transaction),
    and its row is `held` with `held_dispatched_at` = the `dispatched_at` the
    CAS stamped and `admission_nonce` = the request's `AdmissionNonce` (NULL
    when empty). `QueuedBefore` and `WaitedSeconds` are set when the stage
    waited during this episode.
  - **Queued**: the stage state is untouched and its row is `queued`.
    `Position` is 1-based. `Holders` lists the live holders and is never nil,
    so an empty list renders `[]`. `NewlyQueued` is true once per queue episode
    (when the row is inserted or restarted). `Contended` means the group lock
    was busy, so this call queued without deciding.
  - **Drift**: if the row-locked stage state is not `From`, the call returns
    `run.StageStateChangedError` and writes nothing.
  - **Terminal run** (#4035): on the admit path, if the stage's run is
    `succeeded`, `failed` or `cancelled` when read after the CAS, the call
    returns `run.RunTerminalError` and rolls back like any other error (stage
    untouched, no slot-row change). The host-dispatch marker maps it to the
    same 409 `dispatch_not_admissible` as its terminal-run pre-read. Pinned by
    `TestAdmit_RunTerminalAtCASNeverAdmits`.
  - **Failure contract**: ANY error rolls back the whole transaction. The
    stage keeps its state, and its row keeps exactly the queue standing it had
    before the call: no row if it never queued, and an unchanged
    `enqueued_at`/`last_seen_at`/`state` if it did. Pinned by
    `TestAdmit_FailedTransactionCommitsNothing`.
- `StatusForStages(ctx, ids)` returns only ACTIVE rows:
  - a `queued` row whose stage is still `pending`/`awaiting_host_dispatch`,
    with position, holders and `WaiterLive` (refreshed within `QueueTTL`);
  - a `held` row that currently counts (same attempt and live).

  Settled, parked, bypassed and previous-episode rows are omitted. A held
  status carries `Host` + `HeldDispatchedAt` (informational) and
  `AdmissionNonce`, which is what a client that lost an admission response
  uses to tell its own admission from another session's (§ "Admission
  nonce").

## Admit, step by step (one transaction, one pooled connection)

1. Read the stage's `runs.account_id` and `clock_timestamp()`, the **round
   start**, BEFORE taking the lock.
2. Run `pg_try_advisory_xact_lock(lockKey(account, group))`. It never waits.
3. Lock the stage row (`FOR UPDATE`) and run the drift check. This step runs
   on BOTH the hit and the miss path. Every slot-row write for a stage happens
   under that stage's row lock, so the restart decision never races a
   concurrent admission of the same stage.
4. Upsert the queue row with the **episode-restart rule**. This rule is
   identical on both paths (approval condition 1). The stage was just
   verified host-dispatchable, so an existing `held` row belongs to a PREVIOUS
   episode. The row restarts at the TAIL (`enqueued_at = clock_timestamp()`)
   when it was `held`, was queued in a different group, or was queued but
   stale past `QueueTTL`. Otherwise it keeps its `enqueued_at`. In every case
   it gets `last_seen_at = now`, and `acquired_at`/`held_dispatched_at`/
   `admission_nonce` are cleared.
5. Read the holders and the queued-ahead counts.
6. **Lock miss**: commit the queued row and return `Contended=true`, with a
   position/holders snapshot.
7. **Lock hit**: admit when `holders + round_ahead < Limit`. `round_ahead`
   counts only fresh queued rows enqueued at or before the round start
   (approval condition 2). A contended loser's row is inserted after it
   missed the lock, which is after this call's round start. So a row committed
   during the winner's own round can never block the winner. Pinned by
   `TestAdmit_SameRoundLoserDoesNotPreemptWinner` and
   `TestAdmit_PoolSmallerThanConcurrentAdmits` (exactly one admitted among
   stages that never queued before).
   What this does NOT guarantee is that a free slot admits someone in EVERY
   round. When an OLDER fresh queued row polls in the same instant a newer
   row wins the lock, the winner counts the older row ahead and stays
   queued, and the older row misses the lock and queues `Contended` without
   deciding: nobody is admitted that round. The store does not close this;
   the MCP slot waiter bounds it. After a contended answer it retries once at
   once, by which time the winner's transaction has normally committed, so
   the older row wins the lock and is admitted. Its poll interval is jittered
   ±20%, so two waiters' polls do not stay phase-aligned across rounds.
   `TestAdmit_ContendedHeadAdmittedOnRetry` pins the store half (winner
   parked on `afterLock` while the older row polls, then the older row's
   retry admits it); the waiter half is
   `backend/internal/mcpserver/README.md` § "Local concurrency slot waiter".
   Residual: the round start is read a few microseconds before the try-lock.
   A lock-HIT admitter that commits a queued row inside that gap is also
   excluded, so the winner may go ahead of it once. Someone is still admitted.
8. Admit: `run.TransitionStageFromLiveRunTx(... From → dispatched)` (the 0072
   trigger stamps `dispatched_at`), then mark the row `held`, recording the
   request's admission nonce. The live-run CAS then reads the run's state in
   the same transaction (#4035). That read sits AFTER the holders read on
   purpose: B is admitted only once that read sees the previous holder
   settled, so under READ COMMITTED a cancel or failure that committed before
   the slot was seen free is visible to the later run read, and the admission
   is refused with `run.RunTerminalError`. A cancel committing after the run
   read is a cancel of an already-dispatched run (the existing cancel path).

Every time comparison uses the database clock. No Go `time.Now()` value is
passed into SQL.

## Admission nonce

A slot waiter sends a random nonce (one per waiter) on every marker POST;
the marker passes it as `Request.AdmissionNonce`. Only an ADMISSION writes it
(the held mark); every queue upsert clears it, so a queued row never carries
one and a later admission overwrites any previous episode's. A waiter that saw
a transport error and then `200 transitioned:false` claims the admission only
when the held row's nonce equals its own. Another session on the same host, or
a manual `fishhawk_dispatch_stage` (which sends no nonce), records a different
or empty nonce, so the waiter exits without spawning. Two sessions on one host
share a host label and can both satisfy "same host, admitted this episode",
which is why the label is not used for ownership. The nonce is an ownership
marker, not a credential: it is visible on the stage read to any reader of the
run, and a caller able to replay it could already spawn a runner with
`write:runs`.

## Derived holders (the release rule)

A `held` row counts as a holder only while ALL of these hold:

- `stages.dispatched_at = held_dispatched_at`: it is the SAME attempt the row
  admitted. A re-open, or a bypass dispatch that never went through the
  marker, gets a new `dispatched_at` and does not count.
- The stage is `running` and `live_at > now - RunningStaleAfter`, or the stage
  is `dispatched` and `live_at > now - DispatchedStaleAfter`.

`live_at = GREATEST(dispatched_at, fishhawk_stage_heartbeat_at(progress))`.
The heartbeat is the DB-stamped `progress.reported_at` that
`RecordStageProgress` writes. The 0097 function `fishhawk_stage_heartbeat_at`
returns NULL for an absent, non-string, non-ISO-shaped or unparsable value,
including the special inputs `now`/`infinity`. GREATEST then falls back to
`dispatched_at`, so one tampered row cannot wedge or pin its group.
`updated_at` is deliberately not used: every write bumps it, and fixtures
cannot backdate it.

The release rule follows from this. A holder that settles or parks releases
at once, because it is no longer dispatched/running. A crashed holder is
released by the existing reap machinery (the stage leaves `dispatched`/
`running`) or, failing that, by the backstop.

A queued row counts as "ahead" only while all of these hold: it is fresh
(`last_seen_at > now - QueueTTL`), its stage is still `pending`/
`awaiting_host_dispatch`, and it is in the same group and account.

## Constants

| Constant | Value | Why |
|---|---|---|
| `DefaultGroupPrefix` | `local-implement:` | The default group is per host: `local-implement:<host>`. |
| `UnknownHost` | `unknown` | The host label used when the client sends none (a pre-change MCP). |
| `DefaultLimit` / `MaxLimit` | 1 / 64 | One local implement per host by default. 64 matches the 0097 CHECK and the spec bound. |
| `QueueTTL` | 60s | 12× the waiter poll interval, so a live waiter never goes stale. A dead waiter stops blocking the queue within a minute. |
| `WaiterPollInterval` | 5s | The cadence of the MCP slot waiter. Also the marker's `Retry-After`. |
| `RunningStaleAfter` | 45m | Runner heartbeats arrive about every 15s, but only during agent invocations, not during the verify gate. 45 minutes outlasts a verify gate. |
| `DispatchedStaleAfter` | 15m | A spawned runner flips `dispatched → running` at its first prompt fetch. This bounds a stage that was admitted but never spawned (the MCP died between admission and spawn). |

## Atomicity and lock order

- **One connection per admission.** `Admit` holds exactly one pooled
  connection, for exactly one transaction. The group lock is
  transaction-scoped: it is released at COMMIT/ROLLBACK, even on a cancelled
  context, so it cannot leak. A miss returns instead of waiting. So no
  admitter ever holds a connection while waiting for another admitter, and
  the lock winner never needs a second connection. Pinned by
  `TestAdmit_PoolSmallerThanConcurrentAdmits` (8 admitters on a 2-connection
  pool).
- **The server's admission lock holds no connection (approval condition 4).**
  `orchestrator.LockStageAdmission` is a process-local per-stage
  `sync.Mutex`. It holds NO pooled connection and NO transaction. A marker
  call holding it uses exactly one connection, inside `Admit`. So N
  concurrent marker calls need at most N connections only while each one is
  inside `Admit`, and none of them waits on a connection held by another.
- **Full lock order:** admission mutex (process-local) → group advisory lock →
  stage row (`FOR UPDATE`) → run row. `Admit` itself never locks a run row: it
  READS the run's state with a plain `SELECT` on the admit path (#4035), which
  adds no lock edge.
  The existing stage-then-run paths (`transitionStage`,
  `ResumeAwaitingInputAndAppend`) start below the group lock, and no other
  path takes the group lock, so no cycle is introduced.
- **The CAS must stay inside the admission transaction.** A CAS on another
  connection blocks on locks the admission transaction holds. The
  counterfactual that routes it through the pool hangs.

## Lock-key domain

`lockKey(account, group)` is the first 8 bytes of
`sha256("fishhawk:concurrency:" + account-or-"untenanted" + 0x00 + group)`,
read as an int64. It mirrors `captain.captainLockKey` but uses its own domain
string, so its key space is disjoint from every other advisory lock. A 64-bit
collision only produces a spurious contended/queued answer.

## Tenancy (approval condition 6)

`Admit` and `StatusForStages` run on the raw pool with **no tenant GUC**,
under the runtime role. Today that role is a superuser, which bypasses RLS
(see 0057's caveat). The table has no `account_id` and no RLS, like
`repo_acl_entries`. Account scoping is therefore EXPLICIT:

- every holder and queue read joins `runs.account_id` and matches the
  requesting stage's account (`IS NOT DISTINCT FROM`, so untenanted runs share
  one scope);
- the lock key is per account.

A holder in another account is neither counted nor disclosed.
`TestAdmit_OtherAccountHolderNeitherCountedNorDisclosed` pins this under the
pgtest superuser role, which bypasses RLS. That is the point: the test does
not depend on RLS. Residual: if the runtime moves to a non-superuser
NOBYPASSRLS role, these reads need the BYPASSRLS system context. Without it,
RLS would hide other runs' rows from an unset GUC, and the store would then
under-count holders.

## Residuals

- The host key is a client-supplied label, not a security boundary.
- Stages already dispatched before deploy hold no row and are not counted.
- A bypass dispatch is not counted, by design. One example is the prompt-fetch
  liveness flip on a direct `bin/fishhawk-runner` run.
- The MCP may die between admission and spawn. The stage then sits
  `dispatched` until `DispatchedStaleAfter`, the dispatch watchdog, or an
  operator reap.
