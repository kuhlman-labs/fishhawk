-- 0107: merge_candidate_queue_entries.merge_intent_at — the operator's merge
-- intent on a queue episode (ADR-093 D2, #4295). fishhawk_merge_run on an
-- eligible run the ADR-090 gate would refuse as behind or unverified enqueues
-- it (or marks its already-active entry) and stamps this column; ADR-093's
-- merge-on-pass (slice 2) merges only an entry that carries it.
--
-- NULL = no recorded merge intent: every pre-0107 row and every ADR-092
-- rebase/base-advance path entry. No backfill. Set once per row from the
-- database clock (clock_timestamp()) by backend/internal/mergequeue and never
-- cleared; a terminal row keeps it as history, and a run re-entering after a
-- terminal row starts a FRESH row without it (ADR-093 Q2).
--
-- Additive and nullable, so the ADR-092 serializer of a reverted binary keeps
-- processing rows that carry it (it ignores the column).
ALTER TABLE merge_candidate_queue_entries
    ADD COLUMN merge_intent_at TIMESTAMPTZ NULL;
