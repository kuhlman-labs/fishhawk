package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// mergeReconcilerActor is the audit ActorSubject recorded when the
// review gate is resolved by the merge-status reconciler poll
// (ADR-031 Phase 1) rather than the pull_request.closed webhook. The
// poll path lacks the merger/closer login the webhook payload carries,
// so it records this system marker; the audit category stays
// pr_merged / pr_closed_without_merge so consumers + the SPA render
// identically regardless of which surface resolved the gate.
const mergeReconcilerActor = "merge-reconciler"

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

	// Build the resolution metadata from the webhook payload and hand
	// off to the shared resolver. actorLogin is the merger when merged,
	// the closer (sender) otherwise; the webhook carries the head/base
	// SHAs the poll path lacks.
	meta := reviewMergeMeta{
		prURL:     prURL,
		headSHA:   p.PullRequest.Head.SHA,
		baseSHA:   p.PullRequest.Base.SHA,
		actorKind: audit.ActorKind("user"),
	}
	if p.PullRequest.Merged {
		meta.actorLogin = mergerLogin(p)
	} else {
		meta.actorLogin = p.Sender.Login
	}
	s.resolveReviewStageOnMerge(ctx, target, p.PullRequest.Merged, meta)
}

// reviewMergeMeta carries the PR-identifying detail recorded when the
// review gate is resolved on a terminal PR state, decoupled from the
// signal source. The pull_request.closed webhook populates every field
// (actorLogin = merger/closer, head/base SHAs, actorKind = user); the
// merge-status reconciler poll (ResolveReviewFromPollState) populates
// only prURL and sets actorLogin = mergeReconcilerActor + a system
// actorKind, leaving the SHAs empty.
type reviewMergeMeta struct {
	prURL      string
	headSHA    string
	baseSHA    string
	actorLogin string
	actorKind  audit.ActorKind
}

