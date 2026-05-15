-- 0022 (down): revert the approvals.surface CHECK constraint to the
-- pre-E17.4 set. Drops rows whose surface is `github_reply_comment`
-- by failing the constraint re-add — the operator should clear
-- those rows manually before downgrading.

ALTER TABLE approvals
    DROP CONSTRAINT approvals_surface_check;

ALTER TABLE approvals
    ADD CONSTRAINT approvals_surface_check
    CHECK (surface IN ('api', 'ui', 'cli', 'github_comment'));
