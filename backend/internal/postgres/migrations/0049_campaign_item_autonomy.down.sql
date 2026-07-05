-- Down-migration for 0049: drop the campaign_items.autonomy column and its
-- fail-closed CHECK constraint.
--
-- The column is additive with a NOT NULL DEFAULT '' and no dependent objects, so
-- the rollback drops the constraint then the column cleanly with no data
-- normalization needed (every row carried the '' default). Campaigns behave
-- exactly as pre-0049 after the roll back: every item is treated as
-- non-human-led.
ALTER TABLE campaign_items
    DROP CONSTRAINT campaign_items_autonomy_check,
    DROP COLUMN autonomy;
