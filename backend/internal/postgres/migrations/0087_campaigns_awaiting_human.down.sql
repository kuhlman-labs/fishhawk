-- Down-migration for 0087: reverse the 'awaiting_human' campaign state.
--
-- Rollback realism (the shape 0040's down migration established): before
-- re-adding the narrower CHECK, normalize any LIVE 'awaiting_human' row to
-- 'running' so the re-added constraint validates against existing data. Without
-- this, ADD CONSTRAINT fails with SQLSTATE 23514 whenever such a row exists.
--
-- WHY the rewrite target is 'running': that is exactly the state these campaigns
-- held BEFORE 0087 (DeriveState's progress arm, or the pre-change default for a
-- started campaign), so the rollback restores the prior behavior rather than
-- inventing one. Pre-0087 code sweeps only 'running' campaigns in the campaign
-- driver and its start gate admits pending/running/failed, so a row left in
-- 'awaiting_human' would be STRANDED — unswept and unstartable.
--
-- The SAME statement is required for a CODE-ONLY revert (Go reverted, 0087 left
-- applied): see backend/internal/campaign/README.md.
UPDATE campaigns SET state = 'running' WHERE state = 'awaiting_human';

ALTER TABLE campaigns DROP CONSTRAINT campaigns_state_check;
ALTER TABLE campaigns ADD CONSTRAINT campaigns_state_check CHECK (
    state IN ('pending', 'running', 'paused', 'succeeded', 'failed', 'cancelled')
);
