-- Revert 0044: narrow stages_type_check back to the pre-0044 set
-- (plan/implement/review/deploy — the 0038 type set).
--
-- The CHECK re-add fails loudly if any stage row currently holds an
-- 'acceptance' type, so the down is safe only when no acceptance stage
-- exists — exactly the narrowing precondition a rollback of an additive
-- CHECK widening needs (the 0038 / 0035 down precedent). Existing
-- plan/implement/review/deploy rows are untouched. stages_state_check was
-- never widened by 0044, so there is nothing to narrow there.
ALTER TABLE stages DROP CONSTRAINT stages_type_check;
ALTER TABLE stages ADD CONSTRAINT stages_type_check CHECK (
    stage_type IN ('plan', 'implement', 'review', 'deploy')
);
