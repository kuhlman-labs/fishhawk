-- 0101: merge_candidate_queue_entries — the per-(repository, base ref)
-- held-pass queue behind ADR-092 D1 (#4200): at most ONE live
-- merge-candidate verify pass per base, every other eligible pass held FIFO
-- by eligibility time (enqueued_at). backend/internal/mergequeue is the only
-- reader and writer; its README is the contract.
--
-- One row per queue EPISODE of a run. state 'held' and 'live' are ACTIVE (at
-- most one active row per run, the partial unique index below); 'merged',
-- 'ejected' and 'dropped' are TERMINAL and never change again. A run that
-- re-enters after a terminal row gets a FRESH row at the tail (ADR-093 Q2).
--
-- Shaped for ADR-093 (the Fishhawk-owned merge queue) so it extends these rows
-- without a second migration: phase is free TEXT with no CHECK (ADR-092 writes
-- admitted|verifying|passed|unverified; ADR-093 adds its own), eject_reason
-- carries the eject/drop reason, and the anchored head + base SHAs are the
-- re-anchor-at-admission state.
--
-- No account_id and no RLS, like stage_concurrency_slots (0097): account
-- scoping is applied explicitly by the store (it joins runs.account_id), so it
-- holds even under the superuser runtime role that bypasses RLS.
--
-- ON DELETE CASCADE from runs, so deleting a run is never blocked by its
-- queue rows.
CREATE TABLE merge_candidate_queue_entries (
    id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id            UUID        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    repo              TEXT        NOT NULL CONSTRAINT merge_candidate_queue_entries_repo_check CHECK (repo <> ''),
    base_ref          TEXT        NOT NULL CONSTRAINT merge_candidate_queue_entries_base_ref_check CHECK (base_ref <> ''),
    state             TEXT        NOT NULL CONSTRAINT merge_candidate_queue_entries_state_check
                                  CHECK (state IN ('held', 'live', 'merged', 'ejected', 'dropped')),
    phase             TEXT        NULL,
    eject_reason      TEXT        NULL,
    enqueued_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    anchored_head_sha TEXT        NOT NULL CONSTRAINT merge_candidate_queue_entries_head_check CHECK (anchored_head_sha <> ''),
    anchored_base_sha TEXT        NULL,
    anchored_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    admitted_at       TIMESTAMPTZ NULL,
    settled_at        TIMESTAMPTZ NULL,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- An ejected or dropped row names why.
    CONSTRAINT merge_candidate_queue_entries_reason_check
        CHECK (state NOT IN ('ejected', 'dropped') OR COALESCE(eject_reason, '') <> ''),
    -- settled_at is set exactly when the row is terminal.
    CONSTRAINT merge_candidate_queue_entries_settled_check
        CHECK ((state IN ('held', 'live')) = (settled_at IS NULL)),
    -- A live row records when it was admitted.
    CONSTRAINT merge_candidate_queue_entries_admitted_check
        CHECK (state <> 'live' OR admitted_at IS NOT NULL)
);

-- At most one ACTIVE entry per run.
CREATE UNIQUE INDEX merge_candidate_queue_entries_active_run_idx
    ON merge_candidate_queue_entries (run_id) WHERE state IN ('held', 'live');

-- The per-base FIFO read: live first, then held by (enqueued_at, id).
CREATE INDEX merge_candidate_queue_entries_base_idx
    ON merge_candidate_queue_entries (repo, base_ref, state, enqueued_at, id);

CREATE TRIGGER merge_candidate_queue_entries_set_updated_at
    BEFORE UPDATE ON merge_candidate_queue_entries
    FOR EACH ROW EXECUTE FUNCTION fishhawk_set_updated_at();
