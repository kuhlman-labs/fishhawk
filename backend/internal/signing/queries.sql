-- Signing-key queries consumed by the postgres adapter for the
-- signing.Repository interface (E2.3 / #24). Append-only — no Update
-- or Delete queries; the schema's triggers backstop that.
--
-- Migration 0012 dropped the (run_id) PRIMARY KEY constraint so
-- multiple keys can exist per run (one per Issue call from a fresh
-- Actions-runner process). GetLatestSigningKey returns the most
-- recently issued key — Verify uses this so the runner doesn't have
-- to track which key signed which payload across stages.

-- name: IssueSigningKey :one
INSERT INTO signing_keys (run_id, public_key, issued_at, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetLatestSigningKey :one
SELECT * FROM signing_keys
WHERE run_id = $1
ORDER BY issued_at DESC
LIMIT 1;

-- name: ListSigningKeysForRun :many
-- Used by the standalone verifier to walk every key that was
-- active during a run's lifetime, since a single run may have
-- shipped traces signed by different keys at different stages.
SELECT * FROM signing_keys
WHERE run_id = $1
ORDER BY issued_at ASC;
