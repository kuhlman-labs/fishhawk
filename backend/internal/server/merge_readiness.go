package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Merge-readiness refusal codes (#4086). Both are 409 preconditions the
// operator merge endpoint returns BEFORE the merge_verdict_recorded append, so
// a merge that would only queue-then-time-out records no verdict and names the
// verb that clears it instead.
const (
	mergeCodeAcceptanceStale   = "acceptance_stale"
	mergeCodeApprovalDismissed = "approval_dismissed"
)

// Values of the approval_dismissed refusal's dismissing_cause detail: what the
// audit chain says wrote the live PR head that the dismissed approval no longer
// covers. The forge does not say which push dismissed a review, so the cause is
// read from Fishhawk's own chain.
const (
	dismissingCauseVouchCommit = "vouch_commit"
	dismissingCauseFixupPush   = "fixup_push"
	dismissingCausePush        = "push"
	dismissingCauseUnknown     = "unknown"
)

// mergeReadinessRefusal is one merge-readiness blocker (#4086). It never
// writes HTTP: handleMergeRun renders it as a 409 and the gate view reuses the
// same value as a blocker, so the two surfaces cannot drift.
type mergeReadinessRefusal struct {
	Code    string
	Message string
	Details map[string]any
}

// acceptanceStaleRefusal reports a STALE acceptance verdict (#4086): the run's
// acceptance stage carries a stage-scoped acceptance_reopened entry NEWER than
// the newest acceptance_outcome_recorded entry. That is the state
// reopenAcceptanceOnFixupPush (and the #1567 operator re-open) leaves behind:
// the stage is back to pending while acceptanceGateState still reads the stale
// recorded outcome and admits the merge, which then sits behind a pending
// fishhawk_audit_complete until the tool times out.
//
// It keys on the chain's OWN invalidation record, never on comparing the
// outcome's head with the live PR head: a vouch commit or an ADR-090 rebase
// changes the head without re-opening acceptance, by design, and a head compare
// would block merges the server has no verb to clear.
//
// nil (admit) when: the audit repo is unwired; the run has no acceptance stage
// row; no reopen is scoped to that stage; no outcome is recorded at all (the
// acceptance gate's pending / outcome-unknown states already refuse that); or
// the newest outcome is newer than the newest reopen (the re-run shipped its
// verdict). FAIL-CLOSED: an audit read error is returned, never resolved to
// admit — the evidence lives on Fishhawk's own chain, like acceptanceGateState.
func (s *Server) acceptanceStaleRefusal(ctx context.Context, runRow *run.Run, stages []*run.Stage) (*mergeReadinessRefusal, error) {
	if s.cfg.AuditRepo == nil {
		return nil, nil
	}
	acc := acceptanceStageOf(stages)
	if acc == nil {
		return nil, nil
	}
	reopen, err := s.latestAcceptanceReopenEntry(ctx, runRow.ID, acc.ID)
	if err != nil {
		return nil, err
	}
	if reopen == nil {
		return nil, nil
	}
	outcomes, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runRow.ID, CategoryAcceptanceOutcomeRecorded)
	if err != nil {
		return nil, fmt.Errorf("list %s audit entries: %w", CategoryAcceptanceOutcomeRecorded, err)
	}
	var outcome *audit.Entry
	for _, e := range outcomes {
		if outcome == nil || e.Sequence > outcome.Sequence {
			outcome = e
		}
	}
	if outcome == nil || outcome.Sequence > reopen.Sequence {
		return nil, nil
	}

	verifiedHead := auditPayloadString(outcome.Payload, "head_sha")
	currentHead := auditPayloadString(reopen.Payload, "head_sha")
	nextStep, how := acceptanceStaleNextStep(acc.State)
	return &mergeReadinessRefusal{
		Code: mergeCodeAcceptanceStale,
		Message: fmt.Sprintf("the recorded acceptance verdict is stale: it validated head %s, and acceptance stage %s was re-opened afterwards at head %s (a fix-up push or an operator re-open), so the verdict no longer covers the pull request and the fishhawk_audit_complete check will not clear. The queued merge would only time out. %s, then re-invoke the merge. No merge verdict was recorded.",
			shaOrUnrecorded(verifiedHead), acc.ID, shaOrUnrecorded(currentHead), how),
		Details: map[string]any{
			"acceptance_stage_id":    acc.ID.String(),
			"acceptance_stage_state": string(acc.State),
			"verified_head_sha":      verifiedHead,
			"current_head_sha":       currentHead,
			"outcome_sequence":       outcome.Sequence,
			"reopened_sequence":      reopen.Sequence,
			"next_step":              nextStep,
		},
	}, nil
}

