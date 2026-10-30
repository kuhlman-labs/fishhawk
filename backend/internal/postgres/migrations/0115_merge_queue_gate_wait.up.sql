-- 0115: docs/adr/112 Amendment A3 (#4780): the merge queue's merge-gate
-- legs hold or yield an intent-bearing entry instead of dropping it.
--
-- merge_candidate_queue_entries.yield_revision is the yield marker's
-- compare-and-swap key (A3.6): every marker write (Yield, Enqueue's mark,
-- UpdateYield, ClearYield, ReanchorYielded, and an insert carrying a marker)
-- bumps it in its own statement, and the yielded-row writes compare-and-swap
-- on (anchored_head_sha, yield_revision), so a delayed re-probe that read an
-- earlier revision can neither clear nor overwrite a newer marker even when
-- the episode continues the same yielded_at.
--
-- row4_flag is the persisted row-4 obligation (A3.6): set by any marker write
-- whose reason is forge_review_required (OR, so no reason transition clears
-- it) and cleared only by a ClearYield that read the fresh snapshot showing
-- the row-4 condition gone. Pre-0115 the obligation was the stored reason
-- itself, so every row yielded forge_review_required is backfilled with the
-- flag; without it an upgraded row-4 yield would clear on an approved fold
-- while the forge still reports BLOCKED.
--
-- gate_wait_started_at / gate_wait_epoch are the per-admission-epoch
-- merge-gate window (A3.2): its start and the admission epoch that opened it.
-- gate_hold_tokens / gate_tokens_refilled_at are the hold budget (A3.7: N = 3,
-- one token refilled per 60 minutes not holding). The merge-gate window never
-- reads or writes approval_wait_started_at.
--
-- approval_wait_open reports an approval wait is open (A3.7): set by
-- ApprovalWait and cleared by ClearYield, which now RETAINS
-- approval_wait_started_at, so a retained anchor after a clear never fires the
-- #4748 readvance deadline. Pre-0115 ClearYield nulled the anchor, so every
-- existing non-NULL anchor is an open wait and is backfilled as one.
--
-- Additive: every column carries a default (or is NULL) every existing row
-- satisfies, so a reverted binary ignores them.
ALTER TABLE merge_candidate_queue_entries
    ADD COLUMN yield_revision BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN row4_flag BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN gate_wait_started_at TIMESTAMPTZ NULL,
    ADD COLUMN gate_wait_epoch BIGINT NULL,
    ADD COLUMN gate_hold_tokens SMALLINT NOT NULL DEFAULT 3,
    ADD COLUMN gate_tokens_refilled_at TIMESTAMPTZ NULL,
    ADD COLUMN approval_wait_open BOOLEAN NOT NULL DEFAULT false,
    ADD CONSTRAINT merge_candidate_queue_entries_gate_hold_tokens_check
        CHECK (gate_hold_tokens BETWEEN 0 AND 3),
    -- The merge-gate window is all-or-nothing: its start and its epoch.
    ADD CONSTRAINT merge_candidate_queue_entries_gate_wait_check
        CHECK ((gate_wait_started_at IS NULL) = (gate_wait_epoch IS NULL));

UPDATE merge_candidate_queue_entries SET approval_wait_open = true
WHERE approval_wait_started_at IS NOT NULL;

UPDATE merge_candidate_queue_entries SET row4_flag = true
WHERE yielded_at IS NOT NULL AND yield_reason = 'forge_review_required';
