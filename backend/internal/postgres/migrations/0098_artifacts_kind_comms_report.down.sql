-- 0098 down: restore the eight-value artifacts kind CHECK that 0095 set
-- ('plan', 'pull_request', 'deployment', 'acceptance', 'release_notes',
-- 'grooming_report', 'acceptance_transcript', 'upkeep_report'). Any
-- 'comms_report' artifact rows written while 0098 was applied would violate
-- the restored CHECK; this down migration assumes the rollback runs before any
-- comms_report artifact is persisted (the additive-change rollback contract
-- 0051, 0073, 0083 and 0095 established — revert before the new kind is used).
-- Rows of every prior kind are untouched.
ALTER TABLE artifacts DROP CONSTRAINT artifacts_kind_check;
ALTER TABLE artifacts ADD CONSTRAINT artifacts_kind_check CHECK (
    kind IN ('plan', 'pull_request', 'deployment', 'acceptance', 'release_notes', 'grooming_report', 'acceptance_transcript', 'upkeep_report')
);
