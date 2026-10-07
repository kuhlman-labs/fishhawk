-- 0098: widen the artifacts kind CHECK to admit 'comms_report'
-- (E81.5 / #3775, phase 1 of the user-report scan).
--
-- A `plan`-typed PROPOSE stage running the user-report-scan workflow emits a
-- comms_report — draft responses and issues proposed from user-authored
-- reports — instead of a plan, and the comms_report ingest handler (#4015)
-- persists it as a `comms_report` artifact. The artifacts kind is a CLOSED set
-- enforced by `artifacts_kind_check` (migration 0002, widened by 0037, 0045,
-- 0051, 0073, 0083 and 0095), so a Create with the new kind fails with
-- SQLSTATE 23514 (check_violation) until the CHECK is widened to admit it —
-- the constant (artifact.KindCommsReport) and this migration MUST ship
-- together, exactly as 0095 paired with KindUpkeepReport.
--
-- PostgreSQL cannot alter a CHECK constraint's expression in place (ALTER
-- CONSTRAINT applies only to foreign-key constraint attributes), which is why
-- this — like 0037, 0045, 0051, 0073, 0083 and 0095 — DROPs and re-ADDs the
-- constraint.
--
-- Additive: existing rows of every prior kind are untouched; this only
-- broadens what NEW rows may carry.
ALTER TABLE artifacts DROP CONSTRAINT artifacts_kind_check;
ALTER TABLE artifacts ADD CONSTRAINT artifacts_kind_check CHECK (
    kind IN ('plan', 'pull_request', 'deployment', 'acceptance', 'release_notes', 'grooming_report', 'acceptance_transcript', 'upkeep_report', 'comms_report')
);
