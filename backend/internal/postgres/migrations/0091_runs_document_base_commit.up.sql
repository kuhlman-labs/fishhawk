-- 0091: persist the run-admission document base commit on the run row
-- (E55.7 / #3746).
--
-- runs.document_base_commit records, ONCE at run admission, the commit that
-- repo-declared documents marked run-admission (repodoc BaseSourceRunAdmission)
-- are resolved from for the life of the run. It is stamped at every ROOT mint
-- seam (POST /v0/runs and campaign item start via CreateRunForTrigger, the
-- GitHub and GitLab webhook dispatchers) and inherited verbatim by every child
-- through run.ChildParamsFrom, so a retry, recovery, decomposition child or
-- fix-up re-serve resolves against the SAME admission commit — never a newer
-- default-branch head, and never the run's own branch.
--
-- TWO STATES:
--   a lowercase 40-hex commit  the admission commit; documents are read there.
--   NULL                       NO commit recorded at admission. Covers a row
--                              minted before this migration, a deployment with
--                              no document seam, and a capture that degraded
--                              (capture never blocks run creation). A consumer
--                              WITHHOLDS run-admission documents for such a
--                              run with a named degradation; it never falls
--                              back to reading a mutable ref.
--
-- NULLABLE and with NO column default ON PURPOSE: a default would assert a pin
-- nothing recorded. NO BACKFILL for the same reason — the admission commit of
-- a historical run is unknowable after the fact.
--
-- The CHECK makes a branch name, a short SHA or an uppercase SHA
-- unrepresentable, so a persisted value can never smuggle a mutable ref into
-- the resolver.
--
-- Plain single-apply DDL (no IF NOT EXISTS), matching 0089/0090: the migration
-- runner applies each version exactly once.
ALTER TABLE runs ADD COLUMN document_base_commit TEXT;

ALTER TABLE runs ADD CONSTRAINT runs_document_base_commit_check CHECK (
    document_base_commit IS NULL OR document_base_commit ~ '^[0-9a-f]{40}$'
);
