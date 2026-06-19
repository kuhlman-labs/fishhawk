package main

import (
	"context"
	"encoding/json"

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
// its slice index (its position in the parent's plan_decomposed child_run_ids,
// the same ordering fishhawk_run_children dispatches in). State mirrors the
// child run's lifecycle state — pending/running/succeeded/failed — or
// "unknown" when the per-child GetRun failed (best-effort: a child read
// failure never fails the parent snapshot).
type ChildStatus struct {
	RunID      string `json:"run_id" jsonschema:"the child run UUID"`
	SliceIndex int    `json:"slice_index" jsonschema:"the child's position in the parent's plan_decomposed child_run_ids (slice-index order)"`
	State      string `json:"state" jsonschema:"the child run's lifecycle state: pending, running, succeeded, failed, or unknown when the per-child read failed"`
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
		child := ChildStatus{RunID: childID, SliceIndex: i, State: "unknown"}
		if childUUID, perr := uuid.Parse(childID); perr == nil {
			if runRow, gerr := r.api.GetRun(ctx, childUUID); gerr == nil {
				child.State = runRow.State
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
				cs.ConsolidatedBranch = decodeConsolidatedBranch(e.Payload)
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

// decodeConsolidatedBranch pulls consolidated_branch from a slices_integrated
// payload (shape {child_run_ids, consolidated_branch, slice_count}). Returns
// "" when absent or unparseable — best-effort, like the other audit decodes.
func decodeConsolidatedBranch(payload any) string {
	raw, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	var p struct {
		ConsolidatedBranch string `json:"consolidated_branch"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return ""
	}
	return p.ConsolidatedBranch
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
