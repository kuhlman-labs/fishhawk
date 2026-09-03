package run

import (
	"fmt"

	"github.com/google/uuid"
)

// runTransitions enumerates allowed Run state transitions. Any
// (from, to) not present here is rejected. Same-state transitions
// (idempotent re-apply) are handled in ValidRunTransition, not here.
var runTransitions = map[State]map[State]struct{}{
	StatePending: {
		StateRunning:   {},
		StateCancelled: {},
		StateFailed:    {}, // setup-time failure (e.g., spec invalid before dispatch)
	},
	StateRunning: {
		StateSucceeded: {},
		StateFailed:    {},
		StateCancelled: {},
	},
}

// ValidRunTransition reports whether transitioning from→to is
// permitted. Same-state transitions are treated as valid no-ops so
// callers can be idempotent.
func ValidRunTransition(from, to State) bool {
	if from == to {
		return true
	}
	if from.IsTerminal() {
		return false
	}
	_, ok := runTransitions[from][to]
	return ok
}

// runRetryTransitions enumerates the explicit run-level reopen
// overrides off a terminal state — moves out of a terminal run
// state that the regular ValidRunTransition refuses.
//
// failed → running is the re-drive override (#698): a decomposition
// child run resolved to failed, but its implement-stage failure was
// in a retryable category (A/C, or D-timeout). An operator re-drives
// the child via POST /v0/runs/{run_id}/redrive, which un-terminals
// the run so orchestrator.Advance (a no-op on terminal runs) can
// re-dispatch the reset implement stage. This mirrors the
// stageRetryTransitions pattern exactly: a separate table consulted
// only by RetryRun, so it does not loosen ValidRunTransition for
// ordinary callers.
//
// `succeeded` IS DELIBERATELY ABSENT FROM THIS TABLE, and its absence is
// LOAD-BEARING FOR A DOWNSTREAM READER (#2586). runs.state is written by
// exactly one query (UpdateRunState, queries.sql), reached by exactly two
// repository methods — postgresRepo.TransitionRun (gated by
// ValidRunTransition, which returns false for any terminal `from`, and
// State.IsTerminal() includes StateSucceeded) and postgresRepo.RetryRun
// (gated by ValidRunRetryTransition, i.e. this table) — each inside a
// SELECT ... FOR UPDATE transaction; ReviveRun refuses any non-failed run
// outright (revive.go), and there is no run-deletion path. Together those
// make run state `succeeded` ABSORBING: no code path moves a run out of it.
//
// server.guardDecompositionWaveOrder (#2546) depends on that. It snapshots
// a decomposed child's SIBLING run states with ListRuns and then CASes a
// DIFFERENT row (the child's stage). No lock spans the two rows, so the
// guard's snapshot is not atomic with that CAS; its soundness rests on
// `succeeded` being absorbing rather than on any serialization — a sibling
// the snapshot saw as succeeded cannot have regressed by CAS time, so the
// only SIBLING-RUN-STATE drift the window admits moves a dependency TOWARD
// satisfaction and can therefore only cause a spurious refusal (fail
// closed, cleared by a retry). It does NOT cover a parent-plan revision
// changing depends_on inside the same window — see the guard's doc comment.
//
// ADDING AN OUT-OF-SUCCEEDED ENTRY HERE RE-OPENS THAT WINDOW and must be
// paired with real serialization in the guard (or an explicit re-derivation
// of its correctness). TestRunSucceededIsAbsorbing (transition_test.go) and
// TestPostgres_SucceededRunNeverLeavesSucceeded (postgres_test.go) pin the
// property; the first reads this table white-box, so it fails on the table
// edit itself and not only on a reachable pair.
var runRetryTransitions = map[State]map[State]struct{}{
	StateFailed: {
		StateRunning: {},
	},
}

// ValidRunRetryTransition reports whether `from` is allowed to retry
// (reopen) into `to`. The retry path is intentionally narrow —
// callers that want a regular transition should keep using
// ValidRunTransition + TransitionRun.
func ValidRunRetryTransition(from, to State) bool {
	_, ok := runRetryTransitions[from][to]
	return ok
}

