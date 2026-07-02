package main

import (
	"fmt"
	"strings"
)

// next_actions (#1024) generalizes the review_action_hint pattern
// (#777/#860/#964) across the whole run lifecycle: for every non-terminal
// run state, fishhawk_get_run_status and fishhawk_run_stage surface at
// least one LEGAL next action as a structured entry — the tool to call
// (with key params), its precondition, what it consumes, and a one-line
// reason. The classifier is a pure function over data the tools already
// fetch (run row, stage rows, review statuses, the computed
// review_action_hint, and the drive read view) — no backend endpoint or
// schema is involved. DISPLAY-ONLY, never gates a run: like the
// periodic-budget block and the hint it generalizes, it is advisory.
//
// Structural invariant (the "done means" condition): nextActionsFor NEVER
// returns an empty actions list for a non-terminal run. Any state the
// table does not match falls through to the labeled "unclassified"
// fallback (re-poll + file a product issue naming the state), and a final
// guard enforces the invariant even if a future arm regresses — the
// invariant is structural, not fixture-dependent.

// Consumes values for SuggestedAction: what taking the action spends.
const (
	consumesNone         = "none"
	consumesFixupBudget  = "fixup_budget"
	consumesRetryBudget  = "retry_budget"
	consumesApprovalSlot = "approval_slot"
	consumesNewRun       = "new_run"
)

// flakeTraceEvents are the known infra-flake trace-event names a
// category-A failure detail may cite (best-effort string inspection —
// no backend contract). When one appears in the implement stage's
// failure reason, the retry_stage action's reason names it so the
// operator knows a retry is the cheapest next step.
var flakeTraceEvents = []string{"verify_infra_flake_retry"}

// SuggestedAction is one legal next move for the run's current state.
// Action is a tool name (fishhawk_resume_run) or a named ritual step
// (approve_pr, merge_pr, post_merge, file_product_issue) when the move
// happens outside the MCP surface.
type SuggestedAction struct {
	Action       string            `json:"action" jsonschema:"the tool to call (e.g. fishhawk_resume_run, fishhawk_fixup_stage) or a named ritual step outside the MCP surface (approve_pr, merge_pr, post_merge, merge_and_file_follow_up, file_product_issue)"`
	Params       map[string]string `json:"params,omitempty" jsonschema:"key parameters for the action (run_id, stage_id, parent_run_id, the concern_ids source, ...); values naming a field path (e.g. run.concerns.items[].id) tell you where to read the real value"`
	Precondition string            `json:"precondition" jsonschema:"one-line statement of when this action is legal"`
	Consumes     string            `json:"consumes" jsonschema:"what taking the action spends: one of none, fixup_budget, retry_budget, approval_slot, new_run"`
	Reason       string            `json:"reason" jsonschema:"one-line why-this-now"`
}

// NextActions is the classified run lifecycle state plus its legal next
// moves. Actions is nil ONLY on a terminal state (the block still names
// the state); every non-terminal state carries at least one action.
type NextActions struct {
	State   string            `json:"state" jsonschema:"the classified run lifecycle state, e.g. plan_gate_parked, plan_awaiting_input, implement_failed_category_b, succeeded_pr_open, terminal states by run state name, or unclassified when no table arm matched"`
	Actions []SuggestedAction `json:"actions,omitempty" jsonschema:"the legal next moves, ordered (first is the suggested default). Nil only on a terminal run state; every non-terminal state carries at least one entry. Display-only — never gates the run"`
}

// nextActionsFor classifies the run's lifecycle state and returns the
// legal next actions (#1024). Pure function over already-fetched data;
// every input except run and stages may be nil. For drive-enabled runs
// with a distilled NextAction, that action is folded in FIRST so drive
// and next_actions never point different ways.
func nextActionsFor(run *Run, stages []Stage, planReviewStatus, implementReviewStatus *ReviewStatus, hint *ReviewActionHint, drive *DriveStatus, mergeObserved bool, acceptanceVerdict, acceptanceTriageDisposition string) *NextActions {
	if run == nil {
		return nil
	}
	// ci_failed (#1045) is decided ahead of the general table: it is a
	// drive-derived presentation status (a red required check on the open
	// PR), not a stage state the table arms key on. Open concerns route to
	// fix-up; zero open concerns is structurally unroutable and names the
	// operator commit+vouch remediation arm (#1044).
	if drive != nil && drive.DerivedStatus == "ci_failed" {
		na := ciFailedNextActions(run, stages, hint)
		if drive.NextAction != nil {
			na.Actions = append([]SuggestedAction{driveAction(run, drive.NextAction)}, na.Actions...)
		}
		return na
	}

	na := classifyNextActions(run, stages, planReviewStatus, implementReviewStatus, hint, mergeObserved, acceptanceVerdict, acceptanceTriageDisposition)

	if drive != nil && drive.NextAction != nil {
		na.Actions = append([]SuggestedAction{driveAction(run, drive.NextAction)}, na.Actions...)
	}

	// Final structural guard: a non-terminal run must never read an empty
	// actions list, even if a future table arm regresses to one.
	if !runStateIsTerminal(run.State) && len(na.Actions) == 0 {
		fallback := unclassifiedNextActions(run, stages)
		fallback.State = na.State
		na = fallback
	}
	return na
}

