// Package latency derives a run's gate-latency (wait-on-human) rollup from
// the timestamps of its audit chain. It is the latency counterpart to the
// cost aggregator (`backend/internal/cost`): pure, deterministic, no
// repository and no clock. The caller maps audit entries to GateEvents in
// audit-chain order (the order ListForRun returns) and folds them via
// AggregateGateLatency.
//
// Three human gates are measured, each as the wall-clock delta between a pair
// of category boundaries in the chain:
//
//   - plan_approval:                first plan_generated → the first following
//     approval_submitted (the wait for a plan reviewer to approve).
//   - implement_review_to_dispatch: the LATEST implement_reviewed (a
//     feature_change run has two reviewers, so the terminal review is the last
//     one) → the first following acceptance_dispatched, falling back to
//     pr_merged when the workflow has no acceptance stage. There is no
//     dedicated stage-dispatch audit category, so the next stage's dispatch
//     closes the interval — a deliberate approximation: a run whose acceptance
//     stage was skipped shows the review→merge span for this gate.
//   - checks_green_to_merge:        the LATEST ci_green → the first following
//     pr_merged (the wait between checks going green and the merge).
//
// A gate whose opening OR closing marker is absent from the chain is OMITTED
// (partial rollup) rather than reported as a zero-length gate. A present pair
// whose closing timestamp precedes its opening timestamp (clock skew /
// out-of-order writers) is emitted with its wait CLAMPED to 0 rather than a
// negative wait. DISPLAY-ONLY — derived from existing audit data, never a
// gate on the run.
package latency

import "time"

// Gate-boundary audit categories. CategoryCIGreen is synthetic: there is no
// bare `ci_green` audit row — the caller synthesizes a CategoryCIGreen event
// from the `run_auto_advanced` entry whose payload rule is
// `checks_green_awaiting_merge` (drive.RuleChecksGreenAwaitingMerge). The
// remaining categories are real audit categories emitted by the backend.
const (
	CategoryPlanGenerated        = "plan_generated"
	CategoryApprovalSubmitted    = "approval_submitted"
	CategoryImplementReviewed    = "implement_reviewed"
	CategoryAcceptanceDispatched = "acceptance_dispatched"
	CategoryPRMerged             = "pr_merged"
	CategoryCIGreen              = "ci_green"
)

// Gate names, in the deterministic order AggregateGateLatency emits them.
const (
	GatePlanApproval              = "plan_approval"
	GateImplementReviewToDispatch = "implement_review_to_dispatch"
	GateChecksGreenToMerge        = "checks_green_to_merge"
)

// GateEvent is one audit-chain entry reduced to the two fields the latency
// aggregator needs: its Category and its Timestamp. The self-contained type
// keeps this package free of any dependency on the audit package (mirroring
// how cost.RunCostEntry decouples the cost aggregator from the ledger row).
type GateEvent struct {
	Category  string
	Timestamp time.Time
}

// GateWait is one measured human gate: the interval between its opening and
// closing markers. WaitSeconds is ClosedAt−OpenedAt in seconds, clamped to 0
// when the delta is negative.
type GateWait struct {
	Gate        string    `json:"gate"`
	OpenedAt    time.Time `json:"opened_at"`
	ClosedAt    time.Time `json:"closed_at"`
	WaitSeconds float64   `json:"wait_seconds"`
}

// Rollup is the per-run gate-latency summary: the present gate waits (in the
// deterministic Gate order), their sum as the total wait on human decisions,
// and the run's end-to-end wall clock. DISPLAY-ONLY.
type Rollup struct {
	Gates                   []GateWait `json:"gates,omitempty"`
	TotalWaitOnHumanSeconds float64    `json:"total_wait_on_human_seconds"`
	WallClockSeconds        float64    `json:"wall_clock_seconds"`
}

