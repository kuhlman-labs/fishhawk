-- 0040: campaign pause/page overlay — the auto-driver's hand-off to a human
-- (Track C of ADR-047 / #1446, E25.7).
--
-- When a run gate refuses a hand-off a human must own (reviewer_reject /
-- requirement_arbitration), the backend auto-driver pauses the affected
-- campaign item — and, per the campaign's pause_policy, optionally the whole
-- campaign — and pages a human/operator-agent. A human resumes (paused →
-- running) once the gate is handled and the next driver tick re-engages.
--
-- This migration is additive: it widens the two state CHECK constraints to
-- admit 'paused' (PostgreSQL cannot edit a CHECK in place — DROP then ADD;
-- https://www.postgresql.org/docs/current/sql-altertable.html), adds the
-- campaigns.pause_policy column (the operator's block-the-campaign vs
-- continue-others choice, defaulting to the conservative 'pause_campaign'),
-- and adds the nullable campaign_items.pause_reason JSONB carrier. No existing
-- row is rewritten.

ALTER TABLE campaigns DROP CONSTRAINT campaigns_state_check;
ALTER TABLE campaigns ADD CONSTRAINT campaigns_state_check CHECK (
    state IN ('pending', 'running', 'paused', 'succeeded', 'failed', 'cancelled')
);

ALTER TABLE campaign_items DROP CONSTRAINT campaign_items_state_check;
ALTER TABLE campaign_items ADD CONSTRAINT campaign_items_state_check CHECK (
    state IN ('pending', 'blocked', 'running', 'paused', 'succeeded', 'failed', 'cancelled')
);

ALTER TABLE campaigns
    ADD COLUMN pause_policy TEXT NOT NULL DEFAULT 'pause_campaign'
        CONSTRAINT campaigns_pause_policy_check CHECK (pause_policy IN ('pause_campaign', 'pause_item'));

ALTER TABLE campaign_items
    ADD COLUMN pause_reason JSONB;
