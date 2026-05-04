-- 0010: users + sessions for the OAuth sign-in flow (E4.2 / #49).
--
-- Per ADR-005, the Web UI uses HTTP-only secure cookie sessions
-- backed by an opaque server-side row. Bearer tokens (E4.5) cover
-- the CLI; sessions cover the browser. The auth middleware
-- recognizes both and resolves them to the same Identity shape.
--
-- The schema is deliberately narrow: the `users` table is what the
-- backend NEEDS for authorization (github_login, github_user_id,
-- name, email). Display preferences, OAuth scopes accepted, and
-- per-user settings can land in follow-up migrations as the UI
-- requires them.

CREATE TABLE users (
    id              UUID         PRIMARY KEY,
    github_user_id  BIGINT       NOT NULL UNIQUE,
    github_login    TEXT         NOT NULL,
    name            TEXT         NOT NULL,
    email           TEXT,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- Lookup-by-github-id is the OAuth callback's hot path.
CREATE INDEX users_github_login_idx ON users (lower(github_login));

CREATE TRIGGER users_set_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION fishhawk_set_updated_at();

-- Sessions are opaque cookie-backed records. Plaintext is in the
-- cookie; only sha256(plaintext) is stored, so a database
-- compromise doesn't surrender live cookies.
--
-- Lifetime semantics (per ADR-005):
--   - sliding: 24h after last use
--   - absolute: 7d after issuance
-- The auth middleware enforces both; expired rows are evicted by
-- a periodic ticker (similar pattern to webhook_deliveries).
CREATE TABLE sessions (
    id                 UUID         PRIMARY KEY,
    user_id            UUID         NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash         TEXT         NOT NULL UNIQUE,
    issued_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    last_used_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    sliding_expires_at TIMESTAMPTZ  NOT NULL,
    absolute_expires_at TIMESTAMPTZ NOT NULL,
    revoked_at         TIMESTAMPTZ
);

-- Hot path: every authenticated request looks up by token_hash.
-- Active rows only (partial index) keeps the index small as
-- expired rows accumulate before eviction.
CREATE INDEX sessions_token_hash_active_idx
    ON sessions (token_hash)
    WHERE revoked_at IS NULL;

CREATE INDEX sessions_user_active_idx
    ON sessions (user_id)
    WHERE revoked_at IS NULL;

-- The eviction ticker queries by absolute_expires_at to find rows
-- to drop. Index over the active set keeps the scan narrow.
CREATE INDEX sessions_absolute_expires_at_idx
    ON sessions (absolute_expires_at)
    WHERE revoked_at IS NULL;
