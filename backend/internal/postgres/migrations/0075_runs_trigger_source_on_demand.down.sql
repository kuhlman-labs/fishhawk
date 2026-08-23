-- Down-migration for 0075: restore the three-value runs_trigger_source_check
-- from 0001 ('github_issue', 'cli', 'ui'), dropping 'on_demand'.
--
-- THIS MIGRATION FAILS LOUDLY IF ANY on_demand RUN ROW EXISTS, AND THAT IS
-- DELIBERATE — read this before running it.
--
-- Re-adding the narrower CHECK VALIDATES every existing row, so a single run
-- with trigger_source='on_demand' makes the ADD CONSTRAINT fail with SQLSTATE
-- 23514 and the whole migration roll back. Nothing is deleted and nothing is
-- silently relabelled.
--
-- That is the correct posture: the alternative — a down migration that DELETEs
-- or rewrites run rows to satisfy the constraint — would destroy run history
-- (and, by ON DELETE CASCADE, that run's stages, artifacts and audit trail) to
-- make a rollback succeed. Whether an on_demand run should be deleted or
-- re-labelled is an OPERATOR decision about their own history, not one a
-- migration should make on their behalf while their back is turned.
--
-- OPERATOR ACTION when this fails: enumerate the offending rows with
--     SELECT id, repo, workflow_id, created_at FROM runs
--      WHERE trigger_source = 'on_demand';
-- then explicitly delete or re-label them (and accept the cascade), and re-run
-- the rollback. Rolling 0075 back BEFORE any grooming run is started needs no
-- such step.
ALTER TABLE runs
    DROP CONSTRAINT runs_trigger_source_check;

ALTER TABLE runs
    ADD CONSTRAINT runs_trigger_source_check CHECK (
        trigger_source IN ('github_issue', 'cli', 'ui')
    );
