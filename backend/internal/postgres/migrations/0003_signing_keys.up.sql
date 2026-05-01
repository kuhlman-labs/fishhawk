-- 0003: signing_keys table — per-run Ed25519 public keys for trace
-- bundle verification (E2.3 / #24).
--
-- Per ADR-008 (#72), the backend mints a fresh keypair at run-start,
-- stores the public half here keyed by run_id, and returns the
-- private half to the runner over the response body. The runner
-- signs sha256(raw_bundle_bytes) and ships (bundle, signature) to
-- the backend, which verifies against this public key before
-- accepting the trace.
--
-- Rows are immutable once written: we want the (run_id, public_key)
-- pair to be a stable, externally-verifiable record forever, the
-- same way audit_entries are. UPDATE/DELETE blocked by triggers
-- below, mirroring the audit_entries pattern from migration 0002.

CREATE TABLE signing_keys (
    run_id      UUID         PRIMARY KEY REFERENCES runs (id) ON DELETE RESTRICT,
    public_key  BYTEA        NOT NULL,
    issued_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ  NOT NULL,
    CONSTRAINT signing_keys_public_key_size_check
        CHECK (octet_length(public_key) = 32)
);

CREATE INDEX signing_keys_expires_at_idx ON signing_keys (expires_at);

CREATE OR REPLACE FUNCTION fishhawk_signing_keys_no_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'signing_keys is append-only; UPDATE/DELETE is forbidden';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER signing_keys_no_update
    BEFORE UPDATE ON signing_keys
    FOR EACH ROW EXECUTE FUNCTION fishhawk_signing_keys_no_mutation();

CREATE TRIGGER signing_keys_no_delete
    BEFORE DELETE ON signing_keys
    FOR EACH ROW EXECUTE FUNCTION fishhawk_signing_keys_no_mutation();
