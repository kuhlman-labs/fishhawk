package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/wavecoverage"
)

// Integration-phase values for ChildrenStatus.IntegrationPhase (E24.7 /
// #1147, reworked #4080). A pure classification over the children's lifecycle
// states plus the parent's fan-in audit kinds (slices_integrated /
// slice_integration_conflict, ADR-041 / #1142; slice_head_missing, #4079;
// slice_integration_failed, #1243):
//
//   - running_children     — at least one child is still pending/running (or
//     failed), and no fan-in failure is newer than the newest clean
//     integration. A between-wave slices_integrated entry does NOT make a
//     mid-fan-out parent integrated (#4080).
//   - ready_to_integrate   — every child succeeded but the NEWEST clean
//     slices_integrated entry (if any) does not cover every child.
//   - integrated           — every child succeeded AND the newest clean
//     slices_integrated entry covers every child (wavecoverage.Uncovered is
//     empty). Before #4080 ANY slices_integrated entry, including a partial
//     between-wave one, read as integrated.
//   - integration_conflict — a slice_integration_conflict audit is the newest
//     fan-in failure and is strictly newer than the newest clean integration.
//   - integration_failed   — a slice_head_missing or slice_integration_failed
//     audit is the newest fan-in failure and is strictly newer than the newest
//     clean integration (#4080).
const (
	integrationPhaseRunningChildren  = "running_children"
	integrationPhaseReadyToIntegrate = "ready_to_integrate"
	integrationPhaseIntegrated       = "integrated"
	integrationPhaseConflict         = "integration_conflict"
	integrationPhaseFailed           = "integration_failed"
)

// Fan-in audit categories the children-status block reads (#4080). The three
// FAILURE kinds compete on Sequence with the one clean kind: the newest failure
// wins only when strictly newer than the newest clean integration.
const (
	auditCategorySlicesIntegrated         = "slices_integrated"
	auditCategorySliceIntegrationConflict = "slice_integration_conflict"
	auditCategorySliceHeadMissing         = "slice_head_missing"
	auditCategorySliceIntegrationFailed   = "slice_integration_failed"
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
	IntegrationPhase string        `json:"integration_phase" jsonschema:"the fan-in phase: running_children (a child is still in flight or failed), ready_to_integrate (all children succeeded but the newest slices_integrated does not cover every child), integrated (all children succeeded AND the newest slices_integrated covers every child), integration_conflict (a slice_integration_conflict newer than the newest clean integration), or integration_failed (a slice_head_missing or slice_integration_failed newer than the newest clean integration)"`
	Children         []ChildStatus `json:"children" jsonschema:"one entry per discovered child, in plan_decomposed (slice-index) order"`
	Total            int           `json:"total" jsonschema:"number of discovered children"`
	Pending          int           `json:"pending" jsonschema:"children in state pending"`
	Running          int           `json:"running" jsonschema:"children in state running"`
	Succeeded        int           `json:"succeeded" jsonschema:"children in state succeeded"`
	Failed           int           `json:"failed" jsonschema:"children in state failed"`
	// ConsolidatedBranch is the fan-in target branch decoded from the NEWEST
	// slices_integrated audit payload — in ANY phase, not only integrated: a
	// between-wave entry names the branch too, and it carries whatever slices
	// IntegratedChildRunIDs lists.
	ConsolidatedBranch string `json:"consolidated_branch,omitempty" jsonschema:"the consolidated branch the newest slices_integrated audit merged slices onto (in any phase); integrated_child_run_ids says which slices it carries"`
	// ConflictingChildRunID is the slice child whose branch could not merge,
	// surfaced from the slice_integration_conflict audit payload — the same
	// structured value the next_actions slices_integration_conflict arm reads.
	// Kept for back-compat; IntegrationFailure is the cause-general form.
	ConflictingChildRunID string `json:"conflicting_child_run_id,omitempty" jsonschema:"the child run whose slice branch failed to merge during fan-in; from the newest slice_integration_conflict audit payload"`
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
	// UnintegratedChildRunIDs is every SUCCEEDED child the newest clean
	// slices_integrated entry does not cover, in slice order (#4080) — decided
	// by wavecoverage.Uncovered, the same predicate the server's acceptance
	// gate refuses on. Non-empty means the consolidated branch lacks those
	// slices, so acceptance and review must wait.
	UnintegratedChildRunIDs []string `json:"unintegrated_child_run_ids,omitempty" jsonschema:"succeeded children whose slices the newest slices_integrated entry does NOT carry, in slice order; acceptance and review must wait until this is empty"`
	// IntegrationFailure names the newest fan-in FAILURE (#4080) — a
	// slice_head_missing, slice_integration_conflict or slice_integration_failed
	// audit — when it is strictly newer than the newest clean integration. nil
	// otherwise, including when a later clean integration superseded it.
	IntegrationFailure *integrationFailure `json:"integration_failure,omitempty" jsonschema:"the newest fan-in failure (cause = its audit category) when it is newer than the newest clean slices_integrated; absent otherwise"`

	// fanInRecorded is true when ANY of the four fan-in audit kinds was read
	// for the parent. Unexported, so it never reaches the wire: it feeds the
	// read-side mirror of the server's no-integration-authority stand-down
	// (integrationAuthorityAbsent, approval condition C3).
	fanInRecorded bool
}

