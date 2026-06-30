-- 0043: runs.upstream_run_id — the cross-workflow deploy-gate reference
-- (E23.11 / #1417).
--
-- A standalone deploy-only `release` run has no implement/review stage of
-- its own, so the deploy stage's required_upstream pre-flight constraint
-- (ci_green / review_merged) has nothing on its OWN run to evaluate. This
-- column names the upstream feature_change run whose ci_green /
-- review_merged the deploy gate evaluates instead. Nullable: every
-- appended-deploy run (a deploy stage in the same run that produced the
-- change) and every legacy row leaves it NULL → the gate evaluates the
-- CURRENT run, byte-for-byte today's behavior.
--
-- DELIBERATELY SEPARATE from parent_run_id (#216, migration 0016) by
-- operator design: parent_run_id carries the follow-up / lineage semantics
-- the get_plan plan-resolution walk, the resume/retry recovery chain, and
-- #455 decomposition provenance all key on. A safety-gating control must
-- not share a column with those consumers, so the deploy-gate pointer gets
-- its own column. Mirrors parent_run_id's 0016 FK shape: ON DELETE SET
-- NULL (deleting an upstream run nulls the reference and the gate falls
-- back to current-run evaluation — the safe direction) plus a partial
-- index over the non-null rows.

ALTER TABLE runs
    ADD COLUMN upstream_run_id UUID REFERENCES runs (id) ON DELETE SET NULL;

CREATE INDEX runs_upstream_run_id_idx ON runs (upstream_run_id)
    WHERE upstream_run_id IS NOT NULL;
