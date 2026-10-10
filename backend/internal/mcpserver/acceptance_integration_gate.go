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

// newestStageByType returns the stage of typ with the HIGHEST Sequence,
// regardless of the slice's order, or nil when there is none. A parent whose
// implement stage was retried carries more than one implement stage; the
// newest is the one whose state decides the fan-in, and the
// ambiguity-erroring resolveStage would read such a parent as unreadable
// (#4165, run 671e7f41 low (b)).
func newestStageByType(stages []Stage, typ string) *Stage {
	var newest *Stage
	for i := range stages {
		if stages[i].Type != typ {
			continue
		}
		if newest == nil || stages[i].Sequence > newest.Sequence {
			newest = &stages[i]
		}
	}
	return newest
}

// sliceIntegrationOf returns the run's capabilities.slice_integration block
// (#4165), nil-safe through run and Capabilities. nil means UNDECIDABLE — a
// list read, or an older backend whose capabilities block predates the key —
// never "unavailable".
func sliceIntegrationOf(run *Run) *runSliceIntegration {
	if run == nil || run.Capabilities == nil {
		return nil
	}
	return run.Capabilities.SliceIntegration
}

// integrationAuthorityAbsent is the read-side mirror of the server's
// no-integration-authority stand-down (approval condition C3). The server's
// decomposed-parent acceptance gate (guardDecomposedParentAcceptance, on both
// the host-dispatch marker and acceptance-admission) admits without a coverage
// check where orchestrator.SliceIntegrationUnavailable reports a reason — no
// GitHub client, or a run with no installation id.
//
// authority is that SAME predicate, surfaced on GET /v0/runs/{run_id} as
// capabilities.slice_integration (#4165). When it is present it is
// AUTHORITATIVE: the answer is !authority.Available, and neither the fan-in
// record nor the parent's stage state is consulted, exactly as the server gate
// consults neither. So a parent whose slices_integrated append was lost — no
// record at all (#4165), or only an earlier between-wave record surviving a
// lost final append (#4221); the wedge fanInRecordLost names — is NOT read as
// authority-less.
//
// When authority is nil (an older backend, or a list read) it falls back to
// the pre-#4165 inference from the one observable consequence of a missing
// authority: NO fan-in record of any kind although the parent's implement
// stage already SUCCEEDED. An unreadable parent stage ("") is never read as
// authority-less, so a read failure cannot release a hold.
func integrationAuthorityAbsent(cs *ChildrenStatus, parentImplementState string, authority *runSliceIntegration) bool {
	if cs == nil {
		return false
	}
	if authority != nil {
		return !authority.Available
	}
	return !cs.fanInRecorded && parentImplementState == "succeeded"
}

// fanInRecordLost reports the lost-record wedge (#4165, generalised in
// #4221): the server HAS slice-integration authority, every child SUCCEEDED,
// the parent's implement stage already SUCCEEDED (so the terminal fan-in ran),
// no integration failure is newer than the newest clean integration, and yet
// the newest clean slices_integrated record does not cover every child. The
// orchestrator's slices_integrated append is best-effort (it WARN-logs a failed
// append), so the record can be lost while the stage advances. That covers
// both the single-wave case where NO record survives (#4165) and a multi-wave
// fan-out whose FINAL append was lost while an earlier between-wave record
// survives (#4221) — the no-record case is the special case where nothing is
// covered. The server gate then refuses acceptance 409
// acceptance_integration_incomplete forever and nothing re-integrates
// automatically; fishhawk_consolidate_slices answers 409 not_awaiting_children
// on the advanced parent, and the uncovered children already succeeded, so
// re-driving them is wrong. The recovery is POST
// /v0/runs/{run_id}/integrate-wave, the idempotent non-settling fan-in that
// rewrites the record without transitioning the stage.
//
// Each conjunct:
//   - authority.Available: a POSITIVE authority signal; with nil authority the
//     C3 inference decides instead (integrationAuthorityAbsent).
//   - parentImplementState == "succeeded": every settle path integrates BEFORE
//     stamping the stage succeeded (runConsolidation in server/consolidate.go,
//     and the childcompletion sweeper's resolveParent), so a succeeded stage
//     means the terminal fan-in already ran — an awaiting_children parent is
//     still integrating, not wedged.
//   - allChildrenSucceeded: "every child succeeded" must be provable; an
//     unknown, failed or running child takes the generic hold reason.
//   - IntegrationFailure == nil: a newer failure has its own recovery.
//   - len(UnintegratedChildRunIDs) > 0: with every child succeeded this is
//     exactly "the newest clean record does not cover every child".
func fanInRecordLost(cs *ChildrenStatus, parentImplementState string, authority *runSliceIntegration) bool {
	return cs != nil && authority != nil && authority.Available &&
		parentImplementState == "succeeded" && allChildrenSucceeded(cs.Children) &&
		cs.IntegrationFailure == nil && len(cs.UnintegratedChildRunIDs) > 0
}

