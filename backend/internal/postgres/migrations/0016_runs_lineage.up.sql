-- 0016: run lineage columns for the threaded-runs view (#216).
--
-- A run today is one trigger → one execution → one PR. But the
-- demo loop wants follow-ups on a PR that's already open: CI fails
-- and the engineer fires another `/fishhawk run`, or a reviewer
-- comments asking for a change, or the implement stage gets
-- re-executed (E8.3 #146). Each of those creates a fresh `runs`
-- row, but today there's no link from the new run back to the
-- original — "show me everything that's happened on this PR" is
-- unanswerable.
--
-- Two columns:
--   - parent_run_id: the run this one followed up on. Nullable;
--     issue-triggered runs that haven't seen a prior run on the
--     same trigger_ref leave it null. References runs(id) so a
--     run-delete cascades; the chain length is bounded by the
--     repo's activity (in practice O(small).)
--   - pull_request_url: the implement-stage PR's URL, copied from
--     the pull_request artifact when it lands. Lets us GROUP runs
--     by the PR they're acting on without a recursive walk —
--     "show me every run on this PR" is a single
--     equality predicate. Nullable: runs that haven't reached the
--     implement stage yet, plus follow-ups before the upstream
--     run's PR URL has propagated, both legitimately have nothing
--     here.
--
-- Decision per #216 body, recommendation (a) + (b) hybrid:
-- parent_run_id is the primitive (explicit causality); the PR URL
-- is the indexed denormalization for the natural UI grouping.

ALTER TABLE runs
    ADD COLUMN parent_run_id    UUID REFERENCES runs (id) ON DELETE SET NULL,
    ADD COLUMN pull_request_url TEXT;

CREATE INDEX runs_parent_run_id_idx ON runs (parent_run_id)
    WHERE parent_run_id IS NOT NULL;

CREATE INDEX runs_pull_request_url_idx ON runs (pull_request_url)
    WHERE pull_request_url IS NOT NULL;
