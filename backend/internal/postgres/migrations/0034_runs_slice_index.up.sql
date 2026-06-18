-- 0034: runs.slice_index — the decomposed child's sub_plan position
-- (E24.1 / #1141 / ADR-041). Nullable INTEGER: NULL for non-decomposed
-- runs (standalone parents and ordinary runs), set to the 0-based
-- sub_plan index on each child minted during orchestrator fanout. The
-- runner reads it back off the prompt-fetch response to route the child
-- onto its own sole-writer slice branch fishhawk/run-<parent>/slice-<n>.
ALTER TABLE runs
    ADD COLUMN slice_index INTEGER;
