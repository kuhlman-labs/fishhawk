ALTER TABLE stages DROP CONSTRAINT stages_state_check;
ALTER TABLE stages ADD CONSTRAINT stages_state_check CHECK (
    state IN ('pending', 'dispatched', 'running', 'awaiting_approval',
              'succeeded', 'failed', 'cancelled')
);

DROP INDEX IF EXISTS runs_decomposed_from_idx;
ALTER TABLE runs DROP COLUMN decomposed_from;
