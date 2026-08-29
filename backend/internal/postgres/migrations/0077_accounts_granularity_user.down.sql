-- 0077 down: restore the three-value accounts granularity CHECK
-- ('enterprise', 'organization', 'group').
--
-- Unlike 0051/0054 (whose new value was a dormant surface never written before
-- rollback), a Mode-1 personal-namespace install WILL have written a 'user'
-- accounts row. Re-adding the narrower CHECK VALIDATES existing rows, so this
-- down migration FAILS LOUDLY with SQLSTATE 23514 (check_violation) if any
-- accounts row still carries granularity='user' — BY DESIGN, not as an
-- oversight. The alternative — deleting those rows — is strictly worse: an
-- accounts row is referenced by account_members ON DELETE CASCADE (its grants
-- would vanish) and by runs / audit_entries ON DELETE SET NULL (their
-- account_id would be nulled), so a silent delete destroys admission history
-- and de-tenants live rows.
--
-- OPERATOR REMEDY before rolling back past 0077: re-key the user-granularity
-- accounts to a supported granularity, or delete them explicitly (accepting the
-- cascade), THEN run the rollback. A deployment that never used the 'user' tier
-- rolls back with no data step at all.
--
-- Existing 'enterprise'/'organization'/'group' rows are untouched.
ALTER TABLE accounts DROP CONSTRAINT accounts_granularity_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_granularity_check CHECK (
    granularity IN ('enterprise', 'organization', 'group')
);
