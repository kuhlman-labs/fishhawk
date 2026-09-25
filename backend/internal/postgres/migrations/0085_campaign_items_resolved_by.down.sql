-- Down-migration for 0085: drop campaign_items.resolved_by.
--
-- The inline CHECK constraint (campaign_items_resolved_by_check) is dropped
-- WITH the column — PostgreSQL removes a constraint whose only referenced
-- column is dropped — so no separate DROP CONSTRAINT is needed. The column has
-- no index and no view depending on it, so the DROP is mechanical and
-- idempotent (IF EXISTS).
--
-- ROLLED-BACK CONSEQUENCE, stated plainly: previously-settled rows keep their
-- state (succeeded or cancelled) and lose only the provenance. An issue-closed
-- CANCELLATION therefore becomes indistinguishable from an operator
-- cancellation, and campaign.NextEligible offers it as Restartable / start_run
-- again — the pre-0085 behaviour, not a new failure mode.

ALTER TABLE campaign_items
    DROP COLUMN IF EXISTS resolved_by;
