-- Revert 0121: drop host_dispatcher_heartbeats (its key index and tenant
-- policy go with it). The table holds only each dispatcher's latest snapshot,
-- never a chain fact, so dropping it loses nothing a later tick does not
-- rewrite.
--
-- Rollback order: revert or redeploy fishhawkd FIRST, then run this down. A
-- fishhawkd that still serves POST /v0/host-dispatchers/heartbeat would
-- answer every heartbeat 500 against a missing table; a reverted binary never
-- reads or writes it.

DROP POLICY IF EXISTS host_dispatcher_heartbeats_tenant_isolation ON host_dispatcher_heartbeats;
DROP TABLE IF EXISTS host_dispatcher_heartbeats;
