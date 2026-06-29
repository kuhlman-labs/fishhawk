-- Down-migration for 0041: drop the campaign-level operator_agent override.
--
-- A nullable JSONB column with no CHECK and no dependent objects drops
-- cleanly; no data normalization is needed (unlike 0040's paused-row rollback).
ALTER TABLE campaigns
    DROP COLUMN operator_agent;
