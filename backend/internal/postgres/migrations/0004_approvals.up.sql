-- 0004: approvals table — record of who approved or rejected the
-- gate on a stage, when, and from what surface (E3.5 / #45).
--
-- Per MVP_SPEC §4.2 + §6, every gated stage either advances on
-- approve, fails as category D on reject, or fails as category D
-- when the SLA elapses (E3.5 follow-up; #109 follow-up).
--
-- Idempotency is enforced at the schema layer: each (stage_id,
-- approver_subject) pair is unique, so a re-submission from the
-- same approver is an ON CONFLICT DO NOTHING returning the
-- existing row. Rows are append-only — UPDATE/DELETE is blocked
-- by the trigger below, the same way audit_entries are.
--
-- Stage transitions on approve/reject are NOT done in this table;
-- the application layer holds a SELECT FOR UPDATE on the stage
-- row inside the same transaction as the INSERT, so concurrent
-- approvers can't fork the state machine.

CREATE TABLE approvals (
    id                UUID         PRIMARY KEY,
    stage_id          UUID         NOT NULL REFERENCES stages (id) ON DELETE RESTRICT,
    approver_subject  TEXT         NOT NULL,
    decision          TEXT         NOT NULL,
    comment           TEXT,
    surface           TEXT         NOT NULL DEFAULT 'api',
    submitted_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT approvals_decision_check
        CHECK (decision IN ('approve', 'reject')),
    CONSTRAINT approvals_surface_check
        CHECK (surface IN ('api', 'ui', 'cli', 'github_comment')),
    CONSTRAINT approvals_unique_approver
        UNIQUE (stage_id, approver_subject)
);

CREATE INDEX approvals_stage_idx ON approvals (stage_id);
CREATE INDEX approvals_submitted_at_idx ON approvals (submitted_at);

CREATE OR REPLACE FUNCTION fishhawk_approvals_no_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'approvals is append-only; UPDATE/DELETE is forbidden';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER approvals_no_update
    BEFORE UPDATE ON approvals
    FOR EACH ROW EXECUTE FUNCTION fishhawk_approvals_no_mutation();

CREATE TRIGGER approvals_no_delete
    BEFORE DELETE ON approvals
    FOR EACH ROW EXECUTE FUNCTION fishhawk_approvals_no_mutation();
