package run

import (
	"fmt"
	"time"

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
// exactly one query (UpdateRunState, queries.sql), reached by exactly three
// repository methods — postgresRepo.TransitionRun (gated by
// ValidRunTransition, which returns false for any terminal `from`, and
// State.IsTerminal() includes StateSucceeded), postgresRepo.RetryRun
// (gated by ValidRunRetryTransition, i.e. this table) and
// postgresRepo.ReviveRunOnReopen (gated by ValidRunReopenTransition, i.e.
// runReopenTransitions below, which admits only cancelled → running, #4082)
// — each inside a SELECT ... FOR UPDATE transaction; ReviveRun refuses any
// non-failed run outright (revive.go), and there is no run-deletion path.
// Together those make run state `succeeded` ABSORBING: no code path moves a
// run out of it.
//
// `cancelled` is terminal for every ordinary path too (no key here, and
// ValidRunTransition refuses a terminal `from`), but it is NOT absorbing:
// it leaves through exactly one edge, cancelled → running, and only via the
// PR-reopen revive capability (RunReopenReviver.ReviveRunOnReopen, #4082).
// That edge lives in its own table, runReopenTransitions, which nothing but
// that capability consults — it is deliberately NOT added here, so RetryRun
// and the redrive verb keep refusing a cancelled run.
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

// runReopenTransitions is the run half of the PR-reopen revive (#4082): a
// run whose PR close cancelled it is reopened cancelled → running when the
// PR is reopened quickly at an unchanged head. It is a SEPARATE table from
// runTransitions and runRetryTransitions, consulted ONLY by the
// RunReopenReviver capability (postgresRepo.ReviveRunOnReopen), so
// TransitionRun, RetryRun and the redrive verb keep refusing the edge. The
// server-side guards that decide WHEN a reopen is a revive (window, head,
// the close having cancelled a running run) live in the caller; this table
// only decides WHICH state pair the repository will write.
//
// `succeeded` has NO key here and must never get one: an out-of-succeeded
// edge would re-open the #2586 wave-order guard window described above
// runRetryTransitions. TestRunSucceededIsAbsorbing reads this table
// white-box for exactly that reason.
var runReopenTransitions = map[State]map[State]struct{}{
	StateCancelled: {
		StateRunning: {},
	},
}

// ValidRunReopenTransition reports whether a run in `from` may be reopened
// into `to` by the PR-reopen revive. There is no idempotent same-state
// shortcut: a run already `running` is not a revive candidate.
func ValidRunReopenTransition(from, to State) bool {
	_, ok := runReopenTransitions[from][to]
	return ok
}

// stageReopenPair is one row of the stage half of the PR-reopen revive
// table: the stage type and from-state the revive may re-park. It is a PAIR,
// like stageMergeSupersedePair, because the edge is type-dependent — only
// the review gate the PR close dissolved is re-parked.
type stageReopenPair struct {
	StageType StageType
	From      StageState
	To        StageState
}

// stageReopenTransitions is the DEFAULT-DENY stage table for the PR-reopen
// revive (#4082). Exactly one row: a REVIEW stage cancelled by a PR close is
// re-parked at its approval gate (cancelled → awaiting_approval). A
// cancelled plan, implement, acceptance or deploy stage is never re-opened
// by a reopen — none of them was parked at a gate the close dissolved.
//
// Separate from every other stage table and consulted by nothing except
// RunReopenReviver.ReviveRunOnReopen: it is deliberately NOT in the
// transitionStageTx union (postgres.go), so TransitionStage, RetryStage and
// the CAS siblings keep refusing cancelled → awaiting_approval.
var stageReopenTransitions = []stageReopenPair{
	{StageType: StageTypeReview, From: StageStateCancelled, To: StageStateAwaitingApproval},
}

// ValidStageReopenTransition reports whether a stage of stageType may move
// from→to via the PR-reopen revive. The stageType comparison is
// load-bearing: without it a cancelled implement stage could be flipped to
// awaiting_approval, a gate that stage type never parks at through a PR.
func ValidStageReopenTransition(stageType StageType, from, to StageState) bool {
	for _, p := range stageReopenTransitions {
		if p.StageType == stageType && p.From == from && p.To == to {
			return true
		}
	}
	return false
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
// guard passes `superseded` because it is terminal, so on the ordinary
// transition path this table is the ONLY thing standing between a merge and a
// fabricated success (the opt-in stranded arm is gated by its own table,
// stageStrandedMergeSupersedeTransitions, below). Adding a row here is
// therefore a decision about run integrity, not a convenience.
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

// stageStrandedMergeSupersedeTransitions is the DEFAULT-DENY table of the
// (stage_type, state) pairs the OPT-IN stranded arm of reconcile-merge may
// terminalize as `superseded` (#4222). It is a SEPARATE table from
// stageMergeSupersedeTransitions: those rows are parked gates any merge
// dissolves, while these are stages STRANDED IN FLIGHT (dispatched/running) or
// gates that never opened (pending) on a run whose PR already merged — a legacy
// shape no runner will ever settle, e.g. implement@running beside
// review@pending, where superseding the implement stage alone would make
// Advance dispatch the stale review gate.
//
// Reachable ONLY through StrandedStageMergeSuperseder
// (postgresRepo.SupersedeStrandedStageOnMerge), whose single caller is the
// operator verb `reconcile-merge` with `supersede_stranded: true`, AFTER the
// handler has found merge evidence on the run's chain and its idle-threshold
// liveness gate passed; the capability re-checks liveness against the
// handler's idle cutoff under the row lock. It is deliberately NOT in
// transitionStageTx's union (postgres.go), so TransitionStage,
// TransitionStageFrom and TransitionStageFromAttempt keep refusing running →
// superseded: no ordinary transition caller can retire a live stage.
//
// plan and deploy stages, implement@pending (the #968 work-never-done shape)
// and every park state other than these rows are denied. Adding a row is a
// decision about run integrity: `superseded` is terminal, so completeRun's
// #968 guard passes it.
var stageStrandedMergeSupersedeTransitions = []stageMergeSupersedePair{
	{StageType: StageTypeImplement, From: StageStateDispatched},
	{StageType: StageTypeImplement, From: StageStateRunning},
	{StageType: StageTypeReview, From: StageStatePending},
	{StageType: StageTypeReview, From: StageStateDispatched},
	{StageType: StageTypeReview, From: StageStateRunning},
	{StageType: StageTypeAcceptance, From: StageStatePending},
	{StageType: StageTypeAcceptance, From: StageStateDispatched},
	{StageType: StageTypeAcceptance, From: StageStateRunning},
}

// StrandedMergeSupersedable reports whether a stage of stageType currently in
// `from` is one the stranded merge-supersede table admits. It is the
// classification half the reconcile-merge handler consults (only when the
// operator opted in); ValidStageStrandedMergeSupersedeTransition is the
// admissibility half enforced under the row lock. The stageType comparison is
// load-bearing for the same reason it is in MergeSupersedable: without it a
// running PLAN stage would be admitted under implement's state.
func StrandedMergeSupersedable(stageType StageType, from StageState) bool {
	for _, p := range stageStrandedMergeSupersedeTransitions {
		if p.StageType == stageType && p.From == from {
			return true
		}
	}
	return false
}

// ValidStageStrandedMergeSupersedeTransition reports whether a stage of
// stageType may move from→to via the stranded merge-supersede capability.
// True ONLY when `to` is StageStateSuperseded AND (stageType, from) is a row
// of stageStrandedMergeSupersedeTransitions.
func ValidStageStrandedMergeSupersedeTransition(stageType StageType, from, to StageState) bool {
	if to != StageStateSuperseded {
		return false
	}
	return StrandedMergeSupersedable(stageType, from)
}

// strandedStageInFlight reports whether a stranded-supersede candidate state
// is one a runner may still hold (dispatched or running) and is therefore
// subject to the idle-cutoff liveness re-check. A pending gate is held by no
// runner; a pending stage that was dispatched since the caller's read fails
// the state pin instead.
func strandedStageInFlight(from StageState) bool {
	return from == StageStateDispatched || from == StageStateRunning
}

// StageRecentlyActiveError is returned by
// StrandedStageMergeSuperseder.SupersedeStrandedStageOnMerge when the
// row-locked stage is in flight (dispatched/running) and its DB-stamped
// updated_at — bumped by every transition and every runner heartbeat through
// the stages_set_updated_at trigger — is NEWER than the caller's idle cutoff:
// the stage showed activity after the caller's liveness read, so a runner may
// still hold it (#4222). The refusal is evaluated under the row lock before
// any write and mutates nothing. The reconcile-merge handler maps it to the
// same 409 reconcile_merge_stage_live its pre-write gate answers.
type StageRecentlyActiveError struct {
	StageID      uuid.UUID
	State        StageState
	LastActivity time.Time
	IdleCutoff   time.Time
}

func (e StageRecentlyActiveError) Error() string {
	return fmt.Sprintf("stage %s (%s) was active at %s, after the idle cutoff %s; a runner may still hold it",
		e.StageID, e.State, e.LastActivity.UTC().Format(time.RFC3339Nano), e.IdleCutoff.UTC().Format(time.RFC3339Nano))
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

// RunTerminalError is returned by the live-run compare-and-swap
// (TransitionStageFromLiveRunTx / StageLiveRunCASTransitioner, #4035) when the
// stage's CAS succeeded but its RUN is terminal (succeeded, failed, cancelled)
// when read inside the same transaction. The caller's transaction rolls the
// CAS back, so the stage is left exactly as it was. Callers match it with
// errors.As; the host-dispatch marker maps it to the same 409
// dispatch_not_admissible its terminal-run pre-read answers.
//
//nolint:revive // "Run" names the RUN (not the stage) as the terminal party, beside StageStateChangedError; run.TerminalError would read as the stage's own state.
type RunTerminalError struct {
	RunID uuid.UUID
	State State
}

func (e RunTerminalError) Error() string {
	return fmt.Sprintf("run %s is %s; a terminal run's stage is never admitted", e.RunID, e.State)
}

// StageAttemptToken renders a stage's per-attempt identity from its
// dispatched_at column (#3598). It is the ONE renderer of that identity: the
// prompt envelope that hands the token to the runner, the reap handler's
// fast-path pre-check and the in-transaction predicate of
// StageAttemptCASTransitioner.TransitionStageFromAttempt all call it, so the
// three sites can never disagree on the format.
//
// dispatched_at is the attempt identity because the migration 0072 trigger
// (fishhawk_stamp_stage_dispatched_at) re-stamps it with the database's now()
// on EVERY transition INTO dispatched — a retry or fix-up re-dispatch resets
// it, while a progress heartbeat cannot advance it (see Stage.DispatchedAt and
// TestPostgres_StageDispatchedAtResetsOnRedispatch). A nil value — a legacy
// pre-0072 row, or a stage that never passed through dispatched — renders as
// the EMPTY string, which callers treat as "no attempt anchor". The value is
// only ever compared as an opaque string against this same function's output
// for a Postgres-stamped column, so no Go-side clock participates (#3048).
func StageAttemptToken(dispatchedAt *time.Time) string {
	if dispatchedAt == nil {
		return ""
	}
	return dispatchedAt.UTC().Format(time.RFC3339Nano)
}

// StageAttemptChangedError is returned by the attempt-pinned compare-and-swap
// (StageAttemptCASTransitioner.TransitionStageFromAttempt) when the row-locked
// current state MATCHED the caller's pinned from-state but the row-locked
// attempt token (StageAttemptToken of dispatched_at) did not: the stage was
// re-dispatched between the caller's load and its write, so the caller's
// premise belongs to a superseded attempt. The transition is refused
// atomically under the row lock and nothing is mutated. It is a sibling of
// StageStateChangedError rather than a reuse of it because a state mismatch
// and an attempt mismatch are different facts, and callers map them to
// different refusals (#3598).
type StageAttemptChangedError struct {
	StageID  uuid.UUID
	Expected string
	Actual   string
}

func (e StageAttemptChangedError) Error() string {
	return fmt.Sprintf("stage %s attempt changed: expected %q, got %q", e.StageID, e.Expected, e.Actual)
}
