package server

import (
	"context"
	"fmt"
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
// every child's APPROVED amendment with its operation preserved. The two
// plan/approval channels come from decomposedParentScopeChannels — the SAME
// resolver the parent's implement-review baseline reads
// (parentFanOutScopeForReview, #4425) — so serve and review cannot disagree on
// them. The gate-time remove_scope_files is then subtracted again, because the
// top-level and child-amendment entries can re-introduce a path the operator
// removed and a removal must win here exactly as it does in each child's slice
// scope.
//
// Slice grants fold with Operation=modify, and that is correct even for a path
// the slice CREATED: on the consolidated branch the slice's commit has already
// made the path tracked, so the runner's #818 untracked-path (created-out-of-
// scope) gate does not apply to it, and drift exclusion is decided by path
// membership in cfg.scopeFiles alone, never by the declared operation.
//
// It is gated (inside decomposedParentScopeChannels) on isDecomposedParentRun,
// the same predicate behind fixupBranchForRun's consolidated branch and the
// fixup_policy_increment flag (#4391), so the widened scope is served only on
// the pass that checks out the consolidated branch and measures
// max_files_changed on its own increment. A children-probe error reads false
// and serves today's narrower scope (fail-toward-today). An empty scope or a
// missing plan stays as it is, so a plan_missing_for_implement fix-up keeps the
// runner's git add -A fallback.
func (s *Server) decomposedParentFixupScope(ctx context.Context, runRow *run.Run, approvedPlan *plan.Plan, scopeFiles []scopeFile, removed []string) []scopeFile {
	if len(scopeFiles) == 0 {
		return scopeFiles
	}
	ch, ok := s.decomposedParentScopeChannels(ctx, runRow, approvedPlan, removed)
	if !ok {
		return scopeFiles
	}
	scoped := s.foldScopeEntries(ctx, scopeFiles, ch.subPlanEntries(), "parent-sub-plan-scope")
	scoped = s.foldScopePaths(ctx, scoped, ch.sliceGrantPaths(), "parent-slice-add-scope-files")
	scoped = s.foldScopeEntries(ctx, scoped, s.childApprovedAmendmentEntries(ctx, runRow.ID), "parent-child-scope-amendment")
	return s.subtractScopePaths(ctx, scoped, removed, "parent-approval-remove-scope-files")
}

// parentSubPlanEntry is one entry of the plan's FULL declared scope with the
// position the first-seen walk took it from: subPlanIndex is the first
// decomposition sub_plan index declaring the path, or -1 when the plan's
// top-level scope.files declares it (top-level entries come first and win).
type parentSubPlanEntry struct {
	entry        scopeamendment.PathEntry
	subPlanIndex int
}

// parentSliceGrant is one add_scope_files_to_slice path with the canonical
// slice key (strconv.Itoa of the slice index) whose grant first named it.
type parentSliceGrant struct {
	path, sliceKey string
}

// parentScopeChannels is a decomposed parent's two plan/approval-derived
// fan-out channels: every sub_plan's declared scope (subPlan, which also
// carries the top-level entries so the walk has one first-seen order) and
// every slice's add_scope_files_to_slice grant (sliceGrants). The
// child-amendment channel is not here: it keeps its own shared walk
// (childApprovedAmendmentRecords), read by serve and review alike.
type parentScopeChannels struct {
	subPlan     []parentSubPlanEntry
	sliceGrants []parentSliceGrant
}

// subPlanEntries projects the sub_plan channel onto fold entries, in walk order.
func (c parentScopeChannels) subPlanEntries() []scopeamendment.PathEntry {
	if len(c.subPlan) == 0 {
		return nil
	}
	out := make([]scopeamendment.PathEntry, 0, len(c.subPlan))
	for _, e := range c.subPlan {
		out = append(out, e.entry)
	}
	return out
}

// sliceGrantPaths projects the slice-grant channel onto its paths, in the
// channel's ascending slice-index order.
func (c parentScopeChannels) sliceGrantPaths() []string {
	if len(c.sliceGrants) == 0 {
		return nil
	}
	out := make([]string, 0, len(c.sliceGrants))
	for _, g := range c.sliceGrants {
		out = append(out, g.path)
	}
	return out
}

// without drops every removed path from both channels (compare-by-path, the
// subtractScopePaths semantics), so a gate-time remove_scope_files wins over
// the fan-out on every reader of the channels.
func (c parentScopeChannels) without(removed []string) parentScopeChannels {
	if len(removed) == 0 {
		return c
	}
	drop := make(map[string]struct{}, len(removed))
	for _, p := range removed {
		drop[p] = struct{}{}
	}
	var out parentScopeChannels
	for _, e := range c.subPlan {
		if _, ok := drop[e.entry.Path]; !ok {
			out.subPlan = append(out.subPlan, e)
		}
	}
	for _, g := range c.sliceGrants {
		if _, ok := drop[g.path]; !ok {
			out.sliceGrants = append(out.sliceGrants, g)
		}
	}
	return out
}

// decomposedParentScopeChannels is the ONE resolver of a decomposed parent's
// plan/approval-derived fan-out scope (#4425): the plan's full declared scope
// with each entry's first declaring sub_plan, and every slice's
// add_scope_files_to_slice grant, with the gate-time remove_scope_files
// subtracted from both. The parent fix-up serve (decomposedParentFixupScope)
// and the parent implement-review baseline (parentFanOutScopeForReview) both
// read it, so the two cannot disagree on what the fan-out authorized.
//
// It returns false — and the zero value — unless the run is a decomposed
// PARENT (isDecomposedParentRun: not itself a child, WITH minted children) and
// an approved plan is in hand. A children-probe error reads false, so every
// reader falls back to today's narrower scope (fail-toward-today).
func (s *Server) decomposedParentScopeChannels(ctx context.Context, runRow *run.Run, approvedPlan *plan.Plan, removed []string) (parentScopeChannels, bool) {
	if approvedPlan == nil || !s.isDecomposedParentRun(ctx, runRow) {
		return parentScopeChannels{}, false
	}
	ch := parentScopeChannels{
		subPlan:     subPlanScopeChannel(approvedPlan),
		sliceGrants: s.parentSliceGrantRecords(ctx, runRow.ID),
	}
	ch = ch.without(removed)
	return ch, true
}

// subPlanScopeChannel is the indexed form of scopeFilesFromApprovedPlanFull: it
// applies the IDENTICAL first-seen rule (the top-level p.Scope.Files first,
// then every p.Decomposition.SubPlans[i].Scope.Files in index order, deduped by
// Path, first-seen operation wins, operations preserved verbatim) and records,
// in the same walk, which sub_plan index first declared each path (-1 for a
// top-level entry). The fold entries and the review Detail's sub_plan index
// therefore come from ONE walk; TestSubPlanScopeChannel_MatchesFullPlanWalk
// pins it to scopeFilesFromApprovedPlanFull. Nil when the plan declares no
// files.
func subPlanScopeChannel(p *plan.Plan) []parentSubPlanEntry {
	if p == nil {
		return nil
	}
	var out []parentSubPlanEntry
	seen := make(map[string]struct{})
	add := func(files []scopeFile, idx int) {
		for _, f := range files {
			if _, ok := seen[f.Path]; ok {
				continue
			}
			seen[f.Path] = struct{}{}
			out = append(out, parentSubPlanEntry{
				entry:        scopeamendment.PathEntry{Path: f.Path, Operation: f.Operation},
				subPlanIndex: idx,
			})
		}
	}
	add(scopeFilesFromScope(&p.Scope), -1)
	if p.Decomposition != nil {
		for i := range p.Decomposition.SubPlans {
			add(scopeFilesFromScope(p.Decomposition.SubPlans[i].Scope), i)
		}
	}
	return out
}

// subPlanScopeEntries converts a plan's FULL declared scope (subPlanScopeChannel,
// the indexed form of scopeFilesFromApprovedPlanFull: top-level unioned with
// every sub_plan, operations preserved, first-seen wins) into fold entries for
// foldScopeEntries. Nil when the plan declares no files.
func subPlanScopeEntries(p *plan.Plan) []scopeamendment.PathEntry {
	return parentScopeChannels{subPlan: subPlanScopeChannel(p)}.subPlanEntries()
}

// parentSliceGrantPaths returns EVERY slice's add_scope_files_to_slice paths
// recorded on the parent's plan-stage approval, flattened and deduped: the
// path projection of parentSliceGrantRecords, with its ordering and dedup.
func (s *Server) parentSliceGrantPaths(ctx context.Context, parentRunID uuid.UUID) []string {
	return parentScopeChannels{sliceGrants: s.parentSliceGrantRecords(ctx, parentRunID)}.sliceGrantPaths()
}

// parentSliceGrantRecords returns EVERY slice's add_scope_files_to_slice paths
// recorded on the parent's plan-stage approval (loadApprovalSliceAddScopeFiles,
// behind its #2598 approvalEntryStageIsPlan wall), flattened and deduped, each
// with the slice key whose grant first named it. Unlike
// resolveApprovalSliceAddScopeFiles, which selects ONE slice's key to keep
// single-owner-file across children, the parent reads the whole map: the
// parent is the integrating branch every slice's grant already landed on.
//
// The order is byte-stable and never follows Go map order: canonical keys
// (strconv.Itoa of a slice index) sort in ascending numeric order, and any
// non-numeric key sorts after them lexicographically, defensively. A path
// named by two slices is kept once, under the first key in that order.
func (s *Server) parentSliceGrantRecords(ctx context.Context, parentRunID uuid.UUID) []parentSliceGrant {
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
	var out []parentSliceGrant
	seen := make(map[string]struct{})
	for _, k := range keys {
		for _, p := range m[k] {
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, parentSliceGrant{path: p, sliceKey: k})
		}
	}
	return out
}

