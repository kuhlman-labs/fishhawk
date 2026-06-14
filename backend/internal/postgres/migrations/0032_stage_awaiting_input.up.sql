-- Migration 0032: widen stages_state_check to admit 'awaiting_input'.
--
-- awaiting_input is a non-terminal stage state the plan stage enters
-- when the planner emits a clarification_request artifact (#1057): the
-- run parks for operator direction — a parked D-category judgment, NOT
-- a failure — and resumes in the SAME run once the answers arrive via
-- the #558 binding-conditions channel. The CHECK constraint rejects the
-- value until widened here.

ALTER TABLE stages DROP CONSTRAINT stages_state_check;
ALTER TABLE stages ADD CONSTRAINT stages_state_check CHECK (
    state IN ('pending', 'dispatched', 'running', 'awaiting_approval',
              'awaiting_children', 'awaiting_input', 'succeeded', 'failed',
              'cancelled')
);
