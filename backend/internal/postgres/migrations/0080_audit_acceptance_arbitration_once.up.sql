-- 0080: at-most-one acceptance_triage_arbitrated audit entry per (run,
-- discharged acceptance outcome) (E48.91 / #2536).
--
-- POST /v0/runs/{run_id}/acceptance-arbitration records the operator's discharge
-- of a PAGED acceptance triage as a chained acceptance_triage_arbitrated entry
-- bound to the acceptance_outcome_recorded sequence it discharges. Before #2536
-- BOTH of the endpoint's controls sat OUTSIDE the append's transaction: the
-- idempotence scan listed prior arbitrations several reads earlier, and guard 7
-- re-read the newest outcome immediately before AppendChained. So two concurrent
-- valid POSTs could each pass and each append a row for the same outcome.
--
-- The PRIMARY control for that race is NOT this index: it is the anchored append
-- primitive (backend/internal/audit/anchored.go) that takes the run-row lock
-- FIRST and then performs the anchor re-read and the dedupe scan INSIDE the same
-- transaction. This index is a store-layer BACKSTOP. It is a genuine backstop
-- rather than the primary control precisely because AppendChainedTx holds
-- SELECT ... FOR UPDATE on the run row and audit_entries.run_id REFERENCES runs:
-- the FK check acquires FOR KEY SHARE on the referenced run row, which conflicts
-- with FOR UPDATE, so while the anchored transaction holds the lock no other
-- transaction can insert ANY audit_entries row for that run. On today's code
-- paths a same-run duplicate collision is therefore near-unreachable, which is
-- why the lock+scan layer is pinned by an index-DROPPED concurrency test rather
-- than by resting on this index.
--
-- The key is (run_id, payload->>'outcome_sequence'), NOT run_id alone: ONE run
-- legitimately carries ONE arbitration PER DISTINCT discharged outcome. A later
-- acceptance re-run records a higher-sequence acceptance_outcome_recorded entry
-- that no prior arbitration names, re-wedging the gate at acceptance_triage —
-- and the operator arbitrates THAT outcome too. Keying on run_id alone would
-- refuse the second, legitimate discharge outright.
--
-- Partial on category: run_id is nullable (run-less "global" chain rows set it
-- NULL), but acceptance_triage_arbitrated is a strictly per-run category so its
-- rows always carry a non-null run_id, and the WHERE predicate excludes every
-- other category and every run-less row — so the index is well-defined and
-- constrains only the intended rows. IF NOT EXISTS keeps re-application
-- idempotent.
--
-- payload->>'outcome_sequence' is an IMMUTABLE jsonb expression, a precondition
-- for indexing it. A payload missing outcome_sequence indexes as NULL, and NULLs
-- are DISTINCT in a PostgreSQL unique index (NULLS NOT DISTINCT is opt-in and is
-- NOT used here), so a key-less row is unconstrained. The handler always sets
-- the key. NOTE the key is the TEXT projection: a JSON number 7 and a JSON
-- string "7" produce the SAME index key '7'. That is deliberate — the recovery
-- read in audit/anchored.go matches on these same TEXT semantics so it always
-- finds whatever row the index actually collided with.
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
--   1. RECREATE THE DATABASE. This repo is PRE-ALPHA; no deployment carrying
--      durable acceptance_triage_arbitrated duplicates exists, so this is the
--      honest and correct remedy here.
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
               payload->>'outcome_sequence' AS outcome_sequence,
               array_agg(id ORDER BY sequence) AS entry_ids
        FROM audit_entries
        WHERE category = 'acceptance_triage_arbitrated'
          AND payload->>'outcome_sequence' IS NOT NULL
        GROUP BY run_id, payload->>'outcome_sequence'
        HAVING count(*) > 1
    LOOP
        detail := detail || format(
            E'\n  run_id=%s outcome_sequence=%s entries=%s',
            dup.run_id, dup.outcome_sequence, dup.entry_ids);
    END LOOP;

    IF detail <> '' THEN
        RAISE EXCEPTION
            'migration 0080: audit_entries already carries duplicate acceptance_triage_arbitrated rows per (run_id, outcome_sequence); the unique index cannot be built:%s', detail
        USING HINT = 'audit_entries is append-only (0002 triggers) AND hash-chained, so the duplicate rows CANNOT be deleted. In pre-alpha, recreate the database. For a real deployment, re-hash the affected run''s whole chain or record an accepted chain break.';
    END IF;
END
$$;

CREATE UNIQUE INDEX IF NOT EXISTS audit_entries_acceptance_triage_arbitrated_once_idx
    ON audit_entries (run_id, (payload->>'outcome_sequence'))
    WHERE category = 'acceptance_triage_arbitrated';
