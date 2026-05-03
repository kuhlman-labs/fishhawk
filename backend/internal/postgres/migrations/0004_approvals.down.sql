-- 0004 down: drop approvals + the no-mutation triggers it created.

DROP TRIGGER IF EXISTS approvals_no_delete ON approvals;
DROP TRIGGER IF EXISTS approvals_no_update ON approvals;
DROP FUNCTION IF EXISTS fishhawk_approvals_no_mutation();
DROP TABLE IF EXISTS approvals;
