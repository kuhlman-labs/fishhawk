-- Revert 0088: drop the decision_index projection. The table is DERIVED from
-- the audit chain, so dropping it loses nothing the chain lacks; re-applying
-- 0088 and running `fishhawkd decision-index backfill --rebuild` reconstructs it.

DROP POLICY IF EXISTS decision_index_tenant_isolation ON decision_index;
DROP TABLE IF EXISTS decision_index;
