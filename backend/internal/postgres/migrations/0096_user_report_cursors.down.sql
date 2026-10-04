-- Revert 0096: drop the user_report_cursors policy and table. The table holds
-- user-report scan read positions only — no chain fact — so dropping it loses
-- nothing but positions; on re-apply the next scan starts again from the
-- initial lookback.

DROP POLICY IF EXISTS user_report_cursors_tenant_isolation ON user_report_cursors;
DROP TABLE IF EXISTS user_report_cursors;
