-- 0041: campaign-level operator_agent override — a campaign tightens or
-- relaxes the per-workflow delegation contract for ALL its issue-runs (Track E
-- follow-on of ADR-047 / #1451, E25.12).
--
-- The override is the outermost rung of the delegation resolution ladder
-- consumed by the auto-driver: campaign-level > gate-level > workflow-level,
-- wholesale (matching spec.Workflow.EffectiveOperatorAgent's never-merged
-- semantics). It is carried opaquely as a nullable JSONB blob — validated once
-- at create time against the spec.OperatorAgent Go type, NOT by a column CHECK,
-- so the campaign table stays decoupled from the spec.
--
-- This migration is additive: a nullable column with no default. Existing
-- campaigns get operator_agent NULL and resolve their delegation exactly as
-- today (each issue-run inherits its workflow's contract). No row is rewritten.

ALTER TABLE campaigns
    ADD COLUMN operator_agent JSONB;
