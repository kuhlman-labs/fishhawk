package campaign

import "fmt"

// campaignTransitions enumerates allowed Campaign state transitions. Any
// (from, to) not present here is rejected. Same-state transitions
// (idempotent re-apply) are handled in ValidCampaignTransition, not here.
//
// pending → running:   the first item dispatched.
// pending → succeeded:  an all-human-led campaign whose every item completed
//
//	out of band (each issue closed-as-completed and settled run-less by the
//	reconcile-on-read pass, #1558) can terminate succeeded WITHOUT a single
//	dispatched run — DeriveState returns StateSucceeded for allSucceeded, so
//	without this edge such a campaign would stay stuck pending.
//
// pending → cancelled:  manually halted before any item ran.
// pending → failed:     setup-time failure (e.g. DAG invalid before dispatch).
// running → succeeded:  every item reached succeeded.
// running → failed:     a terminal item failure ended the campaign.
// running → cancelled:  manually halted mid-flight.
// running → paused:     the auto-driver handed a gate off to a human (E25.7).
// paused  → running:    a human resumed the campaign after handling the gate.
// paused  → cancelled:  manually halted while paused.
//
// THE awaiting_human EDGES (E72.33 / #3660). awaiting_human is DERIVED and
// NON-terminal, so the table must admit both the edges INTO it and — crucially —
// the edges back OUT, because deriveCampaignAfterChange silently DROPS a
// derivation whose transition this table refuses:
//
// pending → awaiting_human: an all-human-led campaign derives it before any
//
//	dispatch (DeriveState's new arm fires with zero progress items).
//
// running → awaiting_human: the agent-drivable work finished and only human-led
//
//	items remain open — the #3660 headline case.
//
// awaiting_human → running:  a human-led item was relabelled and STARTED, or a
//
//	settle made a sibling eligible on a campaign that already HAS progress
//	(anySucceeded/anyRunning/anyFailed holds), so DeriveState's progress arm
//	returns Running.
//
// awaiting_human → pending:  the relabel-WITHOUT-start corner. On a NEVER-STARTED
//
//	campaign no item has succeeded, run or failed, so when the operator relabels
//	an autonomy:low item to autonomy:medium the partition gains an Eligible item,
//	the awaiting_human arm correctly stops firing, and DeriveState falls through
//	to its DEFAULT branch returning StatePending. Without this edge
//	deriveCampaignAfterChange's !ValidCampaignTransition guard drops that
//	derivation and the row STICKS at awaiting_human while next_action, computed
//	from the same refreshed partition, advertises start_run — the state/action
//	contradiction #2681 exists to prevent. The status-read path genuinely reaches
//	this: an autonomy refresh alone makes settleIssueClosedItems report
//	refreshedAny, so reconcileCampaignItemsOnRead re-lists and calls
//	deriveCampaignAfterChange.
//
// awaiting_human → succeeded: the last human-led item closed-as-completed and was
//
//	settled run-less by the reconcile-on-read pass.
//
// awaiting_human → failed:    the last open item settled failed/cancelled, so
//
//	anyFailed && allTerminal now holds.
//
// awaiting_human → cancelled: the operator cancel verb, whose campaign transition
//
//	runs last and must not be refused.
//
// DELIBERATELY ABSENT:
//
//   - paused → awaiting_human: a paused campaign stays sticky. The server read
//     path has its own sticky-paused guard in reconcileCampaignItemsOnRead; this
//     table refusing the edge is the backstop for every other caller.
//   - awaiting_human → paused: the pause/page path only runs over a campaign the
//     driver swept as `running`, and an awaiting_human campaign has no running
//     item whose gate could be paged.
var campaignTransitions = map[State]map[State]struct{}{
	StatePending: {
		StateRunning:       {},
		StateAwaitingHuman: {},
		StateSucceeded:     {},
		StateCancelled:     {},
		StateFailed:        {},
	},
	StateRunning: {
		StateSucceeded:     {},
		StateFailed:        {},
		StateCancelled:     {},
		StatePaused:        {},
		StateAwaitingHuman: {},
	},
	StatePaused: {
		StateRunning:   {},
		StateCancelled: {},
	},
	StateAwaitingHuman: {
		StateRunning:   {},
		StatePending:   {},
		StateSucceeded: {},
		StateFailed:    {},
		StateCancelled: {},
	},
}

