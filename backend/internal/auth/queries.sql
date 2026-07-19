-- Auth (users + sessions) queries (E4.2 / #49). sqlc generates
-- typed Go into ./db per backend/sqlc.yaml.

-- name: UpsertUser :one
-- Upsert keyed on github_user_id (stable across login renames).
-- Refreshes login + name + email on every sign-in.
INSERT INTO users (id, github_user_id, github_login, name, email)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (github_user_id) DO UPDATE
   SET github_login = EXCLUDED.github_login,
       name         = EXCLUDED.name,
       email        = EXCLUDED.email
RETURNING *;

-- name: GetUser :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserByGitHubID :one
SELECT * FROM users WHERE github_user_id = $1;

-- name: CreateSession :one
-- account_id is the workspace account the membership gate resolved at
-- sign-in (E44.3); NULL only where no gate ran.
INSERT INTO sessions (
    id, user_id, token_hash,
    sliding_expires_at, absolute_expires_at, account_id
)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetSessionByHash :one
-- Active-only: revoked or absolute-expired sessions never
-- authenticate. Sliding-expiry is checked at the application
-- layer (the row is read first, then validated against now()).
SELECT * FROM sessions
 WHERE token_hash = $1
   AND revoked_at IS NULL
   AND absolute_expires_at > now();

-- name: TouchSession :exec
-- Slide the sliding-expiry forward on every authenticated request.
-- The absolute_expires_at is fixed at issue time; this only moves
-- the sliding window.
UPDATE sessions
   SET last_used_at       = $2,
       sliding_expires_at = $3
 WHERE id = $1;

-- name: RevokeSessionByID :exec
-- Idempotent: a second call is a no-op (COALESCE).
UPDATE sessions
   SET revoked_at = COALESCE(revoked_at, $2)
 WHERE id = $1;

-- name: EvictExpiredSessions :execrows
-- Used by the eviction ticker. The partial index on
-- absolute_expires_at WHERE revoked_at IS NULL keeps this an
-- index range scan.
DELETE FROM sessions
 WHERE absolute_expires_at < $1;
