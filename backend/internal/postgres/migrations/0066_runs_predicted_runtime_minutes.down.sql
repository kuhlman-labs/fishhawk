-- Reverse 0066 (E48.62 / #2489): drop the runs.predicted_runtime_minutes
-- column.
--
-- Lossless in practice — the value is a cached copy of the approved plan
-- artifact's predicted_runtime_minutes and is re-derivable from that artifact
-- at any time. Nothing gates on it: a reverted binary stops writing and
-- reading it and the advertised stage-wait poll interval returns to the flat
-- 30s floor. Pinned by TestMigrateDown_RunsPredictedRuntimeMinutesReversal.

ALTER TABLE runs DROP COLUMN IF EXISTS predicted_runtime_minutes;
