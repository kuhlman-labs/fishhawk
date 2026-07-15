package main

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
)

// --- fixture helpers -------------------------------------------------------

func naRun(state string) *Run {
	return &Run{ID: uuid.NewString(), Repo: "x/y", WorkflowID: "feature_change", State: state}
}

func naStage(stageType, state string) Stage {
	return Stage{ID: uuid.NewString(), Type: stageType, State: state}
}

func naFailedImplement(category, reason string) Stage {
	s := naStage("implement", "failed")
	s.FailureCategory = &category
	if reason != "" {
		s.FailureReason = &reason
	}
	return s
}

func naReviewStatus(stage, status string) *ReviewStatus {
	return &ReviewStatus{Stage: stage, Status: status}
}

// naDecompChild builds a failed decomposition-child Run: it carries a
// parent_run_id and (paired with an implement-only stage list) has no
// plan or review stage of its own — the orchestrator's minted-child shape.
func naDecompChild() *Run {
	r := naRun("failed")
	parent := uuid.NewString()
	r.ParentRunID = &parent
	return r
}

func actionNames(na *NextActions) []string {
	if na == nil {
		return nil
	}
	names := make([]string, 0, len(na.Actions))
	for _, a := range na.Actions {
		names = append(names, a.Action)
	}
	return names
}

func findAction(t *testing.T, na *NextActions, name string) SuggestedAction {
	t.Helper()
	for _, a := range na.Actions {
		if a.Action == name {
			return a
		}
	}
	t.Fatalf("action %q not found in %v", name, actionNames(na))
	return SuggestedAction{}
}

// --- the state table -------------------------------------------------------

// TestNextActions_StateTable drives every classifier arm from the lived
// loop (#1024 plan steps a–k plus the approval-amended implement
// pending/running states) and asserts the expected state label, action
// names (in order), and consumes values.
func TestNextActions_StateTable(t *testing.T) {
	prURL := "https://github.com/x/y/pull/42"

	cases := []struct {
		name         string
		run          *Run
		stages       []Stage
		planRS       *ReviewStatus
		implRS       *ReviewStatus
		hint         *ReviewActionHint
		wantState    string
		wantActions  []string // exact, in order
		wantConsumes []string // parallel to wantActions
	}{
		{
			name:         "a_plan_pending_local_dispatch",
			run:          naRun("pending"),
			stages:       []Stage{naStage("plan", "pending")},
			wantState:    "plan_pending",
			wantActions:  []string{"fishhawk_run_stage"},
			wantConsumes: []string{consumesNone},
		},
		{
			name: "a_plan_pending_github_actions_autodispatch",
			run: func() *Run {
				r := naRun("pending")
				r.RunnerKind = "github_actions"
				return r
			}(),
			stages:       []Stage{naStage("plan", "pending")},
			wantState:    "plan_pending",
			wantActions:  []string{"fishhawk_get_run_status"},
			wantConsumes: []string{consumesNone},
		},
		{
			name:         "a_plan_running_repoll",
			run:          naRun("running"),
			stages:       []Stage{naStage("plan", "running")},
			wantState:    "plan_running",
			wantActions:  []string{"fishhawk_get_run_status"},
			wantConsumes: []string{consumesNone},
		},
		{
			name:         "plan_awaiting_input_answer_clarification",
			run:          naRun("running"),
			stages:       []Stage{naStage("plan", "awaiting_input")},
			wantState:    "plan_awaiting_input",
			wantActions:  []string{"fishhawk_answer_clarification"},
			wantConsumes: []string{consumesNone},
		},
		{
			name:         "b_plan_review_pending_do_not_approve",
			run:          naRun("running"),
			stages:       []Stage{naStage("plan", "awaiting_approval")},
			planRS:       naReviewStatus("plan", "pending"),
			wantState:    "plan_review_pending",
			wantActions:  []string{"fishhawk_get_run_status", "fishhawk_await_review"},
			wantConsumes: []string{consumesNone, consumesNone},
		},
		{
			name:         "c_plan_gate_parked_approve_or_reject",
			run:          naRun("running"),
			stages:       []Stage{naStage("plan", "awaiting_approval")},
			planRS:       naReviewStatus("plan", "complete"),
			wantState:    "plan_gate_parked",
			wantActions:  []string{"fishhawk_approve_plan", "fishhawk_revise_plan", "fishhawk_reject_plan"},
			wantConsumes: []string{consumesApprovalSlot, consumesApprovalSlot, consumesApprovalSlot},
		},
		{
			// #1247: a parked LOCAL implement stage defaults to the
			// non-blocking fishhawk_dispatch_stage (so the session stays free to
			// decide a mid-stage amendment in-band) with fishhawk_run_stage
			// retained as the explicit blocking opt-in, in that order.
			name:         "amended_implement_pending_local_dispatch",
			run:          naRun("running"),
			stages:       []Stage{naStage("plan", "succeeded"), naStage("implement", "pending")},
			wantState:    "implement_pending",
			wantActions:  []string{"fishhawk_dispatch_stage", "fishhawk_run_stage"},
			wantConsumes: []string{consumesNone, consumesNone},
		},
		{
			name: "amended_implement_pending_github_actions_autodispatch",
			run: func() *Run {
				r := naRun("running")
				r.RunnerKind = "github_actions"
				return r
			}(),
			stages:       []Stage{naStage("plan", "succeeded"), naStage("implement", "pending")},
			wantState:    "implement_pending",
			wantActions:  []string{"fishhawk_get_run_status"},
			wantConsumes: []string{consumesNone},
		},
		{
			name:         "amended_implement_running_repoll",
			run:          naRun("running"),
			stages:       []Stage{naStage("plan", "succeeded"), naStage("implement", "running")},
			wantState:    "implement_running",
			wantActions:  []string{"fishhawk_get_run_status"},
			wantConsumes: []string{consumesNone},
		},
		{
			// #1147: a decomposed parent parked at awaiting_children gets the
			// dedicated fan-out arm — run_children then poll children_status.
			name:         "implement_awaiting_children_fan_out",
			run:          naRun("running"),
			stages:       []Stage{naStage("plan", "succeeded"), naStage("implement", "awaiting_children")},
			wantState:    "implement_awaiting_children",
			wantActions:  []string{"fishhawk_run_children", "fishhawk_get_run_status"},
			wantConsumes: []string{consumesNone, consumesNone},
		},
		{
			// #1231: an implement stage parked at awaiting_scope_decision gets
			// the in-band exempt-or-fail decision arm.
			name:         "implement_awaiting_scope_decision",
			run:          naRun("running"),
			stages:       []Stage{naStage("plan", "succeeded"), naStage("implement", "awaiting_scope_decision")},
			wantState:    "implement_awaiting_scope_decision",
			wantActions:  []string{"fishhawk_decide_scope_completeness"},
			wantConsumes: []string{consumesNone},
		},
		{
			name:         "d_category_b_with_succeeded_plan_resume_run",
			run:          naRun("failed"),
			stages:       []Stage{naStage("plan", "succeeded"), naFailedImplement("B", "scope drift")},
			wantState:    "implement_failed_category_b",
			wantActions:  []string{"fishhawk_resume_run"},
			wantConsumes: []string{consumesNewRun},
		},
		{
			name:         "d_category_b_without_plan_fresh_run",
			run:          naRun("failed"),
			stages:       []Stage{naFailedImplement("B", "scope drift")},
			wantState:    "implement_failed_category_b",
			wantActions:  []string{"fishhawk_start_run"},
			wantConsumes: []string{consumesNewRun},
		},
		{
			// #1081: a failed decomposition child (parent_run_id set,
			// implement-only — no plan/review of its own) routes category-B
			// to an IN-PLACE re-drive against THIS child's own id (consumes
			// nothing), not a fresh run.
			name:         "d_category_b_decomposition_child_in_place",
			run:          naDecompChild(),
			stages:       []Stage{naFailedImplement("B", "scope drift")},
			wantState:    "implement_failed_category_b_decomposition_child",
			wantActions:  []string{"fishhawk_resume_run"},
			wantConsumes: []string{consumesNone},
		},
		{
			// A CI-retry child (parent_run_id set, plan-less BUT carrying a
			// review stage) is NOT a decomposition child: it stays on the
			// "resume at the parent / replan" arm, not the in-place re-drive.
			name:         "d_category_b_ci_retry_child_not_in_place",
			run:          naDecompChild(),
			stages:       []Stage{naFailedImplement("B", "scope drift"), naStage("review", "pending")},
			wantState:    "implement_failed_category_b",
			wantActions:  []string{"fishhawk_start_run"},
			wantConsumes: []string{consumesNewRun},
		},
		{
			// #1915: the category-A arm now also offers fishhawk_revive_run (the
			// batch no-dispatch re-park) after the single-stage retry.
			name:         "e_category_a_retry_with_flake_citation",
			run:          naRun("failed"),
			stages:       []Stage{naStage("plan", "succeeded"), naFailedImplement("A", "verify failed after verify_infra_flake_retry absorbed one flake")},
			wantState:    "implement_failed_category_a",
			wantActions:  []string{"fishhawk_retry_stage", "fishhawk_revive_run"},
			wantConsumes: []string{consumesRetryBudget, consumesRetryBudget},
		},
		{
			name:         "e_category_a_retry_without_citation",
			run:          naRun("failed"),
			stages:       []Stage{naStage("plan", "succeeded"), naFailedImplement("A", "agent crashed")},
			wantState:    "implement_failed_category_a",
			wantActions:  []string{"fishhawk_retry_stage", "fishhawk_revive_run"},
			wantConsumes: []string{consumesRetryBudget, consumesRetryBudget},
		},
		{
			// #1915: the default (retryable) arm offers fishhawk_revive_run
			// between the single-stage retry and cancel.
			name:         "f_category_c_retry_or_cancel",
			run:          naRun("failed"),
			stages:       []Stage{naStage("plan", "succeeded"), naFailedImplement("C", "infra")},
			wantState:    "implement_failed",
			wantActions:  []string{"fishhawk_retry_stage", "fishhawk_revive_run", "fishhawk_cancel_run"},
			wantConsumes: []string{consumesRetryBudget, consumesRetryBudget, consumesNone},
		},
		{
			name:         "g_implement_review_pending_repoll",
			run:          naRun("running"),
			stages:       []Stage{naStage("plan", "succeeded"), naStage("implement", "awaiting_approval")},
			implRS:       naReviewStatus("implement", "pending"),
			wantState:    "implement_review_pending",
			wantActions:  []string{"fishhawk_get_run_status", "fishhawk_await_review"},
			wantConsumes: []string{consumesNone, consumesNone},
		},
		{
			name:         "h_concerns_open_below_budget",
			run:          naRun("running"),
			stages:       []Stage{naStage("plan", "succeeded"), naStage("implement", "awaiting_approval")},
			implRS:       naReviewStatus("implement", "complete"),
			hint:         &ReviewActionHint{Concerns: 2, RemainingFixupBudget: 1},
			wantState:    "implement_concerns_open",
			wantActions:  []string{"fishhawk_fixup_stage", "fishhawk_defer_concern", "merge_and_file_follow_up"},
			wantConsumes: []string{consumesFixupBudget, consumesNone, consumesNone},
		},
		{
			name:         "h_concerns_open_budget_spent_override_available",
			run:          naRun("running"),
			stages:       []Stage{naStage("plan", "succeeded"), naStage("implement", "awaiting_approval")},
			implRS:       naReviewStatus("implement", "complete"),
			hint:         &ReviewActionHint{Concerns: 1, RemainingFixupBudget: 0, OverrideAvailable: true},
			wantState:    "implement_concerns_open",
			wantActions:  []string{"merge_and_file_follow_up", "fishhawk_defer_concern", "fishhawk_fixup_stage"},
			wantConsumes: []string{consumesNone, consumesNone, consumesFixupBudget},
		},
		{
			name:      "h_concerns_open_ceiling_reached",
			run:       naRun("running"),
			stages:    []Stage{naStage("plan", "succeeded"), naStage("implement", "awaiting_approval")},
			implRS:    naReviewStatus("implement", "complete"),
			hint:      &ReviewActionHint{Concerns: 1, RemainingFixupBudget: 0, OverrideAvailable: false},
			wantState: "implement_concerns_open",
			// #1097: the ceiling-reached arm now also advertises commit_and_vouch
			// (the operator-vouched patch path for a late CI/SAST finding), which
			// consumes no fix-up budget, between merge-with-follow-up and a fresh run.
			wantActions:  []string{"merge_and_file_follow_up", "commit_and_vouch", "fishhawk_start_run"},
			wantConsumes: []string{consumesNone, consumesNone, consumesNewRun},
		},
		{
			name:         "implement_gate_settled_merge_ritual",
			run:          naRun("running"),
			stages:       []Stage{naStage("plan", "succeeded"), naStage("implement", "succeeded")},
			implRS:       naReviewStatus("implement", "complete"),
			wantState:    "implement_gate_settled",
			wantActions:  []string{"approve_pr", "merge_pr", "post_merge"},
			wantConsumes: []string{consumesNone, consumesNone, consumesNone},
		},
		{
			name: "i_run_succeeded_pr_open_merge_ritual",
			run: func() *Run {
				r := naRun("succeeded")
				r.PullRequestURL = &prURL
				return r
			}(),
			stages:       []Stage{naStage("plan", "succeeded"), naStage("implement", "succeeded")},
			implRS:       naReviewStatus("implement", "complete"),
			wantState:    "succeeded_pr_open",
			wantActions:  []string{"approve_pr", "merge_pr", "post_merge"},
			wantConsumes: []string{consumesNone, consumesNone, consumesNone},
		},
		{
			name: "j_968_wedge_run_succeeded_review_still_pending",
			run: func() *Run {
				r := naRun("succeeded")
				r.PullRequestURL = &prURL
				return r
			}(),
			stages:       []Stage{naStage("plan", "succeeded"), naStage("implement", "succeeded")},
			implRS:       naReviewStatus("implement", "pending"),
			wantState:    "succeeded_review_wedged",
			wantActions:  []string{"merge_and_file_follow_up"},
			wantConsumes: []string{consumesNone},
		},
		{
			// #1082: a succeeded decomposition child (parent_run_id set,
			// implement-only — no plan/review of its own) whose own
			// implement review is still pending is NOT the #968 wedge: the
			// parent gates the consolidated diff (#1061) and there is no
			// per-child PR to merge. It surfaces as
			// awaiting_parent_consolidation, pointing the read-only poll at
			// the PARENT run, never merge_and_file_follow_up.
			name: "j_1082_succeeded_decomp_child_awaits_parent",
			run: func() *Run {
				r := naRun("succeeded")
				parent := uuid.NewString()
				r.ParentRunID = &parent
				return r
			}(),
			stages:       []Stage{naStage("implement", "succeeded")},
			implRS:       naReviewStatus("implement", "pending"),
			wantState:    "awaiting_parent_consolidation",
			wantActions:  []string{"fishhawk_get_run_status"},
			wantConsumes: []string{consumesNone},
		},
		{
			// #1082 negative guard: a SUCCEEDED CI-retry child carries a
			// parent_run_id AND a review stage of its own, so the new arm's
			// `review == nil` clause EXCLUDES it — it must fall through to
			// the genuine #968 succeeded_review_wedged wedge
			// (merge_and_file_follow_up), NOT awaiting_parent_consolidation.
			// This is the case the Risks section names as "the test that
			// would fail if wrong": drop the review==nil clause and this
			// case regresses while every other present case still passes.
			name: "j_1082_succeeded_ci_retry_child_not_consolidation",
			run: func() *Run {
				r := naRun("succeeded")
				r.PullRequestURL = &prURL
				parent := uuid.NewString()
				r.ParentRunID = &parent
				return r
			}(),
			stages:       []Stage{naStage("implement", "succeeded"), naStage("review", "pending")},
			implRS:       naReviewStatus("implement", "pending"),
			wantState:    "succeeded_review_wedged",
			wantActions:  []string{"merge_and_file_follow_up"},
			wantConsumes: []string{consumesNone},
		},
		{
			name:        "k_terminal_failed_no_recovery_arm",
			run:         naRun("failed"),
			stages:      []Stage{naStage("plan", "failed")},
			wantState:   "failed",
			wantActions: nil,
		},
		{
			name:        "k_terminal_cancelled",
			run:         naRun("cancelled"),
			stages:      []Stage{naStage("plan", "succeeded"), naStage("implement", "cancelled")},
			wantState:   "cancelled",
			wantActions: nil,
		},
		{
			name:        "k_terminal_succeeded_no_pr",
			run:         naRun("succeeded"),
			stages:      []Stage{naStage("plan", "succeeded"), naStage("implement", "succeeded")},
			wantState:   "succeeded",
			wantActions: nil,
		},
		{
			name:         "stages_not_materialized_repoll",
			run:          naRun("pending"),
			stages:       nil,
			wantState:    "stages_pending",
			wantActions:  []string{"fishhawk_get_run_status"},
			wantConsumes: []string{consumesNone},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			na := nextActionsFor(tc.run, tc.stages, tc.planRS, tc.implRS, tc.hint, nil, false, false, "", "", releaseSignals{})
			if na == nil {
				t.Fatal("nextActionsFor returned nil; the block must always be present")
			}
			if na.State != tc.wantState {
				t.Errorf("state = %q, want %q", na.State, tc.wantState)
			}
			got := actionNames(na)
			if len(got) != len(tc.wantActions) {
				t.Fatalf("actions = %v, want %v", got, tc.wantActions)
			}
			for i := range tc.wantActions {
				if got[i] != tc.wantActions[i] {
					t.Errorf("actions[%d] = %q, want %q (full: %v)", i, got[i], tc.wantActions[i], got)
				}
				if tc.wantConsumes != nil && na.Actions[i].Consumes != tc.wantConsumes[i] {
					t.Errorf("actions[%d].consumes = %q, want %q", i, na.Actions[i].Consumes, tc.wantConsumes[i])
				}
			}

			// The 'done means' invariant as a loop assertion: every
			// non-terminal run state yields at least one action.
			if !runStateIsTerminal(tc.run.State) && len(na.Actions) == 0 {
				t.Errorf("non-terminal run state %q yielded ZERO actions — the structural invariant is broken", tc.run.State)
			}
			// Every action carries the structured fields.
			for i, a := range na.Actions {
				if a.Precondition == "" || a.Consumes == "" || a.Reason == "" {
					t.Errorf("actions[%d] (%s) missing precondition/consumes/reason: %+v", i, a.Action, a)
				}
			}
		})
	}
}

