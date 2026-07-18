-- 0054: widen the runs runner_kind CHECK to admit 'gitlab_ci' (ADR-058 /
-- E45.8, #1861).
--
-- E45.8 stands up the GitLab pipeline dispatch backend as additive, DORMANT
-- surface. The runs.runner_kind value is a CLOSED set enforced by
-- runs_runner_kind_check — an INLINE, UNNAMED column CHECK created by
-- migration 0024 as IN ('github_actions', 'local') and never widened since
-- (0036 added the runs.runner_kind_resolved lock flag but did not touch this
-- CHECK). PostgreSQL auto-named the inline constraint runs_runner_kind_check,
-- so a run row with runner_kind='gitlab_ci' fails with SQLSTATE 23514
-- (check_violation) until the CHECK is widened to admit it — the constant
-- (run.RunnerKindGitLabCI) and this migration MUST ship together, exactly as
-- migration 0051 paired artifacts_kind_check's 'release_notes' widening with
-- KindReleaseNotes.
--
-- Growth path per ADR-022: new runner kinds extend the enum via a follow-up
-- migration that DROPs and re-ADDs the CHECK. Mirrors 0051's
-- artifacts_kind_check DROP/re-ADD.
--
-- Additive: existing 'github_actions'/'local' rows are untouched; this only
-- broadens what NEW rows may carry. No gitlab_ci run is created in this change
-- (run creation stays parked; enablement is #2043), so no data is rewritten.
ALTER TABLE runs DROP CONSTRAINT runs_runner_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_runner_kind_check CHECK (
    runner_kind IN ('github_actions', 'local', 'gitlab_ci')
);
