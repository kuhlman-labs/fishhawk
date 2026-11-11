package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// ADR-090 D8 Amendment 1 (#4368): an operator BASE MERGE is a base advance.
//
// D8 exempted every operator-vouched head from the merge-candidate pass, on
// the reading that a vouch attributes an operator's own remediation commit.
// Rules 1–2 of classifyMergeCandidateHead only recognise base advances the
// PRODUCT wrote, so an operator who merged the base into the run branch by
// hand and vouched the result (#4293: run e16e6c42, head 4ce8fa15) took the
// exemption, and the combined tree was never product-verified.
//
// As amended, the vouch exempts a head ONLY when a forge probe proves the head
// brings in no base commit absent from the run's newest VERIFIED head. The
// probe compares twice (GitHub three-dot compares, i.e. `git log base..head`
// anchored on the merge base):
//
//	sinceVerified = compare(<newest verified head V>...<head H>)
//	branchOnly    = compare(<PR base ref>...<head H>)
//
// A commit in sinceVerified and NOT in branchOnly is one H has, V lacks and the
// base already contains: a base commit H brought in → base_advance. None → the
// D8 operator-edit case, not_required.
//
// Fail posture:
//
//   - the deployment cannot probe (no GitHub client, no installation, an
//     unparseable repo, no PR number) → not_required with a WARN, keeping the
//     pre-amendment exemption exactly where ADR-090 D7 already leaves the merge
//     gate ungated;
//   - the probe is undecidable on a probeable run (no verified reference head
//     on the chain, an empty PR base ref, a compare at the un-paginated
//     250-commit cap) → base_advance (fail closed: nothing proves the head adds
//     no base content);
//   - a forge or audit read error → returned; every caller of
//     mergeCandidateVerifyState already fails closed on it (D7).
//
// Residual: a base change carried in by a NON-merge commit (a cherry-pick, a
// hand copy) has a fresh SHA the base does not contain, so it is not detected.
// A scope-bounded tree diff (issue option b) was not adopted, because it would
// strip D8's canonical case — an operator remediation commit outside the run's
// scope.

// vouchProbeCompareCap is the un-paginated GitHub compare response's commit
// limit: githubclient.CompareCommits documents that the compare API returns at
// most 250 commits and does not paginate. A list AT the cap may be truncated,
// so it cannot prove a commit's absence.
const vouchProbeCompareCap = 250

// vouchProbeAtCap reports whether a compare result may have been truncated.
func vouchProbeAtCap(n int) bool {
	return n >= vouchProbeCompareCap
}

// classifyVouchedHead is classifyMergeCandidateHead's rule 4 as amended (ADR-090
// D8 Amendment 1, #4368): the cause for an operator-vouched head, or "" when
// the vouch still exempts it. It performs NO writes. Its reference is
// newestVerifiedHead, which excludes only the probed head itself; ADR-142 D2
// leaves this direct live-head reference unchanged.
func (s *Server) classifyVouchedHead(ctx context.Context, runRow *run.Run, headSHA string) (string, error) {
	return s.vouchedHeadProbe(ctx, runRow, headSHA, func(ctx context.Context) (string, error) {
		return s.newestVerifiedHead(ctx, runRow.ID, headSHA)
	})
}

// mergeCandidateCannotProbeReason names why the deployment cannot run the
// vouch probe for runRow (ADR-090 D7's inability-to-probe residual), or ""
// when every input the probe needs is present.
func (s *Server) mergeCandidateCannotProbeReason(runRow *run.Run) string {
	if s.cfg.GitHub == nil {
		return "no GitHub client is wired"
	}
	if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		return "the run carries no GitHub installation"
	}
	if _, err := parseRepoOwnerName(runRow.Repo); err != nil {
		return "the run repo is unparseable"
	}
	if parsePRNumberFromURL(runRow.PullRequestURL) <= 0 {
		return "the run carries no parseable pull request number"
	}
	return ""
}

