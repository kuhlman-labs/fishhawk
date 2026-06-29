-- Down-migration for 0042: drop the campaign idempotency key.
--
-- Reverse order of the up: drop the partial unique index first, then
-- the nullable column. No data normalization is needed — the column is
-- additive and nullable with no dependent objects.
DROP INDEX IF EXISTS campaigns_idempotency_key_repo_idx;

ALTER TABLE campaigns
    DROP COLUMN idempotency_key;
