-- 0046 down: drop the refinement_drafts table (and its session index, dropped
-- implicitly with the table). Purely additive migration, so the rollback is a
-- clean DROP with no data-normalization step.

DROP TABLE IF EXISTS refinement_drafts;
