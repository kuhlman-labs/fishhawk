-- 0001: runs and stages tables for the workflow state machine (E3.3 / #43).
--
-- Schema is a deliberate subset of the full E2.1 (#22) design: only the
-- columns and indices the run/stage state machine and basic listings need.
-- artifacts, audit_entries, signing_keys, approvals, sessions, api_tokens
-- land under their owning epics (E2, E4).

CREATE TABLE runs (
    id              UUID         PRIMARY KEY,
    repo            TEXT         NOT NULL,
    workflow_id     TEXT         NOT NULL,
    workflow_sha    TEXT         NOT NULL,
    trigger_source  TEXT         NOT NULL,
    trigger_ref     TEXT,
    state           TEXT         NOT NULL DEFAULT 'pending',
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT runs_state_check CHECK (
        state IN ('pending', 'running', 'succeeded', 'failed', 'cancelled')
    ),
    CONSTRAINT runs_trigger_source_check CHECK (
        trigger_source IN ('github_issue', 'cli', 'ui')
    )
);

CREATE INDEX runs_repo_state_idx ON runs (repo, state);
CREATE INDEX runs_created_at_idx ON runs (created_at DESC);

CREATE TABLE stages (
    id                UUID         PRIMARY KEY,
    run_id            UUID         NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    sequence          INTEGER      NOT NULL,
    stage_type        TEXT         NOT NULL,
    executor_kind     TEXT         NOT NULL,
    executor_ref      TEXT         NOT NULL,
    state             TEXT         NOT NULL DEFAULT 'pending',
    started_at        TIMESTAMPTZ,
    ended_at          TIMESTAMPTZ,
    failure_category  TEXT,
    failure_reason    TEXT,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (run_id, sequence),
    CONSTRAINT stages_state_check CHECK (
        state IN ('pending', 'dispatched', 'running', 'awaiting_approval', 'succeeded', 'failed', 'cancelled')
    ),
    CONSTRAINT stages_type_check CHECK (
        stage_type IN ('plan', 'implement', 'review')
    ),
    CONSTRAINT stages_executor_kind_check CHECK (
        executor_kind IN ('agent', 'human')
    ),
    CONSTRAINT stages_failure_category_check CHECK (
        failure_category IS NULL OR failure_category IN ('A', 'B', 'C', 'D')
    )
);

CREATE INDEX stages_run_id_idx ON stages (run_id);
CREATE INDEX stages_state_idx  ON stages (state);

-- Trigger keeps updated_at fresh on UPDATE without requiring callers to
-- remember. Both runs and stages share the helper function.
CREATE OR REPLACE FUNCTION fishhawk_set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER runs_set_updated_at   BEFORE UPDATE ON runs   FOR EACH ROW EXECUTE FUNCTION fishhawk_set_updated_at();
CREATE TRIGGER stages_set_updated_at BEFORE UPDATE ON stages FOR EACH ROW EXECUTE FUNCTION fishhawk_set_updated_at();