// classifyNextActions is the state table. Each arm returns a labeled
// state with >= 1 action; only terminal arms return nil actions.
func classifyNextActions(run *Run, stages []Stage, planReviewStatus, implementReviewStatus *ReviewStatus, hint *ReviewActionHint, mergeObserved bool, acceptanceVerdict, acceptanceTriageDisposition string) *NextActions {
	plan := stageByType(stages, "plan")
	impl := stageByType(stages, "implement")
	review := stageByType(stages, "review")
	acceptance := stageByType(stages, "acceptance")
	implReviewPending := implementReviewStatus != nil && implementReviewStatus.Status == "pending"

	// Run already succeeded: the wedge arm, then the merge ritual.
	if run.State == "succeeded" {
		if implReviewPending {
			// A succeeded DECOMPOSITION CHILD (#1082): the run reports
			// succeeded while its own implement review is still pending
			// because the PARENT owns review under #1061 — the child pushes
			// to the shared parent branch and never merges or gets reviewed
			// individually. This is NOT the #968 wedge (which is a top-level
			// run that must merge), so the merge_and_file_follow_up framing is
			// wrong here. Detect the orchestrator's minted-child shape — the
			// SAME predicate implementFailedNextActions uses for a category-B
			// child: a parent_run_id plus an implement stage but NO plan or
			// review stage of its own (a CI-retry child carries a review stage
			// and is excluded by the review == nil clause). Point the operator
			// at the parent run instead of a non-existent per-child PR.
			if run.ParentRunID != nil && plan == nil && review == nil {
				return &NextActions{
					State: "awaiting_parent_consolidation",
					Actions: []SuggestedAction{{
						Action:       "fishhawk_get_run_status",
						Params:       map[string]string{"run_id": *run.ParentRunID},
						Precondition: "this is a succeeded decomposition child (it carries a parent_run_id and has only an implement stage — no plan or review of its own) whose own implement review stays pending because the parent gates the consolidated diff (#1061)",
						Consumes:     consumesNone,
						Reason:       "the slice pushed to the shared parent branch and succeeded; the parent run consolidates the fan-out and gates review, so there is no per-child PR to merge and no #968 wedge to recover — poll the parent run for the consolidation state",
					}},
				}
			}
			// #968-class wedge: the run reported succeeded while the
			// implement review gate is still pending (e.g. a forced fix-up
			// pass completed the run early). Documented recovery arm.
			return &NextActions{
				State: "succeeded_review_wedged",
				Actions: []SuggestedAction{{
					Action:       "merge_and_file_follow_up",
					Params:       prParams(run),
					Precondition: "the run is succeeded but the implement review gate is still pending (#968-class wedge) — no further stage execution will resolve it",
					Consumes:     consumesNone,
					Reason:       "documented recovery: review the diff yourself, approve the PR with an operator verdict, merge, and file a follow-up issue for the unreviewed concerns",
				}},
			}
		}
		if run.PullRequestURL != nil && *run.PullRequestURL != "" {
			// Lifecycle owns its post-merge tail (#1370): when a
			// post_merge_observed audit entry is present the backend has
			// observed the PR merge resolve, so the approve_pr/merge_pr
			// ritual is already complete. Surface succeeded_merged with only
			// the operator post_merge dev-host step (rebuild/reload stays an
			// operator/deploy concern, ADR-038) — dropping the now-done
			// approve_pr/merge_pr steps.
			if mergeObserved {
				return &NextActions{State: "succeeded_merged", Actions: []SuggestedAction{postMergeStep(run)}}
			}
			return &NextActions{State: "succeeded_pr_open", Actions: mergeRitualActions(run, "the run succeeded with its PR open")}
		}
		return &NextActions{State: run.State}
	}

	// Implement-failure recovery arms apply whether the run row is failed
	// (the usual case — a failed stage fails the run) or still running.
	if impl != nil && impl.State == "failed" {
		return implementFailedNextActions(run, plan, stageByType(stages, "review"), impl)
	}

	if runStateIsTerminal(run.State) {
		// failed/cancelled with no recovery arm (e.g. the plan stage
		// failed, or the run was cancelled): nothing legal to do on THIS
		// run — the block still names the state.
		return &NextActions{State: run.State}
	}

	// Plan stage arms.
	if plan != nil && !stageStateIsTerminal(plan.State) {
		return planStageNextActions(run, plan, planReviewStatus)
	}

	// Implement stage arms (the plan gate is behind us, or no plan stage
	// exists — the resume_run recovery-child shape).
	if impl != nil && (plan == nil || plan.State == "succeeded") {
		if a := implementStageNextActions(run, impl, acceptance, implementReviewStatus, hint, acceptanceVerdict, acceptanceTriageDisposition); a != nil {
			return a
		}
	}

	// Deploy stage arms (E23.13 / #1429). A standalone delegating release run
	// has a single deploy stage and no plan/implement of its own, so it falls
	// through every arm above; without this it would read as unclassified.
	// Placed AFTER the implement arm and BEFORE the no-stages / unclassified
	// fallback.
	if deploy := stageByType(stages, "deploy"); deploy != nil && !stageStateIsTerminal(deploy.State) {
		return deployStageNextActions(run, deploy)
	}

	// A run with no stage rows yet (just created; stages not materialized).
	if len(stages) == 0 {
		return &NextActions{
			State: "stages_pending",
			Actions: []SuggestedAction{pollAction(run,
				suggestedStageWaitPollIntervalSeconds,
				"the run has no stage rows yet — re-poll until the stages materialize")},
		}
	}

	return unclassifiedNextActions(run, stages)
}

// planStageNextActions covers the plan stage's non-terminal states:
// not started, running, and parked at the approval gate (split on
// whether the plan review has settled).
func planStageNextActions(run *Run, plan *Stage, planReviewStatus *ReviewStatus) *NextActions {
	switch plan.State {
	case "running":
		return &NextActions{
			State: "plan_running",
			Actions: []SuggestedAction{pollAction(run,
				suggestedStageWaitPollIntervalSeconds,
				"the plan stage is executing — re-poll until plan_stage_wait_status goes terminal")},
		}
	case "awaiting_input":
		// The planner parked at awaiting_input with a clarification_request
		// (#1080/#1057): the issue was not yet plannable, so the operator
		// must answer the parked questions before planning resumes.
		// fishhawk_answer_clarification (#1088) records the answers as a
		// dedicated clarification_answered audit entry and re-opens the SAME
		// plan stage — no new run, no duplicate reviews (distinct from
		// fishhawk_resume_run, which mints a child run).
		return &NextActions{
			State: "plan_awaiting_input",
			Actions: []SuggestedAction{{
				Action:       "fishhawk_answer_clarification",
				Params:       map[string]string{"run_id": run.ID},
				Precondition: "the plan stage parked at awaiting_input with a clarification_request; read the parked questions first (fishhawk_get_run_status or fishhawk_list_audit on category clarification_requested)",
				Consumes:     consumesNone,
				Reason:       "answer the planner's parked questions; the answers inject into the resumed plan agent's binding conditions and re-open this plan stage in the SAME run — pass one {id, answer} per parked question",
			}},
		}
	case "awaiting_approval":
		if planReviewStatus != nil && planReviewStatus.Status == "pending" {
			return &NextActions{
				State: "plan_review_pending",
				Actions: []SuggestedAction{
					pollAction(run, suggestedReviewPollIntervalSeconds,
						"the plan review was dispatched but no verdict has landed — read it before approving, do NOT approve yet"),
					{
						Action:       "fishhawk_await_review",
						Params:       map[string]string{"run_id": run.ID, "stage": "plan"},
						Precondition: "optional convenience block over the same poll",
						Consumes:     consumesNone,
						Reason:       "blocks until the plan review reaches a terminal status",
					},
				},
			}
		}
		return &NextActions{
			State: "plan_gate_parked",
			Actions: []SuggestedAction{
				{
					Action:       "fishhawk_approve_plan",
					Params:       map[string]string{"run_id": run.ID},
					Precondition: "read fishhawk_get_plan and the reviewer verdicts first; scope amendments in the approval reason must name files as dir/name.ext (or use add_scope_files)",
					Consumes:     consumesApprovalSlot,
					Reason:       "the plan is parked at its approval gate" + reviewVerdictSummary(planReviewStatus),
				},
				{
					Action:       "fishhawk_revise_plan",
					Params:       map[string]string{"run_id": run.ID, "constraint": "<binding design constraint>"},
					Precondition: "the plan's direction is sound but a design constraint must change before it proceeds; cheaper than a reject → fresh-run replan",
					Consumes:     consumesApprovalSlot,
					Reason:       "re-plans IN PLACE with your binding constraint injected and the prior plan as the revision base; re-enters the review → approve gate (bounded, default one pass)",
				},
				{
					Action:       "fishhawk_reject_plan",
					Params:       map[string]string{"run_id": run.ID},
					Precondition: "the plan takes a wrong fork that approval conditions cannot amend",
					Consumes:     consumesApprovalSlot,
					Reason:       "a detailed rejection reason propagates to a NEW run for the same issue as PriorRejectionFeedback",
				},
			},
		}
	default: // pending | dispatched | awaiting_children
		return &NextActions{State: "plan_pending", Actions: dispatchOrPollActions(run, "plan")}
	}
}

