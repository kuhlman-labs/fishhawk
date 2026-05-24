-- Audit-entry queries consumed by the postgres adapter for the
-- audit.Repository interface (E2.1 / #22). Append-only — no
-- UpdateAuditEntry or DeleteAuditEntry queries exist or will exist
-- in this file. Database-level triggers backstop the same invariant.

-- name: AppendAuditEntry :one
INSERT INTO audit_entries (
    id, run_id, stage_id, ts, category, actor_kind, actor_subject,
    payload, prev_hash, entry_hash
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
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

-- name: GetLastGlobalAuditEntry :one
-- Mirror of GetLastAuditEntryForRun for the global chain partition
-- (E2.7 / #138). Used by AppendGlobalChained to fetch prev_hash for
-- the next non-run event (token issue/revoke, OAuth sign-in, etc.).
SELECT * FROM audit_entries
 WHERE run_id IS NULL
 ORDER BY sequence DESC
 LIMIT 1;

-- name: ListGlobalAuditEntries :many
-- Used by compliance exports + the verifier to walk the global
-- chain in append order.
SELECT * FROM audit_entries
 WHERE run_id IS NULL
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
       ae.actor_kind, ae.actor_subject, ae.payload, ae.prev_hash, ae.entry_hash
FROM audit_entries ae
JOIN run_chain rc ON ae.run_id = rc.id
ORDER BY ae.sequence ASC;

-- name: ListAuditEntriesAll :many
-- Cross-chain feed (per-run rows + global-chain rows) used by the
-- audit-log search surface (#211). Time-descending so the most-recent
-- governance event is at the top; secondary sort on (id) keeps ordering
-- deterministic when entries share a millisecond. Optional category +
-- run_id filters; sqlc.narg makes them omittable from the WHERE.
SELECT * FROM audit_entries
 WHERE (sqlc.narg(category)::text IS NULL OR category = sqlc.narg(category)::text)
   AND (sqlc.narg(run_id)::uuid  IS NULL OR run_id   = sqlc.narg(run_id)::uuid)
 ORDER BY ts DESC, id DESC;
