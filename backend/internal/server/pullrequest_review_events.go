package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Audit-log categories for the GitHub-side actions ADR-018 / #312
// pulls into the chain. Distinct from the in-Fishhawk
// `approval_submitted` category so consumers can tell which surface
// produced the row.
const (
	// CategoryPRMerged records that a Fishhawk-managed PR merged on
	// GitHub. Payload carries the merger's login + the head/base
	// SHAs. The webhook handler also transitions the run's review
	// stage to succeeded after writing this row.
	CategoryPRMerged = "pr_merged"
	// CategoryPRApprovedOnGitHub records an approving
	// pull_request_review.submitted event on a Fishhawk-managed PR.
	// Audit-only — no state transition; the merge event is what
	// drives the stage forward per ADR-018.
	CategoryPRApprovedOnGitHub = "pr_approved_on_github"
	// CategoryPRReviewSubmitted is the catch-all for non-approve
	// review submissions (commented, changes_requested,
	// dismissed). Same audit-only posture as the approve case;
	// distinct category lets the SPA render the right verb.
	CategoryPRReviewSubmitted = "pr_review_submitted"
	// CategoryPRClosedWithoutMerge records that a Fishhawk-managed
	// PR was closed without merging (#316). The webhook handler
	// also cancels the run's review stage after writing this row,
	// per ADR-018's "closed without merge = abandoned work"
	// stance — the alternative (leave the stage in awaiting_approval
	// indefinitely) clutters the dashboard with stale state and
	// the existing state machine has a clean target (`cancelled`)
	// reachable from `awaiting_approval`.
	CategoryPRClosedWithoutMerge = "pr_closed_without_merge"
)

