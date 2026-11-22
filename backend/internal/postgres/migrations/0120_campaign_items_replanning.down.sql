-- Down-migration for 0120: narrow campaign_items.state back to the 0040 set.
--
-- Rollback realism: before re-adding the narrower CHECK, rewrite every live
-- 'replanning' row to 'failed' — the reading pre-0120 code gave a run whose
-- plan was rejected. A failed item with satisfied dependencies is still
-- offered Restartable (start_run) by campaign.NextEligible, so no row strands.
-- Without this UPDATE, re-adding the narrower CHECK fails (SQLSTATE 23514)
-- whenever a replanning row exists. A code-only revert that leaves 0120
-- applied must run the same UPDATE (backend/internal/campaign/README.md).
UPDATE campaign_items SET state = 'failed' WHERE state = 'replanning';

ALTER TABLE campaign_items DROP CONSTRAINT campaign_items_state_check;
ALTER TABLE campaign_items ADD CONSTRAINT campaign_items_state_check CHECK (
    state IN ('pending', 'blocked', 'running', 'paused', 'succeeded', 'failed', 'cancelled')
);
