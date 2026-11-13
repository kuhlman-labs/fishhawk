-- 0116: widen the artifacts kind CHECK to admit 'conflict_resolution_diff'
-- (#4795, the conflict-resolution re-review).
--
-- A conflict_resolution_pushed report from a runner the backend advertised
-- the summary capability to carries the resolved paths and the remerge-diff
-- of the merge commit. The report handler scans the raw patch for secrets,
-- redacts it, and persists it as a `conflict_resolution_diff` artifact on the
-- implement stage, which the conflict-resolution review round renders as an
-- additional prompt section. The artifacts kind is a CLOSED set enforced by
-- `artifacts_kind_check` (migration 0002, widened by 0037, 0045, 0051, 0073,
-- 0083, 0095 and 0098), so a Create with the new kind fails with SQLSTATE
-- 23514 (check_violation) until the CHECK is widened to admit it — the
-- constant (artifact.KindConflictResolutionDiff) and this migration MUST ship
-- together, exactly as 0098 paired with KindCommsReport.
--
-- PostgreSQL cannot alter a CHECK constraint's expression in place (ALTER
-- CONSTRAINT applies only to foreign-key constraint attributes), which is why
-- this — like 0037, 0045, 0051, 0073, 0083, 0095 and 0098 — DROPs and re-ADDs
-- the constraint.
--
-- Additive: existing rows of every prior kind are untouched; this only
-- broadens what NEW rows may carry.
ALTER TABLE artifacts DROP CONSTRAINT artifacts_kind_check;
ALTER TABLE artifacts ADD CONSTRAINT artifacts_kind_check CHECK (
    kind IN ('plan', 'pull_request', 'deployment', 'acceptance', 'release_notes', 'grooming_report', 'acceptance_transcript', 'upkeep_report', 'comms_report', 'conflict_resolution_diff')
);
