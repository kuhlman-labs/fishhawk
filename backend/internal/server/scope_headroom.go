package server

import (
	"context"
	"log/slog"
	"sort"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
)

// minPhysicalFileCount estimates the MINIMUM number of physical changed
// files a declared scope could produce at the post-implement
// max_files_changed gate — the number the hard gate actually counts (#2415).
// It is deliberately GENEROUS (a lower bound on the physical count) so a
// categorical refusal built on it can never false-fire on a plan whose real
// diff would fit: refuse only when even this most-optimistic reading is over
// cap.
//
// The estimate exempts generated/vendored paths exactly as
// policy.CountedFileCount does (the post-implement gate uses that helper), then
// buckets the remainder by declared operation and returns
//
//	others + max(creates, deletes)
//
// This is minimal because git rename detection emits ONE `R` row per detected
// rename in `git diff --name-status`, collapsing a delete+create PAIR into a
// single physical file. At most min(creates, deletes) declared delete+create
// pairs can collapse, so physical = others + creates + deletes - k is minimized
// at k = min(creates, deletes), giving others + max(creates, deletes).
//
// HONEST LIMIT (binding, #2415): the bound is minimal only over DECLARED
// operations. A path with no declared operation (an add_scope_files or
// amendment path, or any entry the caller left unset) is treated as a
// non-pairing modify — it counts one and pairs with nothing. So an UNDECLARED
// create that git would pair with a declared delete as a rename makes the true
// physical count LOWER than this estimate returns. The schema has no rename
// operation, so a plan cannot declare a pairing the estimator would credit; the
// refusal this feeds is therefore honest about being conservative — it can only
// over-estimate the physical count, never under-estimate it, so it never admits
// an over-cap landing but MAY refuse a rename-heavy plan whose real diff would
// fit. remove_scope_files at the gate is the in-loop escape.
func minPhysicalFileCount(paths []string, ops map[string]plan.FileOperation) int {
	creates, deletes, others := 0, 0, 0
	for _, p := range paths {
		if policy.IsGeneratedPath(p) {
			continue
		}
		switch ops[p] {
		case plan.FileOpCreate:
			creates++
		case plan.FileOpDelete:
			deletes++
		default:
			// modify, or a path with no declared operation: a non-pairing
			// physical file (see the HONEST LIMIT note above).
			others++
		}
	}
	return others + max(creates, deletes)
}

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
	paths, _, maxFiles, ok = s.effectiveScopePathSetWithOps(ctx, runID, extraAdd, removePaths)
	return paths, maxFiles, ok
}

// effectiveScopePathSetWithOps holds the full body of effectiveScopePathSet and
// ALSO returns an ops map: the declared plan.FileOperation for every surviving
// path that the approved plan's scope.Files declares (#2415). The scope-cap
// gate needs it to feed minPhysicalFileCount; effectiveScopePathSet is a thin
// wrapper that drops ops so its other callers are unchanged.
//
// OPS AVAILABILITY (binding, #2415): ops is derived from the SAME approved plan
// object the path set is built from, so whenever paths resolve (ok=true) ops is
// non-nil and populated for every plan-declared surviving path — there is NO
// distinct "ops unavailable" state to represent or fail open on. Paths that the
// plan does not declare (add_scope_files, approved amendments, extraAdd) simply
// carry no ops entry; minPhysicalFileCount reads a missing entry as a
// non-pairing modify (the honest-limit case). The single ok flag therefore
// governs the whole resolution: ok=false is the one fail-open leg (nil repos,
// GetRun error, unresolved spec, missing plan, amendment-list error).
func (s *Server) effectiveScopePathSetWithOps(ctx context.Context, runID uuid.UUID, extraAdd, removePaths []string) (paths []string, ops map[string]plan.FileOperation, maxFiles int, ok bool) {
	if s.cfg.RunRepo == nil || s.cfg.ArtifactRepo == nil {
		return nil, nil, 0, false
	}

	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "scope headroom: get run failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil, nil, 0, false
	}

	constraints, _, resolved := s.resolveImplementConstraints(ctx, runRow)
	if !resolved {
		return nil, nil, 0, false
	}

	approvedPlan, err := s.loadApprovedPlanForRun(ctx, runID)
	if err != nil || approvedPlan == nil {
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "scope headroom: load plan failed",
				slog.String("run_id", runID.String()),
				slog.String("error", err.Error()),
			)
		}
		return nil, nil, 0, false
	}

	seen := make(map[string]struct{}, len(approvedPlan.Scope.Files)+len(extraAdd))
	for _, f := range approvedPlan.Scope.Files {
		seen[f.Path] = struct{}{}
	}
	for _, p := range s.loadApprovalAddScopeFiles(ctx, runID) {
		seen[p] = struct{}{}
	}
	// A PRIOR approval's per-slice adds (#2515) count in the effective set too:
	// the prompt builder folds each slice's entry into that child's scope, so
	// omitting them here would make the number the cap gate reports smaller than
	// the scope actually assembled. Additive — a run with no per-slice map
	// contributes nothing and the set stays byte-identical to today. Map
	// iteration order is irrelevant: this is a set, and the result is sorted.
	for _, paths := range s.loadApprovalSliceAddScopeFiles(ctx, runID) {
		for _, p := range paths {
			seen[p] = struct{}{}
		}
	}

	if s.cfg.ScopeAmendmentRepo != nil {
		items, err := s.cfg.ScopeAmendmentRepo.ListByRun(ctx, runID)
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "scope headroom: list scope amendments failed",
				slog.String("run_id", runID.String()),
				slog.String("error", err.Error()),
			)
			return nil, nil, 0, false
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

	// Build ops from the plan's declared operations, restricted to the
	// surviving set (a removed plan path is absent from seen, so it is left
	// out). Keys are a subset of out; unmatched paths (adds/amendments) carry
	// no entry and read as a non-pairing modify in minPhysicalFileCount.
	ops = make(map[string]plan.FileOperation, len(approvedPlan.Scope.Files))
	for _, f := range approvedPlan.Scope.Files {
		if _, present := seen[f.Path]; present {
			ops[f.Path] = f.Operation
		}
	}
	return out, ops, constraints.MaxFilesChanged, true
}
