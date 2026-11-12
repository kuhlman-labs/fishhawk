package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// Partial-delivery ship-time guard (E83.52 / #4085).
//
// A plan may declare `delivery: partial` (plan.(*Plan).IsPartialDelivery): the
// run deliberately ships only a slice of its triggering issue, so its PR must
// reference the issue with `Refs #N` rather than close it. The implement prompt
// tells the agent so, but an instruction is not a control: an agent that still
// writes `Closes #N` would silently close an issue whose remaining scope is
// still owed. neutralizePartialDeliveryClosingRef is the deterministic backstop
// — on the implement ship it re-reads the LIVE PR body and rewrites every
// active closing reference to the issue as `Refs #N`, recording a
// partial_delivery_closing_reference_neutralized audit row.
//
// Named residuals (the guard does NOT cover them):
//
//   - A later fix-up push, or an operator's hand edit of the PR body, that
//     reintroduces `Closes #N` is not re-checked before merge — the guard runs
//     once, on the implement ship.
//   - The guard is GitHub-only (it needs the GitHub client and an installation
//     id). On GitLab nothing rewrites the body, while the merge-time
//     remaining-scope comment (partial_delivery_merge.go) still posts — which
//     is why that comment is worded to stay true even when the issue closed
//     anyway.
//   - A closing keyword in an agent-authored COMMIT message is not rewritten
//     (the prompt forbids it); after this guard the PR body carries Refs, so a
//     squash merge that copies the body is covered.

// categoryPartialDeliveryClosingReferenceNeutralized is the audit category the
// ship-time guard appends after a successful PR-body rewrite. Registered in
// audit.KnownCategories and admitted to issuecomment.activityCategories.
const categoryPartialDeliveryClosingReferenceNeutralized = "partial_delivery_closing_reference_neutralized"

// neutralizeClosingReferences rewrites every ACTIVE GitHub closing reference to
// issue n in body as `Refs #n`. It delegates to the shared definition in
// orchestrator.NeutralizeClosingReferences (#5192), which the consolidated
// PR body applies too; invariant: hasClosingReference(result, n) is false.
func neutralizeClosingReferences(body string, n int) (string, int) {
	return orchestrator.NeutralizeClosingReferences(body, n)
}

// partialDeliveryPlan loads the run's approved plan (loadApprovedPlanForRun,
// which walks a retry chain to its planning ancestor) and reports whether it
// declares a PARTIAL delivery through plan.(*Plan).IsPartialDelivery — the one
// shared definition. A load error WARN-logs and reports false, as does a run
// with no plan (nil). Shared by the ship-time guard and the merge-time
// remaining-scope comment.
func (s *Server) partialDeliveryPlan(ctx context.Context, runID uuid.UUID) (*plan.Plan, bool) {
	p, err := s.loadApprovedPlanForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"partial delivery: load approved plan failed; treating as full delivery",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return nil, false
	}
	return p, p.IsPartialDelivery()
}

// neutralizePartialDeliveryClosingRef is the ship-time guard (see the file
// comment). handleShipPullRequest calls it on EVERY successful implement ship —
// after the branch-lineage guard passes and OUTSIDE the running-stage terminal
// drive, so the non-gated flow whose stage the trace handler already advanced is
// covered too — and never on the child-push, fix-up, gating-reject or
// lineage-failure returns, nor on the idempotent replay.
//
// The success path of the ship handler never loads the run row, so this loads it
// itself. Each precondition returns before any forge call: the run must load,
// carry a triggering issue (IssueContext.Number > 0, checked before the plan is
// loaded), and its approved plan must declare a PARTIAL delivery; the GitHub
// client and an installation id must be present (GitLab / no installation is an
// INFO skip — the named residual). It then reads the LIVE PR body, so an agent
// body is judged as GitHub holds it. Zero rewrites → no edit and no audit. The
// audit row is appended ONLY after EditPullRequest succeeds, so the chain never
// claims a rewrite the forge did not take. Every failure WARN-logs and never
// unwinds the ship response.
func (s *Server) neutralizePartialDeliveryClosingRef(ctx context.Context, runID, stageID uuid.UUID, prNumber int) {
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil || runRow == nil {
		attrs := []slog.Attr{slog.String("run_id", runID.String())}
		if err != nil {
			attrs = append(attrs, slog.String("error", err.Error()))
		}
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"partial delivery guard: load run failed; skipping closing-reference check", attrs...)
		return
	}
	if runRow.IssueContext == nil || runRow.IssueContext.Number <= 0 {
		return
	}
	issueNumber := runRow.IssueContext.Number
	if _, partial := s.partialDeliveryPlan(ctx, runID); !partial {
		return
	}
	if s.cfg.GitHub == nil || runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"partial delivery guard: no GitHub client or installation; PR body not checked (GitLab residual)",
			slog.String("run_id", runID.String()),
			slog.Int("issue_number", issueNumber))
		return
	}
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"partial delivery guard: unparseable repo; skipping closing-reference check",
			slog.String("run_id", runID.String()),
			slog.String("repo", runRow.Repo),
			slog.String("error", err.Error()))
		return
	}
	scope := forge.FromGitHubInstallationID(*runRow.InstallationID)
	// A non-positive prNumber needs no guard of its own: GetPullRequest refuses
	// it before any request, which lands on the WARN-and-skip below.
	pr, err := s.cfg.GitHub.GetPullRequest(ctx, scope, repo, prNumber)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"partial delivery guard: get pr failed; closing reference not checked",
			slog.String("run_id", runID.String()),
			slog.Int("pr_number", prNumber),
			slog.String("error", err.Error()))
		return
	}
	updated, rewritten := neutralizeClosingReferences(pr.Body, issueNumber)
	if rewritten == 0 {
		return
	}
	if err := s.cfg.GitHub.EditPullRequest(ctx, scope, repo, prNumber, updated); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"partial delivery guard: edit pr body failed; the PR still closes the issue on merge",
			slog.String("run_id", runID.String()),
			slog.Int("pr_number", prNumber),
			slog.Int("issue_number", issueNumber),
			slog.String("error", err.Error()))
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"issue_number": issueNumber,
		"pr_number":    prNumber,
		"rewritten":    rewritten,
	})
	systemKind := audit.ActorSystem
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  categoryPartialDeliveryClosingReferenceNeutralized,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"partial delivery guard: append audit entry failed after the PR body was rewritten",
			slog.String("run_id", runID.String()),
			slog.Int("pr_number", prNumber),
			slog.String("error", err.Error()))
		return
	}
	s.notifyOperatorVisible(ctx, runID, categoryPartialDeliveryClosingReferenceNeutralized)
}
