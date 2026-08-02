-- 0063: OAuth 2.1 authorization-server storage (E66.18 / #2433, ADR-076) —
-- the four tables #2391's HTTP front door persists through: CIMD-derived client
-- registrations, authorization codes, access tokens, refresh tokens.
--
-- Credentials are hashed at rest exactly as api_tokens are (0004): the row
-- stores a hex sha256 of the plaintext and NOTHING else, so a database
-- compromise surrenders no live credential. The plaintext is returned to the
-- caller exactly once, at mint.
--
-- Every table carries the forge-neutral `provider` discriminator (mirroring
-- 0052 accounts/installations and 0061 users) and a nullable `account_id` from
-- day one, so Mode 2 workspace issuance (ADR-057 / E44) needs no token-schema
-- migration later.
--
-- ---------------------------------------------------------------------------
-- ROW-LEVEL SECURITY: these four tables are created OUTSIDE RLS. DELIBERATE.
-- ---------------------------------------------------------------------------
-- 0057 (#1830, E44.6) enabled ENABLE + FORCE ROW LEVEL SECURITY with a
-- <table>_tenant_isolation policy on every account-scoped ROOT table. Silence
-- here would itself be a decision, so it is written down: no ENABLE, no FORCE,
-- no policy on oauth_clients / oauth_authorization_codes / oauth_access_tokens
-- / oauth_refresh_tokens. The reasoning, in four parts:
--
-- (a) The bootstrap read is PRE-IDENTITY BY CONSTRUCTION. POST /oauth/token
--     receives an authorization code and nothing else that names a tenant; the
--     code ROW is what carries account_id. 0057's predicate shape —
--       account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid
--     compares against the very value the read exists to DISCOVER. Under that
--     policy an unset GUC would make every tenanted code invisible to the only
--     query that needs it, and the token endpoint would fail closed for every
--     non-NULL-account code. A rejected #2391 plan applied tenant-style RLS
--     anyway; this header resolves that contradiction.
--
-- (b) These are credential-issuance tables on the AUTHENTICATION path — the
--     same class as users (0010), mcp_tokens and signing_keys, all of which
--     0057 deliberately leaves outside RLS while covering the account-scoped
--     ROOT entities. Applying the tenant-table treatment here would be the
--     same category error 0057's own exclusion argument already rejected.
--
-- (c) What isolation these tables DO have today, stated precisely — no more:
--       * Every CREDENTIAL read (GetAuthorizationCodeByHash, GetAccessTokenByHash,
--         GetRefreshTokenByHash and their FOR UPDATE siblings) is keyed on a
--         256-bit unguessable hash. Reaching another tenant's row requires
--         already holding that tenant's plaintext credential.
--       * Every ID-BASED operation (RevokeAccessToken / RevokeRefreshToken by
--         id, RevokeAccessTokensForCode / RevokeRefreshTokensForCode by code
--         id, LockAuthorizationCodeByID — the lineage lock — by code id)
--         carries NO database-level tenant check whatsoever. Those
--         operations are reachable only from an already-authenticated caller in
--         #2391, which is where the authorization decision belongs. That is an
--         APPLICATION-layer control, not a database one, and this header does
--         not claim otherwise. No shipped query in internal/oauthstore filters
--         on account_id.
--     account_id is present on all four tables from day one so a later child
--     can add a policy without a schema migration.
--
-- (d) The mechanism a later child would need is NAMED, so this is not an
--     open-ended punt. Either:
--       1. a BYPASSRLS system context for the single pre-identity code lookup —
--          the same system-context carve-out 0057's caveat says the
--          cross-account reconcilers will need; or
--       2. a policy whose USING clause permits an unset-GUC read of
--          oauth_authorization_codes ONLY, with SET LOCAL app.account_id issued
--          immediately after the code resolves.
--     Neither is urgent: 0057's policies are INERT in production today because
--     the runtime/migration role `fishhawk` is a superuser and superusers
--     bypass RLS even under FORCE (0057's own caveat).
--
-- The ABSENCE of policies is machine-enforced by
-- TestMigrateDown_OAuthASStorageReversal, which asserts pg_policies carries
-- zero rows for these four tables and pg_class.relrowsecurity is false for all
-- four — so a later accidental policy addition is caught, not assumed away.

-- oauth_clients: the persisted form of oauthas.ClientMetadata, one row per
-- (provider, client_id). client_id is the CIMD document URL.
--
-- REFRESH-ON-FETCH, not pin-at-first-use: UpsertClient OVERWRITES the metadata
-- columns on conflict. The CIMD document is authoritative and client-controlled
-- by design — a legitimate client that rotates its redirect URIs must not be
-- permanently broken by a first-use snapshot, and the fetcher's TTL bounds
-- staleness. Pinning would give only illusory protection: an attacker who can
-- rewrite the client's document already controls the client. first_seen_at
-- records when we first saw the client; updated_at moves on every refresh.
CREATE TABLE oauth_clients (
    id                         UUID         PRIMARY KEY,
    provider                   TEXT         NOT NULL DEFAULT 'github',
    client_id                  TEXT         NOT NULL,
    redirect_uris              TEXT[]       NOT NULL DEFAULT '{}',
    grant_types                TEXT[]       NOT NULL DEFAULT '{}',
    response_types             TEXT[]       NOT NULL DEFAULT '{}',
    token_endpoint_auth_method TEXT         NOT NULL,
    client_name                TEXT,
    client_uri                 TEXT,
    logo_uri                   TEXT,
    scope                      TEXT,
    account_id                 UUID         REFERENCES accounts (id) ON DELETE SET NULL,
    first_seen_at              TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at                 TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT oauth_clients_provider_check CHECK (provider IN ('github', 'gitlab')),
    CONSTRAINT oauth_clients_provider_client_id_key UNIQUE (provider, client_id)
);

-- oauth_authorization_codes: one row per issued authorization code.
--
-- client_id is stored as the STRING, not as a foreign key to oauth_clients.
-- RFC 6749 §4.1.3 requires the token request's client_id to equal the one the
-- code was issued to, and that comparison is byte-exact on the identifier — so
-- storing it directly is what makes the check possible without a join, and it
-- keeps a code verifiable after the client's CIMD row is later refreshed.
--
-- expires_at and consumed_at are BOTH present with DISTINCT predicates:
-- consumed_at is the single-use marker the conditional consume UPDATE keys on;
-- expires_at is the TTL the token endpoint checks. Neither substitutes for the
-- other — a code can lapse unconsumed, and a consumed code stays consumed past
-- its expiry.
--
-- The code_hash UNIQUE constraint's implicit btree index serves every code
-- lookup (all three are keyed on the hash), so no additional index is created.
CREATE TABLE oauth_authorization_codes (
    id                    UUID         PRIMARY KEY,
    code_hash             TEXT         NOT NULL UNIQUE,
    client_id             TEXT         NOT NULL,
    redirect_uri          TEXT         NOT NULL,
    code_challenge        TEXT         NOT NULL,
    code_challenge_method TEXT         NOT NULL,
    scopes                TEXT[]       NOT NULL DEFAULT '{}',
    resource              TEXT,
    subject               TEXT         NOT NULL,
    provider              TEXT         NOT NULL DEFAULT 'github',
    account_id            UUID         REFERENCES accounts (id) ON DELETE SET NULL,
    issued_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),
    expires_at            TIMESTAMPTZ  NOT NULL,
    consumed_at           TIMESTAMPTZ,
    CONSTRAINT oauth_authorization_codes_provider_check CHECK (provider IN ('github', 'gitlab')),
    -- Encodes oauthas's S256-only PKCE contract at the database: "plain" and
    -- every unknown method are refused by the schema, not only by the domain
    -- layer (oauthas.ParseCodeChallengeMethod).
    CONSTRAINT oauth_authorization_codes_challenge_method_check CHECK (code_challenge_method = 'S256')
);

-- oauth_access_tokens: bearer tokens minted against a redeemed code (or a
-- rotated refresh token).
--
-- authorization_code_id is NOT NULL — the lineage edge, and deliberately NOT
-- the nullable ON DELETE SET NULL shape. Every token in v0 descends from an
-- authorization code (the only mint paths are RedeemAuthorizationCode and
-- RotateRefreshToken), so a successor minted without its originating code id is
-- refused by the DATABASE rather than silently escaping the whole-lineage
-- revocation sweep RFC 6749 §4.1.2 mandates on refresh-token reuse. The trade
-- is named: a future grant type that mints tokens with no originating code
-- (client_credentials) needs a follow-up migration relaxing this column.
--
-- No partial unique index on (token_hash) WHERE revoked_at IS NULL: the
-- authentication lookup is deliberately UNFILTERED (the repository classifies
-- revoked → ErrRevoked rather than collapsing it into "not found"), so the
-- plain UNIQUE constraint's btree index is what the hot path uses and an
-- active-only partial index would be dead weight.
CREATE TABLE oauth_access_tokens (
    id                    UUID         PRIMARY KEY,
    token_hash            TEXT         NOT NULL UNIQUE,
    subject               TEXT         NOT NULL,
    client_id             TEXT         NOT NULL,
    audience              TEXT         NOT NULL,
    scopes                TEXT[]       NOT NULL DEFAULT '{}',
    provider              TEXT         NOT NULL DEFAULT 'github',
    account_id            UUID         REFERENCES accounts (id) ON DELETE SET NULL,
    authorization_code_id UUID         NOT NULL REFERENCES oauth_authorization_codes (id) ON DELETE CASCADE,
    issued_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),
    expires_at            TIMESTAMPTZ  NOT NULL,
    last_used_at          TIMESTAMPTZ,
    revoked_at            TIMESTAMPTZ,
    CONSTRAINT oauth_access_tokens_provider_check CHECK (provider IN ('github', 'gitlab'))
);

-- Serves the whole-lineage revocation sweep (RevokeAccessTokensForCode).
CREATE INDEX oauth_access_tokens_authorization_code_id_idx
    ON oauth_access_tokens (authorization_code_id);

-- oauth_refresh_tokens: the OAuth 2.1 rotating refresh-token chain.
--
-- authorization_code_id is NOT NULL here TOO — stated explicitly rather than
-- left to "the same column set as oauth_access_tokens", because the lineage
-- sweep depends on the column existing on BOTH tables. RotateRefreshToken
-- DERIVES it from the presented token's row (it is never taken from a caller),
-- so a successor can never be minted outside the sweep's reach.
--
-- replaced_by_id is genuinely nullable: the newest token in a chain has no
-- successor. consumed_at marks a token that has already been rotated — its
-- second presentation is the RFC 6749 §4.1.2 reuse signal.
CREATE TABLE oauth_refresh_tokens (
    id                    UUID         PRIMARY KEY,
    token_hash            TEXT         NOT NULL UNIQUE,
    subject               TEXT         NOT NULL,
    client_id             TEXT         NOT NULL,
    audience              TEXT         NOT NULL,
    scopes                TEXT[]       NOT NULL DEFAULT '{}',
    provider              TEXT         NOT NULL DEFAULT 'github',
    account_id            UUID         REFERENCES accounts (id) ON DELETE SET NULL,
    authorization_code_id UUID         NOT NULL REFERENCES oauth_authorization_codes (id) ON DELETE CASCADE,
    access_token_id       UUID         NOT NULL REFERENCES oauth_access_tokens (id) ON DELETE CASCADE,
    replaced_by_id        UUID         REFERENCES oauth_refresh_tokens (id) ON DELETE SET NULL,
    issued_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),
    expires_at            TIMESTAMPTZ  NOT NULL,
    last_used_at          TIMESTAMPTZ,
    consumed_at           TIMESTAMPTZ,
    revoked_at            TIMESTAMPTZ,
    CONSTRAINT oauth_refresh_tokens_provider_check CHECK (provider IN ('github', 'gitlab'))
);

-- Serves the whole-lineage revocation sweep (RevokeRefreshTokensForCode).
CREATE INDEX oauth_refresh_tokens_authorization_code_id_idx
    ON oauth_refresh_tokens (authorization_code_id);
