package mcpserver

import "fmt"

// Stage states retryStagePark classifies. Named rather than inlined so the
// state switch and the retryStageStateParks predicate cannot drift apart.
const (
	// stageStateAwaitingHostDispatch is the #1912 explicit local park: the
	// retry re-opened the stage to pending and the orchestrator Advance walked
	// it to this state because the run's runner is host-spawned (ADR-024), so
	// the backend has no channel to start it. NOTHING is driving the stage.
	stageStateAwaitingHostDispatch = "awaiting_host_dispatch"
	// stageStatePending is the DEGRADED shape: no Orchestrator was wired, or
	// its Advance errored (server/retry.go logs and deliberately does not fail
	// the request — "the audit row recorded the retry intent and the stage is
	// in pending"). Equally un-driven, and needs the same host dispatch.
	stageStatePending = "pending"
	// stageStateDispatched is the runner_kind github_actions / gitlab_ci
	// outcome: the orchestrator genuinely fired the fresh CI dispatch.
	stageStateDispatched = "dispatched"
	// stageStateAwaitingApproval is the category-D SLA-timeout re-open: a
	// GATE re-opened, not a dispatch.
	stageStateAwaitingApproval = "awaiting_approval"
	// stageStateAwaitingChildren is the #1891 decomposed-parent fan-in
	// restore: the childcompletion sweeper and /consolidate re-engage, so no
	// host dispatch belongs here.
	stageStateAwaitingChildren = "awaiting_children"
)

// retryStageStateParks reports whether a POST /v0/stages/{id}/retry response
// state means NOTHING is driving the re-opened stage, so the caller must make
// a separate host-dispatch call (E45.89 / #3624).
//
// It is the cheap predicate the retryStage handler tests BEFORE paying for the
// extra GetRun: only a parked state needs the run row (to choose
// fishhawk_run_children over fishhawk_dispatch_stage for a decomposition
// child), so the common CI path costs no additional round-trip.
func retryStageStateParks(state string) bool {
	switch state {
	case stageStateAwaitingHostDispatch, stageStatePending:
		return true
	default:
		return false
	}
}

// retryStagePark derives the fishhawk_retry_stage response's parked flag, its
// pre-filled follow-on call, and any warnings from the POST-RETRY STAGE STATE
// (E45.89 / #3624).
//
// That state is authoritative: server/retry.go's retryStageAs hands the
// pending re-open to Orchestrator.Advance and then RE-FETCHES the stage, so
// the response already reflects dispatched / awaiting_host_dispatch /
// awaiting_approval rather than the intermediate pending.
//
// Enumerated outcomes:
//
//   - awaiting_host_dispatch -> PARKED. The #1912 explicit local park.
//   - pending                -> PARKED. The degraded no-orchestrator /
//     Advance-errored shape; equally un-driven.
//   - dispatched             -> NOT parked. The CI kinds' workflow_dispatch
//     genuinely fired.
//   - awaiting_approval      -> NOT parked. The category-D SLA-timeout
//     re-open is a gate, not a dispatch.
//   - awaiting_children      -> NOT parked. The #1891 decomposed-parent
//     fan-in restore re-engages the sweeper.
//   - anything else          -> NOT parked, no next_step. Fail QUIET rather
//     than emit a pointer derived from a state this function does not
//     understand.
//
// On a parked state the verb is chosen from runRow: a non-nil DecomposedFrom
// selects fishhawk_run_children keyed on the PARENT run id (dispatch_stage
// checks out main and so cannot see a depends_on slice's dependency),
// otherwise fishhawk_dispatch_stage keyed on (run_id, stage).
//
// FAIL OPEN on a run-read failure (runReadErr != nil or runRow == nil): the
// park is already PROVEN by the stage state, so still emit the
// fishhawk_dispatch_stage pointer and append ONE warning saying the
// decomposition-child check was skipped. Swallowing the pointer would put the
// caller back in the poll-a-stage-nothing-is-driving shape this exists to
// close.
//
// Pure: no context, no I/O, so every branch is table-testable.
func retryStagePark(stage Stage, runRow *Run, runReadErr error) (bool, *SuggestedAction, []string) {
	if !retryStageStateParks(stage.State) {
		return false, nil, nil
	}

	reason := fmt.Sprintf(
		"the retry re-opened the stage but it parked at %s — nothing is driving it until you dispatch it from the host",
		stage.State)

	if runReadErr != nil || runRow == nil {
		return true, &SuggestedAction{
			Action:       "fishhawk_dispatch_stage",
			Params:       map[string]string{"run_id": stage.RunID, "stage": stage.Type},
			Precondition: "the stage is parked pre-dispatch (awaiting_host_dispatch or pending) and no sibling stage is in flight",
			Consumes:     "none",
			Reason:       reason,
		}, []string{
			"could not read the run row, so the decomposition-child check was skipped: " +
				"if this stage belongs to a decomposed CHILD run, prefer fishhawk_run_children on the parent run id over fishhawk_dispatch_stage",
		}
	}

	if runRow.DecomposedFrom != nil {
		return true, &SuggestedAction{
			Action:       "fishhawk_run_children",
			Params:       map[string]string{"parent_run_id": *runRow.DecomposedFrom},
			Precondition: "this stage belongs to a decomposed child run whose stage is parked pre-dispatch, which run_children admits",
			Consumes:     "none",
			Reason:       reason + " — this is a decomposition child, so re-spawn it through the parent's fan-out rather than dispatch_stage (which checks out main and cannot see a depends_on slice's dependency)",
		}, nil
	}

	return true, &SuggestedAction{
		Action:       "fishhawk_dispatch_stage",
		Params:       map[string]string{"run_id": stage.RunID, "stage": stage.Type},
		Precondition: "the stage is parked pre-dispatch (awaiting_host_dispatch or pending) and no sibling stage is in flight",
		Consumes:     "none",
		Reason:       reason,
	}, nil
}
