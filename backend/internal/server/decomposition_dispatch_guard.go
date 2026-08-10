package server

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// decompositionSiblingPageSize bounds each ListRuns page in the sibling walk.
// postgresRepo.ListRuns REJECTS Limit <= 0, so an explicit page size is
// mandatory (not stylistic) — a single unpaged zero-value filter would error in
// production while returning an empty slice through an in-memory test fake, i.e.
// fail DIFFERENTLY in test than in production. Mirrors
// listAllDecomposedChildren's convention (consolidate.go).
const decompositionSiblingPageSize = 100

// sliceDependencyError is the wave-order refusal a decomposed child's
// host-dispatch produces when a declared dependency slice has not yet reached
// run state 'succeeded'. It carries the coordinates the 409
// dependency_not_satisfied body surfaces so an operator can name and dispatch
// the blocking sibling first. BlockingRunID is empty and BlockingState is
// "not_minted" when the dependency slice has no minted sibling at all.
type sliceDependencyError struct {
	sliceIndex         int
	blockingSliceIndex int
	blockingRunID      string
	blockingState      string
}

// notMintedState is the synthetic blocking_state for a dependency slice with no
// minted sibling run — distinct from any real run.State so an operator (and the
// tests) can tell "sibling exists but is not succeeded" from "sibling was never
// minted".
const notMintedState = "not_minted"

// message renders the operator-facing refusal string in the campaign
// precedent's shape (start_campaign_item_run's item_not_eligible "blocked on
// dependency issue:N"), naming the blocking sibling and what must happen before
// this slice can dispatch.
func (e *sliceDependencyError) message() string {
	if e.blockingRunID == "" {
		return fmt.Sprintf(
			"slice %d is blocked on dependency slice %d (no sibling run minted for that slice yet); it must succeed before this slice can be dispatched",
			e.sliceIndex, e.blockingSliceIndex)
	}
	return fmt.Sprintf(
		"slice %d is blocked on dependency slice %d (run %s, state %s); it must succeed before this slice can be dispatched",
		e.sliceIndex, e.blockingSliceIndex, e.blockingRunID, e.blockingState)
}

// details is the structured 409 body payload the MCP surface reads.
func (e *sliceDependencyError) details() map[string]any {
	return map[string]any{
		"slice_index":          e.sliceIndex,
		"blocking_slice_index": e.blockingSliceIndex,
		"blocking_run_id":      e.blockingRunID,
		"blocking_state":       e.blockingState,
	}
}

// resolveSliceDependencies returns the declared dependency slice indices for a
// decomposed fan-out child, resolved from the parent's approved plan
// (decomposition.sub_plans[slice_index].depends_on — the same authority
// plan.Waves topologically sorts into the plan_decomposed waves payload).
//
// The three-way partition is load-bearing and deliberate (#2546):
//
//   - ABSENT input is NOT a violation — admit and log. resolved=false is
//     returned (with a nil error) when the run is not a fan-out child
//     (DecomposedFrom nil OR SliceIndex nil), when the parent plan resolves
//     nil / carries no Decomposition, or when *SliceIndex is out of range for
//     the sub_plans (the same defensive degrade matchDecomposedSubPlan takes;
//     the prompt-fetch path stays the fail-closed authority for a slice/plan
//     mismatch via 409 decomposed_scope_unresolved). loadApprovedPlanForRun
//     itself returns (nil,nil) when ArtifactRepo/RunRepo are unconfigured, so
//     an unconfigured deployment reads as ABSENT here.
//   - ERRORED read is RETRYABLE — fail closed. A non-nil plan-LOAD error
//     propagates as err so the caller can 500 rather than silently admit.
//   - Only a positively-UNMET dependency (resolved=true with a non-empty
//     depends_on that a sibling has not satisfied) refuses — and that decision
//     is guardDecompositionWaveOrder's, not this resolver's.
//
// The parent OWNS the plan stage, so loadApprovedPlanForRun's ParentRunID walk
// resolves the parent's approved plan on its first hop.
func (s *Server) resolveSliceDependencies(ctx context.Context, runRow *run.Run) (dependsOn []int, resolved bool, err error) {
	if runRow.DecomposedFrom == nil || runRow.SliceIndex == nil {
		return nil, false, nil
	}
	p, err := s.loadApprovedPlanForRun(ctx, *runRow.DecomposedFrom)
	if err != nil {
		return nil, false, err
	}
	if p == nil || p.Decomposition == nil {
		return nil, false, nil
	}
	idx := *runRow.SliceIndex
	if idx < 0 || idx >= len(p.Decomposition.SubPlans) {
		return nil, false, nil
	}
	return p.Decomposition.SubPlans[idx].DependsOn, true, nil
}

// listDecomposedSiblings returns EVERY decomposed child of parentID, paging past
// the per-query cap exactly as listAllDecomposedChildren does (consolidate.go).
// An explicit page size is mandatory — see decompositionSiblingPageSize.
func (s *Server) listDecomposedSiblings(ctx context.Context, parentID uuid.UUID) ([]*run.Run, error) {
	var all []*run.Run
	for offset := 0; ; offset += decompositionSiblingPageSize {
		page, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
			DecomposedFrom: &parentID,
			Limit:          decompositionSiblingPageSize,
			Offset:         offset,
		})
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < decompositionSiblingPageSize {
			break
		}
	}
	return all, nil
}

