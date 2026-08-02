-- OAuth 2.1 authorization-server storage queries (E66.18 / #2433, ADR-076).
-- sqlc generates typed Go into ./db per backend/sqlc.yaml. Schema:
-- internal/postgres/migrations/0063_oauth_as_storage.up.sql.
--
-- Every query below is keyed either on a 256-bit credential hash or on a row
-- id. NONE filters on account_id — see 0063's header part (c): ID-based
-- operations carry no database-level tenant check today; that authorization
-- decision belongs to the already-authenticated caller in #2391.

-- name: UpsertClient :one
-- REFRESH-ON-FETCH (not pin-at-first-use): a re-fetched CIMD document
-- OVERWRITES the persisted metadata columns. The document is authoritative and
-- client-controlled by design, so a client that rotates its redirect URIs must
-- not be permanently broken by a first-use snapshot; the fetcher's TTL bounds
-- staleness. first_seen_at is preserved by the DO UPDATE (it is not in the SET
-- list); updated_at moves on every refresh.
--
-- account_id is the ONE column the refresh-on-fetch rationale does NOT cover,
-- so it is treated differently and the decision is written down. It is a tenant
-- discriminator, not CIMD document metadata: nothing in the client-controlled
-- document determines it, so "the document is authoritative" says nothing about
-- it. COALESCE makes the assignment STICKY — an established tenant survives
-- every later refresh, whatever account context that fetch runs in, while a row
-- first seen untenanted can still be adopted once. A registration can therefore
-- never be MOVED between tenants by a refresh. That is inert today (no shipped
-- query filters on oauth_clients.account_id) and is the point: when a later
-- child adds the RLS policy 0063's header anticipates, the behaviour is already
-- pinned by TestUpsertClient_AccountIDIsStickyAcrossRefreshes rather than
-- silently becoming a cross-tenant move.
INSERT INTO oauth_clients (
    id, provider, client_id, redirect_uris, grant_types, response_types,
    token_endpoint_auth_method, client_name, client_uri, logo_uri, scope, account_id
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (provider, client_id) DO UPDATE
   SET redirect_uris              = EXCLUDED.redirect_uris,
       grant_types                = EXCLUDED.grant_types,
       response_types             = EXCLUDED.response_types,
       token_endpoint_auth_method = EXCLUDED.token_endpoint_auth_method,
       client_name                = EXCLUDED.client_name,
       client_uri                 = EXCLUDED.client_uri,
       logo_uri                   = EXCLUDED.logo_uri,
       scope                      = EXCLUDED.scope,
       account_id                 = COALESCE(oauth_clients.account_id, EXCLUDED.account_id),
       updated_at                 = now()
RETURNING *;

-- name: GetClientByClientID :one
-- Keyed on the forge-scoped composite, so the same CIMD URL under two
-- providers is two distinct registrations.
SELECT * FROM oauth_clients
 WHERE provider = $1
   AND client_id = $2;

-- name: CreateAuthorizationCode :one
-- code_hash is the hex sha256 of the plaintext; the plaintext itself is never
-- persisted and is returned to the caller exactly once, here.
INSERT INTO oauth_authorization_codes (
    id, code_hash, client_id, redirect_uri, code_challenge, code_challenge_method,
    scopes, resource, subject, provider, account_id, expires_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING *;

-- name: GetAuthorizationCodeByHash :one
-- READ-ONLY load for verify-before-consume: #2391's token endpoint can
-- validate client_id, redirect_uri, PKCE and resource and render a typed
-- oauthas error WITHOUT burning the grant. Touches consumed_at never.
-- Unfiltered on consumed_at/expires_at so the caller sees the real state.
SELECT * FROM oauth_authorization_codes
 WHERE code_hash = $1;

-- name: LockAuthorizationCodeByHash :one
-- The in-transaction load for RedeemAuthorizationCode. FOR UPDATE serializes
-- concurrent redemptions of the same code: under READ COMMITTED the loser
-- blocks until the winner commits and then re-evaluates the row at its new
-- version, so it observes consumed_at as SET rather than the NULL its pre-lock
-- snapshot held.
SELECT * FROM oauth_authorization_codes
 WHERE code_hash = $1
   FOR UPDATE;

-- name: LockAuthorizationCodeByID :exec
-- THE LINEAGE LOCK. The authorization-code row is the mutex for its WHOLE
-- lineage: RotateRefreshToken and RevokeGrantsForCode both take it FOR UPDATE
-- before reading or mutating any descendant, so every rotation and every sweep
-- of one lineage is serialized.
--
-- Without it the whole-lineage revocation guarantee does NOT hold under
-- concurrency. Row locks on the individual token rows are not enough: the sweep
-- UPDATEs (`WHERE authorization_code_id = $1`) take their statement snapshot
-- when the statement starts, so a rotation that INSERTs a successor pair and
-- commits after that instant produces rows the sweep can never see — a usable
-- descendant credential surviving reuse detection. Serializing on the code row
-- forces one of two safe orders instead: the rotation commits first and the
-- sweep's later snapshot includes its successor, or the sweep commits first and
-- the rotation's post-lock re-read observes revoked_at set and mints nothing.
--
-- Selecting only `id` keeps this a pure lock acquisition. A missing row locks
-- nothing and is NOT an error (RevokeGrantsForCode on an unknown code id must
-- still revoke zero rows rather than fail), which is why this is :exec.
SELECT id FROM oauth_authorization_codes
 WHERE id = $1
   FOR UPDATE;

-- name: ConsumeAuthorizationCode :one
-- THE LOAD-BEARING SINGLE-USE CONTROL. `AND consumed_at IS NULL` is what makes
-- an authorization code single-use; it still holds if a future caller reaches
-- this UPDATE without taking the row lock first. A non-matching predicate
-- affects zero rows, which surfaces as pgx.ErrNoRows on this :one and is
-- mapped to ErrCodeConsumed.
--
-- Pinned DIRECTLY, at this level, by
-- TestConsumeAuthorizationCode_PredicateRejectsConsumedRow — which issues this
-- query against an already-consumed row on a plain pool connection, with no
-- lock and no repository-level check in front of it, so deleting the predicate
-- reddens that test. The post-lock consumed check in RedeemAuthorizationCode is
-- the REDUNDANT short-circuit, not this.
UPDATE oauth_authorization_codes
   SET consumed_at = $2
 WHERE id = $1
   AND consumed_at IS NULL
RETURNING *;

-- name: CreateAccessToken :one
-- authorization_code_id is NOT NULL at the schema: every v0 token descends from
-- an authorization code, which is what makes the whole-lineage revocation sweep
-- reachable. The repository DERIVES it — and every authority column below it —
-- from a row it already owns on BOTH mint paths: the consumed authorization code
-- at redemption, the presented refresh token at rotation. None of these values
-- is ever taken from a caller's request.
INSERT INTO oauth_access_tokens (
    id, token_hash, subject, client_id, audience, scopes, provider, account_id,
    authorization_code_id, expires_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetAccessTokenByHash :one
-- UNFILTERED ON revoked_at, DELIBERATELY. The absence of `AND revoked_at IS
-- NULL` is the control: it lets the repository classify revoked → ErrRevoked
-- instead of collapsing revocation into "no such token". A future
-- "optimization" that re-adds that filter is a REGRESSION, not a speedup — the
-- token_hash UNIQUE constraint's btree index serves this lookup either way.
-- TestAuthenticateAccessToken_RevokedExpiredUnknownAreDistinct fails if the
-- three outcomes collapse.
SELECT * FROM oauth_access_tokens
 WHERE token_hash = $1;

-- name: TouchAccessTokenLastUsed :exec
-- Best-effort stamp on a successful authentication; a failure here does not
-- invalidate the auth decision.
UPDATE oauth_access_tokens
   SET last_used_at = $2
 WHERE id = $1;

-- name: RevokeAccessToken :one
-- Idempotent via COALESCE (mirroring apitoken's RevokeToken): a second revoke
-- returns the existing row with its original revoked_at.
UPDATE oauth_access_tokens
   SET revoked_at = COALESCE(revoked_at, $2)
 WHERE id = $1
RETURNING *;

-- name: RevokeAccessTokensForCode :execrows
-- Half of the RFC 6749 §4.1.2 replay response: revoke every still-active access
-- token descended from one authorization code. Served by
-- oauth_access_tokens_authorization_code_id_idx.
UPDATE oauth_access_tokens
   SET revoked_at = $2
 WHERE authorization_code_id = $1
   AND revoked_at IS NULL;

-- name: CreateRefreshToken :one
-- access_token_id pins the sibling access token minted in the same grant;
-- authorization_code_id carries the lineage (NOT NULL, derived by the store —
-- as are subject, client_id, audience, scopes, provider and account_id).
INSERT INTO oauth_refresh_tokens (
    id, token_hash, subject, client_id, audience, scopes, provider, account_id,
    authorization_code_id, access_token_id, expires_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: GetRefreshTokenByHash :one
-- UNFILTERED for the same reason as GetAccessTokenByHash: the rotation path
-- must distinguish revoked from consumed from nonexistent, and a filter here
-- would erase the reuse signal entirely.
SELECT * FROM oauth_refresh_tokens
 WHERE token_hash = $1;

-- name: LockRefreshTokenByHash :one
-- The in-transaction load for RotateRefreshToken. Holding this lock is what
-- keeps the reuse-detection sweep atomic with the classification that triggers
-- it.
SELECT * FROM oauth_refresh_tokens
 WHERE token_hash = $1
   FOR UPDATE;

-- name: ConsumeRefreshToken :one
-- Same load-bearing conditional shape as ConsumeAuthorizationCode: a rotated
-- token can be consumed exactly once, and the second consume affects zero rows
-- regardless of what checks ran before it. replaced_by_id records the successor,
-- forming the OAuth 2.1 rotation chain.
UPDATE oauth_refresh_tokens
   SET consumed_at = $2,
       replaced_by_id = $3
 WHERE id = $1
   AND consumed_at IS NULL
RETURNING *;

-- name: RevokeRefreshToken :one
-- Idempotent via COALESCE, as RevokeAccessToken.
UPDATE oauth_refresh_tokens
   SET revoked_at = COALESCE(revoked_at, $2)
 WHERE id = $1
RETURNING *;

-- name: RevokeRefreshTokensForCode :execrows
-- The other half of the §4.1.2 replay response. Served by
-- oauth_refresh_tokens_authorization_code_id_idx.
UPDATE oauth_refresh_tokens
   SET revoked_at = $2
 WHERE authorization_code_id = $1
   AND revoked_at IS NULL;
