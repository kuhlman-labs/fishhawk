package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/invariantmonitor"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// lineageLedgerCategories are the audit categories whose entries carry
// a reported head_sha for THIS run's own commits: the PR-open report,
// the decomposed-child push report, and the fix-up push report. Their
// union (∪ the current report's head_sha) is the set of commits the
// run is allowed to have placed on its branch.
var lineageLedgerCategories = []string{"pull_request_opened", "child_pushed", "fixup_pushed"}

// lineageVouchLedgerCategory is the audit category carrying an operator's
// vouched-commit declaration (#1044). Unlike the own-chain head categories
// above (whose head_sha payload field names a commit the run itself
// pushed), each entry's vouched_sha payload field names a foreign commit an
// operator has DECLARED to be run-authored lineage — an operator's
// mechanical remediation commit on the run branch that no loop-native
// remediation could route. The ledger unions these alongside the reported
// heads, on the run's own chain AND its decomposition children, so a
// vouched commit attributes cleanly instead of wedging the run it fixed.
// An UN-vouched foreign commit still violates (fail-closed preserved).
const lineageVouchLedgerCategory = CategoryOperatorCommitVouched

// lineageIntegrationLedgerCategory is the audit category the orchestrator
// emits at fan-in (slices_integrated, #1142) once every decomposed slice
// merged cleanly onto the consolidated branch. Its integration_commit_shas
// payload field names the "Integrate slice N" merge commits the fan-in
// created on the consolidated branch (#1459). A decomposed parent's own
// chain carries this entry (RunID = parent); the ledger unions those SHAs in
// so a later report boundary (e.g. a fix-up on the consolidated parent)
// attributes the integration merges instead of flagging them foreign. A
// standalone run has no such entry, so the read is a no-op for it.
const lineageIntegrationLedgerCategory = "slices_integrated"

// lineageIntegrationCommitCategory is the incremental companion to
// slices_integrated (#1806). The orchestrator emits one integration_commit_recorded
// entry the instant each "Integrate slice N" merge commit is created, so the
// merge SHA is durable across a partial fan-in that bails on a later slice's
// conflict/error (never reaching the terminal slices_integrated) and across a
// re-entrant pass that sees the earlier merges as 204 no-ops. Its merge_sha
// payload field names a single integration merge commit; the ledger unions
// these alongside slices_integrated.integration_commit_shas so the ADR-035
// guard attributes the merges even when the clean-integration signal never
// fired. A standalone run has no such entry, so the read is a no-op for it.
const lineageIntegrationCommitCategory = "integration_commit_recorded"

// lineageChildLedgerCategories are the audit categories read from a
// decomposition CHILD run's chain when building the PARENT's ledger
// (#1038). Children push onto the shared parent branch (child_pushed)
// and fix up onto that same branch (the decomposed fixup branch is
// fishhawk/run-<shortID(decomposedFromRunID)> — see FixupBranch in
// prompt.go), so both head reports belong in the parent's ledger.
// pull_request_opened is deliberately absent: children never open PRs
// (ADR-032/#714).
var lineageChildLedgerCategories = []string{"child_pushed", "fixup_pushed"}

// lineageChildRunsLimit caps the decomposition-child enumeration when
// building the reported-head ledger. run.ListRunsFilter requires
// Limit > 0; this is set far above any realistic sub_plans fan-out
// (single digits) so truncation cannot silently shrink the ledger.
const lineageChildRunsLimit = 200

