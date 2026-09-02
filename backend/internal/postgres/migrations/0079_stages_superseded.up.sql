-- Migration 0079: widen stages_state_check to admit the merge-supersede
-- terminal stage state 'superseded' (E64.2 / #3083).
--
-- #3083: a merge can leave a stage parked and unreachable — a fix-up pass
-- re-parks acceptance at 'awaiting_host_dispatch', or a review gate sits at
-- 'awaiting_approval' on a change that has already merged. Nothing
-- re-dispatches the parked stage, and Orchestrator.completeRun's #968 guard
-- correctly refuses to stamp the run succeeded around a non-terminal stage, so
-- the run strands in 'running' forever. Every existing escape hatch records
-- something untrue: reap writes 'failed' (the work was never attempted) and
-- cancel writes 'cancelled' (no operator halted anything).
--
-- The Go model + a default-deny (stage_type, state) transition table learned
-- the new state; this migration teaches the stages_state_check CHECK (unchanged
-- since 0053's awaiting_host_dispatch widening) about it so a real 'superseded'
-- row is insertable — without the widening it is uninsertable, SQLSTATE 23514,
-- mirroring the 0035/0038/0053 precedent.
--
-- NO backfill and NO column change. Nothing writes 'superseded' yet, so there
-- is no legacy row to migrate: this migration only broadens what NEW rows may
-- carry, and every existing stage row is untouched.
ALTER TABLE stages DROP CONSTRAINT stages_state_check;
ALTER TABLE stages ADD CONSTRAINT stages_state_check CHECK (
    state IN ('pending', 'awaiting_host_dispatch', 'dispatched', 'running',
              'awaiting_approval', 'awaiting_children', 'awaiting_input',
              'awaiting_scope_decision', 'awaiting_deploy_approval',
              'awaiting_deployment', 'succeeded', 'failed', 'cancelled',
              'superseded')
);
