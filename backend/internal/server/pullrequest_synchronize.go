package server

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// synchronizePayload is the slice of GitHub's pull_request.synchronize
// payload Fishhawk reads. Mirrors the wire shape; we only need the
// PR URL to look up a Fishhawk run.
type synchronizePayload struct {
	PullRequest struct {
		HTMLURL string `json:"html_url"`
		Number  int    `json:"number"`
		Head    struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
}

// republishOnSynchronize handles a GitHub `pull_request.synchronize`
// event by finding the matching Fishhawk run on the PR URL (#216
// denormalizes pull_request_url onto every run) and re-running the
// audit-complete Compute + publish flow. This is what makes the
// foreign-commit rule (#282) surface drift to branch protection
// without waiting for a SPA visitor to trigger a fresh Compute via
// GET /v0/stages/{id}/checks.
//
// Best-effort throughout. A skip emits a structured log line; an
// unrecoverable I/O failure does too. We never 5xx — the canonical
// signal is already on the audit chain via existing flows, and a
// missed delivery only delays the recompute until the next SPA
// visit or webhook on the same PR.
func (s *Server) republishOnSynchronize(ctx context.Context, raw []byte) {
	if s.cfg.RunRepo == nil || s.cfg.ArtifactRepo == nil || s.cfg.AuditRepo == nil {
		return
	}
	var p synchronizePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "pull_request.synchronize: parse failed",
			slog.String("error", err.Error()))
		return
	}
	prURL := p.PullRequest.HTMLURL
	if prURL == "" {
		return
	}

	runs, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
		PullRequestURL: &prURL,
		Limit:          5,
	})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"pull_request.synchronize: run lookup failed",
			slog.String("pr_url", prURL),
			slog.String("error", err.Error()))
		return
	}
	if len(runs) == 0 {
		// Not a Fishhawk-managed PR. No-op.
		return
	}
	// The most-recent run on the PR is what branch protection cares
	// about. Recompute against that run's chain — the chain walk
	// inside `auditcomplete.gatherForeignCommitInputs` brings in
	// the parent runs' head_shas, so retry chains (post-#276) work
	// without special-casing here.
	target := runs[0]

	state, missing, err := auditcomplete.Compute(ctx, target.ID, s.auditCompleteDeps())
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"pull_request.synchronize: compute failed",
			slog.String("run_id", target.ID.String()),
			slog.String("error", err.Error()))
		return
	}

	// Publish the fresh state to GitHub so branch protection
	// re-evaluates. Nil-safe — `publishAuditCheck` short-circuits
	// when the publisher isn't wired (dev posture).
	s.publishAuditCheck(ctx, target.ID, state, missing)

	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "pull_request.synchronize: republished audit-complete",
		slog.String("pr_url", prURL),
		slog.String("head_sha", p.PullRequest.Head.SHA),
		slog.String("run_id", target.ID.String()),
		slog.String("state", string(state)),
		slog.Int("missing_count", len(missing)),
	)
}
