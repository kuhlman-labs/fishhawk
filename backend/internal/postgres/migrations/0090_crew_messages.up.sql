-- 0090: crew_messages, a DERIVED, rebuildable projection of the crew-message
-- entries on the audit chain (E77.2 / #3736, ADR-081 #3727 D2/D3 and rule 7).
--
-- The audit chain (audit_entries) stays the SOLE authority: every send and
-- every disposition is a chain entry FIRST, and every row here is
-- reconstructable from those entries. The table therefore carries NO
-- append-only trigger: it MUST be truncatable and rebuildable. It is an index,
-- not evidence — a consumer that treats a row as authoritative is reading the
-- index, not the record.
--
--   * sent_sequence IS the primary key: the chain sequence of the message's
--     crew_message_sent entry. audit_entries.sequence is a table-wide BIGSERIAL
--     (0002), unique across runs, accounts and both chains (the property 0088
--     cites for decision_index.source_sequence), so "exactly one row per
--     message" and "a rebuild yields an identical row" are STRUCTURAL.
--   * sent_entry_hash cites the sent entry's entry_hash (tamper detection).
--   * run_id is NULL for the two run-less anchors (issue_ref,
--     decision_record_id); exactly one of the three anchor columns is set.
--   * thread_root_sequence is sent_sequence for a thread root and the root's
--     sent_sequence for a reply.
--   * state is open -> accepted | rejected | expired (open is the only
--     non-terminal state; backend/internal/crewmessage/state.go).
--   * reason_sequence is a POINTER to the chain entry recording the reason —
--     the reason prose is NEVER copied here (ADR-082 rule 1's discipline).
--   * round is the thread's rejection round this disposition closed
--     (backend/internal/crewmessage/README.md).
--   * last_applied_sequence is the chain sequence of the newest entry
--     projected into the row. Every live projection is a MONOTONIC guarded
--     upsert on it (ON CONFLICT ... WHERE last_applied_sequence <
--     EXCLUDED.last_applied_sequence), so a delayed send projection can never
--     regress a row a disposition already made terminal.
--   * There is deliberately NO indexed_at / updated_at column: operational
--     metadata the chain does not carry would break full-row rebuild equality.
--
-- Tenant isolation mirrors 0057's audit_entries policy byte-for-byte (as 0088
-- and 0089 do): account_id IS NULL rows stay visible (the untenanted #1829
-- window) and an unset/empty app.account_id fails CLOSED to NULL-account rows
-- only. FORCE is required because the application connects as the table owner;
-- the 0057 superuser caveat applies here unchanged.

CREATE TABLE crew_messages (
    sent_sequence         BIGINT      PRIMARY KEY,
    sent_entry_hash       TEXT        NOT NULL,
    account_id            UUID,
    run_id                UUID        REFERENCES runs (id) ON DELETE CASCADE,
    issue_ref             TEXT        NOT NULL DEFAULT '',
    decision_record_id    TEXT        NOT NULL DEFAULT '',
    message_type          TEXT        NOT NULL,
    sender_role           TEXT        NOT NULL,
    recipient_role        TEXT        NOT NULL,
    response_required     BOOLEAN     NOT NULL DEFAULT false,
    deadline              TIMESTAMPTZ,
    thread_root_sequence  BIGINT      NOT NULL,
    state                 TEXT        NOT NULL,
    disposition_sequence  BIGINT,
    reason_sequence       BIGINT,
    round                 INT         NOT NULL DEFAULT 0,
    sent_at               TIMESTAMPTZ NOT NULL,
    disposed_at           TIMESTAMPTZ,
    last_applied_sequence BIGINT      NOT NULL
);

CREATE INDEX crew_messages_recipient_state_idx ON crew_messages (recipient_role, state);
CREATE INDEX crew_messages_run_id_idx          ON crew_messages (run_id);
CREATE INDEX crew_messages_thread_root_idx     ON crew_messages (thread_root_sequence);
CREATE INDEX crew_messages_account_id_idx      ON crew_messages (account_id);

ALTER TABLE crew_messages ENABLE ROW LEVEL SECURITY;
ALTER TABLE crew_messages FORCE ROW LEVEL SECURITY;
CREATE POLICY crew_messages_tenant_isolation ON crew_messages
    USING (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);