// latestAcceptanceReopenEntry returns the newest acceptance_reopened entry
// scoped to stageID, or nil when there is none. READ-ONLY: it is the one
// reference to CategoryAcceptanceReopened in this file, exempted BY NAME in
// TestAcceptanceReopenedWriters_AreExactlyTheKnownSites (#3176). Unlike
// latestAcceptanceEpisodeRestartSeq it returns the entry itself, because the
// stale-acceptance refusal reports the reopen's head_sha. The read error is
// propagated so the caller fails closed.
func (s *Server) latestAcceptanceReopenEntry(ctx context.Context, runID, stageID uuid.UUID) (*audit.Entry, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryAcceptanceReopened)
	if err != nil {
		return nil, fmt.Errorf("list %s audit entries: %w", CategoryAcceptanceReopened, err)
	}
	var latest *audit.Entry
	for _, e := range entries {
		if e.StageID == nil || *e.StageID != stageID {
			continue
		}
		if latest == nil || e.Sequence > latest.Sequence {
			latest = e
		}
	}
	return latest, nil
}

// acceptanceStaleNextStep names the verb that re-runs the re-opened acceptance
// stage from its current state, plus the sentence the refusal message uses.
func acceptanceStaleNextStep(state run.StageState) (string, string) {
	switch state {
	case run.StageStateDispatched, run.StageStateRunning:
		return "fishhawk_await_stage", "The acceptance re-run is in flight: await it with fishhawk_await_stage"
	case run.StageStateSucceeded, run.StageStateFailed, run.StageStateCancelled, run.StageStateSuperseded:
		return "fishhawk_retry_stage", "The acceptance stage settled without a newer verdict: re-open it with fishhawk_retry_stage and dispatch it"
	}
	return "fishhawk_dispatch_stage", "Re-run acceptance against the current head with fishhawk_dispatch_stage and await its verdict"
}

// approvalDismissedResult is approvalDismissedCheck's answer. Refusal is
// non-nil only on a determined approval_dismissed blocker. Determined is true
// when the reviews listing was read and folded to a verdict (refuse or admit).
// Undetermined names why the check admitted WITHOUT a verdict (the fail-open
// reasons); it is empty whenever Determined is true.
type approvalDismissedResult struct {
	Refusal      *mergeReadinessRefusal
	Determined   bool
	Undetermined string
}

// approvalDismissedCheck reports a pull request whose approval was DISMISSED
// and not re-given (#4086): GitHub reports mergeable_state=blocked, no
// reviewer's latest review is APPROVED, and at least one reviewer's latest
// review is DISMISSED. A vouch commit, a fix-up push or a rebase dismisses a
// stale approval under "dismiss stale reviews", and the queued merge then
// never fires.
//
// BEST-EFFORT and FAIL-OPEN, in the prMergeConflicting posture: a failed forge
// read must not block a merge GitHub itself would accept. Each of these admits
// with Undetermined naming the reason: no GitHub client, no installation, an
// unparseable repo, no PR number, a GetPullRequest error, a merged PR, a
// mergeable_state other than "blocked", a reviews read error, or a truncated
// listing.
//
// A dismissed review reports DISMISSED and loses its earlier state, so a
// dismissed CHANGES_REQUESTED is indistinguishable from a dismissed approval;
// the refusal therefore says "no approval is live and a prior review was
// dismissed", and re-approving is the remedy either way. A never-reviewed PR
// has no DISMISSED review and still queues: the "queueing before approval is
// safe" contract is unchanged.
func (s *Server) approvalDismissedCheck(ctx context.Context, runRow *run.Run) approvalDismissedResult {
	undetermined := func(why string) approvalDismissedResult {
		return approvalDismissedResult{Undetermined: why}
	}
	if s.cfg.GitHub == nil {
		return undetermined("no GitHub client is wired")
	}
	if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		return undetermined("the run carries no GitHub installation")
	}
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		return undetermined("the run repo is unparseable")
	}
	prNumber := parsePRNumberFromURL(runRow.PullRequestURL)
	if prNumber <= 0 {
		return undetermined("the run carries no parseable pull request number")
	}
	scope := forge.FromGitHubInstallationID(*runRow.InstallationID)
	pr, err := s.cfg.GitHub.GetPullRequest(ctx, scope, repo, prNumber)
	if err != nil {
		return undetermined("the pull request read failed: " + err.Error())
	}
	if pr.Merged {
		return undetermined("the pull request is already merged")
	}
	if pr.MergeableState != "blocked" {
		return undetermined(fmt.Sprintf("GitHub reports mergeable_state %q, not blocked, so a missing approval is not what holds the merge", pr.MergeableState))
	}
	reviews, truncated, err := s.cfg.GitHub.ListPullRequestReviews(ctx, scope, repo, prNumber)
	if err != nil {
		return undetermined("the pull request reviews read failed: " + err.Error())
	}
	if truncated {
		return undetermined("the pull request reviews listing was truncated")
	}

	dismissed, live := latestReviewVerdict(reviews)
	if live || dismissed == nil {
		return approvalDismissedResult{Determined: true}
	}
	cause := s.dismissingCause(ctx, runRow, pr.HeadSHA)
	return approvalDismissedResult{
		Determined: true,
		Refusal: &mergeReadinessRefusal{
			Code: mergeCodeApprovalDismissed,
			Message: fmt.Sprintf("no approval is live on the pull request and a prior review by %s on commit %s was dismissed; GitHub reports mergeable_state=blocked, so the queued merge would never fire. The current head %s (%s) is not covered by an approval. Re-approve the pull request under your own GitHub identity (gh pr review --approve), then re-invoke the merge. No merge verdict was recorded.",
				reviewerOrUnknown(dismissed.UserLogin), shaOrUnrecorded(dismissed.CommitID), shaOrUnrecorded(pr.HeadSHA), dismissingCausePhrase(cause)),
			Details: map[string]any{
				"dismissed_reviewer": dismissed.UserLogin,
				"approved_commit":    dismissed.CommitID,
				"dismissing_commit":  pr.HeadSHA,
				"dismissing_cause":   cause,
				"mergeable_state":    pr.MergeableState,
				"next_step":          "approve_pr",
			},
		},
	}
}

