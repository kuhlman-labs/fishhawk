-- MCP token queries (E19.8 / #348). sqlc generates typed Go into
-- ./db per backend/sqlc.yaml.

-- name: CreateMCPToken :one
INSERT INTO mcp_tokens (id, run_id, token_hash, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetMCPTokenByHash :one
-- Active-only: revoked tokens never authenticate. Expiry is
-- enforced row-level by the caller (the auth path needs to
-- differentiate ErrExpired from ErrNotFound for observability).
SELECT * FROM mcp_tokens
 WHERE token_hash = $1
   AND revoked_at IS NULL;

-- name: GetMCPTokenByID :one
-- Returns the row regardless of revocation / expiry state so
-- handlers can return idempotent 204s on revoke.
SELECT * FROM mcp_tokens
 WHERE id = $1;

-- name: TouchMCPTokenLastUsed :exec
-- Stamp last_used_at on successful auth. Best-effort; the auth
-- decision doesn't depend on this succeeding.
UPDATE mcp_tokens
   SET last_used_at = $2
 WHERE id = $1;

-- name: RevokeMCPToken :one
-- Idempotent: if revoked_at is already set, return the existing
-- row unchanged.
UPDATE mcp_tokens
   SET revoked_at = COALESCE(revoked_at, $2)
 WHERE id = $1
RETURNING *;

-- name: RevokeMCPTokensForRun :execrows
-- Revoke every active token for the run. Returns the count of
-- newly-revoked rows (already-revoked rows aren't double-counted
-- because of the partial-index-friendly WHERE clause).
UPDATE mcp_tokens
   SET revoked_at = $2
 WHERE run_id = $1
   AND revoked_at IS NULL;
