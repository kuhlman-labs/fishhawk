-- 0020: track auto-retry chain depth on the run row (#279).
--
-- E16 (#276) auto-dispatches a fresh implement run when a required
-- CI check fails on a Fishhawk-managed PR. The retry handler in
-- #279 reads runs.retry_attempt to enforce the spec's
-- on_ci_failure.max_retries cap (#277): the dispatcher refuses to
-- create a new child run when `retry_attempt >= max_retries` and
-- emits a `ci_retry_exhausted` audit row instead of looping.
--
-- The column also surfaces on the SPA as a "Retry N/M" badge
-- (#280) — operators see at a glance whether the run they're
-- looking at is the original or a retry.
--
-- Default 0 covers legacy rows (created before this migration)
-- and the canonical "first attempt" case. Retries are explicit:
-- the dispatcher passes RetryAttempt = parent.RetryAttempt + 1
-- when creating the follow-up run.
--
-- No index: lookup is by PK (run_id); the cap check happens
-- in-memory on a single fetched row.

ALTER TABLE runs
    ADD COLUMN retry_attempt INT NOT NULL DEFAULT 0;