// TestNextActions_ImplementLocalDispatchDefault pins the #1247 default: a
// parked LOCAL implement stage leads with fishhawk_dispatch_stage (carrying
// run_id + stage=implement) and its Precondition NAMES the in-band-amendment
// rationale (#1189) — so a regression that strips the why, or that demotes
// dispatch below run_stage, fails here. fishhawk_run_stage is retained as the
// explicit opt-in second entry.
func TestNextActions_ImplementLocalDispatchDefault(t *testing.T) {
	run := naRun("running")
	stages := []Stage{naStage("plan", "succeeded"), naStage("implement", "pending")}

	na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, "", "", releaseSignals{})
	if na == nil || na.State != "implement_pending" {
		t.Fatalf("state = %+v, want implement_pending", na)
	}
	if len(na.Actions) == 0 || na.Actions[0].Action != "fishhawk_dispatch_stage" {
		t.Fatalf("actions[0] = %v, want fishhawk_dispatch_stage as the default first entry", actionNames(na))
	}
	dispatch := na.Actions[0]
	if dispatch.Params["run_id"] != run.ID || dispatch.Params["stage"] != "implement" {
		t.Errorf("dispatch params = %v, want run_id=%s stage=implement", dispatch.Params, run.ID)
	}
	if !strings.Contains(dispatch.Precondition, "#1189") || !strings.Contains(dispatch.Precondition, "amendment") {
		t.Errorf("dispatch precondition must name the in-band-amendment rationale (#1189); got %q", dispatch.Precondition)
	}
	// run_stage is retained as the explicit opt-in, not removed.
	findAction(t, na, "fishhawk_run_stage")
}

// TestNextActions_PlanLocalDispatchUnchanged pins condition (1): the
// plan-local branch is byte-unchanged — a parked LOCAL plan stage still
// offers the single fishhawk_run_stage action and never dispatch_stage.
func TestNextActions_PlanLocalDispatchUnchanged(t *testing.T) {
	run := naRun("pending")
	stages := []Stage{naStage("plan", "pending")}

	na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, "", "", releaseSignals{})
	if got := actionNames(na); len(got) != 1 || got[0] != "fishhawk_run_stage" {
		t.Fatalf("plan-local actions = %v, want exactly [fishhawk_run_stage]", got)
	}
}

// TestNextActions_AwaitingChildren_FanOutArm pins the #1147 arm: a decomposed
// parent at awaiting_children offers fishhawk_run_children (carrying run_id +
// workflow) plus a poll whose reason points the operator at the children_status
// block for the per-child state and fan-in phase.
func TestNextActions_AwaitingChildren_FanOutArm(t *testing.T) {
	run := naRun("running")
	stages := []Stage{naStage("plan", "succeeded"), naStage("implement", "awaiting_children")}

	na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, "", "", releaseSignals{})
	if na == nil || na.State != "implement_awaiting_children" {
		t.Fatalf("state = %+v, want implement_awaiting_children", na)
	}
	rc := findAction(t, na, "fishhawk_run_children")
	if rc.Params["run_id"] != run.ID || rc.Params["workflow"] != run.WorkflowID {
		t.Errorf("run_children params = %v, want run_id=%s workflow=%s", rc.Params, run.ID, run.WorkflowID)
	}
	poll := findAction(t, na, "fishhawk_get_run_status")
	if !strings.Contains(poll.Reason, "children_status") {
		t.Errorf("poll reason should point at the children_status block; got %q", poll.Reason)
	}
}

