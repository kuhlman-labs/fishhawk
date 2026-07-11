package campaign

// engine.go holds the pure campaign scheduling/reduction logic over persisted
// items: NextEligible partitions items by readiness, and DeriveState reduces
// item states to the campaign state. Both are side-effect-free functions over
// []*Item so they are unit-testable without a Repository (Postgres).

// Eligibility is the partition of a campaign's items by readiness, computed by
// NextEligible. Every slice holds issue refs (Item.IssueRef). An item appears
// in exactly one slice. Eligible items are the ones a scheduler may dispatch
// next; Blocked items wait on an unsatisfied dependency; Running/Done/Failed/
// Cancelled reflect items already linked to a run or terminal.
type Eligibility struct {
	// Eligible items have no run yet and every dependency already succeeded —
	// they are ready to dispatch autonomously.
	Eligible []string
	// HumanLed items have no run yet and every dependency already succeeded
	// (deps-satisfied, exactly like Eligible) but carry Autonomy=="low", the
	// tier the methodology reserves for human leadership. They are a disjoint
	// partition from Eligible: a deps-satisfied autonomy:low item is diverted
	// here instead of Eligible so the auto-driver (which keys on Eligible) never
	// mints an agent run on human-led work. A deps-UNsatisfied autonomy:low item
	// stays in Blocked, not HumanLed — HumanLed is the deps-satisfied-but-human-
	// led set only.
	HumanLed []string
	// Blocked items have no run yet but at least one dependency is not yet
	// done (or references an item absent from the campaign).
	Blocked []string
	// Running items are linked to a run that has not reached a terminal item
	// state.
	Running []string
	// Done items have succeeded.
	Done []string
	// Failed items have failed. A deps-satisfied, non-autonomy:low failed item
	// is diverted to Restartable (#1838) — it has a forward path via the operator
	// restart verb; only a genuinely-stuck failed item (deps-unsatisfied or
	// human-led) remains here.
	Failed []string
	// Cancelled items reached the terminal cancelled state. Like Done/Failed
	// they are never re-dispatched; tracked separately so a cancelled item with
	// no run and no deps is not mistaken for Eligible.
	Cancelled []string
	// Restartable holds cancelled OR failed items that a deps-satisfied,
	// non-autonomy:low DAG position makes eligible for an operator-driven restart
	// via the fishhawk_start_campaign_item_run verb (#1729 cancelled; #1838
	// failed). A cancelled/failed item is terminal for auto-dispatch — it is
	// NEVER in Eligible — but unlike Done it has a forward path: the operator verb
	// resets it to pending and mints a fresh run. It is a DISJOINT partition from
	// both Cancelled and Failed (each cancelled/failed item is in exactly one of
	// Restartable vs its terminal slice): an item lands here only when every
	// dependency has succeeded AND its autonomy tier is not "low" (human-led work
	// is never auto-surfaced for restart); otherwise it stays in Cancelled/Failed.
	// computeCampaignNextAction surfaces a Restartable item as start_run; the wire
	// rollup folds it back into the cancelled slice so the rollup contract is
	// unchanged.
	Restartable []string
	// Paused items were handed off to a human by the auto-driver (E25.7). A
	// paused item carries a RunID and a non-terminal state, so it must be
	// classified BEFORE the Running catch-all — otherwise it would be mistaken
	// for Running. It is never re-dispatched until a resume flips it back to
	// running.
	Paused []string
}

