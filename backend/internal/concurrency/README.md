# backend/internal/concurrency

Slot store for local stage concurrency groups (#3964 / ADR-087 #3968). The
host-dispatch spawn marker asks `Admit` for a slot before it moves a
host-dispatched stage to `dispatched`. Read surfaces call `StatusForStages`.
An operator moves one queued waiter to the front with `Promote` (#4191).
Table: `stage_concurrency_slots` (migration 0097; `order_key` added by 0102;
one row per (stage, group) since 0118, § "Migration 0118: deploy and rollback
order"). A stage is admitted into a SET of groups, all or nothing (ADR-094
D2/D3, #4240, § "Group sets and scoped keys").
It also READS `specialist_jobs` (0103) to count running specialist jobs as
holders (§ "Specialist-job holders (ADR-097 D6)").
The SQL is hand-written pgx, with no sqlc (same precedent as
`captain/store.go`).

## Contract

- `Admit(ctx, Request) (Admission, error)`. `Request{StageID, RunID, From,
  GroupKey, Limit, Groups, Host, AdmissionNonce}`. A request names its groups
  EITHER with the single-group shorthand `GroupKey`/`Limit` OR with
  `Groups []GroupRequest{Key, Limit}`, the stage's group set; the shorthand
  is a one-element set and behaves identically
  (`TestAdmitSet_ShorthandEqualsOneElementSet`). Refused with
  `ErrInvalidRequest` before any SQL runs: a nil stage or run id; `From` not
  `pending`/`awaiting_host_dispatch`; the shorthand and `Groups` both set; an
  empty set; more than `MaxGroups` groups; an empty key; a duplicate key; a
  limit outside `1..MaxLimit` (`TestAdmitSet_ValidateRefusesBeforeSQL`, one
  subtest per mode, reads back that no slot row exists). A `RunID` that is
  not the stage's own run returns `run.ErrNotFound`.
  - **Admitted**: the stage was CAS'd `From → dispatched`
    (`run.TransitionStageFromLiveRunTx`, which runs inside the admission
    transaction), and EVERY row of its set is `held`, each with the same
    `acquired_at`, `held_dispatched_at` = the `dispatched_at` the CAS stamped
    and `admission_nonce` = the request's `AdmissionNonce` (NULL when empty).
    `QueuedBefore` and `WaitedSeconds` are set when the stage waited during
    this episode.
  - **Queued**: the stage state is untouched and EVERY row of its set is
    `queued`; nothing is held (no partial hold). `Position` is 1-based.
    `Holders` lists the live holders and is never nil, so an empty list
    renders `[]`. `NewlyQueued` is true once per queue episode (when the set
    is inserted or restarted). `Contended` means a group lock was busy, so
    this call queued without deciding.
  - **Groups**: `Admission.Groups` lists every group of the set in canonical
    key order as `{Key, Scope, Limit, Position, Holders}`. The top-level
    `Position` and `Holders` describe the BINDING group (§ "Group sets and
    scoped keys"), so a single-group admission reads exactly as before.
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
- `StatusForStages(ctx, ids)` returns only ACTIVE stages. A stage's rows are
  read as one set, and the set is active only when EVERY row is:
  - `queued` rows whose stage is still `pending`/`awaiting_host_dispatch`,
    with positions, holders and `WaiterLive` (refreshed within `QueueTTL`);
  - `held` rows that currently count (same attempt and live).

  `Status.Groups` lists every group with its scope, limit, holders and (when
  queued) position; the top-level fields describe the binding group
  (`TestStatusForStages_ListsGroupsAndBindingTopLevel`).
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
2. Run `pg_try_advisory_xact_lock(lockKey(account, group))` for each group
   of the set in canonical (byte) key order, stopping at the first miss. It
   never waits. A miss on ANY group is a lock miss
   (`TestAdmitSet_ContendedOnAnyGroupLock`).
3. Lock the stage row (`FOR UPDATE`) and run the drift check. This step runs
   on BOTH the hit and the miss path. Every slot-row write for a stage happens
   under that stage's row lock, so the restart decision never races a
   concurrent admission of the same stage.
4. Upsert one queue row per group with the **episode-restart rule**. This
   rule is identical on both paths (approval condition 1). The stage was just
   verified host-dispatchable, so an existing `held` row belongs to a PREVIOUS
   episode. The WHOLE set restarts at the TAIL (`enqueued_at` = now) when the
   stage had no rows, any row was `held`, its row set differs from the
   requested set, or any row was queued but stale past `QueueTTL`. Otherwise
   every row keeps its `enqueued_at`. Rows outside the requested set are
   deleted, so a stage re-asking with a different set stops counting ahead
   in the groups it left (§ "Group sets and scoped keys"; the single-group
   case is the re-key of § "Verify group (ADR-092 D3)", approval condition 5
   of #4200). In every case each row gets `last_seen_at` = now, and
   `acquired_at`/`held_dispatched_at`/`admission_nonce` are cleared. Every
   row reads ONE timestamp (`WITH t AS MATERIALIZED (SELECT
   clock_timestamp() AS now)`). A restart also sets `order_key = NULL`: a
   promotion lasts for its queue episode only. The keep arm leaves
   `order_key` untouched, and the upsert returns the set's effective key,
   which step 5 counts ahead from.
5. Read each group's holders and queued-ahead counts (`aheadSQL`:
   `round_ahead`, `all_ahead`, and `multi_round_ahead`, the `round_ahead`
   rows whose stage has more than one slot row).
6. **Lock miss**: commit the queued rows and return `Contended=true`, with a
   position/holders snapshot.
7. **Lock hit**: decide fully before any write. Admit only when EVERY group
   G satisfies (a) `holders + job holders + round_ahead < Limit` AND (b)
   `multi_round_ahead == 0` (§ "Group sets and scoped keys"). For a
   single-group queue (b) is vacuous and (a) is ADR-087's rule. `round_ahead`
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
   trigger stamps `dispatched_at`), then ONE statement marks EVERY row of the
   stage `held`, all reading ONE `acquired_at` (the same MATERIALIZED CTE),
   recording the request's admission nonce and clearing `order_key`. The
   live-run CAS then reads the run's state in
   the same transaction (#4035). That read sits AFTER the holders read on
   purpose: B is admitted only once that read sees the previous holder
   settled, so under READ COMMITTED a cancel or failure that committed before
   the slot was seen free is visible to the later run read, and the admission
   is refused with `run.RunTerminalError`. A cancel committing after the run
   read is a cancel of an already-dispatched run (the existing cancel path).

Every time comparison uses the database clock. No Go `time.Now()` value is
passed into SQL.

## Group sets and scoped keys (ADR-094 D2/D3, Amendment A1)

A stage is admitted into a SET of groups (ADR-094 D2), each named by a
scoped identity `{scope, scope-id, name}` (D3). The store sees only key
strings. The server resolves a stage's set from its workflow-v2
`concurrency` block (`backend/internal/server/stage_concurrency.go`); the
store parses a key only to report its `Scope` on read surfaces (`""` when it
does not parse), and takes each group's limit as given. Who may set a limit
is the server's resolution: a host-scoped group is shared by every
repository on the host, so a repository spec never sets its limit (A1
decision 6).

**Canonical keys.** Amendment A1 keeps the legacy key strings canonical, so
no persisted row, `specialist_jobs.admission_group` value or advisory-lock
key changes. `ScopedGroupKey(scope, scope-id, name)` renders the key and
`ParseGroupKey` is its strict inverse; `ScopedGroupKey(ParseGroupKey(k)) ==
k` for every accepted key (`TestGroupKey_RoundTrip`).

| Identity (scope / scope-id / name) | Canonical key |
|---|---|
| host / `<host>` / `implement` | `local-implement:<host>` (`DefaultGroupKey`) |
| host / `<host>` / `verify` | `local-verify:<host>` (`VerifyGroupKey`) |
| repository / `<owner/repo>` / `<name>` | `spec:<owner/repo>:<name>` (`NamedGroupKey`) |
| host / `<host>` / any other name | `host:<host>:<name>` |
| account / `<id>` / `<name>` | `account:<id>:<name>` (reserved for review capacity, #4242; no spec declares it) |

An empty host scope-id renders as `UnknownHost`. `ParseGroupKey` REJECTS the
non-canonical aliases `repository:<repo>:<name>`, `host:<id>:implement` and
`host:<id>:verify`, a legacy host key whose host contains `:`, and any empty
part, so a key has exactly one spelling. `ScopedGroupKey` refuses a scope-id
containing `:` with `ErrInvalidGroupKey` (`TestParseGroupKey`,
`TestScopedGroupKey_Refusals`). A name may contain `:`, so D5's verify-only
identity is `spec:<repo>:verify:<base>`.

**All or nothing.** `Admit` admits a set only when every group admits it; a
miss on any group commits queued rows only, and nothing is held
(`TestAdmitSet_OneGroupFullHoldsNeither`, with the full group sorting first
and last). Release is derived from stage state as before, so one settle
frees every group of the set at once (`TestAdmitSet_ReleaseFreesEveryGroup`).

**The per-group rule.** The stage is admitted when, for EVERY group G of its
set:

- (a) `holders(G) + job holders(G) + round_ahead(G) < limit(G)`: ADR-087's
  reservation formula, which reserves one slot per earlier fresh waiter; and
- (b) `multi_round_ahead(G) == 0`: no earlier fresh waiter on G belongs to a
  set of more than one group (D2's strict per-group FIFO).

Read the other way: a later waiter on G is refused when (i) an earlier fresh
waiter on G has a multi-group set, or (ii) no slot of G remains after
reserving one per earlier fresh waiter. "Earlier" and "fresh" are the
`round_ahead` window of § "Admit, step by step" step 7, so (b) never counts a
same-round entrant that (a) ignores. A dead multi-group waiter stops
counting once it is stale past `QueueTTL`, so (b) cannot wedge a group.
The ordering `(effective key, stage_id)` is total, so the earliest waiter is
blocked only by holders, which release, and no wait cycle can form.

(b) is vacuous when every earlier waiter on G is single-group, so a queue of
single-group stages decides exactly as ADR-087 did, including "a later
waiter at position p <= limit - holders is admissible" at limit 2 or more
(`TestAdmitSet_SingleGroupQueuesUnchangedByMultiGroupRule`). An earlier
multi-group waiter is different: it can be held up on ANOTHER group for as
long as that group is busy, and (a) alone would let later waiters keep
taking G's unreserved slots past it. (b) keeps them behind it.

Example 1, an earlier multi-group waiter blocks at limit 2
(`TestAdmitSet_MultiGroupEarlierWaiterBlocksAtLimit2`). G1 has limit 2 and
no holders; G2 has limit 1 and H holds it.

1. W1 asks for {G1, G2}. G2 fails (a) (`1 + 0 + 0 < 1` is false), so W1 is
   queued in both groups and holds neither.
2. W2 asks for {G1}. (a) passes (`0 + 0 + 1 < 2`), but (b) finds W1, an
   earlier multi-group waiter on G1, so W2 is queued at position 2.
3. H settles. W1 is admitted into G1 and G2, and W2's next poll is admitted
   (G1 holds two of two).

Example 2, the mixed case: an earlier SINGLE-group waiter does not block a
later multi-group one (`TestAdmitSet_MultiGroupLaterWaiterBehindSingleGroupAdmitted`).
G1 has limit 2 and no holders; G3 has limit 1 and is free.

1. W1, a single-group G1 waiter, is queued and fresh (its poll was
   `Contended`).
2. W2 asks for {G1, G3}. On G1, (a) passes (`0 + 0 + 1 < 2`) and (b) finds
   no earlier multi-group waiter; G3 has no holders or waiters. W2 is
   admitted into both groups, with one `acquired_at`.
3. W1's next poll is admitted into G1, which then holds two of two. This is
   ADR-087's behaviour, unchanged, whether the later waiter's own set is
   single- or multi-group.

**Shared episode state.** A stage's rows are ONE admission episode: `state`,
`enqueued_at`, `last_seen_at`, `order_key`, `acquired_at`,
`held_dispatched_at` and `admission_nonce` are equal across them and always
written together. `clock_timestamp()` advances within a statement, so the
queue upsert and the held mark each read ONE value from `WITH t AS
MATERIALIZED (SELECT clock_timestamp() AS now)`
(`TestAdmitSet_RowsShareEpisodeState`, over a 16-group set: equal
`enqueued_at`, `last_seen_at` and `order_key` after the queue, keep and
restart steps, and one `acquired_at` and `held_dispatched_at` once admitted).

**A set change restarts at the tail.** Re-asking with a different set (a
group added, removed or replaced) deletes the rows outside the new set and
restarts the whole set at the tail with `order_key` cleared and
`NewlyQueued` set. The stage stops counting ahead in a group it left
(`TestAdmitSet_SetChangeRestartsWholeSetAtTail`,
`TestAdmitSet_SetGrowthRestartsAtTail`).

**The binding group.** `Admission`, `Status` and `Promotion` keep their
single-group top-level fields, and those fields describe the BINDING group:
while queued, the group with the highest position, ties broken in canonical
key order; while holding (or once admitted), the first group in canonical
key order. `Promotion` picks it by the old positions. A single-group stage's
binding group is its only group, so its read surfaces are unchanged. The
`Groups` list carries every group.

**`MaxGroups`.** The store accepts up to 16 groups per set, above the
workflow-v2 schema's 8 entries, so #4241 can add the host capacity group to
a full declared set (§ "Constants").

## Migration 0118: deploy and rollback order

0118 widens `stage_concurrency_slots`' primary key from `(stage_id)` to
`(stage_id, group_key)` in place. Every held and queued row survives with
identical columns, and no key string is rewritten. Each binary's
`ON CONFLICT` target needs its own primary key, so a binary running against
the other schema fails every grouped admission (500, fail-closed) until
both match. A rolling upgrade is therefore unsupported for this release.

**Forward:** stop EVERY old fishhawkd, including every API pod of a
multi-replica deployment; migrate up; start the new binary. The Helm chart
runs its migrate Job as a `pre-upgrade` or `post-upgrade` hook, which leaves
a window where the binary and the primary key disagree; scale the API to
zero, migrate, then scale up (`deploy/helm/fishhawk/README.md`).

**Rollback:** first confirm both of these, then stop the new binary (every
replica), migrate down through 0118, and start the reverted binary.

1. No stage holds a multi-group set. 0118.down is fail-closed: while any
   stage has more than one row and holds them, it RAISES an exception naming
   that stage and changes nothing, because collapsing the rows would drop
   real holds and the reverted binary would see those groups free. Drain
   the stage (let it settle), or confirm the query returns no rows:

   ```sql
   SELECT stage_id FROM stage_concurrency_slots
   GROUP BY stage_id HAVING count(*) > 1 AND bool_or(state = 'held');
   ```

   A QUEUED-only multi-row stage collapses to its canonical-first row
   (`COLLATE "C"`, the order the store sorts keys in) and restarts at the
   tail on its next poll with the single group the reverted binary asks for.
   `TestMigrate_StageConcurrencySlotsMultiGroup`
   (`backend/internal/postgres/postgres_test.go`) pins both arms.
2. No non-terminal run's captured workflow spec declares
   `concurrency.groups`, or those runs are cancelled. A reverted binary
   cannot parse such a spec and falls back to the default group. The match
   is deliberately broad (any `groups:` key), so inspect each hit:

   ```sql
   SELECT id FROM runs
   WHERE state NOT IN ('succeeded', 'failed', 'cancelled')
     AND workflow_spec IS NOT NULL
     AND convert_from(workflow_spec, 'UTF8') ~ '\mgroups\s*:';
   ```

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

`order_key` is cleared by the episode-restart rule (held -> queued, a change
of the group set, a stale restart at the tail) and by the held mark. A
promotion therefore means "move to the front for the next admission", and it
ends with the waiter's current episode
(`TestPromote_EpisodeRestartClearsPromotion`).

**Two promotions: the newest wins.** `Promote` sets `order_key` to the minimum
effective key over every OTHER fresh waiter minus 1 ms, so promoting C and then
B orders the queue B, C, A (`TestPromote_NewestPromotionWins`). `timestamptz`
has microsecond resolution, so the promoted key is strictly first.

**Group sets: one key per stage (#4191 option a).** A stage's rows share
ONE effective key. `Promote` writes the same `order_key` to every row of the
set, computed from the other fresh waiters of EVERY group in the set, so the
stage is first in each of them (`TestPromote_MultiGroupOneKeyAcrossSet`).
Admission stays all-or-nothing, so a promoted multi-group stage never holds
one group while it is queued in another, and a later waiter in the group it
could not yet take stays behind it (`TestPromote_MultiGroupNeverWedges`).
Refusing promotion of multi-group stages (option c) was rejected: ADR-094
slice 3 (#4241) puts every host-dispatched stage in at least two groups, which
would disable promotion and the expedited-hotfix auto-promote.

## Promote

`Promote(ctx, PromoteRequest{StageID, Record}) (Promotion, error)` moves one
queued waiter ahead of every other fresh waiter in EVERY group of its set, in
its account.
`Record` is REQUIRED: a nil `Record` or a nil `StageID` is refused with
`ErrInvalidRequest` before any SQL runs. `Record` runs inside the promotion
transaction after the order key is written and before COMMIT, and only when
the order changed. A non-nil error rolls the whole promotion back. The server
appends the `stage_concurrency_promoted` audit row through it, so a promotion
never commits unaudited (`TestPromote_RecordErrorRollsBack` reads the
committed `order_key` back as NULL).

`Promotion{StageID, RunID, GroupKey, Limit, Changed, OldPosition,
NewPosition, Holders, Groups}`. `Groups` lists every group of the set in
canonical key order as `{Key, Limit, OldPosition, NewPosition, Holders}`; the
top-level fields describe the binding group, chosen by the OLD positions (§
"Group sets and scoped keys"). `Holders` is never nil. A stage already first
in every group answers `Changed=false` with positions 1/1, writes nothing and
does not call `Record`; so does a stage whose last competitor ahead vanished
between the position read and the write (step 6).

Step by step (one transaction, one pooled connection):

1. Unlocked peek: the stage's run and `runs.account_id`, and the sorted
   `group_key` set of its slot rows. No stage row returns (wrapped)
   `run.ErrNotFound`. No slot row returns `ErrNotQueued`.
2. `pg_advisory_xact_lock(lockKey(account, group))` for EVERY group of the
   peeked set, BLOCKING, in canonical key order. They are the keys `Admit`
   try-locks, so a promote waits for an in-flight admission round in any
   group of the set to commit (`TestPromote_WaitsForGroupLock`,
   `TestPromote_RacingAdmitNeverDoubleAdmits`, and
   `TestPromote_MultiGroupWaitsForEveryGroupLock`, whose blocker holds only
   the group that sorts second), and an admitter arriving during the promote
   answers `Contended` (queued, undecided; the slot waiter retries). `Admit`
   never waits on these locks, so a blocking acquirer cannot deadlock with
   it.
3. Lock the stage row (`FOR UPDATE`): every slot-row write for a stage happens
   under it.
4. Re-read every slot row of the stage `FOR UPDATE` and classify:

   | Condition | Result |
   |---|---|
   | no row | `ErrNotQueued` |
   | the group set differs from the peek (a row added, removed or re-keyed) | `ErrSlotChanged` (`TestPromote_MultiGroupSetChanged`) |
   | any row `held` and the stage is `dispatched`/`running` | `ErrSlotHeld` (holders are never preempted) |
   | any row `held` otherwise (a previous episode's rows not yet re-polled) | `ErrNotQueued` |
   | `queued` and the stage is not `pending`/`awaiting_host_dispatch` | `ErrNotQueued` |
   | any row's `last_seen_at` past `QueueTTL` | `ErrWaiterNotLive` (`TestPromote_MultiGroupAnyStaleRowRefused`) |

   A stale waiter is refused rather than refreshed: refreshing would let a
   dead waiter block the group for a `QueueTTL`, and a stale row restarts at
   the tail on its next poll anyway, which would lose the promotion. The
   operator re-dispatches it (it re-queues at the tail with a live waiter) and
   then promotes it.
5. Holders, and the old position via `aheadSQL`, per group. With nothing
   ahead in any group, return the no-op.
6. `WITH others AS (SELECT min(effective key), bool_or(strictly ahead of the
   caller) ...) UPDATE ... SET order_key = others.k - 1 ms WHERE stage_id = $2
   AND state = 'queued' AND others.ahead` (`promoteKeySQL`). The "other fresh
   waiters" are the `aheadSQL` predicate over ANY group of the set
   (`group_key = ANY($1)`): same account, fresh, stage still
   host-dispatchable, not the caller's stage, scoped to the caller's account
   by the statement itself (`TestPromote_OrderKeyIgnoresOtherAccountsWaiters`
   pins the exact committed key against another account's earlier waiter
   under the same group key). The one statement writes the SAME key to every
   row of the stage. `enqueued_at`, `last_seen_at` and every other stage's
   rows, including every held row, are untouched
   (`TestPromote_HeldStageRefusedNeverPreempted` and
   `TestPromote_PromotedWaiterAdmittedNextThenFIFO` compare held rows byte for
   byte).

   The write is GUARDED on a fresh waiter still being strictly ahead of the
   caller's own effective key in some group of the set (#4312). Step 5's read
   is an earlier statement,
   and a competitor can go stale or be cancelled (its stage and run rows are
   not under the group lock) before step 6 writes. `min` and the ahead flag
   come from ONE aggregate over ONE scan of the predicate, so they agree on
   each row's `clock_timestamp()` freshness. When the guard fails the `UPDATE`
   matches no row (`pgx.ErrNoRows`) and the waiter gets the same answer as an
   already-first one: `Changed=false`, positions 1/1, nothing written, `Record`
   not called. Reading no row as that no-op is sound only because step 4
   holds the slot row `FOR UPDATE` and has classified it as queued.
   `TestPromote_CompetitorGoneBeforeWriteIsNoop` empties the ahead set at the
   `afterPromotePosition` seam (competitor cancelled, competitor gone stale,
   competitor ahead cancelled with one behind remaining) and reads the
   committed rows back.
7. The new position in each group via `aheadSQL` (it is 1).
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

**ADR-094 interaction (implemented, option a).** A stage in a set of groups
gets ONE order key across every row of its set (§ "Ordering and
`order_key`"), under every group lock (step 2). The set comparison in step 4
refuses a set that changed under the peek, so a promote never writes a key
the locks it took did not cover.

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
- an unreadable trigger (malformed or half-populated payload, or no audit
  repository) serves no pass, so the runner runs an ordinary implement and the
  stage is routed as one (WARN-logged). A trigger READ ERROR never reaches
  this routing: the marker's superseded-pass check (#4811) runs first and
  fails closed with `500 dependency_check_failed`, the stage untouched and no
  slot or queue row written. Only a read error that first appears between
  that check and the routing read takes the WARN-logged implement route.

The store itself is group-agnostic: holders and queue-ahead are keyed by
`group_key`, so the two groups never count each other
(`TestAdmit_VerifyGroupIndependentOfImplementGroup`). Rollout: a stage queued
in `local-implement:<host>` before its next marker POST resolves to
`local-verify:<host>` is re-keyed by step 4's restart rule: its set changed,
so the implement row is deleted and a verify row starts at the verify group's
tail (`TestAdmit_RekeysQueuedRowIntoNewGroup`). The actual verify run still
serialises against implement-stage verifies on the per-repository verify lock
(#3315); the group only stops a pass waiting for an agent slot.

## Specialist-job holders (ADR-097 D6)

A runner-executed specialist job (`backend/internal/specialistjob`, table
`specialist_jobs`, migration 0103, #4300) is admitted through
`local-verify:<host>`, never an implement slot. The coupling is store-only;
the `Store` interface is unchanged:

- `TryGroupLockTx(ctx, tx, acct, group)` try-locks the SAME
  `lockKey(account, group)` `Admit` takes, so a job admission and a stage
  admission of one group contend on one advisory lock. It never waits.
- `GroupOccupancyTx(ctx, tx, acct, group)` returns `Occupancy{StageHolders,
  FreshStageWaiters, JobHolders}`, read under that lock. `specialistjob.AdmitJob`
  admits a job only when `StageHolders + JobHolders < DefaultVerifyLimit`,
  `FreshStageWaiters == 0` (a job yields to merge-candidate passes) and no
  fresh queued job of the group and account is older.
- `admitTx` counts `jobHoldersSQL` in each group's condition (a):
  `len(holders) + jobHolders + roundAhead < Limit`. A job holder is a
  `specialist_jobs` row with `state = 'running'`, the group's
  `admission_group`, the stage's account (`IS NOT DISTINCT FROM`) and
  `last_seen_at > clock_timestamp() - JobHeartbeatTTL`. A merge-candidate
  verify pass therefore never runs beside a live job
  (`TestAdmit_VerifyGroupCountsRunningSpecialistJob`). A running job
  heartbeats by re-POSTing admission every 15s, so a crashed job stops
  counting within `JobHeartbeatTTL` (`TestAdmit_StaleSpecialistJobReleases`);
  a job never occupies `local-implement:<host>`
  (`TestAdmit_ImplementGroupIgnoresJobs`).
- `Admission.Holders` and `Status.Holders` stay STAGE-only. Job holders are
  not listed anywhere in this package's verdicts.

**Rollback order (approval condition C9).** Every `local-verify` admission
runs `jobHoldersSQL`, so the table must outlive the binary that reads it:
revert the fishhawkd binary FIRST, then migrate 0103 down. Migrating down
under a binary that carries this section makes every `local-verify`
admission fail (`relation "specialist_jobs" does not exist`) until the
binary is reverted. `local-implement` and spec groups also run the count
and fail the same way.

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
| `MaxGroups` | 16 | The largest group set one admission may name. Deliberately ABOVE the workflow-v2 schema's `concurrency.groups` `maxItems` of 8, so ADR-094 slice 3 (#4241) can add the host capacity group to a full declared set without a store change (`TestMaxGroupsAboveSchemaMaxItems`). |
| `VerifyGroupPrefix` / `DefaultVerifyLimit` | `local-verify:` / 1 | ADR-092 D3 (#4200): a merge-candidate verify-only pass is admitted through `local-verify:<host>`, separate from `local-implement:<host>`. One pass per host at a time, because each pass runs the full verify gate. |
| `JobHeartbeatTTL` | 2m | A running specialist job counts as a `local-verify` holder only while its runner re-POSTs admission (every 15s) within this window, so a crashed job blocks verify passes for at most about two minutes, not its whole runtime. |
| `QueueTTL` | 60s | 12× the waiter poll interval, so a live waiter never goes stale. A dead waiter stops blocking the queue within a minute. |
| `WaiterPollInterval` | 5s | The cadence of the MCP slot waiter. Also the marker's `Retry-After`. |
| `RunningStaleAfter` | 45m | Runner heartbeats arrive about every 15s, but only during agent invocations, not during the verify gate. 45 minutes outlasts a verify gate. |
| `DispatchedStaleAfter` | 15m | A spawned runner flips `dispatched → running` at its first prompt fetch. This bounds a stage that was admitted but never spawned (the MCP died between admission and spawn). |

## Atomicity and lock order

- **One connection per admission.** `Admit` holds exactly one pooled
  connection, for exactly one transaction, whatever the size of its group
  set. Every group lock is transaction-scoped: it is released at
  COMMIT/ROLLBACK, even on a cancelled context, so it cannot leak. A miss on
  any group returns instead of waiting. So no
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
- **Full lock order:** admission mutex (process-local) → group advisory
  locks, one per group of the set in canonical (byte) key order → stage row
  (`FOR UPDATE`) → slot rows → run row. `Admit` itself never locks a run row:
  it READS the run's state with a plain `SELECT` on the admit path (#4035),
  which adds no lock edge.
  The existing stage-then-run paths (`transitionStage`,
  `ResumeAwaitingInputAndAppend`) start below the group locks. The only other
  path that takes a group lock is `Promote`, which takes the same order:
  group locks (BLOCKING, the same canonical order) → stage row → slot rows →
  run row (its `Record` callback's audit append locks the run row). `Admit`
  only try-locks, so it never waits on a promote. Two promotes of
  overlapping sets both wait, but each acquires the shared groups in the
  same global order, so neither can hold a group the other already waits on
  while waiting on one the other holds. No cycle is introduced.
- **A promote holds one connection while it waits.** The wait is for
  in-flight admission transactions (milliseconds each, one group at a time)
  and is bounded by the request context.
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
- The per-stage concurrency block and `StatusForStages` do not list specialist
  job holders (ADR-094 D7, #4243): a verify pass queued behind a running job
  shows no holder.
- A job runner that stalls past `JobHeartbeatTTL` and then heartbeats again
  under its own nonce refreshes its row without re-deciding, so it can
  briefly run beside a verify pass admitted while it was stale.
- Stages already dispatched before deploy hold no row and are not counted.
- A bypass dispatch is not counted, by design. One example is the prompt-fetch
  liveness flip on a direct `bin/fishhawk-runner` run.
- The MCP may die between admission and spawn. The stage then sits
  `dispatched` until `DispatchedStaleAfter`, the dispatch watchdog, or an
  operator reap.
