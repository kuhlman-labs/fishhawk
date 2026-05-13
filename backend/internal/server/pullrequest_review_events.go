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
// row naming the merger. When merged=false (closed without merging),
// logs and skips — v0 doesn't auto-cancel runs; the operator
// decides whether to manually intervene.
//
// Best-effort throughout: a parse failure, a missing run, or a
// non-Fishhawk-managed PR all log and return without surfacing as
// a 5xx. Idempotent on redeliveries: TransitionStage is a no-op on
// an already-succeeded stage.
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
		// Closed without merging. v0 leaves the run alone — the
		// review stage stays awaiting_approval so the operator sees
		// the run is stuck and can manually cancel via the SPA /
		// API. File a follow-up if customers want auto-cancel here.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"pull_request.closed: not merged; no state change",
			slog.String("pr_url", prURL),
			slog.String("run_id", target.ID.String()),
			slog.String("sender", p.Sender.Login))
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
