-- 0120: campaign_items.state admits 'replanning' (#5379).
--
-- Why: an operator who deliberately REJECTS a campaign item's plan fails the
-- plan stage (category D, reason run.FailureReasonGateRejected) and so fails
-- the run. Before this migration the item then settled `failed` — a rejection
-- read as a failure, and a single-item campaign derived terminal `failed`.
-- `replanning` is the NON-terminal state such an item settles to instead: it
-- keeps its run_id as provenance of the rejected run, is never auto-dispatched
-- by the campaign driver, never fails its campaign, and is reset to pending by
-- the operator restart verb (RestartCampaignItem). See
-- backend/internal/campaign/README.md.
--
-- Additive: PostgreSQL cannot edit a CHECK in place, so the constraint is
-- dropped and re-added with the wider set
-- (https://www.postgresql.org/docs/current/sql-altertable.html). No existing
-- row is rewritten.
ALTER TABLE campaign_items DROP CONSTRAINT campaign_items_state_check;
ALTER TABLE campaign_items ADD CONSTRAINT campaign_items_state_check CHECK (
    state IN ('pending', 'blocked', 'running', 'paused', 'replanning', 'succeeded', 'failed', 'cancelled')
);
