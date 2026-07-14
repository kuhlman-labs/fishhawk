-- 0052: accounts and installations tables — the ADR-057 tenancy foundation,
-- shipped forge-neutral by construction per ADR-058 (#1823, #1851, #1854).
--
-- These are the first two tenancy-scoped tables. They ship carrying a forge
-- `provider` discriminator AT BIRTH so GitLab (ADR-058) becomes an ADDITIVE
-- provider, never a constraint migration that has to retrofit a GitHub-shaped
-- schema. This is the binding contract every downstream E44 child inherits:
-- every future tenancy-scoped table, every RLS predicate, and every per-account
-- audit-chain key composes with `provider`. An account's identity is the
-- forge-neutral natural key (provider, account_key); an installation's provider
-- is structurally pinned to its account's via a composite FK, so the two can
-- never diverge.
--
-- Endpoint configuration is deliberately forge-neutral (forge_base_url /
-- oauth_base_url — the E44.2 #1826 per-account endpoint columns), NEVER named
-- github_base_url, so a GitLab (or self-managed) account slots in with no rename.
-- NULL means "use the provider default endpoints" (api.github.com / github.com
-- today).
--
-- Zero behavioral change: no code reads or writes these tables yet — E44.1
-- (#1825) extends them via additive ALTERs (account_members, account_id columns)
-- instead of creating them GitHub-shaped.
--
-- Schema mirrors 0001_runs_stages.up.sql / 0039_campaigns.up.sql: UUID PKs, TEXT
-- columns with named CHECK constraints, now()-defaulted TIMESTAMPTZ, and BEFORE
-- UPDATE triggers reusing the shared fishhawk_set_updated_at() function defined
-- in 0001 (NOT redefined here).

CREATE TABLE accounts (
    id             UUID         PRIMARY KEY,
    provider       TEXT         NOT NULL DEFAULT 'github',
    account_key    TEXT         NOT NULL,
    display_name   TEXT,
    granularity    TEXT         NOT NULL DEFAULT 'enterprise',
    home_region    TEXT,
    forge_base_url TEXT,
    oauth_base_url TEXT,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT accounts_provider_check CHECK (
        provider IN ('github', 'gitlab')
    ),
    CONSTRAINT accounts_granularity_check CHECK (
        granularity IN ('enterprise', 'organization', 'group')
    ),
    -- Forge-neutral natural key: GitHub enterprise slug / org login today,
    -- GitLab group path later. Scoped by provider so the same key string in two
    -- forges never collides.
    UNIQUE (provider, account_key),
    -- Anchors the installations composite FK so an installation's provider
    -- structurally matches its account's.
    UNIQUE (id, provider)
);

CREATE TABLE installations (
    id               UUID         PRIMARY KEY,
    account_id       UUID         NOT NULL,
    provider         TEXT         NOT NULL DEFAULT 'github',
    -- Forge-neutral credential-scope key: the stringified GitHub App
    -- installation id today, a GitLab group OAuth-application authorization ref
    -- later. TEXT, not BIGINT (ADR-058 scope decision 2) — GitLab's scope is not
    -- an int64; GitHub installation ids stringify losslessly.
    installation_ref TEXT         NOT NULL,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT installations_provider_check CHECK (
        provider IN ('github', 'gitlab')
    ),
    UNIQUE (provider, installation_ref),
    -- Composite FK: an installation's (account_id, provider) must reference an
    -- existing account with the SAME provider, so an installation can never be
    -- attached to an account of a different forge.
    FOREIGN KEY (account_id, provider) REFERENCES accounts (id, provider) ON DELETE CASCADE
);

CREATE INDEX installations_account_idx ON installations (account_id);

CREATE TRIGGER accounts_set_updated_at      BEFORE UPDATE ON accounts      FOR EACH ROW EXECUTE FUNCTION fishhawk_set_updated_at();
CREATE TRIGGER installations_set_updated_at BEFORE UPDATE ON installations FOR EACH ROW EXECUTE FUNCTION fishhawk_set_updated_at();
