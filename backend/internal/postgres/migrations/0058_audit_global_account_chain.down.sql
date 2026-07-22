-- 0058 down: drop the per-account run-less chain index. The 0055
-- account_id column and all rows are untouched — the up migration was
-- index-only, so rollback is lossless.
DROP INDEX IF EXISTS audit_entries_global_account_seq_idx;
