-- 0103: specialist_jobs — the ADR-097 D2/D6 specialist-job ledger (#4300).
-- backend/internal/specialistjob/store.go is the only writer;
-- backend/internal/concurrency reads it (jobHoldersSQL) so a RUNNING job
-- counts as a holder of its local-verify:<host> admission group.
--
-- One row per job. state 'queued' → 'running' → 'completed'; a draft job is
-- inserted 'completed' (rendered in-process, never admitted). A running job
-- whose last_seen_at heartbeat is older than concurrency.JobHeartbeatTTL no
-- longer counts as a holder and may be TAKEN OVER under a new
-- execution_nonce (approval condition C1).
--
-- No RLS, like stage_concurrency_slots (0097) and
-- merge_candidate_queue_entries (0101): account scoping is applied
-- EXPLICITLY by the store (account_id IS NOT DISTINCT FROM the caller's
-- account), so it holds even under the superuser runtime role that bypasses
-- RLS. account_id is nullable for the untenanted scope.
--
-- subject_run_id / subject_campaign_id carry NO foreign keys (like the slot
-- table's host label): a job is evidence ABOUT a run, and deleting the run
-- must never be blocked by, or cascade through, the job ledger. The audit
-- chain is the durable record.
--
-- Rollback order (approval condition C9): revert the fishhawkd binary BEFORE
-- migrating 0103 down. A binary that carries jobHoldersSQL queries this table
-- on EVERY slot admission (local-verify included), so dropping it first makes
-- every such admission error until the binary is reverted.
CREATE TABLE specialist_jobs (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id           UUID        NULL,
    kind                 TEXT        NOT NULL CONSTRAINT specialist_jobs_kind_check
                                     CHECK (kind IN ('counterfactual_battery', 'fanin_build', 'overlap_scan',
                                                     'plan_crosscheck', 'inventory', 'draft')),
    state                TEXT        NOT NULL CONSTRAINT specialist_jobs_state_check
                                     CHECK (state IN ('queued', 'running', 'completed')),
    requester_seat       TEXT        NOT NULL CONSTRAINT specialist_jobs_requester_seat_check
                                     CHECK (requester_seat IN ('operator', 'first_officer')),
    requester_actor_kind TEXT        NOT NULL CONSTRAINT specialist_jobs_requester_actor_kind_check
                                     CHECK (requester_actor_kind <> ''),
    requester_subject    TEXT        NOT NULL CONSTRAINT specialist_jobs_requester_subject_check
                                     CHECK (requester_subject <> ''),
    subject_run_id       UUID        NULL,
    subject_campaign_id  UUID        NULL,
    subject_head_sha     TEXT        NULL,
    subject_repo         TEXT        NULL,
    inputs               JSONB       NOT NULL,
    admission_group      TEXT        NULL,
    host                 TEXT        NOT NULL DEFAULT '',
    execution_nonce      TEXT        NULL,
    verdict              TEXT        NULL CONSTRAINT specialist_jobs_verdict_check
                                     CHECK (verdict IN ('pass', 'fail', 'inconclusive')),
    result               JSONB       NULL,
    requested_sequence   BIGINT      NOT NULL,
    completed_sequence   BIGINT      NULL,
    requested_at         TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    last_seen_at         TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    started_at           TIMESTAMPTZ NULL,
    completed_at         TIMESTAMPTZ NULL,
    -- A completed row carries its completion; any other row carries none.
    CONSTRAINT specialist_jobs_completed_check
        CHECK ((state = 'completed') = (completed_at IS NOT NULL AND verdict IS NOT NULL AND result IS NOT NULL)),
    -- A running row records when it started and the nonce it runs under.
    CONSTRAINT specialist_jobs_running_check
        CHECK (state <> 'running' OR (started_at IS NOT NULL AND COALESCE(execution_nonce, '') <> ''))
);

-- The per-group admission read: running holders and the FIFO among queued
-- jobs, by (requested_at, id).
CREATE INDEX specialist_jobs_admission_idx
    ON specialist_jobs (admission_group, state, requested_at, id);
