-- Down-migration for 0067: drop the at-most-one
-- parent_awaiting_child_scope_decision index.
--
-- Index-only and additive: dropping it restores the prior read-then-append
-- behaviour (an unconditional append at park time, a best-effort
-- read-then-append on sibling settle) with no schema residue and no data
-- migration (audit_entries rows are untouched). IF EXISTS keeps the rollback
-- idempotent.
DROP INDEX IF EXISTS audit_entries_parent_awaiting_child_scope_decision_once_idx;
