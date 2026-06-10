-- Scope amendment queries (E22.X / #961). sqlc generates typed Go
-- into ./db per backend/sqlc.yaml.

-- name: CreateScopeAmendment :one
INSERT INTO scope_amendments (id, run_id, stage_id, paths, reason)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetScopeAmendmentByID :one
SELECT * FROM scope_amendments
 WHERE id = $1;

-- name: ListScopeAmendmentsByRun :many
-- Oldest first so the list endpoint and the fold both see requests
-- in submission order.
SELECT * FROM scope_amendments
 WHERE run_id = $1
 ORDER BY requested_at ASC, id ASC;

-- name: CountScopeAmendmentsByStage :one
-- Budget count is status-blind: a denied request still consumed an
-- operator interruption.
SELECT COUNT(*) FROM scope_amendments
 WHERE stage_id = $1;

-- name: DecideScopeAmendment :one
-- Pending-only guard in the WHERE clause makes the decision
-- transition atomic: a second decide matches zero rows and the
-- caller maps that to ErrAlreadyDecided (or ErrNotFound).
UPDATE scope_amendments
   SET status = $2,
       decision_reason = $3,
       decided_by = $4,
       decided_at = $5
 WHERE id = $1
   AND status = 'pending'
RETURNING *;
