# backend/internal/mergequeue

Persisted held-pass queue for ADR-092 D1 (#4200, `docs/adr/092-serialize-merge-candidate-verify-passes.md`).
At most ONE merge-candidate verify pass is live per `(repository, base ref)`
inside one account. Every other eligible pass is held FIFO by eligibility time
and consumes no runner slot. Table: `merge_candidate_queue_entries`
(migration 0101; `merge_intent_at` added by 0107 for ADR-093 D2, #4295; `merge_intent_sequence`, `admission_epoch` and the `merge_dispatch_claims` ledger by 0108 for ADR-093 slice 2, #4296; the yield marker and the approval-wait anchor by 0109 for ADR-112 R3, #4632; the promote `order_key` by 0111 for ADR-093 slice 3, #4297; ADR-093 D8's resume phases and `Resumer` with no migration, #4298).
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
| `phase` | Free TEXT, no CHECK, NULL when unset. ADR-092 writes `admitted`, `verifying`, `passed`, `unverified`; ADR-093 D8 (#4298) adds `advancing` (stamped before the behind-probe), `merging` (stamped in-lock after the verdict row, BEFORE the merge-dispatch claim) and `settling` (after the forge accepted the merge), without a migration. It records the LAST RECORDED step of the head-of-line cycle, NOT a liveness signal: a process that died mid-step leaves the phase it last wrote, and the server's restart resume continues from it (`backend/internal/server/README.md` § "Merge-queue durability (ADR-093 D8)"). |
| `eject_reason` | Required (non-empty) for `ejected`/`dropped` (DB CHECK + `ErrInvalidRequest`). |
| `enqueued_at` | Eligibility time, the FIFO key (ties broken by `id`). Kept across re-anchor and `Unadmit`, and never rewritten by a promotion. |
| `order_key` | The operator promote's ordering key (0111, ADR-093 slice 3; § "Promote"). NULL = ordered by `enqueued_at`. The EFFECTIVE queue key everywhere order matters (`AdmitNext`, `ListForBase`, the held `Position`) is `(COALESCE(order_key, enqueued_at), id)`, so every unpromoted row is strict FIFO. Set only by `Promote`; kept by `Unadmit`, `Yield`, `ClearYield` and the re-anchors; a FRESH row (a re-entry after a terminal row, a base change) carries none and joins at the tail (`Entry.OrderKey`, `Entry.Promoted()`). |
| `anchored_head_sha` / `anchored_base_sha` / `anchored_at` | The head the pass is bound to, the base tip it was produced against (NULL when unknown), and when. |
| `admitted_at` | Set by `AdmitNext`; required while `live` (DB CHECK). |
| `settled_at` | Set exactly when the row is terminal (DB CHECK). |
| `merge_intent_at` | The operator's merge intent on this episode (0107, ADR-093 D2): NULL = none (every ADR-092 rebase/base-advance entry and every pre-0107 row). Stamped once from the database clock by `Enqueue{MergeIntent}` or `MarkMergeIntent`, never cleared; a terminal row keeps it as history, and a re-entry after a terminal row starts a FRESH row without it. |
| `merge_intent_sequence` | The audit sequence of the `merge_queue_entered` row that recorded the intent (0108, ADR-093 slice 2). Stamped in the SAME statement as `merge_intent_at` (`EnqueueRequest.IntentSequence`, `MergeIntentRequest.IntentSequence`), only on the NULL-to-set transition, never overwritten. NULL (read as `Entry.MergeIntentSequence` 0) = no intent, or an intent recorded before 0108; merge-on-pass then resolves the newest `merge_queue_entered` row naming the base. |
| `admission_epoch` | `BIGINT NOT NULL DEFAULT 0` (0108). Bumped by every admission: `AdmitNext`'s admit AND its stale re-claim, and `AdmitNextReclaiming`'s boot re-claim. The admission-owned writes are compare-and-swaps on it (§ "Admission-epoch CAS"). |
| `yield_reason` / `yield_detail` / `yielded_at` | The YIELD MARKER (0109, ADR-112 R3; § "Yield"). NULL = not marked. `yield_reason` is the server's closed vocabulary (`backend/internal/server/merge_queue_yield.go`); `yield_detail` is untrusted forge-derived text every read surface sanitises (`Entry.YieldDetail`, `""` for NULL). DB CHECKs: `yielded_at` is set exactly when a non-empty `yield_reason` is, and a `live` row never carries a marker; a terminal row keeps it as history. `AdmitNext` skips a marked row. |
| `approval_wait_started_at` | The persisted head-of-line approval-wait anchor (0109): stamped ONCE per yield episode by `ApprovalWait` (COALESCE), never reset by `Unadmit`, a re-admission, a stale re-claim, the boot re-claim or (docs/adr/112 A3.7, 0115) `ClearYield`: RETAINED for the whole episode, so a later approval-fold block in the same episode judges expired at once. |
| `approval_wait_open` | `BOOLEAN NOT NULL DEFAULT false` (0115, A3.7): an approval wait is open. Set by `ApprovalWait`, cleared by `ClearYield`; 0115 backfills it `true` on every row whose anchor is set (pre-0115 `ClearYield` nulled the anchor, so every set anchor was an open wait). The server's #4748 readvance deadline fires only while it is set (`Entry.ApprovalWaitOpen`). |
| `yield_revision` | `BIGINT NOT NULL DEFAULT 0` (0115, A3.6): bumped by EVERY marker write (`Yield`, `Enqueue`'s mark, a marked insert = 1, `UpdateYield`, `ClearYield`, `ReanchorYielded`); a marker-less re-anchor does not. The yielded-row compare-and-swap key with `anchored_head_sha` (`Entry.YieldRevision`). |
| `row4_flag` | `BOOLEAN NOT NULL DEFAULT false` (0115, A3.6): the row-4 obligation. ORed in by a marker write whose `YieldMarker.Row4` is true (no reason transition clears it); cleared only by `ClearYield(clearRow4=true)` (`Entry.Row4Flag`). 0115 backfills it `true` on every row yielded `forge_review_required` (pre-0115 the obligation was that stored reason). |
| `gate_wait_started_at` / `gate_wait_epoch` | The per-admission-epoch merge-gate window (0115, A3.2): its start and the admission epoch that opened it; both NULL or both set (DB CHECK). Written only by `GateWait`. |
| `gate_hold_tokens` / `gate_tokens_refilled_at` | The merge-gate hold budget (0115, A3.7): `SMALLINT NOT NULL DEFAULT 3`, CHECK `BETWEEN 0 AND 3` (`GateHoldBudget`), and its refill clock (NULL = never spent). Written only by `GateWait`; kept by every other write, a clear and a head move included. |
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
    entry was live, its base has no live entry now: the caller pumps it. The
    new row is a NEW approval-wait episode (ADR-112 R3's no-reset rule applies
    within one base): it carries no yield marker (unless this call's `Yield`
    marks it) and no `approval_wait_started_at`; the dropped row keeps its
    marker as history (`TestEnqueue_BaseChangeStartsNewWaitEpisode`).
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
  - …unless that live entry is in phase `admitted` or `advancing` (the
    admission's own steps) with `admitted_at` older than
    `AdmissionStaleAfter`: `admitted_at` is re-stamped, the epoch bumped, and
    the call returns `Admitted=true, Readmitted=true` for it, so exactly one
    pump re-runs a crashed admission. The re-claim writes ONLY `admitted_at`
    and `admission_epoch`; the phase, anchor, `order_key`, `enqueued_at`, the
    yield columns and `approval_wait_started_at` are kept. A live entry in
    any other phase (`verifying`, `unverified`, `passed`, `merging`,
    `settling`) is never re-claimed
    (`TestAdmitNext_StaleReclaimCoversAdvancing`).
  - No live entry → the first UNMARKED `held` entry by
    `(COALESCE(order_key, enqueued_at), id)` becomes `live`, phase `admitted`, `admitted_at = now`; `Admitted=true`. A
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
  order (unmarked before yielded, then the effective key; 0 when live), and
  `LiveRunID` (the base's live run; the entry's own run when it is live; nil
  when none). Position and live run count the run's OWN account only.
- **`ListForBase(base)`**: the base's active entries, live first, then
  unmarked held by `(COALESCE(order_key, enqueued_at), id)`, then yielded
  held by the same key (`TestListForBaseAndPosition_YieldedOrderedLast`,
  `TestPromote_MovesHeldEntryToHead`). Never nil. The seed for ADR-093's
  queue read surface.
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

- **`LatestForRun(runID)`** → `(Entry, found, error)` (`EjectionReader`): the
  run's NEWEST row in any state, by `(enqueued_at, id)` descending; a run's
  rows are inserted one episode at a time on the database clock, so that is
  its latest episode. `found=false` for a run that never queued; nil run id
  is `ErrInvalidRequest` (`TestLatestForRun`).
- **`UnresolvedEjections(base, limit)`** (`EjectionReader`): the base's
  `ejected` rows that are STILL their run's newest row (no re-entry, no later
  episode), newest `settled_at` first, account-scoped like `ListForBase`, at
  most `limit`. A re-entered run, a `dropped` row and another account's
  ejection are excluded; `limit < 1` is `ErrInvalidRequest`; never nil
  (`TestUnresolvedEjections`). The queue read's `ejections[]`.
- **`ReentryCandidates(after, limit)`** (`ReentryReader`): `ejected` rows
  with `ReentryReason` (`verify_failed`) that carry merge intent, are their
  run's newest row, and belong to a run that is not terminal (`succeeded`,
  `failed`, `cancelled`), oldest settle first by `(settled_at, id)`, strictly
  after the exclusive keyset cursor `after` (`ReentryCursor`, from
  `ReentryCursorAt`; nil starts at the oldest), at most `limit`, ACROSS every
  account (the server's re-entry sweep, like `ActiveBases`, which pages with
  it so never-passing candidates cannot crowd newer ones out, #4297).
  `limit < 1` is `ErrInvalidRequest`; never nil (`TestReentryCandidates`,
  `TestReentryCandidates_KeysetCursorPages`). Eligibility, the
  fix-up evidence and the forge are the caller's; the store only narrows.
  `Entry.SettledAge` (`clock_timestamp() − settled_at`, computed in the read;
  0 on an active row) is the database-clock age the sweep's backoff keys on,
  so no Go `time.Now()` is compared against `settled_at`.

- **`WithdrawIntent(WithdrawIntentRequest{RunID, Record})`** →
  `Withdrawal{Entry, LiveRunID}` (`IntentWithdrawer`): settles the run's
  YIELDED held entry `dropped` with `ReasonIntentWithdrawn` in one
  transaction, `Record` before COMMIT (§ "Withdraw intent").

**Capabilities.** `Promoter`, `IntentWithdrawer` (§ "Withdraw intent"),
`EjectionReader`, `ReentryReader` and `Resumer` (§ "AdmitNextReclaiming")
are SEPARATE interfaces, not part of `Store`; `PostgresStore` implements all
five. A caller type-asserts the capability on its `Store`, so a fake that
embeds a (possibly nil) `Store` and does not implement one degrades instead of
panicking on a nil embedded call (approval condition M1 of #4297).

Merge intent is written only for a run the server judged queue-eligible
(`backend/internal/server/README.md`). Known deviation from ADR-093 D6 in
slice 1: that eligibility requires a LIVE forge approval and approval
transfer across a clean advance is deferred to slice 2, so in a
dismiss-stale repository a rebase answers `not_queued` until then. The
store itself never judges eligibility.

### AdmitNextReclaiming

ADR-093 D8's restart resume (#4298) through the `Resumer` capability. The
server's boot pump runs it as each base's FIRST admission, so an admission a
dead process left is re-run at once instead of after `AdmissionStaleAfter`.

- **`DatabaseNow()`** → `time.Time`: `SELECT clock_timestamp()`. fishhawkd
  reads it BEFORE it can admit anything (the boot instant) and passes it back
  as `AdmittedBefore`, so the comparison stays on the database clock and an
  admission THIS process makes after boot is never re-claimed (`TestDatabaseNow`).
- **`AdmitNextReclaiming(ReclaimRequest{Base, AdmittedBefore, Record})`** →
  `Admission`: `AdmitNext` in the SAME admit transaction (one internal path:
  the same base lock, live check, held select and re-claim UPDATE, so the
  yielded-row skip and the re-claim's column preservation cannot drift apart),
  whose live check ALSO re-claims a live `admitted`/`advancing` entry with
  `admitted_at < AdmittedBefore`, stale or not. No other phase is re-claimed
  before the instant (`TestAdmitNextReclaiming_BootInstant`). An empty live
  slot admits the first unmarked held row exactly as `AdmitNext`, and a
  yielded row is skipped and keeps its marker, `yielded_at` and wait anchor;
  a re-claimed row keeps its wait anchor, anchor, `order_key`, `enqueued_at`
  and NULL yield columns (`TestAdmitNextReclaiming_SkipsYieldedAndPreservesWaitState`,
  ruling R6). A zero `AdmittedBefore`, a nil `Record` or an invalid base is
  `ErrInvalidRequest` before any SQL.
- **`Record(ctx, tx, reclaimed)`** (`ReclaimRecordFunc`) is REQUIRED and runs
  on EVERY re-claim the call makes (the boot instant or the stale bound),
  inside the transaction after the re-claim write and before COMMIT, with the
  re-claimed entry (new epoch and `admitted_at`, phase unchanged). It is never
  called for an admission of a held row or when nothing is re-claimed. A
  non-nil error rolls the WHOLE call back (no epoch bump, no `admitted_at`
  change) and returns `ErrReclaimUnrecorded` wrapping it
  (`TestAdmitNextReclaiming_RecordFailureRollsBack`, read as committed
  state). The server appends `merge_queue_resumed` (action `reclaim`) with
  `audit.AppendChainedTx` on that `tx`: the re-claim UPDATE touches no key
  column, so, as with `PromoteRequest.Record`, no foreign-key lock on the
  runs row is taken. Plain `AdmitNext`'s stale re-claim (the periodic ticks)
  records nothing.

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
  (`TestApprovalWait_AnchorSurvivesUnadmitAndReadmission`) and, docs/adr/112
  A3.7, a `ClearYield`: a later epoch's call on a retained anchor past the
  window reports `Expired` at once (`TestApprovalWait_RetainedAnchorAfterClear`).
  It sets `approval_wait_open`. Call it OUTSIDE the merge-intent lock.
- **`GateWait(id, epoch, window)`** → `GateWaitResult{Entry, Opened, Expired,
  NoToken, Remaining}` (docs/adr/112 A3.2/A3.7): live at `epoch` only (epoch
  CAS; `ErrStateConflict`, nothing written); `window <= 0` is
  `ErrInvalidRequest`. ONE window per admission epoch: when `gate_wait_epoch
  = epoch` it CONTINUES (no spend: a (b) hold then a (c) hold in one epoch
  spends one token) and `Expired` is judged against `gate_wait_started_at`;
  otherwise it opens a FRESH window in one statement: the previous window's
  hold, bounded by `window`, is first charged to the refill clock (holding
  never refills), then `k = floor((now − gate_tokens_refilled_at) /
  GateHoldRefill)` CLAMPED to `[0, GateHoldBudget]` (a future clock accrues
  nothing; no negative remainder), one token is spent atomically with the
  window stamp, and the refill clock moves to now (full, capped, NULL or
  future) or keeps its remainder, never past now. An empty budget answers
  `NoToken` and writes nothing. It never reads or writes the approval wait
  (`TestGateWait_*`). Call it OUTSIDE the merge-intent lock.
- **`Yield(id, epoch, YieldMarker{Reason, Detail, Row4})`**: live at `epoch`
  only (epoch CAS). ONE statement moves the row to `held` (phase and
  `admitted_at` cleared, `enqueued_at` kept), stamps `yield_reason`,
  `yield_detail` and `yielded_at`, bumps `yield_revision` and ORs `Row4` into
  `row4_flag`; `approval_wait_started_at` is kept. Empty reason →
  `ErrInvalidRequest` (`TestYield_OneTransactionUnderEpochCAS`).
  `YieldMarker.Continue` (the entry's last cleared episode on its base ref,
  supplied by the server, docs/adr/112 A3.7) makes `yielded_at` that
  episode's instead of the database clock when the row's `base_ref` equals
  the episode's, WHATEVER the head or reason; `Enqueue` marking an unmarked
  held entry applies it after its re-anchor. An already-marked entry keeps
  its own `yielded_at` (`TestYield_ContinuesLastClearedEpisodeOnSameBaseRef`).
- **`ClearYield(id, head, revision, clearRow4)`**: the ONLY way back in. Under
  the base lock, a compare-and-swap on `state = 'held' AND yielded_at IS NOT
  NULL AND anchored_head_sha = head AND yield_revision = revision`: clears
  the marker and CLOSES the approval wait (`approval_wait_open = false`)
  while RETAINING `approval_wait_started_at` (A3.7), and clears `row4_flag`
  only when `clearRow4`. The row rejoins at its original effective key
  (`enqueued_at`, or a kept promote `order_key`). A row whose head moved,
  whose marker was rewritten since (another revision) or that is not held →
  `ErrStateConflict` and the marker stays (`TestClearYield_CompareAndSwap`,
  `TestClearYield_StaleRevisionRefused`, asserted on read-back).
- **`UpdateYield(id, head, revision, YieldMarker)`**: the same CAS; rewrites
  reason and detail and ORs `Row4` into `row4_flag` (a yield transition).
  `TestUpdateYield_RewritesReasonOnly`,
  `TestUpdateYield_StaleRevisionCannotOverwriteNewerReason`, `TestRow4Flag_Lifecycle`.
- **`ReanchorYielded(id, head, revision, newHead, newBase)`**: the same CAS
  on the OLD head; moves the anchor to the PR's current head and KEEPS the
  marker, `yielded_at`, `approval_wait_started_at`, `row4_flag` and the hold
  budget (the same episode). A head move never clears a marker, so a push
  loop cannot buy a fresh wait window per push; the new head is judged by the
  same probe and cleared at the NEW head (`TestReanchorYielded_KeepsMarkerAndEpisode`).

Every marker write bumps `yield_revision` in its own statement
(`TestYieldRevision_EveryMarkerWriteBumps`), so a delayed probe that read an
earlier revision misses the CAS even when the episode continued the same
(head, `yielded_at`). `head` is required and `revision` must be non-negative
on the three marker CASes (`ErrInvalidRequest`); `revision` is the value read
back from the row (`Entry.YieldRevision`).

### Promote

ADR-093 D2's operator promote (#4297, the shape #4191 gave concurrency slots
in `backend/internal/concurrency`). `Promote(PromoteRequest{RunID, Record})` →
`Promotion{Entry, Changed, OldPosition, NewPosition, LiveRunID}` moves the
run's held entry to the head of its base's line among the UNMARKED held
entries in its account.

- **Order.** The run's account (unknown run → `ErrNotFound`), the per-run
  lock (Enqueue holds it across a base change, so the peeked entry cannot be
  swapped for another base's row), an unlocked peek of the active entry, the
  base lock BLOCKING, the entry re-read `FOR UPDATE`.
- **Refusals.** No active entry (none, or only terminal rows; or settled
  between the peek and the lock) → `ErrNotQueued`; live → `ErrEntryLive`
  (the live entry is never preempted); yielded → `ErrEntryYielded` (`AdmitNext`
  skips a marked row whatever its order); nil `RunID` or nil `Record` →
  `ErrInvalidRequest` before any SQL. Every refusal writes nothing
  (`TestPromote_RefusalsWriteNothing`, read back).
- **Write.** Already first → `Changed=false`, positions 1/1, nothing written,
  `Record` not called. Otherwise ONE guarded UPDATE sets `order_key` to the
  minimum effective key of the OTHER unmarked held entries on the base and
  account minus 1 ms, ONLY while one is still strictly ahead (evaluated in the
  same statement; a miss is the already-first answer:
  `TestPromote_AlreadyFirstNoop`). The newest promote therefore wins, and the
  key survives `Unadmit`, `Yield` (the row sorts last while marked) and
  `ClearYield` (it rejoins at its promoted place)
  (`TestPromote_NewestPromoteWinsAndSurvivesUnadmitYieldClear`). Another
  account's rows never count (`TestPromote_AccountScoped`).
- **Record.** `Record(ctx, tx, Promotion)` is REQUIRED and runs inside the
  promotion transaction after the write and before COMMIT, only when the
  order changed. A non-nil error rolls the WHOLE promotion back and is
  returned wrapped (`TestPromote_RecordErrorRollsBack`, read as committed
  state). The server appends `merge_queue_promoted` through it with
  `audit.AppendChainedTx` on that `tx`: unlike `RecordIntentFunc` (whose
  append would run on another connection and wait on the insert's own `FOR
  KEY SHARE`), `Record` receives the transaction, and Promote only UPDATEs a
  non-key column, so no foreign-key lock on the runs row is taken.

### Withdraw intent

ADR-112 R3's withdraw-intent verb (#4685). A yielded entry still carries the
operator's merge intent, so the queue owns the run's merge (ADR-109) and the
inline `merge_run` is refused. `WithdrawIntent(WithdrawIntentRequest{RunID,
Record})` → `Withdrawal{Entry, LiveRunID}` takes the merge out of the queue by
SETTLING the entry, never by clearing `merge_intent_at` (0107's "set once,
never cleared" holds; no migration).

- **Order.** Promote's: the run's account (unknown run → `ErrNotFound`), the
  per-run lock, an unlocked peek of the active entry, the base lock BLOCKING,
  the entry re-read `FOR UPDATE`.
- **Refusals**, classified on the re-read row in this order: no active entry
  (none, only terminal rows, or settled between the peek and the lock) →
  `ErrNotQueued`; live → `ErrEntryLive` (never withdrawn mid-pass); no merge
  intent → `ErrNoIntent`; held but UNMARKED → `ErrEntryNotYielded` (it is
  progressing to the queue's own head-pinned merge); then any
  `merge_dispatch_claims` row for the run, `dispatched` or
  `dispatch_failed`, read inside the transaction → `ErrDispatchClaimed`,
  naming the claim's state, path and head. Nil `RunID` or nil `Record` →
  `ErrInvalidRequest` before any SQL. Every refusal writes nothing
  (`TestWithdrawIntent_RefusalsWriteNothing`, read back, including the
  claim).
- **Write.** `settleTx(..., StateDropped, ReasonIntentWithdrawn)`: only
  `state`, `eject_reason` and `settled_at` change, so `enqueued_at`,
  `merge_intent_at`, `merge_intent_sequence` and the yield columns are
  retained as history (`TestWithdrawIntent_SettlesYieldedEntryDropped`).
  `LiveRunID` is the base's live run, read inside the transaction.
- **Record.** REQUIRED, called after the settle and before COMMIT; a non-nil
  error rolls the whole withdrawal back (`TestWithdrawIntent_RecordErrorRollsBack`).
  The server appends `merge_queue_intent_withdrawn` through it on `tx`; the
  settle touches no key column, so (the `PromoteRequest.Record` precedent)
  the append takes no foreign-key lock on the runs row.
- **Ledger.** The claim is READ, never written: a `dispatch_failed` claim is
  never silently cleared, and still needs reconciliation or the operator
  re-arm (§ "Merge-dispatch ledger"). The server calls `WithdrawIntent` under
  `WithMergeIntentLock`, the lock every `ClaimMergeDispatch` caller holds,
  so no claim lands between the read and the settle.
- **Every intent lookup then reads the intent as absent.** The entry is
  terminal: `ActiveForRun` (the server's queue-ownership check and the
  delegated arm) and the held arm of the merge-candidate gate find nothing,
  `AdmitNext` never sees it, and `ReentryCandidates` / `UnresolvedEjections`
  need an `ejected` newest row, so a later red-pass eject of the dropped row
  settles nothing and the run never re-enters
  (`TestWithdrawIntent_SuppressesAutoReentry`). A later `merge_run` that the
  merge-candidate gate routes back to the queue records intent on a FRESH
  row.

### Admission-epoch CAS

A pump's decision is made against the entry as it read it; another pump may
have Unadmitted and re-admitted it since. The epoch makes that stale decision
change nothing (ADR-093 slice 2, rejection item 1):

| Write | Guard | Who |
|---|---|---|
| `Unadmit` | live AND `admission_epoch = epoch` | admission transient failure, merge-on-pass readvance |
| `SetPhase` | live AND epoch | admission (incl. the D8 `advancing` stamp), verdict hook, transfer re-anchor, merge-on-pass's `merging` / `settling` stamps, the D8 resume |
| `ReanchorAt` | live AND epoch | admission re-anchor |
| `SettleAt` | live AND epoch (terminal: no-op) | admission settles, merge-on-pass settle outcomes |
| `ApprovalWait` / `GateWait` / `Yield` | live AND epoch | merge-on-pass approval and merge-gate waits and yields, admission yields |
| `ClearYield` / `UpdateYield` / `ReanchorYielded` | held AND marked AND `anchored_head_sha` AND `yield_revision` | the yield re-probe |
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
    whose outcome is uncertain. A head-PINNED inline or delegated dispatch
    the forge refuses for a head mismatch
    ([#4641](https://github.com/kuhlman-labs/fishhawk/issues/4641)) carries
    `forge.ErrMergeConflict` on every arm with a
    `*githubclient.HeadMismatchError`, and its basis decides the claim
    ([#4749](https://github.com/kuhlman-labs/fishhawk/issues/4749)): the
    client pre-check (`precheck`, no mutation sent) and the pinned REST PUT's
    409 (`rest_sha`: any 409 on that PUT, which GitHub documents as a `sha`
    mismatch, so the label names the status, not a proven mismatch) are
    refusals where nothing applied and release. The
    pinned auto-merge enable's `graphql_reread` basis (a GraphQL error, then
    a fresh read showing a different head) is INFERRED: a GraphQL execution
    error can accompany an applied mutation, so that enable may in fact have
    armed auto-merge at the pinned head. It is UNCERTAIN: the claim moves to
    `dispatch_failed` and only reconciliation or the operator re-arm clears
    it; an empty or unrecognised basis is treated the same (fail closed). A
    bare `forge.ErrMergeConflict` with no `HeadMismatchError` (the REST PUT
    409 on the queue's leaf, the unpinned ladder, GitLab) still releases.
    The settle's `merge_dispatch_settled` row records `refusal_basis`,
    `expected_head_sha` and `observed_head_sha` as separate keys
    (`backend/internal/server/docs/merge-queue.md` § "ADR-112 forge-approval
    fold", head-pinned bullets). A pinned GraphQL
    error whose re-read shows the SAME head, or whose re-read fails, stays a
    bare `forge.ErrValidation`: uncertain. A pre-forge head-pin refusal
    (`ErrHeadPinnedMergeUnsupported`) is deliberately uncertain too: it is a
    deterministic capability refusal, and a release would re-dispatch into it
    (#4683, ADR-093 D4). **`ReleaseMergeDispatch(claim)`**
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
  touches the ledger (the inline, DELEGATED and queue paths re-observe a
  landed merge BEFORE settling a claim after a failed dispatch, so a merge
  that landed leaves the claim `dispatched`; the delegated path does so with
  or without a claim, except on its own checks_pending refusal, and reports
  the acted outcome `mergeDispatchLandedOnReobserve`, #4617). The next
  merge-on-pass or `merge_run` touching a run with a claim re-reads the PR:
  merged → merge-on-pass reads the claim and settles the entry merged
  (`pr_merged`) only when there is no claim or the claim names the merged
  head, else it holds `merged_claim_head_mismatch` (WARN); the inline answer
  is `already_merged`. A claim recorded at head `unresolved` (an empty gate
  head; inline or delegated only, never a queue claim) names ANY merged head,
  in merge-on-pass and in the ADR-093 D8 resume (#4617). On both paths a
  reconciled `dispatch_failed` claim appends `merge_dispatch_settled`
  `reconciled_merged`, at most ONCE per run: deduped on (run, outcome),
  atomic on Postgres (`AppendChainedDeduped` under the run-row lock), with a
  non-atomic list-then-append fallback for the in-memory fakes. Unmerged → the claim
  stands (except a crash-left queue claim still `dispatched`, which
  merge-on-pass releases: "Crash-left claims" below): merge-on-pass holds
  `merge_already_dispatched` (WARN), the inline answer is
  `409 merge_already_dispatched` naming the claim, and the merge-queue read
  surfaces it on the entry (`dispatch_claim`: state, path, head, age,
  reason).
- **Operator re-arm** (operator-only, and only for an UNCERTAIN outcome; no
  HTTP route, MCP tool or Fishhawk code path deletes a `dispatch_failed`
  claim): after confirming on the forge that the PR did not merge and
  resolving the cause, the operator removes the failed claim with
  `DELETE FROM merge_dispatch_claims WHERE run_id = '<run id>' AND state = 'dispatch_failed';`
  against fishhawkd's database. On the INLINE (and delegated) path the
  operator then re-invokes `fishhawk_merge_run`. On the QUEUE path no
  `fishhawk_merge_run` re-invocation is needed: the queue's next pump
  re-decides from scratch (the ADR-112 forge-approval fold and veto run
  in-lock) and merges at most once. A `dispatched` claim is never re-armed
  by the operator: its merge was handed to the forge (a crash-left queue
  one is released by ADR-109 Amendment A1, below).
- **Crash-left claims (ADR-093 D8, #4298; ADR-109 Amendment A1, #4711).** A
  process that dies between `ClaimMergeDispatch` and classifying the forge
  outcome leaves a `dispatched` claim no live process owns. The server's
  restart resume (`backend/internal/server/README.md` § "Merge-queue
  durability (ADR-093 D8)") reconciles it with ONE forge re-read under the
  run's merge-intent lock, and itself adds NO ledger transition and
  dispatches NO merge (ADR-109); A1's release happens in merge-on-pass:
  - Only a `queue`-path claim is ever reconciled or released. An `inline` or
    `delegated` claim stays `dispatched` while the forge's auto-merge is
    armed, which is normal, as is a GitLab claim: neither the resume nor
    merge-on-pass ever transitions, releases or rewrites one, and any arm
    meeting one holds with no ledger write.
  - A queue claim is crash-left only when it is observed while HOLDING the
    merge-intent lock AND its `claimed_at` is older than the database clock
    (`DatabaseNow`, never Go `time.Now()`) minus `IntentFnTimeout` (the
    ADR-108 lock fn deadline) + the pinned merge-call timeout + a margin (the
    server names the constant and its derivation); a zero `claimed_at`, an
    unreadable clock or a store with no database clock is never crash-left
    (fail closed). There is no boot-time cutoff: a younger claim holds with
    nothing written (`merge_dispatch_in_progress`) and a later tick
    re-decides.
  - Merged at the claimed head → the normal merged reconcile and tail.
    Closed unmerged → the EXISTING `MarkMergeDispatchFailed(run,
    "orphaned_by_restart")` and the entry settles closed.
  - Not merged, PR OPEN (A1) → the resume records `merge_queue_resumed`
    `{redecide, orphaned_claim_open}` and returns the entry to `passed` with
    no ledger write. Ordinary merge-on-pass then re-runs steps 1-7 under the
    lock (entry, PR, head, base, the ADR-112 forge-approval fold, the R4
    `merge_queue_authorized` row, the verdict) and, in step 8's not-claimed
    branch ONLY, releases the claim: a strict `merge_dispatch_settled`
    `{released, released_orphaned_claim}` row FIRST, then
    `ReleaseMergeDispatch` (the CAS DELETE on `state = 'dispatched'`), then a
    fresh `ClaimMergeDispatch` and the ordinary pinned merge (at most one
    extra pinned merge call per restart; no bespoke retry). A failed row or
    release holds `orphaned_claim_release_failed` with the claim standing.
    The release is CONDITIONAL on that decision's fold approving: while the
    fold is not approved the decision waits at step 6b (`await_approval`),
    the PR still needs a qualifying approval, and the orphaned claim keeps
    holding the head of the line until it arrives or the operator acts; the
    ADR-109 pre-yield refusal still never yields a claimed entry.
  - Not merged, PR read INDETERMINATE (a failed read) → the EXISTING
    `MarkMergeDispatchFailed(run, "orphaned_by_restart")` (its
    `merge_dispatch_settled` row carries reason `orphaned_by_restart`), then
    the entry HOLDS with a remedy naming the operator re-arm above. The
    queue-path re-arm needs NO `fishhawk_merge_run` re-invocation: the next
    pump re-decides. An entry in phase `settling` (the forge accepted) is
    never orphaned: PR open or an indeterminate read holds
    `settling_unconfirmed` with no ledger write.
  - What A1 changed and what stays operator-only: the open-PR release above
    is the ONE automatic release of a `dispatched` claim. A `dispatch_failed`
    claim (an uncertain outcome, or `orphaned_by_restart`, including those
    left before A1) is NEVER released automatically: only reconciliation or
    the operator re-arm clears it. Nothing is retried in a loop; the
    retry-once and the moved head/base release are not implemented, and any
    later automatic retry must re-run the ADR-112 forge-approval fold and
    veto in-lock, failing closed, before calling the forge.
  - Visibility: the merge-queue read labels a queue claim
    `dispatch_claim.orphan` (`orphaned`: still `dispatched` and crash-left on
    the database clock; `orphaned_by_restart`: `dispatch_failed` with that
    cause, awaiting the re-arm), and the run read's
    `merge_candidate_queue.dispatch_claim_orphan` carries the same label. A
    released claim is deleted, so its record is the `released_orphaned_claim`
    audit row.

## Reasons

The store writes two reasons itself: `base_changed` (`ReasonBaseChanged`,
`Enqueue`'s base change) and `intent_withdrawn` (`ReasonIntentWithdrawn`,
`WithdrawIntent`, both dropped). Every other reason is the caller's string. It READS one: `ReentryCandidates`
matches `ReentryReason` (`verify_failed`), the only ejection ADR-093 D4
re-enters automatically. ADR-092's set (owned by the server
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
  `MarkMergeIntent`, `ApprovalWait`, `GateWait`, `Yield`, `ClearYield`, `UpdateYield`,
  `ReanchorYielded`): read the entry's base → its base lock → the row
  `FOR UPDATE`. `repo`/`base_ref` never change on a row, so the derived key
  stays correct.
- `AdmitNext` / `AdmitNextReclaiming` (one shared admit transaction): the
  base lock → the live check (and its re-claim, whose `Record` runs under the
  base lock and the live row's lock, before COMMIT) → the first unmarked held
  row by effective key.
- `Promote` and `WithdrawIntent`: per-run lock → peek → the entry's base
  lock → the row `FOR UPDATE` (Enqueue's order with one base).

No path takes a row lock before an advisory lock, and multi-key acquisition is
ascending, so the order is acyclic. The one-live-per-base guarantee is the
base lock: `TestAdmitNext_LockSerializesTheLiveCheck` (deterministic, parks an
admitter between its live check and its held select) and
`TestAdmitNext_ConcurrentAdmittersAdmitExactlyOne` (8 goroutines) both go RED
with the lock removed. Reads (`ActiveForRun`, `ListForBase`,
`RecentLineTurns`, `ActiveBases`, `LatestForRun`, `UnresolvedEjections`,
`ReentryCandidates`) are single statements without the lock.

Every time comparison uses the database clock (`clock_timestamp()`); no Go
`time.Now()` value is passed into SQL (the marker CASes pass back a
`yielded_at` READ from the row, compared for equality, never against the
clock; `AdmitNextReclaiming`'s `AdmittedBefore` is an instant READ from
`DatabaseNow`).

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
| `AdmissionStaleAfter` | 10 min | Bounds phases `admitted` and `advancing`. Admission work (auto-advance merge + bounded post-merge read + trigger append + re-open) takes seconds; 10 min is far past a slow forge, so a re-claim means a crash, not a slow pump. The re-run is idempotent (advance is a no-op when up to date; the pass start is per-head idempotent). At boot, `AdmitNextReclaiming` re-claims a pre-boot admission at once instead. |
| `GateHoldBudget` | 3 | docs/adr/112 A3.7's hold budget N: at most three merge-gate windows per refill period; the 0115 CHECK's upper bound. |
| `GateHoldRefill` | 1 h | A3.7's refill period R: one token per hour the row spends not holding. |
| `IntentFnTimeout` | 90 s (`intentFnTimeout`) | The merge-intent lock's fn deadline (§ "Merge-intent lock"), exported for the D8 resume's crash-left queue-claim age window (§ "Merge-dispatch ledger"). |

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
