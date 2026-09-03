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

-- name: UpsertSingleTenantAccount :one
-- The single-tenant deployment profile's boot-time bootstrap (E44.9 / #1833).
-- Distinct from UpsertAccount above because it is the ONLY writer that sets
-- auto_join_role: the implicit account a self-hosted install admits through
-- must carry a non-NULL role or ListAutoJoinAccountsByKeys cannot see it and
-- the login gate admits nobody.
--
-- home_region is deliberately absent from BOTH the insert column list and the
-- DO UPDATE SET: PinAccountHomeRegion owns that column (first-write-wins), and
-- a boot-time upsert re-running on every restart must never clear a pin.
--
-- The RETURNING list is EXPLICIT (not `*`) and held to the eight columns the
-- hand-written db.Account model carries — auto_join_role (migration 0056) is
-- written but not scanned back.
INSERT INTO accounts (id, provider, account_key, display_name, granularity, auto_join_role)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (provider, account_key) DO UPDATE
   SET display_name   = EXCLUDED.display_name,
       granularity    = EXCLUDED.granularity,
       auto_join_role = EXCLUDED.auto_join_role,
       updated_at     = now()
RETURNING id, provider, account_key, display_name, granularity, home_region, created_at, updated_at;

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

-- name: ListAccounts :many
-- The operator inventory read behind `fishhawkd account list` (E45.33 / #2923):
-- every registered account in stable (provider, account_key) order. No filter —
-- the CLI filters by --provider in Go so one query serves both the unfiltered
-- and provider-scoped forms. Supports the auth-change checklist's impact
-- inventory: list what exists before tightening anything.
SELECT * FROM accounts ORDER BY provider ASC, account_key ASC;

-- name: PinAccountHomeRegion :one
-- The cell-side region pin (ADR-062 A2.3, E44.7 / #1831). First-write-wins is
-- enforced HERE, in SQL, rather than as a check-then-act read/write pair in Go:
-- the WHERE clause matches only a row that is unpinned or ALREADY pinned to the
-- same region, so two concurrent pins proposing different regions serialize on
-- the row lock and exactly one can match.
--
-- The statement is UPDATE-only ON PURPOSE — it must never create an account. A
-- handoff naming an account this cell has never heard of matches no row and is
-- refused, not silently materialized (ADR-062 A2.5).
--
-- Zero rows is therefore ambiguous by design and the Go layer disambiguates:
-- either the account does not exist here, or it is already pinned to a
-- DIFFERENT region. Both are refusals; neither is an overwrite.
UPDATE accounts
   SET home_region = $3,
       updated_at  = now()
 WHERE provider = $1
   AND account_key = $2
   AND (home_region IS NULL OR home_region = $3)
RETURNING *;

-- name: UpsertInstallation :one
-- Idempotent create-or-update keyed on (provider, installation_ref). Carries
-- the relocated forge_base_url / oauth_base_url endpoint columns (Amendment A1)
-- and, since 0078 (E45.26 / #2877), project_path — the GitLab
-- path_with_namespace the run-creation authorization gate binds EXACTLY.
-- project_path is in the DO UPDATE SET so re-registering an installation
-- REPAIRS an unbound row in place rather than duplicating it, which is the
-- documented upgrade and rollback remedy.
INSERT INTO installations (id, account_id, provider, installation_ref, forge_base_url, oauth_base_url, project_path)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (provider, installation_ref) DO UPDATE
   SET account_id     = EXCLUDED.account_id,
       forge_base_url = EXCLUDED.forge_base_url,
       oauth_base_url = EXCLUDED.oauth_base_url,
       project_path   = EXCLUDED.project_path
RETURNING *;

-- name: GetInstallationByRef :one
SELECT * FROM installations WHERE provider = $1 AND installation_ref = $2;

-- name: ListInstallations :many
-- The operator inventory read behind `fishhawkd installation list` (E45.33 /
-- #2923): every registered installation JOINed with its owning account's
-- account_key, so the operator's real question — which project is registered
-- under which namespace — is answered in one row. EXPLICIT column list (not
-- i.*) so the joined account_key is carried and the scan order is pinned.
-- project_path (0078, E45.26 / #2877) is carried so the CLI can mark a gitlab
-- row whose exact-path binding is UNBOUND — the mechanism behind the documented
-- upgrade / rollback remedy. Supports the auth-change checklist's
-- impact-inventory habit: list what exists before tightening anything.
SELECT i.id, i.account_id, i.provider, i.installation_ref, i.forge_base_url, i.oauth_base_url, i.project_path, i.created_at, i.updated_at, a.account_key
  FROM installations i
  JOIN accounts a ON a.id = i.account_id
 ORDER BY i.provider ASC, i.installation_ref ASC;

-- name: UpsertAccountMember :one
-- Idempotent create-or-update keyed on (account_id, provider, member_ref).
INSERT INTO account_members (id, account_id, provider, member_ref, role)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (account_id, provider, member_ref) DO UPDATE
   SET role = EXCLUDED.role
RETURNING *;

-- name: UpsertInvitedAccountMember :one
-- The operator invite write behind `fishhawkd member invite` (E44.34 / #2924).
-- Idempotent create-or-update keyed on (account_id, provider, member_ref), the
-- unnamed UNIQUE constraint on account_members (migration 0055) — inferred here
-- by its column list.
--
-- Distinct from UpsertAccountMember (no origin, pre-0056 shape) and from
-- UpsertAccountMemberWithOrigin (:exec, no RETURNING, owned by the login gate's
-- auto_join write): the CLI must PRINT the row it wrote (hence :one + explicit
-- RETURNING) AND must pin origin to the literal 'invited' rather than accept it
-- as a parameter — an operator verb that could mint an auto_join grant would
-- fabricate a forge-derived provenance no forge ever asserted, and the login
-- gate re-verifies auto_join grants against the live forge, silently revoking
-- such a row later.
--
-- The origin in the DO UPDATE is DELIBERATE: an explicit operator invite
-- OVERWRITES a prior auto_join grant, upgrading that member to DB-only,
-- forge-independent admission.
INSERT INTO account_members (id, account_id, provider, member_ref, role, origin)
VALUES ($1, $2, $3, $4, $5, 'invited')
ON CONFLICT (account_id, provider, member_ref) DO UPDATE
   SET role   = EXCLUDED.role,
       origin = EXCLUDED.origin
RETURNING id, account_id, provider, member_ref, role, origin, created_at, updated_at;

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

-- name: ListAccountMembersWithAccountKey :many
-- The operator inventory read behind `fishhawkd member list` (E44.34 / #2924):
-- every membership grant JOINed with its owning account's account_key, so the
-- operator's real question — who is admitted to which account, and via which
-- origin — is answered in one row. EXPLICIT column list (not m.*) so the joined
-- account_key is carried and the scan order is pinned; origin is surfaced so an
-- invited grant (admits DB-only) is distinguishable from a login-minted
-- auto_join one. No filter — the CLI filters by --provider / --account-key in Go
-- so one query serves the unfiltered and scoped forms, exactly as
-- `installation list` does.
SELECT m.id, m.account_id, m.provider, m.member_ref, m.role, m.origin, m.created_at, a.account_key
  FROM account_members m
  JOIN accounts a ON a.id = m.account_id
 ORDER BY a.provider ASC, a.account_key ASC, m.created_at ASC, m.id ASC;

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
-- Auto-join bootstrap intersection (E44.3, generalized in E44.8): the
-- organization / enterprise / group-granularity accounts whose auto_join_role
-- policy is set and whose (account_key, granularity) PAIR appears in the
-- membership set the resolver derived for this login.
--
-- The two arrays are POSITIONALLY PAIRED via unnest — never two independent
-- ANY() predicates. Their cartesian product would admit across granularities
-- (a live GitHub org key "acme" admitting an ENTERPRISE account keyed "acme",
-- or a derived enterprise short code admitting an organization account of the
-- same key), which is unauthorized admission. Each key stays bound to the
-- granularity it was derived from.
--
-- Stable (account_key, granularity) order keeps the callback's
-- deterministic-first pick reproducible.
SELECT a.id, a.account_key, a.granularity, a.auto_join_role
  FROM accounts a
  JOIN unnest(sqlc.arg(account_keys)::text[], sqlc.arg(granularities)::text[])
       AS p(account_key, granularity)
    ON a.account_key = p.account_key
   AND a.granularity = p.granularity
 WHERE a.provider = $1
   AND a.auto_join_role IS NOT NULL
 ORDER BY a.account_key ASC, a.granularity ASC;

-- name: UpsertAccountMemberWithOrigin :exec
-- Mint (or refresh) a grant with an explicit origin — the auto-join bootstrap's
-- audited origin='auto_join' write. UpsertAccountMember above keeps its
-- pre-0056 shape (origin defaults to 'invited').
INSERT INTO account_members (id, account_id, provider, member_ref, role, origin)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (account_id, provider, member_ref) DO UPDATE
   SET role   = EXCLUDED.role,
       origin = EXCLUDED.origin;
