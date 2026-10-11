-- 0101 down: drop the merge-candidate held-pass queue. Nothing else
-- references it (runs -> entries is ON DELETE CASCADE on the entry side), and
-- dropping the table drops its trigger and indexes with it. Held entries are
-- lost; their PRs need fishhawk_rebase_run_branch again (ADR-090 recovery).
DROP TABLE IF EXISTS merge_candidate_queue_entries;
