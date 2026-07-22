-- Audit-entry queries consumed by the postgres adapter for the
-- audit.Repository interface (E2.1 / #22). Append-only — no
-- UpdateAuditEntry or DeleteAuditEntry queries exist or will exist
-- in this file. Database-level triggers backstop the same invariant.

-- name: AppendAuditEntry :one
INSERT INTO audit_entries (
    id, run_id, stage_id, ts, category, actor_kind, actor_subject,
    payload, prev_hash, entry_hash, account_id
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: GetAuditEntry :one
SELECT * FROM audit_entries WHERE id = $1;

-- name: ListAuditEntriesForRun :many
SELECT * FROM audit_entries
WHERE run_id = $1
ORDER BY sequence ASC;

-- name: GetLastAuditEntryForRun :one
-- Used by Append to fetch prev_hash for the next entry in the run.
SELECT * FROM audit_entries
WHERE run_id = $1
ORDER BY sequence DESC
LIMIT 1;

-- name: ListAuditEntriesByCategory :many
SELECT * FROM audit_entries
WHERE run_id = $1 AND category = $2
ORDER BY sequence ASC;

-- name: GetLastGlobalAuditEntryForAccount :one
-- Mirror of GetLastAuditEntryForRun for one tenant account's run-less
-- chain partition (ADR-057 / #1828, was the single-partition
-- GetLastGlobalAuditEntry, E2.7 / #138). Used by AppendGlobalChained
-- to fetch prev_hash for the next non-run event (token issue/revoke,
-- OAuth sign-in, etc.) within the account's chain. Served by the 0058
-- partial index audit_entries_global_account_seq_idx.
SELECT * FROM audit_entries
 WHERE run_id IS NULL AND account_id = $1
 ORDER BY sequence DESC
 LIMIT 1;

-- name: GetLastGlobalAuditEntryUntenanted :one
-- The account_id IS NULL variant of GetLastGlobalAuditEntryForAccount:
-- the untenanted legacy partition (historical rows plus writers with no
-- resolvable account — the #1829 NULL-allow window) stays one chain of
-- its own.
SELECT * FROM audit_entries
 WHERE run_id IS NULL AND account_id IS NULL
 ORDER BY sequence DESC
 LIMIT 1;

-- name: ListGlobalAuditEntries :many
-- Used by compliance exports + the verifier to walk the run-less
-- partition (all accounts) in append order.
SELECT * FROM audit_entries
 WHERE run_id IS NULL
 ORDER BY sequence ASC;

-- name: ListGlobalAuditEntriesByAccount :many
-- One account's run-less chain partition in append order (ADR-057 /
-- #1828); IS NOT DISTINCT FROM folds the untenanted partition in — a
-- NULL $1 selects the account_id IS NULL rows. Feeds the per-account
-- compliance-export groups and per-partition verification.
SELECT * FROM audit_entries
 WHERE run_id IS NULL AND account_id IS NOT DISTINCT FROM $1
 ORDER BY sequence ASC;

-- name: ListAuditEntriesForRunChain :many
-- Recursive CTE walk: the anchor is the run with id = $1; the
-- recursive arm follows parent_run_id links. When $2::boolean
-- (includeDecomposed) is false, runs where decomposed_from IS NOT
-- NULL are pruned from the walk so the result covers only the CI-
-- retry chain. When true, all descendants are included.
WITH RECURSIVE run_chain AS (
    SELECT id FROM runs WHERE id = $1
    UNION ALL
    SELECT r.id FROM runs r
    JOIN run_chain rc ON r.parent_run_id = rc.id
    WHERE ($2::boolean OR r.decomposed_from IS NULL)
)
SELECT ae.id, ae.sequence, ae.run_id, ae.stage_id, ae.ts, ae.category,
       ae.actor_kind, ae.actor_subject, ae.payload, ae.prev_hash, ae.entry_hash,
       ae.account_id
FROM audit_entries ae
JOIN run_chain rc ON ae.run_id = rc.id
ORDER BY ae.sequence ASC;

-- name: ListAuditEntriesAll :many
-- Cross-chain feed (per-run rows + global-chain rows) used by the
-- audit-log search surface (#211). Time-descending so the most-recent
-- governance event is at the top; secondary sort on (id) keeps ordering
-- deterministic when entries share a millisecond. Optional category +
-- run_id filters; sqlc.narg makes them omittable from the WHERE.
-- account_id (ADR-057 / #1830) scopes to a tenant workspace account: a
-- set filter keeps the account's rows PLUS untenanted (NULL account_id)
-- rows — the same contract as run.ListRuns — while NULL (unset) keeps
-- the internal system readers' cross-account scans unconstrained.
SELECT * FROM audit_entries
 WHERE (sqlc.narg(category)::text IS NULL OR category = sqlc.narg(category)::text)
   AND (sqlc.narg(run_id)::uuid  IS NULL OR run_id   = sqlc.narg(run_id)::uuid)
   AND (sqlc.narg(account_id)::uuid IS NULL OR account_id = sqlc.narg(account_id)::uuid OR account_id IS NULL)
 ORDER BY ts DESC, id DESC;
