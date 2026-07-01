-- Migration 0044: widen stages_type_check to admit the 'acceptance' stage
-- type (ADR-049 / #1519, E31.2).
--
-- The acceptance stage TYPE landed in the Go model (run.StageTypeAcceptance)
-- in this same slice; without this migration the stages_type_check
-- (plan/implement/review/deploy, migration 0038) would never learn the
-- 'acceptance' value and a real acceptance stage row would be uninsertable
-- (SQLSTATE 23514, check_violation) — the exact deploy failure mode #1390 /
-- #1399 hit. Constant + migration MUST ship together; the
-- TestPostgres_AcceptanceStage_PersistRoundTrip round-trip is the done-means.
--
-- Unlike 0038, stages_state_check is DELIBERATELY UNTOUCHED. Acceptance is a
-- runner-hosted advisory agent stage (ADR-049 Recommendation #3): it rides the
-- existing agent-stage lifecycle (pending → dispatched → running →
-- awaiting_approval/succeeded/failed/cancelled) exactly like review, so no new
-- stage state is needed. Deploy's two extra states existed solely for its
-- pre-execution gate park and external-pipeline poll, neither of which applies.
--
-- Additive: this only broadens what NEW rows may carry; existing
-- plan/implement/review/deploy rows are untouched. No column add/drop and no
-- data migration. stages_executor_kind_check (agent/human) needs no change: an
-- acceptance stage is created with a valid existing executor kind.
ALTER TABLE stages DROP CONSTRAINT stages_type_check;
ALTER TABLE stages ADD CONSTRAINT stages_type_check CHECK (
    stage_type IN ('plan', 'implement', 'review', 'deploy', 'acceptance')
);
