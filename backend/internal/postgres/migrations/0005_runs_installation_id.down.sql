-- 0005 down: drop installation_id from runs.

DROP INDEX IF EXISTS runs_installation_id_idx;
ALTER TABLE runs DROP COLUMN IF EXISTS installation_id;
