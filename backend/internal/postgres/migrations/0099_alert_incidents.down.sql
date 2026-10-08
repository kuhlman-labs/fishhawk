-- Down-migration for 0099: drop alert_incidents.
--
-- Roll 0100 (runs_trigger_source_check admitting 'alert') back FIRST; this
-- file touches only the dedup ledger and leaves runs untouched.
--
-- The dedup history is lost, so a repeat alert for a fingerprint filed before
-- the rollback files a NEW incident issue after a re-apply. The issues
-- already filed stay on the forge (they carry the hidden idempotency marker
-- naming the source, repo and fingerprint, for manual reconciliation).
DROP TABLE alert_incidents;