// resolveReviewStageOnMerge is the shared review-gate resolution path
// for a terminal PR state, invoked from BOTH the pull_request.closed
// webhook (handlePullRequestClosed) and the merge-status reconciler
// poll (ResolveReviewFromPollState). merged=true resolves the review
// stage to succeeded and writes a pr_merged audit row; merged=false
// (closed without merging, #316 / ADR-018) resolves to cancelled and
// writes a pr_closed_without_merge row — change not accepted is
// terminal, and `cancelled` is the existing reachable target from
// awaiting_approval.
//
// Idempotent by construction: the webhook and poll route through this
// one method, and TransitionStage short-circuits when from == to (the
// same-state allowance in ValidStageTransition), so a webhook close
// followed by a reconciler poll (or vice versa, or a redelivery) on the
// same review stage converge on a single effective transition.
//
// Best-effort throughout: a missing run shape (routine_change-style
// implement-only workflows) still records the audit row; a
// state-machine transition reject logs without rolling back the audit.
func (s *Server) resolveReviewStageOnMerge(ctx context.Context, target *run.Run, merged bool, meta reviewMergeMeta) {
	reviewStage := s.findReviewStage(ctx, target.ID)
	var stageID *uuid.UUID
	if reviewStage != nil {
		stageID = &reviewStage.ID
	}

	if merged {
		// ADR-036 (#876) implement-review / merge completion gate. When
		// the run has a review stage AND a configured agent implement
		// review (ADR-027) is still in-flight, refuse to resolve the
		// merge to succeeded: leave the review stage parked in
		// awaiting_approval so the merge reconciler re-polls and routes
		// back through here once the review settles (or the backstop
		// elapses). Composes with the #862 lineage re-check already on
		// the tick path — both must pass for the stage to resolve.
		//
		// The gate is placed BEFORE writePRMergedAudit deliberately: it
		// DEFERS the pr_merged audit row from merge-observation time to
		// resolution time so a held run does not write a duplicate
		// pr_merged row on every reconciler re-poll tick. This is a
		// behavioral change from the prior audit-first contract — a
		// GitHub-merged run that is held by this gate has NO pr_merged
		// row until the re-poll clears the gate. The backstop guarantees
		// the run always eventually resolves, so pr_merged is always
		// eventually recorded.
		if reviewStage != nil && !s.checkImplementReviewSettled(ctx, target, reviewStage) {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
				"pull_request merged: held pending implement review; review stage left parked",
				slog.String("pr_url", meta.prURL),
				slog.String("run_id", target.ID.String()),
				slog.String("stage_id", reviewStage.ID.String()),
			)
			return
		}
		// Audit row first — if the transition fails (state-machine
		// reject) we still want the merge recorded.
		s.writePRMergedAudit(ctx, target.ID, stageID, meta)
		if reviewStage == nil {
			// No review stage on this run shape (routine_change-style
			// workflows are implement-only). The merge still happened.
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
				"pull_request merged: no review stage on run; audit-only",
				slog.String("pr_url", meta.prURL),
				slog.String("run_id", target.ID.String()))
			// Sticky status comment (E20.4 / #330) — the audit row
			// reflects the merge; the comment should too.
			s.notifyStatusUpdate(ctx, target.ID, "pr_merged_no_review")
			return
		}
		if _, err := s.cfg.RunRepo.TransitionStage(ctx,
			reviewStage.ID, run.StageStateSucceeded, nil); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"pull_request merged: review-stage transition failed",
				slog.String("run_id", target.ID.String()),
				slog.String("stage_id", reviewStage.ID.String()),
				slog.String("error", err.Error()))
			return
		}
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"pull_request merged: review stage transitioned to succeeded",
			slog.String("pr_url", meta.prURL),
			slog.String("run_id", target.ID.String()),
			slog.String("stage_id", reviewStage.ID.String()),
			slog.String("merger", meta.actorLogin),
		)
		// The review stage is now terminal but the RUN is still
		// running — Advance walks the now-all-terminal stages to
		// completeRun, which yields succeeded on merge. Mirror the
		// approval handler (approvals.go): best-effort, log an Advance
		// error but never roll back the stage transition or audit row.
		s.advanceRunAfterReviewResolve(ctx, target.ID)
		// Sticky status comment (E20.4 / #330). The PR merging is the
		// terminal state for review-gated workflows; this is one of the
		// most operator-visible moments of the run lifecycle.
		s.notifyStatusUpdate(ctx, target.ID, "pr_merged")
		return
	}

	// Closed without merging (ADR-018's "closed without merge =
	// abandoned work" stance). Audit row first.
	s.writePRClosedWithoutMergeAudit(ctx, target.ID, stageID, meta)
	if reviewStage == nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"pull_request closed without merge: no review stage on run; audit-only",
			slog.String("pr_url", meta.prURL),
			slog.String("run_id", target.ID.String()),
			slog.String("closer", meta.actorLogin))
		// Sticky status comment (E20.4 / #330) — audit row is in;
		// surface the close in the issue thread.
		s.notifyStatusUpdate(ctx, target.ID, "pr_closed_no_review")
		return
	}
	if _, err := s.cfg.RunRepo.TransitionStage(ctx,
		reviewStage.ID, run.StageStateCancelled, nil); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"pull_request closed without merge: review-stage cancel transition failed",
			slog.String("run_id", target.ID.String()),
			slog.String("stage_id", reviewStage.ID.String()),
			slog.String("error", err.Error()))
		return
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
		"pull_request closed without merge: review stage cancelled",
		slog.String("pr_url", meta.prURL),
		slog.String("run_id", target.ID.String()),
		slog.String("stage_id", reviewStage.ID.String()),
		slog.String("closer", meta.actorLogin),
	)
	// The review stage is terminal (cancelled) but the RUN is still
	// running — Advance walks the now-all-terminal stages to
	// completeRun, which yields cancelled on a closed-unmerged PR.
	s.advanceRunAfterReviewResolve(ctx, target.ID)
	// Sticky status comment (E20.4 / #330). Review stage cancelled
	// is a terminal-ish surface state — the user should see the
	// run's review row flip to cancelled in the comment.
	s.notifyStatusUpdate(ctx, target.ID, "pr_closed_without_merge")
}

// advanceRunAfterReviewResolve drives the run to its terminal state
// after resolveReviewStageOnMerge transitions the review stage to a
// terminal state. Without this the run is left {review terminal, run
// running} forever (#727) — the stage transition alone never completes
// the run; the orchestrator's Advance does (it routes the now-all-
// terminal stages through completeRun → succeeded/cancelled). Mirrors
// the approval handler (approvals.go): best-effort, nil-guarded, logs an
// Advance error at error level but never rolls back the stage transition
// or audit row (the gate has already resolved).
func (s *Server) advanceRunAfterReviewResolve(ctx context.Context, runID uuid.UUID) {
	if s.cfg.Orchestrator == nil {
		return
	}
	if _, err := s.cfg.Orchestrator.Advance(ctx, runID); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
			"resolve review on merge: orchestrator advance failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
	}
}

