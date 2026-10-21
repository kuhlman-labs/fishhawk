-- 0106 down: drop the campaign hotfix tables (E83.100 / #4234). The
-- campaign_item_added / campaign_hold_released audit entries already written
-- stay on the chain (audit is append-only). An expedited item survives as an
-- ordinary item at its (possibly negative) queue_position, and a campaign
-- holding a dispatch hold at rollback time is simply un-held.
--
-- Rollback order: revert the fishhawkd binary FIRST. A binary carrying the
-- hotfix code reads campaign_item_expedites on every campaign item read and
-- campaign_dispatch_holds at the host-dispatch marker, so dropping the tables
-- under it fails those reads until the binary is reverted.
DROP TABLE IF EXISTS campaign_dispatch_holds;
DROP TABLE IF EXISTS campaign_item_expedites;
