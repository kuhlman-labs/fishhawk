package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// HandleApprovalCommand implements webhook.ApprovalCommandHandler
// for /fishhawk approve and /fishhawk reject (#238). Mirrors the
// HTTP approval handler's checks (role authorization, blocking-
// check enforcement, state-machine advance, orchestrator dispatch)
// but addresses the result in the issue conversation rather than
// an HTTP response.
//
// The handler is intentionally conservative on errors: every path
// posts a reply comment so the reviewer always sees what happened,
// and returns nil to the caller so the webhook receiver doesn't
// surface 5xx for a slash-command flap. Best-effort throughout —
// the SPA approval path remains the canonical surface; this is a
// companion for the demo loop.
func (s *Server) HandleApprovalCommand(ctx context.Context, p webhook.ApprovalCommandParams) error {
	if !s.approvalCommandConfigured() {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"slash-command approval skipped: deps incomplete",
			slog.String("repo", p.Repo),
			slog.Int("issue", p.IssueNumber),
		)
		return nil
	}

	decision, ok := decodeMatchAction(p.Decision)
	if !ok {
		// Defense-in-depth: matchIssueComment shouldn't pass
		// anything other than approve / reject through to here.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"slash-command approval received unrecognized decision",
			slog.String("decision", string(p.Decision)))
		return nil
	}

	subject := p.SenderLogin
	if subject == "" {
		s.replyApproval(ctx, p, "Cannot approve: comment is missing sender identity (no GitHub login).")
		return nil
	}

	// Reply-comment approvals (E17.3 / #338) skip silently on every
	// "no, this comment isn't an approval" branch — an operator who
	// types "+1 yes I agree" on an issue thread that happens not to
	// have a Fishhawk plan should not get an unsolicited reply.
	// Slash-command approvals keep their explicit help replies; the
	// reviewer typed a deliberate command.
	silent := p.Source == webhook.ApprovalSourceReplyComment

	runRow, stage, found, err := s.findAwaitingApprovalStage(ctx, p.Repo, p.IssueNumber)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"slash-command approval: run lookup failed",
			slog.String("repo", p.Repo),
			slog.Int("issue", p.IssueNumber),
			slog.String("error", err.Error()))
		if silent {
			return nil
		}
		s.replyApproval(ctx, p, "Could not look up the run for this issue. Try the dashboard.")
		return nil
	}
	if !found {
		if silent {
			return nil
		}
		s.replyApproval(ctx, p, fmt.Sprintf("No stage on this issue's run is awaiting approval. (Subject: @%s)", subject))
		return nil
	}

	// ADR-018 (#311, #313): review-stage approval is owned by GitHub.
	// The PR merge event (#312) advances the stage; branch protection's
	// required-reviewers enforces the approver list. Reply with a help
	// message pointing at the PR rather than submitting an approval.
	// Plan-stage slash approvals continue to work. Reply-comment
	// approvals skip silently: the operator wasn't necessarily talking
	// about the review stage either.
	if stage.Type == run.StageTypeReview {
		if silent {
			return nil
		}
		s.replyApproval(ctx, p, reviewStageHelpReply(runRow, subject))
		return nil
	}

	if msg, allowed := s.authorizeSlashApprover(ctx, stage, subject); !allowed {
		s.replyApproval(ctx, p, msg)
		return nil
	}

	// ADR-017 (#249, #253): the approval gate no longer reads
	// stage_check state. Reviewers approve based on plan + diff;
	// GitHub branch protection blocks the merge until the required
	// checks (including fishhawk_audit_complete, published per
	// #231) report green.

	var commentPtr *string
	if p.Comment != "" {
		c := p.Comment
		commentPtr = &c
	}

	res, err := s.cfg.ApprovalRepo.Submit(ctx, approval.SubmitParams{
		StageID:         stage.ID,
		ApproverSubject: subject,
		Decision:        decision,
		Comment:         commentPtr,
		Surface:         approval.SurfaceGitHubComment,
	})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
			"slash-command approval: submit failed",
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()))
		s.replyApproval(ctx, p, "Could not record the approval. Try the dashboard.")
		return nil
	}
	if !res.Inserted {
		// A previous submission from the same approver already
		// settled the gate. Surface that fact rather than
		// re-running the transition.
		s.replyApproval(ctx, p, fmt.Sprintf("@%s already submitted a `%s` decision on this stage; the prior decision wins.", subject, res.Approval.Decision))
		return nil
	}

	advanced, err := s.advanceStage(ctx, stage.ID, decision)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
			"slash-command approval: advance stage failed",
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()))
		s.replyApproval(ctx, p, "Approval recorded but the run could not advance. Check the dashboard.")
		return nil
	}

	s.writeSlashApprovalAudit(ctx, advanced, res.Approval)

	if s.cfg.Orchestrator != nil {
		if _, err := s.cfg.Orchestrator.Advance(ctx, advanced.RunID); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
				"slash-command approval: orchestrator advance failed",
				slog.String("run_id", advanced.RunID.String()),
				slog.String("stage_id", advanced.ID.String()),
				slog.String("error", err.Error()))
			// Don't unwind — the gate decision is recorded.
		}
	}

	// Plan-approved comment-back (#274, #304): NotifyPlanApproved is
	// the single source of truth for the plan-approve confirmation
	// on the issue thread. The slash reply duplicates that broadcast
	// for the plan-approve path, so it is deliberately skipped here.
	// The slash reply still fires for plan-reject (no broadcast on
	// that path), review-stage approve/reject (NotifyPlanApproved is
	// plan-scoped), and authorization/error paths handled above.
	if advanced.Type == run.StageTypePlan && decision == approval.DecisionApprove {
		if err := s.issueNotifier.NotifyPlanApproved(ctx, advanced.RunID, p.SenderLogin, decision); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"slash-command approval: plan-approved comment-back failed",
				slog.String("run_id", advanced.RunID.String()),
				slog.String("stage_id", advanced.ID.String()),
				slog.String("error", err.Error()))
		}
	} else {
		s.replyApproval(ctx, p, formatSuccessReply(decision, subject, runRow.ID, advanced))
	}

	// Sticky status comment (E20.4 / #330). Mirrors the HTTP
	// approval path: every approval (approve / reject, any stage
	// type) updates the sticky comment.
	s.notifyStatusUpdate(ctx, advanced.RunID, "slash_approval")
	return nil
}

