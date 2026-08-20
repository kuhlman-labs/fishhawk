-- 0072: stages.dispatched_at — a dedicated dispatch-transition timestamp for
-- the dispatch watchdog (#2744), so a ~15s progress heartbeat can no longer
-- suppress it.
--
-- The watchdog used to read stages.updated_at, which the stages_set_updated_at
-- trigger (migration 0001) bumps on EVERY stages UPDATE — including the
-- progress-only heartbeat 0070 added. That let a wedged-but-heartbeating stage
-- forge the watchdog's clock. This column is stamped ONLY on the state
-- TRANSITION into 'dispatched'; a progress-only UPDATE leaves state at
-- 'dispatched', so the transition-keyed predicate below is false and the stamp
-- is NOT refreshed even though updated_at still moves on that same write. That
-- transition-keyed stamp is the whole control: a heartbeat cannot advance it.
--
-- Additive + nullable, mirroring the 0035/0070 shape. The read is a dedicated
-- purpose-built query (run.ListDispatchedStageLiveness), so no existing stages
-- SELECT list expands and the sqlc Stage model is unchanged (the 0070
-- precedent).
ALTER TABLE stages ADD COLUMN dispatched_at TIMESTAMPTZ;

-- Stamp dispatched_at on a transition INTO 'dispatched' only. The TG_OP branch
-- is load-bearing: OLD is unassigned inside an INSERT-fired trigger, so
-- dereferencing OLD.state unconditionally would raise at runtime — an INSERT
-- that lands directly in 'dispatched' takes the TG_OP = 'INSERT' arm and never
-- touches OLD. Fires alongside stages_set_updated_at (also BEFORE trigger); the
-- two assign disjoint NEW fields, so their relative fire order is immaterial.
CREATE FUNCTION fishhawk_stamp_stage_dispatched_at() RETURNS trigger AS $$
BEGIN
    IF NEW.state = 'dispatched' AND (TG_OP = 'INSERT' OR OLD.state IS DISTINCT FROM 'dispatched') THEN
        NEW.dispatched_at := now();
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER stages_stamp_dispatched_at
    BEFORE INSERT OR UPDATE ON stages
    FOR EACH ROW
    EXECUTE FUNCTION fishhawk_stamp_stage_dispatched_at();

-- Backfill stages already sitting in 'dispatched' when this migration lands so
-- they do not present a NULL to the watchdog and fall back to updated_at.
-- APPROXIMATION: updated_at is a heartbeat-time approximation of the true
-- dispatch time, not the dispatch time itself — a pre-existing heartbeating
-- stage therefore receives at most one extra full budget period, once. The
-- backfill's own write re-bumps updated_at for exactly these rows, which is
-- inert because the watchdog stops reading updated_at for them.
UPDATE stages SET dispatched_at = updated_at WHERE state = 'dispatched' AND dispatched_at IS NULL;
