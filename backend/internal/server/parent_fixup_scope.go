package server

import (
	"context"
	"sort"
	"strconv"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
)

// decomposedParentFixupScope widens a decomposed PARENT's fix-up scope_files to
// everything its fan-out was authorized to touch (#4338). A parent fix-up runs
// on the consolidated branch, whose tree carries every slice's commit, but the
// parent's own scope sources are only its plan's TOP-LEVEL scope.files, its own
// flat add_scope_files and its own stage's amendments. A path declared only in
// a sub_plan, granted to a slice at approval (add_scope_files_to_slice), or
// approved on a CHILD's mid-stage amendment is invisible to them, so a fix-up
// editing it was drift-excluded from the commit and failed category B (run
// af08d34e / #4250). The runner writes cfg.scopeFiles solely from the served
// scope_files (plus its own-run amendment refresh, which never sees a child's
// amendments), so the union has to be served from here.
//
// The union is: the plan's full declared scope (top-level plus every sub_plan,
// operations preserved), every slice's add_scope_files_to_slice paths, and
// every child's APPROVED amendment with its operation preserved. The gate-time
// remove_scope_files is then subtracted again, because the sub_plan union can
// re-introduce a path the operator removed and a removal must win here exactly
// as it does in each child's slice scope.
//
// Slice grants fold with Operation=modify, and that is correct even for a path
// the slice CREATED: on the consolidated branch the slice's commit has already
// made the path tracked, so the runner's #818 untracked-path (created-out-of-
// scope) gate does not apply to it, and drift exclusion is decided by path
// membership in cfg.scopeFiles alone, never by the declared operation.
//
// It is gated on isDecomposedParentRun, the same predicate behind
// fixupBranchForRun's consolidated branch and the fixup_policy_increment flag
// (#4391), so the widened scope is served only on the pass that checks out the
// consolidated branch and measures max_files_changed on its own increment. A
// children-probe error reads false and serves today's narrower scope
// (fail-toward-today). An empty scope or a missing plan stays as it is, so a
// plan_missing_for_implement fix-up keeps the runner's git add -A fallback.
func (s *Server) decomposedParentFixupScope(ctx context.Context, runRow *run.Run, approvedPlan *plan.Plan, scopeFiles []scopeFile, removed []string) []scopeFile {
	if !s.isDecomposedParentRun(ctx, runRow) {
		return scopeFiles
	}
	if len(scopeFiles) == 0 || approvedPlan == nil {
		return scopeFiles
	}
	scoped := s.foldScopeEntries(ctx, scopeFiles, subPlanScopeEntries(approvedPlan), "parent-sub-plan-scope")
	scoped = s.foldScopePaths(ctx, scoped, s.parentSliceGrantPaths(ctx, runRow.ID), "parent-slice-add-scope-files")
	scoped = s.foldScopeEntries(ctx, scoped, s.childApprovedAmendmentEntries(ctx, runRow.ID), "parent-child-scope-amendment")
	return s.subtractScopePaths(ctx, scoped, removed, "parent-approval-remove-scope-files")
}

// subPlanScopeEntries converts a plan's FULL declared scope
// (scopeFilesFromApprovedPlanFull: top-level unioned with every sub_plan,
// operations preserved, first-seen wins) into fold entries for
// foldScopeEntries. Nil when the plan declares no files.
func subPlanScopeEntries(p *plan.Plan) []scopeamendment.PathEntry {
	full := scopeFilesFromApprovedPlanFull(p)
	if len(full) == 0 {
		return nil
	}
	out := make([]scopeamendment.PathEntry, 0, len(full))
	for _, f := range full {
		out = append(out, scopeamendment.PathEntry{Path: f.Path, Operation: f.Operation})
	}
	return out
}

// parentSliceGrantPaths returns EVERY slice's add_scope_files_to_slice paths
// recorded on the parent's plan-stage approval (loadApprovalSliceAddScopeFiles,
// behind its #2598 approvalEntryStageIsPlan wall), flattened and deduped.
// Unlike resolveApprovalSliceAddScopeFiles, which selects ONE slice's key to
// keep single-owner-file across children, the parent reads the whole map: the
// parent is the integrating branch every slice's grant already landed on.
//
// The order is byte-stable and never follows Go map order: canonical keys
// (strconv.Itoa of a slice index) sort in ascending numeric order, and any
// non-numeric key sorts after them lexicographically, defensively.
func (s *Server) parentSliceGrantPaths(ctx context.Context, parentRunID uuid.UUID) []string {
	m := s.loadApprovalSliceAddScopeFiles(ctx, parentRunID)
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ai, aerr := strconv.Atoi(keys[i])
		bi, berr := strconv.Atoi(keys[j])
		switch {
		case aerr == nil && berr == nil && ai != bi:
			return ai < bi
		case aerr == nil && berr == nil:
			// A non-canonical spelling of the same index ("07" beside "7").
		case aerr == nil:
			return true // numeric keys sort before non-numeric ones
		case berr == nil:
			return false
		}
		return keys[i] < keys[j]
	})
	var out []string
	seen := make(map[string]struct{})
	for _, k := range keys {
		for _, p := range m[k] {
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	return out
}