// implementStageNextActions covers the implement stage's post-plan
// states. Returns nil when no arm matches (the caller falls through to
// the unclassified fallback). acceptance is the run's acceptance stage
// (nil when the workflow declares none, E31.9); when present it gates
// the merge, so the settled path branches to the acceptance arm before
// the merge ritual. acceptanceVerdict / acceptanceTriageDisposition are
// the signals extracted from the acceptance_outcome_recorded /
// acceptance_triage_decided audit payloads.
func implementStageNextActions(run *Run, impl, acceptance *Stage, implementReviewStatus *ReviewStatus, hint *ReviewActionHint, acceptanceVerdict, acceptanceTriageDisposition string) *NextActions {
	switch impl.State {
	case "pending", "dispatched":
		return &NextActions{State: "implement_pending", Actions: dispatchOrPollActions(run, "implement")}
	case "awaiting_children":
		// A DECOMPOSED PARENT parked at awaiting_children (#1147): the legal
		// next move is to fan out the still-pending children, and the
		// children_status block on this same snapshot carries each child's
		// live state + the fan-in/integration phase. Dedicated arm so the
		// operator is pointed at fishhawk_run_children + children_status
		// instead of the generic dispatch-or-poll for a single stage.
		return &NextActions{State: "implement_awaiting_children", Actions: awaitingChildrenActions(run)}
	case "running":
		return &NextActions{
			State: "implement_running",
			Actions: []SuggestedAction{pollAction(run,
				suggestedStageWaitPollIntervalSeconds,
				"the implement stage is executing — re-poll until implement_stage_wait_status goes terminal")},
		}
	case "awaiting_scope_decision":
		// #1231: the implement stage's ONLY committed-tree gate failure was
		// the #1151 scope-completeness "missing declared scope file(s)" check
		// and verify otherwise passed, so the runner pushed the gate-verified
		// commit to the run branch (no PR) and PARKED here instead of failing
		// category-B. The legal next move is the in-band operator decision:
		// exempt (open the PR from the held commit with NO agent re-run) or
		// fail (fall through to category-B). The missing declared paths + held
		// SHA are on the scope_completeness_parked audit entry.
		return &NextActions{
			State: "implement_awaiting_scope_decision",
			Actions: []SuggestedAction{{
				Action:       "fishhawk_decide_scope_completeness",
				Params:       map[string]string{"run_id": run.ID, "decision": "exempt|fail"},
				Precondition: "the implement stage parked at awaiting_scope_decision (its sole gate failure was the #1151 missing-declared-scope-file check; the gate-verified commit is already on the run branch). Read the missing paths + held SHA from the scope_completeness_parked audit entry first",
				Consumes:     consumesNone,
				Reason:       "decide in-band: exempt accepts the already-committed tree and opens the PR from the held commit with NO agent re-run (zero re-run); fail falls through to today's category-B fail-and-restore",
			}},
		}
	case "awaiting_approval", "succeeded":
		if implementReviewStatus != nil && implementReviewStatus.Status == "pending" {
			return &NextActions{
				State: "implement_review_pending",
				Actions: []SuggestedAction{
					pollAction(run, suggestedReviewPollIntervalSeconds,
						implementReviewMergeHint(implementReviewStatus)),
					{
						Action:       "fishhawk_await_review",
						Params:       map[string]string{"run_id": run.ID, "stage": "implement"},
						Precondition: "optional convenience block over the same poll",
						Consumes:     consumesNone,
						Reason:       "blocks until the implement review reaches a terminal status",
					},
				},
			}
		}
		if hint != nil {
			// Open concerns: embed the hint's options as actions. The
			// entries derive FROM the computed ReviewActionHint value
			// (review_action_hint.go), so the two surfaces agree by
			// construction.
			return &NextActions{State: "implement_concerns_open", Actions: hint.suggestedActions(run, impl.ID)}
		}
		// Review settled with nothing to route back. When the workflow
		// declares an acceptance stage (E31.9 / ADR-049), it gates the merge
		// — branch to the acceptance arm BEFORE the merge ritual.
		if acceptance != nil {
			return acceptanceStageNextActions(run, acceptance, acceptanceVerdict, acceptanceTriageDisposition)
		}
		// No acceptance stage: the PR is the next surface — approve and merge.
		return &NextActions{State: "implement_gate_settled", Actions: mergeRitualActions(run, "the implement review is settled with no open concerns")}
	default:
		return nil
	}
}

// deployStageNextActions covers a delegating deploy stage's non-terminal
// states (E23.13 / #1429 / ADR-038). A deploy stage's gate is PRE-execution
// (its effect IS the side effect), so the operator judgment point is the
// approval at awaiting_deploy_approval; once approved, the backend triggers the
// external pipeline and the deployreconciler polls it to terminal
// (awaiting_deployment). The defensive pending/dispatched/running arms cover
// the brief windows the backend itself parks/advances the stage through — there
// is nothing for the operator to do but re-poll.
func deployStageNextActions(run *Run, deploy *Stage) *NextActions {
	switch deploy.State {
	case "awaiting_deploy_approval":
		// The pre-execution approval gate (the operator judgment point). The
		// deploy gate is approved via fishhawk_approve_deploy (E23.15 / #1432),
		// which resolves the type=deploy stage and composes the required
		// --environment=<env> (and optional --override-freeze) into the
		// approval comment the backend deploy pre-flight parses. The older
		// fishhawk_approve_plan hint failed here: it resolves a type=plan stage
		// first and errors on a plan-less release run before reaching the
		// approval endpoint. fishhawk_reject_deploy is the reject counterpart.
		return &NextActions{
			State: "deploy_gate_parked",
			Actions: []SuggestedAction{
				{
					Action:       "fishhawk_approve_deploy",
					Params:       map[string]string{"run_id": run.ID, "environment": "<one of the deploy stage's allowed_environments>"},
					Precondition: "the deploy stage is parked at its pre-execution approval gate (awaiting_deploy_approval); requires an operator token with write:deploy (ADR-038/#1390) and a required environment that is one of the deploy stage's allowed_environments (composed into the approval comment as --environment=<env>); pass override_freeze=true when the stage declares change_freeze. Confirm the corresponding change merged and the pre-flight deploy constraints (allowed_environments, change_freeze, required_upstream) hold before approving",
					Consumes:     consumesApprovalSlot,
					Reason:       "approve the deploy INTENT (ADR-038: a deploy stage's effect is the side effect, so the gate is pre-execution) — approval triggers the external pipeline; a production deploy pages the human regardless of runner kind",
				},
				{
					Action:       "fishhawk_reject_deploy",
					Params:       map[string]string{"run_id": run.ID},
					Precondition: "the deploy stage is parked at its pre-execution approval gate (awaiting_deploy_approval) and the deploy should NOT proceed; reject routes through advanceStage so it needs neither write:deploy nor an environment",
					Consumes:     consumesApprovalSlot,
					Reason:       "reject the deploy INTENT, failing the deploy gate without triggering the external pipeline",
				},
			},
		}
	case "awaiting_deployment":
		// Approved and triggered: the backend deployreconciler is polling the
		// external pipeline to terminal. Nothing for the operator to do but
		// re-poll.
		return &NextActions{
			State: "deploy_in_flight",
			Actions: []SuggestedAction{pollAction(run,
				suggestedStageWaitPollIntervalSeconds,
				"the deploy intent was approved and the external pipeline is running — the backend deployreconciler is polling it to terminal; re-poll until the deploy stage settles")},
		}
	case "dispatched", "running":
		// Defensive: brief windows the backend advances the stage through after
		// approval, before the reconciler picks it up. Poll.
		return &NextActions{
			State: "deploy_in_flight",
			Actions: []SuggestedAction{pollAction(run,
				suggestedStageWaitPollIntervalSeconds,
				"the deploy stage is advancing through the backend toward the external pipeline — re-poll until it settles")},
		}
	default: // pending
		// Defensive: a deploy-first run is parked at the gate at creation
		// (#1429), so a pending deploy stage is a transient pre-park window
		// (or a creation-time Advance that has not landed). Poll — the backend
		// parks it at the gate.
		return &NextActions{
			State: "deploy_initializing",
			Actions: []SuggestedAction{pollAction(run,
				suggestedStageWaitPollIntervalSeconds,
				"the deploy stage has not yet parked at its pre-execution approval gate — re-poll until it reaches awaiting_deploy_approval (the backend parks it at creation)")},
		}
	}
}

// awaitingChildrenActions is the decomposed-parent fan-out arm (#1147): drive
// the still-pending children with fishhawk_run_children, then re-poll — the
// children_status block on the same get_run_status snapshot carries each
// child's live state and the fan-in/integration phase.
func awaitingChildrenActions(run *Run) []SuggestedAction {
	return []SuggestedAction{
		{
			Action:       "fishhawk_run_children",
			Params:       map[string]string{"run_id": run.ID, "workflow": run.WorkflowID},
			Precondition: "the decomposed plan is approved and the parent implement stage is awaiting_children; the children are discovered from the parent's plan_decomposed audit entry",
			Consumes:     consumesNone,
			Reason:       "fan out ALL still-pending decomposed children concurrently (idempotent: in-flight and terminal children are left untouched); a child failure is data, not an error",
		},
		pollAction(run, suggestedStageWaitPollIntervalSeconds,
			"the parent is awaiting_children — re-poll and read the children_status block for each child's live state and the fan-in/integration phase (running_children, ready_to_integrate, integrated, or integration_conflict)"),
	}
}

