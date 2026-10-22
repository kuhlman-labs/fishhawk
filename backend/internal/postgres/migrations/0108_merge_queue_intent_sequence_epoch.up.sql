-- 0108: ADR-093 slice 2 (merge-on-pass, E87.5 / #4296).
--
-- merge_candidate_queue_entries.merge_intent_sequence is the audit sequence
-- of the merge_queue_entered row that recorded the operator's merge intent on
-- this episode. backend/internal/mergequeue stamps it in the SAME statement
-- as merge_intent_at, only on the NULL-to-set transition, and never
-- overwrites it. NULL = no intent, or an intent recorded before 0108 (read as
-- 0: merge-on-pass then resolves the newest merge_queue_entered row naming
-- the base). No backfill.
--
-- merge_candidate_queue_entries.admission_epoch is bumped by every admission
-- (AdmitNext's admit AND its stale re-claim). Unadmit, SetPhase, ReanchorAt
-- and SettleAt are compare-and-swaps on it, so a decision made against an
-- earlier admission changes nothing (mergequeue.ErrStateConflict).
--
-- merge_dispatch_claims is the per-run merge-dispatch ledger (approval
-- condition D1): at most ONE dispatch claim per run, DATABASE-enforced by the
-- UNIQUE run_id constraint, taken by every merge dispatch path (queue
-- merge-on-pass, the inline merge_run, the delegated may_merge arm) under the
-- run's merge-intent lock and BEFORE the forge is called. A forge refusal that
-- applied nothing (checks pending, 405/409/422) deletes the exact claim (a
-- compare-and-swap); any other failure updates it to dispatch_failed, which
-- Fishhawk never deletes: only the operator re-arm (mergequeue README §
-- "Merge-dispatch ledger") removes a dispatch_failed claim.
--
-- Additive: one nullable column, one defaulted column and a new table, so a
-- reverted binary keeps processing rows that carry them.
ALTER TABLE merge_candidate_queue_entries
    ADD COLUMN merge_intent_sequence BIGINT NULL,
    ADD COLUMN admission_epoch BIGINT NOT NULL DEFAULT 0;

CREATE TABLE merge_dispatch_claims (
    run_id        UUID        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    state         TEXT        NOT NULL CHECK (state IN ('dispatched', 'dispatch_failed')),
    path          TEXT        NOT NULL CHECK (path IN ('inline', 'delegated', 'queue')),
    head_sha      TEXT        NOT NULL CHECK (head_sha <> ''),
    claimed_at    TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    failed_at     TIMESTAMPTZ NULL,
    failure_cause TEXT        NULL,
    CONSTRAINT merge_dispatch_claims_run_id_key UNIQUE (run_id)
);