// guardDecompositionWaveOrder is the host-dispatch wave-order guard (#2546). For
// a decomposed child carrying declared dependencies it lists the parent's
// minted children, indexes them by slice, and refuses (returns a
// *sliceDependencyError) when the LOWEST dependency slice whose sibling is
// absent or not-yet-succeeded is found — deterministic blocker selection. It
// returns (nil, nil) to ADMIT: a run that is not a fan-out child, one whose
// slice declares no dependencies, and one whose every dependency sibling has
// succeeded. A required read that ERRORS returns (nil, err) so the caller fails
// closed (retryable) rather than admitting silently.
//
// # Atomicity of the predicate vs the CAS (#2586)
//
// The ListRuns sibling snapshot below is NOT atomic with the caller's stage CAS
// in handleHostDispatchStage, and NO lock spans the two rows: the guard reads
// sibling RUN rows, the caller then writes this child's STAGE row. The
// stage-admission lock the caller holds is keyed to the stage and does not
// serialize any sibling-run write. That window is real; what makes it safe is a
// property of the run state machine, not serialization.
//
// Correctness rests on run-state MONOTONICITY: run state 'succeeded' is
// ABSORBING. runs.state is written by exactly one query (UpdateRunState),
// reached by exactly two repository methods, each gated inside a
// SELECT ... FOR UPDATE transaction — TransitionRun (ValidRunTransition refuses
// any terminal `from`) and RetryRun (ValidRunRetryTransition admits only
// failed → running); ReviveRun refuses a non-failed run outright, and there is
// no run-deletion path. See backend/internal/run/transition.go, pinned by
// run.TestRunSucceededIsAbsorbing and
// run.TestPostgres_SucceededRunNeverLeavesSucceeded.
//
// Consequently, of the ways SIBLING RUN STATE can drift inside the window:
//
//   - a dependency REACHING succeeded, or a dependency slice gaining a
//     freshly-minted (pending) sibling — the guard decides on its stale
//     snapshot and REFUSES. Fails closed; the cost is a spurious 409 the
//     operator clears by re-dispatching.
//   - a dependency LEAVING succeeded — this WOULD admit out of wave order, and
//     is the direction that would be dangerous. It is unreachable because
//     succeeded is absorbing. Characterized, not hypothesized, by
//     TestGuard_Window_DependencyLeavesSucceeded_AdmitsOnSnapshot.
//
// Because the enforcement lives in the repository layer under a row lock rather
// than in an in-process mutex, this argument survives multiple fishhawkd
// replicas — unlike the #1936 stage-admission fence, whose residual is
// single-process by construction.
//
// SCOPE OF THAT CLAIM — it covers SIBLING-RUN-STATE staleness ONLY. Two
// residuals are NOT closed by it:
//
//   - PLAN-REVISION WINDOW (not covered). The dependency SET itself is read
//     from the parent's approved plan by resolveSliceDependencies. A parent plan
//     revised between that read and the caller's CAS can change depends_on, so
//     the guard can decide against a depends_on that is no longer current. The
//     absorbing-succeeded argument says nothing about this: it constrains run
//     STATE, not plan CONTENT. This is an operator-level anomaly (a plan revised
//     underneath an in-flight fan-out) rather than a race an ordinary caller can
//     drive, and it is untested and unfixed here.
//   - SPURIOUS REFUSAL (accepted). The fail-closed direction above yields a 409
//     that is stale-but-safe; the operator clears it by re-dispatching.
func (s *Server) guardDecompositionWaveOrder(ctx context.Context, runRow *run.Run) (*sliceDependencyError, error) {
	dependsOn, resolved, err := s.resolveSliceDependencies(ctx, runRow)
	if err != nil {
		return nil, err
	}
	if !resolved || len(dependsOn) == 0 {
		return nil, nil
	}
	siblings, err := s.listDecomposedSiblings(ctx, *runRow.DecomposedFrom)
	if err != nil {
		return nil, err
	}
	bySlice := make(map[int]*run.Run, len(siblings))
	for _, sib := range siblings {
		if sib.SliceIndex != nil {
			bySlice[*sib.SliceIndex] = sib
		}
	}
	// Walk dependencies in ASCENDING slice order so the named blocker is
	// deterministic. Copy before sorting — dependsOn aliases the plan's own
	// slice, which must not be mutated.
	deps := append([]int(nil), dependsOn...)
	sort.Ints(deps)
	for _, depIdx := range deps {
		sib, ok := bySlice[depIdx]
		if !ok {
			return &sliceDependencyError{
				sliceIndex:         *runRow.SliceIndex,
				blockingSliceIndex: depIdx,
				blockingRunID:      "",
				blockingState:      notMintedState,
			}, nil
		}
		if sib.State != run.StateSucceeded {
			return &sliceDependencyError{
				sliceIndex:         *runRow.SliceIndex,
				blockingSliceIndex: depIdx,
				blockingRunID:      sib.ID.String(),
				blockingState:      string(sib.State),
			}, nil
		}
	}
	return nil, nil
}
