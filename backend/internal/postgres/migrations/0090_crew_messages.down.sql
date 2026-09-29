-- Revert 0090: drop the crew_messages projection. The table is DERIVED from
-- the audit chain, so dropping it loses nothing the chain lacks; re-applying
-- 0090 and replaying the crew-message chain entries reconstructs every row.

DROP POLICY IF EXISTS crew_messages_tenant_isolation ON crew_messages;
DROP TABLE IF EXISTS crew_messages;
