-- Down-migration for 0081: drop the at-most-one stage_superseded_by_merge
-- index.
--
-- Index-only and additive: dropping it restores the prior posture — the
-- in-process supersedeRepairMu fast path in server/merge_supersede.go remains,
-- so the same-process guarantee survives and only the CROSS-PROCESS guarantee is
-- withdrawn — with no schema residue and no data migration. audit_entries rows
-- are untouched, and the up migration's DO-block pre-flight is a pure read that
-- leaves nothing to reverse. IF EXISTS keeps the rollback idempotent.
DROP INDEX IF EXISTS audit_entries_stage_superseded_by_merge_once_idx;
