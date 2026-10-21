# backend/internal/mergequeue

Persisted held-pass queue for ADR-092 D1 (#4200, `docs/adr/092-serialize-merge-candidate-verify-passes.md`).
At most ONE merge-candidate verify pass is live per `(repository, base ref)`
inside one account. Every other eligible pass is held FIFO by eligibility time
and consumes no runner slot. Table: `merge_candidate_queue_entries`
(migration 0101; `merge_intent_at` added by 0106 for ADR-093 D2, #4295).
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
| `merge_intent_at` | The operator's merge intent on this episode (0106, ADR-093 D2): NULL = none (every ADR-092 rebase/base-advance entry and every pre-0106 row). Stamped once from the database clock by `Enqueue{MergeIntent}` or `MarkMergeIntent`, never cleared; a terminal row keeps it as history, and a re-entry after a terminal row starts a FRESH row without it. |
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

- **`Enqueue(EnqueueRequest{RunID, BaseRef, HeadSHA, BaseSHA, MergeIntent, RecordIntent})`**. `RunID`,
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
- **`MarkMergeIntent(id, record)`** → `MergeIntentResult{Entry,
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
  never followed by an unrecorded intent. This is the seam the server's
  `merge_queue_entered` audit row uses (the #4358
  `appendCampaignAuditStrict` pattern); the callback runs under the call's
  advisory and row locks and must not call back into the Store
  (`TestRecordIntent_FailureRollsBackAndRetryRecords`).
- **`ActiveForRun(runID)`** → `(Status, found, error)`: the run's active entry,
  its 1-based `Position` among the base's held entries (0 when live), and
  `LiveRunID` (the base's live run; the entry's own run when it is live; nil
  when none). Position and live run count the run's OWN account only.
- **`ListForBase(base)`**: the base's active entries, live first, then held by
  `(enqueued_at, id)`. Never nil. The seed for ADR-093's queue read surface.
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
  `MarkMergeIntent`): read the entry's base → its base lock → the row
  `FOR UPDATE`. `repo`/`base_ref` never change on a row, so the derived key
  stays correct.
- `AdmitNext`: the base lock → the live check → the oldest held row.

No path takes a row lock before an advisory lock, and multi-key acquisition is
ascending, so the order is acyclic. The one-live-per-base guarantee is the
base lock: `TestAdmitNext_LockSerializesTheLiveCheck` (deterministic, parks an
admitter between its live check and its held select) and
`TestAdmitNext_ConcurrentAdmittersAdmitExactlyOne` (8 goroutines) both go RED
with the lock removed. Reads (`ActiveForRun`, `ListForBase`,
`RecentLineTurns`, `ActiveBases`) are single statements without the lock.

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
  until a caller settles it; the serializer drops a terminal-run live entry
  when it pumps, and fishhawkd pumps every `ActiveBases` base once at boot and
  then on a 1-minute ticker (`backend/internal/server/README.md`, "Periodic
  pump"), so for a run that goes terminal without a settle the bound is one
  pump tick. The run's cancel path releases at once.
- A `passed` live entry holds the line until its PR merges (ADR-092 D4); an
  operator who never merges stalls the base. Exception: a re-open of the run
  (fix-up, conflict resolution) or a head reset drops it (#4352), and the run
  re-enters at the tail. ADR-093 merge-on-pass removes the stall.
