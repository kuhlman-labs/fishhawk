-- Down-migration for 0087: reverse the 'awaiting_human' campaign state.
--
-- Rollback realism (the shape 0040's down migration established): before
-- re-adding the narrower CHECK, normalize any LIVE 'awaiting_human' row to
-- 'running' so the re-added constraint validates against existing data. Without
-- this, ADD CONSTRAINT fails with SQLSTATE 23514 whenever such a row exists.
--
-- WHY the rewrite target is 'running': for the #3660 HEADLINE case — a started
-- campaign whose agent-drivable work finished — that is the state the row held
-- BEFORE 0087 (DeriveState's progress arm), so the rollback restores the prior
-- behavior rather than inventing one. ONE EXCEPTION, deliberately accepted: a
-- NEVER-STARTED all-human-led campaign derived 'pending' pre-0087 and lands in
-- 'running' here. That is benign — the old driver sweeps it, finds no eligible
-- item and no-ops, and the pre-0087 start gate admits 'running' — but the old
-- table has no running -> pending edge, so the row sticks at 'running': at
-- worst the pre-#3660 class of cosmetic state overstatement.
--
-- The rewrite itself is not optional. Pre-0087 code sweeps only 'running'
-- campaigns in the campaign driver and its start gate admits
-- pending/running/failed, so a row left in 'awaiting_human' would be STRANDED
-- — unswept and unstartable.
--
-- The SAME statement is required for a CODE-ONLY revert (Go reverted, 0087 left
-- applied): see backend/internal/campaign/README.md.
UPDATE campaigns SET state = 'running' WHERE state = 'awaiting_human';

ALTER TABLE campaigns DROP CONSTRAINT campaigns_state_check;
ALTER TABLE campaigns ADD CONSTRAINT campaigns_state_check CHECK (
    state IN ('pending', 'running', 'paused', 'succeeded', 'failed', 'cancelled')
);
