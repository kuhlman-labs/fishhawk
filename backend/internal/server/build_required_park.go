package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryParentAwaitingChildScopeDecision is written on the PARENT run of a
// decomposition when one of its children parks in awaiting_scope_decision for a
// build-required scope-drift shortfall (#2548).
//
// It exists because a parked child is NON-TERMINAL: awaiting_scope_decision is
// settled (so the SLA reaper does not sweep it) but not terminal, so the child
// run never completes and orchestrator.maybeAdvanceDecomposedParent — which is
// reached ONLY from completeRun on a child's terminal transition — leaves the
// parent in awaiting_children. That wait is CORRECT (resolving the parent to
// failed-C while a live child holds an undecided park would terminate the run
// out from under the very decision the park exists to offer, the #698/#1081
// recoverable-park shape) but it must not be SILENT. This entry is what makes
// it discoverable and actionable.
//
// It is emitted from BOTH triggers:
//
//   - at PARK time (server/pullrequest.go::parkScopeCompletenessStage), which
//     is the only cover for the case where the parked child is the LAST
//     non-terminal sibling and no further terminal transition ever fires
//     maybeAdvanceDecomposedParent again; and
//   - from maybeAdvanceDecomposedParent when a SIBLING settles, so an operator
//     driving the fan-out sees it on the parent's chain at the point they are
//     reading it.
//
// At most one entry exists per (parent run, child stage), enforced at the store
// layer by migration 0067's partial unique index — not by a best-effort read in
// one emitter. Both emitters funnel through AppendChained's per-run
// SELECT ... FOR UPDATE, so the race-loser's insert deterministically hits the
// index and is treated as the benign already-recorded outcome. (Caveat: the key
// has no episode component, so a re-park within the same run — #2591's proposed
// amend-and-resume — would be silently suppressed until the key is widened.)
//
// Payload: {parent_stage_id, child_run_id, child_stage_id, child_slice_index,
// shortfall_class, build_required_paths, owning_slices}. The attribution
// (owning_slices) is the entire operator value — an entry saying only "a child
// is parked" sends the operator back to reading plans by hand.
//
// Like every scope-completeness kind this is an INTERNAL audit kind, NOT an
// issue-comment surface — see docs/issue-comment-surfaces.md.
const CategoryParentAwaitingChildScopeDecision = "parent_awaiting_child_scope_decision"

// ShortfallClassBuildRequired is the shortfall_class discriminator carried on
// the parent-side signal. WIRE VALUE: the orchestrator emits the byte-identical
// string from its own copy (backend/internal/orchestrator/orchestrator.go), so
// an operator filtering the parent chain sees one value regardless of which
// trigger produced the entry.
const ShortfallClassBuildRequired = "build_required"

// attributeBuildRequiredPaths maps each build-required path to the SIBLING
// sub-plan that declares it in the parent's approved decomposition
// (decomposition.sub_plans[i].scope.files — the same authority
// resolveSliceDependencies reads for depends_on).
//
// It inherits resolveSliceDependencies' ABSENT-vs-ERRORED partition, with one
// deliberate difference: here even an ERRORED plan load degrades to an
// unattributed result rather than failing. Attribution is DIAGNOSTIC; it must
// never cost the operator the preserved commit, and the park must be recorded
// either way. Every degrade returns one unattributed owner per path (never a
// nil slice for a non-empty input), so the payload always names the paths.
//
// Degrades to unattributed: the run is not a fan-out child, the parent plan
// resolves nil, the plan carries no decomposition, a plan LOAD error, and any
// path no sub-plan claims.
func (s *Server) attributeBuildRequiredPaths(ctx context.Context, childRun *run.Run, paths []string) []run.BuildRequiredOwner {
	if len(paths) == 0 {
		return nil
	}
	owners := make([]run.BuildRequiredOwner, 0, len(paths))
	for _, p := range paths {
		owners = append(owners, run.BuildRequiredOwner{Path: p})
	}
	if childRun == nil || childRun.DecomposedFrom == nil {
		return owners
	}
	p, err := s.loadApprovedPlanForRun(ctx, *childRun.DecomposedFrom)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"build-required park: parent plan load failed; recording an unattributed park",
			slog.String("child_run_id", childRun.ID.String()),
			slog.String("parent_run_id", childRun.DecomposedFrom.String()),
			slog.String("error", err.Error()))
		return owners
	}
	if p == nil || p.Decomposition == nil {
		return owners
	}
	// Index declared path → owning sub-plan. A child never owns its own
	// build-required drift (the path was EXCLUDED from its commit precisely
	// because it is out of that slice's scope), so no self-filter is needed;
	// first declarer wins if a plan somehow double-declares.
	type owner struct {
		index int
		title string
	}
	byPath := make(map[string]owner)
	for i, sp := range p.Decomposition.SubPlans {
		if sp.Scope == nil {
			continue
		}
		for _, f := range sp.Scope.Files {
			if _, seen := byPath[f.Path]; seen {
				continue
			}
			byPath[f.Path] = owner{index: i, title: sp.Title}
		}
	}
	for i := range owners {
		o, ok := byPath[owners[i].Path]
		if !ok {
			continue
		}
		idx := o.index
		owners[i].SliceIndex = &idx
		owners[i].SliceTitle = o.title
	}
	return owners
}

