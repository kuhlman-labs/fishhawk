-- Down-migration for 0049: reverse the campaign item autonomy tier.
--
-- Drop the CHECK constraint first, then the column. Dropping the column alone
-- would also drop the attached constraint, but the explicit DROP CONSTRAINT
-- mirrors the additive ALTER above and keeps the rollback obvious. The column
-- is NOT NULL DEFAULT '' with no dependents, so the drop is clean — no data
-- normalization is required (unlike 0040, whose widened state CHECK needed live
-- 'paused' rows collapsed first).
ALTER TABLE campaign_items DROP CONSTRAINT campaign_items_autonomy_check;
ALTER TABLE campaign_items DROP COLUMN autonomy;
