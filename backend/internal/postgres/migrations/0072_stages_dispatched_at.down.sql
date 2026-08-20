-- Reverse 0072 in dependency order: the trigger depends on the function, so
-- drop the trigger first, then the function, then the column. Purely additive
-- alongside stages_set_updated_at / fishhawk_set_updated_at (migration 0001):
-- this rollback deliberately does NOT touch those, so the updated_at trigger
-- survives.
DROP TRIGGER IF EXISTS stages_stamp_dispatched_at ON stages;
DROP FUNCTION IF EXISTS fishhawk_stamp_stage_dispatched_at();
ALTER TABLE stages DROP COLUMN dispatched_at;
