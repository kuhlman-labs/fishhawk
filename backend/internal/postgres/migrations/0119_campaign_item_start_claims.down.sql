-- 0119 down: drop campaign_item_start_claims (#5266). Claims in flight vanish:
-- a starter that claimed before the rollback can no longer link through its
-- claim, and items are no longer reserved before a mint.
--
-- Rollback order: revert the fishhawkd binary FIRST. A binary carrying the
-- claim code reads and writes this table on every campaign item start, so
-- dropping it under that binary fails every start until the binary is
-- reverted. A reverted binary never reads the table, so the vanished claims
-- are harmless to it.
DROP TABLE IF EXISTS campaign_item_start_claims;
