package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Partial-delivery merge-time remaining-scope comment (E83.52 / #4085).
//
// A plan that declares `delivery: partial` ships only a slice of its
// triggering issue, so its PR references the issue with `Refs #N` (the prompt
// instruction, the held-commit resume text, the consolidated PR body, and the
// ship-time guard in partial_delivery_pr.go) and the issue is not closed by the
// merge. What the merge leaves behind is the plan's remaining_scope: the work
// the issue still needs. postPartialDeliveryRemainingScope posts it on the
// issue once the PR merges, so the open issue says what is left rather than
// reading as if nothing landed.
//
// The comment is worded to stay TRUE even when the issue was closed anyway —
// it says the remaining scope "was not delivered by this PR", never that the
// issue stays open. Two named residuals can close it regardless:
//
//   - A later fix-up push, or an operator's hand edit of the PR body, that
//     reintroduces `Closes #N` is not re-checked before merge (the ship-time
//     guard runs once, on the implement ship).
//   - On GitLab this comment posts (the issue is resolved forge-neutrally),
//     while the ship-time neutralizer is GitHub-only, so nothing rewrote a
//     closing reference the agent wrote into the merge request description.

// categoryPartialDeliveryRemainingScopePosted is the audit category appended
// after the remaining-scope comment posts. It is also the comment's dedup
// record. Registered in audit.KnownCategories and admitted to
// issuecomment.activityCategories.
const categoryPartialDeliveryRemainingScopePosted = "partial_delivery_remaining_scope_posted"

// partialDeliveryMarkerPrefix opens the hidden marker stamped on the comment,
// `<!-- fishhawk:partial-delivery run=<id> -->`. The remaining scope is passed
// through neutralizeCommsProse, which rewrites `<` and `>`, so planner text can
// never forge one.
const partialDeliveryMarkerPrefix = "<!-- fishhawk:partial-delivery run="

// postPartialDeliveryRemainingScope posts the approved plan's remaining_scope
// on the run's triggering issue after its PR merged. resolveReviewStageOnMerge
// calls it on BOTH merged arms (implement-only and review stage), after
// writePostMergeObservedAudit and before the economics stamp and branch sweep;
// never on the closed-without-merge arm.
//
// Each precondition returns WITHOUT posting: the run carries no triggering
// issue (IssueContext.Number > 0, checked before the plan is loaded); the
// approved plan is not partial (or does not load — partialDeliveryPlan treats
// that as full); a prior partial_delivery_remaining_scope_posted row exists (the
// dedup); the dedup read fails (WARN — a failed read never risks a duplicate);
// or the forge cannot be resolved (family from InstallationRef, repo via
// splitParentRepoRef, ops via issueOpsFor — each unresolved piece WARNs; the
// scope comes from mergeObservationScope, and a zero one is refused by the
// forge client as a failed post).
//
// The audit row is appended ONLY after PostIssueComment succeeds, so a failed
// post leaves no dedup record and a redelivery retries it. Named duplicate
// residuals (cosmetic, mirroring split_parent_close): two genuinely concurrent
// merge deliveries can both pass the dedup read, and an append failure after a
// successful post leaves no record for the next delivery to find. Best-effort
// throughout: nothing here unwinds the merge resolution.
func (s *Server) postPartialDeliveryRemainingScope(ctx context.Context, target *run.Run, meta reviewMergeMeta) {
	if target == nil || target.IssueContext == nil || target.IssueContext.Number <= 0 {
		return
	}
	issueNumber := target.IssueContext.Number
	p, partial := s.partialDeliveryPlan(ctx, target.ID)
	if !partial {
		return
	}
	warn := func(msg string, attrs ...slog.Attr) {
		attrs = append([]slog.Attr{
			slog.String("run_id", target.ID.String()),
			slog.Int("issue_number", issueNumber),
		}, attrs...)
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "partial delivery merge comment: "+msg, attrs...)
	}

	prior, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, target.ID, categoryPartialDeliveryRemainingScopePosted)
	if err != nil {
		warn("dedup read failed; remaining scope not posted", slog.String("error", err.Error()))
		return
	}
	if len(prior) > 0 {
		return
	}

	ref := ""
	if target.InstallationRef != nil {
		ref = *target.InstallationRef
	}
	family := splitParentForgeFamilyFromRef(ref)
	// A run with neither an InstallationRef nor an installation id yields the
	// ZERO scope. That is always the github family (an empty ref), whose client
	// refuses a zero scope before any request, so it lands on the post-failure
	// WARN below with no audit row — no separate guard is needed.
	scope := mergeObservationScope(target)
	repo, ok := splitParentRepoRef(target.Repo)
	if !ok {
		warn("unparseable repo; remaining scope not posted", slog.String("repo", target.Repo))
		return
	}
	ops := s.issueOpsFor(family)
	if ops == nil {
		warn("no issue operations for forge; remaining scope not posted", slog.String("forge", family))
		return
	}

	prURL := meta.prURL
	if prURL == "" && target.PullRequestURL != nil {
		prURL = *target.PullRequestURL
	}
	body := renderPartialDeliveryRemainingScopeComment(target.ID.String(), prURL, p.RemainingScope)
	if err := ops.PostIssueComment(ctx, scope, repo, issueNumber, body); err != nil {
		warn("post issue comment failed; a redelivery retries", slog.String("error", err.Error()))
		return
	}

	payload, _ := json.Marshal(map[string]any{
		"issue_number": issueNumber,
		"pr_url":       prURL,
		"forge":        family,
	})
	systemKind := audit.ActorSystem
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     target.ID,
		Timestamp: time.Now().UTC(),
		Category:  categoryPartialDeliveryRemainingScopePosted,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		warn("append audit entry failed after the comment posted; a redelivery may post a duplicate",
			slog.String("error", err.Error()))
		return
	}
	s.notifyOperatorVisible(ctx, target.ID, categoryPartialDeliveryRemainingScopePosted)
}

// renderPartialDeliveryRemainingScopeComment builds the merge-time comment: the
// hidden marker, a bold heading, the run and merged PR, the remaining scope as a
// blockquote, and what to do next. The remaining scope is planner-authored text
// posted under the bot identity, so it is passed through neutralizeCommsProse
// (no mentions, links, raw HTML, issue autolinks or forged markers). The wording
// never claims the issue stays open (see the file comment's residuals).
func renderPartialDeliveryRemainingScopeComment(runID, prURL, remainingScope string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s -->\n", partialDeliveryMarkerPrefix, runID)
	b.WriteString("**Partial delivery merged.**\n\n")
	merged := "its pull request"
	if prURL != "" {
		merged = prURL
	}
	fmt.Fprintf(&b, "Fishhawk run `%s` merged %s, which its approved plan declared a "+
		"partial delivery of this issue. The remaining scope below was not delivered by this PR:\n\n", runID, merged)
	scope := strings.TrimSpace(remainingScope)
	if scope == "" {
		b.WriteString("> _The plan did not state the remaining scope._\n")
	} else {
		for _, line := range strings.Split(neutralizeCommsProse(scope), "\n") {
			b.WriteString(strings.TrimRight("> "+line, " "))
			b.WriteByte('\n')
		}
	}
	b.WriteString("\nStart another Fishhawk run on this issue for the remaining scope, or close the " +
		"issue by hand once it has landed. If the merge closed this issue anyway, reopen it to " +
		"track the remaining scope.\n")
	return b.String()
}
