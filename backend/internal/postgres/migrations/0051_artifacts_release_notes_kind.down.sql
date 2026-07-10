-- 0051 down: restore the four-value artifacts kind CHECK ('plan',
-- 'pull_request', 'deployment', 'acceptance'). Any 'release_notes' artifact
-- rows written while 0051 was applied would violate the restored CHECK; this
-- down migration assumes the rollback runs before any release_notes artifact is
-- persisted (the additive-change rollback contract — revert before the new kind
-- is used). Existing 'plan'/'pull_request'/'deployment'/'acceptance' rows are
-- untouched.
ALTER TABLE artifacts DROP CONSTRAINT artifacts_kind_check;
ALTER TABLE artifacts ADD CONSTRAINT artifacts_kind_check CHECK (
    kind IN ('plan', 'pull_request', 'deployment', 'acceptance')
);
