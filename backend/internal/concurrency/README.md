# backend/internal/concurrency

Slot store for local stage concurrency groups (#3964 / ADR-087 #3968). The
host-dispatch spawn marker asks `Admit` for a slot before it moves a
host-dispatched stage to `dispatched`. Read surfaces call `StatusForStages`.
An operator moves one queued waiter to the front with `Promote` (#4191).
Table: `stage_concurrency_slots` (migration 0097; `order_key` added by 0102).
The SQL is hand-written pgx, with no sqlc (same precedent as
`captain/store.go`).

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
  nonce"). A queued status carries `Promoted` (its `order_key` is set in this
  episode); a held status never does. Positions follow the effective key
  (§ "Ordering and `order_key`").
- `Promote(ctx, PromoteRequest) (Promotion, error)`: § "Promote".

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
   `admission_nonce` are cleared. A stage has ONE row (`stage_id` is the
   key), so a request naming a different group RE-KEYS that row: it moves
   into the new group at the new group's tail and stops counting ahead in the
   old one (§ "Verify group (ADR-092 D3)", approval condition 5 of #4200).
   A restart also sets `order_key = NULL`: a promotion lasts for its queue
   episode only. The keep arm leaves `order_key` untouched, and the upsert
   returns the row's effective key, which step 5 counts ahead from.
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
   request's admission nonce and clearing `order_key`. The live-run CAS then reads the run's state in
   the same transaction (#4035). That read sits AFTER the holders read on
   purpose: B is admitted only once that read sees the previous holder
   settled, so under READ COMMITTED a cancel or failure that committed before
   the slot was seen free is visible to the later run read, and the admission
   is refused with `run.RunTerminalError`. A cancel committing after the run
   read is a cancel of an already-dispatched run (the existing cancel path).

Every time comparison uses the database clock. No Go `time.Now()` value is
passed into SQL.

## Ordering and `order_key`

Admission is FIFO except for a promoted waiter. A queued row's effective key
is `COALESCE(order_key, enqueued_at)` and the admit order is
`(effective key, stage_id)`. `order_key` (0102) is NULL on every row
`Promote` did not touch, so an unpromoted queue is strict FIFO by
`enqueued_at` (every `TestAdmit_*` FIFO test is unchanged). `aheadSQL` counts
rows ahead of the caller's effective key. Its `round_ahead` filter stays on
`enqueued_at <= round start`, because that filter is about when a row was
COMMITTED (§ "Admit" step 7), and a promotion never changes that.
`enqueued_at` is never rewritten by a promotion, so `WaitedSeconds` and the
queued-for figures stay truthful.

`order_key` is cleared by the episode-restart rule (held -> queued, a re-key
into another group, a stale restart at the tail) and by the held mark. A
promotion therefore means "move to the front for the next admission", and it
ends with the waiter's current episode
(`TestPromote_EpisodeRestartClearsPromotion`).

**Two promotions: the newest wins.** `Promote` sets `order_key` to the minimum
effective key over every OTHER fresh waiter minus 1 ms, so promoting C and then
B orders the queue B, C, A (`TestPromote_NewestPromotionWins`). `timestamptz`
has microsecond resolution, so the promoted key is strictly first.

## Promote

`Promote(ctx, PromoteRequest{StageID, Record}) (Promotion, error)` moves one
queued waiter ahead of every other fresh waiter in its group and account.
`Record` is REQUIRED: a nil `Record` or a nil `StageID` is refused with
`ErrInvalidRequest` before any SQL runs. `Record` runs inside the promotion
transaction after the order key is written and before COMMIT, and only when
the order changed. A non-nil error rolls the whole promotion back. The server
appends the `stage_concurrency_promoted` audit row through it, so a promotion
never commits unaudited (`TestPromote_RecordErrorRollsBack` reads the
committed `order_key` back as NULL).

`Promotion{StageID, RunID, GroupKey, Limit, Changed, OldPosition,
NewPosition, Holders}`. `Holders` is never nil. A stage already first answers
`Changed=false` with positions 1/1, writes nothing and does not call `Record`.

Step by step (one transaction, one pooled connection):

1. Unlocked peek: the stage's run and `runs.account_id`, and its slot row's
   `group_key`. No stage row returns (wrapped) `run.ErrNotFound`. No slot row
   returns `ErrNotQueued`.
2. `pg_advisory_xact_lock(lockKey(account, group))`, BLOCKING. It is the same
   key `Admit` try-locks, so a promote waits for an in-flight admission round
   to commit (`TestPromote_WaitsForGroupLock`,
   `TestPromote_RacingAdmitNeverDoubleAdmits`), and an admitter arriving
   during the promote answers `Contended` (queued, undecided; the slot waiter
   retries). `Admit` never waits on this lock, so a blocking acquirer cannot
   deadlock with it.
3. Lock the stage row (`FOR UPDATE`): every slot-row write for a stage happens
   under it.
4. Re-read the slot row `FOR UPDATE` and classify:

   | Condition | Result |
   |---|---|
   | no row | `ErrNotQueued` |
   | `group_key` differs from the peek | `ErrSlotChanged` |
   | `held` and the stage is `dispatched`/`running` | `ErrSlotHeld` (holders are never preempted) |
   | `held` otherwise (a previous episode's row not yet re-polled) | `ErrNotQueued` |
   | `queued` and the stage is not `pending`/`awaiting_host_dispatch` | `ErrNotQueued` |
   | `queued` but `last_seen_at` past `QueueTTL` | `ErrWaiterNotLive` |

   A stale waiter is refused rather than refreshed: refreshing would let a
   dead waiter block the group for a `QueueTTL`, and a stale row restarts at
   the tail on its next poll anyway, which would lose the promotion. The
   operator re-dispatches it (it re-queues at the tail with a live waiter) and
   then promotes it.
5. Holders, and the old position via `aheadSQL`. With nothing ahead, return
   the no-op.
6. `UPDATE ... SET order_key = min(other fresh waiters' effective key) - 1 ms
   WHERE stage_id = $2 AND state = 'queued'`. The "other fresh waiters" are the
   `aheadSQL` predicate: same group and account, fresh, stage still
   host-dispatchable. `enqueued_at`, `last_seen_at` and every other row,
   including every held row, are untouched
   (`TestPromote_HeldStageRefusedNeverPreempted` and
   `TestPromote_PromotedWaiterAdmittedNextThenFIFO` compare held rows byte for
   byte).
7. The new position via `aheadSQL` (it is 1).
8. `Record(ctx, tx, promotion)`; an error rolls back.

Every time comparison uses `clock_timestamp()`.

"Never more than `Limit` holders" is enforced by `Admit`'s try-lock and the
derived holders, not by `Promote`, which never admits anything.
`TestPromote_StressNeverExceedsLimit` is a regression guard for it.

**Scope: local stage concurrency groups only.** This store never touches the
ADR-092/093 merge-candidate queue (`backend/internal/mergequeue`, its own
`fishhawk:mergequeue` lock domain, run-keyed entries). ADR-093 D7 reserves
`merge_queue_promoted` for that queue's promote, which #4297 ships as a
separate verb; ADR-092 Q4's "the promote verb (#4191) may move a held pass
ahead" is fulfilled by that verb, not this one. A verify-only pass already
re-opened and waiting for a `local-verify:<host>` slot is a stage waiter here
like any other; a pass still HELD in the merge-candidate queue is not.

**ADR-094 interaction (not implemented).** ADR-094 moves a stage to a set of
groups with strict per-group FIFO. A single per-row `order_key` would then
apply in every group of the stage's set; that is carried into the ADR-094
implementation issues.

## Verify group (ADR-092 D3)

A merge-candidate VERIFY-ONLY pass (ADR-090: an implement stage re-opened by a
LIVE `stage_merge_candidate_verify_triggered` row) runs no agent, so it does
not take a `local-implement` slot. The server
(`backend/internal/server/stage_concurrency.go`, `resolveStageConcurrency`)
admits it through `VerifyGroupKey(host)` = `local-verify:<host>` at
`DefaultVerifyLimit`, and decides this BEFORE the spec-declared lookup, so a
declared implement `group`/`limit` never captures a pass. The rule mirrors
what the prompt endpoint serves: a pass is served exactly when an implement
stage has a live (unconsumed) trigger. So:

- a fix-up, a conflict-resolution re-open, and a trigger already consumed by a
  later `merge_candidate_verified` row stay in `local-implement:<host>`;
- an unreadable trigger (read error, malformed or half-populated payload, or no
  audit repository) serves no pass, so the runner runs an ordinary implement
  and the stage is routed as one (WARN-logged).

The store itself is group-agnostic: holders and queue-ahead are keyed by
`group_key`, so the two groups never count each other
(`TestAdmit_VerifyGroupIndependentOfImplementGroup`). Rollout: a stage queued
in `local-implement:<host>` before its next marker POST resolves to
`local-verify:<host>` is re-keyed by step 4's restart rule: its single row
moves to the verify group's tail and leaves the implement queue
(`TestAdmit_RekeysQueuedRowIntoNewGroup`). The actual verify run still
serialises against implement-stage verifies on the per-repository verify lock
(#3315); the group only stops a pass waiting for an agent slot.

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
`awaiting_host_dispatch`, it is in the same group and account, and its
effective key (§ "Ordering and `order_key`") sorts before the caller's.

## Constants

| Constant | Value | Why |
|---|---|---|
| `DefaultGroupPrefix` | `local-implement:` | The default group is per host: `local-implement:<host>`. The MCP client derives `<host>` in `backend/internal/mcpserver/hostlabel.go` (`FISHHAWK_HOST_LABEL` > persisted host-id > hostname, #4212). |
| `UnknownHost` | `unknown` | The host label used when the client sends none (a pre-change MCP, or an MCP whose `hostlabel.go` ladder resolved nothing). |
| `DefaultLimit` / `MaxLimit` | 1 / 64 | One local implement per host by default. 64 matches the 0097 CHECK and the spec bound. |
| `VerifyGroupPrefix` / `DefaultVerifyLimit` | `local-verify:` / 1 | ADR-092 D3 (#4200): a merge-candidate verify-only pass is admitted through `local-verify:<host>`, separate from `local-implement:<host>`. One pass per host at a time, because each pass runs the full verify gate. |
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
  stage row (`FOR UPDATE`) → slot row → run row. `Admit` itself never locks a
  run row: it READS the run's state with a plain `SELECT` on the admit path
  (#4035), which adds no lock edge.
  The existing stage-then-run paths (`transitionStage`,
  `ResumeAwaitingInputAndAppend`) start below the group lock. The only other
  path that takes the group lock is `Promote`, which takes the same order:
  group lock (BLOCKING) → stage row → slot row → run row (its `Record`
  callback's audit append locks the run row). `Admit` only try-locks the
  group, so it never waits on a promote, and no cycle is introduced.
- **A promote holds one connection while it waits.** The wait is for an
  in-flight admission transaction (milliseconds) and is bounded by the
  request context.
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

`Admit`, `StatusForStages` and `Promote` run on the raw pool with **no tenant GUC**,
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
