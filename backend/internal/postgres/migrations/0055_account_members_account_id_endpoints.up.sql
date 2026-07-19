-- 0055: E44.1 tenancy schema — account_members membership grants, the
-- account_id foreign key threaded through every root entity, and the
-- Amendment A1 relocation of the per-forge endpoint columns from accounts to
-- installations (ADR-057 / ADR-058, #1825).
--
-- This is purely additive at the schema level (nullable columns, a new table)
-- except the endpoint-column relocation, which is fully reversible: 0052 put
-- forge_base_url / oauth_base_url on accounts with NO reader (no production
-- code reads them — the only consumers were 0052's own SQL + postgres_test.go
-- assertions), so moving them onto installations loses no data. A forge-agnostic
-- workspace spanning both a github.com install and a gitlab.com group cannot
-- share one per-account base URL, so the endpoints belong per-installation
-- (Amendment A1). This child owns only column LOCATION — endpoint RESOLUTION
-- (reading installations.forge_base_url, threading it through the OAuth/App
-- clients + GitLab endpoints) is deferred to E44.2 (#1826).
--
-- Isolation is NOT enforced here: account_id is nullable throughout and no
-- reader/writer is wired into the server. RLS predicates and handler authz land
-- in later E44 children (#1830 / #1829); a later child tightens account_id to
-- NOT NULL once every row is populated. Schema mirrors 0052: UUID PKs, TEXT
-- columns with named CHECK constraints, now()-defaulted TIMESTAMPTZ, and a
-- BEFORE UPDATE trigger reusing the shared fishhawk_set_updated_at() function
-- defined in 0001 (NOT redefined here).

-- account_members: forge-neutral membership grants — the login-gate source,
-- materialized from GitHub Enterprise / GitLab group membership by a later
-- child. member_ref is the forge-neutral member key (a GitHub login / GitLab
-- username or id). The composite FK pins a grant's provider to its account's,
-- exactly as installations does, so a membership can never attach to an account
-- of a different forge.
CREATE TABLE account_members (
    id          UUID         PRIMARY KEY,
    account_id  UUID         NOT NULL,
    provider    TEXT         NOT NULL DEFAULT 'github',
    member_ref  TEXT         NOT NULL,
    role        TEXT,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT account_members_provider_check CHECK (
        provider IN ('github', 'gitlab')
    ),
    UNIQUE (account_id, provider, member_ref),
    -- A membership grant has no meaning without its account, so CASCADE (unlike
    -- the ON DELETE SET NULL on the root tables below, where run/audit history
    -- must survive an account deletion).
    FOREIGN KEY (account_id, provider) REFERENCES accounts (id, provider) ON DELETE CASCADE
);

CREATE INDEX account_members_account_idx ON account_members (account_id);

CREATE TRIGGER account_members_set_updated_at BEFORE UPDATE ON account_members FOR EACH ROW EXECUTE FUNCTION fishhawk_set_updated_at();

-- Thread a nullable account_id through every root entity. ON DELETE SET NULL:
-- deleting an account nulls the reference rather than cascade-deleting runs or
-- audit history, so the tenancy tables can be torn down without erasing the
-- work ledger. Nullable throughout: CLI/local runs with no installation stay
-- NULL, bound to the single implicit Mode-1 account by a later child.
ALTER TABLE runs                        ADD COLUMN account_id UUID;
ALTER TABLE campaigns                   ADD COLUMN account_id UUID;
ALTER TABLE refinement_drafts           ADD COLUMN account_id UUID;
ALTER TABLE refinement_decisions        ADD COLUMN account_id UUID;
ALTER TABLE refinement_filing_sessions  ADD COLUMN account_id UUID;
ALTER TABLE refinement_filed_items      ADD COLUMN account_id UUID;
ALTER TABLE api_tokens                  ADD COLUMN account_id UUID;
ALTER TABLE audit_entries               ADD COLUMN account_id UUID;

ALTER TABLE runs                        ADD CONSTRAINT runs_account_id_fkey                       FOREIGN KEY (account_id) REFERENCES accounts (id) ON DELETE SET NULL;
ALTER TABLE campaigns                   ADD CONSTRAINT campaigns_account_id_fkey                  FOREIGN KEY (account_id) REFERENCES accounts (id) ON DELETE SET NULL;
ALTER TABLE refinement_drafts           ADD CONSTRAINT refinement_drafts_account_id_fkey          FOREIGN KEY (account_id) REFERENCES accounts (id) ON DELETE SET NULL;
ALTER TABLE refinement_decisions        ADD CONSTRAINT refinement_decisions_account_id_fkey       FOREIGN KEY (account_id) REFERENCES accounts (id) ON DELETE SET NULL;
ALTER TABLE refinement_filing_sessions  ADD CONSTRAINT refinement_filing_sessions_account_id_fkey FOREIGN KEY (account_id) REFERENCES accounts (id) ON DELETE SET NULL;
ALTER TABLE refinement_filed_items      ADD CONSTRAINT refinement_filed_items_account_id_fkey     FOREIGN KEY (account_id) REFERENCES accounts (id) ON DELETE SET NULL;
ALTER TABLE api_tokens                  ADD CONSTRAINT api_tokens_account_id_fkey                 FOREIGN KEY (account_id) REFERENCES accounts (id) ON DELETE SET NULL;
ALTER TABLE audit_entries               ADD CONSTRAINT audit_entries_account_id_fkey              FOREIGN KEY (account_id) REFERENCES accounts (id) ON DELETE SET NULL;

CREATE INDEX runs_account_id_idx                       ON runs (account_id);
CREATE INDEX campaigns_account_id_idx                  ON campaigns (account_id);
CREATE INDEX refinement_drafts_account_id_idx          ON refinement_drafts (account_id);
CREATE INDEX refinement_decisions_account_id_idx       ON refinement_decisions (account_id);
CREATE INDEX refinement_filing_sessions_account_id_idx ON refinement_filing_sessions (account_id);
CREATE INDEX refinement_filed_items_account_id_idx     ON refinement_filed_items (account_id);
CREATE INDEX api_tokens_account_id_idx                 ON api_tokens (account_id);
CREATE INDEX audit_entries_account_id_idx              ON audit_entries (account_id);

-- Amendment A1: relocate the per-forge endpoint columns from accounts to
-- installations. NULL = provider default endpoints (api.github.com / github.com
-- today). Forge-neutral names, never github_-prefixed.
ALTER TABLE installations ADD COLUMN forge_base_url TEXT;
ALTER TABLE installations ADD COLUMN oauth_base_url TEXT;
ALTER TABLE accounts DROP COLUMN forge_base_url;
ALTER TABLE accounts DROP COLUMN oauth_base_url;

-- Backfill runs.account_id from the installations mapping. runs.installation_id
-- is BIGINT (0005) while installations.installation_ref is TEXT (0052), so the
-- join casts installation_id::text = installation_ref. This is a no-op today —
-- no writer populates installations yet — so it updates zero rows; nil-
-- installation_id CLI/local runs stay NULL, bound to the implicit Mode-1
-- account by a later child.
UPDATE runs
   SET account_id = i.account_id
  FROM installations i
 WHERE runs.installation_id IS NOT NULL
   AND i.installation_ref = runs.installation_id::text;
