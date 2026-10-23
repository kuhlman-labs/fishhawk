-- 0109 (down): drop the queue entries' yield marker (yield_reason,
-- yield_detail, yielded_at), the approval-wait anchor
-- (approval_wait_started_at) and their two CHECKs (#4632).
--
-- Queue rows survive; every yield marker and wait anchor is lost, so a
-- formerly yielded held row is a plain held row again and is admitted FIFO.
-- Rollback order: revert the fishhawkd binary FIRST — a binary that selects
-- or writes these columns fails every queue read and write once they are
-- gone.
ALTER TABLE merge_candidate_queue_entries
    DROP CONSTRAINT merge_candidate_queue_entries_yield_live_check,
    DROP CONSTRAINT merge_candidate_queue_entries_yield_marker_check,
    DROP COLUMN approval_wait_started_at,
    DROP COLUMN yielded_at,
    DROP COLUMN yield_detail,
    DROP COLUMN yield_reason;
