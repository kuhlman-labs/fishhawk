-- 0076: runs.installation_ref, the ADR-057/ADR-058 forge-neutral credential
-- reference, plus runs_retry_child_once_idx, the at-most-one-retry-child
-- partial unique index (E45.22 / #2043).
--
-- PART 1 — installation_ref.
--
-- runs.installation_id is a bigint: it can only ever name a GitHub App
-- installation, so a gitlab_ci run has nowhere to record WHICH GitLab project
-- to authenticate as. forge.CredentialScope (backend/internal/forge/
-- credentials.go) already models the forge-neutral key as an opaque TEXT ref,
-- and its canonical GitHub form is the bare base-10 decimal of the
-- installation id (FromGitHubInstallationID = strconv.FormatInt(id, 10)).
-- This column is the persisted home of that ref.
--
-- NULLABLE ON PURPOSE. Three states are distinguishable and they are NOT
-- interchangeable downstream:
--   NULL  — no ref recorded: a pre-0076 row the backfill could not derive one
--           for, or a mint that stamped none. Consumers fall back to
--           installation_id (orchestrator.triggerParams) and the approval-side
--           forge derivation treats it as GitHub-by-ref while the runner_kind
--           signal still decides independently.
--   ''    — recorded-as-empty. Also falls back, but it is a DIFFERENT
--           provenance than NULL and callers must not collapse the two by
--           storing '' for "absent".
--   value — an authoritative ref ('12345' for GitHub, 'gitlab:<project_id>'
--           for GitLab).
--
-- BACKFILL SHAPE. Existing GitHub rows get the BARE DECIMAL, not a
-- 'github:'-prefixed string, because that is what forge.FromGitHubInstallationID
-- produces and what every GitHub credential consumer already parses
-- (CredentialScope.GitHubInstallationID does a bare strconv.ParseInt). A
-- prefixed backfill would make every migrated GitHub row resolve to an unknown
-- scheme.
--
-- installation_id = 0 IS DELIBERATELY EXCLUDED from the backfill. Zero is this
-- codebase's unresolved-installation sentinel: FromGitHubInstallationID(0)
-- returns the ZERO scope (empty ref), and the webhook path is known to persist
-- a pointer-to-zero for a run with no App installation (see
-- backend/internal/run/README.md on the retry site's nil-normalization).
-- Backfilling '0' would turn that sentinel into a NON-zero scope with ref "0",
-- silently converting a "no credentials" run into one that claims installation
-- 0 — so those rows stay NULL and keep taking the existing fallback.
--
-- PART 2 — runs_retry_child_once_idx.
--
-- The CI-failure retry paths (the pre-existing GitHub check_run path and the
-- GitLab pipeline path this issue unparks) both create a follow-up child run
-- keyed to (parent_run_id, retry_attempt). They deduplicated with a read
-- followed by a write and no atomicity between them, so two concurrent
-- webhook deliveries for the same failure could both observe "no child yet"
-- and both insert. This partial unique index makes that race impossible at the
-- DB level, mirroring the 0062/0067/0068 precedent: the loser gets SQLSTATE
-- 23505 with ConstraintName = 'runs_retry_child_once_idx', which
-- run.IsRetryChildDuplicate recognizes as the benign "someone else won" branch
-- while every other 23505 stays a hard error.
--
-- THE INDEX ONLY DEDUPS WHEN BOTH RACING INSERTS COMPUTE THE SAME
-- retry_attempt. That is a contract on the CALLERS, not something the index
-- can enforce: both deliveries MUST derive retry_attempt as
-- parent.RetryAttempt + 1, read from the PARENT row. Deriving it from the
-- latest existing CHILD makes the two inserts disagree (0+1 and 1+1), the
-- index never fires, and two children land — with a concurrency test that
-- still passes because exactly one child exists per attempt value. Any new
-- retry mint site must derive from the parent.
--
-- KEY SHAPE. (parent_run_id, retry_attempt) — not parent_run_id alone. One
-- parent legitimately carries a CHAIN of retry children at successive attempts
-- (attempt 1, then attempt 2 when max_retries allows), so keying on
-- parent_run_id alone would refuse the second, legitimate retry.
--
-- WHY PARTIAL. parent_run_id is also the #216 follow-up/lineage column, which
-- ordinary non-retry children carry with retry_attempt = 0. Those are
-- legitimately many-per-parent, so the predicate excludes them:
-- retry_attempt > 0 restricts the index to retry children exactly. NULL
-- parent_run_id (every root run) is excluded too — NULLs are distinct in a
-- PostgreSQL unique index anyway, but the explicit predicate keeps the index
-- small and its intent readable.
--
-- Fail-loud on a pre-existing duplicate: if any (parent, attempt) pair already
-- holds two rows, CREATE UNIQUE INDEX fails at migrate time rather than
-- silently de-duplicating run history. IF NOT EXISTS keeps re-application
-- idempotent.
ALTER TABLE runs
    ADD COLUMN installation_ref TEXT;

UPDATE runs
   SET installation_ref = installation_id::text
 WHERE installation_id IS NOT NULL
   AND installation_id <> 0;

CREATE UNIQUE INDEX IF NOT EXISTS runs_retry_child_once_idx
    ON runs (parent_run_id, retry_attempt)
 WHERE parent_run_id IS NOT NULL AND retry_attempt > 0;
