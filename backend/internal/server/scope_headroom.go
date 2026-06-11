package server

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
)

// effectiveScopeHeadroom computes the run's effective implement-scope
// file count against the implement stage's resolved max_files_changed
// cap (#983), so every scope-growing decision point — plan approval
// with add_scope_files, scope-amendment request/decision — can surface
// "approving puts effective scope at 31/30" before the implement stage
// burns its budget discovering the same thing.
//
// Effective scope = newest plan artifact's scope.files
//   - approval add_scope_files (loadApprovalAddScopeFiles)
//   - paths of already-APPROVED scope amendments
//   - extraPaths (the caller's not-yet-recorded addition)
//
// deduped by exact Path — the same compare-by-Path semantics as
// prompt.go's foldScopePaths, so the count the gates compute equals the
// effective scope the prompt builder assembles for identical inputs.
//
// Fail-open contract matching checkPlanBudget: any read failure, an
// absent workflow spec, or a run with no plan artifact returns ok=false
// and the caller skips its check (WARN log, never blocks). maxFiles is
// the resolved cap; 0 means no cap is configured (callers treat that as
// nothing to enforce).
func (s *Server) effectiveScopeHeadroom(ctx context.Context, runID uuid.UUID, extraPaths []string) (effectiveCount, maxFiles int, ok bool) {
	if s.cfg.RunRepo == nil || s.cfg.ArtifactRepo == nil {
		return 0, 0, false
	}

	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "scope headroom: get run failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return 0, 0, false
	}

	constraints, _, resolved := s.resolveImplementConstraints(ctx, runRow)
	if !resolved {
		return 0, 0, false
	}

	approvedPlan, err := s.loadApprovedPlanForRun(ctx, runID)
	if err != nil || approvedPlan == nil {
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "scope headroom: load plan failed",
				slog.String("run_id", runID.String()),
				slog.String("error", err.Error()),
			)
		}
		return 0, 0, false
	}

	seen := make(map[string]struct{}, len(approvedPlan.Scope.Files)+len(extraPaths))
	for _, f := range approvedPlan.Scope.Files {
		seen[f.Path] = struct{}{}
	}
	for _, p := range s.loadApprovalAddScopeFiles(ctx, runID) {
		seen[p] = struct{}{}
	}

	if s.cfg.ScopeAmendmentRepo != nil {
		items, err := s.cfg.ScopeAmendmentRepo.ListByRun(ctx, runID)
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "scope headroom: list scope amendments failed",
				slog.String("run_id", runID.String()),
				slog.String("error", err.Error()),
			)
			return 0, 0, false
		}
		for _, a := range items {
			if a.Status != scopeamendment.StatusApproved {
				continue
			}
			for _, p := range a.Paths {
				seen[p.Path] = struct{}{}
			}
		}
	}

	for _, p := range extraPaths {
		seen[p] = struct{}{}
	}

	return len(seen), constraints.MaxFilesChanged, true
}
