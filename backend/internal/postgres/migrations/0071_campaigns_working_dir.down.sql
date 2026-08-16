-- Reverse 0071 (E48.87 / #2527): drop the campaigns.working_dir binding column.
--
-- Lossy in principle — dropping the column discards the bound checkout of any
-- live campaign — but harmless to roll back after a code revert: a reverted
-- item-run handler simply stops inheriting and returns to #2498's per-item
-- working_dir parameter, which remains fully functional on its own. No data
-- backfill and no rewrite: the column landed NOT NULL DEFAULT ''.
-- Pinned by TestMigrateDown_CampaignsWorkingDirReversal.

ALTER TABLE campaigns DROP COLUMN IF EXISTS working_dir;
