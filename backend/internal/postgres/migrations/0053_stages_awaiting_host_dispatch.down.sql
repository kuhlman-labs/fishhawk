-- Revert 0053: reverse the parked-for-host-dispatch backfill (map every
-- 'awaiting_host_dispatch' row back to 'dispatched') BEFORE narrowing
-- stages_state_check back to the exact 0038 set (no 'awaiting_host_dispatch').
--
-- Order matters: the state normalization must run first, because the narrower
-- CHECK re-add would raise SQLSTATE 23514 if any row still held the value it no
-- longer admits (the 0040 normalize-then-narrow precedent). After the UPDATE no
-- row holds 'awaiting_host_dispatch', so the re-add validates. This restores
-- the exact pre-split row shape — a rollback that loses no data in either
-- direction (the old binaries read every 'dispatched' row legally).
UPDATE stages
   SET state = 'dispatched'
 WHERE state = 'awaiting_host_dispatch';

ALTER TABLE stages DROP CONSTRAINT stages_state_check;
ALTER TABLE stages ADD CONSTRAINT stages_state_check CHECK (
    state IN ('pending', 'dispatched', 'running', 'awaiting_approval',
              'awaiting_children', 'awaiting_input', 'awaiting_scope_decision',
              'awaiting_deploy_approval', 'awaiting_deployment',
              'succeeded', 'failed', 'cancelled')
);
