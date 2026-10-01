-- Down-migration for 0091: drop runs.document_base_commit and its CHECK.
--
-- The constraint is dropped first, then the column; no other table or column
-- is touched. Read the consequence honestly: a rollback LOSES every recorded
-- admission commit, so a run created while the column existed can no longer
-- resolve run-admission documents at the commit it was admitted against. No
-- production consumer declares a run-admission document at the time of
-- writing, so no served prompt changes.
ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_document_base_commit_check;
ALTER TABLE runs DROP COLUMN IF EXISTS document_base_commit;
