-- 0105 (down): remove the `lapsed` scope_amendments status (E83.64 / #4127).
--
-- Every lapsed row is mapped back to the pre-0105 shape of an undecided
-- request (pending, no decision fields) BEFORE the CHECK is narrowed, or the
-- re-added constraint would fail on the lapsed rows. The scope_amendment_lapsed
-- audit entries stay in the hash chain (audit is append-only).
--
-- Rollback order: revert the fishhawkd binary FIRST. A binary that still
-- lapses rows can write one between the UPDATE and the narrowing, which fails
-- this down migration.

UPDATE scope_amendments
   SET status = 'pending',
       decision_reason = NULL,
       decided_at = NULL,
       decided_by = NULL
 WHERE status = 'lapsed';

ALTER TABLE scope_amendments
    DROP CONSTRAINT scope_amendments_status_check,
    ADD CONSTRAINT scope_amendments_status_check CHECK (
        status IN ('pending', 'approved', 'denied')
    );
