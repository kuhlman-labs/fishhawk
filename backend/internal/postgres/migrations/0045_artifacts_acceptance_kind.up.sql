-- 0045: widen the artifacts kind CHECK to admit 'acceptance' (E31.3 /
-- #1531, ADR-049 / #1519).
--
-- ADR-049's acceptance stage records a signed `acceptance` artifact — the
-- durable evidence record capturing a structured verdict + per-criterion
-- results + content_hash references to customer-side evidence blobs. The
-- artifacts kind is a CLOSED set enforced by `artifacts_kind_check`
-- (migration 0002, widened by 0037), so a Create with the new kind fails
-- with SQLSTATE 23514 (check_violation) until the CHECK is widened to admit
-- it — the constant (artifact.KindAcceptance) and this migration MUST ship
-- together, exactly as 0037 paired with KindDeployment.
--
-- Additive: existing 'plan'/'pull_request'/'deployment' rows are untouched;
-- this only broadens what NEW rows may carry. The acceptance audit
-- categories (acceptance_dispatched / acceptance_outcome_recorded /
-- acceptance_triage_decided) need NO migration — audit_entries.category is
-- TEXT NOT NULL with only an index (no CHECK), so they are pure open-set
-- strings.
ALTER TABLE artifacts DROP CONSTRAINT artifacts_kind_check;
ALTER TABLE artifacts ADD CONSTRAINT artifacts_kind_check CHECK (
    kind IN ('plan', 'pull_request', 'deployment', 'acceptance')
);
