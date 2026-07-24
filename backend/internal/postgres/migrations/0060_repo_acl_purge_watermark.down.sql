-- 0060 down: drop the repo-ACL purge watermark. Lossless by construction — the
-- table holds only per-subject purge generation counters, so dropping it resets
-- every subject to generation 0, at worst re-admitting one already-in-flight
-- write per subject, itself bounded by the TTL. Purely additive rollback:
-- touches no other table. DROP TABLE removes the BEFORE UPDATE trigger with it;
-- the shared fishhawk_set_updated_at() function is NOT dropped (0001 owns it,
-- reused widely).
DROP TABLE IF EXISTS repo_acl_purge_watermarks;