// implementFailedNextActions branches on the failed implement stage's
// failure category: B routes to the no-replan recovery run (or, for a
// decomposition child, an IN-PLACE re-drive), A to an in-place retry
// (citing a known flake trace event when the failure detail carries one),
// everything else to retry-or-cancel.
func implementFailedNextActions(run *Run, plan, review, impl *Stage) *NextActions {
	category := ""
	if impl.FailureCategory != nil {
		category = *impl.FailureCategory
	}
	switch category {
	case "B":
		// Slice integration conflict (ADR-041 / #1142): the PARENT's
		// implement (awaiting_children) stage failed category-B because a
		// slice branch could not merge onto the consolidated branch during
		// fan-in. Recognized by the stable failure-reason PREFIX (human
		// display only); the machine resume target is SOURCED FROM the
		// structured slice_integration_conflict audit payload, NOT parsed
		// from the free-form reason. The conflicting child id is surfaced as
		// a field-path pointer into that payload (the same idiom ci_failed
		// uses for concern_ids) so the consumer reads the real value from
		// the structured entry — `conflicting_child_run_id`. Placed BEFORE
		// the generic category-B parent arms so it wins for the parent run.
		if impl.FailureReason != nil && strings.HasPrefix(*impl.FailureReason, sliceIntegrationConflictReasonPrefix) {
			return &NextActions{
				State: "slices_integration_conflict",
				Actions: []SuggestedAction{{
					Action: "fishhawk_resume_run",
					// resume_run's parent_run_id param holds the conflicting
					// child's OWN id for an in-place decomposition re-drive
					// (#1081). The value is a field-path pointer: read
					// conflicting_child_run_id from the latest
					// slice_integration_conflict audit entry — structured data,
					// never the reason string.
					Params:       map[string]string{"parent_run_id": "recent_audit[category=slice_integration_conflict].payload.conflicting_child_run_id"},
					Precondition: "the parent implement (awaiting_children) stage failed category-B with a slice integration conflict; read the conflicting child id from the latest slice_integration_conflict audit entry's structured payload (conflicting_child_run_id), NOT from the failure reason string",
					Consumes:     consumesNone,
					Reason:       "slice integration conflict during fan-in: the consolidated branch already holds the earlier slices, so re-drive ONLY the conflicting slice child in place (fishhawk_resume_run pointed at conflicting_child_run_id from the slice_integration_conflict audit) to resolve the conflict and resume fan-in — pointing resume at the parent would replan from scratch and discard the succeeded sibling slices",
				}},
			}
		}
		// A failed DECOMPOSITION CHILD recovers IN PLACE (#1081): point
		// fishhawk_resume_run at THIS child's own id and the backend
		// re-drives the SAME run on the shared parent branch — not a new
		// run — so the parked parent fan-out can still consolidate. The
		// MCP Run row does not mirror the backend's decomposed_from field,
		// so the in-band signal is the orchestrator's minted-child shape:
		// a parent_run_id plus an implement stage but NO plan or review
		// stage of its own (each decomposed child carries a single
		// implement stage; the parent owns plan + review). This matches the
		// recover handler's DecomposedFrom gate for every minted child while
		// excluding a CI-retry child, which carries a review stage and is
		// served by the "resume at the parent" arm below.
		if run.ParentRunID != nil && plan == nil && review == nil {
			return &NextActions{
				State: "implement_failed_category_b_decomposition_child",
				Actions: []SuggestedAction{{
					Action:       "fishhawk_resume_run",
					Params:       map[string]string{"parent_run_id": run.ID},
					Precondition: "this run is a failed decomposition child (it carries a parent_run_id and has only an implement stage — no plan or review of its own) whose implement stage failed category-B; point resume at THIS child's own id, NOT the parent",
					Consumes:     consumesNone,
					Reason:       "category-B decomposition-child failure: fishhawk_resume_run pointed at the child re-opens the SAME run in place on the shared parent branch (folding add_scope_files), so the parked parent fan-out can still consolidate — pointing resume at the parent would replan from scratch and discard the succeeded sibling slices",
				}},
			}
		}
		if plan != nil && plan.State == "succeeded" {
			return &NextActions{
				State: "implement_failed_category_b",
				Actions: []SuggestedAction{{
					Action:       "fishhawk_resume_run",
					Params:       map[string]string{"parent_run_id": run.ID},
					Precondition: "the plan stage succeeded and the implement stage failed category-B; clean the working tree (git status) before dispatching the recovery run's implement stage",
					Consumes:     consumesNewRun,
					Reason:       "category-B (scope/constraint) failure: mint a recovery run re-executing the approved plan without replanning; name missing paths via add_scope_files",
				}},
			}
		}
		return &NextActions{
			State: "implement_failed_category_b",
			Actions: []SuggestedAction{{
				Action:       "fishhawk_start_run",
				Params:       map[string]string{"repo": run.Repo, "workflow_id": run.WorkflowID},
				Precondition: "this run has no succeeded plan stage of its own, so fishhawk_resume_run is not eligible against it (point resume at the original parent instead when one exists)",
				Consumes:     consumesNewRun,
				Reason:       "category-B failure without a resumable plan on this run — replan from scratch",
			}},
		}
	case "A":
		reason := "category-A (agent) failure — fishhawk_retry_stage retries it in place; read the trace first for transient harness errors"
		if flake := citedFlakeEvent(impl); flake != "" {
			reason = fmt.Sprintf("category-A failure whose detail cites %s — an absorbed infra flake recurred; a retry is the cheapest next step", flake)
		}
		return &NextActions{
			State: "implement_failed_category_a",
			Actions: []SuggestedAction{{
				Action:       "fishhawk_retry_stage",
				Params:       map[string]string{"stage_id": impl.ID},
				Precondition: "the implement stage failed category-A",
				Consumes:     consumesRetryBudget,
				Reason:       reason,
			}},
		}
	default:
		return &NextActions{
			State: "implement_failed",
			Actions: []SuggestedAction{
				{
					Action:       "fishhawk_retry_stage",
					Params:       map[string]string{"stage_id": impl.ID},
					Precondition: fmt.Sprintf("the implement stage failed category %q (retry serves categories A/C/D; B routes to fishhawk_resume_run)", category),
					Consumes:     consumesRetryBudget,
					Reason:       "retry the failed stage in place after reading the failure reason",
				},
				{
					Action:       "fishhawk_cancel_run",
					Params:       map[string]string{"run_id": run.ID},
					Precondition: "the failure is not retryable",
					Consumes:     consumesNone,
					Reason:       "cancel this run, then start a fresh one with fishhawk_start_run (consumes a new run)",
				},
			},
		}
	}
}

