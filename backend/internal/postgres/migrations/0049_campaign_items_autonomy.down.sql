-- Down-migration for 0049: drop the campaign_items.autonomy column.
--
-- Clean and total: the column is additive with a DEFAULT '' and no other
-- feature reads it, so dropping it strands no data. The
-- campaign_items_autonomy_check constraint drops with its column.
ALTER TABLE campaign_items DROP COLUMN autonomy;
