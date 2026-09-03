-- Revert 0079: normalize every 'superseded' row to 'cancelled' BEFORE narrowing
-- stages_state_check back to the exact 0053 set (no 'superseded').
--
-- Order matters: the state normalization must run FIRST, because the narrower
-- CHECK re-add would raise SQLSTATE 23514 if any row still held the value it no
-- longer admits (the 0040/0053 normalize-then-narrow precedent).
--
-- This rollback is LOSSY IN THE FORWARD DIRECTION ONLY. 'cancelled' is the
-- closest pre-0079 legal value — both are terminal and neither claims the work
-- was attempted and failed — but it is not the same fact: after the rollback a
-- merge-superseded stage is indistinguishable from an operator-cancelled one.
-- The durable record of what actually happened survives in the
-- stage_superseded_by_merge audit rows, which this rollback does not touch.
-- Rolling FORWARD again does not restore the distinction; re-running 0079 up
-- only re-widens the CHECK.
UPDATE stages
   SET state = 'cancelled'
 WHERE state = 'superseded';

ALTER TABLE stages DROP CONSTRAINT stages_state_check;
ALTER TABLE stages ADD CONSTRAINT stages_state_check CHECK (
    state IN ('pending', 'awaiting_host_dispatch', 'dispatched', 'running',
              'awaiting_approval', 'awaiting_children', 'awaiting_input',
              'awaiting_scope_decision', 'awaiting_deploy_approval',
              'awaiting_deployment', 'succeeded', 'failed', 'cancelled')
);
