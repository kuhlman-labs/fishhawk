-- 0029: scope_amendments table for mid-stage operator-gated scope
-- amendment requests (E22.X / #961). The implement agent may request
-- that specific file paths be folded into the effective scope.files
-- while the stage is running; the operator approves or denies via the
-- decision endpoint. Approved rows are folded into the prompt-fetch
-- scope AND the runner's pre-commit scope refresh, so the verify gates
-- and the push see the same folded tree (#960 invariant).
--
--   - paths is a jsonb array of {path, operation} objects, where
--     operation is modify|create. Kept jsonb (vs a child table)
--     because the array is small (per-request, bounded by the
--     per-stage cap), read whole, and never queried per-element.
--   - status drives the agent's poll loop: pending → approved|denied.
--     A denied row still consumes per-stage budget — the cap bounds
--     operator interruptions, not approvals.
--   - decided_by records the operator subject from the decision
--     endpoint's identity (never a run-bound token: the decision
--     endpoint rejects those outright — no self-approval).

CREATE TABLE scope_amendments (
    id              UUID         PRIMARY KEY,
    run_id          UUID         NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    stage_id        UUID         NOT NULL REFERENCES stages (id) ON DELETE CASCADE,
    paths           JSONB        NOT NULL,
    reason          TEXT         NOT NULL,
    status          TEXT         NOT NULL DEFAULT 'pending',
    decision_reason TEXT,
    decided_by      TEXT,
    requested_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    decided_at      TIMESTAMPTZ,
    CONSTRAINT scope_amendments_status_check CHECK (
        status IN ('pending', 'approved', 'denied')
    )
);

-- Per-stage budget count (the server-side cap of 2 per stage).
CREATE INDEX scope_amendments_stage_idx ON scope_amendments (stage_id);
-- List endpoint + prompt-fetch fold read by run.
CREATE INDEX scope_amendments_run_idx ON scope_amendments (run_id);
