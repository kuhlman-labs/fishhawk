package server

import (
	"context"
	"log/slog"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/reviewsandbox"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// groundedReviewer is the optional capability a reviewer adapter implements to
// run against an exported read-only source tree (#2486, ADR-027 review
// grounding). It mirrors the binaryReporter / versionProber optional-capability
// precedent: a reviewer that does not implement it is driven diff-only via the
// base Review. Both the codex and claudecode adapters implement it; the
// anthropic SDK adapter (which cannot be handed a local directory) does not, so
// a panel containing it stays ungrounded (allInvocationsGrounded is false).
type groundedReviewer interface {
	ReviewGrounded(ctx context.Context, promptText, treeDir string) (verdict *planreview.ReviewVerdict, model string, err error)
}

// allInvocationsGrounded reports whether EVERY resolved reviewer in the loop
// implements the grounding capability, so the whole loop can be grounded
// together (#2486). Grounding is a per-LOOP decision: a mixed panel — one
// grounding-capable reviewer and one not — must be ungrounded for EVERYONE, so
// the prompt is never asymmetric (it claims a tree that half the panel cannot
// read) and the review is never partially grounded. An unresolved invocation
// (resolveErr != nil) cannot be grounded, so its presence also forces the whole
// loop ungrounded. An empty list is not grounded.
func allInvocationsGrounded(invocations []reviewerInvocation) bool {
	if len(invocations) == 0 {
		return false
	}
	for _, inv := range invocations {
		if inv.resolveErr != nil {
			return false
		}
		if _, ok := inv.reviewer.(groundedReviewer); !ok {
			return false
		}
	}
	return true
}

// invokeReview drives one reviewer invocation, routing to the grounded path when
// a tree was exported (treeDir != "") and the adapter implements the capability,
// and to the diff-only Review otherwise (#2486). The treeDir != "" caller
// contract already guarantees allInvocationsGrounded held, but the type
// assertion is repeated here as a defensive belt: a reviewer that somehow lacks
// the capability degrades to diff-only rather than dropping the review.
func (*Server) invokeReview(ctx context.Context, inv reviewerInvocation, promptText, treeDir string) (*planreview.ReviewVerdict, string, error) {
	if treeDir != "" {
		if gr, ok := inv.reviewer.(groundedReviewer); ok {
			return gr.ReviewGrounded(ctx, promptText, treeDir)
		}
	}
	return inv.reviewer.Review(ctx, promptText)
}

// exportReviewTree exports the read-only source tree a grounded review runs
// against (#2486). It returns the export directory, the resolved commit SHA (C4:
// resolved ONCE by reviewsandbox.ExportTree and handed back so the caller names
// in the prompt the exact commit that was archived), the extraction Stats (so
// the caller can disclose skipped entries, C3), and a cleanup closure the caller
// MUST call.
//
// It returns an empty dir + commit, zero Stats, and a NO-OP cleanup on every
// degrade — each WARN-logged with the reason so the grounding decision is
// observable:
//   - the kill switch (FISHHAWKD_REVIEW_GROUNDING=false → ReviewGroundingDisabled);
//   - an empty runRow.WorkingDir (the github_actions runner case, where no local
//     checkout exists on this host to archive);
//   - an empty ref (an implement review with no resolved head SHA);
//   - any ExportTree error (git absent, not a work tree, unresolvable ref,
//     bounds exceeded, a traversal entry).
//
// A caller that receives an empty dir builds an ungrounded, diff-only prompt.
func (s *Server) exportReviewTree(ctx context.Context, runRow *run.Run, ref string) (dir, commit string, stats reviewsandbox.Stats, cleanup func()) {
	noop := func() {}

	if s.cfg.ReviewGroundingDisabled {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "review grounding: disabled by kill switch — degrading to diff-only",
			slog.String("run_id", runRow.ID.String()))
		return "", "", reviewsandbox.Stats{}, noop
	}
	if runRow.WorkingDir == "" {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "review grounding: run has no working dir — degrading to diff-only",
			slog.String("run_id", runRow.ID.String()))
		return "", "", reviewsandbox.Stats{}, noop
	}
	if ref == "" {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "review grounding: empty ref — degrading to diff-only",
			slog.String("run_id", runRow.ID.String()))
		return "", "", reviewsandbox.Stats{}, noop
	}

	exportDir, resolvedSHA, st, cl, err := reviewsandbox.ExportTree(ctx, runRow.WorkingDir, ref, reviewsandbox.DefaultLimits())
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "review grounding: export failed — degrading to diff-only",
			slog.String("run_id", runRow.ID.String()),
			slog.String("ref", ref),
			slog.String("error", err.Error()))
		return "", "", reviewsandbox.Stats{}, noop
	}
	return exportDir, resolvedSHA, st, cl
}