// stageTransitions enumerates allowed Stage state transitions.
//
// Pending → Dispatched: backend has emitted workflow_dispatch.
// Pending → AwaitingHostDispatch: a runner_kind-locked-local agent stage
//
//	parks — the backend wants it executed but the runner is host-spawned per
//	ADR-024 and no spawn attempt exists yet (#1912). Written in exactly one
//	place (orchestrator.dispatchStage, a sibling slice).
//
// AwaitingHostDispatch → Dispatched: the host spawn was marked (the
//
//	host-dispatch endpoint CAS, a sibling slice) — a spawn attempt now exists.
//
// AwaitingHostDispatch → Cancelled: run cancel halts the parked stage.
// Dispatched → Running: runner checked in and started executing.
// Dispatched → Failed: runner never started (category C).
// Running → AwaitingApproval: gate evaluation produced a blocking gate.
// Running → AwaitingInput: the planner emitted a clarification_request
//
//	and the plan stage parked for operator direction (#1057).
//
// Running → Succeeded: gate auto-passed (e.g., implicit no-gate stage).
// Running → Failed: any failure category.
// AwaitingApproval → Succeeded: approver said yes.
// AwaitingApproval → Failed: approver rejected, or D-category timeout.
// AwaitingInput → Pending: operator answered; the orchestrator re-opens
//
//	the parked plan stage to resume in the SAME run (pending-resume).
//
// AwaitingInput → Succeeded: the park resolved without re-dispatch.
// AwaitingInput → Failed: the park was abandoned, or its SLA timed out
//
//	(a D-category judgment, not an agent failure).
//
// Running → AwaitingScopeDecision: the implement stage's ONLY committed-
//
//	tree gate failure was the scope-completeness missing-declared-file
//	check; the verified commit is held on the run branch and the run
//	parks for an operator exempt-or-fail decision (#1231).
//
// AwaitingScopeDecision → Pending: operator exempted; the stage resumes in
//
//	place so the orchestrator re-dispatches it to open the PR from the held
//	commit with NO agent re-run (#2501) — the same resume-in-place shape
//	AwaitingInput → Pending already carries.
//
// AwaitingScopeDecision → Running: retained base-table edge (the exempt
//
//	handler no longer uses it — a `running` stage is not dispatch-admissible).
//
// AwaitingScopeDecision → Failed: operator failed it — today's category-B
//
//	restore path.
//
// Pending → AwaitingDeployApproval: a deploy stage parks at its PRE-execution
//
//	gate before any dispatch (ADR-038 / #1384) — the deploy intent must be
//	approved before anything ships. Mirrors the Pending → AwaitingChildren
//	direct park.
//
// AwaitingDeployApproval → Dispatched: operator approved AND pre-flight
//
//	constraints passed; the stage advances to dispatch (NOT succeeded — the
//	deploy has not happened yet; the downstream executor fires it).
//
// AwaitingDeployApproval → Failed: pre-flight refusal, gate reject, or
//
//	D-category SLA timeout.
//
// Running → AwaitingDeployment: post-approval, the executor is polling the
//
//	external delegating pipeline (ADR-038 / #1384).
//
// AwaitingDeployment → Succeeded / Failed: the external pipeline settled.
//
// AwaitingChildren → Succeeded / Failed: the decomposition fan-in resolved.
//
//	This edge is OWNED by the fan-in resolvers — the childcompletion
//	sweeper, the orchestrator's resolveParent, and the consolidate
//	handler — which validate every child slice's terminal state before
//	resolving the parent park. run.FailStage deliberately REFUSES to fail
//	an awaiting_children stage (see failure.go): an ordinary failure
//	reporter (e.g. the reap backstop firing for a doomed mis-dispatched
//	runner) must never destroy a live fan-in park it does not own, even
//	though the base-table edge exists for the resolvers. The edge stays in
//	the table; the ownership guard lives at the domain layer.
//
// Cancelled is reachable from any non-terminal state via manual halt.
var stageTransitions = map[StageState]map[StageState]struct{}{
	StageStatePending: {
		StageStateDispatched:             {},
		StageStateAwaitingHostDispatch:   {}, // runner_kind-locked-local agent stage parks for a host spawn (#1912)
		StageStateCancelled:              {},
		StageStateFailed:                 {},
		StageStateAwaitingChildren:       {},
		StageStateAwaitingDeployApproval: {}, // deploy stage parks pre-execution (ADR-038 / #1384)
	},
	StageStateAwaitingHostDispatch: {
		StageStateDispatched: {}, // the host spawn was marked — a spawn attempt now exists (#1912)
		StageStateCancelled:  {}, // run cancel halts the parked stage
	},
	StageStateDispatched: {
		StageStateRunning:   {},
		StageStateFailed:    {},
		StageStateCancelled: {},
	},
	StageStateRunning: {
		StageStateAwaitingApproval:      {},
		StageStateAwaitingInput:         {},
		StageStateAwaitingScopeDecision: {},
		StageStateAwaitingDeployment:    {}, // deploy executor begins polling the external pipeline (ADR-038 / #1384)
		StageStateSucceeded:             {},
		StageStateFailed:                {},
		StageStateCancelled:             {},
	},
	StageStateAwaitingApproval: {
		StageStateSucceeded: {},
		StageStateFailed:    {},
		StageStateCancelled: {},
	},
	StageStateAwaitingChildren: {
		StageStateSucceeded: {},
		StageStateFailed:    {},
		StageStateCancelled: {},
	},
	StageStateAwaitingInput: {
		StageStatePending:   {}, // operator answered → resume in place
		StageStateSucceeded: {},
		StageStateFailed:    {},
		StageStateCancelled: {},
	},
	StageStateAwaitingScopeDecision: {
		// operator exempted → resume in place for a re-dispatch that opens the PR
		// from the held commit with NO agent re-run (#2501). The prior refusal of
		// this edge ("never rewinds to a fresh dispatch") was correct only while a
		// fresh dispatch MEANT an agent re-run; the prompt response now carries the
		// open_pr_from_held_commit / held_commit_sha / held_commit_branch fields
		// for an exempt-resolved park, so the re-dispatch is provably agent-free
		// and the runner short-circuits to openHeldCommitPR. Routing through
		// pending (rather than the older direct → running) is what makes the
		// orchestrator's uniform dispatch reachable at all: host_dispatch.go's
		// admission switch accepts only {pending, awaiting_host_dispatch}, so a
		// `running` stage is refused dispatch_not_admissible and NO runner ever
		// spawns. Mirrors the AwaitingInput → Pending resume-in-place edge.
		StageStatePending:   {},
		StageStateRunning:   {}, // retained: the base table is permissive (the exempt handler no longer uses this edge)
		StageStateFailed:    {}, // operator failed it → category-B, today's restore path
		StageStateCancelled: {},
	},
	StageStateAwaitingDeployApproval: {
		StageStateDispatched: {}, // approved + pre-flight passed → advance to dispatch (NOT succeeded; deploy not yet run, ADR-038 / #1384)
		StageStateFailed:     {}, // pre-flight refusal / gate reject / D-timeout
		StageStateCancelled:  {},
	},
	StageStateAwaitingDeployment: {
		StageStateSucceeded: {}, // external delegating pipeline reported success
		StageStateFailed:    {}, // external pipeline failed
		StageStateCancelled: {},
	},
}

