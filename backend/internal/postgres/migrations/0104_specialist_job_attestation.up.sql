-- 0104: specialist-job result attestation (E85.15 / #4363).
-- backend/internal/specialistjob/store.go is the only writer.
--
-- A job admitted by a caller holding the attest:specialist-jobs scope gets a
-- job-scoped Ed25519 key: the PUBLIC half is stored here, bound to the
-- minting runner subject and a short sliding expiry decided on the database
-- clock (attestation_expires_at > clock_timestamp()). The private half is
-- never stored. A completion that verifies against the stored key records
-- provenance 'runner_attested' and consumes the key (public key and expiry
-- set NULL); every other completion records 'bearer'.
--
-- Every column is nullable with no backfill, so the pre-0104 binary keeps
-- working against this schema: its completions leave provenance NULL, which
-- the store reads as 'bearer' (the fail-safe, non-blocking direction).
--
-- Rollback order: revert the fishhawkd binary FIRST, then migrate 0104 down.
-- The 0104 binary selects these columns on every specialist-job read.
ALTER TABLE specialist_jobs
    ADD COLUMN attestation_public_key BYTEA       NULL,
    ADD COLUMN attestation_subject    TEXT        NULL,
    ADD COLUMN attestation_expires_at TIMESTAMPTZ NULL,
    ADD COLUMN provenance             TEXT        NULL CONSTRAINT specialist_jobs_provenance_check
                                      CHECK (provenance IN ('bearer', 'runner_attested')),
    -- Only a completed row records how its result was authenticated.
    ADD CONSTRAINT specialist_jobs_provenance_completed_check
        CHECK (provenance IS NULL OR state = 'completed'),
    -- An Ed25519 public key is exactly 32 bytes (crypto/ed25519.PublicKeySize).
    ADD CONSTRAINT specialist_jobs_attestation_key_length_check
        CHECK (attestation_public_key IS NULL OR octet_length(attestation_public_key) = 32);
