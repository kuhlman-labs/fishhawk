-- 0060: E44.25 repo-ACL purge WATERMARK (#2116). repo_acl_purge_watermarks
-- holds, per (provider, subject), a BIGINT generation counter that is the
-- delete-SURVIVING, clock-INDEPENDENT ordering signal the mirror uses to close
-- the login-purge-vs-in-flight-resolution race (race B).
--
-- The mechanism: InvalidateSubject BUMPS the generation (a row-level write
-- lock) strictly BEFORE it deletes the subject's repo_acl_entries rows, and a
-- memoizing write is a GUARDED conditional insert whose SELECT reads this
-- watermark row under FOR SHARE. FOR SHARE conflicts with the FOR NO KEY UPDATE
-- lock BumpRepoACLPurgeWatermark takes when it updates the non-key generation
-- column, so the guarded read BLOCKS behind an in-flight bump and, on unblock,
-- re-reads the BUMPED generation (EvalPlanQual under READ COMMITTED) — a write
-- whose captured generation now trails the live one is rejected. The counter is
-- a DB-side value that SURVIVES deletion of repo_acl_entries rows (deleting an
-- entry never touches this table), which is what lets the guard hold ACROSS the
-- entry-row delete.
--
-- DELIBERATELY OUTSIDE THE 0057 RLS REGIME, for the SAME reason 0059's
-- repo_acl_entries is: it mirrors an identity-scoped forge fact (a per-(subject)
-- purge ordering), not account-scoped tenant data. Every table 0057 covers is
-- keyed by account_id; this one is not account-scoped at all. Adding account_id
-- purely to satisfy the RLS regime would duplicate a counter per account and
-- invite the copies to disagree. The choice is stated here so it stays
-- challengeable at the gate rather than being silent; TestMigrateUp asserts both
-- the absent account_id column and relrowsecurity = 0.
--
-- Keyed (provider, subject) — the SAME granularity as the login purge
-- (DeleteRepoACLEntriesForSubject), because InvalidateSubject purges the whole
-- subject. The PRIMARY KEY row is the object the guarded upsert takes FOR SHARE
-- on, so it MUST exist to be lockable: FOR SHARE on an ABSENT row locks nothing
-- and would silently reopen the race. EnsureRepoACLPurgeWatermark (called at
-- resolution start) and BumpRepoACLPurgeWatermark (on the first-ever purge) both
-- guarantee the row exists before it is ever locked.
--
-- Purely additive: creates one new table, touches no existing table, column,
-- constraint or policy. Schema mirrors 0059: TEXT key columns with a named CHECK
-- on provider, now()-defaulted TIMESTAMPTZ, and a BEFORE UPDATE trigger reusing
-- the shared fishhawk_set_updated_at() function defined in 0001 (NOT redefined
-- here).

CREATE TABLE repo_acl_purge_watermarks (
    -- Forge discriminator; same named-CHECK shape 0059 uses.
    provider    TEXT         NOT NULL,
    -- The forge-neutral member key (SubjectRef semantics), same as
    -- repo_acl_entries.subject — the login purge keys on this granularity.
    subject     TEXT         NOT NULL,
    -- The monotonic purge generation. InvalidateSubject increments it before
    -- deleting the subject's entry rows; a guarded write captured at generation
    -- G is rejected once a purge has bumped the live value above G. DB-side and
    -- clock-INDEPENDENT: unlike a wall-clock checked_at it cannot go backwards
    -- under NTP correction or collapse under sub-precision ties.
    generation  BIGINT       NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT repo_acl_purge_watermarks_provider_check CHECK (
        provider IN ('github', 'gitlab')
    ),
    -- The row the guarded upsert takes FOR SHARE on. It MUST exist to be
    -- lockable — see the header.
    PRIMARY KEY (provider, subject)
);

CREATE TRIGGER repo_acl_purge_watermarks_set_updated_at BEFORE UPDATE ON repo_acl_purge_watermarks FOR EACH ROW EXECUTE FUNCTION fishhawk_set_updated_at();
