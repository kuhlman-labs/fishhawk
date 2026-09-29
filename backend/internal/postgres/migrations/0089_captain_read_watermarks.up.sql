-- 0089: captain_read_watermarks, the per-captain, per-repository READ
-- WATERMARK behind the "since you last looked" digest (E75.6 / #3734,
-- ADR-082 #3728 rule 7), plus the decision_index (repo, source_sequence) index
-- the digest's windowed read of the index is served by.
--
--   * sequence is a position on audit_entries.sequence — a table-wide BIGSERIAL
--     (0002), so one integer is a sound cross-run watermark for a repository.
--     It is the LAST sequence the captain has read (inclusive); the digest's
--     default window is (sequence, chain head].
--   * The row advances ONLY on an explicit mark-read, never on retrieval, and
--     only AFTER the digest_marked_read chain entry is appended (the ordering
--     lives in backend/internal/digest/watermark.go). It never moves backwards:
--     the upsert's ON CONFLICT ... WHERE sequence < EXCLUDED.sequence guard.
--   * Uniqueness is a UNIQUE expression index over COALESCE(account_id, the nil
--     UUID) rather than a PK over the nullable account_id or NULLS NOT DISTINCT
--     — the conservatism 0068/0080/0081/0086 state in their own headers. A plain
--     UNIQUE (captain_subject, repo, account_id) treats NULLs as distinct, so an
--     untenanted captain would accumulate duplicate rows.
--   * The row is operational state, NOT a chain fact: every fact the digest
--     reports already lives on the chain, so dropping the table loses nothing
--     but read positions.
--
-- Tenant isolation is byte-identical to 0088's decision_index policy (itself
-- 0057's audit_entries predicate): account_id IS NULL rows stay visible (the
-- untenanted #1829 window) and an unset/empty app.account_id fails CLOSED to
-- NULL-account rows only. FORCE is required because the application connects
-- as the table owner; the 0057 superuser caveat applies unchanged.

CREATE TABLE captain_read_watermarks (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id      UUID,
    captain_subject TEXT        NOT NULL,
    repo            TEXT        NOT NULL,
    sequence        BIGINT      NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX captain_read_watermarks_key_idx ON captain_read_watermarks
    (captain_subject, repo, COALESCE(account_id, '00000000-0000-0000-0000-000000000000'::uuid));

ALTER TABLE captain_read_watermarks ENABLE ROW LEVEL SECURITY;
ALTER TABLE captain_read_watermarks FORCE ROW LEVEL SECURITY;
CREATE POLICY captain_read_watermarks_tenant_isolation ON captain_read_watermarks
    USING (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);

-- The digest reads decision_index rows by repo over a source_sequence window.
CREATE INDEX decision_index_repo_sequence_idx ON decision_index (repo, source_sequence);
