-- 0047 down: drop refinement_decisions (and its session index, dropped
-- implicitly with the table) and the refinement_drafts.origin column. Purely
-- additive migration, so the rollback is a clean DROP + column drop with no
-- data-normalization step. 0046's refinement_drafts table itself is untouched.

DROP TABLE IF EXISTS refinement_decisions;

ALTER TABLE refinement_drafts DROP COLUMN IF EXISTS origin;