// integrationFailure is the decoded newest fan-in failure (#4080). Cause is the
// audit category (slice_head_missing, slice_integration_conflict or
// slice_integration_failed). ChildRunID / SliceIndex name the failing slice
// when the payload carries one (slice_integration_failed is parent-wide and
// names none).
type integrationFailure struct {
	Cause      string `json:"cause" jsonschema:"the fan-in failure's audit category: slice_head_missing, slice_integration_conflict or slice_integration_failed"`
	ChildRunID string `json:"child_run_id,omitempty" jsonschema:"the child whose slice failed to integrate; absent for slice_integration_failed, which is parent-wide"`
	SliceIndex *int   `json:"slice_index,omitempty" jsonschema:"the failing child's slice index, when the payload carries one"`
	Branch     string `json:"branch,omitempty" jsonschema:"the slice branch that is missing (slice_head_missing only)"`
	Detail     string `json:"detail,omitempty" jsonschema:"the failure detail from the audit payload (the head-missing detail, or the give-up error with its attempt count)"`
	Sequence   int64  `json:"sequence" jsonschema:"the failure audit entry's sequence; it is newer than the newest clean slices_integrated"`
}

// fanInSnapshot is the classifier's view of the parent's fan-in audit (#4080):
// the newest clean integration (its Sequence and the child_run_ids it merged)
// and the newest fan-in FAILURE of any of the three failure kinds (its Sequence
// and its category). A -1 Sequence means that kind is absent.
type fanInSnapshot struct {
	integratedSeq         int64
	integratedChildRunIDs []string
	failureSeq            int64
	failureCause          string
}

// classifyIntegrationPhase is the pure phase classifier (#1147, reworked for
// coverage in #4080). Evaluation order:
//
//  1. The newest fan-in FAILURE is strictly newer than the newest clean
//     integration → integration_conflict when that failure is a
//     slice_integration_conflict, otherwise integration_failed. An older
//     slices_integrated entry can never mask a newer failure, and a later clean
//     re-integration supersedes an earlier one.
//  2. Every child succeeded AND a clean integration exists AND it covers every
//     child (wavecoverage.Uncovered is empty) → integrated. A partial
//     between-wave entry therefore no longer reads as integrated.
//  3. Every child succeeded → ready_to_integrate.
//  4. Otherwise → running_children.
//
// No I/O, so every branch is exhaustively unit-testable.
func classifyIntegrationPhase(children []ChildStatus, fi fanInSnapshot) string {
	// Sequences are strictly increasing per run, so equality is impossible and
	// the -1 absent sentinel makes a lone failure (failureSeq >= 0) win over an
	// absent integration (integratedSeq == -1).
	if fi.failureSeq >= 0 && fi.failureSeq > fi.integratedSeq {
		if fi.failureCause == auditCategorySliceIntegrationConflict {
			return integrationPhaseConflict
		}
		return integrationPhaseFailed
	}
	if !allChildrenSucceeded(children) {
		return integrationPhaseRunningChildren
	}
	if fi.integratedSeq >= 0 && len(wavecoverage.Uncovered(childRunIDsOf(children), fi.integratedChildRunIDs)) == 0 {
		return integrationPhaseIntegrated
	}
	return integrationPhaseReadyToIntegrate
}

