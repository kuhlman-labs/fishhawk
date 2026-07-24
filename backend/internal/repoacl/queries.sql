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

-- name: EnsureRepoACLPurgeWatermark :one
-- Guarantee the watermark row for (provider, subject) EXISTS and return its
-- current generation. Called at resolution START (before the forge lookup) so
-- the memoizing write can later take a FOR SHARE lock on a row that is
-- guaranteed present — FOR SHARE on an ABSENT row locks nothing and silently
-- reopens the race, so this ensure-row step is mandatory.
--
-- The no-op DO UPDATE (assign generation to itself) is what makes RETURNING
-- yield the EXISTING row's generation on conflict; ON CONFLICT DO NOTHING would
-- return no row on an existing key. It takes a FOR NO KEY UPDATE-class write
-- lock and rewrites the row (dead-tuple churn on the read-only hot path) — the
-- price of guaranteeing a lockable row; see the README ordering-discipline
-- section.
INSERT INTO repo_acl_purge_watermarks (provider, subject)
VALUES ($1, $2)
ON CONFLICT (provider, subject) DO UPDATE
   SET generation = repo_acl_purge_watermarks.generation
RETURNING generation;

-- name: BumpRepoACLPurgeWatermark :exec
-- Raise the purge watermark for one identity. InvalidateSubject calls this
-- FIRST (strictly BEFORE deleting the subject's entry rows) so every in-flight
-- resolution that captured an older generation is invalidated across the entry
-- delete. Creates the row at generation 1 on the FIRST-EVER purge (a
-- never-purged subject) and increments monotonically thereafter. The UPDATE of
-- the non-key generation column takes a FOR NO KEY UPDATE row lock, which
-- CONFLICTS with the guarded upsert's FOR SHARE — that conflict is the
-- serialization the guard relies on.
INSERT INTO repo_acl_purge_watermarks (provider, subject, generation)
VALUES ($1, $2, 1)
ON CONFLICT (provider, subject) DO UPDATE
   SET generation = repo_acl_purge_watermarks.generation + 1,
       updated_at = now();

-- name: UpsertRepoACLEntryGuarded :one
-- Memoize a permission the forge just reported, GUARDED by the purge watermark
-- so a purge that raced this resolution rejects the write. checked_at is
-- refreshed on EVERY write, including one whose permission is unchanged — the
-- TTL clock tracks "when did we last ask", not "when did the answer last
-- change". Only a resolved answer is ever written here; a forge ERROR must never
-- reach this statement.
--
-- FOR SHARE OF w is LOAD-BEARING. It row-locks the watermark so the generation
-- read SERIALIZES against a concurrent BumpRepoACLPurgeWatermark: the read
-- BLOCKS until the bump commits (FOR SHARE conflicts with the bump's FOR NO KEY
-- UPDATE lock — NOT FOR KEY SHARE, which does NOT conflict with FOR NO KEY
-- UPDATE and would leave the read non-blocking and the race open), then
-- EvalPlanQual re-reads the BUMPED generation under READ COMMITTED. A captured
-- generation ($6) now behind the live one fails the WHERE ($6 >= w.generation is
-- false), the SELECT yields zero rows, and NOTHING is inserted. When the SELECT
-- yields zero rows ON CONFLICT never fires and RETURNING is empty — enforced
-- whether or not a conflicting entry row exists (the delete-surviving property).
-- A rejected write returns zero rows (pgx.ErrNoRows), which the Go layer treats
-- as a BENIGN non-memoized rejection, not an error.
INSERT INTO repo_acl_entries (id, provider, subject, repo, permission)
SELECT $1, $2, $3, $4, $5
  FROM repo_acl_purge_watermarks w
 WHERE w.provider = $2
   AND w.subject = $3
   AND $6 >= w.generation
   FOR SHARE OF w
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
