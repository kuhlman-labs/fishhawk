-- 0097: stage_concurrency_slots — the FIFO queue + held marks behind local
-- stage concurrency groups (#3964 / ADR-087). backend/internal/concurrency is
-- the only reader and writer; its README is the contract.
--
-- One row per stage that has asked for a slot. state='queued' is a waiting
-- dispatch; state='held' marks the dispatch attempt the row ADMITTED
-- (held_dispatched_at = the stages.dispatched_at the admission stamped).
-- Holding is DERIVED from the stage, never from this row alone: a held row
-- counts only while its stage is still dispatched/running on that SAME
-- attempt and live (heartbeat-keyed backstop), so a settled, parked or
-- crashed holder releases without any write here.
--
-- No account_id and no RLS, like repo_acl_entries: this is host coordination
-- metadata keyed by stage. Account scoping is applied explicitly by the store
-- (it joins runs.account_id), so it holds even under the superuser runtime
-- role that bypasses RLS.
--
-- admission_nonce is the slot waiter's per-waiter random nonce the marker
-- recorded when it ADMITTED the row (NULL while queued, and for an admission
-- whose request carried none). A waiter that lost its admission response
-- claims the admission only when this equals its own nonce, so another
-- session's admission of the same stage is never mistaken for its own.
--
-- ON DELETE CASCADE from stages, so deleting a stage (for example
-- DeletePendingAcceptanceStage) is never blocked by its slot row.
CREATE TABLE stage_concurrency_slots (
    stage_id           UUID        PRIMARY KEY REFERENCES stages (id) ON DELETE CASCADE,
    run_id             UUID        NOT NULL,
    group_key          TEXT        NOT NULL,
    slot_limit         INT         NOT NULL CONSTRAINT stage_concurrency_slots_limit_check CHECK (slot_limit BETWEEN 1 AND 64),
    host               TEXT        NOT NULL DEFAULT '',
    state              TEXT        NOT NULL CONSTRAINT stage_concurrency_slots_state_check CHECK (state IN ('queued', 'held')),
    enqueued_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    acquired_at        TIMESTAMPTZ NULL,
    held_dispatched_at TIMESTAMPTZ NULL,
    admission_nonce    TEXT        NULL
);

CREATE INDEX stage_concurrency_slots_group_idx
    ON stage_concurrency_slots (group_key, state, enqueued_at, stage_id);

-- fishhawk_stage_heartbeat_at reads stages.progress->'reported_at' (the
-- DB-stamped heartbeat RecordStageProgress writes) as a timestamptz, or NULL
-- when it is absent, not a JSON string, not an ISO-8601 date-time shape, or
-- unparsable. The shape check refuses timestamptz special inputs ('now',
-- 'infinity', 'epoch') that would otherwise make a holder look live forever;
-- the EXCEPTION arm turns a shape-valid but out-of-range value (month 13) into
-- NULL rather than an error, so one tampered row can never wedge admission
-- for its whole group. A NULL falls back to stages.dispatched_at in the
-- store's GREATEST(...).
CREATE FUNCTION fishhawk_stage_heartbeat_at(progress JSONB) RETURNS TIMESTAMPTZ
LANGUAGE plpgsql STABLE AS $$
DECLARE
    raw TEXT;
BEGIN
    IF progress IS NULL OR jsonb_typeof(progress -> 'reported_at') IS DISTINCT FROM 'string' THEN
        RETURN NULL;
    END IF;
    raw := progress ->> 'reported_at';
    IF raw !~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}(:?[0-9]{2})?)$' THEN
        RETURN NULL;
    END IF;
    RETURN raw::timestamptz;
EXCEPTION WHEN others THEN
    RETURN NULL;
END;
$$;
