-- 0031: persist the drive-mode flag on the run row (#1023 / #996
-- theme 1).
--
-- drive opts the run into backend auto-advancement of mechanical
-- transitions (plan-approved dispatch, review-verdict settlement,
-- fixup re-park, checks-green awaiting_merge). The flag is resolved
-- at run-create time — workflow-spec default overridden by the
-- per-run POST /v0/runs field — and snapshotted here so a spec edit
-- mid-run can't change an in-flight run's advancement behavior
-- (same snapshot discipline as max_retries_snapshot, 0021).
--
-- Default false covers all legacy rows: every run created before
-- this migration was operator-driven. Additive with a non-volatile
-- default, so PostgreSQL backfills without rewriting rows and
-- pre-migration binaries never reference the column.
--
-- Verifier impact (verifier/internal/audit/): the flag is metadata
-- atop the tamper-evidence chain, not a hash input; adding the
-- column doesn't change the rehash invariant. The per-advance
-- run_auto_advanced audit entries (sibling slice) are the canonical
-- record of what drive mode actually did.
--
-- No index: read by PK (id) on the same row every run-detail
-- surface already fetches.

ALTER TABLE runs
    ADD COLUMN drive BOOLEAN NOT NULL DEFAULT false;
