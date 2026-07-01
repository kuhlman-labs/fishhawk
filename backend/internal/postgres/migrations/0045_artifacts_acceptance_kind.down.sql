-- 0045 down: restore the three-value artifacts kind CHECK ('plan',
-- 'pull_request', 'deployment'). Any 'acceptance' artifact rows written
-- while 0045 was applied would violate the restored CHECK; this down
-- migration assumes the rollback runs before any acceptance artifact is
-- persisted (the additive-change rollback contract — revert before the new
-- kind is used). Existing 'plan'/'pull_request'/'deployment' rows are
-- untouched.
ALTER TABLE artifacts DROP CONSTRAINT artifacts_kind_check;
ALTER TABLE artifacts ADD CONSTRAINT artifacts_kind_check CHECK (
    kind IN ('plan', 'pull_request', 'deployment')
);
