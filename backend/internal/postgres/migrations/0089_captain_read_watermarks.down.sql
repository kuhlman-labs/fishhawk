-- Revert 0089: drop the digest's decision_index window index and the
-- captain_read_watermarks table. The table holds read positions only — every
-- fact the digest reports lives on the audit chain — so dropping it loses no
-- chain fact; captains simply start from an empty watermark on re-apply.

DROP INDEX IF EXISTS decision_index_repo_sequence_idx;
DROP POLICY IF EXISTS captain_read_watermarks_tenant_isolation ON captain_read_watermarks;
DROP TABLE IF EXISTS captain_read_watermarks;