// ValidCampaignTransition reports whether transitioning from→to is
// permitted. Same-state transitions are treated as valid no-ops so callers
// can be idempotent.
func ValidCampaignTransition(from, to State) bool {
	if from == to {
		return true
	}
	if from.IsTerminal() {
		return false
	}
	_, ok := campaignTransitions[from][to]
	return ok
}

// campaignItemTransitions enumerates allowed CampaignItem state
// transitions. Same-state transitions are handled in
// ValidCampaignItemTransition, not here.
//
// pending → blocked:   depends_on edges are not yet satisfied.
// pending → running:   no open dependencies; the item's run dispatched.
// pending → succeeded: a human-led item completed OUT OF BAND — its issue was
//
//	closed-as-completed (e.g. merged by a maintainer PR) and the run-less
//	reconcile-on-read settle pass (#1558) settles it succeeded WITHOUT it
//	ever running.
//
// pending → cancelled: manually halted before running — OR (#3563) settled by
//
//	the reconcile-on-read issue-closed pass because the item's issue was closed
//	not_planned/duplicate: a NON-manual producer of this edge. Such a settle
//	additionally stamps resolved_by=issue_closed, which suppresses the
//	Restartable offer NextEligible would otherwise make.
//
// pending → failed:    setup-time failure.
// blocked → pending:   a dependency cleared; the item is admissible again.
// blocked → running:   the last dependency cleared and the run dispatched.
// blocked → succeeded: same run-less out-of-band settlement (#1558) for an item
//
//	that was blocked when its issue closed-as-completed (its deps having since
//	cleared, the settle pass only fires on a deps-satisfied item).
//
// blocked → cancelled: manually halted while blocked — OR, like the pending
//
//	edge above, the #3563 issue-closed settle of a not_planned/duplicate
//	closure, which is likewise non-manual and stamps resolved_by=issue_closed.
//
// running → succeeded: the item's run succeeded.
// running → failed:    the item's run failed.
// running → cancelled: manually halted mid-flight.
// running → paused:    the auto-driver handed the item's gate off to a human.
// paused  → running:   resumed after the gate was handled (re-engages next tick).
// paused  → cancelled: manually halted while paused.
var campaignItemTransitions = map[ItemState]map[ItemState]struct{}{
	ItemStatePending: {
		ItemStateBlocked:   {},
		ItemStateRunning:   {},
		ItemStateSucceeded: {},
		ItemStateCancelled: {},
		ItemStateFailed:    {},
	},
	ItemStateBlocked: {
		ItemStatePending:   {},
		ItemStateRunning:   {},
		ItemStateSucceeded: {},
		ItemStateCancelled: {},
	},
	ItemStateRunning: {
		ItemStateSucceeded: {},
		ItemStateFailed:    {},
		ItemStateCancelled: {},
		ItemStatePaused:    {},
	},
	ItemStatePaused: {
		ItemStateRunning:   {},
		ItemStateCancelled: {},
	},
}

// ValidCampaignItemTransition reports whether transitioning from→to is
// permitted. Idempotent same-state re-application is allowed.
func ValidCampaignItemTransition(from, to ItemState) bool {
	if from == to {
		return true
	}
	if from.IsTerminal() {
		return false
	}
	_, ok := campaignItemTransitions[from][to]
	return ok
}

// InvalidTransitionError describes a refused state transition. Callers can
// errors.Is/As against it to surface a 409 Conflict at the HTTP layer.
// Mirrors run.InvalidTransitionError.
type InvalidTransitionError struct {
	Kind string // "campaign" or "campaign_item"
	From string
	To   string
}

func (e InvalidTransitionError) Error() string {
	return fmt.Sprintf("invalid %s transition: %s → %s", e.Kind, e.From, e.To)
}
