-- 0073: widen the artifacts kind CHECK to admit 'grooming_report' (E54.3 /
-- #2235, ADR-065 §3).
--
-- E54's backlog-grooming propose stage records a `grooming_report` artifact —
-- the durable proposal capturing the proposed ordering (each entry citing at
-- least one charter rubric line), duplicate candidates, hygiene defects,
-- suggested depends_on edges, vision-drift flags, and decomposition
-- suggestions, validated against grooming-report-v1. The artifacts kind is a
-- CLOSED set enforced by `artifacts_kind_check` (migration 0002, widened by
-- 0037, 0045 and 0051), so a Create with the new kind fails with SQLSTATE
-- 23514 (check_violation) until the CHECK is widened to admit it — the
-- constant (artifact.KindGroomingReport) and this migration MUST ship
-- together, exactly as 0037 paired with KindDeployment, 0045 with
-- KindAcceptance and 0051 with KindReleaseNotes.
--
-- PostgreSQL cannot alter a CHECK constraint's expression in place (ALTER
-- CONSTRAINT applies only to foreign-key constraint attributes), which is why
-- this — like 0037, 0045 and 0051 — DROPs and re-ADDs the constraint.
--
-- Additive: existing 'plan'/'pull_request'/'deployment'/'acceptance'/
-- 'release_notes' rows are untouched; this only broadens what NEW rows may
-- carry.
ALTER TABLE artifacts DROP CONSTRAINT artifacts_kind_check;
ALTER TABLE artifacts ADD CONSTRAINT artifacts_kind_check CHECK (
    kind IN ('plan', 'pull_request', 'deployment', 'acceptance', 'release_notes', 'grooming_report')
);
