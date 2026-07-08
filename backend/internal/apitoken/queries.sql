-- API token queries (E4.5 / #51). sqlc generates typed Go into
-- ./db per backend/sqlc.yaml.

-- name: CreateToken :one
-- Static operator-token path. auth_method is left to the column
-- DEFAULT 'static' and provider stays NULL.
INSERT INTO api_tokens (id, subject, token_hash, scopes)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: CreateOAuthToken :one
-- OAuth device-flow path (E39.3 / #1708): stamp auth_method='oauth'
-- and the originating provider explicitly. The static Issue path
-- uses CreateToken and relies on the auth_method DEFAULT.
INSERT INTO api_tokens (id, subject, token_hash, scopes, auth_method, provider)
VALUES ($1, $2, $3, $4, 'oauth', $5)
RETURNING *;

-- name: GetTokenByHash :one
-- Active-only: revoked tokens never authenticate. Used on every
-- bearer-auth request, so the partial index on (token_hash) WHERE
-- revoked_at IS NULL is the hot path.
SELECT * FROM api_tokens
 WHERE token_hash = $1
   AND revoked_at IS NULL;

-- name: GetTokenByID :one
-- Returns the row regardless of revocation state — callers (the
-- DELETE handler) need to see revoked rows to return idempotent
-- 204s.
SELECT * FROM api_tokens
 WHERE id = $1;

-- name: TouchTokenLastUsed :exec
-- Stamp last_used_at on a successful auth. Best-effort; the auth
-- decision doesn't depend on this succeeding.
UPDATE api_tokens
   SET last_used_at = $2
 WHERE id = $1;

-- name: ListTokensForSubject :many
-- Active tokens for a user, newest first. Revoked rows are
-- filtered out by the partial index.
SELECT * FROM api_tokens
 WHERE subject = $1
   AND revoked_at IS NULL
 ORDER BY created_at DESC;

-- name: RevokeToken :one
-- Idempotent: if revoked_at is already set, return the existing
-- row unchanged. The handler checks ownership before calling.
UPDATE api_tokens
   SET revoked_at = COALESCE(revoked_at, $2)
 WHERE id = $1
RETURNING *;
