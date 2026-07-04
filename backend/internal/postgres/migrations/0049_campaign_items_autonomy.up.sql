-- 0049: campaign_items.autonomy — the per-item autonomy tier sourced from the
-- epic child's `autonomy:<tier>` issue label (#1551, E25).
--
-- The campaign engine routes an eligible human-led (autonomy 'low') item into a
-- distinct HumanLed partition instead of Eligible, so the auto-driver never
-- mints an agent run for a change METHODOLOGY.md reserves for human leadership.
-- The tier is persisted here so NextEligible / the rollup / next_action can read
-- it back off the item row.
--
-- This migration is additive: a NOT NULL column WITH a DEFAULT '' does not
-- rewrite existing rows to a bad state (they read back '') and the fail-closed
-- CHECK admits '' plus the three real tiers. No existing row is rewritten.

ALTER TABLE campaign_items
    ADD COLUMN autonomy TEXT NOT NULL DEFAULT ''
        CONSTRAINT campaign_items_autonomy_check CHECK (autonomy IN ('', 'low', 'medium', 'high'));
