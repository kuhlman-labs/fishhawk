-- 0059 down: drop the repo-ACL mirror. Lossless by construction — the table
-- holds only a CACHE of forge permission facts, so dropping it costs at most
-- one re-resolve per (subject, repo) on the next read. The shared
-- fishhawk_set_updated_at() function is NOT dropped (0001 owns it); DROP TABLE
-- removes the trigger and the index with it, but both are named explicitly
-- first so the rollback reads as the exact inverse of the up migration.
DROP TRIGGER IF EXISTS repo_acl_entries_set_updated_at ON repo_acl_entries;
DROP INDEX IF EXISTS repo_acl_entries_provider_subject_idx;
DROP TABLE IF EXISTS repo_acl_entries;
