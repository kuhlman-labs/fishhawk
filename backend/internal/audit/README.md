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
