-- 0054 down: narrow the runs.runner_kind CHECK back to the pre-E45.8 set
-- ('github_actions', 'local'), dropping 'gitlab_ci'.
--
-- Safe as long as no run row carries runner_kind='gitlab_ci' — true until this
-- change enables GitLab run creation, so a rollback before any GitLab run
-- exists is clean. If a gitlab_ci run exists the re-add validation raises
-- SQLSTATE 23514; delete or re-tag those rows before applying this down
-- migration (documented in the PR's rollback plan).
ALTER TABLE runs DROP CONSTRAINT runs_runner_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_runner_kind_check
    CHECK (runner_kind IN ('github_actions', 'local'));
