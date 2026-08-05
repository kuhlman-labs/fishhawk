-- Reverse 0065 (E66.42 / #2482): drop the runs.working_dir binding column.
--
-- Lossy in principle — dropping the column discards the bound checkout of
-- any in-flight run — but harmless to roll back after a code revert: a
-- reverted MCP layer simply stops inheriting and returns to demanding the
-- parameter per #2479. Pinned by TestMigrateDown_RunsWorkingDirReversal.

ALTER TABLE runs DROP COLUMN IF EXISTS working_dir;
