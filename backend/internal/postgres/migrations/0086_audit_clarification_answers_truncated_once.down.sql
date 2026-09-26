-- Down-migration for 0086: drop the at-most-one clarification_answers_truncated
-- index.
--
-- Index-only and additive: dropping it restores the prior unconditional-append
-- behaviour (loadClarificationAnswers appends a truncation entry on every plan-
-- prompt build) with no schema residue and no data migration (audit_entries
-- rows are untouched). IF EXISTS keeps the rollback idempotent.
DROP INDEX IF EXISTS audit_entries_clarification_answers_truncated_once_idx;
