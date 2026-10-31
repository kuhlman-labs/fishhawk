-- Reverse 0113 (#4485): drop the campaigns.runner_kind column.
--
-- Lossy in principle — dropping the column discards the recorded runner kind
-- of any live campaign — but harmless to roll back after a code revert: a
-- reverted item-run handler returns to the per-item runner_kind parameter with
-- its github_actions default, which remains fully functional on its own. No
-- data backfill and no rewrite: the column landed NOT NULL DEFAULT ''.
-- Pinned by TestMigrateDown_CampaignsRunnerKindReversal.

ALTER TABLE campaigns DROP COLUMN IF EXISTS runner_kind;