// pullRequestClosedPayload is the subset of the GitHub
// `pull_request.closed` webhook payload Fishhawk reads. The merger's
// login lives on `pull_request.merged_by` when `merged: true`; when
// `merged: false` (PR closed without merging) `merged_by` is absent.
type pullRequestClosedPayload struct {
	PullRequest struct {
		HTMLURL  string `json:"html_url"`
		Number   int    `json:"number"`
		Merged   bool   `json:"merged"`
		MergedBy *struct {
			Login string `json:"login"`
		} `json:"merged_by"`
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			SHA string `json:"sha"`
		} `json:"base"`
	} `json:"pull_request"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}

// pullRequestReviewPayload is the subset of the GitHub
// `pull_request_review.submitted` webhook payload Fishhawk reads.
// `review.state` is one of approved / commented / changes_requested
// / dismissed (post-event; "pending" reviews don't fire submitted).
type pullRequestReviewPayload struct {
	Review struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		State string `json:"state"`
		Body  string `json:"body"`
	} `json:"review"`
	PullRequest struct {
		HTMLURL string `json:"html_url"`
		Number  int    `json:"number"`
	} `json:"pull_request"`
}

// reviewBodyExcerptMax bounds the review body we copy into the
// audit payload. Full bodies can be paragraphs of prose; storing
// them inline bloats the audit chain without adding much auditable
// signal beyond "they said something here." Truncate to a snippet;
// reviewers wanting the full text click through to GitHub.
const reviewBodyExcerptMax = 280

// handlePullRequestClosed handles `pull_request.closed` events.
// When merged=true, transitions the matching Fishhawk run's review
// stage to succeeded (ADR-018 / #311) and writes a pr_merged audit
// row naming the merger. When merged=false (closed without merging,
// #316), transitions the review stage to cancelled and writes a
// pr_closed_without_merge row naming the closer.
//
// Best-effort throughout: a parse failure, a missing run, or a
// non-Fishhawk-managed PR all log and return without surfacing as
// a 5xx. Idempotent on redeliveries: TransitionStage is a no-op on
// an already-terminal stage.
func (s *Server) handlePullRequestClosed(ctx context.Context, raw []byte) {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return
	}
	var p pullRequestClosedPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"pull_request.closed: parse failed",
			slog.String("error", err.Error()))
		return
	}
	prURL := p.PullRequest.HTMLURL
	if prURL == "" {
		return
	}
	target := s.findRunByPullRequestURL(ctx, prURL, "pull_request.closed")
	if target == nil {
		return
	}

	if !p.PullRequest.Merged {
		s.handlePullRequestClosedWithoutMerge(ctx, target, p)
		return
	}

	reviewStage := s.findReviewStage(ctx, target.ID)
	if reviewStage == nil {
		// No review stage on this run shape (routine_change-style
		// workflows are implement-only). Still write the audit row
		// for completeness; the merge happened.
		s.writePRMergedAudit(ctx, target.ID, nil, p)
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"pull_request.closed: no review stage on run; audit-only",
			slog.String("pr_url", prURL),
			slog.String("run_id", target.ID.String()))
		// Sticky status comment (E20.4 / #330) — the audit row
		// reflects the merge; the comment should too.
		s.notifyStatusUpdate(ctx, target.ID, "pr_merged_no_review")
		return
	}

	// Audit row first — if the transition fails (state-machine
	// reject) we still want the merge recorded. Same-state
	// transition is idempotent (TransitionStage short-circuits when
	// from == to per ValidStageTransition's same-state allowance).
	s.writePRMergedAudit(ctx, target.ID, &reviewStage.ID, p)

	if _, err := s.cfg.RunRepo.TransitionStage(ctx,
		reviewStage.ID, run.StageStateSucceeded, nil); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"pull_request.closed: review-stage transition failed",
			slog.String("run_id", target.ID.String()),
			slog.String("stage_id", reviewStage.ID.String()),
			slog.String("error", err.Error()))
		return
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
		"pull_request.closed: review stage transitioned to succeeded",
		slog.String("pr_url", prURL),
		slog.String("run_id", target.ID.String()),
		slog.String("stage_id", reviewStage.ID.String()),
		slog.String("merger", mergerLogin(p)),
	)

	// Sticky status comment (E20.4 / #330). The PR merging is the
	// terminal state for review-gated workflows; this is one of the
	// most operator-visible moments of the run lifecycle.
	s.notifyStatusUpdate(ctx, target.ID, "pr_merged")
}

// handlePullRequestReviewSubmitted handles
// `pull_request_review.submitted` events. Audit-only per ADR-018:
// the merge event is what advances the stage, but the review
// itself is auditable (who approved, who requested changes, when).
//
// Best-effort: parse failures and unknown PRs log + return; an
// audit-append failure logs at error but doesn't surface as 5xx.
func (s *Server) handlePullRequestReviewSubmitted(ctx context.Context, raw []byte) {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return
	}
	var p pullRequestReviewPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"pull_request_review.submitted: parse failed",
			slog.String("error", err.Error()))
		return
	}
	prURL := p.PullRequest.HTMLURL
	if prURL == "" {
		return
	}
	target := s.findRunByPullRequestURL(ctx, prURL, "pull_request_review.submitted")
	if target == nil {
		return
	}
	reviewStage := s.findReviewStage(ctx, target.ID)
	var stageID *uuid.UUID
	if reviewStage != nil {
		stageID = &reviewStage.ID
	}

	category := CategoryPRReviewSubmitted
	if p.Review.State == "approved" {
		category = CategoryPRApprovedOnGitHub
	}
	payload, _ := json.Marshal(map[string]any{
		"pr_url":   prURL,
		"reviewer": p.Review.User.Login,
		"state":    p.Review.State,
		"body":     truncate(p.Review.Body, reviewBodyExcerptMax),
	})
	systemKind := audit.ActorKind("user")
	subject := p.Review.User.Login
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        target.ID,
		StageID:      stageID,
		Timestamp:    time.Now().UTC(),
		Category:     category,
		ActorKind:    &systemKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
			"pull_request_review.submitted: audit append failed",
			slog.String("run_id", target.ID.String()),
			slog.String("error", err.Error()))
		return
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
		"pull_request_review.submitted: audit row appended",
		slog.String("pr_url", prURL),
		slog.String("run_id", target.ID.String()),
		slog.String("reviewer", p.Review.User.Login),
		slog.String("state", p.Review.State),
	)
}

// findRunByPullRequestURL looks up the most-recent Fishhawk run on
// a PR via the `runs.pull_request_url` index (#216). Used by both
// PR-event handlers; the lookup shape matches what
// pullrequest_synchronize.go does. Returns nil + a log line when
// the PR isn't Fishhawk-managed or the query fails.
func (s *Server) findRunByPullRequestURL(ctx context.Context, prURL, eventLabel string) *run.Run {
	runs, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
		PullRequestURL: &prURL,
		Limit:          5,
	})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			eventLabel+": run lookup failed",
			slog.String("pr_url", prURL),
			slog.String("error", err.Error()))
		return nil
	}
	if len(runs) == 0 {
		return nil
	}
	return runs[0]
}

// findReviewStage returns the run's review stage, or nil when the
// workflow shape doesn't have one (routine_change is implement-only).
// Best-effort: a list failure logs and yields nil so the caller can
// keep going with audit-only handling.
func (s *Server) findReviewStage(ctx context.Context, runID uuid.UUID) *run.Stage {
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"list stages failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return nil
	}
	for _, st := range stages {
		if st.Type == run.StageTypeReview {
			return st
		}
	}
	return nil
}

// handlePullRequestClosedWithoutMerge is the merged=false branch of
// `pull_request.closed` (#316). Audit + cancel: write a
// pr_closed_without_merge row naming the closer, then transition
// the review stage to cancelled. Runs that don't have a review
// stage (routine_change-shape) still get the audit row; nothing
// to transition. Idempotent — the state machine treats a
// terminal-state transition as a no-op when the stage is already
// cancelled.
//
// Reopening is intentionally out of scope: the cancelled stage is
// terminal. If a reviewer reopens the PR and wants Fishhawk
// involved again, they re-trigger via `/fishhawk run` on the
// issue; the new run threads off the cancelled parent via the
// `parent_run_id` lineage primitive (#216).
func (s *Server) handlePullRequestClosedWithoutMerge(ctx context.Context, target *run.Run, p pullRequestClosedPayload) {
	reviewStage := s.findReviewStage(ctx, target.ID)
	var stageID *uuid.UUID
	if reviewStage != nil {
		stageID = &reviewStage.ID
	}
	// Audit row first — if the transition fails (state machine
	// reject) we still want the close recorded.
	s.writePRClosedWithoutMergeAudit(ctx, target.ID, stageID, p)

	if reviewStage == nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"pull_request.closed: not merged; no review stage on run; audit-only",
			slog.String("pr_url", p.PullRequest.HTMLURL),
			slog.String("run_id", target.ID.String()),
			slog.String("closer", p.Sender.Login))
		// Sticky status comment (E20.4 / #330) — audit row is in;
		// surface the close in the issue thread.
		s.notifyStatusUpdate(ctx, target.ID, "pr_closed_no_review")
		return
	}
	if _, err := s.cfg.RunRepo.TransitionStage(ctx,
		reviewStage.ID, run.StageStateCancelled, nil); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"pull_request.closed: review-stage cancel transition failed",
			slog.String("run_id", target.ID.String()),
			slog.String("stage_id", reviewStage.ID.String()),
			slog.String("error", err.Error()))
		return
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
		"pull_request.closed: review stage cancelled (PR closed without merging)",
		slog.String("pr_url", p.PullRequest.HTMLURL),
		slog.String("run_id", target.ID.String()),
		slog.String("stage_id", reviewStage.ID.String()),
		slog.String("closer", p.Sender.Login),
	)

	// Sticky status comment (E20.4 / #330). Review stage cancelled
	// is a terminal-ish surface state — the user should see the
	// run's review row flip to cancelled in the comment.
	s.notifyStatusUpdate(ctx, target.ID, "pr_closed_without_merge")
}

// writePRClosedWithoutMergeAudit appends a pr_closed_without_merge
// audit row naming the closer (the `sender.login` on the event;
// closed-without-merge events don't populate `merged_by`).
func (s *Server) writePRClosedWithoutMergeAudit(ctx context.Context, runID uuid.UUID, stageID *uuid.UUID, p pullRequestClosedPayload) {
	closer := p.Sender.Login
	payload, _ := json.Marshal(map[string]any{
		"pr_url":   p.PullRequest.HTMLURL,
		"closer":   closer,
		"head_sha": p.PullRequest.Head.SHA,
		"base_sha": p.PullRequest.Base.SHA,
	})
	systemKind := audit.ActorKind("user")
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        runID,
		StageID:      stageID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryPRClosedWithoutMerge,
		ActorKind:    &systemKind,
		ActorSubject: &closer,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
			"pull_request.closed: pr_closed_without_merge audit append failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
	}
}

// writePRMergedAudit appends a pr_merged audit row. Called from
// handlePullRequestClosed both with a review-stage ID (the common
// case) and without (routine_change-style runs that lack a review
// stage but still merge).
func (s *Server) writePRMergedAudit(ctx context.Context, runID uuid.UUID, stageID *uuid.UUID, p pullRequestClosedPayload) {
	merger := mergerLogin(p)
	payload, _ := json.Marshal(map[string]any{
		"pr_url":   p.PullRequest.HTMLURL,
		"merger":   merger,
		"head_sha": p.PullRequest.Head.SHA,
		"base_sha": p.PullRequest.Base.SHA,
	})
	systemKind := audit.ActorKind("user")
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        runID,
		StageID:      stageID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryPRMerged,
		ActorKind:    &systemKind,
		ActorSubject: &merger,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
			"pull_request.closed: pr_merged audit append failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
	}
}

// mergerLogin extracts the merger's GitHub login from the closed
// payload. Falls back to the sender when merged_by is absent
// (rare — GitHub usually fills it).
func mergerLogin(p pullRequestClosedPayload) string {
	if p.PullRequest.MergedBy != nil && p.PullRequest.MergedBy.Login != "" {
		return p.PullRequest.MergedBy.Login
	}
	return p.Sender.Login
}

// truncate snips s near a max byte count and tacks on an ellipsis;
// returns s unchanged when short enough. Mirrors the helper in the
// issuecomment package — duplicated here to keep this file from
// importing issuecomment for one helper.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "..."
}
