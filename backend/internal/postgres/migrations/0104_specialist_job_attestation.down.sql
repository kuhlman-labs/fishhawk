-- 0104 down: drop the specialist-job attestation columns and their checks.
-- Completed rows lose their recorded provenance; the audit chain's
-- specialist_job_completed payloads keep it.
--
-- Rollback ORDER: revert the fishhawkd binary FIRST, then run this down
-- migration. The 0104 binary selects these columns on every specialist-job
-- read, so dropping them under it fails every such read until it is reverted.
ALTER TABLE specialist_jobs
    DROP CONSTRAINT IF EXISTS specialist_jobs_attestation_key_length_check,
    DROP CONSTRAINT IF EXISTS specialist_jobs_provenance_completed_check,
    DROP COLUMN IF EXISTS provenance,
    DROP COLUMN IF EXISTS attestation_expires_at,
    DROP COLUMN IF EXISTS attestation_subject,
    DROP COLUMN IF EXISTS attestation_public_key;