// ValidStageTransition reports whether transitioning from→to is
// permitted. Idempotent same-state re-application is allowed.
func ValidStageTransition(from, to StageState) bool {
	if from == to {
		return true
	}
	if from.IsTerminal() {
		return false
	}
	_, ok := stageTransitions[from][to]
	return ok
}

// stageRetryTransitions enumerates the explicit retry overrides
// off the normal state machine — moves out of a terminal state
// that the regular ValidStageTransition refuses.
//
// Three retry paths live here:
//
//   - failed → awaiting_approval is the D-timeout retry: the SLA
//     elapsed but no plan needs to be regenerated, just re-open
//     the gate. The updated_at trigger restarts the SLA clock.
//   - failed → pending is the A/C retry (E8.6 #173): the agent
//     crashed (A) or the runner never reported in (C); we want
//     a fresh dispatch. The handler hands off to the orchestrator
//     after the transition; the orchestrator walks pending →
//     dispatched and fires workflow_dispatch.
//   - failed → awaiting_children is the decomposed-parent A/C retry
//     (#1891): retrying a failed implement stage that is a decomposition
//     PARENT (its run has children) must restore the fan-in park, NOT
//     re-dispatch a runner. Targeting pending would permanently suppress
//     the childcompletion sweeper (it lists only awaiting_children stages)
//     and 409 every /consolidate. run.RetryStage selects this target only
//     for a decomposed parent; the sweeper's existing all-terminal +
//     idempotent IntegrateSlices path then re-engages fan-in.
//
// B and D-rejected are deliberately not retriable — the spec or
// the approver said no, the answer doesn't change without a fresh
// run.
var stageRetryTransitions = map[StageState]map[StageState]struct{}{
	StageStateFailed: {
		StageStateAwaitingApproval: {},
		StageStatePending:          {},
		StageStateAwaitingChildren: {},
	},
}