// allChildrenSucceeded reports whether there is at least one child and every
// child is in run state succeeded. An "unknown" child (its read failed) is not
// succeeded, so a read failure can never fake a completed fan-out.
func allChildrenSucceeded(children []ChildStatus) bool {
	if len(children) == 0 {
		return false
	}
	for _, c := range children {
		if c.State != "succeeded" {
			return false
		}
	}
	return true
}

// childRunIDsOf returns the children's run ids in their slice order.
func childRunIDsOf(children []ChildStatus) []string {
	ordered := childrenInSliceOrder(children)
	ids := make([]string, 0, len(ordered))
	for _, c := range ordered {
		ids = append(ids, c.RunID)
	}
	return ids
}

// childrenInSliceOrder returns a copy of children sorted by ascending slice
// index, ties broken by run id so the order is total.
func childrenInSliceOrder(children []ChildStatus) []ChildStatus {
	ordered := make([]ChildStatus, len(children))
	copy(ordered, children)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].SliceIndex != ordered[j].SliceIndex {
			return ordered[i].SliceIndex < ordered[j].SliceIndex
		}
		return ordered[i].RunID < ordered[j].RunID
	})
	return ordered
}

// succeededChildRunIDs returns the run ids of the SUCCEEDED children in slice
// order — the set the server's acceptance gate requires the newest
// slices_integrated entry to cover.
func succeededChildRunIDs(children []ChildStatus) []string {
	var ids []string
	for _, c := range childrenInSliceOrder(children) {
		if c.State == "succeeded" {
			ids = append(ids, c.RunID)
		}
	}
	return ids
}

// childrenStatusFor builds the decomposed-parent ChildrenStatus block (#1147).
// It discovers the children from the parent's plan_decomposed audit entry
// (reusing api.LatestPlanDecomposed), returning (nil, nil) when the run is not
// a decomposed parent. Each child's lifecycle state is read with one GetRun;
// a per-child read failure is best-effort (State="unknown", never fails the
// snapshot). The integration phase and the fan-in fields are derived from the
// four fan-in categories (slices_integrated, slice_integration_conflict,
// slice_head_missing, slice_integration_failed) in the already-fetched
// recentAudit window.
func (r *runResolver) childrenStatusFor(ctx context.Context, parentID uuid.UUID, recentAudit []AuditEntry) (*ChildrenStatus, error) {
	pd, err := r.api.LatestPlanDecomposed(ctx, parentID)
	if err != nil {
		return nil, err
	}
	if pd == nil {
		// Not a decomposed parent — the block is omitted.
		return nil, nil
	}
	return r.childrenStatusFromDecomposition(ctx, pd, recentAudit), nil
}