// TestNextActions_DeployStage covers the E23.13 / #1429 deploy arm: a
// standalone delegating release run (a single deploy stage, no plan/implement)
// classifies per the deploy stage's state instead of falling through to
// unclassified. One behavioral assertion per named branch.
func TestNextActions_DeployStage(t *testing.T) {
	cases := []struct {
		name        string
		deployState string
		wantState   string
		wantAction  string // first action
	}{
		{"awaiting_deploy_approval -> approve", "awaiting_deploy_approval", "deploy_gate_parked", "fishhawk_approve_deploy"},
		{"awaiting_deployment -> poll", "awaiting_deployment", "deploy_in_flight", "fishhawk_get_run_status"},
		{"defensive pending -> poll", "pending", "deploy_initializing", "fishhawk_get_run_status"},
		{"defensive running -> poll", "running", "deploy_in_flight", "fishhawk_get_run_status"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := naRun("running")
			run.WorkflowID = "release"
			stages := []Stage{naStage("deploy", tc.deployState)}

			na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, "", "", releaseSignals{})
			if na == nil {
				t.Fatal("nextActionsFor = nil")
			}
			if na.State == "unclassified" {
				t.Fatalf("deploy state %q classified as unclassified — the deploy arm did not fire", tc.deployState)
			}
			if na.State != tc.wantState {
				t.Errorf("state = %q, want %q", na.State, tc.wantState)
			}
			if len(na.Actions) == 0 {
				t.Fatalf("zero actions for deploy state %q — structural invariant broken", tc.deployState)
			}
			if na.Actions[0].Action != tc.wantAction {
				t.Errorf("actions[0] = %q, want %q (full: %v)", na.Actions[0].Action, tc.wantAction, actionNames(na))
			}
			// The approve action targets the run and consumes an approval slot.
			if tc.wantAction == "fishhawk_approve_deploy" {
				if na.Actions[0].Params["run_id"] != run.ID {
					t.Errorf("approve params = %v, want run_id=%s", na.Actions[0].Params, run.ID)
				}
				if na.Actions[0].Consumes != consumesApprovalSlot {
					t.Errorf("approve consumes = %q, want %q", na.Actions[0].Consumes, consumesApprovalSlot)
				}
				// The deploy gate must surface the required environment and the
				// write:deploy scope in the precondition, and the deploy approve
				// arm must offer a reject counterpart (E23.15 / #1432).
				if _, ok := na.Actions[0].Params["environment"]; !ok {
					t.Errorf("approve_deploy params missing 'environment' key: %v", na.Actions[0].Params)
				}
				if !strings.Contains(na.Actions[0].Precondition, "--environment") {
					t.Errorf("approve_deploy precondition should mention --environment; got %q", na.Actions[0].Precondition)
				}
				if !strings.Contains(na.Actions[0].Precondition, "write:deploy") {
					t.Errorf("approve_deploy precondition should mention write:deploy; got %q", na.Actions[0].Precondition)
				}
				reject := findAction(t, na, "fishhawk_reject_deploy")
				if reject.Params["run_id"] != run.ID {
					t.Errorf("reject_deploy params = %v, want run_id=%s", reject.Params, run.ID)
				}
			}
			// Every action carries the structured fields.
			for i, a := range na.Actions {
				if a.Precondition == "" || a.Consumes == "" || a.Reason == "" {
					t.Errorf("actions[%d] (%s) missing precondition/consumes/reason: %+v", i, a.Action, a)
				}
			}
		})
	}
}

// TestNextActions_DeployStage_TerminalFallsThrough pins that a TERMINAL deploy
// stage does NOT enter the deploy arm (the `!stageStateIsTerminal` guard): a
// succeeded deploy stage on a succeeded run falls through to the run-state
// terminal block, not deploy_gate_parked.
func TestNextActions_DeployStage_TerminalFallsThrough(t *testing.T) {
	run := naRun("succeeded")
	run.WorkflowID = "release"
	stages := []Stage{naStage("deploy", "succeeded")}

	na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, "", "", releaseSignals{})
	if na == nil {
		t.Fatal("nextActionsFor = nil")
	}
	if na.State == "deploy_gate_parked" || na.State == "deploy_in_flight" || na.State == "deploy_initializing" {
		t.Errorf("terminal deploy stage entered the deploy arm (state=%q); the !stageStateIsTerminal guard should skip it", na.State)
	}
}

// TestNextActions_AwaitingScopeDecision_DecideArm pins the #1231 arm: an
// implement stage parked at awaiting_scope_decision offers
// fishhawk_decide_scope_completeness carrying run_id + the exempt|fail
// decision hint, and the reason names the zero-re-run exempt semantics.
func TestNextActions_AwaitingScopeDecision_DecideArm(t *testing.T) {
	run := naRun("running")
	stages := []Stage{naStage("plan", "succeeded"), naStage("implement", "awaiting_scope_decision")}

	na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, "", "", releaseSignals{})
	if na == nil || na.State != "implement_awaiting_scope_decision" {
		t.Fatalf("state = %+v, want implement_awaiting_scope_decision", na)
	}
	dec := findAction(t, na, "fishhawk_decide_scope_completeness")
	if dec.Params["run_id"] != run.ID {
		t.Errorf("decide params = %v, want run_id=%s", dec.Params, run.ID)
	}
	if dec.Params["decision"] != "exempt|fail" {
		t.Errorf("decide params = %v, want the exempt|fail decision hint", dec.Params)
	}
	if !strings.Contains(dec.Reason, "no agent re-run") && !strings.Contains(dec.Reason, "NO agent re-run") {
		t.Errorf("decide reason should name the zero-re-run exempt semantics; got %q", dec.Reason)
	}
}

// TestNextActions_UnclassifiedFallback pins the structural fallback the
// approval conditions mandate: a synthetic non-terminal state the table
// does not match must classify as "unclassified" with a re-poll action
// AND a file-a-product-issue pointer naming the state — never an empty
// actions list.
func TestNextActions_UnclassifiedFallback(t *testing.T) {
	run := naRun("running")
	// A lone review-type stage matches no plan/implement arm while the
	// run is non-terminal — the synthetic unmatched fixture.
	stages := []Stage{naStage("review", "succeeded")}

	na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, "", "", releaseSignals{})
	if na == nil {
		t.Fatal("nextActionsFor returned nil")
	}
	if na.State != "unclassified" {
		t.Errorf("state = %q, want unclassified", na.State)
	}
	if len(na.Actions) == 0 {
		t.Fatal("unclassified fallback returned zero actions — the invariant is structural, it must never be empty for a non-terminal run")
	}
	poll := findAction(t, na, "fishhawk_get_run_status")
	if !strings.Contains(poll.Reason, `"running"`) {
		t.Errorf("re-poll reason should name the run state; got %q", poll.Reason)
	}
	issue := findAction(t, na, "file_product_issue")
	if !strings.Contains(issue.Reason, "review=succeeded") {
		t.Errorf("file_product_issue reason should name the unmatched stage shape; got %q", issue.Reason)
	}
}

// TestNextActions_DriveActionFoldsFirst pins the drive-fold-first
// invariant: when the drive read view carries a distilled next action,
// it becomes the FIRST entry so drive and next_actions never point
// different ways.
func TestNextActions_DriveActionFoldsFirst(t *testing.T) {
	prURL := "https://github.com/x/y/pull/42"
	run := naRun("running")
	run.PullRequestURL = &prURL
	stages := []Stage{naStage("plan", "succeeded"), naStage("implement", "succeeded")}
	drive := &DriveStatus{
		Drive:      true,
		NextAction: &RunNextAction{Action: "merge_pr", Detail: "all gates resolved", PRURL: prURL},
	}

	na := nextActionsFor(run, stages, nil, naReviewStatus("implement", "complete"), nil, drive, false, false, "", "", releaseSignals{})
	if na == nil || len(na.Actions) == 0 {
		t.Fatalf("nextActionsFor = %+v, want the drive action folded in first", na)
	}
	if na.Actions[0].Action != "merge_pr" {
		t.Errorf("actions[0] = %q, want the drive next_action merge_pr first", na.Actions[0].Action)
	}
	if na.Actions[0].Reason != "all gates resolved" {
		t.Errorf("actions[0].reason = %q, want the drive detail", na.Actions[0].Reason)
	}
	if na.Actions[0].Params["pr_url"] != prURL {
		t.Errorf("actions[0].params.pr_url = %q, want %q", na.Actions[0].Params["pr_url"], prURL)
	}
}

// TestNextActions_CategoryAFlakeCitation pins the best-effort flake
// citation: with the verify_infra_flake_retry signature in the failure
// detail the retry reason cites it; without it the retry action is
// still emitted, uncited.
func TestNextActions_CategoryAFlakeCitation(t *testing.T) {
	run := naRun("failed")
	cited := nextActionsFor(run, []Stage{naStage("plan", "succeeded"),
		naFailedImplement("A", "verify aborted after verify_infra_flake_retry")}, nil, nil, nil, nil, false, false, "", "", releaseSignals{})
	retry := findAction(t, cited, "fishhawk_retry_stage")
	if !strings.Contains(retry.Reason, "verify_infra_flake_retry") {
		t.Errorf("retry reason should cite the flake trace event; got %q", retry.Reason)
	}

	uncited := nextActionsFor(run, []Stage{naStage("plan", "succeeded"),
		naFailedImplement("A", "agent crashed")}, nil, nil, nil, nil, false, false, "", "", releaseSignals{})
	retry = findAction(t, uncited, "fishhawk_retry_stage")
	if strings.Contains(retry.Reason, "verify_infra_flake_retry") {
		t.Errorf("retry reason cites a flake event the failure detail does not carry: %q", retry.Reason)
	}
}

// TestNextActions_CategoryAExternalAPICitation pins the #1548 incident
// hint: a category-A failure whose reason carries the runner's stable
// "terminal external API error <N>" phrase names the status code and points
// the operator at status.claude.com to back off; a plain category-A failure
// keeps the generic retry reason with no status code. This pair locks the
// runner->next_actions FailureReason string contract across the module
// boundary (the parse end; the emit end is locked in the runner's
// claudecode/main tests).
func TestNextActions_CategoryAExternalAPICitation(t *testing.T) {
	run := naRun("failed")
	cited := nextActionsFor(run, []Stage{naStage("plan", "succeeded"),
		naFailedImplement("A", "terminal external API error 529 (retries exhausted): exit status 1")},
		nil, nil, nil, nil, false, false, "", "", releaseSignals{})
	retry := findAction(t, cited, "fishhawk_retry_stage")
	if !strings.Contains(retry.Reason, "529") {
		t.Errorf("retry reason should name the 529 status; got %q", retry.Reason)
	}
	if !strings.Contains(retry.Reason, "status.claude.com") {
		t.Errorf("retry reason should point at status.claude.com; got %q", retry.Reason)
	}

	// A plain category-A failure keeps the generic reason and names no status.
	uncited := nextActionsFor(run, []Stage{naStage("plan", "succeeded"),
		naFailedImplement("A", "agent crashed")}, nil, nil, nil, nil, false, false, "", "", releaseSignals{})
	retry = findAction(t, uncited, "fishhawk_retry_stage")
	if strings.Contains(retry.Reason, "529") || strings.Contains(retry.Reason, "status.claude.com") {
		t.Errorf("generic category-A retry reason must not cite an external-API incident: %q", retry.Reason)
	}
}

// TestNextActions_FailedRunOffersReviveRun pins the #1915 addition: both the
// category-A arm and the default (retryable) arm surface fishhawk_revive_run
// keyed to the run id, with a reason that distinguishes the batch no-dispatch
// re-park from the single-stage auto-dispatching retry. This is the arm that
// makes the one-verb revive discoverable when a sibling failure flips the run
// terminal.
func TestNextActions_FailedRunOffersReviveRun(t *testing.T) {
	run := naRun("failed")

	for _, tc := range []struct {
		name  string
		stage Stage
	}{
		{"category_a", naFailedImplement("A", "agent crashed")},
		{"category_c_default_arm", naFailedImplement("C", "infra")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			na := nextActionsFor(run, []Stage{naStage("plan", "succeeded"), tc.stage},
				nil, nil, nil, nil, false, false, "", "", releaseSignals{})
			revive := findAction(t, na, "fishhawk_revive_run")
			if revive.Params["run_id"] != run.ID {
				t.Errorf("revive params run_id = %q, want %q", revive.Params["run_id"], run.ID)
			}
			if revive.Consumes != consumesRetryBudget {
				t.Errorf("revive consumes = %q, want %q", revive.Consumes, consumesRetryBudget)
			}
			// The reason must distinguish revive (re-park, no dispatch) from
			// retry (re-open + auto-dispatch).
			lower := strings.ToLower(revive.Reason)
			if !strings.Contains(lower, "without dispatching") {
				t.Errorf("revive reason must state it re-parks without dispatching; got %q", revive.Reason)
			}
			if !strings.Contains(lower, "fishhawk_retry_stage") {
				t.Errorf("revive reason must contrast with fishhawk_retry_stage; got %q", revive.Reason)
			}
		})
	}
}

