-- 0047: refinement_decisions + refinement_drafts.origin — the approve/reject
-- gate half of ADR-052 option A (E34.2 / #1593).
--
-- A refinement decision is an append-only approve/reject verdict on ONE draft
-- revision. It pins the decided draft's id (FK, ON DELETE RESTRICT so a decided
-- revision can never be deleted out from under the decision) AND a content hash
-- of the decoded EpicDraft, so "what was approved is what files": an edit that
-- lands a new draft revision structurally invalidates a prior approval (the
-- decision no longer targets the session's latest revision, and its pinned hash
-- no longer matches the recomputed hash). Session state is DERIVED from the
-- draft + decision rows, never stored — so there is no mutable approval flag to
-- fall out of sync. Decisions are append-only (no UPDATE/DELETE), mirroring the
-- audit-chain discipline the gate's decisions also ride.
--
-- refinement_drafts.origin makes the brief-amendment budget countable from
-- rows (mirroring how revise counts plan_revised entries): 'brief' for the
-- initial draft, 'amendment' for an agent-re-drafted revision, 'edit' for a
-- direct field edit. It defaults to 'brief' so the 0046 rows backfill to the
-- initial-origin value with no data step.

ALTER TABLE refinement_drafts
    ADD COLUMN origin TEXT NOT NULL DEFAULT 'brief'
        CHECK (origin IN ('brief', 'amendment', 'edit'));

CREATE TABLE refinement_decisions (
    id                  UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id          UUID         NOT NULL,
    draft_id            UUID         NOT NULL REFERENCES refinement_drafts(id) ON DELETE RESTRICT,
    decision            TEXT         NOT NULL CHECK (decision IN ('approved', 'rejected')),
    reason              TEXT         NOT NULL,
    draft_content_hash  TEXT         NOT NULL,
    decided_by          TEXT,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX refinement_decisions_session_idx ON refinement_decisions (session_id, created_at);
