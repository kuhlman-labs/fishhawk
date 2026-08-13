package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/wavecoverage"
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

// waveIntegrationError is the host-dispatch refusal a decomposed child produces
// when its declared dependency slices have all SUCCEEDED — so the wave-order
// guard above admits — but they are not yet merged onto the parent's
// consolidated branch. Spawning there would build the dependent slice on a base
// missing its predecessors' symbols (#1302), so the endpoint refuses 409
// wave_not_integrated BEFORE the state CAS and the child stays cleanly
// re-dispatchable once the between-wave integration lands.
//
// It carries BOTH refusal causes so an operator can tell them apart from the
// 409 body alone:
//
//   - consolidatedBranchPresent=false — the parent has no slices_integrated
//     entry yet (or one carrying an empty branch): the fan-out has never been
//     integrated.
//   - missing non-empty — an integration DID happen but its child_run_ids does
//     not cover these dependency slices: a STALE integration (an earlier wave
//     was merged; this child's newly-succeeded dependency was not).
type waveIntegrationError struct {
	sliceIndex                int
	dependsOn                 []int
	missing                   []int
	consolidatedBranchPresent bool
}

// message names the wait, not a fault: the between-wave integration is a
// background responsibility (the child-completion sweeper's
// IntegrateCompletedWave), so the operator's action is to retry shortly rather
// than to repair anything.
func (e *waveIntegrationError) message() string {
	if !e.consolidatedBranchPresent {
		return fmt.Sprintf(
			"slice %d depends on slices %v, which have succeeded but are not yet integrated: the parent has no consolidated branch recorded yet; the server integrates between waves — retry the dispatch shortly",
			e.sliceIndex, e.dependsOn)
	}
	return fmt.Sprintf(
		"slice %d depends on slices %v, and dependency slices %v are not covered by the parent's newest integration (a stale consolidated branch); the server integrates between waves — retry the dispatch shortly",
		e.sliceIndex, e.dependsOn, e.missing)
}

// details is the structured 409 body payload. missing_dependency_slices is
// always a non-nil array so a consumer need not distinguish null from empty.
func (e *waveIntegrationError) details() map[string]any {
	missing := e.missing
	if missing == nil {
		missing = []int{}
	}
	return map[string]any{
		"slice_index":                 e.sliceIndex,
		"depends_on":                  e.dependsOn,
		"missing_dependency_slices":   missing,
		"consolidated_branch_present": e.consolidatedBranchPresent,
	}
}

// latestSlicesIntegrated reads the parent's NEWEST slices_integrated audit entry
// and decodes BOTH the consolidated branch name and the child run ids merged
// onto it — from THE SAME entry, which is the point of the helper. Reading the
// two fields through separate walks would let the branch and the coverage set
// come from different integrations, i.e. admit a dependent child onto a branch
// whose coverage was proved by an older entry.
//
// The newest entry is the complete merged set at that moment because
// orchestrator.IntegrateSlices always merges EVERY succeeded slice ascending on
// each pass. Same reader (and same newest-entry convention) as
// consolidatedBranchFromAudit, differing only in decoding child_run_ids too.
//
// Returns ("", nil, nil) when no entry exists or the newest payload does not
// decode — ABSENT, which the caller turns into the 409 refusal rather than a
// silent admit. A LIST error propagates so the caller can 500 (retryable).
func (s *Server) latestSlicesIntegrated(ctx context.Context, parentRunID uuid.UUID) (consolidatedBranch string, childRunIDs []string, err error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, parentRunID, "slices_integrated")
	if err != nil {
		return "", nil, err
	}
	if len(entries) == 0 {
		return "", nil, nil
	}
	var payload struct {
		ConsolidatedBranch string   `json:"consolidated_branch"`
		ChildRunIDs        []string `json:"child_run_ids"`
	}
	if json.Unmarshal(entries[len(entries)-1].Payload, &payload) != nil {
		return "", nil, nil
	}
	return payload.ConsolidatedBranch, payload.ChildRunIDs, nil
}

// resolveDependentChildBase answers the per-wave RE-BASE authoritatively: which
// branch must THIS fan-out child's runner spawn against, and may it spawn at
// all?
//
// It runs AFTER guardDecompositionWaveOrder, so a non-empty resolved depends_on
// here means every dependency slice has already reached run state 'succeeded'.
// That is NOT sufficient to spawn: run state flips to succeeded BEFORE the
// between-wave integration merges the slice branches, so a child admitted on
// state alone would build on a base missing its predecessors' symbols. The
// admission is therefore keyed to the shared wavecoverage.Covered predicate —
// the SAME function the sweeper's steady-state short-circuit and the MCP await
// verb's dispatchable release use, so a release can never announce
// dispatchability this endpoint would refuse.
//
// Three outcomes:
//
//   - ADMIT with an empty base ("", nil, nil) — the run is not a fan-out child,
//     its slice declares no dependencies, the parent plan did not resolve, or
//     no AuditRepo is configured. The caller keeps whatever base it had, i.e.
//     byte-identical to pre-#2363 behaviour. ABSENT input is not a violation,
//     the same three-way partition resolveSliceDependencies documents (an
//     unconfigured deployment must not have every dependent dispatch wedged).
//   - ADMIT with the consolidated branch (branch, nil, nil) — dependencies are
//     covered by the newest integration AND that entry names a branch.
//   - REFUSE (a non-nil *waveIntegrationError) — no integration, an empty
//     branch, or a STALE integration whose child_run_ids does not cover every
//     dependency slice. A NON-EMPTY consolidated branch is deliberately NOT
//     sufficient on its own: that is exactly the stale case.
//
// A required read that ERRORS returns err so the caller 500s
// dependency_check_failed (retryable), never a silent admit.
func (s *Server) resolveDependentChildBase(ctx context.Context, runRow *run.Run) (string, *waveIntegrationError, error) {
	dependsOn, resolved, err := s.resolveSliceDependencies(ctx, runRow)
	if err != nil {
		return "", nil, err
	}
	if !resolved || len(dependsOn) == 0 {
		return "", nil, nil
	}
	if s.cfg.AuditRepo == nil {
		return "", nil, nil
	}
	branch, integrated, err := s.latestSlicesIntegrated(ctx, *runRow.DecomposedFrom)
	if err != nil {
		return "", nil, err
	}
	siblings, err := s.listDecomposedSiblings(ctx, *runRow.DecomposedFrom)
	if err != nil {
		return "", nil, err
	}
	sliceRunID := make(map[int]string, len(siblings))
	for _, sib := range siblings {
		if sib.SliceIndex != nil {
			sliceRunID[*sib.SliceIndex] = sib.ID.String()
		}
	}
	covered, missing := wavecoverage.Covered(dependsOn, sliceRunID, integrated)
	if !covered || branch == "" {
		// Copy + sort the declared set for the operator-facing payload; it
		// aliases the plan's own slice, which must not be mutated.
		deps := append([]int(nil), dependsOn...)
		sort.Ints(deps)
		return "", &waveIntegrationError{
			sliceIndex:                *runRow.SliceIndex,
			dependsOn:                 deps,
			missing:                   missing,
			consolidatedBranchPresent: branch != "",
		}, nil
	}
	return branch, nil, nil
}
