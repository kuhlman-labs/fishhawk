-- 0054 down: restore the two-value runs runner_kind CHECK ('github_actions',
-- 'local'). Any 'gitlab_ci' run rows written while 0054 was applied would
-- violate the restored CHECK; this down migration assumes the rollback runs
-- before any gitlab_ci run is created (the additive-change rollback contract —
-- revert before the new kind is used). Because run creation stays parked
-- (#2043 owns enablement), no gitlab_ci row is ever written, so the down is
-- always safe and needs no data cleanup. Existing 'github_actions'/'local'
-- rows are untouched.
ALTER TABLE runs DROP CONSTRAINT runs_runner_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_runner_kind_check CHECK (
    runner_kind IN ('github_actions', 'local')
);