// TestCitedExternalAPIStatus pins the best-effort parse helper: the status
// after the stable phrase is extracted; a nil/absent/malformed reason yields
// (0, false) so the caller keeps the generic hint.
func TestCitedExternalAPIStatus(t *testing.T) {
	mk := func(reason string) *Stage {
		s := naStage("implement", "failed")
		if reason != "__nil__" {
			s.FailureReason = &reason
		}
		return &s
	}
	cases := []struct {
		name       string
		stage      *Stage
		wantStatus int
		wantOK     bool
	}{
		{"nil stage", nil, 0, false},
		{"nil reason", mk("__nil__"), 0, false},
		{"absent phrase", mk("agent exited with error: exit status 1"), 0, false},
		{"529", mk("terminal external API error 529 (retries exhausted): x"), 529, true},
		{"503 at end", mk("terminal external API error 503"), 503, true},
		{"phrase but no digits", mk("terminal external API error : boom"), 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotStatus, gotOK := citedExternalAPIStatus(tc.stage)
			if gotStatus != tc.wantStatus || gotOK != tc.wantOK {
				t.Errorf("citedExternalAPIStatus = (%d, %v), want (%d, %v)", gotStatus, gotOK, tc.wantStatus, tc.wantOK)
			}
		})
	}
}

// TestNextActions_ResumeRunNamesThisRunAsParent pins the #1022
// structural fix: the category-B recovery action's parent_run_id is
// THIS run's id (the failed run resume_run takes), so the remediation
// text can never go stale against a different run.
func TestNextActions_ResumeRunNamesThisRunAsParent(t *testing.T) {
	run := naRun("failed")
	na := nextActionsFor(run, []Stage{naStage("plan", "succeeded"),
		naFailedImplement("B", "undeclared created file")}, nil, nil, nil, nil, false, false, "", "", releaseSignals{})
	resume := findAction(t, na, "fishhawk_resume_run")
	if resume.Params["parent_run_id"] != run.ID {
		t.Errorf("resume_run params.parent_run_id = %q, want this run's id %s", resume.Params["parent_run_id"], run.ID)
	}
}

// TestNextActions_AwaitingParentConsolidationPointsAtParent pins the
// #1082 load-bearing param: the single read-only poll the
// awaiting_parent_consolidation arm emits targets the PARENT run id
// (*run.ParentRunID), not the child's own id or an empty value. A
// regression returning either would still satisfy the state-table's
// action-name/consumes assertions while breaking the intended parent
// poll, so this is asserted on the param directly.
func TestNextActions_AwaitingParentConsolidationPointsAtParent(t *testing.T) {
	r := naRun("succeeded")
	parent := uuid.NewString()
	r.ParentRunID = &parent
	na := nextActionsFor(r, []Stage{naStage("implement", "succeeded")},
		nil, naReviewStatus("implement", "pending"), nil, nil, false, false, "", "", releaseSignals{})
	if na.State != "awaiting_parent_consolidation" {
		t.Fatalf("state = %q, want awaiting_parent_consolidation", na.State)
	}
	poll := findAction(t, na, "fishhawk_get_run_status")
	if poll.Params["run_id"] != parent {
		t.Errorf("poll params.run_id = %q, want the PARENT run id %q (not the child's own id %q)", poll.Params["run_id"], parent, r.ID)
	}
}

// TestNextActions_NilRun pins the nil guard.
func TestNextActions_NilRun(t *testing.T) {
	if na := nextActionsFor(nil, nil, nil, nil, nil, nil, false, false, "", "", releaseSignals{}); na != nil {
		t.Errorf("nextActionsFor(nil run) = %+v, want nil", na)
	}
}

// TestNextActions_PlanReviewPendingDoesNotOfferApproval pins the
// wait-for-the-agent-plan-review discipline: while the plan review is
// pending, the block must NOT contain an approval action.
func TestNextActions_PlanReviewPendingDoesNotOfferApproval(t *testing.T) {
	run := naRun("running")
	na := nextActionsFor(run, []Stage{naStage("plan", "awaiting_approval")},
		naReviewStatus("plan", "pending"), nil, nil, nil, false, false, "", "", releaseSignals{})
	for _, a := range na.Actions {
		if a.Action == "fishhawk_approve_plan" {
			t.Error("approve_plan offered while the plan review is still pending — the verdict must be read first")
		}
	}
}

// TestNextActions_CIFailedRoutable pins the negative-mirror routable arm
// (#1045): a drive run whose derived_status is ci_failed WITH open
// concerns (hint != nil) classifies ci_failed_routable and leads with
// fishhawk_fixup_stage (consuming fixup_budget) carrying the implement
// stage id, then a no-cost rerun_ci_checks flake path. The merge ritual
// is NOT offered — a red required check is not mergeable.
func TestNextActions_CIFailedRoutable(t *testing.T) {
	prURL := "https://github.com/x/y/pull/42"
	run := naRun("running")
	run.PullRequestURL = &prURL
	impl := naStage("implement", "awaiting_approval")
	stages := []Stage{naStage("plan", "succeeded"), impl}
	drive := &DriveStatus{Drive: true, DerivedStatus: "ci_failed"}
	hint := &ReviewActionHint{Concerns: 2, RemainingFixupBudget: 1}

	na := nextActionsFor(run, stages, nil, naReviewStatus("implement", "complete"), hint, drive, false, false, "", "", releaseSignals{})
	if na.State != "ci_failed_routable" {
		t.Fatalf("state = %q, want ci_failed_routable", na.State)
	}
	if na.Actions[0].Action != "fishhawk_fixup_stage" {
		t.Errorf("actions[0] = %q, want fishhawk_fixup_stage first", na.Actions[0].Action)
	}
	if na.Actions[0].Consumes != consumesFixupBudget {
		t.Errorf("fixup consumes = %q, want fixup_budget", na.Actions[0].Consumes)
	}
	if na.Actions[0].Params["stage_id"] != impl.ID {
		t.Errorf("fixup stage_id = %q, want the implement stage id %q", na.Actions[0].Params["stage_id"], impl.ID)
	}
	// #1549: the fix-up precondition must not tell the operator to checkout the
	// run branch (which CAUSES the worktree-conflict failure) and must name the
	// runner's lineage worktree.
	if na.Actions[0].Precondition == "" {
		t.Error("fixup precondition must be non-empty")
	}
	if strings.Contains(na.Actions[0].Precondition, "checkout the run branch") {
		t.Errorf("fixup precondition still says to checkout the run branch: %q", na.Actions[0].Precondition)
	}
	if !strings.Contains(na.Actions[0].Precondition, "lineage worktree") {
		t.Errorf("fixup precondition should name the lineage worktree; got %q", na.Actions[0].Precondition)
	}
	findAction(t, na, "rerun_ci_checks")
	for _, a := range na.Actions {
		if a.Action == "merge_pr" || a.Action == "approve_pr" || a.Action == "merge_and_file_follow_up" {
			t.Errorf("merge ritual action %q offered on a red required check — ci_failed is not mergeable", a.Action)
		}
	}
}

// TestNextActions_CIFailedUnroutable pins the structurally-unroutable arm
// (#1045 / #1044): a ci_failed drive run with NO open concerns (hint ==
// nil) classifies ci_failed_unroutable and offers commit_and_vouch (the
// operator-remediation arm) first, then rerun_ci_checks, then page_human.
func TestNextActions_CIFailedUnroutable(t *testing.T) {
	prURL := "https://github.com/x/y/pull/42"
	run := naRun("running")
	run.PullRequestURL = &prURL
	stages := []Stage{naStage("plan", "succeeded"), naStage("implement", "awaiting_approval")}
	drive := &DriveStatus{Drive: true, DerivedStatus: "ci_failed"}

	na := nextActionsFor(run, stages, nil, naReviewStatus("implement", "complete"), nil, drive, false, false, "", "", releaseSignals{})
	if na.State != "ci_failed_unroutable" {
		t.Fatalf("state = %q, want ci_failed_unroutable", na.State)
	}
	if got := actionNames(na); len(got) != 3 || got[0] != "commit_and_vouch" || got[1] != "rerun_ci_checks" || got[2] != "page_human" {
		t.Fatalf("actions = %v, want [commit_and_vouch rerun_ci_checks page_human]", got)
	}
	for i, a := range na.Actions {
		if a.Precondition == "" || a.Consumes == "" || a.Reason == "" {
			t.Errorf("actions[%d] (%s) missing structured fields: %+v", i, a.Action, a)
		}
	}
}

// TestNextActions_CIFailedFoldsDriveNextActionFirst pins that the drive
// next_action still folds in FIRST on the ci_failed path, so the
// classify_ci_failure distilled step leads and drive/next_actions agree.
func TestNextActions_CIFailedFoldsDriveNextActionFirst(t *testing.T) {
	prURL := "https://github.com/x/y/pull/42"
	run := naRun("running")
	run.PullRequestURL = &prURL
	stages := []Stage{naStage("plan", "succeeded"), naStage("implement", "awaiting_approval")}
	drive := &DriveStatus{
		Drive:         true,
		DerivedStatus: "ci_failed",
		NextAction:    &RunNextAction{Action: "classify_ci_failure", Detail: "required PR checks red", PRURL: prURL},
	}

	na := nextActionsFor(run, stages, nil, naReviewStatus("implement", "complete"), nil, drive, false, false, "", "", releaseSignals{})
	if na.Actions[0].Action != "classify_ci_failure" {
		t.Errorf("actions[0] = %q, want the drive next_action classify_ci_failure folded first", na.Actions[0].Action)
	}
}

// TestNextActions_SliceIntegrationConflict pins the ADR-041 / #1142 arm:
// a decomposed PARENT whose implement (awaiting_children) stage failed
// category-B with the stable "slice integration conflict" reason prefix
// classifies slices_integration_conflict and points fishhawk_resume_run at
// the CONFLICTING child via a field-path POINTER into the structured
// slice_integration_conflict audit payload (conflicting_child_run_id) — the
// resume target is sourced from structured data, NOT parsed from the reason
// string. (The field-path-pointer idiom mirrors ci_failed's concern_ids.)
func TestNextActions_SliceIntegrationConflict(t *testing.T) {
	run := naRun("failed")
	stages := []Stage{
		naStage("plan", "succeeded"),
		naFailedImplement("B", "slice integration conflict: slice 2 could not merge"),
		naStage("review", "pending"),
	}

	na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, "", "", releaseSignals{})
	if na.State != "slices_integration_conflict" {
		t.Fatalf("state = %q, want slices_integration_conflict", na.State)
	}
	resume := findAction(t, na, "fishhawk_resume_run")
	// The resume target is a field-path pointer at the STRUCTURED audit
	// field, never the reason string.
	if got := resume.Params["parent_run_id"]; !strings.Contains(got, "slice_integration_conflict") || !strings.Contains(got, "conflicting_child_run_id") {
		t.Errorf("resume params.parent_run_id = %q, want a field-path pointer into the slice_integration_conflict audit payload's conflicting_child_run_id", got)
	}
}

