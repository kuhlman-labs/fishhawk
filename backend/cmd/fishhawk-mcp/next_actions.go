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
func nextActionsFor(run *Run, stages []Stage, planReviewStatus, implementReviewStatus *ReviewStatus, hint *ReviewActionHint, drive *DriveStatus) *NextActions {
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

	na := classifyNextActions(run, stages, planReviewStatus, implementReviewStatus, hint)

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
func classifyNextActions(run *Run, stages []Stage, planReviewStatus, implementReviewStatus *ReviewStatus, hint *ReviewActionHint) *NextActions {
	plan := stageByType(stages, "plan")
	impl := stageByType(stages, "implement")
	review := stageByType(stages, "review")
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
		if a := implementStageNextActions(run, impl, implementReviewStatus, hint); a != nil {
			return a
		}
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
// the unclassified fallback).
func implementStageNextActions(run *Run, impl *Stage, implementReviewStatus *ReviewStatus, hint *ReviewActionHint) *NextActions {
	switch impl.State {
	case "pending", "dispatched", "awaiting_children":
		return &NextActions{State: "implement_pending", Actions: dispatchOrPollActions(run, "implement")}
	case "running":
		return &NextActions{
			State: "implement_running",
			Actions: []SuggestedAction{pollAction(run,
				suggestedStageWaitPollIntervalSeconds,
				"the implement stage is executing — re-poll until implement_stage_wait_status goes terminal")},
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
		// Review settled with nothing to route back: the PR is the next
		// surface — approve and merge.
		return &NextActions{State: "implement_gate_settled", Actions: mergeRitualActions(run, "the implement review is settled with no open concerns")}
	default:
		return nil
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
// stages, so the action is fishhawk_run_stage; on github_actions the
// backend auto-dispatches and the legal move is to re-poll.
func dispatchOrPollActions(run *Run, stageType string) []SuggestedAction {
	if run.RunnerKind == "github_actions" {
		return []SuggestedAction{pollAction(run,
			suggestedStageWaitPollIntervalSeconds,
			fmt.Sprintf("runner_kind github_actions auto-dispatches the %s stage — nothing to run from the operator host; re-poll until it starts", stageType))}
	}
	reason := fmt.Sprintf("the %s stage is waiting for the operator host to dispatch it (runner_kind local)", stageType)
	precondition := "the run's runner_kind is local and the stage has not started"
	if stageType == "implement" {
		precondition = "the plan gate is approved and the working tree on the operator host is clean (git status before every run_stage)"
	}
	return []SuggestedAction{{
		Action:       "fishhawk_run_stage",
		Params:       map[string]string{"run_id": run.ID, "stage": stageType},
		Precondition: precondition,
		Consumes:     consumesNone,
		Reason:       reason,
	}}
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
		{
			Action:       "post_merge",
			Params:       nil,
			Precondition: "the PR is merged",
			Consumes:     consumesNone,
			Reason:       "scripts/dev post-merge pulls main, prunes the merged branch, and reloads the stack",
		},
	}
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
