-- 0018 down: restore stages.gate_blocking_checks.
--
-- Down migration restores the column shape (a TEXT[] mirroring the
-- workflow-spec field) but cannot recover the data; pre-#254 rows
-- that have been migrated up come back with NULL here. Acceptable
-- for the v0.x window where rollback is only used in dev.

ALTER TABLE stages
    ADD COLUMN IF NOT EXISTS gate_blocking_checks TEXT[];