// ciFailedNextActions covers the drive-derived ci_failed state (#1045):
// a required PR check concluded red on the open PR while the review
// evidence is settled. Routing splits on open-concern presence (the
// same ReviewActionHint signal implementStageNextActions reads, so the
// two surfaces agree by construction):
//
//   - hint != nil (open concerns): ci_failed_routable — route the
//     concerns back with fishhawk_fixup_stage first (a red check is
//     usually the same defect the concerns name), plus a checks re-run
//     for a suspected flake.
//   - hint == nil (no open concerns): ci_failed_unroutable — there is no
//     agent-routable concern, so the fix is the operator's: commit it on
//     the run branch then fishhawk_vouch_commit (#1044), a checks re-run
//     for a flake, or page a human for an unclassifiable failure.
func ciFailedNextActions(run *Run, stages []Stage, hint *ReviewActionHint) *NextActions {
	rerun := SuggestedAction{
		Action:       "rerun_ci_checks",
		Params:       prParams(run),
		Precondition: "the red check is a suspected flake (infra, not a real defect)",
		Consumes:     consumesNone,
		Reason:       "re-run the failed required checks on the PR; a genuine flake goes green on the retry without spending a fix-up pass",
	}
	if hint != nil {
		// Open concerns: the red check is most likely the defect the
		// concerns name — route them back with fishhawk_fixup_stage first,
		// then offer the flake re-run. The merge-with-follow-up ladder that
		// hint.suggestedActions otherwise leads with is deliberately NOT
		// reused here: a red required check is not mergeable.
		fixupParams := map[string]string{"concern_ids": "run.concerns.items[].id"}
		if impl := stageByType(stages, "implement"); impl != nil {
			fixupParams["stage_id"] = impl.ID
		}
		return &NextActions{
			State: "ci_failed_routable",
			Actions: []SuggestedAction{
				{
					Action:       "fishhawk_fixup_stage",
					Params:       fixupParams,
					Precondition: "open implement-review concerns exist and a required PR check is red; checkout the run branch first",
					Consumes:     consumesFixupBudget,
					Reason:       fmt.Sprintf("%d open concern(s) with a red required check — route them back so the fix-up addresses the defect and re-greens the checks", hint.Concerns),
				},
				rerun,
			},
		}
	}
	return &NextActions{
		State: "ci_failed_unroutable",
		Actions: []SuggestedAction{
			{
				Action:       "commit_and_vouch",
				Params:       prParams(run),
				Precondition: "the review is settled with no open concerns, so there is nothing to route back; the fix is yours to make",
				Consumes:     consumesNone,
				Reason:       "commit the fix on the run branch, then fishhawk_vouch_commit so the operator-authored commit clears the run's sole-writer lineage gate (#1044)",
			},
			rerun,
			{
				Action:       "page_human",
				Params:       map[string]string{"run_id": run.ID},
				Precondition: "the failure is neither a flake nor operator-remediable",
				Consumes:     consumesNone,
				Reason:       "the red required check is unclassifiable or non-remediable here — escalate to a human for a judgment call",
			},
		},
	}
}

// dispatchOrPollActions returns the next move for a stage that exists
// but has not started: on runner_kind local the OPERATOR HOST dispatches
// stages; on github_actions the backend auto-dispatches and the legal
// move is to re-poll. For a local IMPLEMENT stage the DEFAULT is the
// non-blocking fishhawk_dispatch_stage (with fishhawk_run_stage retained
// as an explicit opt-in): the implement stage is the one stage type that
// can file a mid-stage scope amendment (#1189), and a blocking run_stage
// holds the MCP session so the amendment cannot be decided in-band. A
// non-blocking dispatch that never sees an amendment polls to terminal
// identically (ADR-037), so defaulting implement to dispatch has no
// downside (#1247). The plan stage (no amendments) keeps the single
// run_stage action unchanged.
func dispatchOrPollActions(run *Run, stageType string) []SuggestedAction {
	if run.RunnerKind == "github_actions" {
		return []SuggestedAction{pollAction(run,
			suggestedStageWaitPollIntervalSeconds,
			fmt.Sprintf("runner_kind github_actions auto-dispatches the %s stage — nothing to run from the operator host; re-poll until it starts", stageType))}
	}
	if stageType == "implement" {
		return []SuggestedAction{
			{
				Action:       "fishhawk_dispatch_stage",
				Params:       map[string]string{"run_id": run.ID, "stage": "implement"},
				Precondition: "the plan gate is approved and the working tree on the operator host is clean (git status first); the implement stage can file a mid-stage scope amendment that a blocking fishhawk_run_stage cannot decide in-band (#1189), so dispatch is the default",
				Consumes:     consumesNone,
				Reason:       "dispatch returns the durable (run_id, stage_id) handle immediately so the SINGLE MCP session stays free to fishhawk_decide_scope_amendment between polls; poll fishhawk_get_run_status to terminal (a dispatch that never sees an amendment behaves identically to blocking, ADR-037)",
			},
			{
				Action:       "fishhawk_run_stage",
				Params:       map[string]string{"run_id": run.ID, "stage": "implement"},
				Precondition: "the plan gate is approved and the working tree is clean; explicit opt-in to BLOCK to terminal — the compact one-shot for when a mid-stage amendment is impossible",
				Consumes:     consumesNone,
				Reason:       "blocks to terminal and returns the full events list, diff_summary, and next_actions in one call — choose this only when no in-band amendment decision is needed",
			},
		}
	}
	if stageType == "acceptance" {
		// The acceptance stage (E31.9) also defaults to the non-blocking
		// fishhawk_dispatch_stage: it validates the change against a running
		// preview/target instance and runs long, so the operator wants the
		// session free while it executes. fishhawk_run_stage stays the blocking
		// opt-in. The acceptance stage files no scope amendments, but the
		// long-run + free-session rationale is the same one that makes dispatch
		// the implement default (#1247).
		return []SuggestedAction{
			{
				Action:       "fishhawk_dispatch_stage",
				Params:       map[string]string{"run_id": run.ID, "stage": "acceptance"},
				Precondition: "the implement review is settled and the customer-provisioned preview/target instance the acceptance stage validates against is up; the working tree on the operator host is clean (git status first). Acceptance runs long against the running instance, so dispatch (non-blocking) is the default",
				Consumes:     consumesNone,
				Reason:       "dispatch returns the durable (run_id, stage_id) handle immediately and polls to terminal; the validator drives the preview and ships a verdict — a FAILED verdict leaves the stage succeeded and routes through deterministic server-side triage (ADR-049 decision #2), so read the acceptance_outcome_recorded entry rather than inferring from stage state",
			},
			{
				Action:       "fishhawk_run_stage",
				Params:       map[string]string{"run_id": run.ID, "stage": "acceptance"},
				Precondition: "the preview/target instance is up and the working tree is clean; explicit opt-in to BLOCK the session to terminal",
				Consumes:     consumesNone,
				Reason:       "blocks to terminal and returns the full events list in one call — choose this only when you do not need the session free while acceptance runs",
			},
		}
	}
	reason := fmt.Sprintf("the %s stage is waiting for the operator host to dispatch it (runner_kind local)", stageType)
	precondition := "the run's runner_kind is local and the stage has not started"
	return []SuggestedAction{{
		Action:       "fishhawk_run_stage",
		Params:       map[string]string{"run_id": run.ID, "stage": stageType},
		Precondition: precondition,
		Consumes:     consumesNone,
		Reason:       reason,
	}}
}

