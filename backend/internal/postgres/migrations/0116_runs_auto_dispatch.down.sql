-- 0116 down: drop the ADR-104 Q3 host-dispatcher mark.
ALTER TABLE runs
    DROP COLUMN IF EXISTS auto_dispatch;
