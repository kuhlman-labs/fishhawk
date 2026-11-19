-- 0118 down: restore PRIMARY KEY (stage_id) on stage_concurrency_slots
-- (#4240). Run it only in the rollback order: stop the new binary, migrate
-- down, then start the reverted binary (0118.up records why).
--
-- FAIL-CLOSED for a held multi-group stage. Collapsing such a stage would drop
-- real holds, and the reverted binary would then see those groups free. So
-- while any stage has more than one row and holds them, this migration
-- RAISES, naming the stage, and changes nothing. Drain it first: wait for the
-- stage to settle, then delete its rows (a settled stage's held rows no
-- longer count for anyone). Confirm with:
--   SELECT stage_id FROM stage_concurrency_slots
--   GROUP BY stage_id HAVING count(*) > 1 AND bool_or(state = 'held');
--
-- A QUEUED-only multi-row stage loses nothing that matters: it keeps its
-- canonical-first row (byte order, COLLATE "C", the order the store sorts
-- group keys in), and on its next poll it re-asks with its single group and
-- restarts at the tail.
--
-- Also confirm, before rolling back, that no non-terminal run's captured
-- workflow spec declares concurrency.groups (0118.up gives the query), or
-- cancel those runs: a reverted binary cannot parse such a spec.
DO $$
DECLARE
    held_stage UUID;
    held_rows  BIGINT;
BEGIN
    SELECT stage_id, count(*) INTO held_stage, held_rows
    FROM stage_concurrency_slots
    GROUP BY stage_id
    HAVING count(*) > 1 AND bool_or(state = 'held')
    ORDER BY stage_id
    LIMIT 1;
    IF held_stage IS NOT NULL THEN
        RAISE EXCEPTION 'rollback of 0118 refused: stage % holds % concurrency slot rows (a multi-group hold); collapsing them would drop real holds', held_stage, held_rows
            USING HINT = 'Drain first: wait for the stage to settle, then DELETE FROM stage_concurrency_slots WHERE stage_id = ''' || held_stage || '''; and retry.';
    END IF;
END;
$$;

DELETE FROM stage_concurrency_slots a
USING stage_concurrency_slots b
WHERE a.stage_id = b.stage_id
  AND a.group_key COLLATE "C" > b.group_key COLLATE "C";

ALTER TABLE stage_concurrency_slots
    DROP CONSTRAINT stage_concurrency_slots_pkey,
    ADD CONSTRAINT stage_concurrency_slots_pkey PRIMARY KEY (stage_id);
