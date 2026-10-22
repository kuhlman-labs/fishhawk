-- 0109: ADR-112 R3 (bounded head-of-line approval wait, then yield) and R5
-- (yield visibility), E89.28 / #4632.
--
-- merge_candidate_queue_entries.yield_reason / yield_detail / yielded_at are
-- the YIELD MARKER: a held entry whose forge approval did not qualify at the
-- head of the line steps aside. backend/internal/mergequeue's AdmitNext skips
-- every marked row, so a marked row never blocks the base; clearing the marker
-- (ClearYield, a compare-and-swap on (anchored_head_sha, yielded_at)) is the
-- only way back in, and a cleared row rejoins FIFO at its original
-- enqueued_at. yield_reason is a closed vocabulary owned by the server
-- (merge_queue_yield.go); yield_detail is untrusted forge-derived text the
-- read surfaces sanitise. The marker is set exactly when yielded_at is.
--
-- merge_candidate_queue_entries.approval_wait_started_at is the persisted
-- anchor of the head-of-line approval wait: stamped once (COALESCE) by
-- Store.ApprovalWait on the first non-qualifying observation, never reset by
-- a readvance (Unadmit), a re-admission or a stale re-claim, and cleared only
-- with the marker. Expiry is judged against it in SQL (database clock).
--
-- A live row never carries a marker (a yield moves the row to held in the
-- same statement); a terminal row keeps it as history.
--
-- Additive: four nullable columns and two CHECKs every existing row satisfies
-- (all four NULL), so a reverted binary keeps processing rows; a reverted
-- binary admits a still-marked held row FIFO, and merge-on-pass's in-lock
-- forge-approval fold still refuses its merge.
ALTER TABLE merge_candidate_queue_entries
    ADD COLUMN yield_reason TEXT NULL,
    ADD COLUMN yield_detail TEXT NULL,
    ADD COLUMN yielded_at TIMESTAMPTZ NULL,
    ADD COLUMN approval_wait_started_at TIMESTAMPTZ NULL,
    -- The marker is all-or-nothing: yielded_at is set exactly when a
    -- non-empty yield_reason is.
    ADD CONSTRAINT merge_candidate_queue_entries_yield_marker_check
        CHECK ((yielded_at IS NULL) = (yield_reason IS NULL) AND COALESCE(yield_reason, 'x') <> ''),
    -- A live row never carries a marker.
    ADD CONSTRAINT merge_candidate_queue_entries_yield_live_check
        CHECK (state <> 'live' OR yielded_at IS NULL);