// latestReviewVerdict folds a chronological reviews listing to each
// reviewer's LATEST review, ignoring COMMENTED and PENDING (neither grants nor
// withdraws an approval). live is true when any reviewer's latest review is
// APPROVED; dismissed is the most recent reviewer-latest DISMISSED review, or
// nil when there is none.
func latestReviewVerdict(reviews []githubclient.PullRequestReview) (dismissed *githubclient.PullRequestReview, live bool) {
	latest := map[string]int{}
	var order []string
	for i, rv := range reviews {
		switch rv.State {
		case "COMMENTED", "PENDING":
			continue
		}
		if _, seen := latest[rv.UserLogin]; !seen {
			order = append(order, rv.UserLogin)
		}
		latest[rv.UserLogin] = i
	}
	dismissedIdx := -1
	for _, login := range order {
		i := latest[login]
		switch reviews[i].State {
		case "APPROVED":
			live = true
		case "DISMISSED":
			if i > dismissedIdx {
				dismissedIdx = i
			}
		}
	}
	if dismissedIdx >= 0 {
		rv := reviews[dismissedIdx]
		dismissed = &rv
	}
	return dismissed, live
}

// dismissingCause reads the audit chain for what wrote headSHA: an
// operator_commit_vouched head is vouch_commit, a fixup_pushed head is
// fixup_push, anything else is push. An audit read error (or an unwired repo,
// or an empty head) degrades to unknown — the refusal itself still stands,
// because its evidence is the forge's.
func (s *Server) dismissingCause(ctx context.Context, runRow *run.Run, headSHA string) string {
	if s.cfg.AuditRepo == nil || headSHA == "" {
		return dismissingCauseUnknown
	}
	for _, c := range []struct {
		category, field, cause string
	}{
		{CategoryOperatorCommitVouched, lineageVouchedSHAField, dismissingCauseVouchCommit},
		{CategoryFixupPushed, "head_sha", dismissingCauseFixupPush},
	} {
		entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runRow.ID, c.category)
		if err != nil {
			return dismissingCauseUnknown
		}
		for _, e := range entries {
			if sha := auditPayloadString(e.Payload, c.field); sha != "" && strings.EqualFold(sha, headSHA) {
				return c.cause
			}
		}
	}
	return dismissingCausePush
}

// dismissingCausePhrase renders a dismissing_cause value for the message.
func dismissingCausePhrase(cause string) string {
	switch cause {
	case dismissingCauseVouchCommit:
		return "an operator-vouched commit (fishhawk_vouch_commit)"
	case dismissingCauseFixupPush:
		return "a fix-up push"
	case dismissingCausePush:
		return "a push Fishhawk did not record as a vouch or a fix-up"
	}
	return "cause unknown: the run's audit chain could not be read"
}

// auditPayloadString decodes one top-level string field from an audit payload,
// returning "" when the payload or the field is absent or not a string.
func auditPayloadString(payload []byte, field string) string {
	var m map[string]any
	if len(payload) == 0 || json.Unmarshal(payload, &m) != nil {
		return ""
	}
	v, _ := m[field].(string)
	return v
}

func shaOrUnrecorded(sha string) string {
	if sha == "" {
		return "(unrecorded)"
	}
	return sha
}

func reviewerOrUnknown(login string) string {
	if login == "" {
		return "(a deleted account)"
	}
	return login
}
