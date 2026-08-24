-- Down-migration for 0076: drop runs_retry_child_once_idx, then
-- runs.installation_ref.
--
-- Index first, column second. The index does not reference installation_ref,
-- so the order is not a dependency requirement — it is the exact reverse of
-- the up migration's creation order, which is the property a reader checks.
--
-- NO DATA IS LOST that installation_id does not still hold for a GitHub row:
-- the backfill is a pure derivation (installation_ref = installation_id::text),
-- so dropping the column returns every migrated GitHub run to the
-- installation_id fallback it used before 0076.
--
-- A run created by the GitLab dispatch path WHILE 0076 was live is the
-- exception, and rolling back is genuinely lossy for it: its
-- 'gitlab:<project_id>' ref has no installation_id equivalent, so after the
-- rollback its stages resolve the zero credential scope and warn-skip. That is
-- the pre-0076 behaviour rather than a new failure mode, but such runs should
-- be cancelled rather than left mid-flight — the rollback cannot recover them.
--
-- Dropping the index restores the prior read-then-write dedup behaviour on
-- both retry paths (racy, but the behaviour that shipped before this change)
-- with no schema residue and no data migration. IF EXISTS keeps the rollback
-- idempotent.
DROP INDEX IF EXISTS runs_retry_child_once_idx;

ALTER TABLE runs
    DROP COLUMN installation_ref;
