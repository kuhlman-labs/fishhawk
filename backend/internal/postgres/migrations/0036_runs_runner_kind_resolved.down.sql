-- 0036 down: drop the runner_kind LOCK flag. Any runner_kind_resolved /
-- runner_kind_mismatch audit entries already written survive as inert
-- historical records; runs revert to treating runner_kind as an
-- un-locked creation hint (pre-change semantics) until a re-migration.
ALTER TABLE runs
    DROP COLUMN IF EXISTS runner_kind_resolved;
