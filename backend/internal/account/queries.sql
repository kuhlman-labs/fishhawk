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

-- name: ListAccountsByAccountKey :many
-- The provider-discriminator lookup for the per-repo conventions loader
-- (E45.16 / #2022): keyed by account_key ALONE — the provider is exactly what
-- the caller is resolving. UNIQUE(provider, account_key) permits the same key
-- under both providers, so this can return more than one row; the resolver
-- treats that as ambiguous (found=false), never an arbitrary first row. Stable
-- provider order keeps the multi-row result deterministic anyway.
SELECT * FROM accounts
 WHERE account_key = $1
 ORDER BY provider ASC;

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

-- name: GetAccountMemberRole :one
-- The handler-authz role lookup (E44.5 / #1829): the caller's role in an
-- account, keyed on the forge-neutral (account_id, provider, member_ref).
-- role is nullable — a grant with no explicit role scans as NULL, which the
-- Go layer interprets as member-tier (least privilege). No row (no membership)
-- surfaces as pgx.ErrNoRows, also member-tier at the caller.
SELECT role FROM account_members
 WHERE account_id = $1
   AND provider = $2
   AND member_ref = $3;

-- name: ListAccountMembers :many
-- Insertion order (created_at ASC + id tiebreak) so a membership roster renders
-- stably.
SELECT * FROM account_members
 WHERE account_id = $1
 ORDER BY created_at ASC, id ASC;

-- name: ListMemberGrantsByRef :many
-- The login-gate admission read (E44.3 / ADR-057 Amendment A2): every grant for
-- a forge member joined with its account's admission fields, so the resolver
-- can admit invited rows DB-only and re-verify auto_join rows against their
-- policy predicate. Explicit columns (a per-query Row struct) — the resolver
-- needs only the admission slice, not the whole roster row.
SELECT m.account_id, m.origin, a.account_key, a.granularity, a.auto_join_role
  FROM account_members m
  JOIN accounts a ON a.id = m.account_id
 WHERE m.provider = $1
   AND m.member_ref = $2
 ORDER BY m.created_at ASC, m.id ASC;

-- name: ListAutoJoinAccountsByKeys :many
-- Auto-join bootstrap intersection (E44.3): organization-granularity accounts
-- whose auto_join_role policy is set and whose org key appears in the user's
-- LIVE forge org list. Stable account_key order keeps the callback's
-- deterministic-first pick reproducible.
SELECT id, account_key, auto_join_role
  FROM accounts
 WHERE provider = $1
   AND granularity = 'organization'
   AND auto_join_role IS NOT NULL
   AND account_key = ANY(sqlc.arg(account_keys)::text[])
 ORDER BY account_key ASC;

-- name: UpsertAccountMemberWithOrigin :exec
-- Mint (or refresh) a grant with an explicit origin — the auto-join bootstrap's
-- audited origin='auto_join' write. UpsertAccountMember above keeps its
-- pre-0056 shape (origin defaults to 'invited').
INSERT INTO account_members (id, account_id, provider, member_ref, role, origin)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (account_id, provider, member_ref) DO UPDATE
   SET role   = EXCLUDED.role,
       origin = EXCLUDED.origin;
