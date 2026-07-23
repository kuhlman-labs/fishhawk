-- Repo-ACL mirror queries (ADR-057 Amendment A2, E44.10 / #2071). sqlc
-- generates typed Go into ./db per the `repoacl` block in /backend/sqlc.yaml.
--
-- sqlc is not regenerated locally (established repo convention — it aborts on
-- an unrelated ambiguity and rewrites out-of-scope packages), so the ./db/*.go
-- files are hand-written to match sqlc's output shape. The Postgres round-trip
-- in postgres_test.go is what proves the SQL and the Go shapes agree.

-- name: GetRepoACLEntry :one
-- The read-path lookup, keyed on the natural key. pgx.ErrNoRows is a MISS,
-- which the Go layer resolves live rather than treating as a deny.
SELECT * FROM repo_acl_entries
 WHERE provider = $1
   AND subject = $2
   AND repo = $3;

-- name: UpsertRepoACLEntry :one
-- Memoize a permission the forge just reported. checked_at is refreshed on
-- EVERY upsert, including one whose permission is unchanged — the TTL clock
-- tracks "when did we last ask", not "when did the answer last change".
--
-- Only a resolved answer is ever written here. A forge ERROR must never reach
-- this statement: memoizing a transient fault would either cache a phantom
-- deny or, worse, hold a stale grant past its real revocation.
INSERT INTO repo_acl_entries (id, provider, subject, repo, permission)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (provider, subject, repo) DO UPDATE
   SET permission = EXCLUDED.permission,
       checked_at = now(),
       updated_at = now()
RETURNING *;

-- name: DeleteRepoACLEntriesForSubject :exec
-- The login purge: drop every mirrored entry for one identity so the next read
-- re-resolves against the forge. Served by
-- repo_acl_entries_provider_subject_idx.
DELETE FROM repo_acl_entries
 WHERE provider = $1
   AND subject = $2;
