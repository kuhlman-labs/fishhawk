-- Down-migration for 0093: restore the four-value runs_trigger_source_check
-- from 0075 ('github_issue', 'cli', 'ui', 'on_demand'), dropping 'scheduled'.
--
-- THIS MIGRATION FAILS LOUDLY IF ANY scheduled RUN ROW EXISTS, AND THAT IS
-- DELIBERATE — the same posture as 0075's down migration.
--
-- Re-adding the narrower CHECK VALIDATES every existing row, so a single run
-- with trigger_source='scheduled' makes the ADD CONSTRAINT fail with SQLSTATE
-- 23514 and the whole migration roll back. Nothing is deleted and nothing is
-- silently relabelled: a down migration that DELETEd or rewrote run rows to
-- satisfy the constraint would destroy run history (and, by ON DELETE
-- CASCADE, that run's stages, artifacts and audit trail). Whether a scheduled
-- run should be deleted or re-labelled is an OPERATOR decision.
--
-- OPERATOR ACTION when this fails: enumerate the offending rows with
--     SELECT id, repo, workflow_id, created_at FROM runs
--      WHERE trigger_source = 'scheduled';
-- then explicitly delete or re-label them (and accept the cascade), and re-run
-- the rollback. Rolling 0093 back BEFORE the scheduler has started any run
-- (it is off by default behind --enable-scheduler) needs no such step.
ALTER TABLE runs
    DROP CONSTRAINT runs_trigger_source_check;

ALTER TABLE runs
    ADD CONSTRAINT runs_trigger_source_check CHECK (
        trigger_source IN ('github_issue', 'cli', 'ui', 'on_demand')
    );
