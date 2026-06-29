-- Down-migration for 0040: reverse the campaign pause/page overlay.
--
-- Rollback realism (binding condition #1): before re-adding the narrower state
-- CHECK constraints, normalize any LIVE 'paused' rows to 'running' so the
-- re-added constraint validates against existing data. A paused campaign/item
-- is always resumable, so collapsing it to running on rollback is safe and
-- never strands a row in an unrepresentable state. Without this, re-adding the
-- narrower CHECK would fail (SQLSTATE 23514) whenever a paused row exists.
UPDATE campaigns      SET state = 'running' WHERE state = 'paused';
UPDATE campaign_items SET state = 'running' WHERE state = 'paused';

ALTER TABLE campaign_items DROP CONSTRAINT campaign_items_state_check;
ALTER TABLE campaign_items ADD CONSTRAINT campaign_items_state_check CHECK (
    state IN ('pending', 'blocked', 'running', 'succeeded', 'failed', 'cancelled')
);

ALTER TABLE campaigns DROP CONSTRAINT campaigns_state_check;
ALTER TABLE campaigns ADD CONSTRAINT campaigns_state_check CHECK (
    state IN ('pending', 'running', 'succeeded', 'failed', 'cancelled')
);

-- Drop the added columns (campaigns_pause_policy_check drops with its column).
ALTER TABLE campaign_items DROP COLUMN pause_reason;
ALTER TABLE campaigns DROP COLUMN pause_policy;
