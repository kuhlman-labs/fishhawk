-- 0087: the derived non-terminal campaign state 'awaiting_human' (E72.33 / #3660).
--
-- A campaign whose only remaining OPEN items are human-led (autonomy:low) had no
-- state to report: DeriveState emitted 'running' forever, because the auto-driver
-- will never dispatch an autonomy:low item and no item state implied a stop.
-- 'awaiting_human' is that state — DERIVED from the campaign.NextEligible
-- partition (>= 1 HumanLed item, no Eligible/Restartable/Running/Paused item) and
-- deliberately NON-terminal, so the campaign can return to pending/running when
-- the operator relabels a human-led issue to a driveable tier.
--
-- Additive and rewrite-free: it only WIDENS campaigns_state_check to admit the
-- new value (PostgreSQL cannot edit a CHECK in place — DROP then ADD;
-- https://www.postgresql.org/docs/current/sql-altertable.html), the same pattern
-- 0040 used to admit 'paused'. No existing row is touched.
--
-- ONLY the CAMPAIGNS constraint changes. 'awaiting_human' is a campaign-level
-- REDUCTION over item states, not an item state, so campaign_items gains nothing.

ALTER TABLE campaigns DROP CONSTRAINT campaigns_state_check;
ALTER TABLE campaigns ADD CONSTRAINT campaigns_state_check CHECK (
    state IN ('pending', 'running', 'paused', 'awaiting_human', 'succeeded', 'failed', 'cancelled')
);
