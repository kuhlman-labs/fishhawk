-- Down-migration for 0049: drop the campaign_items.autonomy column.
--
-- The column CHECK (campaign_items_autonomy_check) drops with the column, so
-- this is a clean single-statement reversal — no data normalization is needed
-- (an additive column with a DEFAULT never left any other table in a state the
-- rollback must repair).

ALTER TABLE campaign_items DROP COLUMN autonomy;
