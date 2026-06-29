-- 0039 (down): drop the campaign tables (children first). The campaign
-- object (ADR-047 / #1437, E25.2) is purely additive — it creates two new
-- tables and references runs(id) with ON DELETE SET NULL but makes no
-- change to runs/stages — so the rollback is a clean two-table drop with
-- no data migration. The shared fishhawk_set_updated_at() function is left
-- in place (it predates this migration, defined in 0001).

DROP TABLE campaign_items;
DROP TABLE campaigns;