// verifyBranchLineage enforces ADR-035's detect-and-halt contract
// (#858): every commit on the run branch must be attributable to one
// of THIS run's own reported head SHAs. A non-attributable ("foreign")
// commit — the #797 shape, a foreign writer's commit silently riding
// the run branch into the PR diff — fails the stage category-B instead
// of being swept into the diff.
//
// The comparison anchor is the run's PR base ref, resolved from GitHub
// (independently trustworthy: a branch commit cannot corrupt what the
// PR targets). The runner-reported base_sha is deliberately NOT used —
// that is exactly the value a contaminated branch launders (#797
// reported the foreign commit itself as the base). GitHub's compare
// API computes the merge-base, so CompareCommits(baseRef, head)
// returns the run's own commits plus any foreign ones; each is checked
// for membership in the reported-head ledger.
//
// Fail-open by construction: a missing/unresolvable anchor, an absent
// GitHub client, or a CompareCommits error WARNs and returns true so
// the happy path never blocks on a transient GitHub failure. The only
// path that returns false is a confirmed foreign commit, and that path
// has already failed the stage + emitted the audit + notified. A
// contamination MISS is acceptable (the invariant monitor backstops
// it); a FALSE BLOCK of a clean run is not.
//
// prNumber is the report-supplied PR number (the pull_request_opened
// boundary has it directly); pass 0 at the push boundaries and the
// anchor is resolved from the run's tracked pull_request_url.
func (s *Server) verifyBranchLineage(ctx context.Context, runID uuid.UUID,
	stage *run.Stage, headSHA string, prNumber int) (ok bool) {
	if s.cfg.GitHub == nil || s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		// Not wired (dev / CLI posture) — nothing to anchor on. Fail open.
		return true
	}

	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch lineage: load run failed; skipping check",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return true
	}
	if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		// No installation to call GitHub with. Fail open.
		return true
	}
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch lineage: unparseable repo; skipping check",
			slog.String("run_id", runID.String()),
			slog.String("repo", runRow.Repo),
			slog.String("error", err.Error()))
		return true
	}

	// The #858 report-boundary check seeds the ledger with the current
	// reported head (first-report bootstrap) and compares the same head.
	// On a confirmed foreign commit, fail the stage category-B.
	offendingSHA, checked := s.detectForeignCommitOnBranch(ctx, runRow,
		forge.FromGitHubInstallationID(*runRow.InstallationID), repo, headSHA, headSHA, prNumber)
	if checked && offendingSHA != "" {
		s.recordForeignCommitViolation(ctx, runID, stage, offendingSHA, headSHA)
		return false
	}
	return true
}

// detectForeignCommitOnBranch is the side-effect-free detection core
// shared by the #858 report-boundary check (verifyBranchLineage) and the
// out-of-band merge-resolution re-check (ReverifyBranchLineage). It
// resolves the run's PR base ref, builds the reported-head ledger seeded
// from ledgerSeedSHA, compares baseRef...compareHead, and returns the
// first commit not attributable to the ledger.
//
// It performs NO writes (no FailStage, no audit, no notify) — purely
// detection. Returns (sha, true) on the first foreign commit; ("", true)
// when every commit is attributable; ("", false) on EVERY fail-open path
// (unresolvable anchor, incomplete ledger, CompareCommits error) so a
// transient GitHub failure never produces a false verdict.
//
// The ledger is decomposition-aware (#1038): for a decomposition parent
// it includes the heads reported on each child run's chain, so sibling
// child commits on the shared fan-out branch attribute cleanly instead
// of false-flagging (and wedging the parent at merge resolution). A
// commit with no child provenance still violates.
//
// ledgerSeedSHA is the critical out-of-band knob (see
// buildReportedHeadLedger): the report-boundary caller seeds with the
// current head; the merge-resolution caller seeds with "" so the live
// branch tip is not auto-whitelisted into the set it is checked against.
func (s *Server) detectForeignCommitOnBranch(ctx context.Context, runRow *run.Run,
	scope forge.CredentialScope, repo githubclient.RepoRef, compareHead, ledgerSeedSHA string,
	prNumber int) (offendingSHA string, checked bool) {
	// Resolve the compare anchor defensively (ADR-035 binding condition):
	// the run's real PR base ref, never a hardcoded branch name and never
	// the laundering-prone reported base_sha. Unconfirmed → fail open.
	baseRef := s.resolveLineageBaseRef(ctx, runRow, scope, repo, prNumber)
	if baseRef == "" {
		return "", false
	}

	ledger, complete := s.buildReportedHeadLedger(ctx, runRow, ledgerSeedSHA)
	if !complete {
		// Could not build the COMPLETE set of this run's legitimate head
		// SHAs (an audit read failed). Enforcing against a partial ledger
		// would false-flag a legitimate prior-push commit as foreign on a
		// multi-push run (e.g. after a fixup_pushed: the original PR-open
		// head + the fix-up head) — exactly the false BLOCK this guard must
		// never produce. If we cannot enumerate what is legitimate, we
		// cannot safely call anything foreign: fail open (defer), as on a
		// CompareCommits error or a missing anchor. A contamination MISS is
		// acceptable (the invariant monitor backstops it); a false block is
		// not.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch lineage: reported-head ledger incomplete (audit read failed); skipping check",
			slog.String("run_id", runRow.ID.String()),
			slog.String("head_sha", compareHead))
		return "", false
	}

	commits, err := s.cfg.GitHub.CompareCommitsScoped(ctx, scope, repo, baseRef, compareHead)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch lineage: compare commits failed; skipping check",
			slog.String("run_id", runRow.ID.String()),
			slog.String("base_ref", baseRef),
			slog.String("head_sha", compareHead),
			slog.String("error", err.Error()))
		return "", false
	}

	for _, sha := range commits {
		if _, member := ledger[sha]; !member {
			return sha, true
		}
	}
	return "", true
}