// TestNextActions_OrdinaryCategoryBParentUnaffected pins that an ordinary
// category-B parent failure (no slice-conflict reason prefix) still routes
// to the existing implement_failed_category_b arm — the conflict arm wins
// ONLY for the conflict-prefixed reason.
func TestNextActions_OrdinaryCategoryBParentUnaffected(t *testing.T) {
	run := naRun("failed")
	stages := []Stage{naStage("plan", "succeeded"), naFailedImplement("B", "undeclared created file")}

	na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, "", "", releaseSignals{})
	if na.State != "implement_failed_category_b" {
		t.Errorf("state = %q, want implement_failed_category_b for an ordinary category-B failure", na.State)
	}
}

// TestNextActions_SucceededMerged pins the #1370 lifecycle-owns-its-tail
// arm: a succeeded run with an open PR URL AND mergeObserved=true (a
// post_merge_observed audit entry was seen) classifies succeeded_merged,
// surfacing ONLY the operator post_merge dev-host step and dropping the
// now-completed approve_pr / merge_pr ritual steps.
func TestNextActions_SucceededMerged(t *testing.T) {
	prURL := "https://github.com/x/y/pull/42"
	run := naRun("succeeded")
	run.PullRequestURL = &prURL
	stages := []Stage{naStage("plan", "succeeded"), naStage("implement", "succeeded")}

	na := nextActionsFor(run, stages, nil, naReviewStatus("implement", "complete"), nil, nil, true, false, "", "", releaseSignals{})
	if na == nil || na.State != "succeeded_merged" {
		t.Fatalf("state = %+v, want succeeded_merged", na)
	}
	// The post_merge dev-host step survives (rebuild/reload stays an
	// operator concern, ADR-038).
	post := findAction(t, na, "post_merge")
	if !strings.Contains(post.Reason, "scripts/dev post-merge") {
		t.Errorf("post_merge reason should name scripts/dev post-merge; got %q", post.Reason)
	}
	// The now-completed merge ritual steps are gone.
	for _, a := range na.Actions {
		if a.Action == "approve_pr" || a.Action == "merge_pr" {
			t.Errorf("merge ritual action %q surfaced on a merged run — approve/merge are already done", a.Action)
		}
	}
}

// TestNextActions_SucceededPROpenUnchangedWhenMergeNotObserved pins the
// negative mirror of #1370: a succeeded run with an open PR but
// mergeObserved=false keeps the prior succeeded_pr_open state and the
// full approve_pr -> merge_pr -> post_merge ritual.
func TestNextActions_SucceededPROpenUnchangedWhenMergeNotObserved(t *testing.T) {
	prURL := "https://github.com/x/y/pull/42"
	run := naRun("succeeded")
	run.PullRequestURL = &prURL
	stages := []Stage{naStage("plan", "succeeded"), naStage("implement", "succeeded")}

	na := nextActionsFor(run, stages, nil, naReviewStatus("implement", "complete"), nil, nil, false, false, "", "", releaseSignals{})
	if na == nil || na.State != "succeeded_pr_open" {
		t.Fatalf("state = %+v, want succeeded_pr_open", na)
	}
	if got := actionNames(na); len(got) != 3 || got[0] != "approve_pr" || got[1] != "merge_pr" || got[2] != "post_merge" {
		t.Fatalf("actions = %v, want [approve_pr merge_pr post_merge]", got)
	}
}

// TestNextActions_SucceededAcceptanceSkippedOutOfScope pins the E38.3 (#1657)
// arm: a succeeded run with an open PR AND the acceptanceSkippedOutOfScope flag
// set classifies succeeded_acceptance_skipped_out_of_scope and STILL returns the
// full merge ritual (approve_pr -> merge_pr -> post_merge) — the run stays
// merge-eligible. The graceful-degradation control proves the flag gates ONLY
// the label: with the flag false (the skip entry aged out of the recent window)
// the same run falls back to plain succeeded_pr_open, itself merge-eligible.
func TestNextActions_SucceededAcceptanceSkippedOutOfScope(t *testing.T) {
	prURL := "https://github.com/x/y/pull/42"
	run := naRun("succeeded")
	run.PullRequestURL = &prURL
	stages := []Stage{naStage("plan", "succeeded"), naStage("implement", "succeeded")}

	t.Run("flag set -> labeled state, still merge-eligible", func(t *testing.T) {
		na := nextActionsFor(run, stages, nil, naReviewStatus("implement", "complete"), nil, nil, false, true, "", "", releaseSignals{})
		if na == nil || na.State != "succeeded_acceptance_skipped_out_of_scope" {
			t.Fatalf("state = %+v, want succeeded_acceptance_skipped_out_of_scope", na)
		}
		if got := actionNames(na); len(got) != 3 || got[0] != "approve_pr" || got[1] != "merge_pr" || got[2] != "post_merge" {
			t.Fatalf("actions = %v, want the full merge ritual [approve_pr merge_pr post_merge]", got)
		}
	})

	t.Run("flag false (aged out) -> falls back to succeeded_pr_open", func(t *testing.T) {
		na := nextActionsFor(run, stages, nil, naReviewStatus("implement", "complete"), nil, nil, false, false, "", "", releaseSignals{})
		if na == nil || na.State != "succeeded_pr_open" {
			t.Fatalf("state = %+v, want succeeded_pr_open (graceful degradation)", na)
		}
		if got := actionNames(na); len(got) != 3 || got[0] != "approve_pr" {
			t.Fatalf("actions = %v, want the merge ritual", got)
		}
	})

	// mergeObserved wins over the skip flag: once the merge is observed the run
	// is succeeded_merged regardless of the acceptance-skip label.
	t.Run("mergeObserved wins", func(t *testing.T) {
		na := nextActionsFor(run, stages, nil, naReviewStatus("implement", "complete"), nil, nil, true, true, "", "", releaseSignals{})
		if na == nil || na.State != "succeeded_merged" {
			t.Fatalf("state = %+v, want succeeded_merged", na)
		}
	})
}

// TestAcceptanceSkippedOutOfScopeIn pins the E38.3 detector: true for a recent
// slice carrying an acceptance_skipped_out_of_scope entry, false otherwise.
func TestAcceptanceSkippedOutOfScopeIn(t *testing.T) {
	if !acceptanceSkippedOutOfScopeIn([]AuditEntry{
		{Category: "pr_opened"},
		{Category: "acceptance_skipped_out_of_scope"},
	}) {
		t.Error("acceptanceSkippedOutOfScopeIn = false, want true when the marker is present")
	}
	if acceptanceSkippedOutOfScopeIn([]AuditEntry{{Category: "acceptance_dispatched"}}) {
		t.Error("acceptanceSkippedOutOfScopeIn = true, want false when no marker is present")
	}
}

// TestMergeObservedIn pins the #1370 detector: it returns true for a
// recent-audit slice carrying a post_merge_observed entry and false for a
// slice with only other categories.
func TestMergeObservedIn(t *testing.T) {
	if !mergeObservedIn([]AuditEntry{
		{Category: "pr_merged"},
		{Category: "post_merge_observed"},
	}) {
		t.Error("mergeObservedIn = false, want true when a post_merge_observed entry is present")
	}
	if mergeObservedIn([]AuditEntry{
		{Category: "pr_merged"},
		{Category: "work_item_transitioned"},
	}) {
		t.Error("mergeObservedIn = true, want false when no post_merge_observed entry is present")
	}
	if mergeObservedIn(nil) {
		t.Error("mergeObservedIn(nil) = true, want false")
	}
}

// --- campaign next-actions arm (E25.8 / #1447) ---

// caRollup is a small helper: a campaign rollup with one issue in the named
// slice (the slice name is not load-bearing for campaignNextActionsFor, which
// keys off the next_action — the rollup is carried for completeness).
func caNextAction(action, issueRef string) CampaignNextAction {
	return CampaignNextAction{Action: action, IssueRef: issueRef, Detail: "detail for " + action}
}

func TestCampaignNextActionsFor_Attention(t *testing.T) {
	na := campaignNextActionsFor(CampaignRollup{Failed: []string{"#27"}}, caNextAction("attention", "#27"))
	if na.State != "campaign_attention" {
		t.Errorf("State = %q, want campaign_attention", na.State)
	}
	if len(na.Actions) == 0 {
		t.Fatal("attention must carry at least one action")
	}
	got := na.Actions[0]
	if got.Action != "fishhawk_get_run_status" {
		t.Errorf("action = %q, want fishhawk_get_run_status", got.Action)
	}
	if got.Consumes != consumesNone {
		t.Errorf("consumes = %q, want none", got.Consumes)
	}
	if got.Params["issue_ref"] != "#27" {
		t.Errorf("issue_ref param = %q, want #27", got.Params["issue_ref"])
	}
	// #1838: the prose must NO LONGER promise the retry/abandon verbs that refuse
	// on a failed item — the whole point of the fix. It must say the item is not
	// auto-restartable (a restartable item surfaces as start_run instead).
	for _, s := range []string{got.Precondition, got.Reason} {
		if strings.Contains(s, "abandon") {
			t.Errorf("attention prose still promises abandon: %q", s)
		}
	}
	if !strings.Contains(got.Precondition+got.Reason, "auto-restart") {
		t.Errorf("attention prose = %q / %q, want it to explain the item is not auto-restartable", got.Precondition, got.Reason)
	}
}

func TestCampaignNextActionsFor_Resume(t *testing.T) {
	na := campaignNextActionsFor(CampaignRollup{Paused: []string{"#28"}}, caNextAction("resume", "#28"))
	if na.State != "campaign_paused" {
		t.Errorf("State = %q, want campaign_paused", na.State)
	}
	got := na.Actions[0]
	if got.Action != "fishhawk_resume_campaign" {
		t.Errorf("action = %q, want fishhawk_resume_campaign", got.Action)
	}
	if got.Consumes != consumesNone {
		t.Errorf("consumes = %q, want none", got.Consumes)
	}
}

// TestCampaignNextActionsFor_StartRun asserts a FRESH ELIGIBLE campaign item
// (in the rollup's eligible slice, no run yet) keeps the established
// fishhawk_start_run dispatch verb — there is no item to restart, so a plain run
// on the issue ref advances the campaign. The restart verb is reserved for the
// restartable path (TestCampaignNextActionsFor_StartRun_Restartable).
func TestCampaignNextActionsFor_StartRun(t *testing.T) {
	na := campaignNextActionsFor(CampaignRollup{Eligible: []string{"#26"}}, caNextAction("start_run", "#26"))
	if na.State != "campaign_start_run" {
		t.Errorf("State = %q, want campaign_start_run", na.State)
	}
	got := na.Actions[0]
	if got.Action != "fishhawk_start_run" {
		t.Errorf("action = %q, want fishhawk_start_run", got.Action)
	}
	if got.Consumes != consumesNewRun {
		t.Errorf("consumes = %q, want new_run", got.Consumes)
	}
	if got.Params["trigger_ref"] != "#26" {
		t.Errorf("trigger_ref param = %q, want #26", got.Params["trigger_ref"])
	}
}

