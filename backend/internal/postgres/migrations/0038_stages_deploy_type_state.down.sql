-- Revert 0038: narrow stages_type_check back to the pre-0038 set
-- (plan/implement/review) and stages_state_check back to the exact 0035
-- set (no 'awaiting_deploy_approval' / 'awaiting_deployment').
--
-- The CHECK re-add fails loudly if any stage row currently holds a
-- 'deploy' type or one of the two deploy states, so the down is safe
-- only when no deploy stage exists — exactly the narrowing precondition a
-- rollback of an additive CHECK widening needs (the 0035 / #1231
-- precedent). Existing plan/implement/review rows and the 0035 states are
-- untouched.
ALTER TABLE stages DROP CONSTRAINT stages_type_check;
ALTER TABLE stages ADD CONSTRAINT stages_type_check CHECK (
    stage_type IN ('plan', 'implement', 'review')
);

ALTER TABLE stages DROP CONSTRAINT stages_state_check;
ALTER TABLE stages ADD CONSTRAINT stages_state_check CHECK (
    state IN ('pending', 'dispatched', 'running', 'awaiting_approval',
              'awaiting_children', 'awaiting_input', 'awaiting_scope_decision',
              'succeeded', 'failed', 'cancelled')
);
