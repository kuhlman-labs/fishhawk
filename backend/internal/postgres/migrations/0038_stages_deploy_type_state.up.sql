-- Migration 0038: widen stages_type_check to admit the 'deploy' stage type
-- and stages_state_check to admit the two deploy stage states
-- 'awaiting_deploy_approval' and 'awaiting_deployment'.
--
-- ADR-038's deploy stage TYPE and its two pre/in-flight STATES landed in
-- the Go model and transition table in E23.4 (#1384), but that slice
-- shipped "no migration this slice," so the stages table's
-- stages_type_check (plan/implement/review, migration 0001) and
-- stages_state_check (the 0035 set) never learned the deploy values. A
-- real deploy stage row is therefore uninsertable (SQLSTATE 23514,
-- check_violation), leaving the merged E23.4–E23.10 deploy backend inert
-- end-to-end (surfaced #1390 / #1399).
--
-- Additive: this only broadens what NEW rows may carry; existing
-- plan/implement/review rows and the 0035 state set are untouched. No
-- column add/drop and no data migration. DeployOutcome is in-memory-only
-- (no deploy_outcome column / no outcome CHECK yet — persistence is a
-- downstream slice), and stages_executor_kind_check (agent/human) needs
-- no change: a deploy stage is created with a valid existing executor
-- kind.
ALTER TABLE stages DROP CONSTRAINT stages_type_check;
ALTER TABLE stages ADD CONSTRAINT stages_type_check CHECK (
    stage_type IN ('plan', 'implement', 'review', 'deploy')
);

ALTER TABLE stages DROP CONSTRAINT stages_state_check;
ALTER TABLE stages ADD CONSTRAINT stages_state_check CHECK (
    state IN ('pending', 'dispatched', 'running', 'awaiting_approval',
              'awaiting_children', 'awaiting_input', 'awaiting_scope_decision',
              'awaiting_deploy_approval', 'awaiting_deployment',
              'succeeded', 'failed', 'cancelled')
);
