-- Account / installation / membership queries for the tenancy identity tables
-- (ADR-057 / ADR-058, E44.1 / #1825). sqlc generates typed Go into ./db per the
-- `account` block in /backend/sqlc.yaml. Mirrors internal/campaign/queries.sql.
--
-- This child adds NO reader/writer wiring into the server — these queries stand
-- up the persistence surface later E44 children (endpoint resolution #1826,
-- authz #1829, RLS #1830) build on. sqlc is not regenerated locally
-- (established convention); the ./db/*.go files are hand-written to match sqlc's
-- output shape.

-- name: UpsertAccount :one
-- Idempotent create-or-update keyed on the forge-neutral natural key
-- (provider, account_key). The endpoint columns now live on installations
-- (Amendment A1), so accounts carries none.
INSERT INTO accounts (id, provider, account_key, display_name, granularity, home_region)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (provider, account_key) DO UPDATE
   SET display_name = EXCLUDED.display_name,
       granularity  = EXCLUDED.granularity,
       home_region  = EXCLUDED.home_region
RETURNING *;

-- name: GetAccount :one
SELECT * FROM accounts WHERE id = $1;

-- name: GetAccountByKey :one
SELECT * FROM accounts WHERE provider = $1 AND account_key = $2;

-- name: UpsertInstallation :one
-- Idempotent create-or-update keyed on (provider, installation_ref). Carries
-- the relocated forge_base_url / oauth_base_url endpoint columns (Amendment A1).
INSERT INTO installations (id, account_id, provider, installation_ref, forge_base_url, oauth_base_url)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (provider, installation_ref) DO UPDATE
   SET account_id     = EXCLUDED.account_id,
       forge_base_url = EXCLUDED.forge_base_url,
       oauth_base_url = EXCLUDED.oauth_base_url
RETURNING *;

-- name: GetInstallationByRef :one
SELECT * FROM installations WHERE provider = $1 AND installation_ref = $2;

-- name: UpsertAccountMember :one
-- Idempotent create-or-update keyed on (account_id, provider, member_ref).
INSERT INTO account_members (id, account_id, provider, member_ref, role)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (account_id, provider, member_ref) DO UPDATE
   SET role = EXCLUDED.role
RETURNING *;

-- name: ListAccountMembers :many
-- Insertion order (created_at ASC + id tiebreak) so a membership roster renders
-- stably.
SELECT * FROM account_members
 WHERE account_id = $1
 ORDER BY created_at ASC, id ASC;
