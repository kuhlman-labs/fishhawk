-- 0085: campaign_items.resolved_by — the DURABLE provenance of HOW a campaign
-- item reached its terminal state, so the engine can tell an issue-closed
-- settle apart from an operator action (#3563 / E72.24).
--
-- WHY THIS COLUMN EXISTS — the Restartable suppression is load-bearing, not
-- cosmetic. The reconcile-on-read issue-closed settle now settles an item
-- CANCELLED when its issue was closed not_planned (or duplicate): the work was
-- abandoned, not delivered. Without a durable marker that cancellation is
-- byte-indistinguishable from an OPERATOR cancellation, and campaign.NextEligible
-- would re-offer it in the Restartable slice — surfacing `start_run` on an
-- abandoned issue, the exact thing the abandonment means must not happen. The
-- marker is what lets NextEligible suppress it while leaving every
-- operator-cancelled item restartable exactly as today.
--
-- It is written ATOMICALLY with the settling state UPDATE (one statement,
-- SettleCampaignItemForClosedIssue), so there is no window in which a
-- cancelled item lacks its marker and is therefore offered as Restartable.
--
-- ADDITIVE with a NOT NULL DEFAULT '': every pre-0085 row reads as 'no recorded
-- resolution provenance' and behaves exactly as today (an operator-cancelled
-- item stays Restartable). No row is rewritten and no backfill is needed.
--
-- The CHECK is fail-closed, mirroring 0049's autonomy CHECK: only the empty
-- marker plus the one recognised value are admitted, so a typo'd or out-of-set
-- value is rejected at write time rather than silently persisting a marker the
-- engine cannot interpret.

ALTER TABLE campaign_items
    ADD COLUMN resolved_by TEXT NOT NULL DEFAULT '',
    ADD CONSTRAINT campaign_items_resolved_by_check CHECK (
        resolved_by IN ('', 'issue_closed')
    );