// acceptanceStageNextActions is the acceptance-stage arm of the classifier
// (E31.9 / ADR-049). Reached from implementStageNextActions' settled path when
// the workflow declares an acceptance stage — it gates the merge. A failed
// acceptance VERDICT leaves the STAGE 'succeeded' (backend run/acceptance.go),
// so the arm reads the acceptance_outcome_recorded verdict + the
// acceptance_triage_decided disposition (passed as acceptanceVerdict /
// acceptanceTriageDisposition), NEVER the stage state, to decide the move.
func acceptanceStageNextActions(run *Run, acceptance *Stage, verdict, disposition string) *NextActions {
	// A non-terminal acceptance stage dispatches (local) or polls
	// (github_actions), mirroring the plan/implement pending arms. A
	// running stage is validating against the preview — poll.
	if !stageStateIsTerminal(acceptance.State) {
		if acceptance.State == "running" {
			return &NextActions{
				State: "acceptance_running",
				Actions: []SuggestedAction{pollAction(run,
					suggestedStageWaitPollIntervalSeconds,
					"the acceptance stage is validating the change against the running preview — re-poll until acceptance_stage_wait_status goes terminal, then read the acceptance_outcome_recorded verdict")},
			}
		}
		return &NextActions{State: "acceptance_pending", Actions: dispatchOrPollActions(run, "acceptance")}
	}

	// A terminal acceptance stage that never recorded a verdict in the recent
	// window (verdict==""; the default audit_limit is 5, so the entry can age
	// out), or a stage that failed/cancelled its own execution, falls to the
	// defensive read arm — deliberately NEVER the merge ritual (fail toward
	// read, not toward merge).
	if acceptance.State != "succeeded" || verdict == "" {
		return acceptanceOutcomeUnknownActions(run, acceptance)
	}

	switch verdict {
	case acceptanceVerdictPassed:
		// ADR-049 decision #6: the merge is gated on the acceptance_passed
		// evidence condition. The stage passed — the PR is the next surface.
		return &NextActions{State: "acceptance_passed", Actions: mergeRitualActions(run,
			"the acceptance stage passed (ADR-049 decision #6: the merge is gated on the acceptance_passed evidence condition)")}
	case acceptanceVerdictFailed:
		if isAcceptancePagedDisposition(disposition) {
			return &NextActions{State: "acceptance_triage_paged", Actions: acceptanceTriagePagedActions(run)}
		}
		// fixup_dispatched / retry_dispatched (or a triage decision not yet
		// recorded): the deterministic server-side triage (E31.8) re-opens the
		// implement stage (class 1) or the acceptance stage (class 2), so on the
		// NEXT snapshot the existing implement_pending / acceptance_pending
		// stage-state arms serve the move — nothing to duplicate here. Poll
		// until the re-opened stage surfaces.
		return &NextActions{
			State: "acceptance_triage_rerouting",
			Actions: []SuggestedAction{pollAction(run,
				suggestedStageWaitPollIntervalSeconds,
				"the acceptance verdict failed and deterministic server-side triage auto-routed it (fixup_dispatched re-opens implement; retry_dispatched re-opens acceptance) — re-poll; the re-opened stage's dispatch arm serves the next move. On the local runner an auto-routed re-open never spawns the runner, so fishhawk_dispatch_stage the re-opened implement (after fixup_dispatched) or acceptance (after retry_dispatched) stage")},
		}
	default:
		return acceptanceOutcomeUnknownActions(run, acceptance)
	}
}

// acceptanceOutcomeUnknownActions is the defensive read arm for a settled
// acceptance stage whose verdict is not visible in the recent-audit window (it
// aged out, or the payload was malformed). It points at the full audit trail
// and, load-bearing, NEVER offers the merge ritual — an unknown acceptance
// outcome must fail toward read, not toward merge (E31.9). It also offers the
// fishhawk_retry_stage recovery verb keyed to the acceptance stage id (#1567):
// when the audit confirms NO acceptance_outcome_recorded entry exists for the
// stage, the reopen lands it in pending so the acceptance_pending arm's
// fishhawk_dispatch_stage serves the actual re-run.
func acceptanceOutcomeUnknownActions(run *Run, acceptance *Stage) *NextActions {
	return &NextActions{
		State: "acceptance_settled_outcome_unknown",
		Actions: []SuggestedAction{
			{
				Action:       "fishhawk_list_audit",
				Params:       map[string]string{"run_id": run.ID, "category": "acceptance_outcome_recorded"},
				Precondition: "the acceptance stage settled but no acceptance_outcome_recorded verdict is visible in the recent-audit window (the default audit_limit is 5 — the entry can age out)",
				Consumes:     consumesNone,
				Reason:       "read the acceptance verdict + triage disposition from the full audit trail before acting — deliberately NOT the merge ritual (fail toward read, not toward merge)",
			},
			{
				Action:       "fishhawk_retry_stage",
				Params:       map[string]string{"stage_id": acceptance.ID},
				Precondition: "ONLY after fishhawk_list_audit confirms NO acceptance_outcome_recorded entry exists for this acceptance stage (the stage settled succeeded but shipped no verdict — the run-f7a4b71b hole). The arm also fires when the verdict merely aged out of the recent-audit window; the server re-checks and 422s retry_not_applicable if a verdict IS recorded",
				Consumes:     consumesNone,
				Reason:       "re-open the settled-outcome-unknown acceptance stage for a re-run (operator token only): the reopen lands the stage in pending, so on the local runner the acceptance_pending arm's fishhawk_dispatch_stage then spawns the actual re-run",
			},
			pollAction(run, suggestedStageWaitPollIntervalSeconds,
				"re-poll fishhawk_get_run_status with a larger audit_limit to surface the acceptance_outcome_recorded / acceptance_triage_decided entries"),
		},
	}
}

// acceptanceTriagePagedActions is the human-arbitration arm for a failed
// acceptance verdict whose deterministic triage disposition landed on the
// human (paged / rerun_budget_exhausted / *_unavailable_paged / unsettled_paged
// — ADR-049 decision #2). It leads with reading the evidence, then the operator
// arbitrates: a manual fix-up pass, accept-and-ship, or cancel.
func acceptanceTriagePagedActions(run *Run) []SuggestedAction {
	return []SuggestedAction{
		{
			Action:       "fishhawk_list_audit",
			Params:       map[string]string{"run_id": run.ID, "category": "acceptance_triage_decided"},
			Precondition: "a failed acceptance verdict landed on a paged triage disposition (paged / rerun_budget_exhausted / *_unavailable_paged / unsettled_paged) — the human arbitrates. Read the acceptance_outcome_recorded criteria results and the acceptance_triage_decided class + reason first",
			Consumes:     consumesNone,
			Reason:       "the deterministic triage classified the failure as page-the-human (class 3/4, an exhausted re-run budget, or an unavailable fix-up/retry route); read the evidence before arbitrating",
		},
		{
			Action:       "fishhawk_fixup_stage",
			Params:       map[string]string{"run_id": run.ID, "concern_ids": "run.concerns.items[].id"},
			Precondition: "you judge the failure is a real, fixable code defect worth a manual fix-up pass (checkout the run branch first)",
			Consumes:     consumesFixupBudget,
			Reason:       "route the acceptance failure back to the implement agent as a manual fix-up pass — consumes the shared fix-up budget the auto-triage also draws on",
		},
		{
			Action:       "merge_and_file_follow_up",
			Params:       prParams(run),
			Precondition: "you judge the failure is a bad/ambiguous acceptance criterion (class 3) or otherwise works-as-planned — accept and ship",
			Consumes:     consumesNone,
			Reason:       "accept the change despite the failed acceptance verdict (e.g. a class-3 bad criterion): approve + merge the PR and file a follow-up issue for the disputed criterion",
		},
		{
			Action:       "fishhawk_cancel_run",
			Params:       map[string]string{"run_id": run.ID},
			Precondition: "the change should not ship and no fix-up is warranted",
			Consumes:     consumesNone,
			Reason:       "cancel the run — the acceptance failure is neither fixable in-loop nor acceptable",
		},
	}
}

// mergeRitualActions is the ordered operator merge ritual for a run
// whose PR is open and safe to act on: approve with an operator verdict,
// merge once fishhawk_audit_complete is green, then the post-merge walk.
func mergeRitualActions(run *Run, why string) []SuggestedAction {
	return []SuggestedAction{
		{
			Action:       "approve_pr",
			Params:       prParams(run),
			Precondition: "the implement review is terminal and the diff is reviewed",
			Consumes:     consumesNone,
			Reason:       why + " — record an operator verdict (gh pr review --approve) before merging",
		},
		{
			Action:       "merge_pr",
			Params:       prParams(run),
			Precondition: "the PR is approved and the required fishhawk_audit_complete check is green",
			Consumes:     consumesNone,
			Reason:       "merging resolves the run via the merge reconciler",
		},
		postMergeStep(run),
	}
}

