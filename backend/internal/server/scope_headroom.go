package server

import (
	"context"
	"log/slog"
	"sort"

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
	paths, maxFiles, ok := s.effectiveScopePathSet(ctx, runID, extraPaths, nil)
	if !ok {
		return 0, 0, false
	}
	return len(paths), maxFiles, true
}

// effectiveScopePathSet is the single source of truth for the run's effective
// implement-scope file set (#1726). It builds the deduped path set from the
// newest plan artifact's scope.files ∪ approval add_scope_files ∪ approved
// scope-amendment paths ∪ extraAdd, then SUBTRACTS removePaths (compare-by-
// Path), and returns the sorted deduped result. Both the scope-cap gate (via
// effectiveScopeHeadroom, which counts this set) and the plan-gate removal
// validations (which need the before/after lists) share it, so the count the
// gate computes equals the effective scope the prompt builder assembles for
// identical inputs.
//
// removePaths not present in the set are a no-op here — presence is validated
// separately by the plan-stage approve gate before this is trusted. maxFiles
// is the resolved cap; 0 means no cap configured. Fail-open contract matching
// effectiveScopeHeadroom: any read failure, an absent spec, or a run with no
// plan artifact returns ok=false.
func (s *Server) effectiveScopePathSet(ctx context.Context, runID uuid.UUID, extraAdd, removePaths []string) (paths []string, maxFiles int, ok bool) {
	if s.cfg.RunRepo == nil || s.cfg.ArtifactRepo == nil {
		return nil, 0, false
	}

	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "scope headroom: get run failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil, 0, false
	}

	constraints, _, resolved := s.resolveImplementConstraints(ctx, runRow)
	if !resolved {
		return nil, 0, false
	}

	approvedPlan, err := s.loadApprovedPlanForRun(ctx, runID)
	if err != nil || approvedPlan == nil {
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "scope headroom: load plan failed",
				slog.String("run_id", runID.String()),
				slog.String("error", err.Error()),
			)
		}
		return nil, 0, false
	}

	seen := make(map[string]struct{}, len(approvedPlan.Scope.Files)+len(extraAdd))
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
			return nil, 0, false
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

	for _, p := range extraAdd {
		seen[p] = struct{}{}
	}

	// Subtract the removals last so the returned set (and the count the cap
	// gate reads from it) already reflects a gate-time removal (#1726).
	for _, p := range removePaths {
		delete(seen, p)
	}

	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, constraints.MaxFilesChanged, true
}
