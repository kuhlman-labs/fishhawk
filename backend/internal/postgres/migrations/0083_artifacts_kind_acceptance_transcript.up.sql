-- 0083: widen the artifacts kind CHECK to admit 'acceptance_transcript'
-- (E72.5 / #3329).
--
-- The acceptance stage now records a per-criterion TRANSCRIPT beside the
-- boolean verdict: the requests sent, the responses observed, the assertion
-- and its outcome, shipped by the runner to POST
-- /v0/runs/{run_id}/acceptance/transcript and persisted as an
-- `acceptance_transcript` artifact the verdict's runner-injected `transcript`
-- ref is cross-checked against. The artifacts kind is a CLOSED set enforced
-- by `artifacts_kind_check` (migration 0002, widened by 0037, 0045, 0051 and
-- 0073), so a Create with the new kind fails with SQLSTATE 23514
-- (check_violation) until the CHECK is widened to admit it — the constant
-- (artifact.KindAcceptanceTranscript) and this migration MUST ship together,
-- exactly as 0073 paired with KindGroomingReport.
--
-- PostgreSQL cannot alter a CHECK constraint's expression in place (ALTER
-- CONSTRAINT applies only to foreign-key constraint attributes), which is why
-- this — like 0037, 0045, 0051 and 0073 — DROPs and re-ADDs the constraint.
--
-- Additive: existing rows of every prior kind are untouched; this only
-- broadens what NEW rows may carry.
ALTER TABLE artifacts DROP CONSTRAINT artifacts_kind_check;
ALTER TABLE artifacts ADD CONSTRAINT artifacts_kind_check CHECK (
    kind IN ('plan', 'pull_request', 'deployment', 'acceptance', 'release_notes', 'grooming_report', 'acceptance_transcript')
);
