package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// Integration-phase values for ChildrenStatus.IntegrationPhase (E24.7 /
// #1147). A pure classification over the children's lifecycle states plus
// the presence of the fan-in audit kinds (slices_integrated /
// slice_integration_conflict, ADR-041 / #1142):
//
//   - running_children     — at least one child is still pending/running (or
//     failed) and the fan-in has not been attempted.
//   - ready_to_integrate   — every child succeeded but no fan-in audit landed.
//   - integrated           — a slices_integrated audit recorded a clean fan-in.
//   - integration_conflict — a slice_integration_conflict audit recorded a
//     merge conflict and no later clean integration.
const (
	integrationPhaseRunningChildren  = "running_children"
	integrationPhaseReadyToIntegrate = "ready_to_integrate"
	integrationPhaseIntegrated       = "integrated"
	integrationPhaseConflict         = "integration_conflict"
)

// ChildStatus is one decomposed child's live lifecycle state, paired with
// its slice index (the child run row's authoritative slice_index — its
// sub_plan position in the parent's decomposition — falling back to the
// position in child_run_ids only for an older backend that omits the field).
// State mirrors the child run's lifecycle state —
// pending/running/succeeded/failed — or "unknown" when the per-child GetRun
// failed (best-effort: a child read failure never fails the parent snapshot).
type ChildStatus struct {
	RunID      string `json:"run_id" jsonschema:"the child run UUID"`
	SliceIndex int    `json:"slice_index" jsonschema:"the child's authoritative slice index from its run row (its sub_plan position in the parent's decomposition); falls back to the position in child_run_ids only for an older backend that omits slice_index"`
	State      string `json:"state" jsonschema:"the child run's lifecycle state: pending, running, succeeded, failed, or unknown when the per-child read failed"`
	// ImplementStageState is the child's IMPLEMENT stage state — the value
	// dispatchability is actually keyed on (#1237), NOT the run-level State
	// above. A local decomposed child parked by RuleChildrenDispatch has its RUN
	// advanced to 'running' while its implement STAGE sits at
	// pending/awaiting_host_dispatch awaiting the host fan-out, so keying
	// dispatchability on the run state would skip exactly the parked population
	// fishhawk_run_children exists to spawn. Populated best-effort by the await
	// path (childrenStatusForAwait resolves each child's implement stage); left
	// empty on the plain get_run_status snapshot, which does not need it.
	ImplementStageState string `json:"implement_stage_state,omitempty" jsonschema:"the child's implement-stage state (pending, awaiting_host_dispatch, dispatched, running, or a terminal state) — the value dispatchability is keyed on, distinct from the run-level state; populated on the fishhawk_await_children path"`
	// DependsOn lists the slice indices this child depends on (E48.99 / #2546),
	// mirrored from the child run row's slice_depends_on (resolved from the
	// parent's approved plan on the single-run read). Omitted for a wave-0
	// child with no declared dependencies, and for a legacy backend that omits
	// slice_depends_on entirely (nil-decode → Blocked false, so the block
	// renders exactly as it did before this field existed).
	DependsOn []int `json:"depends_on,omitempty" jsonschema:"the slice indices this child depends on, from the parent plan's decomposition; omitted for a wave-0 child with no dependencies"`
	// Blocked is true when a dependency slice has not yet reached state
	// succeeded — the child is NOT dispatchable until it clears. An
	// unknown-state dependency (its per-child read failed) counts as blocking,
	// never as dispatchable. A dependency slice with NO minted sibling (absent
	// from the parent's child_run_ids) also counts as blocking: host dispatch
	// refuses that child as not_minted, so the view must not advertise it as
	// dispatchable.
	Blocked bool `json:"blocked" jsonschema:"true when a dependency slice has not yet succeeded, so this child cannot be dispatched yet; an unknown-state or not-yet-minted dependency counts as blocking"`
	// BlockedBy names the run ids of the dependency siblings that have not yet
	// succeeded, in ascending slice order. Empty when the child is not blocked.
	// A not-minted dependency slice has no run id to name, so it is reported as
	// a synthetic "slice N (not_minted)" marker instead — the read-side mirror
	// of the host-dispatch guard's not_minted refusal.
	BlockedBy []string `json:"blocked_by,omitempty" jsonschema:"the not-yet-succeeded dependency blockers for this child, in slice order: a minted sibling's run id, or a synthetic \"slice N (not_minted)\" marker for a dependency slice with no minted sibling"`
}

