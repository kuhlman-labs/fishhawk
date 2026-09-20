-- Down-migration for 0084: drop stage_checks.gitlab_pipeline_id.
--
-- Column-only and additive (no index, no constraint, no backfill), so the
-- drop is clean. Rows written by the GitLab pipeline ingester meanwhile lose
-- only the id column — the verbatim payload still carries
-- object_attributes.id. A rolled-back reader falls back to pure ts ordering
-- for every row (GitHub rows never depended on the new key). IF EXISTS keeps
-- the rollback idempotent.
ALTER TABLE stage_checks DROP COLUMN IF EXISTS gitlab_pipeline_id;