// resolveLastRunAuthoredHead is the side-effect-free classifier behind
// the ADR-035 reset remediation (#867). It resolves the run's PR base
// ref, builds the reported-head ledger seeded "" (so the foreign tip is
// NOT self-whitelisted), runs CompareCommits(baseRef, headSHA) for the
// ordered (merge-base, head] commit list, and walks it to find:
//
//   - lastAuthoredSHA: the NEWEST commit that is a ledger member — the
//     last run-authored HEAD, the SHA a reset rewinds the branch to;
//   - offendingSHA: the FIRST (lowest) foreign (non-ledger) commit;
//   - isOnTop: true iff EVERY foreign commit sits strictly ABOVE the
//     newest ledger member (foreign sits on top). False on the
//     ancestor/interleaved shape (a foreign commit at-or-below a ledger
//     member), which a reset cannot drop — prevention (#861/#865) owns
//     that, so the handler refuses reset_out_of_scope.
//
// FAIL-CLOSED for the destructive action: unlike detectForeignCommitOnBranch
// (which fails OPEN so a transient GitHub failure never blocks a clean
// run), this returns ok=false on EVERY uncertainty — unresolvable base
// ref, incomplete ledger, CompareCommits error, or no identifiable
// run-authored HEAD — so an uncertain classification can never drive a
// force-update. The handler maps ok=false to reset_not_determinable.
func (s *Server) resolveLastRunAuthoredHead(ctx context.Context, runRow *run.Run,
	scope forge.CredentialScope, repo githubclient.RepoRef, headSHA string, prNumber int) (lastAuthoredSHA, offendingSHA string, isOnTop, ok bool) {
	baseRef := s.resolveLineageBaseRef(ctx, runRow, scope, repo, prNumber)
	if baseRef == "" {
		return "", "", false, false
	}

	ledger, complete := s.buildReportedHeadLedger(ctx, runRow, "")
	if !complete {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch reset: reported-head ledger incomplete; refusing (fail-closed)",
			slog.String("run_id", runRow.ID.String()),
			slog.String("head_sha", headSHA))
		return "", "", false, false
	}

	commits, err := s.cfg.GitHub.CompareCommitsScoped(ctx, scope, repo, baseRef, headSHA)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch reset: compare commits failed; refusing (fail-closed)",
			slog.String("run_id", runRow.ID.String()),
			slog.String("base_ref", baseRef),
			slog.String("head_sha", headSHA),
			slog.String("error", err.Error()))
		return "", "", false, false
	}

	// Commits are ordered (merge-base, head] oldest→newest. lastMemberIdx
	// is the newest ledger member; firstForeignIdx is the lowest foreign.
	lastMemberIdx, firstForeignIdx := -1, -1
	for i, sha := range commits {
		if _, member := ledger[sha]; member {
			lastMemberIdx = i
		} else if firstForeignIdx == -1 {
			firstForeignIdx = i
		}
	}

	if lastMemberIdx == -1 {
		// No commit on the branch is attributable to this run — there is
		// no run-authored HEAD to rewind to. Fail closed.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch reset: no run-authored commit on branch; refusing (fail-closed)",
			slog.String("run_id", runRow.ID.String()),
			slog.String("head_sha", headSHA))
		return "", "", false, false
	}

	lastAuthoredSHA = commits[lastMemberIdx]
	if firstForeignIdx >= 0 {
		offendingSHA = commits[firstForeignIdx]
	}
	// firstForeignIdx is the LOWEST foreign index, so firstForeign >
	// lastMember ⟺ every foreign commit is strictly above the newest
	// ledger member. No foreign at all (firstForeignIdx == -1) is "on
	// top" trivially (the handler then sees lastAuthoredSHA == headSHA
	// and returns reset_not_applicable).
	isOnTop = firstForeignIdx == -1 || firstForeignIdx > lastMemberIdx
	return lastAuthoredSHA, offendingSHA, isOnTop, true
}

