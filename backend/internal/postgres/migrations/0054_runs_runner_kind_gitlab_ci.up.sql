-- Migration 0054: widen the runs.runner_kind CHECK to admit the second
-- host-dispatched backend, 'gitlab_ci' (E45.8 / #1861).
--
-- 0024 (#388 / ADR-022) introduced runner_kind as a closed-set column with an
-- inline column-level CHECK enumerating the v0 backends
-- (runs_runner_kind_check: runner_kind IN ('github_actions', 'local')). E45.8
-- adds a GitLabCI RunnerBackend whose dispatch fires a GitLab pipeline instead
-- of a GitHub Actions workflow_dispatch; without widening this CHECK a run
-- stamped runner_kind='gitlab_ci' at create/unpark time is uninsertable
-- (SQLSTATE 23514), mirroring the 0053/0044/0038 CHECK-widening precedent.
--
-- Purely additive: the widening only broadens what NEW rows may carry. No
-- column add/drop, no data backfill (every legacy row already carries an
-- admitted value), and the DEFAULT stays 'github_actions' from 0024.
--
-- The re-add restates the inline CHECK's original name (runs_runner_kind_check)
-- so a future migration can DROP it by the same stable identifier.
ALTER TABLE runs DROP CONSTRAINT runs_runner_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_runner_kind_check
    CHECK (runner_kind IN ('github_actions', 'local', 'gitlab_ci'));
