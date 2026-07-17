package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryBranchReset is the audit-log category for the entry the
// reset-branch handler writes when it force-rewinds a run/PR branch back
// to its last run-authored HEAD, dropping a foreign commit pushed ON TOP
// of the run's commits (ADR-035 remediation, #867). It is the durable
// record of a destructive, operator-gated action; the payload names the
// dropped commit, the reset target, the prior head, and a recovery note,
// so the rewind is fully auditable and reversible. It drives a sticky
// status-comment refresh (an issue-comment surface).
const CategoryBranchReset = "branch_reset"

// resetBranchRequest is the JSON body of POST /v0/runs/{run_id}/reset-branch.
// Confirm MUST be true — the reset is destructive (it force-updates the
// PR head ref), so it is never silent/auto: a missing or false confirm
// returns 400. Reason is an operator note recorded on the audit entry.
type resetBranchRequest struct {
	Reason  string `json:"reason"`
	Confirm bool   `json:"confirm"`
}

// resetBranchResponse summarizes a successful rewind.
type resetBranchResponse struct {
	RunID                 string `json:"run_id"`
	PRNumber              int    `json:"pr_number"`
	Branch                string `json:"branch"`
	DroppedOffendingSHA   string `json:"dropped_offending_sha"`
	ResetToSHA            string `json:"reset_to_sha"`
	PriorHeadSHA          string `json:"prior_head_sha"`
	ReparkedReviewStageID string `json:"reparked_review_stage_id,omitempty"`
	RecoveryNote          string `json:"recovery_note"`
}

