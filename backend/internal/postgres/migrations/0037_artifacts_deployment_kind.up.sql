-- 0037: widen the artifacts kind CHECK to admit 'deployment' (E23.5 /
-- #1385, ADR-038 / #925).
--
-- ADR-038's deploy stage records a signed `deployment` artifact
-- ({environment, ref/sha, external_run_url, outcome, rollback_handle})
-- as the governance record of a delegated release. The artifacts kind
-- is a CLOSED set enforced by `artifacts_kind_check` (migration 0002),
-- so a Create with the new kind fails with SQLSTATE 23514
-- (check_violation) until the CHECK is widened to admit it.
--
-- Additive: existing 'plan'/'pull_request' rows are untouched; this only
-- broadens what NEW rows may carry. The deploy audit categories
-- (deployment_dispatched / deployment_outcome_recorded /
-- deployment_rollback_initiated / deployment_rollback_completed) need NO
-- migration — audit_entries.category is TEXT NOT NULL with only an index
-- (no CHECK), so they are pure open-set strings.
ALTER TABLE artifacts DROP CONSTRAINT artifacts_kind_check;
ALTER TABLE artifacts ADD CONSTRAINT artifacts_kind_check CHECK (
    kind IN ('plan', 'pull_request', 'deployment')
);