// NextEligible partitions a campaign's items into eligible / blocked /
// running / done / failed sets from each item's State, DependsOn edges, and
// run linkage (RunID).
//
// An item is Eligible when it has no run yet (RunID nil and a non-terminal,
// not-yet-running state) AND every dependency ref resolves to a Done
// (succeeded) item AND its autonomy tier is not "low". A deps-satisfied item
// carrying Autonomy=="low" is diverted to HumanLed instead — human-led work
// the auto-driver must never dispatch. It is Blocked when not yet run but at
// least one dependency is not yet done (regardless of autonomy tier). A
// dependency ref absent from the campaign is treated as not-satisfied
// (defensive): a campaign referencing a missing sibling stays blocked rather
// than dispatching against an unresolved edge.
//
// A cancelled item is terminal: it is reported in Cancelled and never Eligible,
// even with no run and no deps (which would otherwise fall through to the
// eligible default branch). A deps-satisfied, non-autonomy:low cancelled item
// is diverted to Restartable instead — still never Eligible (no auto-dispatch)
// but flagged as restartable via the operator verb (#1729).
//
// A failed item is likewise terminal for auto-dispatch (never Eligible), but a
// deps-satisfied, non-autonomy:low failed item is diverted to Restartable too
// (#1838) — mirroring the cancelled arm — so its dependents are not blocked
// forever; a deps-unsatisfied or human-led failed item stays in Failed.
func NextEligible(items []*Item) Eligibility {
	var e Eligibility

	// First pass: the set of refs whose item has succeeded. A dependency is
	// satisfied iff its ref is in this set; an absent ref is therefore
	// not-satisfied for free.
	done := make(map[string]bool, len(items))
	for _, it := range items {
		if it.State == ItemStateSucceeded {
			done[it.IssueRef] = true
		}
	}

	for _, it := range items {
		ref := it.IssueRef
		switch {
		case it.State == ItemStateFailed:
			// Terminal for AUTO-dispatch — a failed item is never Eligible. But,
			// mirroring the cancelled arm (#1729), a deps-satisfied, non-human-led
			// failed item has a forward path via the operator restart verb (#1838):
			// divert it to Restartable so next_action surfaces it as start_run and
			// its dependents no longer stay blocked forever. A deps-unsatisfied
			// item, or an autonomy:low (human-led) one, stays in Failed. Exactly one
			// of Restartable / Failed holds each failed item.
			switch {
			case depsSatisfied(it.DependsOn, done) && it.Autonomy != "low":
				e.Restartable = append(e.Restartable, ref)
			default:
				e.Failed = append(e.Failed, ref)
			}
		case it.State == ItemStateCancelled:
			// Terminal: never eligible for AUTO-dispatch, even with no run and no
			// deps (which would otherwise fall through to the default branch). But
			// a deps-satisfied, non-human-led cancelled item has a forward path via
			// the operator restart verb (#1729): divert it to Restartable so
			// next_action can surface it as start_run. A deps-unsatisfied item, or
			// an autonomy:low (human-led) one, stays in Cancelled. Exactly one of
			// Restartable / Cancelled holds each cancelled item.
			switch {
			case depsSatisfied(it.DependsOn, done) && it.Autonomy != "low":
				e.Restartable = append(e.Restartable, ref)
			default:
				e.Cancelled = append(e.Cancelled, ref)
			}
		case it.State == ItemStatePaused:
			// Paused (E25.7): carries a RunID and a non-terminal state, so it
			// MUST be classified before the Running catch-all below or it would
			// be mis-counted as Running. Never re-dispatched until resumed.
			e.Paused = append(e.Paused, ref)
		case it.State == ItemStateSucceeded:
			e.Done = append(e.Done, ref)
		case it.State == ItemStateRunning || (it.RunID != nil && !it.State.IsTerminal()):
			e.Running = append(e.Running, ref)
		default:
			// Not yet run (RunID nil, state pending/blocked). Eligible only
			// when every dependency has succeeded; a deps-satisfied autonomy:low
			// item is human-led, diverted to HumanLed so the auto-driver never
			// dispatches it. A deps-unsatisfied item stays Blocked regardless of
			// its autonomy tier.
			switch {
			case !depsSatisfied(it.DependsOn, done):
				e.Blocked = append(e.Blocked, ref)
			case it.Autonomy == "low":
				e.HumanLed = append(e.HumanLed, ref)
			default:
				e.Eligible = append(e.Eligible, ref)
			}
		}
	}
	return e
}

// depsSatisfied reports whether every dep ref is in the done set. An empty
// dep list is trivially satisfied; an absent ref is not-satisfied.
func depsSatisfied(deps []string, done map[string]bool) bool {
	for _, d := range deps {
		if !done[d] {
			return false
		}
	}
	return true
}

// DeriveState reduces a campaign's item states to the campaign state. It emits
// only pending / running / succeeded / failed:
//   - no items => pending;
//   - every item succeeded => succeeded;
//   - any item failed AND every item terminal => failed (a QUARANTINE rule: a
//     failed item fails the WHOLE campaign only when no forward progress
//     remains — every item has reached a terminal state; #1838);
//   - any progress (some running, succeeded, or failed) => running (a failed
//     item ALONGSIDE pending/eligible/running work keeps the campaign running so
//     the still-actionable siblings can be driven — the failed item is
//     quarantined, not campaign-terminal);
//   - otherwise (all pending, or pending/blocked with no progress) => pending.
//
// The quarantine rule (anyFailed && allTerminal -> Failed, otherwise a failed
// item is progress -> Running) is the #1838 fix: previously a single failed item
// forced StateFailed unconditionally, driving the whole campaign terminal even
// while eligible siblings remained — and a terminal campaign refuses every
// recovery verb, so the failed item could never be restarted. The anti-over-fix
// guard is the allTerminal conjunct: when NO item remains actionable (every item
// terminal) and at least one failed, the campaign is genuinely terminal-failed
// and still derives StateFailed. A single failed item (the whole campaign) is
// all-terminal, so it still derives Failed.
//
// StateCancelled and StatePaused are NOT derived here: they are operator/
// driver-set overlays (cancel = manual halt; paused = the E25.7 gate hand-off),
// and no item state implies them. A paused item is treated as
// non-succeeding/non-failing — it contributes to none of anyFailed/anyRunning/
// anySucceeded, is non-terminal (so it keeps allTerminal false), and makes
// allSucceeded false, exactly like a still-pending item — so derivation never
// emits StatePaused (the driver overlays it).
func DeriveState(items []*Item) State {
	if len(items) == 0 {
		return StatePending
	}
	var anyFailed, anyRunning, anySucceeded bool
	allSucceeded := true
	allTerminal := true
	for _, it := range items {
		switch it.State {
		case ItemStateFailed:
			anyFailed = true
		case ItemStateRunning:
			anyRunning = true
		case ItemStateSucceeded:
			anySucceeded = true
		}
		if it.State != ItemStateSucceeded {
			allSucceeded = false
		}
		if !it.State.IsTerminal() {
			allTerminal = false
		}
	}
	switch {
	case allSucceeded:
		return StateSucceeded
	case anyFailed && allTerminal:
		// Genuine terminal failure: at least one item failed and NO item remains
		// actionable — the campaign cannot make further progress, so it is failed.
		return StateFailed
	case anyRunning || anySucceeded || anyFailed:
		// Some work is in flight, partially done, OR a failed item sits alongside
		// still-actionable siblings (pending/eligible/running) — the campaign is
		// running so those siblings can be driven; the failed item is quarantined.
		return StateRunning
	default:
		// No progress, nothing running/failed: still pending.
		return StatePending
	}
}