// handleResetRunBranch implements POST /v0/runs/{run_id}/reset-branch.
//
// It is the operator-gated, audited ADR-035 remediation (#867) for a
// foreign commit pushed ON TOP of a run's own commits — the post-#861
// residual vector. It rewinds the open run/PR branch back to its last
// run-authored HEAD, dropping the on-top foreign commit, then re-parks
// the review gate so CI + the merge reconciler re-evaluate the rewound
// head and no pre-reset head can race to merge.
//
// Safety invariants (all BINDING, because this force-rewrites a branch):
//
//   - FAIL-CLOSED. On ANY uncertainty — unresolvable base ref, incomplete
//     ledger, CompareCommits error, no identifiable run-authored HEAD —
//     it REFUSES (reset_not_determinable) and never force-updates. This
//     inverts detection's fail-open posture: a destructive op on an
//     uncertain classification is unacceptable.
//   - ON-TOP ONLY. It proceeds only when EVERY foreign commit sits
//     strictly above the reset target (the newest ledger member). An
//     ancestor/interleaved foreign commit is refused reset_out_of_scope
//     — a reset can't drop an ancestor; prevention (#861/#865) owns that.
//   - LEASE RE-CHECK. Immediately before the force-update it re-reads the
//     live PR head and aborts if it no longer equals the head it
//     classified — the only TOCTOU guard (the REST refs API has no CAS),
//     which narrows but does not eliminate the concurrent-push window.
//   - OPERATOR-GATED + AUDITED. Requires confirm==true (else 400) and
//     write:runs; a run-bound MCP token may reset only its own branch
//     (subject-binding, mirroring the fixup handler). Every rewind emits
//     a branch_reset audit entry and keeps the dropped commit recoverable.
func (s *Server) handleResetRunBranch(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	if id.TokenID != "" && !hasScope(id, "write:runs") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:runs",
			map[string]any{"required_scope": "write:runs"})
		return
	}

	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil || s.cfg.GitHub == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "reset_unconfigured",
			"reset-branch endpoint requires run + audit repositories and a GitHub client", nil)
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	var reqBody resetBranchRequest
	if r.Body != nil {
		if decErr := json.NewDecoder(r.Body).Decode(&reqBody); decErr != nil && !errors.Is(decErr, io.EOF) {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"request body must be valid JSON {reason, confirm}",
				map[string]any{"error": decErr.Error()})
			return
		}
	}
	// Operator-gated: the reset is destructive, so it is never silent/auto.
	if !reqBody.Confirm {
		s.writeError(w, r, http.StatusBadRequest, "confirmation_required",
			"reset-branch force-rewinds the PR head ref; resend with confirm=true to proceed",
			map[string]any{"field": "confirm"})
		return
	}

	// Subject-binding guard: a run-bound MCP token (subject
	// "mcp:run:<uuid>") may reset ONLY its own run's branch. Mirrors the
	// fixup handler.
	if strings.HasPrefix(id.Subject, "mcp:run:") {
		runIDStr := strings.TrimPrefix(id.Subject, "mcp:run:")
		subjectRunID, parseErr := uuid.Parse(runIDStr)
		if parseErr != nil {
			s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
				"mcp token subject is malformed", nil)
			return
		}
		if subjectRunID != runID {
			s.writeError(w, r, http.StatusForbidden, "cross_run_reset",
				"mcp token may only reset its own run's branch",
				map[string]any{
					"token_run_id":  subjectRunID.String(),
					"target_run_id": runID.String(),
				})
			return
		}
	}

	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run failed", map[string]any{"error": err.Error()})
		return
	}

	// Resolve the PR, its live head, and the run branch. Every
	// unresolvable input is a fail-CLOSED refusal (reset_not_determinable)
	// — never a force-update on an uncertain anchor.
	if runRow.InstallationID == nil {
		s.writeResetNotDeterminable(w, r, "run has no installation to authorize a GitHub force-update")
		return
	}
	scope := forge.FromGitHubInstallationID(*runRow.InstallationID)
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		s.writeResetNotDeterminable(w, r, "run repo is unparseable: "+err.Error())
		return
	}
	prNumber := parsePRNumberFromURL(runRow.PullRequestURL)
	if prNumber <= 0 {
		s.writeResetNotDeterminable(w, r, "run has no tracked pull request to reset")
		return
	}
	pr, err := s.cfg.GitHub.GetPullRequestScoped(r.Context(), scope, repo, prNumber)
	if err != nil {
		s.writeResetNotDeterminable(w, r, "resolve live PR head failed: "+err.Error())
		return
	}
	headSHA, branch := pr.HeadSHA, pr.HeadRef
	if headSHA == "" || branch == "" {
		s.writeResetNotDeterminable(w, r, "PR returned an empty head sha or branch")
		return
	}

	// Classify: find the last run-authored HEAD, the offending on-top
	// commit, and whether the foreign commit sits strictly on top.
	lastAuthoredSHA, offendingSHA, isOnTop, ok := s.resolveLastRunAuthoredHead(
		r.Context(), runRow, scope, repo, headSHA, prNumber)
	if !ok {
		s.writeResetNotDeterminable(w, r,
			"could not classify the run branch's lineage with certainty")
		return
	}
	if !isOnTop {
		s.writeError(w, r, http.StatusUnprocessableEntity, "reset_out_of_scope",
			"the foreign commit is an ancestor of (or interleaved with) the run's commits, not strictly on top; a reset cannot drop it — prevention (#861/#865) owns this case",
			map[string]any{"offending_sha": offendingSHA, "last_authored_sha": lastAuthoredSHA})
		return
	}
	if lastAuthoredSHA == headSHA {
		s.writeError(w, r, http.StatusUnprocessableEntity, "reset_not_applicable",
			"the branch tip is already the last run-authored HEAD; there is no foreign commit on top to drop",
			map[string]any{"head_sha": headSHA})
		return
	}

	// LEASE RE-CHECK (the only TOCTOU guard — the REST refs API has no
	// compare-and-swap). Re-read the live head and abort if it changed
	// since classification, so a concurrent push between classify and
	// patch cannot be silently clobbered. Narrows, does not eliminate,
	// the window.
	livePR, err := s.cfg.GitHub.GetPullRequestScoped(r.Context(), scope, repo, prNumber)
	if err != nil {
		s.writeResetNotDeterminable(w, r, "lease re-check: re-read live PR head failed: "+err.Error())
		return
	}
	if livePR.HeadSHA != headSHA {
		s.writeResetNotDeterminable(w, r,
			"lease re-check: the live PR head changed since classification (concurrent push); reset aborted")
		return
	}

	// Force-rewind the run branch to the last run-authored HEAD.
	if err := s.cfg.GitHub.ForceUpdateRefScoped(r.Context(), scope, repo, branch, lastAuthoredSHA); err != nil {
		s.writeError(w, r, http.StatusBadGateway, "reset_force_update_failed",
			"force-update of the PR head ref failed", map[string]any{"error": err.Error()})
		return
	}

	// Re-park the review gate (best-effort) so the merge reconciler +
	// ReverifyBranchLineage re-evaluate the rewound clean tip and no
	// pre-reset head can race to merge. Tolerate the commit-yourself
	// shape with no separate review stage (nil re-park).
	reparkedID := ""
	if reparked, err := s.reparkReviewGateForReset(r.Context(), runID); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"branch reset: re-park review gate failed (best-effort)",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
	} else if reparked != nil {
		reparkedID = reparked.ID.String()
	}

	recoveryNote := "dropped commit " + offendingSHA +
		" remains recoverable from the remote reflog or the foreign pusher's own branch"

	s.writeBranchResetAudit(r, runID, prNumber, branch, offendingSHA, lastAuthoredSHA, headSHA, reqBody.Reason, recoveryNote, reparkedID)
	s.notifyStatusUpdate(r.Context(), runID, "branch_reset")

	s.writeJSON(w, r, http.StatusOK, resetBranchResponse{
		RunID:                 runID.String(),
		PRNumber:              prNumber,
		Branch:                branch,
		DroppedOffendingSHA:   offendingSHA,
		ResetToSHA:            lastAuthoredSHA,
		PriorHeadSHA:          headSHA,
		ReparkedReviewStageID: reparkedID,
		RecoveryNote:          recoveryNote,
	})
}