// ReverifyBranchLineage is the detect-only out-of-band re-check that runs
// at MERGE RESOLUTION (ADR-035 second line of defense, #862, beyond
// #858's report boundary). It re-verifies that the run branch's live tip
// carries no foreign commit before the merge reconciler marks the run
// succeeded.
//
// It mirrors verifyBranchLineage's prelude (same nil guards / fail-open
// posture), resolves the PR's live head SHA, and runs the shared detection
// core with ledgerSeedSHA="" so the current tip is NOT auto-whitelisted.
// On a confirmed foreign commit it emits the shared foreign_commit_on_branch
// invariant audit + a lineage_violation notify and returns clean=false —
// but does NOT FailStage (there is no producing stage at merge time;
// remediation is #867). Every fail-open path returns clean=true so a clean
// run is never wrongly refused.
//
// The emit is idempotent: a contaminated merged run is left PARKED at the
// review gate (the reconciler skips the resolve, not fails it), so the
// merge reconciler re-polls it every tick and calls this method again. To
// avoid audit/notify spam, a foreign_commit_on_branch entry already
// recorded for this run with the SAME offending+head SHA suppresses the
// re-emit and re-notify while still returning clean=false. A genuinely new
// (different) foreign commit still emits.
//
// Honest limitation: this observes a merge GitHub has ALREADY performed,
// so it refuses to mark the run succeeded and flags loudly rather than
// physically blocking the GitHub-side merge. The pre-merge open-PR window
// is covered by the periodic sweep (#868).
func (s *Server) ReverifyBranchLineage(ctx context.Context, runID uuid.UUID, prNumber int) (clean bool) {
	if s.cfg.GitHub == nil || s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return true
	}

	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch lineage reverify: load run failed; skipping check",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return true
	}
	if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		return true
	}
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch lineage reverify: unparseable repo; skipping check",
			slog.String("run_id", runID.String()),
			slog.String("repo", runRow.Repo),
			slog.String("error", err.Error()))
		return true
	}

	if prNumber <= 0 {
		prNumber = parsePRNumberFromURL(runRow.PullRequestURL)
	}
	if prNumber <= 0 {
		return true
	}
	pr, err := s.cfg.GitHub.GetPullRequestScoped(ctx, forge.FromGitHubInstallationID(*runRow.InstallationID), repo, prNumber)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch lineage reverify: resolve PR head failed; skipping check",
			slog.String("run_id", runID.String()),
			slog.Int("pr_number", prNumber),
			slog.String("error", err.Error()))
		return true
	}
	headSHA := pr.HeadSHA
	if headSHA == "" {
		return true
	}

	offendingSHA, checked := s.detectForeignCommitOnBranch(ctx, runRow,
		forge.FromGitHubInstallationID(*runRow.InstallationID), repo, headSHA, "", prNumber)
	if !checked || offendingSHA == "" {
		// Fail open or clean — either way the merge may resolve.
		return true
	}

	// Detect-only on a hit: emit the shared invariant + notify, but leave
	// the run parked (the reconciler refuses the resolve on clean=false).
	// Suppress a duplicate emit/notify when this exact contamination is
	// already on record, so the per-tick re-poll doesn't spam.
	if s.foreignCommitAlreadyRecorded(ctx, runID, offendingSHA, headSHA) {
		return false
	}
	s.emitForeignCommitInvariant(ctx, runID, nil, offendingSHA, headSHA)
	s.notifyStatusUpdate(ctx, runID, "lineage_violation")
	return false
}

