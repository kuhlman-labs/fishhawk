-- 0114: review_concerns judgement columns + duplicate_of (E94.2 phase 1 /
-- #4693, ADR-117 D1).
--
--   judgement             the HUMAN judgement of the concern, orthogonal to
--                         its state and its outcome. Closed vocabulary
--                         (defect, defect_pre_existing, no_defect, tradeoff,
--                         preference, stale, insufficient_evidence,
--                         unlabelled), enforced at READ by
--                         concern.NormalizeJudgement (blank or unknown reads
--                         unlabelled). The DEFAULT makes every pre-0114 row,
--                         and every row a system writer closes, read
--                         'unlabelled'.
--   judgement_actor_kind  who recorded the judgement ('' = nobody).
--   judgement_delegated   the judgement was recorded under a delegation.
--   judgement_bulk        the judgement was recorded by a bulk verb.
--   judgement_backfill    the judgement was recorded after the fact.
--   tradeoff_cost         the stated cost of a 'tradeoff' judgement.
--   duplicate_of          the id of an earlier same-run concern this one
--                         duplicates; NULL otherwise. A duplicate reads its
--                         chain root's judgement (concern.ResolveJudgements);
--                         a dangling, cross-run or cyclic link reads
--                         'unlabelled'.
--
-- This change writes NONE of these columns: their writers land with the
-- human verbs of ADR-117 D1 phases 2 and 3 (#4840, #4841). System writers
-- record only the existing outcome column and never touch a judgement column.
-- Tolerant TEXT with no CHECK and no FK, the 0030/0094/0112 posture.
-- backend/internal/concern's README is the contract.
--
-- No backfill. Postgres APPENDS added columns, so the sqlc column order is
-- ... outcome, rolled_up_from, judgement, judgement_actor_kind,
-- judgement_delegated, judgement_bulk, judgement_backfill, tradeoff_cost,
-- duplicate_of.
ALTER TABLE review_concerns
    ADD COLUMN judgement TEXT NOT NULL DEFAULT 'unlabelled',
    ADD COLUMN judgement_actor_kind TEXT NOT NULL DEFAULT '',
    ADD COLUMN judgement_delegated BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN judgement_bulk BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN judgement_backfill BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN tradeoff_cost TEXT NOT NULL DEFAULT '',
    ADD COLUMN duplicate_of UUID NULL;