// ResolveReviewFromPollState resolves a run's review stage from the
// merge-status reconciler's live PR poll (ADR-031 Phase 1), routing
// through the SAME resolveReviewStageOnMerge path the
// pull_request.closed webhook uses. merged=true -> succeeded;
// merged=false (closed without merge) -> cancelled. Because both
// surfaces share the resolver, the poll is idempotent against the
// webhook and produces identical terminal state.
//
// The poll lacks the merger/SHA detail the webhook payload carries, so
// the audit row records the mergeReconcilerActor system marker with
// empty SHAs; the category is unchanged so audit consumers render
// identically regardless of source.
func (s *Server) ResolveReviewFromPollState(ctx context.Context, runID uuid.UUID, merged bool, prURL string) error {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return errors.New("server: ResolveReviewFromPollState requires RunRepo and AuditRepo")
	}
	target, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("resolve review from poll: get run %s: %w", runID, err)
	}
	s.resolveReviewStageOnMerge(ctx, target, merged, reviewMergeMeta{
		prURL:      prURL,
		actorLogin: mergeReconcilerActor,
		actorKind:  audit.ActorSystem,
	})
	return nil
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

// writePRClosedWithoutMergeAudit appends a pr_closed_without_merge
// audit row naming the closer (meta.actorLogin = the webhook
// `sender.login`, or the mergeReconcilerActor marker on the poll path;
// closed-without-merge events don't populate `merged_by`).
//
// Reopening is intentionally out of scope: the cancelled stage is
// terminal. If a reviewer reopens the PR and wants Fishhawk involved
// again, they re-trigger via `/fishhawk run` on the issue; the new run
// threads off the cancelled parent via the `parent_run_id` lineage
// primitive (#216).
func (s *Server) writePRClosedWithoutMergeAudit(ctx context.Context, runID uuid.UUID, stageID *uuid.UUID, meta reviewMergeMeta) {
	closer := meta.actorLogin
	actorKind := meta.actorKind
	payload, _ := json.Marshal(map[string]any{
		"pr_url":   meta.prURL,
		"closer":   closer,
		"head_sha": meta.headSHA,
		"base_sha": meta.baseSHA,
	})
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        runID,
		StageID:      stageID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryPRClosedWithoutMerge,
		ActorKind:    &actorKind,
		ActorSubject: &closer,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
			"pull_request.closed: pr_closed_without_merge audit append failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
	}
}

// writePRMergedAudit appends a pr_merged audit row. Called from the
// shared resolver both with a review-stage ID (the common case) and
// without (routine_change-style runs that lack a review stage but still
// merge). meta.actorLogin is the merger (webhook) or the
// mergeReconcilerActor marker (poll path).
func (s *Server) writePRMergedAudit(ctx context.Context, runID uuid.UUID, stageID *uuid.UUID, meta reviewMergeMeta) {
	merger := meta.actorLogin
	actorKind := meta.actorKind
	payload, _ := json.Marshal(map[string]any{
		"pr_url":   meta.prURL,
		"merger":   merger,
		"head_sha": meta.headSHA,
		"base_sha": meta.baseSHA,
	})
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        runID,
		StageID:      stageID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryPRMerged,
		ActorKind:    &actorKind,
		ActorSubject: &merger,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
			"pull_request.closed: pr_merged audit append failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
	}
}