// TestCampaignNextActionsFor_StartRun_Restartable asserts a RESTARTABLE
// failed/cancelled item (server-side computeCampaignNextAction surfaces both
// eligible and restartable as start_run, #1729/#1838) surfaces
// fishhawk_start_campaign_item_run — the ONLY verb that reaches the restart
// handler (handleStartCampaignItemRun) which resets the item and mints a fresh
// re-linked run. The generic fishhawk_start_run neither restarts nor links, so a
// test asserting it would pass while the advertised failed-item recovery path
// stays unexercised.
func TestCampaignNextActionsFor_StartRun_Restartable(t *testing.T) {
	// Restartable items are folded into the wire cancelled slice
	// (toCampaignRollupPayload).
	na := campaignNextActionsFor(CampaignRollup{Cancelled: []string{"#40"}}, caNextAction("start_run", "#40"))
	if na.State != "campaign_start_run" {
		t.Errorf("State = %q, want campaign_start_run", na.State)
	}
	if len(na.Actions) == 0 {
		t.Fatal("start_run must carry at least one action")
	}
	got := na.Actions[0]
	if got.Action != "fishhawk_start_campaign_item_run" {
		t.Errorf("action = %q, want fishhawk_start_campaign_item_run — the verb that reaches the restart handler", got.Action)
	}
	if got.Action == "fishhawk_start_run" {
		t.Error("a restartable failed item must NOT surface the generic fishhawk_start_run — it never reaches the restart handler")
	}
	if got.Params["issue_ref"] != "#40" {
		t.Errorf("issue_ref param = %q, want #40", got.Params["issue_ref"])
	}
	// The reason must name the restart path (this is the wire cancelled slice, so
	// the classifier distinguishes it from a fresh eligible start via the rollup).
	if !strings.Contains(got.Reason, "restart") {
		t.Errorf("reason = %q, want it to name the restart path for a restartable item", got.Reason)
	}
}

// TestCampaignNextActionsFor_AttendHumanLed asserts the attend_human_led arm
// returns the classified campaign_attend_human_led state (NOT the unclassified
// fallback) with a non-dispatch, page-the-human suggested action that consumes
// nothing — the operator-agent must lead the item by hand, not mint a run.
func TestCampaignNextActionsFor_AttendHumanLed(t *testing.T) {
	na := campaignNextActionsFor(CampaignRollup{HumanLed: []string{"#12"}}, caNextAction("attend_human_led", "#12"))
	if na.State != "campaign_attend_human_led" {
		t.Errorf("State = %q, want campaign_attend_human_led", na.State)
	}
	if len(na.Actions) == 0 {
		t.Fatalf("attend_human_led must carry at least one action")
	}
	got := na.Actions[0]
	if got.Action == "fishhawk_start_run" {
		t.Errorf("action = %q, human-led work must NOT recommend fishhawk_start_run", got.Action)
	}
	if got.Consumes != consumesNone {
		t.Errorf("consumes = %q, want none (no run minted for human-led work)", got.Consumes)
	}
}

func TestCampaignNextActionsFor_Wait(t *testing.T) {
	na := campaignNextActionsFor(CampaignRollup{Running: []string{"#29"}}, caNextAction("wait", ""))
	if na.State != "campaign_wait" {
		t.Errorf("State = %q, want campaign_wait", na.State)
	}
	got := na.Actions[0]
	if got.Action != "fishhawk_get_campaign_status" {
		t.Errorf("action = %q, want fishhawk_get_campaign_status", got.Action)
	}
	if got.Consumes != consumesNone {
		t.Errorf("consumes = %q, want none", got.Consumes)
	}
}

func TestCampaignNextActionsFor_Complete_TerminalNoActions(t *testing.T) {
	na := campaignNextActionsFor(CampaignRollup{Done: []string{"#26", "#27"}}, caNextAction("complete", ""))
	if na.State != "campaign_complete" {
		t.Errorf("State = %q, want campaign_complete", na.State)
	}
	if len(na.Actions) != 0 {
		t.Errorf("complete is terminal; want nil actions, got %+v", na.Actions)
	}
}

// TestCampaignNextActionsFor_UnknownAction_Unclassified pins the "never
// unclassified" invariant: a future backend-added action value lands in the
// labeled fallback with a NON-empty actions list — proving the classifier
// never returns an empty/unrouted result for a non-complete campaign.
func TestCampaignNextActionsFor_UnknownAction_Unclassified(t *testing.T) {
	na := campaignNextActionsFor(CampaignRollup{}, caNextAction("teleport", "#99"))
	if na.State != "campaign_unclassified" {
		t.Errorf("State = %q, want campaign_unclassified", na.State)
	}
	if len(na.Actions) == 0 {
		t.Fatal("the unclassified fallback must return a non-empty actions list")
	}
	names := actionNames(na)
	var sawPoll, sawFile bool
	for _, n := range names {
		switch n {
		case "fishhawk_get_campaign_status":
			sawPoll = true
		case "file_product_issue":
			sawFile = true
		}
	}
	if !sawPoll || !sawFile {
		t.Errorf("unclassified actions = %v, want both a re-poll and file_product_issue", names)
	}
}

// --- acceptance-stage next-actions arm (E31.9 / ADR-049) -------------------

// naAcceptanceStages builds the settled-implement stage list plus an
// acceptance stage in the given state: [plan succeeded, implement succeeded,
// acceptance <state>]. The implement review is complete + no concerns, so the
// classifier reaches the settled implement path where the acceptance arm lives.
func naAcceptanceStages(acceptanceState string) []Stage {
	return []Stage{
		naStage("plan", "succeeded"),
		naStage("implement", "succeeded"),
		naStage("acceptance", acceptanceState),
	}
}

func naLocalRun(state string) *Run {
	r := naRun(state)
	r.RunnerKind = "local"
	return r
}

// TestNextActions_AcceptanceStateTable drives every acceptance-arm mode the
// issue names (dispatch -> await -> triage -> merge), one behavioral assertion
// per enumerated failure mode (#1199). Each case pins the exact state label and
// the ordered first action(s).
func TestNextActions_AcceptanceStateTable(t *testing.T) {
	prURL := "https://github.com/x/y/pull/42"
	withPR := func(r *Run) *Run { r.PullRequestURL = &prURL; return r }

	cases := []struct {
		name        string
		run         *Run
		stages      []Stage
		verdict     string
		disposition string
		wantState   string
		wantActions []string // prefix-exact (first N actions), full when short
	}{
		{
			// (1) acceptance pending + runner_kind local -> dispatch arm, dispatch first.
			name:        "1_acceptance_pending_local_dispatch",
			run:         naLocalRun("running"),
			stages:      naAcceptanceStages("pending"),
			wantState:   "acceptance_pending",
			wantActions: []string{"fishhawk_dispatch_stage", "fishhawk_run_stage"},
		},
		{
			// (2) acceptance pending + github_actions -> poll.
			name: "2_acceptance_pending_github_actions_poll",
			run: func() *Run {
				r := naRun("running")
				r.RunnerKind = "github_actions"
				return r
			}(),
			stages:      naAcceptanceStages("pending"),
			wantState:   "acceptance_pending",
			wantActions: []string{"fishhawk_get_run_status"},
		},
		{
			// (3) acceptance running -> poll.
			name:        "3_acceptance_running_poll",
			run:         naLocalRun("running"),
			stages:      naAcceptanceStages("running"),
			wantState:   "acceptance_running",
			wantActions: []string{"fishhawk_get_run_status"},
		},
		{
			// (4) acceptance succeeded + verdict passed -> acceptance_passed + merge ritual.
			name:        "4_acceptance_passed_merge_ritual",
			run:         withPR(naLocalRun("running")),
			stages:      naAcceptanceStages("succeeded"),
			verdict:     "passed",
			wantState:   "acceptance_passed",
			wantActions: []string{"approve_pr", "merge_pr", "post_merge"},
		},
		{
			// (6) fixup_dispatched with the implement stage re-opened -> the
			// existing implement_pending dispatch arm wins (acceptance still
			// succeeded, but implement pending short-circuits earlier).
			name: "6_fixup_dispatched_implement_reopened",
			run:  naLocalRun("running"),
			stages: []Stage{
				naStage("plan", "succeeded"),
				naStage("implement", "pending"),
				naStage("acceptance", "succeeded"),
			},
			verdict:     "failed",
			disposition: "fixup_dispatched",
			wantState:   "implement_pending",
			wantActions: []string{"fishhawk_dispatch_stage", "fishhawk_run_stage"},
		},
		{
			// (7) retry_dispatched with the acceptance stage re-opened -> the
			// acceptance dispatch arm (acceptance pending).
			name:        "7_retry_dispatched_acceptance_reopened",
			run:         naLocalRun("running"),
			stages:      naAcceptanceStages("pending"),
			verdict:     "failed",
			disposition: "retry_dispatched",
			wantState:   "acceptance_pending",
			wantActions: []string{"fishhawk_dispatch_stage", "fishhawk_run_stage"},
		},
		{
			// (d-transient) fixup_dispatched but the implement stage is NOT yet
			// re-opened in this snapshot (still succeeded) -> defensive poll,
			// never the merge ritual.
			name:        "d_fixup_dispatched_transient_reroute_poll",
			run:         naLocalRun("running"),
			stages:      naAcceptanceStages("succeeded"),
			verdict:     "failed",
			disposition: "fixup_dispatched",
			wantState:   "acceptance_triage_rerouting",
			wantActions: []string{"fishhawk_get_run_status"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			na := nextActionsFor(tc.run, tc.stages, nil, naReviewStatus("implement", "complete"), nil, nil, false, false, tc.verdict, tc.disposition, releaseSignals{})
			if na == nil {
				t.Fatal("nextActionsFor returned nil")
			}
			if na.State != tc.wantState {
				t.Fatalf("state = %q, want %q", na.State, tc.wantState)
			}
			got := actionNames(na)
			if len(got) < len(tc.wantActions) {
				t.Fatalf("actions = %v, want prefix %v", got, tc.wantActions)
			}
			for i := range tc.wantActions {
				if got[i] != tc.wantActions[i] {
					t.Errorf("actions[%d] = %q, want %q (full: %v)", i, got[i], tc.wantActions[i], got)
				}
			}
			// Every action carries the structured fields.
			for i, a := range na.Actions {
				if a.Precondition == "" || a.Consumes == "" || a.Reason == "" {
					t.Errorf("actions[%d] (%s) missing precondition/consumes/reason: %+v", i, a.Action, a)
				}
			}
		})
	}
}

// TestNextActions_AcceptanceTriagePaged_EveryDisposition covers mode (5): each
// paged-family disposition routes to acceptance_triage_paged with the
// read-evidence-then-arbitrate arm. Table-driven over the vocabulary, pinning
// the literal strings mirrored from backend/internal/server/acceptance.go.
func TestNextActions_AcceptanceTriagePaged_EveryDisposition(t *testing.T) {
	prURL := "https://github.com/x/y/pull/42"
	pagedDispositions := []string{
		"paged",
		"rerun_budget_exhausted",
		"fixup_unavailable_paged",
		"retry_unavailable_paged",
		"unsettled_paged",
		"externally_unvalidatable_paged", // #1671 class-5 terminal page
	}
	for _, disp := range pagedDispositions {
		t.Run(disp, func(t *testing.T) {
			run := naLocalRun("running")
			run.PullRequestURL = &prURL
			na := nextActionsFor(run, naAcceptanceStages("succeeded"), nil,
				naReviewStatus("implement", "complete"), nil, nil, false, false, "failed", disp, releaseSignals{})
			if na == nil || na.State != "acceptance_triage_paged" {
				t.Fatalf("state = %+v, want acceptance_triage_paged (not acceptance_triage_rerouting) for disposition %q", na, disp)
			}
			got := actionNames(na)
			want := []string{"fishhawk_list_audit", "fishhawk_fixup_stage", "merge_and_file_follow_up", "fishhawk_cancel_run"}
			if len(got) != len(want) {
				t.Fatalf("actions = %v, want %v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("actions[%d] = %q, want %q", i, got[i], want[i])
				}
			}
			// The manual fix-up route consumes the fix-up budget; the rest none.
			fixup := findAction(t, na, "fishhawk_fixup_stage")
			if fixup.Consumes != consumesFixupBudget {
				t.Errorf("fixup consumes = %q, want fixup_budget", fixup.Consumes)
			}
			// #1549: the manual fix-up precondition must not tell the operator to
			// checkout the run branch (which CAUSES the worktree-conflict failure)
			// and must name the runner's lineage worktree.
			if fixup.Precondition == "" {
				t.Error("fixup precondition must be non-empty")
			}
			if strings.Contains(fixup.Precondition, "checkout the run branch") {
				t.Errorf("fixup precondition still says to checkout the run branch: %q", fixup.Precondition)
			}
			if !strings.Contains(fixup.Precondition, "lineage worktree") {
				t.Errorf("fixup precondition should name the lineage worktree; got %q", fixup.Precondition)
			}
		})
	}
}

