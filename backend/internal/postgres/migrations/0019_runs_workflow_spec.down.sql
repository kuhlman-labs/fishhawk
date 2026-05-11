-- 0019 down: drop the cached workflow spec column.

ALTER TABLE runs
    DROP COLUMN IF EXISTS workflow_spec;