// foreignCommitAlreadyRecorded reports whether a foreign_commit_on_branch
// invariant entry with this exact offending+head SHA pair is already on
// the run's chain — the idempotency guard for the re-polled merge-resolution
// re-check. It fails open (returns false → proceed with emit) on a read
// error: a duplicate emit is preferable to suppressing a genuine new
// violation.
func (s *Server) foreignCommitAlreadyRecorded(ctx context.Context, runID uuid.UUID,
	offendingSHA, headSHA string) bool {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, invariantmonitor.CategoryInvariantViolation)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch lineage reverify: dedup read failed; proceeding with emit",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return false
	}
	for _, e := range entries {
		var payload struct {
			Kind         string `json:"kind"`
			OffendingSHA string `json:"offending_sha"`
			HeadSHA      string `json:"head_sha"`
		}
		if json.Unmarshal(e.Payload, &payload) != nil {
			continue
		}
		if payload.Kind == invariantmonitor.KindForeignCommitOnBranch &&
			payload.OffendingSHA == offendingSHA && payload.HeadSHA == headSHA {
			return true
		}
	}
	return false
}

// resolveLineageBaseRef resolves the run's PR base ref to anchor the
// lineage comparison on. Preference order (ADR-035 binding condition):
//  1. the run's actual PR base ref via GetPullRequest — independently
//     trustworthy, eliminates the false-positive class entirely.
//  2. fall back to nothing: an unconfirmed anchor returns "" so the
//     caller fails open (never a false block on a guessed anchor).
//
// prNumber is the report-supplied number when available (0 = unknown);
// otherwise the number is parsed from the run's tracked
// pull_request_url.
func (s *Server) resolveLineageBaseRef(ctx context.Context, runRow *run.Run,
	scope forge.CredentialScope, repo githubclient.RepoRef, prNumber int) string {
	if prNumber <= 0 {
		prNumber = parsePRNumberFromURL(runRow.PullRequestURL)
	}
	if prNumber <= 0 {
		// No PR to read the base ref from (e.g. a child-push boundary
		// before the parent opens the consolidated PR). Fail open.
		return ""
	}
	pr, err := s.cfg.GitHub.GetPullRequestScoped(ctx, scope, repo, prNumber)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch lineage: resolve PR base ref failed; skipping check",
			slog.String("run_id", runRow.ID.String()),
			slog.Int("pr_number", prNumber),
			slog.String("error", err.Error()))
		return ""
	}
	if pr.BaseRef == "" {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch lineage: PR returned empty base ref; skipping check",
			slog.String("run_id", runRow.ID.String()),
			slog.Int("pr_number", prNumber))
		return ""
	}
	return pr.BaseRef
}