// emitParentAwaitingChildScopeDecision appends the
// parent_awaiting_child_scope_decision entry on the PARENT run of a
// build-required-parked child. It is the park-time half of the dual emission
// described on CategoryParentAwaitingChildScopeDecision.
//
// No-op (silently) when the run is not a decomposed child or the park carries
// no build-required shortfall. Best-effort WARN-on-error otherwise, matching
// every other emitter in this area (orchestrator.emitParentAwaitingRedrive and
// siblings): a failed append must never unwind the park itself, which is the
// thing preserving the slice's work.
func (s *Server) emitParentAwaitingChildScopeDecision(ctx context.Context, childRun *run.Run,
	childStageID uuid.UUID, park run.ScopeCompletenessPark) {
	if childRun == nil || childRun.DecomposedFrom == nil || len(park.BuildRequiredPaths) == 0 {
		return
	}
	if s.cfg.AuditRepo == nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"build-required park: audit repository unconfigured; skipping "+CategoryParentAwaitingChildScopeDecision,
			slog.String("child_run_id", childRun.ID.String()))
		return
	}
	parentRunID := *childRun.DecomposedFrom
	parentStage, err := s.resolveAwaitingChildrenStage(ctx, parentRunID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"build-required park: list parent stages failed; skipping "+CategoryParentAwaitingChildScopeDecision,
			slog.String("child_run_id", childRun.ID.String()),
			slog.String("parent_run_id", parentRunID.String()),
			slog.String("error", err.Error()))
		return
	}

	payloadMap := map[string]any{
		"child_run_id":         childRun.ID.String(),
		"child_stage_id":       childStageID.String(),
		"shortfall_class":      ShortfallClassBuildRequired,
		"build_required_paths": park.BuildRequiredPaths,
		"owning_slices":        park.OwningSlices,
	}
	var stageID *uuid.UUID
	if parentStage != nil {
		payloadMap["parent_stage_id"] = parentStage.ID.String()
		id := parentStage.ID
		stageID = &id
	}
	if childRun.SliceIndex != nil {
		payloadMap["child_slice_index"] = *childRun.SliceIndex
	}
	payload, err := json.Marshal(payloadMap)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"build-required park: marshal "+CategoryParentAwaitingChildScopeDecision+" payload failed",
			slog.String("child_run_id", childRun.ID.String()),
			slog.String("error", err.Error()))
		return
	}
	systemKind := audit.ActorSystem
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     parentRunID,
		StageID:   stageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryParentAwaitingChildScopeDecision,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		// The 0067 partial unique index (run_id, child_stage_id) makes the
		// at-most-one property TRUE across BOTH emitters: a sibling-settle
		// emitter (or another park-time emitter) that already recorded this
		// child stage makes our insert the deterministic race-loser, which
		// surfaces as a unique_violation on that index. That is the benign
		// already-recorded outcome — log INFO and return, NOT the WARN below.
		// Every other append error keeps the WARN. The park itself is untouched
		// on both paths — a failed or superseded append must never unwind the
		// thing preserving the slice's work.
		if audit.IsParentAwaitingChildScopeDecisionDuplicate(err) {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
				"parent already carries the awaiting-child-scope-decision signal for this child stage",
				slog.String("parent_run_id", parentRunID.String()),
				slog.String("child_run_id", childRun.ID.String()),
				slog.String("child_stage_id", childStageID.String()))
			return
		}
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"build-required park: append "+CategoryParentAwaitingChildScopeDecision+" failed",
			slog.String("child_run_id", childRun.ID.String()),
			slog.String("parent_run_id", parentRunID.String()),
			slog.String("error", err.Error()))
		return
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
		"decomposition parent awaiting a child's scope decision",
		slog.String("parent_run_id", parentRunID.String()),
		slog.String("child_run_id", childRun.ID.String()),
		slog.String("child_stage_id", childStageID.String()))
}

// resolveAwaitingChildrenStage returns the parent's awaiting_children stage, or
// nil when it has none (the parent may not have reached it yet). A nil stage is
// NOT an error: the signal is still appended, run-scoped without a stage id,
// rather than dropped — the operator must learn about the parked child either
// way.
func (s *Server) resolveAwaitingChildrenStage(ctx context.Context, parentRunID uuid.UUID) (*run.Stage, error) {
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, parentRunID)
	if err != nil {
		return nil, err
	}
	for _, st := range stages {
		if st.State == run.StageStateAwaitingChildren {
			return st, nil
		}
	}
	return nil, nil
}