// ChildrenStatus is the decomposed-parent per-child + integration-phase view
// (E24.7 / #1147) surfaced on fishhawk_get_run_status. Best-effort and
// purely additive: a per-child read failure degrades that child to
// State="unknown" rather than failing the snapshot, and the whole block is
// omitted for non-decomposed runs.
type ChildrenStatus struct {
	IntegrationPhase string        `json:"integration_phase" jsonschema:"the fan-in phase: running_children (a child is still in flight), ready_to_integrate (all children succeeded, no fan-in yet), integrated (a slices_integrated audit recorded a clean fan-in), or integration_conflict (a slice_integration_conflict audit recorded a merge conflict)"`
	Children         []ChildStatus `json:"children" jsonschema:"one entry per discovered child, in plan_decomposed (slice-index) order"`
	Total            int           `json:"total" jsonschema:"number of discovered children"`
	Pending          int           `json:"pending" jsonschema:"children in state pending"`
	Running          int           `json:"running" jsonschema:"children in state running"`
	Succeeded        int           `json:"succeeded" jsonschema:"children in state succeeded"`
	Failed           int           `json:"failed" jsonschema:"children in state failed"`
	// ConsolidatedBranch is the fan-in target branch surfaced from the
	// slices_integrated audit payload when a clean integration landed.
	ConsolidatedBranch string `json:"consolidated_branch,omitempty" jsonschema:"the consolidated branch a clean fan-in merged the slices onto; from the slices_integrated audit payload, present only in the integrated phase"`
	// ConflictingChildRunID is the slice child whose branch could not merge,
	// surfaced from the slice_integration_conflict audit payload — the same
	// structured value the next_actions slices_integration_conflict arm reads.
	ConflictingChildRunID string `json:"conflicting_child_run_id,omitempty" jsonschema:"the child run whose slice branch failed to merge during fan-in; from the slice_integration_conflict audit payload, present only in the integration_conflict phase"`
	// IntegratedChildRunIDs is the child_run_ids recorded on the NEWEST
	// slices_integrated audit entry (E50.13 / #2363) — the complete set of slice
	// branches merged onto ConsolidatedBranch at that moment. It is decoded from
	// the SAME entry ConsolidatedBranch comes from, so the branch and the
	// coverage set can never come from different entries.
	//
	// It exists so a reader can answer the COVERAGE question — "are this
	// dependent child's predecessors actually merged?" — with the same
	// wavecoverage.Covered predicate the server admits on, rather than with the
	// weaker Blocked flag. Blocked keys on predecessor run STATE, which flips to
	// succeeded BEFORE the between-wave integration runs.
	IntegratedChildRunIDs []string `json:"integrated_child_run_ids,omitempty" jsonschema:"the child run ids already merged onto consolidated_branch, from the NEWEST slices_integrated audit payload; the coverage set a dependent child's dispatchability is decided against"`
}

// classifyIntegrationPhase is the pure phase classifier (#1147).
// integratedSeq / conflictSeq are the highest audit Sequence among the
// slices_integrated / slice_integration_conflict fan-in audit kinds (-1 when
// that kind is absent). Ordering is significant: a slice_integration_conflict
// yields integration_conflict UNLESS a strictly later slices_integrated event
// recorded a clean re-integration that superseded it — so an older
// slices_integrated entry can never mask a newer conflict. A clean integration
// (with no later conflict) is terminal; otherwise the phase is derived from the
// children's states (all-succeeded vs still-in-flight). No I/O so every branch
// is exhaustively unit-testable.
func classifyIntegrationPhase(children []ChildStatus, integratedSeq, conflictSeq int64) string {
	// A conflict wins unless a strictly later clean integration superseded it.
	// Sequences are strictly increasing per run, so equality is impossible and
	// the -1 absent sentinel makes a lone conflict (conflictSeq >= 0) win over an
	// absent integration (integratedSeq == -1).
	if conflictSeq >= 0 && conflictSeq > integratedSeq {
		return integrationPhaseConflict
	}
	if integratedSeq >= 0 {
		return integrationPhaseIntegrated
	}
	if len(children) > 0 {
		allSucceeded := true
		for _, c := range children {
			if c.State != "succeeded" {
				allSucceeded = false
				break
			}
		}
		if allSucceeded {
			return integrationPhaseReadyToIntegrate
		}
	}
	return integrationPhaseRunningChildren
}

