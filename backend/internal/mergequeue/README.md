# backend/internal/mergequeue

Persisted held-pass queue for ADR-092 D1 (#4200, `docs/adr/092-serialize-merge-candidate-verify-passes.md`).
At most ONE merge-candidate verify pass is live per `(repository, base ref)`
inside one account. Every other eligible pass is held FIFO by eligibility time
and consumes no runner slot. Table: `merge_candidate_queue_entries`
(migration 0101; `merge_intent_at` added by 0107 for ADR-093 D2, #4295; `merge_intent_sequence`, `admission_epoch` and the `merge_dispatch_claims` ledger by 0108 for ADR-093 slice 2, #4296; the yield marker and the approval-wait anchor by 0109 for ADR-112 R3, #4632).
ADR-093's merge queue (slice 1) reuses these rows, the FIFO order, the
one-live-per-base rule and the lock order UNCHANGED; it adds only the
operator's recorded merge intent and the line-turn read below. The SQL is hand-written pgx, with no sqlc (same precedent as
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
| `merge_intent_at` | The operator's merge intent on this episode (0107, ADR-093 D2): NULL = none (every ADR-092 rebase/base-advance entry and every pre-0107 row). Stamped once from the database clock by `Enqueue{MergeIntent}` or `MarkMergeIntent`, never cleared; a terminal row keeps it as history, and a re-entry after a terminal row starts a FRESH row without it. |
| `merge_intent_sequence` | The audit sequence of the `merge_queue_entered` row that recorded the intent (0108, ADR-093 slice 2). Stamped in the SAME statement as `merge_intent_at` (`EnqueueRequest.IntentSequence`, `MergeIntentRequest.IntentSequence`), only on the NULL-to-set transition, never overwritten. NULL (read as `Entry.MergeIntentSequence` 0) = no intent, or an intent recorded before 0108; merge-on-pass then resolves the newest `merge_queue_entered` row naming the base. |
| `admission_epoch` | `BIGINT NOT NULL DEFAULT 0` (0108). Bumped by every admission: `AdmitNext`'s admit AND its stale re-claim. The admission-owned writes are compare-and-swaps on it (§ "Admission-epoch CAS"). |
| `yield_reason` / `yield_detail` / `yielded_at` | The YIELD MARKER (0109, ADR-112 R3; § "Yield"). NULL = not marked. `yield_reason` is the server's closed vocabulary (`backend/internal/server/merge_queue_yield.go`); `yield_detail` is untrusted forge-derived text every read surface sanitises (`Entry.YieldDetail`, `""` for NULL). DB CHECKs: `yielded_at` is set exactly when a non-empty `yield_reason` is, and a `live` row never carries a marker; a terminal row keeps it as history. `AdmitNext` skips a marked row. |
| `approval_wait_started_at` | The persisted head-of-line approval-wait anchor (0109): stamped ONCE per yield episode by `ApprovalWait` (COALESCE), never reset by `Unadmit`, a re-admission or a stale re-claim, kept by `Yield`, cleared only by `ClearYield`. |
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

- **`Enqueue(EnqueueRequest{RunID, BaseRef, HeadSHA, BaseSHA, MergeIntent, RecordIntent, Yield})`**. `RunID`,
  `BaseRef`, `HeadSHA` are required. Repository and account come from the run
  row, never the caller. Merge intent (`MergeIntent`): an inserted row
  (`EnqueueInserted`, `EnqueueBaseChanged`) is stamped at its enqueue
  instant; a same-base active entry is stamped only when its
  `merge_intent_at` is NULL. `IntentRecorded` reports THIS call moved it
  NULL → set (false on a repeat, and always false without `MergeIntent`;
  `MergeIntent=false` never clears an existing intent). A base change carries
  the intent onto the NEW row only.
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
  - Yield marker (`Yield *YieldMarker`, ADR-112 R3, the merge_run queue arm):
    an inserted row (`EnqueueInserted`, `EnqueueBaseChanged`) is inserted
    `held` WITH the marker, `yielded_at` = its enqueue instant; a same-base
    HELD entry is re-anchored and marked, taking the request's reason and
    detail and keeping an existing `yielded_at` (COALESCE: the same episode,
    while the head CAS still defeats a stale probe); a same-base LIVE entry is
    NEVER marked. `Marked` reports the entry carries the marker after the call
    (false for the live entry). An empty reason is `ErrInvalidRequest`. A nil
    `Yield` never clears or moves an existing marker: a marker-less re-anchor
    (the ADR-092 rebase-path re-enqueue) keeps the marker, `yielded_at` and
    the wait anchor (`TestEnqueue_MarkerlessReanchorPreservesMarker`,
    `TestEnqueue_YieldMarkerNeverOnLiveRow`, `TestEnqueue_YieldMarkerArms`).
- **`AdmitNext(base)`**.
  - Base has a live entry → no admission; `Entry` = that live entry.
  - …unless that live entry is in phase `admitted` with `admitted_at` older
    than `AdmissionStaleAfter`: `admitted_at` is re-stamped and the call
    returns `Admitted=true, Readmitted=true` for it, so exactly one pump
    re-runs a crashed admission. A live entry in any other phase is never
    re-claimed.
  - No live entry → the oldest UNMARKED `held` entry by `(enqueued_at, id)`
    becomes `live`, phase `admitted`, `admitted_at = now`; `Admitted=true`. A
    yielded (marked) entry is skipped entirely, however old
    (`TestAdmitNext_SkipsYieldedRows`).
  - Empty base, or a base holding only yielded entries → zero `Admission`
    (`Entry` nil).
- **`Unadmit(id, epoch)`**: `live` → `held` at its ORIGINAL place
  (`enqueued_at` kept), phase and `admitted_at` cleared. Callers: the pump's
  transient admission failure and merge-on-pass's readvance
  (`backend/internal/server/merge_candidate_queue.go`). Epoch CAS: not live,
  or live at another epoch → `ErrStateConflict`, nothing written.
- **`SetPhase(id, epoch, phase)`**: `live` at `epoch` only (epoch CAS;
  `ErrStateConflict` otherwise, nothing written); empty phase is
  `ErrInvalidRequest`; an unchanged phase writes nothing.
- **`ReanchorAt(id, epoch, head, base)`**: `Reanchor` under the epoch CAS, for
  the admission owner.
- **`SettleAt(id, epoch, state, reason)`**: `Settle` under the epoch CAS, for
  the admission owner and merge-on-pass decisions. An already-terminal row
  answers `Settled=false` like `Settle`; held, or live at another epoch →
  `ErrStateConflict`.
- **`Reanchor(id, head, base)`**: active entries only; same in-place semantics
  as the Enqueue re-anchor arm.
- **`Settle(id, state, reason)`**: `state` ∈ `merged|ejected|dropped`
  (`ErrInvalidRequest` otherwise); `reason` required for `ejected`/`dropped`.
  Idempotent: on an already-terminal row it writes nothing and returns
  `Settled=false` with the row's FIRST state and reason.
- **`MarkMergeIntent(id, req *MergeIntentRequest)`** (`req.IntentSequence`
  stamped with the intent, `req.Record` the callback; nil = neither) → `MergeIntentResult{Entry,
  IntentRecorded}`: stamps an ACTIVE entry's `merge_intent_at`
  (`ErrStateConflict` on terminal, nothing written). Idempotent: an
  already-set intent writes nothing (`updated_at` unchanged), calls no
  `record`, and answers `IntentRecorded=false`. For the merge_run arm on a run
  that already holds a held/live entry.
- **Record-before-commit (`RecordIntentFunc`)**: `EnqueueRequest.RecordIntent`
  and `MarkMergeIntent`'s `record` are called INSIDE the call's transaction
  only when it records intent (`IntentRecorded`), after every write and
  before commit, with the entry's `Status` (position, live run) read in that
  transaction. A non-nil error rolls the WHOLE call back — no inserted row,
  no re-anchor, no `base_changed` drop, no `merge_intent_at` — and the call
  returns `ErrIntentUnrecorded` wrapping it. Because nothing committed, a
  retry records again (`IntentRecorded=true`), so a failed audit append is
  never followed by an unrecorded intent. The callback runs under the call's
  advisory and row locks and must not call back into the Store
  (`TestRecordIntent_FailureRollsBackAndRetryRecords`). The server's
  `merge_queue_entered` row does NOT use this seam: a chained append locks
  the runs row `FOR UPDATE` and would wait on the insert's own `FOR KEY
  SHARE`. It appends BEFORE the write instead, under `WithMergeIntentLock`.
- **`WithMergeIntentLock(runID, fn)`**: runs `fn(ctx)` while holding the
  run's merge-intent lock (§ "Merge-intent lock"); `fn`'s context is bounded
  by `intentFnTimeout` and must not take another pglock lock. Returns `fn`'s
  error as is, the context's error (fn not run) when the wait ends first,
  `ErrIntentLockLapsed` (wrapping `pglock.ErrLockLapsed`) when `fn` returned
  nil but the lock could not be confirmed held through its release,
  `pglock.ErrNestedLock` when called from inside another pglock `fn`, and
  `ErrInvalidRequest` for a nil run id.
- **`ActiveForRun(runID)`** → `(Status, found, error)`: the run's active entry,
  its 1-based `Position` among the base's held entries in `ListForBase`'s
  order (unmarked before yielded; 0 when live), and
  `LiveRunID` (the base's live run; the entry's own run when it is live; nil
  when none). Position and live run count the run's OWN account only.
- **`ListForBase(base)`**: the base's active entries, live first, then
  unmarked held by `(enqueued_at, id)`, then yielded held by `(enqueued_at,
  id)` (`TestListForBaseAndPosition_YieldedOrderedLast`). Never nil. The seed
  for ADR-093's queue read surface.
- **`ActiveBases()`**: every base with a held or live entry, one per
  `(account, repo, base ref)`, ordered by those fields. Never nil. The
  periodic pump's sweep list; spans every account.
- **`RecentLineTurns(base, limit)`** → `[]time.Duration`: the line turn
  (`settled_at − admitted_at`, database clock) of the base's newest `limit`
  `merged` entries, newest settle first, account-scoped like `ListForBase`
  (`TestRecentLineTurns_MergedOnlyNewestFirstAccountScoped` seeds a newer
  merged row in a second account on the same base). Ejected/dropped rows and
  a merged row never admitted are skipped; `limit < 1` is
  `ErrInvalidRequest`; never nil. It is the ETA basis of ADR-093's per-base
  queue read: a line turn spans admission to merge, so until ADR-093 slice 2's
  merge-on-pass lands it INCLUDES the operator's merge step after a `passed`
  verdict (a slow merge lengthens every estimate behind it).

Merge intent is written only for a run the server judged queue-eligible
(`backend/internal/server/README.md`). Known deviation from ADR-093 D6 in
slice 1: that eligibility requires a LIVE forge approval and approval
transfer across a clean advance is deferred to slice 2, so in a
dismiss-stale repository a rebase answers `not_queued` until then. The
store itself never judges eligibility.

### Yield

ADR-112 R3: an entry whose forge approval does not qualify at the head of the
line waits (bounded) and then YIELDS: it steps aside, held and marked, so the
line continues. The store owns the persistence; the server decides when, and
which reason (`backend/internal/server/README.md`). Every time is the
database clock: `Entry.YieldAge` is `clock_timestamp() − yielded_at` computed
in the read (0 when unmarked), and the wait's expiry is judged in SQL, so no
Go `time.Now()` is compared against a stored instant.

- **`ApprovalWait(id, epoch, window)`** → `ApprovalWaitResult{Entry, Expired,
  Remaining}`: live at `epoch` only (epoch CAS; `ErrStateConflict`, nothing
  written). Stamps `approval_wait_started_at =
  COALESCE(approval_wait_started_at, clock_timestamp())` and judges `Expired`
  (`started + window <= now`) and `Remaining` (0 when expired) in the same
  transaction. `window <= 0` is `ErrInvalidRequest`. The anchor survives
  `Unadmit`, the re-admission and a stale re-claim
  (`TestApprovalWait_AnchorSurvivesUnadmitAndReadmission`). Call it OUTSIDE
  the merge-intent lock.
- **`Yield(id, epoch, YieldMarker{Reason, Detail})`**: live at `epoch` only
  (epoch CAS). ONE statement moves the row to `held` (phase and `admitted_at`
  cleared, `enqueued_at` kept) and stamps `yield_reason`, `yield_detail` and
  `yielded_at`; `approval_wait_started_at` is kept. Empty reason →
  `ErrInvalidRequest` (`TestYield_OneTransactionUnderEpochCAS`).
  `YieldMarker.Continue` (the entry's last cleared episode for that reason,
  supplied by the server) makes `yielded_at` that episode's instead of the
  database clock, ONLY when the row's anchor in the same statement equals the
  episode's head and base; `Enqueue` marking an unmarked held entry applies
  it after its re-anchor. An already-marked entry keeps its own `yielded_at`
  (`TestYield_ContinuesAClearedEpisodeOnlyOnTheSameAnchor`).
- **`ClearYield(id, head, yieldedAt)`**: the ONLY way back in. Under the base
  lock, a compare-and-swap on `state = 'held' AND anchored_head_sha = head AND
  yielded_at = yieldedAt`: clears the marker AND `approval_wait_started_at`
  (a later non-approval starts a fresh episode). The row rejoins FIFO at its
  original `enqueued_at`. A row whose head moved, whose episode changed
  (another `yielded_at`) or that is not held → `ErrStateConflict` and the
  marker stays (`TestClearYield_CompareAndSwap`, asserted on read-back).
- **`UpdateYield(id, head, yieldedAt, YieldMarker)`**: the same CAS; rewrites
  reason and detail only (a yield transition). `TestUpdateYield_RewritesReasonOnly`.
- **`ReanchorYielded(id, head, yieldedAt, newHead, newBase)`**: the same CAS on
  the OLD head; moves the anchor to the PR's current head and KEEPS the
  marker, `yielded_at` and `approval_wait_started_at` (the same episode). A
  head move never clears a marker, so a push loop cannot buy a fresh wait
  window per push; the new head is judged by the next probe and cleared at
  the NEW head (`TestReanchorYielded_KeepsMarkerAndEpisode`).

`head` and `yieldedAt` are required on the three marker CASes
(`ErrInvalidRequest`). `yieldedAt` is the value read back from the row
(`Entry.YieldedAt`), compared at the column's microsecond precision.

### Admission-epoch CAS

A pump's decision is made against the entry as it read it; another pump may
have Unadmitted and re-admitted it since. The epoch makes that stale decision
change nothing (ADR-093 slice 2, rejection item 1):

| Write | Guard | Who |
|---|---|---|
| `Unadmit` | live AND `admission_epoch = epoch` | admission transient failure, merge-on-pass readvance |
| `SetPhase` | live AND epoch | admission, verdict hook, transfer re-anchor |
| `ReanchorAt` | live AND epoch | admission re-anchor |
| `SettleAt` | live AND epoch (terminal: no-op) | admission settles, merge-on-pass settle outcomes |
| `ApprovalWait` / `Yield` | live AND epoch | merge-on-pass approval wait and yield, admission veto yield |
| `ClearYield` / `UpdateYield` / `ReanchorYielded` | held AND `anchored_head_sha` AND `yielded_at` | the yield re-probe |
| `Settle` / `Reanchor` | active (unconditional) | EXTERNAL facts only: PR resolved, cancel / re-open release, run terminal, the rebase transfer re-anchor, the merged settle after the forge accepted a queue merge |

### Merge-dispatch ledger

`merge_dispatch_claims` (0108, approval condition D1): one row per run,
`UNIQUE (run_id)` (`merge_dispatch_claims_run_id_key`), with `state`
(`dispatched` | `dispatch_failed`), `path` (`inline` | `delegated` | `queue`),
`head_sha`, `claimed_at`, and `failed_at` / `failure_cause` on a failure.

- **`ClaimMergeDispatch(runID, path, head)`**: a plain INSERT. The UNIQUE
  constraint, not any lock, admits exactly one claim per run: a duplicate is
  `ErrAlreadyDispatched`, returned with the existing claim. Every merge
  dispatch path (queue merge-on-pass, the inline `merge_run`, the delegated
  `may_merge` arm) calls it under `WithMergeIntentLock` and BEFORE the forge;
  `ErrAlreadyDispatched` means the forge is never called.
- **After a claim, the forge outcome is classified ONCE**
  (`server.classifyMergeDispatchOutcome`, first-officer ruling R1), the same
  on all three paths:
  - **Release** (the forge guarantees nothing was applied), on TYPED
    sentinels only: our own pre-call `checks_pending` refusal
    (`forge.ErrPullRequestUnstableStatus`) and the REST merge PUT's 405 (not
    mergeable, `forge.ErrPullRequestNotMergeable`), 409 (head-SHA mismatch,
    `forge.ErrMergeConflict`) or 422 (`forge.ErrMergeUnprocessable`, wrapped
    alongside `forge.ErrValidation`). A bare `forge.ErrValidation` is NOT a
    release: it also carries a generic HTTP-200 GraphQL error envelope,
    whose outcome is uncertain. **`ReleaseMergeDispatch(claim)`**
    DELETEs the claim as a compare-and-swap on the EXACT claim (`run_id`,
    `state = 'dispatched'`, `path`, `head_sha`, `claimed_at`); a claim that
    no longer matches is `ErrStateConflict` and is never deleted, so a
    release cannot remove another holder's claim. A later dispatch (the
    `merge_checks_pending` re-invoke, a re-advanced entry's next
    merge-on-pass) claims again.
  - **Uncertain** (everything else: 5xx, timeout, transport error, context
    cancel, 403 that may be a rate limit, 404, a generic GraphQL error):
    **`MarkMergeDispatchFailed(runID, cause)`** moves the claim to
    `dispatch_failed` (first cause and time kept). It is UPDATED, never
    deleted, so nothing re-dispatches on its own.
  - The ledger write runs on `context.WithoutCancel` with a bounded timeout,
    so a cancelled request cannot strand a `dispatched` claim; a release
    that does not land is recorded as `dispatch_failed` instead. Each
    outcome appends a `merge_dispatch_settled` audit row (`outcome`
    `released` | `dispatch_failed`, `path`, `head_sha`, `reason`); an
    uncertain one also logs at WARN.
- **Reconciliation before a re-arm.** Every path re-reads the PR before it
  touches the ledger (the inline and queue paths re-observe a landed merge
  BEFORE settling a claim after a failed dispatch, so a merge that landed
  leaves the claim `dispatched`). The next merge-on-pass or `merge_run`
  touching a run with a claim re-reads the PR: merged → merge-on-pass reads
  the claim and settles the entry merged (`pr_merged`) only when there is
  no claim or the claim names the merged head, else it holds
  `merged_claim_head_mismatch` (WARN); the inline answer is
  `already_merged`. On both paths a reconciled `dispatch_failed` claim
  appends `merge_dispatch_settled` `reconciled_merged`. Unmerged → the claim
  stands: merge-on-pass holds
  `merge_already_dispatched` (WARN), the inline answer is
  `409 merge_already_dispatched` naming the claim, and the merge-queue read
  surfaces it on the entry (`dispatch_claim`: state, path, head, age,
  reason).
- **Operator re-arm** (operator-only, and only for an UNCERTAIN outcome; no
  HTTP route, MCP tool or Fishhawk code path deletes a `dispatch_failed`
  claim): after confirming on the forge that the PR did not merge and
  resolving the cause, the operator removes the failed claim with
  `DELETE FROM merge_dispatch_claims WHERE run_id = '<run id>' AND state = 'dispatch_failed';`
  against fishhawkd's database, then re-invokes `fishhawk_merge_run`. A
  `dispatched` claim is never re-armed: its merge was handed to the forge.

## Reasons

The store writes exactly one reason itself: `base_changed` (`ReasonBaseChanged`).
Every other reason is the caller's string. ADR-092's set (owned by the server
serializer): dropped `run_terminal`, `run_cancelled`, `pr_merged_externally`,
`pr_closed`, `base_changed`; ejected `verify_failed`, `advance_conflict`,
`pass_not_startable`. Re-open releases (#4352, server-owned, dropped):
`reopened_for_fixup`, `reopened_for_conflict_resolution`, `head_reset`.

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
- By-entry mutations (`Unadmit`, `SetPhase`, `Reanchor`, `Settle`,
  `MarkMergeIntent`, `ApprovalWait`, `Yield`, `ClearYield`, `UpdateYield`,
  `ReanchorYielded`): read the entry's base → its base lock → the row
  `FOR UPDATE`. `repo`/`base_ref` never change on a row, so the derived key
  stays correct.
- `AdmitNext`: the base lock → the live check → the oldest unmarked held row.

No path takes a row lock before an advisory lock, and multi-key acquisition is
ascending, so the order is acyclic. The one-live-per-base guarantee is the
base lock: `TestAdmitNext_LockSerializesTheLiveCheck` (deterministic, parks an
admitter between its live check and its held select) and
`TestAdmitNext_ConcurrentAdmittersAdmitExactlyOne` (8 goroutines) both go RED
with the lock removed. Reads (`ActiveForRun`, `ListForBase`,
`RecentLineTurns`, `ActiveBases`) are single statements without the lock.

Every time comparison uses the database clock (`clock_timestamp()`); no Go
`time.Now()` value is passed into SQL (the marker CASes pass back a
`yielded_at` READ from the row, compared for equality, never against the
clock).

### Merge-intent lock

`WithMergeIntentLock` serializes the server's merge-intent decision for one
run across requests AND processes: read the entry, append
`merge_queue_entered`, then `Enqueue{MergeIntent}` / `MarkMergeIntent`. The
append runs on its own connection, so the decision cannot share one
transaction with the write.

- **Lock.** A SESSION-level `pg_try_advisory_lock` on
  `sha256("fishhawk:mergequeue-intent:" + run id)`, taken through
  `pglock.WithSessionLock` and held on one dedicated pooled connection that
  runs no transaction while `fn` runs on others (the shape every pglock
  helper shares; contract in the `backend/internal/pglock` package doc). The
  session is never idle in a transaction, so a server-side
  `idle_in_transaction_session_timeout` cannot end it mid-`fn`
  (`TestWithMergeIntentLock_SurvivesIdleInTransactionTimeout`). The domain
  is disjoint from the per-run Enqueue key, so `fn`'s own Enqueue never
  waits on its holder.
- **Lapse.** After `fn` the lock is released with `pg_advisory_unlock`.
  When that fails or finds no lock (the session died mid-`fn`, so another
  holder may have overlapped it), a nil from `fn` becomes
  `ErrIntentLockLapsed` wrapping `pglock.ErrLockLapsed`
  (`TestWithMergeIntentLock_LapsedSessionSurfaces`). A connection whose
  lock or unlock query failed is closed before release, so a lock-holding
  session never returns to the pool.
- **Connection budget.** A holder keeps one connection for its lock and its
  `fn` uses at most one more at a time. Holders draw on the pool-wide
  `pglock.BudgetFor(pool)` budget, shared with every pglock helper on the
  same pool (`run.WithConsolidatedPROpenLock`, `refinement.WithFilingLock`),
  not a per-store bound (`TestWithMergeIntentLock_SharesThePoolBudget`). It
  admits `pglock.HolderLimit` = max(1, (`MaxConns` − `pglock.ReservedConns`)
  / 2) concurrent holders, with `ReservedConns` = 2 (4 of fishhawkd's 10),
  so at least 2 connections stay free for non-lock work even when every
  slot is taken, and concurrent merges of DISTINCT runs make progress
  without any caller timeout
  (`TestWithMergeIntentLock_DistinctRunsSaturatingThePoolProgress`: eight
  runs on a 6-connection pool). A pool of fewer than `pglock.MinPoolConns`
  (4) connections refuses every holder with `pglock.ErrPoolTooSmall`.
  Detached work `fn` spawns (the server's detached queue pump) is ordinary
  non-lock load bounded by its own context, not `fn`'s extra connection.
- **fn deadline.** `fn` gets a `context.Context` bounded by
  `intentFnTimeout` (90 s since ADR-093 slice 2: the fn now also covers a
  forge merge call, each forge call with its own shorter timeout), applied even when the caller's context has no
  deadline, so every DB call in the decision is bounded
  (`TestWithMergeIntentLock_FnCarriesDeadline`). The bound is cooperative:
  it reaches `fn` only through that context. The context is marked as
  inside a pglock lock, so a nested pglock lock from `fn` is refused with
  `pglock.ErrNestedLock`; work `fn` spawns detached starts from
  `pglock.Detach`.
- **Panic safety.** The unlock is deferred: a panic in `fn` still unlocks
  and releases the connection and the budget slot, then propagates
  (`TestWithMergeIntentLock_PanicReleasesLock`).
- **Waiting.** A held lock releases both the budget slot and the connection
  and re-tries every 25 ms (`pglock.Poll`), so a waiter holds neither
  between tries. The wait is bounded only by the caller's context
  (`TestWithMergeIntentLock_SerializesOneRun`,
  `TestWithMergeIntentLock_ErrorsAndCancel`).

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
  until a caller settles it; the serializer drops a terminal-run live entry
  when it pumps, and fishhawkd pumps every `ActiveBases` base once at boot and
  then on a 1-minute ticker (`backend/internal/server/README.md`, "Periodic
  pump"), so for a run that goes terminal without a settle the bound is one
  pump tick. The run's cancel path releases at once.
- A `passed` live entry holds the line until its PR merges (ADR-092 D4); an
  operator who never merges stalls the base. Exception: a re-open of the run
  (fix-up, conflict resolution) or a head reset drops it (#4352), and the run
  re-enters at the tail. ADR-093 merge-on-pass removes the stall.
