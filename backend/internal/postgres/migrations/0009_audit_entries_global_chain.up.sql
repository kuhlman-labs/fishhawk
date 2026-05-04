-- 0009: extend audit_entries to support a global chain for
-- non-run events (E2.7 / #138).
--
-- The original schema (0002) bound every audit entry to a run via
-- `run_id NOT NULL REFERENCES runs(id)`. That works for everything
-- tied to a workflow execution but breaks down for events that
-- aren't:
--
--   - API token issue / revoke (E4.5 #51)
--   - OAuth sign-in / sign-out (E4.2 #49 — pending)
--   - GitHub App install / uninstall (E4.1 #48 — pending)
--   - Admin / config changes
--
-- We adopt option 2 from #138: nullable run_id with a separate
-- "global" chain partition keyed by `WHERE run_id IS NULL`.
-- Per-run chain semantics are unchanged; the new partition is
-- one chain you read with `WHERE run_id IS NULL ORDER BY
-- sequence`.

ALTER TABLE audit_entries
    ALTER COLUMN run_id DROP NOT NULL;

-- Partial index on the global partition for the
-- "last entry → prev_hash" lookup. The existing
-- audit_entries_run_seq_idx covers per-run lookups; this is the
-- mirror for the global chain.
CREATE INDEX audit_entries_global_seq_idx
    ON audit_entries (sequence DESC)
    WHERE run_id IS NULL;