// postMergeStep is the single source of truth for the operator post-merge
// dev-host SuggestedAction, reused by mergeRitualActions and the
// succeeded_merged arm (#1370). The rebuild/reload of the dev host stays
// an operator/deploy concern (ADR-038 #925) even once the lifecycle owns
// the merge tail, so this step survives in the succeeded_merged state
// after approve_pr/merge_pr drop away.
func postMergeStep(_ *Run) SuggestedAction {
	return SuggestedAction{
		Action:       "post_merge",
		Params:       nil,
		Precondition: "the PR is merged",
		Consumes:     consumesNone,
		Reason:       "scripts/dev post-merge pulls main, prunes the merged branch, and reloads the stack",
	}
}

// mergeObservedIn reports whether the recent-audit slice carries a
// post_merge_observed entry (#1370) — the backend lifecycle signal that
// the run's PR merge resolved. getRunStatus computes this off the `recent`
// slice it already fetches and threads it into nextActionsFor to gate the
// succeeded_merged state.
func mergeObservedIn(recent []AuditEntry) bool {
	for _, e := range recent {
		if e.Category == "post_merge_observed" {
			return true
		}
	}
	return false
}

// unclassifiedNextActions is the labeled fallback for any non-terminal
// state the table does not match: re-poll (always legal) plus a pointer
// to file a product issue naming the state so the table gains an arm.
func unclassifiedNextActions(run *Run, stages []Stage) *NextActions {
	shape := make([]string, 0, len(stages))
	for _, s := range stages {
		shape = append(shape, s.Type+"="+s.State)
	}
	desc := fmt.Sprintf("run state %q with stages [%s]", run.State, strings.Join(shape, ", "))
	return &NextActions{
		State: "unclassified",
		Actions: []SuggestedAction{
			pollAction(run, suggestedStageWaitPollIntervalSeconds,
				desc+" did not match the next-actions state table — re-poll while the run settles"),
			{
				Action:       "file_product_issue",
				Params:       map[string]string{"run_id": run.ID},
				Precondition: "the state persists across polls",
				Consumes:     consumesNone,
				Reason:       "the next-actions classifier has no arm for " + desc + "; file a Fishhawk issue naming it so the table gains one",
			},
		},
	}
}

// driveAction converts the drive read view's distilled next step
// (#1023) into a SuggestedAction so it folds in as the FIRST entry on
// drive-enabled runs — drive and next_actions never point different ways.
func driveAction(run *Run, da *RunNextAction) SuggestedAction {
	reason := da.Detail
	if reason == "" {
		reason = "the drive engine's most recent auto-advance distilled this as the operator next step"
	}
	params := map[string]string{"run_id": run.ID}
	if da.PRURL != "" {
		params["pr_url"] = da.PRURL
	}
	return SuggestedAction{
		Action:       da.Action,
		Params:       params,
		Precondition: "drive-mode (#1023): the backend parked the run on this operator step",
		Consumes:     consumesNone,
		Reason:       reason,
	}
}

// pollAction is the re-poll entry: always legal (read-only), carrying
// the advertised cadence the wait contract suggests for the state.
func pollAction(run *Run, intervalSeconds int, reason string) SuggestedAction {
	return SuggestedAction{
		Action:       "fishhawk_get_run_status",
		Params:       map[string]string{"run_id": run.ID, "poll_interval_seconds": fmt.Sprintf("%d", intervalSeconds)},
		Precondition: "always legal (read-only)",
		Consumes:     consumesNone,
		Reason:       reason,
	}
}

// prParams names the PR an action refers to, when the run carries one.
func prParams(run *Run) map[string]string {
	if run.PullRequestURL == nil || *run.PullRequestURL == "" {
		return nil
	}
	return map[string]string{"pr_url": *run.PullRequestURL}
}

// stageByType returns the first stage of the given type, or nil.
func stageByType(stages []Stage, stageType string) *Stage {
	for i := range stages {
		if stages[i].Type == stageType {
			return &stages[i]
		}
	}
	return nil
}

// sliceIntegrationConflictReasonPrefix is the stable prefix the fan-in
// step stamps on the parent implement stage's failure reason (ADR-041 /
// #1142). The next_actions arm keys on it to recognize the conflict
// state; the machine resume target is sourced from the structured
// slice_integration_conflict audit payload, not parsed from this string.
// MUST match orchestrator.sliceIntegrationConflictReasonPrefix.
const sliceIntegrationConflictReasonPrefix = "slice integration conflict"

// Acceptance audit categories + verdict/disposition vocabulary (E31.9 /
// ADR-049). These strings are the cross-module seam between the backend, which
// WRITES the acceptance_outcome_recorded / acceptance_triage_decided audit
// payloads, and this classifier, which READS them. fishhawk-mcp deliberately
// does NOT import backend/internal/server (the #875 compile trap; same idiom as
// sliceIntegrationConflictReasonPrefix above), so the literals are copied
// verbatim and pinned by the literal-table test. A backend rename that is not
// mirrored here lands the failure in the labeled defensive
// acceptance_settled_outcome_unknown / acceptance_triage_rerouting arms (safe:
// read, never merge), not a wrong action.
//
// MUST match backend/internal/server/acceptance.go: the CategoryAcceptance*
// consts (lines ~42/46/56) and the acceptanceVerdict* / acceptanceDisposition*
// consts (lines ~77-89).
const (
	auditCategoryAcceptanceOutcomeRecorded = "acceptance_outcome_recorded"
	auditCategoryAcceptanceTriageDecided   = "acceptance_triage_decided"

	acceptanceVerdictPassed = "passed"
	acceptanceVerdictFailed = "failed"

	// Auto-routed dispositions (a state transition fired): NOT paged.
	acceptanceDispositionFixupDispatched = "fixup_dispatched"
	acceptanceDispositionRetryDispatched = "retry_dispatched"

	// Paged-family dispositions (no transition — the human arbitrates).
	acceptanceDispositionPaged            = "paged"
	acceptanceDispositionRerunBudget      = "rerun_budget_exhausted"
	acceptanceDispositionFixupUnavailable = "fixup_unavailable_paged"
	acceptanceDispositionRetryUnavailable = "retry_unavailable_paged"
	acceptanceDispositionUnsettled        = "unsettled_paged"
)

// isAcceptancePagedDisposition reports whether a triage disposition is a
// page-the-human variant (ADR-049 decision #2). The two auto-routed
// dispositions (fixup_dispatched / retry_dispatched) return false — they fired
// a state transition and the re-opened stage's own arm serves the next move.
func isAcceptancePagedDisposition(d string) bool {
	switch d {
	case acceptanceDispositionPaged,
		acceptanceDispositionRerunBudget,
		acceptanceDispositionFixupUnavailable,
		acceptanceDispositionRetryUnavailable,
		acceptanceDispositionUnsettled:
		return true
	default:
		return false
	}
}

// latestAcceptanceVerdict returns the verdict on the newest
// acceptance_outcome_recorded audit entry in the recent slice (time-descending,
// item 0 newest — the same slice mergeObservedIn scans), or "" when none is
// present or the payload is malformed.
func latestAcceptanceVerdict(recent []AuditEntry) string {
	for _, e := range recent {
		if e.Category == auditCategoryAcceptanceOutcomeRecorded {
			return acceptancePayloadString(e.Payload, "verdict")
		}
	}
	return ""
}

