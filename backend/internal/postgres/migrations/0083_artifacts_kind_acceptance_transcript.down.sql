-- 0083 down: restore the six-value artifacts kind CHECK ('plan',
-- 'pull_request', 'deployment', 'acceptance', 'release_notes',
-- 'grooming_report'). Any 'acceptance_transcript' artifact rows written while
-- 0083 was applied would violate the restored CHECK; this down migration
-- assumes the rollback runs before any acceptance_transcript artifact is
-- persisted (the additive-change rollback contract 0051 and 0073 established —
-- revert before the new kind is used). Rows of every prior kind are untouched.
ALTER TABLE artifacts DROP CONSTRAINT artifacts_kind_check;
ALTER TABLE artifacts ADD CONSTRAINT artifacts_kind_check CHECK (
    kind IN ('plan', 'pull_request', 'deployment', 'acceptance', 'release_notes', 'grooming_report')
);
