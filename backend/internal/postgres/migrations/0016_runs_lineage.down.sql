-- 0016 down: drop the lineage columns + their indexes.
--
-- The partial indexes are dropped automatically when the columns
-- are dropped, but be explicit so a partial-rollback (drop one
-- column, keep the other) doesn't surprise an operator.

DROP INDEX IF EXISTS runs_parent_run_id_idx;
DROP INDEX IF EXISTS runs_pull_request_url_idx;

ALTER TABLE runs
    DROP COLUMN IF EXISTS parent_run_id,
    DROP COLUMN IF EXISTS pull_request_url;
