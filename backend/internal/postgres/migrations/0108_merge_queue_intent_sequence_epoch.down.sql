-- 0108 (down): drop the merge-dispatch ledger and the queue entries'
-- merge_intent_sequence and admission_epoch columns (#4296).
--
-- Queue rows survive; the recorded intent sequences, the admission epochs and
-- every dispatch claim are lost. Rollback order: revert the fishhawkd binary
-- FIRST — a binary that selects or writes them fails every queue read and
-- write and every merge dispatch once they are gone.
DROP TABLE merge_dispatch_claims;

ALTER TABLE merge_candidate_queue_entries
    DROP COLUMN admission_epoch,
    DROP COLUMN merge_intent_sequence;
