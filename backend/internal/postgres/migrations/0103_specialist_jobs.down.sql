-- 0103 down: drop the specialist-job ledger. Audit entries already written
-- under specialist_job_* categories stay on the chain (the categories remain
-- registered until the code is reverted).
--
-- Rollback ORDER (approval condition C9): revert the fishhawkd binary FIRST,
-- then run this down migration. A binary carrying the concurrency store's
-- jobHoldersSQL reads specialist_jobs on every slot admission (local-verify
-- included), so dropping the table under it fails every such admission until
-- the binary is reverted.
DROP TABLE IF EXISTS specialist_jobs;
