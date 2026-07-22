-- 0058: E44.4 per-account run-less audit chain (ADR-057, #1828). The
-- run-less ("global") partition of audit_entries is now chained per
-- account_id: AppendGlobalChained reads prev_hash from the last entry
-- WHERE run_id IS NULL AND account_id = $1 (or IS NULL for the
-- untenanted legacy partition), so that lookup — on the hot path of
-- every run-less governance write — needs an index keyed the way the
-- query filters. Purely additive: no rows change, no hash format
-- change (account_id is deliberately NOT part of the canonical
-- HashInputs; the partition is carried by the prev_hash-lookup scope).
--
-- Partial on run_id IS NULL: per-run entries never use this path (they
-- chain within run_id, already served by 0002's run index), and the
-- run-less partition is a small fraction of the table. A btree serves
-- both the account_id = $1 and account_id IS NULL scans; sequence DESC
-- matches the LIMIT 1 latest-entry read and the ASC walk reads it
-- backwards equally well.
CREATE INDEX audit_entries_global_account_seq_idx
    ON audit_entries (account_id, sequence DESC)
    WHERE run_id IS NULL;
