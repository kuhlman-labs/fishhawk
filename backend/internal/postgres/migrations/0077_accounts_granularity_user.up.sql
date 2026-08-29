-- 0077: widen the accounts granularity CHECK to admit 'user' (E44.35 /
-- #2925).
--
-- The multi-tenant login gate (E44.3 / E44.8) admits a sign-in only against an
-- existing accounts row whose (account_key, granularity) PAIR the user's
-- memberships derive. A personal-namespace self-host — one whose owner has no
-- enterprise, organization or group to auto-join through — had no granularity
-- to name: accounts.granularity was a CLOSED set enforced by
-- accounts_granularity_check (migration 0052, IN ('enterprise','organization',
-- 'group')), so a bootstrap keyed by the owner's login could not write a row.
-- The 'user' tier closes that gap: its membership predicate is
-- `authenticated login == account_key`, needing NO live-forge read (auth.
-- granularityUser, backend/internal/auth/membership.go).
--
-- The constant (auth.granularityUser and the account.accountGranularities
-- entry) and this migration MUST ship together, exactly as 0051 paired
-- artifacts_kind_check's 'release_notes' widening with KindReleaseNotes and
-- 0054 paired runs_runner_kind_check's 'gitlab_ci' widening with
-- RunnerKindGitLabCI — otherwise an accounts row with granularity='user'
-- fails with SQLSTATE 23514 (check_violation).
--
-- Growth path per ADR-022: new granularities extend the enum via a follow-up
-- migration that DROPs and re-ADDs the CHECK (PostgreSQL has no ALTER
-- CONSTRAINT form for a CHECK expression). Mirrors 0051 / 0054.
--
-- Additive: existing 'enterprise'/'organization'/'group' rows are untouched;
-- this only broadens what NEW rows may carry. No data is rewritten.
ALTER TABLE accounts DROP CONSTRAINT accounts_granularity_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_granularity_check CHECK (
    granularity IN ('enterprise', 'organization', 'group', 'user')
);
