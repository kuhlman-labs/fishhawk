-- 0049: campaign_items.autonomy — the autonomy tier of the item's issue,
-- threaded from the child's `autonomy:<tier>` label so campaign eligibility can
-- divert a deps-satisfied autonomy:low (human-led) item out of the auto-dispatch
-- Eligible slice (#1551 / E32.4).
--
-- The tier is parsed at assembly time from workmgmt.EpicChild.Autonomy and
-- persisted here; the campaign engine reads it back onto campaign.Item.Autonomy
-- to partition readiness (Eligible vs the new HumanLed slice, added by a sibling
-- slice).
--
-- This migration is additive: a NOT NULL column with a '' DEFAULT, so every
-- existing campaign_items row receives the empty (unknown/default) tier and
-- behaves exactly as today (treated as non-human-led → Eligible). No row is
-- rewritten.
--
-- The CHECK is fail-closed: only the empty tier plus the three known autonomy
-- tiers are admitted, so a typo'd or out-of-set value is rejected at write time
-- rather than silently persisting a tier the engine cannot interpret.

ALTER TABLE campaign_items
    ADD COLUMN autonomy TEXT NOT NULL DEFAULT '',
    ADD CONSTRAINT campaign_items_autonomy_check CHECK (
        autonomy IN ('', 'low', 'medium', 'high')
    );
