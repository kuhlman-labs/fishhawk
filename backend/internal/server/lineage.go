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
	if runRow.InstallationID == nil {
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

	// Resolve the compare anchor defensively (ADR-035 binding condition):
	// the run's real PR base ref, never a hardcoded branch name and never
	// the laundering-prone reported base_sha. Unconfirmed → fail open.
	baseRef := s.resolveLineageBaseRef(ctx, runRow, *runRow.InstallationID, repo, prNumber)
	if baseRef == "" {
		return true
	}

	ledger := s.buildReportedHeadLedger(ctx, runID, headSHA)

	commits, err := s.cfg.GitHub.CompareCommits(ctx, *runRow.InstallationID, repo, baseRef, headSHA)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch lineage: compare commits failed; skipping check",
			slog.String("run_id", runID.String()),
			slog.String("base_ref", baseRef),
			slog.String("head_sha", headSHA),
			slog.String("error", err.Error()))
		return true
	}

	for _, sha := range commits {
		if _, member := ledger[sha]; !member {
			s.recordForeignCommitViolation(ctx, runID, stage, sha, headSHA)
			return false
		}
	}
	return true
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
	installationID int64, repo githubclient.RepoRef, prNumber int) string {
	if prNumber <= 0 {
		prNumber = parsePRNumberFromURL(runRow.PullRequestURL)
	}
	if prNumber <= 0 {
		// No PR to read the base ref from (e.g. a child-push boundary
		// before the parent opens the consolidated PR). Fail open.
		return ""
	}
	pr, err := s.cfg.GitHub.GetPullRequest(ctx, installationID, repo, prNumber)
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
// audit entries, plus the current report's headSHA (the first-report
// bootstrap — the PR-open report itself may not yet be in the chain
// when the guard runs). A read error on any category is WARNed and that
// category is skipped; the current headSHA is always a member, so the
// run's own just-pushed commit is never flagged.
func (s *Server) buildReportedHeadLedger(ctx context.Context, runID uuid.UUID, headSHA string) map[string]struct{} {
	ledger := map[string]struct{}{headSHA: {}}
	for _, cat := range lineageLedgerCategories {
		entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, cat)
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"branch lineage: list audit entries failed; ledger may be incomplete",
				slog.String("run_id", runID.String()),
				slog.String("category", cat),
				slog.String("error", err.Error()))
			continue
		}
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
	return ledger
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
	payload, _ := json.Marshal(map[string]any{
		"kind":          invariantmonitor.KindForeignCommitOnBranch,
		"run_id":        runID.String(),
		"stage_id":      stageID.String(),
		"offending_sha": offendingSHA,
		"head_sha":      headSHA,
	})
	systemKind := audit.ActorSystem
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  invariantmonitor.CategoryInvariantViolation,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch lineage: append invariant_violation audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
	}

	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
		"branch lineage: foreign commit on run branch",
		slog.String("kind", invariantmonitor.KindForeignCommitOnBranch),
		slog.String("run_id", runID.String()),
		slog.String("stage_id", stageID.String()),
		slog.String("offending_sha", offendingSHA))

	s.notifyStatusUpdate(ctx, runID, "lineage_violation")
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