// childrenStatusFromDecomposition is childrenStatusFor past the
// plan_decomposed probe: the per-child reads, the dependency pass and the
// fan-in classification. Split out so fanInChildrenStatus can probe the
// decomposition FIRST and pay the paginated fan-in walk only for a real
// decomposed parent (approval condition C1), without a second
// LatestPlanDecomposed read.
func (r *runResolver) childrenStatusFromDecomposition(ctx context.Context, pd *PlanDecomposed, recentAudit []AuditEntry) *ChildrenStatus {
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

	// Scan the audit window for the fan-in outcome, tracking the HIGHEST
	// Sequence per kind so the classifier can honour ordering (a later clean
	// integration supersedes an earlier failure, and vice-versa). The window is
	// time-descending (recent) or category-paged ascending (await) — we do not
	// rely on its order: we keep the max-sequence entry for each kind and decode
	// the surfaced fields from that same newest entry.
	fi := fanInSnapshot{integratedSeq: -1, failureSeq: -1}
	var failureEntry *AuditEntry
	conflictSeq := int64(-1)
	for i := range recentAudit {
		e := &recentAudit[i]
		switch e.Category {
		case auditCategorySlicesIntegrated:
			cs.fanInRecorded = true
			if e.Sequence > fi.integratedSeq {
				fi.integratedSeq = e.Sequence
				// Both fields come from the SAME newest entry (E50.13 / #2363):
				// a branch paired with an older entry's coverage set would
				// admit a dependent child onto a base missing its predecessors.
				p := decodeSlicesIntegrated(e.Payload)
				cs.ConsolidatedBranch = p.ConsolidatedBranch
				cs.IntegratedChildRunIDs = p.ChildRunIDs
				fi.integratedChildRunIDs = p.ChildRunIDs
			}
		case auditCategorySliceIntegrationConflict, auditCategorySliceHeadMissing, auditCategorySliceIntegrationFailed:
			cs.fanInRecorded = true
			if e.Category == auditCategorySliceIntegrationConflict && e.Sequence > conflictSeq {
				conflictSeq = e.Sequence
				cs.ConflictingChildRunID = decodeSliceIntegrationConflict(e.Payload).ConflictingChildRunID
			}
			if e.Sequence > fi.failureSeq {
				fi.failureSeq = e.Sequence
				fi.failureCause = e.Category
				failureEntry = e
			}
		}
	}

	cs.IntegrationPhase = classifyIntegrationPhase(cs.Children, fi)
	cs.UnintegratedChildRunIDs = wavecoverage.Uncovered(succeededChildRunIDs(cs.Children), fi.integratedChildRunIDs)
	if failureEntry != nil && fi.failureSeq > fi.integratedSeq {
		cs.IntegrationFailure = decodeIntegrationFailure(failureEntry)
	}
	return cs
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

// sliceIntegrationConflictPayload is the ONE read-side declaration of the
// slice_integration_conflict payload (shape {parent_stage_id,
// conflicting_slice_index, conflicting_child_run_id}).
type sliceIntegrationConflictPayload struct {
	ConflictingChildRunID string `json:"conflicting_child_run_id"`
	ConflictingSliceIndex *int   `json:"conflicting_slice_index"`
}

// sliceHeadMissingPayload is the ONE read-side declaration of the
// slice_head_missing payload (#4079). Its json tags are tied to
// childcompletion's emitSliceHeadMissing literal on disk by
// TestSliceHeadMissingPayloadKeysMatchEmitter, so a rename on either side
// reddens.
type sliceHeadMissingPayload struct {
	ChildRunID string `json:"child_run_id"`
	SliceIndex *int   `json:"slice_index"`
	Branch     string `json:"branch"`
	Detail     string `json:"detail"`
}

// sliceIntegrationFailedPayload is the ONE read-side declaration of the
// slice_integration_failed payload (#1243 bounded-retry give-up). Its json
// tags are tied to childcompletion's emitSliceIntegrationFailed literal on
// disk by TestSliceIntegrationFailedPayloadKeysMatchEmitter.
type sliceIntegrationFailedPayload struct {
	Attempts int    `json:"attempts"`
	Error    string `json:"error"`
}

// decodeAuditPayload re-decodes an audit payload into out. A marshal or
// unmarshal failure leaves out at its zero value — best-effort, like the other
// audit decodes.
func decodeAuditPayload(payload any, out any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_ = json.Unmarshal(raw, out)
}

// decodeSliceIntegrationConflict decodes a slice_integration_conflict payload;
// the zero value when absent or unparseable.
func decodeSliceIntegrationConflict(payload any) sliceIntegrationConflictPayload {
	var p sliceIntegrationConflictPayload
	decodeAuditPayload(payload, &p)
	return p
}

// decodeIntegrationFailure turns the newest fan-in failure entry into the
// cause-general integrationFailure. An undecodable payload still yields the
// cause and sequence (the category alone tells the operator which recovery
// applies); only the per-cause fields degrade to empty.
func decodeIntegrationFailure(e *AuditEntry) *integrationFailure {
	f := &integrationFailure{Cause: e.Category, Sequence: e.Sequence}
	switch e.Category {
	case auditCategorySliceIntegrationConflict:
		p := decodeSliceIntegrationConflict(e.Payload)
		f.ChildRunID = p.ConflictingChildRunID
		f.SliceIndex = p.ConflictingSliceIndex
	case auditCategorySliceHeadMissing:
		var p sliceHeadMissingPayload
		decodeAuditPayload(e.Payload, &p)
		f.ChildRunID = p.ChildRunID
		f.SliceIndex = p.SliceIndex
		f.Branch = p.Branch
		f.Detail = p.Detail
	case auditCategorySliceIntegrationFailed:
		var p sliceIntegrationFailedPayload
		decodeAuditPayload(e.Payload, &p)
		f.Detail = p.Error
		if p.Attempts > 0 {
			f.Detail = fmt.Sprintf("after %d attempts: %s", p.Attempts, p.Error)
		}
	}
	return f
}
