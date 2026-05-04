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