// buildReportedHeadLedger collects the set of head SHAs this run has
// reported across its pull_request_opened / child_pushed / fixup_pushed
// audit entries, plus the commits an operator has VOUCHED as run-authored
// lineage (operator_commit_vouched, #1044 — read from the vouched_sha
// field, not head_sha), plus an explicit ledgerSeedSHA bootstrap (when
// non-empty).
//
// The seed is the CRITICAL out-of-band subtlety. The #858 report-boundary
// caller passes the current reported head (the first-report bootstrap —
// the PR-open report itself may not yet be in the chain when the guard
// runs) so a legitimate not-yet-audited PR-open head isn't false-flagged.
// The out-of-band merge-resolution caller passes "" so the current branch
// tip is NOT auto-whitelisted into the ledger it is being checked against;
// otherwise a foreign tip would whitelist itself and defeat the check.
//
// Decomposition-awareness (#1038): a decomposition fan-out shares ONE
// branch, but each child's child_pushed/fixup_pushed entries land on the
// CHILD's audit chain (succeedChildPushStage appends with the reporting
// child's run ID), not the parent's. The parent's own chain therefore
// only ever sees the consolidated-PR head (the branch tip), so a
// parent-side check built from the own chain alone false-flags every
// earlier sibling commit as foreign — the wedged-parent shape: the merge
// reconciler's re-check refuses to terminalize a cleanly merged fan-out
// forever. To attribute sibling commits correctly, the ledger also unions
// in the heads reported by this run's decomposition children (runs with
// decomposed_from = this run ID), read from each child's chain. Commits
// WITHOUT that provenance still violate.
//
// It returns complete=false if a read error on ANY ledger category, the
// child enumeration, or ANY per-child chain read prevented building the
// full set. The caller MUST fail open on an incomplete ledger rather than
// enforce membership against it: a partial ledger missing a legitimate
// prior-push or sibling-child head would false-flag that commit as
// foreign. complete=true means every read succeeded and the ledger is
// authoritative.
func (s *Server) buildReportedHeadLedger(ctx context.Context, runRow *run.Run, ledgerSeedSHA string) (ledger map[string]struct{}, complete bool) {
	ledger = map[string]struct{}{}
	if ledgerSeedSHA != "" {
		ledger[ledgerSeedSHA] = struct{}{}
	}
	complete = true
	for _, cat := range lineageLedgerCategories {
		entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runRow.ID, cat)
		if err != nil {
			complete = false
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"branch lineage: list audit entries failed; ledger incomplete (guard fails open)",
				slog.String("run_id", runRow.ID.String()),
				slog.String("category", cat),
				slog.String("error", err.Error()))
			continue
		}
		addReportedHeads(ledger, entries)
	}

	// Union in the commits an operator has VOUCHED on this run's own chain
	// (#1044): an operator's mechanical remediation commit declared
	// run-authored lineage. Unlike the head categories these carry the
	// commit in the vouched_sha payload field, so they read via
	// addVouchedSHAs. A read error sets complete=false (fail open), matching
	// the head-category contract above.
	if vouched, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runRow.ID, lineageVouchLedgerCategory); err != nil {
		complete = false
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch lineage: list vouched commits failed; ledger incomplete (guard fails open)",
			slog.String("run_id", runRow.ID.String()),
			slog.String("category", lineageVouchLedgerCategory),
			slog.String("error", err.Error()))
	} else {
		addVouchedSHAs(ledger, vouched)
	}

	// Union in the integration merge commits the fan-in recorded on THIS
	// run's own chain (#1459): a decomposed parent's slices_integrated entry
	// carries the "Integrate slice N" merge SHAs in integration_commit_shas.
	// Without these, a later boundary (a fix-up on the consolidated parent)
	// reads those merges as foreign and wedges the run category-B. A
	// standalone run has no slices_integrated entry, so this is a no-op and
	// non-parent behavior is unchanged. A read error sets complete=false
	// (fail open), matching the head/vouch-category contract above.
	if integrated, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runRow.ID, lineageIntegrationLedgerCategory); err != nil {
		complete = false
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch lineage: list slices_integrated failed; ledger incomplete (guard fails open)",
			slog.String("run_id", runRow.ID.String()),
			slog.String("category", lineageIntegrationLedgerCategory),
			slog.String("error", err.Error()))
	} else {
		addIntegrationCommitSHAs(ledger, integrated)
	}

	// Union in the integration merges recorded INCREMENTALLY on THIS run's own
	// chain (#1806): the orchestrator emits one integration_commit_recorded
	// entry per successful merge at merge time, so the SHA survives a partial
	// fan-in that bailed before the terminal slices_integrated fired (later
	// slice conflict/error) and a re-entrant 204 no-op pass. Read parent-chain
	// only, exactly like slices_integrated. A standalone run has no such entry,
	// so this is a no-op. A read error sets complete=false (fail open),
	// matching the head/vouch/integration-category contract above.
	if recorded, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runRow.ID, lineageIntegrationCommitCategory); err != nil {
		complete = false
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch lineage: list integration_commit_recorded failed; ledger incomplete (guard fails open)",
			slog.String("run_id", runRow.ID.String()),
			slog.String("category", lineageIntegrationCommitCategory),
			slog.String("error", err.Error()))
	} else {
		addIntegrationMergeSHAs(ledger, recorded)
	}

	// Union in the heads reported by this run's decomposition children.
	// A standalone run (and every child run itself) has no children, so
	// this is a no-op and standalone behavior is unchanged.
	children, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
		DecomposedFrom: &runRow.ID,
		Limit:          lineageChildRunsLimit,
	})
	if err != nil {
		complete = false
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch lineage: list decomposition children failed; ledger incomplete (guard fails open)",
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()))
		return ledger, complete
	}
	for _, child := range children {
		for _, cat := range lineageChildLedgerCategories {
			entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, child.ID, cat)
			if err != nil {
				complete = false
				s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
					"branch lineage: list child audit entries failed; ledger incomplete (guard fails open)",
					slog.String("run_id", runRow.ID.String()),
					slog.String("child_run_id", child.ID.String()),
					slog.String("category", cat),
					slog.String("error", err.Error()))
				continue
			}
			addReportedHeads(ledger, entries)
		}
		// A per-child vouch (an operator vouching a foreign commit against a
		// child run rather than the parent) unions into the parent's ledger
		// too. Same fail-open-on-read-error contract.
		if vouched, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, child.ID, lineageVouchLedgerCategory); err != nil {
			complete = false
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"branch lineage: list child vouched commits failed; ledger incomplete (guard fails open)",
				slog.String("run_id", runRow.ID.String()),
				slog.String("child_run_id", child.ID.String()),
				slog.String("category", lineageVouchLedgerCategory),
				slog.String("error", err.Error()))
			continue
		} else {
			addVouchedSHAs(ledger, vouched)
		}
	}
	return ledger, complete
}