// fanInRecordLostRecovery names the integrate-wave recovery for the
// lost-record wedge (fanInRecordLost), shared by the next_actions hold reason
// and fishhawk_await_children's integration_pending message so the two
// surfaces name the same move. The lead clause says which record is missing:
// with fanInRecordLost true, cs.fanInRecorded is true exactly when a clean
// slices_integrated exists (a failure with no clean record would have set
// IntegrationFailure), so it selects between "no record exists" (#4165) and
// "the newest record covers only an earlier wave" (#4221). Neither variant
// advises re-driving the uncovered children: they already succeeded.
func fanInRecordLostRecovery(runID string, cs *ChildrenStatus) string {
	lead := "no slices_integrated record exists although the parent's implement stage already succeeded and the server HAS slice-integration authority — " +
		"the best-effort slices_integrated append was lost (the orchestrator WARN-logs it)"
	if cs != nil && cs.fanInRecorded {
		lead = fmt.Sprintf("the newest slices_integrated record covers only an earlier wave and lacks the slices of %s, although every child and the parent's implement stage already succeeded "+
			"and the server HAS slice-integration authority — the final best-effort slices_integrated append was lost (the orchestrator WARN-logs it)",
			strings.Join(cs.UnintegratedChildRunIDs, ", "))
	}
	return fmt.Sprintf("%s. The server refuses an acceptance dispatch here with 409 acceptance_integration_incomplete "+
		"and nothing re-integrates automatically (fishhawk_consolidate_slices answers 409 not_awaiting_children on an advanced parent). "+
		"Recovery: POST /v0/runs/%s/integrate-wave (write:runs) — it re-runs the idempotent fan-in WITHOUT transitioning the stage and writes the missing record", lead, runID)
}

// foldAcceptanceIntegrationHold is the pure next_actions hold (#4080). When the
// run is a decomposed parent (cs != nil with at least one child) whose
// integration phase is not integrated, it removes every acceptance dispatch
// action, sets State to acceptance_held_integration_incomplete, and PREPENDS a
// fishhawk_await_children action whose reason names the uncovered children,
// the integration failure, or the lost fan-in record (fanInRecordLost: no
// record, #4165, or a lost final record in a multi-wave fan-out, #4221), and
// the server's 409.
//
// It is a no-op when:
//   - na or cs is nil (not a decomposed parent, or the snapshot was not read);
//   - the parent has no children (the server guard is inert there too);
//   - the phase is integrated (full coverage);
//   - no acceptance dispatch is offered;
//   - integrationAuthorityAbsent (C3: the server stands down, so the display
//     must not hold either — a deployment that cannot integrate is never
//     wedged by this hold). authority is the server's own predicate when the
//     backend surfaces it (#4165); nil falls back to the inference.
func foldAcceptanceIntegrationHold(runID string, cs *ChildrenStatus, parentImplementState string, authority *runSliceIntegration, na *NextActions) {
	if na == nil || cs == nil || len(cs.Children) == 0 {
		return
	}
	if cs.IntegrationPhase == integrationPhaseIntegrated {
		return
	}
	if !offersAcceptanceDispatch(na) {
		return
	}
	if integrationAuthorityAbsent(cs, parentImplementState, authority) {
		return
	}
	kept := make([]SuggestedAction, 0, len(na.Actions)+1)
	kept = append(kept, SuggestedAction{
		Action:       "fishhawk_await_children",
		Params:       map[string]string{"run_id": runID},
		Precondition: "this decomposed parent's consolidated branch does not carry every child's slice yet",
		Consumes:     "none",
		Reason:       acceptanceIntegrationHoldReason(runID, cs, parentImplementState, authority),
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

// acceptanceIntegrationHoldReason names what is blocking: the lost fan-in
// record (fanInRecordLost, #4165/#4221) first, then the integration failure
// when one is newer than the newest clean integration, otherwise the uncovered
// or non-succeeded children.
func acceptanceIntegrationHoldReason(runID string, cs *ChildrenStatus, parentImplementState string, authority *runSliceIntegration) string {
	if fanInRecordLost(cs, parentImplementState, authority) {
		return "acceptance held: " + fanInRecordLostRecovery(runID, cs) + "; then re-invoke fishhawk_await_children"
	}
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
//
// The no-authority decision keys on run.capabilities.slice_integration — the
// server gate's own predicate (#4165) — read off the run the caller already
// holds (zero extra reads); a run without it takes the C3 inference fallback.
// The parent's implement state is the NEWEST implement stage by Sequence, so a
// retried parent with two implement stages is not misread.
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
	if impl := newestStageByType(stages, "implement"); impl != nil {
		parentImplementState = impl.State
	}
	foldAcceptanceIntegrationHold(runID.String(), cs, parentImplementState, sliceIntegrationOf(run), na)
}
