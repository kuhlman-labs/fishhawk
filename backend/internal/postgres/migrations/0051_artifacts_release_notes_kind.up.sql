-- 0051: widen the artifacts kind CHECK to admit 'release_notes' (E33.2 /
-- #1587, ADR-051 option B).
--
-- E33's release-notes persist endpoint records a `release_notes` artifact —
-- the durable record capturing the evidence-derived markdown assembled from
-- the releaseevidence model (per-change summary, plan link, reviewer verdicts,
-- acceptance outcome, deferred concerns, and the per-release cost rollup). The
-- artifacts kind is a CLOSED set enforced by `artifacts_kind_check`
-- (migration 0002, widened by 0037 and 0045), so a Create with the new kind
-- fails with SQLSTATE 23514 (check_violation) until the CHECK is widened to
-- admit it — the constant (artifact.KindReleaseNotes) and this migration MUST
-- ship together, exactly as 0037 paired with KindDeployment and 0045 with
-- KindAcceptance.
--
-- Additive: existing 'plan'/'pull_request'/'deployment'/'acceptance' rows are
-- untouched; this only broadens what NEW rows may carry.
ALTER TABLE artifacts DROP CONSTRAINT artifacts_kind_check;
ALTER TABLE artifacts ADD CONSTRAINT artifacts_kind_check CHECK (
    kind IN ('plan', 'pull_request', 'deployment', 'acceptance', 'release_notes')
);
