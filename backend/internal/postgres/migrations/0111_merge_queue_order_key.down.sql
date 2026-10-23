-- 0111 (down): drop the queue entries' promote ordering key (#4297).
--
-- Queue rows survive; every promotion is lost and held entries fall back to
-- FIFO by enqueued_at. Rollback order: revert the fishhawkd binary FIRST — a
-- binary that selects or writes order_key fails every queue read and write
-- once it is gone.
ALTER TABLE merge_candidate_queue_entries DROP COLUMN order_key;
