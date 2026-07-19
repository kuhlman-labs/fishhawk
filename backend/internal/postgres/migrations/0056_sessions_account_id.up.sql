-- 0056: E44.3 login-gate persistence (ADR-057 Amendment A2, #1827) — bind
-- browser sessions to the workspace account that admitted them, and stand up
-- the admission-model columns:
--
--   * sessions.account_id — the account resolved at sign-in. Nullable (bearer
--     tokens and pre-gate sessions carry none) with ON DELETE SET NULL so
--     deleting an account never cascade-deletes live sessions; the
--     /v0/auth/me account_unresolved 403 then denies those sessions instead.
--   * account_members.origin — distinguishes operator-'invited' grants (admit
--     DB-only, immune to forge-API availability) from login-minted
--     'auto_join' grants (re-verified against the live forge org list at each
--     sign-in). Existing 0055 rows default to 'invited'.
--   * accounts.auto_join_role — the per-org auto-join policy anchor. NULL =
--     no auto-join; non-NULL on an organization-granularity account = members
--     of that org may auto-join at this role.

ALTER TABLE sessions ADD COLUMN account_id UUID;
ALTER TABLE sessions ADD CONSTRAINT sessions_account_id_fkey
    FOREIGN KEY (account_id) REFERENCES accounts (id) ON DELETE SET NULL;
CREATE INDEX sessions_account_id_idx ON sessions (account_id);

ALTER TABLE account_members ADD COLUMN origin TEXT NOT NULL DEFAULT 'invited';
ALTER TABLE account_members ADD CONSTRAINT account_members_origin_check
    CHECK (origin IN ('invited', 'auto_join'));

ALTER TABLE accounts ADD COLUMN auto_join_role TEXT;