// TestNextActions_AcceptanceOutcomeUnknown covers mode (8): a settled
// acceptance stage with an unknown verdict routes to the defensive read arm
// and, load-bearing, NEVER offers the merge ritual (fail toward read, not
// toward merge). It also asserts the #1567 fishhawk_retry_stage recovery verb
// carries the acceptance stage id. BOTH acceptanceOutcomeUnknownActions call
// sites are exercised: the verdict-aged-out arm (verdict=="") and the
// unrecognized-verdict default arm.
func TestNextActions_AcceptanceOutcomeUnknown(t *testing.T) {
	prURL := "https://github.com/x/y/pull/42"

	cases := []struct {
		name    string
		verdict string // "" hits the aged-out arm; a bogus value hits the switch-default arm
	}{
		{name: "verdict aged out of window", verdict: ""},
		{name: "unrecognized verdict value", verdict: "not-a-verdict"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := naLocalRun("running")
			run.PullRequestURL = &prURL
			stages := naAcceptanceStages("succeeded")
			acceptanceID := stages[2].ID // naAcceptanceStages orders plan, implement, acceptance

			na := nextActionsFor(run, stages, nil,
				naReviewStatus("implement", "complete"), nil, nil, false, false, tc.verdict, "", releaseSignals{})
			if na == nil || na.State != "acceptance_settled_outcome_unknown" {
				t.Fatalf("state = %+v, want acceptance_settled_outcome_unknown", na)
			}
			if na.Actions[0].Action != "fishhawk_list_audit" {
				t.Errorf("actions[0] = %q, want fishhawk_list_audit", na.Actions[0].Action)
			}
			var retry *SuggestedAction
			for i := range na.Actions {
				a := na.Actions[i]
				if a.Action == "approve_pr" || a.Action == "merge_pr" {
					t.Fatalf("merge ritual action %q surfaced on an unknown acceptance outcome — must fail toward read, not merge", a.Action)
				}
				if a.Action == "fishhawk_retry_stage" {
					retry = &na.Actions[i]
				}
			}
			if retry == nil {
				t.Fatalf("no fishhawk_retry_stage recovery action in %+v", na.Actions)
			}
			if retry.Params["stage_id"] != acceptanceID {
				t.Errorf("retry stage_id = %q, want the acceptance stage id %q", retry.Params["stage_id"], acceptanceID)
			}
		})
	}
}

// TestNextActions_AcceptanceSkippedOutOfScope_SettledImplement pins the E38.3 /
// #1877 acceptance-arm behavior reached via the settled-implement path (run
// still running, review terminal, acceptance succeeded verdict-less). Three
// modes:
//   - flag set -> acceptance_skipped_out_of_scope + the FULL merge ritual, and
//     crucially NO fishhawk_retry_stage action (the futile reopen the
//     outcome-unknown arm otherwise offers is suppressed for the skip).
//   - flag false (marker aged out) -> the read-first acceptance_settled_outcome_unknown
//     arm unchanged (graceful degradation, fail toward read).
//   - failed verdict + flag set -> the triage arm wins (a recorded verdict takes
//     precedence over the flag).
func TestNextActions_AcceptanceSkippedOutOfScope_SettledImplement(t *testing.T) {
	prURL := "https://github.com/x/y/pull/42"
	newRun := func() *Run { r := naLocalRun("running"); r.PullRequestURL = &prURL; return r }

	t.Run("flag set -> merge ritual, no retry_stage", func(t *testing.T) {
		na := nextActionsFor(newRun(), naAcceptanceStages("succeeded"), nil,
			naReviewStatus("implement", "complete"), nil, nil, false, true, "", "", releaseSignals{})
		if na == nil || na.State != "acceptance_skipped_out_of_scope" {
			t.Fatalf("state = %+v, want acceptance_skipped_out_of_scope", na)
		}
		if got := actionNames(na); len(got) != 3 || got[0] != "approve_pr" || got[1] != "merge_pr" || got[2] != "post_merge" {
			t.Fatalf("actions = %v, want the full merge ritual [approve_pr merge_pr post_merge]", got)
		}
		for _, a := range na.Actions {
			if a.Action == "fishhawk_retry_stage" {
				t.Errorf("fishhawk_retry_stage must NOT be offered for the skip disposition (server 422s it): %+v", na.Actions)
			}
		}
	})

	t.Run("flag false (aged out) -> read-first outcome-unknown arm", func(t *testing.T) {
		na := nextActionsFor(newRun(), naAcceptanceStages("succeeded"), nil,
			naReviewStatus("implement", "complete"), nil, nil, false, false, "", "", releaseSignals{})
		if na == nil || na.State != "acceptance_settled_outcome_unknown" {
			t.Fatalf("state = %+v, want acceptance_settled_outcome_unknown (graceful degradation)", na)
		}
		if na.Actions[0].Action != "fishhawk_list_audit" {
			t.Errorf("actions[0] = %q, want fishhawk_list_audit", na.Actions[0].Action)
		}
		for _, a := range na.Actions {
			if a.Action == "approve_pr" || a.Action == "merge_pr" {
				t.Errorf("merge ritual must not surface on the aged-out arm: %+v", na.Actions)
			}
		}
	})

	t.Run("failed verdict + flag set -> triage wins", func(t *testing.T) {
		na := nextActionsFor(newRun(), naAcceptanceStages("succeeded"), nil,
			naReviewStatus("implement", "complete"), nil, nil, false, true, "failed", "paged", releaseSignals{})
		if na == nil || na.State != "acceptance_triage_paged" {
			t.Fatalf("state = %+v, want acceptance_triage_paged (recorded verdict wins over the flag)", na)
		}
	})
}

// TestNextActions_NoAcceptanceStage_MergeRitualUnchanged covers mode (9): a run
// with no acceptance stage keeps the prior implement_gate_settled + merge
// ritual behavior byte-identical.
func TestNextActions_NoAcceptanceStage_MergeRitualUnchanged(t *testing.T) {
	run := naRun("running")
	stages := []Stage{naStage("plan", "succeeded"), naStage("implement", "succeeded")}
	na := nextActionsFor(run, stages, nil, naReviewStatus("implement", "complete"), nil, nil, false, false, "", "", releaseSignals{})
	if na == nil || na.State != "implement_gate_settled" {
		t.Fatalf("state = %+v, want implement_gate_settled", na)
	}
	if got := actionNames(na); len(got) != 3 || got[0] != "approve_pr" || got[1] != "merge_pr" || got[2] != "post_merge" {
		t.Fatalf("actions = %v, want [approve_pr merge_pr post_merge]", got)
	}
}

// TestLatestAcceptanceSignals covers the extraction helpers over the recent
// audit slice (the mergeObservedIn idiom): newest-wins, category matching, and
// mode (10) malformed payloads (non-object, missing key) yielding "" without
// panic.
func TestLatestAcceptanceSignals(t *testing.T) {
	outcome := func(verdict string) AuditEntry {
		return AuditEntry{Category: "acceptance_outcome_recorded", Payload: map[string]any{"verdict": verdict}}
	}
	triage := func(disp string) AuditEntry {
		return AuditEntry{Category: "acceptance_triage_decided", Payload: map[string]any{"disposition": disp}}
	}

	// Newest wins: the recent slice is time-descending (item 0 newest), so the
	// FIRST matching entry is authoritative.
	recent := []AuditEntry{
		triage("paged"),
		outcome("failed"),
		outcome("passed"), // older; must NOT win
	}
	if v := latestAcceptanceVerdict(recent); v != "failed" {
		t.Errorf("latestAcceptanceVerdict = %q, want failed (newest)", v)
	}
	if d := latestAcceptanceTriageDisposition(recent); d != "paged" {
		t.Errorf("latestAcceptanceTriageDisposition = %q, want paged", d)
	}

	// Cross-attempt correlation (the #1537 fix-up edge): with multiple
	// acceptance attempts in the window, a FRESH failed verdict whose triage
	// has not landed yet must NOT inherit the STALE disposition of an earlier
	// failure. Time-descending: newest failed outcome (attempt B), then the
	// older attempt A's triage + outcome. The stale "paged" sits BELOW the
	// newest verdict, so it belongs to attempt A and must be ignored -> "".
	staleTriage := []AuditEntry{
		outcome("failed"), // attempt B: fresh verdict, no triage yet
		triage("paged"),   // attempt A: older triage — must NOT win
		outcome("failed"), // attempt A: older verdict
	}
	if v := latestAcceptanceVerdict(staleTriage); v != "failed" {
		t.Errorf("verdict over stale triage = %q, want failed (newest)", v)
	}
	if d := latestAcceptanceTriageDisposition(staleTriage); d != "" {
		t.Errorf("disposition over stale triage = %q, want empty (uncorrelated, older attempt)", d)
	}

	// Correlation picks the triage NEWER than the newest verdict, skipping an
	// older attempt's triage: attempt B has both a fresh verdict and a fresh
	// retry_dispatched triage; attempt A's stale "paged" must be ignored.
	correlated := []AuditEntry{
		triage("retry_dispatched"), // attempt B: fresh triage — must win
		outcome("failed"),          // attempt B: fresh verdict
		triage("paged"),            // attempt A: older triage
		outcome("failed"),          // attempt A: older verdict
	}
	if d := latestAcceptanceTriageDisposition(correlated); d != "retry_dispatched" {
		t.Errorf("correlated disposition = %q, want retry_dispatched (newest attempt)", d)
	}

	// A triage entry with NO verdict in the window is uncorrelated -> "" (the
	// classifier is in its defensive read arm when the verdict is absent).
	if d := latestAcceptanceTriageDisposition([]AuditEntry{triage("paged")}); d != "" {
		t.Errorf("disposition with no verdict = %q, want empty", d)
	}

	// Absent categories -> "".
	if v := latestAcceptanceVerdict([]AuditEntry{{Category: "pr_merged"}}); v != "" {
		t.Errorf("verdict on absent category = %q, want empty", v)
	}
	if d := latestAcceptanceTriageDisposition(nil); d != "" {
		t.Errorf("disposition on nil = %q, want empty", d)
	}

	// Mode (10): malformed payloads must not panic and must yield "".
	malformed := []AuditEntry{
		{Category: "acceptance_outcome_recorded", Payload: "not-an-object"},
		{Category: "acceptance_outcome_recorded", Payload: map[string]any{"other": "field"}}, // missing verdict key
		{Category: "acceptance_outcome_recorded", Payload: nil},
	}
	for i, e := range malformed {
		if v := latestAcceptanceVerdict([]AuditEntry{e}); v != "" {
			t.Errorf("malformed[%d] verdict = %q, want empty", i, v)
		}
	}
	// A correlated triage (newer than the verdict) with a malformed payload must
	// still yield "" without panic — the verdict entry keeps it past the
	// correlation short-circuit into the payload parse.
	if d := latestAcceptanceTriageDisposition([]AuditEntry{
		{Category: "acceptance_triage_decided", Payload: []any{1, 2, 3}},
		{Category: "acceptance_outcome_recorded", Payload: map[string]any{"verdict": "failed"}},
	}); d != "" {
		t.Errorf("malformed disposition = %q, want empty", d)
	}
}

