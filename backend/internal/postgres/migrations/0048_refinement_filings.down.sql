-- 0048 down: drop the filing ledger tables. refinement_filed_items first (it
-- FK-references refinement_filing_sessions), then refinement_filing_sessions.
-- Purely additive migration, so the rollback is a clean DROP with no
-- data-normalization step. 0046's refinement_drafts and 0047's
-- refinement_decisions are untouched. Already-filed tracker issues created
-- before a rollback are durable external artifacts by design and need no
-- cleanup.

DROP TABLE IF EXISTS refinement_filed_items;
DROP TABLE IF EXISTS refinement_filing_sessions;