// ValidStageRetryTransition reports whether `from` is allowed to
// retry into `to`. The retry path is intentionally narrow —
// callers that want a regular transition should keep using
// ValidStageTransition + TransitionStage.
func ValidStageRetryTransition(from, to StageState) bool {
	_, ok := stageRetryTransitions[from][to]
	return ok
}

// stageFixupTransitions enumerates the explicit fix-up override off
// the normal state machine — the implement-review fix-up re-open
// (E22.X / #762).
//
// Two fix-up edges live here, both selecting the implement stage's
// re-open target (pending) so the orchestrator walks pending →
// dispatched and re-dispatches the implement stage with the selected
// concerns delivered as binding instructions:
//
//   - awaiting_approval → pending is the commit-yourself flow: the
//     implement stage parked at its OWN review gate (awaiting_approval),
//     an advisory implement reviewer returned approve_with_concerns, and
//     an operator routed concerns back for a bounded fix-up pass.
//   - succeeded → pending is the push_and_open_pr re-open (#780): with
//     push_and_open_pr=true the implement stage SUCCEEDS (it commits and
//     opens the PR) and the human gate is a SEPARATE review stage parked
//     at awaiting_approval. The PR is open, not merged, so a fix-up
//     commit onto the same PR branch is still meaningful. This edge is
//     admitted only when run.FixupStage has confirmed the run's review
//     stage is still at its gate (see fixup.go); the same re-park of the
//     review stage (awaiting_approval → pending) reuses the first edge.
//
// This is deliberately a SEPARATE table from stageRetryTransitions:
// a fix-up is a distinct semantic from a retry (no failure to clear,
// no self_retry_count bump, re-opened from a healthy gate rather than
// a terminal failure), so widening stageRetryTransitions would conflate
// the two. The repo's TransitionStage consults this table in addition
// to ValidStageTransition so the fix-up edge is admissible there
// without loosening the normal machine for ordinary callers.
//
// NOTE this is the STAGE fix-up table, not the run one: it carries a
// succeeded → pending edge, so a STAGE can leave succeeded. That has no
// bearing on the run-level absorbing-succeeded property recorded above
// runRetryTransitions — runs and stages are separate state machines with
// separate tables, and no run-level table admits an out-of-succeeded edge.
var stageFixupTransitions = map[StageState]map[StageState]struct{}{
	StageStateAwaitingApproval: {
		StageStatePending: {},
	},
	StageStateSucceeded: {
		StageStatePending: {},
	},
}

// ValidStageFixupTransition reports whether `from` is allowed to
// re-open into `to` via the fix-up path. The fix-up path is
// intentionally narrow — callers that want a regular transition
// should keep using ValidStageTransition + TransitionStage.
func ValidStageFixupTransition(from, to StageState) bool {
	_, ok := stageFixupTransitions[from][to]
	return ok
}

