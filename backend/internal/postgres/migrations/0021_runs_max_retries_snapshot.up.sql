-- 0021: snapshot the workflow's on_ci_failure.max_retries cap on
-- the run row (#280).
--
-- E16's auto-retry path reads on_ci_failure.max_retries from the
-- cached workflow spec at handler time. The SPA needs the same
-- value to render a "Retry N/M" badge on the run-detail header —
-- without snapshotting it on the run row, the SPA would have to
-- fetch + parse the workflow YAML itself.
--
-- Snapshot semantics match runs.required_checks_snapshot (#251):
-- captured at run-create time and immutable for the life of the
-- run, so spec edits during a long-running auto-retry chain
-- don't shift the goalposts on what the SPA shows.
--
-- Default 1 mirrors spec.DefaultMaxRetries: legacy rows (created
-- before this migration) get the same cap a freshly-created run
-- on a spec without an `on_ci_failure:` block would. New rows
-- always populate it explicitly from the parsed spec; the default
-- only catches the upgrade path.
--
-- No index: read by PK (run_id) on the same row the SPA already
-- fetches for the run-detail page.

ALTER TABLE runs
    ADD COLUMN max_retries_snapshot INT NOT NULL DEFAULT 1;