// approvalCommandConfigured returns true when every dependency the
// slash-command handler needs is wired. Skipping cleanly when
// something's missing keeps the dispatcher's "ApprovalHandler is
// nil → silently skip" promise honest even if the handler is
// wired but its deps aren't.
func (s *Server) approvalCommandConfigured() bool {
	return s.cfg.RunRepo != nil &&
		s.cfg.ApprovalRepo != nil &&
		s.cfg.AuditRepo != nil &&
		s.issueNotifier != nil
}

// findAwaitingApprovalStage resolves (repo, issue_number) to the
// most-recent non-terminal run + its single awaiting-approval
// stage. Returns (run, stage, true, nil) on a hit; (nil, nil,
// false, nil) when no eligible stage exists. v0 has at most one
// awaiting-approval stage per run; the issue body's "out of scope:
// per-stage targeting" note documents that decision.
//
// Implementation pulls a bounded slice of recent runs via
// ListRuns(filter{Repo}) and filters in Go for the matching
// trigger_ref. For v0 cardinality (small handful of runs per repo
// per issue) this is faster than another sqlc query + index. If
// repo cardinality grows past 200 active runs, swap in a focused
// query.
func (s *Server) findAwaitingApprovalStage(ctx context.Context, repo string, issueNumber int) (*run.Run, *run.Stage, bool, error) {
	triggerRef := fmt.Sprintf("issue:%d", issueNumber)
	runs, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
		Repo:  repo,
		Limit: 200,
	})
	if err != nil {
		return nil, nil, false, fmt.Errorf("list runs: %w", err)
	}
	for _, r := range runs {
		if r.TriggerRef == nil || *r.TriggerRef != triggerRef {
			continue
		}
		if r.State.IsTerminal() {
			continue
		}
		stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, r.ID)
		if err != nil {
			return nil, nil, false, fmt.Errorf("list stages: %w", err)
		}
		for _, st := range stages {
			if st.State == run.StageStateAwaitingApproval {
				return r, st, true, nil
			}
		}
	}
	return nil, nil, false, nil
}

