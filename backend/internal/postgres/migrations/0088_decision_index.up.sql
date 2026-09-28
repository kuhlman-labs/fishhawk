-- 0088: decision_index, a DERIVED, rebuildable projection of the
-- decision-bearing audit entries (E75.2 / #3730, ADR-082 #3728 decision (a)2).
--
-- The audit chain (audit_entries) stays the SOLE authority. Every row here is
-- reconstructable from the chain plus its run/stage/plan-artifact joins, so the
-- table carries NO append-only trigger: it MUST be truncatable and rebuildable
-- (`fishhawkd decision-index backfill --rebuild`).
--
--   * source_sequence IS the primary key. audit_entries.sequence is a
--     table-wide BIGSERIAL (0002), so it is unique across runs, accounts and
--     both chains — which makes "exactly one row per decision-bearing entry"
--     and "a rebuild yields a byte-identical index" STRUCTURAL rather than a
--     convention an upsert has to defend.
--   * source_entry_hash cites the source entry's entry_hash (tamper detection).
--   * reason_sequence is a POINTER to the chain entry recording the reason and
--     reason_key names the payload key holding it — the prose is NEVER copied
--     (ADR-082 rule 1).
--   * doctrine_version carries runs.workflow_sha today; ADR-082 rule 4 names
--     the charter revision once E71.2 (#3242) binds it at admission — a
--     column-VALUE change with no schema change, not implemented here.
--   * reject_class is approval_submitted's E75.1 (#3729) structured rejection
--     class; escalation_keys are the fired_keys of the latest escalation_fired
--     entry on the SAME stage below the decision (sorted, de-duplicated).
--     Historical entries predating E75.1 index both as empty.
--   * There is deliberately NO indexed_at column: operational metadata the
--     chain does not carry would break full-row rebuild equality.
--
-- Tenant isolation mirrors 0057's audit_entries policy byte-for-byte:
-- account_id IS NULL rows stay visible (the untenanted #1829 window) and an
-- unset/empty app.account_id fails CLOSED to NULL-account rows only. FORCE is
-- required because the application connects as the table owner; the 0057
-- superuser caveat applies here unchanged.

CREATE TABLE decision_index (
    source_sequence           BIGINT      PRIMARY KEY,
    source_entry_hash         TEXT        NOT NULL,
    run_id                    UUID        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    stage_id                  UUID        REFERENCES stages (id) ON DELETE SET NULL,
    account_id                UUID,
    repo                      TEXT        NOT NULL,
    workflow_id               TEXT        NOT NULL,
    doctrine_version          TEXT        NOT NULL,
    decision_class            TEXT        NOT NULL,
    stage_kind                TEXT        NOT NULL DEFAULT '',
    outcome                   TEXT        NOT NULL DEFAULT '',
    reject_class              TEXT        NOT NULL DEFAULT '',
    concern_category_raw      TEXT        NOT NULL DEFAULT '',
    concern_category          TEXT        NOT NULL DEFAULT '',
    concern_category_unmapped BOOLEAN     NOT NULL DEFAULT false,
    severity                  TEXT        NOT NULL DEFAULT '',
    touched_paths             TEXT[]      NOT NULL DEFAULT '{}',
    escalation_keys           TEXT[]      NOT NULL DEFAULT '{}',
    delegated                 BOOLEAN     NOT NULL DEFAULT false,
    actor_kind                TEXT        NOT NULL DEFAULT '',
    actor_subject             TEXT        NOT NULL DEFAULT '',
    decided_at                TIMESTAMPTZ NOT NULL,
    reason_sequence           BIGINT      NOT NULL,
    reason_key                TEXT        NOT NULL DEFAULT ''
);

-- ADR-082 rule 3's hard filter: precedent is matched on repo + decision class +
-- stage kind before anything softer.
CREATE INDEX decision_index_repo_class_kind_idx ON decision_index (repo, decision_class, stage_kind);
CREATE INDEX decision_index_run_id_idx          ON decision_index (run_id);
CREATE INDEX decision_index_account_id_idx      ON decision_index (account_id);

ALTER TABLE decision_index ENABLE ROW LEVEL SECURITY;
ALTER TABLE decision_index FORCE ROW LEVEL SECURITY;
CREATE POLICY decision_index_tenant_isolation ON decision_index
    USING (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);
