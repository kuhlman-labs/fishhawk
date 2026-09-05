-- Down-migration for 0080: drop the at-most-one acceptance_triage_arbitrated
-- index.
--
-- Index-only and additive: dropping it restores the prior store-layer posture
-- (the anchored append primitive's lock + in-transaction scan remains the
-- primary control) with no schema residue and no data migration — audit_entries
-- rows are untouched, and the up migration's DO-block pre-flight is a pure read
-- that leaves nothing to reverse. IF EXISTS keeps the rollback idempotent.
DROP INDEX IF EXISTS audit_entries_acceptance_triage_arbitrated_once_idx;
