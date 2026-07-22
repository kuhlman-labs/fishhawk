-- 0001: the global directory's entire schema (ADR-062, E44.7 / #1831).
--
-- This database is deliberately tiny and lives in the GLOBAL plane, so it
-- must hold no customer data — only the routing facts needed to send a
-- browser at the right regional cell.

-- account_regions is the single source of truth for an account's home
-- region. There is intentionally NO cell_base_url column: region → cell
-- base URL resolves EXCLUSIVELY from the directory's env config, so a
-- cell can be re-pointed by redeploying config without a data migration
-- and there is exactly one place a cell endpoint is defined.
CREATE TABLE account_regions (
    provider     TEXT        NOT NULL,
    account_key  TEXT        NOT NULL,
    home_region  TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, account_key),
    CONSTRAINT account_regions_home_region_nonempty CHECK (home_region <> '')
);

-- install_states holds the single-use nonce minted when onboarding starts
-- and consumed when the forge's App-install callback returns, binding the
-- callback to the account the directory already assigned. Rows are
-- deleted on consumption; the expires_at index supports periodic pruning
-- of abandoned onboardings.
CREATE TABLE install_states (
    nonce        TEXT        PRIMARY KEY,
    provider     TEXT        NOT NULL,
    account_key  TEXT        NOT NULL,
    home_region  TEXT        NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX install_states_expires_at_idx ON install_states (expires_at);
