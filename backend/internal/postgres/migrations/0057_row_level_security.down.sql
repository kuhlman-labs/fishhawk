-- Revert 0057: drop every tenant-isolation policy and disable (including
-- un-FORCE) row-level security on all ten tables. Purely declarative going
-- up (no data transformed), so the rollback loses nothing — the tables
-- simply stop carrying RLS.

DROP POLICY stages_tenant_isolation ON stages;
ALTER TABLE stages NO FORCE ROW LEVEL SECURITY;
ALTER TABLE stages DISABLE ROW LEVEL SECURITY;

DROP POLICY sessions_tenant_isolation ON sessions;
ALTER TABLE sessions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE sessions DISABLE ROW LEVEL SECURITY;

DROP POLICY audit_entries_tenant_isolation ON audit_entries;
ALTER TABLE audit_entries NO FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_entries DISABLE ROW LEVEL SECURITY;

DROP POLICY api_tokens_tenant_isolation ON api_tokens;
ALTER TABLE api_tokens NO FORCE ROW LEVEL SECURITY;
ALTER TABLE api_tokens DISABLE ROW LEVEL SECURITY;

DROP POLICY refinement_filed_items_tenant_isolation ON refinement_filed_items;
ALTER TABLE refinement_filed_items NO FORCE ROW LEVEL SECURITY;
ALTER TABLE refinement_filed_items DISABLE ROW LEVEL SECURITY;

DROP POLICY refinement_filing_sessions_tenant_isolation ON refinement_filing_sessions;
ALTER TABLE refinement_filing_sessions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE refinement_filing_sessions DISABLE ROW LEVEL SECURITY;

DROP POLICY refinement_decisions_tenant_isolation ON refinement_decisions;
ALTER TABLE refinement_decisions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE refinement_decisions DISABLE ROW LEVEL SECURITY;

DROP POLICY refinement_drafts_tenant_isolation ON refinement_drafts;
ALTER TABLE refinement_drafts NO FORCE ROW LEVEL SECURITY;
ALTER TABLE refinement_drafts DISABLE ROW LEVEL SECURITY;

DROP POLICY campaigns_tenant_isolation ON campaigns;
ALTER TABLE campaigns NO FORCE ROW LEVEL SECURITY;
ALTER TABLE campaigns DISABLE ROW LEVEL SECURITY;

DROP POLICY runs_tenant_isolation ON runs;
ALTER TABLE runs NO FORCE ROW LEVEL SECURITY;
ALTER TABLE runs DISABLE ROW LEVEL SECURITY;
