-- 0019: cache the validated workflow spec on the run row (#283).
--
-- The trace handler re-evaluates policy on every implement-stage
-- upload to produce the auditable `policy_evaluated` row. Pre-#283
-- it tried to refetch the spec from GitHub using `runs.workflow_sha`
-- as the contents-API `ref` parameter — but `workflow_sha` is a
-- BLOB sha (content-addressed for dedup, per the field comment),
-- not a commit / branch / tag ref. GitHub returns 404 for every
-- refetch attempt, `fetchStageConstraints` errors, and the trace
-- handler's early-return path skips the `policy_evaluated` emission.
-- Result: the SPA's <PolicySection> is stuck on "pending" forever
-- for every implement-stage run in production.
--
-- Fix: the dispatcher already parses + validates the spec at
-- run-create time. Cache the raw bytes on the run row so the trace
-- handler reads from storage rather than round-tripping to GitHub.
-- Bytea (not text) avoids re-encoding the yaml; storage is small
-- (~1KB per run); reads are by primary key.
--
-- Legacy rows (created before this migration) have NULL here. The
-- trace handler treats that as a skip-with-reason
-- (`spec_unavailable`) rather than re-introducing the GitHub fetch.
-- No backfill — v0 is pre-alpha and the historical rows are demo
-- data.

ALTER TABLE runs
    ADD COLUMN workflow_spec BYTEA;