// authorizeSlashApprover wraps the role-resolver check into a non-
// HTTP shape. Returns (replyMsg, false) on denial; ("", true) on
// allow. Mirrors the HTTP handler's best-effort posture: a
// resolver / spec-fetch failure logs but allows the submission, so
// transient GitHub flaps don't black-hole the gate.
func (s *Server) authorizeSlashApprover(ctx context.Context, stage *run.Stage, subject string) (string, bool) {
	if s.cfg.RoleResolver == nil {
		return "", true
	}
	gate, err := s.fetchGateForStage(ctx, stage)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"slash-command approval: fetch gate failed",
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()))
		return "", true // best-effort allow
	}
	if gate == nil || gate.approvers == nil {
		// No approval gate on the stage. Treat as allowed —
		// matches the HTTP handler's posture for stages without
		// an explicit approvers block.
		return "", true
	}
	allowed, err := s.cfg.RoleResolver.CanApprove(ctx, gate.installationID, gate.approvers, gate.roles, subject)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"slash-command approval: role resolution failed",
			slog.String("stage_id", stage.ID.String()),
			slog.String("subject", subject),
			slog.String("error", err.Error()))
		return "", true // best-effort allow
	}
	if !allowed {
		return fmt.Sprintf("Cannot approve: @%s is not in this stage's approvers list.", subject), false
	}
	return "", true
}

// writeSlashApprovalAudit mirrors writeApprovalAudit's chain entry
// — same category, same payload shape — so audit consumers don't
// care which surface produced the row.
func (s *Server) writeSlashApprovalAudit(ctx context.Context, stage *run.Stage, app *approval.Approval) {
	systemKind := audit.ActorKind("user")
	approver := app.ApproverSubject
	payload, _ := json.Marshal(map[string]any{
		"stage_id": stage.ID.String(),
		"decision": string(app.Decision),
		"surface":  string(app.Surface),
		"approver": approver,
	})
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        stage.RunID,
		StageID:      &stage.ID,
		Timestamp:    time.Now().UTC(),
		Category:     "approval_submitted",
		ActorKind:    &systemKind,
		ActorSubject: &approver,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
			"slash-command approval: audit append failed",
			slog.String("run_id", stage.RunID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()))
	}
}

// replyApproval posts a reply comment to the issue using the
// existing issuecomment notifier. Best-effort: a GitHub failure
// here is logged but doesn't unwind — the approval (or refusal)
// is already recorded server-side.
func (s *Server) replyApproval(ctx context.Context, p webhook.ApprovalCommandParams, body string) {
	if s.issueNotifier == nil {
		return
	}
	if err := s.issueNotifier.NotifySlashApprovalReply(ctx, issuecomment.SlashApprovalReply{
		Repo:           p.Repo,
		InstallationID: p.InstallationID,
		IssueNumber:    p.IssueNumber,
		Body:           body,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"slash-command approval: reply comment failed",
			slog.String("repo", p.Repo),
			slog.Int("issue", p.IssueNumber),
			slog.String("error", err.Error()))
	}
}

// decodeMatchAction translates the dispatcher's tagged action into
// an approval.Decision. Returns false for anything that isn't an
// approve / reject — defense in depth against the dispatcher
// passing an unexpected value.
func decodeMatchAction(a webhook.MatchAction) (approval.Decision, bool) {
	switch a {
	case webhook.MatchActionApprove:
		return approval.DecisionApprove, true
	case webhook.MatchActionReject:
		return approval.DecisionReject, true
	}
	return "", false
}

// reviewStageHelpReply is the slash-approval response when the
// reviewer targets a review stage. ADR-018 / #313 moved review-
// stage approval onto GitHub; the help text points at the PR
// (when the run row has one stamped) so the reviewer's next action
// is one click away.
func reviewStageHelpReply(runRow *run.Run, subject string) string {
	if runRow != nil && runRow.PullRequestURL != nil && *runRow.PullRequestURL != "" {
		return fmt.Sprintf("Review-stage approval is recorded from GitHub's PR surface. Approve or merge the PR to advance the stage: %s (caller: @%s)",
			*runRow.PullRequestURL, subject)
	}
	return fmt.Sprintf("Review-stage approval is recorded from GitHub's PR surface — approve or merge the PR to advance the stage. (Caller: @%s)", subject)
}

// formatSuccessReply renders the celebratory reply on approve /
// reject success. Includes the run ID's short prefix so a
// reviewer scrolling old issues sees which run a comment refers
// to without cross-referencing.
func formatSuccessReply(decision approval.Decision, subject string, runID uuid.UUID, stage *run.Stage) string {
	verb := "Approved"
	if decision == approval.DecisionReject {
		verb = "Rejected"
	}
	short := runID.String()
	if len(short) >= 8 {
		short = short[:8]
	}
	return fmt.Sprintf("%s by @%s. Stage `%s` advanced for run `%s`.", verb, subject, stage.Type, short)
}
