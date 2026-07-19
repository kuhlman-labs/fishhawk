-- 0057: E44.6 Postgres Row-Level Security backstop (ADR-057, #1830).
--
-- ENABLE + FORCE ROW LEVEL SECURITY on every account-scoped table, with one
-- permissive FOR ALL policy per table reading the per-transaction GUC
-- app.account_id (set via SET LOCAL by backend/internal/postgres/tenant.go):
--
--   * The eight 0055 root tables carrying account_id (runs, campaigns,
--     refinement_drafts, refinement_decisions, refinement_filing_sessions,
--     refinement_filed_items, api_tokens, audit_entries) plus 0056's
--     sessions use the column predicate directly.
--   * stages has no account_id column; its policy scopes via an EXISTS
--     subquery against its parent run (runs.id is the PK, so the per-row
--     probe is an index lookup).
--
-- Predicate semantics:
--   * account_id IS NULL rows stay universally visible — the untenanted-allow
--     window matching #1829's handler checks; a later child tightens this
--     once every row is populated.
--   * current_setting('app.account_id', true) is missing_ok=true: an UNSET
--     GUC yields NULL (not an error), and NULLIF maps the empty string a
--     reverted SET LOCAL leaves behind on a pooled session to NULL too — so
--     a request that never set a tenant fails CLOSED to NULL-account rows
--     only, never errors and never sees another account's rows.
--   * The same predicate is the WITH CHECK, so cross-account INSERT/UPDATE
--     is refused at the database, not just filtered on read.
--
-- FORCE is required because the application connects as the table owner.
-- CAVEAT (binding to the rollout, not this migration): superusers bypass RLS
-- even under FORCE, and today's runtime/migration role `fishhawk` is a
-- superuser — so these policies are defined + tested (rls_test.go proves
-- refusal under a non-superuser NOBYPASSRLS role) but INERT in production
-- until a follow-up moves the runtime to a non-superuser app role with a
-- BYPASSRLS system context for cross-account reconciler/ticker scans.

ALTER TABLE runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE runs FORCE ROW LEVEL SECURITY;
CREATE POLICY runs_tenant_isolation ON runs
    USING (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);

ALTER TABLE campaigns ENABLE ROW LEVEL SECURITY;
ALTER TABLE campaigns FORCE ROW LEVEL SECURITY;
CREATE POLICY campaigns_tenant_isolation ON campaigns
    USING (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);

ALTER TABLE refinement_drafts ENABLE ROW LEVEL SECURITY;
ALTER TABLE refinement_drafts FORCE ROW LEVEL SECURITY;
CREATE POLICY refinement_drafts_tenant_isolation ON refinement_drafts
    USING (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);

ALTER TABLE refinement_decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE refinement_decisions FORCE ROW LEVEL SECURITY;
CREATE POLICY refinement_decisions_tenant_isolation ON refinement_decisions
    USING (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);

ALTER TABLE refinement_filing_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE refinement_filing_sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY refinement_filing_sessions_tenant_isolation ON refinement_filing_sessions
    USING (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);

ALTER TABLE refinement_filed_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE refinement_filed_items FORCE ROW LEVEL SECURITY;
CREATE POLICY refinement_filed_items_tenant_isolation ON refinement_filed_items
    USING (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);

ALTER TABLE api_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE api_tokens FORCE ROW LEVEL SECURITY;
CREATE POLICY api_tokens_tenant_isolation ON api_tokens
    USING (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);

ALTER TABLE audit_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_entries FORCE ROW LEVEL SECURITY;
CREATE POLICY audit_entries_tenant_isolation ON audit_entries
    USING (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);

ALTER TABLE sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY sessions_tenant_isolation ON sessions
    USING (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id IS NULL OR account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);

-- stages scopes via its parent run. The EXISTS predicate restates the runs
-- policy inline so a stage's visibility never depends on how RLS composes
-- with policy subqueries: a stage is visible/writable exactly when its run is.
ALTER TABLE stages ENABLE ROW LEVEL SECURITY;
ALTER TABLE stages FORCE ROW LEVEL SECURITY;
CREATE POLICY stages_tenant_isolation ON stages
    USING (EXISTS (
        SELECT 1 FROM runs r
         WHERE r.id = stages.run_id
           AND (r.account_id IS NULL OR r.account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)))
    WITH CHECK (EXISTS (
        SELECT 1 FROM runs r
         WHERE r.id = stages.run_id
           AND (r.account_id IS NULL OR r.account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)));
