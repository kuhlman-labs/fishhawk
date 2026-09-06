package run

import "fmt"

// AcceptanceBlockerAdvisory renders the operator-facing sentence for a
// NON-TERMINAL acceptance stage that is holding something back — the review
// gate a fix-up wants to re-park, the audit-complete check, or a queued merge
// (E64.63 / #3222).
//
// KEEP IN SYNC — this function is the SINGLE OWNER of this wording. Before
// #3222 the same four shapes were spelled out independently on the surfaces
// that bothered to speak at all, and the two that did not (fishhawk_merge_run
// and next_actions) said nothing while the operator burned admin bypasses.
// Every peer now COMPOSES this renderer rather than re-spelling it:
//
//   - run/fixup.go findOpenReviewStage — the ErrFixupNotApplicable refusal.
//   - auditcomplete.stageNotTerminalDetail — the fishhawk_audit_complete
//     check's `stage_not_terminal` detail.
//   - mcpserver/merge_run.go acceptanceBlockerAdvisoryFor — the timeout and
//     checks_pending merge checkpoints.
//   - mcpserver/next_actions.go foldAcceptanceRedispatchAdvisory — the
//     merge-ritual arms of the next_actions block.
//
// The rendering is split on TWO axes, and both splits are load-bearing:
//
//   - reopened: whether a fix-up push RE-OPENED a settled acceptance verdict
//     (#1682). The caller establishes this by correlating an
//     `acceptance_reopened` audit entry with THIS stage's id — never by
//     category alone, because a run can carry more than one acceptance stage
//     in its history and a stale entry from an earlier one would draw the
//     stronger "a fix-up re-opened this" claim with nothing supporting it
//     (#3222 binding condition 1). When it cannot be established the GENERIC
//     shape is rendered: still true, just not as sharp.
//   - dispatchable: whether the operator can still DISPATCH the stage or can
//     only WAIT for a re-run already in flight (AcceptanceIsDispatchable).
//     Naming a remedy the operator cannot take is its own defect class — it
//     sends them to redo work with false authority — so the in-flight shapes
//     NEVER name a dispatch (#3116, #3224, #3222 binding condition 2).
//
// A TERMINAL state renders the empty string: nothing is blocked, so every
// composing surface stays byte-identical to what it emitted before #3222.
//
// shortStageID is the stage's short id; an empty value omits the id clause
// (the fix-up refusal names its blocker by state, not by id). tail is
// appended VERBATIM, and is how each surface says what clears it — the
// check-clears sentence, the queued-merge sentence, the route-the-fix-up
// sentence.
func AcceptanceBlockerAdvisory(shortStageID string, state StageState, reopened bool, tail string) string {
	if state.IsTerminal() {
		return ""
	}
	idClause := ""
	if shortStageID != "" {
		idClause = " " + shortStageID
	}
	dispatchable := AcceptanceIsDispatchable(state)
	switch {
	case reopened && dispatchable:
		return fmt.Sprintf("acceptance stage%s was re-opened by a fix-up push and has not been re-run; "+
			"the prior acceptance verdict is invalidated. Re-dispatch the acceptance stage "+
			"(fishhawk_dispatch_stage, stage acceptance)%s", idClause, tail)
	case reopened:
		return fmt.Sprintf("acceptance stage%s was re-opened by a fix-up push and its re-run is already in flight "+
			"(state %s); the prior acceptance verdict is invalidated. Wait for the re-run to settle%s",
			idClause, state, tail)
	case dispatchable:
		return fmt.Sprintf("the acceptance stage%s (state %s) must settle first. Dispatch the acceptance stage "+
			"(fishhawk_dispatch_stage, stage acceptance) and let it settle%s", idClause, state, tail)
	default:
		return fmt.Sprintf("the acceptance stage%s is already in flight (state %s). Wait for acceptance to settle%s",
			idClause, state, tail)
	}
}

// AcceptanceIsDispatchable reports whether a blocking acceptance stage is one
// the operator can still DISPATCH (no spawn attempt exists yet) rather than
// one already in flight, which they can only wait on.
//
// The two pre-dispatch park states are `pending` (the state
// ReopenAcceptanceStage writes) and `awaiting_host_dispatch` (a local run
// parked for the host spawn). Every other non-terminal state — `dispatched`,
// `running`, any other `awaiting_*` — means the re-run already exists, so
// naming a re-dispatch there would be untrue.
//
// This is the ONE exported owner of the split (#3222). It replaced
// run.acceptanceIsDispatchable (#3116) and
// auditcomplete.acceptanceAwaitsRedispatch (#3190/#3224), which were
// byte-identical copies of each other that the compiler could not keep in
// step.
func AcceptanceIsDispatchable(s StageState) bool {
	return s == StageStatePending || s == StageStateAwaitingHostDispatch
}
