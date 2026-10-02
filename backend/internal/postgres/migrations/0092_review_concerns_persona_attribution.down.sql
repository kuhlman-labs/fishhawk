-- Down-migration for 0092: drop the three review_concerns persona-ingest
-- columns. The table and every other column are untouched. A rollback LOSES
-- the recorded reviewer_role / quote_unverified / severity_clamped_from
-- values; the plan_reviewed / implement_reviewed audit payloads retain every
-- concern's persona, quote and clamp markers, so nothing authoritative is lost.
ALTER TABLE review_concerns
    DROP COLUMN reviewer_role,
    DROP COLUMN quote_unverified,
    DROP COLUMN severity_clamped_from;
