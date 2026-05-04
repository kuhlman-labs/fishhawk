-- 0008: scoped API tokens for CLI / UI auth (E4.5 / #51).
--
-- Tokens are minted by the backend and attached to a user
-- identity (subject). The plaintext is shown to the caller exactly
-- once at issue time; we store only its sha256 hash so a database
-- compromise doesn't surrender live credentials.
--
-- Token format: "fhk_" prefix + 32 random bytes URL-safe base64
-- (~45 chars). Easy to grep for in customer logs / .env files,
-- easy to revoke if leaked.
--
-- Subject format: opaque to this layer. Production uses
-- "github:<id>" (numeric GitHub user id, stable across renames);
-- the bootstrap CLI command can use any subject the operator
-- supplies.
--
-- Scopes are an array of strings. Enforcement (which endpoints
-- require which scopes) lands in a follow-up; the column exists
-- now so issuance + audit can record the intended scope without
-- a schema change.

CREATE TABLE api_tokens (
    id            UUID         PRIMARY KEY,
    subject       TEXT         NOT NULL,
    token_hash    TEXT         NOT NULL UNIQUE,
    scopes        TEXT[]       NOT NULL DEFAULT '{}',
    last_used_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    revoked_at    TIMESTAMPTZ
);

-- Lookup-by-hash hits this index on every authenticated request.
-- Partial index over non-revoked rows keeps it small as the table
-- grows over time.
CREATE INDEX api_tokens_hash_active_idx
    ON api_tokens (token_hash)
    WHERE revoked_at IS NULL;

-- ListForSubject hits this for the per-user "my tokens" view.
CREATE INDEX api_tokens_subject_active_idx
    ON api_tokens (subject)
    WHERE revoked_at IS NULL;
