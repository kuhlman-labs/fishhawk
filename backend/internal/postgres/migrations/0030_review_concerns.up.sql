-- 0030: review_concerns table — the durable concern store behind stable
-- concern IDs (E22.X / #964). Every plan_reviewed / implement_reviewed
-- verdict's concerns[] is persisted here with a server-minted UUID, so
-- fix-up routing addresses concerns by stable ID instead of a flattened
-- positional index (ambiguous once multiple heterogeneous review entries
-- exist per stage — the run-73456dc8 mis-route).
--
--   - origin_review_sequence is the audit sequence of the *_reviewed
--     entry the concern was decoded from. NOT NULL: rows are inserted
--     AFTER the audit append using the sequence AppendChained returns,
--     keeping the audit chain the sole sequence authority. The audit
--     payload remains the authoritative record; this table is a derived
--     index over it (concern persistence is best-effort/warn-only).
--   - severity / category / state are plain TEXT with no CHECK,
--     mirroring the tolerant-decode posture: unknown reviewer-emitted
--     severities are stored verbatim, and state validity is enforced by
--     the Go state machine (backend/internal/concern) only. The full
--     lifecycle enum is raised, addressed_pending, addressed, reopened,
--     waived, superseded — waived/superseded ship now so the deferred
--     operator waive verb needs no schema change.
--   - reviewer_model is nullable: a verdict can land without a model id.

CREATE TABLE review_concerns (
    id                      UUID         PRIMARY KEY,
    run_id                  UUID         NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    stage_id                UUID         NOT NULL REFERENCES stages (id) ON DELETE CASCADE,
    stage_kind              TEXT         NOT NULL,
    origin_review_sequence  BIGINT       NOT NULL,
    reviewer_model          TEXT,
    severity                TEXT         NOT NULL,
    category                TEXT         NOT NULL DEFAULT '',
    note                    TEXT         NOT NULL,
    state                   TEXT         NOT NULL DEFAULT 'raised',
    state_reason            TEXT         NOT NULL DEFAULT '',
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT review_concerns_stage_kind_check CHECK (
        stage_kind IN ('plan', 'implement')
    )
);

-- Run-status surface lists open concerns by run.
CREATE INDEX review_concerns_run_idx ON review_concerns (run_id);
-- Fix-up routing resolves IDs scoped to a stage + open state.
CREATE INDEX review_concerns_stage_state_idx ON review_concerns (stage_id, state);
