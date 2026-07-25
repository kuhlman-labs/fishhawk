-- Down-migration for 0062: drop the at-most-one merge_verdict_recorded index.
--
-- Index-only and additive: dropping it restores the prior read-then-append
-- behavior with no schema residue and no data migration (audit_entries rows are
-- untouched). IF EXISTS keeps the rollback idempotent.
DROP INDEX IF EXISTS audit_entries_merge_verdict_recorded_once_idx;
