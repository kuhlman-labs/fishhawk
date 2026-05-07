-- 0014: persist the workflow-spec gate shape on each stage so the
-- review-stage UI and future surfaces can render the gate's
-- blocking_checks + approvers without re-parsing the workflow spec
-- at request time. (#213)
--
-- A stage's `gates:` block in workflows.yaml may contain multiple
-- gates. v0 persists the *primary* gate per stage:
--
--   primary = first approval gate (if any),
--             else first check gate,
--             else NULL.
--
-- This mirrors the scoping of `gate_sla` and `requires_approval`
-- already on this table; both refer to the first approval gate.
--
-- The new columns are independent of `requires_approval`:
-- `requires_approval` records "has an approval gate at all" for the
-- trace upload handler's transition decision (#207); `gate_type`
-- distinguishes approval-vs-check for the review page, where check-
-- only gates suppress the approval panel.
--
-- All three columns are nullable so legacy rows (and rows seeded by
-- tests that don't care about gates) keep working. New rows get
-- the right values at create time per the dispatcher.

ALTER TABLE stages
    ADD COLUMN gate_type            TEXT,
    ADD COLUMN gate_blocking_checks TEXT[],
    ADD COLUMN gate_approvers       JSONB,
    ADD CONSTRAINT stages_gate_type_check
        CHECK (gate_type IS NULL OR gate_type IN ('approval', 'check'));