// childrenStatusFor builds the decomposed-parent ChildrenStatus block (#1147).
// It discovers the children from the parent's plan_decomposed audit entry
// (reusing api.LatestPlanDecomposed), returning (nil, nil) when the run is not
// a decomposed parent. Each child's lifecycle state is read with one GetRun;
// a per-child read failure is best-effort (State="unknown", never fails the
// snapshot). The integration phase + ConsolidatedBranch / ConflictingChildRunID
// are derived from the slices_integrated / slice_integration_conflict
// categories in the already-fetched recentAudit window.
func (r *runResolver) childrenStatusFor(ctx context.Context, parentID uuid.UUID, recentAudit []AuditEntry) (*ChildrenStatus, error) {
	pd, err := r.api.LatestPlanDecomposed(ctx, parentID)
	if err != nil {
		return nil, err
	}
	if pd == nil {
		// Not a decomposed parent — the block is omitted.
		return nil, nil
	}

	cs := &ChildrenStatus{
		Children: make([]ChildStatus, 0, len(pd.ChildRunIDs)),
		Total:    len(pd.ChildRunIDs),
	}
	for i, childID := range pd.ChildRunIDs {
		// SliceIndex defaults to the loop position and is overwritten below by
		// the child run row's authoritative slice_index when the GetRun hits and
		// carries it. The positional fallback covers only an OLDER backend that
		// does not send slice_index (nil-decode) — a dense-in-slice-order
		// child_run_ids, where position and slice index coincide anyway.
		child := ChildStatus{RunID: childID, SliceIndex: i, State: "unknown"}
		if childUUID, perr := uuid.Parse(childID); perr == nil {
			if runRow, gerr := r.api.GetRun(ctx, childUUID); gerr == nil {
				child.State = runRow.State
				// slice_depends_on is surfaced on the single-run read GetRun
				// hits here (E48.99 / #2546); nil for a wave-0 child or a
				// legacy backend that omits it.
				child.DependsOn = runRow.SliceDependsOn
				// Prefer the run row's authoritative slice_index over the loop
				// position: a non-dense child_run_ids (slice 0 never minted)
				// otherwise mis-keys the bySlice map below. nil only for an
				// older backend that omits the field — then the positional
				// fallback stands.
				if runRow.SliceIndex != nil {
					child.SliceIndex = *runRow.SliceIndex
				}
			}
			// A GetRun error (or an unparseable id) leaves State="unknown" —
			// best-effort, never fails the snapshot.
		}
		switch child.State {
		case "pending":
			cs.Pending++
		case "running":
			cs.Running++
		case "succeeded":
			cs.Succeeded++
		case "failed":
			cs.Failed++
		}
		cs.Children = append(cs.Children, child)
	}

	// Second pass (E48.99 / #2546): now that every child's state is known,
	// resolve each child's blocked-ness from its DependsOn against the sibling
	// states gathered above. Resolve dependencies BY SLICE INDEX through
	// bySlice (not by slice POSITION), so a plan_decomposed whose child_run_ids
	// is not dense-in-slice-order never associates a dependency with the wrong
	// child. A dependency slice that has not reached "succeeded" (including an
	// "unknown" read failure) blocks the child, and its run id is named in
	// BlockedBy. A dependency slice with NO minted sibling (absent from
	// child_run_ids — the not_minted case the host-dispatch guard refuses in
	// decomposition_dispatch_guard.go) ALSO blocks: the view must not advertise
	// a dispatch the backend would 409 dependency_not_satisfied, so it is named
	// by a synthetic "slice N (not_minted)" marker since no run id exists. So
	// one get_run_status read answers "what may I dispatch next". A child with
	// no DependsOn (wave 0, or a legacy backend that omits slice_depends_on)
	// stays Blocked=false, which renders exactly as it did before this field
	// existed (back-compat).
	bySlice := make(map[int]int, len(cs.Children)) // slice index -> position in cs.Children
	for i := range cs.Children {
		bySlice[cs.Children[i].SliceIndex] = i
	}
	for i := range cs.Children {
		for _, depIdx := range cs.Children[i].DependsOn {
			pos, minted := bySlice[depIdx]
			if !minted {
				// No sibling minted for this dependency slice: host dispatch
				// refuses this child (not_minted), so it is NOT dispatchable.
				cs.Children[i].Blocked = true
				cs.Children[i].BlockedBy = append(cs.Children[i].BlockedBy,
					fmt.Sprintf("slice %d (not_minted)", depIdx))
				continue
			}
			if cs.Children[pos].State != "succeeded" {
				cs.Children[i].Blocked = true
				cs.Children[i].BlockedBy = append(cs.Children[i].BlockedBy, cs.Children[pos].RunID)
			}
		}
	}

	// Scan the recent-audit window for the fan-in outcome, tracking the HIGHEST
	// Sequence per kind so the classifier can honour ordering (a later clean
	// integration supersedes an earlier conflict, and vice-versa). recentAudit
	// is time-descending but we do not rely on its order: we keep the
	// max-sequence entry for each kind and decode the surfaced branch / child
	// from that same latest entry. The cost gate in getRunStatus only calls this
	// when recentAudit carries a decomposition marker (or the implement stage is
	// awaiting_children), so the markers land here when present.
	var integratedSeq, conflictSeq int64 = -1, -1
	for i := range recentAudit {
		e := &recentAudit[i]
		switch e.Category {
		case "slices_integrated":
			if e.Sequence > integratedSeq {
				integratedSeq = e.Sequence
				// Both fields come from the SAME newest entry (E50.13 / #2363):
				// a branch paired with an older entry's coverage set would
				// admit a dependent child onto a base missing its predecessors.
				cs.ConsolidatedBranch = decodeConsolidatedBranch(e.Payload)
				cs.IntegratedChildRunIDs = decodeIntegratedChildRunIDs(e.Payload)
			}
		case "slice_integration_conflict":
			if e.Sequence > conflictSeq {
				conflictSeq = e.Sequence
				cs.ConflictingChildRunID = decodeConflictingChildRunID(e.Payload)
			}
		}
	}

	cs.IntegrationPhase = classifyIntegrationPhase(cs.Children, integratedSeq, conflictSeq)
	return cs, nil
}