// latestAcceptanceTriageDisposition returns the triage disposition CORRELATED
// with the newest acceptance_outcome_recorded verdict — NOT merely the newest
// acceptance_triage_decided entry. The backend WRITES the triage decision AFTER
// the outcome it triages, so for a given attempt the triage entry sits ABOVE
// (newer than / a lower index than) its verdict in the time-descending recent
// slice. This function therefore finds the newest verdict entry and returns the
// newest triage disposition that is strictly NEWER than it (index <
// verdictIdx); a triage entry at or below the newest verdict belongs to an
// OLDER acceptance attempt and is deliberately ignored.
//
// This correlation is load-bearing: with multiple acceptance attempts in the
// recent window, a fresh failed verdict whose triage decision has not landed
// yet would otherwise inherit the STALE disposition of an earlier failure —
// surfacing acceptance_triage_paged / acceptance_triage_rerouting off the wrong
// attempt. Refusing the stale entry makes acceptanceStageNextActions fall to
// the poll/read arm (empty disposition on a failed verdict → rerouting) until
// the matching triage entry appears. Returns "" when no verdict is present
// (the classifier is in its defensive read arm anyway), no correlated triage
// exists yet, or the payload is malformed.
func latestAcceptanceTriageDisposition(recent []AuditEntry) string {
	verdictIdx := -1
	for i, e := range recent {
		if e.Category == auditCategoryAcceptanceOutcomeRecorded {
			verdictIdx = i
			break
		}
	}
	if verdictIdx < 0 {
		return ""
	}
	for i := 0; i < verdictIdx; i++ {
		if recent[i].Category == auditCategoryAcceptanceTriageDecided {
			return acceptancePayloadString(recent[i].Payload, "disposition")
		}
	}
	return ""
}

// acceptancePayloadString reads a string field from a decoded-JSON audit
// payload. AuditEntry.Payload is `any` (a map[string]any after JSON decode);
// any non-object payload or missing/non-string field yields "" — a malformed
// payload never panics and lands the caller in the defensive arm.
func acceptancePayloadString(payload any, field string) string {
	m, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m[field].(string)
	return s
}

// citedFlakeEvent returns the known flake trace-event name the stage's
// failure reason cites, or "" when none does.
func citedFlakeEvent(s *Stage) string {
	if s.FailureReason == nil {
		return ""
	}
	for _, ev := range flakeTraceEvents {
		if strings.Contains(*s.FailureReason, ev) {
			return ev
		}
	}
	return ""
}

// reviewVerdictSummary renders a short reviewer-verdict suffix for the
// plan-gate approve action, e.g. " — reviews settled: agent approve,
// agent approve_with_concerns(2)". Empty when no verdicts are recorded.
func reviewVerdictSummary(rs *ReviewStatus) string {
	if rs == nil || len(rs.Reviews) == 0 {
		return ""
	}
	parts := make([]string, 0, len(rs.Reviews))
	for _, rev := range rs.Reviews {
		p := rev.ReviewerKind + " " + rev.Verdict
		if n := len(rev.Concerns); n > 0 {
			p += fmt.Sprintf("(%d concern(s))", n)
		}
		parts = append(parts, p)
	}
	return " — reviews settled: " + strings.Join(parts, ", ")
}

// campaignNextActionsFor (E25.8 / #1447) is the campaign arm of the
// next-actions classifier: a pure function mapping a campaign's
// server-computed next_action (computeCampaignNextAction, server/campaigns.go)
// onto a legal MCP operator action. It mirrors EXACTLY the backend's closed
// action set — attention | resume | start_run | wait | complete — so a future
// backend-added action value lands in the labeled campaign_unclassified
// fallback rather than crashing. fishhawk_get_campaign_status embeds the result
// in its output so the operator-agent never reads an unclassified campaign
// state.
//
// Structural invariant (the "never unclassified" done-means): this NEVER
// returns an empty actions list for a non-complete campaign. Only the terminal
// "complete" arm returns nil actions; every other arm — including the
// unknown-action fallback — carries at least one entry, the same structural
// guarantee nextActionsFor upholds for runs.
func campaignNextActionsFor(_ CampaignRollup, na CampaignNextAction) *NextActions {
	switch na.Action {
	case "attention":
		// A campaign item failed terminally (FAILED-wins precedence in
		// computeCampaignNextAction): the operator must retry or abandon it
		// before the campaign can proceed. There is no single MCP verb for this
		// — point at fishhawk_get_run_status on the failed item's run, then
		// retry-or-abandon by hand.
		return &NextActions{
			State: "campaign_attention",
			Actions: []SuggestedAction{{
				Action:       "fishhawk_get_run_status",
				Params:       map[string]string{"issue_ref": na.IssueRef},
				Precondition: "a campaign item failed terminally (its issue_ref is on the next_action); resolve the failed run on that item (fishhawk_list_runs / fishhawk_get_run_status) first",
				Consumes:     consumesNone,
				Reason:       "campaign item " + na.IssueRef + " failed — read its run, then retry the stage or abandon the item before the campaign can advance; a failed item blocks dispatch regardless of other eligible items",
			}},
		}
	case "resume":
		// The auto-driver paged a human at a run gate (E25.7) and the campaign
		// (or an item) is paused. Hand it back with fishhawk_resume_campaign
		// once the gate is handled.
		return &NextActions{
			State: "campaign_paused",
			Actions: []SuggestedAction{{
				Action:       "fishhawk_resume_campaign",
				Precondition: "the auto-driver paged a human at a run gate and the campaign (or an item) is paused; handle the gate first",
				Consumes:     consumesNone,
				Reason:       "paused item " + na.IssueRef + " was handed off at a run gate — once you have handled the gate, fishhawk_resume_campaign flips the campaign and every paused item back to running so the driver re-engages",
			}},
		}
	case "start_run":
		// An eligible item's dependencies are satisfied and it has no run yet:
		// open one with fishhawk_start_run on its issue ref.
		return &NextActions{
			State: "campaign_start_run",
			Actions: []SuggestedAction{{
				Action:       "fishhawk_start_run",
				Params:       map[string]string{"trigger_ref": na.IssueRef},
				Precondition: "this campaign item's dependencies are all satisfied (it is in the rollup's eligible slice) and it has no run yet",
				Consumes:     consumesNewRun,
				Reason:       "dispatch the next eligible campaign item " + na.IssueRef + " — start a run on its issue ref to advance the campaign",
			}},
		}
	case "wait":
		// Items are running or blocked on a dependency; nothing to dispatch.
		// Re-poll until an item becomes eligible, paused, or failed.
		return &NextActions{
			State: "campaign_wait",
			Actions: []SuggestedAction{{
				Action:       "fishhawk_get_campaign_status",
				Precondition: "always legal (read-only)",
				Consumes:     consumesNone,
				Reason:       "items are running or blocked on a dependency; nothing to dispatch yet — re-poll fishhawk_get_campaign_status until an item becomes eligible, pauses, or fails",
			}},
		}
	case "complete":
		// Every item reached a terminal state: the campaign is done. Terminal —
		// no actions (the block still names the state), mirroring a terminal run.
		return &NextActions{State: "campaign_complete"}
	default:
		return campaignUnclassifiedNextActions(na)
	}
}

// campaignUnclassifiedNextActions is the labeled fallback for any campaign
// next_action value the arm above does not recognize (a future backend-added
// action): re-poll (always legal) plus a pointer to file a product issue naming
// the action so the classifier gains an arm. It ALWAYS returns a non-empty
// actions list — the campaign analogue of unclassifiedNextActions, upholding
// the "never unclassified" invariant for a non-complete campaign.
func campaignUnclassifiedNextActions(na CampaignNextAction) *NextActions {
	desc := fmt.Sprintf("campaign next_action %q", na.Action)
	return &NextActions{
		State: "campaign_unclassified",
		Actions: []SuggestedAction{
			{
				Action:       "fishhawk_get_campaign_status",
				Precondition: "always legal (read-only)",
				Consumes:     consumesNone,
				Reason:       desc + " did not match the campaign next-actions classifier — re-poll while the campaign settles",
			},
			{
				Action:       "file_product_issue",
				Precondition: "the action persists across polls",
				Consumes:     consumesNone,
				Reason:       "the campaign classifier has no arm for " + desc + "; file a Fishhawk issue naming it so the classifier gains one",
			},
		},
	}
}