// writeResetNotDeterminable is the fail-CLOSED refusal: a 422 that
// carries the reason the classification could not be made with certainty,
// so the operator learns WHY no force-update happened.
func (s *Server) writeResetNotDeterminable(w http.ResponseWriter, r *http.Request, reason string) {
	s.writeError(w, r, http.StatusUnprocessableEntity, "reset_not_determinable",
		"cannot determine a safe reset target with certainty; refusing the destructive action: "+reason,
		nil)
}

// reparkReviewGateForReset re-arms the run's review gate after a rewind so
// the merge reconciler re-runs ReverifyBranchLineage on the rewound tip.
// It finds the run's review stage parked at awaiting_approval and re-parks
// it awaiting_approval → pending → awaiting_approval (both edges admitted by
// TransitionStage's fix-up tables) — synchronously, so the orchestrator
// never observes the intermediate pending state. Returns the re-armed stage,
// or (nil, nil) when the run has no separate review stage awaiting approval
// (the commit-yourself shape) — a tolerated no-op, mirroring the fixup
// handler's nil-review handling.
func (s *Server) reparkReviewGateForReset(ctx context.Context, runID uuid.UUID) (*run.Stage, error) {
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list stages for run: %w", err)
	}
	var review *run.Stage
	for _, st := range stages {
		if st.Type == run.StageTypeReview && st.State == run.StageStateAwaitingApproval {
			review = st
			break
		}
	}
	if review == nil {
		return nil, nil
	}
	if _, err := s.cfg.RunRepo.TransitionStage(ctx, review.ID, run.StageStatePending, nil); err != nil {
		return nil, fmt.Errorf("re-park review → pending: %w", err)
	}
	rearmed, err := s.cfg.RunRepo.TransitionStage(ctx, review.ID, run.StageStateAwaitingApproval, nil)
	if err != nil {
		return nil, fmt.Errorf("re-arm review → awaiting_approval: %w", err)
	}
	return rearmed, nil
}

// writeBranchResetAudit appends the branch_reset audit entry recording the
// full destructive action — the dropped offending commit, the reset
// target, the prior head, the operator reason, and the recovery note — so
// the rewind is auditable and reversible. Operator actor (never a silent
// system action). Best-effort: the force-update already happened, so an
// append failure WARNs but doesn't unwind the response.
func (s *Server) writeBranchResetAudit(r *http.Request, runID uuid.UUID, prNumber int,
	branch, droppedSHA, resetToSHA, priorHeadSHA, reason, recoveryNote, reparkedReviewStageID string) {
	id := IdentityFrom(r.Context())
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := audit.ActorUser

	fields := map[string]any{
		"run_id":                runID.String(),
		"pr_number":             prNumber,
		"branch":                branch,
		"dropped_offending_sha": droppedSHA,
		"reset_to_sha":          resetToSHA,
		"prior_head_sha":        priorHeadSHA,
		"reason":                reason,
		"recovery_note":         recoveryNote,
	}
	if reparkedReviewStageID != "" {
		fields["reparked_review_stage_id"] = reparkedReviewStageID
	}
	payload, _ := json.Marshal(fields)

	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        runID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryBranchReset,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"branch reset: append branch_reset audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
	}
}
