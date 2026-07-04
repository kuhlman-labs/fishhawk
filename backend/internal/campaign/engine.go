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
	// they are ready to dispatch.
	Eligible []string
	// Blocked items have no run yet but at least one dependency is not yet
	// done (or references an item absent from the campaign).
	Blocked []string
	// Running items are linked to a run that has not reached a terminal item
	// state.
	Running []string
	// Done items have succeeded.
	Done []string
	// Failed items have failed.
	Failed []string
	// Cancelled items reached the terminal cancelled state. Like Done/Failed
	// they are never re-dispatched; tracked separately so a cancelled item with
	// no run and no deps is not mistaken for Eligible.
	Cancelled []string
	// Paused items were handed off to a human by the auto-driver (E25.7). A
	// paused item carries a RunID and a non-terminal state, so it must be
	// classified BEFORE the Running catch-all — otherwise it would be mistaken
	// for Running. It is never re-dispatched until a resume flips it back to
	// running.
	Paused []string
	// HumanLed items are eligible in every DAG sense (no run yet, every
	// dependency satisfied) but carry a human-led autonomy tier (autonomy:low
	// per METHODOLOGY.md), so they are NOT autonomously dispatchable (#1551).
	// They are kept OUT of Eligible — which the auto-driver's start path and the
	// start_run next_action both key on — so a human-led item never mints an
	// agent run; a human leads/authors it instead. A human-led item with an
	// UNSATISFIED dependency stays Blocked (it is not yet actionable by anyone),
	// exactly as before.
	HumanLed []string
}

// IsHumanLed reports whether an autonomy tier denotes human-led work — the
// single authoritative place mapping the tier string to the human-led vs
// agent-drivable split. METHODOLOGY.md defines 'Low autonomy (human-led)': the
// human is author and reviewer of record, so autonomy:low is human-led while
// medium/high are agent-drivable. An empty (unlabeled) or unrecognized tier is
// NOT human-led — it falls through to the autonomous path, the additive-column
// default that preserves pre-#1551 behavior for items carrying no autonomy.
func IsHumanLed(autonomy string) bool {
	return autonomy == "low"
}

// NextEligible partitions a campaign's items into eligible / blocked /
// running / done / failed sets from each item's State, DependsOn edges, and
// run linkage (RunID).
//
// An item is Eligible when it has no run yet (RunID nil and a non-terminal,
// not-yet-running state), every dependency ref resolves to a Done (succeeded)
// item, AND it is not human-led. A dependency-satisfied item that IS human-led
// (autonomy:low) lands in HumanLed instead of Eligible, so the auto-driver and
// the start_run next_action (both keyed on Eligible) never mint an agent run
// for it — a human leads it. It is Blocked when not yet run but at least one
// dependency is not yet done (autonomy is irrelevant here — an unactionable
// item stays Blocked whether human-led or not). A dependency ref absent from
// the campaign is treated as not-satisfied (defensive): a campaign referencing
// a missing sibling stays blocked rather than dispatching against an unresolved
// edge.
//
// A cancelled item is terminal: it is reported in Cancelled and never Eligible,
// even with no run and no deps (which would otherwise fall through to the
// eligible default branch).
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
			e.Failed = append(e.Failed, ref)
		case it.State == ItemStateCancelled:
			// Terminal: never eligible for dispatch, even with no run and no
			// deps (which would otherwise fall through to the default branch).
			e.Cancelled = append(e.Cancelled, ref)
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
			// Not yet run (RunID nil, state pending/blocked). Actionable only
			// when every dependency has succeeded; otherwise Blocked.
			switch {
			case !depsSatisfied(it.DependsOn, done):
				// An unsatisfied dependency blocks the item regardless of
				// autonomy — a human-led item is not yet actionable by anyone.
				e.Blocked = append(e.Blocked, ref)
			case IsHumanLed(it.Autonomy):
				// Dependencies satisfied but human-led (autonomy:low): route to
				// HumanLed, NOT Eligible, so it is never autonomously dispatched
				// (#1551). The driver's start path keys on Eligible, so this
				// auto-skips it while DAG-independent autonomous items keep
				// starting.
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
//   - any item failed => failed (a terminal item failure fails the campaign);
//   - every item succeeded => succeeded;
//   - any item running, or partial progress (some succeeded, not all) => running;
//   - otherwise (all pending, or pending/blocked with no progress) => pending.
//
// StateCancelled and StatePaused are NOT derived here: they are operator/
// driver-set overlays (cancel = manual halt; paused = the E25.7 gate hand-off),
// and no item state implies them. A paused item is treated as
// non-succeeding/non-failing — it contributes to none of anyFailed/anyRunning/
// anySucceeded and makes allSucceeded false, exactly like a still-pending item
// — so derivation never emits StatePaused (the driver overlays it).
func DeriveState(items []*Item) State {
	if len(items) == 0 {
		return StatePending
	}
	var anyFailed, anyRunning, anySucceeded bool
	allSucceeded := true
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
	}
	switch {
	case anyFailed:
		return StateFailed
	case allSucceeded:
		return StateSucceeded
	case anyRunning || anySucceeded:
		// Some work is in flight or partially done — the campaign is running.
		return StateRunning
	default:
		// No failure, no progress, nothing running: still pending.
		return StatePending
	}
}