// TestAcceptanceVocabularyMatchesBackend is the cross-module literal-pinning
// table (approval condition 2 + the #875 no-import seam). The verdict /
// disposition / audit-category strings are copied verbatim from
// backend/internal/server/acceptance.go and MUST match it. A backend rename
// that is not mirrored here greps to this test.
func TestAcceptanceVocabularyMatchesBackend(t *testing.T) {
	// MUST match backend/internal/server/acceptance.go verbatim.
	want := map[string]string{
		"CategoryAcceptanceOutcomeRecorded": auditCategoryAcceptanceOutcomeRecorded,
		"CategoryAcceptanceTriageDecided":   auditCategoryAcceptanceTriageDecided,
		"acceptanceVerdictPassed":           acceptanceVerdictPassed,
		"acceptanceVerdictFailed":           acceptanceVerdictFailed,
		"fixup_dispatched":                  acceptanceDispositionFixupDispatched,
		"retry_dispatched":                  acceptanceDispositionRetryDispatched,
		"paged":                             acceptanceDispositionPaged,
		"rerun_budget_exhausted":            acceptanceDispositionRerunBudget,
		"fixup_unavailable_paged":           acceptanceDispositionFixupUnavailable,
		"retry_unavailable_paged":           acceptanceDispositionRetryUnavailable,
		"unsettled_paged":                   acceptanceDispositionUnsettled,
		"externally_unvalidatable_paged":    acceptanceDispositionUnvalidatable,
	}
	expect := map[string]string{
		"CategoryAcceptanceOutcomeRecorded": "acceptance_outcome_recorded",
		"CategoryAcceptanceTriageDecided":   "acceptance_triage_decided",
		"acceptanceVerdictPassed":           "passed",
		"acceptanceVerdictFailed":           "failed",
		"fixup_dispatched":                  "fixup_dispatched",
		"retry_dispatched":                  "retry_dispatched",
		"paged":                             "paged",
		"rerun_budget_exhausted":            "rerun_budget_exhausted",
		"fixup_unavailable_paged":           "fixup_unavailable_paged",
		"retry_unavailable_paged":           "retry_unavailable_paged",
		"unsettled_paged":                   "unsettled_paged",
		"externally_unvalidatable_paged":    "externally_unvalidatable_paged",
	}
	for k, wantVal := range expect {
		if want[k] != wantVal {
			t.Errorf("%s mirror = %q, want %q (drifted from backend/internal/server/acceptance.go)", k, want[k], wantVal)
		}
	}

	// The paged-family predicate: auto-routed dispositions are NOT paged.
	for _, d := range []string{"paged", "rerun_budget_exhausted", "fixup_unavailable_paged", "retry_unavailable_paged", "unsettled_paged", "externally_unvalidatable_paged"} {
		if !isAcceptancePagedDisposition(d) {
			t.Errorf("isAcceptancePagedDisposition(%q) = false, want true", d)
		}
	}
	for _, d := range []string{"fixup_dispatched", "retry_dispatched", ""} {
		if isAcceptancePagedDisposition(d) {
			t.Errorf("isAcceptancePagedDisposition(%q) = true, want false", d)
		}
	}
}

// TestAcceptanceStateStringsMatchDrive pins the E31.17 / #1568 cross-surface
// agreement: the MCP next_actions.state strings for the acceptance arm MUST
// equal the backend drive.Rule* presentation-status strings so the drive path
// and the MCP classifier can never disagree on the acceptance gate. The two
// scalar states (pending / settled-outcome-unknown) match byte-for-byte; the
// failed arm's two states (paged / rerouting) share the drive triage rule as a
// PREFIX. Asserted against the classifier's actual output (not a hand-copied
// literal) so a rename of either surface trips this test.
func TestAcceptanceStateStringsMatchDrive(t *testing.T) {
	run := naRun("running")
	acc := func(state string) *Stage { s := naStage("acceptance", state); return &s }

	// pending: a non-terminal, non-running acceptance stage.
	if got := acceptanceStageNextActions(run, acc("pending"), false, "", "").State; got != string(drive.RuleAcceptancePending) {
		t.Errorf("pending state = %q, want %q (drive.RuleAcceptancePending)", got, drive.RuleAcceptancePending)
	}
	// settled-outcome-unknown: a terminal acceptance stage with no verdict.
	if got := acceptanceStageNextActions(run, acc("succeeded"), false, "", "").State; got != string(drive.RuleAcceptanceOutcomeUnknown) {
		t.Errorf("outcome-unknown state = %q, want %q (drive.RuleAcceptanceOutcomeUnknown)", got, drive.RuleAcceptanceOutcomeUnknown)
	}
	// skipped-out-of-scope (E38.3 / #1877): a terminal verdict-less acceptance
	// stage WITH the skip flag classifies acceptance_skipped_out_of_scope — the
	// MCP state string MUST equal the audit-category marker so the drive gate
	// state (server acceptanceGateSkippedOutOfScope) and this surface agree.
	if got := acceptanceStageNextActions(run, acc("succeeded"), true, "", "").State; got != auditCategoryAcceptanceSkippedOutOfScope {
		t.Errorf("skip state = %q, want %q (audit-category marker)", got, auditCategoryAcceptanceSkippedOutOfScope)
	}
	// failed arm: paged + rerouting states both carry the drive triage prefix.
	paged := acceptanceStageNextActions(run, acc("succeeded"), false, acceptanceVerdictFailed, acceptanceDispositionPaged).State
	rerouting := acceptanceStageNextActions(run, acc("succeeded"), false, acceptanceVerdictFailed, acceptanceDispositionFixupDispatched).State
	for _, st := range []string{paged, rerouting} {
		if !strings.HasPrefix(st, string(drive.RuleAcceptanceTriage)) {
			t.Errorf("failed-arm state %q does not carry the drive triage prefix %q", st, drive.RuleAcceptanceTriage)
		}
	}

	// Lock the exact drive-rule literals too, so a coordinated rename that keeps
	// the two surfaces internally consistent but drifts from the documented
	// strings still trips here.
	if drive.RuleAcceptancePending != "acceptance_pending" ||
		drive.RuleAcceptanceOutcomeUnknown != "acceptance_settled_outcome_unknown" ||
		drive.RuleAcceptanceTriage != "acceptance_triage" {
		t.Errorf("drive acceptance rule literals drifted: pending=%q unknown=%q triage=%q",
			drive.RuleAcceptancePending, drive.RuleAcceptanceOutcomeUnknown, drive.RuleAcceptanceTriage)
	}
}

// naReleaseRun builds a running delegating "release" workflow run — the shape
// the release-loop arm (E33.5 / #1590) keys on. It carries no plan/implement
// stages of its own, so it flows past every earlier classifier arm to the
// release arm (gated on releaseSignals.IsRelease).
func naReleaseRun() *Run {
	return &Run{ID: uuid.NewString(), Repo: "x/y", WorkflowID: "release", State: "running"}
}

// TestNextActions_ReleaseStates covers the five release-loop states (E33.5 /
// #1590, ADR-051): each asserts the classified state, the named release verb it
// points at, and — the structural invariant — that the actions list is never
// empty. It also proves the arm is gated: a release-shaped run with the zero
// releaseSignals (IsRelease false) produces NO release state.
func TestNextActions_ReleaseStates(t *testing.T) {
	cases := []struct {
		name       string
		sig        releaseSignals
		wantState  string
		wantAction string // a verb/step the arm must surface
	}{
		{
			name:       "notes_ready prepares the notes",
			sig:        releaseSignals{IsRelease: true},
			wantState:  "notes_ready",
			wantAction: "fishhawk_release_notes",
		},
		{
			name:       "awaiting_cut previews then cuts",
			sig:        releaseSignals{IsRelease: true, NotesPrepared: true},
			wantState:  "awaiting_cut",
			wantAction: "release_cut",
		},
		{
			name:       "pipeline_running polls the in-flight pipeline",
			sig:        releaseSignals{IsRelease: true, NotesPrepared: true, Cut: true, DeployState: "running"},
			wantState:  "pipeline_running",
			wantAction: "fishhawk_get_run_status",
		},
		{
			name:       "awaiting_publish publishes the notes",
			sig:        releaseSignals{IsRelease: true, NotesPrepared: true, Cut: true, DeployState: "succeeded"},
			wantState:  "awaiting_publish",
			wantAction: "release_publish",
		},
		{
			name:       "awaiting_publish when no deploy stage gates the pipeline",
			sig:        releaseSignals{IsRelease: true, NotesPrepared: true, Cut: true},
			wantState:  "awaiting_publish",
			wantAction: "release_publish",
		},
		{
			name:       "published polls until the run resolves",
			sig:        releaseSignals{IsRelease: true, NotesPrepared: true, Cut: true, Published: true, DeployState: "succeeded"},
			wantState:  "published",
			wantAction: "fishhawk_get_run_status",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := naReleaseRun()
			na := nextActionsFor(run, nil, nil, nil, nil, nil, false, false, "", "", tc.sig)
			if na == nil {
				t.Fatal("nextActionsFor returned nil for a non-terminal release run")
			}
			if na.State != tc.wantState {
				t.Errorf("state = %q, want %q", na.State, tc.wantState)
			}
			if len(na.Actions) == 0 {
				t.Fatal("release arm returned an empty actions list (violates the never-empty invariant)")
			}
			// The named verb must be present (findAction fails the test otherwise).
			_ = findAction(t, na, tc.wantAction)
		})
	}

	// awaiting_cut leads with the read-only preview before the cut, and the cut
	// reason must call out that the tag push is a HUMAN git action (binding
	// approval condition) and that cut records the decision only.
	t.Run("awaiting_cut leads with preview and flags the human tag push", func(t *testing.T) {
		na := nextActionsFor(naReleaseRun(), nil, nil, nil, nil, nil, false, false, "", "",
			releaseSignals{IsRelease: true, NotesPrepared: true})
		if got := actionNames(na); len(got) == 0 || got[0] != "fishhawk_release_notes" {
			t.Fatalf("awaiting_cut actions = %v, want the preview first", got)
		}
		cut := findAction(t, na, "release_cut")
		if !strings.Contains(cut.Reason, "human git action") {
			t.Errorf("release_cut reason must name the human-led tag push; got %q", cut.Reason)
		}
		if !strings.Contains(cut.Reason, "NO git tag") && !strings.Contains(cut.Reason, "no git tag") {
			t.Errorf("release_cut reason must state it pushes no tag (records the decision only); got %q", cut.Reason)
		}
	})

	// Gate proof: the zero releaseSignals (IsRelease false) must NOT synthesize a
	// release state even for a WorkflowID=="release" run — the arm is inert
	// without the signal, so a release run with no stages falls to stages_pending.
	t.Run("gated off when IsRelease is false", func(t *testing.T) {
		na := nextActionsFor(naReleaseRun(), nil, nil, nil, nil, nil, false, false, "", "", releaseSignals{})
		if na == nil {
			t.Fatal("nextActionsFor returned nil")
		}
		for _, rs := range []string{"notes_ready", "awaiting_cut", "pipeline_running", "awaiting_publish", "published"} {
			if na.State == rs {
				t.Errorf("release state %q surfaced with the zero releaseSignals (arm not gated)", rs)
			}
		}
	})
}
