# backend/internal/audit

Append-only audit event log — the product's central artifact (MVP_SPEC §4.4).
This README is the long-form behavioral contract for the hash-chain design;
`docs/ARCHITECTURE.md` §10 points here.

## Chain partitions

Every `audit_entries` row belongs to exactly one hash chain, and each chain
links from a nil-`prev_hash` genesis entry via `prev_hash` = predecessor's
`entry_hash`:

- **Per-run chains** — rows with `run_id` set chain within their run.
  `AppendChained`/`AppendChainedTx` serialize writers with `SELECT … FOR
  UPDATE` on the run row and stamp the locked run's `account_id` on the
  entry (a stamp, not a chain key — per-run chains are already
  tenant-isolated through the run's account).
- **Run-less ("global") chains, one per account** (ADR-057 / #1828) — rows
  with `run_id IS NULL` chain within their `account_id` partition; `NULL`
  account is the untenanted legacy partition (#1829 NULL-allow window).
  `AppendGlobalChained` reads `prev_hash` from the last entry of the
  entry's partition and serializes concurrent appends with a Postgres
  advisory **transaction** lock keyed on the partition
  (`globalChainLockKey`: domain-separated sha256 over the account UUID, a
  fixed sentinel key for the NULL partition). Read committed plus a plain
  transaction is NOT sufficient here — with no run row to lock and no
  unique constraint, two concurrent appends could both observe the same
  last entry (or both see an empty partition) and fork the chain; the
  advisory lock closes that race for tenant and untenanted partitions
  alike. Migration 0058's partial index `(account_id, sequence DESC) WHERE
  run_id IS NULL` serves the last-entry lookup and the partition walk.

Readers: `ListGlobal` returns the whole run-less set across partitions
(compliance-export enumeration); `ListGlobalByAccount` returns ONE
partition in append order — the view per-account verification walks and
the JSON export emits per key.

## Atomic retry-budget append (`RetryBudgetAppender`, #2518)

`AppendChainedUnderBudget` / `AppendChainedUnderBudgetTx` are an **atomic
check-and-consume** append: they count a run's existing entries of one category
and append a fresh chained entry only when that count is below `maxEntries`,
returning `ErrRetryBudgetExhausted` (writing nothing) when the budget is spent.
The payload is built by a `stamp(attempt)` callback with `attempt = count+1`, so
the recorded attempt number is truthful. They back the plan-retry budgets the
server enforces on both the schema-retry (`plan_schema_retry`) and scope-retry
(`plan_scope_retry`) paths, where the old count-then-append had a non-atomic
window: two concurrent plan ships could both read a below-budget count and both
consume the one-shot budget.

**Ordering is load-bearing** and must stay: `LockRunForUpdate(run)` FIRST, THEN
count, THEN append. Because the count runs *after* the row lock is granted, and
Postgres READ COMMITTED takes a fresh snapshot per statement, a second
transaction's count observes the first's committed entry — so the budget is
strictly one-shot under concurrency. The transaction MUST run at the
server-default READ COMMITTED isolation (`AppendChainedUnderBudget` sets no
`TxOptions`): a REPEATABLE READ snapshot would predate the lock and could count
zero after the lock is granted, reopening the race. The delegate to
`AppendChainedTx` keeps the hashing/chaining path byte-identical to every other
chained append (it re-locks the same run row inside the same transaction — a
harmless re-entrant no-op).

This is deliberately a **transactional read-modify-write, NOT a partial unique
index** on `(run_id, category)` (contrast the three at-most-one indexes below).
The audit table is append-only and hash-chained: the merge-verdict / parent-await
/ approval-truncated indexes make a *distinct* category idempotent, and any
pre-existing duplicate there could in principle be removed. A plan-retry budget
guards a category the race can leave with genuine duplicates, and those cannot be
de-duplicated to make an index buildable without deleting an audit row and
breaking the very chain the verifier depends on. The read-modify-write asserts
nothing about already-written rows: a populated database holding duplicate
`plan_scope_retry` rows from the old race keeps them, verifies, and is bounded
only going forward — there is no migration, no index, and no backfill.

`RetryBudgetAppender` is an OPTIONAL capability kept OFF the `Repository`
interface (mirroring `run.StageCASTransitioner`), so the ~20 manually-written
full-interface fakes don't break; `postgres.go` carries a `var _
RetryBudgetAppender = (*postgresRepo)(nil)` compile-time assertion so a
production repo silently losing the capability is a build failure, and the
server's fallback for a non-capable repo (in-memory fakes only) is loud-logged.

## Atomic anchored append (`AnchoredChainAppender`, #2536)

`AppendChainedAnchored` / `AppendChainedAnchoredTx` are an **atomic
anchor-revalidate-and-append**: they re-read the newest entry of an ANCHOR
category and scan for a prior entry already bound to that anchor, then append —
all in ONE transaction, under the run-row lock. They back POST
`/v0/runs/{run_id}/acceptance-arbitration`, whose two controls both sat OUTSIDE
the append's transaction before #2536: the endpoint idempotence scan listed
prior arbitrations several reads earlier, and guard 7 re-read the newest
acceptance outcome immediately before `AppendChained`. So (mode 1) two
concurrent valid POSTs could each pass and each append, and (mode 2) a newer
`acceptance_outcome_recorded` entry could commit between the final re-read and
the append, persisting an arbitration that named an already-superseded outcome
while returning 200.

**Ordering is load-bearing** and must stay: `LockRunForUpdate(run)` FIRST, THEN
the anchor re-read, THEN the dedupe scan, THEN `AppendChainedTx`. Because the
reads run *after* the row lock is granted, and Postgres READ COMMITTED takes a
fresh snapshot per statement, they observe any competing append that committed
before the lock was granted. The transaction MUST run at the server-default READ
COMMITTED isolation (`AppendChainedAnchored` sets no `TxOptions`): a REPEATABLE
READ snapshot would predate the lock and could observe the stale pre-append
state, reopening the race.

**THE GUARANTEE, at the strength the mechanism delivers.** No entry is ever
persisted that named an ALREADY-SUPERSEDED anchor **at append time**. It is
explicitly **NOT** a permanent-newest property.
Anchored-append-commits-then-newer-anchor-lands is a legal, unpreventable
interleaving, and it is not a defect: the entry was valid when written, the
newer anchor supersedes it, and the authoritative gate — which requires the
entry to name the NEWEST anchor — re-wedges. For acceptance arbitration that
means `acceptanceGateState` flips back from `acceptance_arbitrated` to
`acceptance_triage` and the operator arbitrates the new outcome. Fail-closed,
and pinned deterministically by
`server.TestAcceptanceArbitration_ArbitrationFirstOrdering`.

**Two duplicate paths, one caller branch.** `*AnchoredDuplicateError` carrying
the surviving `*Entry` is returned BOTH by the in-transaction dedupe scan and by
the out-of-transaction recovery after a backstop-index 23505 (below), so the
caller has one already-recorded branch.

**Which control carries mode 1.** The migration 0080 index is a genuine
BACKSTOP, not the primary control — and on today's code paths its collision
branch is near-unreachable for a same-run duplicate. `AppendChainedTx` holds
`SELECT … FOR UPDATE` on the run row, and `audit_entries.run_id` REFERENCES
`runs`, so the FK check's `FOR KEY SHARE` on the referenced row conflicts with
that `FOR UPDATE`: while the anchored transaction holds the lock, no other
transaction can insert ANY audit row for that run. The control that actually
carries mode 1 is therefore the **lock + in-transaction dedupe scan**, and it is
pinned standing ALONE by
`TestPostgres_AppendChainedAnchored_ConcurrentExactlyOne_IndexDropped`, which
DROPs the index in its own ephemeral database and still requires exactly one
committed row.

**The 23505 recovery matches on INDEX semantics, deliberately.** On a collision
`pgx.BeginFunc` has already rolled back, so the committed colliding row is
re-read on the pool with a TEXT comparison of `payload->>'<key>'` in SQL — NOT
the typed Go decode the in-transaction scan uses. That rule is load-bearing: the
index key is the TEXT projection, so a JSON number `7` and a JSON string `"7"`
share the key `'7'` while the `*int64` decode misses the string. Re-running the
typed decode could fail to find the very row the index collided with; the text
rule always finds it.

**The not-found leg after a collision is UNREACHABLE BY CONSTRUCTION, and there
is deliberately no test for it.** A 23505 on the partial index PROVES a
committed row exists with the same textual `(run_id, payload->>'<key>')` key,
and audit rows cannot be deleted (migration 0002's BEFORE UPDATE/DELETE triggers
RAISE). Because the re-read matches on those same index semantics, it MUST find
that row. Reaching the leg would mean the index and the append disagree about
identity, which no current code path can produce. It is kept as defensive code
returning a plain error (surfaced as a 500 integrity anomaly) rather than an
unhandled nil — inventing a vehicle to force it would test the vehicle, not the
code.

`AnchoredChainAppender` is an OPTIONAL capability kept OFF the `Repository`
interface (mirroring `RetryBudgetAppender` above), so the ~20 manually-written
full-interface fakes don't break; `postgres.go` carries a `var _
AnchoredChainAppender = (*postgresRepo)(nil)` compile-time assertion so a
production repo silently losing the capability is a build failure. The handler's
fallback for a non-capable repo (in-memory fakes only) is the prior non-atomic
re-read-then-append and is debug-logged — a real residual: **the atomicity
guarantee holds only for the Postgres repository.**

The primitive is authored generically (anchor category + sequence + dedupe
payload key). #2178 was the same shape but is CLOSED as NOT_PLANNED, so there is
no second consumer today and no live coordination — the genericity is
speculative-but-cheap. Compare the store-enforced idiom in
`backend/internal/webhook/postgres.go` (`INSERT … ON CONFLICT DO NOTHING
RETURNING`, an empty result meaning already-seen) and the
`IsDuplicateOnConstraint` narrowings for #1983 / #2594 / #2622 below;
`IsAcceptanceArbitrationDuplicate` is the #2536 member of that family.

## At-most-one merge_verdict_recorded per run (0062 / #1983)

The `merge_verdict_recorded` category (POST `/v0/runs/{run_id}/merge`) is
gated to **at most one row per run** by migration 0062's partial unique
index `audit_entries_merge_verdict_recorded_once_idx` (`ON audit_entries
(run_id) WHERE category = 'merge_verdict_recorded'`). This closes the
endpoint's read-then-append race: two genuinely concurrent merge POSTs for
one run can no longer both observe zero rows and both append a verdict (and
double-dispatch the merge). `AppendChainedTx`'s `SELECT … FOR UPDATE` on the
run row serializes the two appends, so the race-loser's insert
deterministically violates the index.

Callers distinguish that benign collision from an unrelated integrity
failure with the **constraint-specific** helpers in `postgres.go`:
`IsMergeVerdictDuplicate(err)` matches ONLY a `unique_violation` (SQLSTATE
23505) on `MergeVerdictRecordedOnceIndex` — or the `ErrMergeVerdictDuplicate`
sentinel that fakes return — via `IsDuplicateOnConstraint(err, name)` over an
`errors.As` unwrap (`AppendChained` `%w`-wraps the driver error). A 23505 on
any OTHER constraint (hash-chain / entry-hash / `(run_id, sequence)`
uniqueness) is deliberately NOT swallowed, so an unrelated failure stays a
hard error rather than being mistaken for the concurrent-merge race.

## At-most-one parent_awaiting_child_scope_decision per (run, child stage) (0067 / #2594)

The `parent_awaiting_child_scope_decision` category (a decomposition parent's
signal that a child parked awaiting a scope-completeness decision, #2548) is
gated to **at most one row per (parent run, child stage)** by migration 0067's
partial unique index
`audit_entries_parent_awaiting_child_scope_decision_once_idx` (`ON audit_entries
(run_id, (payload->>'child_stage_id')) WHERE category =
'parent_awaiting_child_scope_decision'`). This closes the check-then-append race
between the category's TWO emitters — `server`'s park-time
`emitParentAwaitingChildScopeDecision` and `orchestrator`'s sibling-settle
`surfaceParkedChildren` — which previously de-duplicated with a read in only one
of them; two concurrent sibling terminal transitions, or the park-time emitter
racing a sibling settle, could both land an entry for the same child stage.
`AppendChainedTx`'s `SELECT … FOR UPDATE` on the run row serializes the appends,
so the race-loser's insert deterministically violates the index at the single
choke point both emitters share.

The key is `(run_id, child_stage_id)`, NOT `run_id` alone: one parent
legitimately carries ONE entry per parked child stage, so keying on `run_id`
alone would collapse distinct parked children into one signal.

Both emitters distinguish that benign collision from an unrelated integrity
failure with `IsParentAwaitingChildScopeDecisionDuplicate(err)` in `postgres.go`,
which matches ONLY a `unique_violation` on
`ParentAwaitingChildScopeDecisionOnceIndex` — or the
`ErrParentAwaitingChildScopeDecisionDuplicate` sentinel that fakes return — and
deliberately does NOT swallow a 23505 on any other constraint (mirrors the
merge-verdict narrowing above). On that collision the emitter logs INFO (the
benign already-recorded outcome) and neither unwinds the park nor the parent's
dispatch top-up.

**#2591 caveat — this key has NO episode component.** It enforces at-most-one
entry per parent run per child stage for the ENTIRE life of the run, not per park
episode, which matches today's done-means because a child stage parks exactly
once. If **#2591** (amend-and-resume for a parked child) lands and a child can be
RE-PARKED within the same parent run, the second park's advisory entry will hit
the duplicate-tolerant branch, log INFO, and append nothing — silently
suppressing a legitimate second-park signal. The #2591 implementer MUST widen the
key to carry an episode component (or emit a distinct category) before allowing
re-park; do not rely on this index to surface a second park.

## At-most-one approval_conditions_truncated per (run, source approval comment) (0068 / #2622)

The `approval_conditions_truncated` category (`server/prompt.go`'s
`loadApprovalConditions` → `appendApprovalConditionsTruncatedAudit`, #2583) is
gated to **at most one row per (run, source approval comment)** by migration
0068's partial unique index
`audit_entries_approval_conditions_truncated_once_idx` (`ON audit_entries
(run_id, (payload->>'source_entry_id')) WHERE category =
'approval_conditions_truncated'`). The emitter appends this entry on EVERY
implement-prompt build that loads a legacy over-cap approve comment, and prompt
construction for a stage repeats (retries, prompt-render fetches), so without the
index one truncation would accumulate N entries and the observability surface
(added by #2583 so a dropped operator condition is visible) would report one
dropped condition as five.

The key is `(run_id, source_entry_id)`, NOT `run_id` alone: `source_entry_id` is
the id of the `approval_submitted` entry whose comment was truncated, so a run
that carries two DISTINCT over-cap approve comments across a re-plan cycle still
records each genuine truncation ONCE under its own key. A `run_id`-only key would
silently suppress the second drop — the same class of audit lie in the other
direction.

The single emitter distinguishes the benign already-recorded collision from an
unrelated integrity failure with `IsApprovalConditionsTruncatedDuplicate(err)` in
`postgres.go`, which matches ONLY a `unique_violation` on
`ApprovalConditionsTruncatedOnceIndex` — or the
`ErrApprovalConditionsTruncatedDuplicate` sentinel that fakes return — and
deliberately does NOT swallow a 23505 on any other constraint (mirrors the
merge-verdict and parent-awaiting narrowings above). On that collision the emitter
logs INFO (the benign already-recorded outcome) and still returns the capped
conditions; every other append error keeps its WARN. Both paths are non-fatal — a
bookkeeping append never blocks prompt construction.

**Safe against the duplicates the bug already produced.** Every pre-0068 row lacks
the `source_entry_id` payload key, so `payload->>'source_entry_id'` yields SQL
NULL and — NULLs being distinct in a PostgreSQL unique index — any number of
key-less rows coexist. `CREATE UNIQUE INDEX` therefore never sees a collision
among the pre-existing accumulated rows and cannot fail loud at migrate time.

## At-most-one acceptance_triage_arbitrated per (run, discharged outcome) (0080 / #2536)

The `acceptance_triage_arbitrated` category (POST
`/v0/runs/{run_id}/acceptance-arbitration`) is gated to **at most one row per
(run, discharged acceptance outcome)** by migration 0080's partial unique index
`audit_entries_acceptance_triage_arbitrated_once_idx` (`ON audit_entries
(run_id, (payload->>'outcome_sequence')) WHERE category =
'acceptance_triage_arbitrated'`).

Unlike 0062 / 0067 / 0068, this index is explicitly a **backstop, not the
primary control** — see the anchored-append section above for why (the FK
`KEY SHARE` / `FOR UPDATE` conflict makes a same-run collision near-unreachable)
and for which control actually carries mode 1.

The key is `(run_id, outcome_sequence)`, NOT `run_id` alone: ONE run
legitimately carries ONE arbitration PER DISTINCT discharged outcome, because a
later acceptance re-run records a higher-sequence outcome that no prior
arbitration names and the operator arbitrates that one too. A `run_id`-only key
would refuse the second, legitimate discharge.

`IsAcceptanceArbitrationDuplicate(err)` (in `anchored.go`) matches ONLY a
`unique_violation` on `AcceptanceTriageArbitratedOnceIndex` — or the
`ErrAcceptanceArbitrationDuplicate` sentinel for fakes — mirroring the three
narrowings above; a 23505 on any OTHER constraint stays a hard error.

**The migration FAILS LOUD** on a pre-existing duplicate: a DO-block pre-flight
detects them and RAISEs naming the offending `(run_id, outcome_sequence)` and
entry ids, rather than surfacing an opaque index-build error. Its documented
remedy is chain-integrity-safe, and the obvious remedy is WRONG: there is no
routine "delete the redundant row". Migration 0002's append-only triggers refuse
a DELETE outright, and even with them dropped, removing a hash-chained row
breaks continuity for every later entry in that run and breaks the SHIPPED
verification surface in the separate `verifier/internal/audit` module. In this
PRE-ALPHA repo the honest remedy is to **recreate the database**; for a
hypothetical real deployment it is a chain-aware rewrite that re-hashes the
affected run's whole chain (invalidating any published export) or an explicitly
recorded, accepted chain break.

## Frozen HashInputs (deliberate)

`account_id` is **not** part of the canonical hash (`chain.go`
`HashInputs`). The partition membership is carried entirely by the
`prev_hash`-lookup scope. Rationale: the hash is an unkeyed sha256 over
non-omitempty canonical JSON, so adding a field would break every stored
hash and the external verifier for zero marginal protection against a
DB-writer adversary (who can recompute chains wholesale regardless). A
naive account relabel of a row IS still detected: pulling an entry out of
(or into) a partition breaks the `prev_hash` linkage of the interleaved
sequences within the affected partitions. Cryptographic tenant-ownership
binding (signed/anchored per-account export manifests) is a deliberate
follow-up, not part of this design.

## Append-only enforcement

Three layers: (1) the `Repository` interface has no Update/Delete; (2) DB
triggers (migration 0002) refuse UPDATE/DELETE on `audit_entries`; (3) the
static-analysis test (`static_analysis_test.go`) scans all backend Go/SQL
for mutation statements. Exemptions: this package's trigger tests, the
migrations dir, and `backend/internal/auditrehash`.

## Re-anchor one-shot (`fishhawkd audit-rehash`)

`backend/internal/auditrehash` is the operator-invoked, idempotent,
dry-runnable one-shot that (a) rewrites hashes under the canonical
algorithm (#302) and (b) since ADR-057 / #1828 segments the run-less rows
per `account_id`, re-anchoring each partition (including untenanted) as an
independent chain from its own genesis. It runs in one transaction with
the append-only triggers disabled inside that transaction only; any
failure rolls back both the trigger disable and every row change.
Rollout ordering for per-account verification: **backfill `account_id`
onto historical run-less rows → run `fishhawkd audit-rehash` → rely on
per-account verification.** Until the backfill lands, every historical
row is untenanted and the per-account re-anchor is a no-op.
