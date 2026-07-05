-- 0049: campaign item autonomy tier — the source signal for autonomy-aware
-- next_action eligibility (E32.4 / #1551).
--
-- The campaign engine is otherwise blind to the autonomy:* label the repo
-- applies per convention, so a settled campaign can recommend start_run on an
-- autonomy:low, human-led item. This migration adds the durable carrier for
-- each item's tier, sourced from the epic child's GitHub labels and consumed by
-- the readiness partition (which routes an autonomy:low item to a human-led
-- slice instead of the auto-dispatchable eligible slice).
--
-- Additive: a single NOT NULL DEFAULT '' column with a fail-closed CHECK
-- restricting the value to the empty string (unlabelled → treated as
-- autonomous, the conservative non-regressing default) or one of the three
-- autonomy tiers. Pre-existing rows and any call site that does not set the
-- column persist as '' and behave exactly as before. No existing row is
-- rewritten.

ALTER TABLE campaign_items
    ADD COLUMN autonomy TEXT NOT NULL DEFAULT ''
        CONSTRAINT campaign_items_autonomy_check CHECK (autonomy IN ('', 'low', 'medium', 'high'));
