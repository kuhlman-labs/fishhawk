-- 0059: E44.10 per-identity forge repo-ACL MIRROR (ADR-057 Amendment A2,
-- #2071). repo_acl_entries caches, per (provider, subject, repo), the
-- forge-neutral permission tier identity.IdentityProvider.PermissionLevel
-- resolved, with a checked_at stamp the reader TTLs against. It is a CACHE of
-- a forge fact, never the authority: the forge stays authoritative and a miss
-- or an expired row re-resolves live.
--
-- DELIBERATELY OUTSIDE THE 0057 RLS REGIME. Every table 0057 covers is tenant
-- data keyed by account_id; this one is not account-scoped at all, because the
-- fact it mirrors — "does forge subject S have >= read on repo R" — is a
-- property of the (identity, repo) pair and is identical across every account
-- the subject belongs to. Adding account_id purely to satisfy the RLS regime
-- would duplicate a row per account and invite the two copies to disagree. If
-- a reviewer prefers it inside the regime it needs an account_id column and a
-- <table>_tenant_isolation policy; the choice is stated here so it stays
-- challengeable at the gate rather than being silent.
--
-- Purely additive: creates one new table, touches no existing table, column,
-- constraint or policy. Schema mirrors 0055: UUID PK, TEXT columns with a
-- named CHECK on provider, now()-defaulted TIMESTAMPTZ, and a BEFORE UPDATE
-- trigger reusing the shared fishhawk_set_updated_at() function defined in
-- 0001 (NOT redefined here).

CREATE TABLE repo_acl_entries (
    id          UUID         PRIMARY KEY,
    -- Forge discriminator; same named-CHECK shape 0055 uses so a third forge
    -- is one additive ALTER away.
    provider    TEXT         NOT NULL,
    -- The forge-neutral member key with the "<provider>:" prefix stripped —
    -- account_members.member_ref semantics, so the two tables key on the same
    -- string for the same human.
    subject     TEXT         NOT NULL,
    -- "owner/name", the form identity.IdentityProvider.PermissionLevel takes.
    repo        TEXT         NOT NULL,
    -- A forge-neutral identity.Permission tier ("none"/"read"/.../"admin").
    -- No CHECK: identity.Permission.AtLeast already fails CLOSED on an
    -- unrecognized tier (an unknown value ranks 0 and satisfies no minimum),
    -- so a widened vocabulary must not require a constraint migration to be
    -- readable, and a stale unknown value denies rather than erroring.
    permission  TEXT         NOT NULL,
    -- When the forge was last consulted. The reader TTLs against THIS, not
    -- updated_at, so a no-op re-resolve that leaves permission unchanged still
    -- refreshes the freshness clock.
    checked_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT repo_acl_entries_provider_check CHECK (
        provider IN ('github', 'gitlab')
    ),
    -- The natural key the read path looks up and the upsert conflicts on.
    UNIQUE (provider, subject, repo)
);

-- The login purge (DeleteRepoACLEntriesForSubject) deletes every row for one
-- identity. The UNIQUE index above is a usable prefix scan for that, but a
-- dedicated (provider, subject) index keeps the purge cheap and intent legible.
CREATE INDEX repo_acl_entries_provider_subject_idx ON repo_acl_entries (provider, subject);

CREATE TRIGGER repo_acl_entries_set_updated_at BEFORE UPDATE ON repo_acl_entries FOR EACH ROW EXECUTE FUNCTION fishhawk_set_updated_at();
