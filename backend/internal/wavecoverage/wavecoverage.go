// Package wavecoverage holds the ONE shared predicate that answers a single
// question about a decomposed fan-out: are a dependent child's dependency
// slices already merged onto the parent's consolidated branch?
//
// It exists as a leaf package — no repository, HTTP or git dependency — for
// exactly one reason: THREE callers inside the backend module must answer that
// question identically, and a duplicated reconstruction is the drift class this
// repo already names as load-bearing (see the SliceBranch / childSliceBranch
// "MUST stay byte-identical" note in internal/orchestrator). The callers are
//
//   - the child-completion sweeper's steady-state short-circuit, which skips a
//     between-wave re-merge once the pending wave's dependencies are already
//     covered (internal/orchestrator.IntegrateCompletedWave);
//   - the host-dispatch marker's admission decision, which derives a dependent
//     child's base_branch and otherwise refuses 409 wave_not_integrated; and
//   - the MCP await verb's children_dispatchable release decision.
//
// A caller keying on predecessor run STATE instead would be wrong in a way that
// is invisible in its own tests: run state flips to succeeded BEFORE the
// between-wave integration runs, so a state-keyed release announces
// dispatchability the server then refuses.
package wavecoverage

import "sort"

// Covered reports whether every dependency slice in dependsOn has been merged
// into the parent's consolidated branch, and names the ones that have not.
//
// Inputs:
//   - dependsOn — the dependent slice's declared dependency slice indices, read
//     from the parent's approved plan (decomposition.sub_plans[i].depends_on).
//   - sliceRunID — slice index → child run id, built from the parent's minted
//     decomposed children.
//   - integratedChildRunIDs — the child_run_ids recorded on the parent's NEWEST
//     slices_integrated audit entry, i.e. the complete set merged onto the
//     consolidated branch at that moment.
//
// Semantics:
//   - An EMPTY dependsOn is covered (the wave-0 / no-dependency case): there is
//     nothing to wait for and missing is nil.
//   - Otherwise every dependency index must map to a minted child run id that
//     appears in integratedChildRunIDs.
//   - A dependency slice with NO minted sibling is NOT covered and is reported
//     in missing (FAIL CLOSED). Behind the existing host-dispatch wave-order
//     guard that case cannot arise — the guard refuses not_minted first — but
//     the predicate's safety must not depend on a caller's ordering.
//
// missing is ascending and duplicate-free so callers can surface it verbatim in
// an operator-facing payload; it is nil exactly when covered is true.
func Covered(dependsOn []int, sliceRunID map[int]string, integratedChildRunIDs []string) (covered bool, missing []int) {
	if len(dependsOn) == 0 {
		return true, nil
	}
	integrated := make(map[string]struct{}, len(integratedChildRunIDs))
	for _, id := range integratedChildRunIDs {
		if id != "" {
			integrated[id] = struct{}{}
		}
	}
	seen := make(map[int]struct{}, len(dependsOn))
	for _, dep := range dependsOn {
		if _, dup := seen[dep]; dup {
			continue
		}
		seen[dep] = struct{}{}
		runID, minted := sliceRunID[dep]
		if !minted || runID == "" {
			missing = append(missing, dep)
			continue
		}
		if _, ok := integrated[runID]; !ok {
			missing = append(missing, dep)
		}
	}
	if len(missing) == 0 {
		return true, nil
	}
	sort.Ints(missing)
	return false, missing
}
