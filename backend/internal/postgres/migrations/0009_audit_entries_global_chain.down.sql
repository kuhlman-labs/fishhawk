-- Rolls back 0009. NOT NULL is restored only when no global rows
-- exist; if any have been written the operator must drop them
-- explicitly before downgrading. We don't auto-purge — global
-- audit rows are compliance-relevant and silently deleting them
-- would be a quiet data loss.

DROP INDEX IF EXISTS audit_entries_global_seq_idx;

DELETE FROM audit_entries WHERE run_id IS NULL;

ALTER TABLE audit_entries
    ALTER COLUMN run_id SET NOT NULL;
