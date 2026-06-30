-- Rollback 0043: drop the deploy-gate cross-run reference (#1417). Index
-- first, then the column. Additive forward migration → clean one-step down
-- with no data to unwind (every row defaulted NULL).
DROP INDEX IF EXISTS runs_upstream_run_id_idx;
ALTER TABLE runs DROP COLUMN IF EXISTS upstream_run_id;
