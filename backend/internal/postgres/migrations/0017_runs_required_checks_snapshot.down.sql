-- 0017 down: drop the required-checks snapshot column.

ALTER TABLE runs
    DROP COLUMN IF EXISTS required_checks_snapshot;