// addVouchedSHAs adds every non-empty vouched_sha payload field from the
// given operator_commit_vouched audit entries to the ledger. Parallel to
// addReportedHeads, but reads the vouched_sha field an operator's vouch
// declaration carries instead of head_sha.
func addVouchedSHAs(ledger map[string]struct{}, entries []*audit.Entry) {
	for _, e := range entries {
		var payload struct {
			VouchedSHA string `json:"vouched_sha"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			continue
		}
		if payload.VouchedSHA != "" {
			ledger[payload.VouchedSHA] = struct{}{}
		}
	}
}

// addIntegrationCommitSHAs adds every non-empty SHA from the
// integration_commit_shas payload field of the given slices_integrated audit
// entries to the ledger (#1459). Parallel to addReportedHeads/addVouchedSHAs,
// but reads the []string the fan-in records for the "Integrate slice N" merge
// commits it created on the consolidated branch.
func addIntegrationCommitSHAs(ledger map[string]struct{}, entries []*audit.Entry) {
	for _, e := range entries {
		var payload struct {
			IntegrationCommitSHAs []string `json:"integration_commit_shas"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			continue
		}
		for _, sha := range payload.IntegrationCommitSHAs {
			if sha != "" {
				ledger[sha] = struct{}{}
			}
		}
	}
}

// addIntegrationMergeSHAs adds every non-empty merge_sha payload field from the
// given integration_commit_recorded audit entries to the ledger (#1806).
// Parallel to addIntegrationCommitSHAs, but reads the single merge_sha the
// fan-in records incrementally per merge (one entry per "Integrate slice N"
// commit) rather than the terminal []string batch.
func addIntegrationMergeSHAs(ledger map[string]struct{}, entries []*audit.Entry) {
	for _, e := range entries {
		var payload struct {
			MergeSHA string `json:"merge_sha"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			continue
		}
		if payload.MergeSHA != "" {
			ledger[payload.MergeSHA] = struct{}{}
		}
	}
}

// addReportedHeads adds every non-empty head_sha payload field from the
// given audit entries to the ledger.
func addReportedHeads(ledger map[string]struct{}, entries []*audit.Entry) {
	for _, e := range entries {
		var payload struct {
			HeadSHA string `json:"head_sha"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			continue
		}
		if payload.HeadSHA != "" {
			ledger[payload.HeadSHA] = struct{}{}
		}
	}
}

// recordForeignCommitViolation fails the stage category-B, writes the
// invariant_violation audit entry naming the offending commit, and
// fires the sticky status comment. The offending commit is on the run
// branch but is not in the run's reported-head ledger — a contract
// violation under ADR-035. Each step is best-effort/WARN-logged so a
// downstream write failure doesn't mask the primary verdict.
func (s *Server) recordForeignCommitViolation(ctx context.Context, runID uuid.UUID,
	stage *run.Stage, offendingSHA, headSHA string) {
	reason := "run branch carries a foreign commit " + offendingSHA +
		" not attributable to any of this run's reported head SHAs (ADR-035 lineage violation)"
	if _, err := run.FailStage(ctx, s.cfg.RunRepo, stage.ID, run.FailureB, reason); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch lineage: fail stage failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()))
	}

	stageID := stage.ID
	s.emitForeignCommitInvariant(ctx, runID, &stageID, offendingSHA, headSHA)
	s.notifyStatusUpdate(ctx, runID, "lineage_violation")
}

