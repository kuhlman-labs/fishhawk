package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// acceptanceHeldIntegrationIncompleteState is the next_actions state a
// decomposed parent reports when an acceptance dispatch WOULD be offered but
// the parent's consolidated branch does not provably carry every child's slice
// (#4080). The server refuses that dispatch 409
// acceptance_integration_incomplete on both the host-dispatch marker and the
// acceptance-admission endpoint; this state keeps the display from offering it.
const acceptanceHeldIntegrationIncompleteState = "acceptance_held_integration_incomplete"

// offersAcceptanceDispatch reports whether next_actions carries any
// fishhawk_dispatch_stage or fishhawk_run_stage action for the acceptance
// stage — keyed on params.stage == acceptance, the same key
// foldAcceptancePreviewAdvisory uses. Pure.
func offersAcceptanceDispatch(na *NextActions) bool {
	if na == nil {
		return false
	}
	for _, a := range na.Actions {
		if isAcceptanceDispatchAction(a) {
			return true
		}
	}
	return false
}

// isAcceptanceDispatchAction is the one predicate offersAcceptanceDispatch and
// the hold's strip loop share, so the two can never disagree on what counts as
// an acceptance dispatch.
func isAcceptanceDispatchAction(a SuggestedAction) bool {
	if a.Action != "fishhawk_dispatch_stage" && a.Action != "fishhawk_run_stage" {
		return false
	}
	return a.Params["stage"] == "acceptance"
}

// integrationAuthorityAbsent is the read-side mirror of the server's
// no-integration-authority stand-down (approval condition C3). The server's
// acceptance gate admits without a coverage check where integrateSlices
// graceful-skips — no GitHub client, or a run with no installation id — because
// no slices_integrated record is ever written there. The MCP surface cannot see
// that configuration, so it infers it from its one observable consequence: NO
// fan-in record of any kind exists although the parent's implement stage has
// already SUCCEEDED. Where integration authority exists that combination does
// not arise: the parent's implement stage resolves succeeded only after
// IntegrateSlices ran, and every non-skipped pass writes slices_integrated (a
// succeeded child lacking slice_index now fails the pass closed instead of
// being dropped). An unreadable parent stage ("") is never read as
// authority-less, so a read failure cannot release a hold.
func integrationAuthorityAbsent(cs *ChildrenStatus, parentImplementState string) bool {
	return cs != nil && !cs.fanInRecorded && parentImplementState == "succeeded"
}

// foldAcceptanceIntegrationHold is the pure next_actions hold (#4080). When the
// run is a decomposed parent (cs != nil with at least one child) whose
// integration phase is not integrated, it removes every acceptance dispatch
// action, sets State to acceptance_held_integration_incomplete, and PREPENDS a
// fishhawk_await_children action whose reason names the uncovered children or
// the integration failure and the server's 409.
//
// It is a no-op when:
//   - na or cs is nil (not a decomposed parent, or the snapshot was not read);
//   - the parent has no children (the server guard is inert there too);
//   - the phase is integrated (full coverage);
//   - no acceptance dispatch is offered;
//   - integrationAuthorityAbsent (C3: the server stands down, so the display
//     must not hold either — a deployment that cannot integrate is never
//     wedged by this hold).
func foldAcceptanceIntegrationHold(runID string, cs *ChildrenStatus, parentImplementState string, na *NextActions) {
	if na == nil || cs == nil || len(cs.Children) == 0 {
		return
	}
	if cs.IntegrationPhase == integrationPhaseIntegrated {
		return
	}
	if !offersAcceptanceDispatch(na) {
		return
	}
	if integrationAuthorityAbsent(cs, parentImplementState) {
		return
	}
	kept := make([]SuggestedAction, 0, len(na.Actions)+1)
	kept = append(kept, SuggestedAction{
		Action:       "fishhawk_await_children",
		Params:       map[string]string{"run_id": runID},
		Precondition: "this decomposed parent's consolidated branch does not carry every child's slice yet",
		Consumes:     "none",
		Reason:       acceptanceIntegrationHoldReason(cs),
	})
	for _, a := range na.Actions {
		if isAcceptanceDispatchAction(a) {
			continue
		}
		kept = append(kept, a)
	}
	na.State = acceptanceHeldIntegrationIncompleteState
	na.Actions = kept
}

// acceptanceIntegrationHoldReason names what is blocking: the integration
// failure when one is newer than the newest clean integration, otherwise the
// uncovered or non-succeeded children.
func acceptanceIntegrationHoldReason(cs *ChildrenStatus) string {
	const tail = "; the server refuses an acceptance dispatch here with 409 acceptance_integration_incomplete, so wait for the fan-in to cover every child"
	if f := cs.IntegrationFailure; f != nil {
		who := ""
		if f.ChildRunID != "" {
			who = " on child " + f.ChildRunID
		}
		return fmt.Sprintf("acceptance held: slice integration failed (%s%s)%s", f.Cause, who, tail)
	}
	if len(cs.UnintegratedChildRunIDs) > 0 {
		return fmt.Sprintf("acceptance held: the consolidated branch lacks the slices of %s (integration_phase %s)%s",
			strings.Join(cs.UnintegratedChildRunIDs, ", "), cs.IntegrationPhase, tail)
	}
	return fmt.Sprintf("acceptance held: not every child has succeeded and been integrated (integration_phase %s)%s",
		cs.IntegrationPhase, tail)
}

// gateAcceptanceOnIntegration is the resolver half of the hold, wired at BOTH
// nextActionsFor call sites (getRunStatus and the run_stage post-stage
// snapshot) immediately after nextActionsFor and BEFORE the acceptance
// redispatch/preview folds, so the preview bring-up advisory no-ops once the
// dispatch is stripped.
//
// It does NOTHING — zero reads — unless an acceptance dispatch is offered and
// the run is top-level. It then reads a FRESH snapshot through
// fanInChildrenStatus (plan_decomposed probe first, then the paginated fan-in
// walk), never the bounded recent-audit window, where an aged-out
// slices_integrated would mis-classify a fully integrated parent and wrongly
// strip its acceptance dispatch. It FAILS OPEN on a read error: next_actions is
// display-only and the server's 409 is the authority, so a stale display cannot
// cause a wrong-tree acceptance spawn.
func (r *runResolver) gateAcceptanceOnIntegration(ctx context.Context, runID uuid.UUID, run *Run, stages []Stage, na *NextActions) {
	if run == nil || !offersAcceptanceDispatch(na) {
		return
	}
	if (run.ParentRunID != nil && *run.ParentRunID != "") || (run.DecomposedFrom != nil && *run.DecomposedFrom != "") {
		return
	}
	cs, err := r.fanInChildrenStatus(ctx, runID)
	if err != nil || cs == nil {
		return
	}
	parentImplementState := ""
	if impl := stageByType(stages, "implement"); impl != nil {
		parentImplementState = impl.State
	}
	foldAcceptanceIntegrationHold(runID.String(), cs, parentImplementState, na)
}
