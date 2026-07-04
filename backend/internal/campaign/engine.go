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
	// HumanLed items have no run yet and every dependency satisfied — they would
	// be Eligible — but their autonomy tier is human-led (autonomy:low), a change
	// METHODOLOGY.md reserves for human authorship. They are routed here INSTEAD
	// of Eligible so the auto-driver (which dispatches only Eligible) never mints
	// an agent run for them, while a human is paged to lead/author the work
	// (#1551). A human-led item with an UNSATISFIED dependency stays Blocked,
	// exactly as before — only a would-be-eligible human-led item lands here.
	HumanLed []string
}

// IsHumanLed reports whether an autonomy tier is human-led — the low tier, which
// docs/METHODOLOGY.md ("Low autonomy (human-led)") reserves for human authorship
// and review. It is the single authoritative mapping consulted by NextEligible
// (to keep a human-led item out of the auto-dispatchable Eligible set) and by the
// server's next_action rung. Every other tier (empty/medium/high) is
// agent-drivable and returns false, so an item with no autonomy label is treated
// as auto-dispatchable — the unchanged pre-#1551 behavior.
func IsHumanLed(autonomy string) bool {
	return autonomy == "low"
}

// NextEligible partitions a campaign's items into eligible / blocked /
// running / done / failed sets from each item's State, DependsOn edges, and
// run linkage (RunID).
//
// An item is Eligible when it has no run yet (RunID nil and a non-terminal,
// not-yet-running state) AND every dependency ref resolves to a Done
// (succeeded) item. It is Blocked when not yet run but at least one dependency
// is not yet done. A dependency ref absent from the campaign is treated as
// not-satisfied (defensive): a campaign referencing a missing sibling stays
// blocked rather than dispatching against an unresolved edge.
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
			// Not yet run (RunID nil, state pending/blocked). Eligible only
			// when every dependency has succeeded. A would-be-eligible item whose
			// autonomy tier is human-led (autonomy:low) is routed to HumanLed
			// instead of Eligible so the auto-driver never dispatches it (#1551);
			// a human-led item with an unsatisfied dependency stays Blocked exactly
			// as before.
			switch {
			case !depsSatisfied(it.DependsOn, done):
				e.Blocked = append(e.Blocked, ref)
			case IsHumanLed(it.Autonomy):
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
