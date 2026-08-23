-- Down-migration for 0074: drop campaign_items.queue_position and
-- campaigns.grooming_source.
--
-- BOTH COLUMNS ARE DROPPED, AND THAT IS LOSSY IN ONE DIRECTION — say so
-- plainly rather than describing this as a clean rollback:
--
--   * queue_position: dropping it returns the item listing to (created_at, id)
--     order. For a campaign created from an epic_ref or an explicit items list
--     that is byte-identical to pre-0074 behaviour (every such campaign wrote
--     ascending positions that merely restated insertion order). For a campaign
--     created from a GROOMING ORDER it is NOT: the ratified rank order is the
--     only place that order was written down, so rolling back scrambles that
--     campaign's queue to the random-UUID tiebreak. Roll back BEFORE any
--     grooming-order campaign is created, or accept that its queue order is
--     lost.
--
--   * grooming_source: dropping it destroys the durable provenance of every
--     grooming-order campaign. The campaign_grooming_source_resolved audit
--     rows survive (the audit log stores its category as a string), so
--     provenance remains recoverable from the audit chain for any campaign
--     whose audit append succeeded — but that append is best-effort, which is
--     exactly why the column exists.
--
-- Neither column has a dependent object (no index, no constraint, no view), so
-- the DROPs themselves are mechanical.
ALTER TABLE campaigns
    DROP COLUMN grooming_source;

ALTER TABLE campaign_items
    DROP COLUMN queue_position;
