-- Down-migration for 0068: drop the at-most-one approval_conditions_truncated
-- index.
--
-- Index-only and additive: dropping it restores the prior unconditional-append
-- behaviour (loadApprovalConditions appends a truncation entry on every prompt
-- build) with no schema residue and no data migration (audit_entries rows are
-- untouched). IF EXISTS keeps the rollback idempotent.
DROP INDEX IF EXISTS audit_entries_approval_conditions_truncated_once_idx;
