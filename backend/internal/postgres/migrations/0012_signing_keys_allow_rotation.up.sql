-- 0012: signing_keys — allow multiple keys per run.
--
-- Original 0003 modeled signing_keys.run_id as PRIMARY KEY: one
-- key per run, immutable forever, "externally-verifiable record."
-- That model assumed the runner ran in one process for the whole
-- run lifetime. v0's multi-stage workflows break that assumption:
-- each stage is a separate GitHub Actions runner invocation with
-- no shared in-memory state, so the implement stage's runner has
-- no way to obtain the plan stage's private key. It must issue
-- its own.
--
-- New model: one row per (Issue call). Verify picks the latest
-- unexpired key for the run so the runner-side caller doesn't have
-- to track which key signed which payload. History is preserved so
-- the standalone audit-log verifier (verifier/) can still walk
-- every key that was active at any point during the run.
--
-- The append-only triggers from 0003 stay — UPDATE/DELETE are still
-- forbidden. Rotation happens via additive INSERT, not mutation.

-- A synthetic primary key replaces the run_id PK. Each Issue call
-- gets its own id; run_id becomes a regular indexed column.
ALTER TABLE signing_keys
    DROP CONSTRAINT signing_keys_pkey;

ALTER TABLE signing_keys
    ADD COLUMN id UUID NOT NULL DEFAULT gen_random_uuid();

ALTER TABLE signing_keys
    ADD CONSTRAINT signing_keys_pkey PRIMARY KEY (id);

CREATE INDEX signing_keys_run_id_issued_at_idx
    ON signing_keys (run_id, issued_at DESC);
