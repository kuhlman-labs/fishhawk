-- 0002: artifacts and audit_entries tables completing the v0 audit
-- log schema (E2.1 / #22).
--
-- artifacts holds typed stage outputs (plans, pull-request refs).
-- Small artifacts (plans validated against standard_v1, PR metadata)
-- live inline as JSONB; trace bundles ship to S3 and are tracked in
-- a future trace_bundles table under E2.2.
--
-- audit_entries is the append-only event log. Application code only
-- INSERTs; UPDATE and DELETE are blocked by triggers below as
-- belt-and-suspenders against a buggy code path or careless DBA.
-- The chain-hash columns (prev_hash, entry_hash) carry tamper-
-- evidence per run; the application layer (E2.5 / #26) populates
-- them on Append.

CREATE TABLE artifacts (
    id              UUID         PRIMARY KEY,
    stage_id        UUID         NOT NULL REFERENCES stages (id) ON DELETE CASCADE,
    kind            TEXT         NOT NULL,
    schema_version  TEXT,
    content         JSONB        NOT NULL,
    content_hash    TEXT         NOT NULL,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT artifacts_kind_check CHECK (
        kind IN ('plan', 'pull_request')
    )
);

CREATE INDEX artifacts_stage_id_idx     ON artifacts (stage_id);
CREATE INDEX artifacts_content_hash_idx ON artifacts (content_hash);

CREATE TABLE audit_entries (
    id             UUID         PRIMARY KEY,
    sequence       BIGSERIAL    NOT NULL,
    run_id         UUID         NOT NULL REFERENCES runs (id) ON DELETE RESTRICT,
    stage_id       UUID         REFERENCES stages (id) ON DELETE RESTRICT,
    ts             TIMESTAMPTZ  NOT NULL DEFAULT now(),
    category       TEXT         NOT NULL,
    actor_kind     TEXT,
    actor_subject  TEXT,
    payload        JSONB        NOT NULL,
    prev_hash      TEXT,
    entry_hash     TEXT         NOT NULL,
    CONSTRAINT audit_entries_actor_kind_check CHECK (
        actor_kind IS NULL OR actor_kind IN ('agent', 'user', 'system')
    )
);

CREATE INDEX audit_entries_run_seq_idx     ON audit_entries (run_id, sequence);
CREATE INDEX audit_entries_ts_idx          ON audit_entries (ts);
CREATE INDEX audit_entries_category_idx    ON audit_entries (category);

-- Append-only enforcement at the database layer. RAISE EXCEPTION
-- short-circuits any UPDATE or DELETE before rows change. DROP
-- TABLE / TRUNCATE remain available to migrations (which run with
-- the migration role); production runs of fishhawkd have a
-- least-privileged role that does not have those grants.
CREATE OR REPLACE FUNCTION fishhawk_audit_no_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'audit_entries is append-only; UPDATE/DELETE is forbidden';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER audit_entries_no_update
    BEFORE UPDATE ON audit_entries
    FOR EACH ROW EXECUTE FUNCTION fishhawk_audit_no_mutation();

CREATE TRIGGER audit_entries_no_delete
    BEFORE DELETE ON audit_entries
    FOR EACH ROW EXECUTE FUNCTION fishhawk_audit_no_mutation();
