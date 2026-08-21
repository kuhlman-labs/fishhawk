-- 0073 down: restore the five-value artifacts kind CHECK ('plan',
-- 'pull_request', 'deployment', 'acceptance', 'release_notes'). Any
-- 'grooming_report' artifact rows written while 0073 was applied would violate
-- the restored CHECK; this down migration assumes the rollback runs before any
-- grooming_report artifact is persisted (the additive-change rollback contract
-- 0051 established — revert before the new kind is used). Existing
-- 'plan'/'pull_request'/'deployment'/'acceptance'/'release_notes' rows are
-- untouched.
ALTER TABLE artifacts DROP CONSTRAINT artifacts_kind_check;
ALTER TABLE artifacts ADD CONSTRAINT artifacts_kind_check CHECK (
    kind IN ('plan', 'pull_request', 'deployment', 'acceptance', 'release_notes')
);