// checkImplementReviewSettled enforces the ADR-036 (#876) implement-review /
// merge completion gate, the internal-resolution mirror of
// checkPlanReviewSettled (approvals.go). It returns true to allow the merge to
// resolve the review stage to succeeded, false to hold the run parked for the
// merge reconciler to re-poll. Unlike the plan-gate handler it writes no HTTP
// response — this is the webhook/poll resolution path, not a request handler.
//
// Posture mirrors checkPlanReviewSettled: every read failure fails OPEN
// (WARN-log, return true) so a transient backend hiccup can never strand a
// merge that GitHub already performed. The gate holds only when ALL of:
//   - the run's IMPLEMENT stage declares reviewers.agent > 0, AND
//   - at least one implement_review_started entry exists (dispatched), AND
//   - fewer than reviewers.agent TERMINAL review entries
//     (implement_reviewed | implement_review_failed | implement_review_skipped)
//     have landed, AND
//   - the elapsed time since the earliest implement_review_started is within
//     the backstop bound.
//
// ANY terminal review kind counts toward the unblock, so a budget-killed
// reviewer (trace.go emits a terminal implement_review_failed on timeout)
// never strands the gate. The backstop is the belt for a reviewer that dies
// emitting NO terminal entry: past the bound the merge is ALLOWED to resolve
// and an implement_review_backstop_elapsed audit entry records the degrade.
// The backstop emits exactly once — once it returns true the resolve completes
// the run and the reconciler stops re-polling, so no idempotency guard is
// needed.
func (s *Server) checkImplementReviewSettled(ctx context.Context, target *run.Run, reviewStage *run.Stage) bool {
	reviewersCfg := s.resolveStageReviewers(ctx, target, spec.StageTypeImplement)
	if reviewersCfg == nil || reviewersCfg.Agent == 0 {
		// No agent reviewer configured — byte-for-byte the pre-ADR-036
		// merge resolution (routine_change / check-gate auto-merge flow).
		return true
	}

	started, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, target.ID, "implement_review_started")
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "implement-review gate: list implement_review_started failed",
			slog.String("run_id", target.ID.String()),
			slog.String("stage_id", reviewStage.ID.String()),
			slog.String("error", err.Error()),
		)
		return true
	}
	if len(started) == 0 {
		// Configured but never dispatched — nothing to wait for.
		return true
	}

	terminalCount := 0
	for _, cat := range auditcomplete.TerminalImplementReviewCategories {
		entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, target.ID, cat)
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "implement-review gate: list terminal review entries failed",
				slog.String("run_id", target.ID.String()),
				slog.String("stage_id", reviewStage.ID.String()),
				slog.String("category", cat),
				slog.String("error", err.Error()),
			)
			return true
		}
		terminalCount += len(entries)
	}

	// Delegate the present/in-flight decision to auditcomplete.ReviewPresent
	// (#947) — the single source of truth shared with the audit-complete
	// pre-merge presence gate so the two can never diverge. The backstop
	// (reused planReviewBackstop, approvals.go) belts a reviewer that died
	// emitting no terminal entry; ReviewPresent reports backstopElapsed
	// exactly when the merge is allowed BECAUSE the bound elapsed, so the
	// degrade audit emits exactly once.
	now := time.Now().UTC()
	present, backstopElapsed := auditcomplete.ReviewPresent(auditcomplete.ReviewPresenceInputs{
		ReviewersAgent: reviewersCfg.Agent,
		Started:        started,
		TerminalCount:  terminalCount,
		Backstop:       s.planReviewBackstop(reviewersCfg.Agent),
		Now:            now,
	})
	if backstopElapsed {
		earliest := started[0].Timestamp
		for _, e := range started {
			if e.Timestamp.Before(earliest) {
				earliest = e.Timestamp
			}
		}
		s.appendImplementReviewBackstopElapsed(ctx, reviewStage, reviewersCfg.Agent, terminalCount, earliest, now.Sub(earliest))
	}
	if !present {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "implement-review gate: holding merge resolution; review in-flight",
			slog.String("run_id", target.ID.String()),
			slog.String("stage_id", reviewStage.ID.String()),
			slog.Int("configured_agents", reviewersCfg.Agent),
			slog.Int("landed_terminal", terminalCount),
		)
	}
	return present
}

// appendImplementReviewBackstopElapsed records the ADR-036 backstop degrade:
// the implement-review completion gate allowed a merge to resolve because the
// hard bound elapsed before the configured agent reviews all reached a
// terminal state. Best-effort — a logged audit failure never holds the merge.
// Mirrors appendPlanReviewBackstopElapsed (approvals.go).
func (s *Server) appendImplementReviewBackstopElapsed(ctx context.Context, stage *run.Stage, configuredAgents, landedTerminal int, startedAt time.Time, elapsed time.Duration) {
	systemKind := audit.ActorSystem
	payload, _ := json.Marshal(map[string]any{
		"stage_id":          stage.ID.String(),
		"configured_agents": configuredAgents,
		"landed_terminal":   landedTerminal,
		"started_at":        startedAt.Format(time.RFC3339Nano),
		"elapsed_seconds":   int(elapsed.Seconds()),
	})
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     stage.RunID,
		StageID:   &stage.ID,
		Timestamp: time.Now().UTC(),
		Category:  "implement_review_backstop_elapsed",
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.Error("audit append failed for implement_review_backstop_elapsed",
			"run_id", stage.RunID, "stage_id", stage.ID, "error", err.Error())
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
