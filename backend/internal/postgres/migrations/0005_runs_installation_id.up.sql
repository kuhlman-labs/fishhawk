-- 0005: persist the GitHub App installation_id on the runs row.
--
-- The webhook dispatcher captures installation_id from the event
-- payload (it's required for any backend → GitHub action). The
-- orchestrator then needs to read it back to fire workflow_dispatch
-- for subsequent stages, opening PRs, commenting on issues, etc.
-- Storing it on the row avoids a per-orchestration lookup against
-- a future repos→installation mapping table.
--
-- BIGINT because GitHub installation IDs are int64. Nullable so
-- the column can be added without rewriting prior rows; the
-- runtime path treats 0 / NULL as "no GitHub action available."

ALTER TABLE runs
    ADD COLUMN installation_id BIGINT;

CREATE INDEX runs_installation_id_idx ON runs (installation_id);