// stageReviseTransitions enumerates the explicit plan-gate REVISE
// override off the normal state machine — the plan-revise re-open
// (E22.X / #1099).
//
// One revise edge lives here: awaiting_approval → pending for a plan
// stage parked at its approval gate. A `revise` verdict (the third
// plan-gate option alongside approve/reject) re-plans IN PLACE: it
// re-opens the parked plan stage so the orchestrator walks pending →
// dispatched and re-dispatches the plan stage with the operator's
// binding design constraint injected and the prior plan carried as the
// revision base, then the run re-enters the normal review → approve
// gate.
//
// This is deliberately a SEPARATE table from stageFixupTransitions and
// stageRetryTransitions: a revise is a distinct semantic from a fix-up
// (it re-opens a PLAN stage, not an implement stage, and never touches a
// review stage or an implement diff) and from a retry (no failure to
// clear, re-opened from a healthy gate). The repo's TransitionStage
// consults this table in addition to ValidStageTransition so the revise
// edge is admissible there without loosening the normal machine for
// ordinary callers. The domain gate in run.RevisePlanStage (plan-stage
// type + awaiting_approval state + budget) is the real guard.
var stageReviseTransitions = map[StageState]map[StageState]struct{}{
	StageStateAwaitingApproval: {
		StageStatePending: {},
	},
}

// ValidStageReviseTransition reports whether `from` is allowed to
// re-open into `to` via the plan-revise path. The revise path is
// intentionally narrow and SEPARATE from every other table — callers
// that want a regular transition should keep using ValidStageTransition
// + TransitionStage, and only run.RevisePlanStage reaches this edge.
func ValidStageReviseTransition(from, to StageState) bool {
	_, ok := stageReviseTransitions[from][to]
	return ok
}

// stageFixupRecoveryTransitions enumerates the explicit fix-up
// RECOVERY override off the normal state machine — the edges used to
// restore a run to its pre-fix-up review gate when a fix-up
// re-dispatch FAILS (E22.X / #788).
//
// A fix-up re-opens an implement stage from a HEALTHY gate (the PR is
// open and mergeable); if the re-dispatched implement run then fails,
// the implement stage lands terminal `failed` and the review gate is
// gone — even though the original work is intact. A fix-up is a
// best-effort optional pass, so its failure must NOT destroy that
// work. Recovery un-fails the implement stage back to its captured
// prior state and re-parks the review stage that the fix-up re-parked:
//
//   - implement failed → succeeded restores the push_and_open_pr flow
//     (#780): the implement stage had SUCCEEDED (PR opened) before the
//     fix-up re-opened it. Restoring it to succeeded re-stamps ended_at
//     and clears the stale failure metadata (TransitionStage's
//     UpdateStageState sets failure_category/failure_reason directly,
//     not COALESCE).
//   - implement failed → awaiting_approval restores the commit-yourself
//     flow: the implement stage was its OWN gate at awaiting_approval
//     before the re-open.
//   - review pending → awaiting_approval restores the re-parked review
//     gate: the fix-up re-parked the review stage awaiting_approval →
//     pending (#780); recovery puts it back at its gate.
//
// This is deliberately a SEPARATE table from stageRetryTransitions and
// stageFixupTransitions. Admitting `failed → succeeded` is the critical
// safety hazard: if it leaked into the ordinary retry/transition path
// it would FAKE SUCCESS for any failed stage. Keeping it reachable only
// via ValidStageFixupRecoveryTransition (consulted by TransitionStage,
// guarded at the domain layer by RestoreFixupStage) confines that edge
// to the recovery verb.
var stageFixupRecoveryTransitions = map[StageState]map[StageState]struct{}{
	StageStateFailed: {
		StageStateSucceeded:        {},
		StageStateAwaitingApproval: {},
	},
	StageStatePending: {
		StageStateAwaitingApproval: {},
	},
}

// ValidStageFixupRecoveryTransition reports whether `from` is allowed
// to recover into `to` via the fix-up recovery path. The recovery path
// is intentionally narrow and SEPARATE from every other table — callers
// that want a regular transition should keep using ValidStageTransition
// + TransitionStage, and only run.RestoreFixupStage reaches this edge.
func ValidStageFixupRecoveryTransition(from, to StageState) bool {
	_, ok := stageFixupRecoveryTransitions[from][to]
	return ok
}

// stageMergeSupersedePair is one row of the merge-supersede table: the
// (stage_type, state) pair a merge is allowed to terminalize as
// `superseded`. It is a PAIR, not a bare state, because the admissibility
// question is genuinely type-dependent — `awaiting_approval` on a review
// stage is a gate the merge dissolved, while the same state on some other
// stage type is not.
type stageMergeSupersedePair struct {
	StageType StageType
	From      StageState
}

