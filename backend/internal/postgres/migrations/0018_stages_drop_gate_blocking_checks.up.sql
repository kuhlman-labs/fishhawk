-- 0018: drop stages.gate_blocking_checks (#254 / ADR-017).
--
-- Migration 0014 added the column alongside gate_type and
-- gate_approvers to persist the workflow-spec gate's blocking-checks
-- list. ADR-017 (#249) decoupled the approval surface from CI state
-- entirely: required CI checks are derived from GitHub branch
-- protection at run-create time and snapshotted onto runs.
-- required_checks_snapshot (#251). The spec-level field was removed
-- in v0.2 (#254); the column has no remaining writer or reader.

ALTER TABLE stages
    DROP COLUMN IF EXISTS gate_blocking_checks;
