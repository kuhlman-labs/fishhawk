DROP TRIGGER IF EXISTS stages_set_updated_at ON stages;
DROP TRIGGER IF EXISTS runs_set_updated_at   ON runs;
DROP FUNCTION IF EXISTS fishhawk_set_updated_at();
DROP TABLE   IF EXISTS stages;
DROP TABLE   IF EXISTS runs;