// slicesIntegratedPayload is the ONE declaration of the slices_integrated audit
// payload's shape on the read side. Both decoders below share it deliberately:
// ConsolidatedBranch and ChildRunIDs are read from the SAME entry and a
// divergence between two hand-written anonymous structs is exactly the drift a
// coverage decision cannot survive.
//
// THE KEYS ARE TIED TO THE EMITTER, NOT HAND-MAINTAINED. The producer is
// orchestrator.emitSlicesIntegrated, in another package and unexported, so this
// package cannot call it; instead TestSlicesIntegratedPayloadKeysMatchEmitter
// reflects these json tags and asserts each one appears verbatim in that
// emitter's payload literal on disk. A rename on either side reddens. That
// closes the #2660 blind spot for this decode: a test fixture hand-writing the
// same key the decoder reads would stay green in every mode test while the real
// verb never released.
type slicesIntegratedPayload struct {
	ConsolidatedBranch string   `json:"consolidated_branch"`
	ChildRunIDs        []string `json:"child_run_ids"`
}

// decodeSlicesIntegrated decodes a slices_integrated payload. A marshal or
// unmarshal failure yields the zero value — best-effort, like the other audit
// decodes.
func decodeSlicesIntegrated(payload any) slicesIntegratedPayload {
	var p slicesIntegratedPayload
	raw, err := json.Marshal(payload)
	if err != nil {
		return slicesIntegratedPayload{}
	}
	if json.Unmarshal(raw, &p) != nil {
		return slicesIntegratedPayload{}
	}
	return p
}

// decodeConsolidatedBranch pulls consolidated_branch from a slices_integrated
// payload (shape {child_run_ids, consolidated_branch, slice_count}). Returns
// "" when absent or unparseable.
func decodeConsolidatedBranch(payload any) string {
	return decodeSlicesIntegrated(payload).ConsolidatedBranch
}

// decodeIntegratedChildRunIDs pulls child_run_ids from a slices_integrated
// payload (E50.13 / #2363) — the complete set of slice branches merged onto the
// consolidated branch at that entry. Returns nil when absent or unparseable,
// which the coverage predicate reads as "nothing is merged yet" and so FAILS
// CLOSED: a dependent child is not announced as dispatchable.
func decodeIntegratedChildRunIDs(payload any) []string {
	return decodeSlicesIntegrated(payload).ChildRunIDs
}

// decodeConflictingChildRunID pulls conflicting_child_run_id from a
// slice_integration_conflict payload (shape {parent_stage_id,
// conflicting_slice_index, conflicting_child_run_id}). Returns "" when absent
// or unparseable.
func decodeConflictingChildRunID(payload any) string {
	raw, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	var p struct {
		ConflictingChildRunID string `json:"conflicting_child_run_id"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return ""
	}
	return p.ConflictingChildRunID
}
