-- 0013: persist whether a stage requires human approval at the
-- workflow-spec level, so the trace upload handler can pick the
-- right post-upload transition without re-parsing the spec.
--
-- Per workflows.yaml's `gates:` block, a stage either has an
-- approval-typed gate (plan, review) or it doesn't (implement).
-- Pre-#207 the trace handler unconditionally walked
-- dispatched → running → awaiting_approval, leaving gateless
-- stages stuck waiting for a human action that the workflow
-- author never specified. This column captures the workflow's
-- intent at stage-create time so the handler can branch:
--
--   requires_approval = TRUE  →  trace upload walks to
--                                 awaiting_approval (existing
--                                 plan-stage behavior)
--   requires_approval = FALSE →  trace upload walks to succeeded
--                                 directly; orchestrator dispatches
--                                 the next stage immediately
--
-- Default FALSE is safe for backfill: any pre-existing rows are
-- mid-flight and already either resolved their approval or are
-- stuck in awaiting_approval. New rows get the right value at
-- create time per the dispatcher.

ALTER TABLE stages
    ADD COLUMN requires_approval BOOLEAN NOT NULL DEFAULT FALSE;
