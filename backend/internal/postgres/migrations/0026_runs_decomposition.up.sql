-- Migration 0026: add decomposed_from to runs + awaiting_children stage state.
--
-- decomposed_from links a child run back to the parent run that
-- minted it during orchestrator fanout. NULL for non-decomposed runs.
--
-- awaiting_children is a non-terminal stage state the parent's
-- implement stage enters when the plan carries a decomposition block.
-- The child-completion sweeper transitions it to succeeded/failed once
-- all children reach a terminal state.

ALTER TABLE runs
    ADD COLUMN decomposed_from UUID REFERENCES runs(id) ON DELETE SET NULL;

CREATE INDEX runs_decomposed_from_idx
    ON runs (decomposed_from)
    WHERE decomposed_from IS NOT NULL;

-- stages_state_check must include 'awaiting_children'.
ALTER TABLE stages DROP CONSTRAINT stages_state_check;
ALTER TABLE stages ADD CONSTRAINT stages_state_check CHECK (
    state IN ('pending', 'dispatched', 'running', 'awaiting_approval',
              'awaiting_children', 'succeeded', 'failed', 'cancelled')
);
