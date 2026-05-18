-- 0024 down: drop the runner_kind column. Existing audit payloads
-- that carry the field stay as-is (JSONB; readers tolerate extra
-- fields), but new runs lose the provenance dimension until a
-- re-migration.
ALTER TABLE runs
    DROP COLUMN IF EXISTS runner_kind;
