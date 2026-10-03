-- 0095: widen the artifacts kind CHECK to admit 'upkeep_report'
-- (E79.2 / #3726, phase 2 of the upkeep scan).
--
-- A `plan`-typed PROPOSE stage running the upkeep-scan workflow emits an
-- upkeep_report — recorded-run flake, toolchain-pin drift and deprecation
-- findings to file — instead of a plan, and the upkeep_report ingest handler
-- (#3921) persists it as an `upkeep_report` artifact. The artifacts kind is a
-- CLOSED set enforced by `artifacts_kind_check` (migration 0002, widened by
-- 0037, 0045, 0051, 0073 and 0083), so a Create with the new kind fails with
-- SQLSTATE 23514 (check_violation) until the CHECK is widened to admit it —
-- the constant (artifact.KindUpkeepReport) and this migration MUST ship
-- together, exactly as 0073 paired with KindGroomingReport and 0083 with
-- KindAcceptanceTranscript.
--
-- PostgreSQL cannot alter a CHECK constraint's expression in place (ALTER
-- CONSTRAINT applies only to foreign-key constraint attributes), which is why
-- this — like 0037, 0045, 0051, 0073 and 0083 — DROPs and re-ADDs the
-- constraint.
--
-- Additive: existing rows of every prior kind are untouched; this only
-- broadens what NEW rows may carry.
ALTER TABLE artifacts DROP CONSTRAINT artifacts_kind_check;
ALTER TABLE artifacts ADD CONSTRAINT artifacts_kind_check CHECK (
    kind IN ('plan', 'pull_request', 'deployment', 'acceptance', 'release_notes', 'grooming_report', 'acceptance_transcript', 'upkeep_report')
);
