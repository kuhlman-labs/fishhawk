-- 0031 down: drop the drive-mode flag. Any run_auto_advanced audit
-- entries already written survive as inert historical records; only
-- the per-run opt-in snapshot is lost until a re-migration.
ALTER TABLE runs
    DROP COLUMN IF EXISTS drive;
