-- 0081: at-most-one stage_superseded_by_merge audit entry per (run, stage)
-- (E64.29 / #3133).
--
-- POST /v0/runs/{run_id}/reconcile-merge repairs the missing-row residue the
-- sweep's TRANSITION-FIRST-THEN-AUDIT ordering deliberately allows: a stage that
-- is already `superseded` but carries no audit row gets one back. That repair is
-- a read-then-append (is a row present? if not, write one), and #3083's fix-up
-- serialized it only under a PACKAGE-LEVEL mutex — so two fishhawkd processes
-- (two replicas, or a rolling restart's overlap) could still both observe the
-- row missing and both append, breaking the documented exactly-one-row
-- guarantee. Process memory cannot arbitrate between processes; the database
-- can. This index is that arbiter, and it is the PRIMARY cross-process control
-- (unlike 0080's backstop index) — the in-process mutex demotes to a
-- same-process fast path.
--
-- KEY CHOICE: the TYPED (run_id, stage_id) COLUMN pair, NOT the
-- payload->>'stage_id' text projection 0067 / 0068 / 0080 use. audit_entries
-- carries a real stage_id UUID column; server/merge_supersede.go's
-- appendStageSupersededAudit always populates it, and
-- supersededStageIDsWithAuditRow reads that COLUMN first (falling back to the
-- payload only when the column is nil). Keying on the column therefore matches
-- the reader's own primary identity, avoids 0080's documented TEXT-projection
-- decode asymmetry (a JSON number 7 and a JSON string "7" produce the SAME
-- index key '7'), and needs no IMMUTABLE-expression argument.
--
-- Key is (run_id, stage_id), NOT run_id alone: ONE run legitimately supersedes
-- SEVERAL stages — a merge can strand both an `acceptance` stage at
-- awaiting_host_dispatch AND a `review` stage at awaiting_approval, and a
-- fan-out can park more than one child. A run_id-only key would refuse the
-- second, legitimate supersession.
--
-- HONEST RESIDUAL: audit_entries.stage_id is NULLABLE (migration 0002 line 36),
-- and PostgreSQL treats NULLs as DISTINCT in a unique index (NULLS NOT DISTINCT
-- is opt-in and is NOT used here), so a hypothetical stage-less entry in this
-- category is unconstrained. Today the ONLY non-test writer of this category is
-- server/merge_supersede.go's appendStageSupersededAudit, which sets StageID
-- unconditionally from the swept stage, so the residual is hypothetical. A
-- future writer that set the payload key but left the column NULL would evade
-- this index; no such writer exists.
--
-- Partial on category: run_id is nullable (run-less "global" chain rows set it
-- NULL), but stage_superseded_by_merge is a strictly per-run category so its
-- rows always carry a non-null run_id, and the WHERE predicate excludes every
-- other category and every run-less row. IF NOT EXISTS keeps re-application
-- idempotent.
--
-- FAIL LOUD on a pre-existing duplicate. The DO block below DETECTS duplicates
-- and RAISEs naming them, so the operator diagnostic is an explicit message
-- rather than an opaque index-build error.
--
-- REMEDY, stated correctly because the obvious one is WRONG: there is NO routine
-- "delete the redundant row and re-run".
--   (a) Migration 0002 installs audit_entries_no_update and
--       audit_entries_no_delete BEFORE triggers that RAISE EXCEPTION
--       'audit_entries is append-only; UPDATE/DELETE is forbidden'. The database
--       refuses a DELETE outright.
--   (b) Even with those triggers dropped, audit entries are hash-chained by
--       prev_hash. Removing a row breaks chain continuity for every later entry
--       in that run and breaks the SHIPPED verification surface in the separate
--       verifier module (verifier/internal/audit) — not merely a local
--       invariant.
-- The only chain-integrity-safe treatments are therefore:
--   1. RECREATE THE DATABASE. This repo is PRE-ALPHA; the category was
--      introduced days ago by #3083 and this repo runs ONE fishhawkd, so no
--      deployment carrying durable duplicates exists and this is the honest and
--      correct remedy here.
--   2. For a hypothetical real deployment: a chain-aware rewrite that re-hashes
--      the affected run's WHOLE chain (invalidating any previously published
--      export), or an explicitly recorded and accepted chain break.
-- This migration CANNOT be applied to a chain already carrying duplicates
-- without one of those.
DO $$
DECLARE
    dup record;
    detail text := '';
BEGIN
    FOR dup IN
        SELECT run_id,
               stage_id,
               array_agg(id ORDER BY sequence) AS entry_ids
        FROM audit_entries
        WHERE category = 'stage_superseded_by_merge'
          AND stage_id IS NOT NULL
        GROUP BY run_id, stage_id
        HAVING count(*) > 1
    LOOP
        detail := detail || format(
            E'\n  run_id=%s stage_id=%s entries=%s',
            dup.run_id, dup.stage_id, dup.entry_ids);
    END LOOP;

    IF detail <> '' THEN
        RAISE EXCEPTION
            'migration 0081: audit_entries already carries duplicate stage_superseded_by_merge rows per (run_id, stage_id); the unique index cannot be built:%s', detail
        USING HINT = 'audit_entries is append-only (0002 triggers) AND hash-chained, so the duplicate rows CANNOT be deleted. In pre-alpha, recreate the database. For a real deployment, re-hash the affected run''s whole chain or record an accepted chain break.';
    END IF;
END
$$;

CREATE UNIQUE INDEX IF NOT EXISTS audit_entries_stage_superseded_by_merge_once_idx
    ON audit_entries (run_id, stage_id)
    WHERE category = 'stage_superseded_by_merge';
