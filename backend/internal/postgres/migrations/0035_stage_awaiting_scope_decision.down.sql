-- Revert 0035: drop the scope_completeness_park column and narrow
-- stages_state_check back to the 0032 set (no 'awaiting_scope_decision').
-- The CHECK re-add fails loudly if any stage row currently holds
-- 'awaiting_scope_decision', so the down is safe only when no stage is
-- parked for a scope-completeness decision — exactly the invariant a
-- rollback needs.
ALTER TABLE stages DROP COLUMN scope_completeness_park;

ALTER TABLE stages DROP CONSTRAINT stages_state_check;
ALTER TABLE stages ADD CONSTRAINT stages_state_check CHECK (
    state IN ('pending', 'dispatched', 'running', 'awaiting_approval',
              'awaiting_children', 'awaiting_input', 'succeeded', 'failed',
              'cancelled')
);