// emitForeignCommitInvariant is the single shared writer for the
// foreign_commit_on_branch invariant audit entry. Both the #858
// stage-failing path (recordForeignCommitViolation, stageID non-nil) and
// the detect-only merge-resolution path (ReverifyBranchLineage,
// stageID=nil — no producing stage at merge time) route through here, so
// the {kind, run_id, stage_id, offending_sha, head_sha} attribution is
// defined ONCE, not duplicated. A nil stageID is tolerated: the payload
// stage_id is emptied and the audit row's StageID is nil. Best-effort:
// an append failure WARNs and is swallowed so it can't mask the verdict.
func (s *Server) emitForeignCommitInvariant(ctx context.Context, runID uuid.UUID,
	stageID *uuid.UUID, offendingSHA, headSHA string) {
	stageIDStr := ""
	if stageID != nil {
		stageIDStr = stageID.String()
	}
	payload, _ := json.Marshal(map[string]any{
		"kind":          invariantmonitor.KindForeignCommitOnBranch,
		"run_id":        runID.String(),
		"stage_id":      stageIDStr,
		"offending_sha": offendingSHA,
		"head_sha":      headSHA,
	})
	systemKind := audit.ActorSystem
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   stageID,
		Timestamp: time.Now().UTC(),
		Category:  invariantmonitor.CategoryInvariantViolation,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch lineage: append invariant_violation audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageIDStr),
			slog.String("error", err.Error()))
	}

	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
		"branch lineage: foreign commit on run branch",
		slog.String("kind", invariantmonitor.KindForeignCommitOnBranch),
		slog.String("run_id", runID.String()),
		slog.String("stage_id", stageIDStr),
		slog.String("offending_sha", offendingSHA))
}

// latestRunHeadSHA resolves the run's newest recorded head SHA (#1682): the
// canonical "current head" the acceptance verdict binds to (acceptance.go),
// the head Option C's retry compares against (retry.go), and the head the
// audit-check publisher targets — all resolved through the ONE shared ordering
// in auditcomplete.LatestReportedHeadSHA (fixup_pushed > child_pushed >
// pull_request_opened, each by highest audit sequence). Sharing that resolver
// with auditcheckpublisher.findHeadSHA is the load-bearing guarantee that the
// acceptance/retry path and audit_complete publishing never resolve divergent
// heads for the same audit history.
//
// Returns ("", false, nil) when the run has recorded no head yet. A read error
// on any head-report category is returned so the caller can fail closed (the
// retry admit path treats an unresolvable head as "keep the 422", never as a
// spurious admit).
func (s *Server) latestRunHeadSHA(ctx context.Context, runID uuid.UUID) (string, bool, error) {
	if s.cfg.AuditRepo == nil {
		return "", false, nil
	}
	var entries []*audit.Entry
	for _, cat := range auditcomplete.HeadReportCategoriesByPrecedence {
		es, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, cat)
		if err != nil {
			return "", false, err
		}
		entries = append(entries, es...)
	}
	sha, ok := auditcomplete.LatestReportedHeadSHA(entries)
	return sha, ok, nil
}

// parsePRNumberFromURL extracts the integer PR number from a GitHub PR
// URL of the form https://github.com/{owner}/{repo}/pull/{n}. Returns 0
// when the URL is nil/empty or doesn't carry a parseable trailing
// number, so callers treat it as "unknown" and fail open.
func parsePRNumberFromURL(url *string) int {
	if url == nil || *url == "" {
		return 0
	}
	idx := strings.LastIndex(*url, "/pull/")
	if idx < 0 {
		return 0
	}
	tail := (*url)[idx+len("/pull/"):]
	// Trim any trailing path/query (e.g. "/files", "#discussion").
	if cut := strings.IndexAny(tail, "/?#"); cut >= 0 {
		tail = tail[:cut]
	}
	n, err := strconv.Atoi(tail)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