// stageMergeSupersedeTransitions is the DEFAULT-DENY table of the only
// (stage_type, state) pairs a merge may terminalize as `superseded`
// (#3083). Exactly two rows, each with the fact it records:
//
//   - acceptance @ awaiting_host_dispatch — a fix-up pass re-parked the
//     acceptance stage for a host spawn. Once the PR merges nothing will
//     re-dispatch it, and dispatching it anyway would run the acceptance
//     agent against a preview bound to a commit that is no longer the
//     change. The stage is unreachable, not failed.
//   - review @ awaiting_approval — the human review gate on a change that
//     has ALREADY merged. The judgment the gate exists to collect can no
//     longer alter the outcome.
//
// A state-only allow-list is REJECTED BY DESIGN. A `pending` plan or
// implement stage swept to `superseded` would let Orchestrator.completeRun
// stamp the run `succeeded` having never planned or implemented it —
// defeating exactly the invariant the #968 completion guard protects. The
// guard passes `superseded` because it is terminal, so this table is the
// ONLY thing standing between a merge and a fabricated success. Adding a
// row here is therefore a decision about run integrity, not a convenience.
var stageMergeSupersedeTransitions = []stageMergeSupersedePair{
	{StageType: StageTypeAcceptance, From: StageStateAwaitingHostDispatch},
	{StageType: StageTypeReview, From: StageStateAwaitingApproval},
}

// MergeSupersedable reports whether a stage of stageType currently in
// `from` is one the merge-supersede table admits. It is the classification
// half of the table, consulted by the sweep to decide which parked stages a
// merge may terminalize; ValidStageMergeSupersedeTransition is the
// transition-admissibility half enforced at the repository boundary.
//
// The stageType comparison is load-bearing: without it the predicate
// degrades to a state-only allow-list, which would admit a `pending` plan
// stage under the review row's state and fabricate a `succeeded` run.
func MergeSupersedable(stageType StageType, from StageState) bool {
	for _, p := range stageMergeSupersedeTransitions {
		if p.StageType == stageType && p.From == from {
			return true
		}
	}
	return false
}

// ValidStageMergeSupersedeTransition reports whether a stage of stageType
// may move from→to via the merge-supersede path. True ONLY when `to` is
// StageStateSuperseded AND (stageType, from) is a row of the default-deny
// table above.
//
// It is a SEPARATE table from every other transition validator, and unlike
// them it is TYPE-AWARE: the repository's transition union consults it with
// the row-locked stage's own stage_type (postgres.go), so the table is
// enforced at the boundary rather than being advisory guidance a caller
// reaching the repository directly could bypass.
func ValidStageMergeSupersedeTransition(stageType StageType, from, to StageState) bool {
	if to != StageStateSuperseded {
		return false
	}
	return MergeSupersedable(stageType, from)
}

// InvalidTransitionError describes a refused state transition.
// Callers can errors.Is/As against it to surface a 409 Conflict at
// the HTTP layer.
type InvalidTransitionError struct {
	Kind string // "run" or "stage"
	From string
	To   string
}

func (e InvalidTransitionError) Error() string {
	return fmt.Sprintf("invalid %s transition: %s → %s", e.Kind, e.From, e.To)
}

// StageStateChangedError is returned by the compare-and-swap stage
// transition (StageCASTransitioner.TransitionStageFrom) when the
// row-locked current state differs from the from-state the caller
// expected. It signals that another writer flipped the stage between the
// caller's load and its transition, so the transition was refused
// atomically under the row lock rather than applied against a stale
// premise. Callers classify it with errors.As to treat the flip as a
// benign no-op — see run.FailStage (a concurrent fan-in park landing
// mid-flight) and the reap backstop.
type StageStateChangedError struct {
	StageID  uuid.UUID
	Expected StageState
	Actual   StageState
}

func (e StageStateChangedError) Error() string {
	return fmt.Sprintf("stage %s state changed: expected %s, got %s", e.StageID, e.Expected, e.Actual)
}
