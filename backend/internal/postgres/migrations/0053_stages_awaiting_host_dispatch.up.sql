-- Migration 0053: widen stages_state_check to admit the parked-for-host-
-- dispatch stage state 'awaiting_host_dispatch' (#1912), then backfill the
-- existing local rows that today conflate "parked, no spawn attempt" with
-- "spawn attempt exists" under a single 'dispatched' value.
--
-- #1912 splits the old conflated local 'dispatched' state into two explicit
-- signals: 'awaiting_host_dispatch' (the backend wants this agent stage
-- executed but the runner is host-spawned per ADR-024 and no spawn attempt
-- exists yet) and 'dispatched' (a spawn attempt now exists — workflow_dispatch
-- fired or a host spawn was marked). The Go model + transition table learned
-- the new state; this migration teaches the stages_state_check CHECK
-- (unchanged since 0038's deploy-state widening) about it so a real
-- awaiting_host_dispatch row is insertable (without the widening it is
-- uninsertable, SQLSTATE 23514, mirroring the 0035/0038 precedent).
--
-- Backfill: a stage currently 'dispatched' with started_at IS NULL, on a
-- non-terminal run whose runner_kind is 'local', has never been spawned (a
-- host-spawned runner stamps started_at only on the runner's prompt-fetch
-- liveness flip, #1924) — it is a parked-for-host-dispatch stage under the old
-- conflated encoding. Flip exactly those to 'awaiting_host_dispatch'.
--
-- Deliberately conservative: a re-opened stage carrying a PRIOR attempt's
-- started_at is SKIPPED (started_at IS NOT NULL) and remains 'dispatched' —
-- the surviving read-side stale-threshold arm plus manual re-dispatch tolerate
-- it, rather than risk mis-parking a stage whose runner really did spawn. Only
-- broadens what NEW rows may carry plus this one conservative UPDATE; no column
-- add/drop.
ALTER TABLE stages DROP CONSTRAINT stages_state_check;
ALTER TABLE stages ADD CONSTRAINT stages_state_check CHECK (
    state IN ('pending', 'awaiting_host_dispatch', 'dispatched', 'running',
              'awaiting_approval', 'awaiting_children', 'awaiting_input',
              'awaiting_scope_decision', 'awaiting_deploy_approval',
              'awaiting_deployment', 'succeeded', 'failed', 'cancelled')
);

UPDATE stages
   SET state = 'awaiting_host_dispatch'
 WHERE state = 'dispatched'
   AND started_at IS NULL
   AND run_id IN (
       SELECT id FROM runs
        WHERE runner_kind = 'local'
          AND state NOT IN ('succeeded', 'failed', 'cancelled')
   );
