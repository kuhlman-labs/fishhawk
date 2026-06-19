-- Migration 0035: widen stages_state_check to admit
-- 'awaiting_scope_decision' and add the scope_completeness_park payload
-- column.
--
-- awaiting_scope_decision is a non-terminal stage state the implement
-- stage enters when its ONLY committed-tree gate failure is the
-- scope-completeness "missing declared scope file(s)" check (#1151): the
-- runner has already pushed its verified commit to the run branch (no
-- PR), and the run parks for an operator exempt-or-fail decision (#1231)
-- — a parked judgment, NOT a failure, resolved in-band with zero agent
-- re-run on exempt. The CHECK constraint rejects the value until widened
-- here (mirrors 0032's awaiting_input precedent).
--
-- scope_completeness_park is the durable held-commit payload the park
-- carries: {held_commit_sha, run_branch, verified_tree_sha,
-- missing_paths}. NULL for every stage not parked for a scope decision.

ALTER TABLE stages DROP CONSTRAINT stages_state_check;
ALTER TABLE stages ADD CONSTRAINT stages_state_check CHECK (
    state IN ('pending', 'dispatched', 'running', 'awaiting_approval',
              'awaiting_children', 'awaiting_input', 'awaiting_scope_decision',
              'succeeded', 'failed', 'cancelled')
);

ALTER TABLE stages ADD COLUMN scope_completeness_park JSONB;
