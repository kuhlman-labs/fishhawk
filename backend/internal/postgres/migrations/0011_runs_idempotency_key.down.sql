DROP INDEX IF EXISTS runs_idempotency_key_repo_idx;

ALTER TABLE runs
    DROP COLUMN idempotency_key;
