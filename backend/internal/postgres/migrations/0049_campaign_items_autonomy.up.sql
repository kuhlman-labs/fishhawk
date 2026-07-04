-- 0049: campaign_items.autonomy — the per-item autonomy tier sourced from the
-- issue's `autonomy:<tier>` label (#1551, autonomy-aware campaign eligibility).
--
-- The campaign engine partitions items by DAG/dependency state and, until now,
-- knew nothing about an item's autonomy tier: an eligible autonomy:low
-- (human-led) item landed in Eligible and the auto-driver would mint an agent
-- run for a change METHODOLOGY.md reserves for human leadership. This column
-- carries the sourced tier so NextEligible can route a human-led item out of the
-- auto-dispatchable set.
--
-- Additive and safe on a populated table: a NOT NULL column WITH a DEFAULT ''
-- does not rewrite existing rows to a bad state (they get ''), and the
-- fail-closed CHECK admits only the empty tier plus the three real tiers. No
-- existing row is rewritten.
ALTER TABLE campaign_items
    ADD COLUMN autonomy TEXT NOT NULL DEFAULT ''
        CONSTRAINT campaign_items_autonomy_check CHECK (autonomy IN ('', 'low', 'medium', 'high'));
