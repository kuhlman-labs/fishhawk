-- Revert 0032: narrow stages_state_check back to the 0026 set (no
-- 'awaiting_input'). Safe only when no stage row currently holds
-- 'awaiting_input'; the CHECK re-add fails loudly otherwise.
ALTER TABLE stages DROP CONSTRAINT stages_state_check;
ALTER TABLE stages ADD CONSTRAINT stages_state_check CHECK (
    state IN ('pending', 'dispatched', 'running', 'awaiting_approval',
              'awaiting_children', 'succeeded', 'failed', 'cancelled')
);
