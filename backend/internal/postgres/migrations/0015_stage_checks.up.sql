-- 0015: stage_checks records the state of each blocking check the
-- workflow-spec gate declares. Append-only — every status update
-- from GitHub (check_run events) writes a new row, the latest per
-- (stage_id, check_name) is what the review-stage page reads and
-- the approval handler enforces against. (#228)
--
-- Sources:
--   - GitHub `check_run` webhook → ci_pass and any other check
--     name workflow authors put in their gate's blocking_checks.
--   - Backend self-derivation (#229) → fishhawk_audit_complete and
--     future checks Fishhawk computes itself.
--
-- Indexes:
--   - (stage_id, check_name, ts DESC) for the "what's the latest
--     state of this check on this stage?" lookup the review page
--     and approval handler hit on every read / approve.
--   - (head_sha) for the ingest path to filter check_run events
--     to the runs whose pull_request artifact's head_sha matches.

CREATE TABLE stage_checks (
    id                  UUID        PRIMARY KEY,
    stage_id            UUID        NOT NULL REFERENCES stages(id) ON DELETE CASCADE,
    check_name          TEXT        NOT NULL,
    -- GitHub's check_run.status — queued / in_progress / completed.
    -- For backend-derived checks (fishhawk_audit_complete) we use
    -- the same enum; "completed" + a conclusion is the terminal
    -- shape, anything else is in-progress.
    status              TEXT        NOT NULL,
    -- GitHub's check_run.conclusion — success / failure /
    -- timed_out / cancelled / action_required / neutral / skipped /
    -- stale. NULL while status is not completed.
    conclusion          TEXT,
    -- The commit the check ran against. Lets the ingest path
    -- match a check_run event to the run's pull_request artifact
    -- via head_sha + pr_number.
    head_sha            TEXT        NOT NULL,
    -- GitHub's check_run.id; nullable for backend-derived checks
    -- that don't originate from GitHub. Used for cross-reference
    -- and dedup of redelivered events.
    github_check_run_id BIGINT,
    -- When this state was observed. For GitHub events use the
    -- check_run's completed_at / started_at / triggered timestamp;
    -- for backend-derived checks use time.Now() at compute.
    ts                  TIMESTAMPTZ NOT NULL,
    -- Verbatim event payload (or computed-state explanation), kept
    -- for forensic / audit-export use.
    payload             JSONB       NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX stage_checks_stage_check_ts_idx
    ON stage_checks (stage_id, check_name, ts DESC);

CREATE INDEX stage_checks_head_sha_idx
    ON stage_checks (head_sha);
