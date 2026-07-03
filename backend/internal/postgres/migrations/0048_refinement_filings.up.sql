-- 0048: refinement_filing_sessions + refinement_filed_items — the durable
-- idempotency ledger for the E34.3 filing executor (ADR-052 filing half,
-- #1594).
--
-- The filing executor turns an approved, hash-pinned refinement draft into
-- real tracker items (epic first, then children in wave order). Because the
-- external provider create (a GitHub issue) cannot be made transactional with
-- Postgres, the executor records a durable row per filed item IMMEDIATELY after
-- each provider File returns, so a re-invoke after a mid-sequence failure
-- resumes at the first unfiled ordinal and never re-files a recorded one.
--
-- refinement_filing_sessions is the per-approved-draft filing record: one row
-- keyed by draft_id that pins the target repo at first invoke (a re-invoke
-- naming a different repo fails closed) and whose completed_at flip is the
-- session-closing state change (set durable-AFTER the refinement_filing_completed
-- audit entry). draft_id FK ON DELETE RESTRICT so a decided/filed revision can
-- never be deleted out from under its filing record, mirroring
-- refinement_decisions.
--
-- refinement_filed_items is one durable row per filed item: ordinal 0 is the
-- epic, 1..N the draft children. UNIQUE(draft_id, ordinal) is the DB-level
-- never-double-RECORD backstop that pairs with the executor's record-after-file
-- ordering and the per-draft advisory lock (the concurrent-duplication guard).
--
-- Schema mirrors 0046/0047: now()-defaulted timestamps and a gen_random_uuid()
-- PK default. Nothing here files; the row is a durable ledger entry, not a
-- provider write.

CREATE TABLE refinement_filing_sessions (
    draft_id     UUID         PRIMARY KEY REFERENCES refinement_drafts(id) ON DELETE RESTRICT,
    session_id   UUID         NOT NULL,
    repo         TEXT         NOT NULL,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ
);

CREATE TABLE refinement_filed_items (
    id            UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    draft_id      UUID         NOT NULL REFERENCES refinement_filing_sessions(draft_id) ON DELETE RESTRICT,
    ordinal       INT          NOT NULL CHECK (ordinal >= 0),
    issue_number  INT          NOT NULL,
    issue_url     TEXT         NOT NULL,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (draft_id, ordinal)
);

CREATE INDEX refinement_filed_items_draft_idx ON refinement_filed_items (draft_id, ordinal);
