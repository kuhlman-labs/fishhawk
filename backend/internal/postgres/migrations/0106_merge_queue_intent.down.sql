-- 0106 (down): drop merge_candidate_queue_entries.merge_intent_at (#4295).
--
-- Rows survive; only the recorded merge intent is lost. Rollback order: revert
-- the fishhawkd binary FIRST — a binary that selects or stamps the column
-- fails every queue read and write once it is gone.
ALTER TABLE merge_candidate_queue_entries
    DROP COLUMN merge_intent_at;
