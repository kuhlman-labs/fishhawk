-- 0069: review_concerns.new_evidence + review_concerns.settled_ref — the
-- reviewer's supporting evidence and the re-litigation lineage tag (E60.8 /
-- #2353). Both already ride on the plan_reviewed / implement_reviewed audit
-- payload (planreview.Concern.NewEvidence / SettledRef) but were dropped at
-- the persistence boundary, so the gate view — which reads these rows — had
-- nothing to render and a well-evidenced rejection read as a bare assertion.
--
-- Byte-for-byte the additive shape 0033 used for suggested_patch: NOT NULL
-- DEFAULT '' so every row written before this migration reads back the empty
-- string and no backfill is needed. A concern with no evidence (the common
-- case) carries ''.
ALTER TABLE review_concerns
    ADD COLUMN new_evidence TEXT NOT NULL DEFAULT '',
    ADD COLUMN settled_ref TEXT NOT NULL DEFAULT '';
