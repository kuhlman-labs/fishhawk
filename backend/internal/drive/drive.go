// Package drive classifies a run's named transition points as
// mechanical (auto-advanced when the run opted into drive mode,
// #1023 / #996 theme 1) or judgment (always parked for the operator),
// and emits the run_auto_advanced audit entry that makes every
// auto-advance attributable to a named rule.
//
// The package is deliberately small: it owns the rule table, the
// audit payload shape, and the emission/dedup helpers. The hook
// points that decide WHEN a rule fires live with the transitions they
// stamp (server approval/fixup handlers, the mergereconciler poll) —
// drive never performs a state transition itself, so a bug here can
// mis-record but never mis-advance a run.
package drive

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Category is the audit-log category for one auto-advanced (or
// parked-with-next-action) transition on a drive-enabled run. Audit
// categories are free strings; GET /v0/runs/{run_id} distills its
// auto_advanced list and next_action from entries of this category.
const Category = "run_auto_advanced"

// Rule names one transition point the drive engine classifies. The
// closed set below is the v0 table; rule names are persisted in audit
// payloads, so renaming one is a breaking change for readers.
type Rule string

// Mechanical rules — transitions a drive-enabled run auto-advances.
const (
	// RulePlanApprovedDispatch covers plan gate approved → implement
	// stage dispatched (workflow_dispatch for runner_kind
	// github_actions; a parked ready-to-run next action for local,
	// where the runner is host-spawned per ADR-024 and the backend has
	// no execution channel to it).
	RulePlanApprovedDispatch Rule = "plan_approved_dispatch"
	// RuleReviseReplan covers the plan-gate revise verdict re-opening the
	// plan stage (awaiting_approval → pending) for a re-plan in place. Like
	// RulePlanApprovedDispatch it is mechanical: the operator already
	// expressed intent by calling revise_plan with a binding constraint, so
	// the re-dispatch is a mechanical transition, not a judgment point. For
	// runner_kind github_actions the orchestrator's workflow_dispatch edge
	// is the re-run (auto-advance); local parks with a host-side
	// run_plan_stage next action, because the runner is host-spawned per
	// ADR-024 and the backend has no execution channel to it.
	RuleReviseReplan Rule = "revise_replan"
	// RuleReviewsSettledGate covers every configured agent review for
	// a stage reaching a terminal state, so the gate evaluation
	// proceeds without an operator await/poll.
	RuleReviewsSettledGate Rule = "reviews_settled_gate"
	// RuleFixupRereviewRepark covers the fix-up flow's re-park of the
	// review gate (awaiting_approval → pending) so the re-dispatched
	// implement stage flows back into a fresh review.
	RuleFixupRereviewRepark Rule = "fixup_rereview_repark"
	// RuleChecksGreenAwaitingMerge covers all review evidence terminal
	// + required PR checks green → the derived awaiting_merge
	// presentation status with a distilled merge next action. The
	// merge itself stays a judgment point (RuleMerge).
	RuleChecksGreenAwaitingMerge Rule = "checks_green_awaiting_merge"
	// RuleCIFailed is the negative mirror of
	// RuleChecksGreenAwaitingMerge: all review evidence terminal but a
	// required PR check concluded red → the derived ci_failed
	// presentation status, parking the run with a classify-next-action.
	// Detection only (ADR-040 bucket 1, zero judgment): it parks and
	// never advances — the remediation (fix-up, an operator commit +
	// vouch, or a checks re-run) stays the operator's call.
	RuleCIFailed Rule = "ci_failed"
	// RuleChildrenDispatch covers a decomposed parent parked in
	// awaiting_children dispatching its pending child runs up to the
	// resolved concurrency cap (E24.3 / ADR-041). The orchestrator's
	// DispatchDecomposedChildren picks how many pending children to
	// dispatch (consuming the E24.6 budget.ParallelDecision contract)
	// and advances each via the existing runner-kind-aware Advance path:
	// github_actions auto-advances (the Advance handoff IS the dispatch);
	// local parks each child with a host-side run_implement_stage next
	// action (the backend cannot host-spawn the local runner, ADR-024).
	// This rule is the observability/classification layer only — it
	// performs no state transition.
	RuleChildrenDispatch Rule = "children_dispatch"
)

// Judgment points — never auto-advanced, drive or not. Enumerated so
// the classification is an explicit closed table rather than an
// absence of code.
const (
	// RuleGateApproval is a human approval gate decision.
	RuleGateApproval Rule = "gate_approval"
	// RuleConcernRouting is the operator's selection of review
	// concerns to route back to the agent (fix-up trigger).
	RuleConcernRouting Rule = "concern_routing"
	// RuleMerge is the PR merge (absent ADR-040 delegation).
	RuleMerge Rule = "merge"
)

// mechanical is the closed classification table (#1023): true rules
// auto-advance under drive; false rules always park for the operator.
var mechanical = map[Rule]bool{
	RulePlanApprovedDispatch:     true,
	RuleReviseReplan:             true,
	RuleReviewsSettledGate:       true,
	RuleFixupRereviewRepark:      true,
	RuleChecksGreenAwaitingMerge: true,
	RuleCIFailed:                 true,
	RuleChildrenDispatch:         true,
	RuleGateApproval:             false,
	RuleConcernRouting:           false,
	RuleMerge:                    false,
}

// Mechanical reports whether rule is a mechanical transition
// (auto-advance under drive) as opposed to a judgment point. Unknown
// rules classify as judgment — fail-parked, never fail-advanced.
func Mechanical(rule Rule) bool { return mechanical[rule] }

// NextAction is the distilled operator next step recorded on a
// run_auto_advanced payload. GET /v0/runs/{run_id} surfaces the most
// recent one so the operator sees what (if anything) the run is
// waiting on them for.
type NextAction struct {
	Action string `json:"action"`
	Detail string `json:"detail,omitempty"`
	PRURL  string `json:"pr_url,omitempty"`
}

// Outcome is the engine's decision for one evaluated transition
// point: Advance true means the mechanical transition was (or is
// being) auto-advanced by the existing edge; false means the run
// parks, with NextAction telling the operator what to run.
type Outcome struct {
	Advance    bool
	NextAction *NextAction
}

// EvaluatePlanApproved classifies the plan-approved → implement
// transition for the run's runner kind. github_actions auto-advances
// (the orchestrator's existing workflow_dispatch edge is the
// dispatch); local parks with a ready-to-run next action because the
// runner is a host-spawned subprocess (ADR-024) the backend cannot
// start.
func EvaluatePlanApproved(runnerKind string) Outcome {
	if runnerKind == run.RunnerKindLocal {
		return Outcome{
			Advance: false,
			NextAction: &NextAction{
				Action: "run_implement_stage",
				Detail: "runner_kind local: dispatch the implement stage from the operator host (fishhawk_run_stage implement)",
			},
		}
	}
	return Outcome{Advance: true}
}

// EvaluateReviseReplan classifies the plan-gate revise re-plan
// transition (awaiting_approval → pending → dispatched) for the run's
// runner kind, mirroring EvaluatePlanApproved (the dispatch primitive
// is the same runner-kind-aware Advance edge). github_actions
// auto-advances (the orchestrator's existing workflow_dispatch edge is
// the re-run); local parks with a ready-to-run next action because the
// runner is a host-spawned subprocess (ADR-024) the backend cannot
// start.
func EvaluateReviseReplan(runnerKind string) Outcome {
	if runnerKind == run.RunnerKindLocal {
		return Outcome{
			Advance: false,
			NextAction: &NextAction{
				Action: "run_plan_stage",
				Detail: "runner_kind local: dispatch the re-planned plan stage from the operator host (fishhawk_run_stage plan)",
			},
		}
	}
	return Outcome{Advance: true}
}

// EvaluateChildrenDispatch classifies one decomposed child's
// awaiting_children → dispatched transition for the child's runner
// kind, mirroring EvaluatePlanApproved (the dispatch primitive is the
// same runner-kind-aware Advance edge). github_actions auto-advances
// (the orchestrator's Advance handoff fires the child's
// workflow_dispatch); local parks with a ready-to-run next action
// because the runner is a host-spawned subprocess (ADR-024) the
// backend cannot start. It is the observability/classification layer
// only — DispatchDecomposedChildren performs the transition.
func EvaluateChildrenDispatch(runnerKind string) Outcome {
	if runnerKind == run.RunnerKindLocal {
		return Outcome{
			Advance: false,
			NextAction: &NextAction{
				Action: "run_implement_stage",
				Detail: "runner_kind local: dispatch the decomposed child's implement stage from the operator host (fishhawk_run_stage implement)",
			},
		}
	}
	return Outcome{Advance: true}
}

// Advance is the run_auto_advanced audit payload: the rule that
// fired, the from/to transition it stamps, the triggering event, and
// (when the run parks or the operator has a distilled next step) the
// next action. Parked marks a mechanical rule whose advance could not
// be backend-executed (the runner_kind local dispatch) — the entry
// then records the park-with-next-action, not an executed advance.
type Advance struct {
	Rule       Rule        `json:"rule"`
	From       string      `json:"from"`
	To         string      `json:"to"`
	Event      string      `json:"event"`
	Parked     bool        `json:"parked,omitempty"`
	NextAction *NextAction `json:"next_action,omitempty"`
}

// Engine emits the run_auto_advanced audit trail for drive-enabled
// runs. Callers gate on the run's Drive flag before invoking it.
type Engine struct {
	Audit  audit.Repository
	Logger *slog.Logger
}

// Record appends one run_auto_advanced entry. Best-effort: an append
// failure WARN-logs and never unwinds the transition the entry
// documents — the advance already happened on the existing edge.
func (e *Engine) Record(ctx context.Context, runID uuid.UUID, stageID *uuid.UUID, adv Advance) {
	if e == nil || e.Audit == nil {
		return
	}
	payload, _ := json.Marshal(adv)
	systemKind := audit.ActorSystem
	if _, err := e.Audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   stageID,
		Timestamp: time.Now().UTC(),
		Category:  Category,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		e.logger().LogAttrs(ctx, slog.LevelWarn, "drive: append run_auto_advanced failed",
			slog.String("run_id", runID.String()),
			slog.String("rule", string(adv.Rule)),
			slog.String("error", err.Error()))
	}
}

// Recorded reports whether a run_auto_advanced entry naming rule
// already exists for the (run, stage) pair — the idempotency read the
// poll-driven call sites (mergereconciler tick) and re-checkable
// gates dedup on. A nil stageID matches entries with no stage.
// Fail-open: a read error WARN-logs and returns false, so a degraded
// audit read can at worst duplicate an entry, never suppress the
// trail forever.
func (e *Engine) Recorded(ctx context.Context, runID uuid.UUID, stageID *uuid.UUID, rule Rule) bool {
	if e == nil || e.Audit == nil {
		return false
	}
	entries, err := e.Audit.ListForRunByCategory(ctx, runID, Category)
	if err != nil {
		e.logger().LogAttrs(ctx, slog.LevelWarn, "drive: list run_auto_advanced failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return false
	}
	for _, entry := range entries {
		var p Advance
		if json.Unmarshal(entry.Payload, &p) != nil || p.Rule != rule {
			continue
		}
		switch {
		case stageID == nil && entry.StageID == nil:
			return true
		case stageID != nil && entry.StageID != nil && *entry.StageID == *stageID:
			return true
		}
	}
	return false
}

func (e *Engine) logger() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.Default()
}
