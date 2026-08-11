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
