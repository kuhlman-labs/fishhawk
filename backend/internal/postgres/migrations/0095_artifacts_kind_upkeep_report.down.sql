-- 0095 down: restore the seven-value artifacts kind CHECK ('plan',
-- 'pull_request', 'deployment', 'acceptance', 'release_notes',
-- 'grooming_report', 'acceptance_transcript'). Any 'upkeep_report' artifact
-- rows written while 0095 was applied would violate the restored CHECK; this
-- down migration assumes the rollback runs before any upkeep_report artifact
-- is persisted (the additive-change rollback contract 0051, 0073 and 0083
-- established — revert before the new kind is used). Rows of every prior kind
-- are untouched.
ALTER TABLE artifacts DROP CONSTRAINT artifacts_kind_check;
ALTER TABLE artifacts ADD CONSTRAINT artifacts_kind_check CHECK (
    kind IN ('plan', 'pull_request', 'deployment', 'acceptance', 'release_notes', 'grooming_report', 'acceptance_transcript')
);