// vouchedHeadProbe is the Amendment 1 probe and determinability ladder for
// headSHA, with the verified reference supplied by the caller. The order is
// fixed: the cannot-probe check first, then the reference read, then the PR
// read and the two compares. Rule 4 supplies newestVerifiedHead; ADR-142 D2's
// scenario-terminal use supplies a reference that also excludes every
// scenario-reported head. It performs NO writes.
func (s *Server) vouchedHeadProbe(ctx context.Context, runRow *run.Run, headSHA string, reference func(context.Context) (string, error)) (string, error) {
	if why := s.mergeCandidateCannotProbeReason(runRow); why != "" {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge-candidate verify: cannot probe an operator-vouched head for base commits; keeping the vouch exemption (ADR-090 D7, D8 Amendment 1)",
			slog.String("run_id", runRow.ID.String()),
			slog.String("head_sha", headSHA),
			slog.String("why", why))
		return "", nil
	}
	// Both parses succeeded inside mergeCandidateCannotProbeReason.
	repo, _ := parseRepoOwnerName(runRow.Repo)
	prNumber := parsePRNumberFromURL(runRow.PullRequestURL)

	refHead, err := reference(ctx)
	if err != nil {
		return "", err
	}
	if refHead == "" {
		return mergeCandidateCauseBaseAdvance, nil
	}

	scope := forge.FromGitHubInstallationID(*runRow.InstallationID)
	pr, err := s.cfg.GitHub.GetPullRequest(ctx, scope, repo, prNumber)
	if err != nil {
		return "", fmt.Errorf("merge-candidate vouch probe: reading the pull request for head %s failed: %w", headSHA, err)
	}
	baseRef := strings.TrimSpace(pr.BaseRef)
	if baseRef == "" {
		return mergeCandidateCauseBaseAdvance, nil
	}

	sinceVerified, err := s.cfg.GitHub.CompareCommits(ctx, scope, repo, refHead, headSHA)
	if err != nil {
		return "", fmt.Errorf("merge-candidate vouch probe: comparing verified head %s with head %s failed: %w", refHead, headSHA, err)
	}
	if len(sinceVerified) == 0 {
		return "", nil
	}
	branchOnly, err := s.cfg.GitHub.CompareCommits(ctx, scope, repo, baseRef, headSHA)
	if err != nil {
		return "", fmt.Errorf("merge-candidate vouch probe: comparing base %s with head %s failed: %w", baseRef, headSHA, err)
	}
	if vouchProbeAtCap(len(sinceVerified)) || vouchProbeAtCap(len(branchOnly)) {
		return mergeCandidateCauseBaseAdvance, nil
	}
	if bringsInBaseCommits(sinceVerified, branchOnly) {
		return mergeCandidateCauseBaseAdvance, nil
	}
	return "", nil
}

// bringsInBaseCommits reports whether any commit in sinceVerified (H has, the
// verified head lacks) is absent from branchOnly (H has, the base lacks) — a
// commit the base already contains, which H brought in.
func bringsInBaseCommits(sinceVerified, branchOnly []string) bool {
	onBranch := make(map[string]bool, len(branchOnly))
	for _, sha := range branchOnly {
		onBranch[sha] = true
	}
	for _, sha := range sinceVerified {
		if !onBranch[sha] {
			return true
		}
	}
	return false
}

// newestVerifiedHead returns the newest (by chain sequence) head on the run's
// chain whose tree the product gated or verified, never excludeHead itself
// (newestVerifiedHeadExcept lists the sources).
//
// Excluding the probed head keeps a passed verdict for that head from becoming
// its own reference (H...H is empty, which would read not_required instead of
// passed). "" means nothing on the chain was ever verified. Every audit read
// error is returned.
func (s *Server) newestVerifiedHead(ctx context.Context, runID uuid.UUID, excludeHead string) (string, error) {
	return s.newestVerifiedHeadExcept(ctx, runID, func(sha string) bool { return sha == excludeHead })
}

// newestVerifiedHeadExcept returns the newest (by chain sequence) head on the
// run's chain whose tree the product gated or verified, skipping every head
// for which excluded reports true:
//
//   - the head_sha of a runner-gated report (mergeCandidateRunnerGatedCategories);
//   - the head_sha of a passed merge_candidate_verified row;
//   - ONLY in enforce mode, the to_head of a merge_candidate_verdict_transferred
//     row — matching newestMergeCandidateVerdict's rollback contract, so
//     switching enforce → off/shadow revokes a transfer here too.
//
// The exclusion is by SHA, independent of sequence or result. "" means no
// eligible head exists. Every audit read error is returned.
func (s *Server) newestVerifiedHeadExcept(ctx context.Context, runID uuid.UUID, excluded func(string) bool) (string, error) {
	var newest string
	var newestSeq int64
	consider := func(sha string, seq int64) {
		if sha == "" || excluded(sha) {
			return
		}
		if newest == "" || seq > newestSeq {
			newest, newestSeq = sha, seq
		}
	}

	for _, category := range mergeCandidateRunnerGatedCategories {
		entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, category)
		if err != nil {
			return "", err
		}
		for _, e := range entries {
			var p struct {
				HeadSHA string `json:"head_sha"`
			}
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			consider(p.HeadSHA, e.Sequence)
		}
	}

	verified, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryMergeCandidateVerified)
	if err != nil {
		return "", err
	}
	for _, e := range verified {
		var p mergeCandidateVerifiedPayload
		if json.Unmarshal(e.Payload, &p) != nil || p.Result != mergeCandidateResultPassed {
			continue
		}
		consider(p.HeadSHA, e.Sequence)
	}

	if s.mergeCandidateTransferMode() == MergeCandidateTransferEnforce {
		transfers, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryMergeCandidateVerdictTransferred)
		if err != nil {
			return "", err
		}
		for _, e := range transfers {
			var t mergeCandidateTransferPayload
			if json.Unmarshal(e.Payload, &t) != nil || t.FromHead == "" {
				continue
			}
			consider(t.ToHead, e.Sequence)
		}
	}
	return newest, nil
}
