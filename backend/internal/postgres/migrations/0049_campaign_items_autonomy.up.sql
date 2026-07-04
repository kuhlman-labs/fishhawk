-- 0049: campaign_items.autonomy — the per-item autonomy tier sourced from the
-- epic child's `autonomy:<tier>` issue label (#1551).
--
-- The campaign engine keys on this tier to keep a human-led (autonomy:low)
-- item out of the autonomously-dispatchable Eligible set, so the auto-driver
-- and next_action stop treating a human-led item as an agent run.
--
-- This migration is additive: a new NOT NULL TEXT column with a DEFAULT '',
-- so existing campaign_items rows get '' (unknown/unsourced tier — treated as
-- NOT human-led) and no row is rewritten. The fail-closed CHECK admits only
-- the empty tier and the three METHODOLOGY.md tiers, so a garbage tier is a
-- 23514 rather than a silently-dispatched item.

ALTER TABLE campaign_items
    ADD COLUMN autonomy TEXT NOT NULL DEFAULT ''
        CONSTRAINT campaign_items_autonomy_check CHECK (autonomy IN ('', 'low', 'medium', 'high'));
