-- 0097 down: drop the stage concurrency queue and its heartbeat reader.
-- Nothing else references either (stages -> slots is ON DELETE CASCADE on the
-- slot side).
DROP FUNCTION IF EXISTS fishhawk_stage_heartbeat_at(JSONB);
DROP TABLE IF EXISTS stage_concurrency_slots;
