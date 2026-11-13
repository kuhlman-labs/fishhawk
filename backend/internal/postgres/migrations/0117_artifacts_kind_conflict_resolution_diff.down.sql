-- 0116 down: restore the nine-value artifacts kind CHECK that 0098 set
-- ('plan', 'pull_request', 'deployment', 'acceptance', 'release_notes',
-- 'grooming_report', 'acceptance_transcript', 'upkeep_report',
-- 'comms_report'). Any 'conflict_resolution_diff' artifact rows written while
-- 0116 was applied would violate the restored CHECK; this down migration
-- assumes the rollback runs before any conflict_resolution_diff artifact is
-- persisted (the additive-change rollback contract 0083, 0095 and 0098
-- established — revert before the new kind is used). If such rows exist,
-- delete them first: DELETE FROM artifacts WHERE kind =
-- 'conflict_resolution_diff'. They are review inputs only. Rows of every prior
-- kind are untouched.
ALTER TABLE artifacts DROP CONSTRAINT artifacts_kind_check;
ALTER TABLE artifacts ADD CONSTRAINT artifacts_kind_check CHECK (
    kind IN ('plan', 'pull_request', 'deployment', 'acceptance', 'release_notes', 'grooming_report', 'acceptance_transcript', 'upkeep_report', 'comms_report')
);
