-- 0115 down: drop the docs/adr/112 A3 merge-gate columns and their CHECKs.
ALTER TABLE merge_candidate_queue_entries
    DROP CONSTRAINT IF EXISTS merge_candidate_queue_entries_gate_wait_check,
    DROP CONSTRAINT IF EXISTS merge_candidate_queue_entries_gate_hold_tokens_check,
    DROP COLUMN IF EXISTS approval_wait_open,
    DROP COLUMN IF EXISTS gate_tokens_refilled_at,
    DROP COLUMN IF EXISTS gate_hold_tokens,
    DROP COLUMN IF EXISTS gate_wait_epoch,
    DROP COLUMN IF EXISTS gate_wait_started_at,
    DROP COLUMN IF EXISTS row4_flag,
    DROP COLUMN IF EXISTS yield_revision;