// parentFanOutScopePath is one path a decomposed parent's fan-out authorized,
// projected for the parent's implement-review baseline (#4425): its declared
// Operation, the fold Source it is labelled with in the scope-provenance block
// (the same source names the fix-up serve logs) and a per-path Detail naming
// the exact sub_plan or slice grant.
type parentFanOutScopePath struct {
	Path, Operation, Source, Detail string
}

// parentFanOutScopeForReview projects decomposedParentScopeChannels for the
// parent's implement review (#4425). A parent review — the first consolidated
// round and every fix-up re-review — reads a diff carrying every slice's
// commit, but its baseline was only the plan's top-level scope.files plus the
// flat approval adds, so an edit to a sub_plan-only path or a slice-granted
// path (both admitted by the #4338 fix-up serve) read as scope drift and as an
// unexplained provenance residual.
//
// Sub_plan entries come first (Source parent-sub-plan-scope, Operation
// preserved, Detail naming the first sub_plan index declaring the path — taken
// from the channel's own walk), then slice grants (Source
// parent-slice-add-scope-files, Operation modify, Detail naming the slice).
// Skipped: a path in the raw top-level scope.files (writePlanForReview already
// renders it) and a path in the flat approval add_scope_files (the serve folds
// that channel as modify BEFORE the parent union, so it keeps that label and
// operation — a sub_plan delete must not shadow a flat modify, or the
// reconstruction would drop the coupled *_test.go sibling the serve grants).
// Deduped first-wins. The gate-time remove_scope_files is subtracted inside the
// resolver.
//
// Nil when the run is not a decomposed parent (including on a children-probe
// error — today's narrower baseline), when the plan is missing, or when the
// plan's top-level scope.files is empty (the serve never widens an empty
// scope, so neither does the baseline).
func (s *Server) parentFanOutScopeForReview(ctx context.Context, runRow *run.Run, approvedPlan *plan.Plan) []parentFanOutScopePath {
	if approvedPlan == nil || len(approvedPlan.Scope.Files) == 0 {
		return nil
	}
	removed := s.resolveApprovalRemoveScopeFiles(ctx, runRow)
	ch, ok := s.decomposedParentScopeChannels(ctx, runRow, approvedPlan, removed)
	if !ok {
		return nil
	}
	skip := make(map[string]struct{}, len(approvedPlan.Scope.Files))
	for _, f := range approvedPlan.Scope.Files {
		skip[f.Path] = struct{}{}
	}
	for _, p := range s.resolveApprovalAddScopeFiles(ctx, runRow) {
		skip[p] = struct{}{}
	}
	var out []parentFanOutScopePath
	for _, e := range ch.subPlan {
		// A top-level entry (subPlanIndex -1) is always in skip.
		if _, dup := skip[e.entry.Path]; dup {
			continue
		}
		skip[e.entry.Path] = struct{}{}
		out = append(out, parentFanOutScopePath{
			Path:      e.entry.Path,
			Operation: e.entry.Operation,
			Source:    "parent-sub-plan-scope",
			Detail:    fmt.Sprintf("declared in decomposition sub_plan %d scope.files", e.subPlanIndex),
		})
	}
	for _, g := range ch.sliceGrants {
		if _, dup := skip[g.path]; dup {
			continue
		}
		skip[g.path] = struct{}{}
		out = append(out, parentFanOutScopePath{
			Path:      g.path,
			Operation: scopeamendment.OperationModify,
			Source:    "parent-slice-add-scope-files",
			Detail:    "granted to slice " + g.sliceKey + " at plan approval via add_scope_files_to_slice",
		})
	}
	return out
}

// appendFanOutPaths returns base followed by each fan-out Path not already in
// base, in fan-out order. Nil when both are empty, so an ordinary run's review
// trigger stays byte-identical.
func appendFanOutPaths(base []string, fanOut []parentFanOutScopePath) []string {
	if len(fanOut) == 0 {
		return base
	}
	out := append([]string(nil), base...)
	seen := make(map[string]struct{}, len(base)+len(fanOut))
	for _, p := range base {
		seen[p] = struct{}{}
	}
	for _, e := range fanOut {
		if _, ok := seen[e.Path]; ok {
			continue
		}
		seen[e.Path] = struct{}{}
		out = append(out, e.Path)
	}
	return out
}