// AggregateGateLatency folds a run's gate events into the per-run Rollup. It
// is pure (no repository, no error, no clock): the same events + bounds always
// yield the same Rollup.
//
// events MUST be supplied in audit-chain order (ascending sequence, as
// ListForRun returns) — the boundary resolution is order-sensitive ("first
// following", "latest"). WallClockSeconds is runEnd−runStart clamped to 0 so a
// zero-value or reversed bound never yields a negative wall clock. An empty
// event slice yields a Rollup with no gates and TotalWaitOnHumanSeconds 0.
func AggregateGateLatency(events []GateEvent, runStart, runEnd time.Time) Rollup {
	var r Rollup

	// plan_approval: first plan_generated → first following approval_submitted.
	// "First" (not latest) so a later deploy approval_submitted — the same
	// category is emitted for plan and deploy approvals — can never be
	// mis-attributed to the plan gate.
	if openIdx := firstOf(events, CategoryPlanGenerated); openIdx >= 0 {
		closeIdx := firstAfter(events, openIdx, CategoryApprovalSubmitted)
		if w, ok := gateWait(GatePlanApproval, openIdx, closeIdx, events); ok {
			r.Gates = append(r.Gates, w)
		}
	}

	// implement_review_to_dispatch: LATEST implement_reviewed → first following
	// acceptance_dispatched, falling back to pr_merged when the workflow has no
	// acceptance stage.
	if openIdx := lastOf(events, CategoryImplementReviewed); openIdx >= 0 {
		closeIdx := firstAfter(events, openIdx, CategoryAcceptanceDispatched)
		if closeIdx < 0 {
			closeIdx = firstAfter(events, openIdx, CategoryPRMerged)
		}
		if w, ok := gateWait(GateImplementReviewToDispatch, openIdx, closeIdx, events); ok {
			r.Gates = append(r.Gates, w)
		}
	}

	// checks_green_to_merge: LATEST ci_green → first following pr_merged.
	if openIdx := lastOf(events, CategoryCIGreen); openIdx >= 0 {
		closeIdx := firstAfter(events, openIdx, CategoryPRMerged)
		if w, ok := gateWait(GateChecksGreenToMerge, openIdx, closeIdx, events); ok {
			r.Gates = append(r.Gates, w)
		}
	}

	for _, g := range r.Gates {
		r.TotalWaitOnHumanSeconds += g.WaitSeconds
	}
	if wall := runEnd.Sub(runStart).Seconds(); wall > 0 {
		r.WallClockSeconds = wall
	}
	return r
}

// gateWait builds a GateWait from an opening/closing index pair. It reports
// ok=false — the gate is OMITTED — when either marker is absent (index < 0).
// A negative delta (closing before opening) is clamped to a 0 wait rather than
// surfaced as a negative number.
func gateWait(gate string, openIdx, closeIdx int, events []GateEvent) (GateWait, bool) {
	if openIdx < 0 || closeIdx < 0 {
		return GateWait{}, false
	}
	opened := events[openIdx].Timestamp
	closed := events[closeIdx].Timestamp
	wait := closed.Sub(opened).Seconds()
	if wait < 0 {
		wait = 0
	}
	return GateWait{Gate: gate, OpenedAt: opened, ClosedAt: closed, WaitSeconds: wait}, true
}

// firstOf returns the index of the first event with the given category, or -1.
func firstOf(events []GateEvent, category string) int {
	for i := range events {
		if events[i].Category == category {
			return i
		}
	}
	return -1
}

// firstAfter returns the index of the first event with the given category
// strictly after index `after`, or -1. `after < 0` scans from the start.
func firstAfter(events []GateEvent, after int, category string) int {
	for i := after + 1; i < len(events); i++ {
		if events[i].Category == category {
			return i
		}
	}
	return -1
}

// lastOf returns the index of the last event with the given category, or -1.
func lastOf(events []GateEvent, category string) int {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Category == category {
			return i
		}
	}
	return -1
}
