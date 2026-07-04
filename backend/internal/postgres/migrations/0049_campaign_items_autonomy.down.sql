-- Down: drop the campaign_items.autonomy column (and its CHECK). Additive
-- column, no data migration — the drop is clean.

ALTER TABLE campaign_items DROP COLUMN autonomy;
